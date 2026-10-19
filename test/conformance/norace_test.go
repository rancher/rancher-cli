package conformance

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
