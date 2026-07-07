package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaCommandEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	err = root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &m))
	data := m["data"].(map[string]any)
	assert.Equal(t, "cli.rancher.io/v1", data["outputApiVersion"])
	assert.NotEmpty(t, data["globalFlags"])
}

func TestLoginThenContextCurrentEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.Reader = bytes.NewBufferString("id:secret\n")

	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "auth", "login", "--server", "https://r.ex",
		"--token-stdin", "--name", "acme", "--context", "c-1:p-1", "-o", "json",
	}))

	_, statErr := os.Stat(filepath.Join(dir, "config.yaml"))
	require.NoError(t, statErr, "login must write config.yaml under --config dir")

	_, statErr2 := os.Stat(filepath.Join(dir, "cli2.json"))
	require.True(t, os.IsNotExist(statErr2), "must not write cli2.json")

	out.Reset()
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "config", "current-context", "-o", "json",
	}))
	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Context", env["kind"])
}
