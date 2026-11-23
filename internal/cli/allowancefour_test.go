package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// completionPackage is the package that declares the Allowance class.
const completionPackage = modulePath + "/internal/runtime/completion"

// allowanceFourSources are the declarations of the live source packages and
// of the completion package that may name AllowanceFour, one entry per
// "<package path>/<file>:<declaration>", each with the reason it needs the
// name. A function is named by its name, a constant or variable by its
// first name.
var allowanceFourSources = map[string]string{
	"internal/engine/provisioning/complete/upgrade.go:claimUpgradeVersion": "the `cluster upgrade` `--k8s-version` source: the provisioning Cluster, the management Cluster, the imported-cluster-version-management Setting and one release list: up to four requests",
	"internal/runtime/completion/source.go:AllowanceFour":                  "the declaration of the class member",
}

// allowanceExtensionSources are the declarations of the live source packages
// and of the completion package that may name AllowanceExtension, listed as
// allowanceFourSources lists its own.
var allowanceExtensionSources = map[string]string{
	"internal/engine/catalog/complete/extensioninstall.go:ExtensionInstallSource": "the `extension install` source of the chart name, --repo and --version: the version document, the local Cluster, the ClusterRepo collection, the UIPlugins, the Apps and one index per repository up to completion.ExtensionRepositoryLimit",
	"internal/engine/catalog/complete/extensionupgrade.go:ExtensionUpgradeSource": "the `extension upgrade` source of the chart name and --version: the compatibility reads, the Apps, the ClusterRepo collection and one index per repository the Apps name up to completion.ExtensionRepositoryLimit",
	"internal/runtime/completion/source.go:AllowanceExtension":                    "the declaration of the class member",
}

// allowanceScan is what the scan of one file found.
type allowanceScan struct {
	// uses are the "<file>:<declaration>" entries whose declaration names
	// AllowanceFour.
	uses []string
	// extensionUses are the same for AllowanceExtension.
	extensionUses []string
	// bypasses are the places that build an Allowance without naming a
	// member of the class: a conversion, arithmetic on a member or the
	// field, a basic literal in an Allowance position.
	bypasses []string
}

// isAllowanceType reports whether e names the type Allowance, bare or
// through a package.
func isAllowanceType(e ast.Expr) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return x.Name == "Allowance"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Allowance"
	}
	return false
}

// isAllowanceValue reports whether e names a member of the class or the
// Allowance field of a Requirements.
func isAllowanceValue(e ast.Expr) bool {
	name := ""
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		name = x.Name
	case *ast.SelectorExpr:
		name = x.Sel.Name
	}
	return strings.HasPrefix(name, "Allowance")
}

// isLiteral reports whether e is a basic literal.
func isLiteral(e ast.Expr) bool {
	_, ok := ast.Unparen(e).(*ast.BasicLit)
	return ok
}

// specName returns the name a constant, variable or type is listed by: its
// first name.
func specName(spec ast.Spec) string {
	switch s := spec.(type) {
	case *ast.ValueSpec:
		return s.Names[0].Name
	case *ast.TypeSpec:
		return s.Name.Name
	}
	return ""
}

// namesIdent reports whether the node has the identifier name.
func namesIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if id, ok := m.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// scanAllowanceFile reads one parsed file; key is the file's path below the
// module root with forward slashes.
func scanAllowanceFile(fset *token.FileSet, file *ast.File, key string) allowanceScan {
	var out allowanceScan
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if namesIdent(d, "AllowanceFour") {
				out.uses = append(out.uses, key+":"+d.Name.Name)
			}
			if namesIdent(d, "AllowanceExtension") {
				out.extensionUses = append(out.extensionUses, key+":"+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if namesIdent(spec, "AllowanceFour") {
					out.uses = append(out.uses, key+":"+specName(spec))
				}
				if namesIdent(spec, "AllowanceExtension") {
					out.extensionUses = append(out.extensionUses, key+":"+specName(spec))
				}
			}
		}
	}
	bypass := func(n ast.Node, form string) {
		out.bypasses = append(out.bypasses, key+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+form)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if isAllowanceType(x.Fun) {
				bypass(x, "a conversion to Allowance")
			}
		case *ast.BinaryExpr:
			switch x.Op {
			case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT:
				if isAllowanceValue(x.X) || isAllowanceValue(x.Y) {
					bypass(x, "arithmetic on an Allowance")
				}
			}
		case *ast.UnaryExpr:
			switch x.Op {
			case token.ADD, token.SUB, token.XOR:
				if isAllowanceValue(x.X) {
					bypass(x, "arithmetic on an Allowance")
				}
			}
		case *ast.IncDecStmt:
			if isAllowanceValue(x.X) {
				bypass(x, "arithmetic on an Allowance")
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if !isAllowanceValue(lhs) {
					continue
				}
				switch {
				case x.Tok != token.ASSIGN && x.Tok != token.DEFINE:
					bypass(x, "arithmetic on an Allowance")
				case x.Tok == token.ASSIGN && len(x.Lhs) == len(x.Rhs) && isLiteral(x.Rhs[i]):
					bypass(x, "a basic literal as an Allowance")
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Allowance" && isLiteral(x.Value) {
				bypass(x, "a basic literal as an Allowance")
			}
		case *ast.ValueSpec:
			if x.Type != nil && isAllowanceType(x.Type) && slices.ContainsFunc(x.Values, isLiteral) {
				bypass(x, "a basic literal as an Allowance")
			}
		case *ast.FuncDecl:
			if x.Body == nil || x.Type.Results == nil {
				break
			}
			if !slices.ContainsFunc(x.Type.Results.List, func(f *ast.Field) bool { return isAllowanceType(f.Type) }) {
				break
			}
			ast.Inspect(x.Body, func(m ast.Node) bool {
				if ret, ok := m.(*ast.ReturnStmt); ok && slices.ContainsFunc(ret.Results, isLiteral) {
					bypass(ret, "a basic literal returned as an Allowance")
				}
				return true
			})
		}
		return true
	})
	slices.Sort(out.uses)
	out.uses = slices.Compact(out.uses)
	slices.Sort(out.extensionUses)
	out.extensionUses = slices.Compact(out.extensionUses)
	return out
}

// scanAllowancePackages reads the non-test files of every live source
// package and of the completion package from the source tree.
func scanAllowancePackages(t *testing.T) allowanceScan {
	t.Helper()
	pkgs := []string{completionPackage}
	for pkg := range liveSourcePackages {
		pkgs = append(pkgs, pkg)
	}
	slices.Sort(pkgs)
	var all allowanceScan
	for _, pkg := range pkgs {
		rel := strings.TrimPrefix(pkg, modulePath+"/")
		dir := filepath.Join("..", "..", filepath.FromSlash(rel))
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, filepath.Join(dir, f.Name()), nil, 0)
			require.NoError(t, err)
			found := scanAllowanceFile(fset, parsed, rel+"/"+f.Name())
			all.uses = append(all.uses, found.uses...)
			all.extensionUses = append(all.extensionUses, found.extensionUses...)
			all.bypasses = append(all.bypasses, found.bypasses...)
		}
	}
	slices.Sort(all.uses)
	slices.Sort(all.extensionUses)
	return all
}

// TestOnlyTheListedSourcesClaimAllowanceFour asserts that AllowanceFour is
// named only by the declarations of allowanceFourSources, among the
// non-test files of every live source package and of the completion
// package, that every entry of the list is still used, and that no file
// builds an Allowance without naming a member of the class. The scan matches
// names and literal forms; it does not catch every expression a program can
// build, such as arithmetic on a variable of type Allowance or a constant
// expression of literals.
func TestOnlyTheListedSourcesClaimAllowanceFour(t *testing.T) {
	t.Parallel()
	found := scanAllowancePackages(t)
	for _, use := range found.uses {
		_, listed := allowanceFourSources[use]
		assert.True(t, listed, "%s names AllowanceFour and is not in allowanceFourSources: a source with up to four requests is a reviewed exception, with its reason", use)
	}
	for entry, reason := range allowanceFourSources {
		assert.NotEmpty(t, reason, entry)
		assert.Contains(t, found.uses, entry, "allowanceFourSources lists %s, which names AllowanceFour nowhere", entry)
	}
	for _, form := range found.bypasses {
		assert.Fail(t, "an Allowance is built without naming a member of the class", form)
	}
}

// TestOnlyTheListedSourcesClaimAllowanceExtension asserts that
// AllowanceExtension is named only by the declarations of
// allowanceExtensionSources, among the non-test files of every live source
// package and of the completion package, and that every entry of the list is
// still used.
func TestOnlyTheListedSourcesClaimAllowanceExtension(t *testing.T) {
	t.Parallel()
	found := scanAllowancePackages(t)
	for _, use := range found.extensionUses {
		_, listed := allowanceExtensionSources[use]
		assert.True(t, listed, "%s names AllowanceExtension and is not in allowanceExtensionSources: a source that reads one index per repository is a reviewed exception, with its reason", use)
	}
	for entry, reason := range allowanceExtensionSources {
		assert.NotEmpty(t, reason, entry)
		assert.Contains(t, found.extensionUses, entry, "allowanceExtensionSources lists %s, which names AllowanceExtension nowhere", entry)
	}
}

// TestAllowanceScanSeesEveryBypass asserts that the scan reports each form
// that builds an Allowance without naming a member of the class, and
// nothing in a source that claims a member or passes a value on.
func TestAllowanceScanSeesEveryBypass(t *testing.T) {
	t.Parallel()
	const head = "package p\nimport \"x/completion\"\n"
	tests := []struct {
		name          string
		src           string
		uses          []string
		extensionUses []string
		bypasses      []string
	}{
		{name: "claims a member", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: completion.AllowanceThree} }\n"},
		{name: "passes a parameter on", src: head + "func f(a completion.Allowance) completion.Requirements { return completion.Requirements{Allowance: a} }\n"},
		{name: "assigns a member", src: head + "func f(r *completion.Requirements) { r.Allowance = completion.AllowanceOne }\n"},
		{name: "compares a member", src: head + "func f(r completion.Requirements) bool { return r.Allowance > completion.AllowanceOne }\n"},
		{name: "names AllowanceFour in a function", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: completion.AllowanceFour} }\n", uses: []string{"f.go:f"}},
		{name: "names AllowanceFour in a constructor", src: head + "func ManagementFourReads() Requirements { return Requirements{Allowance: AllowanceFour} }\n", uses: []string{"f.go:ManagementFourReads"}},
		{name: "names AllowanceFour in a variable", src: head + "var four = completion.AllowanceFour\n", uses: []string{"f.go:four"}},
		{name: "names AllowanceExtension in a function", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: completion.AllowanceExtension} }\n", extensionUses: []string{"f.go:f"}},
		{name: "names AllowanceExtension in a variable", src: head + "var all = completion.AllowanceExtension\n", extensionUses: []string{"f.go:all"}},
		{name: "converts a number", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: completion.Allowance(4)} }\n", bypasses: []string{"f.go:3: a conversion to Allowance"}},
		{name: "converts without the package", src: head + "func f() Allowance { return Allowance(4) }\n", bypasses: []string{"f.go:3: a conversion to Allowance"}},
		{name: "adds to a member", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: completion.AllowanceThree + 1} }\n", bypasses: []string{"f.go:3: arithmetic on an Allowance"}},
		{name: "adds to the field", src: head + "func f(r completion.Requirements) completion.Allowance { return r.Allowance * 2 }\n", bypasses: []string{"f.go:3: arithmetic on an Allowance"}},
		{name: "increments the field", src: head + "func f(r *completion.Requirements) { r.Allowance++ }\n", bypasses: []string{"f.go:3: arithmetic on an Allowance"}},
		{name: "adds to the field in place", src: head + "func f(r *completion.Requirements) { r.Allowance += 1 }\n", bypasses: []string{"f.go:3: arithmetic on an Allowance"}},
		{name: "negates a member", src: head + "var n = -completion.AllowanceOne\n", bypasses: []string{"f.go:3: arithmetic on an Allowance"}},
		{name: "writes a literal in the field", src: head + "func f() completion.Requirements { return completion.Requirements{Allowance: 4} }\n", bypasses: []string{"f.go:3: a basic literal as an Allowance"}},
		{name: "assigns a literal to the field", src: head + "func f(r *completion.Requirements) { r.Allowance = 4 }\n", bypasses: []string{"f.go:3: a basic literal as an Allowance"}},
		{name: "declares a typed literal", src: head + "var four completion.Allowance = 4\n", bypasses: []string{"f.go:3: a basic literal as an Allowance"}},
		{name: "returns a literal", src: head + "func four() completion.Allowance { return 4 }\n", bypasses: []string{"f.go:3: a basic literal returned as an Allowance"}},
		{name: "declares the class", src: "package p\ntype Allowance int\nconst (\n\tAllowanceOne Allowance = iota + 1\n\tAllowanceTwo\n)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, "f.go", tc.src, 0)
			require.NoError(t, err)
			found := scanAllowanceFile(fset, parsed, "f.go")
			assert.Equal(t, tc.uses, found.uses)
			assert.Equal(t, tc.extensionUses, found.extensionUses)
			assert.Equal(t, tc.bypasses, found.bypasses)
		})
	}
}
