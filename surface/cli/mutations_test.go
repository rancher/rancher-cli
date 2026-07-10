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

// errEnvelope decodes the {kind:"Error", data:{error:{...}}} envelope written
// to a command's error stream.
type errEnvelope struct {
	Data struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"data"`
}

// TestCreateResourceShortFlag exercises the real urfave tree: `create resource
// -f -` must parse the `-f` alias, read the manifest from stdin, and fail with
// VALIDATION on the unknown kind — proving the alias is wired end-to-end.
func TestCreateResourceShortFlag(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out, errBuf bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errBuf
	root.Reader = bytes.NewBufferString("apiVersion: x/v1\nkind: Widget\nmetadata:\n  name: w\n")
	// The default handler calls os.Exit on a non-zero ExitCoder, which would kill
	// the test process; keep the error as a return value instead.
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(context.Background(), []string{
		"rancher", "--config", dir, "create", "resource", "-f", "-", "--dry-run",
	})
	require.Error(t, runErr, "-f - must parse and the unsupported kind must fail validation")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "Widget")
}

// TestReadOnlyModeRefusesMutatingBeforeDispatch proves the read-only gate
// fires from the real urfave tree, before Plan (which performs real network
// calls unconditionally) ever runs — no -f/--apply is supplied, so if the
// gate did not fire first, some other VALIDATION (missing input) would
// surface instead.
func TestReadOnlyModeRefusesMutatingBeforeDispatch(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	t.Setenv("RANCHER_READONLY", "1")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out, errBuf bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errBuf
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(context.Background(), []string{"rancher", "--config", dir, "create", "resource"})
	require.Error(t, runErr)

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "read-only mode")
	assert.Contains(t, env.Data.Error.Message, "create resource")
}

// TestPatchShortFlag exercises the real urfave tree: `patch pod web -f ops.json`
// must parse the `-f` alias on a generated patch command. The missing file is a
// VALIDATION error, proving the alias reached the leaf (the flag parsed at all).
func TestPatchShortFlag(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out, errBuf bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errBuf
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(context.Background(), []string{
		"rancher", "--config", dir, "patch", "pod", "web",
		"-f", "does-not-exist.json", "--cluster", "local", "--namespace", "default", "--dry-run",
	})
	require.Error(t, runErr, "-f must parse on a generated patch command")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
}
