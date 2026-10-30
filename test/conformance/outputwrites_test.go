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
)

// fieldReference is one reference to a struct field: the function it is in
// and whether it writes the field (a composite literal key, the left side of an
// assignment or an increment, or the operand of &).
type fieldReference struct {
	function string
	write    bool
	pos      string
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
				ast.Inspect(decl, func(n ast.Node) bool {
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
									out = append(out, fieldReference{function, true, pkg.Fset.Position(id.Pos()).String()})
								}
							}
						}
					case *ast.SelectorExpr:
						sel := info.Selections[n]
						if sel == nil || sel.Kind() != types.FieldVal || sel.Obj().Name() != fieldName {
							return true
						}
						if owner := fieldOwner(sel); owner != nil && isNamedType(owner, pkgPath, typeName) {
							out = append(out, fieldReference{function, writes[n], pkg.Fset.Position(n.Sel.Pos()).String()})
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
							out = append(out, fieldReference{function, false, pkg.Fset.Position(id.Pos()).String()})
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

// releasePlatformsLine is the loop of the release workflow that builds each
// platform: the pairs of GOOS and GOARCH it lists, as "goos/goarch".
var releasePlatformsLine = regexp.MustCompile(`for platform in ((?:[a-z0-9]+/[a-z0-9]+ ?)+); do`)

// releasePlatforms are the GOOS and GOARCH pairs the release workflow builds,
// read from the workflow, as "goos/goarch".
func releasePlatforms() ([]string, error) {
	src, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		return nil, err
	}
	m := releasePlatformsLine.FindSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("%s: no loop over the platforms to build", releaseWorkflowPath)
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
		ast.Inspect(decl, func(n ast.Node) bool {
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
					out = append(out, fieldReference{function, true, fset.Position(id.Pos()).String()})
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == fieldName {
					out = append(out, fieldReference{function, writes[n], fset.Position(n.Sel.Pos()).String()})
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
				out = append(out, fieldReference{function, false, fset.Position(id.Pos()).String()})
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
				out = append(out, fieldReference{function, false, fset.Position(sel.Sel.Pos()).String()})
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

// ioOutReferences is every reference to the field Out of runtime.IO in a
// non-test file, one reason for each occurrence. Every text channel the
// pipeline writes goes through one of them, so a new reference is a new
// channel that is listed here with the expansion it uses.
var ioOutReferences = map[string][]string{
	"internal/runtime.renderResult": {
		"the text print, expanded for the invocation's directory (or plain for a PlainText command)",
		"the bytes of a file or script, written as they are",
		"the exec credential encoder, which writes the plugin's one JSON object",
		"the summary line over a table, expanded for the invocation's directory",
		"the envelope writer, which removes this process's marker from result data",
	},
	"internal/runtime.renderEffect": {
		"the dry-run preview text, expanded for the invocation's directory",
		"the envelope writer, which removes this process's marker from result data",
	},
	"internal/runtime.finishOnceOnlyResult": {
		"the swap to a buffer, so the render is discarded when the final write fails",
		"the final write of the buffered output",
	},
	"internal/cli.buildIO": {
		"the writer of the root command becomes the invocation's output",
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
		"the writer becomes the invocation's output, which the pipeline expands",
		"the terminal check of the output",
	},
	"internal/cli.universalShellCompleteFunc": {
		"completion candidates, which are not a remedy channel",
		"completion candidates for a shell the root does not know, which are not a remedy channel",
	},
	"internal/cli.completeCurrentWord": {
		"completion candidates, which are not a remedy channel",
	},
	"internal/cli.showHelpCard": {
		"urfave's help text, which holds no remedy command",
	},
	"internal/cli.helpStyle": {
		"the terminal check that decides the help's styling",
	},
	"internal/cli.recordRun": {
		"the terminal check of a recorded run",
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
var outputWriteFixtureNames = []string{"clean", "ioout", "plainother", "plainuse", "plaintext", "rootwriter", "stdout"}

// TestOutputWriteScanFixtures asserts each scan on the fixtures, which the
// host type-checks and the other platforms the release builds contribute only
// the files they alone build: the references to the field Out of runtime.IO as
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
			files = append(files, sets.extras[platform]...)
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
