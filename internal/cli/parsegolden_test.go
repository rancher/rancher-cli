package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// goldenVersion is the version the golden roots are built with.
	goldenVersion = "v-test"

	// goldenConfigDir is the value the environment rows give to the variable
	// that --config reads. Nothing opens it: the stand-in actions touch no file.
	goldenConfigDir = "/parse/config"
)

// goldenPackageDir is the package directory, absolute because the tests
// change the working directory to a temporary one.
var goldenPackageDir = func() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return dir
}()

var (
	goldenHelpDir  = filepath.Join(goldenPackageDir, "testdata", "help")
	goldenParseDir = filepath.Join(goldenPackageDir, "testdata", "parse")
)

// goldenShown is the path of a golden file for a message: relative to the
// package directory, with / separators.
func goldenShown(path string) string {
	rel, err := filepath.Rel(goldenPackageDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// parseRow is a command line (the words after the program name) and what it
// did. The fields are in the order they have in a golden file. kind is one of
// runs, help, version, error, refused, nothing and complete; a row fills the
// fields its kind names (assertRowFields). Only a completion request has the
// kind complete.
type parseRow struct {
	// Line is the words after the program name.
	Line []string `json:"line"`
	// Env is the environment variables set for the row.
	Env map[string]string `json:"env,omitempty"`
	// Kind says what the line did.
	Kind string `json:"kind"`
	// Command is the words that name the command, joined by one space, with
	// no program name.
	Command string `json:"command"`
	// Args and Flags: the arguments of the command that runs, and the value of
	// each flag set on the line under the flag's first name: a bool, a string,
	// or the strings of a string slice flag in order.
	Args  []string       `json:"args,omitempty"`
	Flags map[string]any `json:"flags,omitempty"`
	// ErrorClass is the kind of error of an error row.
	ErrorClass string `json:"errorClass,omitempty"`
	// Exit is the exit code, written for every row.
	Exit int `json:"exit"`
	// Stdout and Stderr are the lines written.
	Stdout []string `json:"stdout,omitempty"`
	Stderr []string `json:"stderr,omitempty"`
	// HelpPath is the file under testdata/help that holds the help a help row
	// shows.
	HelpPath string `json:"helpPath,omitempty"`
}

// commandSnapshot is what a command that parsed a line holds: its arguments
// and the flags set on the line.
type commandSnapshot struct {
	command string
	args    []string
	flags   map[string]any
}

// observed is what the stand-ins of a tree saw during one run.
type observed struct {
	ran     *commandSnapshot
	refused *commandSnapshot
	// snapshotErr is the first error reading a command that parsed a line.
	snapshotErr error
}

// commandText returns the words that name c, below the root, joined by a
// space.
func commandText(c *Command) string {
	lineage := c.Lineage()
	words := make([]string, 0, len(lineage))
	for _, l := range lineage[:len(lineage)-1] {
		words = append(words, l.Name)
	}
	slices.Reverse(words)
	return strings.Join(words, " ")
}

// snapshotOf reads the command c that parsed a line: its arguments and, for
// each flag of c and of the commands above it that is set (typed, or read from
// its environment variable), its value under the first name of the flag.
func snapshotOf(c *Command, in *Input) (commandSnapshot, error) {
	snap := commandSnapshot{command: commandText(c), args: slices.Clone(in.Args())}
	seen := map[string]bool{}
	for _, l := range c.Lineage() {
		for _, f := range l.Flags {
			names := f.Names()
			if seen[names[0]] {
				continue
			}
			seen[names[0]] = true
			if !slices.ContainsFunc(names, func(name string) bool { return in.IsSet(name) || in.fromEnv(name) }) {
				continue
			}
			var value any
			switch f.Kind {
			case boolFlag:
				value = in.Bool(names[0])
			case stringFlag:
				value = in.String(names[0])
			case sliceFlag:
				value = slices.Clone(in.Strings(names[0]))
			default:
				return snap, fmt.Errorf("flag %s has the kind %d, which a parse row cannot hold", names[0], f.Kind)
			}
			if snap.flags == nil {
				snap.flags = map[string]any{}
			}
			snap.flags[names[0]] = value
		}
	}
	return snap, nil
}

func (o *observed) keepSnapshot(c *Command, in *Input) commandSnapshot {
	snap, err := snapshotOf(c, in)
	if err != nil && o.snapshotErr == nil {
		o.snapshotErr = err
	}
	return snap
}

// wrap makes the commands of cmd and below note what they do. The root
// Before does nothing and every leaf Action only reads the line: the config
// is not read and nothing touches the network, a file or a keychain. The
// Before of a leaf and the completion of every command are the CLI's own. The
// Action of a leaf the engine builds refuses a blank --config first
// (emptyConfigFlag); the leaves built by Adapt (adapted) do not, and neither
// does their stand-in: a row for one has the exit code of the real command.
func (o *observed) wrap(root, cmd *Command, adapted map[string]bool) error {
	switch {
	case cmd == root:
		cmd.Before = func(context.Context, *Command, *Input) error { return nil }
	case len(cmd.Commands) == 0:
		before := cmd.Before
		if before == nil {
			return fmt.Errorf("leaf %q has no Before", commandText(cmd))
		}
		cmd.Before = func(ctx context.Context, c *Command, in *Input) error {
			snap := o.keepSnapshot(c, in)
			err := before(ctx, c, in)
			if err != nil {
				o.refused = &snap
			}
			return err
		}
		cmd.Action = func(_ context.Context, c *Command, in *Input) error {
			snap := o.keepSnapshot(c, in)
			o.ran = &snap
			if adapted[snap.command] {
				return nil
			}
			if ce := emptyConfigFlag(in); ce != nil {
				streams := buildIO(c, in)
				runtime.WriteError(c.Root().ErrWriter, streams.Format, ce, errStyle(c, streams.Color), "")
				return exit(ce)
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands {
		if err := o.wrap(root, sub, adapted); err != nil {
			return err
		}
	}
	return nil
}

// builtTree is a new root with its commands wrapped.
type builtTree struct {
	root *Command
	obs  *observed
	err  error
}

// adaptedPaths returns the full path, the words joined by a space, of each
// leaf in fixed and below it. prefix is the path of the commands in fixed.
func adaptedPaths(fixed []*Command, prefix []string) map[string]bool {
	paths := map[string]bool{}
	for _, c := range fixed {
		path := append(slices.Clone(prefix), c.Name)
		if len(c.Commands) == 0 {
			paths[strings.Join(path, " ")] = true
			continue
		}
		maps.Copy(paths, adaptedPaths(c.Commands, path))
	}
	return paths
}

// adaptedLeaves returns the paths of the commands that Adapt wraps: the fixed
// commands of the root, which have no config directory to check.
var adaptedLeaves = sync.OnceValues(func() (map[string]bool, error) {
	wr, err := buildWiredRegistry(goldenVersion, config.ReadOnlyOverride)
	if err != nil {
		return nil, err
	}
	return adaptedPaths(wr.fixed, nil), nil
})

// TestAdaptedPathsUseTheFullPath asserts that a leaf Adapt wraps inside a
// group is named by its whole path; the stand-in action of the engine's leaf
// with the same last word is then not taken for it.
func TestAdaptedPathsUseTheFullPath(t *testing.T) {
	t.Parallel()
	leaf := func(name string) *Command { return &Command{Name: name} }
	fixed := []*Command{
		leaf("schema"),
		{Name: "tools", Commands: []*Command{leaf("lint"), {Name: "more", Commands: []*Command{leaf("deep")}}}},
	}
	assert.Equal(t, map[string]bool{"schema": true, "tools lint": true, "tools more deep": true}, adaptedPaths(fixed, nil))
	assert.False(t, adaptedPaths(fixed, nil)["lint"], "a leaf is not named by its last word alone")
}

// newWrappedTree builds a root and wraps it. It reads no environment
// variable; that is why building trees ahead of the runs, on other
// goroutines, is safe while a row sets a variable with t.Setenv. A build that
// read one would have to move into the run.
func newWrappedTree() builtTree {
	adapted, err := adaptedLeaves()
	if err != nil {
		return builtTree{err: err}
	}
	root, err := BuildRoot(goldenVersion)
	if err != nil {
		return builtTree{err: err}
	}
	obs := &observed{}
	return builtTree{root: root, obs: obs, err: obs.wrap(root, root, adapted)}
}

// buildTrees starts building n trees ahead of the runs that use them: ready[i]
// gets the tree for run i, in order, and release lets the builders go on to
// one more tree. At most twice the number of CPUs trees wait at once.
func buildTrees(ctx context.Context, n int) (ready []chan builtTree, release func()) {
	ready = make([]chan builtTree, n)
	for i := range ready {
		ready[i] = make(chan builtTree, 1)
	}
	workers := goruntime.GOMAXPROCS(0)
	window := make(chan struct{}, 2*workers)
	jobs := make(chan int)
	for range workers {
		go func() {
			for i := range jobs {
				ready[i] <- newWrappedTree()
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range n {
			select {
			case window <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ready, func() { <-window }
}

// unsetEnv removes the variable name for the rest of the test.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	require.NoError(t, os.Unsetenv(name))
}

// goldenEnvironment gives the test the environment every golden line runs in:
// a temporary home and working directory, and none of the RANCHER_CLI_* and
// color variables except the one that turns telemetry off. It returns the
// home and working directories, which nothing may write to. No line starts a
// process that could inherit a GIT_* variable.
func goldenEnvironment(t *testing.T) (home, work string) {
	t.Helper()
	restoreRawArgv(t)
	home, work = t.TempDir(), t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(name, home)
	}
	t.Chdir(work)
	for _, entry := range testfs.EnvWithoutGit() {
		name := entry
		start := min(1, len(entry))
		if i := strings.IndexByte(entry[start:], '='); i >= 0 {
			name = entry[:start+i]
		}
		upper := name
		if testfs.OnWindows() {
			upper = strings.ToUpper(name)
		}
		if strings.HasPrefix(upper, "RANCHER_CLI_") {
			unsetEnv(t, name)
		}
	}
	for _, name := range []string{"NO_COLOR", "CLICOLOR_FORCE", "FORCE_COLOR"} {
		unsetEnv(t, name)
	}
	t.Setenv(telemetry.EnvTelemetry, "false")
	return home, work
}

// assertUntouched asserts that nothing wrote to the directories the lines ran
// in.
func assertUntouched(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "a command line wrote to %s", dir)
	}
}

// goldenOutcome is a row and the output it was read from.
type goldenOutcome struct {
	row    parseRow
	stdout string
}

// goldenHelps holds the help of each command by file name: known has a name
// for each command that has a help file, text the help in the file.
type goldenHelps struct {
	known map[string]bool
	text  map[string]string
}

// goldenName is the stem of the files that hold the help and the lines of the
// command at path: its words joined by -, "rancher" for the root.
func goldenName(path []string) string {
	if len(path) == 0 {
		return "rancher"
	}
	return strings.Join(path, "-")
}

// textLines splits output written to a stream into its lines.
func textLines(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	body, ok := strings.CutSuffix(s, "\n")
	if !ok {
		return nil, fmt.Errorf("the output %q does not end in a newline", s)
	}
	return strings.Split(body, "\n"), nil
}

// runGoldenLines runs each row's line on a new root with the CLI's own entry point
// (run) and returns what each did. Trees are built ahead of the runs; the
// runs are one after the other, since they share the captured command line.
// helps is nil when the help of the lines is what is being recorded.
func runGoldenLines(t *testing.T, rows []parseRow, helps *goldenHelps) []goldenOutcome {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, release := buildTrees(ctx, len(rows))
	out := make([]goldenOutcome, 0, len(rows))
	for i, row := range rows {
		tree := <-ready[i]
		release()
		require.NoError(t, tree.err)
		out = append(out, runGoldenLine(t, tree, row, helps))
	}
	return out
}

// runGoldenLine runs row.Line on tree. A row with environment variables runs
// in a subtest that sets them and restores them when it ends.
func runGoldenLine(t *testing.T, tree builtTree, row parseRow, helps *goldenHelps) goldenOutcome {
	t.Helper()
	if len(row.Env) == 0 {
		return runGoldenLineHere(t, tree, row, helps)
	}
	var out goldenOutcome
	if !t.Run("env", func(t *testing.T) {
		for name, value := range row.Env {
			t.Setenv(name, value)
		}
		out = runGoldenLineHere(t, tree, row, helps)
	}) {
		t.FailNow()
	}
	return out
}

// runGoldenLineHere runs row.Line on tree in the environment of t.
func runGoldenLineHere(t *testing.T, tree builtTree, row parseRow, helps *goldenHelps) goldenOutcome {
	t.Helper()
	label := fmt.Sprintf("rancher %q", row.Line)
	if completionRequest(row.Line) {
		require.False(t, completionNetworkEnabled(), "%s: live completion is on", label)
	}

	var stdout, stderr bytes.Buffer
	tree.root.Writer, tree.root.ErrWriter = &stdout, &stderr
	tree.root.Reader = strings.NewReader("")
	res := run(t.Context(), tree.root, append([]string{"rancher"}, row.Line...))

	require.NoError(t, tree.obs.snapshotErr, label)
	require.NotContains(t, stderr.String(), goldenVersion, "%s: stderr holds the version of the build", label)
	for _, dir := range []string{os.Getenv("HOME"), mustGetwd(t)} {
		require.NotContains(t, stderr.String(), dir, "%s: stderr holds a temporary directory", label)
	}
	got, err := rowOf(row, tree.obs, res.outcome, res.code, stdout.String(), stderr.String(), helps)
	require.NoError(t, err, label)
	return goldenOutcome{row: got, stdout: stdout.String()}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	return dir
}

// rowOf reads what a run did into the row for its line, from the outcome of
// the run and what the stand-ins saw. The entry point refuses a line before
// it reads it for a command; a completion request is never refused first.
func rowOf(row parseRow, o *observed, oc outcome, code int, stdout, stderr string, helps *goldenHelps) (parseRow, error) {
	res := parseRow{Line: row.Line, Env: row.Env, Exit: code}
	var err error
	if res.Stdout, err = textLines(stdout); err != nil {
		return res, err
	}
	if res.Stderr, err = textLines(stderr); err != nil {
		return res, err
	}
	fill := func(snap commandSnapshot) {
		res.Command, res.Args, res.Flags = snap.command, snap.args, snap.flags
		if len(res.Args) == 0 {
			res.Args = nil
		}
	}
	switch oc.kind {
	case outcomeCompletion:
		if o.ran != nil || o.refused != nil {
			return res, fmt.Errorf("a completion request ran a command: %+v", o)
		}
		res.Kind, res.Command = "complete", commandText(oc.cmd)
	case outcomeRefused:
		if o.ran != nil || o.refused != nil {
			return res, errors.New("the entry point refused the line, but a command ran or reported on it")
		}
		res.Kind = "refused"
	case outcomeBeforeFailed:
		if o.refused == nil {
			return res, errors.New("a Before refused the line and no command Before did")
		}
		fill(*o.refused)
		res.Kind = "refused"
	case outcomeRan:
		if o.ran == nil {
			return res, errors.New("a command ran and its action was not the stand-in")
		}
		fill(*o.ran)
		res.Kind = "runs"
	case outcomeUsageError:
		class, ok := usageClassWords[oc.class]
		if !ok {
			return res, fmt.Errorf("the flag error class %d has no name", oc.class)
		}
		res.Kind, res.ErrorClass = "error", class
	case outcomeUnknownCommand:
		res.Kind, res.ErrorClass = "error", "unknown command"
	case outcomeVersion:
		res.Kind = "version"
	case outcomeHelp:
		res.Kind, res.Command = "help", commandText(oc.cmd)
		name := goldenName(strings.Fields(res.Command))
		// A card that is not the one in the help file of its command stays in
		// stdout, as the card of a command with no help file does.
		if helps != nil && helps.known[name] {
			if want, have := helps.text[name]; have && want == stdout {
				res.Stdout, res.HelpPath = nil, name+".txt"
			}
		}
	default:
		res.Kind = "nothing"
	}
	if res.Kind == "refused" && res.Command == "" && res.Exit != 2 {
		return res, fmt.Errorf("the entry point refused the line with exit %d, want 2", res.Exit)
	}
	return res, assertRowFields(res)
}

// usageClassWords names each class of flag error by the word an error row
// holds.
var usageClassWords = map[lineErrorClass]string{
	undefinedFlag:    "undefined flag",
	missingFlagValue: "missing value",
	repeatedFlag:     "repeated flag",
	badBoolValue:     "bad bool value",
}

// errorClasses are the kinds of error an error row names. A parse error
// outside them is not written into a golden file as the library's own text.
var errorClasses = []string{"undefined flag", "missing value", "repeated flag", "bad bool value", "unknown command"}

// assertRowFields checks that row fills the fields its kind names and no
// others: runs fills command, args and flags (and stderr when the stand-in
// action refused a blank --config); help fills command and helpPath, or the
// stdout for a command with no help file and for a card that is not the one in
// the help file of its command; version fills stdout; complete fills command
// (the command the completion reached, "" for the root) and the candidate lines
// in stdout, which may be none; error fills
// errorClass and stderr; refused fills stderr, and command, args and flags when
// the command's own Before refused; nothing fills what was written.
func assertRowFields(row parseRow) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("kind %s: "+format, append([]any{row.Kind}, args...)...)
	}
	switch row.Kind {
	case "runs":
		if row.Command == "" || row.ErrorClass != "" || len(row.Stdout) > 0 || row.HelpPath != "" {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	case "help":
		if row.ErrorClass != "" || len(row.Args) > 0 || len(row.Flags) > 0 || (row.HelpPath == "") == (len(row.Stdout) == 0) {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	case "version":
		if row.Command != "" || len(row.Stdout) == 0 || row.ErrorClass != "" || len(row.Args) > 0 || len(row.Flags) > 0 || row.HelpPath != "" {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	case "complete":
		if row.ErrorClass != "" || len(row.Args) > 0 || len(row.Flags) > 0 || row.HelpPath != "" {
			return bad("the row holds a field the kind does not use")
		}
	case "error":
		if row.ErrorClass != "" && !slices.Contains(errorClasses, row.ErrorClass) {
			return bad("the error class %q is not one of %v", row.ErrorClass, errorClasses)
		}
		if row.ErrorClass == "" || len(row.Stderr) == 0 || row.Command != "" || len(row.Args) > 0 || len(row.Flags) > 0 || len(row.Stdout) > 0 || row.HelpPath != "" {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	case "refused":
		if len(row.Stderr) == 0 || row.ErrorClass != "" || len(row.Stdout) > 0 || row.HelpPath != "" ||
			(row.Command == "" && (len(row.Args) > 0 || len(row.Flags) > 0)) {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	case "nothing":
		if row.Command != "" || row.ErrorClass != "" || len(row.Args) > 0 || len(row.Flags) > 0 || row.HelpPath != "" {
			return bad("the row holds a field the kind does not use, or lacks one it needs")
		}
	default:
		return fmt.Errorf("unknown kind %q", row.Kind)
	}
	return nil
}

// validUTF8 reports an error for a row that holds text that is not valid
// UTF-8.
func validUTF8(row parseRow) error {
	var texts []string
	texts = append(texts, row.Line...)
	for name, value := range row.Env {
		texts = append(texts, name, value)
	}
	texts = append(texts, row.Command, row.ErrorClass, row.HelpPath)
	texts = append(texts, row.Args...)
	for name, value := range row.Flags {
		texts = append(texts, name)
		switch v := value.(type) {
		case string:
			texts = append(texts, v)
		case []string:
			texts = append(texts, v...)
		}
	}
	texts = append(texts, row.Stdout...)
	texts = append(texts, row.Stderr...)
	for _, s := range texts {
		if !utf8.ValidString(s) {
			return fmt.Errorf("the text %q is not valid UTF-8", s)
		}
	}
	return nil
}

// asciiJSON rewrites the JSON text b so that every rune at or above U+007F is
// a \u escape, a pair for a rune above U+FFFF.
func asciiJSON(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] < 0x7f {
			out = append(out, b[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if r >= 0x10000 {
			r -= 0x10000
			out = fmt.Appendf(out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			continue
		}
		out = fmt.Appendf(out, `\u%04x`, r)
	}
	return out
}

// encodeRow writes row as one line of JSON text, ASCII only, with the newline.
func encodeRow(row parseRow) ([]byte, error) {
	if err := validUTF8(row); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(row); err != nil {
		return nil, err
	}
	return asciiJSON(buf.Bytes()), nil
}

// rowFields decodes the JSON text of a row into its fields as a generic
// value; rows compare by what they hold and not by how they are written.
func rowFields(line []byte) (any, error) {
	var v any
	err := json.Unmarshal(line, &v)
	return v, err
}

// decodeParseFile reads the rows of a parse golden file. A line ends with \n
// or \r\n.
func decodeParseFile(data []byte) (rows []parseRow, fields []any, err error) {
	number := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		number++
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		var row parseRow
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", number, err)
		}
		if dec.More() {
			return nil, nil, fmt.Errorf("line %d: more than one value", number)
		}
		f, err := rowFields(line)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", number, err)
		}
		rows, fields = append(rows, row), append(fields, f)
	}
	return rows, fields, nil
}

// parseSource says where a row of a parse file comes from.
type parseSource string

const (
	sourceGenerator  parseSource = "generator"
	sourceBaseline   parseSource = "baseline"
	sourceAlias      parseSource = "alias"
	sourceSliceFlag  parseSource = "slice flag"
	sourceExecPlugin parseSource = "exec plugin"
	sourceRefusal    parseSource = "refusal"
	sourceEnv        parseSource = "environment"
	sourceTwice      parseSource = "flag given twice"
	sourceCompletion parseSource = "__completion"
)

// goldenFile is a parse golden file: the lines typed for one command.
type goldenFile struct {
	name    string
	lines   []parseRow
	sources []parseSource
}

// goldenCommands lists the root and every command of the tree below it, hidden
// ones included, as the words that name each: the commands that have a help
// file.
func goldenCommands(t *testing.T) [][]string {
	t.Helper()
	root, err := BuildRoot(goldenVersion)
	require.NoError(t, err)
	paths := [][]string{nil}
	for _, c := range allCommands(root, nil) {
		paths = append(paths, c.path)
	}
	return paths
}

// fakeCertificate is certificate text with no key material: a header, one
// base64 line and a footer, with newlines. The exec plugin entry of a
// kubeconfig passes a whole PEM as one argument.
const fakeCertificate = "-----BEGIN CERTIFICATE-----\nZmFrZSBjZXJ0aWZpY2F0ZQ==\n-----END CERTIFICATE-----\n"

// parseFiles lists the lines of every parse golden file: every line the
// generator of the parse test makes for each command, then the four lines of
// every command (the command alone, --help, one extra word, an undefined
// flag), the aliases, the first list flag of one command, the exec plugin and
// refusal lines and the environment lines of the command, the lines that give
// flags twice, the lines that give every list flag a comma value and a
// repeated value, then the lines of __completion, which is not in the tree
// until the root runs.
func parseFiles(t *testing.T) []goldenFile {
	t.Helper()
	root, err := BuildRoot(goldenVersion)
	require.NoError(t, err)
	commands := allCommands(root, nil)
	targets, withSlice, _ := parseTargets(t, root)

	byName := map[string]*goldenFile{}
	var files []*goldenFile
	seen := map[string]bool{}
	file := func(path []string) *goldenFile {
		name := goldenName(path)
		if byName[name] == nil {
			byName[name] = &goldenFile{name: name}
			files = append(files, byName[name])
		}
		return byName[name]
	}
	add := func(path []string, src parseSource, env map[string]string, line []string) {
		f := file(path)
		key := f.name + "\x01" + strings.Join(line, "\x00")
		for _, name := range slices.Sorted(maps.Keys(env)) {
			key += "\x02" + name + "=" + env[name]
		}
		if seen[key] {
			return
		}
		seen[key] = true
		f.lines = append(f.lines, parseRow{Line: slices.Clone(line), Env: env})
		f.sources = append(f.sources, src)
	}

	for _, tg := range targets {
		cmd := root
		for _, name := range tg.path {
			cmd = cmd.Command(name)
		}
		file(tg.path)
		for _, c := range casesFor(tg.path, cmd, root.Flags, tg.tier) {
			add(tg.path, sourceGenerator, nil, c.line)
		}
	}

	// Every command has the same four lines; a command the generator makes no
	// line for (a leaf with no flag of its own) is recorded too.
	for _, tg := range targets {
		for _, words := range [][]string{{}, {"--help"}, {"x"}, {"--bogus"}} {
			add(tg.path, sourceBaseline, nil, append(slices.Clone(tg.path), words...))
		}
	}

	for _, c := range commands {
		if len(c.cmd.Aliases) == 0 {
			continue
		}
		parent := c.path[:len(c.path)-1]
		for _, alias := range c.cmd.Aliases {
			add(c.path, sourceAlias, nil, append(slices.Clone(parent), alias))
		}
		add(c.path, sourceAlias, nil, append(slices.Clone(parent), c.cmd.Aliases[0], "--help"))
	}

	sliceLeaf := root
	for word := range strings.SplitSeq(withSlice, " ") {
		sliceLeaf = sliceLeaf.Command(word)
	}
	slicePath := strings.Fields(withSlice)
	for _, f := range sliceLeaf.Flags {
		if f.Kind != sliceFlag {
			continue
		}
		flag := "--" + f.Names()[0]
		for _, words := range [][]string{
			{flag, "a,b"},
			{flag, "a,b", flag, "c,d"},
			{flag, "a", flag, "b"},
			{flag + "=a b"},
		} {
			add(slicePath, sourceSliceFlag, nil, append(slices.Clone(slicePath), words...))
		}
		break
	}

	getToken := []string{"auth", "get-token"}
	for _, userID := range []bool{false, true} {
		for _, provider := range []bool{false, true} {
			for _, cacert := range []bool{false, true} {
				line := append(slices.Clone(getToken), "--server", "https://rancher.example.test", "--cluster", "c-m-abcde", "--name", "ex")
				if userID {
					line = append(line, "--user-id", "u-abcde")
				}
				if provider {
					line = append(line, "--auth-provider", "keycloakoidc")
				}
				if cacert {
					line = append(line, "--cacert", fakeCertificate)
				}
				add(getToken, sourceExecPlugin, nil, line)
			}
		}
	}
	for _, joined := range []bool{true, false} {
		for _, cluster := range []bool{false, true} {
			for _, cacerts := range []bool{false, true} {
				line := []string{"token"}
				value := func(name, v string) {
					if joined {
						line = append(line, "--"+name+"="+v)
						return
					}
					line = append(line, "--"+name, v)
				}
				value("server", "https://rancher.example.test")
				value("user", "user-abcde")
				if cluster {
					value("cluster", "c-m-abcde")
				}
				if cacerts {
					value("cacerts", "/parse/ca.pem")
				}
				add([]string{"token"}, sourceExecPlugin, nil, line)
			}
		}
	}

	getPod := []string{"get", "pod"}
	for _, words := range [][]string{
		{"x", "--help"},
		{"--yes="},
		{"--yes", "false"},
		{"x", "-", "y"},
	} {
		add(getPod, sourceRefusal, nil, append(slices.Clone(getPod), words...))
	}
	for _, path := range [][]string{getPod, {"schema"}, {"completion"}} {
		add(path, sourceRefusal, nil, append(slices.Clone(path), "-o", "--yes"))
	}

	configDir := map[string]string{"RANCHER_CLI_CONFIG_DIR": ""}
	for _, value := range []string{"", " "} {
		for _, words := range [][]string{{"--config=" + value}, {"--config", value}} {
			line := append(slices.Clone(getPod), words...)
			add(getPod, sourceEnv, nil, line)
			add(getPod, sourceEnv, configDir, line)
			if value != "" {
				add(getPod, sourceEnv, map[string]string{"RANCHER_CLI_CONFIG_DIR": value}, line)
			}
			add(getPod, sourceEnv, map[string]string{"RANCHER_CLI_CONFIG_DIR": goldenConfigDir}, line)
		}
	}
	for _, value := range []string{"", " "} {
		add(getPod, sourceEnv, map[string]string{"RANCHER_CLI_CONFIG_DIR": value}, getPod)
	}
	for _, path := range [][]string{{"schema"}, {"completion"}} {
		for _, words := range [][]string{{"--config="}, {"--config", ""}} {
			add(path, sourceEnv, nil, append(slices.Clone(path), words...))
		}
	}
	bound := map[string]string{"RANCHER_CLI_CONFIG_DIR": goldenConfigDir}
	add(nil, sourceEnv, bound, []string{})
	add(nil, sourceEnv, bound, []string{"--version"})
	add([]string{"get"}, sourceEnv, bound, []string{"get"})
	for _, words := range [][]string{{}, {"--config", "/parse/other"}, {"-c", "/parse/other"}, {"--help"}} {
		add(getPod, sourceEnv, bound, append(slices.Clone(getPod), words...))
	}

	createProject := []string{"create", "project"}
	for _, path := range [][]string{getPod, createProject, {"create", "user"}} {
		cmd := root
		for _, name := range path {
			cmd = cmd.Command(name)
		}
		twice, missing := flagTwiceWords(cmd)
		for _, rule := range missing {
			t.Logf("rancher %s has no %s to give twice", strings.Join(path, " "), rule)
		}
		for _, words := range twice {
			add(path, sourceTwice, nil, append(slices.Clone(path), words...))
		}
	}
	for _, words := range [][]string{
		{"-o", "json", "-o", "yaml"},
		{"--output", "json", "--output=yaml"},
		{"--no-input", "--no-input=false"},
	} {
		add(getPod, sourceTwice, nil, append(slices.Clone(getPod), words...))
	}
	add(getPod, sourceTwice, nil, []string{"-o", "json", "-o", "yaml", "get", "pod"})
	add(createProject, sourceTwice, nil, append(slices.Clone(createProject), "--yes", "--yes=false"))

	sliceRows := func(path []string, cmd *Command) {
		for _, f := range cmd.Flags {
			if f.Kind != sliceFlag {
				continue
			}
			names := f.Names()
			long := "--" + names[0]
			words := [][]string{
				{long, "a,b"},
				{long, "a,b", long, "c"},
				{long + "=a", long, "b"},
			}
			if len(names) > 1 {
				words = append(words, []string{"-" + names[len(names)-1], "a,b", long, "c"})
			}
			for _, w := range words {
				add(path, sourceSliceFlag, nil, append(slices.Clone(path), w...))
			}
		}
	}
	sliceRows(nil, root)
	for _, c := range commands {
		sliceRows(c.path, c.cmd)
	}

	completion := []string{"__completion"}
	for _, words := range [][]string{{}, {"--help"}, {"x"}, {"--bogus"}} {
		add(completion, sourceCompletion, nil, append(slices.Clone(completion), words...))
	}

	out := make([]goldenFile, len(files))
	for i, f := range files {
		out[i] = *f
	}
	return out
}

// checkGoldenNames fails for a file name that two names share when case is
// ignored, or that Windows refuses.
func checkGoldenNames(names []string) error {
	reserved := []string{"con", "prn", "aux", "nul"}
	for i := range 9 {
		reserved = append(reserved, "com"+strconv.Itoa(i+1), "lpt"+strconv.Itoa(i+1))
	}
	lower := map[string]string{}
	for _, name := range names {
		if strings.ContainsAny(name, `:*?"<>|\/`) || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 }) >= 0 {
			return fmt.Errorf("the file name %q has a character Windows refuses", name)
		}
		if slices.Contains(reserved, strings.ToLower(name)) {
			return fmt.Errorf("the file name %q is a reserved name on Windows", name)
		}
		if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
			return fmt.Errorf("the file name %q ends in a dot or a space", name)
		}
		if other, taken := lower[strings.ToLower(name)]; taken {
			return fmt.Errorf("the file names %q and %q differ only in case", other, name)
		}
		lower[strings.ToLower(name)] = name
	}
	return nil
}

// goldenDirFiles returns the names of the files in dir, or none when it does
// not exist.
func goldenDirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// writeGoldens makes the files of dir hold exactly want (file name to
// content): it writes the files whose content differs, removes the others and
// logs each change.
func writeGoldens(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range slices.Sorted(maps.Keys(want)) {
		path := filepath.Join(dir, name)
		have, err := os.ReadFile(path)
		if err == nil && bytes.Equal(have, want[name]) {
			continue
		}
		require.NoError(t, os.WriteFile(path, want[name], 0o644))
		t.Logf("wrote %s", goldenShown(path))
	}
	for _, name := range goldenDirFiles(t, dir) {
		if _, kept := want[name]; kept {
			continue
		}
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
		t.Logf("removed %s", goldenShown(filepath.Join(dir, name)))
	}
}

// recordParse runs every line of file and returns the rows.
func recordParse(t *testing.T, file goldenFile, helps *goldenHelps) []parseRow {
	t.Helper()
	outcomes := runGoldenLines(t, file.lines, helps)
	rows := make([]parseRow, len(outcomes))
	for i, o := range outcomes {
		rows[i] = o.row
	}
	return rows
}

// encodeRows writes rows as the content of a parse golden file.
func encodeRows(rows []parseRow) ([]byte, error) {
	var out []byte
	for _, row := range rows {
		line, err := encodeRow(row)
		if err != nil {
			return nil, fmt.Errorf("rancher %q: %w", row.Line, err)
		}
		out = append(out, line...)
	}
	return out, nil
}

// helpsFromDisk reads the help files of commands into helps; a command whose
// file is missing is known with no text.
func helpsFromDisk(t *testing.T, commands [][]string) *goldenHelps {
	t.Helper()
	helps := &goldenHelps{known: map[string]bool{}, text: map[string]string{}}
	for _, path := range commands {
		name := goldenName(path)
		helps.known[name] = true
		data, err := os.ReadFile(filepath.Join(goldenHelpDir, name+".txt"))
		if err == nil {
			helps.text[name] = strings.ReplaceAll(string(data), "\r\n", "\n")
		}
	}
	return helps
}

// TestParseGolden asserts that every command line of the parse test, the four
// lines of every command (the command alone, --help, one extra word, an
// undefined flag), the aliases of the commands, the string slice flag, the
// exec plugin and refusal lines and the environment lines run through Run on
// a tree with stand-in actions do what testdata/parse records: one file for
// each command and for __completion, each line of a file one JSON row.
// Rewrite the files with -update after a change to the command surface and
// review the diff. Not parallel: it sets the process environment and runs
// lines one after the other, since they share the captured command line.
func TestParseGolden(t *testing.T) {
	home, work := goldenEnvironment(t)
	files := parseFiles(t)
	commands := goldenCommands(t)
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
	}
	require.NoError(t, checkGoldenNames(names))
	counts := map[string]int{}
	sources := map[parseSource]int{}
	total := 0
	for _, f := range files {
		total += len(f.lines)
		for _, s := range f.sources {
			sources[s]++
		}
	}
	t.Logf("%d lines in %d files; by source %v", total, len(files), sources)

	if *update {
		helps := &goldenHelps{known: map[string]bool{}, text: recordHelp(t, commands)}
		for _, path := range commands {
			helps.known[goldenName(path)] = true
		}
		want := map[string][]byte{}
		for _, f := range files {
			first, second := recordParse(t, f, helps), recordParse(t, f, helps)
			require.NotEmpty(t, first, "no line is run for %s", f.name)
			for i := range first {
				if !reflect.DeepEqual(first[i], second[i]) {
					require.Failf(t, "two recordings differ", "file %s, rancher %q:\n%+v\n%+v", f.name, first[i].Line, first[i], second[i])
				}
				counts[first[i].Kind]++
			}
			data, err := encodeRows(first)
			require.NoError(t, err, f.name)
			want[f.name+".jsonl"] = data
		}
		writeGoldens(t, goldenParseDir, want)
		t.Logf("rows by kind %v", counts)
		assertUntouched(t, home, work)
		return
	}

	helps := helpsFromDisk(t, commands)
	expected := map[string]bool{}
	for _, f := range files {
		expected[f.name+".jsonl"] = true
	}
	for _, name := range goldenDirFiles(t, goldenParseDir) {
		assert.True(t, expected[name], "%s has no command: remove it or run with -update", goldenShown(filepath.Join(goldenParseDir, name)))
	}
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			path := filepath.Join(goldenParseDir, f.name+".jsonl")
			data, err := os.ReadFile(path)
			require.NoError(t, err, "no parse file for %s: run with -update", f.name)
			for i, b := range data {
				require.Less(t, b, byte(0x80), "%s: byte %d is not ASCII", goldenShown(path), i)
			}
			want, wantFields, err := decodeParseFile(data)
			require.NoError(t, err, goldenShown(path))
			require.NotEmpty(t, want, "%s holds no row: every command has rows; run with -update", goldenShown(path))
			for i, row := range want {
				require.NotEqual(t, "complete", row.Kind, "%s row %d: a parse file holds no completion row", goldenShown(path), i+1)
			}
			got := recordParse(t, f, helps)
			failures := 0
			for i, row := range got {
				counts[row.Kind]++
				if i >= len(want) {
					break
				}
				gotLine, err := encodeRow(row)
				require.NoError(t, err)
				gotFields, err := rowFields(gotLine)
				require.NoError(t, err)
				if reflect.DeepEqual(wantFields[i], gotFields) {
					continue
				}
				// The first rows that differ show the difference, the rest
				// only their line.
				if failures++; failures <= 5 {
					assert.Equal(t, wantFields[i], gotFields, "%s row %d, rancher %q%s differs; run with -update if intended", goldenShown(path), i+1, row.Line, envText(row))
				} else {
					t.Errorf("%s row %d, rancher %q%s differs; run with -update if intended", goldenShown(path), i+1, row.Line, envText(row))
				}
			}
			assert.Equal(t, len(want), len(got), "%s: the number of rows differs; run with -update if intended", goldenShown(path))
		})
	}
	assertUntouched(t, home, work)
}

// envText names the environment of a row for a failure message.
func envText(row parseRow) string {
	if len(row.Env) == 0 {
		return ""
	}
	return fmt.Sprintf(" with %v", row.Env)
}
