package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Every write the CLI sends to a Kubernetes API carries the field manager
// kube.FieldManager, so an object's managed fields always say the CLI wrote
// them. The writes client-go sends set it in their options; the raw requests
// the CLI builds by hand go through kube.NewWriteRequest. The scans below
// fail on each shape that sends a write without it, now or later:
//
//   - a metav1 CreateOptions, UpdateOptions, PatchOptions or ApplyOptions
//     literal whose FieldManager is missing or is not the constant value
//     of kube.FieldManager; an assignment to the FieldManager of one of them
//     that is not that constant; and any other use of those types as a type
//     (a var, a result, a struct field, an element type, a type argument,
//     new, a conversion), because a zero value sends no field manager;
//   - a call of http.NewRequest or http.NewRequestWithContext whose method
//     is a write or is not a constant, a reference to either function that is
//     not a call, and an http.Request literal with a Method key, each outside
//     fieldManagerRequestAllowlist (the file, the function and the number of
//     matches, with the reason);
//   - a call of Post, Put, Patch or Verb on client-go's rest.Interface or
//     *rest.RESTClient (reads through RESTClient().Get() stay legal);
//   - a call of Post or PostForm on *net/http.Client, a call of
//     net/http.ReadRequest, a literal of a type over net/http.Request with a
//     Method key, and a write to the Method or URL field of an http.Request
//     or to the RawQuery of its URL (an assignment, a range clause, *req.URL =
//     or taking the field's address), with no allowlist; the FieldManager of
//     an options type is guarded the same way;
//   - an import of controller-runtime (its client sends no field manager
//     unless the caller asks) other than pkg/envtest from internal/envtestlog,
//     and of client-go's event recorder, events and leader election packages
//     with their subpackages:
//     Kubernetes writes go through client-go's dynamic and typed clients with
//     a FieldManager option, or through kube.NewWriteRequest, only.
//
// A delete is outside the rule because the Kubernetes API gives it none:
// DeleteOptions has no field manager.
//
// The scans cover the production tree: every non-test Go file outside
// testdata and outside internal/testrancher (the test servers and the
// fidelity recorder, which are not the CLI). A request built in another
// package is covered by the scan of that package, as the tree is scanned
// whole.
//
// Limits: the type-resolved rules (the options, rest client and http.Client
// rules) see the default build tags and GOOS only; the syntactic request
// rule and the import rule see every file, whatever its build tags. A copy
// of a request's URL pointer (u := req.URL, then u.RawQuery = "") is not
// followed, and neither is a value written into an options value through
// reflection or a decoder (json.Unmarshal into &opts). The check of the
// requests the test servers receive
// (testrancher.FailOnWritesWithoutFieldManager) sees only the traffic that
// reaches a Facade, and a test file that starts its own server relies on
// these scans. The testrancher.New rule below covers the subpackages of
// internal/testrancher; TestNoProductionPackageImportsTestRancher covers
// every package outside that tree.

const (
	fieldManagerValue = "cli.rancher.io"
	restPath          = "k8s.io/client-go/rest"
	testrancherPath   = modulePrefix + "internal/testrancher"
	failOnWritesFunc  = "FailOnWritesWithoutFieldManager"
	fieldManagerDir   = "test/conformance/testdata/fieldmanager"
)

var fieldManagerOptionTypes = []string{"CreateOptions", "UpdateOptions", "PatchOptions", "ApplyOptions"}

// requestAllowance is the number of matches an allowlisted function covers
// and why they are not Kubernetes writes.
type requestAllowance struct {
	count int
	why   string
}

// fieldManagerRequestAllowlist maps "<file>:<func>" to the matches of the
// request rule it covers. A different number of matches fails the test.
var fieldManagerRequestAllowlist = map[string]requestAllowance{
	"internal/token/token.go:basicAuth":                    {1, "Rancher's /v1-public/login, not a Kubernetes API"},
	"internal/token/oauth.go:rancherLogin":                 {1, "Rancher's /v1-public/login with an identity provider token, not the identity provider"},
	"internal/token/revoke.go:postLogout":                  {1, "Rancher's /v1/logout, not a Kubernetes API"},
	"internal/token/clustertoken.go:createV3ClusterToken":  {1, "a reference: the Norman /v3/tokens create, where Rancher creates the Token object itself"},
	"cmd/rancher-credential-vault/vault.go:do":             {1, "a variable method on HashiCorp Vault's API, not a Kubernetes API"},
	"internal/client/kube/fieldmanager.go:NewWriteRequest": {1, "the one place a Kubernetes write request is built"},
	"internal/telemetrysend/request.go:newRequest":         {1, "the telemetry upload endpoint, not a Kubernetes API"},
}

// requestMatch is one place the request rule matched.
type requestMatch struct {
	key  string
	text string
}

// httpMember returns the name of the net/http member e refers to, through
// the name the file gives net/http: http.X, or X under a dot import.
func httpMember(e ast.Expr, httpName string) string {
	switch x := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && httpName != "." && id.Name == httpName {
			return x.Sel.Name
		}
	case *ast.Ident:
		if httpName == "." {
			return x.Name
		}
	}
	return ""
}

// methodClass says whether a method argument is a write, a method that is no
// write, or something else (a variable, a local constant, any other text).
func methodClass(arg ast.Expr, httpName string) string {
	name := ""
	if lit, ok := ast.Unparen(arg).(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			name = s
		}
		switch name {
		case "POST", "PUT", "PATCH":
			return "write"
		case "GET", "DELETE", "HEAD", "OPTIONS":
			return "read"
		}
		return "other"
	}
	switch httpMember(arg, httpName) {
	case "MethodPost", "MethodPut", "MethodPatch":
		return "write"
	case "MethodGet", "MethodDelete", "MethodHead", "MethodOptions":
		return "read"
	}
	return "other"
}

// scanRequestBuilders returns the matches of the request rule in f.
func scanRequestBuilders(fset *token.FileSet, filePath string, f *ast.File) []requestMatch {
	httpName := importName(f, netHTTPPath)
	if httpName == "" {
		return nil
	}
	var out []requestMatch
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		key := filePath + ":" + fn
		callees := map[ast.Expr]bool{}
		selected := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				callees[ast.Unparen(n.Fun)] = true
			case *ast.SelectorExpr:
				selected[n.Sel] = true
			}
			return true
		})
		report := func(pos token.Pos, text string) {
			out = append(out, requestMatch{key, fset.Position(pos).String() + ": " + text})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				member := httpMember(n.Fun, httpName)
				methodIndex := -1
				switch member {
				case "NewRequest":
					methodIndex = 0
				case "NewRequestWithContext":
					methodIndex = 1
				}
				if methodIndex < 0 {
					return true
				}
				class := "other"
				if len(n.Args) > methodIndex {
					class = methodClass(n.Args[methodIndex], httpName)
				}
				switch class {
				case "write":
					report(n.Pos(), "http."+member+" with a write method; build a Kubernetes write with kube.NewWriteRequest")
				case "other":
					report(n.Pos(), "http."+member+" with a method that is not a constant GET, DELETE, HEAD or OPTIONS")
				}
			case *ast.SelectorExpr:
				if member := httpMember(n, httpName); (member == "NewRequest" || member == "NewRequestWithContext") && !callees[n] {
					report(n.Pos(), "reference to http."+member+" that is not a call")
				}
			case *ast.Ident:
				if httpName == "." && !selected[n] && !callees[n] && (n.Name == "NewRequest" || n.Name == "NewRequestWithContext") {
					report(n.Pos(), "reference to http."+n.Name+" that is not a call")
				}
			case *ast.CompositeLit:
				if httpMember(n.Type, httpName) != "Request" {
					return true
				}
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Method" {
							report(n.Pos(), "http.Request literal with a Method key")
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// checkRequestMatches returns one message per match outside the allowlist,
// one per allowlist entry whose count differs from the matches it covers, and
// one per entry that matches nothing.
func checkRequestMatches(matches []requestMatch, allow map[string]requestAllowance) []string {
	byKey := map[string][]string{}
	for _, m := range matches {
		byKey[m.key] = append(byKey[m.key], m.text)
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		a, ok := allow[key]
		switch {
		case !ok:
			out = append(out, byKey[key]...)
		case len(byKey[key]) != a.count:
			out = append(out, fmt.Sprintf("%s: the allowlist covers %d match(es) (%s), found %d: %s", key, a.count, a.why, len(byKey[key]), strings.Join(byKey[key], "; ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allow)) {
		if len(byKey[key]) == 0 {
			out = append(out, "allowlist entry "+key+" matched nothing: remove it")
		}
	}
	return out
}

func inTestrancher(filePath string) bool { return strings.HasPrefix(filePath, "internal/testrancher/") }

// TestEveryKubernetesWriteRequestGoesThroughTheOneBuilder asserts no
// production file builds a request with a write method, or an unknown one,
// outside the allowlist.
func TestEveryKubernetesWriteRequestGoesThroughTheOneBuilder(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var matches []requestMatch
	for i, f := range tree.files {
		if inTestrancher(tree.paths[i]) {
			continue
		}
		matches = append(matches, scanRequestBuilders(tree.fset, tree.paths[i], f)...)
	}
	for _, problem := range checkRequestMatches(matches, fieldManagerRequestAllowlist) {
		t.Error(problem)
	}
	for key, a := range fieldManagerRequestAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// parseFieldManagerFixture parses one fixture file under the syntactic
// fixture directory.
func parseFieldManagerFixture(t *testing.T, name string) (*token.FileSet, string, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	p := path.Join(fieldManagerDir, "syntactic", name+".go")
	f, err := parser.ParseFile(fset, filepath.FromSlash(p), nil, parser.AllErrors)
	require.NoError(t, err)
	return fset, p, f
}

// TestRequestScanFixtures asserts the request rule on parsed fixture files:
// each write form, each unknown method, the references, the literal and the
// accepted reads are reported or accepted as named, with positions removed.
// Dropping a form from the scan fails its fixture.
func TestRequestScanFixtures(t *testing.T) {
	t.Parallel()
	const (
		write     = "http.NewRequest with a write method; build a Kubernetes write with kube.NewWriteRequest"
		writeCtx  = "http.NewRequestWithContext with a write method; build a Kubernetes write with kube.NewWriteRequest"
		other     = "http.NewRequest with a method that is not a constant GET, DELETE, HEAD or OPTIONS"
		otherCtx  = "http.NewRequestWithContext with a method that is not a constant GET, DELETE, HEAD or OPTIONS"
		referCtx  = "reference to http.NewRequestWithContext that is not a call"
		referPlan = "reference to http.NewRequest that is not a call"
		literal   = "http.Request literal with a Method key"
	)
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"postconstant", []string{writeCtx}},
		{"putconstant", []string{write}},
		{"patchconstant", []string{writeCtx}},
		{"postliteral", []string{write}},
		{"variablemethod", []string{other}},
		{"localconstant", []string{otherCtx}},
		{"lowercaseliteral", []string{other}},
		{"referencevalue", []string{referCtx}},
		{"referenceassigned", []string{referPlan}},
		{"requestliteral", []string{literal}},
		{"aliasedimport", []string{writeCtx}},
		{"dotimport", []string{write, referCtx}},
		{"reads", nil},
		{"requestliteralnomethod", nil},
		{"noimport", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			fset, p, f := parseFieldManagerFixture(t, tc.fixture)
			var got []string
			for _, m := range scanRequestBuilders(fset, p, f) {
				_, rest, ok := strings.Cut(m.text, ": ")
				require.True(t, ok, m.text)
				got = append(got, rest)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRequestAllowlistRules asserts the allowlist rules on matches: an entry
// covers its function and its count, a function outside the list is
// reported, a different count fails and an entry that matches nothing is
// stale. Accepting a wrong count or keeping a stale entry fails it.
func TestRequestAllowlistRules(t *testing.T) {
	t.Parallel()
	one := requestMatch{"a.go:f", "a.go:1:1: m"}
	allow := map[string]requestAllowance{"a.go:f": {1, "why"}}
	assert.Empty(t, checkRequestMatches([]requestMatch{one}, allow), "an allowlisted post passes")
	assert.Equal(t, []string{"b.go:1:1: m"}, checkRequestMatches([]requestMatch{{"b.go:g", "b.go:1:1: m"}, one}, allow), "a function outside the list is reported")
	got := checkRequestMatches([]requestMatch{one, {"a.go:f", "a.go:2:1: m"}}, allow)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "the allowlist covers 1 match(es)", "a second match in an allowlisted function fails the count")
	assert.Equal(t, []string{"allowlist entry c.go:gone matched nothing: remove it"},
		checkRequestMatches([]requestMatch{one}, map[string]requestAllowance{"a.go:f": {1, "why"}, "c.go:gone": {1, "why"}}), "an entry for a function that no longer exists is stale")
}

// fieldManagerPackages type-checks every package of the module once for the
// type-resolved rules.
var fieldManagerPackages = sync.OnceValues(func() ([]*packages.Package, error) {
	return loadPackages("./...", "")
})

// loadPackages type-checks the packages of pattern. goos "" builds them for
// the host; another value builds them for that GOOS without cgo.
func loadPackages(pattern, goos string) ([]*packages.Package, error) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	if goos != "" {
		cfg.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0")
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, err
	}
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			return nil, fmt.Errorf("type-check %s: %s", pkg.PkgPath, e)
		}
	}
	return pkgs, nil
}

// optionsTypeName returns the name of the metav1 options type t is, or "".
func optionsTypeName(t types.Type) string {
	if t == nil {
		return ""
	}
	for _, name := range fieldManagerOptionTypes {
		if isNamedType(t, metav1Path, name) {
			return name
		}
	}
	return ""
}

// methodOf returns the declared method a selector in a call names.
func methodOf(info *types.Info, call *ast.CallExpr) *types.Func {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		fn, _ := info.Uses[fun.Sel].(*types.Func)
		return fn
	case *ast.Ident:
		fn, _ := info.Uses[fun].(*types.Func)
		return fn
	}
	return nil
}

// receiverName returns the name of the named type a method is declared on.
func receiverName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := types.Unalias(t).(*types.Named); ok {
		return n.Obj().Name()
	}
	return ""
}

// derefType returns t with an alias resolved and one pointer removed.
func derefType(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return t
}

// netHTTPMethodField returns the position of the field named Method that
// net/http declares in the struct underlying t (a pointer to it counts), or
// -1. A defined type or an alias over http.Request shares the field.
func netHTTPMethodField(t types.Type) int {
	t = derefType(t)
	if t == nil {
		return -1
	}
	strukt, ok := t.Underlying().(*types.Struct)
	if !ok {
		return -1
	}
	for i := range strukt.NumFields() {
		if f := strukt.Field(i); f.Name() == "Method" && f.Pkg() != nil && f.Pkg().Path() == netHTTPPath {
			return i
		}
	}
	return -1
}

// setsField reports whether a struct literal sets the field at position
// index named name, by key or by position.
func setsField(lit *ast.CompositeLit, name string, index int) bool {
	for i, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
				return true
			}
			continue
		}
		if i == index {
			return true
		}
	}
	return false
}

// isRequestURL reports whether e is the URL field of a net/http Request.
func isRequestURL(info *types.Info, e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	v, ok := info.Uses[sel.Sel].(*types.Var)
	return ok && v.IsField() && v.Name() == "URL" && v.Pkg() != nil && v.Pkg().Path() == netHTTPPath
}

// guardedField classifies e as a field that decides whether a write carries
// the field manager: the FieldManager of an options type (kind "manager",
// with the type's name), the Method or URL of an http.Request, or the
// RawQuery of such a URL (kinds "method" and "url").
func guardedField(info *types.Info, e ast.Expr) (kind, optionsName string) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	v, ok := info.Uses[sel.Sel].(*types.Var)
	if !ok || !v.IsField() || v.Pkg() == nil {
		return "", ""
	}
	switch {
	case v.Name() == "FieldManager":
		if name := optionsTypeName(derefType(info.TypeOf(sel.X))); name != "" {
			return "manager", name
		}
	case v.Name() == "Method" && v.Pkg().Path() == netHTTPPath:
		return "method", ""
	case v.Name() == "URL" && v.Pkg().Path() == netHTTPPath,
		v.Name() == "RawQuery" && v.Pkg().Path() == "net/url" && isRequestURL(info, sel.X):
		return "url", ""
	}
	return "", ""
}

const (
	urlWriteMessage = "assignment to the URL of an http.Request, or to its RawQuery, can drop the field manager from a built write; build the request with the URL it sends"
)

// scanTypedWrites returns the findings of the type-resolved rules in pkg,
// each as "<file>:<line>: <rule>".
func scanTypedWrites(pkg *packages.Package, wd string) []string {
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
		report := func(pos token.Pos, rule string) {
			out = append(out, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(pos).Line, rule))
		}
		// literalTypes holds the Type node of every options literal, which
		// the literal rule covers; selected holds the name part of each
		// selector, which is no type expression of its own.
		literalTypes := map[ast.Expr]bool{}
		selected := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				selected[sel.Sel] = true
			}
			if e, ok := n.(ast.Expr); ok && !literalTypes[e] {
				if id, isID := e.(*ast.Ident); !isID || !selected[id] {
					if tv, ok := info.Types[e]; ok && tv.IsType() {
						if typeName := optionsTypeName(tv.Type); typeName != "" {
							report(e.Pos(), "metav1."+typeName+" used as a type; build the options as a literal that sets kube.FieldManager")
						}
					}
				}
			}
			switch n := n.(type) {
			case *ast.CompositeLit:
				if typeName := optionsTypeName(info.TypeOf(n)); typeName != "" {
					if n.Type != nil {
						literalTypes[n.Type] = true
					}
					switch value, ok := fieldManagerOf(info, n); {
					case !ok:
						report(n.Pos(), "metav1."+typeName+" literal without a FieldManager; set kube.FieldManager")
					case value != fieldManagerValue:
						report(n.Pos(), "metav1."+typeName+" literal whose FieldManager is not the constant "+fieldManagerValue+"; set kube.FieldManager")
					}
				}
				if index := netHTTPMethodField(info.TypeOf(n)); index >= 0 && setsField(n, "Method", index) {
					report(n.Pos(), "literal of a type over http.Request with a Method key can build a write with no field manager; build the request with kube.NewWriteRequest")
				}
			case *ast.CallExpr:
				if fn := methodOf(info, n); fn != nil && fn.Pkg() != nil {
					switch {
					case fn.Pkg().Path() == restPath && slices.Contains([]string{"Post", "Put", "Patch", "Verb"}, fn.Name()):
						report(n.Pos(), "rest client "+fn.Name()+" sends a write with no field manager; use the dynamic or typed client with a FieldManager option")
					case fn.Pkg().Path() == netHTTPPath && receiverName(fn) == "Client" && (fn.Name() == "Post" || fn.Name() == "PostForm"):
						report(n.Pos(), "http.Client."+fn.Name()+" sends a write with no field manager; build a Kubernetes write with kube.NewWriteRequest")
					case fn.Pkg().Path() == netHTTPPath && receiverName(fn) == "" && fn.Name() == "ReadRequest":
						report(n.Pos(), "http.ReadRequest builds a request with a method no rule checks; build a Kubernetes write with kube.NewWriteRequest")
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if star, ok := ast.Unparen(lhs).(*ast.StarExpr); ok && isRequestURL(info, star.X) {
						report(n.Pos(), urlWriteMessage)
						continue
					}
					constant := n.Tok == token.ASSIGN && len(n.Rhs) == len(n.Lhs)
					if constant {
						s, isConst := constString(info, n.Rhs[i])
						constant = isConst && s == fieldManagerValue
					}
					reportGuardedWrite(info, lhs, constant, func(msg string) { report(n.Pos(), msg) })
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if e == nil || n.Tok != token.ASSIGN {
						continue
					}
					if star, ok := ast.Unparen(e).(*ast.StarExpr); ok && isRequestURL(info, star.X) {
						report(n.Pos(), urlWriteMessage)
						continue
					}
					reportGuardedWrite(info, e, false, func(msg string) { report(n.Pos(), msg) })
				}
			case *ast.UnaryExpr:
				if n.Op != token.AND {
					return true
				}
				switch kind, name := guardedField(info, n.X); kind {
				case "manager":
					report(n.Pos(), "address of the FieldManager of metav1."+name+" taken; a write through it is not seen; set kube.FieldManager in the literal")
				case "method":
					report(n.Pos(), "address of the Method of an http.Request taken; a write through it is not seen; build the request with the method it sends")
				case "url":
					report(n.Pos(), "address of the URL of an http.Request, or of its RawQuery, taken; a write through it is not seen; build the request with the URL it sends")
				}
			}
			return true
		})
	}
	return out
}

// reportGuardedWrite reports a write to lhs when it is a guarded field and,
// for a FieldManager, when the value is not the constant.
func reportGuardedWrite(info *types.Info, lhs ast.Expr, constant bool, report func(string)) {
	switch kind, name := guardedField(info, lhs); kind {
	case "manager":
		if !constant {
			report("assignment to the FieldManager of metav1." + name + " that is not the constant " + fieldManagerValue + "; assign kube.FieldManager")
		}
	case "method":
		report("assignment to the Method of an http.Request can turn a request into a write with no field manager; build the request with the method it sends")
	case "url":
		report(urlWriteMessage)
	}
}

// fieldManagerOf returns the constant value of the FieldManager a metav1
// options literal sets and whether it sets one at all. A value that is not
// constant reads as the empty string.
func fieldManagerOf(info *types.Info, lit *ast.CompositeLit) (string, bool) {
	var strukt *types.Struct
	if t, ok := info.TypeOf(lit).Underlying().(*types.Struct); ok {
		strukt = t
	}
	for i, elt := range lit.Elts {
		val := elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "FieldManager" {
				continue
			}
			val = kv.Value
		} else if strukt == nil || i >= strukt.NumFields() || strukt.Field(i).Name() != "FieldManager" {
			continue
		}
		s, _ := constString(info, val)
		return s, true
	}
	return "", false
}

// TestEveryWriteOptionNamesTheFieldManager asserts the type-resolved rules
// find nothing in the production tree.
func TestEveryWriteOptionNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var findings []string
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, testrancherPath) {
			continue
		}
		findings = append(findings, scanTypedWrites(pkg, wd)...)
	}
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
}

// fieldManagerFixtures type-checks the typed fixture packages once.
var fieldManagerFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+fieldManagerDir+"/typed/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// TestTypedWriteScanFixtures asserts the type-resolved rules on type-checked
// fixture packages: each refusal is reported with its rule, and the passing
// forms (a constant with the right value, kube.FieldManager, reads through a
// REST client) are accepted. A rule that stops seeing its form fails its
// fixture.
func TestTypedWriteScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	const (
		missing  = "metav1.CreateOptions literal without a FieldManager; set kube.FieldManager"
		notConst = "literal whose FieldManager is not the constant cli.rancher.io; set kube.FieldManager"

		requestLiteral = "literal of a type over http.Request with a Method key can build a write with no field manager; build the request with kube.NewWriteRequest"
		urlAddress     = "address of the URL of an http.Request, or of its RawQuery, taken; a write through it is not seen; build the request with the URL it sends"
		urlAssign      = "assignment to the URL of an http.Request, or to its RawQuery, can drop the field manager from a built write; build the request with the URL it sends"
	)
	usedAs := func(name string) string {
		return "metav1." + name + " used as a type; build the options as a literal that sets kube.FieldManager"
	}
	assigned := func(name string) string {
		return "assignment to the FieldManager of metav1." + name + " that is not the constant cli.rancher.io; assign kube.FieldManager"
	}
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"optionsmissing", []string{missing, "metav1.UpdateOptions literal without a FieldManager; set kube.FieldManager"}},
		{"optionsapplymissing", []string{"metav1.ApplyOptions literal without a FieldManager; set kube.FieldManager"}},
		{"optionspatchmissing", []string{"metav1.PatchOptions literal without a FieldManager; set kube.FieldManager"}},
		{"optionsempty", []string{"metav1.UpdateOptions " + notConst}},
		{"optionsother", []string{"metav1.CreateOptions " + notConst}},
		{"optionsvariable", []string{"metav1.CreateOptions " + notConst}},
		{"optionsunkeyed", []string{"metav1.ApplyOptions " + notConst}},
		{"optionselided", []string{missing, usedAs("CreateOptions")}},
		{"optionsvar", []string{usedAs("UpdateOptions"), usedAs("UpdateOptions")}},
		{"optionsnew", []string{usedAs("PatchOptions")}},
		{"optionsconversion", []string{usedAs("CreateOptions"), usedAs("CreateOptions"), usedAs("CreateOptions")}},
		{"optionsalias", []string{"metav1.UpdateOptions literal without a FieldManager; set kube.FieldManager", usedAs("UpdateOptions")}},
		{"optionsresult", []string{usedAs("CreateOptions")}},
		{"optionsfield", []string{usedAs("CreateOptions")}},
		{"optionsembedded", []string{usedAs("UpdateOptions")}},
		{"optionsarray", []string{usedAs("PatchOptions")}},
		{"optionsmap", []string{usedAs("CreateOptions"), usedAs("CreateOptions")}},
		{"optionstypearg", []string{usedAs("CreateOptions")}},
		{"optionspointer", []string{usedAs("UpdateOptions")}},
		{"optionsliteralslice", []string{usedAs("CreateOptions")}},
		{"optionsassign", []string{assigned("UpdateOptions"), assigned("UpdateOptions"), assigned("UpdateOptions"), assigned("UpdateOptions"), assigned("CreateOptions")}},
		{"optionsassignpass", nil},
		{"optionspass", nil},
		{"restpost", []string{"rest client Post sends a write with no field manager; use the dynamic or typed client with a FieldManager option"}},
		{"restclientverb", []string{"rest client Verb sends a write with no field manager; use the dynamic or typed client with a FieldManager option"}},
		{"restclientput", []string{"rest client Put sends a write with no field manager; use the dynamic or typed client with a FieldManager option"}},
		{"restget", nil},
		{"httppost", []string{"http.Client.Post sends a write with no field manager; build a Kubernetes write with kube.NewWriteRequest"}},
		{"httppostform", []string{"http.Client.PostForm sends a write with no field manager; build a Kubernetes write with kube.NewWriteRequest"}},
		{"methodassign", []string{"assignment to the Method of an http.Request can turn a request into a write with no field manager; build the request with the method it sends"}},
		{"httppass", nil},
		{"optionsrange", []string{assigned("UpdateOptions")}},
		{"optionsaddress", []string{"address of the FieldManager of metav1.UpdateOptions taken; a write through it is not seen; set kube.FieldManager in the literal"}},
		{"methodaddress", []string{"address of the Method of an http.Request taken; a write through it is not seen; build the request with the method it sends"}},
		{"urladdress", []string{urlAddress, urlAddress}},
		{"urlstar", []string{urlAssign}},
		{"addresspass", nil},
		{"readrequest", []string{"http.ReadRequest builds a request with a method no rule checks; build a Kubernetes write with kube.NewWriteRequest"}},
		{"requestliteralalias", []string{requestLiteral}},
		{"requestliteraldefined", []string{requestLiteral}},
		{"requestliteralpointer", []string{requestLiteral}},
		{"requestliteralown", nil},
		{"urlassign", []string{urlAssign}},
		{"rawqueryassign", []string{urlAssign}},
		{"urlassignpass", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "no fixture package %s", tc.fixture)
			var got []string
			for _, f := range scanTypedWrites(pkg, wd) {
				_, rest, ok := strings.Cut(f, ".go:")
				require.True(t, ok, f)
				got = append(got, strings.TrimSpace(strings.TrimLeft(rest, "0123456789:")))
			}
			slices.Sort(got)
			assert.Equal(t, slices.Sorted(slices.Values(tc.want)), got)
		})
	}
}

// facadeCheckAllowlist maps a directory to the reason its TestMain does not
// call testrancher.FailOnWritesWithoutFieldManager. An entry is a user
// decision.
var facadeCheckAllowlist = map[string]string{}

// exitCodeCheck reports whether the file declares a TestMain that runs the
// tests into a variable and then assigns that variable the result of
// FailOnWritesWithoutFieldManager called with it, qualified by the import
// name of internal/testrancher (unqualified inside that package), with a
// later statement that reads the variable.
func exitCodeCheck(f goFile, inTestrancherDir bool) bool {
	name := importName(f.ast, testrancherPath)
	if name == "" && !inTestrancherDir {
		return false
	}
	for _, decl := range f.ast.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		params := fn.Type.Params.List
		if len(params) != 1 || len(params[0].Names) != 1 {
			continue
		}
		m := params[0].Names[0].Name
		stmts := fn.Body.List
		for i, stmt := range stmts {
			code, _ := assignedName(stmt)
			if code == "" || !runsTests(stmt, m) {
				continue
			}
			for j := i + 1; j < len(stmts); j++ {
				target, rhs := assignedName(stmts[j])
				if target != code || !callsFailOnWrites(rhs, name, code) {
					continue
				}
				if slices.ContainsFunc(stmts[j+1:], func(later ast.Stmt) bool { return reads(later, code) }) {
					return true
				}
			}
		}
	}
	return false
}

// callsFailOnWrites reports whether e is a call of
// FailOnWritesWithoutFieldManager whose first argument is the variable code.
func callsFailOnWrites(e ast.Expr, importedAs, code string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	if id, ok := call.Args[0].(*ast.Ident); !ok || id.Name != code {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		id, isID := fun.X.(*ast.Ident)
		return isID && importedAs != "" && importedAs != "." && id.Name == importedAs && fun.Sel.Name == failOnWritesFunc
	case *ast.Ident:
		return (importedAs == "." || importedAs == "") && fun.Name == failOnWritesFunc
	}
	return false
}

// facadeCheckProblem returns why the directory's TestMain does not check the
// writes its Facades received, or "" when it does or the directory is out of
// scope. A directory is in scope when a test file imports
// internal/testrancher, and internal/testrancher itself always is.
func facadeCheckProblem(dir string, files []goFile) string {
	inTestrancherDir := dir == "internal/testrancher"
	var triggers []goFile
	for _, f := range files {
		if f.isTest() && (inTestrancherDir || importName(f.ast, testrancherPath) != "") {
			triggers = append(triggers, f)
		}
	}
	if len(triggers) == 0 {
		return ""
	}
	var checked []goFile
	for _, f := range files {
		if f.isTest() && exitCodeCheck(f, inTestrancherDir) {
			checked = append(checked, f)
		}
	}
	if len(checked) == 0 {
		return "its tests use testrancher.Facade but no TestMain assigns the exit code the result of testrancher." + failOnWritesFunc + " after m.Run"
	}
	for _, ctxt := range buildContexts(files) {
		starts := slices.ContainsFunc(triggers, func(f goFile) bool { return builds(ctxt, f, true) })
		guards := slices.ContainsFunc(checked, func(f goFile) bool { return builds(ctxt, f, false) })
		if starts && !guards {
			return "its TestMain that checks the Facade writes is behind a build constraint that does not cover every test file that uses a Facade (" + ctxt.GOOS + "/" + ctxt.GOARCH + " with tags " + strings.Join(ctxt.BuildTags, ",") + ")"
		}
	}
	return ""
}

// scanFacadeChecks returns one message per directory in scope without the
// check, and one per stale allowlist entry.
func scanFacadeChecks(dirs map[string][]goFile, allow map[string]string) []string {
	var out []string
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		problem := facadeCheckProblem(dir, dirs[dir])
		_, allowed := allow[dir]
		switch {
		case problem != "" && !allowed:
			out = append(out, dir+": "+problem)
		case problem == "" && allowed:
			out = append(out, dir+": allowlist entry is stale: the directory checks the Facade writes or no longer uses a Facade; delete the entry")
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(allow)) {
		if _, ok := dirs[dir]; !ok {
			out = append(out, dir+": allowlist entry is stale: no Go files in the directory; delete the entry")
		}
	}
	return out
}

// TestEveryTestBinaryThatUsesAFacadeChecksItsWrites asserts every directory
// whose tests use internal/testrancher assigns the exit code the result of
// testrancher.FailOnWritesWithoutFieldManager after m.Run, so a Kubernetes
// write a test sends to a Facade without the field manager fails the
// package.
func TestEveryTestBinaryThatUsesAFacadeChecksItsWrites(t *testing.T) {
	t.Parallel()
	dirs := parseDirs(t)
	require.NotEmpty(t, dirs)
	for _, problem := range scanFacadeChecks(dirs, facadeCheckAllowlist) {
		t.Error(problem)
	}
}

// TestFacadeCheckScanFixtures asserts the TestMain rule on source snippets:
// the accepted form, a missing call, the call before m.Run, a result
// assigned to another variable, a call inside an if, an aliased and a dot
// import, another package of the same name, a build constraint that leaves a
// test file without the check, the package that defines the check, and an
// allowlist entry that is stale. A looser rule fails its row.
func TestFacadeCheckScanFixtures(t *testing.T) {
	t.Parallel()
	const useFacade = `package p; import "github.com/rancher/rancher-cli/internal/testrancher"; var _ = testrancher.New`
	mainWith := func(imports, body string) string {
		return "package p\nimport (\"os\"; \"testing\"; " + imports + ")\nfunc TestMain(m *testing.M) {\n" + body + "\n}"
	}
	const trImport = `"github.com/rancher/rancher-cli/internal/testrancher"`
	accepted := mainWith(trImport, "code := m.Run()\ncode = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code)")
	for _, tc := range []struct {
		name      string
		files     map[string]string
		allowlist map[string]string
		want      string
	}{
		{"accepted", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": accepted}, nil, ""},
		{"no TestMain", map[string]string{"internal/x/a_test.go": useFacade}, nil, "internal/x: its tests use testrancher.Facade"},
		{"no call", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "os.Exit(m.Run())")}, nil, "internal/x: its tests use"},
		{"call before m.Run", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "code := 0\ncode = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)\ncode = m.Run()\nos.Exit(code)")}, nil, "internal/x: its tests use"},
		{"result assigned to another variable", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "code := m.Run()\nother := testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code + other)")}, nil, "internal/x: its tests use"},
		{"result never read", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "code := m.Run()\ncode = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)")}, nil, "internal/x: its tests use"},
		{"call inside an if", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "code := m.Run()\nif code == 0 {\ncode = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)\n}\nos.Exit(code)")}, nil, "internal/x: its tests use"},
		{"argument is not the exit code", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(trImport, "code := m.Run()\ncode = testrancher.FailOnWritesWithoutFieldManager(0, os.Stderr)\nos.Exit(code)")}, nil, "internal/x: its tests use"},
		{"aliased import", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith("tr "+trImport, "code := m.Run()\ncode = tr.FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code)")}, nil, ""},
		{"dot import", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(". "+trImport, "code := m.Run()\ncode = FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code)")}, nil, ""},
		{"other package with the same name", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": mainWith(`testrancher "example.com/other"`, "code := m.Run()\ncode = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code)")}, nil, "internal/x: its tests use"},
		{"the package that defines the check", map[string]string{"internal/testrancher/a_test.go": `package testrancher`, "internal/testrancher/testmain_test.go": mainWith(`"io"`, "code := m.Run()\ncode = FailOnWritesWithoutFieldManager(code, os.Stderr)\nos.Exit(code)")}, nil, ""},
		{"the package that defines the check without it", map[string]string{"internal/testrancher/a_test.go": `package testrancher`}, nil, "internal/testrancher: its tests use"},
		{"no test file uses a Facade", map[string]string{"internal/x/a_test.go": `package p; import "testing"; func TestA(t *testing.T) {}`}, nil, ""},
		{"tagged TestMain, untagged test file", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": "//go:build e2e\n\n" + accepted}, nil, "build constraint"},
		{"tagged TestMain, same tag on the test file", map[string]string{"internal/x/a_test.go": "//go:build e2e\n\n" + useFacade, "internal/x/testmain_test.go": "//go:build e2e\n\n" + accepted}, nil, ""},
		{"TestMain in a windows-named file", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_windows_test.go": accepted}, nil, "build constraint"},
		{"allowlisted directory without the check", map[string]string{"internal/x/a_test.go": useFacade}, map[string]string{"internal/x": "a reason"}, ""},
		{"stale allowlist entry", map[string]string{"internal/x/a_test.go": useFacade, "internal/x/testmain_test.go": accepted}, map[string]string{"internal/x": "a reason"}, "allowlist entry is stale"},
		{"allowlist entry for a missing directory", map[string]string{"internal/x/a_test.go": `package p`}, map[string]string{"internal/gone": "a reason"}, "internal/gone: allowlist entry is stale: no Go files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := scanFacadeChecks(fixtureDirs(t, tc.files), tc.allowlist)
			if tc.want == "" {
				assert.Empty(t, problems)
				return
			}
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), tc.want)
		})
	}
}

const (
	controllerRuntimePath = "sigs.k8s.io/controller-runtime"
	envtestImportPath     = controllerRuntimePath + "/pkg/envtest"
	envtestlogDir         = "internal/envtestlog/"
)

// fencedClientPackages are client-go packages that write Events or Leases
// with no field manager.
var fencedClientPackages = []string{
	"k8s.io/client-go/tools/record",
	"k8s.io/client-go/tools/events",
	"k8s.io/client-go/tools/leaderelection",
}

// scanClientImports returns one message per import in f of a Kubernetes
// client that sends writes without a FieldManager option: anything under
// controller-runtime except pkg/envtest from internal/envtestlog, and the
// fenced client-go packages with their subpackages. A blank or aliased
// import counts.
func scanClientImports(fset *token.FileSet, filePath string, f *ast.File) []string {
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		fenced := p == controllerRuntimePath || strings.HasPrefix(p, controllerRuntimePath+"/") ||
			slices.ContainsFunc(fencedClientPackages, func(x string) bool { return p == x || strings.HasPrefix(p, x+"/") })
		if p == envtestImportPath && strings.HasPrefix(filePath, envtestlogDir) {
			fenced = false
		}
		if fenced {
			out = append(out, fset.Position(spec.Pos()).String()+": import of "+p+"; Kubernetes writes go through client-go's dynamic and typed clients with a FieldManager option, or through kube.NewWriteRequest")
		}
	}
	return out
}

// TestNoKubernetesClientThatSendsNoFieldManagerIsImported asserts no
// production file imports a fenced Kubernetes client package.
func TestNoKubernetesClientThatSendsNoFieldManagerIsImported(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for i, f := range tree.files {
		for _, problem := range scanClientImports(tree.fset, tree.paths[i], f) {
			t.Error(problem)
		}
	}
}

// scanFacadeConstructors returns one message per use in f of
// testrancher.New (a call or any other reference, through any import name),
// per testrancher.Facade composite literal and new(testrancher.Facade), and
// per dot import of internal/testrancher, unless f is in internal/testrancher
// itself.
func scanFacadeConstructors(fset *token.FileSet, filePath string, f *ast.File) []string {
	if path.Dir(filePath) == "internal/testrancher" {
		return nil
	}
	name := importName(f, testrancherPath)
	switch name {
	case "":
		return nil
	case ".":
		return []string{filePath + ": dot import of internal/testrancher in a non-test file hides the Facade constructor from the scan"}
	}
	var out []string
	const tail = " in a non-test file; only test files and internal/testrancher build a Facade, and a test binary that builds one checks its writes in TestMain"
	isFacade := func(e ast.Expr) bool {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Facade" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == name
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "New" {
				if id, ok := n.X.(*ast.Ident); ok && id.Name == name {
					out = append(out, fset.Position(n.Pos()).String()+": testrancher.New"+tail)
				}
			}
		case *ast.CompositeLit:
			if n.Type != nil && isFacade(n.Type) {
				out = append(out, fset.Position(n.Pos()).String()+": testrancher.Facade literal"+tail)
			}
		case *ast.CallExpr:
			if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 && isFacade(n.Args[0]) {
				out = append(out, fset.Position(n.Pos()).String()+": new(testrancher.Facade)"+tail)
			}
		}
		return true
	})
	return out
}

// TestNoFacadeIsBuiltOutsideTestFiles asserts no non-test file outside
// internal/testrancher itself builds a Facade, so a helper cannot hide a
// Facade from the TestMain rule, which looks at the importing test files.
func TestNoFacadeIsBuiltOutsideTestFiles(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for i, f := range tree.files {
		for _, problem := range scanFacadeConstructors(tree.fset, tree.paths[i], f) {
			t.Error(problem)
		}
	}
}

func parseSnippet(t *testing.T, filePath, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, src, parser.AllErrors)
	require.NoError(t, err)
	return fset, f
}

// TestClientImportAndFacadeConstructorFixtures asserts the import fence and
// the Facade constructor rule on source snippets: the client package, a
// blank import of it, a subpackage of envtest, envtest from the wrong
// directory, the fenced client-go packages, envtest from internal/envtestlog
// (accepted); testrancher.New from a subpackage of internal/testrancher, an
// aliased import, a dot import, a reference (all refused), and a file in
// internal/testrancher (accepted). A looser rule fails its row.
func TestClientImportAndFacadeConstructorFixtures(t *testing.T) {
	t.Parallel()
	const trImport = `"github.com/rancher/rancher-cli/internal/testrancher"`
	for _, tc := range []struct {
		name, filePath, src string
		imports             bool
		want                string
	}{
		{"controller-runtime client", "internal/cli/x.go", `package p; import "sigs.k8s.io/controller-runtime/pkg/client"; var _ = client.Merge`, true, "import of sigs.k8s.io/controller-runtime/pkg/client"},
		{"blank import of the client", "internal/cli/x.go", `package p; import _ "sigs.k8s.io/controller-runtime/pkg/client"; var _ = 1`, true, "import of sigs.k8s.io/controller-runtime/pkg/client"},
		{"aliased import of the client", "internal/cli/x.go", `package p; import crc "sigs.k8s.io/controller-runtime/pkg/client"; var _ = crc.Merge`, true, "import of sigs.k8s.io/controller-runtime/pkg/client"},
		{"envtest from envtestlog", "internal/envtestlog/x.go", `package p; import "sigs.k8s.io/controller-runtime/pkg/envtest"; var _ = envtest.Environment{}`, true, ""},
		{"envtest from another directory", "internal/cli/x.go", `package p; import "sigs.k8s.io/controller-runtime/pkg/envtest"; var _ = envtest.Environment{}`, true, "import of sigs.k8s.io/controller-runtime/pkg/envtest"},
		{"envtest subpackage from envtestlog", "internal/envtestlog/x.go", `package p; import _ "sigs.k8s.io/controller-runtime/pkg/envtest/printer"`, true, "import of sigs.k8s.io/controller-runtime/pkg/envtest/printer"},
		{"the event recorder", "internal/cli/x.go", `package p; import _ "k8s.io/client-go/tools/record"`, true, "import of k8s.io/client-go/tools/record"},
		{"leader election", "internal/cli/x.go", `package p; import _ "k8s.io/client-go/tools/leaderelection"`, true, "import of k8s.io/client-go/tools/leaderelection"},
		{"a subpackage of leader election", "internal/cli/x.go", `package p; import _ "k8s.io/client-go/tools/leaderelection/resourcelock"`, true, "import of k8s.io/client-go/tools/leaderelection/resourcelock"},
		{"a client-go client the options cover", "internal/cli/x.go", `package p; import _ "k8s.io/client-go/dynamic"`, true, ""},
		{"New from a subpackage", "internal/testrancher/fidelity/x.go", `package p; import ` + trImport + `; var _ = testrancher.New(testrancher.Options{})`, false, "testrancher.New in a non-test file"},
		{"New through an alias", "internal/helper/x.go", `package p; import tr ` + trImport + `; var _ = tr.New(tr.Options{})`, false, "testrancher.New in a non-test file"},
		{"New as a reference", "internal/helper/x.go", `package p; import ` + trImport + `; var f = testrancher.New`, false, "testrancher.New in a non-test file"},
		{"a dot import", "internal/helper/x.go", `package p; import . ` + trImport + `; var _ = New(Options{})`, false, "dot import of internal/testrancher"},
		{"a Facade literal", "internal/testrancher/fidelity/x.go", `package p; import ` + trImport + `; var _ = &testrancher.Facade{}`, false, "testrancher.Facade literal"},
		{"new of a Facade", "internal/testrancher/fidelity/x.go", `package p; import ` + trImport + `; var _ = new(testrancher.Facade)`, false, "new(testrancher.Facade)"},
		{"a Facade type in a signature", "internal/testrancher/fidelity/x.go", `package p; import ` + trImport + `; func f(*testrancher.Facade) {}`, false, ""},
		{"another member", "internal/testrancher/fidelity/x.go", `package p; import ` + trImport + `; var _ = testrancher.CRTCollectionPath`, false, ""},
		{"a file in internal/testrancher", "internal/testrancher/x.go", `package testrancher; var _ = New(Options{})`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset, f := parseSnippet(t, tc.filePath, tc.src)
			var got []string
			if tc.imports {
				got = scanClientImports(fset, tc.filePath, f)
			} else {
				got = scanFacadeConstructors(fset, tc.filePath, f)
			}
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}
}
