package main_test

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stderrAllowlist names every production source file allowed to reference
// os.Stderr directly. Every other pipeline-owned diagnostic goes through
// runtime.IO.Err (wired to the invocation's real stderr), so every drain
// feeds exactly one render and decides what an operator sees from there -
// a stray os.Stderr write bypasses that decision entirely, e.g. printing
// under -o json when the machine contract promises stderr is the error
// envelope or empty.
//
// The four entries here are the reviewed exceptions, each for a reason
// that holds regardless of the pipeline's own state:
//   - main.go: a startup failure (before any pipeline, or the embedded
//     skill tree, exists to route through).
//   - internal/commands/config_edit.go: the external editor subprocess is
//     deliberately attached to the process's own stderr - it is not a
//     rendered diagnostic.
//   - credhelper/serve.go and cmd/rancher-credential-vault/main.go: each is
//     its OWN process, invoked by kubectl (or a credential helper caller)
//     as a subprocess - no envelope exists there to carry anything.
//   - surface/cli/completion_debug.go: the opt-in completion trace runs in
//     the shell's completion child and must observe phases from process
//     start (config load included), before any runtime.IO exists; the
//     shell protocol owns stdout, so raw stderr is the only channel, and
//     the trace is inert unless RANCHER_CLI_COMPLETION_DEBUG is set during
//     an active completion request.
var stderrAllowlist = map[string]bool{
	"main.go":                              true,
	"internal/commands/config_edit.go":     true,
	"credhelper/serve.go":                  true,
	"cmd/rancher-credential-vault/main.go": true,
	"surface/cli/completion_debug.go":      true,
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
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	seen := map[string]bool{}
	for i, f := range tree.files {
		if referencesOSStderr(f) {
			seen[tree.paths[i]] = true
		}
	}

	for path := range seen {
		assert.True(t, stderrAllowlist[path], "%s references os.Stderr directly - either route the diagnostic through runtime.IO.Err/NoticeSource, or add it to stderrAllowlist with a reviewed reason", path)
	}
	for path := range stderrAllowlist {
		assert.True(t, seen[path], "%s is allowlisted but no longer references os.Stderr - prune the stale entry", path)
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
