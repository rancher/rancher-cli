package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"
)

func TestBuildRootValidatesRegistry(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("test")
	if err != nil {
		t.Fatalf("BuildRoot on the real wiring must pass validation: %v", err)
	}
	if root == nil {
		t.Fatal("nil root")
	}
}

// fakeEngineCmd is a read command with a caller-chosen name, for exercising the
// registration loop's validation in isolation.
type fakeEngineCmd struct{ name string }

func (f fakeEngineCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: f.name, CommandType: runtime.ReadOnly}
}
func (f fakeEngineCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

func findCommand(cmds []*ucli.Command, name string) *ucli.Command {
	for _, c := range cmds {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func schemaCommandNames(t *testing.T) []string {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	names := make([]string, len(m.Commands))
	for i, c := range m.Commands {
		names[i] = c.Name
	}
	return names
}

// schemaCommandBinding returns the contract/stability the schema manifest
// reports for op, and whether op was found at all.
func schemaCommandBinding(t *testing.T, op string) (contract, stability string, found bool) {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	for _, c := range m.Commands {
		if c.Name == op {
			return c.Contract, c.Stability, true
		}
	}
	return "", "", false
}

// TestLegacyTokenShimBindingIsInternalDeprecated pins the shim's position in
// the public-API contract: the top-level "token" command is internal
// (auth machinery, no backing kind row) and deprecated by design (it exists
// only for Rancher-generated kubeconfigs during migration).
func TestLegacyTokenShimBindingIsInternalDeprecated(t *testing.T) {
	contract, stability, found := schemaCommandBinding(t, "token")
	require.True(t, found, "the token shim must be registered")
	assert.Equal(t, "internal", contract)
	assert.Equal(t, "deprecated", stability)
}

// TestDeleteProvisioningClusterBindingTracksKindRow pins Finding 5's fix:
// "delete provisioning-cluster" has no generic binding (kinds.Bindings()
// skips it — the row is Deletable: false), so BuildRoot must compose an
// EXTRA resolver entry for it derived from the provisioningcluster row's own
// Binding, read via kinds.Lookup at composition time — never a hand-copied
// literal, which has no machine-checked link back to the row and can
// silently drift from a future edit to the row's Binding in kinds.go. This
// test asserts the schema-reported contract/stability for the op equals
// kinds.Lookup("provisioningcluster").Binding exactly; before the fix (a
// hand-copied literal in provider.NewResolver), temporarily editing the
// row's Binding in kinds.go to a different contract/stability made this test
// fail while the hand-copied literal silently kept the stale value — the
// exact drift this composition now closes.
func TestDeleteProvisioningClusterBindingTracksKindRow(t *testing.T) {
	pcRow, ok := kinds.Lookup("provisioningcluster")
	require.True(t, ok)

	contract, stability, found := schemaCommandBinding(t, "delete provisioning-cluster")
	require.True(t, found, "delete provisioning-cluster must be wired despite Deletable: false")
	assert.Equal(t, string(pcRow.Binding.Contract), contract)
	assert.Equal(t, string(pcRow.Binding.Stability), stability)
}

// TestSchemaVerbEnumMatchesCommandVerbs pins the --verb enum in schema's
// descriptor to the surface it filters: the enum is a static literal in
// schemaCmd.Describe(), so a new top-level verb (a new distinct first token of
// a command name) must fail here until the literal is updated. Runs against
// the full real registry — the only place every command is wired.
func TestSchemaVerbEnumMatchesCommandVerbs(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)

	seen := map[string]bool{}
	var wantVerbs []string
	var verbEnum []string
	for _, c := range m.Commands {
		verb, _, _ := strings.Cut(c.Name, " ")
		if !seen[verb] {
			seen[verb] = true
			wantVerbs = append(wantVerbs, verb)
		}
		if c.Name == "schema" {
			for _, f := range c.Flags {
				if f.Name == "verb" {
					verbEnum = f.Enum
				}
			}
		}
	}
	assert.ElementsMatch(t, wantVerbs, verbEnum,
		"schema --verb enum drifted from the registry's verbs; update the Enum literal in schemaCmd.Describe()")
}

// TestSchemaContractEnumMatchesCommandContracts pins the --contract enum to the
// contracts actually PRESENT in the manifest's commands, not every declared
// safety.Contract: an enum value with zero possible matches would make
// completion suggest an impossible filter that validation then accepts.
func TestSchemaContractEnumMatchesCommandContracts(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)

	seen := map[string]bool{}
	var wantContracts []string
	var contractEnum []string
	for _, c := range m.Commands {
		if c.Contract != "" && !seen[c.Contract] {
			seen[c.Contract] = true
			wantContracts = append(wantContracts, c.Contract)
		}
		if c.Name == "schema" {
			for _, f := range c.Flags {
				if f.Name == "contract" {
					contractEnum = f.Enum
				}
			}
		}
	}
	assert.ElementsMatch(t, wantContracts, contractEnum,
		"schema --contract enum drifted from the contracts present in the manifest; update the Enum literal in schemaCmd.Describe()")
}

// TestSchemaDisclosureSizes pins the point of progressive disclosure:
// the bare TOC and a narrowed card stay small (spec acceptance: TOC
// < 20KB, card < 3KB). A regression here means a view regrew a
// full-manifest section.
func TestSchemaDisclosureSizes(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()

	run := func(args ...string) int {
		root, err := BuildRoot("golden")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		require.NoError(t, root.Run(t.Context(), append([]string{"rancher", "--config", dir}, args...)))
		return out.Len()
	}
	assert.Less(t, run("schema", "-o", "json"), 20*1024, "bare TOC JSON")
	assert.Less(t, run("schema", "get", "pod", "-o", "json"), 3*1024, "command card JSON")
}

var update = flag.Bool("update", false, "rewrite golden files")

func TestSchemaManifestGolden(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("golden")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "--all", "-o", "json"}))

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	var manifest any
	require.NoError(t, json.Unmarshal(env.Data, &manifest))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(manifest))
	got := buf.Bytes()

	golden := filepath.Join("testdata", "schema.golden.json")
	if *update {
		require.NoError(t, os.WriteFile(golden, got, 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "schema manifest drifted from golden; run with -update if intended")
}

// TestSchemaTOCGolden pins the bare `schema -o json` TOC shape — the
// progressive-disclosure contract (spec 2026-08-06): orientation payload,
// not the manifest. Regenerate with -update; drift is a signal.
func TestSchemaTOCGolden(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("golden")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	var toc any
	require.NoError(t, json.Unmarshal(env.Data, &toc))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(toc))
	got := buf.Bytes()

	golden := filepath.Join("testdata", "schema-toc.golden.json")
	if *update {
		require.NoError(t, os.WriteFile(golden, got, 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "schema TOC drifted from golden; run with -update if intended")
}

func TestBuildRootWiresGeneratedCommands(t *testing.T) {
	names := schemaCommandNames(t)
	assert.Contains(t, names, "get pod")
	assert.Contains(t, names, "delete project")
	assert.NotContains(t, names, "delete cluster", "clusters are not deletable")
}

// TestKindOverridesReplaceExactlyTheirVerb pins the override mechanism: an
// overridden verb+kind resolves to the override's command, every other verb
// of that kind and every other kind stays generic, and no command name is
// registered twice. With the empty kindOverrideTable this task ships, this
// is a pure regression net for later tasks' overrides.
func TestKindOverridesReplaceExactlyTheirVerb(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	names := map[string]int{}
	var walk func(cmds []*ucli.Command, prefix string)
	walk = func(cmds []*ucli.Command, prefix string) {
		for _, c := range cmds {
			names[strings.TrimSpace(prefix+" "+c.Name)]++
			walk(c.Commands, prefix+" "+c.Name)
		}
	}
	walk(root.Commands, "")
	for n, count := range names {
		assert.Equal(t, 1, count, "command %q registered %d times", n, count)
	}
}

// markerCmd stands in for a real override: Describe().Name matches the
// generic constructor's name (the override contract kinds.Bindings() depends
// on — BINDING NOTE in the plan) while Usage carries a marker string so
// tests can tell it apart from the generic command. It implements
// ReadCommand (not just Command) because reg.Validate requires every
// registered command to satisfy ReadCommand or MutatingCommand.
type markerCmd struct{ k kinds.Kind }

func (m markerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "get " + m.k.Name, CommandType: runtime.ReadOnly, Usage: "MARKER", Shape: runtime.ShapeTabular}
}

func (m markerCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// TestKindOverrideSubstitutesExactlyItsVerb installs a marker override for
// one control kind and proves: the overridden verb resolves to the marker
// command, the kind's OTHER verbs stay generic, and an untouched kind is
// byte-identical. NOT t.Parallel — it mutates the package-level
// kindOverrideTable.
func TestKindOverrideSubstitutesExactlyItsVerb(t *testing.T) {
	orig := kindOverrideTable
	kindOverrideTable = map[string]kindOverride{
		"gitrepo": {get: func(k kinds.Kind, d runDeps) runtime.Command { return markerCmd{k} }},
	}
	t.Cleanup(func() { kindOverrideTable = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	gitrepoGet := findCommand(getGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoGet, "get gitrepo must still be registered")
	assert.Equal(t, "MARKER", gitrepoGet.Usage, "get gitrepo must resolve to the override marker, not the generic constructor")

	gitrepoKind, ok := kinds.Lookup("gitrepo")
	require.True(t, ok)
	genericPatch := resources.NewPatch(gitrepoKind, nil, nil).Describe()
	genericDelete := resources.NewDelete(gitrepoKind, nil).Describe()

	patchGroup := findCommand(root.Commands, "patch")
	require.NotNil(t, patchGroup)
	gitrepoPatch := findCommand(patchGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoPatch)
	assert.Equal(t, genericPatch.Usage, gitrepoPatch.Usage, "patch gitrepo must stay generic when only get is overridden")

	deleteGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, deleteGroup)
	gitrepoDelete := findCommand(deleteGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoDelete)
	assert.Equal(t, genericDelete.Usage, gitrepoDelete.Usage, "delete gitrepo must stay generic when only get is overridden")

	bundleKind, ok := kinds.Lookup("bundle")
	require.True(t, ok)
	genericBundleGet := resources.NewGet(bundleKind, nil).Describe()
	bundleGet := findCommand(getGroup.Commands, "bundle")
	require.NotNil(t, bundleGet)
	assert.Equal(t, genericBundleGet.Usage, bundleGet.Usage, "get bundle (an untouched kind) must be byte-identical to the generic")
}

// TestSchemaManifestExposesProjectScope pins PS-6: a generated get command
// for a ProjectScopable kind (pod: NSPlain+Downstream) advertises
// projectScope: true, while a fixed command (schema itself) does not.
func TestSchemaManifestExposesProjectScope(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)

	byName := map[string]bool{}
	for _, c := range m.Commands {
		byName[c.Name] = c.ProjectScope
	}

	scopable, ok := byName["get pod"]
	require.True(t, ok, "get pod must be a registered command")
	assert.True(t, scopable, "pod is NSPlain+Downstream, so get pod must be projectScope: true")

	fixed, ok := byName["schema"]
	require.True(t, ok, "schema must be a registered command")
	assert.False(t, fixed, "a fixed command must not advertise projectScope")
}

func TestBuildRootWiresAliases(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	pod := findCommand(getGroup.Commands, "pod")
	require.NotNil(t, pod, "get group must have a pod leaf")
	assert.Contains(t, pod.Aliases, "pods")
	assert.Contains(t, pod.Aliases, "po")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	project := findCommand(delGroup.Commands, "project")
	require.NotNil(t, project, "delete group must have a project leaf")
	assert.Contains(t, project.Aliases, "projects")
}

// TestAdaptEntrySetsArgsUsageFromDescriptorArgs pins PS-9's second adapter
// site: adaptEntry (every generated get/patch/delete leaf) sets ArgsUsage
// from the same Descriptor.Args the manifest publishes, exactly like Adapt.
func TestAdaptEntrySetsArgsUsageFromDescriptorArgs(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	pod := findCommand(getGroup.Commands, "pod")
	require.NotNil(t, pod)
	assert.Equal(t, "[name]", pod.ArgsUsage, "get pod's name argument is optional")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	delPod := findCommand(delGroup.Commands, "pod")
	require.NotNil(t, delPod)
	assert.Equal(t, "<name>", delPod.ArgsUsage, "delete always addresses a resource by name")
}

func TestAssembleCommandsUnknownGroupReachable(t *testing.T) {
	t.Parallel()
	leaf := &ucli.Command{Name: "widget"}
	out := assembleCommands(nil, []string{"update"}, map[string][]*ucli.Command{"update": {leaf}})
	grp := findCommand(out, "update")
	require.NotNil(t, grp, "a group not in the usage allowlist must still be reachable")
	require.NotNil(t, findCommand(grp.Commands, "widget"))
}

func TestWireEntryRejectsThreeTokenName(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(runtime.MapResolver(provider.NewResolver()), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(resolver)

	bad := entry{group: "get", make: func(runDeps) runtime.Command {
		return fakeEngineCmd{name: "get pod extra"}
	}}
	_, _, werr := wireEntry(reg, resolver, "", bad)
	require.Error(t, werr)
	assert.ErrorContains(t, werr, "token")
}

// TestBuildRootWiresProvisioningClusterDelete pins the D-6/R1-1 gotcha for
// real: provisioningcluster's row stays Deletable: false, so
// kinds.Bindings() never emits "delete provisioning-cluster" — the ONLY
// reason BuildRoot succeeds with the del override registered is the extra
// resolver entry BuildRoot composes for that exact op string, derived from
// the row's own Binding (see TestDeleteProvisioningClusterBindingTracksKindRow
// for the derivation check). Before that entry existed in any form, this test
// failed with wireEntry's "no resolver binding" error (verified during TDD
// and again at self-review by temporarily removing the entry) — the
// regression net this test now stands guard for.
func TestBuildRootWiresProvisioningClusterDelete(t *testing.T) {
	// Not t.Parallel: schemaCommandNames calls t.Setenv, which panics when
	// combined with t.Parallel (same constraint TestBuildRootWiresGeneratedCommands
	// and TestSchemaManifestExposesProjectScope already observe).
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	leaf := findCommand(delGroup.Commands, "provisioning-cluster")
	require.NotNil(t, leaf, "delete provisioning-cluster must be wired despite Deletable: false")

	names := schemaCommandNames(t)
	assert.Contains(t, names, "delete provisioning-cluster")
}

// markerDelCmd stands in for a real bespoke delete override on a
// hypothetical SECOND Deletable: false kind, proving BuildRoot's
// del-override resolver loop generalizes past provisioningcluster (its only
// production instance today). Describe().Name matches the generic delete
// constructor's name for k (the override contract kinds.Bindings() depends
// on); it implements MutatingCommand, like every real delete command.
type markerDelCmd struct{ k kinds.Kind }

func (m markerDelCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "delete " + m.k.Name, CommandType: runtime.Mutating, Usage: "MARKER-DEL", Shape: runtime.ShapeText}
}

func (m markerDelCmd) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return safety.Effect{}, nil, nil
}

// TestDeleteOverrideResolverGeneralizesBeyondProvisioningCluster pins Finding
// 5 (round 2)'s fix: BuildRoot's extra delete-binding resolver is derived by
// looping kindOverrideTable for every del override on a Deletable: false
// row, not by naming "provisioningcluster" and its op literally. "node"
// (Deletable: false, no del override in production) stands in for a
// hypothetical second such kind: installing a del override for it here
// must, with zero BuildRoot code changes, wire "delete node" and resolve it
// to node's own row Binding — proving the loop's generality rather than a
// coincidence that only happens to work for the one production entry. NOT
// t.Parallel — it mutates the package-level kindOverrideTable (same
// constraint as TestKindOverrideSubstitutesExactlyItsVerb).
func TestDeleteOverrideResolverGeneralizesBeyondProvisioningCluster(t *testing.T) {
	orig := kindOverrideTable
	nodeKind, ok := kinds.Lookup("node")
	require.True(t, ok)
	require.False(t, nodeKind.Deletable, "node must stay non-deletable for this test to exercise the del-override path")

	overrides := make(map[string]kindOverride, len(orig)+1)
	maps.Copy(overrides, orig)
	overrides["node"] = kindOverride{del: func(k kinds.Kind, d runDeps) runtime.Command { return markerDelCmd{k} }}
	kindOverrideTable = overrides
	t.Cleanup(func() { kindOverrideTable = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	nodeDelete := findCommand(delGroup.Commands, "node")
	require.NotNil(t, nodeDelete, "delete node must be wired from a del override on a Deletable: false row")
	assert.Equal(t, "MARKER-DEL", nodeDelete.Usage)

	contract, stability, found := schemaCommandBinding(t, "delete node")
	require.True(t, found, "delete node must resolve a binding derived from node's own row")
	assert.Equal(t, string(nodeKind.Binding.Contract), contract)
	assert.Equal(t, string(nodeKind.Binding.Stability), stability)

	// provisioningcluster's own del override must still resolve too — the
	// loop composes every matching row, not just the last one iterated.
	pcDelete := findCommand(delGroup.Commands, "provisioning-cluster")
	require.NotNil(t, pcDelete, "delete provisioning-cluster must still be wired alongside the new override")
}

// TestDeleteOverrideResolverSkipsDeletableRowToAvoidCollision pins Finding 3
// (fix-round 3): the delOverrides loop's "if row.Deletable { continue }"
// branch had no test ever exercising it — every del override in production,
// and TestDeleteOverrideResolverGeneralizesBeyondProvisioningCluster's own
// "node" stand-in, sits on a Deletable: false row. The comment above the
// loop claims composing an extra "delete <name>" entry for a Deletable: true
// row would collide with kinds.Bindings()'s own entry for that op — this
// test proves that claim rather than leaving it asserted-but-unverified.
// "project" (Deletable: true, no override in production) stands in for a
// hypothetical del override installed on an already-deletable kind:
// installing one here and still getting a successful BuildRoot demonstrates
// the branch genuinely prevents the ComposeResolvers duplicate-binding
// error, not just "happens not to matter today" (verified during TDD by
// temporarily removing the branch, which turned this test's BuildRoot call
// into the "operation \"delete project\" resolved by both resolvers" error).
// NOT t.Parallel — it mutates the package-level kindOverrideTable (same
// constraint as the sibling tests in this file).
func TestDeleteOverrideResolverSkipsDeletableRowToAvoidCollision(t *testing.T) {
	orig := kindOverrideTable
	projectKind, ok := kinds.Lookup("project")
	require.True(t, ok)
	require.True(t, projectKind.Deletable, "project must stay deletable for this test to exercise the collision-avoidance branch")

	overrides := make(map[string]kindOverride, len(orig)+1)
	maps.Copy(overrides, orig)
	overrides["project"] = kindOverride{del: func(k kinds.Kind, d runDeps) runtime.Command { return markerDelCmd{k} }}
	kindOverrideTable = overrides
	t.Cleanup(func() { kindOverrideTable = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err, "a del override on a Deletable: true row must not collide with kinds.Bindings()'s own entry for that op")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	projectDelete := findCommand(delGroup.Commands, "project")
	require.NotNil(t, projectDelete, "delete project must still be wired")
	assert.Equal(t, "MARKER-DEL", projectDelete.Usage, "the override's del constructor must still be used for the generated command")
}

func TestBuildRootFailsOnEngineEntryWithoutBinding(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(runtime.MapResolver(provider.NewResolver()), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(resolver)

	// An engine entry whose Describe().Name is absent from the composed resolver
	// must fail wiring (deferral (a): read OR mutating engine commands need a
	// binding). The name's group matches so the leaf/group check passes first.
	bad := entry{group: "get", make: func(runDeps) runtime.Command {
		return fakeEngineCmd{name: "get gizmo"}
	}}
	_, _, werr := wireEntry(reg, resolver, "", bad)
	require.Error(t, werr)
	assert.ErrorContains(t, werr, "binding")
}

func TestSchemaCommandEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	err = root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "-o", "json"})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &m))
	data := m["data"].(map[string]any)
	assert.Equal(t, "cli.rancher.io/v1", data["outputApiVersion"])
	count, _ := data["commandCount"].(float64)
	assert.True(t, count > 0, "commandCount must be positive")
	assert.Equal(t, config.MinRancherVersion, data["minRancherVersion"])
	assert.Equal(t, false, data["readOnly"], "no config/env/flag read-only source is active for this invocation")

	// The --all path must carry a non-empty globalFlags array (full manifest, not TOC).
	out.Reset()
	err = root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "--all", "-o", "json"})
	require.NoError(t, err)
	var m2 map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &m2))
	data2 := m2["data"].(map[string]any)
	gf, _ := data2["globalFlags"].([]any)
	assert.NotEmpty(t, gf, "schema --all must carry a non-empty globalFlags array")
}

// TestSchemaReportsConfigSourcedReadOnly is the external-review regression:
// the schema command sits on the Adapt path that never calls resolveDeps, so
// a config-file-sourced readOnly must still reach the manifest — not just
// env/flag sources.
func TestSchemaReportsConfigSourcedReadOnly(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data struct {
			ReadOnly        bool     `json:"readOnly"`
			ReadOnlySources []string `json:"readOnlySources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.True(t, env.Data.ReadOnly)
	assert.Contains(t, env.Data.ReadOnlySources, "config readOnly")
}

// DR-006 repro: an unknown top-level command or an unknown kind under a group
// must surface the standard VALIDATION envelope at exit 2, not urfave's own
// unhinted "No help topic" text at exit 3 (indistinguishable from the AUTH
// exit code).
func TestUnknownCommandIsValidationNotExitThree(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	var gotCode int
	ucli.OsExiter = func(code int) { gotCode = code }
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"-o", "json", "bogus-command-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "bogus-command-xyz"}))
	assert.Equal(t, 2, gotCode)
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bogus-command-xyz")

	gotCode, errOut = 0, bytes.Buffer{}
	root.ErrWriter = &errOut
	CaptureArgv([]string{"-o", "json", "get", "bogus-kind-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "bogus-kind-xyz"}))
	assert.Equal(t, 2, gotCode)
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
}

// TestUnknownCommandRendersTextWithoutOutputFlag pins the pre-pipeline
// formatter's default: with no -o/--output anywhere on the raw command
// line, an unmatched command name renders the short text form, not a JSON
// envelope — the deterministic "scan miss falls back to text" rule.
func TestUnknownCommandRendersTextWithoutOutputFlag(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"bogus-command-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "bogus-command-xyz"}))
	assert.Contains(t, errOut.String(), `error (VALIDATION): unknown command "bogus-command-xyz"`)
	assert.NotContains(t, errOut.String(), "apiVersion")
}

// TestUnknownFlagRendersTextWithoutOutputFlag mirrors the command-not-found
// case for onUsageError: an unknown flag with no -o anywhere on the raw
// command line renders as text.
func TestUnknownFlagRendersTextWithoutOutputFlag(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	CaptureArgv([]string{"get", "pod", "--bogus-flag"})
	runErr := root.Run(t.Context(), []string{"rancher", "get", "pod", "--bogus-flag"})
	require.Error(t, runErr)
	assert.Contains(t, errOut.String(), "error (VALIDATION):")
	assert.Contains(t, errOut.String(), "bogus-flag")
	assert.NotContains(t, errOut.String(), "apiVersion")
}

// TestUnknownCommandRendersYAMLEnvelope pins the -o yaml row: the raw-argv
// scan recognizing "yaml" renders the same structured envelope as json,
// serialized as YAML.
func TestUnknownCommandRendersYAMLEnvelope(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"--output=yaml", "bogus-command-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--output=yaml", "bogus-command-xyz"}))
	assert.NotContains(t, errOut.String(), "error (VALIDATION)")
	var env map[string]any
	require.NoError(t, yaml.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "Error", env["kind"])
}

// exercise bug 2 repro: unknown command "version" is the friction item — the
// hint must point at --version, not the generic `rancher schema` hint.
func TestUnknownVersionCommandHintsAtFlag(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"-o", "json", "version"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "version"}))
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Hint, "--version")

	// A subgroup miss on the same literal name has no --version flag to
	// point at (that flag is root-only) — it must get the generic hint.
	errOut.Reset()
	CaptureArgv([]string{"-o", "json", "get", "version"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "version"}))
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
	assert.NotContains(t, env2.Data.Error.Hint, "--version")
}

// "delete cluster" must hint at provisioning-cluster, not the generic schema
// hint, because management clusters are not deletable via the delete group.
// Only the literal tokens "cluster" / "clusters" under "delete" get the
// targeted hint; other unknown tokens under delete keep the generic hint.
func TestDeleteClusterHintsAtProvisioningCluster(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	// "cluster" under delete → must hint at provisioning-cluster.
	CaptureArgv([]string{"-o", "json", "delete", "cluster", "my-cluster"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "delete", "cluster", "my-cluster"}))
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Hint, "provisioning-cluster")

	// "clusters" under delete → same targeted hint.
	errOut.Reset()
	CaptureArgv([]string{"-o", "json", "delete", "clusters", "my-cluster"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "delete", "clusters", "my-cluster"}))
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
	assert.Contains(t, env2.Data.Error.Hint, "provisioning-cluster")

	// unknown token under delete → generic hint, not the cluster-specific one.
	errOut.Reset()
	CaptureArgv([]string{"-o", "json", "delete", "bogus-kind-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "delete", "bogus-kind-xyz"}))
	var env3 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env3))
	assert.Equal(t, "VALIDATION", env3.Data.Error.Code)
	assert.NotContains(t, env3.Data.Error.Hint, "provisioning-cluster")
}

// exercise bug 2 repro: an unknown flag on a real command must surface the
// standard VALIDATION envelope at exit 2, not urfave's own unhinted
// "Incorrect Usage" text.
func TestUnknownFlagIsValidationEnvelope(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	CaptureArgv([]string{"-o", "json", "get", "pod", "--bogus-flag"})
	runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "pod", "--bogus-flag"})
	require.Error(t, runErr)

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bogus-flag")
}

// TestAllNamespacesHintTargeted pins the -A/--all-namespaces friction point:
// both flags are unknown on every get command (list operations already span
// all namespaces), so the hint must say so directly instead of pointing at
// rancher schema. A genuinely unknown flag must still get the generic hint.
// Mirrors TestDeleteClusterHintsAtProvisioningCluster one seam over.
func TestAllNamespacesHintTargeted(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	t.Run("-A shorthand", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "get", "pod", "-A"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "pod", "-A"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		t.Logf("urfave -A error message: %q", env.Data.Error.Message)
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.Equal(t, "there is no all-namespaces flag — list operations already span all namespaces; use --namespace where the kind accepts one", env.Data.Error.Hint)
	})

	t.Run("--all-namespaces long form", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "get", "pod", "--all-namespaces"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "pod", "--all-namespaces"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		t.Logf("urfave --all-namespaces error message: %q", env.Data.Error.Message)
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.Equal(t, "there is no all-namespaces flag — list operations already span all namespaces; use --namespace where the kind accepts one", env.Data.Error.Hint)
	})

	t.Run("genuinely unknown flag gets generic hint", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "get", "pod", "--bogus-flag"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "pod", "--bogus-flag"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.NotContains(t, env.Data.Error.Hint, "all-namespaces",
			"a genuinely unknown flag must not receive the all-namespaces hint")
	})

	t.Run("inspect user --cluster fails as an unknown flag", func(t *testing.T) {
		// inspect user deliberately does NOT declare --cluster (the user
		// kind is management-scoped, not cluster-scoped) — declaring it
		// just to reject it would put a dead flag in the schema and
		// completion. Passing it must fail the same generic way any
		// undeclared flag does.
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "inspect", "user", "u-abc", "--cluster", "c-x"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "inspect", "user", "u-abc", "--cluster", "c-x"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	})

	t.Run("create namespace -A gets verb-agnostic all-namespaces hint", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "create", "namespace", "foo", "-A"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "create", "namespace", "foo", "-A"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.Equal(t, "there is no all-namespaces flag; run: rancher schema for this command's real flags", env.Data.Error.Hint,
			"create namespace -A must get the verb-agnostic hint, not the get-specific or generic hint")
	})

	t.Run("create namespace with genuinely unknown flag gets generic hint", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "create", "namespace", "foo", "--bogus-flag"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "create", "namespace", "foo", "--bogus-flag"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.Equal(t, "run: rancher schema; pass --help for usage", env.Data.Error.Hint,
			"a genuinely unknown flag on a non-get verb must get the generic hint")
	})

	t.Run("bare get group -A pins path-length-2 boundary", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv([]string{"-o", "json", "get", "-A"})
		runErr := root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "-A"})
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
		assert.Equal(t, "there is no all-namespaces flag — list operations already span all namespaces; use --namespace where the kind accepts one", env.Data.Error.Hint,
			"bare get group -A (path length 2) must yield the get-scoped wording, pinning the >= 2 boundary")
	})
}

// TestAllNamespacesHintsBothShareOpeningClause pins F5: both rendered
// wordings of the -A/--all-namespaces hint open with noAllNamespacesFlag.
// Calling allNamespacesEntry.hint directly for a get-context command and a
// non-get-context command verifies the shared clause is not accidentally
// dropped if the branching logic changes.
func TestAllNamespacesHintsBothShareOpeningClause(t *testing.T) {
	t.Parallel()
	root := &ucli.Command{Name: "rancher"}
	get := &ucli.Command{Name: "get"}
	create := &ucli.Command{Name: "create"}
	root.Commands = []*ucli.Command{get, create}
	root.Writer = &bytes.Buffer{}
	_ = root.Run(t.Context(), []string{"rancher"}) // wires parent pointers via setupDefaults

	err := errors.New("flag provided but not defined: -A")
	getHint := allNamespacesEntry.hint(usageErrorInput{c: get, err: err})
	assert.True(t, strings.HasPrefix(getHint, noAllNamespacesFlag),
		"get-scoped -A hint must open with the noAllNamespacesFlag clause")
	nonGetHint := allNamespacesEntry.hint(usageErrorInput{c: create, err: err})
	assert.True(t, strings.HasPrefix(nonGetHint, noAllNamespacesFlag),
		"verb-agnostic -A hint must open with the noAllNamespacesFlag clause")
}

// An explicitly overridden config dir must not put a plain-text notice ahead
// of the error envelope — stderr must parse as pure JSON, exactly as a
// scripted session relies on.
func TestExplicitConfigDirKeepsStderrPureJSON(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv("RANCHER_CLI_URL", "")
	t.Setenv("RANCHER_CLI_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("RANCHER_CLI_CONFIG_DIR", dir)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	// The default handler calls os.Exit on a non-zero ExitCoder, which would kill
	// the test process; keep the error as a return value instead.
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(t.Context(), []string{"rancher", "get", "cluster", "foo", "-o", "json"})
	require.Error(t, runErr)

	stderr := errOut.Bytes()
	require.NotEmpty(t, stderr)
	var env map[string]any
	require.NoError(t, json.Unmarshal(stderr, &env), "stderr must be pure JSON, got %q", stderr)
	data := env["data"].(map[string]any)
	errObj := data["error"].(map[string]any)
	assert.Equal(t, "NOT_LOGGED_IN", errObj["code"])
}

// TestCorruptConfigDoesNotBreakSchemaOrCompletionCallback pins the SC-1 hard
// requirement: resolveReadOnly already degrades to not-read-only on a config
// load error (adapter.go:129-143) rather than failing the command, on BOTH a
// before-only path (schema, Adapt-wired, no resolveDeps) and the completion
// callback (Before runs, then completion returns before Action). This must
// hold with no code change to resolveReadOnly/resolveDeps — the test only
// pins existing behavior against regression.
func TestCorruptConfigDoesNotBreakSchemaOrCompletionCallback(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("not: [valid yaml\n"), 0o600))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))
	var env struct {
		Data struct {
			ReadOnly bool `json:"readOnly"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.False(t, env.Data.ReadOnly, "a corrupt config must degrade to not-read-only, not fail schema")

	out.Reset()
	require.NoError(t, root.Run(t.Context(), []string{
		"rancher", "--config", dir, "get", "--generate-shell-completion",
	}))
	assert.NotEmpty(t, out.String(), "the completion callback must still emit candidates on a corrupt config, not fail")
}

// TestGenerateShellCompletionListsTopLevelCommandNames pins that
// EnableShellCompletion activates default command-name completion for the
// existing tree with no assembleCommands/wireEntry changes. Membership only —
// urfave-owned ordering/format is not ours to golden-test.
func TestGenerateShellCompletionListsTopLevelCommandNames(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	// urfave's DefaultCompleteWithFlags reads the real os.Args (not the argv
	// passed to Run) when invoked at the root command — it always assumes the
	// shell appended the completion flag last on the actual process args. Mirror
	// that so the test drives the same code path a real TAB keystroke does.
	argv := []string{"rancher", "--config", dir, "--generate-shell-completion"}
	origArgs := os.Args
	os.Args = argv
	t.Cleanup(func() { os.Args = origArgs })

	require.NoError(t, root.Run(t.Context(), argv))
	assert.Contains(t, out.String(), "get")
	assert.Contains(t, out.String(), "auth")
}

// TestHiddenCompletionCommandEmitsNoScriptMarkers pins that the renamed
// built-in (__completion) remains invocable as machinery only: its
// subcommands are stripped, so `rancher __completion bash` cannot emit
// urfave's own divergent script. This pins the INTENT, not urfave's own
// help-topic mechanism: post-strip, "bash" is an unmatched subcommand name on
// __completion, which now routes through the repo's standard CommandNotFound
// seam — a structured JSON VALIDATION envelope on stderr at a non-zero exit,
// the same envelope-everywhere contract every other unmatched command name
// gets (see TestUnknownCommandIsValidationNotExitThree), not urfave's own
// unhinted "No help topic" text and not a custom help Action (the command is
// machinery, not surface, per STOP adjudication 2026-07-12).
func TestHiddenCompletionCommandEmitsNoScriptMarkers(t *testing.T) {
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	origExiter := ucli.OsExiter
	var gotCode int
	ucli.OsExiter = func(code int) { gotCode = code }
	defer func() { ucli.OsExiter = origExiter }()

	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"--config", dir, "-o", "json", "__completion", "bash"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "-o", "json", "__completion", "bash"}))
	assert.Equal(t, 2, gotCode, "an unmatched subcommand name is VALIDATION, exit 2, not urfave's own exit 3")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bash")

	combined := out.String() + errOut.String()
	assert.NotContains(t, combined, "--generate-shell-completion",
		"the hidden built-in must never emit urfave's own completion script")
}

// TestPublicContractOpsArePinned closes the op-level half of the public-API
// conformance guarantee (the kind-table half lives in
// internal/engine/kinds/publicapi_test.go): every operation the composed
// resolver marks ContractPublic must either belong to a kind whose row is
// itself ContractPublic (generated verbs and the derived create ops inherit
// the row's binding) or appear in the pinned bespoke list below. Adding a
// new public op compiles fine — this test is what turns the widened public
// surface into a reviewed diff instead of silent drift.
func TestPublicContractOpsArePinned(t *testing.T) {
	t.Parallel()
	publicKindByName := map[string]bool{}
	for _, k := range kinds.All() {
		if k.Binding.Contract == safety.ContractPublic {
			publicKindByName[k.Name] = true
		}
	}
	resolver, err := composeBindings()
	require.NoError(t, err)
	// Both directions: every pinned bespoke op must actually resolve public…
	for op := range bespokePublicOps {
		b, ok := resolver.Resolve(op)
		require.True(t, ok, op)
		assert.Equal(t, safety.ContractPublic, b.Contract,
			"op %q is in bespokePublicOps but does not resolve ContractPublic", op)
	}
	// …and every public op must back a public kind row or be pinned.
	for op, b := range resolver.All() {
		if b.Contract != safety.ContractPublic || bespokePublicOps[op] {
			continue
		}
		if key, ok := derivedOpKinds[op]; ok {
			row, found := kinds.Lookup(key)
			require.True(t, found, key)
			assert.Equal(t, safety.ContractPublic, row.Binding.Contract,
				"derived op %q is ContractPublic but its %q row is not", op, key)
			continue
		}
		// Generated verbs are "<verb> <kind-name>"; the kind name is the
		// last space-separated token.
		parts := strings.Split(op, " ")
		kindName := parts[len(parts)-1]
		assert.True(t, publicKindByName[kindName],
			"op %q is ContractPublic but neither backs a public kind row nor is in bespokePublicOps — widening the public surface must be a reviewed edit there", op)
	}
}

// TestCreateOpBindingsTrackKindRows pins the derivation rule: every op in
// derivedOpKinds must resolve to its kind row's Binding exactly, because
// composeBindings derives them via the row rather than a hand-copied
// literal in provider.NewResolver — the same drift-closure as
// TestDeleteProvisioningClusterBindingTracksKindRow. Ranging over the SAME
// map production uses means a new derived op is covered the moment it is
// added.
func TestCreateOpBindingsTrackKindRows(t *testing.T) {
	t.Parallel()
	resolver, err := composeBindings()
	require.NoError(t, err)
	require.NotEmpty(t, derivedOpKinds)
	// Absence from provider.NewResolver needs no assertion: a literal
	// reappearing there collides in ComposeResolvers and fails the
	// composeBindings call above.
	for op, key := range derivedOpKinds {
		row, ok := kinds.Lookup(key)
		require.True(t, ok, key)
		b, ok := resolver.Resolve(op)
		require.True(t, ok, op)
		assert.Equal(t, row.Binding, b, "op %q must track the %q row's Binding", op, key)
	}
}
