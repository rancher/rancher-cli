package token

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// renderDirectWaitDetails is the default-format error text of an error
// holding details d.
func renderDirectWaitDetails(t *testing.T, d clierr.TextDetails) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, runtime.RenderError(&b, "", clierr.New(clierr.CodeGeneric, "m").WithDetails(d)))
	return b.String()
}

// TestDirectWaitDetailsTextIsUnchangedForSafeValues asserts the
// default-format text of the cancelled-wait payload with safe values, for
// each optional part.
func TestDirectWaitDetailsTextIsUnchangedForSafeValues(t *testing.T) {
	t.Parallel()
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "tok-1"}
	for _, c := range []struct {
		name    string
		details directWaitDetails
		want    string
	}{
		{"revoked, scoped, with a name", directWaitDetails{Fate: &clierr.TokenFate{Ref: ref, Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultRevoked}}},
			"error (GENERIC): m\n  cluster token cleanup: revoked (ext/tok-1)\n"},
		{"failed, unscoped, with a name and an error", directWaitDetails{
			Fate:     &clierr.TokenFate{Ref: ref, Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "denied"}},
			unscoped: true,
		}, "error (GENERIC): m\n  token cleanup: failed (ext/tok-1): denied\n"},
		{"failed with an error and no name", directWaitDetails{Fate: &clierr.TokenFate{Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "denied"}}},
			"error (GENERIC): m\n  cluster token cleanup: failed: denied\n"},
		{"no cleanup", directWaitDetails{Fate: &clierr.TokenFate{Ref: ref}}, "error (GENERIC): m\n"},
		{"no fate", directWaitDetails{}, "error (GENERIC): m\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, renderDirectWaitDetails(t, c.details))
		})
	}
}

// TestDirectWaitDetailsTextIsEscapedAndNothingIsRemoved asserts the
// cancelled-wait payload's default-format text shows each value that holds a
// hidden character as a double-quoted string with escapes, in place; that two
// values that differ only by a hidden character print differently; that safe
// values print as the plain join of the line; and that json and yaml hold
// each value exactly. The conformance table of details payloads names this
// test for the type, which it cannot build.
func TestDirectWaitDetailsTextIsEscapedAndNothingIsRemoved(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[directWaitDetails]()
	paths := remedytest.TextPaths(typ)
	shownPaths := []string{"Fate.Ref.Kind", "Fate.Ref.Name", "Fate.Cleanup.Result", "Fate.Cleanup.Error"}
	const neverPrinted = "the text never prints it"
	notShown := map[string]string{
		"Fate.Outcome":                 neverPrinted,
		"Fate.Kind":                    neverPrinted,
		"Fate.CreatedAt":               neverPrinted,
		"Fate.ExpiresAt":               neverPrinted,
		"Fate.Cleanup.Ref.Kind":        neverPrinted,
		"Fate.Cleanup.Ref.Name":        neverPrinted,
		"Fate.Cleanup.TLSError":        neverPrinted,
		"Fate.Search.AuthProvider":     neverPrinted,
		"Fate.Search.HandoffAt":        neverPrinted,
		"Fate.Search.PollEndedAt":      neverPrinted,
		"Fate.Search.RequestExpiresAt": neverPrinted,
	}
	for _, p := range shownPaths {
		require.Contains(t, paths, p)
	}
	for _, p := range paths {
		_, listed := notShown[p]
		assert.True(t, slices.Contains(shownPaths, p) != listed, "%s is listed as shown or as not shown, once", p)
	}
	for p := range notShown {
		assert.Contains(t, paths, p, "the listed path %s is a text path of the type", p)
	}
	plant := func(value func(path string) string) directWaitDetails {
		d, ok := reflect.TypeAssert[directWaitDetails](remedytest.PlantText(t, typ, value))
		require.True(t, ok)
		return d
	}
	safe := func(path string) string { return "<" + path + ">" }
	const hidden = "a\x1b\u202e\u200b\u2028b"
	unsafe := func(path string) string { return "<" + path + ">" + hidden }

	safeText := renderDirectWaitDetails(t, plant(safe))
	for _, p := range shownPaths {
		assert.Contains(t, safeText, safe(p))
		assert.NotContains(t, safeText, strconv.Quote(safe(p)), "%s is not quoted when it holds nothing hidden", p)
	}
	for p := range notShown {
		assert.NotContains(t, safeText, safe(p), "the listed path %s never shows", p)
	}
	var want strings.Builder
	want.WriteString("error (GENERIC): m\n")
	for _, l := range plant(safe).TextDetails() {
		want.WriteString("  " + l.String() + "\n")
	}
	assert.Equal(t, want.String(), safeText)

	unsafeText := renderDirectWaitDetails(t, plant(unsafe))
	assert.Negative(t, strings.IndexFunc(strings.ReplaceAll(unsafeText, "\n", ""), clierr.IsUnsafeToDisplay), "no hidden rune is printed: %q", unsafeText)
	for _, p := range shownPaths {
		assert.Contains(t, unsafeText, strconv.Quote(unsafe(p)), p)
		assert.NotContains(t, unsafeText, clierr.WithoutUnsafeToDisplay(unsafe(p)), p)
		pair := func(tail string) func(path string) string {
			return func(path string) string {
				if path == p {
					return "<" + path + ">" + tail
				}
				return safe(path)
			}
		}
		assert.NotEqual(t, renderDirectWaitDetails(t, plant(pair("ab"))), renderDirectWaitDetails(t, plant(pair("a\u200bb"))),
			"two values of %s that differ only by a hidden character print differently", p)
	}

	planted := plant(unsafe)
	raw, err := json.Marshal(planted)
	require.NoError(t, err)
	var wantDetails any
	require.NoError(t, json.Unmarshal(raw, &wantDetails))
	for format, decode := range map[string]func([]byte, any) error{
		"json": json.Unmarshal,
		"yaml": func(b []byte, v any) error { return yaml.Unmarshal(b, v) },
	} {
		var got struct {
			Data struct {
				Error struct {
					Details any `json:"details"`
				} `json:"error"`
			} `json:"data"`
		}
		var out bytes.Buffer
		require.NoError(t, runtime.RenderError(&out, format, clierr.New(clierr.CodeGeneric, "m").WithDetails(planted)))
		require.NoError(t, decode(out.Bytes(), &got))
		assert.Equal(t, wantDetails, got.Data.Error.Details, "%s keeps every value exactly", format)
	}
}
