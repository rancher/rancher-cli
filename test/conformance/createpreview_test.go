package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Every create preview lists the fields its command sends, with the
// redacted values hidden. Two rules keep that true for every create, now
// and later:
//
//   - Every use of the shared create planners (engine.PlanCreate and
//     engine.PlanCreateObserved) and of the diff walk they call
//     (engine.CreateDiff) outside package engine is listed by the function
//     that holds it, in internal/cli/testdata/create-planner-callers.txt.
//     The table test in internal/cli requires one row per listed function,
//     and a row runs that command's Plan and asserts the fields it sets
//     appear in the preview. A new command that plans a create therefore
//     has no key and fails here, whatever its name.
//   - Outside the files that own the create preview, no non-test code
//     builds a create Effect, assigns an Effect's Diff or takes the
//     redaction of a resource that has no kind row. A command package
//     cannot drop or rewrite the diff the planner built.
//
// Both scans cover the production tree: every non-test Go file outside
// testdata, type-resolved with the default build tags and GOOS. A key is
// the package path below the module, the receiver type without "*" and the
// function name, joined by ".".

const (
	enginePath             = modulePrefix + "internal/engine"
	kindsPath              = modulePrefix + "internal/engine/kinds"
	safetyPath             = modulePrefix + "internal/runtime/safety"
	createPreviewDir       = "test/conformance/testdata/createpreview"
	createPlannerCallers   = "internal/cli/testdata/create-planner-callers.txt"
	rowlessRedactionFunc   = "RowlessRedaction"
	packageLevelKeySegment = "package-level"
)

// createPlannerFuncs are the engine functions the caller key file covers.
var createPlannerFuncs = []string{"PlanCreate", "PlanCreateObserved", "CreateDiff"}

// createEffectAllowlist maps a file to why it may build a create Effect,
// assign a diff or name the row-less redaction constructor. Each entry is a
// user decision.
var createEffectAllowlist = map[string]string{
	"internal/engine/effect.go":                             "the shared planners: they build the create Effect and set its diff from the walk",
	"internal/engine/diff.go":                               "the create diff walk and the update diff",
	"internal/engine/redact.go":                             "the one function that hides values in a diff",
	"internal/engine/extres/createpasswordchangerequest.go": "the password change request has no kind row; it builds its own Effect and takes the diff from the walk with its own redaction",
}

// plannerCallKeys returns the sorted keys of the functions of pkg that use a
// create planner or the walk: a call, or any other reference to one. A use in
// a closure belongs to the function that holds it, and a use outside every
// function (a package variable) has the key "<package>.package-level".
func plannerCallKeys(pkg *packages.Package) []string {
	info := pkg.TypesInfo
	base := strings.TrimPrefix(pkg.PkgPath, modulePrefix)
	found := map[string]bool{}
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		for _, decl := range f.Decls {
			key := base + "." + packageLevelKeySegment
			if fd, ok := decl.(*ast.FuncDecl); ok {
				key = base + "." + fd.Name.Name
				if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
					if recv := receiverName(fn); recv != "" {
						key = base + "." + recv + "." + fd.Name.Name
					}
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				fn, ok := info.Uses[id].(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != enginePath || !slices.Contains(createPlannerFuncs, fn.Name()) {
					return true
				}
				if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
					return true
				}
				found[key] = true
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(found))
}

// readPlannerCallerKeys reads the key file: one key per line, no blank
// lines and no comments, sorted.
func readPlannerCallerKeys(file string) ([]string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var keys []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" || strings.TrimSpace(line) != line {
			return nil, fmt.Errorf("%s: want one key per line with no blank line or padding, got %q", file, line)
		}
		keys = append(keys, line)
	}
	if !slices.IsSorted(keys) || len(slices.Compact(slices.Clone(keys))) != len(keys) {
		return nil, fmt.Errorf("%s: keys must be sorted and unique", file)
	}
	return keys, nil
}

// comparePlannerCallers describes how the keys the scan found differ from
// the keys the file lists.
func comparePlannerCallers(found, listed []string) []string {
	var out []string
	for _, k := range found {
		if !slices.Contains(listed, k) {
			out = append(out, "a create planner is used in "+k+", which the key file does not list: add the key and a row for the command to the create table in internal/cli")
		}
	}
	for _, k := range listed {
		if !slices.Contains(found, k) {
			out = append(out, "the key file lists "+k+", which uses no create planner: remove the key and its row")
		}
	}
	return out
}

// TestEveryCreatePlannerCallIsListed asserts the key file lists exactly the
// functions outside package engine that use a create planner or the walk.
func TestEveryCreatePlannerCallIsListed(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	var found []string
	for _, pkg := range pkgs {
		if pkg.PkgPath == enginePath {
			continue
		}
		found = append(found, plannerCallKeys(pkg)...)
	}
	slices.Sort(found)
	listed, err := readPlannerCallerKeys(createPlannerCallers)
	require.NoError(t, err)
	require.NotEmpty(t, found)
	for _, msg := range comparePlannerCallers(found, listed) {
		t.Error(msg)
	}
}

// createPreviewFixtures type-checks the fixture packages once.
var createPreviewFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./" + createPreviewDir + "/...")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(path.Dir(pkg.PkgPath))+"/"+path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// TestPlannerCallScanFixtures asserts the scan on type-checked fixtures: a
// method, a pointer method, a function, a closure, a function value and a
// package variable are each keyed by the function that holds the use; two
// uses make one key; a local function of the same name and the update
// planner make none.
func TestPlannerCallScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := createPreviewFixtures()
	require.NoError(t, err)
	key := func(name, rest string) string {
		return createPreviewDirKey(name) + "." + rest
	}
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"method", []string{key("method", "cmd.Plan")}},
		{"pointermethod", []string{key("pointermethod", "cmd.Plan")}},
		{"observed", []string{key("observed", "cmd.Plan")}},
		{"walk", []string{key("walk", "cmd.Plan")}},
		{"function", []string{key("function", "build")}},
		{"closure", []string{key("closure", "cmd.Plan")}},
		{"twice", []string{key("twice", "cmd.Plan")}},
		{"packagelevel", []string{key("packagelevel", packageLevelKeySegment)}},
		{"othercalls", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs["planner/"+tc.fixture]
			require.True(t, ok, "no fixture package %s", tc.fixture)
			assert.Equal(t, tc.want, plannerCallKeys(pkg))
		})
	}
}

// createPreviewDirKey is the key prefix of a planner fixture package.
func createPreviewDirKey(name string) string {
	return createPreviewDir + "/planner/" + name
}

// TestPlannerCallerKeyFileRules asserts the comparison: a use with no key, a
// key with no use, and a matching pair.
func TestPlannerCallerKeyFileRules(t *testing.T) {
	t.Parallel()
	assert.Empty(t, comparePlannerCallers([]string{"a.b.c"}, []string{"a.b.c"}))
	missing := comparePlannerCallers([]string{"a.b.c", "x.y"}, []string{"a.b.c"})
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "used in x.y, which the key file does not list")
	stale := comparePlannerCallers([]string{"a.b.c"}, []string{"a.b.c", "x.y"})
	require.Len(t, stale, 1)
	assert.Contains(t, stale[0], "lists x.y, which uses no create planner")

	dir := t.TempDir()
	for name, content := range map[string]string{"blank.txt": "a\n\nb\n", "unsorted.txt": "b\na\n", "duplicate.txt": "a\na\n", "padded.txt": " a\n"} {
		file := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
		_, err := readPlannerCallerKeys(file)
		require.Error(t, err, name)
	}
	ok := filepath.Join(dir, "ok.txt")
	require.NoError(t, os.WriteFile(ok, []byte("a.b\nc.d\n"), 0o600))
	keys, err := readPlannerCallerKeys(ok)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.b", "c.d"}, keys)
}

// scanCreateEffects returns the findings of the Effect rules in pkg, each as
// "<file>:<line>: <rule>": a safety.Effect literal whose Action is the
// constant "create" or with no keys, a Diff key in a safety.Effect literal,
// an assignment to the Diff of a safety.Effect or to its Action of the
// constant "create" (a multi-value assignment to Action is refused too; an
// assignment of a variable to Action and a literal with a non-constant
// Action are not scanned), an assignment to an element or an element field
// of that Diff, a call of clear on it, a call of append with it as the first
// argument, and any use of kinds.RowlessRedaction outside package kinds. A
// file in allow is skipped. Not scanned: copy, calls of package slices,
// writes through a local alias of the Diff, slices of the Diff (clear or
// append of eff.Diff[:n], an element of such a slice), writes into an
// entry's value, and range assignments.
func scanCreateEffects(pkg *packages.Package, wd string, allow map[string]string) []string {
	info := pkg.TypesInfo
	var out []string
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
		if _, ok := allow[rel]; ok {
			continue
		}
		report := func(pos token.Pos, rule string) {
			out = append(out, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(pos).Line, rule))
		}
		isEffect := func(e ast.Expr) bool {
			t := info.TypeOf(e)
			if t == nil {
				return false
			}
			return isNamedType(derefType(t), safetyPath, "Effect")
		}
		isEffectDiff := func(e ast.Expr) bool {
			sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
			return ok && sel.Sel.Name == "Diff" && isEffect(sel.X)
		}
		// diffElementWrite reports whether e addresses an element of the
		// Diff of a safety.Effect, or a field of one.
		diffElementWrite := func(e ast.Expr) bool {
			for {
				switch t := ast.Unparen(e).(type) {
				case *ast.IndexExpr:
					if isEffectDiff(t.X) {
						return true
					}
					e = t.X
				case *ast.SelectorExpr:
					e = t.X
				case *ast.StarExpr:
					e = t.X
				default:
					return false
				}
			}
		}
		isCreate := func(e ast.Expr) bool {
			v, ok := constString(info, e)
			return ok && v == "create"
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if !isEffect(n) {
					return true
				}
				for _, elt := range n.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						report(n.Pos(), "safety.Effect literal without field names; name each field so the scan can read it")
						break
					}
					key, _ := kv.Key.(*ast.Ident)
					switch {
					case key != nil && key.Name == "Action" && isCreate(kv.Value):
						report(kv.Pos(), "safety.Effect literal with the create action; plan a create with engine.PlanCreate")
					case key != nil && key.Name == "Diff":
						report(kv.Pos(), "Diff key in a safety.Effect literal; the planner builds the diff of a create")
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if diffElementWrite(lhs) {
						report(lhs.Pos(), "assignment to an element of the Diff of a safety.Effect; the planner builds the diff of a create")
						continue
					}
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok || !isEffect(sel.X) {
						continue
					}
					switch sel.Sel.Name {
					case "Diff":
						report(lhs.Pos(), "assignment to the Diff of a safety.Effect; the planner builds the diff of a create")
					case "Action":
						if len(n.Lhs) != len(n.Rhs) || isCreate(n.Rhs[i]) {
							report(lhs.Pos(), "assignment of the create action to a safety.Effect; plan a create with engine.PlanCreate")
						}
					}
				}
			case *ast.CallExpr:
				id, ok := ast.Unparen(n.Fun).(*ast.Ident)
				if !ok || len(n.Args) == 0 || !isEffectDiff(n.Args[0]) {
					return true
				}
				if b, ok := info.Uses[id].(*types.Builtin); ok {
					switch b.Name() {
					case "clear":
						report(n.Pos(), "clear of the Diff of a safety.Effect; the planner builds the diff of a create")
					case "append":
						report(n.Pos(), "append to the Diff of a safety.Effect; the planner builds the diff of a create")
					}
				}
			case *ast.Ident:
				fn, ok := info.Uses[n].(*types.Func)
				if ok && fn.Name() == rowlessRedactionFunc && fn.Pkg() != nil && fn.Pkg().Path() == kindsPath && pkg.PkgPath != kindsPath {
					report(n.Pos(), "use of kinds.RowlessRedaction; take the redaction of a resource with a kind row from the row")
				}
			}
			return true
		})
	}
	slices.Sort(out)
	return out
}

// TestNoCommandBuildsACreateEffectOrItsDiff asserts the Effect rules find
// nothing in the production tree outside the allowlisted files.
func TestNoCommandBuildsACreateEffectOrItsDiff(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for file := range createEffectAllowlist {
		_, err := os.Stat(file)
		require.NoError(t, err, "allowlist entry %s: no such file", file)
	}
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, testrancherPath) {
			continue
		}
		for _, finding := range scanCreateEffects(pkg, wd, createEffectAllowlist) {
			t.Error(finding)
		}
	}
}

// TestCreateEffectScanFixtures asserts each Effect rule on type-checked
// fixtures: a literal with the create action (a named constant too), a Diff
// key, an unkeyed literal, an assignment to Diff and to Action, a write to a
// Diff element or element field, clear and append on the Diff, and a use of
// the row-less constructor are each reported; update and delete literals,
// reads of Diff and look-alike types are not; an allowlisted file is
// skipped.
func TestCreateEffectScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := createPreviewFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	const (
		create   = "safety.Effect literal with the create action; plan a create with engine.PlanCreate"
		diffKey  = "Diff key in a safety.Effect literal; the planner builds the diff of a create"
		unkeyed  = "safety.Effect literal without field names; name each field so the scan can read it"
		diffSet  = "assignment to the Diff of a safety.Effect; the planner builds the diff of a create"
		actionSe = "assignment of the create action to a safety.Effect; plan a create with engine.PlanCreate"
		element  = "assignment to an element of the Diff of a safety.Effect; the planner builds the diff of a create"
		clearing = "clear of the Diff of a safety.Effect; the planner builds the diff of a create"
		appendTo = "append to the Diff of a safety.Effect; the planner builds the diff of a create"
		rowless  = "use of kinds.RowlessRedaction; take the redaction of a resource with a kind row from the row"
	)
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"createliteral", []string{create}},
		{"createconstant", []string{create}},
		{"diffliteral", []string{diffKey}},
		{"unkeyed", []string{unkeyed}},
		{"assigndiff", []string{diffSet, diffSet}},
		{"assignaction", []string{actionSe}},
		{"assignelement", []string{element, element, element}},
		{"cleardiff", []string{clearing}},
		{"appenddiff", []string{appendTo}},
		{"rowless", []string{rowless}},
		{"pass", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs["effect/"+tc.fixture]
			require.True(t, ok, "no fixture package %s", tc.fixture)
			var got []string
			for _, f := range scanCreateEffects(pkg, wd, nil) {
				_, rest, ok := strings.Cut(f, ".go:")
				require.True(t, ok, f)
				got = append(got, strings.TrimSpace(strings.TrimLeft(rest, "0123456789:")))
			}
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("allowlist", func(t *testing.T) {
		t.Parallel()
		pkg := pkgs["effect/createliteral"]
		require.NotNil(t, pkg)
		file := filepath.ToSlash(filepath.Join(createPreviewDir, "effect", "createliteral", "createliteral.go"))
		assert.NotEmpty(t, scanCreateEffects(pkg, wd, map[string]string{"other.go": "why"}))
		assert.Empty(t, scanCreateEffects(pkg, wd, map[string]string{file: "why"}))
	})
}
