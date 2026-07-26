package provisioning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeCRTObjectIsPolicyNeutral proves the decoder returns values
// exactly as decoded — including a still-unformatted "{token}"
// placeholder — and never errors on one: detection is PlaceholderFields'
// job, and deciding what to do about it is the caller's (IC-4a).
func TestDecodeCRTObjectIsPolicyNeutral(t *testing.T) {
	t.Parallel()
	status := splicedStatus("tok-aaa")
	status["nodeCommand"] = placeholderToken
	body, err := json.Marshal(crtObject("c-aaa", "default-token", status))
	require.NoError(t, err)

	obj, err := DecodeCRTObject("/v1/x", body)
	require.NoError(t, err, "a placeholder-bearing object must DECODE fine; policy lives in the caller")
	assert.Equal(t, "c-aaa", obj.Namespace)
	assert.Equal(t, "default-token", obj.Name)
	assert.Equal(t, "c-aaa", obj.ClusterName)
	assert.Equal(t, placeholderToken, obj.NodeCommand, "the decoded value is returned verbatim")
	assert.Equal(t, "tok-aaa", obj.Token)
	assert.Contains(t, obj.Command, "tok-aaa")
	assert.Equal(t, "2099-01-01T00:00:00Z", obj.ExpiresAt)
}

// TestDecodeCRTObjectMalformedBody pins the typed decode error: never a
// panic, never a zero CRTObject standing in for a failure.
func TestDecodeCRTObjectMalformedBody(t *testing.T) {
	t.Parallel()
	_, err := DecodeCRTObject("/v1/broken", []byte("not json{"))
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "decoding")
	assert.Contains(t, ce.Message, "/v1/broken")
}

// TestDecodeCRTObjectWrongShape pins the STRUCTURAL invariant: a body
// that is valid JSON but not a Steve clusterregistrationtoken object —
// missing metadata.namespace, metadata.name, or the status object — is
// the same typed decode error as malformed JSON, never a silently-empty
// CRTObject. Without this, FetchDefaultCRT would hand inspect cluster a
// successful-looking empty registration read and the IC-8 warning would
// be skipped.
func TestDecodeCRTObjectWrongShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"unrelated object", `{"kind":"Status","apiVersion":"v1","code":403}`},
		{"missing status", `{"id":"c-aaa/default-token","metadata":{"namespace":"c-aaa","name":"default-token"},"spec":{"clusterName":"c-aaa"}}`},
		{"missing metadata name", `{"metadata":{"namespace":"c-aaa"},"status":{}}`},
		{"missing metadata namespace", `{"metadata":{"name":"default-token"},"status":{}}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeCRTObject("/v1/shape", []byte(tc.body))
			require.Error(t, err, "valid JSON of the wrong shape must be a decode error")
			ce, ok := err.(*clierr.Error)
			require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Contains(t, ce.Message, "decoding")
			assert.Contains(t, ce.Message, "/v1/shape")
		})
	}

	t.Run("status may be empty but must be PRESENT", func(t *testing.T) {
		t.Parallel()
		// A CRT whose controller has not filled status yet still carries the
		// status object; empty FIELDS are fine — absence of the object is not.
		obj, err := DecodeCRTObject("/v1/shape",
			[]byte(`{"metadata":{"namespace":"c-aaa","name":"default-token"},"spec":{"clusterName":"c-aaa"},"status":{}}`))
		require.NoError(t, err)
		assert.Equal(t, "c-aaa", obj.Namespace)
		assert.Equal(t, "", obj.NodeCommand)
	})
}

// TestDecodeCRTCollectionWrongShapeElement pins the same invariant per
// collection element: one structurally invalid object fails the whole
// decode with a typed error naming the path — consistent with the
// existing malformed-body contract (decode failures abort; only the
// PLACEHOLDER policy degrades per row).
func TestDecodeCRTCollectionWrongShapeElement(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{"data": []map[string]any{
		crtObject("c-aaa", "default-token", splicedStatus("tok-aaa")),
		{"metadata": map[string]any{"namespace": "c-bbb"}}, // no name, no status
	}})
	require.NoError(t, err)

	_, derr := DecodeCRTCollection(crtCollectionPath, body)
	require.Error(t, derr)
	ce, ok := derr.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", derr)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "decoding")
	assert.Contains(t, ce.Message, crtCollectionPath)
}

// TestDecodeCRTCollectionEnvelopeInvariant pins the collection ENVELOPE's
// own shape rule: {"data":[]} is a VALID empty collection, while a
// missing, null, or non-array data field is the typed wrong-shape error —
// a bare {} or an error payload must never decode as a successful empty
// list.
func TestDecodeCRTCollectionEnvelopeInvariant(t *testing.T) {
	t.Parallel()

	t.Run("empty data array stays valid", func(t *testing.T) {
		t.Parallel()
		objs, err := DecodeCRTCollection("/v1/coll", []byte(`{"data":[]}`))
		require.NoError(t, err, `{"data":[]} is a legitimate empty collection`)
		assert.NotNil(t, objs)
		assert.Empty(t, objs)
	})

	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"unrelated object", `{"kind":"Status","apiVersion":"v1","code":403}`},
		{"null data", `{"data":null}`},
		{"string data", `{"data":"x"}`},
		{"object data", `{"data":{}}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeCRTCollection("/v1/coll", []byte(tc.body))
			require.Error(t, err, "an envelope without a data ARRAY must be a decode error")
			ce, ok := err.(*clierr.Error)
			require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Contains(t, ce.Message, "decoding")
			assert.Contains(t, ce.Message, "/v1/coll")
		})
	}
}

// TestDecodeCRTCollectionDecodesEveryObject pins the collection envelope
// decode ({"data": [...]}) over the same flat CRTObject view.
func TestDecodeCRTCollectionDecodesEveryObject(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{"data": []map[string]any{
		crtObject("c-aaa", "default-token", splicedStatus("tok-aaa")),
		crtObject("c-bbb", "default-token", splicedStatus("tok-bbb")),
	}})
	require.NoError(t, err)

	objs, err := DecodeCRTCollection(crtCollectionPath, body)
	require.NoError(t, err)
	require.Len(t, objs, 2)
	assert.Equal(t, "c-aaa", objs[0].Namespace)
	assert.Equal(t, "tok-bbb", objs[1].Token)
}

// TestPlaceholderFieldsReportsEachProtectedField is IC-4a's reporter
// contract, table-driven over the seven formatter-protected fields: the
// reporter names exactly the placeholder-bearing field(s) and decides
// nothing. Token is deliberately never reported — it is the splice
// SOURCE, not a template the formatter substitutes into.
func TestPlaceholderFieldsReportsEachProtectedField(t *testing.T) {
	t.Parallel()
	fields := []string{
		"command",
		"insecureCommand",
		"nodeCommand",
		"insecureNodeCommand",
		"windowsNodeCommand",
		"insecureWindowsNodeCommand",
		"manifestUrl",
	}
	for _, field := range fields {
		field := field
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			status := splicedStatus("tok-aaa")
			status[field] = placeholderToken
			body, err := json.Marshal(crtObject("c-aaa", "default-token", status))
			require.NoError(t, err)
			obj, err := DecodeCRTObject("/v1/x", body)
			require.NoError(t, err)
			assert.Equal(t, []string{field}, PlaceholderFields(obj))
		})
	}

	t.Run("clean object reports nothing", func(t *testing.T) {
		t.Parallel()
		body, err := json.Marshal(crtObject("c-aaa", "default-token", splicedStatus("tok-aaa")))
		require.NoError(t, err)
		obj, err := DecodeCRTObject("/v1/x", body)
		require.NoError(t, err)
		assert.Empty(t, PlaceholderFields(obj))
	})

	t.Run("token field is never reported", func(t *testing.T) {
		t.Parallel()
		status := splicedStatus("tok-aaa")
		status["token"] = placeholderToken
		body, err := json.Marshal(crtObject("c-aaa", "default-token", status))
		require.NoError(t, err)
		obj, err := DecodeCRTObject("/v1/x", body)
		require.NoError(t, err)
		assert.Empty(t, PlaceholderFields(obj), "Token is the substitution source, not a protected template field")
	})
}

// TestFetchDefaultCRTPathAndDecode pins the narrow default-token fetch:
// exactly "<crtCollectionPath>/<management-cluster-id>/default-token" on
// the same Steve path the get override reads, decoded through the shared
// decoder.
func TestFetchDefaultCRTPathAndDecode(t *testing.T) {
	t.Parallel()
	obj := crtObject("c-abc", "default-token", splicedStatus("tok-abc"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, crtCollectionPath+"/c-abc/default-token", r.URL.Path)
		writeJSONFixture(t, w, obj)
	}))
	defer srv.Close()

	got, err := FetchDefaultCRT(context.Background(), fakeRootClient{srv: srv}, "c-abc")
	require.NoError(t, err)
	assert.Equal(t, "c-abc", got.Namespace)
	assert.Equal(t, "default-token", got.Name)
	assert.Equal(t, "tok-abc", got.Token)
	assert.Contains(t, got.NodeCommand, "tok-abc")
}

// TestFetchDefaultCRTPropagatesRootError pins that a transport/RBAC/404
// failure surfaces as the RootClient's own typed error — inspect cluster
// degrades it to a warning (IC-8), which needs the error intact.
func TestFetchDefaultCRTPropagatesRootError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := FetchDefaultCRT(context.Background(), fakeRootClient{srv: srv}, "c-abc")
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}
