package cli

import (
	"context"
	"encoding/json"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// capabilityChecker composes the provider's per-invocation server-info probe
// with the management-scope feature client behind runtime.CapabilityChecker -
// a deliberate composition: provider owns version/edition
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
// CapabilityUnknown - unknown never blocks, and detector errors are
// never returned.
func (c *capabilityChecker) Check(ctx context.Context, req runtime.CapabilityRequirement) runtime.CapabilityDecision {
	server := ""
	if eff, err := c.f.resolve(); err == nil {
		server = eff.URL
	}
	dec := runtime.CapabilityDecision{Outcome: runtime.CapabilityUnknown, Server: server}

	if req.Tier == runtime.CapabilityExtDiscovery {
		dec.Outcome = c.checkExtDiscovery(ctx, req.Feature)
		return dec
	}

	var enabled, prime *bool
	if cl, err := c.f.Cluster(ctx, engine.LocalCluster); err == nil {
		if k, ok := kinds.Lookup("feature"); ok {
			if obj, err := cl.Get(ctx, k.GVR, "", req.Feature); err == nil {
				enabled, prime = featureState(obj)
			}
		}
	}
	// The edition is fetched when EITHER decideCapability needs it (the
	// feature is confirmed prime-classified - see decideCapability) OR the
	// disabled-message renderer needs it (dec.EditionPrime picks the
	// actionable "enable feature" wording on a known-Prime server, versus
	// the neutral wording everywhere else - pipeline.go). A feature that is
	// both non-prime and affirmatively enabled needs neither, so the
	// /rancherversion round trip is skipped for that common case.
	edition := provider.EditionUnknown
	if (prime != nil && *prime) || (enabled != nil && !*enabled) {
		if info, err := c.f.ServerInfo(ctx); err == nil {
			edition = info.Edition
		}
	}

	dec.Outcome = decideCapability(enabled, prime, edition)
	dec.EditionPrime = edition == provider.EditionPrime
	return dec
}

// extDiscoveryGroup is the API group every ext resource this CLI knows about
// (tokens, kubeconfigs, and the user-password workflow's
// passwordchangerequests) lives under.
const extDiscoveryGroup = "ext.cattle.io"

// apiGroup is the subset of a Kubernetes APIGroup discovery document this
// checker reads; unknown fields are ignored.
type apiGroup struct {
	PreferredVersion struct {
		GroupVersion string `json:"groupVersion"`
	} `json:"preferredVersion"`
}

// apiResourceList is the subset of a Kubernetes APIResourceList discovery
// document this checker reads; unknown fields are ignored.
type apiResourceList struct {
	Resources []struct {
		Name string `json:"name"`
	} `json:"resources"`
}

// checkExtDiscovery resolves the ext.cattle.io group's preferred served
// version - never a pinned one; the served version varies by Rancher
// release, same rationale as the token/kubeconfig kind rows' empty GVR
// Version - and reads that version's discovery document to report whether
// resource is listed. A readable document is authoritative either way
// (CapabilityProceed or CapabilityUnsupported); any read/decode failure -
// network, RBAC, a server whose ext.cattle.io group does not even exist yet -
// folds into CapabilityUnknown, never CapabilityUnsupported: an unreadable
// signal must never be misread as a confirmed absence.
func (c *capabilityChecker) checkExtDiscovery(ctx context.Context, resource string) runtime.CapabilityOutcome {
	root, err := c.f.Root()
	if err != nil {
		return runtime.CapabilityUnknown
	}
	groupBody, err := root.GetJSON(ctx, "/apis/"+extDiscoveryGroup)
	if err != nil {
		return runtime.CapabilityUnknown
	}
	var group apiGroup
	if err := json.Unmarshal(groupBody, &group); err != nil {
		return runtime.CapabilityUnknown
	}
	if group.PreferredVersion.GroupVersion == "" {
		return runtime.CapabilityUnknown
	}
	resBody, err := root.GetJSON(ctx, "/apis/"+group.PreferredVersion.GroupVersion)
	if err != nil {
		return runtime.CapabilityUnknown
	}
	var list apiResourceList
	if err := json.Unmarshal(resBody, &list); err != nil {
		return runtime.CapabilityUnknown
	}
	for _, r := range list.Resources {
		if r.Name == resource {
			return runtime.CapabilityProceed
		}
	}
	return runtime.CapabilityUnsupported
}

// featureState reads a Feature object's effective-enabled value and its
// status.prime classification through the canonical engine accessors, so
// the field-path knowledge lives in one place.
func featureState(obj *unstructured.Unstructured) (enabled, prime *bool) {
	return engine.FeatureEffectiveValue(obj), engine.FeaturePrimeOnly(obj)
}

// decideCapability is the four-outcome capability truth table. enabled/prime
// nil means the signal was unavailable, malformed, forbidden, or absent -
// never guessed. A feature confirmed prime-classified on a confirmed
// community edition always resolves PRIME_REQUIRED (outcome 3) BEFORE
// enabled is even consulted: Rancher's own Feature.Enabled() checks
// "prime && not a Prime build" ahead of status.default, spec.value, and
// lockedValue, so on a community server a prime feature is unconditionally
// disabled server-side regardless of what its Feature object's enabled
// reading says - status.default:true does not mean the feature actually
// runs there. Only once prime+community is not both confirmed does an
// affirmative enabled=true proceed (outcome 1); an explicit enabled=false is
// disabled regardless of edition (outcome 2); and a missing enabled reading,
// with prime+community not both confirmed, is unknown (outcome 4).
// primeInertOnCommunity reports the one server shape where a feature can
// never act no matter what its object says: the feature is confirmed
// prime-classified and the edition is affirmatively community - the server's
// own build gate wins over any enabled value, operator-set or defaulted.
// Both the capability gate and the get-feature advisory key on this exact
// predicate so the two cannot drift apart.
func primeInertOnCommunity(prime *bool, edition provider.Edition) bool {
	return prime != nil && *prime && edition == provider.EditionCommunity
}

func decideCapability(enabled, prime *bool, edition provider.Edition) runtime.CapabilityOutcome {
	if primeInertOnCommunity(prime, edition) {
		return runtime.CapabilityPrimeRequired
	}
	if enabled == nil {
		return runtime.CapabilityUnknown
	}
	if *enabled {
		return runtime.CapabilityProceed
	}
	return runtime.CapabilityDisabled
}
