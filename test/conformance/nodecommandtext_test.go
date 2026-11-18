package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The node commands inspect cluster composes with node command options are
// printed commands that the command builder does not build: Rancher's node
// command as the server returns it, then the option words. The one function
// that may compose that text is ComposeNodeCommand, in the file named below;
// the scan refuses, anywhere else in the production tree:
//   - a write to a field of the type that carries the composed commands,
//     unless the assignment's right side is a call of ComposeNodeCommand (a
//     composite literal of the type with a field counts as a write);
//   - a text construction with a node command field of provisioning.CRTObject
//     as an operand: a + chain, a +=, a fmt.Sprint* or fmt.Fprint* call, a
//     strings.Join of a slice literal, and a Write, WriteString, WriteByte or
//     WriteRune call on a strings.Builder.
//
// Fields of another type with the same names (the registration struct of
// inspect cluster, the row of get clusterregistrationtoken) are not matched:
// the scan reads types, not names.
//
// Known limits: a write through a pointer to a field, or a node command
// field passed to another function that composes the text, is not seen; the
// exact-text table of ComposeNodeCommand covers the text it composes.

const (
	nodeCommandComposeFile = "internal/engine/provisioning/nodecommand.go"
	nodeCommandComposeFunc = modulePrefix + "internal/engine/provisioning.ComposeNodeCommand"
	nodeCommandFixtures    = "test/conformance/testdata/nodecommandtext"
)

// nodeCommandFields are the node command fields of provisioning.CRTObject.
var nodeCommandFields = []string{"NodeCommand", "InsecureNodeCommand", "WindowsNodeCommand", "InsecureWindowsNodeCommand"}

// nodeCommandTextAllowlist names the declarations, as "<file>:<name>", that
// the scan would flag and that compose no node command text; each value says
// why.
var nodeCommandTextAllowlist = map[string]string{}

// nodeCommandScan is what the scan matches: the type that carries the
// composed commands and the type whose node command fields are Rancher's.
type nodeCommandScan struct {
	composedPkg, composedName string
	crtPkg, crtName           string
}

// productionNodeCommandScan is the scan of the production tree.
var productionNodeCommandScan = nodeCommandScan{
	composedPkg: modulePrefix + "internal/engine/inspect", composedName: "registrationWithOptions",
	crtPkg: modulePrefix + "internal/engine/provisioning", crtName: "CRTObject",
}

// fieldOf reports whether sel selects a field of the named struct type
// pkg.name, through a value or a pointer.
func fieldOf(info *types.Info, sel *ast.SelectorExpr, pkg, name string, fields []string) bool {
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.FieldVal {
		return false
	}
	if fields != nil && !slices.Contains(fields, sel.Sel.Name) {
		return false
	}
	return isNamedType(derefType(s.Recv()), pkg, name)
}

// unparen removes parentheses and type conversions such as []byte(x).
func unparen(info *types.Info, e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.CallExpr:
			if tv, ok := info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
				e = x.Args[0]
				continue
			}
			return e
		default:
			return e
		}
	}
}

// calledFunc is the function or method a call names, nil when it names none.
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var id *ast.Ident
	switch f := call.Fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

// scanNodeCommandText applies the rule to the files of pkg outside the
// composition file and returns each finding as "<file>:<name>: <what>".
func scanNodeCommandText(pkg *packages.Package, wd string, scan nodeCommandScan) []string {
	info := pkg.TypesInfo
	var out []string
	isCRTField := func(e ast.Expr) bool {
		sel, ok := unparen(info, e).(*ast.SelectorExpr)
		return ok && fieldOf(info, sel, scan.crtPkg, scan.crtName, nodeCommandFields)
	}
	isComposedField := func(e ast.Expr) bool {
		sel, ok := unparen(info, e).(*ast.SelectorExpr)
		return ok && fieldOf(info, sel, scan.composedPkg, scan.composedName, nil)
	}
	isCompose := func(e ast.Expr) bool {
		call, ok := unparen(info, e).(*ast.CallExpr)
		if !ok {
			return false
		}
		fn := calledFunc(info, call)
		return fn != nil && fn.FullName() == nodeCommandComposeFunc
	}
	for _, f := range pkg.Syntax {
		name, err := filepath.Rel(wd, pkg.Fset.Position(f.Pos()).Filename)
		if err != nil {
			continue
		}
		name = filepath.ToSlash(name)
		if strings.HasSuffix(name, "_test.go") || name == nodeCommandComposeFile {
			continue
		}
		visit := func(key string, decl ast.Node) {
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if !isComposedField(lhs) {
							continue
						}
						if x.Tok != token.ADD_ASSIGN && len(x.Rhs) == 1 && isCompose(x.Rhs[0]) {
							continue
						}
						out = append(out, key+": a composed command field is written with something other than a ComposeNodeCommand call")
					}
					if x.Tok == token.ADD_ASSIGN && (slices.ContainsFunc(x.Lhs, isCRTField) || slices.ContainsFunc(x.Rhs, isCRTField)) {
						out = append(out, key+": += on a node command")
					}
				case *ast.CompositeLit:
					if tv, ok := info.Types[x]; ok && isNamedType(derefType(tv.Type), scan.composedPkg, scan.composedName) && len(x.Elts) > 0 {
						out = append(out, key+": a composite literal writes a composed command field")
					}
				case *ast.BinaryExpr:
					if x.Op == token.ADD && (isCRTField(x.X) || isCRTField(x.Y)) {
						out = append(out, key+": a + chain with a node command")
					}
				case *ast.CallExpr:
					fn := calledFunc(info, x)
					if fn == nil {
						return true
					}
					pkgPath := ""
					if fn.Pkg() != nil {
						pkgPath = fn.Pkg().Path()
					}
					recv := ""
					if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
						if isNamedType(derefType(sig.Recv().Type()), "strings", "Builder") {
							recv = "strings.Builder"
						}
					}
					switch {
					case pkgPath == "fmt" && (strings.HasPrefix(fn.Name(), "Sprint") || strings.HasPrefix(fn.Name(), "Fprint")):
						if slices.ContainsFunc(x.Args, isCRTField) {
							out = append(out, key+": fmt."+fn.Name()+" with a node command")
						}
					case pkgPath == "strings" && fn.Name() == "Join" && len(x.Args) > 0:
						if lit, ok := unparen(info, x.Args[0]).(*ast.CompositeLit); ok && slices.ContainsFunc(lit.Elts, isCRTField) {
							out = append(out, key+": strings.Join with a node command")
						}
					case recv == "strings.Builder" && slices.Contains([]string{"Write", "WriteString", "WriteByte", "WriteRune"}, fn.Name()):
						if slices.ContainsFunc(x.Args, isCRTField) {
							out = append(out, key+": a strings.Builder write of a node command")
						}
					}
				}
				return true
			})
		}
		for _, d := range f.Decls {
			switch x := d.(type) {
			case *ast.FuncDecl:
				visit(name+":"+x.Name.Name, x)
			case *ast.GenDecl:
				for _, spec := range x.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							visit(name+":"+n.Name, vs)
						}
					}
				}
			}
		}
	}
	return out
}

// TestOnlyComposeNodeCommandComposesNodeCommandText asserts the rule over
// the production tree, that every finding is allowlisted, and that every
// allowlist entry still has a finding.
func TestOnlyComposeNodeCommandComposesNodeCommandText(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	wd, err := os.Getwd()
	require.NoError(t, err)
	seen := map[string]bool{}
	composedTypeFound := false
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			require.Fail(t, "type-check "+pkg.PkgPath, e.Error())
		}
		if pkg.PkgPath == productionNodeCommandScan.composedPkg && pkg.Types.Scope().Lookup(productionNodeCommandScan.composedName) != nil {
			composedTypeFound = true
		}
		for _, f := range scanNodeCommandText(pkg, wd, productionNodeCommandScan) {
			key, _, _ := strings.Cut(f, ": ")
			seen[key] = true
			if _, ok := nodeCommandTextAllowlist[key]; !ok {
				t.Error(f)
			}
		}
	}
	assert.True(t, composedTypeFound, "the type that carries the composed commands exists")
	for _, k := range slices.Sorted(maps.Keys(nodeCommandTextAllowlist)) {
		assert.True(t, seen[k], "allowlist entry %s has no finding: remove it", k)
	}
}

// TestNodeCommandTextScanFlagsEachShape asserts the scan flags each shape
// that composes text from a node command or writes a composed command other
// than from ComposeNodeCommand, in the fixture package, and passes the
// assignment from ComposeNodeCommand.
func TestNodeCommandTextScanFlagsEachShape(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./"+nodeCommandFixtures)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	for _, e := range pkgs[0].Errors {
		require.Fail(t, "type-check fixture", e.Error())
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	scan := nodeCommandScan{
		composedPkg: modulePrefix + nodeCommandFixtures, composedName: "withOptions",
		crtPkg: productionNodeCommandScan.crtPkg, crtName: productionNodeCommandScan.crtName,
	}
	got := map[string]int{}
	for _, f := range scanNodeCommandText(pkgs[0], wd, scan) {
		key, _, _ := strings.Cut(f, ": ")
		_, fn, _ := strings.Cut(key, ".go:")
		got[fn]++
	}
	assert.Equal(t, map[string]int{
		"plusChain":           2,
		"plusFromLocal":       1,
		"plusAssign":          1,
		"sprintf":             2,
		"fprintf":             1,
		"join":                1,
		"builder":             2,
		"builderBytes":        2,
		"literal":             1,
		"windowsField":        2,
		"composeThenAppended": 1,
		"packageValue":        1,
	}, got)
}
