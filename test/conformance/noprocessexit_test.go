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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Nothing below main ends the process. A command returns an error, Run
// turns it into the exit code and main exits with it, so one place sees how
// every run ended. The scan covers every non-test Go file under internal/,
// credhelper/, skills/ and cmd/rancher, testdata excluded; the other
// binaries under cmd/ are separate programs and out of scope. It matches a
// package by its import path, whatever name the file gives it (an unaliased
// import is read by the package's own name, not its last path element), sees
// through parentheses, and refuses every use, a call or a reference held as
// a value, of:
//   - os.Exit, syscall.Exit and syscall.ExitProcess;
//   - Exit and ExitProcess of golang.org/x/sys/windows, and Exit of
//     golang.org/x/sys/unix (windows.TerminateProcess ends another process
//     and is allowed);
//   - log.Fatal, log.Fatalf, log.Fatalln, log.Panic, log.Panicf and
//     log.Panicln;
//   - the Fatal and Exit functions of k8s.io/klog/v2 (klog has no such
//     methods), its FlushAndExit and its OsExit variable, a call or an
//     assignment;
//   - urfave's OsExiter (a call or an assignment), HandleExitCoder and
//     every Show...AndExit function;
//   - urfave's Exit, unless it is called with an empty string literal as
//     its first argument: such a call returns an error with no message, which
//     Run reads as a declared exit status. A message, a non-literal first
//     argument or a reference to the function that is not a call is refused;
//   - credhelper.Serve, which ends the process;
//   - the Fatal and Panic methods of *log.Logger, whatever expression
//     reaches the logger: a call, a method value, a method expression, an
//     embedded logger, a logger from slog.NewLogLogger. These are found with
//     type information, so the testing.TB methods of the same names pass.
//
// It does not see runtime.Goexit, a signal the process sends itself or a
// call through an interface. The rules that read syntax see every file whatever
// its build tags; the *log.Logger rule sees the packages as they build for
// the host system.
//
// A refused use needs a row in processExitAllowlist: the file and function
// that holds it, the exact number of uses and the reason. A row that
// matches nothing, or a different number of uses, fails the scan.

const (
	urfavePath     = "github.com/urfave/cli/v3"
	credhelperPath = "github.com/rancher/rancher-cli/credhelper"
	windowsPath    = "golang.org/x/sys/windows"
	unixPath       = "golang.org/x/sys/unix"
	klogPath       = "k8s.io/klog/v2"
)

// processExitScope reports whether the scan covers path.
func processExitScope(path string) bool {
	for _, prefix := range []string{"internal/", "credhelper/", "skills/", "cmd/rancher/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// processExitAllowance is the number of refused uses a function holds and
// the reason they are allowed.
type processExitAllowance struct {
	count int
	why   string
}

// processExitAllowlist is keyed by "file:function".
var processExitAllowlist = map[string]processExitAllowance{
	"cmd/rancher/main.go:main":  {3, "the exit when the command tree cannot be built, the os.Exit value InterruptContext calls for the forced exit on a second interrupt, and the exit with the code Run returned"},
	"credhelper/serve.go:Serve": {1, "the credential helper's serve mode ends its process with the status the helper protocol defines"},
}

// processExitUse is one refused use of a process-ending function.
type processExitUse struct {
	key  string
	what string
	pos  string
}

// processExitNames lists, per import path, the names refused whatever the
// arguments are. Urfave's Show...AndExit functions are matched by their
// shape in processExitRefused.
var processExitNames = map[string][]string{
	"os":        {"Exit"},
	"syscall":   {"Exit", "ExitProcess"},
	"log":       {"Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln"},
	windowsPath: {"Exit", "ExitProcess"},
	unixPath:    {"Exit"},
	klogPath: {
		"Fatal", "Fatalf", "Fatalln", "FatalDepth", "FatalfDepth", "FatallnDepth",
		"Exit", "Exitf", "Exitln", "ExitDepth", "ExitfDepth", "ExitlnDepth",
		"FlushAndExit", "OsExit",
	},
	urfavePath:     {"OsExiter", "HandleExitCoder"},
	credhelperPath: {"Serve"},
}

// processExitDefaultNames are the names the watched packages declare, for
// an import without an alias; the last element of the path is the name
// otherwise.
var processExitDefaultNames = map[string]string{
	windowsPath:    "windows",
	unixPath:       "unix",
	klogPath:       "klog",
	urfavePath:     "cli",
	credhelperPath: "credhelper",
}

// processExitRefused reports whether name from the package at importPath is
// refused whatever its arguments are.
func processExitRefused(importPath, name string) bool {
	if slices.Contains(processExitNames[importPath], name) {
		return true
	}
	return importPath == urfavePath && strings.HasPrefix(name, "Show") && strings.HasSuffix(name, "AndExit")
}

// processExitImportName returns the name the file gives the package at
// importPath: the declared name, "." for a dot import, the package's own
// name for an import without an alias, and "" when the file does not import
// it or imports it only as _.
func processExitImportName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		switch {
		case spec.Name == nil:
			if name, ok := processExitDefaultNames[p]; ok {
				return name
			}
			return path.Base(p)
		case spec.Name.Name == "_":
			return ""
		default:
			return spec.Name.Name
		}
	}
	return ""
}

// processExitImports are the import paths the scan watches.
var processExitImports = slices.Sorted(maps.Keys(processExitNames))

// emptyLiteralFirstArg reports whether call's first argument is an empty
// string literal.
func emptyLiteralFirstArg(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == ""
}

// scanProcessExits returns every refused use in f, a file at path.
func scanProcessExits(fset *token.FileSet, path string, f *ast.File) []processExitUse {
	names := map[string]string{}
	var dotImports []string
	for _, p := range processExitImports {
		switch name := processExitImportName(f, p); name {
		case "":
		case ".":
			dotImports = append(dotImports, p)
		default:
			names[name] = p
		}
	}
	if len(names) == 0 && len(dotImports) == 0 {
		return nil
	}
	var uses []processExitUse
	for _, decl := range f.Decls {
		fn := "<package>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		allowedExit := map[ast.Node]bool{}
		add := func(n ast.Node, what string) {
			uses = append(uses, processExitUse{key: path + ":" + fn, what: what, pos: fset.Position(n.Pos()).String()})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok {
					if x, ok := ast.Unparen(sel.X).(*ast.Ident); ok && names[x.Name] == urfavePath && sel.Sel.Name == "Exit" && emptyLiteralFirstArg(n) {
						allowedExit[sel] = true
					}
				}
				if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && slices.Contains(dotImports, urfavePath) && id.Name == "Exit" && emptyLiteralFirstArg(n) {
					allowedExit[id] = true
				}
			case *ast.SelectorExpr:
				x, ok := ast.Unparen(n.X).(*ast.Ident)
				if !ok {
					return true
				}
				importPath, imported := names[x.Name]
				if !imported {
					return true
				}
				switch {
				case processExitRefused(importPath, n.Sel.Name):
					add(n, x.Name+"."+n.Sel.Name)
				case importPath == urfavePath && n.Sel.Name == "Exit" && !allowedExit[n]:
					add(n, x.Name+".Exit without an empty string literal as its first argument")
				}
			case *ast.Ident:
				for _, p := range dotImports {
					switch {
					case processExitRefused(p, n.Name):
						add(n, n.Name+" from a dot import of "+p)
					case p == urfavePath && n.Name == "Exit" && !allowedExit[n]:
						add(n, "Exit from a dot import without an empty string literal as its first argument")
					}
				}
			}
			return true
		})
	}
	return uses
}

// loggerExitNames are the methods of *log.Logger that end the process.
var loggerExitNames = []string{"Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln"}

// isLoggerExitMethod reports whether fn is one of loggerExitNames declared
// on *log.Logger.
func isLoggerExitMethod(fn *types.Func) bool {
	if fn.Pkg() == nil || fn.Pkg().Path() != "log" || receiverName(fn) != "Logger" {
		return false
	}
	return slices.Contains(loggerExitNames, fn.Name())
}

// scanLoggerExits returns every use in pkg of a *log.Logger method that
// ends the process, in a file inScope accepts. A use is any mention of the
// method the type checker resolves: a call, a method value, a method
// expression, a method promoted from an embedded logger. relPath turns a
// file name into the path the allowlist uses.
func scanLoggerExits(pkg *packages.Package, inScope func(string) bool, relPath func(string) string) []processExitUse {
	var uses []processExitUse
	for _, file := range pkg.Syntax {
		filePath := relPath(pkg.Fset.Position(file.Pos()).Filename)
		if !inScope(filePath) {
			continue
		}
		for _, decl := range file.Decls {
			fn := "<package>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if f, ok := pkg.TypesInfo.Uses[id].(*types.Func); ok && isLoggerExitMethod(f) {
					uses = append(uses, processExitUse{key: filePath + ":" + fn, what: "(*log.Logger)." + f.Name(), pos: pkg.Fset.Position(id.Pos()).String()})
				}
				return true
			})
		}
	}
	return uses
}

// moduleRelativePath returns name relative to the working directory, which
// the tests of this package run in the module root, with slashes.
func moduleRelativePath(name string) string {
	wd, err := os.Getwd()
	if err != nil {
		return name
	}
	rel, err := filepath.Rel(wd, name)
	if err != nil {
		return name
	}
	return filepath.ToSlash(rel)
}

// checkProcessExits returns one message per use outside the allowlist, one
// per allowlist entry whose count differs from the uses it covers, and one
// per entry that covers nothing.
func checkProcessExits(uses []processExitUse, allow map[string]processExitAllowance) []string {
	byKey := map[string][]processExitUse{}
	for _, u := range uses {
		byKey[u.key] = append(byKey[u.key], u)
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		a, ok := allow[key]
		switch {
		case !ok:
			for _, u := range byKey[key] {
				out = append(out, fmt.Sprintf("%s: %s ends the process below main; return an error and let Run's caller exit", u.pos, u.what))
			}
		case a.count != len(byKey[key]):
			out = append(out, fmt.Sprintf("%s: the allowlist covers %d use(s) (%s), found %d", key, a.count, a.why, len(byKey[key])))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allow)) {
		if len(byKey[key]) == 0 {
			out = append(out, "allowlist entry "+key+" matched nothing: remove it")
		}
	}
	return out
}

// TestNoProcessExitBelowMain asserts that no non-test Go file under
// internal/, credhelper/, skills/ and cmd/rancher ends the process outside the
// allowlist rows, which hold the exits main makes and the credential
// helper's serve mode.
func TestNoProcessExitBelowMain(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var uses []processExitUse
	scanned := 0
	for i, f := range tree.files {
		if !processExitScope(tree.paths[i]) {
			continue
		}
		scanned++
		uses = append(uses, scanProcessExits(tree.fset, tree.paths[i], f)...)
	}
	require.Greater(t, scanned, 100, "the scan reads the production tree")
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	for _, pkg := range pkgs {
		uses = append(uses, scanLoggerExits(pkg, processExitScope, moduleRelativePath)...)
	}
	assert.Empty(t, checkProcessExits(uses, processExitAllowlist))
}

// TestProcessExitScanFixtures asserts the rules of the scan on in-memory
// files: each refused form is found, a use as a value is found, an aliased
// or dot import does not hide a use, a package that only shares a name with
// a watched one is not matched, an empty-literal urfave Exit passes, and the
// allowlist rules on stale rows and wrong counts hold.
func TestProcessExitScanFixtures(t *testing.T) {
	t.Parallel()
	const header = "package p\nimport (\n\t\"os\"\n\t\"syscall\"\n\t\"log\"\n\tucli \"github.com/urfave/cli/v3\"\n\t\"github.com/rancher/rancher-cli/credhelper\"\n)\n"
	scan := func(t *testing.T, src string) []processExitUse {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", src, parser.AllErrors)
		require.NoError(t, err)
		return scanProcessExits(fset, "p.go", f)
	}
	refused := map[string]string{
		"os.Exit call":                   "func f() { os.Exit(1) }",
		"os.Exit as a value":             "func f() { exit := os.Exit; exit(1) }",
		"os.Exit passed as an argument":  "func f() { g(os.Exit) }\nfunc g(func(int)) {}",
		"syscall.Exit call":              "func f() { syscall.Exit(1) }",
		"log.Fatal":                      "func f() { log.Fatal(\"x\") }",
		"log.Fatalf":                     "func f() { log.Fatalf(\"x\") }",
		"log.Fatalln":                    "func f() { log.Fatalln(\"x\") }",
		"log.Panic":                      "func f() { log.Panic(\"x\") }",
		"log.Panicf":                     "func f() { log.Panicf(\"x\") }",
		"OsExiter call":                  "func f() { ucli.OsExiter(2) }",
		"OsExiter assignment":            "func f() { ucli.OsExiter = func(int) {} }",
		"HandleExitCoder":                "func f(err error) { ucli.HandleExitCoder(err) }",
		"ShowAppHelpAndExit":             "func f(c *ucli.Command) { ucli.ShowAppHelpAndExit(c, 0) }",
		"ShowCommandHelpAndExit":         "func f(c *ucli.Command) { ucli.ShowCommandHelpAndExit(c, \"x\", 0) }",
		"urfave Exit with a message":     "func f() error { return ucli.Exit(\"text\", 4) }",
		"urfave Exit with a variable":    "func f(msg string) error { return ucli.Exit(msg, 4) }",
		"urfave Exit with a constant":    "const m = \"\"\nfunc f() error { return ucli.Exit(m, 4) }",
		"urfave Exit with a raw message": "func f() error { return ucli.Exit(`text`, 4) }",
		"urfave Exit as a value":         "func f() { g(ucli.Exit) }\nfunc g(func(any, int) ucli.ExitCoder) {}",
		"urfave Exit with no argument":   "func f() { _ = ucli.Exit }",
		"credhelper.Serve":               "func f() { credhelper.Serve(nil) }",
		"a package-level var":            "var exit = os.Exit",
	}
	for name, body := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			assert.NotEmpty(t, scan(t, header+body))
		})
	}
	t.Run("an aliased import does not hide a use", func(t *testing.T) {
		uses := scan(t, "package p\nimport stdos \"os\"\nfunc f() { stdos.Exit(1) }")
		require.Len(t, uses, 1)
		assert.Equal(t, "p.go:f", uses[0].key)
	})
	t.Run("a dot import does not hide a use", func(t *testing.T) {
		assert.Len(t, scan(t, "package p\nimport . \"os\"\nfunc f() { Exit(1) }"), 1)
		assert.Len(t, scan(t, "package p\nimport . \"github.com/urfave/cli/v3\"\nfunc f() error { return Exit(\"x\", 1) }"), 1)
		assert.Empty(t, scan(t, "package p\nimport . \"github.com/urfave/cli/v3\"\nfunc f() error { return Exit(\"\", 1) }"))
	})
	t.Run("an empty string literal passes", func(t *testing.T) {
		assert.Empty(t, scan(t, header+"func f() error { return ucli.Exit(\"\", 7) }"))
		assert.Empty(t, scan(t, header+"func f() error { return ucli.Exit(``, 7) }"))
	})
	t.Run("a package that shares a name is not matched", func(t *testing.T) {
		assert.Empty(t, scan(t, "package p\nimport (\n\t\"example.com/os\"\n\t\"example.com/log\"\n)\nfunc f() { os.Exit(1); log.Fatal(\"x\") }"))
	})
	t.Run("a local value named like a package is not matched", func(t *testing.T) {
		assert.Empty(t, scan(t, "package p\ntype s struct{}\nfunc (s) Exit(int) {}\nfunc f() { var os s; os.Exit(1) }"))
	})
	t.Run("a use is keyed by file and function", func(t *testing.T) {
		uses := scan(t, header+"func a() { os.Exit(1) }\nfunc b() { os.Exit(1); os.Exit(2) }")
		require.Len(t, uses, 3)
		assert.Equal(t, 1, countKey(uses, "p.go:a"))
		assert.Equal(t, 2, countKey(uses, "p.go:b"))
	})

	t.Run("every refused name is found", func(t *testing.T) {
		for importPath, names := range map[string][]string{
			"os":        {"Exit"},
			"syscall":   {"Exit", "ExitProcess"},
			"log":       {"Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln"},
			windowsPath: {"Exit", "ExitProcess"},
			unixPath:    {"Exit"},
			klogPath: {
				"Fatal", "Fatalf", "Fatalln", "FatalDepth", "FatalfDepth", "FatallnDepth",
				"Exit", "Exitf", "Exitln", "ExitDepth", "ExitfDepth", "ExitlnDepth",
				"FlushAndExit", "OsExit",
			},
			urfavePath:     {"OsExiter", "HandleExitCoder", "ShowAppHelpAndExit", "ShowCommandHelpAndExit", "ShowSubcommandHelpAndExit", "ShowRootCommandHelpAndExit"},
			credhelperPath: {"Serve"},
		} {
			for _, name := range names {
				src := "package p\nimport x " + strconv.Quote(importPath) + "\nfunc f() { _ = x." + name + " }\n"
				assert.Len(t, scan(t, src), 1, "%s.%s", importPath, name)
			}
		}
	})
	t.Run("klog is matched by its path with and without an alias", func(t *testing.T) {
		assert.Len(t, scan(t, "package p\nimport \"k8s.io/klog/v2\"\nfunc f() { klog.Fatal(\"x\") }"), 1)
		assert.Len(t, scan(t, "package p\nimport k \"k8s.io/klog/v2\"\nfunc f() { k.Exitf(\"x\") }"), 1)
		assert.Len(t, scan(t, "package p\nimport \"k8s.io/klog/v2\"\nfunc f() { klog.OsExit = nil }"), 1)
		assert.Empty(t, scan(t, "package p\nimport \"k8s.io/klog/v2\"\nfunc f() { klog.Info(\"x\"); klog.Flush() }"))
	})
	t.Run("an unaliased urfave import is matched", func(t *testing.T) {
		assert.Len(t, scan(t, "package p\nimport \"github.com/urfave/cli/v3\"\nfunc f() error { return cli.Exit(\"x\", 1) }"), 1)
		assert.Empty(t, scan(t, "package p\nimport \"github.com/urfave/cli/v3\"\nfunc f() error { return cli.Exit(\"\", 7) }"))
	})
	t.Run("a parenthesized selector is matched", func(t *testing.T) {
		assert.Len(t, scan(t, header+"func f() { (os).Exit(1) }"), 1)
		assert.Len(t, scan(t, header+"func f() { (os.Exit)(1) }"), 1)
		assert.Len(t, scan(t, header+"func f() error { return (ucli.Exit)(\"x\", 1) }"), 1)
		assert.Empty(t, scan(t, header+"func f() error { return (ucli.Exit)(\"\", 7) }"))
	})
	t.Run("windows.TerminateProcess is allowed", func(t *testing.T) {
		assert.Empty(t, scan(t, "package p\nimport \"golang.org/x/sys/windows\"\nfunc f() { _ = windows.TerminateProcess(0, 1) }"))
	})

	t.Run("the allowlist rules", func(t *testing.T) {
		uses := scan(t, header+"func a() { os.Exit(1) }\nfunc b() { os.Exit(1); os.Exit(2) }")
		assert.Empty(t, checkProcessExits(uses, map[string]processExitAllowance{"p.go:a": {1, "r"}, "p.go:b": {2, "r"}}))
		got := checkProcessExits(uses, map[string]processExitAllowance{"p.go:a": {1, "r"}})
		require.Len(t, got, 2, "each use of b is reported")
		assert.Contains(t, got[0], "ends the process below main")
		got = checkProcessExits(uses, map[string]processExitAllowance{"p.go:a": {1, "r"}, "p.go:b": {1, "r"}})
		assert.Equal(t, []string{"p.go:b: the allowlist covers 1 use(s) (r), found 2"}, got)
		got = checkProcessExits(uses, map[string]processExitAllowance{"p.go:a": {1, "r"}, "p.go:b": {2, "r"}, "p.go:gone": {1, "r"}})
		assert.Equal(t, []string{"allowlist entry p.go:gone matched nothing: remove it"}, got)
	})
}

func countKey(uses []processExitUse, key string) int {
	n := 0
	for _, u := range uses {
		if u.key == key {
			n++
		}
	}
	return n
}

// TestProcessExitScopeCoversTheBinary asserts every package of this module
// that cmd/rancher links lies inside the process-exit scan's scope: a new
// top-level package the binary imports cannot sit outside the scan.
func TestProcessExitScopeCoversTheBinary(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps}
	pkgs, err := packages.Load(cfg, "./cmd/rancher")
	require.NoError(t, err)
	const module = "github.com/rancher/rancher-cli/"
	var outside []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if rel, ok := strings.CutPrefix(p.PkgPath, module); ok && !processExitScope(rel+"/") {
			outside = append(outside, p.PkgPath)
		}
	})
	assert.Empty(t, outside, "packages cmd/rancher links that the process-exit scan does not cover")
}

// TestLoggerExitScanFixtures asserts, on small packages the type checker
// loads, that the *log.Logger rule finds every method that ends the process
// however the logger is reached (a call on a constructor's result, a
// variable, a method value, a method expression, an embedded logger, a
// logger from slog.NewLogLogger), one use for each name in the rule, and that
// methods of the same names on testing.TB and on a type of the package's own,
// and the package-level log.Fatal (the other rule's), pass.
func TestLoggerExitScanFixtures(t *testing.T) {
	t.Parallel()
	always := func(string) bool { return true }
	keep := func(name string) string { return filepath.Base(name) }
	load := func(name string) []processExitUse {
		pkgs, err := loadPackages("./test/conformance/testdata/noprocessexit/"+name, "")
		require.NoError(t, err)
		require.Len(t, pkgs, 1)
		return scanLoggerExits(pkgs[0], always, keep)
	}
	got := map[string]int{}
	for _, u := range load("logger") {
		got[strings.TrimPrefix(u.key, "logger.go:")]++
	}
	assert.Equal(t, map[string]int{
		"callFatal": 1, "callFatalf": 1, "callFatalln": 1,
		"callPanic": 1, "callPanicf": 1, "callPanicln": 1,
		"newLogger": 1, "variable": 1, "methodValue": 1, "methodExpression": 1,
		"embeddedLogger": 1, "slogLogger": 1,
	}, got)
	assert.Empty(t, load("clean"))
}
