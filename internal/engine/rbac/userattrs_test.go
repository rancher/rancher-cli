package rbac

import (
	"context"
	"encoding/json"
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// listAllowedClient posts no review but answers the list check: allowed.
type listAllowedClient struct {
	engine.ClusterClient
	allowed bool
}

func (c listAllowedClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{Status: 201, Object: &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"status": map[string]any{"allowed": c.allowed},
	}}}, nil
}

// userAttributeGetter serves items as steve's UserAttributes collection, or
// answers every request with err, and counts the requests in calls when it
// is not nil.
func userAttributeGetter(items []unstructured.Unstructured, err error, calls *int) steve.Getter {
	return func(_ context.Context, _ steve.Request) ([]byte, error) {
		if calls != nil {
			*calls++
		}
		if err != nil {
			return nil, err
		}
		data := make([]map[string]any, 0, len(items))
		for i := range items {
			item := map[string]any{"id": items[i].GetName()}
			maps.Copy(item, items[i].Object)
			data = append(data, item)
		}
		body := map[string]any{"type": "collection", "revision": "1", "data": data}
		if len(data) > 0 {
			body["count"] = len(data)
		}
		return json.Marshal(body)
	}
}

// attrsOver reads fc's userattributes list through steve, with the list
// check allowed.
func attrsOver(fc *fakeClient) *steve.UserAttributes {
	var items []unstructured.Unstructured
	if l := fc.listsByGVR["userattributes"]; l != nil {
		items = l.Items
	}
	return steve.NewUserAttributes(userAttributeGetter(items, fc.listErrByGVR["userattributes"], nil), listAllowedClient{allowed: true})
}
