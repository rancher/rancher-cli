package runtime

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaGateLineMatrix is the SE-3 conformance test: the gate line is a
// total function of (commandType, destructive, stability) with exact wordings,
// enumerated over every combination. The expected strings restate the spec
// independently of the implementation.
func TestSchemaGateLineMatrix(t *testing.T) {
	t.Parallel()
	commandTypes := []CommandType{Local, ReadOnly, Mutating}
	destructives := []string{"", "false", "true", "dependsOnInput"}
	stabilities := []string{"", "stable", "beta", "deprecated"}
	for _, ct := range commandTypes {
		for _, d := range destructives {
			for _, st := range stabilities {
				var want string
				switch ct {
				case Local:
					want = "gates: none (local command)"
				case ReadOnly:
					want = "gates: none (read-only)"
				case Mutating:
					switch d {
					case "true":
						want = "gates: --apply --yes"
					case "dependsOnInput":
						want = "gates: --apply; --yes when the input is destructive (see description)"
					default:
						want = "gates: --apply"
					}
					if st == "deprecated" {
						want += " --allow-deprecated"
					}
				}
				assert.Equal(t, want, gateLine(ct, d, st),
					"commandType=%s destructive=%q stability=%q", ct, d, st)
			}
		}
	}
}

func cardManifest() Manifest {
	return Manifest{ExitCodes: map[string]int{"GENERIC": 1, "VALIDATION": 2, "AUTH": 3}}
}

func TestSchemaCommandCardReadOnly(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:    "get pod",
		Aliases: []string{"pods", "po"},
		Usage:   "List pods or show one",
		Desc:    "Reads pods on the downstream cluster via the management proxy.",
		Args:    []ArgSpec{{Name: "name", Type: "string"}},
		Flags: []ArgSpec{
			{Name: "cluster", Type: "string", Desc: "target cluster `id|name`"},
			{Name: "wait", Type: "bool", Desc: "wait for the resource"},
		},
		Examples:    []string{"rancher get pod -o json"},
		CommandType: ReadOnly,
		Network:     true,
		Auth:        true,
		Contract:    "internal",
		Stability:   "beta",
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	require.NotEmpty(t, lines)
	assert.Equal(t, "rancher get pod [name]", lines[0], "usage line with args leads the card")
	assert.Equal(t, "  List pods or show one", lines[1])
	assert.Contains(t, lines, "flags:")
	assert.Contains(t, lines, "  --cluster string  target cluster `id|name`")
	assert.Contains(t, lines, "  --wait bool  wait for the resource")
	assert.Contains(t, lines, "contract: readOnly · internal · beta · network · auth")
	assert.Contains(t, lines, "gates: none (read-only)")
	assert.Contains(t, lines, "Reads pods on the downstream cluster via the management proxy.")
	assert.Contains(t, lines, "examples:")
	assert.Contains(t, lines, "  rancher get pod -o json")
	assert.Contains(t, lines, "exit codes: GENERIC=1 · VALIDATION=2 · AUTH=3")
	assert.Contains(t, lines, "global flags: see `rancher schema -o json` (globalFlags)")
}

func TestSchemaCommandCardDestructiveMutating(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:  "delete project",
		Usage: "Delete a project",
		Desc:  "Removes the project and its namespaces after a server-side dry-run preview.",
		Args:  []ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []ArgSpec{
			{Name: "filename", Aliases: []string{"f"}, Type: "string", Required: true,
				Desc: "manifest file", Env: []string{"RANCHER_FILE"}},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type"},
		},
		CommandType: Mutating,
		Destructive: "true",
		Network:     true,
		Auth:        true,
		Contract:    "internal",
		Stability:   "deprecated",
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	require.NotEmpty(t, lines)
	assert.Equal(t, "rancher delete project <name>", lines[0], "required arg renders in angle brackets")
	assert.Contains(t, lines, "  --filename/-f string (required)  manifest file  [env: RANCHER_FILE]")
	assert.Contains(t, lines, "  --type user|group  subject type")
	assert.Contains(t, lines, "contract: mutating · internal · deprecated · destructive=true · network · auth")
	assert.Contains(t, lines, "gates: --apply --yes --allow-deprecated")
}

// TestSchemaCommandCardOmitsEmptySections pins the card's behavior for a
// minimal local command: no flags block, no examples block, no empty
// contract segments, and the footer still carries the exit strip + pointer.
func TestSchemaCommandCardOmitsEmptySections(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:        "config current-context",
		Usage:       "Print the current context",
		CommandType: Local,
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	assert.Equal(t, "rancher config current-context", lines[0])
	assert.NotContains(t, lines, "flags:")
	assert.NotContains(t, lines, "examples:")
	assert.Contains(t, lines, "contract: local")
	assert.Contains(t, lines, "gates: none (local command)")
	assert.Contains(t, lines, "exit codes: GENERIC=1 · VALIDATION=2 · AUTH=3")
	assert.Contains(t, lines, "global flags: see `rancher schema -o json` (globalFlags)")
}

// TestSchemaCommandCardDependsOnInput pins the SE-3 dependsOnInput wording on
// a rendered card (not just the gate function).
func TestSchemaCommandCardDependsOnInput(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:        "apply",
		Usage:       "Apply a manifest",
		CommandType: Mutating,
		Destructive: "dependsOnInput",
		Network:     true,
		Auth:        true,
		Contract:    "public",
		Stability:   "stable",
	}
	card := renderCommandCard(cardManifest(), c)
	assert.Contains(t, strings.Split(card, "\n"),
		"gates: --apply; --yes when the input is destructive (see description)")
}

// TestSchemaKindCardFields pins the SE-5 kind card: identity (canonical
// name, key, aliases), GVR, scope, the namespace/routing line, wait default,
// traits, and the verb table in get/patch/delete order.
func TestSchemaKindCardFields(t *testing.T) {
	t.Parallel()
	k := SchemaKind{
		Name: "gitrepo", Key: "gitrepo", Aliases: []string{"gitrepos"},
		Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "gitrepos",
		Scope: "management", NamespacePolicy: "workspace", DefaultNamespace: "fleet-default",
		WaitDefault: "condition=Ready", Sensitive: true, Completable: true,
		Verbs: map[string]VerbState{
			"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
			"patch":  {State: VerbAvailable, Contract: "internal", Stability: "beta"},
			"delete": {State: VerbAbsent, Note: "controller-owned; delete the owning source object"},
		},
	}
	card := renderKindCard(k)
	lines := strings.Split(card, "\n")

	assert.Equal(t, "kind: gitrepo", lines[0])
	assert.Contains(t, lines, "key: gitrepo")
	assert.Contains(t, lines, "aliases: gitrepos")
	assert.Contains(t, lines, "group: fleet.cattle.io")
	assert.Contains(t, lines, "version: v1alpha1")
	assert.Contains(t, lines, "resource: gitrepos")
	assert.Contains(t, lines, "scope: management")
	assert.Contains(t, lines, "namespace: workspace; named ops take --namespace (default fleet-default)")
	assert.Contains(t, lines, "wait: bare --wait waits for condition=Ready")
	assert.Contains(t, lines, "traits: sensitive · completable")

	verbsAt := slices.Index(lines, "verbs:")
	require.GreaterOrEqual(t, verbsAt, 0)
	assert.Equal(t, "  get: available (internal · beta)", lines[verbsAt+1])
	assert.Equal(t, "  patch: available (internal · beta)", lines[verbsAt+2])
	assert.Equal(t, "  delete: not available — controller-owned; delete the owning source object", lines[verbsAt+3])
}

// TestSchemaKindCardOmitsEmptyFields pins the card for a minimal kind: no
// aliases, group, version, wait, or traits lines when those fields are empty.
func TestSchemaKindCardOmitsEmptyFields(t *testing.T) {
	t.Parallel()
	k := SchemaKind{
		Name: "widget", Key: "widget", Resource: "widgets",
		Scope: "downstream", NamespacePolicy: "plain",
		Verbs: map[string]VerbState{
			"get":    {State: VerbAvailable, Contract: "internal", Stability: "beta"},
			"patch":  {State: VerbAvailable, Contract: "internal", Stability: "beta"},
			"delete": {State: VerbAbsent},
		},
	}
	lines := strings.Split(renderKindCard(k), "\n")

	assert.Equal(t, "kind: widget", lines[0])
	for _, l := range lines {
		assert.False(t, strings.HasPrefix(l, "aliases:"))
		assert.False(t, strings.HasPrefix(l, "group:"))
		assert.False(t, strings.HasPrefix(l, "version:"))
		assert.False(t, strings.HasPrefix(l, "wait:"))
		assert.False(t, strings.HasPrefix(l, "traits:"))
	}
	assert.Contains(t, lines, "  delete: not available")
}

// TestSchemaKindNamespaceLine enumerates the namespace-policy routing line:
// the policy plus the routing flags a named op needs, per the kinds package's
// policy semantics carried into the manifest.
func TestSchemaKindNamespaceLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		policy, scope, defaultNS, want string
	}{
		{"none", "management", "", "namespace: none (cluster-scoped)"},
		{"none", "downstream", "", "namespace: none (cluster-scoped); named ops need --cluster"},
		{"cluster", "management", "", "namespace: cluster (the namespace is a cluster id); named ops need --cluster"},
		{"workspace", "management", "fleet-default", "namespace: workspace; named ops take --namespace (default fleet-default)"},
		{"plain", "management", "", "namespace: plain; named ops need --namespace"},
		{"plain", "downstream", "", "namespace: plain; named ops need --namespace and --cluster"},
	}
	for _, tc := range cases {
		got := kindNamespaceLine(SchemaKind{
			NamespacePolicy: tc.policy, Scope: tc.scope, DefaultNamespace: tc.defaultNS,
		})
		assert.Equal(t, tc.want, got, "policy=%s scope=%s", tc.policy, tc.scope)
	}
}

// TestSchemaKindVerbLine covers the SE-5 wording as a function of VerbState,
// including the degenerate shapes production data never produces.
func TestSchemaKindVerbLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vs   VerbState
		want string
	}{
		{"available with binding", VerbState{State: VerbAvailable, Contract: "internal", Stability: "beta"}, "get: available (internal · beta)"},
		{"available without binding", VerbState{State: VerbAvailable}, "get: available"},
		{"refused with note", VerbState{State: VerbRefused, Note: "read-only: consumed, not edited"}, "get: refused — read-only: consumed, not edited"},
		{"refused without note", VerbState{State: VerbRefused}, "get: refused"},
		{"reasoned absence", VerbState{State: VerbAbsent, Note: "expires on its own TTL"}, "get: not available — expires on its own TTL"},
		{"plain absence", VerbState{State: VerbAbsent}, "get: not available"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, verbLine("get", tc.vs), tc.name)
	}
}

func TestSchemaExitCodeStripOrdersByExitThenName(t *testing.T) {
	t.Parallel()
	got := exitCodeStrip(map[string]int{
		"RBAC": 6, "VALIDATION": 2, "GENERIC": 1, "AUTH": 3, "AMBIGUOUS_SELECTION": 2,
	})
	assert.Equal(t,
		"exit codes: GENERIC=1 · AMBIGUOUS_SELECTION=2 · VALIDATION=2 · AUTH=3 · RBAC=6",
		got)
}

// tocManifest is a hand-ordered manifest exercising every SE-9 TOC shape: a
// leading unnamed section, a named section, a generated group that also holds
// a non-generated command, a later unnamed section, and a second generated
// group that must NOT render a second vocabulary block.
func tocManifest() Manifest {
	return Manifest{
		CLIVersion: "v9.9",
		ExitCodes:  map[string]int{"GENERIC": 1, "VALIDATION": 2},
		Commands: []SchemaCommand{
			{Name: "auth login", Usage: "Log in"},
			{Name: "completion", Usage: "Emit a completion script"},
			{Name: "get kubernetes-versions", Usage: "List installable versions"},
			{Name: "get pod", Usage: "List pods"},
			{Name: "grant", Usage: "Grant a role"},
			{Name: "patch pod", Usage: "Patch a pod"},
			{Name: "schema", Usage: "Print the capability manifest"},
		},
		Kinds: []SchemaKind{{Name: "pod", Verbs: map[string]VerbState{
			"get":    {State: VerbAvailable},
			"patch":  {State: VerbAvailable},
			"delete": {State: VerbAbsent},
		}}},
		Groups: []SchemaGroup{
			{Name: "", Commands: []string{"schema", "completion"}},
			{Name: "auth", Commands: []string{"auth login"}},
			{Name: "get", Commands: []string{"get pod", "get kubernetes-versions"}},
			{Name: "", Commands: []string{"grant"}},
			{Name: "patch", Commands: []string{"patch pod"}},
		},
	}
}

// TestSchemaTOCSectionOrderAndVocabularyBlock covers SE-9: the bare TOC
// renders the manifest's sections in order, one line per non-generated
// command, with generated get/patch/delete collapsed into ONE kind-vocabulary
// block at the position of the first generated command's group, counts
// derived from the manifest, a version+counts header, and the exit-code +
// `-o json` footer.
func TestSchemaTOCSectionOrderAndVocabularyBlock(t *testing.T) {
	t.Parallel()
	text := renderTOC(tocManifest())
	lines := strings.Split(text, "\n")

	assert.Equal(t, "rancher v9.9 — 7 commands · 1 kinds", lines[0],
		"header carries the CLI version and derived counts")

	idx := func(line string) int {
		i := slices.Index(lines, line)
		require.GreaterOrEqual(t, i, 0, "missing TOC line %q\nfull TOC:\n%s", line, text)
		return i
	}
	schemaAt := idx("schema — Print the capability manifest")
	completionAt := idx("completion — Emit a completion script")
	authHdrAt := idx("auth:")
	authAt := idx("  auth login — Log in")
	blockAt := idx("get/patch/delete <kind> — 2 generated commands across 1 kinds")
	blockPointerAt := idx("  kind view: rancher schema <kind>")
	getHdrAt := idx("get:")
	kvAt := idx("  get kubernetes-versions — List installable versions")
	grantAt := idx("grant — Grant a role")
	stripAt := idx("exit codes: GENERIC=1 · VALIDATION=2")
	pointerAt := idx("full manifest: rancher schema -o json")

	assert.Less(t, schemaAt, completionAt, "unnamed leading section keeps registration order")
	assert.Less(t, completionAt, authHdrAt)
	assert.Less(t, authHdrAt, authAt)
	assert.Less(t, authAt, blockAt, "the block sits at the first generated group's position")
	assert.Equal(t, blockAt+1, blockPointerAt)
	assert.Less(t, blockPointerAt, getHdrAt, "the generated group's own non-generated commands follow the block")
	assert.Less(t, getHdrAt, kvAt)
	assert.Less(t, kvAt, grantAt, "the later unnamed section renders at its own position")
	assert.Less(t, grantAt, stripAt)
	assert.Less(t, stripAt, pointerAt)

	assert.NotContains(t, lines, "  get pod — List pods", "generated commands collapse into the block")
	assert.NotContains(t, lines, "patch:", "a fully generated section renders nothing of its own")
	assert.Equal(t, 1, strings.Count(text, "get/patch/delete <kind>"), "exactly one vocabulary block")
}

// TestSchemaSearchFragment covers SE-11's fragment rendering: a match visible
// in the rendered line (name or usage) carries no fragment; an alias match
// names the full alias form; a desc match shows the surrounding fragment with
// ellipses.
func TestSchemaSearchFragment(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name: "get pod", Aliases: []string{"pods", "po"}, Usage: "List pods or show one",
		Desc: "Reads pods on the downstream cluster via the management proxy, one page at a time.",
	}
	assert.Equal(t, "", searchFragment(c, "get pod"), "name match is visible in the line")
	assert.Equal(t, "", searchFragment(c, "show one"), "usage match is visible in the line")
	assert.Equal(t, "alias: get pods", searchFragment(c, "get pods"),
		"an alias-only match names the full alias form")

	frag := searchFragment(c, "MANAGEMENT")
	assert.Contains(t, frag, "desc: ")
	assert.Contains(t, frag, "management proxy")
	assert.Contains(t, frag, "…", "a mid-desc match is clipped with ellipses")

	assert.Equal(t, "", searchFragment(c, "zzz"), "no match yields no fragment")
}

// TestTOCRendersReadOnlyLine pins the spec'd read-only mirror: BOTH TOC text views gain
// a read-only line when active, so tocView's unconditional readOnly
// fields mirror text on the bare and filtered paths alike.
func TestTOCRendersReadOnlyLine(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	assert.Contains(t, renderTOC(m), "read-only (RANCHER_CLI_READONLY)")
	assert.Contains(t, renderFilteredTOC(m, schemaFilters{verb: "get"}), "read-only (RANCHER_CLI_READONLY)")

	// Zero-hit filtered result: the early-return branch must carry the
	// read-only line too (review P1).
	empty := readOnlyTOCManifest()
	empty.Commands = nil
	assert.Contains(t, renderFilteredTOC(empty, schemaFilters{verb: "zzz"}), "read-only (RANCHER_CLI_READONLY)")

	m.ReadOnly = false
	m.ReadOnlySources = nil
	assert.NotContains(t, renderTOC(m), "read-only")
	assert.NotContains(t, renderFilteredTOC(m, schemaFilters{verb: "get"}), "read-only")
}
