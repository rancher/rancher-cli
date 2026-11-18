package provider

import (
	"net/url"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const podListBody = `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"4"},"items":[` +
	`{"metadata":{"name":"web","namespace":"apps"},"spec":{"nodeName":"worker-1"}}]}`

// TestListFieldSelectorSendsTheFieldSelectorOnly asserts the request of
// ListFieldSelector has the fieldSelector query and no labelSelector, in
// every namespace or in one, and decodes the listed objects.
func TestListFieldSelectorSendsTheFieldSelectorOnly(t *testing.T) {
	t.Parallel()
	srv := newTableServer(t, answer(200, "application/json", podListBody))
	c := tableClientFor(t, srv.Server)
	lister, ok := c.(engine.FieldSelectorLister)
	require.True(t, ok, "the provider client lists by field selector")

	list, err := lister.ListFieldSelector(t.Context(), podsGVR, "", "spec.nodeName=worker-1")
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "web", list.Items[0].GetName())
	got := srv.last(t)
	assert.Equal(t, "/api/v1/pods", got.Path)
	query, err := url.ParseQuery(got.Query)
	require.NoError(t, err)
	assert.Equal(t, "spec.nodeName=worker-1", query.Get("fieldSelector"))
	assert.NotContains(t, query, "labelSelector")

	_, err = lister.ListFieldSelector(t.Context(), podsGVR, "apps", "spec.nodeName=worker-1")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/namespaces/apps/pods", srv.last(t).Path)
}

// TestListFieldSelectorMapsAResponseLikeList asserts a refused List by field
// selector is the clierr error List gives.
func TestListFieldSelectorMapsAResponseLikeList(t *testing.T) {
	t.Parallel()
	srv := newTableServer(t, answer(403, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pods is forbidden: User \"u\" cannot list resource \"pods\" in API group \"\" at the cluster scope","reason":"Forbidden","code":403}`))
	c := tableClientFor(t, srv.Server)
	_, err := c.(engine.FieldSelectorLister).ListFieldSelector(t.Context(), podsGVR, "", "spec.nodeName=worker-1")
	require.Error(t, err)
	_, errList := c.List(t.Context(), podsGVR, "", "")
	require.Error(t, errList)
	assert.Equal(t, errList.Error(), err.Error())
}
