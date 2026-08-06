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
	view := newSchemaView(m, commandNames(m))

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

	y, err := view.MarshalYAML()
	require.NoError(t, err)
	assert.Equal(t, m, y, "MarshalYAML delegates to the wrapped manifest")

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
		wantKind  string   // expected single kinds entry, "" = kinds untouched
		isTOC     bool     // bare/filtered selections use tocView, not schemaView
	}

	shapes := []shapeTC{
		{
			name: "bare", in: tin{},
			wantCmds: allNames, wantNames: allNames, isTOC: true,
		},
		{
			name: "command selection", in: tin{a: []string{"kubeconfig"}},
			wantCmds: []string{"kubeconfig"}, wantNames: []string{"kubeconfig"},
		},
		{
			name: "kind selection", in: tin{a: []string{"crt"}},
			wantCmds:  []string{"get clusterregistrationtoken", "patch clusterregistrationtoken"},
			wantNames: []string{"clusterregistrationtoken"},
			wantKind:  "clusterregistrationtoken",
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
	assertData := func(t *testing.T, data map[string]any, tc shapeTC) {
		t.Helper()
		cmds, ok := data["commands"].([]any)
		require.True(t, ok, "data.commands must be an array, got %T", data["commands"])
		names := make([]string, len(cmds))
		for i, c := range cmds {
			names[i] = c.(map[string]any)["name"].(string)
		}
		assert.ElementsMatch(t, tc.wantCmds, names)
		if tc.wantKind != "" {
			kinds, ok := data["kinds"].([]any)
			require.True(t, ok)
			require.Len(t, kinds, 1)
			assert.Equal(t, tc.wantKind, kinds[0].(map[string]any)["name"])
		}
		if len(tc.wantCmds) == 0 {
			groups, _ := data["groups"].([]any)
			assert.Empty(t, groups, "zero hit carries no groups")
		}
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
				if tc.isTOC {
					assertTOCData(t, data, tc)
				} else {
					assertData(t, data, tc)
				}
			})

			t.Run("yaml", func(t *testing.T) {
				t.Parallel()
				var env map[string]any
				require.NoError(t, yaml.Unmarshal([]byte(run(t, tc.in, "yaml")), &env))
				assert.Equal(t, "Schema", env["kind"])
				data, ok := env["data"].(map[string]any)
				require.True(t, ok)
				if tc.isTOC {
					assertTOCData(t, data, tc)
				} else {
					assertData(t, data, tc)
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
				if tc.isTOC {
					tpl = "template={{ .commandCount }}"
					want = fmt.Sprintf("%d\n", len(tc.wantCmds))
				} else {
					tpl = "template={{len .commands}}"
					want = fmt.Sprintf("%d\n", len(tc.wantCmds))
				}
				out := run(t, tc.in, tpl)
				assert.Equal(t, want, out,
					"templates render over the selection's JSON view")
			})
		})
	}
}
