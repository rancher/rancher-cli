package runtime

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
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

// groupCommandNames extracts all command names from the groups of a TOC JSON
// map, in order across all groups - the flat name list the group membership and
// bare-generated assertions need. Call sites that require per-group structure
// (e.g. asserting group count or accessing a specific group's entries) should
// access toc["groups"] directly instead of using this helper.
func groupCommandNames(t *testing.T, toc map[string]any) []string {
	t.Helper()
	var out []string
	groups, _ := toc["groups"].([]any)
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		require.True(t, ok, "group must be a JSON object")
		// All-generated groups carry no commands key (omitempty); use a
		// nil-safe assertion so they contribute zero names rather than panic.
		cmds, _ := gm["commands"].([]any)
		for _, e := range cmds {
			em, ok := e.(map[string]any)
			require.True(t, ok, "command entry must be a JSON object")
			out = append(out, em["name"].(string))
		}
	}
	return out
}

// viewGroup extracts the groupView a group-selection Result carries as Data.
func viewGroup(t *testing.T, res Result) groupView {
	t.Helper()
	v, ok := res.Data.(groupView)
	require.True(t, ok, "schema Result.Data must be groupView for group selections, got %T", res.Data)
	return v
}

// viewKind extracts the SchemaKind from the kindView a kind-selection Result carries as Data.
func viewKind(t *testing.T, res Result) SchemaKind {
	t.Helper()
	v, ok := res.Data.(kindView)
	require.True(t, ok, "schema Result.Data must be kindView for kind selections, got %T", res.Data)
	return v.kind
}

func TestSchemaFilter(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", Aliases: []string{"pods"}, CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	t.Run("no args returns the full manifest", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{}, nil)
		require.NoError(t, err)
		toc := viewTOC(t, res)
		assert.Equal(t, float64(2), toc["commandCount"])
	})

	t.Run("multi-word name filters to one", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		got := viewCard(t, res)
		assert.Equal(t, "get pod", got["name"])
	})

	t.Run("alias matches", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pods"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, "get pod", viewCard(t, res)["name"])
	})

	t.Run("unknown command is VALIDATION", func(t *testing.T) {
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "nonsense"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

// cardRegistry builds a registry with one readonly command, one mutating
// command with a resolver binding, and ordered groups - enough surface to
// exercise selection, the card, and structured output.
func cardRegistry() *Registry {
	reg := NewRegistry(stubResolver{
		"delete project": {Contract: safety.ContractInternal, Stability: safety.Deprecated},
	})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get pod", Aliases: []string{"pods", "po"}, Usage: "List pods or show one",
		Args: []ArgSpec{{Name: "name", Type: "string"}}, CommandType: ReadOnly,
		Auth: true,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get project", Usage: "List projects", CommandType: ReadOnly,
		Desc: "Reads management projects across all clusters.",
		Auth: true,
	}})
	reg.Register(fakeMutating{d: Descriptor{
		Name: "delete project", Usage: "Delete a project",
		Args:        []ArgSpec{{Name: "name", Type: "string", Required: true}},
		CommandType: Mutating, Destructive: "true", Auth: true,
	}})
	reg.SetGroups([]SchemaGroup{
		{Name: "", Commands: []string{"schema"}},
		{Name: "get", Commands: []string{"get pod", "get project"}},
		{Name: "delete", Commands: []string{"delete project"}},
	})
	return reg
}

// TestSchemaSelectionSetsCardText covers the wiring: selecting a command
// yields a Result whose Text is the card (rendered under default/table) while
// Data keeps the filtered manifest for the structured formats.
func TestSchemaSelectionSetsCardText(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("readonly command card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher get pod [name]", lines[0])
		assert.Contains(t, lines, "gates: none (read-only)")
		got := viewCard(t, res)
		assert.Equal(t, "get pod", got["name"])
	})

	t.Run("destructive deprecated mutating command card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"delete", "project"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(res.Text, "\n")
		assert.Equal(t, "rancher delete project <name>", lines[0])
		assert.Contains(t, lines, "contract: mutating · internal · deprecated · destructive=true · server=write · auth")
		assert.Contains(t, lines, "gates: --apply --yes --allow-deprecated")
	})

	t.Run("bare manifest renders the TOC as Text", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "rancher test - "),
			"bare schema Text is the TOC, headed by the CLI version and counts")
	})
}

// TestSchemaSelectionCardShape covers the command card's flattened structured
// shape: the selected command's fields are at the top level, no commands
// wrapper array, no groups (card view is flattened - not a manifest slice).
func TestSchemaSelectionCardShape(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
	res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pod"}}, nil)
	require.NoError(t, err)
	got := viewCard(t, res)
	assert.Equal(t, "get pod", got["name"])
	assert.NotContains(t, got, "commands", "flattened: no commands wrapper array")
	assert.NotContains(t, got, "groups", "flattened: no groups in structured output")
}

// kindRegistry builds a registry with a kind vocabulary mirroring the
// production shapes the kind card must render - a refused verb (CRT patch), an
// override-added verb (provisioning-cluster delete), a reasoned absence
// (cluster delete), a plain absence (widget delete), and the kubeconfig
// command/kind collision. The registered generated commands and the ordered
// sections are DERIVED from the kind rows' own verb states (available and
// refused verbs are registered commands; absent verbs are not), so the fixture
// cannot contradict itself.
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

// TestSchemaSelectionCommandWinsCollision covers the collision case: a
// positional naming both a command and a kind resolves to the command card
// (with the kind-view footnote); --kind forces the kind interpretation.
func TestSchemaSelectionCommandWinsCollision(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)

	t.Run("command match wins the bare positional", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"kubeconfig"}}, nil)
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
		res, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"kubeconfig"}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "kind: kubeconfig\n"))
		k := viewKind(t, res)
		assert.Equal(t, "kubeconfig", k.Name)
	})

	t.Run("non-colliding command card carries no kind footnote", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"get", "clusterregistrationtoken"}}, nil)
		require.NoError(t, err)
		assert.NotContains(t, res.Text, "kind view:")
	})

	t.Run("a kind identifier resolves without --kind", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"crt"}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "kind: clusterregistrationtoken\n"),
			"a kind alias is a valid selector when no command matches")
	})
}

// TestSchemaKindCardVerbTable covers the verb table: for every exemplar
// kind, each verb line is derived from the manifest's own VerbState via the
// fixed wording - available (contract · stability) | refused - <note> |
// not available - <reason> | not available - then the four interesting
// states are asserted so the fixture cannot drift silently.
func TestSchemaKindCardVerbTable(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	sel := func(t *testing.T, name string) ([]string, SchemaKind) {
		t.Helper()
		res, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{name}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		return strings.Split(res.Text, "\n"), viewKind(t, res)
	}
	wantVerbLine := func(verb string, vs VerbState) string {
		switch vs.State {
		case VerbAvailable:
			return verb + ": available (" + vs.Contract + " · " + vs.Stability + ")"
		case VerbRefused:
			return verb + ": refused - " + vs.Note
		default:
			if vs.Note != "" {
				return verb + ": not available - " + vs.Note
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
		assert.Contains(t, lines, "  patch: refused - read-only: consumed, not edited")
		assert.Contains(t, lines, "  delete: not available - consumed, not deleted")

		lines, _ = sel(t, "provisioning-cluster")
		assert.Contains(t, lines, "  delete: available (internal · beta)")

		lines, _ = sel(t, "cluster")
		assert.Contains(t, lines, "  delete: not available - cluster deletion goes through delete provisioning-cluster")

		lines, _ = sel(t, "widget")
		assert.Contains(t, lines, "  delete: not available")
	})
}

// TestSchemaKindStructured covers the kind row for structured output:
// -o json is the envelope whose data IS the kind row - name and verbs at top
// level, no commands or groups wrapper (the per-verb detail is one more call away).
func TestSchemaKindStructured(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, tin{a: []string{"clusterregistrationtoken"}},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)

	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "Schema", env["kind"])
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)

	// kind-row shape: the kind's fields are at top level - no kinds/commands/groups wrapper.
	assert.Equal(t, "clusterregistrationtoken", data["name"])
	verbs, ok := data["verbs"].(map[string]any)
	require.True(t, ok, "kind row carries verbs")
	getState, ok := verbs["get"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, VerbAvailable, getState["state"])
	patchState, ok := verbs["patch"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, VerbRefused, patchState["state"], "a refused verb is present in the kind row")
	assert.NotContains(t, data, "commands", "kind-row carries no commands wrapper")
	assert.NotContains(t, data, "groups", "kind-row carries no groups wrapper")
}

// TestSchemaKindFlagRequiresSelector covers the conflict rule: --kind is a
// selection modifier, so bare --kind without a positional selector is
// VALIDATION.
func TestSchemaKindFlagRequiresSelector(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	_, err := cmd.(ReadCommand).Execute(t.Context(),
		tin{flags: map[string]bool{"kind": true}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--kind")
}

// TestSchemaKindFlagWithGlobalFlags asserts the success case: --kind
// composes with global flags - `schema kubeconfig --kind -o json` succeeds
// and emits the kind-filtered envelope.
func TestSchemaKindFlagWithGlobalFlags(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd,
		tin{a: []string{"kubeconfig"}, flags: map[string]bool{"kind": true}},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)

	env := decodeEnvelope(t, out.Bytes())
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)

	// kind-row shape: kind fields at top level - no kinds/commands wrapper, so the
	// colliding top-level "kubeconfig" command is naturally absent.
	assert.Equal(t, "kubeconfig", data["name"])
	verbs, ok := data["verbs"].(map[string]any)
	require.True(t, ok, "kind row carries verbs")
	assert.Len(t, verbs, 3, "get, patch, delete all available")
	assert.NotContains(t, data, "commands", "kind-row carries no commands wrapper")
}

// TestSchemaUnknownSelectionNearestMatch covers the nearest match: an unknown
// selection is a VALIDATION error listing nearest matches - case-insensitive
// substring over command names, full alias forms, and kind names/aliases/keys.
func TestSchemaUnknownSelectionNearestMatch(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("alias substring matches, case-insensitive", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"PODS"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.Len(t, ce.Candidates, 1)
		assert.Equal(t, "get pod", ce.Candidates[0].Name)
		assert.Contains(t, ce.Message, "rancher schema")
		assert.Empty(t, ce.Hint, "the dual tier's guidance already names both commands in the message")
	})

	t.Run("name substring matches multiple commands", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"proj"}}, nil)
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
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"frobnicate"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Empty(t, ce.Candidates)
		assert.Contains(t, ce.Message, "rancher schema")
	})

	t.Run("kind names join the candidate set", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"widg"}}, nil)
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
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"kubeconf"}}, nil)
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
		_, err := kcmd.(ReadCommand).Execute(t.Context(),
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

	t.Run("message without --kind covers both commands and kinds", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"frobnicate"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Contains(t, ce.Message, "rancher schema -o name")
		assert.Contains(t, ce.Message, "rancher schema -o json")
		assert.Contains(t, ce.Message, "kinds array")
		assert.Empty(t, ce.Hint, "the dual tier's guidance already names both commands in the message")
	})

	t.Run("get before a command name is an unknown command", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get get pod"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, `unknown command "get get pod"; see the full command list: rancher schema -o name`, ce.Message)
		assert.Equal(t, "rancher schema -o name", ce.Hint)
	})

	t.Run("multi-token miss is command-shaped: single -o name hint, no kinds noise", func(t *testing.T) {
		t.Parallel()
		// `schema get pdo` is structurally a
		// command typo - kinds are single tokens, always - so the noun and
		// hint collapse to the command tier.
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pdo"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Contains(t, ce.Message, `unknown command "get pdo"`)
		assert.Equal(t, "rancher schema -o name", ce.Hint)
		assert.NotContains(t, ce.Hint, "kinds array")
	})

	t.Run("dual message on kind-shaped miss without --kind covers commands and kinds", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"pdo"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "rancher schema -o name")
		assert.Contains(t, ce.Message, "rancher schema -o json")
		assert.Contains(t, ce.Message, "kinds array")
		assert.Empty(t, ce.Hint, "the dual tier's guidance already names both commands in the message")
	})

	t.Run("hint with --kind points to the kinds array command", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"frobnicate"}, flags: map[string]bool{"kind": true}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, "rancher schema -o json", ce.Hint, "the --kind tier's hint is a single paste-valid command")
		assert.Contains(t, ce.Message, "kinds array")
		assert.NotContains(t, ce.Hint, "-o name")
	})

	t.Run("hint with --kind and nearest matches still points to the kinds array command", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"kubeconf"}, flags: map[string]bool{"kind": true}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.NotEmpty(t, ce.Candidates)
		// Nearest-match candidates ride the structured Candidates field; the
		// Hint still names the one concrete command this tier points to.
		assert.Equal(t, "rancher schema -o json", ce.Hint)
		assert.NotContains(t, ce.Hint, "-o name")
	})

	// TestSchemaUnknownSelectionPaddedSingleTokenGetsDualHint asserts that
	// a single arg with surrounding whitespace (e.g. " pod") contains a space
	// but is a single token - strings.Fields splits it to one element, so it
	// must stay in the dual tier, not the command tier.
	t.Run("padded single-token selector gets dual guidance not command-only guidance", func(t *testing.T) {
		t.Parallel()
		kcmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := kcmd.(ReadCommand).Execute(t.Context(), tin{a: []string{" pod"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "kinds array",
			"padded single token must get the dual guidance, not the command-only guidance")
		assert.Contains(t, ce.Message, "rancher schema -o name")
		assert.Empty(t, ce.Hint, "the dual tier's guidance already names both commands in the message")
	})
}

// TestSchemaSelectionFormats is the W2a seam regression for the schema result
// itself: through the real pipeline, a selection renders the card (Text)
// under ""/"table" and the envelope over the filtered manifest (Data) under
// json/yaml/template - decoded assertions, never string-matched JSON.
func TestSchemaSelectionFormats(t *testing.T) {
	t.Parallel()
	newRun := func(t *testing.T, format string) (string, string) {
		t.Helper()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, tin{a: []string{"get", "pod"}},
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

	res, err := cmd.(ReadCommand).Execute(t.Context(), tin{}, nil)
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

	res, err := cmd.(ReadCommand).Execute(t.Context(), tin{}, nil)
	require.NoError(t, err)
	toc := viewTOC(t, res)
	assert.Equal(t, false, toc["readOnly"])
	assert.NotContains(t, toc, "readOnlySources")
}

// TestSchemaFilters covers the filters: --verb, --destructive, --contract, and
// --mutating filter the manifest's commands (composing with each other and
// with --search), prune Groups to the retained set, and render the
// filtered TOC as Text.
func TestSchemaFilters(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
	run := func(t *testing.T, in tin) (map[string]any, string) {
		t.Helper()
		res, err := cmd.(ReadCommand).Execute(t.Context(), in, nil)
		require.NoError(t, err)
		return viewTOC(t, res), res.Text
	}
	t.Run("--verb keeps commands whose first name token matches", func(t *testing.T) {
		t.Parallel()
		toc, text := run(t, tin{strs: map[string]string{"verb": "get"}})
		assert.Equal(t, []string{"get pod", "get project"}, groupCommandNames(t, toc))
		groups, _ := toc["groups"].([]any)
		require.Len(t, groups, 1, "sections prune to the retained set")
		assert.Equal(t, "get", groups[0].(map[string]any)["name"])
		assert.Contains(t, text, "2 commands match --verb get")
		assert.Contains(t, text, "get pod - List pods or show one")
	})

	t.Run("--destructive keeps true and dependsOnInput", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{flags: map[string]bool{"destructive": true}})
		assert.Equal(t, []string{"delete project"}, groupCommandNames(t, toc))
	})

	t.Run("--contract keeps the contract's commands", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{strs: map[string]string{"contract": "internal"}})
		assert.Equal(t, []string{"delete project"}, groupCommandNames(t, toc))
	})

	t.Run("--mutating keeps mutating commands", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{flags: map[string]bool{"mutating": true}})
		assert.Equal(t, []string{"delete project"}, groupCommandNames(t, toc))
	})

	t.Run("filters compose (AND)", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{
			strs:  map[string]string{"verb": "delete"},
			flags: map[string]bool{"destructive": true},
		})
		assert.Equal(t, []string{"delete project"}, groupCommandNames(t, toc))
	})

	t.Run("filters compose with --search", func(t *testing.T) {
		t.Parallel()
		toc, _ := run(t, tin{strs: map[string]string{"verb": "get", "search": "pods"}})
		assert.Equal(t, []string{"get pod"}, groupCommandNames(t, toc))
	})

	t.Run("composed filters can produce zero hits, naming every active filter", func(t *testing.T) {
		t.Parallel()
		toc, text := run(t, tin{strs: map[string]string{"verb": "get", "contract": "public"}})
		assert.Equal(t, float64(0), toc["commandCount"])
		assert.Nil(t, toc["groups"])
		assert.Equal(t, "no commands match --verb get --contract public\n", text)
	})
}

// TestSchemaSearch covers search: --search is a case-insensitive substring over
// name, aliases, usage, and desc; the filtered TOC shows the matching
// fragment; zero hits print a no-matches message naming the active filters and
// exit 0.
func TestSchemaSearch(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)

	t.Run("desc match is case-insensitive and shows the fragment", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(),
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
		res, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{strs: map[string]string{"search": "delete"}}, nil)
		require.NoError(t, err)
		toc := viewTOC(t, res)
		assert.Equal(t, float64(1), toc["commandCount"])
		groups, _ := toc["groups"].([]any)
		require.Len(t, groups, 1)
		entries := groups[0].(map[string]any)["commands"].([]any)
		assert.Equal(t, "delete project", entries[0].(map[string]any)["name"])
		assert.Contains(t, res.Text, "delete project - Delete a project")
		assert.NotContains(t, res.Text, "[", "a match visible in the line carries no fragment")
	})

	t.Run("zero hits exit 0 with a message naming the filters", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, tin{strs: map[string]string{"search": "zzz"}},
			IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
		require.Nil(t, ce, "zero hits are not an error")
		assert.Equal(t, "no commands match --search \"zzz\"\n", out.String())
	})
}

// TestSchemaSelectorFilterConflict covers the conflict rule: a positional
// selector (command or kind) or --kind combined with any of the five
// filter/search flags is a VALIDATION error naming the conflict.
func TestSchemaSelectorFilterConflict(t *testing.T) {
	t.Parallel()

	t.Run("command selector plus --verb", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"get", "pod"}, strs: map[string]string{"verb": "get"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--verb")
		assert.Contains(t, ce.Message, "selector")
		assert.Contains(t, ce.Message, "drop the selector or the filter")
	})

	t.Run("kind selector plus --search", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"crt"}, strs: map[string]string{"search": "pod"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--search")
	})

	t.Run("--kind plus a filter", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		_, err := cmd.(ReadCommand).Execute(t.Context(),
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
			_, err := cmd.(ReadCommand).Execute(t.Context(), in, nil)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
		}
	})
}

// TestSchemaGroupsCommandsConsistency asserts, for every result shape,
// that the flattened Groups[].Commands set-equals the manifest's
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
			// Groups alignment is a manifest invariant: assert on the manifest
			// directly for bare/filtered selections (their tocView JSON
			// collapses generated commands and does not carry a flat commands
			// list).
			var m Manifest
			switch tc.name {
			case "bare":
				m = full
			case "filtered", "search hit", "zero hit":
				m = filterManifest(full, filtersFrom(tc.in))
			default:
				res, err := cmd.(ReadCommand).Execute(t.Context(), tc.in, nil)
				require.NoError(t, err)
				if cv, ok := res.Data.(cardView); ok {
					// cardView carries only exitCodes+cmd; reconstruct the
					// single-command sub-manifest to verify the Groups
					// set-equals invariant for command selections.
					m = Manifest{
						Commands: []SchemaCommand{cv.cmd},
						Groups:   filterGroups(full.Groups, map[string]bool{cv.cmd.Name: true}),
					}
				} else if kv, ok := res.Data.(kindView); ok {
					// Groups alignment is a manifest invariant; kindView
					// carries only the kind row - no manifest to check - so
					// assert the row is non-empty and return early.
					assert.NotEmpty(t, kv.kind.Name)
					return
				} else {
					t.Fatalf("unexpected view type %T for %s", res.Data, tc.name)
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
			// For shapes that derive manifests directly (never calling Execute),
			// also verify via the live Execute path: decode the tocView JSON and
			// assert every groups[].commands[].name is in the expected command
			// set. For the bare shape, also assert no generated command name
			// appears in the groups (the bare tocJSON collapses them). For the
			// zero-hit shape, filterManifest prunes Groups to nil and
			// tocJSON.Groups carries omitempty - assert the key is absent
			// entirely (the omitted-on-zero-hit contract as a checked fact).
			if tc.name != "bare" && tc.name != "filtered" && tc.name != "search hit" && tc.name != "zero hit" {
				return
			}
			res, err := cmd.(ReadCommand).Execute(t.Context(), tc.in, nil)
			require.NoError(t, err)
			decoded := viewTOC(t, res)
			if tc.zeroHit {
				assert.NotContains(t, decoded, "groups", "omitempty on zero-hit must elide the groups key")
			} else {
				gen := generatedCommandNames(full.Kinds)
				for _, name := range groupCommandNames(t, decoded) {
					assert.True(t, want[name], "group command %q not in expected command set for %s", name, tc.name)
					if tc.name == "bare" {
						assert.False(t, gen[name], "generated command %q must not appear in bare groups", name)
					}
				}
			}
		})
	}
}

// completionRegistry builds the shape exemplars for positional completion: a
// verbed command whose aliases are also kind identifiers (get pod), a verbed
// command whose alias is NOT a kind identifier (get project → projects, so the
// bare-leaf-alias exclusion is observable), the kubeconfig command/kind
// collision, and a kind whose key differs from its name (cluster →
// managementcluster).
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

// TestSchemaPositionalCompletionCandidatesResolve is the shipping gate:
// every candidate CompleteArgs offers must resolve through the actual
// selection logic in Execute - a suggestion `rancher schema` would then
// reject cannot ship. Run against every registry fixture so command-only,
// collision-heavy, and alias-heavy surfaces are all covered.
func TestSchemaPositionalCompletionCandidatesResolve(t *testing.T) {
	t.Parallel()
	regs := map[string]*Registry{
		"commands only":         cardRegistry(),
		"kinds plus collision":  kindRegistry(),
		"aliased verb commands": completionRegistry(),
		"groups":                groupCardRegistry(),
	}
	for name, reg := range regs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := NewSchemaCommand(reg, "test", "2.14", nil)
			sc, ok := cmd.(ShellCompleter)
			require.True(t, ok, "schema must implement ShellCompleter")
			cands := sc.CompleteArgs(t.Context(), nil, nil)
			require.NotEmpty(t, cands)
			for _, cand := range cands {
				res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: strings.Fields(cand)}, nil)
				require.NoError(t, err, "candidate %q must resolve as a schema selector", cand)
				if cv, ok := res.Data.(cardView); ok {
					assert.NotEmpty(t, cv.cmd.Name, "candidate %q must resolve to a named command", cand)
				} else if kv, ok := res.Data.(kindView); ok {
					assert.NotEmpty(t, kv.kind.Name, "candidate %q must select one kind", cand)
				} else if gv, ok := res.Data.(groupView); ok {
					assert.NotEmpty(t, gv.group.Name, "candidate %q must select one group", cand)
				} else {
					t.Fatalf("unexpected view type %T for candidate %q", res.Data, cand)
				}
			}
		})
	}
}

// TestSchemaAllReturnsFullManifest asserts --all as exact delegation: the
// result marshals byte-equivalent to the registry's own manifest -
// nothing filtered, nothing added. Text is the TOC (the
// table view stays the TOC under --all; structured output carries the
// full manifest). The self-referential footer must be absent.
func TestSchemaAllReturnsFullManifest(t *testing.T) {
	t.Parallel()
	reg := kindRegistry()
	cmd := NewSchemaCommand(reg, "v-test", "2.14", nil)
	res, err := cmd.(ReadCommand).Execute(t.Context(), tin{flags: map[string]bool{"all": true}}, nil)
	require.NoError(t, err)
	got, err := json.Marshal(res.Data)
	require.NoError(t, err)
	want, err := json.Marshal(reg.Schema("v-test", "2.14"))
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got), "--all must delegate the complete, unchanged manifest")

	assert.True(t, strings.HasPrefix(res.Text, "rancher v-test - "),
		"--all text is the TOC, headed by the CLI version and counts")
	assert.NotContains(t, res.Text, "full manifest: rancher schema --all",
		"--all text must not carry the self-referential footer")
}

// TestSchemaAllConflicts asserts the VALIDATION conflicts: --all crossed with
// a selector, any filter, or --kind.
func TestSchemaAllConflicts(t *testing.T) {
	t.Parallel()
	reg := kindRegistry()
	cmd := NewSchemaCommand(reg, "v-test", "2.14", nil)
	cases := []tin{
		{flags: map[string]bool{"all": true}, a: []string{"get", "pod"}},
		{flags: map[string]bool{"all": true}, strs: map[string]string{"search": "pod"}},
		{flags: map[string]bool{"all": true, "kind": true}},
		{flags: map[string]bool{"all": true, "destructive": true}},
	}
	for _, in := range cases {
		_, err := cmd.(ReadCommand).Execute(t.Context(), in, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--all")
	}
}

// TestSchemaResultStyledTextWiring asserts which schema views carry a
// StyledText builder: the bare TOC, --all, the filtered TOC, the command
// card, and the group card each set one whose active-style output strips
// back to Text exactly; the kind card sets none.
func TestSchemaResultStyledTextWiring(t *testing.T) {
	t.Parallel()
	active := style.Style{Enabled: true}

	t.Run("bare TOC", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.NotEqual(t, res.Text, res.StyledText(active))
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("all manifest", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{flags: map[string]bool{"all": true}}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("filtered TOC", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{strs: map[string]string{"verb": "get"}}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("command card", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(cardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("command card carries its kind-collision footnote through styling", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"kubeconfig"}}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("group card", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(groupCardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"create"}}, nil)
		require.NoError(t, err)
		require.NotNil(t, res.StyledText)
		assert.Equal(t, res.Text, style.Strip(res.StyledText(active)))
	})

	t.Run("kind card carries no StyledText", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(kindRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"crt"}}, nil)
		require.NoError(t, err)
		assert.Nil(t, res.StyledText)
	})
}

// TestSchemaSecondArgumentCompletionCandidatesResolve asserts the second
// argument's completion: for every first word a candidate starts with, each
// second word offered, typed after it, resolves through Execute to the
// command whose name or alias is the two words; a first word that begins
// no two-word selector gets nothing, and nothing is offered after two
// words. Run against every registry fixture.
func TestSchemaSecondArgumentCompletionCandidatesResolve(t *testing.T) {
	t.Parallel()
	regs := map[string]*Registry{
		"commands only":         cardRegistry(),
		"kinds plus collision":  kindRegistry(),
		"aliased verb commands": completionRegistry(),
		"groups":                groupCardRegistry(),
	}
	for name, reg := range regs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := NewSchemaCommand(reg, "test", "2.14", nil)
			sc := cmd.(ShellCompleter)
			first := map[string]bool{}
			twoWord := map[string]bool{}
			for _, cand := range sc.CompleteArgs(t.Context(), nil, nil) {
				w, _, two := strings.Cut(cand, " ")
				first[w] = true
				if two {
					twoWord[w] = true
				}
			}
			offered := 0
			for w := range first {
				cands := sc.CompleteArgs(t.Context(), nil, []string{w})
				if !twoWord[w] {
					assert.Empty(t, cands, "%q begins no two-word selector", w)
					continue
				}
				require.NotEmpty(t, cands, "%q begins a two-word selector", w)
				for _, cand := range cands {
					offered++
					res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{w, cand}}, nil)
					require.NoError(t, err, "schema %s %s must resolve", w, cand)
					cv, ok := res.Data.(cardView)
					require.True(t, ok, "schema %s %s must resolve to a command, got %T", w, cand, res.Data)
					assert.True(t, cv.cmd.Name == w+" "+cand || matchesAlias(cv.cmd, w+" "+cand),
						"schema %s %s resolved to %q", w, cand, cv.cmd.Name)
				}
				assert.Empty(t, sc.CompleteArgs(t.Context(), nil, []string{w, cands[0]}), "nothing after two words")
			}
			assert.Positive(t, offered, "the fixture has a two-word selector")
		})
	}
}

// TestSchemaPositionalCompletionShape asserts the candidate shape: command
// aliases appear only in their full typed form ("get pods", never a bare
// "projects"), kind names/keys/aliases appear as bare tokens, the
// command/kind collision (kubeconfig) dedupes to one candidate, and the set
// is deterministically sorted with no duplicates.
func TestSchemaPositionalCompletionShape(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(completionRegistry(), "test", "2.14", nil)
	sc, ok := cmd.(ShellCompleter)
	require.True(t, ok, "schema must implement ShellCompleter")
	cands := sc.CompleteArgs(t.Context(), nil, nil)

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

// groupCardRegistry builds a registry with two named groups - enough surface
// to exercise group card resolution, structured output, and completion.
func groupCardRegistry() *Registry {
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{
		Name: "create project", Usage: "Create a project", CommandType: ReadOnly,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name:        "create resource",
		Usage:       "Create a resource from a manifest file or stdin",
		CommandType: ReadOnly,
	}})
	reg.Register(fakeRead{d: Descriptor{
		Name: "auth login", Usage: "Log in to a Rancher server", CommandType: ReadOnly,
	}})
	reg.SetGroups([]SchemaGroup{
		{
			Name:     "create",
			Desc:     "Create Rancher resources. `create resource -f <file|->` is the generic path for any kind the kind table knows - one document per invocation.",
			Commands: []string{"create project", "create resource"},
		},
		{
			Name:     "auth",
			Desc:     "Authenticate with a Rancher server and manage API tokens.",
			Commands: []string{"auth login"},
		},
	})
	return reg
}

// TestSchemaGroupCardResolution covers the group card path: a positional that
// names a group (and matches no command or kind) resolves to a group card.
// Text carries the group name, desc, and member commands; JSON shape is
// {name, desc, commands:[{name,usage}], readOnly, readOnlySources}.
func TestSchemaGroupCardResolution(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(groupCardRegistry(), "test", "2.14", nil)

	t.Run("group name resolves to group card text", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"create"}}, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "group: create\n"),
			"group card text starts with the group name line")
		assert.Contains(t, res.Text, "create resource -f", "create desc mentions the generic -f path")
		assert.Contains(t, res.Text, "create project", "member command listed")
		assert.Contains(t, res.Text, "create resource", "member command listed")
	})

	t.Run("group card JSON shape", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"create"}}, nil)
		require.NoError(t, err)
		gv := viewGroup(t, res)
		b, err := json.Marshal(gv)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, "create", got["name"])
		desc, _ := got["desc"].(string)
		assert.NotEmpty(t, desc, "desc present in group JSON")
		assert.Contains(t, desc, "create resource -f", "create desc mentions the generic -f path")
		cmds, ok := got["commands"].([]any)
		require.True(t, ok, "commands must be a JSON array")
		assert.Len(t, cmds, 2)
		var names []string
		for i, e := range cmds {
			em, ok := e.(map[string]any)
			require.True(t, ok, "cmds[%d] must be a JSON object", i)
			name, ok := em["name"].(string)
			require.True(t, ok, "cmds[%d].name must be a string", i)
			names = append(names, name)
		}
		assert.Equal(t, []string{"create project", "create resource"}, names,
			"commands in registration order")
		_, hasReadOnly := got["readOnly"]
		assert.True(t, hasReadOnly, "readOnly present in group JSON (mirrors card and kind views)")
	})

	t.Run("Result.Kind is Schema", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"auth"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, "Schema", res.Kind)
		gv := viewGroup(t, res)
		assert.Equal(t, "auth", gv.group.Name)
	})
}

// TestSchemaGroupResolutionPrecedence covers the resolution-order contract:
// command match wins over a same-name group; a GROUP wins over a same-name
// kind (kinds are reachable via --kind, groups have no
// disambiguator), each collision card naming the other view.
func TestSchemaGroupResolutionPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("command wins over same-name group", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry(stubResolver{})
		reg.Register(fakeRead{d: Descriptor{Name: "auth", Usage: "Auth command", CommandType: Local}})
		reg.Register(fakeRead{d: Descriptor{Name: "auth login", Usage: "Log in", CommandType: ReadOnly}})
		reg.SetGroups([]SchemaGroup{
			{Name: "auth", Desc: "Auth group", Commands: []string{"auth login"}},
			{Name: "", Commands: []string{"auth"}},
		})
		cmd := NewSchemaCommand(reg, "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"auth"}}, nil)
		require.NoError(t, err)
		_, isCard := res.Data.(cardView)
		assert.True(t, isCard, "command wins: must resolve to command card, not group card")
	})

	t.Run("group wins over same-name kind, with the kind-view hint", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry(stubResolver{})
		reg.Register(fakeRead{d: Descriptor{
			Name: "create project", Usage: "Create a project", CommandType: ReadOnly,
		}})
		reg.SetKinds([]SchemaKind{{
			Name: "create", Key: "create", Resource: "creates",
			Scope: "management", NamespacePolicy: "none",
			Verbs: map[string]VerbState{"get": {State: VerbAbsent}},
		}})
		reg.SetGroups([]SchemaGroup{
			{Name: "create", Desc: "Create group", Commands: []string{"create project"}},
		})
		cmd := NewSchemaCommand(reg, "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"create"}}, nil)
		require.NoError(t, err)
		gv, isGroup := res.Data.(groupView)
		assert.True(t, isGroup, "group wins: a kind must not shadow a command group (--kind reaches the kind)")
		assert.Equal(t, "create", gv.kindHint, "the group card must name the colliding kind view")
		assert.Contains(t, res.Text, "kind view: rancher schema create --kind")
	})

	t.Run("--kind reaches the shadowed kind, with the group-view hint", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry(stubResolver{})
		reg.SetKinds([]SchemaKind{{
			Name: "create", Key: "create", Resource: "creates",
			Scope: "management", NamespacePolicy: "none",
			Verbs: map[string]VerbState{"get": {State: VerbAbsent}},
		}})
		reg.SetGroups([]SchemaGroup{
			{Name: "create", Desc: "Create group", Commands: []string{"create project"}},
		})
		cmd := NewSchemaCommand(reg, "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"create"}, flags: map[string]bool{"kind": true}}, nil)
		require.NoError(t, err)
		kv, isKind := res.Data.(kindView)
		assert.True(t, isKind, "--kind must resolve the kind card")
		assert.Equal(t, "create", kv.groupHint, "the kind card must name the colliding group view")
		assert.Contains(t, res.Text, "command group: rancher schema create")
	})
}

// TestSchemaGroupKindFlagBlocksGroupResolution asserts that --kind
// forces the kind-only interpretation, so a
// group-only selector with --kind must return kind-not-found (the kindOnly
// unknownSelection path), never fall through to the group tier.
// A positive case (kind selector + --kind) is already covered by
// TestSchemaSelectionCommandWinsCollision ("--kind flips the collision").
func TestSchemaGroupKindFlagBlocksGroupResolution(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(groupCardRegistry(), "test", "2.14", nil)

	t.Run("group-only selector with --kind → kind-not-found VALIDATION", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"auth"}, flags: map[string]bool{"kind": true}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "auth", "selector named in the error")
	})

	t.Run("group selector without --kind still resolves to group card", func(t *testing.T) {
		t.Parallel()
		res, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"auth"}}, nil)
		require.NoError(t, err)
		_, isGroup := res.Data.(groupView)
		assert.True(t, isGroup, "without --kind: group selector returns group card")
	})
}

// TestSchemaUnknownSelectionSuggestsGroups covers the nearest-match
// extension: a partial selector that substring-matches a group name returns
// the group as a candidate alongside command candidates. --kind excludes
// groups from the candidate set (consistent with the group-tier guard).
func TestSchemaUnknownSelectionSuggestsGroups(t *testing.T) {
	t.Parallel()
	cmd := NewSchemaCommand(groupCardRegistry(), "test", "2.14", nil)

	t.Run("partial group name appears in candidates", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"aut"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		var found bool
		for _, cand := range ce.Candidates {
			if cand.ID == "auth" {
				found = true
				break
			}
		}
		assert.True(t, found, `candidate "auth" must appear in nearest matches for "aut"`)
	})

	t.Run("--kind excludes groups from candidates", func(t *testing.T) {
		t.Parallel()
		_, err := cmd.(ReadCommand).Execute(t.Context(),
			tin{a: []string{"aut"}, flags: map[string]bool{"kind": true}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		for _, cand := range ce.Candidates {
			assert.NotEqual(t, "auth", cand.ID, "groups excluded from candidates under --kind")
		}
	})
}

// mixedGroupRegistry builds a registry whose "get" group contains both a
// bespoke command and generated per-kind commands - enough to assert the
// collapse behavior in text and JSON.
func mixedGroupRegistry() *Registry {
	ks := []SchemaKind{
		{
			Name: "pod", Key: "pod", Resource: "pods",
			Scope: "downstream", NamespacePolicy: "plain",
			Verbs: map[string]VerbState{"get": {State: VerbAvailable, Contract: "internal", Stability: "beta"}},
		},
		{
			Name: "cluster", Key: "managementcluster", Resource: "clusters",
			Scope: "management", NamespacePolicy: "none",
			Verbs: map[string]VerbState{"get": {State: VerbAvailable, Contract: "internal", Stability: "beta"}},
		},
	}
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{
		Name: "get cloud-credential", Usage: "Get a cloud credential", CommandType: ReadOnly,
	}})
	for _, k := range ks {
		reg.Register(fakeRead{d: Descriptor{Name: "get " + k.Name, CommandType: ReadOnly}})
	}
	reg.SetKinds(ks)
	reg.SetGroups([]SchemaGroup{
		{
			Name:     "get",
			Desc:     "Fetch resources.",
			Commands: []string{"get cloud-credential", "get pod", "get cluster"},
		},
	})
	return reg
}

// TestSchemaGroupCardCollapsesGeneratedCommands asserts that
// the group card reuses the TOC's generated-command
// collapse - bespoke commands listed individually; generated per-kind
// commands collapsed to one summary line. Both text and JSON are covered.
func TestSchemaGroupCardCollapsesGeneratedCommands(t *testing.T) {
	t.Parallel()

	t.Run("text lists bespoke and collapses generated", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(mixedGroupRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get"}}, nil)
		require.NoError(t, err)
		assert.Contains(t, res.Text, "get cloud-credential", "bespoke command listed")
		assert.Contains(t, res.Text, "Get a cloud credential", "bespoke usage listed")
		assert.Contains(t, res.Text, "get <kind>", "generated commands collapsed to pattern")
		assert.Contains(t, res.Text, "generated for 2 kinds", "count in collapsed line")
		assert.NotContains(t, res.Text, "\n  get pod\n", "generated command not expanded")
		assert.NotContains(t, res.Text, "\n  get cluster\n", "generated command not expanded")
	})

	t.Run("JSON has bespoke commands array + generated summary", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(mixedGroupRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get"}}, nil)
		require.NoError(t, err)
		gv := viewGroup(t, res)
		b, err := json.Marshal(gv)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		cmds, ok := got["commands"].([]any)
		require.True(t, ok, "commands must be a JSON array")
		assert.Len(t, cmds, 1, "only bespoke commands in the commands array")
		em, ok := cmds[0].(map[string]any)
		require.True(t, ok, "command entry must be a JSON object")
		assert.Equal(t, "get cloud-credential", em["name"])
		gen, ok := got["generated"].(map[string]any)
		require.True(t, ok, "generated summary must be present in JSON")
		assert.Equal(t, float64(2), gen["count"], "generated count")
		assert.Equal(t, "get <kind>", gen["pattern"], "generated pattern")
	})

	t.Run("all-bespoke group has no generated key in JSON", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(groupCardRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"auth"}}, nil)
		require.NoError(t, err)
		gv := viewGroup(t, res)
		b, err := json.Marshal(gv)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		assert.NotContains(t, got, "generated", "all-bespoke group omits generated key")
		cmds, ok := got["commands"].([]any)
		require.True(t, ok)
		assert.Len(t, cmds, 1, "auth group has one bespoke command")
	})

	t.Run("size sanity: mixed get-group card stays small", func(t *testing.T) {
		t.Parallel()
		cmd := NewSchemaCommand(mixedGroupRegistry(), "test", "2.14", nil)
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: []string{"get"}}, nil)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimRight(res.Text, "\n"), "\n")
		assert.Less(t, len(lines), 15, "group card stays compact even with many generated commands")
	})
}

// TestSchemaGroupCompletionCandidates covers groups: every named
// group is offered as a completion candidate, unnamed ("") sections are not,
// and every group candidate resolves through Execute.
func TestSchemaGroupCompletionCandidates(t *testing.T) {
	t.Parallel()
	reg := groupCardRegistry()
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)
	sc, ok := cmd.(ShellCompleter)
	require.True(t, ok)

	cands := sc.CompleteArgs(t.Context(), nil, nil)
	assert.Contains(t, cands, "create", "named group is a completion candidate")
	assert.Contains(t, cands, "auth", "named group is a completion candidate")

	for _, cand := range cands {
		res, err := cmd.(ReadCommand).Execute(t.Context(), tin{a: strings.Fields(cand)}, nil)
		require.NoError(t, err, "candidate %q must resolve", cand)
		switch res.Data.(type) {
		case cardView, kindView, groupView:
			// all valid view types
		default:
			t.Fatalf("unexpected view type %T for candidate %q", res.Data, cand)
		}
	}
}

// TestSchemaFilterGroupsStripsDesc verifies that filterGroups omits Desc from
// filtered sections. A filtered section covers only the retained commands, not
// the full set the group desc was written for - carrying the desc would mislead
// a reader whose filter pruned the commands the desc describes.
func TestSchemaFilterGroupsStripsDesc(t *testing.T) {
	gs := []SchemaGroup{
		{Name: "auth", Desc: "Auth group desc", Commands: []string{"auth login", "auth logout"}},
		{Name: "", Desc: "Should also strip", Commands: []string{"schema"}},
	}
	keep := map[string]bool{"auth login": true, "schema": true}
	got := filterGroups(gs, keep)
	require.Len(t, got, 2)
	assert.Empty(t, got[0].Desc, "named section Desc must be stripped on the filtered path")
	assert.Empty(t, got[1].Desc, "unnamed section Desc must be stripped on the filtered path")
	assert.Equal(t, []string{"auth login"}, got[0].Commands)
	assert.Equal(t, []string{"schema"}, got[1].Commands)
}
