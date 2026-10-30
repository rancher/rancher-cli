package engine

import (
	"reflect"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScrubResultRemovesWhatTheRedactionCovers asserts the result scrub
// removes the Secret payload, the last-applied annotation and a machine
// config's secret field, in any letter case, and keeps every other entry.
func TestScrubResultRemovesWhatTheRedactionCovers(t *testing.T) {
	t.Parallel()
	annotations := func() map[string]any {
		return map[string]any{
			kinds.LastAppliedAnnotation: `{"data":{"k":"s3cretA"}}`,
			"example.invalid/outside":   "outsideA",
		}
	}
	for name, tc := range map[string]struct {
		redaction kinds.Redaction
		obj       map[string]any
		want      map[string]any
	}{
		"secret payload and annotation": {
			redaction: rowRedaction(t, "secret"),
			obj: map[string]any{
				"kind":       "Secret",
				"data":       map[string]any{"k": "czNjcmV0QQ=="},
				"StringData": map[string]any{"k": "s3cretA"},
				"metadata":   map[string]any{"name": "s", "annotations": annotations()},
			},
			want: map[string]any{
				"kind": "Secret",
				"metadata": map[string]any{"name": "s", "annotations": map[string]any{
					"example.invalid/outside": "outsideA",
				}},
			},
		},
		"machine config secret field in another case": {
			redaction: rowRedaction(t, "digitaloceanconfig"),
			obj: map[string]any{
				"sshKeyContents": "s3cretA",
				"SSHKEYCONTENTS": "s3cretB",
				"region":         "nyc3",
				"metadata":       map[string]any{"annotations": annotations()},
			},
			want: map[string]any{
				"region": "nyc3",
				"metadata": map[string]any{"annotations": map[string]any{
					"example.invalid/outside": "outsideA",
				}},
			},
		},
		"row path": {
			redaction: rowRedaction(t, "user"),
			obj: map[string]any{
				"Password": "s3cretA",
				"username": "alice",
			},
			want: map[string]any{"username": "alice"},
		},
		"a kind that hides nothing keeps the annotation": {
			redaction: rowRedaction(t, "namespace"),
			obj: map[string]any{
				"metadata": map[string]any{"annotations": annotations()},
			},
			want: map[string]any{
				"metadata": map[string]any{"annotations": annotations()},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, scrubResult(tc.obj, tc.redaction))
		})
	}
}

// TestScrubResultKeepsNilValuesAndArrays asserts a nil value stays and an
// array is not entered, so a path below an array item is never reached.
func TestScrubResultKeepsNilValuesAndArrays(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"data": nil,
		"items": []any{
			map[string]any{"data": map[string]any{"k": "s3cretA"}},
		},
		"spec": map[string]any{"data": map[string]any{"k": "v"}},
	}
	got := scrubResult(obj, rowRedaction(t, "secret"))
	assert.Equal(t, obj["items"], got["items"], "an array is left alone")
	assert.Contains(t, got, "data", "a nil value stays")
	assert.Nil(t, got["data"])
	assert.Equal(t, obj["spec"], got["spec"], "only a top-level payload is hidden")
}

// TestScrubResultLeavesTheOriginalObjectUnchanged asserts the scrub copies
// every map it changes: the committed object the client returned is never
// modified.
func TestScrubResultLeavesTheOriginalObjectUnchanged(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"data": map[string]any{"k": "czNjcmV0QQ=="},
		"metadata": map[string]any{
			"name":        "s",
			"annotations": map[string]any{kinds.LastAppliedAnnotation: "s3cretA", "keep": "v"},
		},
	}
	before := map[string]any{
		"data": map[string]any{"k": "czNjcmV0QQ=="},
		"metadata": map[string]any{
			"name":        "s",
			"annotations": map[string]any{kinds.LastAppliedAnnotation: "s3cretA", "keep": "v"},
		},
	}
	got := scrubResult(obj, rowRedaction(t, "secret"))
	require.NotContains(t, got, "data")
	assert.Equal(t, before, obj)
}

// TestScrubResultReturnsAnObjectWithNothingToRemoveAsItIs asserts an
// object the redaction does not touch is returned as the same map, not a
// copy: for a redaction that hides something but finds nothing to remove (a
// payload, a path) and for one that hides nothing.
func TestScrubResultReturnsAnObjectWithNothingToRemoveAsItIs(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"spec": map[string]any{"x": "y"}}
	for name, r := range map[string]kinds.Redaction{
		"hides a payload": rowRedaction(t, "secret"),
		"hides nothing":   rowRedaction(t, "namespace"),
		"hides a path":    rowRedaction(t, "user"),
	} {
		got := scrubResult(obj, r)
		assert.Equal(t, obj, got, name)
		assert.Equal(t, reflect.ValueOf(obj).Pointer(), reflect.ValueOf(got).Pointer(), "%s: the same map", name)
	}
}

// TestPlanCreateObservedResultHidesWhatThePreviewHides asserts the create
// executor's result leaves out the committed object's hidden values.
func TestPlanCreateObservedResultHidesWhatThePreviewHides(t *testing.T) {
	t.Parallel()
	r := rowRedaction(t, "secret")
	obj := newObj(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "s", "namespace": "ns"},
		"stringData": map[string]any{"k": "s3cretA"},
	})
	_, _, exec, err := PlanCreateObserved(t.Context(), &obsClient{}, r.GVR(), "ns", obj, r, "c-x", "secret", "Secret", true)
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, data, "stringData")
	assert.Equal(t, "Secret", data["kind"])
}

// TestPlanUpdateObservedResultHidesWhatThePreviewHides asserts the update
// executor's result leaves out the committed object's hidden values.
func TestPlanUpdateObservedResultHidesWhatThePreviewHides(t *testing.T) {
	t.Parallel()
	r := rowRedaction(t, "secret")
	obj := newObj(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "s", "namespace": "ns", "resourceVersion": "3",
			"annotations": map[string]any{kinds.LastAppliedAnnotation: "s3cretA", "example.invalid/outside": "outsideA"}},
		"data": map[string]any{"k": "czNjcmV0QQ=="},
	})
	_, _, exec, err := PlanUpdateObserved(t.Context(), &obsClient{}, r.GVR(), "ns", obj, r, "c-x", "secret", "Secret", nil, true)
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, data, "data")
	assert.Equal(t, map[string]any{"example.invalid/outside": "outsideA"},
		data["metadata"].(map[string]any)["annotations"])
}

// TestScrubObjectWithAReadRedactionKeepsASecretPayload asserts a read
// redaction of the secret row leaves data and stringData in any letter case
// and still removes the last-applied annotation, while the redaction of the
// same row for a preview removes all three.
func TestScrubObjectWithAReadRedactionKeepsASecretPayload(t *testing.T) {
	t.Parallel()
	secret, ok := kinds.ByKey("secret")
	require.True(t, ok)
	obj := func() map[string]any {
		return map[string]any{
			"data":       map[string]any{"k": "czNjcmV0QQ=="},
			"StringData": map[string]any{"k": "s3cretA"},
			"metadata": map[string]any{"annotations": map[string]any{
				kinds.LastAppliedAnnotation: `{"data":{"k":"s3cretA"}}`,
				"example.invalid/outside":   "outsideA",
			}},
		}
	}
	got := ScrubObject(obj(), secret.ReadRedaction())
	assert.Equal(t, map[string]any{
		"data":       map[string]any{"k": "czNjcmV0QQ=="},
		"StringData": map[string]any{"k": "s3cretA"},
		"metadata": map[string]any{"annotations": map[string]any{
			"example.invalid/outside": "outsideA",
		}},
	}, got)
	assert.Equal(t, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			"example.invalid/outside": "outsideA",
		}},
	}, ScrubObject(obj(), secret.Redaction()))
}

// TestScrubObjectWithAReadRedactionHidesOtherSensitivePayloads asserts the
// read redaction of a row other than secret hides what the preview hides:
// data and stringData of a Sensitive row, a machine config Secret field in
// another letter case and a row path.
func TestScrubObjectWithAReadRedactionHidesOtherSensitivePayloads(t *testing.T) {
	t.Parallel()
	for key, obj := range map[string]map[string]any{
		"user":            {"data": "s3cretA", "Password": "s3cretB"},
		"amazonec2config": {"SSHKeyContents": "s3cretA", "sessiontoken": "s3cretB"},
		"managementtoken": {"stringData": "s3cretA", "token": "s3cretB"},
	} {
		k, ok := kinds.ByKey(key)
		require.True(t, ok, key)
		obj["keep"] = "outsideA"
		assert.Equal(t, map[string]any{"keep": "outsideA"}, ScrubObject(obj, k.ReadRedaction()), key)
	}
}

// TestCheckRedactionRefusesAReadRedaction asserts a preview and an apply
// result cannot be built from a read redaction, even one built for the
// resource the call previews: the secret row's read redaction leaves the
// payload visible.
func TestCheckRedactionRefusesAReadRedaction(t *testing.T) {
	t.Parallel()
	secret, ok := kinds.ByKey("secret")
	require.True(t, ok)
	read := secret.ReadRedaction()
	require.True(t, read.IsRead())

	err := checkRedaction(secret.GVR, read, "create")
	require.ErrorContains(t, err, "internal error: create preview of secrets uses the redaction for reads")
	require.NoError(t, checkRedaction(secret.GVR, secret.Redaction(), "create"))

	client := &obsClient{}
	obj := newObj(map[string]any{"metadata": map[string]any{"name": "s", "namespace": "ns"}})
	_, _, _, err = PlanCreateObserved(t.Context(), client, secret.GVR, "ns", obj, read, "c-x", "secret", "Secret", false)
	require.ErrorContains(t, err, "uses the redaction for reads")
	_, _, _, err = PlanUpdateObserved(t.Context(), client, secret.GVR, "ns", obj, read, "c-x", "secret", "Secret", nil, false)
	require.ErrorContains(t, err, "uses the redaction for reads")
	assert.Empty(t, client.dryRuns, "no request before the refusal")
}
