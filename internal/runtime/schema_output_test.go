package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestSchemaViewMarshalDelegation is the SE-19 regression guard: the schema
// view marshals byte-identically to the manifest it wraps under both json and
// yaml (the view adds the -o name contract WITHOUT changing structured
// output), and the default/table path still renders Result.Text, never the
// view's NAME table.
func TestSchemaViewMarshalDelegation(t *testing.T) {
	t.Parallel()
	m := kindRegistry().Schema("test", "2.14")
	view := newSchemaView(m)

	gotJSON, err := json.Marshal(view)
	require.NoError(t, err)
	wantJSON, err := json.Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, wantJSON, gotJSON, "json of the view is byte-identical to the manifest")

	gotYAML, err := yaml.Marshal(view)
	require.NoError(t, err)
	wantYAML, err := yaml.Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, wantYAML, gotYAML, "yaml of the view is byte-identical to the manifest")

	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	for _, format := range []string{"", "table"} {
		var out, errBuf bytes.Buffer
		_, ce := Run(context.Background(), cmd, tin{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
		require.Nil(t, ce)
		assert.True(t, strings.HasPrefix(out.String(), "rancher test — "),
			"format %q renders the TOC Text, not the view's table", format)
		assert.NotContains(t, out.String(), "NAME", "the view's NAME column must not render under %q", format)
	}
}

// TestSchemaFormatCompat is the SE-2 matrix for W4: every selection shape
// (bare, command, kind, filtered-to-zero) rendered under json, yaml, name, and
// template, asserted on decoded fields. `-o name` emits the selection's name
// set one per line — for bare and filtered selections that is the command
// names (NEW: it previously emitted nothing).
func TestSchemaFormatCompat(t *testing.T) {
	t.Parallel()
	m := kindRegistry().Schema("test", "2.14")
	allNames := commandNames(m)
	require.NotEmpty(t, allNames)

	type shapeTC struct {
		name      string
		in        tin
		wantCmds  []string // expected data.commands names (schemaView) or commandCount (tocView)
		wantNames []string // expected -o name lines
		wantKind  string   // expected kind name for kind-row shape
		isTOC     bool     // bare/filtered selections use tocView, not schemaView
		isCard    bool     // command selections use cardView (flattened name at top level)
		isKind    bool     // kind selections use kindView (kind row at top level)
	}

	shapes := []shapeTC{
		{
			name: "bare", in: tin{},
			wantCmds: allNames, wantNames: allNames, isTOC: true,
		},
		{
			name: "command selection", in: tin{a: []string{"kubeconfig"}},
			wantCmds: []string{"kubeconfig"}, wantNames: []string{"kubeconfig"}, isCard: true,
		},
		{
			name: "kind selection", in: tin{a: []string{"crt"}},
			wantNames: []string{"clusterregistrationtoken"},
			wantKind:  "clusterregistrationtoken",
			isKind:    true,
		},
		{
			name: "zero hit", in: tin{strs: map[string]string{"search": "zzz"}},
			wantCmds: []string{}, wantNames: nil, isTOC: true,
		},
	}

	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	run := func(t *testing.T, in tin, format string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		_, ce := Run(context.Background(), cmd, in, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
		require.Nil(t, ce, "stderr: %s", errBuf.String())
		return out.String()
	}
	// assertCardData checks the cardView JSON shape used by command selections:
	// the command's fields are flattened to the top level — name at root, no
	// commands wrapper array — plus globalExitCodes always present.
	assertCardData := func(t *testing.T, data map[string]any, tc shapeTC) {
		t.Helper()
		require.Len(t, tc.wantCmds, 1, "card selection must have exactly one command name")
		assert.Equal(t, tc.wantCmds[0], data["name"])
		assert.Contains(t, data, "globalExitCodes")
		assert.NotContains(t, data, "commands", "flattened: no commands wrapper array")
	}
	// assertKindData checks the kindView JSON shape used by kind selections:
	// the kind's fields are at top level — name, verbs — no commands or groups wrapper.
	assertKindData := func(t *testing.T, data map[string]any, tc shapeTC) {
		t.Helper()
		assert.Equal(t, tc.wantKind, data["name"])
		_, ok := data["verbs"].(map[string]any)
		assert.True(t, ok, "kind row carries verbs")
		assert.NotContains(t, data, "commands", "kind-row carries no commands wrapper")
		assert.NotContains(t, data, "groups", "kind-row carries no groups wrapper")
	}
	// assertTOCData checks the tocView JSON shape used by bare and filtered selections:
	// commandCount instead of a commands array, groups with name+usage entries.
	assertTOCData := func(t *testing.T, data map[string]any, tc shapeTC) {
		t.Helper()
		count, ok := data["commandCount"].(float64)
		require.True(t, ok, "tocView data must have commandCount, got %T", data["commandCount"])
		assert.Equal(t, len(tc.wantCmds), int(count))
		if len(tc.wantCmds) == 0 {
			groups, _ := data["groups"].([]any)
			assert.Empty(t, groups, "zero hit carries no groups")
		}
	}

	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("json", func(t *testing.T) {
				t.Parallel()
				env := decodeEnvelope(t, []byte(run(t, tc.in, "json")))
				assert.Equal(t, "Schema", env["kind"])
				data, ok := env["data"].(map[string]any)
				require.True(t, ok)
				switch {
				case tc.isCard:
					assertCardData(t, data, tc)
				case tc.isTOC:
					assertTOCData(t, data, tc)
				case tc.isKind:
					assertKindData(t, data, tc)
				}
			})

			t.Run("yaml", func(t *testing.T) {
				t.Parallel()
				var env map[string]any
				require.NoError(t, yaml.Unmarshal([]byte(run(t, tc.in, "yaml")), &env))
				assert.Equal(t, "Schema", env["kind"])
				data, ok := env["data"].(map[string]any)
				require.True(t, ok)
				switch {
				case tc.isCard:
					assertCardData(t, data, tc)
				case tc.isTOC:
					assertTOCData(t, data, tc)
				case tc.isKind:
					assertKindData(t, data, tc)
				}
			})

			t.Run("name", func(t *testing.T) {
				t.Parallel()
				out := run(t, tc.in, "name")
				if len(tc.wantNames) == 0 {
					assert.Empty(t, out, "-o name emits nothing for an empty selection")
					return
				}
				assert.Equal(t, strings.Join(tc.wantNames, "\n")+"\n", out)
			})

			t.Run("template", func(t *testing.T) {
				t.Parallel()
				var tpl, want string
				switch {
				case tc.isCard:
					require.Len(t, tc.wantCmds, 1)
					tpl = "template={{ .name }}"
					want = tc.wantCmds[0] + "\n"
				case tc.isTOC:
					tpl = "template={{ .commandCount }}"
					want = fmt.Sprintf("%d\n", len(tc.wantCmds))
				case tc.isKind:
					tpl = "template={{ .name }}"
					want = tc.wantKind + "\n"
				}
				out := run(t, tc.in, tpl)
				assert.Equal(t, want, out,
					"templates render over the selection's JSON view")
			})
		})
	}
}
