package catalog

import (
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCatalogClassifiersHandleErrorsOfAFixedTextClient asserts the catalog's
// handling of the two action error types, postAction, refused and
// outcomeUnknown, shows the server's text with the values hidden when the
// error's Err holds only the status and a fixed text, as the Norman cloud
// credential client builds it: the server's text is read from Body, and the
// value is shown nowhere.
func TestCatalogClassifiersHandleErrorsOfAFixedTextClient(t *testing.T) {
	t.Parallel()
	values := canaryValues(t)
	body := []byte(`{"type":"error","code":"MissingRequired","message":"field ` + canary + ` is missing"}`)
	request := &clierr.Request{Server: "https://rancher.test"}
	tgt := actionTarget{release: testRelease, namespace: testNS, typedCluster: testTypedCluster}

	t.Run("a refused answer", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		refused := &engine.ActionRefused{
			Err:     clierr.FromResponse("POST /v3/x on the Rancher server", http.MethodPost, http.StatusUnprocessableEntity, nil, request),
			Body:    body,
			Request: request,
		}
		f.postAnswer = func(string) ([]byte, error) { return nil, refused }
		_, err := postAction(t.Context(), f, "/v3/x", map[string]any{}, tgt, values)
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "field <hidden> is missing")
		assert.NotContains(t, chainText(err), canary)
	})

	t.Run("an answer whose outcome is unknown", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		unknown := &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, http.StatusInternalServerError, nil), Body: body}
		f.postAnswer = func(string) ([]byte, error) { return nil, unknown }
		_, err := postAction(t.Context(), f, "/v3/x", map[string]any{}, tgt, values)
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.Contains(t, ce.Message, "field <hidden> is missing")
		assert.NotContains(t, chainText(err), canary)
	})

	t.Run("refused and outcomeUnknown called directly", func(t *testing.T) {
		t.Parallel()
		r := &engine.ActionRefused{
			Err:     clierr.FromResponse("POST /v3/x on the Rancher server", http.MethodPost, http.StatusForbidden, nil, request),
			Body:    body,
			Request: request,
		}
		require.NotContains(t, chainText(refused("/v3/x", r, values)), canary)
		u := &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, http.StatusBadGateway, nil), Body: body}
		require.NotContains(t, chainText(outcomeUnknown(tgt, u, values)), canary)
	})
}
