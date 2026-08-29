package main_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stderrAllowlist names every production source file allowed to reference
// os.Stderr directly. Every other pipeline-owned diagnostic goes through
// runtime.IO.Err (wired to the invocation's real stderr), so every drain
// feeds exactly one render and decides what an operator sees from there —
// a stray os.Stderr write bypasses that decision entirely, e.g. printing
// under -o json when the machine contract promises stderr is the error
// envelope or empty.
//
// The four entries here are the reviewed exceptions, each for a reason
// that holds regardless of the pipeline's own state:
//   - main.go: a startup failure (before any pipeline, or the embedded
//     skill tree, exists to route through).
//   - internal/commands/config_edit.go: the external editor subprocess is
//     deliberately attached to the process's own stderr — it is not a
//     rendered diagnostic.
//   - credhelper/serve.go and cmd/rancher-credential-vault/main.go: each is
//     its OWN process, invoked by kubectl (or a credential helper caller)
//     as a subprocess — no envelope exists there to carry anything.
var stderrAllowlist = map[string]bool{
	"main.go":                              true,
	"internal/commands/config_edit.go":     true,
	"credhelper/serve.go":                  true,
	"cmd/rancher-credential-vault/main.go": true,
}

// skipDirNames are walked over but never descended into: VCS/build
// metadata, tooling config, and test fixtures (testdata trees ship
// deliberately contrived helper binaries, not pipeline-owned production
// code).
var skipDirNames = map[string]bool{
	".git": true, ".github": true, ".githooks": true, ".claude": true,
	".superpowers": true, "bin": true, "testdata": true, "node_modules": true,
	"vendor": true,
}

// TestNoStrayStderrWriterOutsideAllowlist is the grep-level (here, an AST
// walk, so a comment or string literal mentioning "os.Stderr" cannot
// false-positive) check enforcing the stderr-purity invariant: every
// production .go file's os.Stderr reference must be one of the reviewed
// allowlist entries above. A new direct write anywhere else fails the
// build instead of silently reintroducing an out-of-pipeline stray (a
// lazy-store pin notice, a credential-store keychain/permission warning,
// written straight to stderr instead of through runtime.IO.Err/NoticeSource).
func TestNoStrayStderrWriterOutsideAllowlist(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	seen := map[string]bool{}

	walkErr := filepath.WalkDir(".", func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if path != "." && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		require.NoError(t, perr, "parsing %s", path)
		if referencesOSStderr(f) {
			seen[filepath.ToSlash(path)] = true
		}
		return nil
	})
	require.NoError(t, walkErr)

	for path := range seen {
		assert.True(t, stderrAllowlist[path], "%s references os.Stderr directly — either route the diagnostic through runtime.IO.Err/NoticeSource, or add it to stderrAllowlist with a reviewed reason", path)
	}
	for path := range stderrAllowlist {
		assert.True(t, seen[path], "%s is allowlisted but no longer references os.Stderr — prune the stale entry", path)
	}
}

// referencesOSStderr reports whether f contains an "os.Stderr" selector
// expression anywhere in its declarations.
func referencesOSStderr(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Stderr" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" {
			found = true
			return false
		}
		return true
	})
	return found
}
