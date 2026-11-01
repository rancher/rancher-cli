package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archiveSegmentAllowlist names the files of internal/engine/observe whose
// entryPath calls the segment scan leaves unread, each with the reason. It is
// empty: every call is read.
var archiveSegmentAllowlist = map[string]string{}

// segmentJoinPackages are the packages whose calls join values into one
// string. A call of one of them as an entryPath argument is refused.
var segmentJoinPackages = map[string]bool{"fmt": true, "strings": true, "path": true, "filepath": true}

// segmentScan reads the entryPath calls of one set of files and collects what
// breaks the rule that a path segment of an archive entry name holds at most
// one value from the server.
type segmentScan struct {
	fset *token.FileSet
	// fixed holds the package-level constants whose value is a string literal.
	fixed map[string]bool

	offenders map[string]bool

	calls             int
	callsWithChain    int
	followedIdentArgs int
}

// segmentOffenders returns the offenders found in files, whose paths are in
// paths. Every function body is read; a call outside a function is read with
// no assignments to follow.
func segmentOffenders(fset *token.FileSet, files []*ast.File, paths []string) (*segmentScan, []string) {
	s := &segmentScan{fset: fset, fixed: map[string]bool{}, offenders: map[string]bool{}}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s.fixed[name.Name] = true
					}
				}
			}
		}
	}
	var out []string
	for i, file := range files {
		if _, allowed := archiveSegmentAllowlist[paths[i]]; allowed {
			continue
		}
		s.offenders = map[string]bool{}
		for _, decl := range file.Decls {
			var scope *ast.FuncDecl
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body == nil {
					continue
				}
				scope = fn
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "entryPath" {
					s.readCall(call, scope)
				}
				return true
			})
		}
		for msg := range s.offenders {
			out = append(out, msg)
		}
	}
	return s, out
}

func (s *segmentScan) report(pos token.Pos, msg string) {
	s.offenders[s.fset.Position(pos).String()+": "+msg] = true
}

func (s *segmentScan) readCall(call *ast.CallExpr, scope *ast.FuncDecl) {
	s.calls++
	if call.Ellipsis.IsValid() {
		s.report(call.Pos(), "entryPath is called with a spread argument; the segments it joins cannot be read")
		return
	}
	for _, arg := range call.Args {
		if bin, ok := arg.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
			s.callsWithChain++
		}
		if id, ok := arg.(*ast.Ident); ok && len(assignmentsTo(scope, id.Name)) > 0 {
			s.followedIdentArgs++
		}
		if n := s.values(arg, scope, map[ast.Node]bool{}); n > 1 {
			s.report(arg.Pos(), "a path segment holds more than one value; pass each value as its own segment")
		}
	}
}

// assignment is one right side that sets a name, with the operator that sets
// it.
type assignment struct {
	node ast.Node
	rhs  ast.Expr
	add  bool
}

// assignmentsTo returns every assignment, definition and variable declaration
// of name in scope.
func assignmentsTo(scope *ast.FuncDecl, name string) []assignment {
	if scope == nil {
		return nil
	}
	var out []assignment
	ast.Inspect(scope.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				rhs := v.Rhs[0]
				if len(v.Lhs) == len(v.Rhs) {
					rhs = v.Rhs[i]
				}
				out = append(out, assignment{node: v, rhs: rhs, add: v.Tok == token.ADD_ASSIGN})
			}
		case *ast.ValueSpec:
			for i, id := range v.Names {
				if id.Name != name || len(v.Values) == 0 {
					continue
				}
				rhs := v.Values[0]
				if len(v.Names) == len(v.Values) {
					rhs = v.Values[i]
				}
				out = append(out, assignment{node: v, rhs: rhs})
			}
		}
		return true
	})
	return out
}

// values returns how many server values e holds. A string literal and a
// package-level constant set from one hold none. An identifier holds the most
// any assignment to it in the function gives; a "+=" adds its right side to
// the most the other assignments give. A name with no assignment in the
// function (a parameter, a range variable, a package variable) is one value,
// and so is any expression that is not one of these. A call of a package
// that joins strings is an offender in its own right.
func (s *segmentScan) values(e ast.Expr, scope *ast.FuncDecl, visiting map[ast.Node]bool) int {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return s.values(v.X, scope, visiting)
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return 0
		}
		return 1
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return s.values(v.X, scope, visiting) + s.values(v.Y, scope, visiting)
		}
		return 1
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && segmentJoinPackages[pkg.Name] {
				s.report(v.Pos(), "a path segment is built by a call of "+pkg.Name+"."+sel.Sel.Name+"; pass each value as its own segment")
			}
		}
		return 1
	case *ast.Ident:
		assigns := assignmentsTo(scope, v.Name)
		if len(assigns) == 0 {
			if s.fixed[v.Name] {
				return 0
			}
			return 1
		}
		best := 0
		for _, a := range assigns {
			if a.add || visiting[a.node] {
				continue
			}
			visiting[a.node] = true
			best = max(best, s.values(a.rhs, scope, visiting))
			delete(visiting, a.node)
		}
		for _, a := range assigns {
			if !a.add || visiting[a.node] {
				continue
			}
			visiting[a.node] = true
			best = max(best, best+s.values(a.rhs, scope, visiting))
			delete(visiting, a.node)
		}
		return best
	}
	return 1
}

// TestArchiveEntrySegmentsHoldOneValue asserts that no path segment of a
// support-bundle archive entry name holds two values from the server, which
// would let two different pairs of values make one name (the pod "a" with the
// container "b-c" and the pod "a-b" with the container "c" both make
// "a-b-c"). Over every production file of internal/engine/observe, each
// argument of every entryPath call is one of:
//
//  1. a string literal or a package-level constant set from one (fixed text);
//  2. one expression that is not fixed text (a value);
//  3. a "+" chain of fixed text with at most one value.
//
// A call of fmt, strings, path or filepath as an argument fails; a "+" chain
// with two values fails; a spread call (entryPath(parts...)) fails. An
// identifier argument is followed: every assignment to it in the enclosing
// function is read by the same rule, and "x += y" counts as a chain that
// already holds the value of x.
//
// The scan reads call shapes in the syntax tree only. It cannot see a helper
// function that joins two values and returns one, a value that is itself two
// values joined elsewhere, a field or a slice element passed as the argument
// (read as one value), an identifier set in another function, a name built
// outside the package, entryPath called through a function value, a strings
// or fmt call through an aliased import or a function value, a
// strings.Builder, or a string conversion of joined bytes. The writer's
// refusal of a repeated name and the bijection test of the log names are the
// guards for those.
func TestArchiveEntrySegmentsHoldOneValue(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	const (
		observeDir = "internal/engine/observe"
		bundleFile = observeDir + "/bundle.go"
	)
	var files []*ast.File
	var paths []string
	for i, file := range tree.files {
		if path.Dir(tree.paths[i]) == observeDir {
			files = append(files, file)
			paths = append(paths, tree.paths[i])
		}
	}
	assert.Contains(t, paths, bundleFile, "the scan did not see bundle.go")

	scan, offenders := segmentOffenders(tree.fset, files, paths)
	assert.Empty(t, offenders, "a path segment of an archive entry name holds one value at most")
	assert.Positive(t, scan.calls, "the scan saw no entryPath call")
	assert.Positive(t, scan.callsWithChain, "the scan saw no entryPath argument that is a + chain")
	assert.Positive(t, scan.followedIdentArgs, "the scan followed no identifier argument of entryPath")
	for file, reason := range archiveSegmentAllowlist {
		assert.NotEmpty(t, reason, file)
		assert.Contains(t, paths, file, "the allowlist names a file the scan did not see")
	}
}

// TestArchiveEntrySegmentScanReadsEachShape asserts the scan's reading on
// small sources: the shapes it accepts, and each way of putting two values in
// one segment that it must refuse.
func TestArchiveEntrySegmentScanReadsEachShape(t *testing.T) {
	t.Parallel()
	const header = "package observe\n\nconst fixedSegment = \"logs\"\n\nvar notConstant = \"x\"\n\n"
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"fixed and one value in a chain", `func f(a string) { entryPath("logs", a, "events-"+a+".json") }`, 0},
		{"constant segment", `func f(a string) { entryPath(fixedSegment, a) }`, 0},
		{"one value from an assignment", `func f(a []string) { p := a[0]; entryPath("logs", p, "x.log") }`, 0},
		{"value chain assigned to a variable", `func f(a string) { s := "x-" + a; entryPath("logs", s) }`, 0},
		{"two values in a chain", `func f(a, b string) { entryPath("logs", a+"-"+b+".log") }`, 1},
		{"two values in a parenthesis", `func f(a, b string) { entryPath("logs", (a + "-" + b)) }`, 1},
		{"joined by Sprintf", `func f(a, b string) { entryPath("logs", fmt.Sprintf("%s-%s.log", a, b)) }`, 1},
		{"joined by strings.Join", `func f(a, b string) { entryPath("logs", strings.Join([]string{a, b}, "-")) }`, 1},
		{"joined by path.Join", `func f(a, b string) { entryPath("logs", path.Join(a, b)) }`, 1},
		{"joined by filepath.Join", `func f(a, b string) { entryPath("logs", filepath.Join(a, b)) }`, 1},
		{"two values in the cluster segment", `func f(c, t string) { entryPath("logs", c+"-"+t) }`, 1},
		{"chain assigned to a variable", `func f(a, b string) { s := a + "-" + b + ".log"; entryPath("logs", s) }`, 1},
		{"chain assigned with a var declaration", `func f(a, b string) { var s = a + "-" + b; entryPath("logs", s) }`, 1},
		{"value then += a value", `func f(a, b string) { s := a; s += "-" + b; entryPath("logs", s) }`, 1},
		{"value then = itself plus a value", `func f(a, b string) { s := a; s = s + "-" + b; entryPath("logs", s) }`, 1},
		{"fixed text then += a value", `func f(a string) { s := "x-"; s += a; entryPath("logs", s) }`, 0},
		{"fixed text then = itself plus a value", `func f(a string) { s := "x-"; s = s + a; entryPath("logs", s) }`, 0},
		{"chain through a second variable", `func f(a, b string) { s := a + "-" + b; u := s; entryPath("logs", u) }`, 1},
		{"spread call", `func f(parts []string) { entryPath(parts...) }`, 1},
		{"two calls, one refused", `func f(a, b string) { entryPath("logs", a); entryPath("logs", a+b) }`, 1},
		{"call outside a function", `var _, _ = entryPath("logs", notConstant+notConstant)`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "observe/x.go", header+tc.body+"\n", parser.AllErrors)
			require.NoError(t, err, tc.body)
			_, offenders := segmentOffenders(fset, []*ast.File{file}, []string{"internal/engine/observe/x.go"})
			assert.Len(t, offenders, tc.want, "%s: %v", strings.TrimSpace(tc.body), offenders)
		})
	}
}
