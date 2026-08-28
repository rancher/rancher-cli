package cli

import (
	"reflect"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oidcOnceOnlyInventory is the reviewed, closed-world set of commands that
// declare Delivery: DeliveryOnceOnly: a REVIEWED mapping, never automatic
// inference of a future secret-bearing command. An entry may be listed here
// before its command is registered —
// TestDeliveryOnceOnlyClosedWorldConformance asserts only over commands the
// registry actually has, so an early entry activates automatically the day
// its command registers, with no test change.
var oidcOnceOnlyInventory = map[string]bool{
	"create token":              true,
	"create kubeconfig":         true,
	"create oidc-client":        true,
	"oidc-client add-secret":    true,
	"oidc-client rotate-secret": true,
}

// zeroOutputPolicyExempt is the closed, named allowlist of once-only
// commands permitted a zero-value OutputPolicy: exactly create
// kubeconfig, whose RawBytes payload (internal/engine/extres/kubeconfig.go)
// is format-independent and needs no restriction. Every other once-only
// descriptor must equal runtime.OnceOnlyOutputPolicy() exactly — a silent
// zero-value anywhere else would allow -o name and reintroduce the hazard
// the once-only OutputPolicy exists to prevent.
var zeroOutputPolicyExempt = map[string]bool{
	"create kubeconfig": true,
}

// onceOnlyOutputPolicyOK reports whether a once-only descriptor's
// OutputPolicy passes conformance: exactly runtime.OnceOnlyOutputPolicy(),
// or the zero value if and only if name is in zeroOutputPolicyExempt.
func onceOnlyOutputPolicyOK(name string, p runtime.OutputPolicy) bool {
	if reflect.DeepEqual(p, runtime.OnceOnlyOutputPolicy()) {
		return true
	}
	return len(p.AllowedFormats) == 0 && zeroOutputPolicyExempt[name]
}

// TestOnceOnlyOutputPolicyOKRejectsZeroValueOutsideTheNamedException pins
// the fix's core claim: a zero-value OutputPolicy is accepted ONLY for the
// named exception, never universally — a future once-only command that
// forgets OutputPolicy: runtime.OnceOnlyOutputPolicy() must fail this
// check, not pass it by accident.
func TestOnceOnlyOutputPolicyOKRejectsZeroValueOutsideTheNamedException(t *testing.T) {
	t.Parallel()
	assert.True(t, onceOnlyOutputPolicyOK("create kubeconfig", runtime.OutputPolicy{}),
		"the named exception must still pass with a zero-value OutputPolicy")
	assert.True(t, onceOnlyOutputPolicyOK("create token", runtime.OnceOnlyOutputPolicy()),
		"the canonical policy must pass for any once-only command")
	assert.False(t, onceOnlyOutputPolicyOK("create oidc-client", runtime.OutputPolicy{}),
		"a zero-value OutputPolicy must be rejected for a command outside the named exception")
}

// TestDeliveryOnceOnlyClosedWorldConformance pins the once-only marker
// bidirectionally over the real, registered command set: every registered
// inventory entry must declare Delivery: DeliveryOnceOnly, and every
// registered command that declares it must be a reviewed inventory entry —
// so a future secret-bearing command cannot silently opt out of the safety
// seam, and no command can silently opt in without review.
func TestDeliveryOnceOnlyClosedWorldConformance(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		inInventory := oidcOnceOnlyInventory[d.Name]
		isOnceOnly := d.Delivery == runtime.DeliveryOnceOnly
		if inInventory {
			assert.True(t, isOnceOnly,
				"reviewed once-only inventory entry %q must declare Delivery: DeliveryOnceOnly", d.Name)
		}
		if isOnceOnly {
			assert.True(t, inInventory,
				"command %q declares Delivery: DeliveryOnceOnly but is not in the reviewed once-only inventory — this is a closed-world mapping, add it deliberately", d.Name)
			assert.True(t, onceOnlyOutputPolicyOK(d.Name, d.OutputPolicy),
				"command %q declares an OutputPolicy that is neither runtime.OnceOnlyOutputPolicy() nor the named zero-value exception (create kubeconfig only)", d.Name)
		}
	}
}
