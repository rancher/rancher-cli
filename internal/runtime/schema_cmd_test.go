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

// tin is a minimal Input stub for tests that only need positional Args and
// bool flags.
type tin struct {
	a     []string
	flags map[string]bool
}

func (t tin) String(string) string { return "" }
func (t tin) Bool(name string) bool {
	return t.flags[name]
}
func (t tin) Args() []string { return t.a }

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

// kindRegistry builds a registry with a kind vocabulary mirroring the
// production shapes W3 must render — a refused verb (CRT patch), an
// override-added verb (provisioning-cluster delete), a reasoned absence
// (cluster delete), a plain absence (widget delete), and the kubeconfig
// command/kind collision. The registered generated commands and the ordered
// sections are DERIVED from the kind rows' own verb states (SE-17's rule:
// available and refused verbs are registered commands; absent verbs are not),
// so the fixture cannot contradict itself.
func kindRegistry() *Registry {
	ks := []SchemaKind{
		{
			Name: "cluster", Key: "managementcluster", Aliases: []string{"clusters"},
			Group: "management.cattle.io", Version: "v3", Resource: "clusters",
			Scope: "management", NamespacePolicy: "none",
			Completable: true, WaitDefault: "condition=Ready",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"patch":  {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"delete": {State: VerbAbsent, Note: "cluster deletion goes through delete provisioning-cluster"},
			},
		},
		{
			Name: "clusterregistrationtoken", Key: "clusterregistrationtoken",
			Aliases: []string{"clusterregistrationtokens", "crt"},
			Group:   "management.cattle.io", Version: "v3", Resource: "clusterregistrationtokens",
			Scope: "management", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"patch":  {State: VerbRefused, Tier: "crd", Stability: "beta", Note: "read-only: consumed, not edited"},
				"delete": {State: VerbAbsent, Note: "consumed, not deleted"},
			},
		},
		{
			Name: "provisioning-cluster", Key: "provisioningcluster", Aliases: []string{"provisioningclusters"},
			Group: "provisioning.cattle.io", Version: "v1", Resource: "clusters",
			Scope: "management", NamespacePolicy: "workspace", DefaultNamespace: "fleet-default",
			Sensitive: true, WaitDefault: "condition=Ready",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"patch":  {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"delete": {State: VerbAvailable, Tier: "crd", Stability: "beta"},
			},
		},
		{
			Name: "kubeconfig", Key: "kubeconfig", Aliases: []string{"kubeconfigs"},
			Group: "ext.cattle.io", Resource: "kubeconfigs",
			Scope: "management", NamespacePolicy: "none", Sensitive: true,
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Tier: "public", Stability: "beta"},
				"patch":  {State: VerbAvailable, Tier: "public", Stability: "beta"},
				"delete": {State: VerbAvailable, Tier: "public", Stability: "beta"},
			},
		},
		{
			Name: "widget", Key: "widget", Resource: "widgets",
			Scope: "downstream", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"patch":  {State: VerbAvailable, Tier: "crd", Stability: "beta"},
				"delete": {State: VerbAbsent},
			},
		},
	}
	res := stubResolver{}
	reg := NewRegistry(res)
	reg.Register(fakeRead{d: Descriptor{
		Name: "kubeconfig", Usage: "Print an exec-credential kubeconfig for a cluster",
		CommandType: Local,
	}})
	groups := []SchemaGroup{{Name: "", Commands: []string{"kubeconfig"}}}
	byVerb := map[string][]string{}
	verbs := []string{"get", "patch", "delete"}
	for _, k := range ks {
		for _, verb := range verbs {
			vs := k.Verbs[verb]
			if vs.State == VerbAbsent {
				continue
			}
			name := verb + " " + k.Name
			if verb == "get" {
				reg.Register(fakeRead{d: Descriptor{Name: name, CommandType: ReadOnly}})
			} else {
				res[name] = safety.Binding{Tier: safety.Tier(vs.Tier), Stability: safety.Stability(vs.Stability)}
				reg.Register(fakeMutating{d: Descriptor{Name: name, CommandType: Mutating}})
			}
			byVerb[verb] = append(byVerb[verb], name)
		}
	}
	for _, verb := range verbs {
		groups = append(groups, SchemaGroup{Name: verb, Commands: byVerb[verb]})
	}
	reg.SetKinds(ks)
	reg.SetGroups(groups)
	return reg
}

// TestSchemaSelectionCommandWinsCollision covers SE-6: a positional naming
// both a command and a kind resolves to the command card (with the kind-view
// footnote); --kind forces the kind interpretation.
func TestSchemaSelectionCommandWinsCollision(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)

	t.Run("command match wins the bare positional", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"kubeconfig"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher kubeconfig", lines[0])
		assert.Contains(t, lines, "kind view: rancher schema kubeconfig --kind")
		m := res.Data.(Manifest)
		require.Len(t, m.Commands, 1)
		assert.Equal(t, "kubeconfig", m.Commands[0].Name)
		assert.Len(t, m.Kinds, 5, "command selection leaves the kind vocabulary untouched")
	})

	t.Run("--kind flips the collision to the kind card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"kubeconfig"}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "kind: kubeconfig\n"))
		m := res.Data.(Manifest)
		require.Len(t, m.Kinds, 1)
		assert.Equal(t, "kubeconfig", m.Kinds[0].Name)
	})

	t.Run("non-colliding command card carries no kind footnote", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"get", "clusterregistrationtoken"}}, nil)
		require.NoError(t, err)
		assert.NotContains(t, res.Text, "kind view:")
	})

	t.Run("a kind identifier resolves without --kind", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"crt"}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "kind: clusterregistrationtoken\n"),
			"a kind alias is a valid selector when no command matches")
	})
}

// TestSchemaKindCardVerbTable covers SE-5's verb table: for every exemplar
// kind, each verb line is derived from the manifest's own VerbState via the
// spec wording — available (tier · stability) | refused — <note> |
// not available — <reason> | not available — then the four interesting
// states are pinned so the fixture cannot drift silently.
func TestSchemaKindCardVerbTable(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	sel := func(t *testing.T, name string) ([]string, SchemaKind) {
		t.Helper()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{name}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		m := res.Data.(Manifest)
		require.Len(t, m.Kinds, 1)
		return strings.Split(res.Text, "\n"), m.Kinds[0]
	}
	wantVerbLine := func(verb string, vs VerbState) string {
		switch vs.State {
		case VerbAvailable:
			return verb + ": available (" + vs.Tier + " · " + vs.Stability + ")"
		case VerbRefused:
			return verb + ": refused — " + vs.Note
		default:
			if vs.Note != "" {
				return verb + ": not available — " + vs.Note
			}
			return verb + ": not available"
		}
	}

	for _, name := range []string{"cluster", "clusterregistrationtoken", "provisioning-cluster", "widget"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines, k := sel(t, name)
			assert.Contains(t, lines, "verbs:")
			for verb, vs := range k.Verbs {
				assert.Contains(t, lines, "  "+wantVerbLine(verb, vs))
			}
		})
	}

	t.Run("exemplar states are the production shapes", func(t *testing.T) {
		t.Parallel()
		lines, _ := sel(t, "clusterregistrationtoken")
		assert.Contains(t, lines, "  patch: refused — read-only: consumed, not edited")
		assert.Contains(t, lines, "  delete: not available — consumed, not deleted")

		lines, _ = sel(t, "provisioning-cluster")
		assert.Contains(t, lines, "  delete: available (crd · beta)")

		lines, _ = sel(t, "cluster")
		assert.Contains(t, lines, "  delete: not available — cluster deletion goes through delete provisioning-cluster")

		lines, _ = sel(t, "widget")
		assert.Contains(t, lines, "  delete: not available")
	})
}

// TestSchemaKindStructured covers the SE-2 kind row for structured output:
// -o json is the envelope whose data is the manifest filtered to that kind,
// with commands exactly the kind's registered generated verbs (available OR
// refused — derived from the kind's own verb states) and Groups pruned to
// the same set.
func TestSchemaKindStructured(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, tin{a: []string{"clusterregistrationtoken"}},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)

	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "Schema", env["kind"])
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)

	kinds, ok := data["kinds"].([]any)
	require.True(t, ok)
	require.Len(t, kinds, 1)
	k, ok := kinds[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "clusterregistrationtoken", k["name"])

	want := map[string]bool{}
	verbs, ok := k["verbs"].(map[string]any)
	require.True(t, ok)
	for verb, v := range verbs {
		state := v.(map[string]any)["state"].(string)
		if state == VerbAvailable || state == VerbRefused {
			want[verb+" clusterregistrationtoken"] = true
		}
	}
	require.NotEmpty(t, want)

	got := map[string]bool{}
	cmds, ok := data["commands"].([]any)
	require.True(t, ok)
	for _, c := range cmds {
		got[c.(map[string]any)["name"].(string)] = true
	}
	assert.Equal(t, want, got, "commands are exactly the kind's registered generated verbs")
	assert.True(t, got["patch clusterregistrationtoken"], "a refused verb is a registered command")

	grouped := map[string]bool{}
	groups, ok := data["groups"].([]any)
	require.True(t, ok)
	for _, g := range groups {
		names := g.(map[string]any)["commands"].([]any)
		require.NotEmpty(t, names, "empty sections are dropped")
		for _, n := range names {
			grouped[n.(string)] = true
		}
	}
	assert.Equal(t, got, grouped, "Groups carry the same command set as commands (SE-18)")
}

// TestSchemaKindFlagRequiresSelector covers SE-20: --kind is a selection
// modifier, so bare --kind without a positional selector is VALIDATION.
func TestSchemaKindFlagRequiresSelector(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	_, err := cmd.(ReadCommand).Execute(context.Background(),
		tin{flags: map[string]bool{"kind": true}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--kind")
}

// TestSchemaKindFlagWithGlobalFlags pins the SE-20 success case: --kind
// composes with global flags — `schema kubeconfig --kind -o json` succeeds
// and emits the kind-filtered envelope.
func TestSchemaKindFlagWithGlobalFlags(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd,
		tin{a: []string{"kubeconfig"}, flags: map[string]bool{"kind": true}},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)

	env := decodeEnvelope(t, out.Bytes())
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)
	kinds, ok := data["kinds"].([]any)
	require.True(t, ok)
	require.Len(t, kinds, 1)
	assert.Equal(t, "kubeconfig", kinds[0].(map[string]any)["name"])

	names := map[string]bool{}
	for _, c := range data["commands"].([]any) {
		names[c.(map[string]any)["name"].(string)] = true
	}
	assert.Equal(t, map[string]bool{
		"get kubeconfig": true, "patch kubeconfig": true, "delete kubeconfig": true,
	}, names, "the colliding top-level command is not part of the kind view")
}

// TestSchemaUnknownSelectionNearestMatch covers SE-4: an unknown selection is
// a VALIDATION error listing nearest matches — case-insensitive substring
// over command names, full alias forms, and kind names/aliases/keys.
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

	t.Run("kind names join the candidate set", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"widg"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = c.Name
		}
		assert.Equal(t, []string{"get widget", "patch widget", "widget"}, names)
	})

	t.Run("a command/kind collision yields one candidate", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"kubeconf"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = c.Name
		}
		assert.Equal(t, []string{"delete kubeconfig", "get kubeconfig", "kubeconfig", "patch kubeconfig"}, names)
	})

	t.Run("--kind restricts candidates to kinds", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"kubeconf"}, flags: map[string]bool{"kind": true}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "kind")
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = c.Name
		}
		assert.Equal(t, []string{"kubeconfig"}, names)
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
