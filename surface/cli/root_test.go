package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaCommandEndToEnd(t *testing.T) {
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
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "login", "https://r.ex",
		"--token", "id:secret", "--name", "acme", "--context", "c-1:p-1", "-o", "json",
	}))

	out.Reset()
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "context", "current", "-o", "json",
	}))
	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Context", env["kind"])
}
