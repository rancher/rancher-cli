package testrancher

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// fieldManager repeats production's kube.FieldManager as a literal, so a
// change to that name fails the tests instead of moving this check with it.
const fieldManager = "cli.rancher.io"

// maxReportedWrites bounds how many writes FailOnWritesWithoutFieldManager
// lists.
const maxReportedWrites = 20

// unmanagedWrite is one POST, PUT or PATCH to a Kubernetes path that arrived
// at a Facade without the field manager query parameter.
type unmanagedWrite struct {
	facade       *Facade
	method, path string
}

var (
	unmanagedMu     sync.Mutex
	unmanagedWrites []unmanagedWrite
)

// recordWriteWithoutFieldManager records r when it is a write to a Kubernetes
// path that names no field manager of cli.rancher.io. Every write the CLI
// sends to a Kubernetes API carries it, so an entry is a request the CLI
// built without it. A request from kubectl is skipped: kubectl sends its own
// manager or none, and its writes are not the CLI's (the exec plugin kubectl
// starts is the CLI, and its requests are checked). A Facade built with
// Options.WritesWithoutFieldManager records nothing.
func (f *Facade) recordWriteWithoutFieldManager(r *http.Request) {
	if f.opts.WritesWithoutFieldManager != "" {
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return
	}
	if !isKubernetesPath(r.URL.EscapedPath()) {
		return
	}
	if strings.HasPrefix(r.UserAgent(), "kubectl/") {
		return
	}
	if r.URL.Query().Get("fieldManager") == fieldManager {
		return
	}
	unmanagedMu.Lock()
	defer unmanagedMu.Unlock()
	unmanagedWrites = append(unmanagedWrites, unmanagedWrite{facade: f, method: r.Method, path: r.URL.RequestURI()})
}

// isKubernetesPath reports whether an escaped request path is a Kubernetes
// API path: /api/ or /apis/ at the root (a direct endpoint) or under
// /k8s/clusters/<id> (through Rancher's proxy).
func isKubernetesPath(path string) bool {
	if rest, ok := strings.CutPrefix(path, "/k8s/clusters/"); ok {
		_, after, found := strings.Cut(rest, "/")
		if !found {
			return false
		}
		path = "/" + after
	}
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/apis/")
}

// FailOnWritesWithoutFieldManager is the exit code for a TestMain whose
// tests ran with code: code itself, or 1 when any Facade received a write to
// a Kubernetes path without the field manager parameter, which it lists on
// w (up to 20). A test whose point is such a write builds its Facade with
// Options.WritesWithoutFieldManager.
func FailOnWritesWithoutFieldManager(code int, w io.Writer) int {
	unmanagedMu.Lock()
	entries := append([]unmanagedWrite(nil), unmanagedWrites...)
	unmanagedMu.Unlock()
	return failOnUnmanagedWrites(code, entries, w)
}

// failOnUnmanagedWrites is FailOnWritesWithoutFieldManager over a given list.
func failOnUnmanagedWrites(code int, entries []unmanagedWrite, w io.Writer) int {
	if len(entries) == 0 {
		return code
	}
	_, _ = fmt.Fprintf(w, "a test Facade received %d write(s) to a Kubernetes path without fieldManager=%s; build the request with kube.NewWriteRequest or set the parameter in the test helper:\n", len(entries), fieldManager)
	for i, e := range entries {
		if i == maxReportedWrites {
			break
		}
		_, _ = fmt.Fprintf(w, "  %s %s\n", e.method, e.path)
	}
	return 1
}

// WithFieldManager returns rawURL with the fieldManager query parameter
// added when a test sends method to it as a Kubernetes write: a POST, PUT or
// PATCH whose path is a Kubernetes API path (the check the Facade applies).
// A query the URL already has is kept, and any other request is returned as
// it is. Test helpers that write to a Facade by hand build their URL with
// it, as the CLI builds its writes with kube.NewWriteRequest.
func WithFieldManager(method, rawURL string) string {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if !isKubernetesPath(u.EscapedPath()) {
		return rawURL
	}
	if u.Query().Has("fieldManager") {
		return rawURL
	}
	param := "fieldManager=" + url.QueryEscape(fieldManager)
	if u.RawQuery == "" {
		u.RawQuery = param
	} else {
		u.RawQuery += "&" + param
	}
	return u.String()
}
