package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// renderWithPayload is tabular data that holds HTML characters, U+2028,
// non-ASCII text and, when metadata is set, a managedFields entry.
type renderWithPayload struct {
	Name     string         `json:"name"`
	Note     string         `json:"note"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (p renderWithPayload) Columns() []string { return []string{"NAME", "NOTE"} }
func (p renderWithPayload) Rows() [][]string  { return [][]string{{p.Name, p.Note}} }

// renderWithEnvelopes are two envelopes: one whose data holds no managedFields
// and one whose data does, so both branches of renderSource run.
func renderWithEnvelopes() map[string]*Envelope {
	plain := renderWithPayload{Name: "a<b>&c", Note: "line\u2028break caf\u00e9 \u4e16\u754c"}
	managed := plain
	managed.Metadata = map[string]any{
		"name":          "x",
		"managedFields": []any{map[string]any{"manager": "kubectl", "fieldsV1": map[string]any{"f:spec": map[string]any{}}}},
	}
	return map[string]*Envelope{
		"no managedFields": NewEnvelope("Report", plain),
		"managedFields":    NewEnvelope("Report", managed),
	}
}

// writeCounter records each Write it receives.
type writeCounter struct{ writes [][]byte }

func (w *writeCounter) Write(b []byte) (int, error) {
	w.writes = append(w.writes, bytes.Clone(b))
	return len(b), nil
}

// TestRenderWithNoRewriteWritesWhatTheEncodersWrite asserts, for data with and
// without a managedFields entry, that a nil rewrite and a rewrite that returns
// its input write the bytes of the json encoder of the package for json and
// the bytes of yaml.Marshal for yaml, with HTML characters, U+2028 and
// non-ASCII text in the data.
func TestRenderWithNoRewriteWritesWhatTheEncodersWrite(t *testing.T) {
	t.Parallel()
	for name, env := range renderWithEnvelopes() {
		src, err := renderSource(env)
		require.NoError(t, err, name)
		var wantJSON bytes.Buffer
		require.NoError(t, encodeJSON(&wantJSON, src), name)
		wantYAML, err := yaml.Marshal(src)
		require.NoError(t, err, name)

		for format, want := range map[string][]byte{"json": wantJSON.Bytes(), "yaml": wantYAML} {
			for rewriteName, rewrite := range map[string]func([]byte) []byte{
				"nil rewrite":      nil,
				"identity rewrite": func(b []byte) []byte { return b },
			} {
				var got bytes.Buffer
				require.NoError(t, RenderWith(&got, format, env, style.Style{}, rewrite), "%s %s %s", name, format, rewriteName)
				assert.Equal(t, string(want), got.String(), "%s %s %s", name, format, rewriteName)
			}
		}
		var viaRenderStyled bytes.Buffer
		require.NoError(t, RenderStyled(&viaRenderStyled, "json", env, style.Style{}))
		assert.Equal(t, wantJSON.String(), viaRenderStyled.String(), name)
	}
}

// TestRenderWithCallsRewriteForJSONAndYAMLOnly asserts the rewrite is called
// once for json, with the whole indented JSON document, and once for yaml, with
// the JSON the yaml is converted from, and never for table, wide, name or a
// template; and that json and yaml each make one Write.
func TestRenderWithCallsRewriteForJSONAndYAMLOnly(t *testing.T) {
	t.Parallel()
	for name, env := range renderWithEnvelopes() {
		for _, format := range []string{"", "table", "wide", "name", "json", "yaml", "template={{.name}}"} {
			var seen [][]byte
			var out writeCounter
			rewrite := func(b []byte) []byte {
				seen = append(seen, bytes.Clone(b))
				return b
			}
			require.NoError(t, RenderWith(&out, format, env, style.Style{}, rewrite), "%s %q", name, format)
			switch format {
			case "json", "yaml":
				require.Len(t, seen, 1, "%s %s: one call", name, format)
				assert.True(t, json.Valid(seen[0]), "%s %s: the rewrite gets a JSON document", name, format)
				assert.Len(t, out.writes, 1, "%s %s: one Write", name, format)
			default:
				assert.Empty(t, seen, "%s %q: the format is not rewritten", name, format)
			}
		}
	}
}

// TestRenderWithWritesWhatTheRewriteReturns asserts the bytes the rewrite
// returns are the bytes written for json, and the JSON the yaml is converted
// from for yaml.
func TestRenderWithWritesWhatTheRewriteReturns(t *testing.T) {
	t.Parallel()
	env := renderWithEnvelopes()["no managedFields"]
	replaced := func([]byte) []byte { return []byte(`{"replaced": true}`) }
	var asJSON, asYAML bytes.Buffer
	require.NoError(t, RenderWith(&asJSON, "json", env, style.Style{}, replaced))
	assert.Equal(t, `{"replaced": true}`, asJSON.String())
	require.NoError(t, RenderWith(&asYAML, "yaml", env, style.Style{}, replaced))
	assert.Equal(t, "replaced: true\n", asYAML.String())
}
