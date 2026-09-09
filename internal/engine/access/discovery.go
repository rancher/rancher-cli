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

// steveClustersPath is the steve collection of management Clusters. Unlike
// the Kubernetes-style LIST through the proxy, which Rancher refuses for
// every caller who is not an administrator, this collection answers with
// exactly the clusters the caller's access set grants. It is the one steve
// read the access suite makes; nothing else in the suite reads steve.
const steveClustersPath = "/v1/management.cattle.io.clusters"

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

// steveCollection is the part of a steve collection response the reader
// consumes. count is the collection total before paging and must be on
// every page and agree across them; continue is a per-page token, opaque
// here; revision is the cache's resourceVersion for the page.
type steveCollection struct {
	Data     []steveClusterItem `json:"data"`
	Count    *int               `json:"count"`
	Continue string             `json:"continue"`
	Revision string             `json:"revision"`
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

// listClusters reads the caller's clusters from steve. The read is retried
// once as a whole when its pages disagree with each other (a revision that
// moves, a count that does not match what was read, a repeated item that
// does not agree with itself); a second disagreement is the retryable
// collectionChanged error. Malformed items and responses are terminal on
// the first read. The local cluster is exactly one row: taken from the
// collection when present, inserted otherwise.
func listClusters(ctx context.Context, root engine.RootClient) ([]discoveredCluster, error) {
	var lastErr error
	for attempt := range 2 {
		rows, err := readClustersOnce(ctx, root)
		if err == nil {
			return withLocal(rows), nil
		}
		ce, ok := errors.AsType[*clierr.Error](err)
		if !ok || !ce.Retryable || attempt == 1 {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func readClustersOnce(ctx context.Context, root engine.RootClient) ([]discoveredCluster, error) {
	var (
		rows     []discoveredCluster
		byID     = map[string]discoveredCluster{}
		raw      int
		total    *int
		revision string
		seen     = map[string]bool{}
		token    string
	)
	for page := 0; ; page++ {
		if page == steveMaxPages {
			return nil, protocolError(steveClustersPath, fmt.Sprintf("more than %d pages", steveMaxPages))
		}
		path := steveClustersPath
		if token != "" {
			path += "?continue=" + url.QueryEscape(token)
		}
		body, err := root.GetJSON(ctx, path)
		if err != nil {
			return nil, err
		}
		var col steveCollection
		if err := json.Unmarshal(body, &col); err != nil {
			return nil, protocolError(steveClustersPath, "not a steve collection: "+err.Error())
		}
		if col.Revision == "" {
			return nil, protocolError(steveClustersPath, "a page without a revision")
		}
		if col.Count == nil || *col.Count < 0 {
			return nil, protocolError(steveClustersPath, "a page without a count")
		}
		if page == 0 {
			revision = col.Revision
			total = col.Count
		} else {
			if col.Revision != revision {
				return nil, collectionChanged(steveClustersPath, "revision moved between pages")
			}
			if *col.Count != *total {
				return nil, collectionChanged(steveClustersPath, fmt.Sprintf("count moved between pages (%d, then %d)", *total, *col.Count))
			}
		}
		for i, item := range col.Data {
			if item.ID == "" {
				return nil, protocolError(steveClustersPath, fmt.Sprintf("item %d has no id", raw+i))
			}
			if item.Metadata.Name != "" && item.Metadata.Name != item.ID {
				return nil, protocolError(steveClustersPath,
					fmt.Sprintf("item %q names itself %q", item.ID, item.Metadata.Name))
			}
			row := discoveredCluster{ID: item.ID, DisplayName: item.Spec.DisplayName, Returned: true}
			if prev, dup := byID[item.ID]; dup {
				if prev != row {
					return nil, collectionChanged(steveClustersPath, "repeated item "+item.ID+" disagrees with itself")
				}
				continue
			}
			byID[item.ID] = row
			rows = append(rows, row)
		}
		raw += len(col.Data)
		if col.Continue == "" {
			break
		}
		if seen[col.Continue] {
			return nil, protocolError(steveClustersPath, "the continue token repeated")
		}
		seen[col.Continue] = true
		token = col.Continue
	}
	if *total != raw {
		return nil, collectionChanged(steveClustersPath, fmt.Sprintf("count %d, read %d", *total, raw))
	}
	slices.SortFunc(rows, func(a, b discoveredCluster) int { return strings.Compare(a.ID, b.ID) })
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
