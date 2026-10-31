package skillinstall_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/skillinstall"
)

// decodedStrings returns every string of a rendered json or yaml document,
// keys and values, as its reader sees them.
func decodedStrings(t *testing.T, document string) []string {
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

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// leaksMarker reports whether a rendered document holds one of the five
// characters, as raw bytes or in a string it decodes to.
func leaksMarker(t *testing.T, document string) bool {
	t.Helper()
	return holdsMarkerRune(document) || slices.ContainsFunc(decodedStrings(t, document), holdsMarkerRune)
}

// TestRecordOfAPartialInstallReportInStructuredOutput is a record, not a
// guard: the details payload of a skill install that failed for part of
// its targets has no mapping of its own and no field that holds a command, so
// no change to the code that builds it makes this test fail. It records that
// the decoded json and yaml hold the outcomes and no character of the remedy
// marker (U+2060 to U+2064) when the pipeline renders the error for an
// invocation against a config directory.
func TestRecordOfAPartialInstallReportInStructuredOutput(t *testing.T) {
	t.Parallel()
	report := skillinstall.Report{Outcomes: []skillinstall.Outcome{
		{Target: skillinstall.Target{Harness: "claude", Dir: "/home/u/.claude/skills/rancher"}, Action: "changed", CLIVersion: "v1.0.0"},
		{Target: skillinstall.Target{Harness: "codex", Dir: "/home/u/.codex/skills/rancher"}, Action: "failed", Reason: "permission denied"},
	}}
	ce := clierr.New(clierr.CodeGeneric, "skill install failed for part of the selected targets").
		WithDetails(report).WithWarnings("see rancher skill status")
	io := runtime.IO{RemedyDir: t.TempDir()}
	for _, format := range []string{"json", "yaml"} {
		var out bytes.Buffer
		require.NoError(t, runtime.RenderErrorStyled(&out, format, io.ExpandError(ce), style.Style{}))
		assert.Contains(t, decodedStrings(t, out.String()), "permission denied", format)
		assert.False(t, leaksMarker(t, out.String()), "%s output holds a character of the remedy marker: %q", format, out.String())
	}
}
