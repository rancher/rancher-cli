package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// mgmtReadAllowed is every place that may name a management cluster or
// project collection: the file, the enclosing function ("" allows the whole
// file) and why.
var mgmtReadAllowed = []struct{ file, fn, why string }{
	{"internal/engine/kinds/kinds.go", "", "the kind table declares every GVR and the by-name lookups"},
	{"internal/engine/access/complete/clusters.go", "completeClusters", "the management cluster kind's name source for get/patch cluster"},
}

var (
	mgmtClusterPath = regexp.MustCompile(`management\.cattle\.io/v3/clusters\b`)
	mgmtProjectPath = regexp.MustCompile(`management\.cattle\.io/v3/(namespaces/[^/"]+/)?projects(\?|$)`)

	mgmtClusterNames = []string{"cluster", "clusters", "managementcluster"}
	mgmtProjectNames = []string{"project", "projects"}
)

const completionPackage = modulePrefix + "internal/runtime/completion"

// TestNoManagementClusterReadThroughTheLocalProxy asserts no production code
// lists or gets a management cluster, or lists projects, through the local
// cluster proxy. Rancher answers those only to a user who may get cluster
// local, so clusters are read through steve and a project is resolved by a
// get. The scan type-checks every production package (test files and
// internal/testrancher excluded) and fails on a constant path to either
// collection (a literal, a named constant, the constant parts of a
// concatenation run together, or a path.Join, url.JoinPath, strings.Join
// or fmt.Sprintf of constants), a management.cattle.io clusters
// GroupVersionResource (a literal, a management GroupVersion's
// WithResource, or a ParseGroupResource's WithVersion), a projects one
// reaching a List call, a kind looked
// up by a constant name, alias or group/version and kind naming the
// management cluster or project kind whose GVR reaches List, Get or a
// completion Management*Path builder, directly or through a variable
// assigned from that GVR, and a builder called with those literals.
//
// Known limits: a lookup whose kind name is a variable carries no
// constant and is not seen, and neither is a management GroupVersion
// declared in another package, then given to WithResource; a
// table test drives those paths by behavior.
func TestNoManagementClusterReadThroughTheLocalProxy(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./...")
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	used := map[int]bool{}
	var findings []string
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		for _, e := range pkg.Errors {
			require.Fail(t, "type-check "+pkg.PkgPath, e.Error())
		}
		findings = append(findings, scanManagementReads(pkg, wd, used)...)
	}
	require.NotEmpty(t, pkgs)
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
	for i, a := range mgmtReadAllowed {
		if !used[i] {
			t.Errorf("allow list entry %s %s matched nothing: remove it", a.file, a.fn)
		}
	}
}

func scanManagementReads(pkg *packages.Package, wd string, used map[int]bool) []string {
	info := pkg.TypesInfo
	var out []string
	walk := func(visit func(file, fn string, n ast.Node)) {
		for _, f := range pkg.Syntax {
			name := pkg.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel, err := filepath.Rel(wd, name)
			if err != nil {
				rel = name
			}
			for _, d := range f.Decls {
				fn := ""
				if fd, ok := d.(*ast.FuncDecl); ok {
					fn = fd.Name.Name
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if n != nil {
						visit(filepath.ToSlash(rel), fn, n)
					}
					return true
				})
			}
		}
	}
	report := func(file, fn string, pos token.Pos, rule string) {
		for i, a := range mgmtReadAllowed {
			if a.file == file && (a.fn == "" || a.fn == fn) {
				used[i] = true
				return
			}
		}
		out = append(out, fmt.Sprintf("%s:%d: %s", file, pkg.Fset.Position(pos).Line, rule))
	}

	// Bindings: identifiers initialized with a projects GVR literal or a
	// management.cattle.io GroupVersion literal, identifiers bound to a
	// kind looked up by a constant name, and identifiers assigned from such
	// a kind's GVR, each also through any
	// chain of identifiers assigned from one another. The walk repeats
	// until nothing new binds, so a binding may use one declared later.
	projectGVR := map[types.Object]bool{}
	kindName := map[types.Object]string{}
	gvrKind := map[types.Object]string{}
	mgmtGV := map[types.Object]bool{}
	bind := func(file, fn string, n ast.Node) {
		var lhs, rhs []ast.Expr
		switch n := n.(type) {
		case *ast.ValueSpec:
			for _, id := range n.Names {
				lhs = append(lhs, id)
			}
			rhs = n.Values
		case *ast.AssignStmt:
			lhs, rhs = n.Lhs, n.Rhs
		case *ast.KeyValueExpr:
			lhs, rhs = []ast.Expr{n.Key}, []ast.Expr{n.Value}
		}
		for i, r := range rhs {
			if i >= len(lhs) || (len(rhs) == 1 && i > 0) {
				break
			}
			id, ok := lhs[i].(*ast.Ident)
			if !ok || id.Name == "_" || info.ObjectOf(id) == nil {
				continue
			}
			obj := info.ObjectOf(id)
			if src, ok := ast.Unparen(r).(*ast.Ident); ok && info.Uses[src] != nil {
				from := info.Uses[src]
				if projectGVR[from] {
					projectGVR[obj] = true
				}
				if mgmtGV[from] {
					mgmtGV[obj] = true
				}
				if name, ok := kindName[from]; ok {
					kindName[obj] = name
				}
				if name, ok := gvrKind[from]; ok {
					gvrKind[obj] = name
				}
			}
			if isGVRLiteral(info, r, "projects", mgmtGV) {
				projectGVR[obj] = true
			}
			if isMgmtGroupVersion(info, r, mgmtGV) {
				mgmtGV[obj] = true
			}
			if call, ok := ast.Unparen(r).(*ast.CallExpr); ok {
				if name, ok := constKindName(info, call); ok {
					kindName[info.ObjectOf(id)] = name
				}
			}
			if name, ok := gvrOfKind(info, r, kindName); ok {
				gvrKind[info.ObjectOf(id)] = name
			}
		}
	}
	for size := -1; size != len(projectGVR)+len(kindName)+len(gvrKind)+len(mgmtGV); {
		size = len(projectGVR) + len(kindName) + len(gvrKind) + len(mgmtGV)
		walk(bind)
	}

	// chained holds the operands of a concatenation already scanned as a
	// whole, so a part is not scanned again on its own.
	chained := map[ast.Expr]bool{}
	walk(func(file, fn string, n ast.Node) {
		if e, ok := n.(ast.Expr); ok && !chained[e] {
			for _, s := range constantStrings(info, e, chained) {
				if mgmtClusterPath.MatchString(s) || mgmtProjectPath.MatchString(s) {
					report(file, fn, e.Pos(), fmt.Sprintf("constant path %q reads a management cluster or project collection through the local proxy", s))
				}
			}
		}
		switch n := n.(type) {
		case *ast.CompositeLit:
			if isGVRLiteral(info, n, "clusters", mgmtGV) {
				report(file, fn, n.Pos(), "management.cattle.io clusters GroupVersionResource: read clusters through steve")
			}
		case *ast.CallExpr:
			if isGVRLiteral(info, n, "clusters", mgmtGV) {
				report(file, fn, n.Pos(), "management.cattle.io clusters GroupVersionResource: read clusters through steve")
			}
			for _, s := range joinedConstants(info, n) {
				if mgmtClusterPath.MatchString(s) || mgmtProjectPath.MatchString(s) {
					report(file, fn, n.Pos(), fmt.Sprintf("constant path %q reads a management cluster or project collection through the local proxy", s))
				}
			}
			scanReadCall(info, n, projectGVR, kindName, gvrKind, mgmtGV, func(rule string) { report(file, fn, n.Pos(), rule) })
		}
	})
	slices.Sort(out)
	return slices.Compact(out)
}

// constantStrings returns the constant strings e spells: its own constant
// value, or for a concatenation with a part that is not constant, each run
// of adjacent constant parts joined. The parts of a concatenation are
// added to chained.
func constantStrings(info *types.Info, e ast.Expr, chained map[ast.Expr]bool) []string {
	if s, ok := constString(info, e); ok {
		if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.ADD {
			markChain(b, chained)
		}
		return []string{s}
	}
	b, ok := ast.Unparen(e).(*ast.BinaryExpr)
	if !ok || b.Op != token.ADD {
		return nil
	}
	if t, ok := info.TypeOf(b).Underlying().(*types.Basic); !ok || t.Info()&types.IsString == 0 {
		return nil
	}
	markChain(b, chained)
	var out []string
	run, inRun := "", false
	for _, part := range concatParts(b) {
		if s, ok := constString(info, part); ok {
			run, inRun = run+s, true
			continue
		}
		if inRun {
			out = append(out, run)
		}
		run, inRun = "", false
	}
	if inRun {
		out = append(out, run)
	}
	return out
}

// joinedConstants returns the constant strings a call builds: for
// path.Join, url.JoinPath or a URL's JoinPath, each run of adjacent
// constant arguments joined the way the call joins them; for
// strings.Join of a slice literal with a constant separator, each run of
// adjacent constant elements joined with it; for fmt.Sprintf with a
// constant format and only constant string arguments, the result.
func joinedConstants(info *types.Info, call *ast.CallExpr) []string {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return nil
	}
	join := path.Join
	args := call.Args
	switch {
	case fn.Pkg().Path() == "path" && fn.Name() == "Join",
		fn.Pkg().Path() == "net/url" && fn.Name() == "JoinPath":
	case fn.Pkg().Path() == "strings" && fn.Name() == "Join" && len(call.Args) == 2:
		lit, ok := ast.Unparen(call.Args[0]).(*ast.CompositeLit)
		sep, sepOK := constString(info, call.Args[1])
		if !ok || !sepOK {
			return nil
		}
		join = func(elems ...string) string { return strings.Join(elems, sep) }
		args = lit.Elts
	case fn.Pkg().Path() == "fmt" && fn.Name() == "Sprintf" && len(call.Args) > 0:
		format, ok := constString(info, call.Args[0])
		if !ok {
			return nil
		}
		var vals []any
		for _, a := range call.Args[1:] {
			v, ok := constString(info, a)
			if !ok {
				return nil
			}
			vals = append(vals, v)
		}
		return []string{fmt.Sprintf(format, vals...)}
	default:
		return nil
	}
	var out, run []string
	flush := func() {
		if len(run) > 0 {
			out = append(out, join(run...))
		}
		run = nil
	}
	for _, a := range args {
		if s, ok := constString(info, a); ok {
			run = append(run, s)
			continue
		}
		flush()
	}
	flush()
	return out
}

// concatParts flattens a chain of + into its operands, left to right.
func concatParts(e ast.Expr) []ast.Expr {
	if b, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return append(concatParts(b.X), concatParts(b.Y)...)
	}
	return []ast.Expr{e}
}

// markChain adds the parts and the inner concatenations of b to chained.
func markChain(b *ast.BinaryExpr, chained map[ast.Expr]bool) {
	for _, e := range []ast.Expr{b.X, b.Y} {
		chained[e] = true
		if inner, ok := ast.Unparen(e).(*ast.BinaryExpr); ok && inner.Op == token.ADD {
			chained[inner] = true
			markChain(inner, chained)
		}
	}
}

// gvrOfKind returns the kind name behind e when e is the GVR of a kind
// looked up by a constant name: <ident>.GVR of a bound kind, or
// <lookup call>.GVR.
func gvrOfKind(info *types.Info, e ast.Expr, kindName map[types.Object]string) (string, bool) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "GVR" {
		return "", false
	}
	switch x := ast.Unparen(sel.X).(type) {
	case *ast.Ident:
		name, ok := kindName[info.Uses[x]]
		return name, ok
	case *ast.CallExpr:
		return constKindName(info, x)
	}
	return "", false
}

// scanReadCall reports the rules a single call breaks: a List or Get fed a
// cluster or project GVR, and a completion Management*Path builder fed the
// cluster or project group and resource.
func scanReadCall(info *types.Info, call *ast.CallExpr, projectGVR map[types.Object]bool, kindName, gvrKind map[types.Object]string,
	mgmtGV map[types.Object]bool, emit func(string)) {
	seen := map[string]bool{}
	report := func(rule string) {
		if !seen[rule] {
			seen[rule] = true
			emit(rule)
		}
	}
	callee := ""
	var calleeID *ast.Ident
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		callee, calleeID = f.Sel.Name, f.Sel
	case *ast.Ident:
		callee, calleeID = f.Name, f
	}
	builder := ""
	if fn, ok := info.Uses[calleeID].(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == completionPackage &&
		strings.HasPrefix(fn.Name(), "Management") && strings.HasSuffix(fn.Name(), "Path") {
		builder = fn.Name()
	}
	collection := builder == "ManagementPath" || builder == "ManagementNamespacedPath"
	var consts []string
	for _, a := range call.Args {
		if s, ok := constString(info, a); ok {
			consts = append(consts, s)
		}
		if callee != "List" && callee != "Get" && builder == "" {
			continue
		}
		kindGVR := func(name string) {
			switch {
			case slices.Contains(mgmtClusterNames, name) && (callee == "List" || callee == "Get" || builder != ""),
				slices.Contains(mgmtProjectNames, name) && (callee == "List" || collection):
				report(fmt.Sprintf("kind %q GVR reaches %s: read it through steve", name, reachedBy(callee, builder)))
			}
		}
		ast.Inspect(a, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if callee == "List" && projectGVR[info.Uses[n]] {
					report("projects GroupVersionResource reaches List: list projects through steve")
				}
				if name, ok := gvrKind[info.Uses[n]]; ok {
					kindGVR(name)
				}
			case *ast.CompositeLit, *ast.CallExpr:
				if callee == "List" && isGVRLiteral(info, n.(ast.Expr), "projects", mgmtGV) {
					report("projects GroupVersionResource reaches List: list projects through steve")
				}
			case *ast.SelectorExpr:
				if name, ok := gvrOfKind(info, n, kindName); ok {
					kindGVR(name)
				}
			}
			return true
		})
	}
	if builder != "" && slices.Contains(consts, "management.cattle.io") &&
		(slices.Contains(consts, "clusters") || (collection && slices.Contains(consts, "projects"))) {
		report("completion." + builder + " builds a management cluster or project collection path")
	}
}

func reachedBy(callee, builder string) string {
	if builder != "" {
		return "completion." + builder
	}
	return callee
}

// constKindName returns the kind a call producing a kinds.Kind (alone or
// as the first result) names with its constant string arguments: the key
// of the kind two adjacent ones name as a group/version and kind, else of
// the kind one names by name, alias or key, else the first one
// lower-cased.
func constKindName(info *types.Info, call *ast.CallExpr) (string, bool) {
	t := info.TypeOf(call)
	if tup, ok := t.(*types.Tuple); ok && tup.Len() > 0 {
		t = tup.At(0).Type()
	}
	if !isNamedType(t, modulePrefix+"internal/engine/kinds", "Kind") {
		return "", false
	}
	var consts []string
	for _, a := range call.Args {
		if s, ok := constString(info, a); ok {
			consts = append(consts, s)
		}
	}
	for i := 1; i < len(consts); i++ {
		if k, ok := kinds.LookupGVK(consts[i-1], consts[i]); ok {
			return k.Key, true
		}
	}
	for _, s := range consts {
		if k, ok := kinds.Lookup(strings.ToLower(s)); ok {
			return k.Key, true
		}
	}
	if len(consts) > 0 {
		return strings.ToLower(consts[0]), true
	}
	return "", false
}

// isGVRLiteral reports whether e is a schema.GroupVersionResource literal
// of the management.cattle.io group and the given resource; a
// management.cattle.io GroupVersion, as a literal or an identifier in
// mgmtGV, whose WithResource is called with the resource; or a
// schema.ParseGroupResource of "<resource>.management.cattle.io" whose
// WithVersion is called.
func isGVRLiteral(info *types.Info, e ast.Expr, resource string, mgmtGV map[types.Object]bool) bool {
	if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || len(call.Args) != 1 {
			return false
		}
		switch sel.Sel.Name {
		case "WithResource":
			res, _ := constString(info, call.Args[0])
			return isMgmtGroupVersion(info, sel.X, mgmtGV) && res == resource
		case "WithVersion":
			parse, ok := ast.Unparen(sel.X).(*ast.CallExpr)
			if !ok || len(parse.Args) != 1 {
				return false
			}
			psel, ok := ast.Unparen(parse.Fun).(*ast.SelectorExpr)
			if !ok {
				return false
			}
			fn, ok := info.Uses[psel.Sel].(*types.Func)
			gr, _ := constString(info, parse.Args[0])
			return ok && fn.Pkg() != nil && fn.Pkg().Path() == "k8s.io/apimachinery/pkg/runtime/schema" &&
				fn.Name() == "ParseGroupResource" && gr == resource+".management.cattle.io"
		}
		return false
	}
	lit, ok := ast.Unparen(e).(*ast.CompositeLit)
	if !ok || !isNamedType(info.TypeOf(lit), "k8s.io/apimachinery/pkg/runtime/schema", "GroupVersionResource") {
		return false
	}
	var group, res string
	for i, elt := range lit.Elts {
		key, val := fmt.Sprint(i), elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			key, val = fmt.Sprint(kv.Key), kv.Value
		}
		s, _ := constString(info, val)
		switch key {
		case "Group", "0":
			group = s
		case "Resource", "2":
			res = s
		}
	}
	return group == "management.cattle.io" && res == resource
}

// isMgmtGroupVersion reports whether e is a schema.GroupVersion literal of
// the management.cattle.io group, or an identifier mgmtGV binds to one.
func isMgmtGroupVersion(info *types.Info, e ast.Expr, mgmtGV map[types.Object]bool) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return mgmtGV[info.Uses[x]]
	case *ast.CompositeLit:
		return isNamedType(info.TypeOf(x), "k8s.io/apimachinery/pkg/runtime/schema", "GroupVersion") &&
			litField(info, x, "Group", 0) == "management.cattle.io"
	}
	return false
}

// litField is the constant string of a struct literal's field, given by
// name or by position.
func litField(info *types.Info, lit *ast.CompositeLit, name string, index int) string {
	for i, elt := range lit.Elts {
		key, val := fmt.Sprint(i), elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			key, val = fmt.Sprint(kv.Key), kv.Value
		}
		if key == name || key == fmt.Sprint(index) {
			s, _ := constString(info, val)
			return s
		}
	}
	return ""
}

func isNamedType(t types.Type, pkgPath, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value), true
	}
	return "", false
}

// mgmtReadFixtures holds the fixture packages the management read scan
// is proven on.
const mgmtReadFixtures = "test/conformance/testdata/mgmtread"

// scanManagementFixture runs the management read scan on one fixture
// package and returns its findings without their file positions.
func scanManagementFixture(t *testing.T, name string) []string {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./"+mgmtReadFixtures+"/"+name)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	for _, e := range pkgs[0].Errors {
		require.Fail(t, "type-check "+name, e.Error())
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	var out []string
	for _, f := range scanManagementReads(pkgs[0], wd, map[int]bool{}) {
		_, rest, ok := strings.Cut(f, ".go:")
		require.True(t, ok, f)
		out = append(out, strings.TrimSpace(strings.TrimLeft(rest, "0123456789:")))
	}
	return out
}

// TestManagementReadScanRejectsFixtures asserts the management read scan
// accepts the clean fixture and reports each shape of a local management
// cluster or project read: a literal path, a named constant, a
// concatenation, a path.Join, the cluster kind's GVR through a variable
// and an alias of it, a kinds.LookupGVK of the management Cluster, a
// management clusters GroupVersionResource literal, a GroupVersion's
// WithResource of clusters or projects (a literal, a package variable, a
// local variable), a fmt.Sprintf and a strings.Join of constants, and a
// ParseGroupResource's WithVersion.
func TestManagementReadScanRejectsFixtures(t *testing.T) {
	t.Parallel()
	const localClusters = `"/k8s/clusters/local/apis/management.cattle.io/v3/clusters"`
	constant := "constant path " + localClusters + " reads a management cluster or project collection through the local proxy"
	assert.Empty(t, scanManagementFixture(t, "accepted"))
	for _, tt := range []struct {
		fixture string
		want    []string
	}{
		{"literal", []string{constant}},
		{"constant", []string{constant, constant}},
		{"concat", []string{constant}},
		{"pathjoin", []string{constant}},
		{"gvrvar", []string{`kind "managementcluster" GVR reaches Get: read it through steve`, `kind "managementcluster" GVR reaches List: read it through steve`}},
		{"lookupgvk", []string{`kind "managementcluster" GVR reaches Get: read it through steve`}},
		{"gvpackage", []string{"management.cattle.io clusters GroupVersionResource: read clusters through steve"}},
		{"gvlocal", []string{"management.cattle.io clusters GroupVersionResource: read clusters through steve"}},
		{"sprintf", []string{constant}},
		{"stringsjoin", []string{constant}},
		{"parsegroupresource", []string{"management.cattle.io clusters GroupVersionResource: read clusters through steve"}},
		{"gvrliteral", []string{"management.cattle.io clusters GroupVersionResource: read clusters through steve"}},
		{"withresource", []string{
			"management.cattle.io clusters GroupVersionResource: read clusters through steve",
			"projects GroupVersionResource reaches List: list projects through steve",
		}},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			got := scanManagementFixture(t, tt.fixture)
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tt.want))
			assert.Equal(t, want, got)
		})
	}
}
