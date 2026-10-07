package access

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// The steve collections of management Clusters and Projects (steve.Clusters,
// steve.Projects) answer, unlike the Kubernetes-style LIST through the
// proxy, which Rancher refuses for every caller who is not an
// administrator, with exactly the objects the caller's access set grants.
// The namespaces collection on one cluster is the same kind of read: a
// project member sees the namespaces of its projects there while the
// Kubernetes LIST refuses it. These are the only steve reads the access
// suite makes.

// steveProjectsPath is the all-clusters project collection, named in
// protocol errors about the project set.
var steveProjectsPath = steve.Request{Type: steve.ProjectsType}.Path()

func steveNamespacesPath(clusterID string) string {
	return steve.Request{Cluster: clusterID, Type: "namespaces"}.Path()
}

// projectIDLabel is the namespace label Rancher sets to the bare id of the
// project the namespace belongs to. The annotation of the same name carries
// <cluster>:<project> and is not read.
const projectIDLabel = "field.cattle.io/projectId"

// discoveredCluster is one row of the caller's cluster set.
type discoveredCluster struct {
	ID          string
	DisplayName string
	// Returned is false for the local row inserted after discovery when
	// the collection did not carry it.
	Returned bool
}

// discoveredProject is one project of the caller's project set. Project is
// the bare project id (the part after the cluster in steve's
// <cluster>/<project> id), the value a namespace's project label carries.
type discoveredProject struct {
	Cluster     string
	Project     string
	DisplayName string
}

// discoveredNamespace is one namespace of a cluster as the caller sees it.
// Project is the bare project id from the namespace's label, empty when the
// namespace belongs to no project.
type discoveredNamespace struct {
	Name    string
	Project string
}

type steveNamespaceItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// protocolError is the terminal answer to a response the reader cannot
// trust: a shape it does not recognize is never guessed at.
func protocolError(path, why string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("unexpected answer from %s: %s", path, why))
}

// listClusters reads the caller's clusters from steve. The local cluster is
// exactly one row: taken from the collection when present, inserted
// otherwise.
func listClusters(ctx context.Context, root engine.RootClient) ([]discoveredCluster, error) {
	list, err := steve.Clusters(ctx, steve.RootGetter(root))
	if err != nil {
		return nil, err
	}
	return discoveredClusters(list), nil
}

// discoveredClusters is the cluster set from steve's rows: ordered by id,
// with the local row.
func discoveredClusters(list []steve.Cluster) []discoveredCluster {
	rows := make([]discoveredCluster, 0, len(list)+1)
	for _, c := range list {
		rows = append(rows, discoveredCluster{ID: c.ID, DisplayName: c.DisplayName, Returned: true})
	}
	slices.SortFunc(rows, func(a, b discoveredCluster) int { return strings.Compare(a.ID, b.ID) })
	return withLocal(rows)
}

// listProjects reads the caller's projects from steve, ordered by cluster
// then project.
func listProjects(ctx context.Context, root engine.RootClient) ([]discoveredProject, error) {
	list, err := steve.Projects(ctx, steve.RootGetter(root), "")
	if err != nil {
		return nil, err
	}
	rows := make([]discoveredProject, len(list))
	for i, p := range list {
		rows[i] = discoveredProject{Cluster: p.Cluster, Project: p.ID, DisplayName: p.DisplayName}
	}
	slices.SortFunc(rows, func(a, b discoveredProject) int {
		if c := strings.Compare(a.Cluster, b.Cluster); c != 0 {
			return c
		}
		return strings.Compare(a.Project, b.Project)
	})
	return rows, nil
}

// listNamespaces reads one cluster's namespaces as the caller sees them,
// ordered by name.
func listNamespaces(ctx context.Context, root engine.RootClient, clusterID string) ([]discoveredNamespace, error) {
	rows, err := steve.ReadCollection(ctx, steve.RootGetter(root), steve.Request{Cluster: clusterID, Type: "namespaces"}, decodeNamespaceItem)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b discoveredNamespace) int { return strings.Compare(a.Name, b.Name) })
	return rows, nil
}

// decodeNamespaceItem consumes id, metadata.name and the project label.
func decodeNamespaceItem(path string, index int, raw json.RawMessage) (string, discoveredNamespace, error) {
	var item steveNamespaceItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", discoveredNamespace{}, protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
	}
	if item.ID == "" {
		return "", discoveredNamespace{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
	}
	if item.Metadata.Name != "" && item.Metadata.Name != item.ID {
		return "", discoveredNamespace{}, protocolError(path, fmt.Sprintf("item %q names itself %q", item.ID, item.Metadata.Name))
	}
	return item.ID, discoveredNamespace{Name: item.ID, Project: item.Metadata.Labels[projectIDLabel]}, nil
}

// withLocal guarantees the local row: the management cluster is always a
// row of the caller's cluster set, whether or not the collection listed it.
func withLocal(rows []discoveredCluster) []discoveredCluster {
	if slices.ContainsFunc(rows, func(c discoveredCluster) bool { return c.ID == engine.LocalCluster }) {
		return rows
	}
	rows = append(rows, discoveredCluster{ID: engine.LocalCluster, DisplayName: engine.LocalCluster, Returned: false})
	slices.SortFunc(rows, func(a, b discoveredCluster) int { return strings.Compare(a.ID, b.ID) })
	return rows
}

// projectsWithinClusters checks that every project's cluster is a row of the
// final cluster set: a project steve lists on a cluster steve did not list is
// an answer the two collections cannot both have given from one state.
func projectsWithinClusters(projects []discoveredProject, clusters []discoveredCluster) error {
	known := map[string]bool{}
	for _, c := range clusters {
		known[c.ID] = true
	}
	for _, p := range projects {
		if !known[p.Cluster] {
			return protocolError(steveProjectsPath, fmt.Sprintf("project %s/%s belongs to a cluster the cluster collection did not list", p.Cluster, p.Project))
		}
	}
	return nil
}
