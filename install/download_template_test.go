package install_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// functionText returns the text of the named function from a script, as the
// lines from start through the first line equal to end.
func functionText(t *testing.T, file, start, end string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", file))
	require.NoError(t, err)
	var out []string
	in := false
	for l := range strings.SplitSeq(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if !in && strings.HasPrefix(l, start) {
			in = true
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
		if l == end {
			break
		}
	}
	require.NotEmpty(t, out, "%s: no function starting %q", file, start)
	return strings.Join(out, "\n")
}

var (
	quotedString   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	shellVariable  = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*|\$[0-9@*#?]`)
	psVariable     = regexp.MustCompile(`\$\([^()]*\)|\$[A-Za-z_][A-Za-z0-9_]*`)
	variableMarker = "\x00"
)

// templateFragments lists, in order, the literal text between the variables
// of every double-quoted string in text, skipping strings with no literal
// text (a bare variable) and the release metadata note, which each script
// words for the metadata status it keeps.
func templateFragments(text string, variable *regexp.Regexp) []string {
	var out []string
	for _, lit := range quotedString.FindAllString(text, -1) {
		lit = lit[1 : len(lit)-1]
		if strings.Contains(lit, "unreadable") {
			continue
		}
		for frag := range strings.SplitSeq(variable.ReplaceAllString(lit, variableMarker), variableMarker) {
			if frag != "" {
				out = append(out, frag)
			}
		}
	}
	return out
}

// TestDownloadErrorTemplatesMatchInBothScripts asserts install.sh and
// install.ps1 build the failed-download error and the retry warning from the
// same literal text, in the same order, so the two scripts print the same
// message for every status case. Only the transport error text that follows
// the clause differs: curl's in one, the .NET message in the other. It reads
// both files and needs no Windows host.
func TestDownloadErrorTemplatesMatchInBothScripts(t *testing.T) {
	t.Parallel()
	sh := templateFragments(
		functionText(t, "install.sh", "dl_fail() {", "}")+"\n"+
			functionText(t, "install.sh", "download_asset() {", "}"), shellVariable)
	ps := templateFragments(
		functionText(t, "install.ps1", "    function Get-AssetFailure", "    }")+"\n"+
			functionText(t, "install.ps1", "    function Save-Asset", "    }"), psVariable)
	require.NotEmpty(t, sh)
	assert.Contains(t, sh, "download failed: ")
	assert.Contains(t, sh, "); retrying once")
	assert.Equal(t, sh, ps)
}
