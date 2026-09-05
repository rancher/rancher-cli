package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReviewKindsMapToClosedGVRSet asserts every review kind resolves to
// exactly one authorization or authentication review resource and that an
// unknown kind resolves to nothing.
func TestReviewKindsMapToClosedGVRSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind     ReviewKind
		group    string
		resource string
		kindName string
	}{
		{ReviewSubjectAccess, "authorization.k8s.io", "subjectaccessreviews", "SubjectAccessReview"},
		{ReviewSelfSubjectAccess, "authorization.k8s.io", "selfsubjectaccessreviews", "SelfSubjectAccessReview"},
		{ReviewSelfSubjectRules, "authorization.k8s.io", "selfsubjectrulesreviews", "SelfSubjectRulesReview"},
		{ReviewSelfSubjectIdentity, "authentication.k8s.io", "selfsubjectreviews", "SelfSubjectReview"},
	}
	for _, c := range cases {
		gvr, ok := ReviewGVR(c.kind)
		require.True(t, ok, c.kind)
		assert.Equal(t, c.group, gvr.Group)
		assert.Equal(t, "v1", gvr.Version)
		assert.Equal(t, c.resource, gvr.Resource)
		gvk, ok := ReviewGVK(c.kind)
		require.True(t, ok)
		assert.Equal(t, c.kindName, gvk.Kind)
	}
	_, ok := ReviewGVR(ReviewKind("secrets"))
	assert.False(t, ok, "a non-review kind must not resolve")
}
