package conformance

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
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

	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// A lookup of a cluster or a project by id reports a 403 as RBAC, never as
// NOT_FOUND: Rancher refuses (HTTP 403) the get of a cluster or project by its
// id both for one the caller cannot read and for one that does not exist. Only
// the caller's own list can tell them apart, and a 403 does not mean the
// object is missing. Two scans look for the places that could break the rule:
//
//   - a check for a 403 and a 404 together in one expression or one case
//     clause (http.StatusForbidden, the constant 403 or clierr.CodeRBAC beside
//     http.StatusNotFound, the constant 404 or clierr.CodeNotFound) outside
//     byIDTogetherStatusAllowlist, which gives the function and why the check
//     is not a lookup of a cluster or project;
//   - a get of a cluster or project by its id: a steve.Request with a Name, an
//     assignment to its Name, a string that has a clusters or projects
//     path with an id after it, a kinds.Lookup or kinds.ByKey whose literal
//     argument names a row with the management.cattle.io clusters or
//     projects GVR (every name, key and alias of those rows) or whose
//     argument is not a literal, when the same function calls Get, and a
//     schema.GroupVersionResource literal for those resources, outside
//     byIDReadAllowlist, which names the test function that asserts the 403
//     of that function or why it is no lookup of a cluster or project.
//
// An entry the scan no longer finds fails, and so does an entry that names a
// test function that is gone. The scans type-check the module for the host
// platform only.
//
// The first scan sees only a 403 and a 404 checked in one expression or one
// case clause. It does not see them handled by separate if statements or
// separate case clauses that give the same result, by fallthrough, or by a
// call such as slices.Contains([]int{403, 404}, status); the second scan and
// the tests of each request in the packages the allowlist names are what catch
// a lookup written that way. The second scan does not see a get of a cluster
// or project by its id through a type or a path it cannot resolve statically (a
// path built from parts, a request type chosen at run time). Its check for a
// GroupVersionResource does not see a literal whose Group or Resource is not a
// constant, a GroupVersion{...}.WithResource("clusters") call, an assignment
// to a field of a GroupVersionResource, or kinds.Lookup called through a
// function value. The scan does not find such a site. Whoever adds one has to
// check its 403 and add the site to byIDReadAllowlist.

const (
	byIDStevePath    = modulePrefix + "internal/engine/steve"
	byIDKindsPath    = modulePrefix + "internal/engine/kinds"
	byIDSchemaPath   = "k8s.io/apimachinery/pkg/runtime/schema"
	byIDFixtureDir   = "test/conformance/testdata/byidforbidden"
	byIDTogetherRule = "a check for a 403 and a 404 together in one expression or case clause; return an RBAC error for a 403 on a cluster or project lookup, or add the function to byIDTogetherStatusAllowlist with the reason"
	byIDLiteralRule  = "steve.Request with a Name gets one object by its id; return an RBAC error when Rancher refuses (HTTP 403) to get it by its id, or add the function to byIDReadAllowlist"
	byIDAssignRule   = "assignment to the Name of a steve.Request gets one object by its id; return an RBAC error when Rancher refuses (HTTP 403) to get it by its id, or add the function to byIDReadAllowlist"
	byIDPathRule     = "a string with a clusters or projects path and an id after it; return an RBAC error when Rancher refuses (HTTP 403) to get it by its id, or add the function to byIDReadAllowlist"
	byIDLookupRule   = "kinds.Lookup or kinds.ByKey of a cluster, a project or a kind chosen at run time in a function that calls Get; return an RBAC error when Rancher refuses (HTTP 403) to get it by its id, or add the function to byIDReadAllowlist"
	byIDGVRRule      = "a management.cattle.io clusters or projects GroupVersionResource; return an RBAC error when Rancher refuses (HTTP 403) to get the cluster or project by its id with it, or add the function to byIDReadAllowlist"
)

// byIDAllowance is the allowlist entry for the matches in one function. It
// names either the test function that asserts the function's 403
// ("<package directory>:<test function>") or the reason the matches are not a
// lookup of a cluster or project.
type byIDAllowance struct {
	count  int
	row    string
	reason string
}

// byIDTogetherStatusAllowlist maps "<file>:<function>" to the 403 and 404
// checks together it covers.
var byIDTogetherStatusAllowlist = map[string]byIDAllowance{
	"internal/engine/steve/userattributes.go:(*UserAttributes).List": {1, "", "user attributes, not a cluster or project lookup; the refusal is refusedList, never NOT_FOUND"},
	"internal/telemetry/refresh.go:answerOutcome":                    {1, "", "the status of an install id or version request, not a cluster or project lookup"},
	"internal/engine/resources/steveread.go:steveRefused": {1, "internal/engine/resources:TestGetThroughSteveReturnsTheKubernetesErrorWhenSteveRefuses",
		"reports a Steve 403 or 404; its callers return the Kubernetes API's own 403 error (RBAC) for either; only a Steve 404 while waiting becomes NOT_FOUND"},
	"internal/testrancher/testrancher.go:deterministic4xx": {1, "", "whether a status proves a request had no effect on the server; not a cluster or project lookup"},
}

// byIDReadAllowlist maps "<file>:<function>" to the gets by id it covers.
var byIDReadAllowlist = map[string]byIDAllowance{
	"internal/engine/steve/cluster.go:ResolveCluster":     {1, "internal/engine/steve:TestClusterLookupsRefusedByIDAreRBAC", ""},
	"internal/engine/steve/cluster.go:GetCluster":         {1, "internal/engine/steve:TestClusterLookupsRefusedByIDAreRBAC", ""},
	"internal/engine/steve/objects.go:GetClusterObject":   {1, "internal/engine/steve:TestClusterLookupsRefusedByIDAreRBAC", ""},
	"internal/engine/steve/objects.go:GetProject":         {1, "internal/engine/steve:TestGetProjectKeepsTheStatusOfARefusedRead", ""},
	"internal/engine/steve/projects.go:resolveProject":    {1, "internal/engine/steve:TestProjectLookupsRefusedByIDAreRBAC", ""},
	"internal/engine/steve/objects.go:GetTypeObject":      {1, "", "a provisioning cluster or etcd snapshot by name, not a cluster or project"},
	"internal/engine/steve/objects.go:GetNamespaceObject": {1, "internal/engine/resources:TestGetNamespaceOneReadsThroughSteveAfterAForbiddenGet", ""},
	"internal/engine/nodes/target.go:readSteveNode":       {1, "internal/engine/nodes:TestDrainPlanSteveReads", ""},

	"internal/engine/inspect/project.go:(*projectCmd).Execute": {1, "", "a read after a resolution; the status table classifies its 403 as RBAC"},
	"internal/engine/inspect/user.go:projectDisplayName":       {1, "", "a read after a resolution; the status table classifies its 403 as RBAC"},
	"internal/engine/projects/resolve.go:BackingNamespace":     {1, "", "a read after a resolution; the status table classifies its 403 as RBAC"},

	"internal/engine/kinds/kinds.go:(package level)":       {2, "", "the kind table: the rows that Lookup and ByKey return, which the scan follows at each call"},
	"internal/engine/projects/projects.go:(package level)": {1, "", "the resource a project create uses, no get by id"},

	"internal/engine/inspect/usage.go:checkEmptyScope":                        {1, "", "namespace and node reads, not a cluster or project"},
	"internal/engine/rbac/grant.go:checkRole":                                 {1, "", "a role template read, not a cluster or project"},
	"internal/engine/resources/deleteproject.go:waitForBackingNamespace":      {1, "", "a namespace read, not a cluster or project"},
	"internal/engine/steve/userattributes.go:(*UserAttributes).Get":           {1, "", "a user attribute get by id, not a cluster or project"},
	"internal/engine/steve/userattributes.go:(*UserAttributes).schemaVisible": {1, "", "a schema read, not a cluster or project"},
	"internal/engine/catalog/action.go:actionRequest":                         {1, "", "a chart repo or release by id, not a cluster or project"},
	"internal/engine/catalog/clustervalues.go:readSetting":                    {1, "", "a setting by id, not a cluster or project"},
	"internal/engine/catalog/index.go:ReadIndex":                              {1, "", "a chart repo by id, not a cluster or project"},
	"internal/engine/catalog/index.go:ReadInfo":                               {1, "", "a chart repo by id, not a cluster or project"},
	"internal/engine/catalog/release.go:readReleaseValues":                    {1, "", "a release secret by id, not a cluster or project"},

	"internal/testrancher/testrancher.go:(*Facade).dispatch":                         {2, "", "the test server's own routes for a cluster object, not a request the CLI sends"},
	"internal/testrancher/testrancher.go:projectsCollectionCluster":                  {1, "", "the test server's own route parsing, not a request the CLI sends"},
	"internal/testrancher/fidelity/callerscope_probes.go:callerScopeProbes":          {5, "", "probe paths recorded against a Rancher the fidelity suite boots, not a lookup the CLI makes"},
	"internal/testrancher/fidelity/projectlookup_probes.go:projectLookupProbes":      {5, "", "probe paths recorded against a Rancher the fidelity suite boots, not a lookup the CLI makes"},
	"internal/testrancher/fidelity/extension_probes.go:extensionProbes":              {1, "", "the local cluster path recorded against a Rancher the fidelity suite boots, not a lookup the CLI makes"},
	"internal/testrancher/fidelity/catalogcleanup.go:(catalogCleanup).clusterValues": {2, "", "reads of the downstream cluster the fidelity suite boots, not a lookup the CLI makes"},
}

// byIDFinding is one place a scan matched.
type byIDFinding struct {
	key  string
	text string
}

// byIDFunctionName names fd the way the allowlists do: Name, or
// (*Type).Name and (Type).Name for a method.
func byIDFunctionName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	switch t := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fd.Name.Name
		}
	case *ast.Ident:
		return "(" + t.Name + ")." + fd.Name.Name
	case *ast.IndexExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(" + id.Name + ")." + fd.Name.Name
		}
	}
	return fd.Name.Name
}

// byIDStatusMark says which status a constant expression tests: 403 for
// http.StatusForbidden, the integer 403 and clierr.CodeRBAC, 404 for
// http.StatusNotFound, the integer 404 and clierr.CodeNotFound, else 0.
func byIDStatusMark(info *types.Info, e ast.Expr) int {
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.Int {
		switch tv.Value.ExactString() {
		case "403":
			return 403
		case "404":
			return 404
		}
	}
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		id = x.Sel
	case *ast.Ident:
		id = x
	default:
		return 0
	}
	c, ok := info.Uses[id].(*types.Const)
	if !ok || c.Pkg() == nil || c.Pkg().Path() != clierrPath {
		return 0
	}
	switch c.Name() {
	case "CodeRBAC":
		return 403
	case "CodeNotFound":
		return 404
	}
	return 0
}

// byIDMarksIn returns the set of statuses (403 and 404) tested anywhere in the
// expressions.
func byIDMarksIn(info *types.Info, exprs ...ast.Expr) map[int]bool {
	marks := map[int]bool{}
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if x, ok := n.(ast.Expr); ok {
				if m := byIDStatusMark(info, x); m != 0 {
					marks[m] = true
				}
			}
			return true
		})
	}
	return marks
}

// byIDOperands flattens the || chain e into its operands.
func byIDOperands(e ast.Expr) []ast.Expr {
	if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.LOR {
		return append(byIDOperands(b.X), byIDOperands(b.Y)...)
	}
	return []ast.Expr{e}
}

// byIDRel returns the slash path of the file of pos relative to wd.
func byIDRel(pkg *packages.Package, pos token.Pos, wd string) string {
	name := pkg.Fset.Position(pos).Filename
	rel, err := filepath.Rel(wd, name)
	if err != nil {
		rel = name
	}
	return filepath.ToSlash(rel)
}

// byIDFunctions calls visit for every top-level declaration of every
// non-test file of pkg, with the key prefix of the declaration.
func byIDFunctions(pkg *packages.Package, wd string, visit func(key string, decl ast.Decl)) {
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		rel := byIDRel(pkg, f.Pos(), wd)
		for _, decl := range f.Decls {
			name := "(package level)"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				name = byIDFunctionName(fd)
			}
			visit(rel+":"+name, decl)
		}
	}
}

// scanByIDTogetherStatus returns the places in the non-test files of pkg that
// check a 403 and a 404 together.
func scanByIDTogetherStatus(pkg *packages.Package, wd string) []byIDFinding {
	info := pkg.TypesInfo
	var out []byIDFinding
	byIDFunctions(pkg, wd, func(key string, decl ast.Decl) {
		inner := map[*ast.BinaryExpr]bool{}
		file, _, _ := strings.Cut(key, ":")
		report := func(pos token.Pos) {
			out = append(out, byIDFinding{key, fmt.Sprintf("%s:%d: %s", file, pkg.Fset.Position(pos).Line, byIDTogetherRule)})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.LOR || inner[n] {
					break
				}
				marks := byIDMarksIn(info, byIDOperands(n)...)
				if marks[403] && marks[404] {
					report(n.Pos())
				}
				var mark func(e ast.Expr)
				mark = func(e ast.Expr) {
					if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.LOR {
						inner[b] = true
						mark(b.X)
						mark(b.Y)
					}
				}
				mark(n.X)
				mark(n.Y)
			case *ast.CaseClause:
				// A single expression that has both is reported as that
				// expression.
				together := false
				for _, e := range n.List {
					if m := byIDMarksIn(info, e); m[403] && m[404] {
						together = true
					}
				}
				if m := byIDMarksIn(info, n.List...); m[403] && m[404] && !together {
					report(n.Pos())
				}
			}
			return true
		})
	})
	return out
}

// byIDIsNamed reports whether t, or what it points to, is the named type name
// of pkgPath.
func byIDIsNamed(t types.Type, pkgPath, name string) bool {
	return t != nil && isNamedType(derefType(t), pkgPath, name)
}

// byIDNameField reports whether sel selects the Name field of a
// steve.Request.
func byIDNameField(info *types.Info, sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "Name" {
		return false
	}
	s, ok := info.Selections[sel]
	return ok && s.Kind() == types.FieldVal && byIDIsNamed(s.Recv(), byIDStevePath, "Request")
}

// byIDEmptyString reports whether e is the constant empty string.
func byIDEmptyString(info *types.Info, e ast.Expr) bool {
	v, ok := constString(info, e)
	return ok && v == ""
}

// byIDManagementRow reports whether k is a row for the management.cattle.io
// clusters or projects resource.
func byIDManagementRow(k kinds.Kind) bool {
	return byIDManagementResource(k.GVR.Group, k.GVR.Resource)
}

// byIDManagementResource reports whether group and resource name the
// management.cattle.io clusters or projects.
func byIDManagementResource(group, resource string) bool {
	return group == "management.cattle.io" && (resource == "clusters" || resource == "projects")
}

// byIDLookupCall reports whether call is kinds.Lookup or kinds.ByKey and
// whether it is one the rule covers: its argument is a string literal that
// names a row of the management.cattle.io clusters or projects, or is no
// string literal.
func byIDLookupCall(info *types.Info, call *ast.CallExpr) (isLookup, covered bool) {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		id = f.Sel
	case *ast.Ident:
		id = f
	default:
		return false, false
	}
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != byIDKindsPath || (fn.Name() != "Lookup" && fn.Name() != "ByKey") || len(call.Args) != 1 {
		return false, false
	}
	arg := ast.Unparen(call.Args[0])
	if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v := constant.StringVal(info.Types[lit].Value)
		row, found := kinds.Lookup(v)
		if fn.Name() == "ByKey" {
			row, found = kinds.ByKey(v)
		}
		return true, found && byIDManagementRow(row)
	}
	return true, true
}

// byIDGVRLiteral reports whether lit is a schema.GroupVersionResource of the
// management.cattle.io clusters or projects.
func byIDGVRLiteral(info *types.Info, lit *ast.CompositeLit) bool {
	if !byIDIsNamed(info.TypeOf(lit), byIDSchemaPath, "GroupVersionResource") {
		return false
	}
	var group, resource string
	for i, elt := range lit.Elts {
		name, value := "", elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok {
				name = id.Name
			}
			value = kv.Value
		} else {
			name = [...]string{"Group", "Version", "Resource"}[min(i, 2)]
		}
		v, _ := constString(info, value)
		switch name {
		case "Group":
			group = v
		case "Resource":
			resource = v
		}
	}
	return byIDManagementResource(group, resource)
}

// scanByIDReads returns the gets by id of a cluster or project in the
// non-test files of pkg.
func scanByIDReads(pkg *packages.Package, wd string) []byIDFinding {
	info := pkg.TypesInfo
	var out []byIDFinding
	byIDFunctions(pkg, wd, func(key string, decl ast.Decl) {
		file, _, _ := strings.Cut(key, ":")
		report := func(pos token.Pos, rule string) {
			out = append(out, byIDFinding{key, fmt.Sprintf("%s:%d: %s", file, pkg.Fset.Position(pos).Line, rule)})
		}
		var lookups []*ast.CallExpr
		callsGet := false
		seenStrings := map[ast.Expr]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if byIDGVRLiteral(info, n) {
					report(n.Pos(), byIDGVRRule)
					break
				}
				if !byIDIsNamed(info.TypeOf(n), byIDStevePath, "Request") {
					break
				}
				st, ok := derefType(info.TypeOf(n)).Underlying().(*types.Struct)
				if !ok {
					break
				}
				for i, elt := range n.Elts {
					name, value := "", elt
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							name = id.Name
						}
						value = kv.Value
					} else if i < st.NumFields() {
						name = st.Field(i).Name()
					}
					if name == "Name" && !byIDEmptyString(info, value) {
						report(n.Pos(), byIDLiteralRule)
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok && byIDNameField(info, sel) {
						report(lhs.Pos(), byIDAssignRule)
					}
				}
			case *ast.BinaryExpr, *ast.BasicLit:
				e := n.(ast.Expr)
				if seenStrings[e] {
					break
				}
				v, ok := constString(info, e)
				if !ok || (!strings.Contains(v, "management.cattle.io.clusters/") && !strings.Contains(v, "management.cattle.io.projects/")) {
					break
				}
				report(e.Pos(), byIDPathRule)
				ast.Inspect(e, func(m ast.Node) bool {
					if x, ok := m.(ast.Expr); ok {
						seenStrings[x] = true
					}
					return true
				})
			case *ast.CallExpr:
				if isLookup, covered := byIDLookupCall(info, n); isLookup && covered {
					lookups = append(lookups, n)
				}
				if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" {
					callsGet = true
				}
			}
			return true
		})
		if callsGet {
			for _, call := range lookups {
				report(call.Pos(), byIDLookupRule)
			}
		}
	})
	return out
}

// byIDFixtures type-checks the fixture packages once.
var byIDFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+byIDFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// byIDTestFunctionExists reports whether the test files of the package
// directory dir declare the function name.
func byIDTestFunctionExists(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	for _, p := range files {
		if path.Dir(p) != dir || !strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.FromSlash(p), nil, parser.SkipObjectResolution)
		require.NoError(t, err, p)
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// byIDCheck returns one message per finding outside the allowlist, one per
// entry whose count differs from the findings it covers, and one per entry
// that matches nothing.
func byIDCheck(findings []byIDFinding, allow map[string]byIDAllowance) []string {
	matches := make([]requestMatch, len(findings))
	for i, f := range findings {
		matches[i] = requestMatch(f)
	}
	counts := map[string]requestAllowance{}
	for key, a := range allow {
		counts[key] = requestAllowance{a.count, cmp.Or(a.row, a.reason)}
	}
	return checkRequestMatches(matches, counts)
}

// byIDRepositoryFindings runs scan on every non-test file of the module.
func byIDRepositoryFindings(t *testing.T, scan func(*packages.Package, string) []byIDFinding) []byIDFinding {
	t.Helper()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var out []byIDFinding
	for _, pkg := range pkgs {
		out = append(out, scan(pkg, wd)...)
	}
	return out
}

// requireByIDAllowlistReasons asserts every entry names a test function that
// exists or gives a reason.
func requireByIDAllowlistReasons(t *testing.T, allow map[string]byIDAllowance) {
	t.Helper()
	for key, a := range allow {
		assert.True(t, a.row != "" || a.reason != "", "%s: give a test function, a reason or both", key)
		assert.Positive(t, a.count, key)
		if a.row == "" {
			continue
		}
		dir, name, ok := strings.Cut(a.row, ":")
		require.True(t, ok, "%s: %q must be <package directory>:<test function>", key, a.row)
		assert.True(t, byIDTestFunctionExists(t, dir, name), "%s: test function %s is not declared in %s", key, name, dir)
	}
}

// TestNoForbiddenAndNotFoundCheckedTogetherOutsideTheAllowlist asserts no
// production code checks a 403 and a 404 together except where the allowlist
// says it is no lookup of a cluster or project.
func TestNoForbiddenAndNotFoundCheckedTogetherOutsideTheAllowlist(t *testing.T) {
	t.Parallel()
	for _, problem := range byIDCheck(byIDRepositoryFindings(t, scanByIDTogetherStatus), byIDTogetherStatusAllowlist) {
		t.Error(problem)
	}
	requireByIDAllowlistReasons(t, byIDTogetherStatusAllowlist)
}

// TestEveryReadOfAClusterOrProjectByIDIsInTheAllowlist asserts every read by
// id of a cluster or project is in the allowlist with the test function that
// asserts its 403, or with the reason it is no lookup.
func TestEveryReadOfAClusterOrProjectByIDIsInTheAllowlist(t *testing.T) {
	t.Parallel()
	for _, problem := range byIDCheck(byIDRepositoryFindings(t, scanByIDReads), byIDReadAllowlist) {
		t.Error(problem)
	}
	requireByIDAllowlistReasons(t, byIDReadAllowlist)
}

// byIDFixtureCases lists every fixture package and the rules the scan named
// in scan reports for it.
var byIDFixtureCases = []struct {
	fixture string
	scan    func(*packages.Package, string) []byIDFinding
	want    []string
}{
	{"togetherstatus", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"togethercode", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"togetherliteral", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"togetherthree", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"togethercase", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"togethermixed", scanByIDTogetherStatus, []string{byIDTogetherRule}},
	{"forbiddenonly", scanByIDTogetherStatus, nil},
	{"forbiddenorother", scanByIDTogetherStatus, nil},
	{"separatetests", scanByIDTogetherStatus, nil},
	{"requestliteral", scanByIDReads, []string{byIDLiteralRule}},
	{"requestpositional", scanByIDReads, []string{byIDLiteralRule}},
	{"requestassign", scanByIDReads, []string{byIDAssignRule}},
	{"requestnoname", scanByIDReads, nil},
	{"requestemptyname", scanByIDReads, nil},
	{"pathclusters", scanByIDReads, []string{byIDPathRule}},
	{"pathprojects", scanByIDReads, []string{byIDPathRule}},
	{"pathconcat", scanByIDReads, []string{byIDPathRule}},
	{"pathcollection", scanByIDReads, nil},
	{"lookupproject", scanByIDReads, []string{byIDLookupRule}},
	{"lookupdynamic", scanByIDReads, []string{byIDLookupRule}},
	{"lookupclusteralias", scanByIDReads, []string{byIDLookupRule}},
	{"lookupprojectsalias", scanByIDReads, []string{byIDLookupRule}},
	{"bykeycluster", scanByIDReads, []string{byIDLookupRule}},
	{"bykeyother", scanByIDReads, nil},
	{"gvrliteral", scanByIDReads, []string{byIDGVRRule}},
	{"gvrpositional", scanByIDReads, []string{byIDGVRRule}},
	{"gvrother", scanByIDReads, nil},
	{"lookupnoget", scanByIDReads, nil},
	{"lookupother", scanByIDReads, nil},
}

// TestByIDScanFixtures asserts each rule on type-checked fixture packages:
// every refused form is reported with its rule, and each form that looks
// alike but gets no cluster or project by its id is accepted.
func TestByIDScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := byIDFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range byIDFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, f := range tc.scan(pkg, wd) {
				got = append(got, cutFindingPosition.ReplaceAllString(f.text, ""))
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestByIDScanFixturesAreAllListed asserts every fixture package on disk is
// in the table above. A fixture added without its row fails.
func TestByIDScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.FromSlash(byIDFixtureDir))
	require.NoError(t, err)
	pkgs, err := byIDFixtures()
	require.NoError(t, err)
	var onDisk, listed, loaded []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range byIDFixtureCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
	assert.ElementsMatch(t, listed, loaded)
	assert.False(t, slices.Contains(listed, ""))
}
