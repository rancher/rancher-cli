package clierr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// projectsWebhook is the name of rancher-webhook's webhook for projects.
const projectsWebhook = "rancher.cattle.io.projects.management.cattle.io"

// webhookLead is the start of the API server's 500 for projectsWebhook.
const webhookLead = `Internal error occurred: failed calling webhook "rancher.cattle.io.projects.management.cattle.io"`

// unknownBody is apimachinery's text for a 5xx with the response body shown
// as "unknown".
const unknownBody = `an error on the server ("unknown") has prevented the request from succeeding`

// projectWebhookMessage is the message rancher-webhook gives when a
// project's namespace already exists.
const projectWebhookMessage = "Internal error occurred: failed to create project namespace : namespace c-x-p-x already exists"

// projectText is the CLI's message for that response.
const projectText = "admission webhook 'rancher.cattle.io.projects.management.cattle.io' failed: " + projectWebhookMessage

// webhookServiceURL is the URL the API server posts the review to.
const webhookServiceURL = "https://rancher-webhook.cattle-system.svc:443/v1/webhook/mutation/projects.management.cattle.io?timeout=10s"

// callerPrefix is the text a caller puts in front of the status.
const callerPrefix = "create projects.management.cattle.io"

// refusedDial is the dialer's error for a refused connection.
const refusedDial = "dial tcp 10.43.0.10:443: connect: connection refused"

// objectData is every value of the objects a webhook was sent; none of them
// may appear in an error.
var objectData = []string{
	"c2VjcmV0LWRhdGEtdmFsdWU=",
	"plain-secret-value",
	"token-like-value-0123456789",
	"from-the-object",
}

// localRequest is the project create through the local cluster.
func localRequest() *clierr.Request {
	return &clierr.Request{
		Verb: "create", Group: "management.cattle.io", Resource: "projects", Name: "p-x",
		Cluster: "local", Server: "https://rancher.example.com",
	}
}

// logsHint is the hint that shows rancher-webhook's logs.
func logsHint() string { return clierr.NewCommand("logs webhook").Field() }

// callingWebhook is a copy of k8s.io/apiserver's ErrCallingWebhook with the
// same Error text.
type callingWebhook struct {
	name   string
	reason error
}

func (e callingWebhook) Error() string {
	if e.reason != nil {
		return fmt.Sprintf("failed calling webhook %q: %v", e.name, e.reason)
	}
	return fmt.Sprintf("failed calling webhook %q; no further details available", e.name)
}

// apiServer500 is the API server's 500 for a failed admission webhook call.
func apiServer500(name string, reason error) *apierrors.StatusError {
	return apierrors.NewInternalError(callingWebhook{name: name, reason: reason})
}

// restError is client-go's error for a response body that is not a Status:
// the body cut to 2048 bytes and trimmed, or "unknown" when the content
// type of the response is not text.
func restError(code int, body []byte, text bool, gr schema.GroupResource) error {
	if len(body) > 2048 {
		body = body[:2048]
	}
	message := "unknown"
	if text {
		message = strings.TrimSpace(string(body))
	}
	return apierrors.NewGenericServerResponse(code, http.MethodPost, gr, "", message, 0, true)
}

// webhookResponse is the webhook error the API server gives when the
// webhook responded with status code and body.
func webhookResponse(code int, body []byte, text bool) error {
	return fmt.Errorf("failed to call webhook: %w", restError(code, body, text, schema.GroupResource{}))
}

// transport is the webhook error the API server gives when the POST
// request to the webhook failed with err.
func transport(err error) error {
	return transportTo(webhookServiceURL, err)
}

// transportTo is transport for a POST request to target.
func transportTo(target string, err error) error {
	return fmt.Errorf("failed to call webhook: %w", &url.Error{Op: "Post", URL: target, Err: err})
}

// shownReason is the webhook error as the message shows it: cut to 200
// bytes, with U+2026 (an ellipsis) added when it was longer. Every webhook
// error it is given is ASCII.
func shownReason(t *testing.T, reason string) string {
	t.Helper()
	for _, r := range reason {
		require.Less(t, r, rune(0x80), "the webhook error is ASCII")
	}
	if len(reason) <= 200 {
		return reason
	}
	return reason[:200] + "\u2026"
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// fixtureMeta is the metadata of a fixture object; filler goes last.
type fixtureMeta struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// dataObject is the first fixture object, its data fields first.
type dataObject struct {
	Data       map[string]string `json:"data"`
	StringData map[string]string `json:"stringData"`
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Metadata   fixtureMeta       `json:"metadata"`
}

// tokenObject is the second fixture object, its token field first.
type tokenObject struct {
	Token      string      `json:"token"`
	Kind       string      `json:"kind"`
	APIVersion string      `json:"apiVersion"`
	Metadata   fixtureMeta `json:"metadata"`
}

// projectObject is the project the create sends.
type projectObject struct {
	Kind       string      `json:"kind"`
	APIVersion string      `json:"apiVersion"`
	Metadata   fixtureMeta `json:"metadata"`
	Spec       projectSpec `json:"spec"`
}

type projectSpec struct {
	DisplayName string `json:"displayName"`
}

// fixture names a fixture object.
type fixture int

const (
	firstFixture fixture = iota
	secondFixture
)

var fixtures = []fixture{firstFixture, secondFixture}

func (f fixture) String() string { return "fixture " + strconv.Itoa(int(f)+1) }

// fixtureObject is the JSON of fixture f, with filler in its last field
// when filler is not empty.
func fixtureObject(t *testing.T, f fixture, filler string) []byte {
	t.Helper()
	meta := fixtureMeta{Name: "o-x"}
	if filler != "" {
		meta.Annotations = map[string]string{"filler": filler}
	}
	if f == secondFixture {
		return mustJSON(t, tokenObject{
			Token: "token-like-value-0123456789", Kind: "Token", APIVersion: "management.cattle.io/v3", Metadata: meta,
		})
	}
	return mustJSON(t, dataObject{
		Data:       map[string]string{"token": "c2VjcmV0LWRhdGEtdmFsdWU="},
		StringData: map[string]string{"password": "plain-secret-value", "message": "from-the-object"},
		Kind:       "Secret", APIVersion: "v1", Metadata: meta,
	})
}

// projectJSON is the JSON of the project the create sends.
func projectJSON(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, projectObject{
		Kind: "Project", APIVersion: "management.cattle.io/v3",
		Metadata: fixtureMeta{Name: "p-x"}, Spec: projectSpec{DisplayName: "x"},
	})
}

// reviewOrder is the order of the fields of an AdmissionReview body.
type reviewOrder int

const (
	goOrder reviewOrder = iota
	objectOnlyOrder
	objectFirstOrder
)

var reviewOrders = []reviewOrder{goOrder, objectOnlyOrder, objectFirstOrder}

func (o reviewOrder) String() string {
	return [...]string{"fields in Go struct order", "a request with only an object field", "object field first"}[o]
}

type objectOnlyRequest struct {
	Object json.RawMessage `json:"object"`
}

type objectOnlyReview struct {
	Kind       string                         `json:"kind"`
	APIVersion string                         `json:"apiVersion"`
	Request    objectOnlyRequest              `json:"request"`
	Response   *admissionv1.AdmissionResponse `json:"response"`
}

type objectFirstRequest struct {
	Object    json.RawMessage `json:"object"`
	UID       string          `json:"uid"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Operation string          `json:"operation"`
}

type objectFirstReview struct {
	Request    objectFirstRequest             `json:"request"`
	Kind       string                         `json:"kind"`
	APIVersion string                         `json:"apiVersion"`
	Response   *admissionv1.AdmissionResponse `json:"response"`
}

// admissionReview is the body a webhook writes with json.Encoder: the
// review it was sent, with object as the object of its request, and a
// response whose status is status (no status when it is nil).
func admissionReview(t *testing.T, order reviewOrder, object []byte, status *metav1.Status) []byte {
	t.Helper()
	response := &admissionv1.AdmissionResponse{UID: "a", Allowed: false, Result: status}
	var review any
	switch order {
	case objectOnlyOrder:
		review = objectOnlyReview{
			Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1",
			Request: objectOnlyRequest{Object: object}, Response: response,
		}
	case objectFirstOrder:
		review = objectFirstReview{
			Request: objectFirstRequest{Object: object, UID: "a", Name: "p-x", Namespace: "c-x", Operation: "CREATE"},
			Kind:    "AdmissionReview", APIVersion: "admission.k8s.io/v1", Response: response,
		}
	default:
		gvk := metav1.GroupVersionKind{Group: "management.cattle.io", Version: "v3", Kind: "Project"}
		gvr := metav1.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}
		review = admissionv1.AdmissionReview{
			Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1",
			Request: &admissionv1.AdmissionRequest{
				UID: "a", Kind: gvk, Resource: gvr, RequestKind: &gvk, RequestResource: &gvr,
				Name: "p-x", Namespace: "c-x", Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{Username: "u-x"},
				Object:   kruntime.RawExtension{Raw: object},
			},
			Response: response,
		}
	}
	return append(mustJSON(t, review), '\n')
}

// projectStatus is the status rancher-webhook writes when the project's
// namespace already exists.
func projectStatus() *metav1.Status {
	st := apierrors.NewInternalError(fmt.Errorf("failed to create project namespace %s: %w", "", fmt.Errorf("namespace %v already exists", "c-x-p-x"))).ErrStatus
	st.Code = http.StatusInternalServerError
	return &st
}

// messageStatus is a response status with message.
func messageStatus(message string) *metav1.Status {
	return &metav1.Status{Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Message: message}
}

// projectReview is the review for the project create, in Go order.
func projectReview(t *testing.T) []byte {
	t.Helper()
	return admissionReview(t, goOrder, projectJSON(t), projectStatus())
}

// fixtureReview is a review whose object is fixture f, short enough that
// client-go keeps it whole.
func fixtureReview(t *testing.T, order reviewOrder, f fixture) []byte {
	t.Helper()
	body := admissionReview(t, order, fixtureObject(t, f, ""), projectStatus())
	require.LessOrEqual(t, len(body), 2048, "the review is kept whole")
	return body
}

// cutReview is a review whose object is fixture f, long enough that
// client-go cuts it inside the request.
func cutReview(t *testing.T, order reviewOrder, f fixture) []byte {
	t.Helper()
	body := admissionReview(t, order, fixtureObject(t, f, strings.Repeat("f", 2500)), projectStatus())
	require.Greater(t, bytes.Index(body, []byte(`"response"`)), 2048, "the cut falls inside the request")
	if f == firstFixture {
		require.Contains(t, string(body[:2048]), "from-the-object", "the cut falls after the last data field")
	}
	return body
}

// conversionRequest and conversionReview are a ConversionReview body.
type conversionRequest struct {
	UID               string            `json:"uid"`
	DesiredAPIVersion string            `json:"desiredAPIVersion"`
	Objects           []json.RawMessage `json:"objects"`
}

type conversionReview struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Request    conversionRequest `json:"request"`
}

// conversionError is apiextensions-apiserver's error for a conversion
// webhook that responded with status code and body.
func conversionError(code int, body []byte) error {
	gvk := schema.GroupVersionKind{Group: "stable.example.com", Version: "v1", Kind: "Widget"}
	return fmt.Errorf("conversion webhook for %v failed: %v", gvk, restError(code, body, true, schema.GroupResource{}))
}

// unknownStatus is the API server's 500 for an error that is not a
// Status: reason Unknown and the error's text.
func unknownStatus(err error) *apierrors.StatusError {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Message: err.Error(),
	}}
}

// deniedBy is k8s.io/apiserver's ToStatusErr.
func deniedBy(webhookName string, result *metav1.Status) *apierrors.StatusError {
	denied := fmt.Sprintf("admission webhook %q denied the request", webhookName)
	if result == nil {
		result = &metav1.Status{Status: metav1.StatusFailure}
	}
	if result.Code < http.StatusBadRequest {
		result.Code = http.StatusBadRequest
	}
	if result.Status == "" || result.Status == metav1.StatusSuccess {
		result.Status = metav1.StatusFailure
	}
	switch {
	case len(result.Message) > 0:
		result.Message = fmt.Sprintf("%s: %s", denied, result.Message)
	case len(result.Reason) > 0:
		result.Message = fmt.Sprintf("%s: %s", denied, result.Reason)
	default:
		result.Message = fmt.Sprintf("%s %s", denied, "without explanation")
	}
	return &apierrors.StatusError{ErrStatus: *result}
}

// quotedBody is body quoted into apimachinery's text for a 5xx.
func quotedBody(body []byte) string {
	return fmt.Sprintf("an error on the server (%q) has prevented the request from succeeding", strings.TrimSpace(string(body)))
}

// rancherErrorObject is Rancher's own JSON error object.
type rancherErrorObject struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// webhookWant is what a row expects of the error.
type webhookWant struct {
	code      string
	message   string
	retryable bool
	hint      string
}

func generic(message string) webhookWant {
	return webhookWant{code: clierr.CodeGeneric, message: message}
}

// webhookRow is one input of the table and what it expects.
type webhookRow struct {
	name       string
	classify   func() *clierr.Error
	want       webhookWant
	objectData bool
}

// wireStatus is the Status body the API server writes for se.
func wireStatus(t *testing.T, se *apierrors.StatusError) []byte {
	t.Helper()
	st := se.ErrStatus
	st.Kind, st.APIVersion = "Status", "v1"
	return mustJSON(t, st)
}

// typedRow classifies err as client-go returns it (the typed form).
func typedRow(name string, err error, req *clierr.Request, want webhookWant, objectData bool) webhookRow {
	return webhookRow{name, func() *clierr.Error { return clierr.FromStatus(err, req) }, want, objectData}
}

// wireRow classifies se as read from the Status body a server sent (the
// wire form).
func wireRow(t *testing.T, name string, se *apierrors.StatusError, req *clierr.Request, want webhookWant, objectData bool) webhookRow {
	t.Helper()
	body := wireStatus(t, se)
	code := int(se.ErrStatus.Code)
	return webhookRow{name + " on the wire", func() *clierr.Error {
		return clierr.FromResponse("", http.MethodPost, code, body, req)
	}, want, objectData}
}

// onTwoPaths is the typed row and the wire row of se.
func onTwoPaths(t *testing.T, name string, se *apierrors.StatusError, req *clierr.Request, want webhookWant, objectData bool) []webhookRow {
	t.Helper()
	return []webhookRow{
		typedRow(name, se, req, want, objectData),
		wireRow(t, name, se, req, want, objectData),
	}
}

// onThreePaths is onTwoPaths and a third row, se behind the caller's
// prefix, which the message keeps in front.
func onThreePaths(t *testing.T, name string, se *apierrors.StatusError, req *clierr.Request, want webhookWant, objectData bool) []webhookRow {
	t.Helper()
	prefixed := want
	prefixed.message = callerPrefix + ": " + want.message
	return append(onTwoPaths(t, name, se, req, want, objectData),
		typedRow(name+" behind a prefix", fmt.Errorf(callerPrefix+": %w", se), req, prefixed, objectData))
}

// envelopeStrings is every string of a decoded JSON document, keys and
// values.
func envelopeStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = append(out, envelopeStrings(e)...)
		}
	case map[string]any:
		for k, e := range x {
			out = append(out, k)
			out = append(out, envelopeStrings(e)...)
		}
	}
	return out
}

func assertNoObjectData(t *testing.T, ce *clierr.Error) {
	t.Helper()
	var buf bytes.Buffer
	runtime.WriteError(&buf, "json", ce, style.Style{}, "")
	var doc any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	strs := envelopeStrings(doc)
	require.NotEmpty(t, strs)
	for _, v := range objectData {
		assert.NotContains(t, ce.Message, v, "object data never shown")
		assert.NotContains(t, ce.Hint, v, "object data never shown")
		assert.NotContains(t, fmt.Sprint(ce.Details), v, "object data never shown")
		for _, s := range strs {
			assert.NotContains(t, s, v, "object data never shown")
		}
	}
}

// TestWebhookCallErrorTexts asserts the error for a failed admission
// webhook call and for every error text that holds a response body in
// apimachinery's quoted form. The texts are built as k8s.io/apiserver,
// k8s.io/apiextensions-apiserver, k8s.io/apimachinery and k8s.io/client-go
// write them. It asserts that a webhook's own message is shown, cut to 200
// bytes when it is longer; that a response body is replaced by "unknown";
// that the data of the objects the write sent never appears in the error;
// that a failed admission webhook call is retryable only when it did not
// reach the webhook or came back with a 503 that is not a Status; and
// that, for rancher-webhook's own webhooks on the local cluster, a call
// that did not reach the webhook, timed out, or came back with a 503 or
// 504 that is not a Status gets the hint for rancher-webhook's logs.
func TestWebhookCallErrorTexts(t *testing.T) {
	t.Parallel()
	var rows []webhookRow
	rows = append(rows, denialRows(t)...)
	rows = append(rows, respondedRows(t)...)
	rows = append(rows, failedCallRows(t)...)
	rows = append(rows, nameRows(t)...)
	rows = append(rows, messageFormRows(t)...)
	rows = append(rows, defaultRows(t)...)
	rows = append(rows, perCodeRows(t)...)
	rows = append(rows, responseBodyPathRows(t)...)
	names := map[string]bool{}
	for _, row := range rows {
		require.False(t, names[row.name], "row names are unique: %s", row.name)
		names[row.name] = true
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ce := row.classify()
			require.NotNil(t, ce)
			assert.Equal(t, row.want.code, ce.Code, "code")
			assert.Equal(t, row.want.retryable, ce.Retryable, "retryable")
			assert.Equal(t, row.want.message, ce.Message, "message")
			assert.Equal(t, row.want.hint, ce.Hint, "hint")
			assert.Empty(t, ce.Candidates, "candidates")
			assert.Nil(t, ce.Details, "details")
			assert.False(t, clierr.HasRemedy(ce.Message), "the message holds no remedy command")
			shown := output.Printable(ce.Message)
			assert.False(t, strings.ContainsFunc(shown, clierr.IsUnsafeToDisplay), "the shown message holds no unsafe rune")
			assert.False(t, strings.ContainsAny(shown, "\n\r\x1b"), "the shown message holds no control character")
			if row.objectData {
				assertNoObjectData(t, ce)
			}
		})
	}
}

// denialRows are webhook denials, which keep the arm and text of their
// code.
func denialRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	const denied = `admission webhook "x.example.io" denied the request`
	codes := []struct {
		name string
		code int32
		want string
	}{
		{"400", http.StatusBadRequest, clierr.CodeValidation},
		{"403", http.StatusForbidden, clierr.CodeRBAC},
		{"409", http.StatusConflict, clierr.CodeConflict},
		{"422", http.StatusUnprocessableEntity, clierr.CodeValidation},
		{"500", http.StatusInternalServerError, clierr.CodeGeneric},
		{"200 raised to 400", http.StatusOK, clierr.CodeValidation},
	}
	for _, c := range codes {
		forms := []struct {
			name   string
			result metav1.Status
			want   string
		}{
			{"with a message", metav1.Status{Code: c.code, Message: "the object is not allowed"}, denied + ": the object is not allowed"},
			{"with a reason only", metav1.Status{Code: c.code, Reason: "PolicyViolation"}, denied + ": PolicyViolation"},
			{"with neither", metav1.Status{Code: c.code}, denied + " without explanation"},
		}
		for _, f := range forms {
			result := f.result
			se := deniedBy("x.example.io", &result)
			rows = append(rows, onThreePaths(t, "denial "+c.name+" "+f.name, se, localRequest(),
				webhookWant{code: c.want, message: f.want}, false)...)
		}
	}
	precondition := localRequest()
	precondition.Precondition = true
	se := deniedBy("x.example.io", &metav1.Status{Code: http.StatusConflict, Message: "the object is not allowed"})
	want := webhookWant{code: clierr.CodeConflict, message: denied + ": the object is not allowed"}
	rows = append(rows, onTwoPaths(t, "denial 409 on a write with a precondition keeps the webhook's message", se, precondition, want, false)...)
	want.message = callerPrefix + ": " + want.message
	rows = append(rows, typedRow("denial 409 on a write with a precondition keeps the webhook's message behind a prefix",
		fmt.Errorf(callerPrefix+": %w", se), precondition, want, false))
	dryRun := apierrors.NewBadRequest(`admission webhook "x.example.io" does not support dry run`)
	rows = append(rows, onThreePaths(t, "dry run unsupported", dryRun, localRequest(),
		webhookWant{code: clierr.CodeValidation, message: `admission webhook "x.example.io" does not support dry run`}, false)...)
	return rows
}

// respondedRows are calls to a webhook that responded with a response body.
func respondedRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	cutText := webhookLead + ": failed to call webhook: " + unknownBody
	rows = append(rows, onThreePaths(t, "responded, the project case",
		apiServer500(projectsWebhook, webhookResponse(500, projectReview(t), true)), localRequest(), generic(projectText), false)...)
	for _, f := range fixtures {
		for _, order := range reviewOrders {
			se := apiServer500(projectsWebhook, webhookResponse(500, fixtureReview(t, order, f), true))
			rows = append(rows, onThreePaths(t, "responded, "+f.String()+", "+order.String()+", object data never shown",
				se, localRequest(), generic(projectText), true)...)
		}
	}
	for _, f := range fixtures {
		for _, order := range reviewOrders {
			reason := webhookResponse(500, cutReview(t, order, f), true)
			if order != goOrder {
				early := reason.Error()[:200]
				found := false
				for _, v := range objectData {
					found = found || strings.Contains(early, v)
				}
				require.True(t, found, "object data lies within the first 200 bytes of the webhook error")
			}
			rows = append(rows, onThreePaths(t, "responded with a cut review, "+f.String()+", "+order.String()+", object data never shown",
				apiServer500(projectsWebhook, reason), localRequest(), generic(cutText), true)...)
		}
	}
	for _, f := range fixtures {
		var body []byte
		for pad := range 2 {
			filler := strings.Repeat("x", pad) + strings.Repeat("\u00e9", 1500)
			body = admissionReview(t, objectFirstOrder, fixtureObject(t, f, filler), projectStatus())
			if body[2047] == 0xc3 {
				break
			}
		}
		require.Equal(t, byte(0xc3), body[2047], "the cut splits a character")
		reason := webhookResponse(500, body, true)
		require.Contains(t, reason.Error(), `\xc3`, "the quoted text holds the stray byte")
		rows = append(rows, onThreePaths(t, "responded with a review cut inside a character, "+f.String()+", object data never shown",
			apiServer500(projectsWebhook, reason), localRequest(), generic(cutText), true)...)
	}
	plain := []struct {
		name string
		code int
		body string
		want string
	}{
		{"plain text", 500, "namespace c-x-p-x already exists\n", "namespace c-x-p-x already exists"},
		{"plain text of two lines", 500, "namespace c-x-p-x already exists\nsecond line\n", "namespace c-x-p-x already exists"},
		{"a 502 from a proxy", 502, "Bad Gateway", "Bad Gateway"},
	}
	for _, p := range plain {
		rows = append(rows, onThreePaths(t, "responded with "+p.name,
			apiServer500(projectsWebhook, webhookResponse(p.code, []byte(p.body), true)), localRequest(),
			generic("admission webhook '"+projectsWebhook+"' failed: "+p.want), false)...)
	}
	for _, body := range []string{"[ERROR] boom", "{oops"} {
		rows = append(rows, onThreePaths(t, "responded with plain text starting "+body[:1],
			apiServer500(projectsWebhook, webhookResponse(500, []byte(body), true)), localRequest(), generic(cutText), false)...)
	}
	rows = append(rows, onThreePaths(t, "responded with an empty body",
		apiServer500(projectsWebhook, webhookResponse(500, nil, true)), localRequest(),
		generic(webhookLead+`: failed to call webhook: an error on the server ("") has prevented the request from succeeding`), false)...)
	rows = append(rows, onThreePaths(t, "responded with a content type that is not text",
		apiServer500(projectsWebhook, webhookResponse(500, projectReview(t), false)), localRequest(), generic(cutText), false)...)
	noMessage := []struct {
		name string
		body []byte
	}{
		{"a review with no response status", admissionReview(t, goOrder, projectJSON(t), nil)},
		{"a response that is a string", []byte(`{"response":"x"}`)},
		{"a null response status", []byte(`{"response":{"status":null}}`)},
		{"a status message that is a number", []byte(`{"response":{"status":{"message":1}}}`)},
		{"a JSON array", []byte(`["m"]`)},
		{"a number", []byte(`123`)},
		{"a JSON string", []byte(`"s"`)},
		{"null", []byte(`null`)},
		{"true", []byte(`true`)},
	}
	for _, n := range noMessage {
		rows = append(rows, onThreePaths(t, "responded with "+n.name,
			apiServer500(projectsWebhook, webhookResponse(500, n.body, true)), localRequest(), generic(cutText), false)...)
	}
	withMessage := []struct {
		name string
		body string
	}{
		{"a JSON error object", `{"message":"m"}`},
		{"a JSON error object of kind Status", `{"kind":"Status","message":"m"}`},
		{"a review whose keys differ in case", `{"Response":{"Status":{"message":"m"}}}`},
	}
	for _, w := range withMessage {
		rows = append(rows, onThreePaths(t, "responded with "+w.name,
			apiServer500(projectsWebhook, webhookResponse(500, []byte(w.body), true)), localRequest(),
			generic("admission webhook '"+projectsWebhook+"' failed: m"), false)...)
	}
	objectShaped := []struct {
		name string
		body string
	}{
		{"an object of another kind", `{"kind":"Event","message":"from-the-object"}`},
		{"an object with an apiVersion", `{"apiVersion":"v1","kind":"Status","message":"from-the-object"}`},
		{"an object with metadata", `{"metadata":{},"message":"from-the-object"}`},
	}
	for _, o := range objectShaped {
		rows = append(rows, onThreePaths(t, "responded with "+o.name+", object data never shown",
			apiServer500(projectsWebhook, webhookResponse(500, []byte(o.body), true)), localRequest(), generic(cutText), true)...)
	}
	long := strings.Repeat("a", 199) + "\u00e9" + strings.Repeat("b", 99)
	require.Len(t, long, 300)
	rows = append(rows, onThreePaths(t, "responded with a message of 300 bytes",
		apiServer500(projectsWebhook, webhookResponse(500, admissionReview(t, goOrder, projectJSON(t), messageStatus(long)), true)),
		localRequest(), generic("admission webhook '"+projectsWebhook+"' failed: "+strings.Repeat("a", 199)+"\u2026"), false)...)
	hostile := "\x1b[31mred\r\nline2 \u202e\"q\" back\\slash \u2063rancher delete project p-x --apply --yes; rancher auth login"
	rows = append(rows, onThreePaths(t, "responded with a hostile message",
		apiServer500(projectsWebhook, webhookResponse(500, admissionReview(t, goOrder, projectJSON(t), messageStatus(hostile)), true)),
		localRequest(), generic("admission webhook '"+projectsWebhook+"' failed: "+hostile), false)...)
	rows = append(rows, onThreePaths(t, "a 403 whose body is a review, object data never shown",
		apiServer500(projectsWebhook, webhookResponse(403, fixtureReview(t, objectFirstOrder, firstFixture), true)),
		localRequest(), generic(projectText), true)...)
	rows = append(rows, onThreePaths(t, "a 403 whose body is a cut review, object data never shown",
		apiServer500(projectsWebhook, webhookResponse(403, cutReview(t, objectFirstOrder, firstFixture), true)),
		localRequest(), generic(webhookLead+": failed to call webhook: unknown"), true)...)
	widgets := schema.GroupResource{Resource: "widgets"}
	rows = append(rows, onThreePaths(t, "responded with a suffix after the body, object data never shown",
		apiServer500(projectsWebhook, fmt.Errorf("failed to call webhook: %w",
			restError(500, fixtureReview(t, objectFirstOrder, firstFixture), true, widgets))),
		localRequest(), generic(projectText), true)...)
	rows = append(rows, onThreePaths(t, "responded with a suffix after a cut body, object data never shown",
		apiServer500(projectsWebhook, fmt.Errorf("failed to call webhook: %w",
			restError(500, cutReview(t, objectFirstOrder, firstFixture), true, widgets))),
		localRequest(), generic(cutText+" (post widgets)"), true)...)
	rows = append(rows, onThreePaths(t, "an unterminated response body",
		apiServer500(projectsWebhook, errors.New(`failed to call webhook: an error on the server ("{\"kind\":\"AdmissionReview\"`)),
		localRequest(), generic(cutText), false)...)
	rows = append(rows, onThreePaths(t, "the words before a response body, followed by no quote",
		apiServer500(projectsWebhook, errors.New(`failed to call webhook: an error on the server (x) has prevented the request from succeeding`)),
		localRequest(), generic(cutText), false)...)
	rows = append(rows, wireCapRows(t)...)
	rows = append(rows, onThreePaths(t, "a call that failed with no text after the colon",
		apiServer500(projectsWebhook, errors.New("failed to call webhook:")), localRequest(),
		generic(webhookLead+": failed to call webhook:"), false)...)
	return rows
}

// wireCapRows are the size limits on a response body, for an error read
// from a Status body that a server sent, where a body of any length can
// arrive.
func wireCapRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	cutText := webhookLead + ": failed to call webhook: " + unknownBody
	sized := func(filler int) []byte {
		return admissionReview(t, objectFirstOrder, fixtureObject(t, firstFixture, strings.Repeat("f", filler)), projectStatus())
	}
	over := bytes.TrimSpace(sized(3000))
	require.Greater(t, len(over), 2048)
	require.LessOrEqual(t, len(over), 8192)
	require.LessOrEqual(t, len(strconv.Quote(string(over))), 4*2048+2, "the quoted body is read whole")
	require.True(t, json.Valid(over), "the body decodes")
	rows = append(rows, wireRow(t, "a quoted body longer than client-go keeps, object data never shown",
		apiServer500(projectsWebhook, errors.New("failed to call webhook: "+quotedBody(over))), localRequest(), generic(cutText), true))
	huge := bytes.TrimSpace(sized(9000))
	require.Greater(t, len(strconv.Quote(string(huge))), 4*2048+2)
	rows = append(rows, wireRow(t, "a quoted body longer than is read, object data never shown",
		apiServer500(projectsWebhook, errors.New("failed to call webhook: "+quotedBody(huge))), localRequest(), generic(cutText), true))
	rows = append(rows, wireRow(t, "an unquoted body longer than client-go keeps, object data never shown",
		apiServer500(projectsWebhook, errors.New("failed to call webhook: "+string(over))), localRequest(),
		generic(webhookLead+": failed to call webhook: unknown"), true))
	base := bytes.TrimSpace(sized(1))
	exact := bytes.TrimSpace(sized(1 + 2048 - len(base)))
	require.Len(t, exact, 2048)
	rows = append(rows, wireRow(t, "a quoted body of exactly 2048 bytes, object data never shown",
		apiServer500(projectsWebhook, errors.New("failed to call webhook: "+quotedBody(exact))), localRequest(), generic(projectText), true))
	return rows
}

// failedCallRows are failed calls to a webhook whose error text holds no
// quoted response body: the request did not reach the webhook, the call
// timed out, a 503 or 504 that is not a Status, and other failures.
func failedCallRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	shown := func(reason error) string { return webhookLead + ": " + shownReason(t, reason.Error()) }
	notReached := []struct {
		name   string
		reason error
	}{
		{"connection refused", transport(errors.New(refusedDial))},
		{"no route to host", transport(errors.New("dial tcp 10.43.0.10:443: connect: no route to host"))},
		{"a host name that does not resolve", transport(errors.New("dial tcp: lookup rancher-webhook.cattle-system.svc on 10.43.0.10:53: no such host"))},
		{"a timeout while connecting to the webhook's address", transport(errors.New("dial tcp 10.43.0.10:443: i/o timeout"))},
		{"no endpoints", transport(apierrors.NewServiceUnavailable(fmt.Sprintf("no endpoints available for service %q", "rancher-webhook")))},
		{"a 503 that is not a Status", webhookResponse(503, nil, true)},
	}
	for _, n := range notReached {
		rows = append(rows, onThreePaths(t, "retryable, "+n.name, apiServer500(projectsWebhook, n.reason), localRequest(),
			webhookWant{code: clierr.CodeGeneric, message: shown(n.reason), retryable: true, hint: logsHint()}, false)...)
	}
	refused := transport(errors.New(refusedDial))
	rows = append(rows, onThreePaths(t, "not reached, no request", apiServer500(projectsWebhook, refused), nil,
		webhookWant{code: clierr.CodeGeneric, message: shown(refused), retryable: true}, false)...)
	downstream := localRequest()
	downstream.Cluster = "c-x"
	rows = append(rows, onThreePaths(t, "not reached, another cluster", apiServer500(projectsWebhook, refused), downstream,
		webhookWant{code: clierr.CodeGeneric, message: shown(refused), retryable: true}, false)...)
	rows = append(rows, onThreePaths(t, "not reached, another webhook", apiServer500("policy.example.io", refused), localRequest(),
		webhookWant{code: clierr.CodeGeneric, message: `Internal error occurred: failed calling webhook "policy.example.io": ` + refused.Error(), retryable: true}, false)...)
	timedOut := []struct {
		name   string
		reason error
	}{
		{"context deadline exceeded", transport(context.DeadlineExceeded)},
		{"a read timeout", transport(errors.New("read tcp 10.42.0.5:4123->10.43.0.10:443: i/o timeout"))},
		{"a DNS lookup timeout", transport(errors.New("dial tcp: lookup rancher-webhook.cattle-system.svc on 10.43.0.10:53: read udp 10.42.0.5:4123->10.43.0.10:53: i/o timeout"))},
		{"the client timeout", transport(errors.New("net/http: request canceled (Client.Timeout exceeded while awaiting headers)"))},
		{"a TLS handshake timeout", transport(errors.New("net/http: TLS handshake timeout"))},
		{"a 504 that is not a Status", webhookResponse(504, nil, true)},
	}
	for _, n := range timedOut {
		rows = append(rows, onThreePaths(t, "timed out, "+n.name, apiServer500(projectsWebhook, n.reason), localRequest(),
			webhookWant{code: clierr.CodeGeneric, message: shown(n.reason), hint: logsHint()}, false)...)
	}
	other := []struct {
		name   string
		reason error
	}{
		{"EOF", transport(errors.New("EOF"))},
		{"connection reset", transport(errors.New("read tcp 10.42.0.5:4123->10.43.0.10:443: read: connection reset by peer"))},
		{"network unreachable", transport(errors.New("dial tcp 10.43.0.10:443: connect: network is unreachable"))},
		{"a Status the webhook responded with", fmt.Errorf("failed to call webhook: %w",
			&apierrors.StatusError{ErrStatus: *messageStatus("the webhook could not read the review")})},
		{"no REST client", errors.New("could not get REST client: x509: certificate signed by unknown authority")},
		{"no REST client for a webhook with neither service nor URL", fmt.Errorf("could not get REST client: %w",
			callingWebhook{projectsWebhook, errors.New("webhook configuration must have either service or URL")})},
		{"no REST client for an unparsable URL", fmt.Errorf("could not get REST client: %w",
			callingWebhook{projectsWebhook, fmt.Errorf("Unparsable URL: %w", parseError())})},
		{"an invalid response", errors.New(`received invalid webhook response: expected response.uid="a", got "b"`)},
		{"an undecodable patch", errors.New("received undecodable patch in webhook response: invalid character 'x' looking for beginning of value")},
		{"an unsupported patch type", fmt.Errorf("unsupported patch type %q", "x")},
		{"sideEffects not set", errors.New("Webhook SideEffects is nil")},
		{"the API server could not create the admission objects", errors.New(`could not create admission objects: no kind "Widget" is registered for version "stable.example.com/v1"`)},
		{"a webhook error of 300 bytes", errors.New("could not create admission objects: " + strings.Repeat("x", 264))},
	}
	for _, n := range other {
		rows = append(rows, onThreePaths(t, "other, "+n.name, apiServer500(projectsWebhook, n.reason), localRequest(),
			generic(shown(n.reason)), false)...)
	}
	longURL := "https://rancher-webhook.cattle-system.svc:443/" + strings.Repeat("a", 150)
	rows = append(rows, onThreePaths(t, "not reached, a long URL",
		apiServer500(projectsWebhook, transportTo(longURL, errors.New(refusedDial))), localRequest(),
		webhookWant{
			code:      clierr.CodeGeneric,
			message:   webhookLead + `: failed to call webhook: Post "https://rancher-webhook.cattle-system.svc:443/` + strings.Repeat("a", 124) + "\u2026",
			retryable: true, hint: logsHint(),
		}, false)...)
	invalid := "received invalid webhook response: could not decode the review " + string(bytes.TrimSpace(fixtureReview(t, objectFirstOrder, firstFixture)))
	rows = append(rows, onThreePaths(t, "other, a webhook error that holds JSON, object data never shown",
		apiServer500(projectsWebhook, errors.New(invalid)), localRequest(),
		generic(webhookLead+": received invalid webhook response: could not decode the review \u2026"), true)...)
	rows = append(rows, onThreePaths(t, "no further details", apiServer500(projectsWebhook, nil), localRequest(),
		generic(webhookLead+"; no further details available"), false)...)
	return rows
}

// parseError is the error net/url's Parse returns for a URL with a space
// in its scheme.
func parseError() error {
	return &url.Error{Op: "parse", URL: "h ttp://x", Err: errors.New("first path segment in URL cannot contain colon")}
}

// nameRows are webhook names that need escaping.
func nameRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	names := []struct {
		label  string
		name   string
		quoted string
	}{
		{"a name with a double quote", "a\"b.example.io", "'a\"b.example.io'"},
		{"a name with a backslash", "a\\b.example.io", "'a\\b.example.io'"},
		{"a name with a non-ASCII letter", "caf\u00e9.example.io", "'caf\u00e9.example.io'"},
		{"a name with a newline", "a\nb.example.io", "'a\nb.example.io'"},
		{"a name with U+2063 (an invisible separator)", "a\u2063b.example.io", "'ab.example.io'"},
		{"a name with a single quote", "a'b.example.io", "'a'\\''b.example.io'"},
	}
	refused := transport(errors.New(refusedDial))
	for _, n := range names {
		label := n.label
		rows = append(rows, onThreePaths(t, label+", responded",
			apiServer500(n.name, webhookResponse(500, projectReview(t), true)), localRequest(),
			generic("admission webhook "+n.quoted+" failed: "+projectWebhookMessage), false)...)
		rows = append(rows, onThreePaths(t, label+", not reached",
			apiServer500(n.name, refused), localRequest(),
			webhookWant{
				code:      clierr.CodeGeneric,
				message:   "Internal error occurred: failed calling webhook " + strconv.Quote(n.name) + ": " + refused.Error(),
				retryable: true,
			}, false)...)
	}
	rancherName := "rancher.cattle.io.a\u2063b"
	rows = append(rows, onThreePaths(t, "a rancher-webhook name that needs escaping, not reached",
		apiServer500(rancherName, refused), localRequest(),
		webhookWant{
			code:      clierr.CodeGeneric,
			message:   "Internal error occurred: failed calling webhook " + strconv.Quote(rancherName) + ": " + refused.Error(),
			retryable: true, hint: logsHint(),
		}, false)...)
	return rows
}

// createFailed is an error whose text is its own and that wraps the
// status.
type createFailed struct{ err error }

func (e createFailed) Error() string { return "create failed" }
func (e createFailed) Unwrap() error { return e.err }

// messageFormRows are other texts that hold the API server's 500 for a
// webhook.
func messageFormRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	project := apiServer500(projectsWebhook, webhookResponse(500, projectReview(t), true))
	withData := apiServer500(projectsWebhook, webhookResponse(500, fixtureReview(t, goOrder, firstFixture), true))
	rows = append(rows, typedRow("the status with text after it", fmt.Errorf("%w (after retry)", project), localRequest(),
		generic(projectText+" (after retry)"), false))
	rows = append(rows, typedRow("the status twice, object data never shown", fmt.Errorf("%v; %w", withData, withData), localRequest(),
		generic(projectText+"; "+projectText), true))
	lead := strings.Index(project.Error(), "an error on the server (")
	require.Positive(t, lead)
	beforeLead := typedRow("a Status message cut before the body", fmt.Errorf("first try: %v; retry: %w", project.Error()[:lead], project),
		localRequest(), generic("first try: "+webhookLead+": failed to call webhook: ; retry: "+projectText), false)
	rows = append(rows, beforeLead)
	early := apiServer500(projectsWebhook, webhookResponse(500, fixtureReview(t, objectFirstOrder, firstFixture), true))
	rows = append(rows, typedRow("a Status message cut inside the body, object data never shown",
		fmt.Errorf("first try: %v; retry: %w", early.Error()[:400], early), localRequest(),
		generic("first try: "+webhookLead+": failed to call webhook: "+unknownBody+projectText), true))
	cause := early.ErrStatus.Details.Causes[0].Message
	rows = append(rows, typedRow("the message of the Status's first cause in front, object data never shown",
		fmt.Errorf("%s: %w", cause, early), localRequest(),
		generic(`failed calling webhook "`+projectsWebhook+`": failed to call webhook: `+unknownBody+": "+projectText), true))
	rows = append(rows, typedRow("an error with its own text", createFailed{project}, localRequest(), generic(projectText), false))
	noDetails := apiServer500(projectsWebhook, webhookResponse(500, projectReview(t), true))
	noDetails.ErrStatus.Details = nil
	rows = append(rows, onThreePaths(t, "the status with no details", noDetails, localRequest(), generic(projectText), false)...)
	spaced := apiServer500(projectsWebhook, webhookResponse(500, projectReview(t), true))
	spaced.ErrStatus.Message += "  "
	rows = append(rows, typedRow("the status with trailing spaces behind a prefix", fmt.Errorf("p: %w", spaced), localRequest(),
		generic("p: "+projectText), false))
	inObject := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Reason: metav1.StatusReasonInternalError,
		Message: string(mustJSON(t, map[string]string{"message": early.Error()})),
	}}
	rows = append(rows, onThreePaths(t, "the Status message in a JSON error object, object data never shown", inObject, localRequest(),
		generic(webhookLead+": failed to call webhook: "+unknownBody), true)...)
	short := apiServer500(projectsWebhook, transportTo("https://w/", errors.New(refusedDial)))
	shortObject := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Reason: metav1.StatusReasonInternalError,
		Message: string(mustJSON(t, map[string]string{"message": short.Error()})),
	}}
	require.LessOrEqual(t, len(short.Error()), 200, "the Status message is kept whole")
	rows = append(rows, onThreePaths(t, "a short Status message in a JSON error object", shortObject, localRequest(),
		webhookWant{code: clierr.CodeGeneric, message: short.Error(), retryable: true, hint: logsHint()}, false)...)
	return rows
}

// defaultRows are texts the status table's last arm shows.
func defaultRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	etcd := apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	rows = append(rows, onThreePaths(t, "another 500, etcd", etcd, localRequest(),
		generic("Internal error occurred: etcdserver: request timed out"), false)...)
	modify := apierrors.NewInternalError(fmt.Errorf("admission webhook %q attempted to modify the object, which is not supported for this operation", "x"))
	rows = append(rows, onThreePaths(t, "another 500, a webhook that modified the object", modify, localRequest(),
		generic(`Internal error occurred: admission webhook "x" attempted to modify the object, which is not supported for this operation`), false)...)
	upstream := `an error on the server ("upstream text") has prevented the request from succeeding`
	rows = append(rows, onThreePaths(t, "another 500, a status that starts with a quoted body",
		unknownStatus(errors.New(upstream)), localRequest(), generic(upstream), false)...)
	emptyConversion := conversionError(500, nil)
	emptyText := `conversion webhook for stable.example.com/v1, Kind=Widget failed: an error on the server ("") has prevented the request from succeeding`
	rows = append(rows, onThreePaths(t, "another 500, a conversion webhook with an empty body", unknownStatus(emptyConversion), localRequest(),
		generic(emptyText), false)...)
	rows = append(rows, onThreePaths(t, "another 500, a conversion webhook with an empty body wrapped", apierrors.NewInternalError(emptyConversion), localRequest(),
		generic("Internal error occurred: "+emptyText), false)...)
	conversion := conversionError(500, mustJSON(t, conversionReview{
		Kind: "ConversionReview", APIVersion: "apiextensions.k8s.io/v1",
		Request: conversionRequest{UID: "a", DesiredAPIVersion: "stable.example.com/v1", Objects: []json.RawMessage{fixtureObject(t, firstFixture, "")}},
	}))
	conversionText := "conversion webhook for stable.example.com/v1, Kind=Widget failed: " + unknownBody
	rows = append(rows, onThreePaths(t, "another 500, a conversion webhook, object data never shown", unknownStatus(conversion), localRequest(),
		generic(conversionText), true)...)
	rows = append(rows, onThreePaths(t, "another 500, a conversion webhook wrapped, object data never shown", apierrors.NewInternalError(conversion), localRequest(),
		generic("Internal error occurred: "+conversionText), true)...)
	review := fixtureReview(t, objectFirstOrder, firstFixture)
	response := webhookResponse(500, review, true)
	rows = append(rows, onThreePaths(t, "another 500, the webhook text after other text, object data never shown",
		apierrors.NewInternalError(fmt.Errorf("storage: %w", callingWebhook{"x.example.io", response})), localRequest(),
		generic(`Internal error occurred: storage: failed calling webhook "x.example.io": failed to call webhook: `+unknownBody), true)...)
	names := []struct {
		label string
		name  string
	}{
		{"a back-quoted name", "`x.example.io`"},
		{"a single-quoted name", "'x'"},
		{"an unquoted name", "x.example.io"},
		{"an empty name", `""`},
		{"a name too long to read", strconv.Quote(strings.Repeat("a", 1100))},
	}
	for _, n := range names {
		text := "failed calling webhook " + n.name + ": " + response.Error()
		rows = append(rows, onThreePaths(t, "another 500, "+n.label+", object data never shown",
			apierrors.NewInternalError(errors.New(text)), localRequest(),
			generic("Internal error occurred: failed calling webhook "+n.name+": failed to call webhook: "+unknownBody), true)...)
	}
	rows = append(rows, onThreePaths(t, "another 500, JSON after a quoted body, object data never shown",
		apierrors.NewInternalError(errors.New("upstream: "+quotedBody(review)+` {"x":1}`)), localRequest(),
		generic("Internal error occurred: upstream: "+unknownBody+" \u2026"), true)...)
	nested := quotedBody([]byte(`Internal error occurred: failed calling webhook "a.io": failed to call webhook: ` + quotedBody(review)))
	rows = append(rows, onThreePaths(t, "another 500, a quoted body that holds another, object data never shown",
		unknownStatus(errors.New(nested)), localRequest(), generic(unknownBody), true)...)
	nestedInternal := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Reason: metav1.StatusReasonInternalError, Message: nested,
	}}
	rows = append(rows, onThreePaths(t, "another 500, a quoted body that holds another, reason InternalError, object data never shown",
		nestedInternal, localRequest(), generic(unknownBody), true)...)
	return rows
}

// perCodeRows are rows with a response body quoted into the Status
// message, for the status codes 400, 401, 403, 404, 409, 410, 422, 429,
// 502, 503, 504 and 530; 410 reaches the last arm of the status table. The
// last rows hold the body in a 409 that is not a Status, in the text a
// caller put in front of the error, and as a cluster agent error.
func perCodeRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	review := fixtureReview(t, objectFirstOrder, firstFixture)
	const denied = `admission webhook "x.example.io" denied the request: `
	shown := denied + unknownBody
	status := func(code int32) *apierrors.StatusError {
		return deniedBy("x.example.io", &metav1.Status{Code: code, Message: quotedBody(review)})
	}
	answered := func(lead string) string {
		return "https://rancher.example.com answered HTTP " + lead + ": " + shown + " - retry later"
	}
	precondition := localRequest()
	precondition.Precondition = true
	codes := []struct {
		name string
		code int32
		req  *clierr.Request
		want webhookWant
	}{
		{"400", 400, localRequest(), webhookWant{code: clierr.CodeValidation, message: shown}},
		{"401", 401, localRequest(), webhookWant{code: clierr.CodeAuth, message: shown}},
		{"403", 403, localRequest(), webhookWant{code: clierr.CodeRBAC, message: shown}},
		{"404", 404, localRequest(), webhookWant{code: clierr.CodeNotFound, message: shown}},
		{"409", 409, localRequest(), webhookWant{code: clierr.CodeConflict, message: shown}},
		{"409 with a precondition keeps the webhook's message", 409, precondition, webhookWant{code: clierr.CodeConflict, message: shown}},
		{"410", 410, localRequest(), webhookWant{code: clierr.CodeGeneric, message: shown}},
		{"422", 422, localRequest(), webhookWant{code: clierr.CodeValidation, message: shown}},
		{"429 with the server named", 429, localRequest(), webhookWant{code: clierr.CodeGeneric, message: answered("429 Too Many Requests"), retryable: true}},
		{"429", 429, nil, webhookWant{code: clierr.CodeGeneric, message: shown + " - retry later", retryable: true}},
		{"502 with the server named", 502, localRequest(), webhookWant{code: clierr.CodeGeneric, message: answered("502 Bad Gateway"), retryable: true}},
		{"502", 502, nil, webhookWant{code: clierr.CodeGeneric, message: shown + " - retrying may succeed", retryable: true}},
		{"503 with the server named", 503, localRequest(), webhookWant{code: clierr.CodeGeneric, message: answered("503 Service Unavailable"), retryable: true}},
		{"503", 503, nil, webhookWant{code: clierr.CodeGeneric, message: shown + " - retrying may succeed", retryable: true}},
		{"504 with the server named", 504, localRequest(), webhookWant{code: clierr.CodeGeneric, message: answered("504 Gateway Timeout"), retryable: true}},
		{"504", 504, nil, webhookWant{code: clierr.CodeGeneric, message: shown + " - retrying may succeed", retryable: true}},
		{"530 with the server named", 530, localRequest(), webhookWant{
			code:      clierr.CodeGeneric,
			message:   "https://rancher.example.com answered HTTP 530: " + shown + "; a proxy or load balancer in front of Rancher could not reach Rancher - retry later",
			retryable: true,
		}},
	}
	for _, c := range codes {
		rows = append(rows, onTwoPaths(t, "a body in a "+c.name+", object data never shown", status(c.code), c.req, c.want, true)...)
	}
	conflict := apierrors.NewGenericServerResponse(http.StatusConflict, http.MethodPut, schema.GroupResource{}, "", denied+quotedBody(review), 0, true)
	rows = append(rows, typedRow("a body in a 409 response that is not a Status, object data never shown", conflict, localRequest(),
		webhookWant{code: clierr.CodeConflict, message: shown}, true))
	prefix := "first " + quotedBody(review)
	cleanPrefix := "first " + unknownBody
	rows = append(rows, typedRow("a body in the prefix of a 404, object data never shown",
		fmt.Errorf("%s: %w", prefix, apierrors.NewNotFound(schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}, "p-x")),
		localRequest(), webhookWant{code: clierr.CodeNotFound, message: cleanPrefix + `: projects.management.cattle.io "p-x" not found`}, true))
	slowDown := deniedBy("x.example.io", &metav1.Status{Code: 429, Message: "slow down"})
	rows = append(rows, typedRow("a body in the prefix of a 429 with the server named, object data never shown",
		fmt.Errorf("%s: %w", prefix, slowDown), localRequest(), webhookWant{
			code:      clierr.CodeGeneric,
			message:   cleanPrefix + ": https://rancher.example.com answered HTTP 429 Too Many Requests: " + denied + "slow down - retry later",
			retryable: true,
		}, true))
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}, "p-x")
	rows = append(rows, typedRow("an unterminated response body in the prefix", fmt.Errorf("first an error on the server (\"{abc: %w", notFound), localRequest(),
		webhookWant{code: clierr.CodeNotFound, message: "first " + unknownBody + `projects.management.cattle.io "p-x" not found`}, false))
	agent := apierrors.NewGenericServerResponse(http.StatusInternalServerError, http.MethodGet, schema.GroupResource{}, "",
		"error trying to reach service: cluster agent disconnected", 0, true)
	rows = append(rows, typedRow("the agent disconnecting with text after the status", fmt.Errorf("get: %w (after retry)", agent), nil,
		webhookWant{code: clierr.CodeGeneric, message: "get: " + unknownBody + " (after retry) - retry once the cluster reconnects", retryable: true}, false))
	return rows
}

// responseBodyPathRows are rows with a Rancher JSON error object that
// holds the API server's text for a failed admission webhook call, read
// from a response and from a client-go error, with the server named in
// the message and without. The last row holds an error object whose
// message quotes no response body.
func responseBodyPathRows(t *testing.T) []webhookRow {
	t.Helper()
	var rows []webhookRow
	text := `Internal error occurred: failed calling webhook "a.io": ` + webhookResponse(500, fixtureReview(t, objectFirstOrder, firstFixture), true).Error()
	found := false
	for _, v := range objectData {
		found = found || strings.Contains(text[:200], v)
	}
	require.True(t, found, "object data lies within the first 200 bytes of the message")
	body := mustJSON(t, rancherErrorObject{Type: "error", Code: "ServerError", Message: text})
	shown := `Internal error occurred: failed calling webhook "a.io": failed to call webhook: ` + unknownBody
	named := webhookWant{
		code:      clierr.CodeGeneric,
		message:   "https://rancher.example.com answered HTTP 500 Internal Server Error: " + shown + " - retry later",
		retryable: true,
	}
	unnamed := webhookWant{
		code:      clierr.CodeGeneric,
		message:   fmt.Sprintf("an error on the server (%q) has prevented the request from succeeding", shown) + " - retrying may succeed",
		retryable: true,
	}
	rows = append(rows, webhookRow{"a Rancher error object with the server named, object data never shown", func() *clierr.Error {
		return clierr.FromResponse("", http.MethodPost, 500, body, localRequest())
	}, named, true})
	rows = append(rows, webhookRow{"a Rancher error object, object data never shown", func() *clierr.Error {
		return clierr.FromResponse("", http.MethodPost, 500, body, nil)
	}, unnamed, true})
	typed := apierrors.NewGenericServerResponse(http.StatusInternalServerError, http.MethodPost, schema.GroupResource{}, "", string(body), 0, true)
	rows = append(rows, typedRow("a Rancher error object from client-go with the server named, object data never shown", typed, localRequest(), named, true))
	rows = append(rows, typedRow("a Rancher error object from client-go, object data never shown", typed, nil, unnamed, true))
	unavailable := apierrors.NewGenericServerResponse(http.StatusServiceUnavailable, http.MethodPost, schema.GroupResource{}, "", string(body), 0, true)
	rows = append(rows, typedRow("a Rancher error object in a 503 from client-go with the server named, object data never shown", unavailable, localRequest(),
		webhookWant{
			code:      clierr.CodeGeneric,
			message:   "https://rancher.example.com answered HTTP 503 Service Unavailable: " + shown + " - retry later",
			retryable: true,
		}, true))
	rows = append(rows, typedRow("a Rancher error object in a 503 from client-go, object data never shown", unavailable, nil,
		webhookWant{code: clierr.CodeGeneric, message: "the server is currently unable to handle the request - retrying may succeed", retryable: true}, true))
	plainBody := mustJSON(t, rancherErrorObject{Type: "error", Code: "ServerError", Message: "boom"})
	rows = append(rows, webhookRow{"a Rancher error object with no quoted body", func() *clierr.Error {
		return clierr.FromResponse("", http.MethodPost, 500, plainBody, localRequest())
	}, webhookWant{
		code:      clierr.CodeGeneric,
		message:   "https://rancher.example.com answered HTTP 500 Internal Server Error: boom - retry later",
		retryable: true,
	}, false})
	return rows
}
