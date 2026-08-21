package runtime

import "context"

// CapabilityOutcome is the detector's four-outcome result for one capability
// requirement (design: two normative signals, four outcomes).
type CapabilityOutcome int

const (
	// CapabilityProceed means the preflight permits execution — it does NOT
	// prove the target API/CRD/route is installed and ready; the actual
	// request stays authoritative. Proceed (affirmative evidence) and
	// Unknown (no evidence) run the target identically TODAY, and the
	// distinction is kept anyway: it is the design's reviewed contract, and
	// collapsing them would discard the detector's information the moment a
	// consumer (verbose messaging, telemetry) wants it.
	CapabilityProceed CapabilityOutcome = iota
	// CapabilityDisabled means the feature reads effective-disabled on a
	// server whose edition is affirmatively Prime — "enable feature <name>"
	// messaging, the existing error taxonomy (no new code).
	CapabilityDisabled
	// CapabilityPrimeRequired means the feature reads effective-disabled AND
	// prime-classified AND the server edition is affirmatively community —
	// the only outcome that returns the PRIME_REQUIRED error.
	CapabilityPrimeRequired
	// CapabilityUnknown covers everything else: either signal missing,
	// malformed, forbidden, absent, unrecognized, or contradictory in a way
	// the truth table does not resolve. Unknown never blocks and never
	// surfaces a detector-side error — the target request executes and its
	// own result/error is handled as if no detector existed.
	CapabilityUnknown
)

// PrimeSentence is the exact wording every rendered surface uses to project
// a Prime requirement: the command card's appended sentence and, via the
// surface adapters, the normal --help Description. One string, never
// separately worded per surface.
const PrimeSentence = "Requires Rancher Prime."

// CapabilityDecision is one Check call's result, carrying the detail fields
// a PRIME_REQUIRED error's Details needs.
type CapabilityDecision struct {
	Outcome CapabilityOutcome
	// Server is the connected server's URL, for the PRIME_REQUIRED error's
	// details.server — populated whenever the checker could resolve it,
	// regardless of Outcome.
	Server string
}

// CapabilityChecker probes whether the resolved server permits a capability
// requirement. It lives in internal/runtime (beside the pipeline contract it
// serves) so Run can invoke it without runtime importing engine or provider
// — the composed implementation lives in surface/cli, built from the
// provider's server-info probe and the engine feature client, and is handed
// to Run as an invocation dependency via IO.Capability.
type CapabilityChecker interface {
	Check(ctx context.Context, req CapabilityRequirement) CapabilityDecision
}
