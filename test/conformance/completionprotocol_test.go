package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secondCompletionProtocol matches what a completion path for lines without
// the protocol marker brings back: the parameter or flag that selected it,
// urfave's own completion of the raw argv (which only works when the word
// being completed is left on the line), and comments naming the scripts that
// sent such lines. Wording that names a protocol version is
// TestNoCompletionProtocolVersion's.
var secondCompletionProtocol = regexp.MustCompile(`(?i)\bhasMarker\b|\brawV1|DefaultCompleteWithFlags\(|\b(older|earlier|old installed) scripts?\b`)

// completionProtocolVersionName matches, in any letter case, "v1" or "v2"
// joined to a word of the completion code, on either side, directly or by
// "_" or "-" (v1Lines, listV1Lines, rawProtoV2, runCompletionV2,
// v2-protocol). An API version joined to other words (corev1,
// CoreV1Interface) or after a slash (/v1) is left alone.
var completionProtocolVersionName = regexp.MustCompile(`(?i)` +
	`(proto|protocol|completion|argv|raw|word|line|lines|script|marker|sentinel|cur)[-_]?v[12](\D|$)` +
	`|v[12][-_]?(lines?|requests?|marker|proto|protocol|completion|scripts?|sentinel|words?|cur|argv)`)

// completionProtocolVersionText matches a version in text: "protocol v2",
// "protocol version 2", "v1" or "v2" before or after a completion word
// ("v1 protocol", "v2 line", "under v2"), "v1" or "v2" joined by "-" to any
// word ("v2-gated"), and a protocol number (proto=2).
// The same words name other protocols (the credential helper's "protocol
// v1") and API versions ("Rancher v2 line of clusters"), so a match counts
// only on a line that also holds a word of completionContext.
var completionProtocolVersionText = regexp.MustCompile(`(?i)` +
	`\bprotocol\s+(version\s+)?v?[12]\b` +
	`|(^|[^/a-z0-9])v[12]\s+(protocol|proto|completion|scripts?|lines?|marker|sentinel|branch|vector|test|bug|bash|zsh|fish|pwsh)\b` +
	`|(^|[^/a-z0-9])v[12]-[a-z]` +
	`|\b(proto|completion|script|under)\s+v[12]\b` +
	`|\bproto\s*[=:]?\s*[12]\b`)

// completionContext matches a word that ties a line to shell completion.
var completionContext = regexp.MustCompile(`(?i)complet|\bscripts?\b|\btab\b|<tab>|\bmarker\b|\bsentinel\b|\bshells?\b|current.?word|\bcandidates?\b|\b(bash|zsh|fish|pwsh)\b|\bcur\b|\bwords?\b|\bflags?\b|\bargv\b`)

// completionProtocolVersion returns the text in line that names a completion
// protocol version, or "". The marker --rancher-completion-proto=2 is
// removed from the line first.
func completionProtocolVersion(line string) string {
	line = strings.ReplaceAll(line, completionProtocolMarker, "")
	if m := completionProtocolVersionName.FindString(line); m != "" {
		return m
	}
	if m := completionProtocolVersionText.FindString(line); m != "" && completionContext.MatchString(line) {
		return m
	}
	return ""
}

// completionProtocolMarker is the one allowed spelling of the protocol number.
const completionProtocolMarker = "--rancher-completion-proto=2"

// completionNames lists, for each name that carries the completion protocol,
// the functions in internal/cli that may use it.
//
//   - protoSentinel (the marker) and completionFlag: where a line is
//     classified and rewritten for urfave.
//   - rawCompleting: where it is set, and completionActive, which reports it.
//   - completionActive: the Before hooks Adapt and adaptEntry install, which
//     pass it to checkFlagValues so the unset-variable guard stays silent
//     while a line is completed, and completionDebugEnabled, which keeps the
//     completion trace to completion requests, and resolveReadOnly, which
//     keeps a completion request from deciding whether to record a usage
//     event. None picks a way to complete; a completion callback
//     (universalShellCompleteFunc, aliasAwareGroupComplete, the root's,
//     completeCurrentWord) runs only for a marked line and must not ask.
//   - completionDebugEnabled: completionDebugf, which writes the trace.
//
// A use is counted against the top-level function that holds it, so a
// closure inside an allowed function counts as that function.
var completionNames = map[string][]string{
	"protoSentinel":    {"completionRequest", "SanitizedArgs", "CaptureArgv"},
	"completionFlag":   {"OutdatedCompletionScript", "SanitizedArgs", "CaptureArgv"},
	"rawCompleting":    {"completionActive", "CaptureArgv"},
	"completionActive": {"Adapt", "adaptEntry", "completionDebugEnabled", "resolveReadOnly"},
	// completionDebugEnabled carries completionActive, so it is watched too.
	"completionDebugEnabled": {"completionDebugf"},
}

// completionLiteral reports whether a Go string literal spells a word of the
// completion protocol, which only the constants in internal/cli/argtail.go
// may hold.
func completionLiteral(lit *ast.BasicLit) bool {
	if lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && (strings.Contains(s, "--rancher-completion-") || strings.Contains(s, "--generate-shell-completion"))
}

// inCompletionScope reports whether path is a Go file of the completion code
// or of the binary's entry point.
func inCompletionScope(path string) bool {
	return strings.HasSuffix(path, ".go") && (strings.HasPrefix(path, "internal/cli/") || strings.HasPrefix(path, "cmd/rancher/"))
}

// TestOneCompletionProtocol asserts that the completion code reads one
// protocol: no production Go file under internal/cli or cmd/rancher holds a
// path for completion lines without the protocol marker, only the functions
// in completionNames use the names that carry the protocol, and no string
// literal outside the constant declarations spells a protocol word. So no
// callback can branch on the marker, the completion flag or the request
// state itself. Such a line gets OutdatedCompletionScript and nothing else.
func TestOneCompletionProtocol(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	scanned := 0
	for _, path := range files {
		if !inCompletionScope(path) || strings.HasSuffix(path, "_test.go") || inTestdata(path) {
			continue
		}
		scanned++
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		n := 0
		for line := range strings.SplitSeq(string(src), "\n") {
			n++
			if m := secondCompletionProtocol.FindString(line); m != "" {
				t.Errorf("%s:%s: %q: completion reads only lines that open with the protocol marker; a line without it gets OutdatedCompletionScript", path, strconv.Itoa(n), m)
			}
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				// The function's own name is its declaration, not a use.
				checkCompletionUses(t, fset, decl.Type, decl.Name.Name, true)
				if decl.Body != nil {
					checkCompletionUses(t, fset, decl.Body, decl.Name.Name, true)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					// The constant declarations in argtail.go spell the
					// protocol words; a watched name in any value, an
					// alias constant included, is still a use.
					constants := decl.Tok == token.CONST && path == "internal/cli/argtail.go"
					for _, v := range value.Values {
						checkCompletionUses(t, fset, v, "", !constants)
					}
				}
			}
		}
	}
	require.NotZero(t, scanned, "no completion source files found")
}

// checkCompletionUses fails the test for every name of completionNames used
// in node outside the functions allowed to use it and, when literals is set,
// for every string literal that spells a protocol word; fn is the enclosing
// function, empty outside one.
func checkCompletionUses(t *testing.T, fset *token.FileSet, node ast.Node, fn string, literals bool) {
	t.Helper()
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			allowed, watched := completionNames[n.Name]
			if watched && !slices.Contains(allowed, fn) {
				t.Errorf("%s: %s is used in %q; only %s may use it", fset.Position(n.Pos()), n.Name, fn, strings.Join(allowed, ", "))
			}
		case *ast.BasicLit:
			if literals && completionLiteral(n) {
				t.Errorf("%s: %s in %q spells a completion protocol word; use the constants in internal/cli/argtail.go", fset.Position(n.Pos()), n.Value, fn)
			}
		}
		return true
	})
}

// TestNoCompletionProtocolVersion asserts that no name or comment in the
// completion code, its tests, the shipped scripts or the e2e completion
// scenario names a completion protocol version: there is one protocol, and a
// version implies another. The marker --rancher-completion-proto=2 is the
// one allowed match.
func TestNoCompletionProtocolVersion(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	scanned := 0
	for _, path := range files {
		script := strings.HasPrefix(path, "internal/cli/completions/") && (strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".ps1"))
		if !inCompletionScope(path) && !script && path != "test/e2e/scripts/completion.txtar" {
			continue
		}
		scanned++
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		n := 0
		for line := range strings.SplitSeq(string(src), "\n") {
			n++
			if m := completionProtocolVersion(line); m != "" {
				t.Errorf("%s:%s: %q names a completion protocol version; there is one protocol, marked by %s", path, strconv.Itoa(n), m, completionProtocolMarker)
			}
		}
	}
	require.NotZero(t, scanned, "no completion files found")
}

// TestCompletionProtocolVersionPattern asserts what the version rule
// matches: protocol versions in names and text, in any letter case, and
// never an API version or the marker.
func TestCompletionProtocolVersionPattern(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"var v1Lines = 0",
		"var V2Lines = 0",
		"var listV1Lines = 0",
		"rawProtoV2 = true",
		"func runCompletionV2() {}",
		"func TestCaptureArgvV2EmptyCurrentWord(t *testing.T) {}",
		"rawV1Completion bool",
		"// The protocol version 2 line opens with the marker.",
		"// Under the v1 protocol the line holds the word.",
		"// The v2 line carries the word.",
		"// protocol v2 reads argv",
		"// runCompletion runs one v2-protocol TAB for words",
		"// This confirms the v2-gated flag branch does NOT overwrite the listing.",
		"var v2_lines = 0",
		"// v2-lines end here.",
		"// the raw-v1 shape",
		"# Protocol v2: self-prefixed current-word field.",
		"# The v1 script sent no marker.",
		"// `get po<TAB>` under v2 must return the alias.",
		"// the REAL v2 script shape",
		"// bashProtoArgv runs the v2 bash script with the given words",
		"// SiteNameLike v2 bug: `usage --<TAB>` must emit long-form flag candidates",
		"// v2 test: words contain flag only (no partial value); currentWord is the partial.",
		"--rancher-completion-proto=1",
	} {
		assert.NotEmpty(t, completionProtocolVersion(line), line)
	}
	for _, line := range []string{
		`protoSentinel  = "--rancher-completion-proto=2"`,
		"// The Steve v1 request path carries the cluster id.",
		`path := "/v1/management.cattle.io.clusters"`,
		"// a /v1 protocol-level list call",
		`ns := corev1.Namespace{}`,
		`meta := metav1.ObjectMeta{}`,
		`const manifest = "apiVersion: v1\nkind: Namespace\n"`,
		"// GET /k8s/clusters/c-x/api/v1/namespaces",
		"// steve v1 lists the clusters",
		"// The exec credential helper speaks protocol v1.",
		"dec := json.NewDecoder(stdin) // lenient: unknown request fields are ignored per protocol v1",
		"// The credential helper protocol version 1 has no expiry field.",
		"// Rancher v2 line of clusters",
		"// CoreV1Interface lists the namespaces.",
		`body, err := rc.GetJSON(t.Context(), "/v1-rke2-release/release")`,
	} {
		assert.Empty(t, completionProtocolVersion(line), line)
	}
}
