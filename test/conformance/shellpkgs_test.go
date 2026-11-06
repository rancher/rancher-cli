package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellProgramCalls returns the position of every call in file of
// exec.Command, exec.CommandContext or exec.LookPath whose program argument
// is a string literal (interpreted or raw) that names fish or zsh, alone or
// as the last element of a path ("/bin/zsh"). The os/exec package is found
// under the name the file imports it by, including a dot import. The limits:
// a program name held in a variable or built by a call is not seen, and
// neither is a call through an exec function held in a variable.
func shellProgramCalls(fset *token.FileSet, file *ast.File) []string {
	name := execImportName(file)
	if name == "" {
		return nil
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := ""
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := f.X.(*ast.Ident); ok && name != "." && pkg.Name == name {
				fn = f.Sel.Name
			}
		case *ast.Ident:
			if name == "." {
				fn = f.Name
			}
		}
		arg := 0
		switch fn {
		case "Command", "LookPath":
		case "CommandContext":
			arg = 1
		default:
			return true
		}
		if arg >= len(call.Args) {
			return true
		}
		lit, ok := call.Args[arg].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if program, err := strconv.Unquote(lit.Value); err == nil {
			if base := path.Base(program); base == "fish" || base == "zsh" {
				found = append(found, fset.Position(call.Pos()).String()+": "+program)
			}
		}
		return true
	})
	return found
}

// TestShellProgramScanSeesTheCalls asserts the scan reports each call that
// starts or looks up fish or zsh by a literal name or path (such as
// /bin/zsh), in every spelling of the import, and leaves alone other
// programs, a name in another argument, a name held in a variable and an
// exec function held in a variable.
func TestShellProgramScanSeesTheCalls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"command zsh", "import \"os/exec\"\nvar c = exec.Command(\"zsh\")", 1},
		{"command fish with arguments", "import \"os/exec\"\nvar c = exec.Command(\"fish\", \"-c\", \"true\")", 1},
		{"command context fish", "import \"os/exec\"\nvar c = exec.CommandContext(nil, \"fish\")", 1},
		{"look path zsh", "import \"os/exec\"\nvar _, err = exec.LookPath(\"zsh\")", 1},
		{"raw string", "import \"os/exec\"\nvar c = exec.Command(`zsh`)", 1},
		{"path to zsh", "import \"os/exec\"\nvar c = exec.Command(\"/bin/zsh\", \"-c\", \"true\")", 1},
		{"path to fish in a look up", "import \"os/exec\"\nvar _, err = exec.LookPath(\"/usr/bin/fish\")", 1},
		{"path to a longer name", "import \"os/exec\"\nvar c = exec.Command(\"/usr/bin/fishy\")", 0},
		{"exec function held in a variable", "import \"os/exec\"\nvar run = exec.Command\nvar c = run(\"zsh\")", 0},
		{"renamed import", "import x \"os/exec\"\nvar c = x.Command(\"zsh\")", 1},
		{"dot import command", "import . \"os/exec\"\nvar c = Command(\"fish\")", 1},
		{"dot import look path", "import . \"os/exec\"\nvar _, err = LookPath(\"zsh\")", 1},
		{"two calls", "import \"os/exec\"\nvar a = exec.Command(\"zsh\")\nvar b = exec.Command(\"fish\")", 2},
		{"bash", "import \"os/exec\"\nvar c = exec.Command(\"bash\")", 0},
		{"sh and pwsh", "import \"os/exec\"\nvar a = exec.Command(\"sh\")\nvar b = exec.Command(\"pwsh\")", 0},
		{"shell name in another argument", "import \"os/exec\"\nvar c = exec.Command(\"sh\", \"-c\", \"zsh\")", 0},
		{"shell name as the third argument of command context", "import \"os/exec\"\nvar c = exec.CommandContext(nil, \"bash\", \"zsh\")", 0},
		{"a name in a variable", "import \"os/exec\"\nvar program = \"zsh\"\nvar c = exec.Command(program)", 0},
		{"a longer name", "import \"os/exec\"\nvar c = exec.Command(\"zshrc\")", 0},
		{"no os/exec import", "var c = exec.Command(\"zsh\")", 0},
		{"another package named like it", "import \"example.com/exec\"\nvar c = exec.Command(\"zsh\")", 0},
		{"blank import", "import _ \"os/exec\"\nvar c = exec.Command(\"zsh\")", 0},
		{"a method named Command", "import \"os/exec\"\nvar _ = exec.ErrNotFound\nvar c = other.Command(\"zsh\")", 0},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "x.go", "package x\n"+c.src+"\n", parser.SkipObjectResolution)
		require.NoError(t, err, c.name)
		assert.Len(t, shellProgramCalls(fset, file), c.want, c.name)
	}
}

// shellPackageDirs reads the shellpkgs list of scripts/test.
func shellPackageDirs(t *testing.T) []string {
	t.Helper()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	dirs, err := shellPackages(string(script))
	require.NoError(t, err)
	return dirs
}

// TestOnlyTheShellPackagesStartFishOrZsh asserts that no Go file git lists
// outside the shellpkgs directories, test file or not, calls exec.Command,
// exec.CommandContext or exec.LookPath with a literal program name that is
// fish or zsh, or a path ending in one. CI installs those shells in one job
// only, so a test elsewhere that starts them (directly or through a helper)
// would fail or skip in the jobs without them. A program name held in a
// variable and an exec function held in a variable are not seen.
func TestOnlyTheShellPackagesStartFishOrZsh(t *testing.T) {
	t.Parallel()
	dirs := shellPackageDirs(t)
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	scanned := 0
	for _, file := range files {
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		if slices.ContainsFunc(dirs, func(dir string) bool { return strings.HasPrefix(file, dir+"/") }) {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		scanned++
		for _, call := range shellProgramCalls(fset, parsed) {
			t.Errorf("only the packages listed in shellpkgs (scripts/test) may start fish or zsh, which CI installs in one job: %s", call)
		}
	}
	assert.NotZero(t, scanned, "no Go file found outside the shellpkgs directories")
}

// shellPackageProblems returns what is wrong with the shellpkgs list: it
// lacks the directory of shellHelperFile, or a listed directory has no test
// file that calls requireShell (callsHelper reports whether it has one).
func shellPackageProblems(listed []string, callsHelper func(dir string) bool) []string {
	var out []string
	if helperDir := path.Dir(shellHelperFile); !slices.Contains(listed, helperDir) {
		out = append(out, fmt.Sprintf("shellpkgs in scripts/test must list %s, the directory of %s: its tests start the shells CI installs in one job only", helperDir, shellHelperFile))
	}
	for _, dir := range listed {
		if !callsHelper(dir) {
			out = append(out, fmt.Sprintf("%s is in shellpkgs but none of its test files calls requireShell", dir))
		}
	}
	return out
}

// TestShellPackagesListTheShellTests asserts that the shellpkgs list of
// scripts/test holds the directory of shellHelperFile, whose tests start the
// shells, and that every listed directory has a test file git lists that
// calls requireShell, so a directory that no longer starts a shell is
// removed from the list. The completions tests pass the shell in a variable,
// so the scan of program names does not see them: this check asserts that the
// completions directory is listed.
func TestShellPackagesListTheShellTests(t *testing.T) {
	t.Parallel()
	listed := shellPackageDirs(t)
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	callers := map[string]bool{}
	for _, file := range files {
		dir := path.Dir(file)
		if !strings.HasSuffix(file, "_test.go") || !slices.Contains(listed, dir) {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(parsed, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "requireShell" {
					callers[dir] = true
				}
			}
			return true
		})
	}
	assert.Empty(t, shellPackageProblems(listed, func(dir string) bool { return callers[dir] }))
}

// TestShellPackageRules asserts what the shellpkgs checks accept and refuse:
// a list with the helper directory whose directories call requireShell, and
// not a non-empty list without the helper directory or a directory with no
// call of the helper.
func TestShellPackageRules(t *testing.T) {
	t.Parallel()
	helperDir := path.Dir(shellHelperFile)
	always := func(string) bool { return true }
	assert.Empty(t, shellPackageProblems([]string{helperDir}, always), "the helper directory alone")
	assert.Empty(t, shellPackageProblems([]string{"internal/other", helperDir}, always), "the helper directory and another one that calls the helper")
	assert.Contains(t, shellPackageProblems([]string{"internal/cli"}, always), fmt.Sprintf("shellpkgs in scripts/test must list %s, the directory of %s: its tests start the shells CI installs in one job only", helperDir, shellHelperFile), "a non-empty list without the helper directory")
	assert.Contains(t, shellPackageProblems([]string{helperDir, "internal/other"}, func(dir string) bool { return dir == helperDir }), "internal/other is in shellpkgs but none of its test files calls requireShell", "a directory with no call of the helper")
	assert.NotEmpty(t, shellPackageProblems(nil, always), "an empty list")
}

// shellStepPackages returns the module-relative directories that the go test
// line of the completion shell tests step body names as ./dir/ arguments.
func shellStepPackages(body string) []string {
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.Contains(line, "go test ") {
			continue
		}
		var dirs []string
		for word := range strings.FieldsSeq(line) {
			if rest, ok := strings.CutPrefix(word, "./"); ok {
				dirs = append(dirs, strings.TrimSuffix(rest, "/"))
			}
		}
		return dirs
	}
	return nil
}

// shellStepProblems returns what is wrong with the shellpkgs list next to
// the packages the completion shell tests step runs: a listed directory the
// step does not run (the shards leave it out and no job runs it), a package
// the step runs that is not listed (the shards run it as well), and a step
// that runs no package.
func shellStepProblems(listed, stepDirs []string) []string {
	var out []string
	if len(stepDirs) == 0 {
		out = append(out, "the completion shell tests step runs no package")
	}
	for _, dir := range listed {
		if !slices.Contains(stepDirs, dir) {
			out = append(out, fmt.Sprintf("%s is in shellpkgs but the completion shell tests step does not run ./%s/: its tests would run in no CI job", dir, dir))
		}
	}
	for _, dir := range stepDirs {
		if !slices.Contains(listed, dir) {
			out = append(out, fmt.Sprintf("the completion shell tests step runs ./%s/, which is not in shellpkgs: the race shards would run it as well", dir))
		}
	}
	return out
}

// TestShellPackagesAreTheStepPackages asserts that the shellpkgs list of
// scripts/test and the packages named on the go test line of the completion
// shell tests step (which ci.yaml must hold whole, see allowedPartSteps) are
// the same set, so a package the shards leave out always runs in that step.
func TestShellPackagesAreTheStepPackages(t *testing.T) {
	t.Parallel()
	assert.Empty(t, shellStepProblems(shellPackageDirs(t), shellStepPackages(completionStepBody)))
}

// TestShellStepRules asserts what the check of the step against shellpkgs
// accepts and refuses: the same set, and not a listed directory the step does
// not run, a package the step runs that is not listed, or a step with no go
// test package.
func TestShellStepRules(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"internal/cli/completions"}, shellStepPackages(completionStepBody))
	assert.Equal(t, []string{"a", "b/c"}, shellStepPackages("true\nif ! go test -race ./a/ ./b/c > \"$log\"; then\n"))
	assert.Empty(t, shellStepPackages("echo hello\n"), "a body with no go test line")
	assert.Empty(t, shellStepProblems([]string{"a"}, []string{"a"}))
	assert.Empty(t, shellStepProblems([]string{"a", "b"}, []string{"b", "a"}), "the same set in another order")
	assert.Contains(t, shellStepProblems([]string{"a", "b"}, []string{"a"}), "b is in shellpkgs but the completion shell tests step does not run ./b/: its tests would run in no CI job", "a listed directory the step does not run")
	assert.Contains(t, shellStepProblems([]string{"a"}, []string{"a", "b"}), "the completion shell tests step runs ./b/, which is not in shellpkgs: the race shards would run it as well", "a package the step runs that is not listed")
	assert.Contains(t, shellStepProblems([]string{"a"}, nil), "the completion shell tests step runs no package", "a step with no package")
}
