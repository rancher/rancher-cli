package provider

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
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
			if !errors.As(mapPreflightErr(tt.err), &ce) {
				t.Fatalf("mapPreflightErr(%v) did not return *clierr.Error", tt.err)
			}
			if ce.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tt.wantCode)
			}
			if ce.Retryable != tt.wantRetryable {
				t.Errorf("retryable = %v, want %v", ce.Retryable, tt.wantRetryable)
			}
		})
	}
}
