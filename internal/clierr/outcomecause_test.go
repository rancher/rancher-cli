package clierr

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kubernetesStatusError is the error client-go returns for a response whose
// body is a Kubernetes Status with the code, reason and message.
func kubernetesStatusError(code int32, reason metav1.StatusReason, message string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: code, Reason: reason, Message: message,
	}}
}

// TestNotAKubernetesStatusReadsTheUnexpectedResponseCause asserts that
// NotAKubernetesStatus is true for a *apierrors.StatusError built for a body
// that is not a Kubernetes Status (a text body, an empty body, or a JSON
// object that is not a Status), for each of 500, 502, 503 and 504, however the
// error is built and when it is wrapped, and also for a Status that carries
// the unexpected-server-response cause (the cautious side for a write). It
// asserts false for a Status without that cause, a StatusError with no
// details, an error that holds no StatusError and nil.
func TestNotAKubernetesStatusReadsTheUnexpectedResponseCause(t *testing.T) {
	t.Parallel()
	groupResource := schema.GroupResource{Resource: "configmaps"}
	for _, status := range []int{500, 502, 503, 504} {
		for _, tc := range []struct {
			name string
			err  error
			want bool
		}{
			{"client-go text", apierrors.NewGenericServerResponse(status, http.MethodPost, groupResource, "web", "upstream failed", 0, true), true},
			{"client-go empty body", apierrors.NewGenericServerResponse(status, http.MethodPost, groupResource, "web", "", 0, true), true},
			{"client-go JSON that is not a Status", apierrors.NewGenericServerResponse(status, http.MethodPost, groupResource, "web", `{"type":"error","message":"proxy failed"}`, 0, true), true},
			{"read without client-go, text", ResponseError(http.MethodPost, status, []byte("upstream failed")), true},
			{"read without client-go, JSON that is not a Status", ResponseError(http.MethodPost, status, []byte(`{"type":"error","message":"proxy failed"}`)), true},
			{"wrapped", fmt.Errorf("create: %w", apierrors.NewGenericServerResponse(status, http.MethodPost, groupResource, "web", "upstream failed", 0, true)), true},
			{"a Kubernetes Status", kubernetesStatusError(int32(status), metav1.StatusReasonInternalError, "Internal error occurred: etcd is down"), false},
			{"a Kubernetes Status read without client-go", ResponseError(http.MethodPost, status, []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"etcd is down","code":`+fmt.Sprint(status)+`}`)), false},
			{"a wrapped Kubernetes Status", fmt.Errorf("create: %w", kubernetesStatusError(int32(status), metav1.StatusReasonInternalError, "etcd is down")), false},
			{"a Kubernetes Status that carries the unexpected-server-response cause", &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Code: int32(status), Reason: metav1.StatusReasonInternalError, Message: "etcd is down",
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: metav1.CauseTypeUnexpectedServerResponse, Message: "etcd is down"}}},
			}}, true},
			{"a StatusError with no details", &apierrors.StatusError{ErrStatus: metav1.Status{Code: int32(status)}}, false},
		} {
			t.Run(fmt.Sprintf("%d %s", status, tc.name), func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, tc.want, NotAKubernetesStatus(tc.err))
			})
		}
	}
	assert.False(t, NotAKubernetesStatus(io.EOF), "an error that is no response")
	assert.False(t, NotAKubernetesStatus(New(CodeGeneric, "a CLI error")), "an error that holds no StatusError")
	assert.False(t, NotAKubernetesStatus(nil))
}

// TestOutcomeCauseHasTheTextTheAnsweredMessageShows asserts that the cause of
// an unknown outcome and the message clierr words for the same answered
// status carry the same server text: a Status 504, a text 502, a JSON object
// with a message and a body with no text, as client-go builds the error and
// as the CLI reads it without client-go. A response with no text is worded by
// its status alone.
func TestOutcomeCauseHasTheTextTheAnsweredMessageShows(t *testing.T) {
	t.Parallel()
	groupResource := schema.GroupResource{Resource: "configmaps"}
	for _, tc := range []struct {
		name   string
		status int
		err    error
		text   string
	}{
		{"a Status 504", 504, kubernetesStatusError(504, metav1.StatusReasonTimeout, "Timeout: request did not complete within the requested timeout"), "Timeout: request did not complete within the requested timeout"},
		{"client-go text 502", 502, apierrors.NewGenericServerResponse(502, http.MethodPost, groupResource, "web", "upstream connect error", 0, true), "upstream connect error"},
		{"read without client-go, text 502", 502, ResponseError(http.MethodPost, 502, []byte("upstream connect error")), "upstream connect error"},
		{"client-go JSON object with a message", 504, apierrors.NewGenericServerResponse(504, http.MethodPost, groupResource, "web", `{"type":"error","message":"proxy failed"}`, 0, true), "proxy failed"},
		{"read without client-go, JSON object with a message", 504, ResponseError(http.MethodPost, 504, []byte(`{"type":"error","message":"proxy failed"}`)), "proxy failed"},
		{"client-go body with no text", 502, apierrors.NewGenericServerResponse(502, http.MethodPost, groupResource, "web", "", 0, true), ""},
		{"client-go JSON object with no message", 502, apierrors.NewGenericServerResponse(502, http.MethodPost, groupResource, "web", `{"type":"error"}`, 0, true), ""},
		{"read without client-go, body with no text", 502, ResponseError(http.MethodPost, 502, nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := &Request{Server: answeredServer}
			answered := FromStatus(tc.err, req)
			want := AnsweredLead(req, tc.status) + " - retry later"
			cause := "the server responded with " + fmt.Sprint(tc.status)
			if tc.text != "" {
				want = AnsweredLead(req, tc.status) + ": " + tc.text + " - retry later"
				cause += ": " + tc.text
			}
			assert.Equal(t, want, answered.Message)
			assert.Equal(t, cause, OutcomeCause(tc.err))
		})
	}
}
