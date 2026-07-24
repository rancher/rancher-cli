package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// tin is a minimal Input stub for tests that only need positional Args.
type tin struct{ a []string }

func (t tin) String(string) string { return "" }
func (t tin) Bool(string) bool     { return false }
func (t tin) Args() []string       { return t.a }

func TestSchemaFilter(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", Aliases: []string{"pods"}, CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	t.Run("no args returns the full manifest", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
		require.NoError(t, err)
		m := res.Data.(Manifest)
		assert.Len(t, m.Commands, 2)
	})

	t.Run("multi-word name filters to one", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		m := res.Data.(Manifest)
		require.Len(t, m.Commands, 1)
		assert.Equal(t, "get pod", m.Commands[0].Name)
		assert.NotEmpty(t, m.ErrorCodes, "filtered manifest keeps the shared context")
	})

	t.Run("alias matches", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pods"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, "get pod", res.Data.(Manifest).Commands[0].Name)
	})

	t.Run("unknown command is VALIDATION", func(t *testing.T) {
		_, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "nonsense"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

// cardRegistry builds a registry with one readonly command, one mutating
// command with a resolver binding, and ordered groups — enough surface to
// exercise selection, the card, and structured output.
func cardRegistry() *Registry {
	reg := NewRegistry(stubResolver{
		"delete project": {Tier: safety.TierNorman, Stability: safety.Deprecated},
	})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get pod", Aliases: []string{"pods", "po"}, Usage: "List pods or show one",
		Args: []ArgSpec{{Name: "name", Type: "string"}}, CommandType: ReadOnly,
		Network: true, Auth: true,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get project", Usage: "List projects", CommandType: ReadOnly,
		Network: true, Auth: true,
	}})
	reg.Register(fakeMutating{d: Descriptor{
		Name: "delete project", Usage: "Delete a project",
		Args:        []ArgSpec{{Name: "name", Type: "string", Required: true}},
		CommandType: Mutating, Destructive: "true", Network: true, Auth: true,
	}})
	reg.SetGroups([]SchemaGroup{
		{Name: "", Commands: []string{"schema"}},
		{Name: "get", Commands: []string{"get pod", "get project"}},
		{Name: "delete", Commands: []string{"delete project"}},
	})
	return reg
}

// TestSchemaSelectionSetsCardText covers SE-1's wiring: selecting a command
// yields a Result whose Text is the card (rendered under default/table) while
// Data keeps the filtered manifest for the structured formats.
func TestSchemaSelectionSetsCardText(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("readonly command card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher get pod [name]", lines[0])
		assert.Contains(t, lines, "gates: none (read-only)")
		m := res.Data.(Manifest)
		require.Len(t, m.Commands, 1)
		assert.Equal(t, "get pod", m.Commands[0].Name)
	})

	t.Run("destructive deprecated mutating command card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"delete", "project"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher delete project <name>", lines[0])
		assert.Contains(t, lines, "contract: mutating · norman · deprecated · destructive=true · network · auth")
		assert.Contains(t, lines, "gates: --apply --yes --allow-deprecated")
	})

	t.Run("bare manifest has no Text yet (TOC is a later slice)", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
		require.NoError(t, err)
		assert.Empty(t, res.Text)
	})
}

// TestSchemaSelectionPrunesGroups covers SE-18's consistency rule under
// selection: each section's Commands is pruned to the retained command set
// and empty sections are dropped, preserving relative order.
func TestSchemaSelectionPrunesGroups(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
	require.NoError(t, err)
	m := res.Data.(Manifest)
	require.Len(t, m.Groups, 1)
	assert.Equal(t, "get", m.Groups[0].Name)
	assert.Equal(t, []string{"get pod"}, m.Groups[0].Commands)
}

// TestSchemaUnknownSelectionNearestMatch covers SE-4: an unknown selection is
// a VALIDATION error listing nearest matches — case-insensitive substring
// over command names and full alias forms.
func TestSchemaUnknownSelectionNearestMatch(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("alias substring matches, case-insensitive", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"PODS"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.Len(t, ce.Candidates, 1)
		assert.Equal(t, "get pod", ce.Candidates[0].Name)
		assert.Contains(t, ce.Hint, "get pod")
		assert.Contains(t, ce.Hint, "rancher schema")
	})

	t.Run("name substring matches multiple commands", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"proj"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = c.Name
		}
		assert.Equal(t, []string{"delete project", "get project"}, names)
	})

	t.Run("no match keeps the plain error", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"frobnicate"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Empty(t, ce.Candidates)
		assert.Contains(t, ce.Hint, "rancher schema")
	})
}

// TestSchemaSelectionFormats is the W2a seam regression for the schema result
// itself: through the real pipeline, a selection renders the card (Text)
// under ""/"table" and the envelope over the filtered manifest (Data) under
// json/yaml/template — decoded assertions, never string-matched JSON.
func TestSchemaSelectionFormats(t *testing.T) {
	t.Parallel()
	newRun := func(t *testing.T, format string) (string, string) {
		t.Helper()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), cmd, tin{a: []string{"get", "pod"}},
			IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
		require.Nil(t, ce)
		return out.String(), errBuf.String()
	}

	t.Run("default format renders the card verbatim", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "")
		assert.True(t, strings.HasPrefix(out, "rancher get pod [name]\n"))
		assert.Contains(t, out, "gates: none (read-only)")
		assert.NotContains(t, out, "apiVersion", "card must not be an envelope")
	})

	t.Run("table format identical to default", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "table")
		def, _ := newRun(t, "")
		assert.Equal(t, def, out)
	})

	t.Run("json renders the envelope over the filtered manifest", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "json")
		env := decodeEnvelope(t, []byte(out))
		assert.Equal(t, "Schema", env["kind"])
		data, ok := env["data"].(map[string]any)
		require.True(t, ok)
		cmds, ok := data["commands"].([]any)
		require.True(t, ok)
		require.Len(t, cmds, 1)
		assert.Equal(t, "get pod", cmds[0].(map[string]any)["name"])
		exits, ok := data["exitCodes"].(map[string]any)
		require.True(t, ok, "W1's exitCodes map must survive selection")
		assert.EqualValues(t, 2, exits["VALIDATION"])
		assert.NotContains(t, out, "gates: none", "card text must not leak into json")
	})

	t.Run("yaml renders the envelope over the filtered manifest", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "yaml")
		var env map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(out), &env))
		assert.Equal(t, "Schema", env["kind"])
		data, ok := env["data"].(map[string]any)
		require.True(t, ok)
		cmds, ok := data["commands"].([]any)
		require.True(t, ok)
		require.Len(t, cmds, 1)
		assert.Equal(t, "get pod", cmds[0].(map[string]any)["name"])
	})

	t.Run("template renders over the JSON view of Data", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "template={{ (index .commands 0).name }}")
		assert.Equal(t, "get pod\n", out)
	})
}

func TestSchemaReportsReadOnlyFromResolver(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", CommandType: ReadOnly}})
	cmd := NewSchemaCommand(reg, "test", "2.14", func() (bool, []string) {
		return true, []string{"config readOnly"}
	})

	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
	require.NoError(t, err)
	m := res.Data.(Manifest)
	assert.True(t, m.ReadOnly)
	assert.Equal(t, []string{"config readOnly"}, m.ReadOnlySources)
	assert.Equal(t, "2.14", m.MinRancherVersion)
}

func TestSchemaReadOnlyFalseWhenResolverNil(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", CommandType: ReadOnly}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
	require.NoError(t, err)
	m := res.Data.(Manifest)
	assert.False(t, m.ReadOnly)
	assert.Empty(t, m.ReadOnlySources)
}
