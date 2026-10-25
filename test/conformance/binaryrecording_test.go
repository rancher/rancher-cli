package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// binaryRecordingSelf is this file, which names the identifiers the scan
// looks for and runs no binary.
const binaryRecordingSelf = "test/conformance/binaryrecording_test.go"

// telemetryOffText is the text a test file holds when it turns recording off
// for a binary it runs.
const telemetryOffText = "RANCHER_CLI_TELEMETRY=false"

// binaryRecordingAllowlist maps a test file that runs the built binary
// without turning recording off to the reason.
var binaryRecordingAllowlist = map[string]string{
	"internal/cli/completions/bash_execcommand_test.go":  "sends completion requests only; a completion request records nothing",
	"internal/cli/completions/doubledash_test.go":        "sends completion requests only; a completion request records nothing",
	"internal/cli/completions/pwsh_continuation_test.go": "sends completion requests only; a completion request records nothing",
	"internal/cli/completions/pwsh_exec_test.go":         "sends completion requests only; a completion request records nothing",
	"internal/cli/completions/pwsh_execcommand_test.go":  "sends completion requests only; a completion request records nothing",
}

// builtPackage reports whether a string literal names the main package of
// the binary: a path that ends with cmd/rancher once a trailing slash or an
// @version suffix is cut.
func builtPackage(lit string) bool {
	lit, _, _ = strings.Cut(lit, "@")
	return strings.HasSuffix(strings.TrimRight(lit, "/"), "cmd/rancher")
}

// joinsThePackage reports whether call is a filepath.Join or path.Join call
// whose last two arguments are the string literals "cmd" and "rancher".
func joinsThePackage(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || (pkg.Name != "filepath" && pkg.Name != "path") || len(call.Args) < 2 {
		return false
	}
	last := call.Args[len(call.Args)-2:]
	return stringLiteral(last[0]) == "cmd" && stringLiteral(last[1]) == "rancher"
}

// runsTheBinary reports whether call is an exec.Command or
// exec.CommandContext call that runs the built binary: one of its arguments
// holds the identifier binPath, or builds it, with a string literal that
// names cmd/rancher (builtPackage) or a Join call that spells it
// (joinsThePackage).
func runsTheBinary(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
		return false
	}
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				found = found || v.Name == "binPath"
			case *ast.BasicLit:
				found = found || builtPackage(stringLiteral(v))
			case *ast.CallExpr:
				found = found || joinsThePackage(v)
			}
			return !found
		})
	}
	return found
}

// turnsRecordingOff reports whether the file holds the string literal
// RANCHER_CLI_TELEMETRY=false, or a call with the two literals
// RANCHER_CLI_TELEMETRY and false (Setenv).
func turnsRecordingOff(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil && s == telemetryOffText {
					found = true
				}
			}
		case *ast.CallExpr:
			if len(v.Args) == 2 && stringLiteral(v.Args[0]) == "RANCHER_CLI_TELEMETRY" && stringLiteral(v.Args[1]) == "false" {
				found = true
			}
		}
		return !found
	})
	return found
}

// stringLiteral returns the text of a string literal expression, or "".
func stringLiteral(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, _ := strconv.Unquote(lit.Value)
	return s
}

// binaryRecordingFindings parses the test files in sources (path to text)
// and returns one line per file that runs the built binary without turning
// recording off and without an allowlist row, and per allowlist row that
// names a file that does not run the binary or that turns recording off.
func binaryRecordingFindings(t *testing.T, sources map[string]string, allow map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var out []string
	runs := map[string]bool{}
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err, path)
		ran := false
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && runsTheBinary(call) {
				ran = true
			}
			return !ran
		})
		if !ran {
			continue
		}
		runs[path] = true
		off := turnsRecordingOff(f)
		_, allowed := allow[path]
		switch {
		case !off && !allowed:
			out = append(out, path+": runs the built binary and does not set "+telemetryOffText)
		case off && allowed:
			out = append(out, path+": has an allowlist row and sets "+telemetryOffText)
		}
	}
	for path := range allow {
		if !runs[path] {
			out = append(out, path+": has an allowlist row and does not run the built binary")
		}
	}
	slices.Sort(out)
	return out
}

// TestEveryTestThatRunsTheBinaryTurnsRecordingOff asserts each test file that
// runs the built binary sets RANCHER_CLI_TELEMETRY=false or has an allowlist
// row with a reason, and that the e2e harness is among them.
func TestEveryTestThatRunsTheBinaryTurnsRecordingOff(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") || inTestdata(path) || path == binaryRecordingSelf {
			continue
		}
		data, rerr := os.ReadFile(path)
		require.NoError(t, rerr, path)
		sources[path] = string(data)
	}
	require.NotEmpty(t, sources)
	assert.Empty(t, binaryRecordingFindings(t, sources, binaryRecordingAllowlist))
	for path, reason := range binaryRecordingAllowlist {
		assert.NotEmpty(t, reason, path)
	}
	harness, ok := sources["test/e2e/harness_test.go"]
	require.True(t, ok)
	assert.Contains(t, harness, `e.Setenv("RANCHER_CLI_TELEMETRY", "false")`)
}

// TestBinaryRecordingScanCatchesFixtures asserts the scan reports a new
// site that runs the binary without the setting, a stale allowlist row for a
// file that sets it and for a file that runs nothing, reports a build
// spelled with a trailing slash, an @version suffix or a Join call, and does
// not count a string literal elsewhere or a comment.
func TestBinaryRecordingScanCatchesFixtures(t *testing.T) {
	t.Parallel()
	runs := "package p\nimport \"os/exec\"\nfunc f(binPath string) { _ = exec.Command(binPath, \"x\") }\n"
	builds := "package p\nimport \"os/exec\"\nfunc f() { _ = exec.Command(\"go\", \"build\", \"example.com/x/cmd/rancher\") }\n"
	sets := runs + "var env = []string{\"RANCHER_CLI_TELEMETRY=false\"}\n"
	setenv := runs + "func g(t T) { t.Setenv(\"RANCHER_CLI_TELEMETRY\", \"false\") }\n"
	slash := "package p\nimport \"os/exec\"\nfunc f() { _ = exec.Command(\"go\", \"build\", \"-o\", \"bin\", \"./cmd/rancher/\") }\n"
	version := "package p\nimport \"os/exec\"\nfunc f() { _ = exec.Command(\"go\", \"build\", \"example.com/x/cmd/rancher@v1.0.0\") }\n"
	joined := "package p\nimport (\n\"os/exec\"\n\"path/filepath\"\n)\nfunc f() { _ = exec.Command(\"go\", \"build\", \"-o\", \"bin\", filepath.Join(\"..\", \"..\", \"cmd\", \"rancher\")) }\n"
	elsewhere := "package p\nimport \"os/exec\"\n// exec.Command(binPath)\nvar s = \"cmd/rancher\"\nfunc f() { _ = exec.Command(\"ls\") }\n"

	got := binaryRecordingFindings(t, map[string]string{"a_test.go": runs, "b_test.go": builds}, nil)
	assert.Len(t, got, 2)
	assert.Len(t, binaryRecordingFindings(t, map[string]string{"d_test.go": slash}, nil), 1, "a trailing slash")
	assert.Len(t, binaryRecordingFindings(t, map[string]string{"e_test.go": version}, nil), 1, "an @version suffix")
	assert.Len(t, binaryRecordingFindings(t, map[string]string{"f_test.go": joined}, nil), 1, "a Join spelling")
	assert.Empty(t, binaryRecordingFindings(t, map[string]string{"a_test.go": sets, "b_test.go": setenv}, nil))
	assert.Empty(t, binaryRecordingFindings(t, map[string]string{"a_test.go": runs}, map[string]string{"a_test.go": "reason"}))
	assert.Len(t, binaryRecordingFindings(t, map[string]string{"a_test.go": sets}, map[string]string{"a_test.go": "reason"}), 1, "a stale row for a file that sets it")
	assert.Len(t, binaryRecordingFindings(t, map[string]string{"c_test.go": elsewhere}, map[string]string{"c_test.go": "reason"}), 1, "a row for a file that runs nothing")
	assert.Empty(t, binaryRecordingFindings(t, map[string]string{"c_test.go": elsewhere}, nil))
}
