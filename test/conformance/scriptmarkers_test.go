package conformance

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scriptDir         = "test/e2e/scripts"
	scriptHarnessFile = "test/e2e/harness_test.go"

	// scriptMinRead and scriptMinWithMarker keep the scan from passing on
	// nothing: it reads at least this many scripts, and at least this many
	// of them hold a file marker. Raise them when scripts are added in
	// bulk; lower them only when scripts are removed on purpose.
	scriptMinRead       = 77
	scriptMinWithMarker = 53
)

// scriptBuiltinCommands are the commands testscript runs without any
// registration: the table scriptCmds in testscript/cmd.go at the module
// version go.mod names. A testscript upgrade that adds a command needs a new
// entry here.
var scriptBuiltinCommands = []string{
	"cd", "chmod", "cmp", "cmpenv", "cp", "env", "exec", "exists", "grep",
	"kill", "mkdir", "mv", "rm", "skip", "stderr", "stdin", "stdout", "ttyin",
	"ttyout", "stop", "symlink", "unix2dos", "unquote", "wait",
}

// scriptHarnessCommands reads the names of the commands the e2e harness
// registers: the string keys of the Cmds field of the testscript.Params
// literal in TestScenarios. The command set of a script is these names plus
// scriptBuiltinCommands. Two more tables can name commands: one a test adds
// with TestScript.SetCmd, and the bare commands testscript.Main and RunMain
// register; nothing in this repository uses either today, and a use needs
// its names added to the set.
var scriptHarnessCommands = sync.OnceValues(func() ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), scriptHarnessFile, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var keys []string
	found, bad := false, false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestScenarios" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Params" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if name, ok := kv.Key.(*ast.Ident); !ok || name.Name != "Cmds" {
					continue
				}
				cmds, ok := kv.Value.(*ast.CompositeLit)
				if !ok {
					continue
				}
				found = true
				for _, e := range cmds.Elts {
					entry, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := entry.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						bad = true
						continue
					}
					name, err := strconv.Unquote(key.Value)
					if err != nil {
						bad = true
						continue
					}
					keys = append(keys, name)
				}
			}
			return false
		})
	}
	if !found {
		return nil, errors.New("no Cmds field in the testscript.Params literal of TestScenarios in " + scriptHarnessFile)
	}
	if bad {
		return nil, errors.New("a Cmds key in " + scriptHarnessFile + " is not a string literal; the command set cannot be read")
	}
	return keys, nil
})

// scriptCommandSet is every command name a script may call: the builtins and
// the ones the harness registers.
func scriptCommandSet(t *testing.T) []string {
	t.Helper()
	custom, err := scriptHarnessCommands()
	require.NoError(t, err)
	return append(slices.Clone(scriptBuiltinCommands), custom...)
}

// scriptCommandHits reports every line the archive holds after its first
// file marker whose first word, after any [condition] words and one "!", is
// a command name. testscript runs only the text before the first marker and
// treats the rest as the content of files, so such a line never runs. Each
// hit reads <name>:<line>: <trimmed line>, with the line number counted in
// the whole archive.
//
// The rule leaves out, on purpose: a "#" line (content of a YAML or shell
// fixture); a command word inside quoted content; and a line whose first
// word is not in the command set, such as a misspelled or a future command,
// which cannot be told from fixture content.
func scriptCommandHits(name string, data []byte, commands []string) []string {
	archive := txtar.Parse(data)
	line := strings.Count(string(archive.Comment), "\n")
	var hits []string
	for _, file := range archive.Files {
		line++
		for text := range strings.Lines(string(file.Data)) {
			line++
			trimmed := strings.TrimSpace(text)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if scriptCallsCommand(trimmed, commands) {
				hits = append(hits, name+":"+strconv.Itoa(line)+": "+trimmed)
			}
		}
	}
	return hits
}

// scriptCallsCommand reports whether a trimmed line reads as a testscript
// command: [condition] words, then an optional "!", then a command name.
func scriptCallsCommand(trimmed string, commands []string) bool {
	fields := strings.Fields(trimmed)
	for len(fields) > 0 && strings.HasPrefix(fields[0], "[") && strings.HasSuffix(fields[0], "]") {
		fields = fields[1:]
	}
	if len(fields) > 0 && fields[0] == "!" {
		fields = fields[1:]
	}
	return len(fields) > 0 && slices.Contains(commands, fields[0])
}

// TestScriptsHoldNoCommandAfterAFileMarker asserts that no script under
// test/e2e/scripts holds a command line after its first file marker: the
// archive reader of testscript takes everything after the marker as file
// content, so a command placed there is never run.
func TestScriptsHoldNoCommandAfterAFileMarker(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	commands := scriptCommandSet(t)
	read, withMarker := 0, 0
	var hits []string
	for _, file := range files {
		if ext := path.Ext(file); path.Dir(file) != scriptDir || (ext != ".txtar" && ext != ".txt") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		read++
		if len(txtar.Parse(data).Files) > 0 {
			withMarker++
		}
		hits = append(hits, scriptCommandHits(file, data, commands)...)
	}
	assert.GreaterOrEqual(t, read, scriptMinRead, "scripts read in %s", scriptDir)
	assert.GreaterOrEqual(t, withMarker, scriptMinWithMarker, "scripts with a file marker in %s", scriptDir)
	for _, hit := range hits {
		t.Error(hit + "; testscript reads a line after the first file marker as file content and never runs it, so move it above the marker")
	}
}

// TestScriptHarnessCommandsAreRead asserts the reader finds the commands the
// harness registers.
func TestScriptHarnessCommandsAreRead(t *testing.T) {
	t.Parallel()
	custom, err := scriptHarnessCommands()
	require.NoError(t, err)
	assert.Contains(t, custom, "jsonquery")
	assert.Contains(t, custom, "setenvfrom")
}

// TestScriptCommandHits asserts the rule over inline archives.
func TestScriptCommandHits(t *testing.T) {
	t.Parallel()
	commands := scriptCommandSet(t)
	archive := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
	rows := []struct {
		name string
		data []byte
		want []string
	}{
		{"command after the marker", archive("exec rancher a", "-- f.txt --", "exec rancher b"), []string{"s.txtar:3: exec rancher b"}},
		{"negated command", archive("exec rancher a", "-- f.txt --", "data", "! stdout x"), []string{"s.txtar:4: ! stdout x"}},
		{"condition", archive("-- f.txt --", "[kubectl] exec kubectl version"), []string{"s.txtar:2: [kubectl] exec kubectl version"}},
		{"negated condition", archive("-- f.txt --", "[!unix] skip"), []string{"s.txtar:2: [!unix] skip"}},
		{"conditions and negation", archive("-- f.txt --", "[unix] [kubectl] ! exec kubectl"), []string{"s.txtar:2: [unix] [kubectl] ! exec kubectl"}},
		{"harness command", archive("-- f.txt --", "jsonquery -yaml kc.yaml a b"), []string{"s.txtar:2: jsonquery -yaml kc.yaml a b"}},
		{"indented command", archive("a", "b", "-- f.txt --", "  \texec rancher x"), []string{"s.txtar:4: exec rancher x"}},
		{"command after a second marker", archive("exec a", "-- f.txt --", "data", "-- g.txt --", "stop"), []string{"s.txtar:5: stop"}},
		{"line count across files", archive("c1", "c2", "-- f.txt --", "d1", "d2", "-- g.txt --", "d3", "cp a b"), []string{"s.txtar:8: cp a b"}},
		{"comment line", archive("-- f.txt --", "# exec rancher x"), nil},
		{"blank line", archive("-- f.txt --", "", "   "), nil},
		{"json content", archive("-- f.json --", `{"exec": "x"}`, `  "env": 1`), nil},
		{"yaml content", archive("-- f.yaml --", "exec: x", "env:", "  - cp", "- stdout"), nil},
		{"word that starts like a command", archive("-- f.txt --", "executable x", "env: y", "stdoutput"), nil},
		{"no marker", archive("exec rancher a", "stdout x"), nil},
		{"commands before the marker", archive("exec rancher a", "! stdout x", "jsonquery a b c", "-- f.txt --", "data"), nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, row.want, scriptCommandHits("s.txtar", row.data, commands))
		})
	}
}
