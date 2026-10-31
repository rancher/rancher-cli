package conformance

import (
	"go/ast"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loginCAFlagKeys maps the sign-in input types of internal/token to the field
// each literal of one must set: the CA flag name of the command that runs the
// sign-in, which a refusal about the identity provider's certificate names.
var loginCAFlagKeys = map[string]string{
	"LoginRequest": "CAFlag",
	"loginInput":   "caFlag",
}

// tokenPackagePath is the import path of the package that declares the
// sign-in input types.
const tokenPackagePath = "github.com/rancher/rancher-cli/internal/token"

// tokenImportName returns the name file f gives its import of
// internal/token, and false when it has none.
func tokenImportName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != tokenPackagePath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return "token", true
	}
	return "", false
}

// loginLiteralType returns the name of the type a composite literal builds
// when it is one of loginCAFlagKeys' types: written without a qualifier
// inside the files of the token package itself, or qualified by the name
// file f gives its import of internal/token.
func loginLiteralType(lit *ast.CompositeLit, f *ast.File, path string) (string, bool) {
	switch x := lit.Type.(type) {
	case *ast.Ident:
		if !strings.HasPrefix(path, "internal/token/") || f.Name.Name != "token" {
			return "", false
		}
		_, ok := loginCAFlagKeys[x.Name]
		return x.Name, ok
	case *ast.SelectorExpr:
		name, imported := tokenImportName(f)
		if id, ok := x.X.(*ast.Ident); ok && imported && id.Name == name {
			_, ok := loginCAFlagKeys[x.Sel.Name]
			return x.Sel.Name, ok
		}
	}
	return "", false
}

// TestEverySignInInputLiteralSetsItsCAFlag asserts every composite literal of
// token.LoginRequest and of the loginInput of internal/token, in non-test
// code, sets the CA flag field. A sign-in with none is refused at run time,
// but only a test that reaches it would see that; the scan finds a new caller
// that leaves the field out, whatever its tests cover.
//
// Forms it follows: a literal with the type written out, unqualified in the
// files of the token package and qualified by the import of internal/token
// under any name elsewhere. Forms it does not follow: a literal whose type is
// left out inside a slice or map literal ([]token.LoginRequest{{Server: s}}),
// a value built by assigning the fields one at a time after a bare
// declaration, and a literal of an alias of the types. The run-time refusal
// of an empty CA flag covers them when a test reaches the call.
func TestEverySignInInputLiteralSetsItsCAFlag(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	seen := map[string]int{}
	for i, f := range tree.files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typ, ok := loginLiteralType(lit, f, tree.paths[i])
			if !ok {
				return true
			}
			seen[typ]++
			key := loginCAFlagKeys[typ]
			set := false
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
						set = true
					}
				}
			}
			if !set {
				pos := tree.fset.Position(lit.Pos())
				t.Errorf("%s:%d: the %s literal does not set %s", tree.paths[i], pos.Line, typ, key)
			}
			return true
		})
	}
	assert.GreaterOrEqual(t, seen["LoginRequest"], 2, "the scan reaches the LoginRequest literals of auth login and the plugin login")
	assert.GreaterOrEqual(t, seen["loginInput"], 2, "the scan reaches the loginInput literals of get-token and AcquireLoginCredential")
}
