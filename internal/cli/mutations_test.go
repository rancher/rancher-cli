package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
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
			Hint    string `json:"hint"`
		} `json:"error"`
	} `json:"data"`
}

// TestCreateResourceShortFlag exercises the real urfave tree: `create resource
// -f -` must parse the `-f` alias, read the manifest from stdin, and fail with
// VALIDATION on the unknown kind - proving the alias is wired end-to-end.
func TestCreateResourceShortFlag(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
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

	runErr := root.Run(t.Context(), []string{
		"rancher", "--config", dir, "-o", "json", "create", "resource", "-f", "-", "--dry-run",
	})
	require.Error(t, runErr, "-f - must parse and the unsupported kind must fail validation")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "Widget")
}

// TestReadOnlyModeRefusesMutatingBeforeDispatch proves the read-only gate
// fires from the real urfave tree, before Plan (which performs real network
// calls unconditionally) ever runs - no -f/--apply is supplied, so if the
// gate did not fire first, some other VALIDATION (missing input) would
// surface instead.
func TestReadOnlyModeRefusesMutatingBeforeDispatch(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv("RANCHER_CLI_READONLY", "1")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out, errBuf bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errBuf
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(t.Context(), []string{"rancher", "--config", dir, "-o", "json", "create", "resource"})
	require.Error(t, runErr)

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "read-only mode")
	assert.Contains(t, env.Data.Error.Message, "create resource")
}

// TestPatchFlagSpellings asserts on the real urfave tree that a generated
// patch command takes kubectl's spellings of its two input flags: -p is
// --patch and --patch-file names the patch file, each reaching the leaf
// (the patch is decoded or the file read), while -f and --filename are
// unknown flags, which a command that takes a secret input names by their
// position on the command line, not as typed.
func TestPatchFlagSpellings(t *testing.T) {
	restoreRawArgv(t)
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for _, tc := range []struct {
		name string
		tail []string
		want string
	}{
		{"-p is --patch", []string{"-p", "{}"}, "invalid patch: "},
		{"--patch-file reads the file", []string{"--patch-file", "does-not-exist.json"}, `cannot read patch "does-not-exist.json"`},
		{"-f is unknown", []string{"-f", "ops.json"}, "unknown flag at word 8 after rancher"},
		{"--filename is unknown", []string{"--filename", "ops.json"}, "unknown flag at word 8 after rancher"},
		{"--type takes json or merge", []string{"--type", "strategic", "-p", "{}"}, "--type value is not one of json|merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			argv := slices.Concat([]string{"--config", t.TempDir(), "-o", "json", "patch", "pod", "web"}, tc.tail,
				[]string{"--cluster", "local", "--namespace", "default", "--dry-run"})
			stderr, code := runAsMain(t, root, argv)
			assert.Equal(t, 2, code)

			var env errEnvelope
			require.NoError(t, json.Unmarshal([]byte(stderr), &env))
			assert.Equal(t, "VALIDATION", env.Data.Error.Code)
			assert.Contains(t, env.Data.Error.Message, tc.want)
		})
	}
}
