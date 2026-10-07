package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrAllowanceExceeded is returned by GetJSON once a source has used its
// allowance, and by every call on a zero Connection; nothing is sent and
// the refusal is remembered.
var ErrAllowanceExceeded = errors.New("completion: request allowance exceeded")

// ErrEmptyPath is returned by GetJSON for a zero Path, which no builder
// produced; nothing is sent and nothing is counted.
var ErrEmptyPath = errors.New("completion: empty path")

// Connection is the surface-owned request capability a source uses. It
// holds the HTTP client and the bearer token privately, so a source can
// neither send the token elsewhere nor bypass the surface's CA, proxy,
// redirect and deadline policy. Every request counts against the granted
// allowance, and a refusal is remembered: from then on every call fails
// and the surface discards the whole Result. The zero value fails closed.
type Connection struct {
	client    *http.Client
	token     string
	serverURL string
	idOnly    bool
	limit     int
	trace     TraceFunc
	projects  ProjectLister
	clusters  ClusterLister

	mu       sync.Mutex
	used     int
	exceeded bool
}

// TraceFunc receives one line of the surface's completion trace per
// request sent: the server-relative path, the HTTP status and body size
// (zero when the request failed before a response), the elapsed time,
// and the transport error if any. It never sees the token. The
// Connection serializes its calls: a source fanning requests out over
// several goroutines (Parallel) still reaches the hook one call at a
// time.
type TraceFunc func(path string, status, size int, elapsed time.Duration, err error)

// SetTrace installs the surface's trace hook; nil turns tracing off.
func (c *Connection) SetTrace(fn TraceFunc) { c.trace = fn }

// NewConnection is the surface's constructor and the only way to a usable
// Connection. limit is the number of requests the surface granted for the
// claim; the mapping from an Allowance to its number lives in the surface.
func NewConnection(client *http.Client, token, serverURL string, idOnly bool, limit int) *Connection {
	return &Connection{client: client, token: token, serverURL: serverURL, idOnly: idOnly, limit: limit}
}

// IDOnly reports an exec-credential-only token: management listings are
// refused by the surface before a source runs, and cluster values are
// ids, never display names.
func (c *Connection) IDOnly() bool { return c.idOnly }

// Exceeded reports whether the source asked for more than its allowance.
// The surface reads it after Complete and drops the Result when set.
func (c *Connection) Exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exceeded
}

// Used reports how many requests the connection has sent.
func (c *Connection) Used() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// GetJSON issues one authenticated GET for p and decodes the JSON body
// into dst. accept is the Accept header, "" for application/json. A
// request past the allowance returns ErrAllowanceExceeded without
// sending; a non-2xx status and an undecodable body are errors.
func (c *Connection) GetJSON(ctx context.Context, p Path, accept string, dst any) error {
	if p.path == "" {
		return ErrEmptyPath
	}
	if !c.take() {
		return ErrAllowanceExceeded
	}
	if accept == "" {
		accept = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.serverURL+p.path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", accept)
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		c.traceRequest(p.path, 0, 0, time.Since(start), err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.traceRequest(p.path, resp.StatusCode, 0, time.Since(start), err)
		return err
	}
	c.traceRequest(p.path, resp.StatusCode, len(body), time.Since(start), nil)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, dst)
}

// traceRequest reports one request to the trace hook when one is set,
// under the connection's mutex so concurrent senders reach the hook one
// at a time.
func (c *Connection) traceRequest(path string, status, size int, elapsed time.Duration, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trace != nil {
		c.trace(path, status, size, elapsed, err)
	}
}

// take consumes one request from the allowance, or marks the connection
// exceeded and reports false. A zero Connection has no client and no
// allowance, so it is exceeded on the first call.
func (c *Connection) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.exceeded || c.used >= c.limit {
		c.exceeded = true
		return false
	}
	c.used++
	return true
}
