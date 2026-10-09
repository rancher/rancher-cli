package clierr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestResponseErrorKeepsAStatusBodysErrorCode asserts a Status body is
// classified by its own error code, as client-go classifies one, and a
// Status body without an error code by the HTTP status.
func TestResponseErrorKeepsAStatusBodysErrorCode(t *testing.T) {
	t.Parallel()
	notFound := []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"nope","reason":"NotFound","code":404}`)
	ce := FromStatus(ResponseError(http.MethodGet, http.StatusForbidden, notFound), nil)
	require.NotNil(t, ce)
	assert.Equal(t, CodeNotFound, ce.Code)
	assert.Equal(t, "nope", ce.Message)

	noCode := []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"nope","reason":"NotFound"}`)
	ce = FromStatus(ResponseError(http.MethodGet, http.StatusForbidden, noCode), nil)
	require.NotNil(t, ce)
	assert.Equal(t, CodeRBAC, ce.Code)
}

// TestResponseErrorShowsRancherErrorObjectsMessage asserts that the
// message of Rancher's JSON error object, never the raw JSON, is the
// server's reason, that a long body is cut, and that a 5xx that is not a
// Status is retryable whatever its body.
func TestResponseErrorShowsRancherErrorObjectsMessage(t *testing.T) {
	t.Parallel()
	steve := []byte(`{"type":"error","links":{},"code":"PermissionDenied","message":"can not list management.cattle.io.clusters","status":403}`)
	ce := FromResponse("rancher root GET /v1/x", http.MethodGet, http.StatusForbidden, steve, &Request{})
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, "rancher root GET /v1/x: can not list management.cattle.io.clusters", ce.Message)

	empty := []byte(`{"code":"PermissionDenied","status":403,"type":"error","baseType":"error"}`)
	ce = FromResponse("login failed", http.MethodPost, http.StatusForbidden, empty, nil)
	assert.Equal(t, "login failed: the server refused the request (403) and gave no reason", ce.Message)

	long := []byte(strings.Repeat("x", 300))
	ce = FromResponse("GET /", http.MethodGet, http.StatusForbidden, long, nil)
	assert.Equal(t, "GET /: "+strings.Repeat("x", maxServerMessage)+"…", ce.Message)

	rancher500 := []byte(`{"code":"ServerError","status":500,"type":"error","baseType":"error"}`)
	assert.True(t, FromResponse("login failed", http.MethodPost, http.StatusInternalServerError, rancher500, nil).Retryable)
	assert.True(t, FromResponse("GET /", http.MethodGet, http.StatusInternalServerError, []byte("upstream connect error"), nil).Retryable)
}

// TestFromStatusKeepsTheStatusReadable asserts the classified error keeps
// the server's error as its cause, so HTTPStatus still reads the status
// through it and through a wrapper around it.
func TestFromStatusKeepsTheStatusReadable(t *testing.T) {
	t.Parallel()
	ce := FromStatus(fmt.Errorf("GET /x: %w", ResponseError(http.MethodGet, http.StatusForbidden, nil)), nil)
	require.NotNil(t, ce)
	status, ok := HTTPStatus(ce)
	assert.True(t, ok)
	assert.Equal(t, http.StatusForbidden, status)
	status, ok = HTTPStatus(fmt.Errorf("listing: %w", From(ce)))
	assert.True(t, ok)
	assert.Equal(t, http.StatusForbidden, status)
}

// TestFromNilIsNil asserts From and FromStatus give nil for a nil error.
func TestFromNilIsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, From(nil))
	assert.Nil(t, FromStatus(nil, nil))
}

// TestFromGivesAnErrorThatIsNoStatusItsText asserts an error that holds no
// server status is GENERIC with its own text, and that FromStatus leaves it
// alone.
func TestFromGivesAnErrorThatIsNoStatusItsText(t *testing.T) {
	t.Parallel()
	err := errors.New("boom")
	assert.Nil(t, FromStatus(err, nil))
	ce := From(err)
	require.NotNil(t, ce)
	assert.Equal(t, CodeGeneric, ce.Code)
	assert.Equal(t, "boom", ce.Message)
	assert.ErrorIs(t, ce, err)
}

// TestEndpointNotFoundNamesWhatToCheck asserts a 404 on a call that names
// an API: through the local cluster it is GENERIC naming the server URL;
// on the owner check through another cluster it is NOT_FOUND naming the
// cluster, and a 403 there names the cluster too; through a cluster already
// resolved both are the plain NOT_FOUND and RBAC.
func TestEndpointNotFoundNamesWhatToCheck(t *testing.T) {
	t.Parallel()
	router := []byte(`{"Code":{"Code":"NotFound","Status":404},"Message":"clusters.management.cattle.io \"c-1\" not found","Cause":null,"FieldName":""}`)
	ce := FromResponse("posting a SelfSubjectReview", http.MethodPost, http.StatusNotFound, router, &Request{Cluster: "c-1", Endpoint: true, Unresolved: true})
	assert.Equal(t, CodeNotFound, ce.Code)
	assert.Equal(t, "posting a SelfSubjectReview: the server could not find the requested resource; Rancher answers 404 for a cluster it does not know: check the --cluster value 'c-1'", ce.Message)

	forbidden := []byte(`{"Code":{"Code":"Forbidden","Status":403},"Message":"clusters.management.cattle.io \"c-1\" is forbidden: User \"u-1\" cannot get resource \"clusters\"","Cause":null,"FieldName":""}`)
	ce = FromResponse("posting a SelfSubjectReview", http.MethodPost, http.StatusForbidden, forbidden, &Request{Cluster: "c-1", Endpoint: true, Unresolved: true})
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, `posting a SelfSubjectReview: clusters.management.cattle.io "c-1" is forbidden: User "u-1" cannot get resource "clusters"; Rancher answers 403 for a cluster the user cannot read and, to a user who cannot read every cluster, for one it does not know: check the --cluster value 'c-1'`, ce.Message)

	ce = FromResponse("posting a SelfSubjectReview", http.MethodPost, http.StatusNotFound, nil, &Request{Cluster: "local", Endpoint: true, Unresolved: true})
	assert.Equal(t, CodeGeneric, ce.Code)
	assert.Equal(t, "posting a SelfSubjectReview: the server could not find the requested resource; check the server URL", ce.Message)

	ce = FromResponse("posting a SelfSubjectReview", http.MethodPost, http.StatusForbidden, nil, &Request{Cluster: "local", Endpoint: true, Unresolved: true})
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, "posting a SelfSubjectReview: the server refused the request (403) and gave no reason", ce.Message)

	downstream := []byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"forbidden: User \"u-1\" cannot get path \"/api\"","reason":"Forbidden","code":403}`)
	ce = FromResponse("GET /api", http.MethodGet, http.StatusForbidden, downstream, &Request{Cluster: "c-1", Endpoint: true})
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, `GET /api: forbidden: User "u-1" cannot get path "/api"`, ce.Message)

	ce = FromResponse("GET /api", http.MethodGet, http.StatusNotFound, nil, &Request{Cluster: "c-1", Endpoint: true})
	assert.Equal(t, CodeNotFound, ce.Code)
	assert.Equal(t, "GET /api: the server could not find the requested resource", ce.Message)

	ce = FromResponse("GET /version", http.MethodGet, http.StatusNotFound, nil, &Request{Cluster: "local", Endpoint: true})
	assert.Equal(t, CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, "GET /version: the server could not find the requested resource; check the server URL", ce.Message)
}

// wrapped puts a prefix in front of a server error, as a caller does.
func wrapped(prefix string, err error) error { return fmt.Errorf("%s: %w", prefix, err) }

// typed500 is the error client-go builds for a 500 whose body it read as
// text, the body quoted into the message; body "unknown" is what client-go
// passes for a body it does not keep.
func typed500(gr schema.GroupResource, body string) error {
	return apierrors.NewGenericServerResponse(http.StatusInternalServerError, http.MethodGet, gr, "", body, 0, true)
}

// TestClassifyStatusWordingRules asserts the table's wording rules one by
// one: each status and the error that is not one getting its code; the
// rancher auth status hint only for a named call that sent a credential;
// the text in front kept for a 401 with no reason, a bare "Unauthorized",
// or apimachinery's default; a refusal with no reason worded by the
// request, also behind a text with an empty status text; client-go's
// "unknown" as no reason in a 403, a 415 and a 5xx; a 406 and a 415 with
// no reason naming the status text; a 429 retryable; the agent wording
// for a 5xx only through a cluster other than local; an unexpected 500
// worded by the server's own text; and a long reason cut to 200 bytes on
// a character boundary.
func TestClassifyStatusWordingRules(t *testing.T) {
	t.Parallel()
	unknown403 := apierrors.NewForbidden(schema.GroupResource{}, "", errors.New(unknownMessage))
	unknown403.ErrStatus.Message = unknownMessage
	unknown415 := apierrors.NewGenericServerResponse(http.StatusUnsupportedMediaType, http.MethodPost, schema.GroupResource{}, "", unknownMessage, 0, true)
	gr := schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}
	nodes := schema.GroupResource{Resource: "nodes"}
	provisioning := schema.GroupResource{Group: "provisioning.cattle.io", Resource: "clusters"}
	local := &Request{Cluster: "local"}
	emptyForbidden := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
	}}
	emptyUnauthorized := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonUnauthorized, Code: http.StatusUnauthorized,
	}}
	defaultUnauthorized := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonUnauthorized, Code: http.StatusUnauthorized, Message: unauthorizedDefault,
	}}
	whitespaceForbidden := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden, Message: "   ",
	}}
	rancher401 := []byte(`{"apiVersion":"v1","code":401,"kind":"Status","message":"Unauthorized","metadata":{},"reason":"Unauthorized","status":"Failure"}`)
	for _, tc := range []struct {
		name      string
		err       error
		req       *Request
		code      string
		message   string
		hint      string
		retryable bool
	}{
		{"notfound", apierrors.NewNotFound(gr, "p-x"), local,
			CodeNotFound, `projects.management.cattle.io "p-x" not found`, "", false},
		{"forbidden with a reason passes through", apierrors.NewForbidden(gr, "p-x", errors.New("nope")), local,
			CodeRBAC, `projects.management.cattle.io "p-x" is forbidden: nope`, "", false},
		{"unauthorized with a reason passes through", apierrors.NewUnauthorized("bad token"), local,
			CodeAuth, "bad token", "", false},
		{"conflict is retryable with plain wording", apierrors.NewConflict(provisioning, "c-x", errors.New("the object has been modified; please apply your changes to the latest version and try again")), local,
			CodeConflict, "the resource changed on the server since it was last read; retry the command", "", true},
		{"already exists is not retryable", apierrors.NewAlreadyExists(provisioning, "c-x"), local,
			CodeConflict, `clusters.provisioning.cattle.io "c-x" already exists`, "", false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: gr.Group, Kind: "Project"}, "p-x", nil), local,
			CodeValidation, `Project.management.cattle.io "p-x" is invalid`, "", false},
		{"bad request", apierrors.NewBadRequest("bad"), local,
			CodeValidation, "bad", "", false},
		{"401 with no request names no remedy", ResponseError(http.MethodGet, http.StatusUnauthorized, nil), nil,
			CodeAuth, "the server rejected the credential (401)", "", false},
		{"401 for a named call keeps the text in front", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusUnauthorized, nil)), &Request{},
			CodeAuth, "GET /x: the server rejected the credential (401)", AuthStatusRemedy, false},
		{"401 with apimachinery's default message is no reason", defaultUnauthorized, local,
			CodeAuth, "the server rejected the credential (401)", AuthStatusRemedy, false},
		{"401 with an empty message is no reason", emptyUnauthorized, local,
			CodeAuth, "the server rejected the credential (401)", AuthStatusRemedy, false},
		{"401 with Rancher's Status body is no reason", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusUnauthorized, rancher401)), &Request{},
			CodeAuth, "GET /x: the server rejected the credential (401)", AuthStatusRemedy, false},
		{"401 whose message is Unauthorized in any case is no reason", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusUnauthorized, []byte("UNAUTHORIZED"))), &Request{},
			CodeAuth, "GET /x: the server rejected the credential (401)", AuthStatusRemedy, false},
		{"401 that sent no credential says so, with no remedy", wrapped("listing providers", ResponseError(http.MethodGet, http.StatusUnauthorized, nil)), &Request{Endpoint: true, NoCredential: true},
			CodeAuth, "listing providers: the server answered 401; the request carried no credential", "", false},
		{"401 with Rancher's Status body that sent no credential says so", wrapped("listing providers", ResponseError(http.MethodGet, http.StatusUnauthorized, rancher401)), &Request{NoCredential: true},
			CodeAuth, "listing providers: the server answered 401; the request carried no credential", "", false},
		{"401 with a reason that sent no credential keeps the reason", wrapped("listing providers", ResponseError(http.MethodGet, http.StatusUnauthorized, []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"token expired","reason":"Unauthorized","code":401}`))), &Request{NoCredential: true},
			CodeAuth, "listing providers: token expired", "", false},
		{"unknown 403 gave no reason", wrapped("GET /x", unknown403), nil,
			CodeRBAC, "GET /x: the server refused the request (403) and gave no reason", "", false},
		{"403 with an empty message says the server gave no reason, with no hint", emptyForbidden, local,
			CodeRBAC, "the server refused the request (403) and gave no reason", "", false},
		{"403 with a whitespace-only message counts as empty", whitespaceForbidden, local,
			CodeRBAC, "the server refused the request (403) and gave no reason", "", false},
		{"bare empty 403", ResponseError(http.MethodGet, http.StatusForbidden, nil), nil,
			CodeRBAC, "the server refused the request (403) and gave no reason", "", false},
		{"empty 403 behind a prefix", fmt.Errorf("a: %w", ResponseError(http.MethodGet, http.StatusForbidden, nil)), nil,
			CodeRBAC, "a: the server refused the request (403) and gave no reason", "", false},
		{"empty 403 in the middle of a text is kept as is", fmt.Errorf("a: %w; b", ResponseError(http.MethodGet, http.StatusForbidden, nil)), nil,
			CodeRBAC, "a: ; b", "", false},
		{"415 with no reason names its status text", wrapped("creating derived token", ResponseError(http.MethodPost, http.StatusUnsupportedMediaType, nil)), nil,
			CodeGeneric, "creating derived token: Unsupported Media Type", "", false},
		{"415 whose body client-go did not keep names its status text", wrapped("creating derived token", unknown415), nil,
			CodeGeneric, "creating derived token: Unsupported Media Type", "", false},
		{"415 with a reason keeps it", wrapped("creating derived token", ResponseError(http.MethodPost, http.StatusUnsupportedMediaType, []byte("application/json only"))), nil,
			CodeGeneric, "creating derived token: application/json only", "", false},
		{"406 with no reason has apimachinery's sentence", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusNotAcceptable, nil)), nil,
			CodeGeneric, "GET /x: the server was unable to respond with a content type that the client supports", "", false},
		{"406 Status with no message names its status text", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusNotAcceptable, []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":406}`))), nil,
			CodeGeneric, "GET /x: Not Acceptable", "", false},
		{"a 403 reason holding the placeholder is kept", wrapped("GET /x", ResponseError(http.MethodGet, http.StatusForbidden, []byte(`user ("unknown") forbidden`))), nil,
			CodeRBAC, `GET /x: user ("unknown") forbidden`, "", false},
		{"429 is retryable", ResponseError(http.MethodGet, http.StatusTooManyRequests, nil), nil,
			CodeGeneric, "the server has received too many requests and has asked us to try again later - retry later", "", true},
		{"503 through the local cluster is plainly retryable", apierrors.NewServiceUnavailable("ClusterUnavailable 503: ClusterUnavailable 503"), local,
			CodeGeneric, "ClusterUnavailable 503: ClusterUnavailable 503 - retrying may succeed", "", true},
		{"503 with no cluster is plainly retryable", ResponseError(http.MethodGet, http.StatusServiceUnavailable, nil), &Request{},
			CodeGeneric, "the server is currently unable to handle the request - retrying may succeed", "", true},
		{"503 through a cluster names its agent", ResponseError(http.MethodGet, http.StatusServiceUnavailable, nil), &Request{Cluster: "c-1"},
			CodeGeneric, "the server is currently unable to handle the request - its agent may not be registered yet; retry once the cluster is active", "", true},
		{"504 through a cluster names its agent", ResponseError(http.MethodGet, http.StatusGatewayTimeout, nil), &Request{Cluster: "c-1"},
			CodeGeneric, "the server was unable to return a response in the time allotted, but may still be processing the request - its agent may not be registered yet; retry once the cluster is active", "", true},
		{"500 whose text is the agent disconnecting is retryable once it reconnects", typed500(nodes, "error trying to reach service: cluster agent disconnected"), local,
			CodeGeneric, `an error on the server ("error trying to reach service: cluster agent disconnected") has prevented the request from succeeding (get nodes) - retry once the cluster reconnects`, "", true},
		{"500 Status body from an API server is not retryable", apierrors.NewInternalError(errors.New("etcdserver: request timed out")), local,
			CodeGeneric, "Internal error occurred: etcdserver: request timed out", "", false},
		{"unexpected 500 through the local cluster is plainly retryable", typed500(nodes, "upstream connect error"), local,
			CodeGeneric, `an error on the server ("upstream connect error") has prevented the request from succeeding (get nodes) - retrying may succeed`, "", true},
		{"unexpected 500 through a cluster names the proxy path", typed500(nodes, "upstream connect error"), &Request{Cluster: "c-1"},
			CodeGeneric, `an error on the server ("upstream connect error") has prevented the request from succeeding (get nodes) - often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed`, "", true},
		{"empty 500 names its status text", ResponseError(http.MethodGet, http.StatusInternalServerError, nil), nil,
			CodeGeneric, `an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`, "", true},
		{"500 with client-go's unknown is no reason", typed500(schema.GroupResource{}, unknownMessage), &Request{},
			CodeGeneric, `an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`, "", true},
		{"a long reason is cut on a character boundary", ResponseError(http.MethodGet, http.StatusForbidden, []byte(strings.Repeat("x", 199)+"\u00e9\u00e9")), nil,
			CodeRBAC, strings.Repeat("x", 199) + "…", "", false},
		{"a reason of exactly 200 bytes is kept", ResponseError(http.MethodGet, http.StatusForbidden, []byte(strings.Repeat("x", 200))), nil,
			CodeRBAC, strings.Repeat("x", 200), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := FromStatus(tc.err, tc.req)
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, tc.retryable, ce.Retryable)
		})
	}
}

// TestTypedServerErrorShowsTheServersText asserts the text of a 5xx that
// client-go read as text, with the body quoted into apimachinery's
// message, is the server's own message the way a response read without
// client-go shows it: the message of Rancher's JSON error object (the
// cluster router's norman object and steve's, both served as text/plain),
// the first line of other text, at most 200 bytes, never the raw JSON; a
// body client-go did not keep (a non-text type) reads as the status text;
// a "unknown" inside the server's own text stays.
func TestTypedServerErrorShowsTheServersText(t *testing.T) {
	t.Parallel()
	const tail = `") has prevented the request from succeeding - retrying may succeed`
	nodes := schema.GroupResource{Resource: "nodes"}
	norman := `{"Code":{"Code":"ServerError","Status":500},"Message":"failed to find cluster agent","Cause":null,"FieldName":""}`
	steve := `{"type":"error","links":{},"code":"ServerError","message":"boom","status":500}`
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the router's norman object as text/plain", typed500(schema.GroupResource{}, norman), `an error on the server ("failed to find cluster agent` + tail},
		{"steve's object as text/plain", typed500(schema.GroupResource{}, steve), `an error on the server ("boom` + tail},
		{"steve's object served as application/json, which client-go does not keep", typed500(schema.GroupResource{}, unknownMessage), `an error on the server ("Internal Server Error` + tail},
		{"a JSON object with no message", typed500(schema.GroupResource{}, `{"code":"ServerError","status":500}`), `an error on the server ("Internal Server Error` + tail},
		{"JSON that is not an error object", typed500(schema.GroupResource{}, `[1,2]`), `an error on the server ("Internal Server Error` + tail},
		{"several lines give the first", typed500(schema.GroupResource{}, "upstream connect error\nreset before headers\n"), `an error on the server ("upstream connect error` + tail},
		{"a long line is cut at 200 bytes", typed500(schema.GroupResource{}, strings.Repeat("y", 300)), `an error on the server ("` + strings.Repeat("y", maxServerMessage) + `…` + tail},
		{"a text with the placeholder inside is kept", typed500(schema.GroupResource{}, `a ("unknown") b`), `an error on the server ("a (\"unknown\") b` + tail},
		{"the verb and resource after the text are kept", typed500(nodes, norman), `an error on the server ("failed to find cluster agent") has prevented the request from succeeding (get nodes) - retrying may succeed`},
		{"a prefix is kept", wrapped("GET /x", typed500(schema.GroupResource{}, norman)), `GET /x: an error on the server ("failed to find cluster agent` + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := FromStatus(tc.err, &Request{})
			require.NotNil(t, ce)
			assert.Equal(t, CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable)
			assert.Equal(t, tc.want, ce.Message)
			assert.NotContains(t, ce.Message, `{"`)
		})
	}
}

// TestTypedServerErrorKeepsWhatItCannotRebuild asserts a 5xx whose text
// does not have apimachinery's form is shown as it is.
func TestTypedServerErrorKeepsWhatItCannotRebuild(t *testing.T) {
	t.Parallel()
	se := typed500(schema.GroupResource{}, "boom").(*apierrors.StatusError)
	se.ErrStatus.Message = `something else ("unknown")`
	ce := FromStatus(se, nil)
	require.NotNil(t, ce)
	assert.Equal(t, `something else ("unknown") - retrying may succeed`, ce.Message)
}

// TestFromStatusKeepsAnEmptyTextWhole asserts the text of a status that
// has none: standing alone or ending the text after a prefix it is worded
// as a refusal with no reason; anywhere else the text is kept as is.
func TestFromStatusKeepsAnEmptyTextWhole(t *testing.T) {
	t.Parallel()
	ce := FromStatus(fmt.Errorf("a: %w; b", ResponseError(http.MethodGet, http.StatusForbidden, nil)), &Request{Verb: "get", Resource: "nodes"})
	require.NotNil(t, ce)
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, "a: ; b", ce.Message)
}

// TestTypedBadGatewayShowsTheServersText asserts a typed 502, whose body
// apimachinery quotes into its text as for a 500, shows the server's text
// the same way: the JSON object's message, or the status text where
// client-go kept no body.
func TestTypedBadGatewayShowsTheServersText(t *testing.T) {
	t.Parallel()
	norman := `{"Code":{"Code":"ServerError","Status":502},"Message":"failed to find cluster agent"}`
	for _, tc := range []struct{ body, want string }{
		{norman, `an error on the server ("failed to find cluster agent") has prevented the request from succeeding - retrying may succeed`},
		{unknownMessage, `an error on the server ("Bad Gateway") has prevented the request from succeeding - retrying may succeed`},
	} {
		err := apierrors.NewGenericServerResponse(http.StatusBadGateway, "get", schema.GroupResource{}, "", tc.body, 0, true)
		ce := FromStatus(err, &Request{})
		require.NotNil(t, ce)
		assert.Equal(t, CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, tc.want, ce.Message)
	}
}

// TestNoReasonStatusReadsItsStatusTextInEveryArm asserts a status that
// gave no reason reads its status text after the text in front of it, in
// every arm that shows the message, with one space between them; "status
// <code>" where the code has no standard text. The 5xx that client-go
// reads as text quotes the text into apimachinery's sentence.
func TestNoReasonStatusReadsItsStatusTextInEveryArm(t *testing.T) {
	t.Parallel()
	statusBody := func(code int, reason string) string {
		body := fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":%d`, code)
		if reason != "" {
			body += fmt.Sprintf(`,"reason":%q`, reason)
		}
		return body + "}"
	}
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		code      string
		message   string
		retryable bool
	}{
		{"400", http.StatusBadRequest, statusBody(400, ""), CodeValidation, "p: Bad Request", false},
		{"404", http.StatusNotFound, statusBody(404, ""), CodeNotFound, "p: Not Found", false},
		{"422", http.StatusUnprocessableEntity, statusBody(422, ""), CodeValidation, "p: Unprocessable Entity", false},
		{"already exists", http.StatusConflict, statusBody(409, "AlreadyExists"), CodeConflict, "p: Conflict", false},
		{"429", http.StatusTooManyRequests, statusBody(429, ""), CodeGeneric, "p: Too Many Requests - retry later", true},
		{"502", http.StatusBadGateway, statusBody(502, ""), CodeGeneric, "p: Bad Gateway - retrying may succeed", true},
		{"503", http.StatusServiceUnavailable, statusBody(503, ""), CodeGeneric, "p: Service Unavailable - retrying may succeed", true},
		{"504", http.StatusGatewayTimeout, statusBody(504, ""), CodeGeneric, "p: Gateway Timeout - retrying may succeed", true},
		{"a status with no standard text", 599, statusBody(599, ""), CodeGeneric, "p: status 599", false},
		{"406", http.StatusNotAcceptable, statusBody(406, ""), CodeGeneric, "p: Not Acceptable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := FromResponse("p", http.MethodGet, tc.status, []byte(tc.body), &Request{})
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.NotContains(t, ce.Message, "  ")
		})
	}
}

// TestTypedAndUntypedServerTextAreTheSame asserts a 5xx that is not a
// Status reads the same server text whether client-go built it (the body
// of a text answer quoted whole into apimachinery's sentence) or this
// package did from the body: the whole message of a JSON error object, the
// first line of other text, taken from the body once; the status text, or
// "status <code>", where the body gives none.
func TestTypedAndUntypedServerTextAreTheSame(t *testing.T) {
	t.Parallel()
	const tail = `") has prevented the request from succeeding - retrying may succeed`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a multi-line message of a JSON object is kept whole", 500, `{"message":"line1\nline2"}`, `p: an error on the server ("line1\nline2` + tail},
		{"the first line of a text body that starts with JSON", 500, "\"quoted\"\nmore", `p: an error on the server ("\"quoted\"` + tail},
		{"a JSON object's message that is itself JSON", 500, `{"message":"\"quoted\""}`, `p: an error on the server ("\"quoted\"` + tail},
		{"a JSON object's message that is valid JSON", 500, `{"message":"[1,2]"}`, `p: an error on the server ("[1,2]` + tail},
		{"a JSON object's message cut at 200 bytes", 500, `{"message":"` + strings.Repeat("y", 300) + `"}`, `p: an error on the server ("` + strings.Repeat("y", maxServerMessage) + `…` + tail},
		{"a body that gives no message names its status text", 500, `{"code":"ServerError"}`, `p: an error on the server ("Internal Server Error` + tail},
		{"a body that is client-go's placeholder reads as no text", 500, "unknown", `p: an error on the server ("Internal Server Error` + tail},
		{"no body names its status text", 502, "", `p: an error on the server ("Bad Gateway` + tail},
		{"no body on a status with no standard text", 599, "", `p: an error on the server ("status 599` + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typed := apierrors.NewGenericServerResponse(tc.status, http.MethodGet, schema.GroupResource{}, "", strings.TrimSpace(tc.body), 0, true)
			ce := FromStatus(wrapped("p", typed), &Request{})
			require.NotNil(t, ce)
			assert.Equal(t, tc.want, ce.Message, "typed")
			assert.True(t, ce.Retryable)

			ce = FromResponse("p", http.MethodGet, tc.status, []byte(tc.body), &Request{})
			assert.Equal(t, tc.want, ce.Message, "untyped")
			assert.True(t, ce.Retryable)
		})
	}
}

// TestTypedNoReasonStatusReadsAReason asserts the answers client-go builds
// from a text body that is a Status with no message, or from no body,
// read a reason after the text in front of them, with no double space.
func TestTypedNoReasonStatusReadsAReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		message string
	}{
		{"502", http.StatusBadGateway, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":502}`, `p: an error on the server ("Bad Gateway") has prevented the request from succeeding - retrying may succeed`},
		{"429", http.StatusTooManyRequests, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":429}`, "p: the server has received too many requests and has asked us to try again later - retry later"},
		{"599", 599, "", `p: an error on the server ("status 599") has prevented the request from succeeding - retrying may succeed`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := apierrors.NewGenericServerResponse(tc.status, http.MethodGet, schema.GroupResource{}, "", tc.body, 0, true)
			ce := FromStatus(wrapped("p", err), &Request{})
			require.NotNil(t, ce)
			assert.Equal(t, tc.message, ce.Message)
			assert.True(t, ce.Retryable)
			assert.NotContains(t, ce.Message, "  ")
		})
	}
}
