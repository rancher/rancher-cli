package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	userLookupResolverRow = "resolver: row in TestNoUserSubjectResolverMatchesADisplayName"
	userLookupAccessRow   = "resolver: row in TestAccessUserLookupMatchesNoDisplayName"
	userLookupReadsByID   = "reads users by id or for display only: "
)

// userLookups lists every function and package-level variable under internal/
// and cmd/ that reaches the user kind or a user resolver. A resolver of a user value
// has a row in one of the two behavior tables, which assert a display name
// is never matched; every other entry says why it matches no user by a
// value the caller typed.
var userLookups = map[string]string{
	"internal/engine/rbac.ResolveSubject":     userLookupResolverRow,
	"internal/engine/rbac.ResolveInspectUser": userLookupResolverRow,
	"internal/engine/rbac.grantCmd.Plan":      userLookupResolverRow,
	"internal/engine/rbac.revokeCmd.Plan":     userLookupResolverRow,
	"internal/engine/access.findRancherUser":  userLookupAccessRow,

	"internal/engine/access.enrichCaller":            userLookupReadsByID + "gets the caller's User by the username the server reports, which is the user id",
	"internal/engine/access.readProvenance":          userLookupReadsByID + "gets the User by the name of a subject already resolved, which is the user id",
	"internal/engine/access.resolveCandidate":        userLookupReadsByID + "gets the User by the name a binding names, which is the user id",
	"internal/engine/inspect.systemPrincipalPrefix":  userLookupReadsByID + "reads the user kind row's system filter prefix, no user objects",
	"internal/engine/inspect.userCmd.Describe":       userLookupReadsByID + "reads the user kind row's aliases for the command description",
	"internal/engine/inspect.userCmd.Execute":        userLookupReadsByID + "resolves its argument through rbac.ResolveInspectUser, which has a row, then gets the User by the resolved id",
	"internal/engine/rbac.resolveUserDisplayNames":   userLookupReadsByID + "lists users to show the display names of bindings' users and principals, matched by id and principal id",
	"internal/engine/rbac/complete.completeSubjects": userLookupReadsByID + "offers ids and usernames for completion; the completion parity tests assert each value resolves to its user",
	"internal/engine/user.userKind":                  userLookupReadsByID + "returns the user kind row for create user, no user objects",
	"internal/engine/user.createCmd.Plan":            userLookupReadsByID + "creates a user from the username and display name given as flags; it reads no user by a value the caller typed",
	"internal/engine/user/complete.completeUserIDs":  userLookupReadsByID + "offers user ids for completion, with username and display name as descriptions only",
}

const (
	kindsImportPath  = "github.com/rancher/rancher-cli/internal/engine/kinds"
	rbacImportPath   = "github.com/rancher/rancher-cli/internal/engine/rbac"
	schemaImportPath = "k8s.io/apimachinery/pkg/runtime/schema"
	modulePath       = "github.com/rancher/rancher-cli/"
)

// userScanFile is one parsed file with what the scan needs to read it: its
// package directory and the name it gives each import (an alias, the last
// path element, or "." for a dot import).
type userScanFile struct {
	file    *ast.File
	dir     string
	imports map[string]string
}

// userScanDecl is one function declaration or package-level variable, the
// node the scan reads for it (a function body or a variable's initializer),
// whether it is a variable, and whether it is a function whose results
// include kinds.Kind.
type userScanDecl struct {
	key        string
	name       string
	node       ast.Node
	file       *userScanFile
	isVar      bool
	returnKind bool
}

// userLookupSites returns, for each function declaration and package-level
// variable of the files under internal/ and cmd/, keyed "<package dir>.<Func>",
// "<package dir>.<Recv>.<Method>" or "<package dir>.<Var>", the position of
// its first reach of the user kind or a user resolver: kinds.Lookup or
// kinds.ByKey with "user" or "users", kinds.LookupGVK with the kind "User",
// a schema.GroupVersionResource literal of the resource "users" or a
// WithResource("users") call outside the kinds package, a reference to
// rbac.ResolveSubject or rbac.ResolveInspectUser, a call of a function of the
// same package that is a site and returns kinds.Kind, or a reference to a
// package-level variable of the same package that holds the user kind row or
// the users resource (its initializer is itself one of these reaches, such as
// `var userRow, _ = kinds.Lookup("user")`). Each value may be written as a
// string literal or as a constant; the kinds, schema and rbac packages are
// recognized under any import name. The blank identifier is never a key.
//
// A constant declared inside a function counts for the whole package, and a
// local name that shadows a variable holding the user kind row still counts
// as a reference to it: both only add sites. Shapes the scan does not find: a
// method, or a function of another package, that returns the user kind row
// or the users resource; a function value that holds such a function; a
// GroupVersionResource literal with positional fields, or one whose type is
// left out inside a slice or map literal; and a raw /v3/users path.
func userLookupSites(fset *token.FileSet, paths []string, files []*ast.File) map[string]string {
	consts := map[string]map[string]string{}
	var decls []userScanDecl
	for i, f := range files {
		p := paths[i]
		if !strings.HasPrefix(p, "internal/") && !strings.HasPrefix(p, "cmd/") {
			continue
		}
		sf := &userScanFile{file: f, dir: path.Dir(p), imports: userScanImports(f)}
		collectUserScanConsts(f, consts, sf.dir)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				key := sf.dir + "." + d.Name.Name
				if recv := methodReceiverName(d); recv != "" {
					key = sf.dir + "." + recv + "." + d.Name.Name
				}
				decls = append(decls, userScanDecl{key: key, name: d.Name.Name, node: d.Body, file: sf,
					returnKind: d.Recv == nil && resultsIncludeKind(d, sf.imports)})
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for j, name := range vs.Names {
						if name.Name == "_" {
							continue
						}
						for k, value := range vs.Values {
							if len(vs.Values) == len(vs.Names) && k != j {
								continue
							}
							decls = append(decls, userScanDecl{key: sf.dir + "." + name.Name, name: name.Name, node: value, file: sf, isVar: true})
						}
					}
				}
			}
		}
	}
	sites := map[string]string{}
	// kindFuncs holds, per package directory, the functions that are sites and
	// return kinds.Kind: a call of one of them reaches the user kind too.
	// kindVars holds the variables whose initializer is itself a reach of the
	// user kind: a reference to one of them reaches it too.
	kindFuncs := map[string]map[string]bool{}
	kindVars := map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, dir, name string) {
		if m[dir] == nil {
			m[dir] = map[string]bool{}
		}
		m[dir][name] = true
	}
	for changed := true; changed; {
		changed = false
		for _, d := range decls {
			if _, ok := sites[d.key]; ok {
				continue
			}
			pos := firstUserLookup(d.file, d.node, consts, kindFuncs[d.file.dir], kindVars[d.file.dir])
			if !pos.IsValid() {
				continue
			}
			sites[d.key] = fset.Position(pos).String()
			changed = true
			if d.returnKind {
				add(kindFuncs, d.file.dir, d.name)
			}
			if d.isVar && pos == d.node.Pos() {
				add(kindVars, d.file.dir, d.name)
			}
		}
	}
	return sites
}

// userScanImports maps each import path of f to the name the file uses for it.
func userScanImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[p] = name
	}
	return out
}

// collectUserScanConsts records every constant of f, at package level or inside
// a function, whose value is a string literal, under the package directory
// dir.
func collectUserScanConsts(f *ast.File, consts map[string]map[string]string, dir string) {
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for j, name := range vs.Names {
				if j < len(vs.Values) {
					if v := stringLiteral(vs.Values[j]); v != "" {
						if consts[dir] == nil {
							consts[dir] = map[string]string{}
						}
						consts[dir][name.Name] = v
					}
				}
			}
		}
		return true
	})
}

// resultsIncludeKind reports whether d returns a kinds.Kind.
func resultsIncludeKind(d *ast.FuncDecl, imports map[string]string) bool {
	if d.Type.Results == nil {
		return false
	}
	for _, r := range d.Type.Results.List {
		if isImportedName(r.Type, imports, kindsImportPath, "Kind") {
			return true
		}
	}
	return false
}

// isImportedName reports whether x names the identifier name of the package at
// importPath, under the name the file gives that import.
func isImportedName(x ast.Expr, imports map[string]string, importPath, name string) bool {
	local, ok := imports[importPath]
	if !ok {
		return false
	}
	if local == "." {
		id, ok := x.(*ast.Ident)
		return ok && id.Name == name
	}
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == local
}

// userScanStringValue is the value of a string literal, of a string constant of the
// package directory dir, or of a string constant of another package of the
// module, or "" for anything else.
func userScanStringValue(x ast.Expr, sf *userScanFile, consts map[string]map[string]string) string {
	switch v := x.(type) {
	case *ast.BasicLit:
		return stringLiteral(v)
	case *ast.Ident:
		return consts[sf.dir][v.Name]
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		if !ok {
			return ""
		}
		for p, local := range sf.imports {
			if local == id.Name && strings.HasPrefix(p, modulePath) {
				return consts[strings.TrimPrefix(p, modulePath)][v.Sel.Name]
			}
		}
	}
	return ""
}

// firstUserLookup returns the position of the first reach of the user kind or
// a user resolver inside n, or token.NoPos. kindFuncs are the functions of
// the package that reach the user kind and return kinds.Kind; kindVars are
// the variables of the package that hold the user kind row or the users
// resource.
func firstUserLookup(sf *userScanFile, n ast.Node, consts map[string]map[string]string, kindFuncs, kindVars map[string]bool) token.Pos {
	pos := token.NoPos
	inKinds := sf.dir == "internal/engine/kinds"
	value := func(x ast.Expr) string { return userScanStringValue(x, sf, consts) }
	ast.Inspect(n, func(n ast.Node) bool {
		if pos.IsValid() {
			return false
		}
		switch v := n.(type) {
		case *ast.CallExpr:
			switch {
			case isImportedName(v.Fun, sf.imports, kindsImportPath, "Lookup") || isImportedName(v.Fun, sf.imports, kindsImportPath, "ByKey"):
				if len(v.Args) > 0 && slices.Contains([]string{"user", "users"}, value(v.Args[0])) {
					pos = v.Pos()
				}
			case isImportedName(v.Fun, sf.imports, kindsImportPath, "LookupGVK"):
				if len(v.Args) > 1 && value(v.Args[1]) == "User" {
					pos = v.Pos()
				}
			}
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && !inKinds && sel.Sel.Name == "WithResource" && len(v.Args) == 1 && value(v.Args[0]) == "users" {
				pos = v.Pos()
			}
			if id, ok := v.Fun.(*ast.Ident); ok && kindFuncs[id.Name] {
				pos = v.Pos()
			}
		case *ast.CompositeLit:
			if inKinds || !isImportedName(v.Type, sf.imports, schemaImportPath, "GroupVersionResource") {
				return true
			}
			for _, elt := range v.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Resource" && value(kv.Value) == "users" {
						pos = v.Pos()
					}
				}
			}
		case *ast.SelectorExpr:
			if isImportedName(v, sf.imports, rbacImportPath, "ResolveSubject") || isImportedName(v, sf.imports, rbacImportPath, "ResolveInspectUser") {
				pos = v.Pos()
			}
		case *ast.Ident:
			if sf.dir == "internal/engine/rbac" && (v.Name == "ResolveSubject" || v.Name == "ResolveInspectUser") || kindVars[v.Name] {
				pos = v.Pos()
			}
		}
		return true
	})
	return pos
}

// TestEveryUserLookupIsListed asserts every function and package-level
// variable under internal/ and cmd/ that reaches the user kind or a user
// resolver is a key of userLookups, and every key is still found: a new
// resolver of a user value cannot appear without a row in a behavior table
// that asserts it never matches a display name.
func TestEveryUserLookupIsListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	sites := userLookupSites(tree.fset, tree.paths, tree.files)
	for key, pos := range sites {
		if _, ok := userLookups[key]; !ok {
			t.Errorf("%s: %s reaches the user kind or a user resolver and is not in userLookups; give it a row in TestNoUserSubjectResolverMatchesADisplayName (or TestAccessUserLookupMatchesNoDisplayName) if it resolves a user value, else say why it reads users by id or for display only", pos, key)
		}
	}
	for key, value := range userLookups {
		if _, ok := sites[key]; !ok {
			t.Errorf("%s is in userLookups but no longer reaches the user kind or a user resolver; remove its entry", key)
		}
		reason, readsByID := strings.CutPrefix(value, userLookupReadsByID)
		assert.True(t, value == userLookupResolverRow || value == userLookupAccessRow || (readsByID && reason != ""),
			"%s: the entry names a behavior table row or says why it reads users by id or for display only: %q", key, value)
	}
}

// userLookupFixtureSites parses each source of files (path to text) and
// returns the sorted keys of the sites the scan finds in them.
func userLookupFixtureSites(t *testing.T, files map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var paths []string
	var parsed []*ast.File
	for _, p := range slices.Sorted(maps.Keys(files)) {
		f, err := parser.ParseFile(fset, p, files[p], 0)
		require.NoError(t, err, p)
		paths = append(paths, p)
		parsed = append(parsed, f)
	}
	return slices.Sorted(maps.Keys(userLookupSites(fset, paths, parsed)))
}

// TestUserLookupScanFixtures asserts the scan's rules on source snippets: each
// way of reaching the user kind or a user resolver is found, under the key of
// the function, method or variable it is in, and other kinds are not.
func TestUserLookupScanFixtures(t *testing.T) {
	t.Parallel()
	direct := `package p
import (
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
)
var direct = func() int { kinds.Lookup("user"); return 0 }()
var other = kinds.Lookup("cluster")
func byKey() { kinds.ByKey("users") }
func (r *recv) byGVK() { kinds.LookupGVK(v, "User") }
func (r gen[T]) byGVR() { kinds.ByGVR(schema.GroupVersionResource{Group: "management.cattle.io", Resource: "users"}) }
func resolves() { rbac.ResolveInspectUser(ctx, c, a) }
func otherKind() { kinds.Lookup("project"); kinds.LookupGVK(v, "Project") }
func noBody()
`
	assert.Equal(t, []string{"internal/x.byKey", "internal/x.direct", "internal/x.gen.byGVR", "internal/x.recv.byGVK", "internal/x.resolves"},
		userLookupFixtureSites(t, map[string]string{"internal/x/p.go": direct}))

	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "a call of a function of the same package that returns the user kind", files: map[string]string{
			"internal/x/a.go": `package x
import "github.com/rancher/rancher-cli/internal/engine/kinds"
func userKind() kinds.Kind { k, _ := kinds.Lookup("user"); return k }
func otherKind() kinds.Kind { k, _ := kinds.Lookup("project"); return k }
`,
			"internal/x/b.go": `package x
func plan() { _ = userKind() }
func planOther() { _ = otherKind() }
`,
			"internal/y/c.go": `package y
func userKind() int { return 0 }
func elsewhere() { _ = userKind() }
`,
		}, want: []string{"internal/x.plan", "internal/x.userKind"}},
		{name: "a function of the same package that reads a variable holding the user kind", files: map[string]string{
			"internal/x/a.go": `package x
import (
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/runtime/schema"
)
var userRow, _ = kinds.Lookup("user")
var usersGVR = schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("users")
var rowGVR = userRow.GVR
var prefix = func() string { k, _ := kinds.Lookup("user"); return k.Name }()
var _ = kinds.Lookup("users")
`,
			"internal/x/b.go": `package x
func resolve() { _ = userRow.GVR }
func list() { _ = usersGVR }
func viaRow() { _ = rowGVR }
func readsPrefix() { _ = prefix }
`,
			"internal/y/c.go": `package y
func elsewhere() { _ = userRow }
`,
		}, want: []string{"internal/x.list", "internal/x.prefix", "internal/x.resolve", "internal/x.rowGVR", "internal/x.userRow", "internal/x.usersGVR", "internal/x.viaRow"}},
		{name: "a constant argument", files: map[string]string{
			"internal/x/a.go": `package x
import "github.com/rancher/rancher-cli/internal/engine/kinds"
const userKey = "user"
func byConst() { kinds.Lookup(userKey) }
func byLocalConst() { const k = "users"; kinds.ByKey(k) }
func byOtherPackage() { kinds.Lookup(names.User) }
`,
			"internal/names/n.go": `package names
const User = "user"
`,
		}, want: []string{"internal/x.byConst", "internal/x.byLocalConst"}},
		{name: "a constant of another package of the module", files: map[string]string{
			"internal/x/a.go": `package x
import (
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/names"
)
func byOtherPackage() { kinds.Lookup(names.User) }
`,
			"internal/names/n.go": `package names
const User = "user"
`,
		}, want: []string{"internal/x.byOtherPackage"}},
		{name: "a renamed or dot import", files: map[string]string{
			"internal/x/a.go": `package x
import (
	k "github.com/rancher/rancher-cli/internal/engine/kinds"
	r "github.com/rancher/rancher-cli/internal/engine/rbac"
)
func renamed() { k.Lookup("user") }
func renamedResolver() { r.ResolveSubject(ctx, c, s) }
func notTheImport() { kinds.Lookup("user") }
`,
			"internal/x/b.go": `package x
import . "github.com/rancher/rancher-cli/internal/engine/kinds"
func dot() { ByKey("user") }
`,
		}, want: []string{"internal/x.dot", "internal/x.renamed", "internal/x.renamedResolver"}},
		{name: "WithResource", files: map[string]string{
			"internal/x/a.go": `package x
import "k8s.io/apimachinery/pkg/runtime/schema"
var gvr = schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("users")
func other() { schema.GroupVersion{}.WithResource("projects") }
`,
		}, want: []string{"internal/x.gvr"}},
		{name: "a resolver call inside the rbac package", files: map[string]string{
			"internal/engine/rbac/p.go": `package rbac
func plan() { ResolveSubject(ctx, c, s) }
`,
		}, want: []string{"internal/engine/rbac.plan"}},
		{name: "the kinds package declares the user row", files: map[string]string{
			"internal/engine/kinds/p.go": `package kinds
import "k8s.io/apimachinery/pkg/runtime/schema"
var row = schema.GroupVersionResource{Resource: "users"}
var other = schema.GroupVersion{}.WithResource("users")
`,
		}},
		{name: "cmd is scanned and other trees are not", files: map[string]string{
			"cmd/x/main.go": `package main
import "github.com/rancher/rancher-cli/internal/engine/kinds"
func main() { kinds.Lookup("user") }
`,
			"install/x/p.go": `package x
import "github.com/rancher/rancher-cli/internal/engine/kinds"
func outside() { kinds.Lookup("user") }
`,
		}, want: []string{"cmd/x.main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := userLookupFixtureSites(t, tt.files)
			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
