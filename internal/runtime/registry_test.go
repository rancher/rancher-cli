package runtime

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubResolver map[string]safety.Binding

func (s stubResolver) Resolve(op string) (safety.Binding, bool) { b, ok := s[op]; return b, ok }
func (s stubResolver) All() map[string]safety.Binding           { return s }

func TestSchemaMergesResolverContract(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{
		"get cluster": {Contract: safety.ContractInternal, Stability: safety.Beta},
	})
	reg.Register(fakeRead{d: Descriptor{Name: "widgets ls", Usage: "list", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "widgets set", CommandType: Local,
		Flags: []ArgSpec{{Name: "token", Type: "string", Required: true}}}})

	m := reg.Schema("v-test", "2.14")
	assert.Equal(t, "cli.rancher.io/v1", m.OutputAPIVersion)
	assert.Equal(t, "2.14", m.MinRancherVersion)
	assert.False(t, m.ReadOnly, "readOnly defaults false; Schema itself carries no invocation state")
	assert.NotEmpty(t, m.ErrorCodes)
	assert.NotEmpty(t, m.GlobalFlags)

	byName := map[string]SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}
	require.Contains(t, byName, "get cluster")
	assert.Equal(t, "internal", byName["get cluster"].Contract) // merged from resolver

	// Per-command Flags must reach the manifest (agent discoverability).
	require.Contains(t, byName, "widgets set")
	require.Len(t, byName["widgets set"].Flags, 1)
	assert.Equal(t, "token", byName["widgets set"].Flags[0].Name)
	assert.True(t, byName["widgets set"].Flags[0].Required)
}

// TestSchemaManifestExitCodes asserts that the manifest's two views of the
// exit mapping agree: every errorCodes entry carries the exit status the
// exitCodes map gives its code, the map covers every listed code, every
// exit is non-zero, and every entry carries a description.
func TestSchemaManifestExitCodes(t *testing.T) {
	t.Parallel()
	m := NewRegistry(stubResolver{}).Schema("v-test", "2.14")

	assert.Equal(t, clierr.Codes(), m.ErrorCodes)
	require.NotEmpty(t, m.ExitCodes)
	assert.Equal(t, clierr.CodeExits(), m.ExitCodes)
	for _, info := range m.ErrorCodes {
		exit, ok := m.ExitCodes[info.Code]
		require.True(t, ok, "error code %q has no exit mapping", info.Code)
		assert.Equal(t, info.Exit, exit, "%s: the entry and the map agree", info.Code)
		assert.Positive(t, exit, "every error code exits non-zero")
		assert.NotEmpty(t, info.Desc, "%s has no description", info.Code)
	}
}

func TestValidateRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly, Shape: ShapeTabular}})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly, Shape: ShapeTabular}})
	err := reg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate")
}

func TestValidateRejectsMutatingWithoutBinding(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{}) // no binding for "create widget"
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, `mutating command "create widget" has no contract and stability set`)
}

func TestValidateRejectsCommandTypeMismatch(t *testing.T) {
	t.Parallel()
	t.Run("read command declared mutating", func(t *testing.T) {
		reg := NewRegistry(stubResolver{"x": {Contract: safety.ContractPublic, Stability: safety.Stable}})
		reg.Register(fakeRead{d: Descriptor{Name: "x", CommandType: Mutating}})
		err := reg.Validate()
		require.Error(t, err)
		assert.EqualError(t, err, `command "x" has commandType "mutating", which does not match the interface it implements (ReadCommand or MutatingCommand)`)
	})
	t.Run("mutating command declared readOnly", func(t *testing.T) {
		reg := NewRegistry(stubResolver{})
		reg.Register(fakeMutating{d: Descriptor{Name: "y", CommandType: ReadOnly}})
		err := reg.Validate()
		require.Error(t, err)
		assert.EqualError(t, err, `command "y" has commandType "readOnly", which does not match the interface it implements (ReadCommand or MutatingCommand)`)
	})
}

type fakeBoth struct{ d Descriptor }

func (f fakeBoth) Describe() Descriptor { return f.d }
func (f fakeBoth) Execute(context.Context, Input, Prompter) (Result, error) {
	return Result{}, nil
}
func (f fakeBoth) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	return safety.Effect{}, nil, nil
}

func TestValidateRejectsUnknownCapabilityTier(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: "bogus", Feature: "widget-feature"}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, `get widget: unknown capability tier "bogus"`)
}

func TestValidateRejectsPrimeWithoutFeatureName(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityPrime}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, `get widget: capability tier "prime" requires a Feature name`)
}

func TestValidatePassesWellFormedCapability(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeTabular,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}}})
	assert.NoError(t, reg.Validate())
}

func TestValidateRejectsExtDiscoveryWithoutUnsupported(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityExtDiscovery, Feature: "passwordchangerequests"}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, `get widget: capability tier "extDiscovery" requires Unsupported`)
}

func TestValidateRejectsPrimeWithUnsupportedSet(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature", Unsupported: "widgets"}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, `get widget: capability tier "prime" does not use Unsupported`)
}

func TestValidatePassesWellFormedExtDiscoveryCapability(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeTabular,
		Capability: CapabilityRequirement{Tier: CapabilityExtDiscovery, Feature: "passwordchangerequests", Unsupported: "the user-password workflow"}}})
	assert.NoError(t, reg.Validate())
}

// TestValidateRejectsFeatureWithoutTier asserts the full-shape validator at the
// registry boundary: a half-set value ({Feature set, Tier ""}) reads as
// absent under IsSet and would otherwise vanish from every gate - it must
// be a startup rejection instead.
func TestValidateRejectsFeatureWithoutTier(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Feature: "widget-feature"}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.EqualError(t, err, "get widget: capability requirement has Feature/Unsupported set with no Tier")
}

// TestValidateRejectsUnknownOutputPolicyFormat asserts the startup rejection for
// a malformed OutputPolicy declaration: an AllowedFormats entry outside
// OutputFormats/"template" is a command bug caught at Validate, never a
// runtime surprise.
func TestValidateRejectsUnknownOutputPolicyFormat(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		OutputPolicy: OutputPolicy{AllowedFormats: []string{"json", "xml"}}}})
	err := reg.Validate()
	require.ErrorContains(t, err, "outputPolicy")
	assert.ErrorContains(t, err, `"xml"`)
}

// TestValidatePassesWellFormedOutputPolicy asserts the accepted shape: every
// OutputFormats literal plus the "template" stand-in.
func TestValidatePassesWellFormedOutputPolicy(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeTabular,
		OutputPolicy: OutputPolicy{AllowedFormats: []string{"table", "wide", "json", "yaml", "template"}}}})
	assert.NoError(t, reg.Validate())
}

// TestOnceOnlyOutputPolicyExcludesName asserts the once-only contract every
// caller shares via this one constructor: every OutputFormats literal is
// allowed except "name", which would silently discard a once-only value
// under -o name's NAME-column-only projection.
func TestOnceOnlyOutputPolicyExcludesName(t *testing.T) {
	t.Parallel()
	p := OnceOnlyOutputPolicy()
	assert.False(t, p.Allows("name"))
	for _, f := range []string{"", "table", "wide", "json", "yaml", "template=x"} {
		assert.True(t, p.Allows(f), "format %q must be allowed", f)
	}
}

// TestValidateRejectsUnspecifiedShape asserts the closed-world OutputShape
// contract: the zero value (ShapeUnspecified) is never valid on a registered
// descriptor - an omitted Shape field must fail at startup, never silently
// default to Tabular.
func TestValidateRejectsUnspecifiedShape(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly}})
	err := reg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "output shape must be declared")
}

// TestValidateRejectsDeliveryOnceOnlyOnReadOnly asserts Delivery's Mutating-only
// restriction: a read command promising once-only delivery makes no sense
// (there is no mutation to have committed) and must fail at startup.
func TestValidateRejectsDeliveryOnceOnlyOnReadOnly(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeTabular, Delivery: DeliveryOnceOnly}})
	err := reg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "is allowed only on mutating commands")
}

// TestValidateRejectsUnknownDeliveryValue asserts the closed Delivery enum: any
// value other than DeliveryOnceOnly (or the zero value) is a startup
// rejection, matching the CapabilityTier precedent.
func TestValidateRejectsUnknownDeliveryValue(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{"create widget": {}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating, Shape: ShapeText, Delivery: "bogus"}})
	err := reg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, `command "create widget": unknown delivery "bogus"`)
}

// TestValidatePassesDeliveryOnceOnlyOnMutating asserts the accepted shape.
func TestValidatePassesDeliveryOnceOnlyOnMutating(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{"create widget": {}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating, Shape: ShapeText, Delivery: DeliveryOnceOnly}})
	assert.NoError(t, reg.Validate())
}

// fakeReadWithResolver is a ReadCommand that also implements
// CapabilityResolver - used to assert the registry's mutual-exclusion rule.
type fakeReadWithResolver struct{ fakeRead }

func (f fakeReadWithResolver) ResolveCapability(context.Context, Input) (CapabilityRequirement, error) {
	return CapabilityRequirement{}, nil
}

// TestValidateRejectsStaticCapabilityAndResolverTogether asserts the
// mutual-exclusion rule: a static requirement and CapabilityResolver on the
// same command are two sources for one value - the resolver would be dead
// code that silently loses to static-wins, so it is a startup rejection.
func TestValidateRejectsStaticCapabilityAndResolverTogether(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeReadWithResolver{fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}}}})
	err := reg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "mutually exclusive")
}

// TestSchemaPrimeCommandProjection asserts the marker wording on a fixture: with
// no production command carrying a requirement yet, a test-registered prime
// command asserts the manifest fields (prime + learnMore ride together,
// projected from the one product-owned URL constant) and the EXACT
// classification sentence - the static surfaces state a product-tier
// classification, never a per-server "requires" claim, so an accidental
// rewording fails here rather than shipping unnoticed.
func TestSchemaPrimeCommandProjection(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}}})
	reg.Register(fakeRead{d: Descriptor{Name: "get plain", CommandType: ReadOnly}})

	byName := map[string]SchemaCommand{}
	for _, c := range reg.Schema("v-test", "2.14").Commands {
		byName[c.Name] = c
	}
	prime := byName["get widget"]
	assert.True(t, prime.Prime)
	assert.Equal(t, clierr.LearnMorePrimeURL, prime.LearnMore,
		"learnMore rides beside prime, projected from the single constant")
	plain := byName["get plain"]
	assert.False(t, plain.Prime)
	assert.Empty(t, plain.LearnMore, "learnMore must be absent without the prime marker")

	assert.Equal(t,
		"A Rancher Prime feature; the connected server decides whether it is available.",
		PrimeSentence,
		"classification wording is a reviewed contract; change it deliberately or not at all")
}

func TestValidateRejectsDualInterface(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{"do thing": {}})
	reg.Register(fakeBoth{d: Descriptor{Name: "do thing", CommandType: Mutating}})
	require.Error(t, reg.Validate(), "dual ReadCommand+MutatingCommand implementer must fail validation")
}

func TestValidatePassesWellFormedRegistry(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{
		"create widget": {Contract: safety.ContractPublic, Stability: safety.Stable},
	})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly, Shape: ShapeTabular}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating, Shape: ShapeText}})
	require.NoError(t, reg.Validate())
}

type ssResolver struct{}

func (ssResolver) Resolve(string) (safety.Binding, bool) { return safety.Binding{}, false }
func (ssResolver) All() map[string]safety.Binding        { return nil }

type ssCmd struct{}

func (ssCmd) Describe() Descriptor {
	return Descriptor{
		Name:        "ss-cmd",
		CommandType: ReadOnly,
		Flags:       []ArgSpec{{Name: "set", Type: "stringSlice"}},
	}
}

func (ssCmd) Execute(context.Context, Input, Prompter) (Result, error) {
	return Result{}, nil
}

// TestSchemaPublishesStringSliceFlagType asserts the manifest contract:
// the stringSlice flag type is published verbatim in the schema manifest.
func TestSchemaPublishesStringSliceFlagType(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(ssResolver{})
	reg.Register(ssCmd{})
	m := reg.Schema("v0-test", "2.14")
	require.Len(t, m.Commands, 1)
	require.Len(t, m.Commands[0].Flags, 1)
	assert.Equal(t, "stringSlice", m.Commands[0].Flags[0].Type)
}

type exitDeclCmd struct {
	name  string
	codes []int
}

func (c exitDeclCmd) Describe() Descriptor {
	return Descriptor{Name: c.name, CommandType: Local, Toolset: "core", Shape: ShapeTabular, SuccessExitCodes: c.codes}
}

// probeTabularData is exitDeclCmd's Result.Data: Tabular (matching its
// declared ShapeTabular) while still exposing a "probe" JSON field for the
// exit-status tests that decode the rendered envelope.
type probeTabularData struct {
	Probe string `json:"probe"`
}

func (p probeTabularData) Columns() []string { return []string{"PROBE"} }
func (p probeTabularData) Rows() [][]string  { return [][]string{{p.Probe}} }

func (c exitDeclCmd) Execute(_ context.Context, _ Input, _ Prompter) (Result, error) {
	return Result{Kind: "probe", Data: probeTabularData{Probe: "ok"}}, nil
}

// TestValidateSuccessExitCodes asserts the declaration invariant: zero exactly
// once; additional values positive, unique, and disjoint from error exits.
func TestValidateSuccessExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		codes   []int
		wantErr string
	}{
		{"accepts zero plus seven", []int{0, 7}, ""},
		{"rejects missing zero", []int{7}, "must contain 0"},
		{"rejects duplicate zero", []int{0, 0, 7}, "must contain 0 exactly once"},
		{"rejects negative", []int{0, -1}, "must be positive"},
		{"rejects duplicate", []int{0, 7, 7}, "duplicate"},
		{"rejects error exit collision", []int{0, 2}, "is also an error exit code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry(stubResolver{})
			reg.Register(exitDeclCmd{name: "probe", codes: tc.codes})
			err := reg.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestSchemaCommandGates asserts the per-command gates field: the manifest
// states each command's unconditional gate flags instead of leaving them
// inferable from commandType/destructive convention.
func TestSchemaCommandGates(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{
		"rm old": {Contract: safety.ContractInternal, Stability: safety.Deprecated},
	})
	reg.Register(fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "cfg set", CommandType: Local}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating}})
	reg.Register(fakeMutating{d: Descriptor{Name: "rm widget", CommandType: Mutating, Destructive: "true"}})
	reg.Register(fakeMutating{d: Descriptor{Name: "apply widget", CommandType: Mutating, Destructive: "dependsOnInput"}})
	reg.Register(fakeMutating{d: Descriptor{Name: "rm old", CommandType: Mutating, Destructive: "true"}})

	byName := map[string]SchemaCommand{}
	for _, c := range reg.Schema("v-test", "2.14").Commands {
		byName[c.Name] = c
	}
	assert.Empty(t, byName["get widget"].Gates, "read-only commands carry no gates")
	assert.Empty(t, byName["cfg set"].Gates, "local commands carry no gates")
	assert.Equal(t, []string{"--apply"}, byName["create widget"].Gates)
	assert.Equal(t, []string{"--apply", "--yes"}, byName["rm widget"].Gates)
	assert.Equal(t, []string{"--apply"}, byName["apply widget"].Gates,
		"dependsOnInput signals the conditional --yes via the destructive field, not gates")
	assert.Equal(t, []string{"--apply", "--yes", "--allow-deprecated"}, byName["rm old"].Gates)
}

type hintingRead struct{ fakeRead }

func (hintingRead) ProjectRefusalHint(Input) clierr.Command { return clierr.Command{} }

// TestValidateProjectFlagRules asserts the three rules on how a command
// takes the global --project flag: only as ProjectFlag's entry, never
// beside a refusal hint for it, and no other flag reuses a global flag's
// name or short form.
func TestValidateProjectFlagRules(t *testing.T) {
	t.Parallel()
	read := func(flags ...ArgSpec) Descriptor {
		return Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText, Flags: flags}
	}
	for _, tc := range []struct {
		name string
		cmd  Command
		want string // "" = valid
	}{
		{"the ProjectFlag entry", fakeRead{d: read(ProjectFlag(false, "d"))}, ""},
		{"a required ProjectFlag entry", fakeRead{d: read(ProjectFlag(true, "d"))}, ""},
		{"a bool project flag", fakeRead{d: read(ArgSpec{Name: FlagProject, Type: "bool"})}, "must be runtime.ProjectFlag's entry"},
		{"a project flag with an alias", fakeRead{d: read(ArgSpec{Name: FlagProject, Type: "string", Aliases: []string{"p"}})}, "must be runtime.ProjectFlag's entry"},
		{"a project flag with an enum", fakeRead{d: read(ArgSpec{Name: FlagProject, Type: "string", Enum: []string{"a"}})}, "must be runtime.ProjectFlag's entry"},
		{"a project flag with an env var", fakeRead{d: read(ArgSpec{Name: FlagProject, Type: "string", Env: []string{"X"}})}, "must be runtime.ProjectFlag's entry"},
		{"a refusal hint without the flag", hintingRead{fakeRead{d: read()}}, ""},
		{"a refusal hint beside the flag", hintingRead{fakeRead{d: read(ProjectFlag(false, "d"))}}, "also implements ProjectRefusalHinter"},
		{"a flag named like a global flag", fakeRead{d: read(ArgSpec{Name: "output", Type: "string"})}, `flag "output" reuses the name of a global flag`},
		{"a flag aliased like a global short form", fakeRead{d: read(ArgSpec{Name: "context", Type: "string", Aliases: []string{"c"}})}, `flag "c" reuses the name of a global flag`},
		{"a bool flag named like a global flag", fakeRead{d: read(ArgSpec{Name: "dry-run", Type: "bool"})}, `flag "dry-run" reuses the name of a global flag`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry(stubResolver{})
			reg.Register(tc.cmd)
			err := reg.Validate()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestValidateFlagNamesAreDeclaredOnce asserts that a command's flag names
// and aliases are each declared once: two flags with one name, a flag whose
// alias is another flag's alias or name, and a flag whose alias is its own
// name are refused with the name in the message, and distinct names and
// aliases are accepted.
func TestValidateFlagNamesAreDeclaredOnce(t *testing.T) {
	t.Parallel()
	read := func(flags ...ArgSpec) Descriptor {
		return Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText, Flags: flags}
	}
	for _, tc := range []struct {
		name  string
		flags []ArgSpec
		want  string // "" = valid
	}{
		{"distinct names and aliases", []ArgSpec{
			{Name: "namespace", Type: "string", Aliases: []string{"n"}},
			{Name: "selector", Type: "string", Aliases: []string{"l"}},
			{Name: "wait", Type: "bool"},
		}, ""},
		{"two flags with one name", []ArgSpec{
			{Name: "pool", Type: "string"}, {Name: "pool", Type: "string"},
		}, `command "get widget": the flag name or alias "pool" is declared twice`},
		{"an alias that is another flag's alias", []ArgSpec{
			{Name: "namespace", Type: "string", Aliases: []string{"n"}},
			{Name: "node", Type: "string", Aliases: []string{"n"}},
		}, `the flag name or alias "n" is declared twice`},
		{"an alias that is another flag's name", []ArgSpec{
			{Name: "pool", Type: "string"},
			{Name: "node", Type: "string", Aliases: []string{"pool"}},
		}, `the flag name or alias "pool" is declared twice`},
		{"an alias that is its own flag's name", []ArgSpec{
			{Name: "pool", Type: "string", Aliases: []string{"pool"}},
		}, `the flag name or alias "pool" is declared twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry(stubResolver{})
			reg.Register(fakeRead{d: read(tc.flags...)})
			err := reg.Validate()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}
