package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const helpRenderingDir = "test/conformance/testdata/helprendering"

// urfaveHelpRendering are the names of urfave/cli that render a help page or a
// flag row: the CLI renders every help page with its own code.
var urfaveHelpRendering = []string{"HelpPrinterCustom", "DefaultPrintHelp", "DefaultPrintHelpCustom", "FlagStringer"}

// urfaveFlagType returns the interface type Flag of urfave/cli as pkg sees it,
// found among the packages pkg imports, directly or not, or nil when pkg does
// not import it.
func urfaveFlagType(pkg *types.Package, seen map[*types.Package]bool) *types.Interface {
	if seen[pkg] {
		return nil
	}
	seen[pkg] = true
	if pkg.Path() == urfaveCLIPath {
		if obj := pkg.Scope().Lookup("Flag"); obj != nil {
			iface, _ := obj.Type().Underlying().(*types.Interface)
			return iface
		}
		return nil
	}
	for _, imported := range pkg.Imports() {
		if iface := urfaveFlagType(imported, seen); iface != nil {
			return iface
		}
	}
	return nil
}

// helpRenderingUses returns the uses, one for each file and line, of the names
// of urfaveHelpRendering and the calls of a String method on a value whose
// type implements the Flag interface of urfave/cli (it writes the flag row
// through FlagStringer) in the non-test files of pkgs, as file:line, and the
// number of files read. A file several packages share, as the loads for
// several operating systems do, is read once.
func helpRenderingUses(pkgs []*packages.Package) (uses []string, files int) {
	seenFiles := map[string]bool{}
	seenUses := map[string]bool{}
	note := func(pkg *packages.Package, pos token.Pos, name string) {
		position := pkg.Fset.Position(pos)
		where := fmt.Sprintf("%s:%d", position.Filename, position.Line)
		if !seenUses[where] {
			seenUses[where] = true
			uses = append(uses, where+" uses "+name)
		}
	}
	for _, pkg := range pkgs {
		flagType := urfaveFlagType(pkg.Types, map[*types.Package]bool{})
		for _, f := range pkg.Syntax {
			if name := pkg.Fset.Position(f.Pos()).Filename; !seenFiles[name] {
				seenFiles[name] = true
				files++
			}
			if flagType == nil {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "String" || len(call.Args) != 0 {
					return true
				}
				if selection := pkg.TypesInfo.Selections[sel]; selection != nil && selection.Kind() == types.MethodVal &&
					implementsOrPointerDoes(pkg.TypesInfo.TypeOf(sel.X), flagType) {
					note(pkg, sel.Sel.Pos(), "String")
				}
				return true
			})
		}
		for id, obj := range pkg.TypesInfo.Uses {
			if obj.Pkg() == nil || obj.Pkg().Path() != urfaveCLIPath || !slices.Contains(urfaveHelpRendering, obj.Name()) {
				continue
			}
			note(pkg, id.Pos(), obj.Name())
		}
	}
	slices.Sort(uses)
	return uses, files
}

// TestNoCodeUsesUrfaveHelpRendering asserts that no non-test code the release
// builds uses HelpPrinterCustom, DefaultPrintHelp, DefaultPrintHelpCustom or
// FlagStringer of urfave/cli, or calls the String method of a flag of
// urfave/cli, which writes the flag row through FlagStringer: the CLI renders
// every help page and flag row with its own code, and the test has no
// allowlist. HelpPrinter, VersionPrinter, the template variables
// (RootCommandHelpTemplate, CommandHelpTemplate, SubcommandHelpTemplate),
// ShowCommandHelp, DefaultShowCommandHelp, ShowRootCommandHelp,
// ShowSubcommandHelp and DocGenerationFlag stay allowed: urfave's command
// handling still calls or reads them.
func TestNoCodeUsesUrfaveHelpRendering(t *testing.T) {
	t.Parallel()
	pkgs, err := discardScanPackages()
	require.NoError(t, err)
	uses, files := helpRenderingUses(pkgs)
	assert.Greater(t, files, 100, "the scan read too few files")
	assert.Empty(t, uses, "code uses urfave's help rendering")
}

// helpRenderingFixtures reads the fixture package the way the module is read.
var helpRenderingFixtures = sync.OnceValues(func() ([]*packages.Package, error) {
	return loadDiscardPackages("./" + helpRenderingDir + "/...")
})

// TestNoCodeUsesUrfaveHelpRenderingFixtures asserts that the scan finds
// exactly the five planted uses of the fixture package (the four names and a
// call of String on a flag), each once with its file and line, and none of the
// names that stay allowed.
func TestNoCodeUsesUrfaveHelpRenderingFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := helpRenderingFixtures()
	require.NoError(t, err)
	var fixture []*packages.Package
	for _, pkg := range pkgs {
		if path.Base(pkg.PkgPath) == "helprendering" {
			fixture = append(fixture, pkg)
		}
	}
	require.NotEmpty(t, fixture)
	uses, _ := helpRenderingUses(fixture)
	require.Len(t, uses, 5, "%v", uses)
	var names []string
	for _, use := range uses {
		where, name, _ := strings.Cut(use, " uses ")
		assert.Contains(t, where, "helprendering.go:")
		names = append(names, name)
	}
	assert.ElementsMatch(t, append(slices.Clone(urfaveHelpRendering), "String"), names)
}
