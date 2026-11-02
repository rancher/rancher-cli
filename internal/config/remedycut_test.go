package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cutCommandOnly lists the functions of config.go whose result is a command
// and nothing else.
var cutCommandOnly = map[string]bool{"LoginHintFor": true, "LogoutHintFor": true}

// notLoggedInTexts are the messages of ErrNotLoggedInFor that hold a command,
// each with the phrase it keeps when the config directory has characters a
// terminal cannot show and the commands are left out.
func notLoggedInTexts(t *testing.T) []struct{ text, fact string } {
	t.Helper()
	const url = "https://r.example.test"
	entry := ExecLoginPrefix + "r.example.test"
	var out []struct{ text, fact string }
	add := func(ce *clierr.Error, fact string) {
		require.NotNil(t, ce)
		require.True(t, clierr.HasRemedy(ce.Message), "%q", ce.Message)
		out = append(out, struct{ text, fact string }{ce.Message, fact})
	}
	add(ErrNotLoggedInFor(nil, nil), "<url> is the address of your Rancher server")
	add(ErrNotLoggedInFor(map[string]string{entry: url}, nil), "which auth login refuses")
	add(ErrNotLoggedInFor(map[string]string{entry: url}, map[string]bool{entry: true}), "carries the CA inline in config.yaml and --cacert needs a file")
	add(ErrNotLoggedInFor(map[string]string{"prod": url, "staging": url}, nil), "configured servers: prod, staging")
	return out
}

// TestTheFactBeforeACommandSurvivesTheCutInTheNotLoggedInRemedies asserts, for
// each shape of the not-logged-in message, that the text left when the config
// directory has characters a terminal cannot show holds no command, is not the
// note alone and keeps the phrase named for it.
func TestTheFactBeforeACommandSurvivesTheCutInTheNotLoggedInRemedies(t *testing.T) {
	io := runtime.IO{RemedyDir: "/d\x01"}
	for _, row := range notLoggedInTexts(t) {
		cut := io.ExpandText(row.text)
		assert.NotContains(t, cut, "rancher ", "no command is left: %q", cut)
		assert.NotEqual(t, runtime.UnprintableConfigDirNote, cut, "the fact before the command is kept: %q", row.text)
		assert.Contains(t, cut, row.fact)
	}
}

// TestEveryRemedyBuilderOfConfigHasACutRow asserts every function of
// config.go that writes a command of the CLI is ErrNotLoggedInFor, whose
// messages the cut table holds, or is listed as a command alone; a new
// builder fails until it is in one of them.
func TestEveryRemedyBuilderOfConfigHasACutRow(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "config.go", nil, 0)
	require.NoError(t, err)
	rows := map[string]bool{"ErrNotLoggedInFor": true}
	var found []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		writes := false
		ast.Inspect(fn, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "clierr" && x.Sel.Name == "Rancher" {
					writes = true
				}
			case *ast.Ident:
				writes = writes || x.Name == "LoginCommand"
			}
			return !writes
		})
		if writes {
			found = append(found, fn.Name.Name)
		}
	}
	for _, name := range found {
		assert.True(t, rows[name] != cutCommandOnly[name], "%s is in exactly one of the cut table and the list of commands", name)
	}
	assert.Equal(t, []string{"ErrNotLoggedInFor", "LoginHintFor", "LogoutHintFor"}, slices.Sorted(slices.Values(found)))
}
