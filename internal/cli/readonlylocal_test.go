package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runBounded runs argv through root as runCaptured does, with a closed
// stdin, an editor that cannot start and a time bound, so a command that
// runs instead of being refused cannot wait on a terminal or an editor. It
// returns stdout, stderr and the exit code.
func runBounded(t *testing.T, root *ucli.Command, argv []string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("EDITOR", "rancher-test-no-such-editor")
	t.Setenv("VISUAL", "rancher-test-no-such-editor")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader("")
	CaptureArgv(argv)
	err := root.Run(ctx, append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), exitCodeOf(err)
}

// localCommands returns every command of the built tree whose command type
// is Local, from the schema manifest.
func localCommands(t *testing.T) (wiredRegistry, []runtime.SchemaCommand) {
	t.Helper()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	var local []runtime.SchemaCommand
	for _, c := range wr.reg.Schema("v-test", config.MinRancherVersion).Commands {
		if c.CommandType == runtime.Local {
			local = append(local, c)
		}
	}
	require.NotEmpty(t, local)
	return wr, local
}

func jsonFormatFor(c runtime.SchemaCommand) string {
	if (runtime.OutputPolicy{AllowedFormats: c.OutputPolicy}).Allows("json") {
		return "json"
	}
	return ""
}

// TestEveryLocalCommandRefusesATypedReadOnly asserts that every Local
// command of the built tree, given --read-only typed ahead of the command
// name or after its arguments, with a true or false value or with one dash,
// is refused with VALIDATION at exit 2 and no hint, with no request to the
// server it is pointed at, no file written and no prompt or editor started.
// Not parallel: it sets the process environment and the captured command
// line.
func TestEveryLocalCommandRefusesATypedReadOnly(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	dirs := emptyDirs(t)
	wr, local := localCommands(t)
	for _, c := range local {
		format := jsonFormatFor(c)
		for _, form := range []struct {
			name string
			argv func(leaf, args, flag []string) []string
		}{
			{"ahead of the command", func(leaf, args, flag []string) []string {
				return append(append(append([]string{}, flag...), leaf...), args...)
			}},
			{"after the arguments", func(leaf, args, flag []string) []string {
				return append(append(append([]string{}, leaf...), args...), flag...)
			}},
		} {
			for _, flag := range []string{"--read-only", "--read-only=false", "--read-only=true", "-read-only"} {
				argv := form.argv(strings.Fields(c.Name), argValues(c.Args), []string{flag})
				if format != "" {
					argv = append(argv, "-o", format)
				}
				t.Run(c.Name+" "+form.name+" "+flag, func(t *testing.T) {
					defer rec.take()
					_, stderr, code := runBounded(t, wr.root("v-test"), argv)
					label := "rancher " + strings.Join(argv, " ")
					assert.Equal(t, 2, code, label)
					assert.Empty(t, rec.take(), "%s: no request", label)
					assertNoFiles(t, label, dirs...)
					ce := renderedError(t, format, stderr)
					assert.Equal(t, clierr.CodeValidation, ce.Code, label)
					assert.Empty(t, ce.Hint, label)
					assert.Contains(t, ce.Message, c.Name+" is a local command", label)
					assert.NotContains(t, ce.Message, "enabled by", label)
				})
			}
		}
	}
}

// TestReadOnlyAfterDoubleDashIsAnArgument asserts that a --read-only after
// "--" is an argument of the command, not the flag, so a Local command is
// not refused for it. Not parallel: it sets the process environment and the
// captured command line.
func TestReadOnlyAfterDoubleDashIsAnArgument(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	stdout, stderr, code := runBounded(t, root, []string{"auth", "delete-token", "-o", "json", "--", "--read-only"})
	assert.Equal(t, 0, code, stderr)
	assert.Empty(t, stderr)
	assert.NotEmpty(t, stdout)
}

// TestGetTokenRunsUnderEnvReadOnly asserts that the exec plugin's command,
// which never carries --read-only, is not refused under the environment
// source: it runs and reaches the server. Not parallel: it sets the process
// environment and the captured command line.
func TestGetTokenRunsUnderEnvReadOnly(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	t.Setenv(config.EnvReadOnly, "1")
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, _ := runBounded(t, root, []string{
		"auth", "get-token", "--server", os.Getenv(config.EnvURL), "--cacert", os.Getenv(config.EnvCACert), "--cluster", "c1", "--name", "kube",
	})
	assert.NotContains(t, stderr, "is a local command")
	assert.NotEmpty(t, rec.take(), "the command reached the server")
}

// TestTypedReadOnlyOnLocalCommandWithEnvReadOnlyIsRefusedByTheTypedRule
// asserts that a typed --read-only on a Local command with
// RANCHER_CLI_READONLY also set is refused by the typed rule: the message
// does not give the environment variable as the cause. Not parallel: it
// sets the process environment and the captured command line.
func TestTypedReadOnlyOnLocalCommandWithEnvReadOnlyIsRefusedByTheTypedRule(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	t.Setenv(config.EnvReadOnly, "1")
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runBounded(t, root, []string{"--read-only", "config", "path", "-o", "json"})
	assert.Equal(t, 2, code)
	assert.Empty(t, rec.take())
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, ce.Message, "config path is a local command")
	assert.NotContains(t, ce.Message, "enabled by")
	assert.NotContains(t, ce.Message, "read-only mode:")
}

// TestLocalCommandsRunUnderEnvAndConfigReadOnly asserts that the
// environment and config read-only sources, with no typed --read-only,
// leave Local commands usable.
// Not parallel: it sets the process environment and the captured command
// line.
func TestLocalCommandsRunUnderEnvAndConfigReadOnly(t *testing.T) {
	restoreRawArgv(t)
	for _, source := range []string{"env", "config"} {
		for _, argv := range [][]string{
			{"auth", "logout", "--all", "-o", "json"},
			{"config", "list-servers", "-o", "json"},
			{"config", "path", "-o", "json"},
		} {
			t.Run(source+" "+strings.Join(argv, " "), func(t *testing.T) {
				rec := extraArgsEnv(t)
				if source == "env" {
					t.Setenv(config.EnvReadOnly, "1")
				} else {
					require.NoError(t, os.WriteFile(config.FilePath(os.Getenv(config.EnvConfigDir)), []byte("readOnly: true\n"), 0o600))
				}
				root, err := BuildRoot("v-test")
				require.NoError(t, err)
				stdout, stderr, code := runBounded(t, root, argv)
				assert.Equal(t, 0, code, stderr)
				assert.Empty(t, stderr)
				assert.NotEmpty(t, stdout)
				assert.Empty(t, rec.take())
			})
		}
	}
}

// TestTypedReadOnlyIsAcceptedOnAReadOnlyCommand asserts that a ReadOnly
// command runs with --read-only typed. Not parallel: it sets the process
// environment and the captured command line.
func TestTypedReadOnlyIsAcceptedOnAReadOnlyCommand(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	stdout, stderr, code := runBounded(t, root,
		[]string{"--read-only", "get", "namespace", "ns1", "--cluster", "local", "-o", "json"})
	assert.Equal(t, 0, code, stderr)
	assert.NotEmpty(t, stdout)
	assert.NotEmpty(t, rec.take(), "the command reached the server")
}

// TestTypedReadOnlyOnAMutatingCommandKeepsItsRefusal asserts that a
// Mutating command with --read-only typed is refused by read-only mode and
// names --read-only as the source. Not parallel: it sets the process
// environment and the captured command line.
func TestTypedReadOnlyOnAMutatingCommandKeepsItsRefusal(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runBounded(t, root,
		[]string{"--read-only", "create", "namespace", "ns1", "--cluster", "local", "-o", "json"})
	assert.Equal(t, 2, code)
	assert.Empty(t, rec.take())
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "read-only mode: create namespace is a mutating command; enabled by --read-only")
}

// TestTypedReadOnlyOnALocalCommandKeepsTheDryRunRefusalSecond asserts that
// --dry-run alone on a Local command keeps DRY_RUN_NOT_APPLICABLE and that
// with --read-only also typed the --read-only refusal comes first. Not
// parallel: it sets the process environment and the captured command line.
func TestTypedReadOnlyOnALocalCommandKeepsTheDryRunRefusalSecond(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runBounded(t, root, []string{"config", "path", "--dry-run", "-o", "json"})
	assert.Equal(t, clierr.CodeDryRunNotApplicable, renderedError(t, "json", stderr).Code, "exit %d", code)

	root, err = BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code = runBounded(t, root, []string{"config", "path", "--dry-run", "--read-only", "-o", "json"})
	assert.Equal(t, 2, code)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "config path is a local command")
}

// TestHelpWithTypedReadOnlyIsNotRefused asserts that help, which never
// reaches a command, prints and exits 0 with --read-only typed. Not
// parallel: it sets the process environment and the captured command line.
func TestHelpWithTypedReadOnlyIsNotRefused(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, argv := range [][]string{
		{"--read-only"},
		{"--read-only", "--help"},
		{"config", "path", "--read-only", "--help"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stdout, stderr, code := runBounded(t, root, argv)
			assert.Equal(t, 0, code, stderr)
			assert.Empty(t, stderr)
			assert.Contains(t, stdout, "USAGE")
		})
	}
}

// TestSchemaWithTypedReadOnlyReportsTheFlagAsSource asserts that schema,
// which is not a Local command, still runs with --read-only typed and
// reports read-only mode on with --read-only as its source. Not parallel:
// it sets the process environment and the captured command line.
func TestSchemaWithTypedReadOnlyReportsTheFlagAsSource(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	stdout, stderr, code := runBounded(t, root, []string{"--read-only", "schema", "-o", "json"})
	require.Equal(t, 0, code, stderr)
	var env struct {
		Data struct {
			ReadOnly        bool     `json:"readOnly"`
			ReadOnlySources []string `json:"readOnlySources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env))
	assert.True(t, env.Data.ReadOnly)
	assert.Equal(t, []string{"--read-only"}, env.Data.ReadOnlySources)
}

// TestCompletionWithReadOnlyOnTheLineIsUnchanged asserts that completing
// the shell argument of `completion` on a line that holds --read-only,
// before or after the command, lists the shells and writes nothing to
// stderr.
func TestCompletionWithReadOnlyOnTheLineIsUnchanged(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	for name, typed := range map[string][]string{
		"ahead of the command": {"--read-only", "--config", dir, "completion"},
		"after the command":    {"--config", dir, "completion", "--read-only"},
	} {
		t.Run(name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out, errOut bytes.Buffer
			root.Writer = &out
			root.ErrWriter = &errOut
			require.NoError(t, runCompletionRequest(t, root, typed, ""))
			assert.Equal(t, "bash\nfish\npwsh\nzsh\n", out.String())
			assert.Empty(t, errOut.String())
		})
	}
}

// TestLocalCommandsNeitherListNorOfferReadOnly asserts, for every
// registered command, that a Local command lists --read-only neither under
// GLOBAL OPTIONS in its help nor among its flag-name completion
// candidates, since it refuses the flag, and that every other command
// still does both.
func TestLocalCommandsNeitherListNorOfferReadOnly(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv(config.EnvCredentialStore, "file")
	t.Cleanup(func() { CaptureArgv(nil) })
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	local, other := 0, 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		words := strings.Fields(d.Name)
		_, globals := helpSections(surfaceRun(t, slices.Concat(words, []string{"--help"})))
		completion := surfaceRun(t, slices.Concat([]string{"--rancher-completion-proto=2"}, words,
			[]string{"--rancher-completion-cur=--read", "--generate-shell-completion"}))
		offered := slices.ContainsFunc(strings.Split(completion, "\n"), func(line string) bool {
			return candidateToken(line) == "--"+runtime.FlagReadOnly
		})
		listed := listsFlag(globals, runtime.FlagReadOnly)
		if d.CommandType == runtime.Local {
			local++
			assert.False(t, listed, "%s: GLOBAL OPTIONS lists --read-only", d.Name)
			assert.False(t, offered, "%s: completion offers --read-only", d.Name)
			continue
		}
		other++
		assert.True(t, listed, "%s: GLOBAL OPTIONS lists --read-only", d.Name)
		assert.True(t, offered, "%s: completion offers --read-only", d.Name)
	}
	assert.Positive(t, local)
	assert.Positive(t, other)
}
