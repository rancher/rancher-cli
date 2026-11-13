package steve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ProjectsType is the steve type of Rancher's management projects.
const ProjectsType = "management.cattle.io.projects"

// Project is one project the caller can read. ID is the bare
// metadata.name, the form the project label and the backing namespace use.
// System is true for the cluster's System project: the project whose
// authz.management.cattle.io/system-project label is "true".
type Project struct {
	Cluster, ID, DisplayName string
	System                   bool
}

// systemProjectLabel marks the System project of a cluster.
const systemProjectLabel = "authz.management.cattle.io/system-project"

type projectItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ClusterName string `json:"clusterName"`
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

// Projects reads the projects the caller can read on clusterID, or on
// every cluster when clusterID is "". Every item's steve id must be
// <cluster>/<metadata.name> with the cluster its spec.clusterName and, for
// one cluster, clusterID.
func Projects(ctx context.Context, get Getter, clusterID string) ([]Project, error) {
	r := Request{Type: ProjectsType, Namespace: clusterID}
	rows, err := ReadCollection(ctx, get, r, func(path string, index int, raw json.RawMessage) (string, Project, error) {
		var item projectItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return "", Project{}, protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
		}
		cluster, name, ok := strings.Cut(item.ID, "/")
		switch {
		case item.ID == "":
			return "", Project{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
		case !ok || cluster == "" || name == "" || strings.Contains(name, "/"):
			return "", Project{}, protocolError(path, fmt.Sprintf("item %q is not a <cluster>/<project> id", item.ID))
		case item.Spec.ClusterName == "":
			return "", Project{}, protocolError(path, fmt.Sprintf("item %q has no spec.clusterName", item.ID))
		case item.Spec.ClusterName != cluster:
			return "", Project{}, protocolError(path, fmt.Sprintf("item %q belongs to cluster %q", item.ID, item.Spec.ClusterName))
		case clusterID != "" && cluster != clusterID:
			return "", Project{}, protocolError(path, fmt.Sprintf("item %q is not on cluster %q", item.ID, clusterID))
		case item.Metadata.Name != "" && item.Metadata.Name != name:
			return "", Project{}, protocolError(path, fmt.Sprintf("item %q has metadata.name %q, which differs from its id", item.ID, item.Metadata.Name))
		}
		return item.ID, Project{Cluster: cluster, ID: name, DisplayName: item.Spec.DisplayName, System: item.Metadata.Labels[systemProjectLabel] == "true"}, nil
	})
	if err != nil {
		return nil, collectionRefusal(r, err)
	}
	return rows, nil
}

// collectionRefusal words a collection read's non-2xx answer: steve
// answers a signed-in user's collection read with 200; a 403 came from
// something in front of it, and any other status, 404 included, is
// reported as one Rancher's steve API does not return for this request. A
// 401, a 429 and a 5xx keep the root client's error, classified by the
// status table.
func collectionRefusal(r Request, err error) error {
	st, ok := clierr.HTTPStatus(err)
	if !ok {
		return err
	}
	switch st {
	case http.StatusUnauthorized, http.StatusTooManyRequests:
		return err
	case http.StatusForbidden:
		return clierr.New(clierr.CodeRBAC, fmt.Sprintf(
			"the server refused GET %s (403); Rancher returns 200 for this request to every signed-in user; a proxy in front of Rancher or an unsupported Rancher version refused it",
			r.Path())).WithCause(err)
	default:
		if st >= http.StatusInternalServerError {
			return err
		}
		return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
			"GET %s returned %d; Rancher's steve API does not return this status for this request", r.Path(), st)).WithCause(err)
	}
}

// ResolveProject maps v, a project id or display name, to one project the
// caller can read on clusterID. When v is one project's id and also other
// projects' display name, the id wins and warning names the others. When nothing matches, one GET by id
// through the local cluster covers a project steve's cache has not listed
// yet; its 403 or 404 keeps the NOT_FOUND.
func ResolveProject(ctx context.Context, get Getter, local engine.ClusterClient, clusterID, v string) (p Project, warning string, err error) {
	return resolveProject(ctx, get, local, clusterID, v, "--project")
}

// ResolveProjectArgument is ResolveProject for a value typed as a
// positional argument: its warning names no flag.
func ResolveProjectArgument(ctx context.Context, get Getter, local engine.ClusterClient, clusterID, v string) (p Project, warning string, err error) {
	return resolveProject(ctx, get, local, clusterID, v, "")
}

func resolveProject(ctx context.Context, get Getter, local engine.ClusterClient, clusterID, v string, flag clierr.CommandWord) (p Project, warning string, err error) {
	list, err := Projects(ctx, get, clusterID)
	if err != nil {
		return Project{}, "", err
	}
	ids := make([]string, 0, len(list))
	display := make(map[string]string, len(list))
	cands := make([]clierr.Candidate, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
		display[p.ID] = p.DisplayName
		cands = append(cands, clierr.Candidate{ID: p.ID, Name: p.DisplayName})
	}
	c := clierr.ShellQuote(clusterID)
	t := engine.MatchText{Noun: "project", Plural: "projects", Flag: flag,
		NotFound: fmt.Sprintf("project not found: %s; it is not among the projects you can read on cluster %s; pass a project id or display name",
			clierr.ShellQuote(v), c),
		NotFoundHint: clierr.NewCommand("access projects").FlagValue("--cluster", clusterID)}
	if len(list) == 0 {
		t.NotFound = fmt.Sprintf("project not found: %s; you can read no project on cluster %s; a cluster owner or a Rancher administrator grants project roles",
			clierr.ShellQuote(v), c)
		t.NotFoundHint = clierr.Command{}
	}
	id, warning, err := engine.MatchOne(v, ids, func(id string) string { return display[id] }, cands, t)
	if err == nil {
		return Project{Cluster: clusterID, ID: id, DisplayName: display[id]}, warning, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeNotFound || len(validation.IsDNS1123Subdomain(v)) > 0 {
		return Project{}, "", err
	}
	projectKind, _ := kinds.Lookup("project")
	obj, gerr := local.Get(ctx, projectKind.GVR, clusterID, v)
	if gerr != nil {
		if ge, ok := errors.AsType[*clierr.Error](gerr); ok && (ge.Code == clierr.CodeNotFound || ge.Code == clierr.CodeRBAC) {
			return Project{}, "", err
		}
		return Project{}, "", gerr
	}
	dn, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	return Project{Cluster: clusterID, ID: v, DisplayName: dn}, "", nil
}

// ProjectNamespaces returns the names of projectID's namespaces on
// clusterID: the members, by their field.cattle.io/projectId annotation, of
// ProjectNamespaceObjects.
func ProjectNamespaces(ctx context.Context, downstream engine.ClusterClient, get Getter, clusterID, projectID string) (map[string]bool, error) {
	items, _, err := ProjectNamespaceObjects(ctx, downstream, get, clusterID, projectID)
	if err != nil {
		return nil, err
	}
	members := make(map[string]bool, len(items))
	for i := range items {
		members[items[i].GetName()] = true
	}
	return members, nil
}

// ProjectNamespaceObjects returns projectID's namespaces on clusterID as
// objects: the namespace LIST through downstream, narrowed server-side by
// the project label, or, when that LIST is refused (a project member may
// not list namespaces cluster-wide), the namespaces steve lists for the
// caller. Either answer is kept by projects.IsMember: the label only
// narrows the list. viaSteve is true for the steve answer, whose items for a
// namespace the caller cannot get carry only a few fields (see
// ReadableInFull).
func ProjectNamespaceObjects(ctx context.Context, downstream engine.ClusterClient, get Getter, clusterID, projectID string) (items []unstructured.Unstructured, viaSteve bool, err error) {
	selector, err := projects.MemberSelector(projectID)
	if err != nil {
		return nil, false, err
	}
	nsKind, _ := kinds.Lookup("namespace")
	list, err := downstream.List(ctx, nsKind.GVR, "", selector)
	if err == nil {
		for i := range list.Items {
			if projects.IsMember(&list.Items[i], clusterID, projectID) {
				items = append(items, list.Items[i])
			}
		}
		return items, false, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeRBAC {
		return nil, false, err
	}
	r := Request{Cluster: clusterID, Type: "namespaces"}
	rows, err := ReadCollection(ctx, get, r, func(path string, index int, raw json.RawMessage) (string, string, error) {
		var item unstructured.Unstructured
		if err := json.Unmarshal(raw, &item.Object); err != nil {
			return "", "", protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
		}
		name := item.GetName()
		if name == "" {
			return "", "", protocolError(path, fmt.Sprintf("item %d has no metadata.name", index))
		}
		if projects.IsMember(&item, clusterID, projectID) {
			return name, string(raw), nil
		}
		return name, "", nil
	})
	if st, ok := clierr.HTTPStatus(err); ok && st == http.StatusUnauthorized {
		return nil, false, clierr.New(clierr.CodeRBAC, fmt.Sprintf(
			"Rancher refused access to cluster %s (401 with no body, its answer to a user with no role on that cluster)", clierr.ShellQuote(clusterID))).
			WithHintCommand(clierr.NewCommand("access clusters")).WithCause(err)
	}
	if err != nil {
		return nil, false, collectionRefusal(r, err)
	}
	for _, raw := range rows {
		if raw == "" {
			continue
		}
		var item unstructured.Unstructured
		if err := json.Unmarshal([]byte(raw), &item.Object); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	return items, true, nil
}

// ReadableInFull reports whether a namespace object from steve's list is the
// whole object. For a namespace the caller cannot get, steve answers only
// the name, resourceVersion, phase and a few annotations and labels; every
// namespace the API returns in full carries the kubernetes.io/metadata.name
// label.
func ReadableInFull(ns *unstructured.Unstructured) bool {
	return ns.GetLabels()[namespaceNameLabel] != ""
}

const namespaceNameLabel = "kubernetes.io/metadata.name"
