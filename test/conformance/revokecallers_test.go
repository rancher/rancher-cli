package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenDeleteFiles are the files that may delete a token on the server: the
// revoke primitive itself, login acquisition and its cleanup, the plugin
// login, the retirement pass and the revocation that follows a local
// delete. A Local command writes to the server only objects the CLI
// created for its own use and can prove it created; every other delete is
// a resource mutation and goes through the gated pipeline.
var tokenDeleteFiles = []string{
	"internal/token/revoke.go",
	"internal/token/acquire.go",
	"internal/token/directwait.go",
	"internal/token/failure.go",
	"internal/token/pluginlogin.go",
	"internal/token/retire.go",
	"internal/token/revocation.go",
}

// TestTokenRevokesOnlyFromTheFilesThatMayDeleteTokens asserts that the
// revoke entry points - LoginCredential.Revoke, LoginCredential.RevokeCreated
// and revokeToken - are called or referenced only from tokenDeleteFiles, in
// every non-test Go file of the repository.
func TestTokenRevokesOnlyFromTheFilesThatMayDeleteTokens(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || slices.Contains(tokenDeleteFiles, path) {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if v.Sel.Name == "Revoke" || v.Sel.Name == "RevokeCreated" {
					offenders = append(offenders, fset.Position(v.Pos()).String()+": ."+v.Sel.Name)
				}
			case *ast.Ident:
				if v.Name == "revokeToken" {
					offenders = append(offenders, fset.Position(v.Pos()).String()+": revokeToken")
				}
			}
			return true
		})
	}
	assert.Empty(t, offenders, "a token is deleted on the server only from %v", tokenDeleteFiles)
}
