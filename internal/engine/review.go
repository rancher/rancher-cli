package engine

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// ReviewKind names one of the four review resources a ClusterClient may
// post. The set is closed: PostReview refuses every other kind, so the
// read-only review path can never create a persisted object.
type ReviewKind string

const (
	ReviewSubjectAccess       ReviewKind = "subjectAccess"
	ReviewSelfSubjectAccess   ReviewKind = "selfSubjectAccess"
	ReviewSelfSubjectRules    ReviewKind = "selfSubjectRules"
	ReviewSelfSubjectIdentity ReviewKind = "selfSubjectIdentity"
)

// ReviewRequest is the object PostReview sends. Object must carry the
// GVK ReviewGVK reports for Kind; the provider rejects a mismatch before
// any request leaves the process.
type ReviewRequest struct {
	Kind   ReviewKind
	Object *unstructured.Unstructured
}

// ReviewResponse preserves what the server answered. Status and Body are
// set for every HTTP response; Object is decoded for 2xx only; Mapped is
// the provider's own error mapping of a non-2xx status and nil on 2xx.
// Callers classify on Status and Body first (a 403 body tells the
// Rancher proxy's refusals apart from the apiserver's) and adopt Mapped
// otherwise.
type ReviewResponse struct {
	Status int
	Body   []byte
	Object *unstructured.Unstructured
	Mapped *clierr.Error
}

// APIResource is one entry of the server's preferred-version discovery,
// reduced to what resource resolution and completion need.
type APIResource struct {
	Group        string
	Version      string
	Name         string
	SingularName string
	Kind         string
	ShortNames   []string
	Namespaced   bool
	Verbs        []string
}

// Impersonation names the subject a cluster client acts as. Only the user
// is sent: Rancher attaches the subject's groups itself and refuses a
// client-supplied group header on the downstream.
type Impersonation struct {
	User string
}

type reviewAPI struct {
	gvr schema.GroupVersionResource
	gvk schema.GroupVersionKind
}

var reviewAPIs = map[ReviewKind]reviewAPI{
	ReviewSubjectAccess: {
		gvr: schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"},
		gvk: schema.GroupVersionKind{Group: "authorization.k8s.io", Version: "v1", Kind: "SubjectAccessReview"},
	},
	ReviewSelfSubjectAccess: {
		gvr: schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews"},
		gvk: schema.GroupVersionKind{Group: "authorization.k8s.io", Version: "v1", Kind: "SelfSubjectAccessReview"},
	},
	ReviewSelfSubjectRules: {
		gvr: schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectrulesreviews"},
		gvk: schema.GroupVersionKind{Group: "authorization.k8s.io", Version: "v1", Kind: "SelfSubjectRulesReview"},
	},
	ReviewSelfSubjectIdentity: {
		gvr: schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"},
		gvk: schema.GroupVersionKind{Group: "authentication.k8s.io", Version: "v1", Kind: "SelfSubjectReview"},
	},
}

// ReviewGVR reports the resource a review kind posts to.
func ReviewGVR(k ReviewKind) (schema.GroupVersionResource, bool) {
	a, ok := reviewAPIs[k]
	return a.gvr, ok
}

// ReviewGVK reports the object kind a review request must carry.
func ReviewGVK(k ReviewKind) (schema.GroupVersionKind, bool) {
	a, ok := reviewAPIs[k]
	return a.gvk, ok
}
