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

// execWriteAdvice is what a test uses instead of creating a file with an
// execute bit itself.
const execWriteAdvice = "use testfs.WriteExecutable, or testfs.WriteExecutableFile where an error must be returned"

// execWriteModeArgument maps an import path to the functions of that package
// that create a file, with the position of their mode argument.
var execWriteModeArgument = map[string]map[string]int{
	"os":        {"WriteFile": 2, "OpenFile": 2},
	"io/ioutil": {"WriteFile": 2},
	"syscall":   {"Open": 2},
}

// execWriteSite is one creation of a file that may have an execute bit.
type execWriteSite struct {
	line int
	what string
}

func (s execWriteSite) message(file string) string {
	return fmt.Sprintf("%s:%d: %s in test code: %s", file, s.line, s.what, execWriteAdvice)
}

// execWriteSites parses one Go file, whatever its build tags, and returns
// every creation of a file that may have an execute bit: a call of os.WriteFile,
// os.OpenFile, ioutil.WriteFile or syscall.Open whose mode is not an integer
// literal or has any bit of 0o111, and any use of those functions that is not
// a call. A named constant counts as not a literal. The package selector is
// resolved through the file's own imports, so an aliased import is seen.
//
// Known limits: the scan sees creation by Go code in a test file. It does not
// see a file made executable by a command a test runs (chmod +x in a shell,
// cp), os.Chmod after os.Create (a use of os.Chmod already needs a row in
// testfsAllowlist, but the method (*os.File).Chmod is not seen), unix.Open from
// golang.org/x/sys/unix, a helper that takes a mode argument and is called
// with 0o755, the methods of *os.Root (root.OpenFile, root.WriteFile) or a dot
// import of io/ioutil.
func execWriteSites(file string, src string) ([]execWriteSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	imports := map[string]string{}
	for _, spec := range f.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = importPath
	}
	unparen := func(e ast.Expr) ast.Expr {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				return e
			}
			e = p.X
		}
	}
	// creator returns the spelled name and the mode argument position when e
	// is a selector on one of the functions in execWriteModeArgument.
	creator := func(e ast.Expr) (string, int, bool) {
		sel, ok := unparen(e).(*ast.SelectorExpr)
		if !ok {
			return "", 0, false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", 0, false
		}
		importPath, ok := imports[id.Name]
		if !ok {
			return "", 0, false
		}
		position, ok := execWriteModeArgument[importPath][sel.Sel.Name]
		return id.Name + "." + sel.Sel.Name, position, ok
	}

	var sites []execWriteSite
	called := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			name, position, ok := creator(x.Fun)
			if !ok {
				return true
			}
			called[unparen(x.Fun)] = true
			line := fset.Position(x.Pos()).Line
			if len(x.Args) <= position {
				sites = append(sites, execWriteSite{line, name + " with no mode argument"})
				return true
			}
			lit, ok := unparen(x.Args[position]).(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				sites = append(sites, execWriteSite{line, name + " with a mode that is not an integer literal"})
				return true
			}
			mode, err := strconv.ParseInt(lit.Value, 0, 64)
			switch {
			case err != nil:
				sites = append(sites, execWriteSite{line, name + " with the mode " + lit.Value + ", which is not a number"})
			case mode&0o111 != 0:
				sites = append(sites, execWriteSite{line, name + " with the mode " + lit.Value})
			}
		case *ast.SelectorExpr:
			if name, _, ok := creator(x); ok && !called[x] {
				sites = append(sites, execWriteSite{fset.Position(x.Pos()).Line, name + " used as a value"})
			}
		}
		return true
	})
	return sites, nil
}

// execWriteRow lists how many sites a file holds and why they stay.
type execWriteRow struct {
	path   string
	count  int
	reason string
}

// execWriteProblems compares what the scan found with the allowlist: a site
// in a file with no row, a count that differs from its row in either
// direction, and a row that has no reason, repeats a file, names a missing
// file or lists no site.
func execWriteProblems(found map[string][]execWriteSite, rows []execWriteRow, exists func(string) bool) []string {
	var problems []string
	seen := map[string]bool{}
	for _, r := range rows {
		switch {
		case seen[r.path]:
			problems = append(problems, fmt.Sprintf("%s: two rows", r.path))
			continue
		case strings.TrimSpace(r.reason) == "":
			problems = append(problems, fmt.Sprintf("%s: the row has no reason", r.path))
		case !exists(r.path):
			problems = append(problems, fmt.Sprintf("%s: the file of the row no longer exists", r.path))
		case r.count < 1:
			problems = append(problems, fmt.Sprintf("%s: the row lists no site; delete it", r.path))
		}
		seen[r.path] = true
		got := len(found[r.path])
		switch {
		case got > r.count:
			problems = append(problems, fmt.Sprintf("%s: %d calls may create an executable file, and its row in execWriteAllowlist (test/conformance/execwriteuse_test.go) allows %d; %s", r.path, got, r.count, execWriteAdvice))
		case got < r.count:
			problems = append(problems, fmt.Sprintf("%s: %d calls may create an executable file, and its row in execWriteAllowlist (test/conformance/execwriteuse_test.go) lists %d; lower the count or delete the row", r.path, got, r.count))
		}
	}
	for file, sites := range found {
		if seen[file] {
			continue
		}
		for _, s := range sites {
			problems = append(problems, s.message(file))
		}
	}
	slices.Sort(problems)
	return problems
}

// TestTestsCreateExecutablesThroughTestfs asserts no test creates a file with
// an execute bit except through testfs.WriteExecutable or
// testfs.WriteExecutableFile, which hold syscall.ForkLock while the file is
// open for writing. Linux refuses to run a file that a forked child still has
// open for writing, so a test that writes a script and runs it fails now and
// then with "text file busy" when other tests start processes. Every site
// that remains is counted in a row of the allowlist, with a reason.
func TestTestsCreateExecutablesThroughTestfs(t *testing.T) {
	t.Parallel()
	found := map[string][]execWriteSite{}
	for _, f := range testfsScanFiles(t) {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		sites, err := execWriteSites(f, string(src))
		require.NoError(t, err, f)
		if len(sites) > 0 {
			found[f] = sites
		}
	}
	problems := execWriteProblems(found, execWriteAllowlist, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	assert.Empty(t, problems)
}

const (
	windowsOnlyReason   = "runs only on Windows, where a program starts even while another process has it open for writing"
	windowsHandleReason = windowsOnlyReason + "; one test keeps a handle open on purpose"
	modeConstantReason  = "the mode is identityFilePerm or transitionFilePerm, package constants whose value is 0o600"
)

var execWriteAllowlist = []execWriteRow{
	{"install/windows_harness_test.go", 3, windowsOnlyReason},
	{"install/windows_install_test.go", 4, windowsHandleReason},
	{"internal/config/credstoreid_test.go", 6, modeConstantReason},
	{"internal/config/finishedname_test.go", 1, modeConstantReason},
	{"internal/config/keychainidentity_test.go", 2, modeConstantReason},
}

// TestExecWriteCheckerTable asserts the scan accepts a file created without
// an execute bit, and refuses an execute bit, a mode it cannot read, an
// aliased import and a function used as a value.
func TestExecWriteCheckerTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int
	}{
		{"owner-only literal", `os.WriteFile("x", nil, 0o600)`, 0},
		{"plain literal", `os.WriteFile("x", nil, 0o644)`, 0},
		{"readable by all but not executable", `os.WriteFile("x", nil, 0o666)`, 0},
		{"old octal spelling", `os.WriteFile("x", nil, 0600)`, 0},
		{"open for reading", `os.OpenFile("x", os.O_RDONLY, 0)`, 0},
		{"comment names the call", "// os.WriteFile(\"x\", nil, 0o755)\n_ = 1", 0},
		{"string names the call", `_ = "os.WriteFile(x, nil, 0o755)"`, 0},
		{"another package", `other.WriteFile("x", nil, 0o755)`, 0},
		{"owner execute bit", `os.WriteFile("x", nil, 0o755)`, 1},
		{"owner only execute bit", `os.WriteFile("x", nil, 0o700)`, 1},
		{"execute bits only", `os.WriteFile("x", nil, 0o111)`, 1},
		{"old octal execute bit", `os.WriteFile("x", nil, 0755)`, 1},
		{"hexadecimal execute bit", `os.WriteFile("x", nil, 0x1ed)`, 1},
		{"variable mode", `mode := os.FileMode(0o600); _ = os.WriteFile("x", nil, mode)`, 1},
		{"converted mode", `os.WriteFile("x", nil, os.FileMode(0o755))`, 1},
		{"named constant", `os.WriteFile("x", nil, someMode)`, 1},
		{"permission mask", `os.WriteFile("x", nil, fs.ModePerm)`, 1},
		{"function value", `w := os.WriteFile; _ = w`, 1},
		{"parenthesized call with an execute bit", `(os.WriteFile)("x", nil, 0o755)`, 1},
		{"parenthesized call without an execute bit", `(os.WriteFile)("x", nil, 0o644)`, 0},
		{"deprecated package", `ioutil.WriteFile("x", nil, 0o755)`, 1},
		{"open with create", `os.OpenFile("x", os.O_CREATE|os.O_WRONLY, 0o755)`, 1},
		{"syscall open", `syscall.Open("x", 0, 0o755)`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport (\n\t\"io/fs\"\n\t\"io/ioutil\"\n\t\"os\"\n\t\"syscall\"\n\n\t\"example.com/other\"\n)\n\nfunc f() {\n" + c.body + "\n}\n"
			sites, err := execWriteSites("p.go", src)
			require.NoError(t, err)
			assert.Len(t, sites, c.want, "%v", sites)
		})
	}

	t.Run("aliased import", func(t *testing.T) {
		t.Parallel()
		sites, err := execWriteSites("p.go", "package p\n\nimport stdos \"os\"\n\nfunc f() {\n\tstdos.WriteFile(\"x\", nil, 0o755)\n}\n")
		require.NoError(t, err)
		require.Len(t, sites, 1)
		assert.Equal(t, 6, sites[0].line)
	})

	t.Run("carriage returns", func(t *testing.T) {
		t.Parallel()
		sites, err := execWriteSites("p.go", "package p\r\n\r\nimport \"os\"\r\n\r\nfunc f() {\r\n\tos.WriteFile(\"x\", nil, 0o755)\r\n}\r\n")
		require.NoError(t, err)
		assert.Len(t, sites, 1)
	})

	t.Run("message names the file, the line and the writer", func(t *testing.T) {
		t.Parallel()
		sites, err := execWriteSites("p.go", "package p\n\nimport \"os\"\n\nfunc f() {\n\tos.WriteFile(\"x\", nil, 0o755)\n}\n")
		require.NoError(t, err)
		require.Len(t, sites, 1)
		msg := sites[0].message("dir/p_test.go")
		assert.Contains(t, msg, "dir/p_test.go:6")
		assert.Contains(t, msg, "testfs.WriteExecutable")
		assert.Contains(t, msg, "testfs.WriteExecutableFile")
	})
}

// TestExecWriteAllowlistTable asserts the allowlist check reports a count that
// is too high or too low, a missing file, a repeated file, a row with no
// reason and a site in a file with no row, and accepts an exact count.
func TestExecWriteAllowlistTable(t *testing.T) {
	t.Parallel()
	site := execWriteSite{line: 4, what: "os.WriteFile with the mode 0o755"}
	exists := func(p string) bool { return p != "gone_test.go" }
	cases := []struct {
		name  string
		found map[string][]execWriteSite
		rows  []execWriteRow
		want  string
	}{
		{"exact count", map[string][]execWriteSite{"a_test.go": {site, site}}, []execWriteRow{{"a_test.go", 2, "why"}}, ""},
		{"count too high", map[string][]execWriteSite{"a_test.go": {site, site}}, []execWriteRow{{"a_test.go", 1, "why"}}, "2 calls may create an executable file, and its row in execWriteAllowlist (test/conformance/execwriteuse_test.go) allows 1"},
		{"count too low", map[string][]execWriteSite{"a_test.go": {site}}, []execWriteRow{{"a_test.go", 2, "why"}}, "1 calls may create an executable file, and its row in execWriteAllowlist (test/conformance/execwriteuse_test.go) lists 2"},
		{"missing file", nil, []execWriteRow{{"gone_test.go", 1, "why"}}, "gone_test.go: the file of the row no longer exists"},
		{"repeated file", map[string][]execWriteSite{"a_test.go": {site}}, []execWriteRow{{"a_test.go", 1, "why"}, {"a_test.go", 1, "why"}}, "a_test.go: two rows"},
		{"row without a reason", map[string][]execWriteSite{"a_test.go": {site}}, []execWriteRow{{"a_test.go", 1, " "}}, "a_test.go: the row has no reason"},
		{"site without a row", map[string][]execWriteSite{"b_test.go": {site}}, nil, "b_test.go:4: os.WriteFile with the mode 0o755 in test code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := execWriteProblems(c.found, c.rows, exists)
			if c.want == "" {
				assert.Empty(t, problems)
				return
			}
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), c.want)
		})
	}
}
