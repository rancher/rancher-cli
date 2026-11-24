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
// stringData keys Rancher's store writes, and responded to with an id and the
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
	assert.Equal(t, "fake-secret-value-1", answer["amazonec2credentialConfig"].(map[string]any)["secretKey"], "Norman's response decodes the config")

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

	// With no fallthrough the Secret is not stored and the create still
	// gets a response.
	bare := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken}))
	defer bare.Close()
	status, _ = normanRequest(t, bare, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, body)
	assert.Equal(t, http.StatusCreated, status)
}

// TestFacadeNormanCloudCredentialStoreRefusal asserts a create whose Secret
// the store refuses gets a response of 500, not 201.
func TestFacadeNormanCloudCredentialStoreRefusal(t *testing.T) {
	t.Parallel()
	refusing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	srv := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken, Fallthrough: refusing}))
	defer srv.Close()
	body := map[string]any{
		"type": "cloudCredential", "name": "prod",
		"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKE", "secretKey": "fake-secret-value-1"},
	}
	status, _ := normanRequest(t, srv, http.MethodPost, "/v3/cloudcredentials", cloudCredentialToken, body)
	assert.Equal(t, http.StatusInternalServerError, status)
}

// TestFacadeNormanCloudCredentialRefusals asserts a body with no config object
// is refused with MissingRequired and a Harvester kubeconfig is checked as
// Rancher's store checks it (a 500 with the store's prefix).
func TestFacadeNormanCloudCredentialRefusals(t *testing.T) {
	t.Parallel()
	store := &secretStore{}
	f := New(Options{SeedToken: cloudCredentialToken, Fallthrough: store})
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
}

// requestStore is a fallthrough that records every request it receives with
// its method, request URI, content type and decoded body, and responds with status.
type requestStore struct {
	mu       sync.Mutex
	status   int
	requests []storedRequest
}

type storedRequest struct {
	method, uri, contentType string
	body                     map[string]any
}

func (s *requestStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.requests = append(s.requests, storedRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), body})
	s.mu.Unlock()
	w.WriteHeader(s.status)
}

// TestFacadeNormanCloudCredentialUpdateStores asserts an update accepts a
// config object that is present and empty, merges the given config fields, the
// name and description annotations and the given annotations into the stored
// Secret through the fallthrough with the field manager, leaves out what the
// body does not give, and gets 200; and that with no fallthrough it
// responds with 200 without storing.
func TestFacadeNormanCloudCredentialUpdateStores(t *testing.T) {
	t.Parallel()
	const path = "/v3/cloudCredentials/cattle-global-data:cc-abcde"
	store := &requestStore{status: http.StatusOK}
	srv := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken, Fallthrough: store}))
	defer srv.Close()

	status, answer := normanRequest(t, srv, http.MethodPut, path, cloudCredentialToken, map[string]any{
		"description":               "new",
		"annotations":               map[string]any{"e2e/keep": "kept"},
		"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKE"},
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "cattle-global-data:cc-abcde", answer["id"])
	require.Len(t, store.requests, 1)
	got := store.requests[0]
	assert.Equal(t, http.MethodPatch, got.method)
	assert.Equal(t, "application/merge-patch+json", got.contentType)
	assert.Contains(t, got.uri, "/namespaces/cattle-global-data/secrets/cc-abcde?")
	assert.Contains(t, got.uri, "fieldManager=cli.rancher.io")
	annotations := got.body["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, map[string]any{"field.cattle.io/description": "new", "e2e/keep": "kept"}, annotations)
	assert.Equal(t, map[string]any{"amazonec2credentialConfig-accessKey": "AKIAFAKE"}, got.body["stringData"])

	status, _ = normanRequest(t, srv, http.MethodPut, path, cloudCredentialToken, map[string]any{
		"name": "renamed", "amazonec2credentialConfig": map[string]any{}})
	require.Equal(t, http.StatusOK, status, "a config object that is present and empty passes")
	require.Len(t, store.requests, 2)
	assert.Equal(t, map[string]any{}, store.requests[1].body["stringData"])
	assert.Equal(t, map[string]any{"field.cattle.io/name": "renamed"},
		store.requests[1].body["metadata"].(map[string]any)["annotations"])

	bare := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken}))
	defer bare.Close()
	status, _ = normanRequest(t, bare, http.MethodPut, path, cloudCredentialToken, map[string]any{"amazonec2credentialConfig": map[string]any{}})
	assert.Equal(t, http.StatusOK, status)
	status, _ = normanRequest(t, bare, http.MethodDelete, path, cloudCredentialToken, nil)
	assert.Equal(t, http.StatusNoContent, status)
}

// TestFacadeNormanCloudCredentialOneValidator asserts create and update run one
// validator: a body with no config object is refused with MissingRequired by
// both, and a config object that is present and empty passes both.
func TestFacadeNormanCloudCredentialOneValidator(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken, Fallthrough: &requestStore{status: http.StatusCreated}}))
	defer srv.Close()
	for _, tc := range []struct {
		name         string
		method, path string
		body         map[string]any
		want         int
	}{
		{"create with no config object", http.MethodPost, "/v3/cloudcredentials", map[string]any{"type": "cloudCredential", "name": "x"}, http.StatusUnprocessableEntity},
		{"create with an empty config object", http.MethodPost, "/v3/cloudcredentials", map[string]any{"type": "cloudCredential", "name": "x", "s3credentialConfig": map[string]any{}}, http.StatusCreated},
		{"update with no config object", http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-abcde", map[string]any{"name": "x"}, http.StatusUnprocessableEntity},
		{"update with an empty config object", http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-abcde", map[string]any{"s3credentialConfig": map[string]any{}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, answer := normanRequest(t, srv, tc.method, tc.path, cloudCredentialToken, tc.body)
			assert.Equal(t, tc.want, status)
			if tc.want == http.StatusUnprocessableEntity {
				assert.Equal(t, "MissingRequired", answer["code"])
			}
		})
	}
}

// TestFacadeNormanCloudCredentialUpdateHarvester asserts an update of a
// Harvester credential runs the kubeconfig check, and refuses a kubeconfig that
// names a token the credential already has as Rancher does, with a 500 that
// stores nothing.
func TestFacadeNormanCloudCredentialUpdateHarvester(t *testing.T) {
	t.Parallel()
	store := &requestStore{status: http.StatusOK}
	srv := httptest.NewServer(New(Options{SeedToken: cloudCredentialToken, Fallthrough: store}))
	defer srv.Close()
	for content, want := range map[string]string{
		"users:\n- user:\n    token: kubeconfig-user-same:x":    "token kubeconfig-user-same is already in use by secret",
		"users:\n- user:\n    token: kubeconfig-user-expired:x": "token kubeconfig-user-expired is expired",
	} {
		status, answer := normanRequest(t, srv, http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-abcde", cloudCredentialToken,
			map[string]any{"harvestercredentialConfig": map[string]any{"kubeconfigContent": content}})
		assert.Equal(t, http.StatusInternalServerError, status, content)
		assert.Equal(t, "failed to process harvester cloud credential: "+want, answer["message"], content)
	}
	assert.Empty(t, store.requests, "a refused update stores nothing")
}

// TestFacadeNormanCloudCredentialDeleteStores asserts a delete deletes the
// Secret through the fallthrough unless the id is listed as in use, and that a
// store that refuses the change gives 500 on both update and delete, with the
// request recorded.
func TestFacadeNormanCloudCredentialDeleteStores(t *testing.T) {
	t.Parallel()
	store := &requestStore{status: http.StatusOK}
	f := New(Options{SeedToken: cloudCredentialToken, Fallthrough: store, NormanCloudCredentialsInUse: []string{"cattle-global-data:cc-used"}})
	srv := httptest.NewServer(f)
	defer srv.Close()

	status, _ := normanRequest(t, srv, http.MethodDelete, "/v3/cloudCredentials/cattle-global-data:cc-abcde", cloudCredentialToken, nil)
	assert.Equal(t, http.StatusNoContent, status)
	require.Len(t, store.requests, 1)
	assert.Equal(t, http.MethodDelete, store.requests[0].method)
	assert.Contains(t, store.requests[0].uri, "/namespaces/cattle-global-data/secrets/cc-abcde")

	status, answer := normanRequest(t, srv, http.MethodDelete, "/v3/cloudCredentials/cattle-global-data:cc-used", cloudCredentialToken, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.Equal(t, "InvalidAction", answer["code"])
	assert.Len(t, store.requests, 1, "a delete refused as in use deletes nothing")

	refusing := &requestStore{status: http.StatusForbidden}
	bad := New(Options{SeedToken: cloudCredentialToken, Fallthrough: refusing})
	badSrv := httptest.NewServer(bad)
	defer badSrv.Close()
	status, answer = normanRequest(t, badSrv, http.MethodDelete, "/v3/cloudCredentials/cattle-global-data:cc-abcde", cloudCredentialToken, nil)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, "the test facade could not store the change", answer["message"])
	status, answer = normanRequest(t, badSrv, http.MethodPut, "/v3/cloudCredentials/cattle-global-data:cc-abcde", cloudCredentialToken,
		map[string]any{"s3credentialConfig": map[string]any{}})
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, "the test facade could not store the change", answer["message"])
	records := bad.NormanCloudCredentialRequests()
	require.Len(t, records, 2)
	assert.Equal(t, http.MethodDelete, records[0].Method)
	assert.Equal(t, http.MethodPut, records[1].Method)
}

// TestFacadeNormanCloudCredentialStoreNotFound asserts that when the store
// responds with 404 because the Secret is gone, an update and a delete respond
// with 404 NotFound as Norman does, and the request is recorded.
func TestFacadeNormanCloudCredentialStoreNotFound(t *testing.T) {
	t.Parallel()
	const path = "/v3/cloudCredentials/cattle-global-data:cc-gone"
	for _, tc := range []struct {
		name, method string
		body         any
	}{
		{"update", http.MethodPut, map[string]any{"s3credentialConfig": map[string]any{}}},
		{"delete", http.MethodDelete, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &requestStore{status: http.StatusNotFound}
			f := New(Options{SeedToken: cloudCredentialToken, Fallthrough: store})
			srv := httptest.NewServer(f)
			defer srv.Close()
			status, answer := normanRequest(t, srv, tc.method, path, cloudCredentialToken, tc.body)
			assert.Equal(t, http.StatusNotFound, status)
			assert.Equal(t, "NotFound", answer["code"])
			assert.Equal(t, "error", answer["type"])
			records := f.NormanCloudCredentialRequests()
			require.Len(t, records, 1)
			assert.Equal(t, tc.method, records[0].Method)
			assert.Len(t, store.requests, 1)
		})
	}
}
