package oidcclient

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// alwaysProceedCapabilityChecker satisfies runtime.IO.Capability for tests
// that drive a command through the full runtime.Run pipeline (rather than
// calling Plan/exec directly): every oidc-client command declares the
// oidc-provider Prime capability, so Run refuses to dispatch without a
// checker wired, regardless of what the check would actually decide.
type alwaysProceedCapabilityChecker struct{}

func (alwaysProceedCapabilityChecker) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// TestRecoveryHint asserts the single shared spelling every verb's recovery
// text is built from - a future wording change only has to happen here.
func TestRecoveryHint(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" get oidcclient 'my-app' -o json", recoveryHint("my-app"))
}

// TestOidcClientKindResolvesWithoutPanicking asserts oidcClientKind's
// tripwire: it panics if the "oidcclient" row is ever renamed or removed
// from kinds.go without this package being updated in lockstep - every
// bespoke descriptor's Capability, GVR, and Sensitive flag come from this
// one lookup, so a silent zero-value fallback here would silently weaken
// the Prime gate every one of them shares. This test asserts today's row
// still resolving; a future rename that breaks it will fail loudly here,
// at panic, rather than as a silently-ungated command.
func TestOidcClientKindResolvesWithoutPanicking(t *testing.T) {
	t.Parallel()
	assert.NotPanics(t, func() {
		k := oidcClientKind()
		assert.Equal(t, runtime.CapabilityPrime, k.Capability.Tier)
		assert.Equal(t, "oidc-provider", k.Capability.Feature)
	})
}
