package completion

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ErrNoLister is returned by a lister read (Projects) on a Connection the
// surface set no lister on: the read fails closed, like the zero
// Connection. A cluster lister added beside the project one returns it
// too.
var ErrNoLister = errors.New("completion: no lister")

// StatusError is a non-2xx answer to GetBody.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return "HTTP " + strconv.Itoa(e.Status) }

// ProjectItem is one project a lister read: the bare id (metadata.name)
// and the display name.
type ProjectItem struct{ ID, DisplayName string }

// ProjectLister reads the projects of a cluster through c, so every page
// counts against c's allowance. The surface sets it; a source only calls
// Connection.Projects.
type ProjectLister func(ctx context.Context, c *Connection, clusterID string) ([]ProjectItem, error)

// SetProjectLister installs the surface's project lister; nil removes it.
func (c *Connection) SetProjectLister(fn ProjectLister) { c.projects = fn }

// Projects returns the projects of clusterID as the surface's lister reads
// them, or ErrNoLister when none is set.
func (c *Connection) Projects(ctx context.Context, clusterID string) ([]ProjectItem, error) {
	if c.projects == nil {
		return nil, ErrNoLister
	}
	return c.projects(ctx, c, clusterID)
}

// GetBody issues one authenticated GET for p and returns the body. It
// counts against the allowance like GetJSON; a non-2xx answer is a
// *StatusError.
func (c *Connection) GetBody(ctx context.Context, p Path) ([]byte, error) {
	if p.path == "" {
		return nil, ErrEmptyPath
	}
	if !c.take() {
		return nil, ErrAllowanceExceeded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.serverURL+p.path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		c.traceRequest(p.path, 0, 0, time.Since(start), err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	c.traceRequest(p.path, resp.StatusCode, len(body), time.Since(start), err)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	return body, nil
}

// SteveNamespacedCollectionPath is a namespaced collection of Rancher's
// steve API at the server root: /v1/<resource>/<namespace> (for projects,
// the namespace is the cluster id). Both segments go through
// ValidSegment; a continuation token goes through WithQuery.
func SteveNamespacedCollectionPath(resource, namespace string) (Path, error) {
	return build([]string{"v1", resource, namespace})
}
