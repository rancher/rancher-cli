package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// decodedStrings returns every string of a rendered json or yaml document,
// keys and values, as its reader sees them; nil when the text is not one
// document.
func decodedStrings(text string) []string {
	var doc any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
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

// holdsMarkerRuneDecoded reports whether text holds one of the five
// characters as raw bytes or, when text is a json or yaml document, in a
// string it decodes to: an escape such as \u2063 in the bytes is found too.
func holdsMarkerRuneDecoded(text string) bool {
	return holdsMarkerRune(text) || slices.ContainsFunc(decodedStrings(text), holdsMarkerRune)
}

// notRevokedFixture is a token that may still be live, as a revoke that
// failed on the server's certificate reports it: the recovery commands are
// the ones the CLI writes for a token of its kind.
func notRevokedFixture() token.NotRevokedToken {
	ref := token.TokenRef{Kind: token.TokenKindExt, Name: "token-abc"}
	list, revoke := clierr.RecoveryCommands(ref)
	return token.NotRevokedToken{
		Ref: &ref, Source: "login", Server: "https://r.example", Entry: "prod", Reason: "failed",
		Code: clierr.CodeTLSTrust, Error: "the server presented a certificate signed by an unknown authority",
		Retry: "manual", Recovery: []string{list, revoke},
	}
}

// TestRecordOfRevokeReportsInStructuredOutput is a record, not a guard: the
// two details payloads that carry what a revoke did with each token have no
// mapping of their own, and the literals here hold plain commands from
// clierr.RecoveryCommands, so no change to the code that builds these reports
// makes it fail. It records that the recovery commands are written as plain
// commands, with no character of the remedy marker (U+2060 to U+2064) in the
// decoded json and yaml, when the pipeline expands the error for an
// invocation against a config directory. The guard for real reports is
// internal/token's TestAFailedAuthDeleteTokenLeavesNoMarkerInStructuredOutput.
func TestRecordOfRevokeReportsInStructuredOutput(t *testing.T) {
	t.Parallel()
	notRevoked := notRevokedFixture()
	for name, details := range map[string]clierr.TextDetails{
		"the report of a revoke": token.RevocationReport{NotRevoked: []token.NotRevokedToken{notRevoked}},
		"a partial logout of every server": auth.LogoutAllPartialFailure{
			LoggedOut: []string{"prod"}, NotRevoked: []token.NotRevokedToken{notRevoked},
		},
	} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(name+" "+format, func(t *testing.T) {
				t.Parallel()
				ce := clierr.New(clierr.CodeGeneric, "stopped before every token was revoked").WithDetails(details)
				io := runtime.IO{RemedyDir: t.TempDir()}
				var out bytes.Buffer
				require.NoError(t, runtime.RenderErrorStyled(&out, format, io.ExpandError(ce), style.Style{}))
				decoded := decodedStrings(out.String())
				assert.Contains(t, decoded, "rancher get tokens", "the recovery commands reach the output")
				assert.False(t, holdsMarkerRuneDecoded(out.String()), "the output holds a character of the remedy marker: %q", out.String())
			})
		}
	}
}
