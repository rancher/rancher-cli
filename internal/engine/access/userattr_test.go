package access

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// userAttributeRefusal sets how a fakeClient's steve refuses UserAttributes.
type userAttributeRefusal int

const (
	// attrServed: the fake serves what it holds.
	attrServed userAttributeRefusal = iota
	// attrNoRight: no right on userattributes: 404 with no body to every
	// read, no schema.
	attrNoRight
	// attrGetOnly: get without list: the collection answers 200 with no
	// items and the list check says no.
	attrGetOnly
)

// steveGet serves the fake's UserAttributes in steve's shape: the object
// Objects["<id>-attr"], the collection Lists["userattributes||"], their
// errors (GetErrFor, ListErr, ListErrFor), and the refusal the test sets.
func (f *fakeClient) steveGet(_ context.Context, r steve.Request) ([]byte, error) {
	f.mu.Lock()
	f.AttrPaths = append(f.AttrPaths, r.Path())
	f.mu.Unlock()
	notFound := clierr.ResponseError(http.MethodGet, http.StatusNotFound, nil)
	switch {
	case r.Type == "schemas":
		if f.AttrRefusal == attrNoRight {
			return nil, notFound
		}
		return []byte(`{"id":"management.cattle.io.userattribute","type":"schema"}`), nil
	case r.Name != "":
		if f.AttrRefusal == attrNoRight {
			return nil, notFound
		}
		key := r.Name + "-attr"
		if err, ok := f.GetErrFor[key]; ok {
			return nil, steveError(err)
		}
		if f.GetErr != nil {
			return nil, steveError(f.GetErr)
		}
		o, ok := f.Objects[key]
		if !ok {
			return nil, notFound
		}
		return userAttrItem(o)
	}
	if f.AttrRefusal == attrNoRight {
		return nil, notFound
	}
	if f.ListErr != nil {
		return nil, steveError(f.ListErr)
	}
	if err, ok := f.ListErrFor["userattributes||"]; ok {
		return nil, steveError(err)
	}
	var data []json.RawMessage
	if l, ok := f.Lists["userattributes||"]; ok && f.AttrRefusal != attrGetOnly {
		for i := range l.Items {
			item, err := userAttrItem(&l.Items[i])
			if err != nil {
				return nil, err
			}
			data = append(data, item)
		}
	}
	body := map[string]any{"type": "collection", "revision": "1", "data": data}
	if data == nil {
		body["data"] = []any{}
	} else {
		body["count"] = len(data)
	}
	return json.Marshal(body)
}

// userAttrItem is obj with the id steve adds.
func userAttrItem(obj *unstructured.Unstructured) ([]byte, error) {
	item := map[string]any{"id": obj.GetName(), "type": "management.cattle.io.userattribute"}
	maps.Copy(item, obj.Object)
	return json.Marshal(item)
}

// steveError is the answer steve gives for a refusal the fake holds as a
// mapped error: a 403 for RBAC, a 404 for NOT_FOUND, the error itself
// otherwise.
func steveError(err error) error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		switch ce.Code {
		case clierr.CodeRBAC:
			return clierr.ResponseError(http.MethodGet, http.StatusForbidden, nil)
		case clierr.CodeNotFound:
			return clierr.ResponseError(http.MethodGet, http.StatusNotFound, nil)
		}
	}
	return err
}

// attrsFor reads the fake's UserAttributes through steve.
func attrsFor(local *fakeClient) *steve.UserAttributes {
	return steve.NewUserAttributes(local.steveGet, local)
}

// attrRoot serves a fake's UserAttributes as the server root.
type attrRoot struct{ local *fakeClient }

func (r attrRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	p, query, _ := strings.Cut(path, "?")
	_ = query
	parts := strings.Split(strings.TrimPrefix(p, "/v1/"), "/")
	req := steve.Request{Type: parts[0]}
	if len(parts) > 1 {
		name, err := url.PathUnescape(parts[1])
		if err != nil {
			return nil, err
		}
		req.Name = name
	}
	return r.local.steveGet(ctx, req)
}

type attrRoots struct{ local *fakeClient }

func (r attrRoots) Root() (engine.RootClient, error) { return attrRoot(r), nil }

// attrRootsFor is the root client factory over a fake's UserAttributes. A
// nil fake (a command built only to describe itself) gives a nil factory.
func attrRootsFor(local *fakeClient) engine.RootClientFactory {
	if local == nil {
		return nil
	}
	return attrRoots{local}
}

// newCanOver is `access can` reading UserAttributes from local.
func newCanOver(f *fakeFactory, local *fakeClient) runtime.Command {
	return NewCan(f, attrRootsFor(local), local)
}

// readProvenanceOver is readProvenance reading UserAttributes from fc.
func readProvenanceOver(ctx context.Context, fc *fakeClient, in provenanceInput) (provenance, error) {
	return readProvenance(ctx, fc, attrsFor(fc), in)
}

// planIdentityOver is planIdentity reading UserAttributes from local.
func planIdentityOver(ctx context.Context, clients engine.ClusterClientFactory, callerClient engine.ClusterClient, local *fakeClient,
	clusterID string, caller callerIdentity, as ParsedSubject, forList bool) (identityPlan, error) {
	return planIdentity(ctx, clients, callerClient, local, attrsFor(local), clusterID, caller, as, forList)
}

// attrOverRoot answers the UserAttribute and schema paths from a fake and
// every other path from inner.
type attrOverRoot struct {
	inner engine.RootClient
	local *fakeClient
}

func (r attrOverRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	if strings.HasPrefix(path, "/v1/management.cattle.io.userattributes") || strings.HasPrefix(path, "/v1/schemas/management.cattle.io.userattribute") {
		return attrRoot{r.local}.GetJSON(ctx, path)
	}
	return r.inner.GetJSON(ctx, path)
}

// rootsOver is a root client factory for inner that reads UserAttributes
// from local when local is a fake.
func rootsOver(inner engine.RootClient, local engine.ClusterClient) engine.RootClientFactory {
	if f, ok := local.(*fakeClient); ok && f != nil {
		return fakeRootFactory{attrOverRoot{inner: inner, local: f}}
	}
	return fakeRootFactory{inner}
}

// isListUserAttributesReview reports whether req is the reader's list check.
func isListUserAttributesReview(req engine.ReviewRequest) bool {
	if req.Kind != engine.ReviewSelfSubjectAccess || req.Object == nil {
		return false
	}
	ra, found, _ := unstructured.NestedMap(req.Object.Object, "spec", "resourceAttributes")
	return found && ra["verb"] == "list" && ra["group"] == "management.cattle.io" && ra["resource"] == "userattributes"
}

// userAttributesRemedyText is the grant every UserAttribute refusal names.
var userAttributesRemedyText = "; a GlobalRole with get and list on userattributes.management.cattle.io allows it; see: " + clierr.Rancher + " get globalroles"
