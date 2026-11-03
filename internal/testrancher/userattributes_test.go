package testrancher_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
)

const (
	uaNone    = "none:secret"
	uaGet     = "get:secret"
	uaGetList = "admin:secret"

	uaItemPath   = testrancher.UserAttributesCollectionPath + "/u-abc"
	uaSchema     = "/v1/schemas/management.cattle.io.userattribute"
	uaSchemaPlur = "/v1/schemas/management.cattle.io.userattributes"
	uaReview     = "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
)

func uaOptions() testrancher.Options {
	return testrancher.Options{
		UserAttributeAccess: func(bearer string) (testrancher.UserAttributeLevel, bool) {
			switch bearer {
			case uaNone:
				return testrancher.UserAttributesNone, true
			case uaGet:
				return testrancher.UserAttributesGet, true
			case uaGetList:
				return testrancher.UserAttributesGetList, true
			}
			return 0, false
		},
		UserAttributeObjects: func() []map[string]any {
			return []map[string]any{
				{"apiVersion": "management.cattle.io/v3", "kind": "UserAttribute", "metadata": map[string]any{"name": "u-def"}},
				{"apiVersion": "management.cattle.io/v3", "kind": "UserAttribute", "metadata": map[string]any{"name": "u-abc"}, "GroupPrincipals": map[string]any{}},
			}
		},
	}
}

func uaSelfReview(t *testing.T, srv *httptest.Server, url, bearer string, resource string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]any{"verb": "list", "group": "management.cattle.io", "resource": resource}}}
	resp, raw := doJSON(t, srv.Client(), http.MethodPost, url, body, bearer)
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		out = nil
	}
	return resp.StatusCode, out
}

// TestUserAttributeRoutesPerBearer asserts what each level of access gets
// from the steve routes of UserAttributes: nothing served without a right,
// the object and an empty collection with get, every object with get and
// list, and 401 for a bearer the facade does not know.
func TestUserAttributeRoutesPerBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, uaOptions(), &recordingHandler{})
	for _, path := range []string{uaItemPath, testrancher.UserAttributesCollectionPath, uaSchema, uaSchemaPlur} {
		resp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+path, nil, "nobody:secret")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unknown bearer: "+path)

		resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+path, nil, uaNone)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no right: "+path)
		assert.Empty(t, body, "no right: "+path)
	}

	for _, bearer := range []string{uaGet, uaGetList} {
		status, item := getDecoded(t, srv, uaItemPath, bearer)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "u-abc", item["id"])
		assert.Equal(t, "management.cattle.io.userattribute", item["type"])
		assert.Equal(t, map[string]any{}, item["links"])
		assert.Equal(t, "active", stateName(t, item))

		status, missing := getDecoded(t, srv, testrancher.UserAttributesCollectionPath+"/u-none", bearer)
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, map[string]any{
			"type": "error", "links": map[string]any{}, "code": "NotFound", "status": float64(404),
			"message": `userattributes.management.cattle.io "u-none" not found`,
		}, missing)

		for _, path := range []string{uaSchema, uaSchemaPlur} {
			status, schema := getDecoded(t, srv, path, bearer)
			assert.Equal(t, http.StatusOK, status)
			assert.Equal(t, []any{"GET"}, schema["resourceMethods"])
			assert.Equal(t, []any{"GET"}, schema["collectionMethods"])
		}
	}

	status, coll := getDecoded(t, srv, testrancher.UserAttributesCollectionPath, uaGet)
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, projectItems(t, coll), "get without list sees no item in the collection")

	status, coll = getDecoded(t, srv, testrancher.UserAttributesCollectionPath, uaGetList)
	require.Equal(t, http.StatusOK, status)
	var ids []any
	for _, item := range projectItems(t, coll) {
		ids = append(ids, item["id"])
	}
	assert.Equal(t, []any{"u-abc", "u-def"}, ids)
}

// TestUserAttributeCollectionIsPaged asserts the full collection is paged
// like the other steve collections.
func TestUserAttributeCollectionIsPaged(t *testing.T) {
	t.Parallel()
	opts := uaOptions()
	opts.ClustersPageSize = 1
	_, srv := newFacade(t, opts, &recordingHandler{})
	status, first := getDecoded(t, srv, testrancher.UserAttributesCollectionPath, uaGetList)
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, projectItems(t, first), 1)
	assert.Equal(t, "1", first["continue"])
	assert.Equal(t, float64(2), first["count"])
}

// TestUserAttributeListReview asserts the SelfSubjectAccessReview for list
// on userattributes: allowed with get and list, denied otherwise, 401 for an
// unknown bearer, and any other review goes to the fallthrough; without the
// option the path falls through as before.
func TestUserAttributeListReview(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, uaOptions(), fall)
	url := srv.URL + uaReview

	for bearer, allowed := range map[string]bool{uaGetList: true, uaGet: false, uaNone: false} {
		status, body := uaSelfReview(t, srv, url, bearer, "userattributes")
		require.Equal(t, http.StatusCreated, status, bearer)
		assert.Equal(t, map[string]any{"allowed": allowed}, body["status"], bearer)
		assert.Equal(t, "SelfSubjectAccessReview", body["kind"])
	}
	status, _ := uaSelfReview(t, srv, url, "nobody:secret", "userattributes")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Empty(t, fall.requests)

	status, _ = uaSelfReview(t, srv, url, uaGetList, "pods")
	assert.Equal(t, http.StatusTeapot, status, "a review about another resource is not the facade's")
	require.Len(t, fall.requests, 1)
	assert.Contains(t, string(fall.bodies[0]), "pods", "the fallthrough still sees the body")

	plain := &recordingHandler{}
	_, off := newFacade(t, testrancher.Options{}, plain)
	status, _ = uaSelfReview(t, off, off.URL+uaReview, uaGetList, "userattributes")
	assert.Equal(t, http.StatusTeapot, status, "without the option the path falls through")

	resp, _ := doJSON(t, off.Client(), http.MethodGet, off.URL+uaItemPath, nil, uaGetList)
	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "without the option the steve route falls through")
}

// TestUserAttributeRoutesIgnoreTheLocalGate asserts the permission is
// independent of LocalManagementGate: a bearer the gate would refuse reads
// through steve.
func TestUserAttributeRoutesIgnoreTheLocalGate(t *testing.T) {
	t.Parallel()
	opts := uaOptions()
	opts.LocalManagementGate = func(string) bool { return false }
	_, srv := newFacade(t, opts, &recordingHandler{})
	status, _ := getDecoded(t, srv, uaItemPath, uaGet)
	assert.Equal(t, http.StatusOK, status)
	status, _ = uaSelfReview(t, srv, srv.URL+uaReview, uaGetList, "userattributes")
	assert.Equal(t, http.StatusCreated, status)
}
