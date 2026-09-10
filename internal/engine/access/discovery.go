package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// steveClustersPath and steveProjectsPath are the steve collections of
// management Clusters and Projects. Unlike the Kubernetes-style LIST through
// the proxy, which Rancher refuses for every caller who is not an
// administrator, these collections answer with exactly the objects the
// caller's access set grants. steveNamespacesPath is the same kind of
// collection on one cluster: a project member sees the namespaces of its
// projects there while the Kubernetes LIST refuses it. These are the only
// steve reads the access suite makes.
const (
	steveClustersPath = "/v1/management.cattle.io.clusters"
	steveProjectsPath = "/v1/management.cattle.io.projects"
)

func steveNamespacesPath(clusterID string) string {
	return "/k8s/clusters/" + clusterID + "/v1/namespaces"
}

// projectIDLabel is the namespace label Rancher sets to the bare id of the
// project the namespace belongs to. The annotation of the same name carries
// <cluster>:<project> and is not read.
const projectIDLabel = "field.cattle.io/projectId"

// steveMaxPages bounds a paged collection read. steve's default page holds
// 100000 items, so a real installation fits in one; the bound exists so a
// server that keeps handing out continue tokens cannot keep the CLI reading.
const steveMaxPages = 100

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

// steveCollection is the part of a steve collection response the reader
// consumes. count is the collection total before paging and must be on
// every page and agree across them; continue is a per-page token, opaque
// here; revision is the cache's resourceVersion for the page. Items are
// decoded per collection.
type steveCollection struct {
	Data     []json.RawMessage `json:"data"`
	Count    *int              `json:"count"`
	Continue string            `json:"continue"`
	Revision string            `json:"revision"`
}

type steveClusterItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

type steveProjectItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ClusterName string `json:"clusterName"`
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

type steveNamespaceItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// collectionChanged is the retryable error a read ends with when the pages
// it saw cannot have come from one state of the collection.
func collectionChanged(path, why string) *clierr.Error {
	return clierr.Retryable(clierr.CodeGeneric,
		fmt.Sprintf("the collection %s changed while it was being read (%s); retry the command", path, why))
}

// protocolError is the terminal answer to a response the reader cannot
// trust: a shape it does not recognize is never guessed at.
func protocolError(path, why string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("unexpected answer from %s: %s", path, why))
}

// steveDecoder turns one raw collection item into a row and the id it is
// deduplicated by. The index is the item's position in the raw stream, for
// messages about items that carry no id.
type steveDecoder[T comparable] func(path string, index int, raw json.RawMessage) (id string, row T, err error)

// listClusters reads the caller's clusters from steve. The local cluster is
// exactly one row: taken from the collection when present, inserted
// otherwise.
func listClusters(ctx context.Context, root engine.RootClient) ([]discoveredCluster, error) {
	rows, err := readSteve(ctx, root, steveClustersPath, decodeClusterItem)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b discoveredCluster) int { return strings.Compare(a.ID, b.ID) })
	return withLocal(rows), nil
}

// listProjects reads the caller's projects from steve, ordered by cluster
// then project.
func listProjects(ctx context.Context, root engine.RootClient) ([]discoveredProject, error) {
	rows, err := readSteve(ctx, root, steveProjectsPath, decodeProjectItem)
	if err != nil {
		return nil, err
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
	rows, err := readSteve(ctx, root, steveNamespacesPath(clusterID), decodeNamespaceItem)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b discoveredNamespace) int { return strings.Compare(a.Name, b.Name) })
	return rows, nil
}

func decodeClusterItem(path string, index int, raw json.RawMessage) (string, discoveredCluster, error) {
	var item steveClusterItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", discoveredCluster{}, protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
	}
	if item.ID == "" {
		return "", discoveredCluster{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
	}
	if item.Metadata.Name != "" && item.Metadata.Name != item.ID {
		return "", discoveredCluster{}, protocolError(path, fmt.Sprintf("item %q names itself %q", item.ID, item.Metadata.Name))
	}
	return item.ID, discoveredCluster{ID: item.ID, DisplayName: item.Spec.DisplayName, Returned: true}, nil
}

// decodeProjectItem consumes id, metadata.name, spec.clusterName and
// spec.displayName. steve's id is <cluster>/<project>; the cluster part must
// agree with spec.clusterName and the project part with metadata.name.
func decodeProjectItem(path string, index int, raw json.RawMessage) (string, discoveredProject, error) {
	var item steveProjectItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
	}
	if item.ID == "" {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
	}
	cluster, project, ok := strings.Cut(item.ID, "/")
	if !ok || cluster == "" || project == "" || strings.Contains(project, "/") {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %q is not a <cluster>/<project> id", item.ID))
	}
	if item.Spec.ClusterName == "" {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %q has no spec.clusterName", item.ID))
	}
	if item.Spec.ClusterName != cluster {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %q belongs to cluster %q", item.ID, item.Spec.ClusterName))
	}
	if item.Metadata.Name != "" && item.Metadata.Name != project {
		return "", discoveredProject{}, protocolError(path, fmt.Sprintf("item %q names itself %q", item.ID, item.Metadata.Name))
	}
	return item.ID, discoveredProject{Cluster: cluster, Project: project, DisplayName: item.Spec.DisplayName}, nil
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

// readSteve reads a whole steve collection. The read is retried once as a
// whole when its pages disagree with each other (a revision that moves, a
// count that does not match what was read, a repeated item that does not
// agree with itself); a second disagreement is the retryable
// collectionChanged error. Malformed items and responses are terminal on
// the first read.
func readSteve[T comparable](ctx context.Context, root engine.RootClient, path string, decode steveDecoder[T]) ([]T, error) {
	var lastErr error
	for attempt := range 2 {
		rows, err := readSteveOnce(ctx, root, path, decode)
		if err == nil {
			return rows, nil
		}
		ce, ok := errors.AsType[*clierr.Error](err)
		if !ok || !ce.Retryable || attempt == 1 {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func readSteveOnce[T comparable](ctx context.Context, root engine.RootClient, path string, decode steveDecoder[T]) ([]T, error) {
	var (
		rows     []T
		byID     = map[string]T{}
		raw      int
		total    *int
		revision string
		seen     = map[string]bool{}
		token    string
	)
	for page := 0; ; page++ {
		if page == steveMaxPages {
			return nil, protocolError(path, fmt.Sprintf("more than %d pages", steveMaxPages))
		}
		pagePath := path
		if token != "" {
			pagePath += "?continue=" + url.QueryEscape(token)
		}
		body, err := root.GetJSON(ctx, pagePath)
		if err != nil {
			return nil, err
		}
		var col steveCollection
		if err := json.Unmarshal(body, &col); err != nil {
			return nil, protocolError(path, "not a steve collection: "+err.Error())
		}
		if col.Revision == "" {
			return nil, protocolError(path, "a page without a revision")
		}
		if col.Count == nil || *col.Count < 0 {
			return nil, protocolError(path, "a page without a count")
		}
		if page == 0 {
			revision = col.Revision
			total = col.Count
		} else {
			if col.Revision != revision {
				return nil, collectionChanged(path, "revision moved between pages")
			}
			if *col.Count != *total {
				return nil, collectionChanged(path, fmt.Sprintf("count moved between pages (%d, then %d)", *total, *col.Count))
			}
		}
		for i, item := range col.Data {
			id, row, err := decode(path, raw+i, item)
			if err != nil {
				return nil, err
			}
			if prev, dup := byID[id]; dup {
				if prev != row {
					return nil, collectionChanged(path, "repeated item "+id+" disagrees with itself")
				}
				continue
			}
			byID[id] = row
			rows = append(rows, row)
		}
		raw += len(col.Data)
		if col.Continue == "" {
			break
		}
		if seen[col.Continue] {
			return nil, protocolError(path, "the continue token repeated")
		}
		seen[col.Continue] = true
		token = col.Continue
	}
	if *total != raw {
		return nil, collectionChanged(path, fmt.Sprintf("count %d, read %d", *total, raw))
	}
	return rows, nil
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
