package token

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// renderedAsTheErrorPath renders err as an invocation against a config
// directory does after a command fails: expanded for the invocation, then
// written in format. It returns what was written.
func renderedAsTheErrorPath(t *testing.T, err error, format string) string {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	io := runtime.IO{RemedyDir: t.TempDir()}
	var out bytes.Buffer
	require.NoError(t, runtime.RenderErrorStyled(&out, format, io.ExpandError(ce), style.Style{}))
	return out.String()
}

// decodedStringsOf returns every string of a rendered json or yaml
// document, keys and values, as its reader sees them.
func decodedStringsOf(t *testing.T, document string) []string {
	t.Helper()
	var doc any
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc))
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// leaksMarker reports whether a rendered document holds one of the five
// characters of the remedy marker, as raw bytes or in a string it decodes to.
func leaksMarker(t *testing.T, document string) bool {
	t.Helper()
	return holdsMarkerRune(document) || slices.ContainsFunc(decodedStringsOf(t, document), holdsMarkerRune)
}

// decodedHolds reports whether a string of the rendered document contains
// text.
func decodedHolds(t *testing.T, document, text string) bool {
	t.Helper()
	return slices.ContainsFunc(decodedStringsOf(t, document), func(s string) bool { return strings.Contains(s, text) })
}

// TestADirectWaitFailureLeavesNoMarkerInStructuredOutput asserts the error a
// cancelled wait returns, with the fate of the token it had created, writes
// no character of the remedy marker to its json or yaml output, whose
// details payload has no mapping of its own. The failure of the delete is the
// request's, with its certificate failure.
func TestADirectWaitFailureLeavesNoMarkerInStructuredOutput(t *testing.T) {
	cert := tlsCert(t, "private-ca")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, unknownAuthorityFailure(cert)
	})}
	fresh := newFreshToken(CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-abc"}}, client, "https://r.example", "bearer")
	cleanup := fresh.cleanup(t.Context())
	require.Equal(t, ResultFailed, cleanup.Result)
	err := directWaitCancelled(context.Canceled, fresh, cleanup)
	for _, format := range []string{"json", "yaml"} {
		got := renderedAsTheErrorPath(t, err, format)
		assert.True(t, decodedHolds(t, got, "token-abc"), "%s output holds the token's name: %q", format, got)
		assert.False(t, leaksMarker(t, got), "%s output holds a character of the remedy marker: %q", format, got)
	}
}

// TestAFailedAuthDeleteTokenLeavesNoMarkerInStructuredOutput asserts the
// failure payload of auth delete-token, built from the report of a revoke
// whose request failed on the server's certificate, writes the plain remedy
// commands and no character of the remedy marker to its json and yaml
// output.
func TestAFailedAuthDeleteTokenLeavesNoMarkerInStructuredOutput(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	cert := tlsCert(t, "private-ca")
	_, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	notRevoked(t, rep, login.Ref.Name)
	err := clierr.New(clierr.CodeGeneric, "the token was not revoked").
		WithDetails(deleteTokenFailure{Deleted: []string{"k"}, Revoked: rep.Revoked, NotRevoked: rep.NotRevoked})
	for _, format := range []string{"json", "yaml"} {
		got := renderedAsTheErrorPath(t, err, format)
		assert.True(t, decodedHolds(t, got, "rancher config update-server 'prod' --cacert <file>"), "%s output holds the plain command: %q", format, got)
		assert.False(t, leaksMarker(t, got), "%s output holds a character of the remedy marker: %q", format, got)
	}
}
