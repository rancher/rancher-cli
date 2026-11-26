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
		NotFoundHint: clierr.NewCommand("access clusters")}
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
		t.NotFound = fmt.Sprintf("cluster not found: %s; %s", clierr.ShellQuote(v), noReadableCluster)
		t.NotFoundHint = clierr.Command{}
	}
	return engine.MatchOne(v, ids, func(id string) string { return display[id] }, cands, t)
}

// clusterRefused is in the cause chain of the RBAC error returned when Rancher
// refuses (HTTP 403) the get of one cluster by its id. Code that receives the
// error can use it to tell that error from a 403 on the clusters collection
// and to get the cluster id.
type clusterRefused struct{ id string }

func (e *clusterRefused) Error() string {
	return "Rancher refused the get of cluster " + clierr.ShellQuote(e.id) + " by its id (HTTP 403)"
}

// RefusedClusterID returns the cluster id from the RBAC error returned when
// Rancher refuses (HTTP 403) the get of one cluster by its id. For any other
// error, such as a 403 on the clusters collection, ok is false.
func RefusedClusterID(err error) (string, bool) {
	r, ok := errors.AsType[*clusterRefused](err)
	if !ok {
		return "", false
	}
	return r.id, true
}

// clusterRefusedByID returns the RBAC error for the request r that gets
// cluster id by its id, when Rancher refuses it (HTTP 403). Rancher refuses it
// for a cluster the caller cannot read and for an id no cluster has. The
// message therefore does not say whether the cluster exists. suffix ends the
// message and hint is the command that lists the clusters the caller can read;
// either may be empty.
func clusterRefusedByID(r Request, id string, getterErr error, hint clierr.Command, suffix string) *clierr.Error {
	return clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot read cluster %s (GET %s answered 403)", clierr.ShellQuote(id), r.Path())+suffix).
		WithHintCommand(hint).WithCause(fmt.Errorf("%w: %w", &clusterRefused{id: id}, getterErr))
}

// noReadableCluster ends the message of an error for a cluster lookup when
// the caller can read no cluster.
const noReadableCluster = "you can read no cluster on this server; a Rancher administrator grants cluster and project roles"

// ResolveCluster is MatchCluster over list, the caller's own read of the
// collection, plus one GET by id when nothing matches: a cluster steve's
// cache has not listed yet. The GET's 403 returns an RBAC error. Its message
// ends with t.RBACSuffix and its hint is t.NotFoundHint, and it lists the same
// candidates as the NOT_FOUND error; when list is empty the message ends with
// noReadableCluster and there is no hint, as for the NOT_FOUND error. The
// GET's 404 returns that NOT_FOUND error. Any other failure is returned.
func ResolveCluster(ctx context.Context, get Getter, list []Cluster, v string, t engine.MatchText) (id, warning string, err error) {
	id, warning, err = MatchCluster(list, v, t)
	if err == nil {
		return id, warning, nil
	}
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeNotFound || len(validation.IsDNS1123Subdomain(v)) > 0 {
		return "", "", err
	}
	r := Request{Type: ClustersType, Name: v}
	body, gerr := get(ctx, r)
	if gerr != nil {
		switch st, ok := clierr.HTTPStatus(gerr); {
		case ok && st == http.StatusForbidden:
			suffix, hint := t.RBACSuffix, t.NotFoundHint
			if len(list) == 0 {
				suffix, hint = "; "+noReadableCluster, clierr.Command{}
			}
			return "", "", clusterRefusedByID(r, v, gerr, hint, suffix).WithCandidatesOf(ce)
		case ok && st == http.StatusNotFound:
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
			return nil, clusterRefusedByID(r, id, err, clierr.NewCommand("access clusters"), "")
		case ok && st == http.StatusNotFound:
			return nil, clierr.New(clierr.CodeNotFound, "cluster not found: "+clierr.ShellQuote(id)).
				WithHintCommand(clierr.NewCommand("access clusters")).WithCause(err)
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
