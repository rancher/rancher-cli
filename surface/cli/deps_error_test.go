package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestResolveDepsFailureRendersError pins the non-bare Action's resolveDeps
// failure path: an invalid credential-store policy renders a structured
// error on stderr — the envelope under -o json, the text block otherwise —
// and exits 1, never a bare non-zero exit with an empty stderr.
func TestResolveDepsFailureRendersError(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "bogus")
	dir := t.TempDir()

	run := func(t *testing.T, args ...string) (*bytes.Buffer, error) {
		t.Helper()
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		// The default handler calls os.Exit on a non-zero ExitCoder, which
		// would kill the test process; keep the error as a return value.
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
		runErr := root.Run(t.Context(), append([]string{"rancher", "--config", dir}, args...))
		return &errOut, runErr
	}

	t.Run("json envelope", func(t *testing.T) {
		errOut, runErr := run(t, "-o", "json", "config", "path")
		require.Error(t, runErr)
		var ec ucli.ExitCoder
		require.ErrorAs(t, runErr, &ec)
		assert.Equal(t, 1, ec.ExitCode())
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "GENERIC", env.Data.Error.Code)
		assert.Contains(t, env.Data.Error.Message, "bogus")
	})

	t.Run("default text", func(t *testing.T) {
		errOut, runErr := run(t, "config", "path")
		require.Error(t, runErr)
		var ec ucli.ExitCoder
		require.ErrorAs(t, runErr, &ec)
		assert.Equal(t, 1, ec.ExitCode())
		assert.Contains(t, errOut.String(), "error (GENERIC):")
		assert.Contains(t, errOut.String(), "bogus")
	})
}
