package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// capabilityChecker composes the provider's per-invocation server-info probe
// with the management-scope feature client behind runtime.CapabilityChecker
// — a deliberate composition: provider owns version/edition
// (config.Effective's unauthenticated HTTP path, never RootClientFactory);
// the feature GET reuses the same engine.ClusterClientFactory every
// Management-scope generated verb uses. Both are the SAME per-invocation
// clientFactory f, so the edition fetch is memoized exactly once regardless
// of how many capability checks (or the version-skew consumer) share it.
type capabilityChecker struct {
	f *clientFactory
}

// newCapabilityChecker builds the invocation-scoped checker adaptEntry wires
// onto IO.Capability for every non-bare command.
func newCapabilityChecker(f *clientFactory) *capabilityChecker {
	return &capabilityChecker{f: f}
}

// Check implements runtime.CapabilityChecker. It never returns an error:
// every failure mode (unresolved server, unreachable /rancherversion,
// forbidden or absent Feature GET, malformed field) folds into
// CapabilityUnknown — unknown never blocks, and detector errors are
// never returned.
func (c *capabilityChecker) Check(ctx context.Context, req runtime.CapabilityRequirement) runtime.CapabilityDecision {
	server := ""
	if eff, err := c.f.resolve(); err == nil {
		server = eff.URL
	}
	dec := runtime.CapabilityDecision{Outcome: runtime.CapabilityUnknown, Server: server}

	var enabled, prime *bool
	if cl, err := c.f.Cluster(ctx, engine.LocalCluster); err == nil {
		if k, ok := kinds.Lookup("feature"); ok {
			if obj, err := cl.Get(ctx, k.GVR, "", req.Feature); err == nil {
				enabled, prime = featureState(obj)
			}
		}
	}
	// The edition is fetched only when the decision can read it: an
	// affirmatively enabled feature decides alone (truth-table outcome 1),
	// and an unreadable feature (enabled == nil) is unknown regardless of
	// edition — both paths skip the /rancherversion round trip. The
	// decision itself is still decideCapability's alone; only the fetch is
	// conditional.
	edition := provider.EditionUnknown
	if enabled != nil && !*enabled {
		if info, err := c.f.ServerInfo(ctx); err == nil {
			edition = info.Edition
		}
	}

	dec.Outcome = decideCapability(enabled, prime, edition)
	dec.EditionPrime = edition == provider.EditionPrime
	return dec
}

// featureState reads a Feature object's effective-enabled value and its
// status.prime classification through the canonical engine accessors, so
// the field-path knowledge lives in one place.
func featureState(obj *unstructured.Unstructured) (enabled, prime *bool) {
	return engine.FeatureEffectiveValue(obj), engine.FeaturePrimeOnly(obj)
}

// decideCapability is the four-outcome capability truth table. enabled/prime
// nil means the signal was unavailable, malformed, forbidden, or absent —
// never guessed. Evaluation order encodes the stated precedence: an
// affirmative enabled=true always wins (outcome 1), regardless of edition;
// only then does prime-required require BOTH prime=true and edition=community
// (outcome 3); every other explicit enabled=false reading is disabled,
// regardless of edition (outcome 2) — an explicit false is certain enough on
// its own that a committed mutation against a stopped controller is worse
// than a slightly generic refusal message; only a missing enabled reading is
// unknown (outcome 4).
func decideCapability(enabled, prime *bool, edition provider.Edition) runtime.CapabilityOutcome {
	if enabled == nil {
		return runtime.CapabilityUnknown
	}
	if *enabled {
		return runtime.CapabilityProceed
	}
	if prime != nil && *prime && edition == provider.EditionCommunity {
		return runtime.CapabilityPrimeRequired
	}
	return runtime.CapabilityDisabled
}
