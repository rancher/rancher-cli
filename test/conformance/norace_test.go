package conformance

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// noRaceScriptPath is the script that lists the packages whose tests run
// without the race detector.
const noRaceScriptPath = "scripts/test"

// noRaceListRE finds the list line in scripts/test: norace=(dir dir ...).
var noRaceListRE = regexp.MustCompile(`(?m)^norace=\(([^()\n]*)\)[ \t]*$`)

// noRaceAssignREs find every command in the script that sets, extends or
// removes the list, in any form and anywhere on a line (after `&&`, inside
// an `if` or an `eval` string), so a second assignment (norace+=(dir),
// norace[1]=dir, unset norace) cannot change the list behind the one line
// the check reads. They run on the script with its comments removed.
var noRaceAssignREs = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_])norace(?:\[[^\]\n]*\])?\+?=`),
	regexp.MustCompile(`(?m)\b(?:unset|read|mapfile|readarray)\b[^\n]*\bnorace\b`),
}

// noRaceCommentRE matches a shell comment: a "#" at the start of a line or
// after a space or tab, to the end of the line.
var noRaceCommentRE = regexp.MustCompile(`(?m)(?:^|[ \t])#.*$`)

// noRaceEntryRE is the form of a listed directory: module-relative words
// joined by "/", the form the script compares with a package's import
// path after the module path.
var noRaceEntryRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)

// noRacePackages reads the module-relative directories listed on the
// norace line of script. It fails when the line is missing, appears more
// than once, lists nothing, or is not the only command that sets the
// list; and when an entry is spelled with a leading "./" or a trailing
// "/", or in any form but noRaceEntryRE, because the script matches the
// whole import path and would run the package in both passes or neither.
// Each failure keeps a reworded script from turning the check below into
// a pass over the wrong packages.
func noRacePackages(script string) ([]string, error) {
	code := noRaceCommentRE.ReplaceAllString(script, "")
	assigns := 0
	for _, re := range noRaceAssignREs {
		assigns += len(re.FindAllString(code, -1))
	}
	if assigns != 1 {
		return nil, errors.New("want exactly one command that sets norace, found " + strconv.Itoa(assigns))
	}
	found := noRaceListRE.FindAllStringSubmatch(script, -1)
	if len(found) != 1 {
		return nil, errors.New("want exactly one line of the form norace=(dir dir ...), found " + strconv.Itoa(len(found)))
	}
	dirs := strings.Fields(found[0][1])
	if len(dirs) == 0 {
		return nil, errors.New("the norace list is empty")
	}
	for _, dir := range dirs {
		switch {
		case strings.HasPrefix(dir, "./"):
			return nil, errors.New("norace entry " + strconv.Quote(dir) + " must not start with ./")
		case strings.HasSuffix(dir, "/"):
			return nil, errors.New("norace entry " + strconv.Quote(dir) + " must not end with /")
		case !noRaceEntryRE.MatchString(dir):
			return nil, errors.New("norace entry " + strconv.Quote(dir) + " must be directory names joined by /")
		}
	}
	return dirs, nil
}

// concurrencyUses lists what in file starts or coordinates a goroutine:
// a go statement, a channel type, a send, a receive, a select, a timer
// callback (time.AfterFunc, context.AfterFunc), an import of sync/atomic
// or golang.org/x/sync, a dot import of sync, and any use of the sync
// package other than OnceValue and OnceValues. Each entry is a position
// and a description.
func concurrencyUses(fset *token.FileSet, file *ast.File) []string {
	var uses []string
	add := func(pos token.Pos, what string) {
		uses = append(uses, fset.Position(pos).String()+": "+what)
	}
	syncNames := map[string]bool{}
	callbackNames := map[string]string{}
	dotCallback := ""
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case path == "sync" && name == ".":
			add(imp.Pos(), "dot import of sync")
		case path == "sync":
			syncNames[name] = true
		case path == "sync/atomic" || strings.HasPrefix(path, "golang.org/x/sync"):
			add(imp.Pos(), "imports "+path)
		case (path == "time" || path == "context") && name == ".":
			dotCallback = path
		case path == "time" || path == "context":
			callbackNames[name] = path
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			add(x.Pos(), "go statement")
		case *ast.ChanType:
			add(x.Pos(), "channel type")
		case *ast.SendStmt:
			add(x.Pos(), "channel send")
		case *ast.SelectStmt:
			add(x.Pos(), "select statement")
		case *ast.UnaryExpr:
			if x.Op == token.ARROW {
				add(x.Pos(), "channel receive")
			}
		case *ast.Ident:
			if dotCallback != "" && x.Name == "AfterFunc" {
				add(x.Pos(), dotCallback+".AfterFunc")
			}
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			switch {
			case !ok:
			case syncNames[id.Name] && x.Sel.Name != "OnceValue" && x.Sel.Name != "OnceValues":
				add(x.Pos(), "sync."+x.Sel.Name)
			case callbackNames[id.Name] != "" && x.Sel.Name == "AfterFunc":
				add(x.Pos(), callbackNames[id.Name]+".AfterFunc")
			}
		}
		return true
	})
	return uses
}

// TestNoRacePackagesHoldNoConcurrency asserts that no package listed in
// scripts/test as running without the race detector contains a go
// statement, a channel, a select, a timer callback, a sync/atomic use, or
// a sync use other than sync.OnceValue and sync.OnceValues, in any of its
// Go files. The list is read from the script, so the two cannot drift.
// The check is syntactic: a goroutine started by a function the package
// calls from elsewhere is not seen, and nor is a parallel test that
// writes package state another reads; only -race finds that, so no test
// in a listed package may do either.
func TestNoRacePackagesHoldNoConcurrency(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	dirs, err := noRacePackages(string(script))
	require.NoError(t, err)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err, "%s is listed in %s", dir, noRaceScriptPath)
		fset := token.NewFileSet()
		parsed := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.ToSlash(filepath.Join(dir, e.Name())), nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			parsed++
			for _, use := range concurrencyUses(fset, file) {
				t.Errorf("%s runs without the race detector (%s) and must hold no concurrency: %s", dir, noRaceScriptPath, use)
			}
		}
		assert.NotZero(t, parsed, "%s is listed in %s but holds no Go files", dir, noRaceScriptPath)
	}
}

// TestNoRaceListReading asserts the list reader accepts the script's form
// and fails on a missing, repeated, empty or misspelled list, and on any
// other command that sets it.
func TestNoRaceListReading(t *testing.T) {
	t.Parallel()
	dirs, err := noRacePackages("set -e\n\nnorace=(test/a  test/b)\nflags=(x)\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"test/a", "test/b"}, dirs)
	for _, bad := range []string{
		"",
		"# norace=(test/a)\n",
		"  norace=(test/a)\n",
		"norace=()\n",
		"norace=(test/a)\nnorace=(test/b)\n",
		"norace=(test/a) # why\n",
		"norace=(test/a)\nnorace+=(test/b)\n",
		"norace=(test/a)\nnorace[1]=test/b\n",
		"norace=(test/a)\n  declare -a norace=(test/b)\n",
		"norace=(test/a)\nunset norace\n",
		"norace=(test/a)\ntrue && norace+=(test/b)\n",
		"norace=(test/a)\neval \"norace+=(test/b)\"\n",
		"norace=(test/a)\nif true; then norace+=(test/b); fi\n",
		"norace=(test/a)\nif true; then unset norace; fi\n",
		"norace=(./test/a)\n",
		"norace=(test/a/)\n",
		"norace=(/test/a)\n",
		"norace=(test//a)\n",
		"norace=(test/a.b)\n",
		"norace=(../a)\n",
	} {
		_, err := noRacePackages(bad)
		assert.Error(t, err, "must reject %q", bad)
	}
}

// TestNoRaceScanSeesConcurrency asserts the scan reports each construct it
// is meant to find and leaves alone the ones scripts/test allows.
func TestNoRaceScanSeesConcurrency(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"go statement", "func f() { go g() }", 1},
		{"channel type", "var c chan int", 1},
		{"make channel", "func f() { _ = make(chan struct{}) }", 1},
		{"send", "func f(c chan int) { c <- 1 }", 2},
		{"receive", "func f(c <-chan int) { <-c }", 2},
		{"select", "func f() { select {} }", 1},
		{"mutex", "import \"sync\"\nvar mu sync.Mutex", 1},
		{"wait group", "import \"sync\"\nvar wg sync.WaitGroup", 1},
		{"renamed import", "import s \"sync\"\nvar mu s.Mutex", 1},
		{"atomic import", "import \"sync/atomic\"\nvar n atomic.Int32", 1},
		{"errgroup import", "import \"golang.org/x/sync/errgroup\"\nvar g errgroup.Group", 1},
		{"once value", "import \"sync\"\nvar v = sync.OnceValue(func() int { return 1 })", 0},
		{"once values", "import \"sync\"\nvar v = sync.OnceValues(func() (int, error) { return 1, nil })", 0},
		{"dot import of sync", "import . \"sync\"\nvar mu Mutex", 1},
		{"dot import of sync once value", "import . \"sync\"\nvar v = OnceValue(func() int { return 1 })", 1},
		{"renamed atomic import", "import a \"sync/atomic\"\nvar n a.Int32", 1},
		{"dot atomic import", "import . \"sync/atomic\"\nvar n Int32", 1},
		{"time after func", "import \"time\"\nfunc f() { time.AfterFunc(0, nil) }", 1},
		{"renamed time after func", "import t \"time\"\nfunc f() { t.AfterFunc(0, nil) }", 1},
		{"dot time after func", "import . \"time\"\nfunc f() { AfterFunc(0, nil) }", 1},
		{"context after func", "import \"context\"\nfunc f(c context.Context) { context.AfterFunc(c, nil) }", 1},
		{"dot context after func", "import . \"context\"\nfunc f(c Context) { AfterFunc(c, nil) }", 1},
		{"time without a callback", "import \"time\"\nvar d = time.Second", 0},
		{"parallel test", "import \"testing\"\nfunc TestX(t *testing.T) { t.Parallel() }", 0},
		{"other package named sync-like selector", "var x = other.Mutex", 0},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "x.go", "package x\n"+c.src+"\n", parser.SkipObjectResolution)
		require.NoError(t, err, c.name)
		assert.Len(t, concurrencyUses(fset, file), c.want, c.name)
	}
}

// freshTimeoutRE matches the script's one assignment of the timeout the
// fresh pass runs with, in whole minutes.
var freshTimeoutRE = regexp.MustCompile(`(?m)^fresh_timeout=([0-9]+)m$`)

// freshTimeoutOf returns the minutes of the fresh timeout the script
// assigns.
func freshTimeoutOf(t *testing.T, script string) int {
	t.Helper()
	m := freshTimeoutRE.FindAllStringSubmatch(script, -1)
	require.Len(t, m, 1, "scripts/test assigns fresh_timeout=<minutes>m exactly once")
	minutes, err := strconv.Atoi(m[0][1])
	require.NoError(t, err)
	return minutes
}

// freshListRE matches the script's one list of packages that run with
// -count=1.
var freshListRE = regexp.MustCompile(`(?m)^fresh=\(([^)]*)\)$`)

// TestFreshPackagesRunUncached asserts that the script lists the source
// scans' package as one that always runs uncached, in one fresh list whose
// every entry is also in norace (the pass that runs it).
func TestFreshPackagesRunUncached(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	lists := freshListRE.FindAllStringSubmatch(string(script), -1)
	require.Len(t, lists, 1, "%s holds exactly one fresh=(...) list", noRaceScriptPath)
	fresh := strings.Fields(lists[0][1])
	assert.Contains(t, fresh, "test/conformance", "the source scans list files through git, which the test cache does not see")
	norace, err := noRacePackages(string(script))
	require.NoError(t, err)
	for _, dir := range fresh {
		assert.Contains(t, norace, dir, "%s is in fresh but not in norace", dir)
	}
}

// passCall is one go test call that the go stub logged. run is the value of
// a -run flag given as its own word, the form a shard uses.
type passCall struct {
	race, count1 bool
	flags, pkgs  []string
	run, timeout string
}

// passStubModule is the module path the go stub prints for go list -m.
const passStubModule = "example.com/m"

// passStubCLI is the import path of the one package a shard assigns test by
// test.
const passStubCLI = passStubModule + "/internal/cli"

// passStubNames is what the go stub prints for go test -list of the sharded
// package unless a test says otherwise: names in no order, then the summary
// line.
var passStubNames = []string{"TestD", "BenchmarkB", "TestA", "FuzzF", "TestC", "ExampleE", "TestB", "ok  \t" + passStubCLI + "\t0.012s"}

// passRun is the result of one run of the copied scripts/test.
type passRun struct {
	code      int
	stdout    string
	stderr    string
	goLog     []string
	calls     []passCall
	listCalls []string
	dir       string
}

// runPassScript runs the copied scripts/test with the default stub names.
func runPassScript(t *testing.T, packages []string, args ...string) passRun {
	t.Helper()
	return runPassScriptWith(t, packages, passStubNames, nil, args...)
}

// runPassScriptWith copies the real scripts/test into a temp directory, puts
// a go stub first on PATH and runs the copy with args. The stub logs every
// call; go list -m prints passStubModule, go list ./... prints packages, go
// test -list prints names (and fails when STUB_LIST_FAIL is set), and a go
// test call with -coverprofile writes a profile that names each package it
// was given.
func runPassScriptWith(t *testing.T, packages, names, extraEnv []string, args ...string) passRun {
	t.Helper()
	root := t.TempDir()
	copyRepositoryFile(t, noRaceScriptPath, root)
	stubs := filepath.Join(root, "stubs")
	logPath := filepath.Join(root, "go.log")
	listPath := filepath.Join(root, "packages.txt")
	namesPath := filepath.Join(root, "names.txt")
	writeTestFile(t, listPath, strings.Join(packages, "\n")+"\n", 0o644)
	writeTestFile(t, namesPath, strings.Join(names, "\n")+"\n", 0o644)
	writeTestFile(t, filepath.Join(stubs, "go"), `#!/bin/bash
printf 'go %s\n' "$*" >> "$STUB_LOG"
if [ "$1" = list ]; then
  if [ "$2" = -m ]; then
    echo "`+passStubModule+`"
  else
    cat "$STUB_PACKAGES"
  fi
  exit 0
fi
if [ "$1" = test ] && [ "$2" = -list ]; then
  if [ -n "${STUB_LIST_FAIL:-}" ]; then
    echo "list failed" >&2
    exit 1
  fi
  cat "$STUB_NAMES"
  exit 0
fi
for arg in "$@"; do
  case "$arg" in
    -coverprofile=*)
      file=${arg#*=}
      echo "mode: atomic" > "$file"
      for pkg in "$@"; do
        case "$pkg" in
          `+passStubModule+`*) echo "$pkg/file.go:1.1,2.1 1 1" >> "$file" ;;
        esac
      done
      ;;
  esac
done
`, 0o755)
	env := append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + logPath,
		"STUB_PACKAGES=" + listPath,
		"STUB_NAMES=" + namesPath,
	}, extraEnv...)
	cmd := exec.CommandContext(t.Context(), filepath.Join(root, noRaceScriptPath), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := exitCodeOf(t, cmd.Run())
	run := passRun{code: code, stdout: stdout.String(), stderr: stderr.String(), goLog: readLog(t, logPath), dir: root}
	for _, line := range run.goLog {
		if list, ok := strings.CutPrefix(line, "go test -list "); ok {
			run.listCalls = append(run.listCalls, list)
			continue
		}
		rest, ok := strings.CutPrefix(line, "go test ")
		if !ok {
			continue
		}
		var call passCall
		words := strings.Fields(rest)
		for i := 0; i < len(words); i++ {
			word := words[i]
			switch {
			case word == "-race":
				call.race = true
			case word == "-count=1":
				call.count1 = true
			case strings.HasPrefix(word, "-timeout="):
				call.timeout = strings.TrimPrefix(word, "-timeout=")
			case word == "-run" && i+1 < len(words):
				i++
				call.run = words[i]
			case strings.HasPrefix(word, passStubModule):
				call.pkgs = append(call.pkgs, word)
			default:
				call.flags = append(call.flags, word)
			}
		}
		run.calls = append(run.calls, call)
	}
	return run
}

// TestTestPassesSplitTheRun asserts that scripts/test with -pass=race makes
// one go test call with the race detector over every listed package except
// the norace ones (internal/cli included, with no -run and no -list call);
// with -pass=norace, two calls without it, the norace packages that are not
// fresh and then the fresh ones with -count=1 and the script's fresh
// timeout (only the fresh call has a -timeout, and a timeout in any spelling
// among the flags replaces it); with -pass=norace-cached only
// the first of those two and with -pass=fresh only the second; with no
// -pass, the race call followed by the two norace calls, so that every
// listed package appears in exactly one call; that -pass takes its value
// in the four spellings the flag package accepts; that every call carries
// exactly the flags the script was given (two flags; with none, the default
// -cover) and none carries -pass; and that an
// unknown, empty, missing or repeated -pass exits 2 before any go call. The
// norace and fresh lists are read from the script, so the stub package list
// has the real classes.
func TestTestPassesSplitTheRun(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	noRaceDirs, err := noRacePackages(string(script))
	require.NoError(t, err)
	freshLists := freshListRE.FindAllStringSubmatch(string(script), -1)
	require.Len(t, freshLists, 1)
	freshDirs := strings.Fields(freshLists[0][1])
	require.NotEmpty(t, freshDirs)
	freshTimeout := strconv.Itoa(freshTimeoutOf(t, string(script))) + "m"

	var listed, racePkgs, noRaceOnly, fresh []string
	for _, dir := range noRaceDirs {
		pkg := passStubModule + "/" + dir
		listed = append(listed, pkg)
		if slices.Contains(freshDirs, dir) {
			fresh = append(fresh, pkg)
		} else {
			noRaceOnly = append(noRaceOnly, pkg)
		}
	}
	for _, name := range []string{"", "/internal/a", "/internal/b", "/internal/cli"} {
		listed = append(listed, passStubModule+name)
		racePkgs = append(racePkgs, passStubModule+name)
	}
	require.NotEmpty(t, noRaceOnly, "the norace list holds a package that is not fresh")

	scriptFlags := []string{"-vet=off", "-run=TestX"}
	run := func(args ...string) passRun {
		t.Helper()
		return runPassScript(t, listed, append(slices.Clone(scriptFlags), args...)...)
	}
	assertCalls := func(name string, r passRun) {
		t.Helper()
		require.Zero(t, r.code, "%s: %s", name, r.stderr)
		for i, call := range r.calls {
			assert.Equal(t, scriptFlags, call.flags, "%s: call %d carries exactly the flags the script was given, and no -pass", name, i+1)
		}
	}
	for _, args := range [][]string{nil, {"-pass=race"}, {"-pass=norace"}} {
		r := runPassScript(t, listed, args...)
		require.Zero(t, r.code, "arguments %q: %s", args, r.stderr)
		require.NotEmpty(t, r.calls, "arguments %q", args)
		for i, call := range r.calls {
			assert.Equal(t, []string{"-cover"}, call.flags, "arguments %q: call %d of a run with no flags carries the default -cover", args, i+1)
		}
	}

	raceOnly := run("-pass=race")
	assertCalls("-pass=race", raceOnly)
	require.Len(t, raceOnly.calls, 1, "race mode makes exactly one go test call, got %q", raceOnly.goLog)
	assert.True(t, raceOnly.calls[0].race, "race mode runs with -race")
	assert.Equal(t, racePkgs, raceOnly.calls[0].pkgs, "race mode names every listed package except the norace ones")

	noRaceRun := run("-pass=norace")
	assertCalls("-pass=norace", noRaceRun)
	require.Len(t, noRaceRun.calls, 2, "norace mode makes exactly two go test calls, got %q", noRaceRun.goLog)
	for i, call := range noRaceRun.calls {
		assert.False(t, call.race, "norace call %d runs without -race", i+1)
	}
	assert.Equal(t, noRaceOnly, noRaceRun.calls[0].pkgs)
	assert.False(t, noRaceRun.calls[0].count1, "the norace packages that are not fresh may come from the test cache")
	assert.Equal(t, fresh, noRaceRun.calls[1].pkgs)
	assert.True(t, noRaceRun.calls[1].count1, "the fresh packages run with -count=1")
	assert.Empty(t, noRaceRun.calls[0].timeout, "the norace packages that are not fresh keep go test's default timeout")
	assert.Equal(t, freshTimeout, noRaceRun.calls[1].timeout, "the fresh packages run with the script's fresh timeout")

	for _, spelling := range [][]string{{"-pass", "race"}, {"-pass=race"}, {"--pass", "race"}, {"--pass=race"}} {
		r := run(spelling...)
		assertCalls(strings.Join(spelling, " "), r)
		require.Len(t, r.calls, 1, "%q makes the -pass=race call, got %q", spelling, r.goLog)
		assert.True(t, r.calls[0].race, "%q", spelling)
		assert.Equal(t, racePkgs, r.calls[0].pkgs, "%q", spelling)
	}

	cachedOnly := run("-pass=norace-cached")
	assertCalls("-pass=norace-cached", cachedOnly)
	require.Len(t, cachedOnly.calls, 1, "norace-cached mode makes exactly one go test call, got %q", cachedOnly.goLog)
	assert.False(t, cachedOnly.calls[0].race, "norace-cached mode runs without -race")
	assert.False(t, cachedOnly.calls[0].count1, "the norace packages that are not fresh may come from the test cache")
	assert.Equal(t, noRaceOnly, cachedOnly.calls[0].pkgs)

	freshOnly := run("-pass=fresh")
	assertCalls("-pass=fresh", freshOnly)
	require.Len(t, freshOnly.calls, 1, "fresh mode makes exactly one go test call, got %q", freshOnly.goLog)
	assert.False(t, freshOnly.calls[0].race, "fresh mode runs without -race")
	assert.True(t, freshOnly.calls[0].count1, "the fresh packages run with -count=1")
	assert.Equal(t, freshTimeout, freshOnly.calls[0].timeout, "the fresh packages run with the script's fresh timeout")
	assert.Equal(t, fresh, freshOnly.calls[0].pkgs)
	assert.Empty(t, raceOnly.calls[0].timeout, "race mode keeps go test's default timeout")
	assert.Empty(t, cachedOnly.calls[0].timeout, "norace-cached mode keeps go test's default timeout")

	for _, spelling := range [][]string{
		{"-timeout", "5m"}, {"--timeout", "5m"}, {"-timeout=5m"}, {"--timeout=5m"},
		{"-test.timeout", "5m"}, {"--test.timeout", "5m"}, {"-test.timeout=5m"}, {"--test.timeout=5m"},
	} {
		r := runPassScript(t, listed, append([]string{"-pass=fresh"}, spelling...)...)
		require.Zero(t, r.code, "%q: %s", spelling, r.stderr)
		require.Len(t, r.calls, 1, "%q", spelling)
		var timeoutWords []string
		for _, line := range r.goLog {
			if rest, ok := strings.CutPrefix(line, "go test "); ok {
				words := strings.Fields(rest)
				for i, word := range words {
					if strings.Contains(word, "timeout") {
						timeoutWords = append(timeoutWords, words[i:min(i+len(spelling), len(words))]...)
					}
				}
			}
		}
		assert.Equal(t, spelling, timeoutWords, "a timeout in the flags replaces the fresh timeout and reaches go test as given")
	}

	for _, r := range []passRun{raceOnly, noRaceRun, cachedOnly, freshOnly, run()} {
		assert.Empty(t, r.listCalls, "a run without -shard lists no tests")
		for _, call := range r.calls {
			assert.Empty(t, call.run, "a run without -shard sets no -run of its own")
		}
	}

	both := run()
	assertCalls("no -pass", both)
	assert.Equal(t, slices.Concat(raceOnly.calls, noRaceRun.calls), both.calls, "no -pass makes the race call, then the norace calls")
	counts := map[string]int{}
	for _, call := range both.calls {
		for _, pkg := range call.pkgs {
			counts[pkg]++
		}
	}
	for _, pkg := range listed {
		assert.Equal(t, 1, counts[pkg], "%s must appear in exactly one go test call", pkg)
	}

	for _, args := range [][]string{
		{"-pass=other"}, {"-pass", "other"}, {"-pass="}, {"-pass", ""}, {"--pass=Race"},
		{"-pass"}, {"--pass"},
		{"-pass=race", "-pass=race"}, {"-pass=race", "-pass=norace"}, {"-pass=norace", "--pass=race"},
		{"-pass", "race", "-pass=race"}, {"--pass", "norace", "--pass", "norace"},
		{"-pass=Fresh"}, {"-pass=norace-cache"}, {"-pass=fresh", "-pass=norace-cached"},
	} {
		r := run(args...)
		assert.Equal(t, 2, r.code, "arguments %q", args)
		assert.Empty(t, r.goLog, "arguments %q must make no go call", args)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), "arguments %q: want one line on stderr, got %q", args, r.stderr)
	}
}

// shardOf returns the items of the sorted list that belong to shard i (1, 2
// or 3): item k, counting from 0, goes to shard k mod 3 + 1.
func shardOf(items []string, i int) []string {
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	var mine []string
	for k, item := range sorted {
		if k%3+1 == i {
			mine = append(mine, item)
		}
	}
	return mine
}

// shardStubPackages returns the package list the go stub prints for the
// shard tests: the norace packages the script lists, the module root, five
// packages and the sharded one, in no order.
func shardStubPackages(t *testing.T) (listed, shardable []string) {
	t.Helper()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	noRaceDirs, err := noRacePackages(string(script))
	require.NoError(t, err)
	for _, dir := range noRaceDirs {
		listed = append(listed, passStubModule+"/"+dir)
	}
	for _, name := range []string{"/internal/e", "", "/internal/b", "/internal/a", "/internal/d", "/internal/c"} {
		listed = append(listed, passStubModule+name)
		shardable = append(shardable, passStubModule+name)
	}
	listed = append(listed, passStubCLI)
	return listed, shardable
}

// TestTestShardsSplitTheRacePass asserts that scripts/test with
// -pass=race and -shard=I/3 lists the tests of internal/cli once, with no
// script flag, and makes two go test calls with the race detector: the
// shard's packages (every race package except internal/cli, sorted bytewise
// and assigned by position, item k to shard k mod 3 + 1) and internal/cli
// with -run set to the shard's names (every name go test -list prints, tests,
// fuzz tests, examples and benchmarks, the summary line dropped, assigned the
// same way); that the call for internal/cli is skipped when the shard has no
// name; that no norace call is made; that the three shards together make each
// package and each name appear exactly once; that -shard takes its value in
// the four spellings; that every call carries the flags the script was given;
// and that a failing list, an empty list or a name that is not a Go
// identifier ends the run with a non-zero status before any go test call.
func TestTestShardsSplitTheRacePass(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	listed, shardable := shardStubPackages(t)
	names := []string{"BenchmarkB", "ExampleE", "FuzzF", "TestA", "TestB", "TestC", "TestD"}

	pattern := func(names []string) string { return "^(" + strings.Join(names, "|") + ")$" }
	pkgCounts := map[string]int{}
	nameCounts := map[string]int{}
	for i := 1; i <= 3; i++ {
		r := runPassScriptWith(t, listed, passStubNames, nil, "-vet=off", "-pass=race", "-shard="+strconv.Itoa(i)+"/3")
		require.Zero(t, r.code, "shard %d: %s", i, r.stderr)
		assert.Equal(t, []string{".* " + passStubCLI}, r.listCalls, "shard %d lists the tests of internal/cli once, with no script flag", i)
		require.Len(t, r.calls, 2, "shard %d makes two go test calls, got %q", i, r.goLog)
		for j, call := range r.calls {
			assert.True(t, call.race, "shard %d call %d runs with -race", i, j+1)
			assert.Equal(t, []string{"-vet=off"}, call.flags, "shard %d call %d carries the script's flags", i, j+1)
			assert.Empty(t, call.timeout, "shard %d call %d keeps go test's default timeout", i, j+1)
		}
		assert.Equal(t, shardOf(shardable, i), r.calls[0].pkgs, "shard %d packages", i)
		assert.Empty(t, r.calls[0].run, "the package call of shard %d sets no -run", i)
		assert.Equal(t, []string{passStubCLI}, r.calls[1].pkgs, "shard %d runs internal/cli in a call of its own", i)
		assert.Equal(t, pattern(shardOf(names, i)), r.calls[1].run, "shard %d names", i)
		for _, pkg := range r.calls[0].pkgs {
			pkgCounts[pkg]++
		}
		for name := range strings.SplitSeq(strings.Trim(r.calls[1].run, "^()$"), "|") {
			nameCounts[name]++
		}
	}
	for _, pkg := range shardable {
		assert.Equal(t, 1, pkgCounts[pkg], "%s is in exactly one shard", pkg)
	}
	assert.Len(t, pkgCounts, len(shardable), "no norace package and not internal/cli is in a package call")
	for _, name := range names {
		assert.Equal(t, 1, nameCounts[name], "%s is in exactly one shard", name)
	}
	assert.Len(t, nameCounts, len(names))

	for _, spelling := range [][]string{{"-shard=2/3"}, {"--shard=2/3"}, {"-shard", "2/3"}, {"--shard", "2/3"}} {
		r := runPassScriptWith(t, listed, passStubNames, nil, append([]string{"-pass=race"}, spelling...)...)
		require.Zero(t, r.code, "%q: %s", spelling, r.stderr)
		require.Len(t, r.calls, 2, "%q", spelling)
		assert.Equal(t, shardOf(shardable, 2), r.calls[0].pkgs, "%q", spelling)
		assert.Equal(t, pattern(shardOf(names, 2)), r.calls[1].run, "%q", spelling)
		assert.Equal(t, []string{"-cover"}, r.calls[0].flags, "%q: a run with no flags carries the default -cover", spelling)
	}

	few := runPassScriptWith(t, listed, []string{"TestA", "TestB"}, nil, "-pass=race", "-shard=3/3")
	require.Zero(t, few.code, few.stderr)
	require.Len(t, few.calls, 1, "a shard with no internal/cli name makes only the package call, got %q", few.goLog)
	assert.Equal(t, shardOf(shardable, 3), few.calls[0].pkgs)
	assert.Empty(t, few.calls[0].run)

	one := runPassScriptWith(t, listed, []string{"TestA"}, nil, "-pass=race", "-shard=1/3")
	require.Zero(t, one.code, one.stderr)
	require.Len(t, one.calls, 2)
	assert.Equal(t, "^(TestA)$", one.calls[1].run)

	for name, c := range map[string]struct {
		names []string
		env   []string
	}{
		"a failing list":                   {names: passStubNames, env: []string{"STUB_LIST_FAIL=1"}},
		"a list with only the summary":     {names: []string{"ok  \t" + passStubCLI + "\t0.1s"}},
		"a list with only a no-test line":  {names: []string{"?   \t" + passStubCLI + "\t[no test files]"}},
		"an empty list":                    {names: nil},
		"a name that is not an identifier": {names: []string{"TestA", "Test-B"}},
		"a name with a space":              {names: []string{"TestA", "Test B"}},
		"a name with a pipe":               {names: []string{"TestA", "TestB|TestC"}},
	} {
		r := runPassScriptWith(t, listed, c.names, c.env, "-pass=race", "-shard=1/3")
		assert.NotZero(t, r.code, "%s must fail the run", name)
		assert.Empty(t, r.calls, "%s: no go test call before the list is known to be right, got %q", name, r.goLog)
	}

	t.Run("coverage", func(t *testing.T) {
		t.Parallel()
		profile := filepath.Join(t.TempDir(), "cover.out")
		r := runPassScriptWith(t, listed, passStubNames, nil, "-pass=race", "-shard=1/3", "-coverprofile="+profile)
		require.Zero(t, r.code, r.stderr)
		require.Len(t, r.calls, 2)
		for i, call := range r.calls {
			assert.Contains(t, call.flags, "-covermode=atomic", "call %d", i+1)
		}
		data, err := os.ReadFile(profile)
		require.NoError(t, err)
		got := strings.Split(strings.TrimSpace(string(data)), "\n")
		assert.Equal(t, "mode: atomic", got[0])
		var profiled []string
		for _, line := range got[1:] {
			profiled = append(profiled, strings.TrimSuffix(strings.Fields(line)[0], "/file.go:1.1,2.1"))
		}
		want := append(shardOf(shardable, 1), passStubCLI)
		assert.ElementsMatch(t, want, profiled, "the merged profile holds the packages of both calls of the shard")
	})
}

// TestTestShardCheck asserts that scripts/test -shard-check lists the tests
// of internal/cli once, makes no go test call, prints one size line per shard
// and kind (six lines and the final line, no list of items) and succeeds when
// every race package and name is in exactly one shard; that a failing list,
// an empty list and a name that is not a Go identifier fail it; and that a
// name listed twice, a package listed twice and a package list without
// internal/cli reach the comparison and fail it, with the item named on
// stderr and the three lists printed there.
func TestTestShardCheck(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	listed, shardable := shardStubPackages(t)
	stubNames := []string{"BenchmarkB", "ExampleE", "FuzzF", "TestA", "TestB", "TestC", "TestD"}
	r := runPassScriptWith(t, listed, passStubNames, nil, "-shard-check")
	require.Zero(t, r.code, r.stderr)
	assert.Empty(t, r.calls, "the shard check runs no tests")
	assert.Equal(t, []string{".* " + passStubCLI}, r.listCalls)
	assert.Equal(t, []string{"go list -m", "go list ./...", "go test -list .* " + passStubCLI}, r.goLog)
	assert.Empty(t, r.stderr)
	out := strings.Split(strings.TrimSpace(r.stdout), "\n")
	require.Len(t, out, 7, "six size lines and the final line, got %q", r.stdout)
	for i, kind := range []string{"packages", "names"} {
		items := shardable
		if kind == "names" {
			items = stubNames
		}
		for j := 1; j <= 3; j++ {
			assert.Equal(t, "shard "+strconv.Itoa(j)+"/3: "+strconv.Itoa(len(shardOf(items, j)))+" "+kind, out[i*3+j-1])
		}
	}
	assert.True(t, strings.HasPrefix(out[6], "ok: "), out[6])

	for name, c := range map[string]struct {
		names []string
		env   []string
	}{
		"a failing list":                   {names: passStubNames, env: []string{"STUB_LIST_FAIL=1"}},
		"an empty list":                    {names: nil},
		"a name that is not an identifier": {names: []string{"TestA", "Test-B"}},
	} {
		r := runPassScriptWith(t, listed, c.names, c.env, "-shard-check")
		assert.NotZero(t, r.code, "%s must fail the check", name)
	}

	withoutCLI := slices.DeleteFunc(slices.Clone(listed), func(pkg string) bool { return pkg == passStubCLI })
	twice := passStubModule + "/internal/a"
	for name, c := range map[string]struct {
		packages, names []string
		wantStderr      string
	}{
		"a name listed twice":                 {listed, []string{"TestA", "TestA", "TestB"}, "repeated: TestA\n"},
		"a package listed twice":              {append(slices.Clone(listed), twice), passStubNames, "repeated: " + twice + "\n"},
		"a package list without internal/cli": {withoutCLI, passStubNames, "unexpected: " + passStubCLI + "\n"},
	} {
		r := runPassScriptWith(t, c.packages, c.names, nil, "-shard-check")
		assert.Equal(t, 1, r.code, "%s must fail the check", name)
		assert.Empty(t, r.calls, "%s: no tests run", name)
		assert.Contains(t, r.stderr, "error: the shards do not hold every one of the", name)
		assert.Contains(t, r.stderr, c.wantStderr, name)
		assert.Contains(t, r.stderr, "shard 1/3 ", "%s: the lists are printed on failure", name)
		assert.NotContains(t, r.stdout, "ok: ", name)
	}
}

// TestTestShardRefusals asserts that scripts/test refuses a malformed,
// repeated or misplaced -shard, -shard with any pass but race or with a
// -run of any spelling, and -shard-check with any other flag, each with one
// line on stderr, exit 2 and no go call.
func TestTestShardRefusals(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	listed, _ := shardStubPackages(t)
	for _, args := range [][]string{
		{"-pass=race", "-shard=0/3"}, {"-pass=race", "-shard=4/3"}, {"-pass=race", "-shard=1/2"},
		{"-pass=race", "-shard="}, {"-pass=race", "-shard", ""}, {"-pass=race", "-shard=a/3"},
		{"-pass=race", "-shard=1"}, {"-pass=race", "-shard=1/3/3"}, {"-pass=race", "-shard=01/3"},
		{"-pass=race", "-shard"}, {"-pass=race", "--shard"},
		{"-pass=race", "-shard=1/3", "-shard=1/3"}, {"-pass=race", "-shard=1/3", "--shard", "2/3"},
		{"-pass=race", "-shard", "1/3", "-shard", "1/3"},
		{"-shard=1/3"}, {"-shard", "1/3"},
		{"-pass=norace", "-shard=1/3"}, {"-pass=fresh", "-shard=1/3"}, {"-pass=norace-cached", "-shard=1/3"},
		{"-pass=race", "-shard=1/3", "-run"}, {"-pass=race", "-shard=1/3", "-run=TestX"},
		{"-pass=race", "-shard=1/3", "--run", "TestX"}, {"-pass=race", "-shard=1/3", "--run=TestX"},
		{"-pass=race", "-shard=1/3", "-test.run=TestX"}, {"-pass=race", "-shard=1/3", "--test.run", "TestX"},
		{"-pass=race", "-shard=1/3", "-test.run"}, {"-pass=race", "-shard=1/3", "--test.run=TestX"},
		{"-shard-check", "-pass=race"}, {"-shard-check", "-vet=off"}, {"-vet=off", "-shard-check"},
		{"-shard-check", "-shard=1/3"}, {"-shard-check", "-shard-check"}, {"--shard-check", "-coverprofile=x"},
	} {
		r := runPassScript(t, listed, args...)
		assert.Equal(t, 2, r.code, "arguments %q", args)
		assert.Empty(t, r.goLog, "arguments %q must make no go call", args)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), "arguments %q: want one line on stderr, got %q", args, r.stderr)
	}
}
