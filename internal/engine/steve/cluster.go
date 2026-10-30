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
		if st, ok := clierr.HTTPStatus(gerr); ok && (st == http.StatusForbidden || st == http.StatusNotFound) {
			return "", "", err
		}
		return "", "", gerr
	}
	got, _, derr := decodeCluster(r.Path(), 0, body)
	if derr != nil {
		return "", "", derr
	}
	if got != v {
		return "", "", protocolError(r.Path(), fmt.Sprintf("the response names cluster %q, not the cluster requested", got))
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
		st, ok := clierr.HTTPStatus(err)
		switch {
		case ok && st == http.StatusForbidden:
			return nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot read cluster %s (GET %s answered 403)",
				clierr.ShellQuote(id), r.Path())).WithHint(clierr.Rancher + " access clusters").WithCause(err)
		case ok && st == http.StatusNotFound:
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

// ObjectState returns the state steve serves for an object it returned. Steve
// computes a state for every object it serves, from the object's fields and
// conditions, and puts it in metadata.state; this returns its name. It is
// empty when the object has no metadata.state or the name is not a string.
func ObjectState(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	name, _, _ := unstructured.NestedString(obj.Object, "metadata", "state", "name")
	return name
}
