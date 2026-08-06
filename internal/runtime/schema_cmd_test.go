package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// tin is a minimal Input stub for tests that only need positional Args plus
// bool and string flags.
type tin struct {
	a     []string
	flags map[string]bool
	strs  map[string]string
}

func (t tin) String(name string) string { return t.strs[name] }
func (t tin) Bool(name string) bool {
	return t.flags[name]
}
func (t tin) Strings(string) []string { return nil }
func (t tin) IsSet(string) bool       { return false }
func (t tin) Args() []string          { return t.a }

// viewManifest unwraps the SE-19 schema view a schema Result carries as Data,
// returning the wrapped (possibly filtered) manifest.
func viewManifest(t *testing.T, res Result) Manifest {
	t.Helper()
	v, ok := res.Data.(schemaView)
	require.True(t, ok, "schema Result.Data must be the SE-19 view, got %T", res.Data)
	return v.manifest
}

// viewTOC decodes the tocView a bare or filtered schema Result carries as Data
// into a plain map for field-level assertions against the TOC JSON shape.
func viewTOC(t *testing.T, res Result) map[string]any {
	t.Helper()
	v, ok := res.Data.(tocView)
	require.True(t, ok, "schema Result.Data must be tocView for bare/filtered, got %T", res.Data)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// viewCard decodes the cardView a command selection carries as Data into a
// plain map for field-level assertions against the flattened card JSON shape.
func viewCard(t *testing.T, res Result) map[string]any {
	t.Helper()
	v, ok := res.Data.(cardView)
	require.True(t, ok, "schema Result.Data must be cardView for command selections, got %T", res.Data)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func TestSchemaFilter(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", Aliases: []string{"pods"}, CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	t.Run("no args returns the full manifest", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
		require.NoError(t, err)
		toc := viewTOC(t, res)
		assert.Equal(t, float64(2), toc["commandCount"])
	})

	t.Run("multi-word name filters to one", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		got := viewCard(t, res)
		assert.Equal(t, "get pod", got["name"])
	})

	t.Run("alias matches", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pods"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, "get pod", viewCard(t, res)["name"])
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
		"delete project": {Contract: safety.ContractInternal, Stability: safety.Deprecated},
	})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get pod", Aliases: []string{"pods", "po"}, Usage: "List pods or show one",
		Args: []ArgSpec{{Name: "name", Type: "string"}}, CommandType: ReadOnly,
		Network: true, Auth: true,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get project", Usage: "List projects", CommandType: ReadOnly,
		Desc:    "Reads management projects across all clusters.",
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
		got := viewCard(t, res)
		assert.Equal(t, "get pod", got["name"])
	})

	t.Run("destructive deprecated mutating command card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"delete", "project"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher delete project <name>", lines[0])
		assert.Contains(t, lines, "contract: mutating · internal · deprecated · destructive=true · network · auth")
		assert.Contains(t, lines, "gates: --apply --yes --allow-deprecated")
	})

	t.Run("bare manifest renders the TOC as Text (SE-9)", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "rancher test — "),
			"bare schema Text is the TOC, headed by the CLI version and counts")
	})
}

// TestSchemaSelectionCardShape covers the command card's flattened structured
// shape: the selected command's fields are at the top level, no commands
// wrapper array, no groups (card view is flattened — not a manifest slice).
func TestSchemaSelectionCardShape(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
	require.NoError(t, err)
	got := viewCard(t, res)
	assert.Equal(t, "get pod", got["name"])
	assert.NotContains(t, got, "commands", "flattened: no commands wrapper array")
	assert.NotContains(t, got, "groups", "flattened: no groups in structured output")
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
				"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"patch":  {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"delete": {State: VerbAbsent, Note: "cluster deletion goes through delete provisioning-cluster"},
			},
		},
		{
			Name: "clusterregistrationtoken", Key: "clusterregistrationtoken",
			Aliases: []string{"clusterregistrationtokens", "crt"},
			Group:   "management.cattle.io", Version: "v3", Resource: "clusterregistrationtokens",
			Scope: "management", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"patch":  {State: VerbRefused, Contract: "internal", Stability: "beta", Note: "read-only: consumed, not edited"},
				"delete": {State: VerbAbsent, Note: "consumed, not deleted"},
			},
		},
		{
			Name: "provisioning-cluster", Key: "provisioningcluster", Aliases: []string{"provisioningclusters"},
			Group: "provisioning.cattle.io", Version: "v1", Resource: "clusters",
			Scope: "management", NamespacePolicy: "workspace", DefaultNamespace: "fleet-default",
			Sensitive: true, WaitDefault: "condition=Ready",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"patch":  {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"delete": {State: VerbAvailable, Contract: "internal", Stability: "beta"},
			},
		},
		{
			Name: "kubeconfig", Key: "kubeconfig", Aliases: []string{"kubeconfigs"},
			Group: "ext.cattle.io", Resource: "kubeconfigs",
			Scope: "management", NamespacePolicy: "none", Sensitive: true,
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Contract: "public", Stability: "stable"},
				"patch":  {State: VerbAvailable, Contract: "public", Stability: "stable"},
				"delete": {State: VerbAvailable, Contract: "public", Stability: "stable"},
			},
		},
		{
			Name: "widget", Key: "widget", Resource: "widgets",
			Scope: "downstream", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{
				"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
				"patch":  {State: VerbAvailable, Contract: "internal", Stability: "beta"},
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
				res[name] = safety.Binding{Contract: safety.Contract(vs.Contract), Stability: safety.Stability(vs.Stability)}
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
		got := viewCard(t, res)
		assert.Equal(t, "kubeconfig", got["name"])
		assert.Equal(t, "kubeconfig", got["kindViewHint"], "kind-collision pointer in structured output")
	})

	t.Run("--kind flips the collision to the kind card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"kubeconfig"}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "kind: kubeconfig\n"))
		m := viewManifest(t, res)
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
// spec wording — available (contract · stability) | refused — <note> |
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
		m := viewManifest(t, res)
		require.Len(t, m.Kinds, 1)
		return strings.Split(res.Text, "\n"), m.Kinds[0]
	}
	wantVerbLine := func(verb string, vs VerbState) string {
		switch vs.State {
		case VerbAvailable:
			return verb + ": available (" + vs.Contract + " · " + vs.Stability + ")"
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
		assert.Contains(t, lines, "  delete: available (internal · beta)")

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
	_, ce := Run(context.Background(), cmd, tin{a: []string{"clusterregistrationtoken"}},
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
	_, ce := Run(context.Background(), cmd,
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
		_, ce := Run(context.Background(), cmd, tin{a: []string{"get", "pod"}},
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

	t.Run("json renders the envelope over the card", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "json")
		env := decodeEnvelope(t, []byte(out))
		assert.Equal(t, "Schema", env["kind"])
		data, ok := env["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "get pod", data["name"], "command name at top level (flattened card)")
		assert.Contains(t, data, "globalExitCodes", "exit map always present on card")
		assert.NotContains(t, data, "commands", "flattened: no commands wrapper array")
		assert.NotContains(t, out, "gates: none", "card text must not leak into json")
	})

	t.Run("yaml renders the envelope over the card", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "yaml")
		var env map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(out), &env))
		assert.Equal(t, "Schema", env["kind"])
		data, ok := env["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "get pod", data["name"], "command name at top level (flattened card)")
		assert.NotContains(t, data, "commands", "flattened: no commands wrapper array")
	})

	t.Run("template renders over the JSON view of Data", func(t *testing.T) {
		t.Parallel()
		out, _ := newRun(t, "template={{ .name }}")
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
	toc := viewTOC(t, res)
	assert.Equal(t, true, toc["readOnly"])
	assert.Equal(t, []any{"config readOnly"}, toc["readOnlySources"])
	assert.Equal(t, "2.14", toc["minRancherVersion"])
}

func TestSchemaReadOnlyFalseWhenResolverNil(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", CommandType: ReadOnly}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
	require.NoError(t, err)
	toc := viewTOC(t, res)
	assert.Equal(t, false, toc["readOnly"])
	assert.NotContains(t, toc, "readOnlySources")
}

// TestSchemaFilters covers SE-10: --verb, --destructive, --contract, and
// --mutating filter the manifest's commands (composing with each other and
// with --search), prune Groups to the retained set (SE-18), and render the
// filtered TOC as Text.
func TestSchemaFilters(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
	run := func(t *testing.T, in tin) (map[string]any, string) {
		t.Helper()
		res, err := cmd.(ReadCommand).Execute(context.Background(), in, nil)
		require.NoError(t, err)
		return viewTOC(t, res), res.Text
	}
	// tocNames extracts command names from the groups of a TOC JSON map.
	tocNames := func(toc map[string]any) []string {
		var out []string
		groups, _ := toc["groups"].([]any)
		for _, g := range groups {
			for _, e := range g.(map[string]any)["commands"].([]any) {
				out = append(out, e.(map[string]any)["name"].(string))
			}
		}
		return out
	}

	t.Run("--verb keeps commands whose first name token matches", func(t *testing.T) {
		t.Parallel()
		toc, text := run(t, tin{strs: map[string]string{"verb": "get"}})
		assert.Equal(t, []string{"get pod", "get project"}, tocNames(toc))
		groups, _ := toc["groups"].([]any)
		require.Len(t, groups, 1, "sections prune to the retained set")
		assert.Equal(t, "get", groups[0].(map[string]any)["name"])
		assert.Contains(t, text, "2 commands match --verb get")
		assert.Contains(t, text, "get pod — List pods or show one")
	})

	t.Run("--destructive keeps true and dependsOnInput", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{flags: map[string]bool{"destructive": true}})
		assert.Equal(t, []string{"delete project"}, tocNames(toc))
	})

	t.Run("--contract keeps the contract's commands", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{strs: map[string]string{"contract": "internal"}})
		assert.Equal(t, []string{"delete project"}, tocNames(toc))
	})

	t.Run("--mutating keeps mutating commands", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{flags: map[string]bool{"mutating": true}})
		assert.Equal(t, []string{"delete project"}, tocNames(toc))
	})

	t.Run("filters compose (AND)", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{
			strs:  map[string]string{"verb": "delete"},
			flags: map[string]bool{"destructive": true},
		})
		assert.Equal(t, []string{"delete project"}, tocNames(toc))
	})

	t.Run("filters compose with --search", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{strs: map[string]string{"verb": "get", "search": "pods"}})
		assert.Equal(t, []string{"get pod"}, tocNames(toc))
	})

	t.Run("composed filters can produce zero hits, naming every active filter", func(t *testing.T) {
		t.Parallel()
		toc, text := run(t, tin{strs: map[string]string{"verb": "get", "contract": "public"}})
		assert.Equal(t, float64(0), toc["commandCount"])
		assert.Nil(t, toc["groups"])
		assert.Equal(t, "no commands match --verb get --contract public\n", text)
	})
}

// TestSchemaSearch covers SE-11: --search is a case-insensitive substring over
// name, aliases, usage, and desc; the filtered TOC shows the matching
// fragment; zero hits print a no-matches message naming the active filters and
// exit 0.
func TestSchemaSearch(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("desc match is case-insensitive and shows the fragment", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{strs: map[string]string{"search": "MANAGEMENT"}}, nil)
		require.NoError(t, err)
		toc := viewTOC(t, res)
		assert.Equal(t, float64(1), toc["commandCount"])
		groups, _ := toc["groups"].([]any)
		require.Len(t, groups, 1)
		entries := groups[0].(map[string]any)["commands"].([]any)
		assert.Equal(t, "get project", entries[0].(map[string]any)["name"])
		assert.Contains(t, res.Text, "[desc: ")
		assert.Contains(t, res.Text, "management projects")
	})

	t.Run("name match needs no fragment", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{strs: map[string]string{"search": "delete"}}, nil)
		require.NoError(t, err)
		toc := viewTOC(t, res)
		assert.Equal(t, float64(1), toc["commandCount"])
		groups, _ := toc["groups"].([]any)
		require.Len(t, groups, 1)
		entries := groups[0].(map[string]any)["commands"].([]any)
		assert.Equal(t, "delete project", entries[0].(map[string]any)["name"])
		assert.Contains(t, res.Text, "delete project — Delete a project")
		assert.NotContains(t, res.Text, "[", "a match visible in the line carries no fragment")
	})

	t.Run("zero hits exit 0 with a message naming the filters", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(context.Background(), cmd, tin{strs: map[string]string{"search": "zzz"}},
			IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
		require.Nil(t, ce, "zero hits are not an error")
		assert.Equal(t, "no commands match --search \"zzz\"\n", out.String())
	})
}

// TestSchemaSelectorFilterConflict covers SE-20: a positional selector
// (command or kind) or --kind combined with any of the five filter/search
// flags is a VALIDATION error naming the conflict.
func TestSchemaSelectorFilterConflict(t *testing.T) {
	t.Parallel()

	t.Run("command selector plus --verb", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"get", "pod"}, strs: map[string]string{"verb": "get"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--verb")
		assert.Contains(t, ce.Message, "selector")
		assert.Contains(t, ce.Hint, "drop the selector or the filter")
	})

	t.Run("kind selector plus --search", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{a: []string{"crt"}, strs: map[string]string{"search": "pod"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--search")
	})

	t.Run("--kind plus a filter", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(context.Background(),
			tin{flags: map[string]bool{"kind": true}, strs: map[string]string{"contract": "internal"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--contract")
		assert.Contains(t, ce.Message, "--kind")
	})

	t.Run("every filter flag conflicts with a selector", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		cases := []tin{
			{a: []string{"get", "pod"}, strs: map[string]string{"verb": "get"}},
			{a: []string{"get", "pod"}, flags: map[string]bool{"destructive": true}},
			{a: []string{"get", "pod"}, strs: map[string]string{"contract": "internal"}},
			{a: []string{"get", "pod"}, flags: map[string]bool{"mutating": true}},
			{a: []string{"get", "pod"}, strs: map[string]string{"search": "pod"}},
		}
		for _, in := range cases {
			_, err := cmd.(ReadCommand).Execute(context.Background(), in, nil)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
		}
	})
}

// TestSchemaGroupsCommandsConsistency is the SE-18 invariant after every W4
// result shape: the flattened Groups[].Commands set-equals the manifest's
// Commands names, empty sections are dropped, and a zero hit carries empty
// Groups.
func TestSchemaGroupsCommandsConsistency(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	shapes := []struct {
		name    string
		in      tin
		zeroHit bool
	}{
		{"bare", tin{}, false},
		{"command selection", tin{a: []string{"kubeconfig"}}, false},
		{"kind selection", tin{a: []string{"crt"}}, false},
		{"filtered", tin{strs: map[string]string{"verb": "get"}}, false},
		{"search hit", tin{strs: map[string]string{"search": "kubeconfig"}}, false},
		{"zero hit", tin{strs: map[string]string{"search": "zzzz"}}, true},
	}
	full := kindRegistry().Schema("test", "2.14")
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// SE-18 is a manifest invariant: assert on the manifest directly
			// for bare/filtered selections (their tocView JSON collapses
			// generated commands and does not carry a flat commands list).
			var m Manifest
			switch tc.name {
			case "bare":
				m = full
			case "filtered", "search hit", "zero hit":
				m = filterManifest(full, filtersFrom(tc.in))
			default:
				res, err := cmd.(ReadCommand).Execute(context.Background(), tc.in, nil)
				require.NoError(t, err)
				if cv, ok := res.Data.(cardView); ok {
					m = cv.manifest
				} else {
					m = viewManifest(t, res)
				}
			}
			want := map[string]bool{}
			for _, c := range m.Commands {
				want[c.Name] = true
			}
			got := map[string]bool{}
			for _, g := range m.Groups {
				require.NotEmpty(t, g.Commands, "empty sections are dropped")
				for _, n := range g.Commands {
					got[n] = true
				}
			}
			assert.Equal(t, want, got, "flattened Groups set-equals Commands")
			if tc.zeroHit {
				assert.Empty(t, m.Commands)
				assert.Empty(t, m.Groups)
			} else {
				assert.NotEmpty(t, m.Commands)
			}
		})
	}
}

// completionRegistry builds the SE-12 shape exemplars: a verbed command whose
// aliases are also kind identifiers (get pod), a verbed command whose alias is
// NOT a kind identifier (get project → projects, so the bare-leaf-alias
// exclusion is observable), the kubeconfig command/kind collision, and a kind
// whose key differs from its name (cluster → managementcluster).
func completionRegistry() *Registry {
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get pod", Aliases: []string{"pods", "po"}, CommandType: ReadOnly,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get project", Aliases: []string{"projects"}, CommandType: ReadOnly,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get kubeconfig", Aliases: []string{"kubeconfigs"}, CommandType: ReadOnly,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "kubeconfig", Usage: "Print an exec-credential kubeconfig for a cluster",
		CommandType: Local,
	}})
	reg.SetKinds([]SchemaKind{
		{
			Name: "pod", Key: "pod", Aliases: []string{"pods", "po"},
			Resource: "pods", Scope: "downstream", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{"get": {State: VerbAvailable, Contract: "internal", Stability: "beta"}},
		},
		{
			Name: "kubeconfig", Key: "kubeconfig", Aliases: []string{"kubeconfigs"},
			Resource: "kubeconfigs", Scope: "management", NamespacePolicy: "none",
			Verbs: map[string]VerbState{"get": {State: VerbAvailable, Contract: "public", Stability: "stable"}},
		},
		{
			Name: "cluster", Key: "managementcluster", Aliases: []string{"clusters"},
			Resource: "clusters", Scope: "management", NamespacePolicy: "none",
			Verbs: map[string]VerbState{"get": {State: VerbAbsent, Note: "listing not wired in this fixture"}},
		},
	})
	return reg
}

// TestSchemaPositionalCompletionCandidatesResolve is SE-12's shipping gate:
// every candidate CompleteArgs offers must resolve through the actual
// selection logic in Execute — a suggestion `rancher schema` would then
// reject cannot ship. Run against every registry fixture so command-only,
// collision-heavy, and alias-heavy surfaces are all covered.
func TestSchemaPositionalCompletionCandidatesResolve(t *testing.T) {
	t.Parallel()
	regs := map[string]*Registry{
		"commands only":         cardRegistry(),
		"kinds plus collision":  kindRegistry(),
		"aliased verb commands": completionRegistry(),
	}
	for name, reg := range regs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := NewSchemaCommand(reg, "test", "2.14", nil)
			sc, ok := cmd.(ShellCompleter)
			require.True(t, ok, "schema must implement ShellCompleter (SE-12)")
			cands := sc.CompleteArgs(context.Background(), nil)
			require.NotEmpty(t, cands)
			for _, cand := range cands {
				res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: strings.Fields(cand)}, nil)
				require.NoError(t, err, "candidate %q must resolve as a schema selector", cand)
				if cv, ok := res.Data.(cardView); ok {
					assert.NotEmpty(t, cv.cmd.Name, "candidate %q must resolve to a named command", cand)
				} else {
					m := viewManifest(t, res)
					assert.Equal(t, 1, len(m.Kinds),
						"candidate %q must select one kind", cand)
				}
			}
		})
	}
}

// TestSchemaPositionalCompletionShape pins SE-12's candidate shape: command
// aliases appear only in their full typed form ("get pods", never a bare
// "projects"), kind names/keys/aliases appear as bare tokens, the
// command/kind collision (kubeconfig) dedupes to one candidate, and the set
// is deterministically sorted with no duplicates.
func TestSchemaPositionalCompletionShape(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(completionRegistry(), "test", "2.14", nil)
	sc, ok := cmd.(ShellCompleter)
	require.True(t, ok, "schema must implement ShellCompleter (SE-12)")
	cands := sc.CompleteArgs(context.Background(), nil)

	assert.True(t, sort.StringsAreSorted(cands), "candidates are deterministically sorted: %v", cands)
	counts := map[string]int{}
	for _, c := range cands {
		counts[c]++
	}
	for cand, n := range counts {
		assert.Equal(t, 1, n, "candidate %q must appear exactly once", cand)
	}

	for _, want := range []string{"get pod", "get project", "get kubeconfig", "kubeconfig"} {
		assert.Contains(t, cands, want, "full command names are candidates")
	}
	for _, want := range []string{"get pods", "get po", "get projects", "get kubeconfigs"} {
		assert.Contains(t, cands, want, "command aliases appear in their full typed form")
	}
	assert.NotContains(t, cands, "projects",
		"a command alias never appears in its bare leaf form (it is not a selector)")
	for _, want := range []string{"pod", "pods", "po", "cluster", "clusters", "managementcluster", "kubeconfigs"} {
		assert.Contains(t, cands, want, "kind names, aliases, and keys are bare-token candidates")
	}
	assert.Equal(t, 1, counts["kubeconfig"], "the command/kind collision dedupes to one candidate")
}
