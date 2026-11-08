package conformance

import (
	"go/ast"
	"go/types"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const accessPath = modulePrefix + "internal/engine/access"

// accessMessageFields are the fields of the access commands' own types that
// hold a message or a note, which can name a remedy command.
var accessMessageFields = []string{"Message", "Note"}

// accessTextFindings reports, for the access package among pkgs, every call
// of output.Printable that filters a message or a note field of one of its
// own types, every call of it on a line of renderTree (the range variable of
// a loop over renderTree, or the call itself), and a printableAll that
// filters through output.Printable or never through
// runtime.PrintableRemedies: output.Printable removes the remedy marker the
// text of those fields can hold.
//
// Limits: only a call of output.Printable is read, and only an argument that
// reads the field or the line itself. A builder that writes a message field
// through another function, a field read into a variable first
// (m := ns.Issue.Message; output.Printable(m)), a line of renderTree copied
// into a variable first, a result of renderTree that is stored anywhere but
// a variable of the same function or passed to another function, a stored
// result that is read by index, copied to a second variable or sliced
// (renderTree(n, mark)[1:]) instead of ranged over, and a marked cell in a
// column that is padded are not found.
func accessTextFindings(pkgs []*packages.Package) []string {
	return findAccessText(pkgs, accessPath)
}

// findAccessText is accessTextFindings for the package at pkgPath.
func findAccessText(pkgs []*packages.Package, pkgPath string) []string {
	var out []string
	for _, pkg := range pkgs {
		if pkg.PkgPath != pkgPath {
			continue
		}
		for _, f := range pkg.Syntax {
			for _, decl := range f.Decls {
				fd, isFunc := decl.(*ast.FuncDecl)
				printableAllCalls := 0
				treeLines := map[types.Object]bool{}
				treeResults := map[types.Object]bool{}
				isTreeCall := func(e ast.Expr) bool {
					call, ok := ast.Unparen(e).(*ast.CallExpr)
					return ok && isPackageFunction(calledFunction(pkg.TypesInfo, call), pkgPath, "renderTree")
				}
				remember := func(into map[types.Object]bool, e ast.Expr) {
					if id, ok := e.(*ast.Ident); ok && pkg.TypesInfo.ObjectOf(id) != nil {
						into[pkg.TypesInfo.ObjectOf(id)] = true
					}
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.AssignStmt:
						for i, rhs := range node.Rhs {
							if isTreeCall(rhs) && i < len(node.Lhs) {
								remember(treeResults, node.Lhs[i])
							}
						}
						return true
					case *ast.ValueSpec:
						for i, value := range node.Values {
							if isTreeCall(value) && i < len(node.Names) {
								remember(treeResults, node.Names[i])
							}
						}
						return true
					case *ast.RangeStmt:
						stored := false
						if id, ok := ast.Unparen(node.X).(*ast.Ident); ok {
							stored = treeResults[pkg.TypesInfo.ObjectOf(id)]
						}
						if (isTreeCall(node.X) || stored) && node.Value != nil {
							remember(treeLines, node.Value)
						}
						return true
					}
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					fn := calledFunction(pkg.TypesInfo, call)
					if isFunc && fd.Name.Name == "printableAll" && isPackageFunction(fn, runtimePath, "PrintableRemedies") {
						printableAllCalls++
					}
					if !isPackageFunction(fn, outputPath, "Printable") {
						return true
					}
					at := pkg.Fset.Position(call.Pos())
					if isFunc && fd.Name.Name == "printableAll" {
						out = append(out, at.Filename+":"+strconv.Itoa(at.Line)+": printableAll filters with output.Printable; use runtime.PrintableRemedies")
					}
					for _, arg := range call.Args {
						if readsMessageField(pkg.TypesInfo, arg, pkgPath) {
							out = append(out, at.Filename+":"+strconv.Itoa(at.Line)+": output.Printable removes the remedy marker from a message or a note; use runtime.PrintableRemedies")
							break
						}
						if readsTreeLine(pkg.TypesInfo, arg, treeLines, pkgPath) {
							out = append(out, at.Filename+":"+strconv.Itoa(at.Line)+": output.Printable removes the remedy marker from a line of renderTree; use runtime.PrintableRemedies")
							break
						}
					}
					return true
				})
				if isFunc && fd.Name.Name == "printableAll" && printableAllCalls == 0 {
					at := pkg.Fset.Position(fd.Pos())
					out = append(out, at.Filename+":"+strconv.Itoa(at.Line)+": printableAll never calls runtime.PrintableRemedies")
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// readsTreeLine reports whether e reads a variable of lines (the range
// variables of the loops over renderTree) or calls renderTree.
func readsTreeLine(info *types.Info, e ast.Expr, lines map[types.Object]bool, pkgPath string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if obj := info.ObjectOf(n); obj != nil && lines[obj] {
				found = true
			}
		case *ast.CallExpr:
			if isPackageFunction(calledFunction(info, n), pkgPath, "renderTree") {
				found = true
			}
		}
		return true
	})
	return found
}

// readsMessageField reports whether e reads a field named Message or Note of
// a struct type the access package declares.
func readsMessageField(info *types.Info, e ast.Expr, pkgPath string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !slices.Contains(accessMessageFields, sel.Sel.Name) {
			return true
		}
		selection := info.Selections[sel]
		if selection == nil || selection.Kind() != types.FieldVal {
			return true
		}
		recv := selection.Recv()
		if ptr, ok := recv.(*types.Pointer); ok {
			recv = ptr.Elem()
		}
		if named, ok := recv.(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath {
			found = true
		}
		return true
	})
	return found
}

// TestAccessTextPrintsMessagesThroughPrintableRemedies asserts the access
// commands' text keeps the remedy marker in every message and note it
// prints, so the invocation can name its config directory in a command
// they name: no call of output.Printable on such a field, and the table
// cells filtered through runtime.PrintableRemedies.
func TestAccessTextPrintsMessagesThroughPrintableRemedies(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == accessPath }))
	for _, finding := range accessTextFindings(pkgs) {
		t.Error(finding)
	}
}

const accessTextFixtureDir = "test/conformance/testdata/accesstext"

// accessTextFixtureCases lists every fixture package of the lines of
// renderTree and how many calls of output.Printable on one the scan reports.
var accessTextFixtureCases = map[string]int{
	"definedrange":   1,
	"assignedrange":  1,
	"storedresult":   1,
	"declaredresult": 1,
	"directcall":     1,
	"unrelated":      0,
}

// TestAccessTextScanFindsTheLinesOfRenderTree asserts the scan reports a
// call of output.Printable on a line of renderTree reached through a range
// variable the loop defines or assigns, through a result stored in a
// variable first, or by the call itself, and none when the loop never reads
// the line.
func TestAccessTextScanFindsTheLinesOfRenderTree(t *testing.T) {
	t.Parallel()
	pkgs, err := loadPackages("./"+accessTextFixtureDir+"/...", "")
	require.NoError(t, err)
	require.Len(t, pkgs, len(accessTextFixtureCases))
	for _, pkg := range pkgs {
		name := path.Base(pkg.PkgPath)
		want, ok := accessTextFixtureCases[name]
		require.True(t, ok, "fixture %s is not listed", name)
		findings := findAccessText(pkgs, pkg.PkgPath)
		assert.Len(t, findings, want, name)
		for _, f := range findings {
			assert.Contains(t, f, "a line of renderTree", name)
		}
	}
}

// TestAccessTextFixturesAreAllListed asserts every fixture package of the
// scan on disk is in the table above.
func TestAccessTextFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(accessTextFixtureDir)
	require.NoError(t, err)
	var onDisk, listed []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for name := range accessTextFixtureCases {
		listed = append(listed, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
}
