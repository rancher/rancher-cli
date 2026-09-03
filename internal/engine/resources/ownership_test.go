package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// managedFieldsFixture is a managed-fields list in the server's shape: one
// Apply manager owning spec.replicas and the whole spec.template subtree,
// one Apply manager owning a list, and one Update manager (a controller)
// owning status and a label.
const managedFieldsFixture = `[
  {"manager":"kustomize-controller","operation":"Apply","apiVersion":"apps/v1","fieldsType":"FieldsV1",
   "fieldsV1":{"f:spec":{"f:replicas":{},"f:template":{}}}},
  {"manager":"argocd-controller","operation":"Apply","apiVersion":"apps/v1","fieldsType":"FieldsV1",
   "fieldsV1":{"f:spec":{"f:ports":{"k:{\"port\":80}":{"f:port":{}}}}}},
  {"manager":"deployment-controller","operation":"Update","apiVersion":"apps/v1","fieldsType":"FieldsV1",
   "fieldsV1":{"f:metadata":{"f:labels":{"f:app":{}}},"f:status":{"f:readyReplicas":{}}}},
  {"manager":"kubectl-edit","operation":"Update","apiVersion":"apps/v1","fieldsType":"FieldsV1",
   "fieldsV1":{"f:spec":{"f:replicas":{}}}}
]`

func managedFieldsOf(t *testing.T) []any {
	t.Helper()
	o := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","managedFields":`+managedFieldsFixture+`}}`)
	mf, _, err := unstructured.NestedSlice(o.Object, "metadata", "managedFields")
	require.NoError(t, err)
	return mf
}

// TestApplyOwnersMapsDiffPathsToApplyManagers asserts the mapping from a diff
// path (a JSON pointer) to the managers that own it through server-side
// apply: a map field by its f: key, a whole subtree when the owner's entry
// ends above the path, a list by its field when the path indexes into it
// (managed fields key list items by value, not position), and never an
// Update-operation owner.
func TestApplyOwnersMapsDiffPathsToApplyManagers(t *testing.T) {
	t.Parallel()
	mf := managedFieldsOf(t)
	cases := []struct {
		path string
		want []string
	}{
		{"/spec/replicas", []string{"kustomize-controller"}},
		{"/spec/template/spec/containers/0/image", []string{"kustomize-controller"}},
		{"/spec/ports/0/port", []string{"argocd-controller"}},
		{"/spec/ports/1", []string{"argocd-controller"}},
		{"/metadata/labels/app", nil},
		{"/status/readyReplicas", nil},
		{"/spec/paused", nil},
		{"/metadata/annotations/x", nil},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, applyOwners(mf, tc.path), tc.path)
	}
	assert.Nil(t, applyOwners(nil, "/spec/replicas"), "no managed fields, no owners")
}

// TestPatchWarnsWhenAnApplyManagerOwnsAChangedField asserts the plan-time
// warning: a change to a field an Apply manager owns names that manager
// and says it may revert the change; a change to a field only Update
// managers own, or that nobody owns, produces no warning; the warning never
// gates.
func TestPatchWarnsWhenAnApplyManagerOwnsAChangedField(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7","labels":{"app":"web"},"managedFields":`+managedFieldsFixture+`},"spec":{"replicas":1,"paused":false}}`)
	cases := []struct {
		name string
		ops  string
		want []string
	}{
		{"applied field", `[{"op":"replace","path":"/spec/replicas","value":3}]`,
			[]string{"field /spec/replicas is applied by kustomize-controller; that manager may revert this change on its next reconcile"}},
		{"update-managed field", `[{"op":"replace","path":"/metadata/labels/app","value":"other"}]`, nil},
		{"unowned field", `[{"op":"replace","path":"/spec/paused","value":true}]`, nil},
		{"two changes, one applied", `[{"op":"replace","path":"/spec/replicas","value":3},{"op":"replace","path":"/spec/paused","value":true}]`,
			[]string{"field /spec/replicas is applied by kustomize-controller; that manager may revert this change on its next reconcile"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &patchClient{obj: orig}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			cmd := NewPatch(mustKind(t, "deployment"), fac, nil)
			eff, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
				"cluster": "prod", "namespace": "ns1", "patch": tc.ops,
			}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, eff.Warnings)
			assert.False(t, eff.Destructive, "the warning never gates")
		})
	}
}

// TestApplyOwnersHandlesKeysDiffPathsCannotSplit asserts the shapes the plain
// segment walk got wrong: a label or annotation key containing "/" is one
// f: key in managed fields but two segments in a diff path; an all-digit
// map key is a map key, not a list index; an entry whose fieldsV1 is empty
// owns nothing; and one manager with two entries is reported once.
func TestApplyOwnersHandlesKeysDiffPathsCannotSplit(t *testing.T) {
	t.Parallel()
	o := objFromJSON(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","managedFields":[
	  {"manager":"flux","operation":"Apply","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{"f:app.kubernetes.io/name":{}},"f:labels":{"f:kustomize.toolkit.fluxcd.io/name":{}}},"f:data":{"f:9090":{}}}},
	  {"manager":"flux","operation":"Apply","apiVersion":"v2","fieldsType":"FieldsV1","fieldsV1":{"f:data":{"f:9090":{}}}},
	  {"manager":"empty-apply","operation":"Apply","fieldsType":"FieldsV1","fieldsV1":{}},
	  {"manager":"tie","operation":"Apply","fieldsType":"FieldsV1","fieldsV1":{"f:data":{"f:a/b":{}}}},
	  {"manager":"tie-loser","operation":"Apply","fieldsType":"FieldsV1","fieldsV1":{"f:data":{"f:a":{"f:c":{}}}}}
	]}}`)
	mf, _, err := unstructured.NestedSlice(o.Object, "metadata", "managedFields")
	require.NoError(t, err)
	cases := []struct {
		path string
		want []string
	}{
		{"/metadata/annotations/app.kubernetes.io/name", []string{"flux"}},
		{"/metadata/labels/kustomize.toolkit.fluxcd.io/name", []string{"flux"}},
		{"/metadata/annotations/app.kubernetes.io", nil},
		// Diff paths are unescaped, so a key "a" with child "b" and a key
		// "a/b" read the same; the longest matching key wins, by contract.
		{"/data/a/b", []string{"tie"}},
		{"/data/9090", []string{"flux"}},
		{"/data/8080", nil},
		{"/spec/anything", nil},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, applyOwners(mf, tc.path), tc.path)
	}
}
