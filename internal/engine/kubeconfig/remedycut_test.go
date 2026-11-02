package kubeconfig

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	tokenpkg "github.com/rancher/rancher-cli/internal/token"
)

// cutRows are the texts of the remedy builders of records.go, each with the
// phrase it keeps when the config directory has characters a terminal cannot
// show and the commands are left out.
func cutRows(t *testing.T) map[string][]struct{ text, fact string } {
	t.Helper()
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod": {URL: "https://r.example.test"}, "staging": {URL: "https://r.example.test"},
	}}
	exec := config.ExecLoginPrefix + "r.example.test"
	owner := func(lk tokenpkg.StoredLogin) string {
		_, notice := lookupCall{cfg: cfg, clusterID: "c-1"}.recordedOwner(lk)
		return notice
	}
	const noOwner = "records no owner; the exec plugin does not check the user"
	reserved := reservedName(config.LegacyCachePrefix+"x", "c-1", "https://r.example.test", "", config.CASource{})
	require.Error(t, reserved)
	cfg.Servers[exec] = config.Server{URL: "https://r.example.test"}
	return map[string][]struct{ text, fact string }{
		"recordedOwner": {
			{owner(tokenpkg.StoredLogin{Outcome: tokenpkg.LookupAnswered, Entry: "prod", Identified: "prod"}), noOwner},
			{owner(tokenpkg.StoredLogin{Outcome: tokenpkg.LookupAnswered, Entry: exec, Identified: "staging"}), noOwner},
			{owner(tokenpkg.StoredLogin{Outcome: tokenpkg.LookupAnswered, Entry: exec, Identified: exec, PluginEntry: true}), noOwner},
		},
		"lookupNotice": {
			{lookupNotice(lookupCall{cfg: cfg, dir: "/x", name: "prod", clusterID: "c-1", serverID: "https://r.example.test"}, tokenpkg.StoredLogin{Outcome: tokenpkg.LookupNone}),
				"kubectl will sign in on first use when it has a terminal"},
		},
		"reservedName": {
			{reserved.(*clierr.Error).Message, "which auth get-token refuses"},
		},
	}
}

// cutCommandOnly lists the builders whose text is a step the sentence that
// holds it names: with no printable directory the step is left out.
var cutCommandOnly = map[string]bool{"kubeconfigAgain": true}

// TestTheFactBeforeACommandSurvivesTheCutInTheKubeconfigRemedies asserts, for
// the text of every remedy builder of records.go, that the text left when the
// config directory has characters a terminal cannot show holds no command, is
// not the note alone and keeps the phrase named for its builder.
func TestTheFactBeforeACommandSurvivesTheCutInTheKubeconfigRemedies(t *testing.T) {
	io := runtime.IO{RemedyDir: "/d\x01"}
	rows := cutRows(t)
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		require.NotEmpty(t, rows[name], name)
		for _, row := range rows[name] {
			require.True(t, clierr.HasRemedy(row.text), "%s: the text holds a remedy: %q", name, row.text)
			cut := io.ExpandText(row.text)
			assert.NotContains(t, cut, "rancher ", "%s: no command is left: %q", name, cut)
			assert.NotEqual(t, runtime.UnprintableConfigDirNote, cut, "%s: the fact before the command is kept: %q", name, row.text)
			assert.Contains(t, cut, row.fact, "%s keeps its fact", name)
		}
	}
}

// TestEveryRemedyBuilderOfTheKubeconfigRecordsHasACutRow asserts every
// function of records.go that writes a command of the CLI is a row of the cut
// table or listed as a step; a new builder fails until it has one.
func TestEveryRemedyBuilderOfTheKubeconfigRecordsHasACutRow(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "records.go", nil, 0)
	require.NoError(t, err)
	rows := cutRows(t)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		writes := false
		ast.Inspect(fn, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "clierr" && sel.Sel.Name == "Rancher" {
					writes = true
				}
			}
			return !writes
		})
		if !writes {
			continue
		}
		_, inRows := rows[fn.Name.Name]
		assert.True(t, inRows != cutCommandOnly[fn.Name.Name], "%s is in exactly one of the cut table and the list of steps", fn.Name.Name)
	}
}
