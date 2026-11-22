package clierr

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// authorizerRequest is the request a refusal is read against.
type authorizerRequest struct {
	verb, group, resource, ns, name string
}

var (
	// namespacedGet is a GET of a ConfigMap in a namespace.
	namespacedGet = authorizerRequest{"get", "", "configmaps", "ns1", "obj1"}
	// clusterGet is a GET of an object at the cluster scope.
	clusterGet = authorizerRequest{"get", "management.cattle.io", "globalrolebindings", "", "grb-1"}
	// namespaceGet is a GET of a namespace, which the API server words as a
	// request in that namespace.
	namespaceGet = authorizerRequest{"get", "", "namespaces", "", "ns1"}
	// namedGroupNamespacesGet is a GET of a resource named namespaces in a
	// named group. The API server reads /namespaces/<name> as the namespace
	// of the request in any group and words its refusal "in the namespace
	// <name>" too. AuthorizerRefused applies that form only to the core
	// namespaces resource: for this GET it is false, the delete takes no uid
	// precondition, and the command returns the refusal unchanged.
	namedGroupNamespacesGet = authorizerRequest{"get", "example.io", "namespaces", "", "ns1"}
)

// authorizerForbidden is the 403 the API server writes when its authorizer
// refuses a request, built as responsewriters.ForbiddenStatusError builds it:
// apierrors.NewForbidden of the group, resource and name, with the message
// of forbiddenMessage and, when reason is set, ": <reason>" after it.
func authorizerForbidden(group, resource, name, text, reason string) *apierrors.StatusError {
	msg := `User "u-delete" ` + text
	if reason != "" {
		msg += ": " + reason
	}
	return apierrors.NewForbidden(schema.GroupResource{Group: group, Resource: resource}, name, errors.New(msg))
}

const (
	namespacedGetText = `cannot get resource "configmaps" in API group "" in the namespace "ns1"`
	clusterGetText    = `cannot get resource "globalrolebindings" in API group "management.cattle.io" at the cluster scope`
)

// authorizerRows are 403 responses to a GET and whether each is the
// authorizer's refusal of that GET.
var authorizerRows = []struct {
	name    string
	req     authorizerRequest
	err     func() error
	refused bool
}{
	{"get in a namespace", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", namespacedGetText, "")
	}, true},
	{"get in a namespace with a reason", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", namespacedGetText, `RBAC: role.rbac.authorization.k8s.io "x" not found`)
	}, true},
	{"get at the cluster scope", clusterGet, func() error {
		return authorizerForbidden("management.cattle.io", "globalrolebindings", "grb-1", clusterGetText, "")
	}, true},
	{"get at the cluster scope with a reason", clusterGet, func() error {
		return authorizerForbidden("management.cattle.io", "globalrolebindings", "grb-1", clusterGetText, "no rule allows it")
	}, true},
	{"get of a namespace", namespaceGet, func() error {
		return authorizerForbidden("", "namespaces", "ns1", `cannot get resource "namespaces" in API group "" in the namespace "ns1"`, "")
	}, true},
	{"get of a namespace worded at the cluster scope", namespaceGet, func() error {
		return authorizerForbidden("", "namespaces", "ns1", `cannot get resource "namespaces" in API group "" at the cluster scope`, "")
	}, false},
	{"get of a namespaces resource in a named group, in the namespace form the server writes, is not read as a refusal", namedGroupNamespacesGet, func() error {
		return authorizerForbidden("example.io", "namespaces", "ns1", `cannot get resource "namespaces" in API group "example.io" in the namespace "ns1"`, "")
	}, false},
	{"get of another resource", namespacedGet, func() error {
		return authorizerForbidden("", "secrets", "obj1", `cannot get resource "secrets" in API group "" in the namespace "ns1"`, "")
	}, false},
	{"get text of another resource in details of this one", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", `cannot get resource "secrets" in API group "" in the namespace "ns1"`, "")
	}, false},
	{"get in another group", namespacedGet, func() error {
		return authorizerForbidden("example.io", "configmaps", "obj1", `cannot get resource "configmaps" in API group "example.io" in the namespace "ns1"`, "")
	}, false},
	{"get in another namespace", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", `cannot get resource "configmaps" in API group "" in the namespace "ns2"`, "")
	}, false},
	{"get of another name", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj2", namespacedGetText, "")
	}, false},
	{"get at the cluster scope for a request in a namespace", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", `cannot get resource "configmaps" in API group "" at the cluster scope`, "")
	}, false},
	{"get in a namespace for a request at the cluster scope", clusterGet, func() error {
		return authorizerForbidden("management.cattle.io", "globalrolebindings", "grb-1", `cannot get resource "globalrolebindings" in API group "management.cattle.io" in the namespace "ns1"`, "")
	}, false},
	{"get text followed by other words", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", namespacedGetText+" and more", "")
	}, false},
	{"list", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", `cannot list resource "configmaps" in API group "" in the namespace "ns1"`, "")
	}, false},
	{"delete", namespacedGet, func() error {
		return authorizerForbidden("", "configmaps", "obj1", `cannot delete resource "configmaps" in API group "" in the namespace "ns1"`, "")
	}, false},
	{"no reason", namespacedGet, func() error {
		return ResponseError(http.MethodGet, http.StatusForbidden, nil)
	}, false},
	{"admission text", namespacedGet, func() error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("exceeded quota: q, requested: count/configmaps=1, used: count/configmaps=1, limited: count/configmaps=1"))
	}, false},
	{"Steve error object", namespacedGet, func() error {
		return ResponseError(http.MethodGet, http.StatusForbidden,
			jsonBody(map[string]any{"type": "error", "links": map[string]any{}, "code": "PermissionDenied", "status": 403,
				"message": `configmaps "obj1" is forbidden: User "u-delete" ` + namespacedGetText}))
	}, false},
	{"Norman error object", namespacedGet, func() error {
		return ResponseError(http.MethodGet, http.StatusForbidden,
			jsonBody(map[string]any{"Code": map[string]any{"Code": "Forbidden", "Status": 403}, "Cause": nil, "FieldName": "",
				"Message": `configmaps "obj1" is forbidden: User "u-delete" ` + namespacedGetText}))
	}, false},
	{"401 with the get text", namespacedGet, func() error {
		se := authorizerForbidden("", "configmaps", "obj1", namespacedGetText, "")
		se.ErrStatus.Code = http.StatusUnauthorized
		return se
	}, false},
	{"403 with another reason", namespacedGet, func() error {
		se := authorizerForbidden("", "configmaps", "obj1", namespacedGetText, "")
		se.ErrStatus.Reason = "Unauthorized"
		return se
	}, false},
	{"not a server response", namespacedGet, func() error {
		return errors.New(`configmaps "obj1" is forbidden: User "u-delete" ` + namespacedGetText)
	}, false},
}

// jsonBody is v as a JSON body.
func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestAuthorizerRefusedReadsOnlyTheAuthorizersRefusalOfThatRequest asserts,
// for each 403 response, as client-go returns it, as the classified error the
// clients return, and as the body ResponseError reads, that AuthorizerRefused
// is true only for the authorizer's refusal of the verb, group, resource,
// namespace and name of the request, with or without a reason after it.
func TestAuthorizerRefusedReadsOnlyTheAuthorizersRefusalOfThatRequest(t *testing.T) {
	t.Parallel()
	for _, row := range authorizerRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			raw := row.err()
			shapes := map[string]error{"as the server responded": raw, "classified": From(raw)}
			if se, ok := errors.AsType[*apierrors.StatusError](raw); ok && se.ErrStatus.Details != nil && len(se.ErrStatus.Details.Causes) == 0 {
				st := se.ErrStatus
				st.Kind, st.APIVersion = "Status", "v1"
				body, err := json.Marshal(st)
				require.NoError(t, err)
				shapes["as a body"] = FromResponse("", http.MethodGet, int(st.Code), body, nil)
			}
			for shape, err := range shapes {
				r := row.req
				assert.Equal(t, row.refused, AuthorizerRefused(err, r.verb, r.group, r.resource, r.ns, r.name), shape)
			}
		})
	}
}

// uidConflictRows are 409 responses and whether each says the uid sent with
// the request is different from the object's.
var uidConflictRows = []struct {
	name     string
	err      func() error
	conflict bool
}{
	{"delete uid precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("the UID in the precondition (u1) does not match the UID in record (u2). The object might have been deleted and then recreated"))
	}, true},
	{"storage uid precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("StorageError: invalid object, Code: 4, Key: /registry/configmaps/ns1/obj1, ResourceVersion: 0, AdditionalErrorMsg: Precondition failed: UID in precondition: 00000000-0000-0000-0000-000000000000, UID in object meta: 119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f"))
	}, true},
	{"namespace uid precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, "ns1",
			errors.New("Precondition failed: UID in precondition: u1, UID in object meta: u2"))
	}, true},
	{"CustomResourceDefinition uid precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "widgets.example.io",
			errors.New("precondition failed: UID in precondition: u1, UID in object meta: u2"))
	}, true},
	{"kubeconfig store uid precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Group: "ext.cattle.io", Resource: "kubeconfigs"}, "kc-1",
			errors.New("the UID in the precondition (u1) does not match the UID in record (u2)"))
	}, true},
	{"admission webhook denial", func() error {
		se := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1", errors.New("x"))
		se.ErrStatus.Message = `admission webhook "x.example.io" denied the request: duplicate not allowed`
		return se
	}, false},
	{"admission webhook denial with the uid words", func() error {
		se := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1", errors.New("x"))
		se.ErrStatus.Message = `admission webhook "x" denied the request: the UID in the precondition (a) does not match the UID in record (b)`
		return se
	}, false},
	{"resourceVersion race", func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}, false},
	{"delete resourceVersion precondition", func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("the ResourceVersion in the precondition (4) does not match the ResourceVersion in record (5). The object might have been modified"))
	}, false},
	{"uid text with another code", func() error {
		se := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "obj1",
			errors.New("the UID in the precondition (u1) does not match the UID in record (u2)"))
		se.ErrStatus.Code, se.ErrStatus.Reason = http.StatusUnprocessableEntity, "Invalid"
		return se
	}, false},
	{"no body", func() error { return ResponseError(http.MethodDelete, http.StatusConflict, nil) }, false},
	{"not a server response", func() error {
		return errors.New("the UID in the precondition (u1) does not match the UID in record (u2)")
	}, false},
}

// TestUIDConflictReadsOnlyAUIDConflict asserts, for each 409 response as
// client-go returns it and as the classified error the clients return, that
// UIDConflict is true only for the texts of a uid precondition that is not the
// object's, and false for an admission webhook's 409, a resourceVersion race and
// any other response.
func TestUIDConflictReadsOnlyAUIDConflict(t *testing.T) {
	t.Parallel()
	for _, row := range uidConflictRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			raw := row.err()
			assert.Equal(t, row.conflict, UIDConflict(raw), "as the server responded")
			assert.Equal(t, row.conflict, UIDConflict(From(raw)), "classified")
			if ce := FromStatus(raw, &Request{Verb: "delete", Resource: "configmaps", Name: "obj1"}); ce != nil {
				assert.Equal(t, row.conflict, UIDConflict(ce), "classified for a delete")
			}
		})
	}
	assert.False(t, UIDConflict(nil))
}
