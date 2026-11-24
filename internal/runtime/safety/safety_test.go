package safety

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action Action
		want   bool
	}{
		{ActionCreate, true},
		{ActionUpdate, true},
		{ActionDelete, true},
		{Action(""), false},
		{Action("destroy"), false},
		{Action("scale"), false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.action.Valid(), "Action(%q).Valid()", tt.action)
	}
}

func TestEffectJSONShape(t *testing.T) {
	t.Parallel()
	eff := Effect{
		Action: ActionDelete, Target: "projects/web",
		Server: "prod", Cluster: "c-abc",
		ResourceVersion: "42", ConflictDetection: true,
		Destructive: true,
	}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "delete", m["action"])
	assert.Equal(t, "prod", m["server"])
	assert.Equal(t, "c-abc", m["cluster"])
	// contract and stability are NOT in the serialized Effect (manifest is
	// the single source).
	assert.NotContains(t, m, "contract")
	assert.NotContains(t, m, "stability")
	assert.NotContains(t, m, "tier")
}

// TestEffectWarningsNeverSerialize asserts the json:"-" tag deliberately kept on
// Warnings: the Effect marshals inside the envelope's data
// field, and renderEffect (internal/runtime/pipeline.go) copies these into
// the envelope's top-level warnings - serializing here too would emit every
// warning twice (data.warnings + warnings).
func TestEffectWarningsNeverSerialize(t *testing.T) {
	t.Parallel()
	eff := Effect{Action: ActionUpdate, Target: "x", Warnings: []string{"note-a"}}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.NotContains(t, m, "warnings")
	assert.NotContains(t, m, "Warnings")
}

// TestDiffEntryJSONFromUnread asserts an entry marks an unread stored value by
// leaving `from` out, keeps a null `from` for a field that had no value, and
// writes its keys in the order path, from, to.
func TestDiffEntryJSONFromUnread(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry DiffEntry
		want  string
	}{
		{"unread with a nil from", DiffEntry{Path: "/a", To: "x", FromUnread: true}, `{"path":"/a","to":"x"}`},
		{"unread with a value in from", DiffEntry{Path: "/a", From: "old", To: "x", FromUnread: true}, `{"path":"/a","to":"x"}`},
		{"read with a nil from", DiffEntry{Path: "/a", To: "x"}, `{"path":"/a","from":null,"to":"x"}`},
		{"read with a value in from", DiffEntry{Path: "/a", From: "old", To: "x"}, `{"path":"/a","from":"old","to":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tt.entry)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(b), "the key order is path, from, to")
			var m map[string]any
			require.NoError(t, json.Unmarshal(b, &m))
			_, hasFrom := m["from"]
			assert.Equal(t, !tt.entry.FromUnread, hasFrom)
			assert.NotContains(t, m, "FromUnread")
		})
	}
}

// TestDiffEntryJSONInAnEffect asserts the entries of an Effect marshal through
// DiffEntry.MarshalJSON, so a slice of entries keeps the contract.
func TestDiffEntryJSONInAnEffect(t *testing.T) {
	t.Parallel()
	eff := Effect{Action: ActionUpdate, Target: "x", Diff: []DiffEntry{
		{Path: "/a", To: "x", FromUnread: true},
		{Path: "/b", To: "y"},
	}}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	var got struct {
		Diff []map[string]any `json:"diff"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got.Diff, 2)
	assert.NotContains(t, got.Diff[0], "from")
	assert.Contains(t, got.Diff[1], "from")
}

// TestDiffEntryJSONKeepsHTMLCharactersLiteral asserts the rendered bytes of a
// diff keep `<`, `>` and `&` as they are, in both forms of an entry. The
// bytes are asserted on purpose: a decoder reads `<` and \u003c as the same
// value, but the JSON renderer writes them literally and an entry must not
// change that. The test never marshals the Effect with json.Marshal, which
// escapes them.
func TestDiffEntryJSONKeepsHTMLCharactersLiteral(t *testing.T) {
	t.Parallel()
	eff := Effect{Action: ActionUpdate, Target: "x", Diff: []DiffEntry{
		{Path: "/a", To: "<redacted>", FromUnread: true},
		{Path: "/b", From: "a&b", To: "<redacted> a&b"},
	}}
	var buf bytes.Buffer
	require.NoError(t, output.Render(&buf, "json", output.NewEnvelope("Effect", eff)))
	out := buf.String()
	assert.Contains(t, out, `"to": "<redacted>"`)
	assert.Contains(t, out, `"from": "a&b"`)
	assert.Contains(t, out, `"to": "<redacted> a&b"`)
	assert.NotContains(t, out, `\u003c`)
	assert.NotContains(t, out, `\u003e`)
	assert.NotContains(t, out, `\u0026`)

	var got struct {
		Data struct {
			Diff []map[string]any `json:"diff"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got.Data.Diff, 2)
	assert.Equal(t, "<redacted>", got.Data.Diff[0]["to"])
	assert.NotContains(t, got.Data.Diff[0], "from")
	assert.Equal(t, "a&b", got.Data.Diff[1]["from"])
	assert.Equal(t, "<redacted> a&b", got.Data.Diff[1]["to"])
}
