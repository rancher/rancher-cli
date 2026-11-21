package catalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionCaller is a call of postAction or pullSecretsWarningApplies in a
// non-test file, with the function that contains it and the row of
// admissionActions that has the responses the action is tested with.
type actionCaller struct {
	file     string
	function string
	call     string
	row      string
}

// actionCallers is every call the catalog package makes that sends an action
// POST or reads the gate of the sentence on the image pull secrets. An action
// that is not here fails the scan; a new action therefore comes with its rows.
var actionCallers = []actionCaller{
	{"install.go", "installCmd.Plan", "pullSecretsWarningApplies", "install"},
	{"install.go", "installCmd.execute", "postAction", "install"},
	{"upgrade.go", "upgradeCmd.Plan", "pullSecretsWarningApplies", "upgrade"},
	{"upgrade.go", "upgradeCmd.execute", "postAction", "upgrade"},
	{"uninstall.go", "uninstallCmd.Plan", "postAction", "uninstall"},
	{"uninstall.go", "uninstallCmd.Plan", "pullSecretsWarningApplies", "uninstall"},
}

// functionName is the name of decl, with the type of its receiver when it has
// one.
func functionName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	typ := decl.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// TestEveryActionCallerHasItsRowsAndItsPullSecretNote asserts the non-test
// files of the package call postAction and pullSecretsWarningApplies only
// where actionCallers lists them, that each listed call exists, that each row
// named is in admissionActions, and that a function that reads the gate of
// the pull secret sentence also sets the sentence. Every action then reports a
// refused pod with the same pull-secret sentence its preview warns with.
func TestEveryActionCallerHasItsRowsAndItsPullSecretNote(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var found []actionCaller
	gateWithoutNote := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			function := functionName(fn)
			setsNote, readsGate := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "postAction" || id.Name == "pullSecretsWarningApplies") {
						found = append(found, actionCaller{file: name, function: function, call: id.Name})
						readsGate = readsGate || id.Name == "pullSecretsWarningApplies"
					}
				case *ast.SelectorExpr:
					setsNote = setsNote || n.Sel.Name == "pullSecretNote"
				}
				return true
			})
			if readsGate && !setsNote {
				gateWithoutNote[name+" "+function] = true
			}
		}
	}

	key := func(c actionCaller) string { return c.file + " " + c.function + " calls " + c.call }
	var want, got []string
	for _, c := range actionCallers {
		want = append(want, key(c))
	}
	for _, c := range found {
		got = append(got, key(c))
	}
	slices.Sort(want)
	slices.Sort(got)
	assert.Equal(t, want, got, "the calls of postAction and pullSecretsWarningApplies are the listed ones")

	rows := map[string]bool{}
	for _, a := range admissionActions {
		rows[a.name] = true
	}
	for _, c := range actionCallers {
		assert.True(t, rows[c.row], "%s has no row %q in admissionActions", key(c), c.row)
	}
	assert.Empty(t, gateWithoutNote, "a function that reads the gate of the pull secret sentence sets pullSecretNote")
}
