package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderJSONSingleDocument(t *testing.T) {
	var buf bytes.Buffer
	env := NewEnvelope("ClusterList", []map[string]string{{"id": "c-x", "name": "prod"}})
	env.Tier, env.Stability = "crd", "beta"
	require.NoError(t, Render(&buf, "json", env))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got)) // must be ONE document
	assert.Equal(t, "cli.rancher.io/v1", got["apiVersion"])
	assert.Equal(t, "ClusterList", got["kind"])
	assert.Equal(t, "crd", got["tier"])
}

func TestRenderYAML(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "yaml", NewEnvelope("X", map[string]string{"a": "b"})))
	assert.Contains(t, buf.String(), "apiVersion: cli.rancher.io/v1")
}

type namedRows struct{ rows [][]string }

func (n namedRows) Columns() []string { return []string{"NAME", "URL"} }
func (n namedRows) Rows() [][]string  { return n.rows }

func TestRenderNameEmitsNameColumn(t *testing.T) {
	var buf bytes.Buffer
	env := NewEnvelope("ServerList", namedRows{rows: [][]string{{"acme", "https://a"}, {"beta", "https://b"}}})
	require.NoError(t, Render(&buf, "name", env))
	assert.Equal(t, "acme\nbeta\n", buf.String())
}

func TestRenderNameNoNameColumnIsEmpty(t *testing.T) {
	var buf bytes.Buffer
	env := NewEnvelope("X", map[string]string{"a": "b"}) // not Tabular
	require.NoError(t, Render(&buf, "name", env))
	assert.Empty(t, buf.String())
}
