package conformance

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The result of a mutating command hides what its preview hides. The
// planners (engine.PlanCreate, engine.PlanCreateObserved, engine.PlanUpdate
// and engine.PlanUpdateObserved) scrub the committed object with the
// kind's redaction before they put it in the result. This scan keeps every
// other command from putting a server object in a result's Data: in
// production code outside the planners, no assignment to the Data of a
// runtime.Result and no Data key of a runtime.Result literal may hold an
// expression whose tree contains a server object:
//
//   - a value of type unstructured.Unstructured, unstructured.UnstructuredList,
//     a pointer to either, or a slice of Unstructured;
//   - a read of the Object or Items field of one;
//   - a call of GetAnnotations, GetLabels, MarshalJSON, UnstructuredContent,
//     DeepCopy or DeepCopyObject on one.
//
// A read that yields a basic value is not a server object: created.GetName(),
// an index into a result that is a string (created.GetAnnotations()[k]), a
// call whose result is a string, a boolean or a number, and a type assertion
// to one of those.
//
// When the Data expression names a local variable that is not of a basic
// type, the scan also checks the expressions assigned to it in the same
// function: by :=, = and var v = x, and by an index assignment v[k] = x. It
// follows the locals those expressions name in turn, to any depth, each
// local once for each Data expression; a local of a basic type ends a
// chain.
//
// The scan cannot see data flow beyond that. It reports nothing for: a copy
// through a range loop or a call that fills a variable (maps.Copy,
// json.Unmarshal); a call on a receiver of an interface type (a
// metav1.Object or client.Object); a field assignment (v.f = x); a helper
// that builds the Result from a parameter, or that fetches an object and
// returns its map; a closure called in place; a Result literal without
// field names; and a string built from an object (fmt.Sprint(obj),
// string(b)), which the basic-result rule lets through. The result table
// in internal/cli runs every mutating command and asserts the output of
// each: it covers every mutating command that exists, where this scan
// covers the shapes a new command is most likely to use.
//
// A finding names the file, the enclosing function and the line. An
// allowlist entry is keyed "<file>:<function>", or "<file>:<receiver>.<method>"
// for a method (the receiver type without its star), and carries the
// reason; an entry no finding matches fails as stale.

const (
	runtimePath      = modulePrefix + "internal/runtime"
	unstructuredPath = "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	resultScrubDir   = "test/conformance/testdata/resultscrub"
)

// resultDataAllowlist maps "<file>:<function>" to why that function may put
// a server object in a result's Data. Each entry but the planners' is a
// read: the command shows what it read as the answer to a read, with no
// write behind it.
var resultDataAllowlist = map[string]string{
	"internal/engine/effect.go:PlanCreateObserved":                      "the create planner scrubs the committed object with the kind's redaction before it sets the result's Data",
	"internal/engine/effect.go:PlanUpdateObserved":                      "the update planner scrubs the committed object with the kind's redaction before it sets the result's Data",
	"internal/engine/resources/get.go:getCmd.Execute":                   "get shows the table of the objects it read; its json, yaml and template output removes what the kind's read redaction hides",
	"internal/engine/provisioning/cloudcred.go:getCloudCredCmd.Execute": "get cloud-credential builds its rows from the Secret's metadata and the names of its data keys, never their values",
	"internal/engine/inspect/deployment.go:deploymentCmd.Execute":       "inspect deployment is a read; the object it shows is one a caller may read",
	"internal/engine/inspect/pod.go:podCmd.Execute":                     "inspect pod is a read; the object it shows is one a caller may read",
	"internal/engine/inspect/user.go:userCmd.Execute":                   "inspect user is a read that copies named fields of the objects it reads",
	"internal/engine/inspect/project.go:projectCmd.Execute":             "inspect project is a read that copies named fields of the objects it reads",
	"internal/engine/access/clusters.go:clustersCmd.Execute":            "access clusters is a read that copies named fields of the objects it reads",
	"internal/engine/access/projects.go:projectsCmd.Execute":            "access projects is a read that copies named fields of the objects it reads",
	"internal/engine/observe/logs.go:logsCmd.Execute":                   "logs reads pods for their names; the result holds names and log lines only",
}

const resultDataRule = "a server object in the Data of a runtime.Result; let the planner scrub it with the kind's redaction (engine.PlanCreate, engine.PlanUpdate), or build Data from the fields the result needs"

// objectContentMethods are the methods of an unstructured value that return
// its content as a map, a slice, bytes or a copy.
var objectContentMethods = []string{"GetAnnotations", "GetLabels", "MarshalJSON", "UnstructuredContent", "DeepCopy", "DeepCopyObject"}

// isObjectType reports whether t is unstructured.Unstructured or
// unstructured.UnstructuredList, a pointer to either, or a slice of
// Unstructured.
func isObjectType(t types.Type) bool {
	if t == nil {
		return false
	}
	t = derefType(t)
	if s, ok := types.Unalias(t).(*types.Slice); ok {
		t = derefType(s.Elem())
	}
	return isNamedType(t, unstructuredPath, "Unstructured") || isNamedType(t, unstructuredPath, "UnstructuredList")
}

// isBasicType reports whether t is a string, a boolean or a number.
func isBasicType(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := types.Unalias(t).Underlying().(*types.Basic)
	return ok && b.Info()&(types.IsString|types.IsBoolean|types.IsNumeric) != 0
}

// resultScan is the state of the scan of one function: the types, and the
// function body whose assignments a local variable is traced through.
type resultScan struct {
	info *types.Info
	body ast.Node
}

// holds reports whether the expression tree of e contains a server object.
// visited holds the locals already traced for this Data expression, so a
// chain that loops (a = b; b = a) ends.
func (sc resultScan) holds(e ast.Expr, visited map[*types.Var]bool) bool {
	if e == nil {
		return false
	}
	if isObjectType(sc.info.TypeOf(e)) {
		return true
	}
	switch e := e.(type) {
	case *ast.Ident:
		return sc.variableHolds(e, visited)
	case *ast.SelectorExpr:
		if isObjectType(sc.info.TypeOf(e.X)) {
			return e.Sel.Name == "Object" || e.Sel.Name == "Items"
		}
		return sc.holds(e.X, visited)
	case *ast.CallExpr:
		if isBasicType(sc.info.TypeOf(e)) {
			return false
		}
		if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok && isObjectType(sc.info.TypeOf(sel.X)) {
			if slices.Contains(objectContentMethods, sel.Sel.Name) {
				return true
			}
		} else if sc.holds(e.Fun, visited) {
			return true
		}
		return slices.ContainsFunc(e.Args, func(a ast.Expr) bool { return sc.holds(a, visited) })
	case *ast.CompositeLit:
		return slices.ContainsFunc(e.Elts, func(a ast.Expr) bool { return sc.holds(a, visited) })
	case *ast.KeyValueExpr:
		return sc.holds(e.Key, visited) || sc.holds(e.Value, visited)
	case *ast.ParenExpr:
		return sc.holds(e.X, visited)
	case *ast.StarExpr:
		return sc.holds(e.X, visited)
	case *ast.UnaryExpr:
		return sc.holds(e.X, visited)
	case *ast.BinaryExpr:
		return sc.holds(e.X, visited) || sc.holds(e.Y, visited)
	case *ast.IndexExpr:
		if isBasicType(sc.info.TypeOf(e)) {
			return false
		}
		return sc.holds(e.X, visited) || sc.holds(e.Index, visited)
	case *ast.SliceExpr:
		return sc.holds(e.X, visited)
	case *ast.TypeAssertExpr:
		// A comma-ok assertion has a tuple type; the asserted type is the
		// one to test.
		if isBasicType(sc.info.TypeOf(e.Type)) {
			return false
		}
		return sc.holds(e.X, visited)
	}
	return false
}

// variableHolds reports whether id names a local variable, not of a basic
// type, that the enclosing function assigns an expression holding a server
// object, directly or through other locals.
func (sc resultScan) variableHolds(id *ast.Ident, visited map[*types.Var]bool) bool {
	v, ok := sc.info.Uses[id].(*types.Var)
	if !ok || v.IsField() || v.Parent() == nil || v.Pkg() == nil || v.Parent() == v.Pkg().Scope() || isBasicType(v.Type()) || visited[v] {
		return false
	}
	visited[v] = true
	is := func(e ast.Expr) bool {
		i, ok := ast.Unparen(e).(*ast.Ident)
		return ok && (sc.info.Uses[i] == v || sc.info.Defs[i] == v)
	}
	found := false
	ast.Inspect(sc.body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				target := is(lhs)
				if ix, ok := ast.Unparen(lhs).(*ast.IndexExpr); ok && is(ix.X) {
					target = true
				}
				if !target {
					continue
				}
				rhs := n.Rhs
				if len(n.Lhs) == len(n.Rhs) {
					rhs = n.Rhs[i : i+1]
				}
				if slices.ContainsFunc(rhs, func(r ast.Expr) bool { return sc.holds(r, visited) }) {
					found = true
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if !is(name) {
					continue
				}
				rhs := n.Values
				if len(n.Names) == len(n.Values) {
					rhs = n.Values[i : i+1]
				}
				if slices.ContainsFunc(rhs, func(r ast.Expr) bool { return sc.holds(r, visited) }) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// resultFinding is one hit of the Data rule.
type resultFinding struct {
	file     string
	function string
	line     int
}

// key is the allowlist key of the finding.
func (f resultFinding) key() string { return f.file + ":" + f.function }

func (f resultFinding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.file, f.line, f.function, resultDataRule)
}

// functionKey is the name of a function in a finding: Name, or
// Receiver.Name for a method, with the receiver's star and type arguments
// removed.
func functionKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fd.Name.Name
		}
		return fd.Name.Name
	}
}

// packageLevelFunction is the function name of a finding outside every
// function (a package variable).
const packageLevelFunction = "package-level"

// scanResultData returns the findings of the Data rule in pkg.
func scanResultData(pkg *packages.Package, wd string) []resultFinding {
	info := pkg.TypesInfo
	var out []resultFinding
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		rel = filepath.ToSlash(rel)
		isResult := func(e ast.Expr) bool {
			t := info.TypeOf(e)
			return t != nil && isNamedType(derefType(t), runtimePath, "Result")
		}
		for _, decl := range f.Decls {
			function := packageLevelFunction
			if fd, ok := decl.(*ast.FuncDecl); ok {
				function = functionKey(fd)
			}
			sc := resultScan{info: info, body: decl}
			report := func(n ast.Node) {
				out = append(out, resultFinding{file: rel, function: function, line: pkg.Fset.Position(n.Pos()).Line})
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Data" || !isResult(sel.X) {
							continue
						}
						rhs := n.Rhs
						if len(n.Lhs) == len(n.Rhs) {
							rhs = n.Rhs[i : i+1]
						}
						if slices.ContainsFunc(rhs, func(r ast.Expr) bool { return sc.holds(r, map[*types.Var]bool{}) }) {
							report(lhs)
						}
					}
				case *ast.CompositeLit:
					if !isResult(n) {
						return true
					}
					for _, elt := range n.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, _ := kv.Key.(*ast.Ident); key != nil && key.Name == "Data" && sc.holds(kv.Value, map[*types.Var]bool{}) {
							report(kv)
						}
					}
				}
				return true
			})
		}
	}
	slices.SortFunc(out, func(a, b resultFinding) int {
		return strings.Compare(fmt.Sprintf("%s:%06d", a.file, a.line), fmt.Sprintf("%s:%06d", b.file, b.line))
	})
	return out
}

// applyResultAllowlist returns the findings no allowlist entry covers and
// the keys of the entries that matched a finding.
func applyResultAllowlist(findings []resultFinding, allow map[string]string) (remaining []resultFinding, used map[string]bool) {
	used = map[string]bool{}
	for _, f := range findings {
		if _, ok := allow[f.key()]; ok {
			used[f.key()] = true
			continue
		}
		remaining = append(remaining, f)
	}
	return remaining, used
}

// TestNoCommandPutsAServerObjectInAResult asserts the Data rule finds
// nothing in the production tree outside the allowlisted functions, and
// that every allowlist entry matches a finding.
func TestNoCommandPutsAServerObjectInAResult(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for key, reason := range resultDataAllowlist {
		file, _, _ := strings.Cut(key, ":")
		_, err := os.Stat(file)
		require.NoError(t, err, "allowlist entry %s: no such file", key)
		require.NotEmpty(t, reason, key)
	}
	var findings []resultFinding
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, testrancherPath) {
			continue
		}
		findings = append(findings, scanResultData(pkg, wd)...)
	}
	remaining, used := applyResultAllowlist(findings, resultDataAllowlist)
	for _, f := range remaining {
		t.Error(f.String())
	}
	for key := range resultDataAllowlist {
		if !used[key] {
			t.Errorf("allowlist entry %s matches no finding: remove it", key)
		}
	}
}

// TestResultDataScanFixtures asserts the Data rule on type-checked
// fixtures, each with the functions it reports: an object, its Object map,
// a list, a list's Items and content, a method result that holds the
// object's content, an object nested in a literal or a conversion, and an
// object assigned to a local variable by :=, =, var and an index
// assignment are each reported; a result built from names and basic
// values, a look-alike type with a Data field, a read of one field and an
// index that yields a string are not; a chain of locals is followed until
// it reaches a local of a basic type; what the rule does not follow (a
// field assignment) is not reported.
func TestResultDataScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := loadPackages("./"+resultScrubDir+"/...", "")
	require.NoError(t, err)
	byName := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		byName[filepath.Base(pkg.PkgPath)] = pkg
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"assignobject", []string{"run"}},
		{"assignvalue", []string{"run", "run"}},
		{"nestedliteral", []string{"run"}},
		{"content", []string{"run"}},
		{"items", []string{"run"}},
		{"conversion", []string{"run"}},
		{"listvalue", []string{"run", "run", "run"}},
		{"listcontent", []string{"run", "run"}},
		{"methodresult", []string{"run", "run", "run", "run"}},
		{"localassign", []string{"viaDefine", "viaAssign", "viaVar", "viaIndex", "viaCall", "viaChain", "viaLoop", "viaTwoSites", "viaTwoSites", "viaCommaOk"}},
		{"basicresult", nil},
		{"receivers", []string{"second.Execute"}},
		{"pass", nil},
		{"localpass", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := byName[tc.fixture]
			require.True(t, ok, "no fixture package %s", tc.fixture)
			var got []string
			for _, f := range scanResultData(pkg, wd) {
				got = append(got, f.function)
			}
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("allowlist", func(t *testing.T) {
		t.Parallel()
		pkg := byName["assignobject"]
		require.NotNil(t, pkg)
		file := filepath.ToSlash(filepath.Join(resultScrubDir, "assignobject", "assignobject.go"))
		findings := scanResultData(pkg, wd)
		require.Len(t, findings, 1)
		assert.Equal(t, file+":run", findings[0].key())
		remaining, used := applyResultAllowlist(findings, map[string]string{"other.go:run": "why", file + ":other": "why"})
		assert.Len(t, remaining, 1, "an entry for another file or another function of the file covers nothing")
		assert.Empty(t, used)
		remaining, used = applyResultAllowlist(findings, map[string]string{file + ":run": "why"})
		assert.Empty(t, remaining)
		assert.Equal(t, map[string]bool{file + ":run": true}, used)
	})
}
