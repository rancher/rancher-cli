package testrancher

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writesOf returns the writes without a field manager that f received.
func writesOf(f *Facade) []unmanagedWrite {
	unmanagedMu.Lock()
	defer unmanagedMu.Unlock()
	var out []unmanagedWrite
	for _, e := range unmanagedWrites {
		if e.facade == f {
			out = append(out, e)
		}
	}
	return out
}

// forgetWrites removes the writes f received and leaves every other
// Facade's: the tests of a package run in parallel, and the package's
// TestMain fails on any write left in the list.
func forgetWrites(f *Facade) {
	unmanagedMu.Lock()
	defer unmanagedMu.Unlock()
	unmanagedWrites = slices.DeleteFunc(unmanagedWrites, func(e unmanagedWrite) bool { return e.facade == f })
}

func send(f *Facade, method, target, userAgent string) {
	req := httptest.NewRequest(method, target, nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	f.ServeHTTP(httptest.NewRecorder(), req)
}

// TestFacadeRecordsAKubernetesWriteWithoutTheFieldManager asserts a Facade
// records a POST, PUT or PATCH to a Kubernetes path that lacks the
// fieldManager parameter, and records nothing for a write that has it, a
// read, a delete, or a path that is not a Kubernetes API. Removing the
// recording from ServeHTTP, or recording every request, fails it.
func TestFacadeRecordsAKubernetesWriteWithoutTheFieldManager(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, target string
		recorded             bool
	}{
		{"a POST through the proxy", http.MethodPost, "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens", true},
		{"a PUT through the proxy", http.MethodPut, "/k8s/clusters/c-1/api/v1/namespaces/x", true},
		{"a PATCH on the direct endpoint", http.MethodPatch, "/apis/authentication.k8s.io/v1/selfsubjectreviews", true},
		{"a POST with another query", http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews?dryRun=All", true},
		{"a POST with another manager", http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews?fieldManager=other", true},
		{"a POST with an escaped cluster id", http.MethodPost, "/k8s/clusters/c%2Fx/apis/authentication.k8s.io/v1/selfsubjectreviews", true},
		{"a POST with the manager", http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews?fieldManager=cli.rancher.io", false},
		{"a GET", http.MethodGet, "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens", false},
		{"a DELETE", http.MethodDelete, "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens/t-1", false},
		{"a Rancher login", http.MethodPost, "/v1-public/login", false},
		{"a Norman create", http.MethodPost, "/v3/tokens", false},
		{"a steve write", http.MethodPost, "/k8s/clusters/local/v1/namespaces", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := New(Options{})
			t.Cleanup(func() { forgetWrites(f) })
			send(f, tc.method, tc.target, "")
			got := writesOf(f)
			if !tc.recorded {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, tc.method, got[0].method)
			assert.Equal(t, tc.target, got[0].path)
		})
	}
}

// TestFacadeSkipsAWriteFromKubectl asserts a write whose User-Agent starts
// with kubectl/ is not recorded while the same write from another client is.
// Removing the User-Agent check fails it.
func TestFacadeSkipsAWriteFromKubectl(t *testing.T) {
	t.Parallel()
	f := New(Options{})
	t.Cleanup(func() { forgetWrites(f) })
	const target = "/k8s/clusters/c-1/api/v1/namespaces/x/configmaps"
	send(f, http.MethodPost, target, "kubectl/v1.37.0 (darwin/arm64) kubernetes/abc")
	assert.Empty(t, writesOf(f))
	send(f, http.MethodPost, target, "rancher/v1.0.0 (darwin/arm64)")
	assert.Len(t, writesOf(f), 1, "the exec plugin kubectl starts is the CLI")
}

// TestFacadeRecordsNothingWithTheExplicitOption asserts a Facade built with
// Options.WritesWithoutFieldManager records no write. Ignoring the option
// fails it.
func TestFacadeRecordsNothingWithTheExplicitOption(t *testing.T) {
	t.Parallel()
	f := New(Options{WritesWithoutFieldManager: "the test sends a write without a field manager on purpose"})
	t.Cleanup(func() { forgetWrites(f) })
	send(f, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", "")
	assert.Empty(t, writesOf(f))
}

// TestForgetWritesRemovesOnlyOneFacadesWrites asserts the test hook leaves
// the writes of every other Facade in the list. A hook that clears the whole
// list fails it.
func TestForgetWritesRemovesOnlyOneFacadesWrites(t *testing.T) {
	t.Parallel()
	a, b := New(Options{}), New(Options{})
	t.Cleanup(func() { forgetWrites(a); forgetWrites(b) })
	send(a, http.MethodPost, "/apis/x/v1/things", "")
	send(b, http.MethodPost, "/apis/x/v1/things", "")
	forgetWrites(a)
	assert.Empty(t, writesOf(a))
	assert.Len(t, writesOf(b), 1)
}

// TestFailOnUnmanagedWrites asserts the exit code and the listing: the code
// unchanged with no writes, 1 and the method and path of one write, and at
// most 20 lines for 21 writes. Returning the code unchanged, or listing
// every write, fails it.
func TestFailOnUnmanagedWrites(t *testing.T) {
	t.Parallel()
	t.Run("no writes", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		assert.Equal(t, 0, failOnUnmanagedWrites(0, nil, &out))
		assert.Equal(t, 3, failOnUnmanagedWrites(3, nil, &out))
		assert.Empty(t, out.String())
	})
	t.Run("one write", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		code := failOnUnmanagedWrites(0, []unmanagedWrite{{method: "POST", path: "/apis/x/v1/things"}}, &out)
		assert.Equal(t, 1, code)
		assert.Contains(t, out.String(), "POST /apis/x/v1/things")
	})
	t.Run("twenty one writes", func(t *testing.T) {
		t.Parallel()
		entries := make([]unmanagedWrite, 21)
		for i := range entries {
			entries[i] = unmanagedWrite{method: "PUT", path: "/apis/x/v1/things"}
		}
		var out bytes.Buffer
		assert.Equal(t, 1, failOnUnmanagedWrites(0, entries, &out))
		listed := 0
		for line := range strings.Lines(out.String()) {
			if strings.HasPrefix(line, "  PUT ") {
				listed++
			}
		}
		assert.Equal(t, 20, listed)
	})
}

// TestWithFieldManager asserts the helper adds the parameter to a write to a
// Kubernetes API path (an escaped cluster id included), keeps an existing
// query, and returns anything else as it was, a steve path under /k8s/
// among them. A helper that adds nothing, adds it to a read, or tests only
// for the /k8s/ prefix fails it.
func TestWithFieldManager(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, method, in, want string }{
		{"a proxy POST", http.MethodPost, "http://h/k8s/clusters/local/apis/g/v1/r", "http://h/k8s/clusters/local/apis/g/v1/r?fieldManager=cli.rancher.io"},
		{"a direct PUT", http.MethodPut, "http://h/api/v1/namespaces/x", "http://h/api/v1/namespaces/x?fieldManager=cli.rancher.io"},
		{"a PATCH with a query", http.MethodPatch, "http://h/apis/g/v1/r?dryRun=All", "http://h/apis/g/v1/r?dryRun=All&fieldManager=cli.rancher.io"},
		{"the manager already named", http.MethodPost, "http://h/apis/g/v1/r?fieldManager=other", "http://h/apis/g/v1/r?fieldManager=other"},
		{"a GET", http.MethodGet, "http://h/apis/g/v1/r", "http://h/apis/g/v1/r"},
		{"a Rancher login", http.MethodPost, "http://h/v1-public/login", "http://h/v1-public/login"},
		{"a steve path under the proxy", http.MethodPost, "http://h/k8s/clusters/local/v1/management.cattle.io.projects", "http://h/k8s/clusters/local/v1/management.cattle.io.projects"},
		{"an escaped cluster id", http.MethodPost, "http://h/k8s/clusters/c-1%2Fx/apis/g/v1/r", "http://h/k8s/clusters/c-1%2Fx/apis/g/v1/r?fieldManager=cli.rancher.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, WithFieldManager(tc.method, tc.in))
		})
	}
}
