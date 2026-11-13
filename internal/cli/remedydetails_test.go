package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// detailsFailureCommand is a local command that fails with a details
// payload.
type detailsFailureCommand struct{ details clierr.TextDetails }

func (detailsFailureCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "details-failure", CommandType: runtime.Local}
}

func (c detailsFailureCommand) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, clierr.New(clierr.CodeGeneric, "failed").WithDetails(c.details)
}

// nonBareRoot is a root with the real global flags and cmd as its one
// command, wired as an entry that opens its directory.
func nonBareRoot(t *testing.T, cmd runtime.Command) *ucli.Command {
	t.Helper()
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	leaf := adaptEntry(reg, "v-test", false, func(runDeps) runtime.Command { return cmd })
	wr := wiredRegistry{reg: reg, groups: map[string][]*ucli.Command{"": {leaf}}, order: []string{""}}
	return wr.root("v-test")
}

// TestPartialFailureDetailsNameTheDirectoryInTheCommandTheyCarry asserts the
// message of a failed delete, copied from the error it failed with, names the
// run's config directory in the remedy command it holds, in the details lines
// of the text error, and leaves no marker in the JSON error. A command that
// reads a directory and fails with a timed-out read carries
// the inspect cluster command in such a message.
func TestPartialFailureDetailsNameTheDirectoryInTheCommandTheyCarry(t *testing.T) {
	for name, details := range map[string]clierr.TextDetails{
		"delete project": resources.DeleteProjectPartialFailure{Objects: []safety.Object{
			{Kind: "namespace", Name: "ns-1", Outcome: safety.OutcomeFailed, Code: clierr.CodeGeneric, Message: "the read timed out (check with: " + clierr.Text(clierr.NewCommand("inspect cluster").Arg("c-m-x")) + ")"},
		}},
		"revoke": rbac.RevokePartialFailure{
			Deleted: []string{"b-1"},
			Failed:  rbac.RevokeFailure{Name: "b-2", Code: clierr.CodeGeneric, Message: "the read timed out (check with: " + clierr.Text(clierr.NewCommand("inspect cluster").Arg("c-m-x")) + ")"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			directoryRuleEnv(t)
			dir := filepath.Join(t.TempDir(), "cfg")
			want := "rancher --config " + clierr.ShellQuote(dir) + " inspect cluster 'c-m-x'"

			_, stderr := runDirectoryLine(t, nonBareRoot(t, detailsFailureCommand{details}), "--config", dir, "details-failure")
			assert.Contains(t, stderr, want)
			assert.NotContains(t, stderr, clierr.RemedyMarker)

			_, stderr = runDirectoryLine(t, nonBareRoot(t, detailsFailureCommand{details}), "--config", dir, "details-failure", "-o", "json")
			var doc any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stderr)), &doc), stderr)
			held := false
			for _, s := range jsonStrings(doc) {
				assert.NotContains(t, s, clierr.RemedyMarker)
				held = held || strings.Contains(s, want)
			}
			assert.True(t, held, "the details carry the command with the directory: %s", stderr)
		})
	}
}
