package steve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ClusterText words a lookup of v among the clusters the caller can read,
// for a command's --cluster: the NOT_FOUND names the access clusters
// command, which lists them.
func ClusterText(v string) engine.MatchText {
	return engine.MatchText{Noun: "cluster", Plural: "clusters", Flag: "--cluster",
		NotFound: "cluster not found: " + clierr.ShellQuote(v) +
			"; it is not among the clusters you can read; pass a cluster id or display name",
		NotFoundHint: clierr.Rancher + " access clusters"}
}

// MatchCluster picks the cluster v names in list, by id or display name,
// worded by t. It sends no request. When list is empty the NOT_FOUND says
// the caller can read no cluster, in place of t.NotFound and
// t.NotFoundHint.
func MatchCluster(list []Cluster, v string, t engine.MatchText) (id, warning string, err error) {
	ids := make([]string, 0, len(list))
	display := make(map[string]string, len(list))
	cands := make([]clierr.Candidate, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
		display[c.ID] = c.DisplayName
		cands = append(cands, clierr.Candidate{ID: c.ID, Name: c.DisplayName})
	}
	if len(list) == 0 {
		t.NotFound = fmt.Sprintf("cluster not found: %s; you can read no cluster on this server; a Rancher administrator grants cluster and project roles",
			clierr.ShellQuote(v))
		t.NotFoundHint = ""
	}
	return engine.MatchOne(v, ids, func(id string) string { return display[id] }, cands, t)
}

// ResolveCluster is MatchCluster over list, the caller's own read of the
// collection, plus one GET by id when nothing matches: a cluster steve's
// cache has not listed yet. The GET's 403 or 404 keeps the NOT_FOUND; any
// other failure is returned.
func ResolveCluster(ctx context.Context, get Getter, list []Cluster, v string, t engine.MatchText) (id, warning string, err error) {
	id, warning, err = MatchCluster(list, v, t)
	if err == nil {
		return id, warning, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeNotFound || len(validation.IsDNS1123Subdomain(v)) > 0 {
		return "", "", err
	}
	r := Request{Type: ClustersType, Name: v}
	body, gerr := get(ctx, r)
	if gerr != nil {
		if se, ok := errors.AsType[*StatusError](gerr); ok && (se.Status == http.StatusForbidden || se.Status == http.StatusNotFound) {
			return "", "", err
		}
		return "", "", gerr
	}
	got, _, derr := decodeCluster(r.Path(), 0, body)
	if derr != nil {
		return "", "", derr
	}
	if got != v {
		return "", "", protocolError(r.Path(), fmt.Sprintf("the answer is cluster %q", got))
	}
	return v, "", nil
}

// GetCluster reads one cluster through steve. A 403 is the caller's lack
// of access to that cluster; a 404 is NOT_FOUND, worded for a cluster
// deleted after it was resolved; any other failure is the getter's.
func GetCluster(ctx context.Context, get Getter, id string) (*unstructured.Unstructured, error) {
	r := Request{Type: ClustersType, Name: id}
	body, err := get(ctx, r)
	if err != nil {
		se, ok := errors.AsType[*StatusError](err)
		switch {
		case ok && se.Status == http.StatusForbidden:
			return nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot read cluster %s (GET %s answered 403)",
				clierr.ShellQuote(id), r.Path())).WithHint(clierr.Rancher + " access clusters").WithCause(err)
		case ok && se.Status == http.StatusNotFound:
			return nil, clierr.New(clierr.CodeNotFound, "cluster not found: "+clierr.ShellQuote(id)).
				WithHint(clierr.Rancher + " access clusters").WithCause(err)
		}
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &obj.Object); err != nil || obj.Object == nil {
		return nil, protocolError(r.Path(), "not a JSON object")
	}
	return obj, nil
}
