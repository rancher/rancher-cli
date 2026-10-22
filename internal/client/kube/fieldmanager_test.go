package kube

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFieldManagerIsTheCLIDomain asserts the manager name is the CLI's
// domain; the name outlives a binary rename, so changing it fails the test.
func TestFieldManagerIsTheCLIDomain(t *testing.T) {
	assert.Equal(t, "cli.rancher.io", FieldManager)
}

// TestNewWriteRequestAddsTheFieldManager asserts the parameter is appended
// to a URL with no query, appended after an existing query without
// reordering or re-encoding it, and left alone when the URL already names
// the CLI. The fragment and the escaped path stay as given. A function that
// adds nothing, drops or reorders a parameter, or decodes the path fails it.
func TestNewWriteRequestAddsTheFieldManager(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"no query", "https://h/apis/g/v1/r", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io"},
		{"an existing query", "https://h/apis/g/v1/r?z=1&a=%20b&dryRun=All", "https://h/apis/g/v1/r?z=1&a=%20b&dryRun=All&fieldManager=cli.rancher.io"},
		{"the same manager", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io"},
		{"the same manager among others", "https://h/apis/g/v1/r?b=2&fieldManager=cli.rancher.io&a=1", "https://h/apis/g/v1/r?b=2&fieldManager=cli.rancher.io&a=1"},
		{"a fragment", "https://h/apis/g/v1/r#frag", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io#frag"},
		{"a query and a fragment", "https://h/apis/g/v1/r?a=1#frag", "https://h/apis/g/v1/r?a=1&fieldManager=cli.rancher.io#frag"},
		{"an escaped path", "https://h/k8s/clusters/c-1%2Fx/apis/g/v1/r", "https://h/k8s/clusters/c-1%2Fx/apis/g/v1/r?fieldManager=cli.rancher.io"},
		{"an empty query mark", "https://h/apis/g/v1/r?", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := NewWriteRequest(t.Context(), "POST", tc.in, strings.NewReader("{}"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, req.URL.String())
			assert.Equal(t, "POST", req.Method)
		})
	}
}

// TestNewWriteRequestKeepsTheEscapedPath asserts the request goes out with
// the path as the caller escaped it, so a cluster id with a slash stays one
// path segment. A function that rebuilds the URL from decoded parts fails it.
func TestNewWriteRequestKeepsTheEscapedPath(t *testing.T) {
	t.Parallel()
	req, err := NewWriteRequest(t.Context(), "POST", "https://h/k8s/clusters/c-1%2Fx/apis/g/v1/r", nil)
	require.NoError(t, err)
	assert.Equal(t, "/k8s/clusters/c-1%2Fx/apis/g/v1/r", req.URL.EscapedPath())
	assert.Equal(t, "/k8s/clusters/c-1/x/apis/g/v1/r", req.URL.Path)
}

// TestNewWriteRequestRefusesAWrongManagerOrQuery asserts a URL that names a
// different, empty or repeated field manager, a query that cannot be
// decoded, or a URL that cannot be parsed is refused. A function that
// overwrites the value or adds a second parameter fails it.
func TestNewWriteRequestRefusesAWrongManagerOrQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"a different manager", "https://h/apis/g/v1/r?fieldManager=kubectl", `names the field manager "kubectl"`},
		{"an empty manager", "https://h/apis/g/v1/r?fieldManager=", `names the field manager ""`},
		{"a manager with no value", "https://h/apis/g/v1/r?fieldManager", `names the field manager ""`},
		{"two managers", "https://h/apis/g/v1/r?fieldManager=cli.rancher.io&fieldManager=cli.rancher.io", "names 2 field managers"},
		{"a query that cannot be decoded", "https://h/apis/g/v1/r?a=%zz", "reading the query"},
		{"a URL that cannot be parsed", "https://h/%zz", "parsing the Kubernetes write URL"},
		{"a URL with a bad host", "http://[::1", "parsing the Kubernetes write URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := NewWriteRequest(t.Context(), "POST", tc.in, nil)
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, req)
		})
	}
}

// TestNewWriteRequestMethods asserts POST, PUT and PATCH build a request and
// every other method is refused with an error naming it. A function that
// accepts DELETE, GET or an empty method fails it.
func TestNewWriteRequestMethods(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"POST", "PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req, err := NewWriteRequest(t.Context(), method, "https://h/apis/g/v1/r", nil)
			require.NoError(t, err)
			assert.Equal(t, method, req.Method)
			assert.Equal(t, FieldManager, req.URL.Query().Get("fieldManager"))
		})
	}
	for _, method := range []string{"GET", "DELETE", "HEAD", "", "post"} {
		t.Run("refuses "+method, func(t *testing.T) {
			t.Parallel()
			req, err := NewWriteRequest(t.Context(), method, "https://h/apis/g/v1/r", nil)
			require.ErrorContains(t, err, `not "`+method+`"`)
			assert.Nil(t, req)
		})
	}
}

// TestNewWriteRequestCarriesTheContextAndBody asserts the built request has
// the caller's context and body. A function that drops either fails it.
func TestNewWriteRequestCarriesTheContextAndBody(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	req, err := NewWriteRequest(ctx, "PATCH", "https://h/apis/g/v1/r", strings.NewReader("body"))
	require.NoError(t, err)
	assert.Equal(t, ctx, req.Context())
	require.NotNil(t, req.Body)
	assert.EqualValues(t, 4, req.ContentLength)
}
