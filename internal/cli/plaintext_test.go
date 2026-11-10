package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// plainTextCommands are the commands whose descriptor sets PlainText, each
// with the reason its text prints a command with no config directory.
var plainTextCommands = map[string]string{
	"logs": "the text is the output of a program in a container, never a remedy channel of this invocation",
}

// TestPlainTextIsSetOnlyOnTheListedCommands asserts, over every command the
// root registers, the generated ones included, that the commands whose
// descriptor sets PlainText are exactly the listed ones: a command outside the
// list that sets it fails, and so does a listed command that does not.
func TestPlainTextIsSetOnlyOnTheListedCommands(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var set []string
	for _, c := range wr.reg.Commands() {
		if d := c.Describe(); d.PlainText {
			set = append(set, d.Name)
		}
	}
	for _, name := range set {
		_, listed := plainTextCommands[name]
		assert.True(t, listed, "%s sets PlainText and is not in the list of commands that may", name)
	}
	for name, why := range plainTextCommands {
		assert.NotEmpty(t, why, "%s has no reason", name)
		assert.True(t, slices.Contains(set, name), "%s is in the list and does not set PlainText", name)
	}
}

// TestPlainTextListCoversTheWholeRegistry asserts the registry the test
// above reads holds more than 150 commands, so an empty one cannot pass.
func TestPlainTextListCoversTheWholeRegistry(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	assert.Greater(t, len(wr.reg.Commands()), 150)
}

// argInput is the input of a command that takes no flags and the arguments
// it is given.
type argInput struct{ args []string }

func (argInput) String(string) string    { return "" }
func (argInput) Bool(string) bool        { return false }
func (argInput) Strings(string) []string { return nil }
func (argInput) IsSet(string) bool       { return false }
func (in argInput) Args() []string       { return in.args }

// argumentsFor is one argument for each argument a descriptor declares: the
// first value of its enum, or a word.
func argumentsFor(d runtime.Descriptor) []string {
	args := make([]string, len(d.Args))
	for i, a := range d.Args {
		args[i] = "x"
		if len(a.Enum) > 0 {
			args[i] = a.Enum[0]
		}
	}
	return args
}

// supportedCapability reports every capability requirement as met.
type supportedCapability struct{}

func (supportedCapability) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// markedRead is a read or local command that returns a text holding a marked
// command.
type markedRead struct{ d runtime.Descriptor }

func (c markedRead) Describe() runtime.Descriptor { return c.d }

func (c markedRead) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return markedResult(c.d), nil
}

// markedMutation is a mutating command that returns a text holding a marked
// command once it is applied.
type markedMutation struct{ d runtime.Descriptor }

func (c markedMutation) Describe() runtime.Descriptor { return c.d }

func (c markedMutation) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: "c"}
	return eff, func(context.Context) (runtime.Result, error) { return markedResult(c.d), nil }, nil
}

// markedText is a line that holds a marked command.
func markedText() string { return "ran " + clierr.Rancher + " get pods\n" }

// markedResult is the result of a command with descriptor d: the marked line,
// and for a command that delivers a value once, the value line, its notice and
// the committed context that path needs.
func markedResult(d runtime.Descriptor) runtime.Result {
	res := runtime.Result{Text: markedText()}
	if d.Delivery == runtime.DeliveryOnceOnly {
		line, notice := runtime.OnceOnlyLine("token", "secret", "token")
		res.Text += line + "\n"
		res.OnceOnlyNotice = notice
		res.CommittedContext = &runtime.CommittedContext{ResourceKind: "token", ResourceName: "t"}
	}
	return res
}

// TestOnlyTheListedCommandsPrintAMarkedCommandPlain runs, through the
// pipeline, the real descriptor of every command the root registers on a
// command that returns a text holding a marked command, with a config
// directory that is not the default, and asserts the text names the directory
// for every command but the listed ones, which print the plain command. The
// descriptor keeps its arguments (one input argument for each) and its
// capability requirement (a checker that reports every requirement met). Two
// fields change: the flags are cleared, since the input holds none and a
// required flag would refuse the run, and the shape is the text shape, since
// the command returns text. A condition on either of those two fields in the
// choice of the expansion is not seen; a condition on any other field is.
// Every command runs under one invocation, the default format with --apply
// and --yes: a condition that joins a descriptor field with another output
// format, or with those gates unset, is not seen either.
func TestOnlyTheListedCommandsPrintAMarkedCommandPlain(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	commands := wr.reg.Commands()
	require.Greater(t, len(commands), 150)
	for _, c := range commands {
		d := c.Describe()
		t.Run(d.Name, func(t *testing.T) {
			t.Parallel()
			stand := d
			stand.Flags, stand.Shape = nil, runtime.ShapeText
			var cmd runtime.Command = markedRead{stand}
			if d.CommandType == runtime.Mutating {
				cmd = markedMutation{stand}
			}
			var out, errBuf bytes.Buffer
			streams := runtime.IO{Out: &out, Err: &errBuf, Apply: true, Yes: true, RemedyDir: "/d", Capability: supportedCapability{}}
			_, ce := runtime.Run(t.Context(), cmd, argInput{argumentsFor(d)}, streams, safety.Binding{})
			require.Nil(t, ce)
			want := "ran rancher --config '/d' get pods\n"
			if _, plain := plainTextCommands[d.Name]; plain {
				want = "ran rancher get pods\n"
			}
			assert.True(t, strings.HasPrefix(out.String(), want), "%q", out.String())
		})
	}
}
