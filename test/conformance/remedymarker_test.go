package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/remedytest"
)

// remedyPathRoots are the packages whose text kubectl's exec plugin (auth
// get-token) and the kubeconfig command print: the two commands, and the
// cluster client the CLI layer hands kubeconfig. Every module package they
// import is on those paths too - its own text and the shared text they
// relay - so every command either one prints must open with clierr.Rancher,
// so the invocation can name its config directory.
var remedyPathRoots = []string{"./internal/token", "./internal/engine/kubeconfig", "./internal/client/provider"}

// remedyPathPackages lists the module packages the roots import, and the
// roots, as directories relative to the module root.
func remedyPathPackages(t *testing.T) []string {
	t.Helper()
	args := append([]string{"list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}"}, remedyPathRoots...)
	out, err := exec.CommandContext(t.Context(), "go", args...).Output()
	require.NoError(t, err)
	var dirs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if rest, ok := strings.CutPrefix(line, modulePrefix); ok {
			dirs = append(dirs, rest)
		}
	}
	return dirs
}

// directoryWords is remedytest.DirectoryCommandWords, failing t on error.
func directoryWords(t *testing.T, golden string) string {
	t.Helper()
	words, err := remedytest.DirectoryCommandWords(golden)
	require.NoError(t, err)
	return words
}

// remedyLiteralRE matches a rancher command written as plain text: the
// word rancher, not part of a longer word, followed by a command that
// reads the config directory.
func remedyLiteralRE(t *testing.T) *regexp.Regexp {
	t.Helper()
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher (?:` + directoryWords(t, "internal/cli/testdata/schema.golden.json") + `)(?:[^A-Za-z0-9_-]|$)`)
}

// remedyLiteralAllowed are the plain rancher commands on those paths that
// are not written through the marker, each with the reason.
var remedyLiteralAllowed = []struct {
	file, text, why string
}{
	{"internal/token/entry.go", "the legacy rancher token command cannot name one",
		"names the command whose kubeconfig this is; it runs nothing"},
	{"internal/token/token.go", "(e.g. rancher auth delete-token prod, or rancher auth delete-token --all)",
		"auth delete-token's own usage error, printed by neither command"},
	{"internal/clierr/loginfailure.go", "rancher get management-tokens",
		"RecoveryCommands also fills result data of other commands; the exec plugin marks it where it prints it (clierr.MarkRemedy)"},
	{"internal/clierr/loginfailure.go", "rancher delete management-token ",
		"RecoveryCommands, as above"},
	{"internal/clierr/loginfailure.go", "rancher get tokens",
		"RecoveryCommands, as above"},
	{"internal/clierr/loginfailure.go", "rancher delete token ",
		"RecoveryCommands, as above"},
	{"internal/clierr/recovery.go", "rancher auth login ",
		"RecoverySequence also fills result data of other commands; the exec plugin marks it where it prints it (clierr.MarkRemedy)"},
	{"internal/clierr/recovery.go", "rancher config use-server ",
		"RecoverySequence, as above"},
	{"internal/clierr/recovery.go", "rancher auth logout ",
		"RecoverySequence, as above"},
}

// descriptorDocFields hold a command's own help and examples, which are
// not remedies.
var descriptorDocFields = map[string]bool{"Examples": true, "Desc": true, "Usage": true}

// TestRemedyCommandsOnTheExecPluginPathsUseTheMarker asserts no remedy on
// the exec plugin's and the kubeconfig command's paths is written as plain
// text, where the invocation could not name its config directory in it.
func TestRemedyCommandsOnTheExecPluginPathsUseTheMarker(t *testing.T) {
	t.Parallel()
	used := make([]bool, len(remedyLiteralAllowed))
	scanned := 0
	literalRE := remedyLiteralRE(t)
	pkgs := remedyPathPackages(t)
	for _, want := range []string{"internal/engine/kinds", "internal/client/provider", "internal/runtime", "internal/config"} {
		require.Contains(t, pkgs, want)
	}
	for _, pkg := range pkgs {
		entries, err := os.ReadDir(pkg)
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.ToSlash(filepath.Join(pkg, name))
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			scanned++
			for lit := range plainRemedyLiterals(f) {
				text, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				if !literalRE.MatchString(text) {
					continue
				}
				allowed := false
				for i, a := range remedyLiteralAllowed {
					if a.file == path && strings.Contains(text, a.text) {
						allowed, used[i] = true, true
					}
				}
				assert.True(t, allowed, "%s: %q writes a rancher command as plain text; build it on clierr.Rancher",
					fset.Position(lit.Pos()), text)
			}
		}
	}
	require.Positive(t, scanned)
	for i, a := range remedyLiteralAllowed {
		assert.True(t, used[i], "allowlist entry %s %q matches nothing; remove it", a.file, a.text)
	}
}

// plainRemedyLiterals yields every string literal of f outside a
// descriptor's documentation fields.
func plainRemedyLiterals(f *ast.File) func(func(*ast.BasicLit) bool) {
	return func(yield func(*ast.BasicLit) bool) {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, p := range stack {
				if kv, ok := p.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && descriptorDocFields[id.Name] {
						return true
					}
				}
			}
			return yield(lit)
		})
	}
}
