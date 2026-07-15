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

func TestRenderJSONNoHTMLEscape(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "json", NewEnvelope("Hint", map[string]string{"hint": "--cluster <c> a&b"})))
	out := buf.String()
	assert.Contains(t, out, "<c>", "`<`/`>` must stay literal")
	assert.Contains(t, out, "a&b", "`&` must stay literal")
	assert.NotContains(t, out, "\\u003c", "must not HTML-escape the < character")
	assert.NotContains(t, out, "\\u0026", "must not HTML-escape the & character")
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

func TestRenderTemplatePreservesIntegerPrecision(t *testing.T) {
	t.Parallel()
	type widget struct {
		Bytes int64 `json:"bytes"`
	}
	var buf bytes.Buffer
	env := &Envelope{Data: widget{Bytes: 1073741824}}
	require.NoError(t, Render(&buf, "{{.bytes}}", env))
	assert.Equal(t, "1073741824\n", buf.String(),
		"a large int64 must round-trip through the JSON view verbatim, not as scientific notation")
}

type onlyMarshaler struct{}

func (onlyMarshaler) MarshalJSON() ([]byte, error) { return []byte(`{"name":"m"}`), nil }

func TestRenderTemplateUsesJSONView(t *testing.T) {
	t.Parallel()

	type widget struct {
		DisplayName string `json:"displayName"`
	}
	var buf bytes.Buffer
	env := &Envelope{Data: widget{DisplayName: "w1"}}
	require.NoError(t, Render(&buf, "{{.displayName}}", env))
	assert.Equal(t, "w1\n", buf.String(),
		"templates must see the serialized (json-tag) shape, not Go field names")

	// A json.Marshaler-only payload (like resources.table) must be addressable too.
	buf.Reset()
	env = &Envelope{Data: onlyMarshaler{}}
	require.NoError(t, Render(&buf, "{{.name}}", env))
	assert.Equal(t, "m\n", buf.String())
}

// TestRenderJSONNoHTMLEscape above pins the -o json path. The two below cover
// the paths it does not: the shared EncodeJSON helper and the default/table
// fallback for a non-Tabular payload — the fallback is what actually regressed
// (bare `schema` emitted the escaped form for the `>` in descriptions).

const escapeProbe = "a < b > c & d"

func assertNotHTMLEscaped(t *testing.T, out string) {
	t.Helper()
	assert.Contains(t, out, escapeProbe, "raw < > & must survive verbatim")
	// Go's default JSON encoder escapes <, >, and & into their unicode form;
	// the u003c/u003e/u0026 substrings below appear only if that escaping ran.
	assert.NotContains(t, out, "u003c", "must not HTML-escape <")
	assert.NotContains(t, out, "u003e", "must not HTML-escape >")
	assert.NotContains(t, out, "u0026", "must not HTML-escape &")
}

func TestEncodeJSONKeepsAngleAndAmpLiteral(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, EncodeJSON(&buf, map[string]string{"desc": escapeProbe}))
	assertNotHTMLEscaped(t, buf.String())
}

func TestRenderTableFallbackKeepsAngleAndAmpLiteral(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "table"} {
		format := format
		name := format
		if name == "" {
			name = "empty"
		}
		t.Run("format="+name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			env := NewEnvelope("probe", map[string]string{"desc": escapeProbe})
			require.NoError(t, Render(&buf, format, env))
			assertNotHTMLEscaped(t, buf.String())
		})
	}
}
