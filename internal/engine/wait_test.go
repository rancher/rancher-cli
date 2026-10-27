package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestWaitShouldStop asserts the rule every poll wait applies to a GET
// error: an authentication or certificate failure ends the wait, in a bare
// or a wrapped classified error; every other error, and nil, is retried.
func TestWaitShouldStop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"auth", clierr.New(clierr.CodeAuth, "the server rejected the credential"), true},
		{"tls trust", clierr.New(clierr.CodeTLSTrust, "certificate is not trusted"), true},
		{"auth wrapped", fmt.Errorf("get x: %w", clierr.New(clierr.CodeAuth, "rejected")), true},
		{"tls trust wrapped", fmt.Errorf("get x: %w", clierr.New(clierr.CodeTLSTrust, "untrusted")), true},
		{"rbac", clierr.New(clierr.CodeRBAC, "forbidden"), false},
		{"not found", clierr.New(clierr.CodeNotFound, "gone"), false},
		{"retryable generic", clierr.Retryable(clierr.CodeGeneric, "server error (500)"), false},
		{"rate limited", clierr.Retryable(clierr.CodeGeneric, "too many requests (429)"), false},
		{"deadline", clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded"), false},
		{"raw", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, WaitShouldStop(tt.err))
		})
	}
}
