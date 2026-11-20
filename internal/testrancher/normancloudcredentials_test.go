package testrancher

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cloudCredentialToken = "facade-seed-token"

// secretStore is a fallthrough that records the Secret creates it receives.
type secretStore struct {
	mu      sync.Mutex
	posts   []map[string]any
	rawPath []string
}

func (s *secretStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.posts = append(s.posts, body)
	s.rawPath = append(s.rawPath, r.URL.RequestURI())
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func normanRequest(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, payload)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestFacadeNormanCloudCredentialCreate asserts a create is recorded, stored
// as a Secret through the fallthrough with the labels, annotations and
// stringData keys Rancher's store writes, and answered with an id and the
// decoded config; and that the route is absent unless the option is on and
// needs the caller's bearer.
func TestFacadeNormanCloudCredentialCreate(t *testing.T) {
	t.Parallel()
	store := &secretStore{}
	f := New(Options{SeedToken: cloudCredentialToken, Fallthrough: store})
	srv := httptest.NewServer(f)
	defer srv.Close()
	body := map[string]any{
		"type": "cloudCredential", "name": "prod", "description": "d",
		"annotations":               map[string]any{"provisioning.cattle.io/driver": "aws"},
		"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKE", "secretKey": "fake-secret-value-1"},
	}

	status, _ := normanRequest(t, srv, http.MethodPost, "/v3/cloudcredentials", "", body)
	assert.Equal(t, http.StatusUnauthorized, status)

	status, answer := normanRequest(t, srv, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, body)
	require.Equal(t, http.StatusCreated, status)
	id, _ := answer["id"].(string)
	assert.True(t, strings.HasPrefix(id, "cattle-global-data:cc-"), id)
	assert.Equal(t, "fake-secret-value-1", answer["amazonec2credentialConfig"].(map[string]any)["secretKey"], "Norman's answer decodes the config")

	records := f.NormanCloudCredentialRequests()
	require.Len(t, records, 1, "a request with no bearer is refused before it is recorded")
	assert.Equal(t, cloudCredentialToken, records[0].Bearer)
	assert.Equal(t, "prod", records[0].Body["name"])
	require.Len(t, store.posts, 1)
	secret := store.posts[0]
	metadata := secret["metadata"].(map[string]any)
	assert.Equal(t, "cattle-global-data", metadata["namespace"])
	assert.True(t, strings.HasPrefix(metadata["name"].(string), "cc-"))
	assert.Equal(t, "norman", metadata["labels"].(map[string]any)["cattle.io/creator"])
	annotations := metadata["annotations"].(map[string]any)
	assert.Equal(t, "prod", annotations["field.cattle.io/name"])
	assert.Equal(t, "aws", annotations["provisioning.cattle.io/driver"])
	assert.Equal(t, "fake-secret-value-1", secret["stringData"].(map[string]any)["amazonec2credentialConfig-secretKey"])
	assert.Contains(t, store.rawPath[0], "fieldManager=cli.rancher.io")

	// With no fallthrough the Secret is not stored and the create is still
	// answered.
	bare := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken}))
	defer bare.Close()
	status, _ = normanRequest(t, bare, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, body)
	assert.Equal(t, http.StatusCreated, status)
}

// TestFacadeNormanCloudCredentialRefusals asserts a body with no config object
// is refused with MissingRequired, a Harvester kubeconfig is checked as
// Rancher's store checks it (a 500 with the store's prefix), a delete of an
// id listed as in use is refused with 422, and update and delete answer.
func TestFacadeNormanCloudCredentialRefusals(t *testing.T) {
	t.Parallel()
	store := &secretStore{}
	f := New(Options{SeedToken: cloudCredentialToken, Fallthrough: store, NormanCloudCredentialsInUse: []string{"cattle-global-data:cc-used"}})
	srv := httptest.NewServer(f)
	defer srv.Close()

	status, answer := normanRequest(t, srv, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, map[string]any{"type": "cloudCredential", "name": "x"})
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.Equal(t, "MissingRequired", answer["code"])

	for content, want := range map[string]string{
		"not a kubeconfig": "failed to process harvester cloud credential: failed to read the tokens from kubeconfig:",
		"users:\n- user:\n    token: kubeconfig-user-expired:x": "token kubeconfig-user-expired is expired",
		"users:\n- user:\n    token: kubeconfig-user-used:x":    "is already in use by secret",
	} {
		status, answer = normanRequest(t, srv, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, map[string]any{
			"type": "cloudCredential", "name": "h", "harvestercredentialConfig": map[string]any{"clusterId": "c", "kubeconfigContent": content}})
		assert.Equal(t, http.StatusInternalServerError, status, content)
		assert.Contains(t, answer["message"], want, content)
	}
	assert.Empty(t, store.posts, "a refused create stores nothing")

	status, _ = normanRequest(t, srv, http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-x", cloudCredentialToken,
		map[string]any{"amazonec2credentialConfig": map[string]any{"accessKey": "k"}})
	assert.Equal(t, http.StatusOK, status)
	status, _ = normanRequest(t, srv, http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-x", cloudCredentialToken, map[string]any{"name": "n"})
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = normanRequest(t, srv, http.MethodDelete, "/v3/cloudCredentials/cattle-global-data:cc-x", cloudCredentialToken, nil)
	assert.Equal(t, http.StatusNoContent, status)
	status, answer = normanRequest(t, srv, http.MethodDelete, "/v3/cloudCredentials/cattle-global-data:cc-used", cloudCredentialToken, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.Equal(t, "InvalidAction", answer["code"])
}
