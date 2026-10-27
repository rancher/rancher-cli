package engine

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestCommittedPartialSuccessKeepsWrappedCode asserts a wrapped classified
// error keeps its code and the text its wrapper adds.
func TestCommittedPartialSuccessKeepsWrappedCode(t *testing.T) {
	t.Parallel()
	d := clierr.CommittedContextDetails{Committed: true, ResourceKind: "Token", ResourceName: "t"}
	got := CommittedPartialSuccess(fmt.Errorf("x: %w", clierr.New(clierr.CodeRBAC, "no")), d)
	assert.Equal(t, clierr.CodeRBAC, got.Code)
	assert.Equal(t, "x: no", got.Message)
}

// TestCommittedPartialSuccessAddsNoFactsWithoutCertificateDetails asserts a
// TLS_TRUST error adds nothing to its message when its details hold no
// certificate lines: no details at all, or committed details from an
// earlier wrap.
func TestCommittedPartialSuccessAddsNoFactsWithoutCertificateDetails(t *testing.T) {
	t.Parallel()
	d := clierr.CommittedContextDetails{Committed: true, ResourceKind: "OIDCClient", ResourceName: "c", Recovery: "r"}
	for name, in := range map[string]*clierr.Error{
		"no details":        clierr.New(clierr.CodeTLSTrust, "untrusted"),
		"committed details": clierr.New(clierr.CodeTLSTrust, "untrusted").WithDetails(d),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := CommittedPartialSuccess(in, d)
			assert.Equal(t, clierr.CodeTLSTrust, got.Code)
			assert.Equal(t, "untrusted", got.Message)
			assert.Equal(t, d, got.Details)
		})
	}
}
