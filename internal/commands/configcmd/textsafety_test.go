package configcmd

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

// hostileValue is the value planted at the path p: the path in angle brackets,
// then the hostile tail.
func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// plainValue is the value planted at a path whose rule keeps a newline out of
// it: the path in angle brackets alone.
func plainValue(p string) string { return "<" + p + ">" }

// textClasses classifies every text path of a stored type: the paths whose
// values the builder filters ("printed" and "filtered"), the paths whose values hold no newline by rule, the
// paths the builder does not print, and the paths a test sets by hand. Each
// entry of a map says why in a sentence.
type textClasses struct {
	printed    []string
	filtered   map[string]string
	rawByRule  map[string]string
	notPrinted map[string]string
	setByHand  map[string]string
}

// assertComplete asserts that every reason is non-empty and that the four
// classes together are exactly the text paths of typ.
func (c textClasses) assertComplete(t *testing.T, typ reflect.Type) {
	t.Helper()
	union := slices.Clone(c.printed)
	for _, m := range []map[string]string{c.filtered, c.rawByRule, c.notPrinted, c.setByHand} {
		for path, reason := range m {
			assert.NotEmpty(t, reason, "the reason for %s", path)
			union = append(union, path)
		}
	}
	assert.ElementsMatch(t, remedytest.TextPaths(typ), union, "every text field of %s is classified exactly once", typ)
}

// value is the planted text for the path p.
func (c textClasses) value(p string) string {
	if _, ok := c.rawByRule[p]; ok {
		return plainValue(p)
	}
	return hostileValue(p)
}

// shown is every path whose value a text prints: the printed and filtered
// paths and the paths whose values hold no newline by rule.
func (c textClasses) shown() []string {
	out := slices.Clone(c.printed)
	for p := range c.filtered {
		out = append(out, p)
	}
	for p := range c.rawByRule {
		out = append(out, p)
	}
	return out
}

// assertLineRule asserts that out holds nothing the writer filter removes and
// has no line that starts with the forged line after its leading spaces.
func assertLineRule(t *testing.T, out string) {
	t.Helper()
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

// storedRun is one text the recovery of a stored token takes: the kind of
// the tokens, the entry's URL and the server a record names.
type storedRun struct {
	name         string
	kind         clierr.TokenKind
	url          string
	recordServer string
}

var storedRuns = []storedRun{
	{"sequence", clierr.TokenKindExt, "https://rancher.invalid", ""},
	{"v3", clierr.TokenKindV3, "https://rancher.invalid", ""},
	{"no server", clierr.TokenKindExt, "", ""},
	{"unusable record server", clierr.TokenKindExt, "https://rancher.invalid", "not a url"},
}

func TestConfigStatusKeepsStoredTokenValuesOnTheirLines(t *testing.T) {
	t.Parallel()
	classes := textClasses{
		printed: []string{
			"CredentialExpiresAt", "TokenRef.Name",
			"SupersededTokens[].Ref.Name", "SupersededTokens[].ExpiresAt", "SupersededTokens[].LastAttempt.Error",
		},
		filtered: map[string]string{
			"TokenUserID":               "the token owner's user id as the server reported it, stored without a check, so it can hold a newline",
			"TokenAuthConfig":           "cut from the token's principal name on the server and stored without a check, so it can hold a newline",
			"SupersededTokens[].UserID": "the token owner's user id as the server reported it, stored without a check, so it can hold a newline",
		},
		rawByRule: map[string]string{
			"SupersededTokens[].SupersededAt": "a time the CLI wrote itself",
			"LoggedOutAt":                     "a time the CLI wrote itself",
		},
		notPrinted: map[string]string{
			"CA":                "the certificate is never printed",
			"CAFingerprint":     "the fingerprint is never printed",
			"CacheBindingID":    "internal state, never printed",
			"CacheWritesPaused": "printed by a separate section that reads the paused entries",
			"SupersededTokens[].LastAttempt.Ref.Kind": "the record's own reference is printed instead",
			"SupersededTokens[].LastAttempt.Ref.Name": "the record's own reference is printed instead",
			"SupersededTokens[].LastAttempt.TLSError": "carried in the structured error, never printed here",
		},
		setByHand: map[string]string{
			"Provenance":                            "a fixed value that makes the stored token the CLI's own",
			"TokenRef.Kind":                         "set to the kind of the run",
			"URL":                                   "set to the URL of the run",
			"SupersededTokens[].Ref.Kind":           "set to the kind of the run",
			"SupersededTokens[].Server":             "set to the server of the run",
			"SupersededTokens[].LastAttempt.Result": "a fixed value of the CLI",
		},
	}
	typ := reflect.TypeFor[config.Server]()
	classes.assertComplete(t, typ)

	var all strings.Builder
	for _, run := range storedRuns {
		t.Run(run.name, func(t *testing.T) {
			planted := remedytest.PlantText(t, typ, classes.value)
			s, ok := reflect.TypeAssert[config.Server](planted)
			require.True(t, ok)
			s.Provenance = clierr.ProvenanceDerived
			s.URL = run.url
			s.TokenRef.Kind = run.kind
			rec := &s.SupersededTokens[0]
			rec.Ref.Kind = run.kind
			rec.Server = run.recordServer
			rec.LastAttempt.Result = clierr.ResultFailed

			var b strings.Builder
			renderFields(entrySection("e", s), "  ", &b)
			text := b.String()
			assertLineRule(t, text)
			for p := range classes.notPrinted {
				assert.NotContains(t, text, "<"+p+">", "the text does not print %s", p)
			}
			all.WriteString(text)

			report := entryReport("e", s)
			current, ok := report["currentToken"].(map[string]any)
			require.True(t, ok)
			ref, ok := current["ref"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, s.TokenRef.Name, ref["name"], "json carries the stored name as it is")
			records, ok := report["supersededTokens"].([]map[string]any)
			require.True(t, ok)
			require.Len(t, records, 1)
			recordRef, ok := records[0]["ref"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, rec.Ref.Name, recordRef["name"], "json carries the record's name as it is")

			assert.Equal(t, s.TokenUserID, report["tokenUserId"], "json carries the stored owner as it is")
			assert.Equal(t, s.TokenAuthConfig, report["tokenAuthConfig"], "json carries the stored auth config as it is")
			assert.Equal(t, s.CredentialExpiresAt, report["credentialExpiresAt"], "json carries the stored expiry as it is")
			assert.Equal(t, s.TokenUserID, current["userId"], "json carries the current token's owner as it is")
			assert.Equal(t, s.CredentialExpiresAt, current["expiresAt"], "json carries the current token's expiry as it is")
			assert.Equal(t, rec.UserID, records[0]["userId"], "json carries the record's owner as it is")
			assert.Equal(t, rec.ExpiresAt, records[0]["expiresAt"], "json carries the record's expiry as it is")
			attempt, ok := records[0]["lastAttempt"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, rec.LastAttempt.Error, attempt["error"], "json carries the revoke error as it is")
		})
	}
	for _, p := range classes.shown() {
		assert.Contains(t, all.String(), "<"+p+">", "a text prints %s", p)
	}
}

func TestTokenRecordLinesKeepStoredTokenValuesOnTheirLines(t *testing.T) {
	t.Parallel()
	classes := textClasses{
		printed: []string{"Ref.Name", "LastAttempt.Error"},
		filtered: map[string]string{
			"UserID": "the token owner's user id as the server reported it, stored without a check, so it can hold a newline",
		},
		rawByRule: map[string]string{
			"SupersededAt": "a time the CLI wrote itself",
		},
		notPrinted: map[string]string{
			"ExpiresAt":            "the record header does not print it",
			"LastAttempt.Ref.Kind": "the record's own reference is printed instead",
			"LastAttempt.Ref.Name": "the record's own reference is printed instead",
			"LastAttempt.TLSError": "carried in the structured error, never printed here",
		},
		setByHand: map[string]string{
			"Ref.Kind":           "set to the kind of the run",
			"Server":             "set to the server of the run",
			"LastAttempt.Result": "a fixed value of the CLI",
		},
	}
	typ := reflect.TypeFor[clierr.SupersededToken]()
	classes.assertComplete(t, typ)

	var all strings.Builder
	for _, run := range storedRuns {
		t.Run(run.name, func(t *testing.T) {
			planted := remedytest.PlantText(t, typ, classes.value)
			rec, ok := reflect.TypeAssert[clierr.SupersededToken](planted)
			require.True(t, ok)
			rec.Ref.Kind = run.kind
			rec.Server = run.recordServer
			rec.LastAttempt.Result = clierr.ResultFailed

			s := config.Server{URL: run.url, TokenUserID: "u-1", SupersededTokens: []config.SupersededToken{rec}}
			text := strings.Join(recordLines("e", s, "  "), "\n") + "\n"
			assertLineRule(t, text)
			for p := range classes.notPrinted {
				assert.NotContains(t, text, "<"+p+">", "the text does not print %s", p)
			}
			all.WriteString(text)
		})
	}
	for _, p := range classes.shown() {
		assert.Contains(t, all.String(), "<"+p+">", "a text prints %s", p)
	}
}
