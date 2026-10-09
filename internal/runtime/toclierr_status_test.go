package runtime

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestToClierrClassifiesAServerStatus asserts that a server status which
// reaches the pipeline unclassified gets its code from the status table,
// keeping the text the error adds in front of it, and stays readable;
// nothing is known about the call there, so no remedy is named.
func TestToClierrClassifiesAServerStatus(t *testing.T) {
	t.Parallel()
	wrap := func(status int, body string) error {
		return fmt.Errorf("discovering version for group %q: %w", "ext.cattle.io",
			clierr.ResponseError(http.MethodGet, status, []byte(body)))
	}
	for _, tc := range []struct {
		name      string
		err       error
		code      string
		message   string
		hint      string
		retryable bool
	}{
		{"401 no body", wrap(http.StatusUnauthorized, ""), clierr.CodeAuth,
			`discovering version for group "ext.cattle.io": the server rejected the credential (401)`, "", false},
		{"403 no body", wrap(http.StatusForbidden, ""), clierr.CodeRBAC,
			`discovering version for group "ext.cattle.io": the server refused the request (403) and gave no reason`, "", false},
		{"404", wrap(http.StatusNotFound, ""), clierr.CodeNotFound,
			`discovering version for group "ext.cattle.io": the server could not find the requested resource`, "", false},
		{"503", wrap(http.StatusServiceUnavailable, ""), clierr.CodeGeneric,
			`discovering version for group "ext.cattle.io": the server is currently unable to handle the request - retrying may succeed`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := toClierr(t.Context(), tc.err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, tc.retryable, ce.Retryable)
			status, ok := clierr.HTTPStatus(ce)
			assert.True(t, ok)
			assert.NotZero(t, status)
		})
	}
}
