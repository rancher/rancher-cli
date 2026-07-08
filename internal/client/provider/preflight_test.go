package provider

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

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
