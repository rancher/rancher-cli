package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/config"
)

// runConfigCLI runs one command line on a fresh root and returns its
// output and error streams.
func runConfigCLI(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	_ = root.Run(t.Context(), append([]string{"rancher"}, args...))
	return out.String(), errOut.String()
}

// TestEmptyConfigFlagAndVariable asserts the blank --config refusal and
// the environment rule beside it: a variable set to "" is unset, a
// blank variable is refused naming it, and a blank --config is refused
// with the variable unset or set to a directory.
func TestEmptyConfigFlagAndVariable(t *testing.T) {
	str := func(v string) *string { return &v }
	for _, tc := range []struct {
		name string
		env  *string
		args []string
		want string // "" = not refused
	}{
		{"flag blank, variable unset", nil, []string{"--config", "", "config", "path"}, "--config is empty; pass a value or omit --config"},
		{"variable empty, no flag", str(""), []string{"config", "path"}, ""},
		{"flag blank, variable a directory", str("/tmp/x"), []string{"--config", "", "config", "path"}, "--config is empty; pass a value or omit --config"},
		{"flag blank, variable empty", str(""), []string{"--config", "", "config", "path"}, ""},
		{"variable blank, no flag", str(" "), []string{"config", "path"}, "RANCHER_CLI_CONFIG_DIR is blank; set it to a directory or unset it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The rows that run use the default directory: keep it under
			// a temporary home, never the operator's.
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv(config.EnvConfigDir, "")
			if tc.env == nil {
				require.NoError(t, os.Unsetenv(config.EnvConfigDir))
			} else {
				t.Setenv(config.EnvConfigDir, *tc.env)
			}
			stdout, stderr := runConfigCLI(t, append(tc.args, "-o", "json")...)
			var env errEnvelope
			decoded := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &env) == nil
			if tc.want == "" {
				assert.NotContains(t, stderr, "is empty")
				assert.NotContains(t, stderr, "is blank")
				assert.True(t, !decoded || env.Data.Error.Code == "", "runs with no error envelope: %s", stderr)
				var out struct {
					Data struct {
						Path string `json:"path"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
				assert.True(t, strings.HasPrefix(out.Data.Path, home), "the default directory is under the test's home: %s", out.Data.Path)
				return
			}
			require.True(t, decoded, "an error envelope: %s", stderr)
			assert.Equal(t, "VALIDATION", env.Data.Error.Code)
			assert.Equal(t, tc.want, env.Data.Error.Message)
		})
	}
}
