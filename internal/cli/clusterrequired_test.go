package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

const (
	// sharedClusterHint is the hint every refusal of an absent --cluster
	// carries, with the config directory left out.
	sharedClusterHint = "rancher access clusters"

	// getTokenClusterMessage is the refusal of auth get-token with neither
	// --cluster nor --user-id, for a call that typed no --server.
	getTokenClusterMessage = "--cluster is required; pass a cluster id or name, or --user-id for a token for Rancher itself"
)

// getTokenServerClause is what auth get-token adds to its message when a
// --server was typed: what the hint lists, and the server as typed in its
// display form.
func getTokenServerClause(shown string) string {
	return "; the command in the hint lists the clusters of the current server, and --server (" + shown + ") does not change that"
}

// clusterOptionalRow classifies one command that declares an optional
// --cluster. Exactly one of message and allow is set.
//
// A row with a message names the arguments that make the command refuse an
// absent --cluster, and the exact refusal text. A row with allow says why the
// command never refuses it; probe, when present, is a command line the test
// runs to check that.
type clusterOptionalRow struct {
	args    []string
	message string
	allow   string
	probe   []string
}

// clusterOptionalRows has a row for every registered command that declares
// an optional --cluster. A command added later has none and fails the test
// until it is classified.
var clusterOptionalRows = map[string]clusterOptionalRow{
	"images": {
		args:    []string{"images"},
		message: "--cluster or --all-clusters is required; pass --cluster <id|name> for one cluster, or --all-clusters for every cluster you can list",
	},
	"logs": {
		args:    []string{"logs", "agent"},
		message: "--cluster is required; agent runs only on a downstream cluster, pass a downstream cluster id or name",
	},
	"get clusterroletemplatebinding": {
		args:    []string{"get", "clusterroletemplatebinding", "x"},
		message: "--cluster is required when you give the name of one clusterroletemplatebinding",
	},
	"get project": {
		args:    []string{"get", "project", "x"},
		message: "--cluster is required when you give the id of one project",
	},
	"auth get-token": {
		args:    []string{"auth", "get-token"},
		message: getTokenClusterMessage,
	},
	"access projects": {
		allow: "lists the projects of every cluster when --cluster is absent",
		probe: []string{"access", "projects"},
	},
	"create resource": {
		allow: "the cluster comes from the manifest, not from --cluster",
	},
	"get projectroletemplatebinding": {
		allow: "--cluster goes with --project: the command refuses either one without the other, and takes --namespace when neither is given",
	},
	"patch projectroletemplatebinding": {
		allow: "--cluster goes with --project: the command refuses either one without the other, and takes --namespace when neither is given",
	},
	"delete projectroletemplatebinding": {
		allow: "--cluster goes with --project: the command refuses either one without the other, and takes --namespace when neither is given",
	},
	"create token": {
		allow: "an absent --cluster creates a token for no cluster; the cluster is resolved only when the flag is set",
	},
	"grant": {
		allow: "a scope is required instead, with its own refusal: --global takes no cluster and --project with no --cluster is answered by the project lookup",
	},
	"revoke": {
		allow: "the scope is read as grant reads it, with the same refusal",
	},
	"token": {
		allow: "the legacy shim: an empty --cluster is a deliberate token for no cluster",
	},
}

// clusterGuardProblems compares the commands that declare --cluster (name to
// whether it is required) with the rows: a command with an optional --cluster
// and no row, a row for a command that does not declare it, and a row for a
// command whose --cluster is now required, which the required check covers.
func clusterGuardProblems(declared map[string]bool, rows map[string]clusterOptionalRow) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if _, ok := rows[name]; !declared[name] && !ok {
			problems = append(problems, name+" declares an optional --cluster and has no row")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		row := rows[name]
		required, ok := declared[name]
		switch {
		case !ok:
			problems = append(problems, "row "+name+" names no command that declares --cluster: delete the row")
		case required:
			problems = append(problems, "row "+name+": --cluster is required now: delete the row")
		case (row.message == "") == (row.allow == ""):
			problems = append(problems, "row "+name+" needs exactly one of message and allow")
		case row.message != "" && len(row.args) == 0:
			problems = append(problems, "row "+name+" has a message and no arguments")
		}
	}
	return problems
}

// refusalError runs cmd on in through the pipeline and returns the error it
// is refused with. A command that runs fails the test.
func refusalError(t *testing.T, cmd runtime.Command, in sweepInput) *clierr.Error {
	t.Helper()
	d := cmd.Describe()
	var ce *clierr.Error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s: the command ran: %v", d.Name, r)
			}
		}()
		var out, errOut bytes.Buffer
		_, ce = runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &out, Err: &errOut}, safety.Binding{})
	}()
	return ce
}

// withoutConfigDir is a hint with the ` --config '<dir>'` that names the
// run's config directory removed.
func withoutConfigDir(hint, dir string) string {
	return strings.Replace(hint, " --config "+clierr.ShellQuote(dir), "", 1)
}

// TestEveryCommandWithAClusterFlagRefusesItsAbsenceWithTheSharedHint asserts,
// for every registered command that declares --cluster, that an absent one
// is refused as VALIDATION with the hint that lists the clusters:
//
//   - a required --cluster: the pipeline's refusal carries the declaration's
//     message and hint, and the hint is the literal above;
//   - an optional --cluster: a row of clusterOptionalRows either names the
//     arguments that provoke the refusal, run through the root with its exit
//     code, message and hint read from the rendered error, or says why the
//     command never refuses.
//
// The expected texts are literals, never read from the code that writes them.
// The required check cannot see a hint dropped from the declaration and the
// refusal together (both read one variable); the optional rows do. A command
// whose --project is required gets the declaration and hint check only: a set
// --project defers the --cluster check.
func TestEveryCommandWithAClusterFlagRefusesItsAbsenceWithTheSharedHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	byName := map[string]runtime.Command{}
	declared := map[string]bool{}
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		byName[d.Name] = cmd
		for _, f := range d.Flags {
			if f.Name == "cluster" {
				declared[d.Name] = f.Required
			}
		}
	}
	require.Greater(t, len(declared), 60, "the registry holds the commands with a --cluster flag")
	for _, p := range clusterGuardProblems(declared, clusterOptionalRows) {
		t.Error(p)
	}

	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !declared[name] {
			continue
		}
		cmd := byName[name]
		d := cmd.Describe()
		t.Run(name+"/required", func(t *testing.T) {
			var decl runtime.ArgSpec
			for _, f := range d.Flags {
				if f.Name == "cluster" {
					decl = f
				}
			}
			assert.Equal(t, clierr.Rancher+" access clusters", decl.MissingHint)
			if slices.ContainsFunc(d.Flags, func(p runtime.ArgSpec) bool { return p.Name == "project" && p.Required }) {
				return
			}
			ce := refusalError(t, cmd, fillRequired(d, -1, "cluster"))
			require.NotNil(t, ce, "--cluster missing: not refused")
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, decl.MissingMessage, ce.Message)
			assert.Equal(t, decl.MissingHint, ce.Hint)
			assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
		})
	}

	for _, name := range slices.Sorted(maps.Keys(clusterOptionalRows)) {
		row := clusterOptionalRows[name]
		if _, ok := declared[name]; !ok {
			continue
		}
		t.Run(name+"/optional", func(t *testing.T) {
			rec := extraArgsEnv(t)
			dir := os.Getenv(config.EnvConfigDir)
			if row.message == "" {
				if row.probe == nil {
					return
				}
				res, stderr := runResultOf(t, append(slices.Clone(row.probe), "-o", "json")...)
				if res.err != nil {
					assert.NotContains(t, res.err.Message, "--cluster is required", "%s now refuses an absent --cluster: delete the row; stderr: %s", name, stderr)
					assert.NotContains(t, res.err.Message, "--cluster or --all-clusters is required", "%s now refuses an absent --cluster: delete the row", name)
				}
				return
			}
			res, stderr := runResultOf(t, append(slices.Clone(row.args), "-o", "json")...)
			require.NotNil(t, res.err, "not refused; requests: %v", rec.take())
			assert.Equal(t, 2, res.code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, row.message, ce.Message)
			assert.Equal(t, sharedClusterHint, withoutConfigDir(ce.Hint, dir))
		})
	}
}

// TestClusterGuardCatchesAnUnclassifiedCommand asserts the completeness
// rules of the guard on commands the registry does not hold: an optional
// --cluster with no row, a row for a command that declares none, a row for a
// command whose --cluster is required, and a row with both or neither of its
// two kinds.
func TestClusterGuardCatchesAnUnclassifiedCommand(t *testing.T) {
	rows := map[string]clusterOptionalRow{
		"listed":  {allow: "a reason"},
		"refuses": {args: []string{"refuses"}, message: "text"},
	}
	declared := map[string]bool{"listed": false, "refuses": false, "needs it": true}

	assert.Empty(t, clusterGuardProblems(declared, rows))

	declared["added later"] = false
	assert.Equal(t, []string{"added later declares an optional --cluster and has no row"}, clusterGuardProblems(declared, rows))
	delete(declared, "added later")

	delete(declared, "listed")
	assert.Equal(t, []string{"row listed names no command that declares --cluster: delete the row"}, clusterGuardProblems(declared, rows))
	declared["listed"] = true
	assert.Equal(t, []string{"row listed: --cluster is required now: delete the row"}, clusterGuardProblems(declared, rows))
	declared["listed"] = false

	rows["both"] = clusterOptionalRow{args: []string{"both"}, message: "m", allow: "a"}
	rows["neither"] = clusterOptionalRow{}
	rows["no arguments"] = clusterOptionalRow{message: "m"}
	declared["both"], declared["neither"], declared["no arguments"] = false, false, false
	assert.Equal(t, []string{
		"row both needs exactly one of message and allow",
		"row neither needs exactly one of message and allow",
		"row no arguments has a message and no arguments",
	}, clusterGuardProblems(declared, rows))
}

// TestGetTokenWithoutAClusterKeepsItsTextAndExitCode asserts auth get-token
// with neither --cluster nor --user-id is a VALIDATION error with exit code
// 2, whose message opens with the text that names --user-id as the other way
// and whose hint is the command that lists the clusters, written for the
// run's config directory. With a --server the message adds the clause that
// names it.
func TestGetTokenWithoutAClusterKeepsItsTextAndExitCode(t *testing.T) {
	exitEnv(t)
	dir := os.Getenv(config.EnvConfigDir)
	wantHint := "rancher --config " + clierr.ShellQuote(dir) + " access clusters"

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no server", []string{"auth", "get-token", "-o", "json"}, getTokenClusterMessage},
		{"a server and an entry name", []string{"auth", "get-token", "--server", "https://r.example.com", "--name", "prod-c-1", "-o", "json"},
			getTokenClusterMessage + getTokenServerClause("https://r.example.com")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, stderr := runResultOf(t, tc.argv...)
			assert.Equal(t, 2, res.code)
			require.NotNil(t, res.err)
			assert.Equal(t, clierr.CodeValidation, res.err.Code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Equal(t, wantHint, ce.Hint)
		})
	}
}

// schemaCommandFlags reads the schema manifest golden: the flag names each
// command lists (by command path) and the global flag names, with the short
// spellings.
func schemaCommandFlags(t *testing.T) (commands map[string]map[string]bool, global map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "schema.golden.json"))
	require.NoError(t, err)
	var m struct {
		Commands []struct {
			Name  string `json:"name"`
			Flags []struct {
				Name    string   `json:"name"`
				Aliases []string `json:"aliases"`
			} `json:"flags"`
		} `json:"commands"`
		GlobalFlags []struct {
			Name string `json:"name"`
		} `json:"globalFlags"`
	}
	require.NoError(t, json.Unmarshal(b, &m))
	commands, global = map[string]map[string]bool{}, map[string]bool{}
	for _, c := range m.Commands {
		flags := map[string]bool{}
		for _, f := range c.Flags {
			flags[f.Name] = true
			for _, a := range f.Aliases {
				flags[a] = true
			}
		}
		commands[c.Name] = flags
	}
	for _, g := range m.GlobalFlags {
		for spelling := range strings.SplitSeq(g.Name, "/") {
			global[strings.TrimLeft(spelling, "-")] = true
		}
	}
	return commands, global
}

// TestGetTokenClusterHintIsValidForThePluginsFlags asserts the hint of the
// refusal an exec plugin entry gets when its --cluster is missing is one
// command that can be pasted: the words are rancher, --config and the
// directory the plugin's environment named (here one with a space in its
// path), a command path of the schema manifest, and only flags the manifest
// lists for that command or globally. The plugin's own flags are the
// --server it types and its --name, with or without --auth-provider. No
// request is made.
func TestGetTokenClusterHintIsValidForThePluginsFlags(t *testing.T) {
	exitEnv(t)
	dir := filepath.Join(t.TempDir(), "config dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	t.Setenv(config.EnvConfigDir, dir)
	commands, global := schemaCommandFlags(t)

	var hints []string
	for _, extra := range [][]string{nil, {"--auth-provider", "x"}} {
		argv := append([]string{"auth", "get-token", "--server", "https://r.example.com", "--name", "prod-c-1"}, extra...)
		res, stderr := runResultOf(t, append(argv, "-o", "json")...)
		require.NotNil(t, res.err, "stderr: %s", stderr)
		ce := renderedError(t, "json", stderr)
		require.Equal(t, clierr.CodeValidation, ce.Code, "stderr: %s", stderr)
		hints = append(hints, ce.Hint)

		words := shellWords(t, ce.Hint)
		require.Greater(t, len(words), 4, "hint: %s", ce.Hint)
		assert.Equal(t, []string{"rancher", "--config", dir}, words[:3], "hint: %s", ce.Hint)

		rest := words[3:]
		var path string
		for name := range commands {
			if slices.Equal(strings.Fields(name), rest[:min(len(rest), len(strings.Fields(name)))]) && len(name) > len(path) {
				path = name
			}
		}
		require.NotEmpty(t, path, "no command of the schema starts %v", rest)
		for _, w := range rest[len(strings.Fields(path)):] {
			require.True(t, strings.HasPrefix(w, "-"), "%q after %q is no flag", w, path)
			flag, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
			assert.True(t, commands[path][flag] || global[flag], "--%s is no flag of %s or a global flag", flag, path)
		}
	}
	assert.Equal(t, hints[0], hints[1], "the hint is the same with and without --auth-provider")
}

// TestGetTokenClusterMessageNamesTheTypedServerWithoutItsPassword asserts
// the message of auth get-token without --cluster names a typed --server by
// scheme and host only, whatever the value holds: no user information, path
// or query reaches stdout, stderr, the JSON envelope's message or hint, and
// the text form. A value that is no URL shows as <unusable URL>, an empty
// value is refused by the pipeline before the cluster is read, and the
// message is the same whether or not the typed server is the current one.
func TestGetTokenClusterMessageNamesTheTypedServerWithoutItsPassword(t *testing.T) {
	leaks := []string{"secret", "user:", "/path", "x=1"}

	t.Run("a URL with user information, a path and a query", func(t *testing.T) {
		exitEnv(t)
		typed := "https://user:secret@r.example.com/path?x=1"
		want := getTokenClusterMessage + getTokenServerClause("https://r.example.com")

		res, stderr := runResultOf(t, "auth", "get-token", "--server", typed, "-o", "json")
		require.NotNil(t, res.err)
		assert.Equal(t, 2, res.code)
		ce := renderedError(t, "json", stderr)
		assert.Equal(t, want, ce.Message)
		for _, leak := range leaks {
			assert.NotContains(t, ce.Message, leak)
			assert.NotContains(t, ce.Hint, leak)
			assert.NotContains(t, stderr, leak)
		}

		res, stderr = runResultOf(t, "auth", "get-token", "--server", typed)
		require.NotNil(t, res.err)
		assert.Equal(t, want, renderedError(t, "", stderr).Message)
		for _, leak := range leaks {
			assert.NotContains(t, stderr, leak)
		}
	})

	t.Run("a value with no scheme", func(t *testing.T) {
		exitEnv(t)
		typed := "user:secret@r.example.com"
		want := getTokenClusterMessage + getTokenServerClause(config.DisplayServerURL(typed))

		res, stderr := runResultOf(t, "auth", "get-token", "--server", typed, "-o", "json")
		require.NotNil(t, res.err)
		ce := renderedError(t, "json", stderr)
		assert.Equal(t, want, ce.Message)
		assert.Contains(t, ce.Message, "(<unusable URL>)")
		assert.NotContains(t, stderr, "secret")
		assert.NotContains(t, ce.Hint, "secret")

		res, stderr = runResultOf(t, "auth", "get-token", "--server", typed)
		require.NotNil(t, res.err)
		assert.NotContains(t, stderr, "secret")
	})

	t.Run("a host and port with no scheme", func(t *testing.T) {
		exitEnv(t)
		typed := "u:secret@r.example.com:8443/p?x=1"
		res, stderr := runResultOf(t, "auth", "get-token", "--server", typed, "-o", "json")
		require.NotNil(t, res.err)
		ce := renderedError(t, "json", stderr)
		assert.Equal(t, getTokenClusterMessage+getTokenServerClause(config.DisplayServerURL(typed)), ce.Message)
		for _, leak := range []string{"secret", "/p", "x=1"} {
			assert.NotContains(t, stderr, leak)
		}
	})

	t.Run("the typed server is the current server", func(t *testing.T) {
		exitEnv(t)
		typed := "https://r.example.com"
		_, stderr := runResultOf(t, "auth", "get-token", "--server", typed, "-o", "json")
		other := renderedError(t, "json", stderr).Message

		t.Setenv(config.EnvURL, typed)
		res, stderr := runResultOf(t, "auth", "get-token", "--server", typed, "-o", "json")
		require.NotNil(t, res.err)
		assert.Equal(t, other, renderedError(t, "json", stderr).Message)
	})

	t.Run("an empty --server is refused before the cluster is read", func(t *testing.T) {
		require.Empty(t, config.DisplayServerURL(""), "an empty value is the one input that shows as empty")
		for _, argv := range [][]string{
			{"auth", "get-token", "--server", "", "-o", "json"},
			{"auth", "get-token", "--server=", "-o", "json"},
		} {
			exitEnv(t)
			res, stderr := runResultOf(t, argv...)
			require.NotNil(t, res.err, "argv: %v", argv)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, "--server is empty; pass a value or omit --server", ce.Message, "argv: %v", argv)
			assert.NotContains(t, stderr, "unusable", "argv: %v", argv)
		}
	})
}
