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
