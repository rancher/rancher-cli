package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

// Round-2 repro: Preflight (behind `auth check`) used to rewrap every
// RESTConfig error as AUTH, so `auth check` reported a different code/exit
// than every other command for the identical missing-context/missing-token
// state. RESTConfig's own typed errors must pass through unchanged.
func TestPreflightPassesThroughRESTConfigErrors(t *testing.T) {
	t.Parallel()
	t.Run("no context and no RANCHER_CLI_URL stays CONTEXT_NOT_SET", func(t *testing.T) {
		t.Parallel()
		_, err := Preflight(context.Background(), config.Effective{})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
		assert.Equal(t, config.LoginOrEnvHint, ce.Hint)
	})
	t.Run("URL resolved but no token stays AUTH with the login-or-env hint", func(t *testing.T) {
		t.Parallel()
		_, err := Preflight(context.Background(), config.Effective{URL: "https://r.ex"})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, config.LoginOrEnvHint, ce.Hint)
	})
}

func TestDiscoveryConfigBoundsTimeout(t *testing.T) {
	t.Parallel()
	in := &rest.Config{Host: "https://r.ex", BearerToken: "tok"}
	out := discoveryConfig(in)
	assert.Equal(t, requestTimeout, out.Timeout, "discovery config must carry the request timeout")
	assert.Equal(t, "https://r.ex", out.Host, "other fields must be preserved")
	assert.Zero(t, in.Timeout, "input config must not be mutated")
}

func TestMapPreflightErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantRetryable bool
	}{
		{
			name:     "unauthorized maps to AUTH",
			err:      apierrors.NewUnauthorized("bad token"),
			wantCode: clierr.CodeAuth,
		},
		{
			name:          "unreachable maps to GENERIC retryable",
			err:           errors.New("dial tcp: timeout"),
			wantCode:      clierr.CodeGeneric,
			wantRetryable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var ce *clierr.Error
			require.ErrorAs(t, mapPreflightErr(tt.err), &ce)
			assert.Equal(t, tt.wantCode, ce.Code)
			assert.Equal(t, tt.wantRetryable, ce.Retryable)
		})
	}
}
