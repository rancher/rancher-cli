package cli

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

var errWaitFlagsClientUsed = errors.New("a client was used before the flags were validated")

// failingClients and failingRoots fail every use, so a command that reaches a
// client before refusing its flags shows up as a non-VALIDATION error.
type failingClients struct{}

func (failingClients) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return nil, errWaitFlagsClientUsed
}

func (failingClients) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return nil, errWaitFlagsClientUsed
}

func (failingClients) ResolveCluster(context.Context, string) (string, error) {
	return "", errWaitFlagsClientUsed
}

type failingRoots struct{}

func (failingRoots) Root() (engine.RootClient, error) { return nil, errWaitFlagsClientUsed }

// waitFlagInput is an Input with string flags only and no positional
// argument, the shape of a get that names no object.
type waitFlagInput map[string]string

func (in waitFlagInput) String(name string) string { return in[name] }
func (in waitFlagInput) Bool(string) bool          { return false }
func (in waitFlagInput) Strings(string) []string   { return nil }
func (in waitFlagInput) IsSet(name string) bool    { _, ok := in[name]; return ok }
func (in waitFlagInput) Args() []string            { return nil }

// declaredFlags lists the flag names a command declares.
func declaredFlags(c runtime.Command) []string {
	var out []string
	for _, f := range c.Describe().Flags {
		out = append(out, f.Name)
	}
	return out
}

// commandsDeclaringForWithoutWait names the commands that declare --for and
// not --wait: a --for that starts a wait belongs to a command that waits.
func commandsDeclaringForWithoutWait(cmds []runtime.Command) []string {
	var out []string
	for _, c := range cmds {
		flags := declaredFlags(c)
		if slices.Contains(flags, "for") && !slices.Contains(flags, "wait") {
			out = append(out, c.Describe().Name)
		}
	}
	return out
}

type stubCommand struct {
	name  string
	flags []string
}

func (s stubCommand) Describe() runtime.Descriptor {
	d := runtime.Descriptor{Name: s.name}
	for _, f := range s.flags {
		d.Flags = append(d.Flags, runtime.ArgSpec{Name: f, Type: "string"})
	}
	return d
}

// TestForWithoutWaitCheckFindsACommandDeclaringOnlyFor asserts the check
// reports a command that declares --for without --wait and passes one that
// declares both.
func TestForWithoutWaitCheckFindsACommandDeclaringOnlyFor(t *testing.T) {
	t.Parallel()
	only := stubCommand{name: "only for", flags: []string{"for", "timeout"}}
	both := stubCommand{name: "both", flags: []string{"wait", "for", "timeout"}}
	neither := stubCommand{name: "neither", flags: []string{"timeout"}}
	assert.Equal(t, []string{"only for"}, commandsDeclaringForWithoutWait([]runtime.Command{only, both, neither}))
	assert.Empty(t, commandsDeclaringForWithoutWait([]runtime.Command{both, neither}))
}

// wantWaitCommands is the commands that declare --wait: the get of every
// kind but clusterregistrationtoken, whose get never waits. The --timeout of
// the access and oidc-client commands is a time limit, not a wait, and those
// commands declare no --wait.
func wantWaitCommands() []string {
	var want []string
	for _, k := range kinds.All() {
		if k.Name != "clusterregistrationtoken" {
			want = append(want, "get "+k.Name)
		}
	}
	return want
}

// TestOnlyGetDeclaresTheWaitFlags asserts that the commands declaring --wait
// are exactly the generic gets, so delete, patch and the clusterregistrationtoken
// get do not offer a flag they ignore or refuse, and that every command
// declaring --for declares --wait.
func TestOnlyGetDeclaresTheWaitFlags(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	cmds := wr.reg.Commands()

	var got []string
	for _, c := range cmds {
		if slices.Contains(declaredFlags(c), "wait") {
			got = append(got, c.Describe().Name)
		}
	}
	assert.ElementsMatch(t, wantWaitCommands(), got)
	assert.Empty(t, commandsDeclaringForWithoutWait(cmds))
}

// TestEveryGetRefusesTheWaitFlagsWithoutAName asserts that, for every command
// declaring --wait, --for alone and --timeout alone start the wait and so
// refuse a missing resource argument as a VALIDATION error naming the flag,
// before the command reaches a client.
func TestEveryGetRefusesTheWaitFlagsWithoutAName(t *testing.T) {
	t.Parallel()
	want := wantWaitCommands()
	deps := runDeps{clients: failingClients{}, roots: failingRoots{}}
	seen := map[string]bool{}
	for _, e := range buildEntries() {
		name := e.make(runDeps{}).Describe().Name
		if !slices.Contains(want, name) {
			continue
		}
		seen[name] = true
		cmd, ok := e.make(deps).(runtime.ReadCommand)
		require.True(t, ok, name)
		for _, row := range []struct{ flag, value string }{{"for", "state=x"}, {"timeout", "1s"}} {
			t.Run(name+" "+row.flag, func(t *testing.T) {
				_, err := cmd.Execute(t.Context(), waitFlagInput{row.flag: row.value}, nil)
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				require.ErrorContains(t, err, "requires a resource")
				require.ErrorContains(t, err, "--"+row.flag)
			})
		}
	}
	for _, name := range want {
		assert.True(t, seen[name], "no entry builds %s", name)
	}
}
