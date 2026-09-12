package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLiveDispatcherCarriesNoMachineConfigKnowledge asserts the surface's
// live completion files name the machine-config kinds only in the two
// compiled-in static projections (the `<kind>/` prefixes and the --set
// keys) and the dispatch step that calls them: every other declaration
// in dispatch.go, livedispatch.go and livesource.go is free of the
// word. The live half of --machine-config belongs to create
// cluster's completion source, and no live path here may grow one back.
func TestLiveDispatcherCarriesNoMachineConfigKnowledge(t *testing.T) {
	t.Parallel()
	staticProjections := []string{"machineConfigPrefixCandidates", "machineConfigSetKeyCandidates", "dispatchSite"}
	fset := token.NewFileSet()
	for _, file := range []string{"dispatch.go", "livedispatch.go", "livesource.go"} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		parsed, err := parser.ParseFile(fset, file, src, parser.ParseComments)
		require.NoError(t, err)
		for _, decl := range parsed.Decls {
			name := ""
			start, end := decl.Pos(), decl.End()
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name = d.Name.Name
				if d.Doc != nil {
					start = d.Doc.Pos()
				}
			case *ast.GenDecl:
				if d.Doc != nil {
					start = d.Doc.Pos()
				}
			}
			if slices.Contains(staticProjections, name) {
				continue
			}
			text := string(src[fset.Position(start).Offset:fset.Position(end).Offset])
			require.NotContains(t, strings.ToLower(text), "machine",
				"%s: %s names the machine-config kinds; only the static projections may", file, name)
		}
	}
}
