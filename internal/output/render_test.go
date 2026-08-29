package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestRenderJSONSingleDocument(t *testing.T) {
	var buf bytes.Buffer
	env := NewEnvelope("ClusterList", []map[string]string{{"id": "c-x", "name": "prod"}})
	require.NoError(t, Render(&buf, "json", env))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got)) // must be ONE document
	assert.Equal(t, "cli.rancher.io/v1", got["apiVersion"])
	assert.Equal(t, "ClusterList", got["kind"])
	assert.NotContains(t, got, "tier", "tier/stability are no longer in the envelope")
	assert.NotContains(t, got, "contract")
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

// wideRows is a WideTabular fixture: Columns/Rows are the narrow table view,
// WideColumns/WideRows add one extra column.
type wideRows struct{}

func (wideRows) Columns() []string     { return []string{"NAME"} }
func (wideRows) Rows() [][]string      { return [][]string{{"acme"}} }
func (wideRows) WideColumns() []string { return []string{"NAME", "URL"} }
func (wideRows) WideRows() [][]string  { return [][]string{{"acme", "https://a"}} }

var _ WideTabular = wideRows{}

// TestRenderWidePrefersWideTabular pins the superset render and column
// order: -o wide on a WideTabular payload uses WideColumns/WideRows, not
// the narrow Columns/Rows.
func TestRenderWidePrefersWideTabular(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "wide", NewEnvelope("ServerList", wideRows{})))
	out := buf.String()
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "URL")
	assert.Contains(t, out, "acme")
	assert.Contains(t, out, "https://a")
}

// TestRenderWideFallsBackToTabularByteIdentical pins the no-wide-columns
// contract: a plain Tabular payload (no WideTabular) renders byte-identical
// under -o wide and -o table.
func TestRenderWideFallsBackToTabularByteIdentical(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("ServerList", namedRows{rows: [][]string{{"acme", "https://a"}}})
	var wide, table bytes.Buffer
	require.NoError(t, Render(&wide, "wide", env))
	require.NoError(t, Render(&table, "table", env))
	assert.Equal(t, table.String(), wide.String())
}

// TestRenderWideAndTableRejectNonTabular pins the closed-world contract:
// there is no pretty-JSON fallback for -o table/-o wide. A payload that is
// neither WideTabular nor Tabular is a rendering error for both formats —
// the caller's declared shape promised Tabular and its Data does not honor
// that.
func TestRenderWideAndTableRejectNonTabular(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]string{"a": "b"})
	var wide, table bytes.Buffer
	require.Error(t, Render(&wide, "wide", env))
	require.Error(t, Render(&table, "table", env))
}

func TestRenderTemplatePreservesIntegerPrecision(t *testing.T) {
	t.Parallel()
	type widget struct {
		Bytes int64 `json:"bytes"`
	}
	var buf bytes.Buffer
	env := &Envelope{Data: widget{Bytes: 1073741824}}
	require.NoError(t, Render(&buf, "template={{.bytes}}", env))
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
	require.NoError(t, Render(&buf, "template={{.displayName}}", env))
	assert.Equal(t, "w1\n", buf.String(),
		"templates must see the serialized (json-tag) shape, not Go field names")

	// A json.Marshaler-only payload (like resources.table) must be addressable too.
	buf.Reset()
	env = &Envelope{Data: onlyMarshaler{}}
	require.NoError(t, Render(&buf, "template={{.name}}", env))
	assert.Equal(t, "m\n", buf.String())
}

func TestRenderTemplatePrefixRequired(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	env := &Envelope{Data: map[string]string{"name": "w1"}}
	require.NoError(t, Render(&buf, TemplatePrefix+"{{.name}}", env))
	assert.Equal(t, "w1\n", buf.String())
}

func TestRenderRejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"csv", "template", "{{.name}}"} {
		t.Run("format="+format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			err := Render(&buf, format, &Envelope{Data: map[string]string{"name": "w1"}})
			require.Error(t, err)
			require.ErrorContains(t, err, "unsupported output format")
			assert.Empty(t, buf.String())
		})
	}
}

// TestRenderTemplateEmptySource: a bare "template=" must be an error, not a
// silent bare-newline render — the pipeline's validateFormat rejects it, and
// Render's defense-in-depth contract for direct callers must match.
func TestRenderTemplateEmptySource(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(&buf, TemplatePrefix, &Envelope{Data: map[string]string{"name": "w1"}})
	require.Error(t, err)
	require.ErrorContains(t, err, "empty template source")
	assert.Empty(t, buf.String(), "an empty template source must render nothing")
}

func TestRenderTemplateInvalidSource(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(&buf, TemplatePrefix+"{{", &Envelope{Data: map[string]string{"name": "w1"}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid template:")
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
	require.NoError(t, encodeJSON(&buf, map[string]string{"desc": escapeProbe}))
	assertNotHTMLEscaped(t, buf.String())
}

func TestRenderJSONStripsManagedFields(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]any{
		"metadata": map[string]any{
			"name":          "cm<&>",
			"managedFields": []any{map[string]any{"manager": "k3s"}},
		},
	})
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "json", env))
	out := buf.String()
	assert.NotContains(t, out, "managedFields")
	assert.Contains(t, out, "\n  \"data\"", "indentation preserved on the slow path (MF-2)")
	assert.Contains(t, out, "cm<&>", "HTML escaping stays disabled on the slow path")
	assert.True(t, strings.HasSuffix(out, "\n"), "trailing newline preserved")
	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	meta := got["data"].(map[string]any)["metadata"].(map[string]any)
	assert.Equal(t, "cm<&>", meta["name"], "neighboring metadata survives")
}

func TestRenderYAMLStripsManagedFields(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]any{
		"metadata": map[string]any{
			"name":          "cm",
			"managedFields": []any{map[string]any{"manager": "k3s"}},
		},
	})
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "yaml", env))
	assert.NotContains(t, buf.String(), "managedFields")
	assert.Contains(t, buf.String(), "name: cm")
}

func TestRenderFastPathByteIdentical(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]any{
		"metadata": map[string]any{"name": "clean<&>"},
	})
	var got bytes.Buffer
	require.NoError(t, Render(&got, "json", env))
	var want bytes.Buffer
	require.NoError(t, encodeJSON(&want, env))
	assert.Equal(t, want.String(), got.String(),
		"no managedFields: output is byte-identical to today's encoder (MF-2 fast path)")

	var gotYAML bytes.Buffer
	require.NoError(t, Render(&gotYAML, "yaml", env))
	wantYAML, err := yaml.Marshal(env)
	require.NoError(t, err)
	assert.Equal(t, string(wantYAML), gotYAML.String(),
		"no managedFields: yaml output is byte-identical to yaml.Marshal (MF-2 fast path)")
}

func TestRenderTemplateRootAndStrip(t *testing.T) {
	t.Parallel()
	envDirty := NewEnvelope("X", map[string]any{
		"name": "root-field",
		"obj": map[string]any{
			"metadata": map[string]any{"managedFields": "x", "uid": "u1"},
		},
	})
	// The template interrogates the managedFields key itself: this MUST
	// render "absent" only once the template branch scrubs its view, so
	// the test is red before Step 7 (MF-7).
	const probe = "template={{.name}}/{{.obj.metadata.uid}}/" +
		"{{if index .obj.metadata \"managedFields\"}}present{{else}}absent{{end}}"
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, probe, envDirty))
	assert.Equal(t, "root-field/u1/absent\n", buf.String(),
		"root stays env.Data AND managedFields is gone from the template view (MF-2)")

	envClean := NewEnvelope("X", map[string]any{"name": "root-field"})
	buf.Reset()
	require.NoError(t, Render(&buf, "template={{.name}}", envClean))
	assert.Equal(t, "root-field\n", buf.String(), "fast path root unchanged")
}

// TestRenderTableRejectsNonTabularEffectPayload pins the closed-world
// contract over the same shape the old pretty-JSON fallback used to paper
// over: a non-Tabular payload (e.g. an Effect preview) is a rendering error
// under table/default format, never a JSON dump. The scrubbing behavior
// these fixtures used to exercise for that dump is still covered for
// json/yaml/template by TestRenderJSONStripsManagedFields and its siblings.
func TestRenderTableRejectsNonTabularEffectPayload(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		"observed": map[string]any{
			"metadata": map[string]any{
				"name":          "x",
				"managedFields": []any{map[string]any{"manager": "k3s"}},
			},
		},
	}
	for _, format := range []string{"", "table"} {
		name := format
		if name == "" {
			name = "empty"
		}
		t.Run("format="+name, func(t *testing.T) {
			t.Parallel()
			env := NewEnvelope("Effect", payload)
			var buf bytes.Buffer
			assert.Error(t, Render(&buf, format, env))
		})
	}
}

type marshalBomb struct{}

func (marshalBomb) Columns() []string { return []string{"NAME"} }
func (marshalBomb) Rows() [][]string  { return [][]string{{"a"}} }
func (marshalBomb) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("must never be marshaled by table/name")
}

func TestRenderTableAndNameNeverMarshal(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", marshalBomb{})
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "table", env), "table renders via Tabular alone (MF-2 step 0)")
	assert.Contains(t, buf.String(), "NAME")
	buf.Reset()
	require.NoError(t, Render(&buf, "name", env))
	assert.Equal(t, "a\n", buf.String())
	buf.Reset()
	err := Render(&buf, "bogus-format", env)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported output format",
		"format error precedes any marshal")
}
