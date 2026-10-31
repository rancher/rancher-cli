package runtime

import "context"

// CapabilityOutcome is the detector's four-outcome result for one capability
// requirement.
type CapabilityOutcome int

const (
	// CapabilityProceed means the preflight permits execution - it does NOT
	// prove the target API/CRD/route is installed and ready; the actual
	// request stays authoritative. Proceed (affirmative evidence) and
	// Unknown (no evidence) run the target identically TODAY, and the
	// distinction is kept anyway - deliberately, as a contract - because
	// collapsing them would discard the detector's information the moment a
	// consumer (verbose messaging, telemetry) wants it.
	CapabilityProceed CapabilityOutcome = iota
	// CapabilityDisabled means the feature reads effective-disabled and is
	// not prime-required (see CapabilityPrimeRequired) - on ANY edition,
	// including community and unknown. An explicit disabled reading is
	// treated as certain regardless of what else is unknown about the
	// server, because letting it fall through as Unknown would let a
	// mutation commit against a CRD whose controller is not running.
	// Messaging varies by edition: a server known to be Prime gets
	// "enable feature <name>"; every other edition gets a neutral
	// statement true regardless of tier.
	CapabilityDisabled
	// CapabilityPrimeRequired means the feature reads effective-disabled AND
	// prime-classified AND the server edition is affirmatively community -
	// the only outcome that returns the PRIME_REQUIRED error.
	CapabilityPrimeRequired
	// CapabilityUnknown covers everything else: either signal missing,
	// malformed, forbidden, absent, unrecognized, or contradictory in a way
	// the truth table does not resolve. Unknown never blocks and never
	// surfaces a detector-side error - the target request executes and its
	// own result/error is handled as if no detector existed.
	CapabilityUnknown
	// CapabilityUnsupported is CapabilityExtDiscovery's negative outcome: a
	// readable discovery document that affirmatively lacks the named
	// resource - the server is a version that never carries this workflow,
	// not a feature flag that could be turned on. Returns the
	// CAPABILITY_UNSUPPORTED error. A discovery document that could not be
	// read at all is CapabilityUnknown, never this outcome - the
	// unknown-never-blocks rule holds identically across tiers.
	CapabilityUnsupported
)

// PrimeSentence is the exact wording every rendered surface uses to project
// a Prime marker: the command card's appended sentence and, via the surface
// adapters, the normal --help Description. One string, never separately
// worded per surface. The wording is a PRODUCT-TIER CLASSIFICATION, not a
// per-server requirement claim: a capability can be community-usable on one
// supported server version and Prime-gated on the next, so a static surface
// must never assert "requires" - only the runtime PRIME_REQUIRED error does
// that, backed by the detector's affirmative per-server evidence.
const PrimeSentence = "A Rancher Prime feature; the connected server decides whether it is available."

// PrimeMarker is the token every command list places at the head of a
// Prime command's usage column, and after an all-Prime group's label. One
// spelling, used by every renderer and colored by ColorPrimeMarker.
const PrimeMarker = "[Prime]"

// CapabilityDecision is one Check call's result, carrying the detail fields
// a PRIME_REQUIRED error's Details needs.
type CapabilityDecision struct {
	Outcome CapabilityOutcome
	// Server is the connected server's URL, for the PRIME_REQUIRED error's
	// details.server - populated whenever the checker could resolve it,
	// regardless of Outcome.
	Server string
	// EditionPrime is true when the connected server's edition is
	// affirmatively Prime. It only changes behavior for CapabilityDisabled,
	// selecting between the two message variants; every other outcome
	// ignores it.
	EditionPrime bool
}

// CapabilityChecker probes whether the resolved server permits a capability
// requirement. It lives in internal/runtime (beside the pipeline contract it
// serves) so Run can invoke it without runtime importing engine or provider -
// the composed implementation lives in internal/cli, built from the
// provider's server-info probe and the engine feature client, and is handed
// to Run as an invocation dependency via IO.Capability.
type CapabilityChecker interface {
	Check(ctx context.Context, req CapabilityRequirement) CapabilityDecision
}
