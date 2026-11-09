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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	outputWritesDir = "test/conformance/testdata/outputwrites"
	urfaveCLIPath   = "github.com/urfave/cli/v3"
	uiPath          = modulePrefix + "internal/ui"
	outputPath      = modulePrefix + "internal/output"
)

// fieldReference is one reference to a struct field: the function it is in,
// whether it writes the field (a composite literal key, the left side of an
// assignment or an increment, or the operand of &) and what holds it, as
// referenceCallee names it. A scan that does not look at what holds a
// reference leaves callee empty.
type fieldReference struct {
	function string
	write    bool
	pos      string
	callee   string
}

// fieldOwner is the type whose struct declares the field a selection
// names: the type the selection's receiver holds, or, for a field promoted
// through an embedded struct, the embedded type that declares it.
func fieldOwner(sel *types.Selection) types.Type {
	t := sel.Recv()
	index := sel.Index()
	for _, i := range index[:len(index)-1] {
		st, ok := derefType(t).Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		t = st.Field(i).Type()
	}
	return derefType(t)
}

// declarationFunction is the key of the function a declaration is in, as
// enclosingFunctionKey writes it, or "<package path>.<package level>" for a
// declaration that is not a function. A function literal belongs to the
// function that declares it.
func declarationFunction(pkg *packages.Package, decl ast.Decl) string {
	if fd, ok := decl.(*ast.FuncDecl); ok {
		return enclosingFunctionKey(pkg, fd)
	}
	return strings.TrimPrefix(pkg.PkgPath, modulePrefix) + ".<package level>"
}

// fieldReferences returns every reference in the non-test files of pkgs to
// the field fieldName of the struct type typeName of the package at pkgPath.
// It follows a selector (read or written, also one promoted through an
// embedded struct) and a key of a composite literal of the type, and finds
// the type by what go/types resolves, never by the field's name alone, so a
// field of another type with the same name is not counted. It does not
// follow a write through a value that holds a copy of the field's value (the
// reference where the copy is made is counted, the writes through the copy
// are not), nor anything a library does through the field inside its own
// code.
func fieldReferences(pkgs []*packages.Package, pkgPath, typeName, fieldName string) []fieldReference {
	var out []fieldReference
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			for _, decl := range f.Decls {
				function := declarationFunction(pkg, decl)
				writes := map[ast.Expr]bool{}
				var stack []ast.Node
				ast.Inspect(decl, func(n ast.Node) bool {
					if n == nil {
						stack = stack[:len(stack)-1]
						return true
					}
					stack = append(stack, n)
					switch n := n.(type) {
					case *ast.AssignStmt:
						for _, lhs := range n.Lhs {
							writes[ast.Unparen(lhs)] = true
						}
					case *ast.IncDecStmt:
						writes[ast.Unparen(n.X)] = true
					case *ast.UnaryExpr:
						if n.Op == token.AND {
							writes[ast.Unparen(n.X)] = true
						}
					case *ast.CompositeLit:
						if !isNamedType(derefType(info.TypeOf(n)), pkgPath, typeName) {
							return true
						}
						for _, e := range n.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								if id, ok := kv.Key.(*ast.Ident); ok && id.Name == fieldName {
									literal := "literal " + path.Base(pkgPath) + "." + typeName + "." + fieldName
									out = append(out, fieldReference{function: function, write: true, pos: pkg.Fset.Position(id.Pos()).String(), callee: literal})
								}
							}
						}
					case *ast.SelectorExpr:
						sel := info.Selections[n]
						if sel == nil || sel.Kind() != types.FieldVal || sel.Obj().Name() != fieldName {
							return true
						}
						if owner := fieldOwner(sel); owner != nil && isNamedType(owner, pkgPath, typeName) {
							out = append(out, fieldReference{function: function, write: writes[n], pos: pkg.Fset.Position(n.Sel.Pos()).String(), callee: referenceCallee(info, stack)})
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// referencesByFunction counts refs by function, those that write the field
// or those that do not.
func referencesByFunction(refs []fieldReference, write bool) map[string]int {
	out := map[string]int{}
	for _, r := range refs {
		if r.write == write {
			out[r.function]++
		}
	}
	return out
}

// allReferencesByFunction counts every reference in refs by function.
func allReferencesByFunction(refs []fieldReference) map[string]int {
	out := map[string]int{}
	for _, r := range refs {
		out[r.function]++
	}
	return out
}

// referencesByCallee counts every reference in refs by the function it is in
// and what holds it, written "<function> -> <callee>".
func referencesByCallee(refs []fieldReference) map[string]int {
	out := map[string]int{}
	for _, r := range refs {
		out[r.function+" -> "+r.callee]++
	}
	return out
}

// noCall is the callee of a reference that no call holds directly.
const noCall = "no call"

// untypedNoCall is noCall for a file read without types.
const untypedNoCall = "untyped " + noCall

// heldBy walks up the nodes enclosing the reference at the top of stack and
// returns the call that holds it directly as an argument, or the selector
// that has it as its receiver and the call of that selector. Parentheses are
// removed. Anything deeper is not held directly: a reference inside a
// function literal or a composite literal that is an argument is held by
// whatever holds it inside that literal. At most one of call and receiver is
// set; both are nil when no call holds the reference.
func heldBy(stack []ast.Node) (call *ast.CallExpr, receiver *ast.SelectorExpr) {
	node := stack[len(stack)-1]
	for i := len(stack) - 2; i >= 0; i-- {
		switch parent := stack[i].(type) {
		case *ast.ParenExpr:
			node = parent
		case *ast.CallExpr:
			for _, arg := range parent.Args {
				if ast.Node(arg) == node {
					return parent, nil
				}
			}
			return nil, nil
		case *ast.SelectorExpr:
			if ast.Node(parent.X) != node || i == 0 {
				return nil, nil
			}
			if c, ok := stack[i-1].(*ast.CallExpr); ok && ast.Node(ast.Unparen(c.Fun)) == ast.Node(parent) {
				return nil, parent
			}
			return nil, nil
		default:
			return nil, nil
		}
	}
	return nil, nil
}

// referenceCallee names what holds the reference at the top of stack in a
// type-checked file:
//   - an argument of a call to a declared function or method: the function,
//     as "<package>.<function>" or "<package>.<Type>.<method>" with the
//     module prefix trimmed, a method of an interface included;
//   - an argument of a call through a function-typed variable, parameter or
//     field: "func value <the call's function as written>";
//   - the receiver of a method call: "receiver of <the method as above>";
//   - a direct argument of a conversion or a built-in: "conversion <type>"
//     or "builtin <name>";
//   - every other place: noCall.
func referenceCallee(info *types.Info, stack []ast.Node) string {
	call, receiver := heldBy(stack)
	switch {
	case call != nil:
		return calleeOfCall(info, call)
	case receiver != nil:
		if s := info.Selections[receiver]; s != nil && s.Kind() == types.MethodVal {
			if fn, ok := s.Obj().(*types.Func); ok {
				return "receiver of " + declaredFunctionKey(fn)
			}
		}
		return "receiver of " + types.ExprString(receiver)
	}
	return noCall
}

// untypedCallee is referenceCallee for a file read without types: "untyped "
// and the call's function as written, or untypedNoCall.
func untypedCallee(stack []ast.Node) string {
	call, receiver := heldBy(stack)
	switch {
	case call != nil:
		return "untyped " + types.ExprString(call.Fun)
	case receiver != nil:
		return "untyped " + types.ExprString(receiver)
	}
	return untypedNoCall
}

// calleeOfCall names the function a call calls, as referenceCallee does.
func calleeOfCall(info *types.Info, call *ast.CallExpr) string {
	fun := ast.Unparen(call.Fun)
	if tv, ok := info.Types[fun]; ok {
		switch {
		case tv.IsType():
			return "conversion " + types.TypeString(tv.Type, func(p *types.Package) string { return p.Name() })
		case tv.IsBuiltin():
			return "builtin " + types.ExprString(fun)
		}
	}
	var obj types.Object
	switch f := fun.(type) {
	case *ast.Ident:
		obj = info.Uses[f]
	case *ast.SelectorExpr:
		if s := info.Selections[f]; s != nil {
			if s.Kind() == types.MethodVal {
				obj = s.Obj()
			}
		} else {
			obj = info.Uses[f.Sel]
		}
	}
	if fn, ok := obj.(*types.Func); ok {
		return declaredFunctionKey(fn)
	}
	return "func value " + types.ExprString(fun)
}

// declaredFunctionKey is the key of a declared function or method with the module
// prefix trimmed: "<package path>.<function>" or "<package path>.<Type>.<method>".
func declaredFunctionKey(fn *types.Func) string {
	name := fn.FullName()
	if rest, ok := strings.CutPrefix(name, "("); ok {
		recv, method, _ := strings.Cut(rest, ").")
		name = strings.TrimPrefix(recv, "*") + "." + method
	}
	return strings.TrimPrefix(name, modulePrefix)
}

// objectUses returns every reference in the non-test files of pkgs to an
// object match accepts (a call, a value, a read), outside the definition of a
// function, by the object go/types resolves for the identifier.
func objectUses(pkgs []*packages.Package, match func(types.Object) bool) []fieldReference {
	var out []fieldReference
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			for _, decl := range f.Decls {
				function := declarationFunction(pkg, decl)
				ast.Inspect(decl, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						if obj := info.Uses[id]; obj != nil && match(obj) {
							out = append(out, fieldReference{function: function, pos: pkg.Fset.Position(id.Pos()).String()})
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// isFunctionOf returns a matcher for the package function name of the package
// at pkgPath, not a method.
func isFunctionOf(pkgPath string, names ...string) func(types.Object) bool {
	return func(obj types.Object) bool {
		fn, ok := obj.(*types.Func)
		return ok && slices.ContainsFunc(names, func(name string) bool { return isPackageFunction(fn, pkgPath, name) })
	}
}

// releasePlatformsLine is the loop of the release build script that builds
// each platform: the pairs of GOOS and GOARCH it lists, as "goos/goarch".
var releasePlatformsLine = regexp.MustCompile(`for platform in ((?:[a-z0-9]+/[a-z0-9]+ ?)+); do`)

// releasePlatforms are the GOOS and GOARCH pairs the release builds, read
// from the build script release.yaml runs, as "goos/goarch".
func releasePlatforms() ([]string, error) {
	src, err := os.ReadFile(releaseBuildScript)
	if err != nil {
		return nil, err
	}
	m := releasePlatformsLine.FindSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("%s: no loop over the platforms to build", releaseBuildScript)
	}
	return strings.Fields(string(m[1])), nil
}

// extraFile is a source file that a platform builds and the host does not,
// parsed and not type-checked.
type extraFile struct {
	pkgPath string
	fset    *token.FileSet
	file    *ast.File
}

// systemPackages holds the packages of one pattern as the host builds them,
// type-checked, and, for each platform the release builds (keyed
// "goos/goarch"), the files that platform builds and the host does not,
// parsed.
type systemPackages struct {
	platforms []string
	typed     []*packages.Package
	extras    map[string][]extraFile
}

// loadSystemPackages reads the packages of pattern, the non-test files only,
// with cgo off. The host is type-checked in full, with the environment as it
// is. Each platform the release builds costs a listing of its files only: the
// files it builds that the host does not are parsed, since type-checking a
// platform compiles the export data of every dependency for it.
func loadSystemPackages(pattern string) (systemPackages, error) {
	platforms, err := releasePlatforms()
	if err != nil {
		return systemPackages{}, err
	}
	typed, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles,
		Env:  append(os.Environ(), "CGO_ENABLED=0"),
	}, pattern)
	if err != nil {
		return systemPackages{}, err
	}
	hostFiles := map[string]bool{}
	for _, pkg := range typed {
		for _, e := range pkg.Errors {
			return systemPackages{}, fmt.Errorf("type-check %s: %s", pkg.PkgPath, e)
		}
		for _, f := range pkg.GoFiles {
			hostFiles[f] = true
		}
	}
	out := systemPackages{platforms: platforms, typed: typed, extras: map[string][]extraFile{}}
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		listed, err := packages.Load(&packages.Config{
			Mode: packages.NeedName | packages.NeedFiles,
			Env:  append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0"),
		}, pattern)
		if err != nil {
			return systemPackages{}, err
		}
		fset := token.NewFileSet()
		for _, pkg := range listed {
			for _, name := range pkg.GoFiles {
				if hostFiles[name] {
					continue
				}
				file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
				if err != nil {
					return systemPackages{}, fmt.Errorf("parse %s for %s: %w", name, platform, err)
				}
				out.extras[platform] = append(out.extras[platform], extraFile{pkg.PkgPath, fset, file})
			}
		}
	}
	return out, nil
}

// outputScan is one scan of the tree: how it reads a type-checked package, and
// how it reads a file that only another platform builds. The first resolves
// every identifier by go/types. The second has no types: it counts every
// identifier that has the name the scan looks for, so it may count a reference
// to something of another type, which a list then names.
type outputScan struct {
	typed     func([]*packages.Package) []fieldReference
	syntactic func(pkgPath string, fset *token.FileSet, f *ast.File) []fieldReference
}

// fieldScan is the scan of the field fieldName of the struct type typeName of
// the package at pkgPath.
func fieldScan(pkgPath, typeName, fieldName string) outputScan {
	return outputScan{
		typed: func(pkgs []*packages.Package) []fieldReference {
			return fieldReferences(pkgs, pkgPath, typeName, fieldName)
		},
		syntactic: func(path string, fset *token.FileSet, f *ast.File) []fieldReference {
			return syntacticFieldReferences(path, fset, f, fieldName)
		},
	}
}

// functionScan is the scan of the package function name of the package at
// declPath.
func functionScan(declPath, name string) outputScan {
	return outputScan{
		typed: func(pkgs []*packages.Package) []fieldReference {
			return objectUses(pkgs, isFunctionOf(declPath, name))
		},
		syntactic: func(path string, fset *token.FileSet, f *ast.File) []fieldReference {
			return syntacticIdentifierUses(path, fset, f, name)
		},
	}
}

// stdoutScan is the scan of os.Stdout and fmt.Print, Printf and Println.
var stdoutScan = outputScan{typed: stdoutUses, syntactic: syntacticStdoutUses}

// syntacticFieldReferences returns every selector with the name fieldName
// and every key of that name in a composite literal in f, by enclosing
// function. Without types it also counts a package-qualified name such as
// io.Writer; such a match is listed with its reason.
func syntacticFieldReferences(pkgPath string, fset *token.FileSet, f *ast.File, fieldName string) []fieldReference {
	pkg := &packages.Package{PkgPath: pkgPath}
	var out []fieldReference
	for _, decl := range f.Decls {
		function := declarationFunction(pkg, decl)
		writes := map[ast.Expr]bool{}
		var stack []ast.Node
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					writes[ast.Unparen(lhs)] = true
				}
			case *ast.IncDecStmt:
				writes[ast.Unparen(n.X)] = true
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					writes[ast.Unparen(n.X)] = true
				}
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == fieldName {
					out = append(out, fieldReference{function: function, write: true, pos: fset.Position(id.Pos()).String(), callee: untypedNoCall})
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == fieldName {
					out = append(out, fieldReference{function: function, write: writes[n], pos: fset.Position(n.Sel.Pos()).String(), callee: untypedCallee(stack)})
				}
			}
			return true
		})
	}
	return out
}

// syntacticIdentifierUses returns every identifier named name in f outside the
// declaration of a function of that name, by enclosing function.
func syntacticIdentifierUses(pkgPath string, fset *token.FileSet, f *ast.File, name string) []fieldReference {
	pkg := &packages.Package{PkgPath: pkgPath}
	var out []fieldReference
	for _, decl := range f.Decls {
		function := declarationFunction(pkg, decl)
		var declared *ast.Ident
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
			declared = fd.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name && id != declared {
				out = append(out, fieldReference{function: function, pos: fset.Position(id.Pos()).String()})
			}
			return true
		})
	}
	return out
}

// syntacticStdoutUses returns every selector os.Stdout and fmt.Print,
// fmt.Printf and fmt.Println in f, where os and fmt are the names the file
// gives the two packages, by enclosing function. A dot import of os or fmt is
// not followed: a file only another platform builds that uses one is not seen.
func syntacticStdoutUses(pkgPath string, fset *token.FileSet, f *ast.File) []fieldReference {
	imported := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || (p != "os" && p != "fmt") {
			continue
		}
		name := p
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imported[name] = p
	}
	pkg := &packages.Package{PkgPath: pkgPath}
	var out []fieldReference
	for _, decl := range f.Decls {
		function := declarationFunction(pkg, decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch p := imported[id.Name]; {
			case p == "os" && sel.Sel.Name == "Stdout", p == "fmt" && slices.Contains([]string{"Print", "Printf", "Println"}, sel.Sel.Name):
				out = append(out, fieldReference{function: function, pos: fset.Position(sel.Sel.Pos()).String()})
			}
			return true
		})
	}
	return out
}

// references runs scan on the host's packages and on the files only another
// platform builds, and returns each reference once.
func (sp systemPackages) references(scan outputScan) []fieldReference {
	seen := map[string]bool{}
	var out []fieldReference
	add := func(refs []fieldReference) {
		for _, r := range refs {
			if !seen[r.pos] {
				seen[r.pos] = true
				out = append(out, r)
			}
		}
	}
	add(scan.typed(sp.typed))
	for _, platform := range slices.Sorted(maps.Keys(sp.extras)) {
		for _, e := range sp.extras[platform] {
			add(scan.syntactic(e.pkgPath, e.fset, e.file))
		}
	}
	return out
}

// extraReferences is what scan finds in the files only one platform builds.
func (sp systemPackages) extraReferences(platform string, scan outputScan) []fieldReference {
	var out []fieldReference
	for _, e := range sp.extras[platform] {
		out = append(out, scan.syntactic(e.pkgPath, e.fset, e.file)...)
	}
	return out
}

// only is the packages and files of sp whose package is named by the last
// element of its path.
func (sp systemPackages) only(names ...string) systemPackages {
	out := systemPackages{platforms: sp.platforms, extras: map[string][]extraFile{}}
	for _, pkg := range sp.typed {
		if slices.Contains(names, path.Base(pkg.PkgPath)) {
			out.typed = append(out.typed, pkg)
		}
	}
	for platform, files := range sp.extras {
		for _, e := range files {
			if slices.Contains(names, path.Base(e.pkgPath)) {
				out.extras[platform] = append(out.extras[platform], e)
			}
		}
	}
	return out
}

// outputWritePackages reads the module once for the whole test binary.
var outputWritePackages = sync.OnceValues(func() (systemPackages, error) {
	return loadSystemPackages("./...")
})

// assertReferencesListed asserts the references counted by function are the
// listed ones, one reason for each occurrence: the counts are the lengths of
// the lists, a function with no list fails, and so does a list entry that
// matches nothing.
func assertReferencesListed(t assert.TestingT, what string, list map[string][]string, got map[string]int) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	want := map[string]int{}
	for function, reasons := range list {
		want[function] = len(reasons)
		for _, reason := range reasons {
			assert.NotEmpty(t, reason, "%s: an occurrence in %s has no reason", what, function)
		}
		assert.Contains(t, got, function, "%s: the list entry for %s matches nothing; remove it", what, function)
	}
	assert.Equal(t, want, got, what)
}

// Every reason of the lists below starts with its class, followed by a colon:
//   - "filtered:" the text goes through a filter that removes control and
//     format characters before the write; the reason ends with the rows of
//     TestServerTextCannotDriveTheTerminal that plant server text in the
//     channel, as "(rows: <name>, <name>)";
//   - "raw:" the bytes are the data, or the output of another program, and are
//     written as they are; the reason says why and may end with rows;
//   - "no text:" the reference writes no text of its own: a terminal check, the
//     assignment of a writer, the swap to a buffer, the final write of text a
//     listed reference rendered, the line end of a cancelled read.
//
// One reference has one reason. A reference whose formats differ (the
// envelope writer, which filters table, wide and name and writes json, yaml
// and template as they are) names both classes in its one reason: the reason
// starts with the class the check reads, "filtered:", gives the "raw:" part
// after it, and ends with the rows of both parts.
//
// TestEveryListedWriterNamesItsTerminalTextRows checks the classes and rows of
// every list; it reads the class at the start of a reason only.

// ioOutReferences is every reference to the field Out of runtime.IO in a
// non-test file, one reason for each occurrence. Every text channel the
// pipeline writes goes through one of them, so a new reference is a new
// channel that is listed here with the expansion it uses.
var ioOutReferences = map[string][]string{
	"internal/runtime.renderResult": {
		"filtered: the text print, through output.PrintableLines and expanded for the invocation's directory; a PlainText command's text is raw: the output of a program in a container (rows: text, styled text, styled text with server text, styled text adds conceal, styled text adds clear screen, wide text, once-only line, logs text)",
		"raw: the bytes of a file or script, written as they are (rows: raw bytes)",
		"raw: the exec credential encoder, which writes the plugin's one JSON object",
		"filtered: the summary line over a table, one line, expanded for the invocation's directory (rows: summary line)",
		"filtered: the envelope writer, which expands json and yaml data for the invocation and removes this process's marker from the other formats; the table, wide and name renderers filter each header and cell; raw: json, yaml and template, whose bytes are the data (rows: table, wide, name, json, yaml, template)",
	},
	"internal/runtime.renderEffect": {
		"filtered: the dry-run preview text, through output.PrintableLines and expanded for the invocation's directory (rows: preview)",
		"raw: the envelope writer, which expands json and yaml data for the invocation and removes this process's marker from the other formats; an Effect is not a table, and its name and template output is the data",
	},
	"internal/runtime.finishOnceOnlyResult": {
		"no text: the swap to a buffer, so the render is discarded when the final write fails",
		"no text: the final write of the buffered output, which renderResult rendered",
	},
	"internal/cli.buildIO": {
		"no text: the writer of the root command becomes the invocation's output",
	},
}

// ioErrReferences is every reference to the field Err of runtime.IO in a
// non-test file, keyed "<function> -> <what holds the reference>" (see
// referenceCallee), one reason for each occurrence.
var ioErrReferences = map[string][]string{
	"internal/runtime.runRead -> internal/ui.New": {
		"filtered: the prompter, which filters every notice and prompt label (rows: notice, prompt label, secret prompt label)",
	},
	"internal/runtime.renderResult -> internal/runtime.mirrorWarnings": {
		"filtered: the warnings of a text result (rows: warnings text)",
		"filtered: the warnings of a result that writes bytes (rows: warnings raw bytes)",
		"filtered: the warnings of an exec credential result (rows: warnings exec credential)",
		"filtered: the warnings of a result whose envelope is not json or yaml (rows: warnings envelope)",
	},
	"internal/runtime.renderEffect -> internal/runtime.mirrorWarnings": {
		"filtered: the warnings of a dry-run preview text (rows: warnings preview text)",
		"filtered: the warnings of a dry-run preview envelope that is not json or yaml (rows: warnings preview envelope)",
	},
	"internal/runtime.renderErrDrained -> internal/runtime.WriteError": {
		"filtered: the error block and the warnings of a failed run (rows: warnings failed run, error block)",
	},
	"internal/cli.buildIO -> literal runtime.IO.Err": {
		"no text: the writer of the root command becomes the invocation's error output",
	},
}

// TestEveryReferenceToIOOutIsListed asserts every reference to the field Out
// of runtime.IO in a non-test file (a selector, also one promoted through an
// embedded IO, and a key of a composite literal) is listed with a reason, by
// enclosing function. Every file counts whatever its build constraint on the
// system or the architecture: the host's packages are type-checked, and each
// GOOS and GOARCH pair the release workflow builds, read from the workflow,
// adds the files only it builds, read without types, where every selector
// named Out counts. The field is resolved as the field of IO's struct in the
// type-checked files, so a field named Out of another type is not counted
// there. It does not follow a write through a value that holds a copy of the
// writer (the reference where the copy is made is counted, the writes through
// the copy are not), a file behind a build tag other than the system and the
// architecture, or the writes urfave makes through its own Writer field inside
// the library. The writes to the process's standard output
// are listed by TestEveryProcessStdoutReferenceIsListed.
func TestEveryReferenceToIOOutIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(fieldScan(runtimePath, "IO", "Out")))
	assertReferencesListed(t, "the field Out of runtime.IO", ioOutReferences, got)
}

// rootWriterReferences is every reference to the field Writer of urfave's
// Command in a non-test file, one reason for each occurrence. None writes a
// remedy command.
var rootWriterReferences = map[string][]string{
	"internal/cli.buildIO": {
		"no text: the writer becomes the invocation's output, which the pipeline expands",
		"no text: the terminal check of the output",
	},
	"internal/cli.universalShellCompleteFunc": {
		"raw: completion candidates, which pass their own filter and are not a remedy channel",
		"raw: completion candidates for a shell the root does not know, which pass their own filter and are not a remedy channel",
	},
	"internal/cli.completeCurrentWord": {
		"raw: completion candidates, which pass their own filter and are not a remedy channel",
	},
	"internal/cli.showHelpCard": {
		"raw: urfave's help text, which holds only text the CLI wrote as literals and no remedy command",
	},
	"internal/cli.helpStyle": {
		"no text: the terminal check that decides the help's styling",
	},
	"internal/cli.printVersion": {
		"raw: the version line, which holds the command name and the version and no remedy command",
	},
	"internal/cli.recordRun": {
		"no text: the terminal check of a recorded run",
	},
}

// prompterErrReferences is every reference to the unexported field err of
// ui.Prompter in a non-test file, one reason for each occurrence, by
// function.
var prompterErrReferences = map[string][]string{
	"internal/ui.New": {
		"no text: the writer a prompter is built with",
	},
	"internal/ui.Discard": {
		"no text: the writer of the prompter that drops what it is given",
	},
	"internal/ui.Prompter.Notice": {
		"filtered: every notice, after the rewrite (rows: notice)",
	},
	"internal/ui.Prompter.writeErr": {
		"filtered: every prompt label (rows: prompt label, secret prompt label)",
	},
	"internal/ui.Prompter.readLine": {
		"no text: the line end after a cancelled read",
	},
}

// urfaveErrWriterReferences is every reference to the field ErrWriter of
// urfave's Command in a non-test file, one reason for each occurrence, by
// function.
var urfaveErrWriterReferences = map[string][]string{
	"internal/cli.buildIO": {
		"no text: the writer becomes the invocation's error output",
		"no text: the terminal check of the error output",
	},
	"internal/cli.Adapt": {
		"filtered: the error block of a refused flag value, written by WriteError (rows: error block)",
	},
	"internal/cli.renderDepsError": {
		"filtered: the error block of a failure to open the directory, written by WriteError (rows: error block)",
	},
	"internal/cli.renderRawArgvError": {
		"filtered: the error block of a command line that did not parse, written by WriteError (rows: error block)",
	},
	"internal/cli.adaptEntry": {
		"filtered: the error block of a refused flag value, written by WriteError (rows: error block)",
		"filtered: the error block of an empty --config value, written by WriteError (rows: error block)",
	},
	"internal/cli.errStyle": {
		"no text: the terminal check that decides the error block's styling",
	},
	"internal/cli.uploadStoredEvents": {
		"no text: the writer becomes the upload's debug output",
	},
	"internal/cli.commandUpload": {
		"no text: the writer becomes the upload command's debug output",
	},
	"internal/cli.runArgv": {
		"no text: the nil check before the root's writer is set",
		"no text: the assignment of the default error writer",
		"filtered: the error block of a refused command line, written by WriteError (rows: error block)",
	},
}

// TestEveryReferenceToTheRootWriterIsListed asserts every reference to the
// field Writer of the Command type of github.com/urfave/cli/v3 in a non-test
// file (a selector, read or assigned, and a key of a composite literal) is
// listed with a reason, by enclosing function; a function literal counts in the
// function that declares it. No remedy command is written through any listed
// reference: a later reference that writes one is a new text channel and is
// listed with its expansion. A field named Writer of another type is not
// counted in the type-checked files. It reads every file whatever its build
// constraint on the system or the architecture (the files only another release
// platform builds without types, where every selector named Writer counts),
// follows the field of that type by those forms and does not follow the writes
// urfave makes through the field inside the library.
func TestEveryReferenceToTheRootWriterIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(fieldScan(urfaveCLIPath, "Command", "Writer")))
	assertReferencesListed(t, "the field Writer of urfave's Command", rootWriterReferences, got)
}

// TestEveryReferenceToIOErrIsListed asserts every reference to the field Err
// of runtime.IO in a non-test file is listed with a reason, by enclosing
// function and by what holds the reference: the function it is passed to,
// the method it is the receiver of, a literal key, or no call. Swapping the
// call a reference is passed to fails even when the count stays. It follows
// a selector (read or written, also one promoted through an embedded
// struct) and a key of a composite literal of the type, resolves the callee
// through go/types, and reads every file whatever its build constraint on the
// system or the architecture (the files only another release platform builds
// without types: the callee is "untyped" and the call's function as written).
// It does not follow a write through a value that holds a copy of the writer
// (the reference where the copy is made is counted as "no call"), os.NewFile,
// the print built-ins, a file behind another build tag, or what urfave writes
// through its own writer field inside the library.
func TestEveryReferenceToIOErrIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := referencesByCallee(sets.references(fieldScan(runtimePath, "IO", "Err")))
	assertReferencesListed(t, "the field Err of runtime.IO", ioErrReferences, got)
}

// TestEveryReferenceToThePrompterErrorWriterIsListed asserts every reference
// to the unexported field err of ui.Prompter in a non-test file (a selector,
// read or assigned, and a key of a composite literal) is listed with a
// reason, by enclosing function. A field named err of another type is not
// counted in the type-checked files. It does not follow a copy of the writer,
// os.NewFile, the print built-ins or a file behind another build tag.
func TestEveryReferenceToThePrompterErrorWriterIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(fieldScan(uiPath, "Prompter", "err")))
	assertReferencesListed(t, "the field err of ui.Prompter", prompterErrReferences, got)
}

// TestEveryReferenceToTheRootErrorWriterIsListed asserts every reference to
// the field ErrWriter of the Command type of github.com/urfave/cli/v3 in a
// non-test file (a selector, read or assigned, and a key of a composite
// literal) is listed with a reason, by enclosing function; a function literal
// counts in the function that declares it. It reads every file whatever its
// build constraint on the system or the architecture (the files only another
// release platform builds without types, where every selector named ErrWriter
// counts) and does not follow what urfave writes through the field inside
// the library.
func TestEveryReferenceToTheRootErrorWriterIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(fieldScan(urfaveCLIPath, "Command", "ErrWriter")))
	assertReferencesListed(t, "the field ErrWriter of urfave's Command", urfaveErrWriterReferences, got)
}

// rawBytesWriters are the functions that write the field RawBytes of
// runtime.Result, one reason for each. The pipeline writes the bytes as they
// are, so a command that returns its text this way bypasses the writer filter.
var rawBytesWriters = map[string][]string{
	"internal/cli.completionCommand.Execute": {
		"the shell completion script, a file for the shell",
	},
	"internal/engine/extres.createKubeconfigCmd.Plan": {
		"the kubeconfig file the command creates, a file for kubectl",
	},
	"internal/engine/kubeconfig.kubeconfigCmd.Execute": {
		"the kubeconfig file the command writes, a file for kubectl",
	},
}

// rawWriters are the functions that write the field Raw of runtime.Result,
// one reason for each. The pipeline encodes the result's data as the one JSON
// object kubectl reads from the exec plugin.
var rawWriters = map[string][]string{
	"internal/token.tokenCmd.runWithParams": {
		"the exec credential the plugin prints after it obtained one",
	},
	"internal/token.tokenCmd.cachedResultOnce": {
		"the exec credential the plugin prints from its cache",
	},
	"internal/token.probeCacheWith": {
		"the exec credential the plugin prints from a cache probe",
	},
}

// TestRawPayloadsAreWrittenInListedFunctionsOnly asserts the fields RawBytes
// and Raw of runtime.Result are written (a key of a composite literal, an
// assignment or the operand of &) only in the listed functions: a command
// whose text reaches the terminal as a raw payload bypasses the writer filter,
// so a new writer is listed with its reason first.
func TestRawPayloadsAreWrittenInListedFunctionsOnly(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	assertReferencesListed(t, "writes of Result.RawBytes", rawBytesWriters,
		referencesByFunction(sets.references(fieldScan(runtimePath, "Result", "RawBytes")), true))
	assertReferencesListed(t, "writes of Result.Raw", rawWriters,
		referencesByFunction(sets.references(fieldScan(runtimePath, "Result", "Raw")), true))
}

// terminalClasses are the classes a reason of the writer lists starts with.
var terminalClasses = []string{"filtered:", "raw:", "no text:"}

// terminalRowsSuffix finds the rows a reason ends with: "(rows: a, b)".
var terminalRowsSuffix = regexp.MustCompile(`\(rows: ([^)]+)\)\s*$`)

// checkWriterReasons asserts every reason of list starts with exactly one of
// the classes, a filtered reason names at least one row, a no text reason
// names none, and every row a reason names is in rows; it adds the rows to
// named.
func checkWriterReasons(t assert.TestingT, what string, list map[string][]string, rows, named map[string]bool) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	for function, reasons := range list {
		for _, reason := range reasons {
			var class string
			classes := 0
			for _, c := range terminalClasses {
				if strings.HasPrefix(reason, c) {
					class = c
					classes++
				}
			}
			if !assert.Equal(t, 1, classes, "%s: the reason of %s starts with exactly one of %v: %q", what, function, terminalClasses, reason) {
				continue
			}
			var names []string
			if m := terminalRowsSuffix.FindStringSubmatch(reason); m != nil {
				for name := range strings.SplitSeq(m[1], ",") {
					names = append(names, strings.TrimSpace(name))
				}
			}
			switch class {
			case "filtered:":
				assert.NotEmpty(t, names, "%s: the filtered reason of %s names the rows that plant text in it: %q", what, function, reason)
			case "no text:":
				assert.Empty(t, names, "%s: the no text reason of %s names no row: %q", what, function, reason)
			}
			for _, name := range names {
				assert.True(t, rows[name], "%s: %s names the row %q, which is not a row", what, function, name)
				named[name] = true
			}
		}
	}
}

// TestEveryListedWriterNamesItsTerminalTextRows asserts the reasons of the
// five writer lists carry a class and the rows of
// TestServerTextCannotDriveTheTerminal that exercise them, and that every
// row is named by at least one reason: a listed writer with no row, and a row
// for a writer that is gone, both fail.
func TestEveryListedWriterNamesItsTerminalTextRows(t *testing.T) {
	t.Parallel()
	rows := map[string]bool{}
	for _, name := range terminalRowNames() {
		assert.False(t, rows[name], "the row %q is listed twice", name)
		rows[name] = true
	}
	named := map[string]bool{}
	for what, list := range map[string]map[string][]string{
		"ioOutReferences":           ioOutReferences,
		"rootWriterReferences":      rootWriterReferences,
		"ioErrReferences":           ioErrReferences,
		"prompterErrReferences":     prompterErrReferences,
		"urfaveErrWriterReferences": urfaveErrWriterReferences,
	} {
		checkWriterReasons(t, what, list, rows, named)
	}
	assert.Equal(t, slices.Sorted(maps.Keys(rows)), slices.Sorted(maps.Keys(named)), "the rows the reasons name and the rows of the table")
}

// TestWriterReasonCheckFailsForABadReason asserts the check fails for a
// reason with no class, a class in another case, a filtered reason with no
// rows, a no text reason with rows and a row that does not exist, and passes
// for a good list.
func TestWriterReasonCheckFailsForABadReason(t *testing.T) {
	t.Parallel()
	rows := map[string]bool{"a": true, "b": true}
	var ok failureRecorder
	named := map[string]bool{}
	checkWriterReasons(&ok, "x", map[string][]string{"f": {"filtered: one (rows: a, b)", "raw: two", "raw: three (rows: a)", "no text: four"}}, rows, named)
	assert.Empty(t, ok.failures)
	assert.Equal(t, map[string]bool{"a": true, "b": true}, named)
	for name, reason := range map[string]string{
		"no class":                 "the text print",
		"a class in another case":  "Filtered: one (rows: a)",
		"a filtered reason no row": "filtered: one",
		"a no text reason, a row":  "no text: one (rows: a)",
		"a row that is not one":    "filtered: one (rows: missing)",
	} {
		var rec failureRecorder
		checkWriterReasons(&rec, "x", map[string][]string{"f": {reason}}, rows, map[string]bool{})
		assert.NotEmpty(t, rec.failures, name)
	}
}

// plainRemedyUses is every reference to the package function plainRemedies
// outside its definition, one reason for each occurrence.
var plainRemedyUses = map[string][]string{
	"internal/runtime.RenderErrorStyled": {
		"the error of a caller with no invocation, which has no directory to name",
		"the warnings of the error of a caller with no invocation",
	},
	"internal/runtime.renderResult": {
		"the text of a command whose descriptor sets PlainText",
	},
}

// TestEveryPlainRemedyUseIsListed asserts every reference to the package
// function plainRemedies of internal/runtime outside its definition, a call or
// a function value, is listed with a reason, by enclosing function, in every
// file whatever its build constraint on the system or the architecture.
func TestEveryPlainRemedyUseIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(functionScan(runtimePath, "plainRemedies")))
	assertReferencesListed(t, "plainRemedies", plainRemedyUses, got)
}

// renderWithUses is every reference to the package function RenderWith of
// internal/output outside its definition, one reason for each occurrence.
var renderWithUses = map[string][]string{
	"internal/runtime.renderResult": {
		"the result envelope, whose json and yaml data this invocation expands",
	},
	"internal/runtime.renderEffect": {
		"the dry-run envelope, whose json and yaml data this invocation expands",
	},
	"internal/output.RenderStyled": {
		"no rewrite: the styled form for a caller outside the result path",
	},
}

// renderStyledUses is every reference to the package function RenderStyled of
// internal/output outside its definition, one reason for each occurrence.
var renderStyledUses = map[string][]string{
	"internal/output.Render": {
		"the form with no style",
	},
}

// renderUses is every reference to the package function Render of
// internal/output outside its definition, one reason for each occurrence.
var renderUses = map[string][]string{
	"internal/runtime.RenderErrorStyled": {
		"the error envelope, whose remedy commands ExpandError has written",
	},
}

// TestEveryRenderCallIsListed asserts every reference to the package
// functions RenderWith, RenderStyled and Render of internal/output outside
// their definitions, a call or a function value, is listed with a reason, by
// enclosing function: a render path that writes an envelope without the
// invocation's expansion of json and yaml data is a new entry here. It reads
// every file whatever its build constraint on the system or the architecture,
// package output included, and resolves each function by go/types. It does
// not follow an encoder a command builds itself:
// TestEveryReferenceToIOOutIsListed lists every writer of the output stream.
func TestEveryRenderCallIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	for name, list := range map[string]map[string][]string{
		"RenderWith":   renderWithUses,
		"RenderStyled": renderStyledUses,
		"Render":       renderUses,
	} {
		got := allReferencesByFunction(sets.references(functionScan(outputPath, name)))
		assertReferencesListed(t, name, list, got)
	}
}

// TestPlainTextIsWrittenAndReadInOneFunctionEach asserts the field PlainText
// of runtime.Descriptor is written (a key of a composite literal, an
// assignment or the operand of &) in exactly the descriptor of the logs
// command and read in exactly renderResult, so a copy of a descriptor that
// gains the field after its command returned it is found.
func TestPlainTextIsWrittenAndReadInOneFunctionEach(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	refs := sets.references(fieldScan(runtimePath, "Descriptor", "PlainText"))
	assertReferencesListed(t, "writes of PlainText", map[string][]string{
		"internal/engine/observe.logsCmd.Describe": {"the text of logs is the output of a program in a container"},
	}, referencesByFunction(refs, true))
	assertReferencesListed(t, "reads of PlainText", map[string][]string{
		"internal/runtime.renderResult": {"chooses the expansion of the text print"},
	}, referencesByFunction(refs, false))
}

// stdoutReferences is every reference in a non-test file to os.Stdout and to
// fmt.Print, fmt.Printf and fmt.Println, which write to the process's standard
// output without the invocation's writer, one reason for each occurrence. Every
// one is in a program that has no invocation or writes no remedy command.
var stdoutReferences = map[string][]string{
	"credhelper.Serve": {
		"the credential helper protocol answer of a program the CLI runs as a subprocess (rancher-credential-<name>)",
	},
	"cmd/rancher-credential-vault.main": {
		"the credential helper protocol answer of a program the CLI runs as a subprocess (rancher-credential-<name>)",
	},
	"internal/commands/configcmd.execEditor": {
		"the standard output of the editor the user runs, which is the process's own",
	},
}

// TestEveryProcessStdoutReferenceIsListed asserts every reference to the
// variable Stdout of os and to the functions Print, Printf and Println of fmt
// in a non-test file, in every file whatever its build constraint on the
// system or the architecture, is listed with a reason, by enclosing function. A write
// that reaches standard output by another route (the print and println
// built-ins, os.NewFile on file descriptor 1) is not followed.
func TestEveryProcessStdoutReferenceIsListed(t *testing.T) {
	t.Parallel()
	sets, err := outputWritePackages()
	require.NoError(t, err)
	got := allReferencesByFunction(sets.references(stdoutScan))
	assertReferencesListed(t, "os.Stdout and fmt.Print", stdoutReferences, got)
}

// stdoutUses returns every reference in pkgs to os.Stdout and to fmt.Print,
// fmt.Printf and fmt.Println.
func stdoutUses(pkgs []*packages.Package) []fieldReference {
	printers := isFunctionOf("fmt", "Print", "Printf", "Println")
	return objectUses(pkgs, func(obj types.Object) bool {
		if v, ok := obj.(*types.Var); ok {
			return v.Pkg() != nil && v.Pkg().Path() == "os" && v.Name() == "Stdout" && !v.IsField()
		}
		return printers(obj)
	})
}

// outputWriteFixtures reads the fixture packages once, as the module is read.
var outputWriteFixtures = sync.OnceValues(func() (systemPackages, error) {
	return loadSystemPackages("./" + outputWritesDir + "/...")
})

// outputWriteFixtureNames are the fixture packages on disk.
var outputWriteFixtureNames = []string{"clean", "errwriter", "ioerr", "ioout", "plainother", "plainuse", "plaintext", "prompterr", "rawpayload", "rootwriter", "stdout"}

// TestOutputWriteScanFixtures asserts each scan on the fixtures, among them the
// references to the field Err of runtime.IO with what holds each one (an
// argument of a declared function, of fmt.Fprint, of a method and of a
// function value; the receiver of a method call; a literal key; no call for
// an assignment, a return, a comparison, a method value, and a reference
// inside a function literal or a composite literal that is an argument; a
// conversion; a built-in; and, in a file only windows builds, an untyped
// call and no call), the err of a Prompter, the ErrWriter of urfave's Command
// and the writes of the fields RawBytes and Raw of runtime.Result, each with a
// field of another type left out. The host type-checks the fixtures and the
// other platforms the release builds contribute only the files they alone
// build: the references to the field Out of runtime.IO as
// a selector, a promoted selector, an assignment, a key of a composite
// literal, one in a function literal, one at package level, one in a file only
// windows builds and one in a file only s390x builds, with a field Out of
// another type and a clean file left out; the same forms for the Writer of
// urfave's Command with a Writer of another type left out; the calls and
// function values of a function named plainRemedies, with one of another
// package left out; the writes and reads of PlainText, with a field of another
// type left out; and the uses of os.Stdout and fmt.Print, Printf and Println,
// with fmt.Sprintf, fmt.Fprintln and os.Stderr left out.
func TestOutputWriteScanFixtures(t *testing.T) {
	t.Parallel()
	sets, err := outputWriteFixtures()
	require.NoError(t, err)
	var names []string
	for _, pkg := range sets.typed {
		names = append(names, path.Base(pkg.PkgPath))
	}
	require.ElementsMatch(t, outputWriteFixtureNames, names)
	key := func(pkg, function string) string { return outputWritesDir + "/" + pkg + "." + function }
	pathOf := func(name string) string {
		for _, pkg := range sets.typed {
			if path.Base(pkg.PkgPath) == name {
				return pkg.PkgPath
			}
		}
		return ""
	}

	t.Run("IO.Out", func(t *testing.T) {
		t.Parallel()
		both := sets.only("ioout", "clean")
		scan := fieldScan(runtimePath, "IO", "Out")
		assert.Equal(t, map[string]int{
			key("ioout", "selector"):        1,
			key("ioout", "promoted"):        1,
			key("ioout", "literalKey"):      1,
			key("ioout", "assigned"):        1,
			key("ioout", "closure"):         1,
			key("ioout", "<package level>"): 1,
			key("ioout", "windowsWrite"):    1,
			key("ioout", "s390xWrite"):      1,
		}, allReferencesByFunction(both.references(scan)))
		onWindows := allReferencesByFunction(append(scan.typed(both.typed), both.extraReferences("windows/amd64", scan)...))
		assert.Contains(t, onWindows, key("ioout", "windowsWrite"))
		onS390x := allReferencesByFunction(append(scan.typed(both.typed), both.extraReferences("linux/s390x", scan)...))
		assert.Contains(t, onS390x, key("ioout", "s390xWrite"))
		onLinux := allReferencesByFunction(both.extraReferences("linux/amd64", scan))
		assert.NotContains(t, onLinux, key("ioout", "windowsWrite"), "a file only windows builds is not read for linux")
		assert.NotContains(t, onLinux, key("ioout", "s390xWrite"), "a file only s390x builds is not read for linux/amd64")
	})

	t.Run("urfave Command.Writer", func(t *testing.T) {
		t.Parallel()
		both := sets.only("rootwriter", "clean")
		assert.Equal(t, map[string]int{
			key("rootwriter", "read"):       1,
			key("rootwriter", "assign"):     1,
			key("rootwriter", "literalKey"): 1,
			key("rootwriter", "root"):       1,
		}, allReferencesByFunction(both.references(fieldScan(urfaveCLIPath, "Command", "Writer"))))
	})

	t.Run("IO.Err", func(t *testing.T) {
		t.Parallel()
		both := sets.only("ioerr", "clean")
		const (
			consume = outputWritesDir + "/ioerr.consume"
			write   = "receiver of io.Writer.Write"
		)
		assert.Equal(t, map[string]int{
			key("ioerr", "passedToFunction") + " -> " + consume:                             1,
			key("ioerr", "passedToFmt") + " -> fmt.Fprint":                                  1,
			key("ioerr", "passedToMethod") + " -> " + outputWritesDir + "/ioerr.sink.write": 1,
			key("ioerr", "passedToClosure") + " -> func value f":                            1,
			key("ioerr", "receiver") + " -> " + write:                                       1,
			key("ioerr", "literalKey") + " -> literal runtime.IO.Err":                       1,
			key("ioerr", "assigned") + " -> no call":                                        1,
			key("ioerr", "returned") + " -> no call":                                        1,
			key("ioerr", "methodValue") + " -> no call":                                     1,
			key("ioerr", "inClosure") + " -> " + write:                                      1,
			key("ioerr", "compared") + " -> no call":                                        1,
			key("ioerr", "inFuncLitArgument") + " -> no call":                               1,
			key("ioerr", "inCompositeArgument") + " -> no call":                             1,
			key("ioerr", "converted") + " -> conversion io.Writer":                          1,
			key("ioerr", "appended") + " -> builtin append":                                 1,
			key("ioerr", "methodValueArgument") + " -> no call":                             1,
			key("ioerr", "windowsWrite") + " -> untyped fmt.Fprint":                         1,
			key("ioerr", "windowsAssigned") + " -> untyped no call":                         1,
		}, referencesByCallee(both.references(fieldScan(runtimePath, "IO", "Err"))))
	})

	t.Run("Prompter.err", func(t *testing.T) {
		t.Parallel()
		both := sets.only("prompterr", "clean")
		assert.Equal(t, map[string]int{
			key("prompterr", "Prompter.Notice"): 1,
			key("prompterr", "New"):             1,
			key("prompterr", "assign"):          1,
		}, allReferencesByFunction(both.references(fieldScan(pathOf("prompterr"), "Prompter", "err"))))
	})

	t.Run("urfave Command.ErrWriter", func(t *testing.T) {
		t.Parallel()
		both := sets.only("errwriter", "clean")
		assert.Equal(t, map[string]int{
			key("errwriter", "read"):       1,
			key("errwriter", "assign"):     1,
			key("errwriter", "literalKey"): 1,
			key("errwriter", "root"):       1,
		}, allReferencesByFunction(both.references(fieldScan(urfaveCLIPath, "Command", "ErrWriter"))))
	})

	t.Run("Result.RawBytes and Result.Raw", func(t *testing.T) {
		t.Parallel()
		both := sets.only("rawpayload", "clean")
		assert.Equal(t, map[string]int{
			key("rawpayload", "bytesLiteralKey"): 1,
			key("rawpayload", "bytesAssign"):     1,
			key("rawpayload", "bytesAddress"):    1,
		}, referencesByFunction(both.references(fieldScan(runtimePath, "Result", "RawBytes")), true))
		assert.Equal(t, map[string]int{
			key("rawpayload", "rawLiteralKey"): 1,
			key("rawpayload", "rawAssign"):     1,
		}, referencesByFunction(both.references(fieldScan(runtimePath, "Result", "Raw")), true))
		assert.Equal(t, map[string]int{key("rawpayload", "read"): 1}, referencesByFunction(both.references(fieldScan(runtimePath, "Result", "Raw")), false),
			"a read is not a write; a field Raw of another type is not counted")
	})

	t.Run("plainRemedies", func(t *testing.T) {
		t.Parallel()
		both := sets.only("plainuse", "plainother", "clean")
		assert.Equal(t, map[string]int{
			key("plainuse", "call"):  1,
			key("plainuse", "value"): 1,
		}, allReferencesByFunction(both.references(functionScan(pathOf("plainuse"), "plainRemedies"))))
		assert.Equal(t, map[string]int{key("plainother", "call"): 1}, allReferencesByFunction(both.references(functionScan(pathOf("plainother"), "plainRemedies"))))
	})

	t.Run("Descriptor.PlainText", func(t *testing.T) {
		t.Parallel()
		refs := sets.only("plaintext", "clean").references(fieldScan(runtimePath, "Descriptor", "PlainText"))
		assert.Equal(t, map[string]int{
			key("plaintext", "literalKey"):   1,
			key("plaintext", "assign"):       1,
			key("plaintext", "addressTaken"): 1,
		}, referencesByFunction(refs, true))
		assert.Equal(t, map[string]int{key("plaintext", "read"): 1}, referencesByFunction(refs, false))
	})

	t.Run("standard output", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, map[string]int{
			key("stdout", "variable"): 1,
			key("stdout", "call"):     1,
			key("stdout", "printf"):   1,
			key("stdout", "value"):    1,
			key("stdout", "toStdout"): 1,
		}, allReferencesByFunction(sets.only("stdout", "clean").references(stdoutScan)))
	})

	t.Run("syntactic reading of a file only another platform builds", func(t *testing.T) {
		t.Parallel()
		var files []extraFile
		for _, platform := range sets.platforms {
			files = append(files, sets.only("ioout").extras[platform]...)
		}
		require.NotEmpty(t, files)
		for _, e := range files {
			assert.NotEmpty(t, fieldScan(runtimePath, "IO", "Out").syntactic(e.pkgPath, e.fset, e.file), "a selector Out in a file only another platform builds is counted")
		}
		const src = `package p

import (
	f "fmt"
	"os"
)

func plainRemedies() { plainRemedies(); _, _ = os.Stdout.Write(nil); f.Println() }
`
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "x.go", src, 0)
		require.NoError(t, err)
		assert.Len(t, syntacticStdoutUses("p", fset, file), 2, "os.Stdout and a fmt import under another name")
		assert.Len(t, syntacticIdentifierUses("p", fset, file, "plainRemedies"), 1, "the declaration is not a use")
	})
}

// failureRecorder is an assert.TestingT that keeps the failures it is given.
type failureRecorder struct{ failures []string }

func (r *failureRecorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestOutputWriteListsAreChecked asserts the comparison of a list with the
// references found fails for a reference the list lacks, for a list entry
// that matches nothing and for a count that differs, and passes for a list
// that matches.
func TestOutputWriteListsAreChecked(t *testing.T) {
	t.Parallel()
	list := map[string][]string{"a.f": {"one", "two"}, "a.g": {"one"}}
	var ok failureRecorder
	assertReferencesListed(&ok, "x", list, map[string]int{"a.f": 2, "a.g": 1})
	assert.Empty(t, ok.failures)
	for name, got := range map[string]map[string]int{
		"a reference the list lacks":    {"a.f": 2, "a.g": 1, "a.h": 1},
		"an entry that matches nothing": {"a.f": 2},
		"a count that differs":          {"a.f": 1, "a.g": 1},
	} {
		var rec failureRecorder
		assertReferencesListed(&rec, "x", list, got)
		assert.NotEmpty(t, rec.failures, name)
	}
}

// TestReleasePlatformsAreTheOnesTheScansLoad asserts the platforms the scans
// read are the pairs of GOOS and GOARCH the release workflow builds: more than
// one, each with both parts, the host type-checked in full and every platform
// read through the files it builds and the host does not.
func TestReleasePlatformsAreTheOnesTheScansLoad(t *testing.T) {
	t.Parallel()
	platforms, err := releasePlatforms()
	require.NoError(t, err)
	require.Greater(t, len(platforms), 1)
	for _, platform := range platforms {
		goos, goarch, ok := strings.Cut(platform, "/")
		assert.True(t, ok && goos != "" && goarch != "", platform)
	}
	sets, err := outputWritePackages()
	require.NoError(t, err)
	assert.Equal(t, platforms, sets.platforms)
	for platform := range sets.extras {
		assert.Contains(t, platforms, platform)
	}
}
