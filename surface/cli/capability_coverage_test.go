package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// gatedWidgetRow is a synthetic Capability-bearing kind row: the closed-world
// kinds table has no way to register one, so the coverage rule's own tests
// build rows by hand instead of reaching into the real table.
func gatedWidgetRow() kinds.Kind {
	return kinds.Kind{
		Key: "widget", Name: "widget", Deletable: true,
		GVR:        schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"},
		Capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"},
	}
}

func TestCapabilityCoverageGatedKindUncoveredCommandFails(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{gatedWidgetRow()},
		registered: map[string]registeredCommand{
			"get widget":    {},
			"patch widget":  {},
			"delete widget": {},
		},
	})
	assert.False(t, res.clean())
	require.Len(t, res.uncoveredGated, 3, "get/patch/delete must each be flagged uncovered")
	for _, op := range []string{"get widget", "patch widget", "delete widget"} {
		want := op + `: targets Capability-bearing kind "widget" but is neither Capability-carrying nor a resolver`
		assert.Contains(t, res.uncoveredGated, want)
	}
}

func TestCapabilityCoverageCoveredPasses(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{gatedWidgetRow()},
		registered: map[string]registeredCommand{
			"get widget":    {capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}},
			"patch widget":  {capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}},
			"delete widget": {resolver: true},
		},
	})
	assert.True(t, res.clean())
}

func TestCapabilityCoverageMissingCommandIsUncovered(t *testing.T) {
	t.Parallel()
	// No registered entry at all for a Capability-bearing kind's op — the
	// gap the retired tripwire existed to catch, expressed generically.
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows:       []kinds.Kind{gatedWidgetRow()},
		registered: map[string]registeredCommand{},
	})
	assert.NotEmpty(t, res.uncoveredGated)
}

func TestCapabilityCoverageUnclassifiedOpFails(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		registered: map[string]registeredCommand{"mystery op": {}},
	})
	require.Len(t, res.unclassified, 1)
	assert.Contains(t, res.unclassified[0], "mystery op")
	assert.Contains(t, res.unclassified[0], "classify this op")
}

func TestCapabilityCoverageAllowlistedOpPasses(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		registered: map[string]registeredCommand{"auth login": {}},
		allowlist:  map[string]string{"auth login": "local auth, no kind row"},
	})
	assert.True(t, res.clean())
}

// coveredWidgetVerbs registers get/patch/delete widget as already covered,
// so the create-edge tests below exercise only the "create resource" op —
// the generic verb enumeration runs over every row in rows regardless of
// createOp, and would otherwise flag get/patch/delete widget too.
func coveredWidgetVerbs() map[string]registeredCommand {
	req := runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}
	return map[string]registeredCommand{
		"get widget":    {capability: req},
		"patch widget":  {capability: req},
		"delete widget": {capability: req},
	}
}

// TestCapabilityCoverageCreateEdgeUncoveredWhenNoResolver and its sibling
// below pin the create-edge exactly: a synthetic creatable gated row is
// covered when the create command implements the capability resolver, and
// uncovered when it does not.
func TestCapabilityCoverageCreateEdgeUncoveredWhenNoResolver(t *testing.T) {
	t.Parallel()
	registered := coveredWidgetVerbs()
	registered["create resource"] = registeredCommand{}
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows:       []kinds.Kind{gatedWidgetRow()},
		createOp:   "create resource",
		registered: registered,
	})
	require.Len(t, res.uncoveredGated, 1)
	assert.Contains(t, res.uncoveredGated[0], "create resource")
}

func TestCapabilityCoverageCreateEdgeCoveredWithResolver(t *testing.T) {
	t.Parallel()
	registered := coveredWidgetVerbs()
	registered["create resource"] = registeredCommand{resolver: true}
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows:       []kinds.Kind{gatedWidgetRow()},
		createOp:   "create resource",
		registered: registered,
	})
	assert.Empty(t, res.uncoveredGated)
}

// TestCapabilityCoverageCreateEdgeExemptsNoCreateViaManifest pins the
// per-row structural-refusal exemption: a NoCreateViaManifest row is exempt
// on the create edge even though it is Capability-bearing, the same class
// as an override's refusals entry.
func TestCapabilityCoverageCreateEdgeExemptsNoCreateViaManifest(t *testing.T) {
	t.Parallel()
	row := gatedWidgetRow()
	row.NoCreateViaManifest = "use rancher create widget"
	registered := coveredWidgetVerbs()
	registered["create resource"] = registeredCommand{}
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows:       []kinds.Kind{row},
		createOp:   "create resource",
		registered: registered,
	})
	assert.Empty(t, res.uncoveredGated, "a NoCreateViaManifest row must be exempt on the create edge")
}

func TestCapabilityCoverageRefusedVerbExemptFromCapabilityButClassified(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{gatedWidgetRow()},
		overrides: map[string]kindOverride{
			"widget": {
				get:      func(kinds.Kind, runDeps) runtime.Command { return nil },
				refusals: map[string]string{"get": "read-only"},
			},
		},
		registered: map[string]registeredCommand{
			"get widget":    {}, // refused: exempt from the capability requirement
			"patch widget":  {capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}},
			"delete widget": {resolver: true},
		},
	})
	assert.True(t, res.clean(), "a refused verb must not be flagged uncovered: %v", res.uncoveredGated)
}

func TestCapabilityCoverageDanglingRefusalFails(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{{Key: "widget", Name: "widget"}},
		overrides: map[string]kindOverride{
			// refusals names "patch" but no ctor is wired for it: a
			// conformance failure, not a live refusal.
			"widget": {refusals: map[string]string{"patch": "read-only"}},
		},
		registered: map[string]registeredCommand{"get widget": {}, "patch widget": {}},
	})
	require.Len(t, res.danglingRefusals, 1)
	assert.Contains(t, res.danglingRefusals[0], "widget")
	assert.Contains(t, res.danglingRefusals[0], `"patch"`)
}

// TestCapabilityCoverageRealWiring runs the coverage rule against the actual
// registry. The gating half is LIVE and non-vacuous as of the oidcclient row
// (2026-08-26, the first Capability-bearing kind — feature "oidc-provider"):
// its generated get/patch/delete each copy the row's requirement into their
// own Descriptor (resources/get.go, patch.go, delete.go), and `create
// resource` is covered structurally via its CapabilityResolver — so
// uncoveredGated now exercises the real coverage rule, not just the
// pure-function tests above. The classification half has been LIVE since
// before: every registered op must resolve to a kind mapping or
// capabilityCoverageAllowlist, so a new op classified as neither fails here,
// the same silence-is-impossible property the retired tripwire
// (TestNoKindDeclaresCapabilityUntilCreateGatingLands, now deleted) had.
func TestCapabilityCoverageRealWiring(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	registered := map[string]registeredCommand{}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		_, isResolver := c.(runtime.CapabilityResolver)
		registered[d.Name] = registeredCommand{capability: d.Capability, resolver: isResolver}
	}
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows:       kinds.All(),
		overrides:  kindOverrideTable,
		derivedOps: derivedOpKinds,
		extraOps:   capabilityCoverageExtraOps,
		createOp:   "create resource",
		allowlist:  capabilityCoverageAllowlist,
		registered: registered,
	})
	assert.Empty(t, res.uncoveredGated,
		"every Capability-bearing kind's ops must be Capability-carrying or a resolver")
	assert.Empty(t, res.unclassified,
		"classify this op: add it to capabilityCoverageExtraOps or capabilityCoverageAllowlist")
	assert.Empty(t, res.danglingRefusals)
	assert.Empty(t, res.unknownKinds,
		"every mapping key (overrides, derivedOpKinds, capabilityCoverageExtraOps) must name a real kind row")
}

// TestCapabilityCoverageStaleMappingKeyFails: an extraOps/derivedOps entry
// naming a kind key with no row must fail the rule, never silently mark the
// op covered — a stale key after a kind rename would otherwise let a gated
// kind reachable only through that op ship ungated while the rule stayed
// green.
func TestCapabilityCoverageStaleMappingKeyFails(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{gatedWidgetRow()},
		derivedOps: map[string]string{
			"frob widget": "widget-renamed",
		},
		extraOps: map[string][]string{
			"inspect widget": {"widget-typo"},
		},
		registered: map[string]registeredCommand{
			"frob widget":    {},
			"inspect widget": {},
		},
	})
	assert.False(t, res.clean())
	require.Len(t, res.unknownKinds, 2)
	assert.Contains(t, res.unknownKinds[0], "widget-renamed")
	assert.Contains(t, res.unknownKinds[1], "widget-typo")
}

// TestCapabilityCoverageStaleOverrideKeyFails: a kindOverrideTable entry
// keyed to a nonexistent row is the same stale-key class and must fail
// rather than be skipped.
func TestCapabilityCoverageStaleOverrideKeyFails(t *testing.T) {
	t.Parallel()
	res := checkCapabilityCoverage(capabilityCoverageInput{
		rows: []kinds.Kind{gatedWidgetRow()},
		overrides: map[string]kindOverride{
			"widget-gone": {},
		},
	})
	assert.False(t, res.clean())
	require.Len(t, res.unknownKinds, 1)
	assert.Contains(t, res.unknownKinds[0], `override "widget-gone"`)
}
