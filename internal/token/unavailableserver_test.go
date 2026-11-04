package token

import (
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestTheOwnerRequestKeepsItsTextOnAnUnavailableServer asserts a server that
// answers the owner request with a 5xx or a 429, with a Retry-After header of
// 1 or 120 seconds or with none, gets a retryable GENERIC error that holds
// the answered status and keeps the text this request has always had, which
// names no server.
func TestTheOwnerRequestKeepsItsTextOnAnUnavailableServer(t *testing.T) {
	t.Parallel()
	for _, status := range append([]int{429}, testtransport.UnavailableStatuses...) {
		for _, retryAfter := range []string{"", "1", "120"} {
			t.Run(strconv.Itoa(status)+" Retry-After "+retryAfter, func(t *testing.T) {
				t.Parallel()
				srv := testtransport.NewAnswers(t, true, status, retryAfter, "application/json", `{"type":"https://developers.cloudflare.com/error-1033/","title":"Error 1033"}`)
				_, err := requestOwner(t.Context(), srv.Client(), srv.URL(), "c-1", "tok:secret")
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Contains(t, ce.Message, "posting a SelfSubjectReview: ")
				assert.NotContains(t, ce.Message, "answered HTTP")
				got, ok := clierr.HTTPStatus(err)
				assert.True(t, ok)
				assert.Equal(t, status, got)
			})
		}
	}
}
