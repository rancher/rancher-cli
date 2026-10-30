package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac/complete"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	tokencomplete "github.com/rancher/rancher-cli/internal/token/complete"
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

// TestLegacyTokenShimBindingIsInternalDeprecated asserts the shim's position in
// the public-API contract: the top-level "token" command is internal
// (auth machinery, no backing kind row) and deprecated by design (it exists
// only for Rancher-generated kubeconfigs during migration).
func TestLegacyTokenShimBindingIsInternalDeprecated(t *testing.T) {
	contract, stability, found := schemaCommandBinding(t, "token")
	require.True(t, found, "the token shim must be registered")
	assert.Equal(t, "internal", contract)
	assert.Equal(t, "deprecated", stability)
}

// TestDeleteProvisioningClusterBindingTracksKindRow asserts that
// "delete provisioning-cluster" has no generic binding (kinds.Bindings()
// skips it - the row is Deletable: false), so BuildRoot must compose an
// EXTRA resolver entry for it derived from the provisioningcluster row's own
// Binding, read via kinds.Lookup at composition time - never a hand-copied
// literal, which has no machine-checked link back to the row and can
// silently drift from a future edit to the row's Binding in kinds.go. This
// test asserts the schema-reported contract/stability for the op equals
// kinds.Lookup("provisioningcluster").Binding exactly. A hand-copied literal
// would silently keep a stale value after an edit to the row's Binding in
// kinds.go; deriving it from the row closes that drift.
func TestDeleteProvisioningClusterBindingTracksKindRow(t *testing.T) {
	pcRow, ok := kinds.Lookup("provisioningcluster")
	require.True(t, ok)

	contract, stability, found := schemaCommandBinding(t, "delete provisioning-cluster")
	require.True(t, found, "delete provisioning-cluster must be wired despite Deletable: false")
	assert.Equal(t, string(pcRow.Binding.Contract), contract)
	assert.Equal(t, string(pcRow.Binding.Stability), stability)
}

// TestSchemaVerbEnumMatchesCommandVerbs asserts the --verb enum in schema's
// descriptor to the surface it filters: the enum is a static literal in
// schemaCmd.Describe(), so a new top-level verb (a new distinct first token of
// a command name) must fail here until the literal is updated. Runs against
// the full real registry - the only place every command is wired.
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

// TestSchemaContractEnumMatchesCommandContracts asserts the --contract enum to the
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

// TestSchemaDisclosureSizes asserts the point of progressive disclosure:
// the bare TOC and a narrowed card stay small (TOC
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

// TestSchemaTOCMarksEveryPrimeCommand cross-checks the full manifest against
// the bare TOC: every bespoke command carrying prime:true in schema --all
// must also carry prime:true in the TOC's groups[].commands[] entries.
// Generated commands are skipped - the bare TOC collapses them into a
// per-group summary and never lists them individually.
func TestSchemaTOCMarksEveryPrimeCommand(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("golden")
	require.NoError(t, err)

	runJSON := func(args ...string) map[string]any {
		var out bytes.Buffer
		root.Writer = &out
		require.NoError(t, root.Run(t.Context(), append([]string{"rancher", "--config", dir}, args...)))
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &env))
		var v map[string]any
		require.NoError(t, json.Unmarshal(env.Data, &v))
		return v
	}

	full := runJSON("schema", "--all", "-o", "json")
	bare := runJSON("schema", "-o", "json")

	tocPrime := map[string]bool{}
	for _, g := range bare["groups"].([]any) {
		cmds, _ := g.(map[string]any)["commands"].([]any)
		for _, c := range cmds {
			m := c.(map[string]any)
			prime, _ := m["prime"].(bool)
			tocPrime[m["name"].(string)] = prime
		}
	}

	for _, c := range full["commands"].([]any) {
		m := c.(map[string]any)
		prime, _ := m["prime"].(bool)
		if !prime {
			continue
		}
		name := m["name"].(string)
		got, ok := tocPrime[name]
		if !ok {
			continue // generated command: collapsed on the bare TOC, never listed by name
		}
		assert.True(t, got, "command %q is prime:true in the manifest but not TOC-marked", name)
	}

	// The group JSON card is a third projection of the same per-command
	// prime flag - hold it to the identical rule so a prime command can
	// never read prime-less on its own group's card.
	for _, groupName := range []string{"cluster", "oidc-client"} {
		card := runJSON("schema", groupName, "-o", "json")
		for _, c := range card["commands"].([]any) {
			m := c.(map[string]any)
			name := m["name"].(string)
			prime, _ := m["prime"].(bool)
			var want bool
			for _, fc := range full["commands"].([]any) {
				fm := fc.(map[string]any)
				if fm["name"] == name {
					want, _ = fm["prime"].(bool)
					break
				}
			}
			assert.Equal(t, want, prime, "group card %q: command %q prime flag disagrees with the manifest", groupName, name)
		}
	}
}

// TestBuildRootFlagsPrimeCommandsInMetadata asserts every adapted ucli.Command
// carries the Prime metadata flag exactly when the manifest says its
// descriptor is Prime, and that a group carries the flag exactly when every
// leaf underneath it does - the manifest is the source of truth, so this
// walks it rather than hardcoding a full command list.
func TestBuildRootFlagsPrimeCommandsInMetadata(t *testing.T) {
	wr, err := buildWiredRegistry("golden", config.ReadOnlyOverride)
	require.NoError(t, err)
	manifest := wr.reg.Schema("golden", config.MinRancherVersion)
	primeByName := map[string]bool{}
	for _, c := range manifest.Commands {
		primeByName[c.Name] = c.Prime
	}

	// BuildRoot is called fresh, rather than reused from wr, so the tree
	// walked below is the one BuildRoot returns, never one a Run() has set
	// up: urfave's setup adds hidden nodes and flags to the commands it
	// visits.
	root, err := BuildRoot("golden")
	require.NoError(t, err)

	byName := map[string]*ucli.Command{}
	var checked int
	for _, top := range root.Commands {
		byName[top.Name] = top
		if len(top.Commands) == 0 {
			if want, ok := primeByName[top.Name]; ok {
				assert.Equal(t, want, commandIsPrime(top), "command %q metadata disagrees with manifest", top.Name)
				checked++
			}
			continue
		}
		allPrime := true
		for _, leaf := range top.Commands {
			fullName := top.Name + " " + leaf.Name
			want, ok := primeByName[fullName]
			require.True(t, ok, "manifest missing command %q", fullName)
			assert.Equal(t, want, commandIsPrime(leaf), "command %q metadata disagrees with manifest", fullName)
			checked++
			allPrime = allPrime && want
		}
		assert.Equal(t, allPrime, commandIsPrime(top), "group %q metadata disagrees with its leaves", top.Name)
	}
	require.NotZero(t, checked)

	require.Contains(t, byName, "oidc-client")
	assert.True(t, commandIsPrime(byName["oidc-client"]), "oidc-client is an all-Prime group")
	require.Contains(t, byName, "cluster")
	assert.False(t, commandIsPrime(byName["cluster"]), "cluster mixes Prime and non-Prime verbs")
}

// TestPrimeMarkerConformance is the manifest-wide conformance check for the
// "[Prime]" marker: every command's help row carries it iff the manifest
// says the command is Prime, and every named group's root help row and
// schema TOC label carry it iff every member of that group is Prime. It
// walks the real manifest rather than a curated list, so a future command
// or group is covered automatically.
func TestPrimeMarkerConformance(t *testing.T) {
	wr, err := buildWiredRegistry("golden", config.ReadOnlyOverride)
	require.NoError(t, err)
	manifest := wr.reg.Schema("golden", config.MinRancherVersion)
	primeByName := map[string]bool{}
	for _, c := range manifest.Commands {
		primeByName[c.Name] = c.Prime
	}
	// The rule is restated here on purpose rather than
	// imported from runtime: a conformance test that asked the code under
	// test what to expect would only check that the code agrees with itself.
	groupAllPrime := func(g runtime.SchemaGroup) bool {
		if len(g.Commands) == 0 {
			return false
		}
		for _, name := range g.Commands {
			if !primeByName[name] {
				return false
			}
		}
		return true
	}

	rootHelp := renderHelp(t, "")
	schemaText := runSchemaText(t)

	var checkedCommands, checkedGroups int
	for _, g := range manifest.Groups {
		if g.Name == "" {
			continue
		}
		checkedGroups++
		wantGroupPrime := groupAllPrime(g)

		rootLine := lineForCommandName(t, rootHelp, g.Name)
		if wantGroupPrime {
			assert.Contains(t, rootLine, "[Prime]", "all-Prime group %q must carry the marker on root help", g.Name)
		} else {
			assert.NotContains(t, rootLine, "[Prime]", "group %q must carry no marker on root help", g.Name)
		}

		tocLine := lineForCommandName(t, schemaText, g.Name)
		if wantGroupPrime {
			assert.Contains(t, tocLine, "[Prime]", "all-Prime group %q must carry the marker on the schema TOC label", g.Name)
		} else {
			assert.NotContains(t, tocLine, "[Prime]", "group %q must carry no marker on the schema TOC label", g.Name)
		}

		groupHelp := renderHelp(t, "", g.Name)
		for _, name := range g.Commands {
			leaf, _, _ := strings.Cut(strings.TrimPrefix(name, g.Name+" "), " ")
			line := lineForCommandName(t, groupHelp, leaf)
			checkedCommands++
			if primeByName[name] {
				assert.Contains(t, line, "[Prime]", "command %q must carry the marker on its group's help", name)
			} else {
				assert.NotContains(t, line, "[Prime]", "command %q must carry no marker on its group's help", name)
			}
		}
	}
	require.NotZero(t, checkedGroups)
	require.NotZero(t, checkedCommands)
}

// lineForCommandName finds a rendered COMMANDS row by its canonical name -
// the row's comma-joined name list may carry aliases after it (e.g.
// "deployment, deployments, deploy:"), so a plain substring match on
// "name:" alone would miss it.
func lineForCommandName(t *testing.T, text, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, name+":") || strings.HasPrefix(trimmed, name+",") {
			return line
		}
	}
	t.Fatalf("no COMMANDS row for %q in:\n%s", name, text)
	return ""
}

// runSchemaText runs `rancher schema` and returns its rendered text.
func runSchemaText(t *testing.T) string {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "schema"}))
	return out.String()
}

// TestNoUsageCarriesPrimeMarker asserts no command's Usage text contains the
// marker literal: the renderers prepend it, and the colorizers color the
// first occurrence on a row, so a Usage that carried its own copy would
// render two tokens and color the wrong one. The manifest is the source.
func TestNoUsageCarriesPrimeMarker(t *testing.T) {
	wr, err := buildWiredRegistry("golden", config.ReadOnlyOverride)
	require.NoError(t, err)
	for _, c := range wr.reg.Schema("golden", config.MinRancherVersion).Commands {
		assert.NotContains(t, c.Usage, runtime.PrimeMarker, "%s: Usage must not carry the marker literal", c.Name)
	}
}

// TestGroupPrimeMetadataEmptyGroupIsNotPrime asserts the group rollup's
// rule matches groupAllPrime's: a group with no leaves is never Prime, even
// though "every leaf is Prime" is vacuously true of an empty slice.
func TestGroupPrimeMetadataEmptyGroupIsNotPrime(t *testing.T) {
	t.Parallel()
	assert.Nil(t, groupPrimeMetadata(nil))
	assert.Nil(t, groupPrimeMetadata([]*ucli.Command{}))
}

// TestNoLowercasePrimeMarkerLiteral asserts the retired "[prime]" spelling
// appears nowhere in the repository's Go, Markdown, or txtar files - the
// capitalized "[Prime]" is the one spelling. It walks the whole tree, not
// just this package: the retired spelling lived in internal/runtime, and the
// docs and skill are where a reader would copy it from.
func TestNoLowercasePrimeMarkerLiteral(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "[prime]", "banned lowercase marker in %s", path)
	}
	root := filepath.Join("..", "..")
	skip := map[string]bool{".git": true, "bin": true, "docs": true, "vendor": true, "graphify-out": true, "node_modules": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".txtar":
		default:
			return nil
		}
		if strings.HasSuffix(path, filepath.Join("internal", "cli", "root_test.go")) {
			return nil
		}
		check(t, path)
		return nil
	})
	require.NoError(t, err)
}

// TestSchemaTOCGolden asserts the bare `schema -o json` TOC shape - the
// progressive-disclosure contract: orientation payload,
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

// TestKindOverridesReplaceExactlyTheirVerb asserts the override mechanism: an
// overridden verb+kind resolves to the override's command, every other verb
// of that kind and every other kind stays generic, and no command name is
// registered twice.
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
// on) while Usage carries a marker string so
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
// byte-identical. NOT t.Parallel - it mutates the package-level
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

// TestSchemaManifestListsProjectWhereCommandsTakeIt asserts that a command
// takes --project exactly when its manifest entry lists a project flag: a
// generated get of a project scopable kind (pod) lists it as optional,
// project add-namespace as required, and a fixed command (schema), project
// remove-namespace and create secret's removed --project-scoped do not
// appear.
func TestSchemaManifestListsProjectWhereCommandsTakeIt(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)

	flags := map[string]map[string]runtime.ArgSpec{}
	for _, c := range m.Commands {
		flags[c.Name] = map[string]runtime.ArgSpec{}
		for _, f := range c.Flags {
			flags[c.Name][f.Name] = f
		}
	}
	project := func(cmd string) (runtime.ArgSpec, bool) {
		t.Helper()
		fs, ok := flags[cmd]
		require.True(t, ok, "%s must be a registered command", cmd)
		f, ok := fs[runtime.FlagProject]
		return f, ok
	}

	f, ok := project("get pod")
	require.True(t, ok, "pod is NSPlain+Downstream, so get pod must list the project flag")
	assert.Equal(t, "string", f.Type)
	assert.False(t, f.Required)
	f, ok = project("project add-namespace")
	require.True(t, ok)
	assert.True(t, f.Required, "add-namespace needs its target project")
	for _, cmd := range []string{"schema", "project remove-namespace", "get node"} {
		_, ok := project(cmd)
		assert.False(t, ok, "%s must not list the project flag", cmd)
	}
	assert.NotContains(t, flags["create secret"], "project-scoped")
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

// TestAdaptEntrySetsArgsUsageFromDescriptorArgs asserts that adaptEntry (every
// generated get/patch/delete leaf) sets ArgsUsage
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
	require.ErrorContains(t, werr, `internal error: command name "get pod extra" must have one or two space-separated parts, has 3`)
}

// TestWireEntryRefusesWithInternalError asserts the whole text of the
// refusals wireEntry and lookupRow raise for a defect in the command tables.
func TestWireEntryRefusesWithInternalError(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(runtime.MapResolver(provider.NewResolver()), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(resolver)

	_, _, werr := wireEntry(reg, resolver, "", entry{group: "get"})
	require.EqualError(t, werr, `internal error: the entry in group "get" has no function that creates its command`)

	mismatched := entry{group: "get", make: func(runDeps) runtime.Command {
		return fakeEngineCmd{name: "create thing"}
	}}
	_, _, werr = wireEntry(reg, resolver, "", mismatched)
	require.EqualError(t, werr, `internal error: command "create thing" is listed under group "get" but its name belongs to group "create"`)

	_, lerr := lookupRow("no-such-kind")
	require.EqualError(t, lerr, `internal error: kind "no-such-kind" not found`)
}

// TestValidateWiringLeadsWithInternalError asserts that each failure of the
// checks of a built command tree reaches the caller with its lead: a registry
// with an invalid command, a live completion table with a flag that is not
// shared, and a command whose completion source needs no credential but has no
// completion target.
func TestValidateWiringLeadsWithInternalError(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(runtime.MapResolver(provider.NewResolver()), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	leaves := map[string]runtime.Descriptor{"x login": {Name: "x login", Flags: []runtime.ArgSpec{{Name: "auth-provider", Type: "string"}}}}
	noCredential := map[string]completion.Source{"x login": tokencomplete.AuthConfigSource()}

	invalid := runtime.NewRegistry(resolver)
	invalid.Register(fakeEngineCmd{name: "get a"})
	err = validateWiring(invalid, nil, nil, nil, nil, nil, nil)
	require.ErrorContains(t, err, `internal error: command definitions: command "get a"`)

	empty := runtime.NewRegistry(resolver)
	err = validateWiring(empty, leaves, nil, nil, map[string]completion.Source{"role": complete.RoleSource()}, nil, nil)
	require.EqualError(t, err, "internal error: live completion setup: --role has a live completion source but is not a shared completion flag")

	err = validateWiring(empty, leaves, noCredential, map[string]bool{}, nil, nil, nil)
	require.EqualError(t, err, "internal error: live completion setup: command x login has a completion source that needs no credential, but the command has no completion target (the server to connect to)")

	require.NoError(t, validateWiring(empty, leaves, noCredential, map[string]bool{"x login": true}, nil, nil, nil))
}

// TestBuildWiredRegistryFailsThroughValidateWiring asserts that the build of
// the command tree returns the error of the wiring checks as it is: a flag
// source for a flag that is not shared fails the build with the lead. Not
// parallel: it replaces the package table of flag sources for the run.
func TestBuildWiredRegistryFailsThroughValidateWiring(t *testing.T) {
	orig := flagSources
	t.Cleanup(func() { flagSources = orig })
	flagSources = map[string]completion.Source{"role": complete.RoleSource()}

	_, err := buildWiredRegistry("v-test", nil)
	require.EqualError(t, err, "internal error: live completion setup: --role has a live completion source but is not a shared completion flag")
}

// TestBuildRootWiresProvisioningClusterDelete asserts, for real, that
// provisioningcluster's row stays Deletable: false, so kinds.Bindings() never
// emits "delete provisioning-cluster" - the ONLY reason BuildRoot succeeds with
// the del override registered is the extra resolver entry BuildRoot composes
// for that exact op string, derived from the row's own Binding (see
// TestDeleteProvisioningClusterBindingTracksKindRow for the derivation check).
// Without that entry, BuildRoot fails with wireEntry's "has no contract and
// stability set" error; this test guards against that.
func TestBuildRootWiresProvisioningClusterDelete(t *testing.T) {
	// Not t.Parallel: schemaCommandNames calls t.Setenv, which panics when
	// combined with t.Parallel (same constraint TestBuildRootWiresGeneratedCommands
	// and TestSchemaManifestListsProjectWhereCommandsTakeIt already observe).
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

// TestDeleteOverrideResolverGeneralizesBeyondProvisioningCluster asserts that
// BuildRoot's extra delete-binding resolver is derived by
// looping kindOverrideTable for every del override on a Deletable: false
// row, not by naming "provisioningcluster" and its op literally. "node"
// (Deletable: false, no del override in production) stands in for a
// hypothetical second such kind: installing a del override for it here
// must, with zero BuildRoot code changes, wire "delete node" and resolve it
// to node's own row Binding - proving the loop's generality rather than a
// coincidence that only happens to work for the one production entry. NOT
// t.Parallel - it mutates the package-level kindOverrideTable (same
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

	// provisioningcluster's own del override must still resolve too - the
	// loop composes every matching row, not just the last one iterated.
	pcDelete := findCommand(delGroup.Commands, "provisioning-cluster")
	require.NotNil(t, pcDelete, "delete provisioning-cluster must still be wired alongside the new override")
}

// TestDeleteOverrideResolverSkipsDeletableRowToAvoidCollision asserts the
// delOverrides loop's "if row.Deletable { continue }" branch: composing an
// extra "delete <name>" entry for a Deletable: true row would collide with
// kinds.Bindings()'s own entry for that op. Every del override in production,
// and TestDeleteOverrideResolverGeneralizesBeyondProvisioningCluster's own
// "node" stand-in, sits on a Deletable: false row, so this test covers the
// other case. "pod" (Deletable: true, no override in production) stands in
// for a hypothetical del override installed on an already-deletable kind:
// installing one here and still getting a successful BuildRoot demonstrates the
// branch prevents the ComposeResolvers duplicate-binding error; without the
// branch, BuildRoot fails with the "operation \"delete pod\" resolved by
// both resolvers" error. NOT t.Parallel - it mutates the package-level
// kindOverrideTable (same constraint as the sibling tests in this file).
func TestDeleteOverrideResolverSkipsDeletableRowToAvoidCollision(t *testing.T) {
	orig := kindOverrideTable
	podKind, ok := kinds.Lookup("pod")
	require.True(t, ok)
	require.True(t, podKind.Deletable, "pod must stay deletable for this test to exercise the collision-avoidance branch")
	_, overridden := orig["pod"]
	require.False(t, overridden, "pod must have no del override in production for this stand-in")

	overrides := make(map[string]kindOverride, len(orig)+1)
	maps.Copy(overrides, orig)
	overrides["pod"] = kindOverride{del: func(k kinds.Kind, d runDeps) runtime.Command { return markerDelCmd{k} }}
	kindOverrideTable = overrides
	t.Cleanup(func() { kindOverrideTable = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err, "a del override on a Deletable: true row must not collide with kinds.Bindings()'s own entry for that op")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	podDelete := findCommand(delGroup.Commands, "pod")
	require.NotNil(t, podDelete, "delete pod must still be wired")
	assert.Equal(t, "MARKER-DEL", podDelete.Usage, "the override's del constructor must still be used for the generated command")
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
	require.ErrorContains(t, werr, `internal error: command "get gizmo" has no contract and stability set`)
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

// TestSchemaReportsConfigSourcedReadOnly asserts that a config-file-sourced
// readOnly reaches the manifest, not just env/flag sources: the schema
// command sits on the Adapt path that never calls resolveDeps.
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
	assert.Contains(t, env.Data.ReadOnlySources, "config setting readOnly")
}

// An unknown top-level command or an unknown kind under a group
// must surface the standard VALIDATION envelope at exit 2, not urfave's own
// unhinted "No help topic" text at exit 3 (indistinguishable from the AUTH
// exit code).
func TestUnknownCommandIsValidationNotExitThree(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"-o", "json", "bogus-command-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "bogus-command-xyz"}))
	assert.Equal(t, 2, exitCodeOf(root, nil))
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bogus-command-xyz")

	errOut = bytes.Buffer{}
	root.ErrWriter = &errOut
	CaptureArgv([]string{"-o", "json", "get", "bogus-kind-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "get", "bogus-kind-xyz"}))
	assert.Equal(t, 2, exitCodeOf(root, nil))
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
}

// TestUnknownCommandRendersTextWithoutOutputFlag asserts the pre-pipeline
// formatter's default: with no -o/--output anywhere on the raw command
// line, an unmatched command name renders the short text form, not a JSON
// envelope - the deterministic "scan miss falls back to text" rule.
func TestUnknownCommandRendersTextWithoutOutputFlag(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

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

// TestCommandNotFoundStyledWithColorAlways asserts that the errors the adapter
// renders itself - raised before any command dispatches, so never passing
// through the pipeline's own style resolution - still honor --color: the
// unknown-command error's prefix is ANSI-styled under --color=always (read
// from raw argv, since flags never parsed) and byte-plain by default on a
// non-terminal stderr.
func TestCommandNotFoundStyledWithColorAlways(t *testing.T) {
	orig := rawArgv
	t.Cleanup(func() { rawArgv = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"--color", "always", "bogus-command-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--color", "always", "bogus-command-xyz"}))
	assert.Contains(t, errOut.String(), "\x1b[", "prefix styled under --color=always")
	assert.Contains(t, errOut.String(), `unknown command "bogus-command-xyz"`)

	errOut.Reset()
	root2, err := BuildRoot("v-test")
	require.NoError(t, err)
	root2.Writer = &out
	root2.ErrWriter = &errOut
	CaptureArgv([]string{"--color", "always", "-o", "json", "bogus-command-xyz"})
	require.NoError(t, root2.Run(t.Context(), []string{"rancher", "--color", "always", "-o", "json", "bogus-command-xyz"}))
	assert.NotContains(t, errOut.String(), "\x1b[", "json envelope never styled, whatever --color says")
	assert.Contains(t, errOut.String(), "apiVersion")
}

// TestUsageErrorStyledWithColorAlways asserts the same --color honoring on the
// flag-parse-failure path, the other pre-parse renderer.
func TestUsageErrorStyledWithColorAlways(t *testing.T) {
	orig := rawArgv
	t.Cleanup(func() { rawArgv = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"--color=always", "schema", "--bogus-flag-xyz"})
	_ = root.Run(t.Context(), []string{"rancher", "--color=always", "schema", "--bogus-flag-xyz"})
	assert.Contains(t, errOut.String(), "\x1b[", "prefix styled under --color=always")
	assert.Contains(t, errOut.String(), "error (VALIDATION")
}

// TestUnknownFlagRendersTextWithoutOutputFlag mirrors the command-not-found
// case for onUsageError: an unknown flag with no -o anywhere on the raw
// command line renders as text.
func TestUnknownFlagRendersTextWithoutOutputFlag(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
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

// TestUnknownCommandRendersYAMLEnvelope asserts the -o yaml row: the raw-argv
// scan recognizing "yaml" renders the same structured envelope as json,
// serialized as YAML.
func TestUnknownCommandRendersYAMLEnvelope(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

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

// An unknown command "version" is a common mistake: the hint must point at
// --version, not the generic `rancher schema` hint.
func TestUnknownVersionCommandHintsAtFlag(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

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
	// point at (that flag is root-only) - it must get the generic hint.
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
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

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
	assert.Equal(t, "unknown command \"cluster\"; to delete a cluster, use rancher delete provisioning-cluster <name>; management clusters cannot be deleted directly", env.Data.Error.Message)
	assert.Equal(t, "rancher schema", env.Data.Error.Hint,
		"no single concrete command names both provisioning-cluster and the missing name - the hint stays generic")

	// "clusters" under delete → same targeted detail.
	errOut.Reset()
	CaptureArgv([]string{"-o", "json", "delete", "clusters", "my-cluster"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "delete", "clusters", "my-cluster"}))
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
	assert.Equal(t, "unknown command \"clusters\"; to delete a cluster, use rancher delete provisioning-cluster <name>; management clusters cannot be deleted directly", env2.Data.Error.Message)

	// unknown token under delete → generic message, not the cluster-specific one.
	errOut.Reset()
	CaptureArgv([]string{"-o", "json", "delete", "bogus-kind-xyz"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "-o", "json", "delete", "bogus-kind-xyz"}))
	var env3 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env3))
	assert.Equal(t, "VALIDATION", env3.Data.Error.Code)
	assert.NotContains(t, env3.Data.Error.Message, "provisioning-cluster")
}

// An unknown flag on a real command must surface the standard VALIDATION
// envelope at exit 2, not urfave's own unhinted "Incorrect Usage" text.
func TestUnknownFlagIsValidationEnvelope(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
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

// TestAllNamespacesHintTargeted asserts the -A/--all-namespaces friction point:
// both flags are unknown on every get command (it lists every namespace when
// --namespace is omitted, except for a kind with a default namespace), so the
// hint must say so directly instead of pointing at rancher schema. A
// genuinely unknown flag must still get the generic hint.
// Mirrors TestDeleteClusterHintsAtProvisioningCluster one seam over.
func TestAllNamespacesHintTargeted(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
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
		assert.Contains(t, env.Data.Error.Message, "there is no all-namespaces flag - a get command lists every namespace when you omit --namespace, except for a kind with a default namespace (fleet-default); use --namespace for a kind that takes one")
		assert.Equal(t, "rancher schema", env.Data.Error.Hint)
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
		assert.Contains(t, env.Data.Error.Message, "there is no all-namespaces flag - a get command lists every namespace when you omit --namespace, except for a kind with a default namespace (fleet-default); use --namespace for a kind that takes one")
		assert.Equal(t, "rancher schema", env.Data.Error.Hint)
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
		// kind is management-scoped, not cluster-scoped) - declaring it
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
		assert.Contains(t, env.Data.Error.Message, "there is no all-namespaces flag",
			"create namespace -A must get the verb-agnostic detail, not the get-specific wording")
		assert.NotContains(t, env.Data.Error.Message, "a get command lists every namespace",
			"the get-specific wording must not leak onto a non-get verb")
		assert.Equal(t, "rancher schema", env.Data.Error.Hint)
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
		assert.Contains(t, env.Data.Error.Message, "pass --help for usage")
		assert.Equal(t, "rancher schema", env.Data.Error.Hint,
			"a genuinely unknown flag on a non-get verb must get the generic hint")
	})

	t.Run("bare get group -A asserts path-length-2 boundary", func(t *testing.T) {
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
		assert.Contains(t, env.Data.Error.Message, "there is no all-namespaces flag - a get command lists every namespace when you omit --namespace, except for a kind with a default namespace (fleet-default); use --namespace for a kind that takes one",
			"bare get group -A (path length 2) must yield the get-scoped wording, asserting the >= 2 boundary")
		assert.Equal(t, "rancher schema", env.Data.Error.Hint)
	})
}

// TestAllNamespacesHintsBothShareOpeningClause asserts that both rendered
// wordings of the -A/--all-namespaces message detail open with
// noAllNamespacesFlag. Calling allNamespacesEntry.detail directly for a
// get-context command and a non-get-context command verifies the shared
// clause is not accidentally dropped if the branching logic changes.
func TestAllNamespacesHintsBothShareOpeningClause(t *testing.T) {
	t.Parallel()
	root := &ucli.Command{Name: "rancher"}
	get := &ucli.Command{Name: "get"}
	create := &ucli.Command{Name: "create"}
	root.Commands = []*ucli.Command{get, create}
	root.Writer = &bytes.Buffer{}
	_ = root.Run(t.Context(), []string{"rancher"}) // wires parent pointers via setupDefaults

	err := errors.New("flag provided but not defined: -A")
	getDetail := allNamespacesEntry.detail(usageErrorInput{c: get, err: err})
	assert.True(t, strings.HasPrefix(getDetail, noAllNamespacesFlag),
		"get-scoped -A detail must open with the noAllNamespacesFlag clause")
	nonGetDetail := allNamespacesEntry.detail(usageErrorInput{c: create, err: err})
	assert.True(t, strings.HasPrefix(nonGetDetail, noAllNamespacesFlag),
		"verb-agnostic -A detail must open with the noAllNamespacesFlag clause")
}

// An explicitly overridden config dir must not put a plain-text notice ahead
// of the error envelope - stderr must parse as pure JSON, exactly as a
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

// TestCorruptConfigDoesNotBreakSchemaOrCompletionCallback asserts the hard
// requirement that resolveReadOnly already degrades to not-read-only on a
// config load error (adapter.go:129-143) rather than failing the command, on
// BOTH a before-only path (schema, Adapt-wired, no resolveDeps) and the
// completion callback (Before runs, then completion returns before Action).
// This must hold with no code change to resolveReadOnly/resolveDeps - the test
// only asserts existing behavior against regression.
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
	require.Zero(t, runCompletionRequest(t, root, []string{"--config", dir, "get"}, ""))
	assert.NotEmpty(t, out.String(), "the completion callback must still emit candidates on a corrupt config, not fail")
}

// TestGenerateShellCompletionListsTopLevelCommandNames asserts that
// EnableShellCompletion activates default command-name completion for the
// existing tree with no assembleCommands/wireEntry changes. Membership only -
// urfave-owned ordering/format is not ours to golden-test.
func TestGenerateShellCompletionListsTopLevelCommandNames(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.Zero(t, runCompletionRequest(t, root, []string{"--config", dir}, ""))
	assert.Contains(t, out.String(), "get")
	assert.Contains(t, out.String(), "auth")
}

// TestHiddenCompletionCommandEmitsNoScriptMarkers asserts that the renamed
// built-in (__completion) remains invocable as machinery only: its
// subcommands are stripped, so `rancher __completion bash` cannot emit
// urfave's own divergent script. This asserts the INTENT, not urfave's own
// help-topic mechanism: post-strip, "bash" is an unmatched subcommand name on
// __completion, which now routes through the repo's standard CommandNotFound
// seam - a structured JSON VALIDATION envelope on stderr at a non-zero exit,
// the same envelope-everywhere contract every other unmatched command name
// gets (see TestUnknownCommandIsValidationNotExitThree), not urfave's own
// unhinted "No help topic" text and not a custom help Action (the command is
// internal machinery, not part of the command surface).
func TestHiddenCompletionCommandEmitsNoScriptMarkers(t *testing.T) {
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	CaptureArgv([]string{"--config", dir, "-o", "json", "__completion", "bash"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "-o", "json", "__completion", "bash"}))
	assert.Equal(t, 2, exitCodeOf(root, nil), "an unmatched subcommand name is VALIDATION, exit 2, not urfave's own exit 3")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bash")

	combined := out.String() + errOut.String()
	assert.NotContains(t, combined, "--generate-shell-completion",
		"the hidden built-in must never emit urfave's own completion script")
}

// TestPublicContractOpsAreListed closes the op-level half of the public-API
// conformance guarantee (the kind-table half lives in
// internal/engine/kinds/publicapi_test.go): every operation the composed
// resolver marks ContractPublic must either belong to a kind whose row is
// itself ContractPublic (generated verbs and the derived create ops inherit
// the row's binding) or appear in the bespoke list below. Adding a
// new public op compiles fine - this test is what turns the widened public
// surface into a reviewed diff instead of silent drift.
func TestPublicContractOpsAreListed(t *testing.T) {
	t.Parallel()
	publicKindByName := map[string]bool{}
	for _, k := range kinds.All() {
		if k.Binding.Contract == safety.ContractPublic {
			publicKindByName[k.Name] = true
		}
	}
	resolver, err := composeBindings()
	require.NoError(t, err)
	// Both directions: every listed bespoke op must actually resolve public…
	for op := range bespokePublicOps {
		b, ok := resolver.Resolve(op)
		require.True(t, ok, op)
		assert.Equal(t, safety.ContractPublic, b.Contract,
			"op %q is in bespokePublicOps but does not resolve ContractPublic", op)
	}
	// …and every public op must back a public kind row or be listed.
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
			"op %q is ContractPublic but neither backs a public kind row nor is in bespokePublicOps - widening the public surface must be a reviewed edit there", op)
	}
}

// TestCreateOpBindingsTrackKindRows asserts the derivation rule: every op in
// derivedOpKinds must resolve to its kind row's Binding exactly, because
// composeBindings derives them via the row rather than a hand-copied
// literal in provider.NewResolver - the same drift-closure as
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

// TestFeaturePatchWarnsWhenFlagIsNotDynamic asserts the feature kind's
// patch override wires the restart advisory: a read status.dynamic of
// false yields the warning, true or absent yields none, and the override
// table actually carries a feature patch entry.
func TestFeaturePatchWarnsWhenFlagIsNotDynamic(t *testing.T) {
	t.Parallel()
	require.NotNil(t, kindOverrideTable["feature"].patch, "the feature kind must override patch to carry the restart advisory")

	obj := func(status map[string]any) *unstructured.Unstructured {
		o := map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Feature"}
		if status != nil {
			o["status"] = status
		}
		return &unstructured.Unstructured{Object: o}
	}
	for _, tc := range []struct {
		name   string
		orig   *unstructured.Unstructured
		warned bool
	}{
		{"non-dynamic flag warns", obj(map[string]any{"dynamic": false}), true},
		{"dynamic flag stays silent", obj(map[string]any{"dynamic": true}), false},
		{"absent field stays silent", obj(nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, ok := featureRestartWarning(tc.orig)
			assert.Equal(t, tc.warned, ok)
			if tc.warned {
				assert.Contains(t, w, "Rancher restarts to apply it")
			} else {
				assert.Empty(t, w)
			}
		})
	}
}

// versionServer serves a fixed /rancherversion body - enough for
// primeFeatureCommunityWarning's ServerInfo call, no Feature route needed
// since the enricher is exercised directly against an in-memory object.
func versionServer(t *testing.T, rancherVersionBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/rancherversion" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rancherVersionBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// primeFeatureObj builds a minimal Feature object carrying exactly the
// fields primeFeatureCommunityWarning reads: metadata.name (for the
// warning text), status.prime, and status.default (the enabled reading,
// standing in for either spec.value or its status.default fallback).
func primeFeatureObj(name string, prime, enabled bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"prime": prime, "default": enabled},
	}}
}

// TestPrimeFeatureCommunityWarning asserts the advisory's trigger: a feature confirmed
// prime-classified AND confirmed enabled warns only when the server is
// confirmed community - never on Prime, never on an unreadable/unknown
// edition, never for a non-prime feature, never for a disabled one.
func TestPrimeFeatureCommunityWarning(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		rancherPrime string
		prime        bool
		enabled      bool
		warned       bool
	}{
		{"community + prime + enabled: warns", "false", true, true, true},
		{"prime edition + prime + enabled: silent", "true", true, true, false},
		{"community + prime + disabled: silent", "false", true, false, false},
		{"community + non-prime + enabled: silent", "false", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := versionServer(t, fmt.Sprintf(`{"Version":"v2.15.1","RancherPrime":%q}`, tc.rancherPrime))
			f := checkerFor(t, srv).f
			enrich := primeFeatureCommunityWarning(f)
			w, ok := enrich(t.Context(), primeFeatureObj("cluster-autoscaling", tc.prime, tc.enabled))
			assert.Equal(t, tc.warned, ok)
			if tc.warned {
				assert.Contains(t, w, "cluster-autoscaling")
				assert.Contains(t, w, "inactive without Rancher Prime")
			} else {
				assert.Empty(t, w)
			}
		})
	}

	t.Run("edition unreadable: silent, never speculates", func(t *testing.T) {
		t.Parallel()
		srv, hits := versionServer(t, "not json")
		f := checkerFor(t, srv).f
		enrich := primeFeatureCommunityWarning(f)
		w, ok := enrich(t.Context(), primeFeatureObj("cluster-autoscaling", true, true))
		assert.False(t, ok)
		assert.Empty(t, w)
		assert.Positive(t, hits.Load(), "the fake received a request: silence came from its answer, not from a failed connection")
	})

	t.Run("feature kind get override wires the warning", func(t *testing.T) {
		require.NotNil(t, kindOverrideTable["feature"].get, "the feature kind must override get to carry the community-prime advisory")
	})
}

// TestGetFeatureCommunityPrimeWarningLive proves the full composition live
// through the pipeline: `get feature <name>` on a community server whose
// Feature reads prime:true and enabled(status.default:true) carries the
// advisory in the envelope's standard top-level warnings field.
func TestGetFeatureCommunityPrimeWarningLive(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	mux := http.NewServeMux()
	mux.HandleFunc("/rancherversion", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.1","RancherPrime":"false"}`))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/features/cluster-autoscaling", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "Feature",
			"metadata":   map[string]any{"name": "cluster-autoscaling", "resourceVersion": "1"},
			"status":     map[string]any{"prime": true, "default": true},
		})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	dir := grConfigDir(t, srv)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	stdout, stderr, err := grRun(t, root, dir, "get", "feature", "cluster-autoscaling", "-o", "json")
	require.NoError(t, err, "stderr: %s", stderr)

	var env struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env))
	require.Len(t, env.Warnings, 1)
	assert.Contains(t, env.Warnings[0], "cluster-autoscaling")
	assert.Contains(t, env.Warnings[0], "inactive without Rancher Prime")
}
