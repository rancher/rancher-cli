package provisioning

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runtimePackagePath is the import path of the package whose Run function runs
// a command.
const runtimePackagePath = "github.com/rancher/rancher-cli/internal/runtime"

// cliPackagePath is the import path of the package whose functions run a whole
// command line through the dispatcher.
const cliPackagePath = "github.com/rancher/rancher-cli/internal/cli"

// cliDirectory is the directory of package cli, where the functions that run a
// command line are called by their bare names.
const cliDirectory = "internal/cli"

// actionReference is the call of the one reference that is counted by file and
// function, not an offender at once: a selector named Action that is not called
// and not assigned.
const actionReference = "reference to Action"

// commandRunPlaces are the only places in the non-test Go files of the module
// that run a command: a call of a method named Execute with three arguments or
// with four (a method expression), a call of Run of package runtime, a call of
// a function named Action with three arguments, a call of Run, run, runArgv or
// dispatch of package cli (the functions that run a whole command line), a call
// of a selector named Run with two arguments, and a reference to Action that is
// not a call or an assignment. Each is listed by file and function (never by
// line), with the call and the number of calls the function has today, and a
// reason. A function with more calls than listed fails, and a row whose
// function no longer has its call fails, so a second call needs a review and a
// row. No row lists a call of Execute with four arguments: none exists today,
// and a new one needs a row with a reason.
var commandRunPlaces = []struct {
	file, function, call string
	count                int
	reason               string
}{
	{"internal/runtime/pipeline.go", "runRead", "Execute", 1, "the pipeline runs the read command it was given"},
	{"internal/commands/cmdtest/kubectl.go", "RunKubectl", "Execute", 1, "the kubectl test helper"},
	{"internal/cli/adapter.go", "Adapt", "runtime.Run", 1, "the action of a command wired through Adapt runs its command"},
	{"internal/cli/engine.go", "adaptEntry", "runtime.Run", 1, "the action of a command wired through an entry runs its command"},
	{"internal/cli/dispatcher.go", "dispatch", "Action", 1, "the dispatcher runs the command the line names"},
	{"cmd/rancher/main.go", "main", "cli.Run", 1, "the program runs the command line it was started with"},
	{"internal/cli/run.go", "Run", "cli.run", 1, "the exported entry runs the command line it was given"},
	{"internal/cli/run.go", "run", "cli.runArgv", 1, "the entry passes the command line on to the dispatcher"},
	{"internal/cli/dispatcher.go", "Command.Run", "cli.runArgv", 1, "the method of the root command runs the command line it was given"},
	{"internal/cli/run.go", "runArgv", "cli.dispatch", 2, "the dispatcher runs the command the line names, in both of its two cases"},
	{"internal/commands/cmdtest/prelude.go", "RunPreludeRows", "Run with two arguments", 1, "testing.T.Run of the test helper, not a command"},
	{"internal/cli/dispatcher.go", "dispatch", actionReference, 1, "the dispatcher's nil check of Action"},
	{"internal/commands/skill/skill.go", "outcomeRows", actionReference, 1, "a data field named Action"},
	{"internal/runtime/pipeline.go", "checkPlan", actionReference, 3, "a data field named Action"},
	{"internal/runtime/pipeline.go", "dryRunText", actionReference, 1, "a data field named Action"},
	{"internal/skillinstall/ops.go", "Report.TextDetails", actionReference, 1, "a data field named Action"},
	{"internal/runtime/registry.go", "effectSchema", actionReference, 1, "the type safety.Action"},
}

// commandRun is one call that runs a command, or a reference to a function
// that runs one without a call.
type commandRun struct {
	function, call, at string
}

// findCommandRuns parses the Go source of filePath, a slash-separated path
// relative to the module root (read from the file when src is nil), and returns
// each call that runs a command, with the function it is in ("" at the package
// level), as the call it is:
//   - "Execute" for a call of a selector named Execute with three arguments, and
//     "Execute with four arguments" for one with four: a method expression such
//     as runtime.ReadCommand.Execute(rc, ctx, in, p) takes the receiver as its
//     first argument, and go/ast cannot tell a type from a value, so the count
//     decides;
//   - "Action" for a call of a selector named Action with three arguments;
//   - "runtime.Run" for a call of Run of package runtime, found through the
//     import of the file: the selector Run whose operand is the name the file
//     gives the package (the alias when it has one, else runtime), and in the
//     files of internal/runtime itself and of a file that imports the package
//     with a dot, a bare Run;
//   - "cli.Run" for a call of Run of package internal/cli, found as runtime.Run
//     is (a bare Run in the files of internal/cli and of a file that imports the
//     package with a dot), and "cli.run", "cli.runArgv" and "cli.dispatch" for a
//     bare call of run, runArgv and dispatch in the files of internal/cli, the
//     functions that run a whole command line;
//   - "Run with two arguments" for a call of any selector named Run with
//     exactly two arguments, which is how the Run of the root command runs a
//     command line.
//
// A reference to Execute, to Run of package runtime, or to Run, runArgv or
// dispatch of package cli that is not the function of a call (a method value or
// a function value runs a command later) is returned with the call "reference
// to <name>". A reference to run is not returned, because a file of
// internal/cli names a local variable run. A selector named Action that is
// neither the function of a call nor the left side of an assignment is returned
// as "reference to Action"; a call of Action with another number of arguments
// is not returned. A call of Execute with another number of arguments, such as
// a template's, and a declaration named Execute, Run, run, runArgv or dispatch
// are not returned, and neither is a name that a composite literal key, a
// variable declared with := or in a range, or a label declares.
//
// The scan does not see a call made through reflection, a call of a function
// value that a reported reference stored (the reference is reported, the later
// call is not), a function value of run (the function in run.go), the Run
// method of the root command written as a method expression
// ((*Command).Run(root, ctx, args)) or taken as a method value (f := root.Run),
// a call of Execute, Action or Run whose arguments are the results of one other
// call (c.Execute(f())), or a file the module scan leaves out (a test file, a
// file under testdata). It skips the declaration of a local variable named Run
// in internal/runtime and reports each use of it as a use of the function Run.
// Whatever the scan misses, TestEveryMutationSelectsACredentialByID (create
// cluster, patch cloud-credential and delete cloud-credential select a
// credential by id only) and TestMutationsOfferOnlyCredentialIDs (their
// completion offers ids only) cover the three mutations that take a cloud
// credential.
func findCommandRuns(filePath string, src any) ([]commandRun, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, src, 0)
	if err != nil {
		return nil, err
	}
	runtimeName, cliName := "", ""
	bareRun := path.Dir(filePath) == "internal/runtime"
	bareCLI := path.Dir(filePath) == cliDirectory
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || (importPath != runtimePackagePath && importPath != cliPackagePath) {
			continue
		}
		name, bare := &runtimeName, &bareRun
		if importPath == cliPackagePath {
			name, bare = &cliName, &bareCLI
		}
		switch {
		case imp.Name == nil:
			*name = path.Base(importPath)
		case imp.Name.Name == ".":
			*bare = true
		case imp.Name.Name != "_":
			*name = imp.Name.Name
		}
	}
	var runs []commandRun
	for _, decl := range file.Decls {
		function := enclosingFunction(decl)
		called := map[ast.Expr]*ast.CallExpr{}
		assigned := map[ast.Expr]bool{}
		declared := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				called[n.Fun] = n
			case *ast.AssignStmt:
				for _, left := range n.Lhs {
					assigned[left] = true
					if id, ok := left.(*ast.Ident); ok && n.Tok == token.DEFINE {
						declared[id] = true
					}
				}
			case *ast.RangeStmt:
				if n.Tok == token.DEFINE {
					for _, expr := range []ast.Expr{n.Key, n.Value} {
						if id, ok := expr.(*ast.Ident); ok {
							declared[id] = true
						}
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok {
					declared[id] = true
				}
			case *ast.LabeledStmt:
				declared[n.Label] = true
			case *ast.BranchStmt:
				if n.Label != nil {
					declared[n.Label] = true
				}
			case *ast.SelectorExpr:
				declared[n.Sel] = true
			case *ast.FuncDecl:
				declared[n.Name] = true
			case *ast.TypeSpec:
				declared[n.Name] = true
			case *ast.ValueSpec:
				for _, id := range n.Names {
					declared[id] = true
				}
			case *ast.Field:
				for _, id := range n.Names {
					declared[id] = true
				}
			}
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			var name string
			var pos token.Pos
			var call *ast.CallExpr
			var selector *ast.SelectorExpr
			switch n := n.(type) {
			case *ast.SelectorExpr:
				selector = n
				call = called[n]
				operand, _ := n.X.(*ast.Ident)
				switch {
				case n.Sel.Name == "Execute", n.Sel.Name == "Action":
					name = n.Sel.Name
				case n.Sel.Name == "Run" && operand != nil && runtimeName != "" && operand.Name == runtimeName:
					name = "runtime.Run"
				case n.Sel.Name == "Run" && operand != nil && cliName != "" && operand.Name == cliName:
					name = "cli.Run"
				case n.Sel.Name == "Run" && call != nil && len(call.Args) == 2:
					name = "Run with two arguments"
				default:
					return true
				}
				pos = n.Sel.Pos()
			case *ast.Ident:
				switch {
				case declared[n]:
					return true
				case bareRun && n.Name == "Run":
					name = "runtime.Run"
				case bareCLI && slices.Contains([]string{"Run", "run", "runArgv", "dispatch"}, n.Name):
					name = "cli." + n.Name
				default:
					return true
				}
				pos, call = n.Pos(), called[n]
			default:
				return true
			}
			at := fset.Position(pos).String()
			switch {
			case name == "Action" && call != nil && (len(call.Args) == 3 || call.Ellipsis.IsValid()):
				runs = append(runs, commandRun{function, name, at})
			case name == "Action" && (call != nil || assigned[selector]):
			case name == "Action":
				runs = append(runs, commandRun{function, actionReference, at})
			case name == "cli.run" && call == nil:
			case call == nil:
				runs = append(runs, commandRun{function, "reference to " + name, at})
			case name == "Execute" && len(call.Args) == 4 && !call.Ellipsis.IsValid():
				runs = append(runs, commandRun{function, "Execute with four arguments", at})
			case name == "Execute" && len(call.Args) != 3 && !call.Ellipsis.IsValid():
			default:
				runs = append(runs, commandRun{function, name, at})
			}
			return true
		})
	}
	return runs, nil
}

// TestCommandsAreRunOnlyAtTheListedPlaces asserts that a call that runs a
// command (Execute with three arguments or with four, Run of package runtime,
// Action with three arguments, Run, run, runArgv or dispatch of package cli, a
// selector Run with two arguments) and a reference to Action that is not a call
// or an assignment appear in the non-test Go files of the module only in a
// function of commandRunPlaces, as many times as the row says, so no command
// runs another command: a mutation that takes a cloud credential cannot run
// the read command that selects a credential by display name. It also asserts
// that no other reference to Execute, Run, runArgv or dispatch hides a call,
// and that every row still has its call.
func TestCommandsAreRunOnlyAtTheListedPlaces(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	counts := map[string]int{}
	var offenders []string
	for _, rel := range moduleGoFiles(t, root) {
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		require.NoError(t, err)
		runs, err := findCommandRuns(rel, source)
		require.NoError(t, err)
		for _, r := range runs {
			if strings.HasPrefix(r.call, "reference to ") && r.call != actionReference {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q runs a command later; call it where it is listed", r.at, r.call, r.function))
				continue
			}
			counts[rel+" "+r.function+" "+r.call]++
		}
	}
	listed := map[string]int{}
	for _, p := range commandRunPlaces {
		assert.NotEmpty(t, p.reason, "%s %s lists no reason", p.file, p.function)
		listed[p.file+" "+p.function+" "+p.call] = p.count
	}
	for key, n := range counts {
		switch want, ok := listed[key]; {
		case !ok:
			offenders = append(offenders, fmt.Sprintf("%s has %d call(s) that run a command and is not in commandRunPlaces", key, n))
		case n > want:
			offenders = append(offenders, fmt.Sprintf("%s has %d calls that run a command; commandRunPlaces lists %d", key, n, want))
		}
	}
	assert.Empty(t, offenders, "a command is run only at the listed places")
	for _, p := range commandRunPlaces {
		key := p.file + " " + p.function + " " + p.call
		assert.Positive(t, counts[key], "%s is listed for %s and no longer has it; remove the row", key, p.call)
	}
}

// TestTheCommandRunScanFindsEveryRun asserts the rule of findCommandRuns on
// small sources: a call of Execute with three arguments or with four (a method
// expression), of Run of package runtime and of package cli (through its alias
// or its bare name), of run, runArgv and dispatch in internal/cli, of a
// selector Run with two arguments and of Action with three arguments is found,
// and so is a reference to Execute, Run, runArgv or dispatch that is not a call
// and a reference to Action that is neither a call nor an assignment; a call
// with another number of arguments, a method named Execute, another package
// named runtime or cli, and the names that a composite literal key, a local
// variable, a range variable or a label declares are not.
func TestTheCommandRunScanFindsEveryRun(t *testing.T) {
	t.Parallel()
	const runtimeImport = "import (\n\truntime \"github.com/rancher/rancher-cli/internal/runtime\"\n)\n\n"
	const aliasImport = "import (\n\tiruntime \"github.com/rancher/rancher-cli/internal/runtime\"\n)\n\n"
	const cliImport = "import (\n\t\"github.com/rancher/rancher-cli/internal/cli\"\n)\n\n"
	const cliAliasImport = "import (\n\tc2 \"github.com/rancher/rancher-cli/internal/cli\"\n)\n\n"
	for _, tc := range []struct {
		name, file, src string
		want            []string
	}{
		{"Execute with three arguments", "internal/cli/p.go", "func f() { c.Execute(ctx, in, p) }\n", []string{"f Execute"}},
		{"Execute with three arguments at the package level", "internal/cli/p.go", "var _ = c.Execute(ctx, in, p)\n", []string{" Execute"}},
		{"Execute through a type assertion", "internal/cli/p.go", "func f() { c.read.(runtime.ReadCommand).Execute(ctx, in, p) }\n", []string{"f Execute"}},
		{"Execute with a spread argument", "internal/cli/p.go", "func f() { c.Execute(args...) }\n", []string{"f Execute"}},
		{"Execute with four arguments", "internal/cli/p.go", "func f() { c.Execute(ctx, in, p, x) }\n", []string{"f Execute with four arguments"}},
		{"a method expression of an interface type", "internal/cli/p.go", "func f() { runtime.ReadCommand.Execute(rc, ctx, in, p) }\n", []string{"f Execute with four arguments"}},
		{"a method expression of a pointer type", "internal/cli/p.go", "func f() { (*T).Execute(c, ctx, in, p) }\n", []string{"f Execute with four arguments"}},
		{"Execute with five arguments", "internal/cli/p.go", "func f() { x.Execute(a, b, c, d, e) }\n", nil},
		{"Execute with two arguments", "internal/cli/p.go", "func f() { t.Execute(w, v) }\n", nil},
		{"a method named Execute", "internal/cli/p.go", "func (c *T) Execute(ctx int, in int, p int) {}\n", nil},
		{"a method value of Execute", "internal/cli/p.go", "func f() { g := c.Execute; g(ctx, in, p) }\n", []string{"f reference to Execute"}},
		{"Run of runtime", "internal/cli/p.go", runtimeImport + "func f() { runtime.Run(ctx, cmd, in, io, b) }\n", []string{"f runtime.Run"}},
		{"Run through an alias", "internal/cli/p.go", aliasImport + "func f() { iruntime.Run(ctx, cmd, in, io, b) }\n", []string{"f runtime.Run"}},
		{"the alias name and a Run of another package", "internal/cli/p.go", aliasImport + "func f() { runtime.Run(ctx) }\n", nil},
		{"Run of the standard runtime package", "internal/cli/p.go", "import \"runtime\"\n\nfunc f() { runtime.Run(ctx) }\n", nil},
		{"a function value of Run", "internal/cli/p.go", runtimeImport + "func f() { g := runtime.Run; _ = g }\n", []string{"f reference to runtime.Run"}},
		{"a bare Run in internal/runtime", "internal/runtime/p.go", "func f() { Run(ctx, cmd, in, io, b) }\n", []string{"f runtime.Run"}},
		{"a bare Run in another package", "internal/token/p.go", "func f() { Run(ctx, cmd, in, io, b) }\n", nil},
		{"the declaration of Run in internal/runtime", "internal/runtime/p.go", "func Run(ctx int) {}\n\ntype T struct{ Run func() }\n", nil},
		{"a method Run called in internal/runtime", "internal/runtime/p.go", "func f() { t.Run(\"x\") }\n", nil},
		{"a dot import and a bare Run", "internal/token/p.go", "import . \"github.com/rancher/rancher-cli/internal/runtime\"\n\nfunc f() { Run(ctx, cmd, in, io, b) }\n", []string{"f runtime.Run"}},
		{"a composite literal key named Run in internal/runtime", "internal/runtime/p.go", "func f() { _ = hooks{Run: g} }\n", nil},
		{"the value of a composite literal key in internal/runtime", "internal/runtime/p.go", "func f() { _ = hooks{Fn: Run} }\n", []string{"f reference to runtime.Run"}},
		{"a range variable named Run in internal/runtime", "internal/runtime/p.go", "func f() { for Run := range xs {} }\n", nil},
		{"a label named Run and a break of it in internal/runtime", "internal/runtime/p.go", "func f() {\nRun:\n\tfor {\n\t\tbreak Run\n\t}\n}\n", nil},
		{"a type switch binding named Run in internal/runtime", "internal/runtime/p.go", "func f() { switch Run := x.(type) {\n\tdefault:\n\t} }\n", nil},
		{"a local variable named Run is declared and its use is a reference", "internal/runtime/p.go", "func f() { Run := 1; use(Run) }\n", []string{"f reference to runtime.Run"}},
		{"Run of internal/cli through its import", "internal/token/p.go", cliImport + "func f() { cli.Run(ctx, c, args) }\n", []string{"f cli.Run"}},
		{"Run of internal/cli through an alias", "internal/token/p.go", cliAliasImport + "func f() { c2.Run(ctx, c, args) }\n", []string{"f cli.Run"}},
		{"a package named cli of another import path", "internal/token/p.go", "import \"example.com/cli\"\n\nfunc f() { cli.Run(ctx) }\n", nil},
		{"a function value of Run of internal/cli", "internal/token/p.go", cliImport + "func f() { g := cli.Run; _ = g }\n", []string{"f reference to cli.Run"}},
		{"a dot import of internal/cli and a bare Run", "internal/token/p.go", "import . \"github.com/rancher/rancher-cli/internal/cli\"\n\nfunc f() { Run(ctx, c, args) }\n", []string{"f cli.Run"}},
		{"a bare Run in internal/cli", "internal/cli/p.go", "func f() { Run(ctx, c, args) }\n", []string{"f cli.Run"}},
		{"a bare run in internal/cli", "internal/cli/p.go", "func f() { run(ctx, c, args) }\n", []string{"f cli.run"}},
		{"a bare runArgv in internal/cli", "internal/cli/p.go", "func f() { runArgv(ctx, c, args, false) }\n", []string{"f cli.runArgv"}},
		{"a bare dispatch in internal/cli", "internal/cli/p.go", "func f() { dispatch(ctx, c, args) }\n", []string{"f cli.dispatch"}},
		{"a function value of runArgv", "internal/cli/p.go", "func f() { g := runArgv; _ = g }\n", []string{"f reference to cli.runArgv"}},
		{"a function value of dispatch", "internal/cli/p.go", "func f() { g := dispatch; _ = g }\n", []string{"f reference to cli.dispatch"}},
		{"a function value of run is not found", "internal/cli/p.go", "func f() { g := run; _ = g }\n", nil},
		{"a local variable named run", "internal/cli/p.go", "func f() { run := rec{}; run.Outcome = 1; g(run) }\n", nil},
		{"the declarations of the functions that run a command line", "internal/cli/p.go", "func Run() {}\n\nfunc run() {}\n\nfunc runArgv() {}\n\nfunc dispatch() {}\n", nil},
		{"Run of the root command", "internal/cli/p.go", "func f() { c.Root().Run(ctx, args) }\n", []string{"f Run with two arguments"}},
		{"Run of a command that takes no arguments", "internal/cli/p.go", "func f() { exec.Command(x).Run() }\n", nil},
		{"Run with three arguments", "internal/cli/p.go", "func f() { t.Run(a, b, c) }\n", nil},
		{"Action with three arguments", "internal/cli/p.go", "func f() { c.Action(ctx, c, in) }\n", []string{"f Action"}},
		{"Action found through the root", "internal/cli/p.go", "func (c *T) g() { get := c.Root().Command(\"get\"); get.Action(ctx, get, in) }\n", []string{"T.g Action"}},
		{"Action with no argument", "internal/cli/p.go", "func f() { actions.Action() }\n", nil},
		{"an assignment of Action", "internal/cli/p.go", "func f() { cmd.Action = func(ctx int, c int, in int) error { return nil } }\n", nil},
		{"an assignment of nil to Action", "internal/cli/p.go", "func f() { cmd.Action = nil }\n", nil},
		{"a key named Action", "internal/cli/p.go", "func f() { _ = &Command{Action: g} }\n", nil},
		{"a field named Action", "internal/cli/p.go", "func f() { row = append(row, o.Action) }\n", []string{"f reference to Action"}},
		{"a function value of Action", "internal/cli/p.go", "func f() { act := get.Action; act(ctx, get, in) }\n", []string{"f reference to Action"}},
		{"a comparison of Action", "internal/cli/p.go", "func f() { if get.Action == nil {} }\n", []string{"f reference to Action"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs, err := findCommandRuns(tc.file, "package p\n\n"+tc.src)
			require.NoError(t, err)
			var got []string
			for _, r := range runs {
				got = append(got, r.function+" "+r.call)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
