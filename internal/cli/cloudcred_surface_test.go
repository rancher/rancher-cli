package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ccRefusedValue = "fake-refused-value-0001"
	ccRefusedText  = "this command takes no patch document; give fields with --set <field>=<value>, secret values with --from-file <field>=<path>, and --display-name or --description"
)

// ccGroupCommands lists the names of the commands of the group name of root.
func ccGroupCommands(t *testing.T, group string) []string {
	t.Helper()
	root, err := BuildRoot("test")
	require.NoError(t, err)
	var names []string
	for _, c := range root.Commands {
		if c.Name != group {
			continue
		}
		for _, sub := range c.Commands {
			names = append(names, sub.Name)
		}
	}
	return names
}

// TestNoKindRowIsNamedCloudCredential asserts cloud credentials have no kind
// row, so no generated command takes the name, and that the patch group and the
// delete group each have exactly one command named cloud-credential: the
// dedicated one.
func TestNoKindRowIsNamedCloudCredential(t *testing.T) {
	t.Parallel()
	_, ok := kinds.Lookup("cloud-credential")
	assert.False(t, ok, "a kind row is named cloud-credential")
	for _, group := range []string{"patch", "delete"} {
		assert.Equal(t, 1, strings.Count(strings.Join(ccGroupCommands(t, group), "\n")+"\n", "cloud-credential\n"), group)
	}
}

// ccFailingStdin fails the test when anything reads it.
type ccFailingStdin struct{ t *testing.T }

func (r ccFailingStdin) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, errors.New("stdin was read")
}

// ccRefusedRun runs argv with the stub server up, stdin that fails on read and
// telemetry recording on, and returns what the run wrote and the files it left.
func ccRefusedRun(t *testing.T, argv []string) (stdout, stderr string, code int, events int) {
	t.Helper()
	ccEnableRecording(t)
	secretServer(t, nil, secretBearer)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = ccFailingStdin{t}
	code = Run(t.Context(), root, append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), code, len(storedRuns(t, os.Getenv(config.EnvConfigDir)))
}

// TestPatchCloudCredentialRefusesPatchFlags asserts each form of the four
// patch document flags (--patch, -p, --patch-file, --type, with a space and with
// "=") is refused with VALIDATION and the text that names the flag as typed and
// the flags to use instead, typed before patch, after patch and after the
// credential. Each row asserts the exit code, that the planted value is in no
// output, that no event was recorded (the line is refused at parse), that no
// request was sent and that stdin, named by --from-file, was never read. Not
// parallel: it sets the environment.
func TestPatchCloudCredentialRefusesPatchFlags(t *testing.T) {
	restoreRawArgv(t)
	type form struct {
		typed string   // the flag as typed, without its value
		words []string // the words that carry the flag and its value
	}
	forms := []form{
		{"--patch", []string{"--patch", ccRefusedValue}},
		{"-p", []string{"-p", ccRefusedValue}},
		{"--patch-file", []string{"--patch-file", ccRefusedValue}},
		{"--type", []string{"--type", ccRefusedValue}},
		{"--patch", []string{"--patch=" + ccRefusedValue}},
		{"-p", []string{"-p=" + ccRefusedValue}},
		{"--patch-file", []string{"--patch-file=" + ccRefusedValue}},
		{"--type", []string{"--type=" + ccRefusedValue}},
	}
	positions := []struct {
		name  string
		build func(words []string) []string
	}{
		{"after the credential", func(w []string) []string {
			return slices.Concat([]string{"patch", "cloud-credential", "cc-abcde", "--from-file", "secretKey=-"}, w)
		}},
		{"after patch", func(w []string) []string {
			return slices.Concat([]string{"patch"}, w, []string{"cloud-credential", "cc-abcde", "--from-file", "secretKey=-"})
		}},
		{"before patch", func(w []string) []string {
			return slices.Concat(w, []string{"patch", "cloud-credential", "cc-abcde", "--from-file", "secretKey=-"})
		}},
	}
	for _, pos := range positions {
		for _, f := range forms {
			t.Run(pos.name+" "+strings.Join(f.words, " "), func(t *testing.T) {
				argv := append(pos.build(f.words), "-o", "json")
				stdout, stderr, code, events := ccRefusedRun(t, argv)
				assert.Equal(t, clierr.New(clierr.CodeValidation, "x").ExitCode(), code, stderr)
				var envelope struct {
					Kind string         `json:"kind"`
					Data map[string]any `json:"data"`
				}
				require.NoError(t, json.Unmarshal([]byte(stderr), &envelope), stderr)
				errBody, _ := envelope.Data["error"].(map[string]any)
				assert.Equal(t, clierr.CodeValidation, errBody["code"])
				assert.Equal(t, f.typed+": "+ccRefusedText+"; pass --help for usage", errBody["message"])
				assert.Zero(t, events, "a line refused at parse records no event")
				assert.Empty(t, secretSeen.all(), "no request was sent")
				for _, form := range ccForms(ccRefusedValue) {
					assert.NotContains(t, stdout, form, "stdout")
					assert.NotContains(t, stderr, form, "stderr")
				}
			})
		}
	}
	t.Run("a backslash path stops at the refused flag", func(t *testing.T) {
		stdout, stderr, code, events := ccRefusedRun(t, []string{"patch", "cloud-credential", "cc-abcde", "--from-file", `secretKey=C:\temp\key.txt`, "--patch", ccRefusedValue, "-o", "json"})
		assert.Equal(t, clierr.New(clierr.CodeValidation, "x").ExitCode(), code)
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(stderr), &envelope), stderr)
		errBody, _ := envelope.Data["error"].(map[string]any)
		assert.Equal(t, clierr.CodeValidation, errBody["code"])
		assert.True(t, strings.HasPrefix(errBody["message"].(string), "--patch: "), errBody["message"])
		assert.NotContains(t, stdout+stderr, `C:\\temp`)
		assert.Zero(t, events)
	})
}

// TestRefusedFlagsNeverShadowDeclaredFlags asserts, over every registered
// command, that no flag name a command refuses is a flag the command declares,
// by name or alias, or a flag the command inherits from a parent, since a
// refusal would hide it.
func TestRefusedFlagsNeverShadowDeclaredFlags(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("test")
	require.NoError(t, err)
	refusing := 0
	var walk func(cmd *Command, inherited []string)
	walk = func(cmd *Command, inherited []string) {
		own := []string{}
		for _, f := range cmd.Flags {
			own = append(own, f.Names()...)
		}
		if d, ok := commandDescriptor(cmd); ok {
			for _, flag := range d.Flags {
				own = append(own, flag.Name)
			}
			for _, r := range d.RefusedFlags {
				refusing++
				assert.NotContains(t, own, r.Name, "%s refuses a flag it declares", d.Name)
				assert.NotContains(t, inherited, r.Name, "%s refuses a flag it inherits", d.Name)
				assert.NotEmpty(t, r.Text, d.Name)
			}
		}
		next := slices.Clone(inherited)
		for _, f := range cmd.Flags {
			if f.Local {
				continue
			}
			next = append(next, f.Names()...)
		}
		for _, sub := range cmd.Commands {
			walk(sub, next)
		}
	}
	walk(root, nil)
	assert.Positive(t, refusing, "a command refuses flags")
}

// TestPatchCloudCredentialOffersNoRefusedFlag asserts the schema entry of the
// command lists none of the four refused names as a flag and that completing
// the flags of the command offers none of them.
func TestPatchCloudCredentialOffersNoRefusedFlag(t *testing.T) {
	restoreRawArgv(t)
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	root, err := BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", t.TempDir(), "schema", "patch", "cloud-credential", "-o", "json"}))
	var card struct {
		Data struct {
			Flags []struct {
				Name string `json:"name"`
			} `json:"flags"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &card), out.String())
	var declared []string
	for _, f := range card.Data.Flags {
		declared = append(declared, f.Name)
	}
	for _, want := range []string{"set", "from-file", "display-name", "description"} {
		assert.Contains(t, declared, want)
	}
	for _, refused := range []string{"patch", "p", "patch-file", "type"} {
		assert.NotContains(t, declared, refused)
	}
	var offered []string
	for _, line := range runCompletion(t, []string{"patch", "cloud-credential", "cc-abcde"}, "--") {
		name, _, _ := strings.Cut(line, ":")
		offered = append(offered, name)
	}
	for _, refused := range []string{"--patch", "--patch-file", "--type", "-p"} {
		assert.NotContains(t, offered, refused)
	}
	assert.Contains(t, offered, "--display-name")
}
