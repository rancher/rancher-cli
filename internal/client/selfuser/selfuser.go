// Package selfuser provides the shared GVR, object constructor, and
// userID extractor for the ext.cattle.io/v1 SelfUser identity probe used
// by both the auth status identity check and the create cluster creatorId
// stamper.
package selfuser

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVR is the create-only "whoami" virtual resource. The server fills
// status.userID from the request's bearer identity on every Create; the
// resource has no Get or List. Shared by the auth status identity probe
// (internal/client/provider) and the create cluster creatorId stamper
// (internal/engine/provisioning).
var GVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "selfusers"}

// New returns the bare SelfUser object a probe must POST. An empty spec is
// sufficient - the server fills status.userID from the caller's bearer
// token.
func New() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "SelfUser",
	}}
}

// UserID extracts status.userID from the SelfUser returned by a successful
// probe. It returns "" when the field is absent - an empty id with no error
// is a real case the caller must guard against.
func UserID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "userID")
	return id
}
