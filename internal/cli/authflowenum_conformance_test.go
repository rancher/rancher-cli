package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// TestEveryAuthFlowFlagListsTheSameValues asserts every registered command
// that declares --auth-flow lists exactly the values token.AuthFlowValues
// returns, in that order, so the flag's allowed values, its completion and
// the refusal for a value from the environment cannot differ.
func TestEveryAuthFlowFlagListsTheSameValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var seen []string
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		for _, f := range d.Flags {
			if f.Name != "auth-flow" {
				continue
			}
			seen = append(seen, d.Name)
			assert.NotEmpty(t, f.Enum, "%s declares --auth-flow with no allowed values", d.Name)
			assert.Equal(t, token.AuthFlowValues(), f.Enum, "%s lists other values for --auth-flow", d.Name)
		}
	}
	assert.Subset(t, seen, []string{"auth get-token", "auth login", "token"},
		"the walk reaches the commands known to declare --auth-flow")
}

// TestATypedAuthFlowOutsideTheValuesIsRefusedByThePipeline asserts a typed
// --auth-flow value outside the declared values is a VALIDATION refusal
// (exit 2) naming the flag and both values, before the command runs, on
// every command that declares it, and never prints the value. Not parallel:
// the captured argv is process-wide.
func TestATypedAuthFlowOutsideTheValuesIsRefusedByThePipeline(t *testing.T) {
	restoreRawArgv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RANCHER_CLI_CONFIG_DIR", t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")

	const secret = "s3cr3t"
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"auth get-token", []string{"-o", "json", "auth", "get-token", "--server", "https://rancher.example.test", "--cluster", "c-1", "--auth-flow", secret}},
		{"token", []string{"-o", "json", "token", "--server", "https://rancher.example.test", "--user", "alice", "--auth-flow", secret}},
		{"auth login", []string{"-o", "json", "auth", "login", "prod", "--auth-flow", secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runLine(t, tc.argv)
			assert.Equal(t, 2, got.exit, "stderr: %s", got.stderr)
			assert.Equal(t, "VALIDATION", got.code, "stderr: %s", got.stderr)
			assert.Contains(t, got.message, "--auth-flow")
			assert.Contains(t, got.message, "is not one of "+runtime.OneOf(token.AuthFlowValues()))
			assert.NotContains(t, got.stderr, secret)
		})
	}
}
