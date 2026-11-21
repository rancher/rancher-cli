package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Request names the call a server status answers. A nil *Request means
// nothing is known about the call: the texts for a refusal with no reason
// name no request, and no remedy is attached.
//
// A request with a Server words a 429 and a 5xx status-first, as
// "<Server> answered HTTP <code> ..."; one without keeps the older wording.
type Request struct {
	// Verb, Group, Resource and Name name the object the call reads or
	// writes; an empty Verb names none.
	Verb, Group, Resource, Name string
	// Cluster is the --cluster value whose proxy the call goes through;
	// a 5xx on that path is worded for the proxy and the cluster agent.
	Cluster string
	// Endpoint marks a call that names an API, not an object (a version or
	// discovery document, a review, the provider list): a 404 means the URL
	// does not lead to that API, not that an object is missing. Through the
	// local cluster or none that is the server URL to check; through another
	// cluster it is a plain NOT_FOUND, and a 403 a plain RBAC, unless the call
	// is Unresolved.
	Endpoint bool
	// Unresolved marks a call whose cluster id reaches Rancher's router
	// lookup unresolved (the owner check): there a 404 means Rancher does not
	// know the cluster, and a 403 that the user cannot read it or Rancher does
	// not know it; both name the --cluster value.
	Unresolved bool
	// NoCredential marks a call that sends no credential, so a 401 does not
	// name one and carries no remedy.
	NoCredential bool
	// Precondition marks a write that sent a resourceVersion precondition: an
	// update whose body carries metadata.resourceVersion, or a delete with
	// DeleteOptions preconditions.resourceVersion. A 409 on any other request
	// cannot mean the resourceVersion is out of date.
	Precondition bool
	// Server is the server the call went to, in the form output may show
	// it (config.DisplayServerURL: scheme and host, no userinfo or path).
	// The caller sets it: this package cannot import config.
	Server string
	// Login is the command a 401 gives as its hint: it gets a new token for the
	// stored login whose token the call sent. The zero value names none: the
	// credential did not come from a stored login, or the login has no such
	// command. The caller sets it: this package cannot import config.
	Login Command
}

// what is "<verb> <resource>.<group>", the group left out for the core
// group.
func (q *Request) what() string {
	gr := q.Resource
	if q.Group != "" {
		gr += "." + q.Group
	}
	return q.Verb + " " + gr
}

// localManagementRefusal is why a management.cattle.io request through the
// local cluster's proxy can be refused with no reason.
const localManagementRefusal = "; through the local cluster's proxy, Rancher answers some management.cattle.io resources, clusters among them, only for users with access to the local cluster"

// unauthorizedDefault is apimachinery's message for a 401 whose body
// carried none.
const unauthorizedDefault = "the server has asked for the client to provide credentials"

// unknownMessage is client-go's message for an error body it does not
// decode and does not show: a JSON body that is not a Kubernetes Status.
const unknownMessage = "unknown"

// maxServerMessage bounds the server's text an error carries, so an HTML
// error page or a long body cannot fill the message.
const maxServerMessage = 200

// maxAdmissionMessage bounds the server's text of an admission refusal that a
// caller shows in full. It keeps more bytes than maxServerMessage because the
// text names the policy and what it refused.
const maxAdmissionMessage = 2048

// forbiddenText words a 403 that gave no reason: client-go leaves the
// message empty for the dynamic client's AbsPath requests, and Rancher's
// proxy answers a user without access with an empty body.
func forbiddenText(q *Request) string {
	if q == nil || q.Verb == "" {
		return "the server refused the request (403) and gave no reason"
	}
	name := ""
	if q.Name != "" {
		name = " " + ShellQuote(q.Name)
	}
	msg := "the server refused " + q.what() + name + " (403) and gave no reason"
	if q.Cluster == "local" && q.Group == "management.cattle.io" {
		msg += localManagementRefusal
	}
	return msg
}

// unauthorizedText words a 401 by its status alone.
func unauthorizedText(q *Request) string {
	if q != nil && q.NoCredential {
		return "the server answered 401; the request carried no credential"
	}
	if q == nil || q.Verb == "" {
		return "the server rejected the credential (401)"
	}
	return "the server rejected the credential (401) for " + q.what()
}

// FromStatus classifies the server status in err's chain, worded for req;
// req may be nil. An *Error already in the chain is returned as is; nil
// when the chain holds neither. The result keeps err as its cause, so
// HTTPStatus still reads the status.
func FromStatus(err error, req *Request) *Error {
	if err == nil {
		return nil
	}
	if ce, ok := errors.AsType[*Error](err); ok {
		return ce
	}
	se, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok {
		return nil
	}
	return classifyStatus(err, se, req).WithCause(err)
}

// From gives err its envelope shape: FromStatus's answer, else GENERIC
// with err's text and err as the cause. Nil for nil.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if ce := FromStatus(err, nil); ce != nil {
		return ce
	}
	return New(CodeGeneric, err.Error()).WithCause(err)
}

// FromResponse classifies a non-2xx answer read without client-go, the
// message led by prefix (none when empty); never nil.
func FromResponse(prefix, method string, status int, body []byte, req *Request) *Error {
	var err error = ResponseError(method, status, body)
	if prefix != "" {
		err = fmt.Errorf("%s: %w", prefix, err)
	}
	return FromStatus(err, req)
}

// HTTPStatus returns the status code of the server answer in err's chain:
// the HTTP status, or the code of a Kubernetes Status body, which wins
// where client-go or ResponseError decoded one. It reads
// *apierrors.StatusError, the type client-go returns.
func HTTPStatus(err error) (int, bool) {
	se, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok {
		return 0, false
	}
	return int(se.ErrStatus.Code), true
}

// ResponseError is the error for a non-2xx answer read without client-go.
// A Kubernetes Status body with an error code is kept as sent, as
// client-go keeps it; a Status body with another code takes the HTTP
// status and loses its reason. Any other body becomes the answer
// client-go builds for it (apierrors.NewGenericServerResponse), with the
// server's message taken from a JSON error object's "message" (Rancher's
// own and its proxy's both carry one), kept whole, or the first line of
// a body that is not JSON, at most maxServerMessage bytes; other JSON
// gives none, and a 5xx with none names its status text, or "status
// <code>" where there is no standard text. Every answer that is not a
// Status carries a second cause that records the server's message has
// already been taken from the body, so classifying it does not take it
// again; a 5xx among them is an unexpected server response, as client-go
// marks it. A 403 whose message was cut to maxServerMessage bytes carries,
// after those two, a cause with the whole message, which only the admission
// text match reads.
func ResponseError(method string, status int, body []byte) *apierrors.StatusError {
	if st, ok := statusBody(status, body); ok {
		return &apierrors.StatusError{ErrStatus: st}
	}
	message := serverMessage(body)
	if message == "" && status >= http.StatusInternalServerError {
		// apimachinery quotes the server's message into a 5xx's text.
		message = statusText(status)
	}
	se := apierrors.NewGenericServerResponse(status, method, schema.GroupResource{}, "", message, 0, true)
	se.ErrStatus.Details.Causes = append(se.ErrStatus.Details.Causes, metav1.StatusCause{Type: causeServerMessageTaken})
	if full := fullServerMessage(body); status == http.StatusForbidden && full != message {
		// The message is cut to maxServerMessage bytes; the admission text match
		// reads the whole message.
		se.ErrStatus.Details.Causes = append(se.ErrStatus.Details.Causes, metav1.StatusCause{Type: causeServerMessageFull, Message: full})
	}
	return se
}

// causeServerMessageTaken marks an unexpected server response whose
// quoted message is already the server's message as the CLI shows it, not
// the body client-go quotes.
const causeServerMessageTaken metav1.CauseType = "ServerMessageTaken"

// causeServerMessageFull carries the message of a 403 before ResponseError
// cut it to maxServerMessage bytes.
const causeServerMessageFull metav1.CauseType = "ServerMessageFull"

// fullForbiddenMessage returns the server's whole 403 message, uncut. It uses,
// in this order: the message saved in the ServerMessageFull cause; the message
// of the JSON error object inside the Status message, when the error ends with
// that Status message; the `reason` argument, which is the Status message.
func fullForbiddenMessage(se *apierrors.StatusError, reason string, wrapped bool) string {
	if se.ErrStatus.Details != nil {
		for _, c := range se.ErrStatus.Details.Causes {
			if c.Type == causeServerMessageFull {
				return strings.TrimSpace(c.Message)
			}
		}
	}
	if wrapped {
		if m, ok := objectMessageBound(strings.TrimSpace(se.ErrStatus.Message), keepText, uncut); ok {
			return m
		}
	}
	return reason
}

// statusText is the standard text of an HTTP status, else "status <code>".
func statusText(code int) string {
	if text := http.StatusText(code); text != "" {
		return text
	}
	return fmt.Sprintf("status %d", code)
}

// statusBody decodes body as a Kubernetes Status, giving it the HTTP
// status when its own code is not an error code.
func statusBody(status int, body []byte) (metav1.Status, bool) {
	var st metav1.Status
	if len(body) == 0 || json.Unmarshal(body, &st) != nil || st.Kind != "Status" {
		return metav1.Status{}, false
	}
	if st.Code < 400 {
		st.Reason = ""
		st.Code = int32(status)
	}
	return st, true
}

// errorObject is a JSON error body with a message: Rancher's own error
// object ({"type":"error","code":...,"message":...}) and norman's, which
// its proxy and cluster router write ({"Code":{...},"Message":...}); keys
// match without regard to case.
type errorObject struct {
	Message string `json:"message"`
}

// serverMessage is the text of body a message may carry.
func serverMessage(body []byte) string { return decodeMessage(body, keepText) }

// FilteredServerMessage is the text of body a message may carry, decoded as
// every server message is (a JSON error object's message, or the first
// line of a body that is not JSON; none for other JSON or a blank body),
// with filter run on the whole text before it is trimmed and cut to the
// bound every server message keeps. Running the filter first lets it hide a
// value the cut would split.
func FilteredServerMessage(body []byte, filter func(string) string) string {
	return filteredMessage(body, filter, bounded)
}

// filteredMessage decodes body as every server message is decoded, runs filter
// on the whole text, trims it and cuts it with bound.
func filteredMessage(body []byte, filter, bound func(string) string) string {
	return decodeMessageBound(body, func(s string) string { return strings.TrimSpace(filter(s)) }, bound)
}

// keepText is the filter of the callers that show the message as sent.
func keepText(s string) string { return s }

// decodeMessage is serverMessage with filter run on the decoded text before
// it is bounded.
func decodeMessage(body []byte, filter func(string) string) string {
	return decodeMessageBound(body, filter, bounded)
}

// uncut returns s unchanged. It stands in for bounded where the message is
// only read, never shown.
func uncut(s string) string { return s }

// fullServerMessage is serverMessage with no cut to maxServerMessage bytes:
// the message the admission text match reads, while the message the CLI shows
// stays cut.
func fullServerMessage(body []byte) string { return decodeMessageBound(body, keepText, uncut) }

// decodeMessageBound is decodeMessage with the function that cuts the message
// passed as an argument.
func decodeMessageBound(body []byte, filter, bound func(string) string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	if m, ok := objectMessageBound(text, filter, bound); ok {
		return m
	}
	if json.Valid([]byte(text)) {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	return bound(strings.TrimRight(filter(line), "\r"))
}

// objectMessage decodes text as a JSON error object.
func objectMessage(text string) (string, bool) { return objectMessageFiltered(text, keepText) }

// objectMessageFiltered is objectMessage with filter run on the message
// before it is trimmed and bounded.
func objectMessageFiltered(text string, filter func(string) string) (string, bool) {
	return objectMessageBound(text, filter, bounded)
}

// objectMessageBound is objectMessageFiltered with the function that cuts the
// message passed as an argument.
func objectMessageBound(text string, filter, bound func(string) string) (string, bool) {
	if !strings.HasPrefix(text, "{") {
		return "", false
	}
	var obj errorObject
	if json.Unmarshal([]byte(text), &obj) != nil {
		return "", false
	}
	return bound(strings.TrimSpace(filter(obj.Message))), true
}

// bounded cuts s to maxServerMessage bytes on a character boundary.
func bounded(s string) string { return cutAt(s, maxServerMessage) }

// boundedAdmission cuts s to maxAdmissionMessage bytes on a character boundary.
func boundedAdmission(s string) string { return cutAt(s, maxAdmissionMessage) }

// cutAt cuts s to limit bytes on a character boundary and ends a cut text with
// an ellipsis.
func cutAt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// classifyStatus is the status table. msg is err's text. When err ends
// with the status's own text, a reason that is a JSON error object is
// shown as its message, and a refusal that gave no reason keeps the text
// err adds in front and words the rest for req.
func classifyStatus(err error, se *apierrors.StatusError, req *Request) *Error {
	msg := err.Error()
	prefix, wrapped := strings.CutSuffix(msg, se.Error())
	if se.Error() == "" {
		// Every text ends with an empty one: take it as the status's own
		// only where it stands alone or ends the text after a prefix.
		wrapped = msg == "" || strings.HasSuffix(msg, ": ")
	}
	reason := strings.TrimSpace(se.ErrStatus.Message)
	if m, ok := objectMessage(reason); ok && wrapped {
		reason = m
		msg = prefix + m
	}
	noReason := wrapped && (reason == "" || reason == unknownMessage)
	code := int(se.ErrStatus.Code)
	// The status-first wording needs a server and a text that ends with the
	// status's own, so the caller's prefix is known.
	named := wrapped && req != nil && req.Server != ""
	if noReason {
		// A status with no reason reads its status text in every arm that
		// shows the message; the 401 and 403 arms word their own.
		msg = prefix + statusText(code)
	}
	// A response body that apimachinery quoted into the caller's prefix, or
	// into the Status message after its start, is replaced by "unknown" in
	// every arm (an empty body shows nothing and stays as it is). The prefix
	// and the Status message are processed separately: an unterminated body in
	// the prefix does not hide the Status message, and a Status message that
	// starts with a body reads the same with or without a prefix. raw is the
	// text as it was, for the webhook arm, which finds each occurrence of the
	// Status message in it and replaces the bodies itself, and for the
	// cluster agent check.
	raw := msg
	if own, ok := strings.CutPrefix(msg, prefix); wrapped && ok {
		msg = errorTextWithoutResponseBodies(prefix, false) + errorTextWithoutResponseBodies(own, true)
	} else {
		msg = errorTextWithoutResponseBodies(msg, true)
	}
	prefix = errorTextWithoutResponseBodies(prefix, false)
	switch {
	case apierrors.IsNotFound(se):
		switch {
		case req != nil && req.Unresolved && req.Cluster != "" && req.Cluster != "local":
			return New(CodeNotFound, msg+"; Rancher answers 404 for a cluster it does not know: check the --cluster value "+ShellQuote(req.Cluster))
		case req != nil && req.Endpoint && (req.Cluster == "" || req.Cluster == "local"):
			return New(CodeGeneric, msg+"; check the server URL")
		}
		return New(CodeNotFound, msg)
	case apierrors.IsForbidden(se):
		// A 403 whose message is an admission text is not a missing
		// permission. The admission text match reads the server's own text, not
		// msg, which starts with the caller's prefix, and runs for every request:
		// a write can reach admission with no verb or no request.
		if ce, ok := AdmissionRefusal(fullForbiddenMessage(se, reason, wrapped), msg); ok {
			return ce
		}
		if noReason {
			msg = prefix + forbiddenText(req)
		}
		if req != nil && req.Unresolved && req.Cluster != "" && req.Cluster != "local" {
			msg += "; Rancher answers 403 for a cluster the user cannot read and, to a user who cannot read every cluster, for one it does not know: check the --cluster value " + ShellQuote(req.Cluster)
		}
		return New(CodeRBAC, msg)
	case apierrors.IsUnauthorized(se):
		if noReason || (wrapped && (reason == unauthorizedDefault || strings.EqualFold(reason, http.StatusText(http.StatusUnauthorized)))) {
			ce := New(CodeAuth, prefix+unauthorizedText(req))
			if req != nil && !req.NoCredential && !req.Login.isZero() {
				ce = ce.WithHintCommand(req.Login)
			}
			return ce
		}
		return New(CodeAuth, msg)
	case apierrors.IsConflict(se) && req != nil && req.Precondition && !webhookDenial(serverDetail(se, reason, noReason)):
		// A resourceVersion precondition race: another writer updated the
		// object between the read and this write; a fresh read-modify-write
		// resolves it. A 409 on a write that sent a resourceVersion is read
		// as that race, except when the server's text says an admission
		// webhook denied the request: a retry gets the same denial, and the
		// CONFLICT case below shows the webhook's message.
		return Retryable(CodeConflict, "the resource changed on the server since it was last read; retry the command")
	case apierrors.IsConflict(se):
		// A 409 can mean the resourceVersion is out of date only for a write
		// that sent one. Any other request was refused for another reason,
		// and repeating it gets the same response: show the server's message.
		if _, _, ok := unexpectedCause(se); ok && wrapped {
			text := serverDetail(se, reason, noReason)
			if text == "" {
				text = statusText(code)
			}
			msg = prefix + text
		}
		return New(CodeConflict, msg)
	case apierrors.IsAlreadyExists(se):
		return New(CodeConflict, msg)
	case apierrors.IsInvalid(se), apierrors.IsBadRequest(se):
		return New(CodeValidation, msg)
	case apierrors.IsTooManyRequests(se):
		if named {
			return Retryable(CodeGeneric, answeredMessage(prefix, req, code, serverDetail(se, reason, noReason), false))
		}
		return Retryable(CodeGeneric, msg+" - retry later")
	case apierrors.IsServiceUnavailable(se), se.ErrStatus.Code == http.StatusBadGateway, se.ErrStatus.Code == http.StatusGatewayTimeout:
		if named {
			return Retryable(CodeGeneric, answeredMessage(prefix, req, code, serverDetail(se, reason, noReason), false))
		}
		if wrapped && unexpectedServerResponse(se) {
			// apimachinery quotes a 502's body into its text.
			msg = prefix + unexpectedResponseText(se)
		}
		if throughCluster(req) {
			return Retryable(CodeGeneric, msg+" - its agent may not be registered yet; retry once the cluster is active")
		}
		return Retryable(CodeGeneric, msg+" - retrying may succeed")
	case unexpectedServerResponse(se):
		if named {
			return Retryable(CodeGeneric, answeredMessage(prefix, req, code, serverDetail(se, reason, noReason), true))
		}
		if wrapped {
			msg = prefix + unexpectedResponseText(se)
		}
		// The substring refines the message only, never the class.
		switch {
		case strings.Contains(raw, "cluster agent disconnected"):
			return Retryable(CodeGeneric, msg+" - retry once the cluster reconnects")
		case throughCluster(req):
			return Retryable(CodeGeneric, msg+" - often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed")
		}
		return Retryable(CodeGeneric, msg+" - retrying may succeed")
	case named && isCloudflareCode(code):
		// A Status body that carries a proxy's own code.
		return Retryable(CodeGeneric, answeredMessage(prefix, req, code, serverDetail(se, reason, noReason), false))
	case webhookCall(reason):
		return webhookCallError(raw, se.ErrStatus.Message, reason, req)
	default:
		return New(CodeGeneric, msg)
	}
}

// isCloudflareCode reports the codes only a CDN in front of Rancher sends:
// 520 to 527 and 530. 528 and 529 are not among them.
func isCloudflareCode(code int) bool {
	return (code >= 520 && code <= 527) || code == 530
}

// AnsweredLead is the start of a message about a status the server answered:
// "<server> answered HTTP <code>" and the standard status text when there
// is one. It reads "the server" when req is nil or names no server.
func AnsweredLead(req *Request, status int) string {
	who := "the server"
	if req != nil && req.Server != "" {
		who = req.Server
	}
	lead := who + " answered HTTP " + strconv.Itoa(status)
	if text := http.StatusText(status); text != "" {
		lead += " " + text
	}
	return lead
}

// answeredMessage words a 429 or a 5xx for a request that names its server:
// the lead of AnsweredLead, then the server's own message (detail) and what
// the status proves, then what to do. prefix is the text the caller put in
// front of the error. unexpected marks an answer whose body was not a
// Kubernetes Status, the one kind whose message can say the cluster agent
// is disconnected.
func answeredMessage(prefix string, req *Request, code int, detail string, unexpected bool) string {
	var parts []string
	if detail != "" {
		parts = append(parts, detail)
	}
	advice := "retry later"
	switch {
	case isCloudflareCode(code) && throughCluster(req):
		parts = append(parts, "cluster "+ShellQuote(req.Cluster)+" could not be reached")
	case isCloudflareCode(code):
		parts = append(parts, "a proxy or load balancer in front of Rancher could not reach Rancher")
	case (code == http.StatusBadGateway || code == http.StatusGatewayTimeout) && throughCluster(req):
		parts = append(parts, "cluster "+ShellQuote(req.Cluster)+" could not be reached")
		advice = "its agent may not be registered yet; retry once the cluster is active"
	case unexpected && strings.Contains(detail, "cluster agent disconnected"):
		advice = "retry once the cluster reconnects"
	}
	lead := AnsweredLead(req, code)
	if len(parts) > 0 {
		lead += ": " + strings.Join(parts, "; ")
	}
	return prefix + lead + " - " + advice
}

// serverDetail is the text the server itself gave for an error, "" when it
// gave none. A Kubernetes Status body gives its message (reason). Any other
// body gives the text of the unexpected-server-response cause, read as
// unexpectedResponseText reads it. Text that only repeats the status, or
// apimachinery's own words for it, is no detail.
func serverDetail(se *apierrors.StatusError, reason string, noReason bool) string {
	code := int(se.ErrStatus.Code)
	text := ""
	if cause, taken, ok := unexpectedCause(se); ok {
		switch {
		case cause == unknownMessage:
		case taken:
			text = bounded(withoutResponseBodies(cause))
		default:
			text = decodeMessage([]byte(cause), withoutResponseBodies)
		}
	} else if !noReason {
		text = bounded(withoutResponseBodies(reason))
	}
	if text == statusText(code) || sameWords(text, code) {
		return ""
	}
	return text
}

// sameWords reports text as apimachinery's own message for code, with the
// " (<verb> <resource> <name>)" suffix a typed request adds cut off.
func sameWords(text string, code int) bool {
	if i := strings.LastIndex(text, " ("); i > 0 && strings.HasSuffix(text, ")") {
		text = text[:i]
	}
	own := apierrors.NewGenericServerResponse(code, "", schema.GroupResource{}, "", "", 0, false)
	return text == own.ErrStatus.Message
}

// unexpectedCause is the server's text recorded in an unexpected server
// response, and whether it was already taken from the body as the CLI shows
// it; ok is false for an error that is not one.
func unexpectedCause(se *apierrors.StatusError) (cause string, taken, ok bool) {
	if se.ErrStatus.Details == nil {
		return "", false, false
	}
	for _, c := range se.ErrStatus.Details.Causes {
		switch c.Type {
		case metav1.CauseTypeUnexpectedServerResponse:
			cause, ok = c.Message, true
		case causeServerMessageTaken:
			taken = true
		}
	}
	return cause, taken, ok
}

// throughCluster reports a call through a cluster other than the local one,
// whose proxy fronts a downstream cluster and its agent.
func throughCluster(req *Request) bool {
	return req != nil && req.Cluster != "" && req.Cluster != "local"
}

// unexpectedResponseText is the text of an unexpected server response as
// apimachinery words it, with the server's text rebuilt as the CLI shows
// every server text: the message of a JSON error object, else the first
// line, at most maxServerMessage bytes, else the status text, or "status
// <code>" where there is none. client-go passes "unknown" for a body it
// does not keep; that reads as no text. The quoted text of a ResponseError
// is already the server's message and is used as it is. A text that does
// not have apimachinery's form is returned as is.
func unexpectedResponseText(se *apierrors.StatusError) string {
	var cause string
	taken := false
	for _, c := range se.ErrStatus.Details.Causes {
		switch c.Type {
		case metav1.CauseTypeUnexpectedServerResponse:
			cause = c.Message
		case causeServerMessageTaken:
			taken = true
		}
	}
	// Whatever follows the quoted text (the verb and resource) is kept.
	rest, ok := strings.CutPrefix(se.Error(), fmt.Sprintf(unexpectedResponseFormat, cause))
	if !ok {
		return se.Error()
	}
	text := ""
	switch {
	case cause == unknownMessage:
	case taken:
		text = bounded(withoutResponseBodies(cause))
	default:
		text = decodeMessage([]byte(cause), withoutResponseBodies)
	}
	if text == "" {
		text = statusText(int(se.ErrStatus.Code))
	}
	return fmt.Sprintf(unexpectedResponseFormat, text) + rest
}

// unexpectedResponseFormat is apimachinery's text for a 5xx that is not a
// Status, the server's text quoted in.
const unexpectedResponseFormat = "an error on the server (%q) has prevented the request from succeeding"

// unexpectedServerResponse reports client-go's shape for a 5xx whose body
// was not a Kubernetes Status: a proxy's plain text, or Rancher's own
// error object, which also carries identity-provider outages. A Status
// 500 from an API server stays out.
func unexpectedServerResponse(se *apierrors.StatusError) bool {
	if !apierrors.IsInternalError(se) || se.ErrStatus.Details == nil {
		return false
	}
	for _, c := range se.ErrStatus.Details.Causes {
		if c.Type == metav1.CauseTypeUnexpectedServerResponse {
			return true
		}
	}
	return false
}

// internalErrorLead is the text apimachinery's NewInternalError puts in
// front of the error it wraps.
const internalErrorLead = "Internal error occurred: "

// webhookCallPhrase starts the text k8s.io/apiserver writes when an
// admission webhook call fails: the phrase, the webhook name in Go's
// double-quoted form, then ": <webhook error>" or webhookNoDetails. The
// webhook error is the Reason of k8s.io/apiserver's ErrCallingWebhook.
const webhookCallPhrase = "failed calling webhook "

// webhookCallLead starts the API server's 500 for a failed admission
// webhook call.
const webhookCallLead = internalErrorLead + webhookCallPhrase

// webhookNoDetails ends that text when the API server has no webhook
// error.
const webhookNoDetails = "; no further details available"

// webhookCallFailed starts the webhook error when the API server's request
// to the webhook returned an error.
const webhookCallFailed = "failed to call webhook: "

// rancherWebhookPrefix starts the name of every webhook rancher-webhook
// registers.
const rancherWebhookPrefix = "rancher.cattle.io."

// maxQuotedWebhookName bounds the text read for the quoted webhook name: a
// webhook name is at most 253 bytes, and Go's quoting writes a byte as at
// most four.
const maxQuotedWebhookName = 4*253 + 2

// maxWebhookBody is the number of bytes of a response body that client-go
// keeps in its error. A body longer than that gives no webhook message.
const maxWebhookBody = 2048

// maxQuotedWebhookBody bounds the text read for the quoted form of a
// response body.
const maxQuotedWebhookBody = 4*maxWebhookBody + 2

// quotedUnknown stands in for a response body that is not shown: the word
// client-go writes for a body it does not keep, quoted as apimachinery
// quotes a body.
const quotedUnknown = `"unknown"`

// responseBodyLead and responseBodyTail are apimachinery's text before and
// after a response body it quotes into an error (unexpectedResponseFormat).
var responseBodyLead, responseBodyTail, _ = strings.Cut(unexpectedResponseFormat, "%q")

// quotedResponseBody finds the first response body that apimachinery
// quoted into s, at or after index from. start is the index of its opening
// quote and end the index just past its closing quote. When what follows
// responseBodyLead is not a quoted string that ends within
// maxQuotedWebhookBody bytes, the body is unterminated: unterminated is
// true and end is len(s). found is false when s holds no responseBodyLead
// there. The function returns indexes, not text.
func quotedResponseBody(s string, from int) (start, end int, unterminated, found bool) {
	_, after, found := strings.Cut(s[from:], responseBodyLead)
	if !found {
		return 0, 0, false, false
	}
	start = len(s) - len(after)
	quoted, err := strconv.QuotedPrefix(after[:min(len(after), maxQuotedWebhookBody)])
	if err != nil {
		return start, len(s), true, true
	}
	return start, start + len(quoted), false, true
}

// withoutResponseBodies returns s with every response body that
// apimachinery quoted into it replaced by quotedUnknown. An unterminated
// body is replaced together with the rest of s, and responseBodyTail ends
// the text. Everything from the first "{" at or after the first replaced
// body is then cut, and U+2026 is added. s is returned as it is when it
// holds no body, and an empty body is kept as sent. Use it for text from a
// server only. For an error's text, which can hold a command, use
// errorTextWithoutResponseBodies: the result of this function can be
// printed in Go-quoted form.
func withoutResponseBodies(s string) string {
	var b strings.Builder
	first := -1
	at := 0
	for {
		start, end, unterminated, found := quotedResponseBody(s, at)
		if !found {
			break
		}
		if !unterminated && end-start == len(`""`) {
			b.WriteString(s[at:end])
			at = end
			continue
		}
		b.WriteString(s[at:start])
		if first < 0 {
			first = b.Len()
		}
		b.WriteString(quotedUnknown)
		if unterminated {
			b.WriteString(responseBodyTail)
			at = len(s)
			break
		}
		at = end
	}
	if first < 0 {
		return s
	}
	b.WriteString(s[at:])
	shown := b.String()
	if before, _, cut := strings.Cut(shown[first:], "{"); cut {
		return shown[:first] + before + "\u2026"
	}
	return shown
}

// errorTextWithoutResponseBodies does the same for an error's text, which
// can hold a command or a value from ShellQuote; its result is never
// printed in Go-quoted form. With keepFirst, when s starts with
// responseBodyLead and the quoted body holds no other quoted response
// body, that body is kept: it is the server's own message in
// apimachinery's form.
func errorTextWithoutResponseBodies(s string, keepFirst bool) string {
	at := 0
	if keepFirst {
		start, end, unterminated, found := quotedResponseBody(s, 0)
		if found && start == len(responseBodyLead) && !strings.Contains(s[start:end], responseBodyLead) {
			if unterminated {
				return s
			}
			at = end
		}
	}
	var b strings.Builder
	b.WriteString(s[:at])
	first := -1
	for {
		start, end, unterminated, found := quotedResponseBody(s, at)
		if !found {
			break
		}
		if !unterminated && end-start == len(`""`) {
			b.WriteString(s[at:end])
			at = end
			continue
		}
		b.WriteString(s[at:start])
		if first < 0 {
			first = b.Len()
		}
		b.WriteString(quotedUnknown)
		if unterminated {
			b.WriteString(responseBodyTail)
			at = len(s)
			break
		}
		at = end
	}
	if first < 0 {
		return s
	}
	b.WriteString(s[at:])
	shown := b.String()
	if before, _, cut := strings.Cut(shown[first:], "{"); cut {
		return shown[:first] + before + "\u2026"
	}
	return shown
}

// webhookCallParts reads the API server's 500 for a failed admission
// webhook call: the webhook name, its quoted form as sent, and the webhook
// error, "" when the text ends with webhookNoDetails. ok is false for any
// other text, a name that is not double-quoted included.
func webhookCallParts(text string) (name, quoted, reason string, ok bool) {
	rest, found := strings.CutPrefix(text, webhookCallLead)
	if !found {
		return "", "", "", false
	}
	quoted, name, found = quotedName(rest)
	if !found {
		return "", "", "", false
	}
	rest = rest[len(quoted):]
	if rest == webhookNoDetails {
		return name, quoted, "", true
	}
	reason, found = strings.CutPrefix(rest, ": ")
	if !found || reason == "" {
		return "", "", "", false
	}
	return name, quoted, reason, true
}

// webhookCall reports whether text is the API server's 500 for a failed
// admission webhook call.
func webhookCall(text string) bool {
	_, _, _, ok := webhookCallParts(text)
	return ok
}

// webhookCallOutcome is what the error of a call to a webhook shows: that
// the request did not reach the webhook (a 503 that is not a Status
// included), that the call timed out (a 504 that is not a Status
// included), or neither (a response, or any other error).
type webhookCallOutcome int

const (
	webhookCallOther webhookCallOutcome = iota
	webhookCallNotReached
	webhookCallTimedOut
)

// notReachedSuffixes are the endings of the error text of a POST request
// that did not reach the webhook: the connection was refused, there was no
// route to the webhook's address, or its host name did not resolve.
var notReachedSuffixes = []string{
	": connect: connection refused",
	": connect: no route to host",
	": no such host",
}

// timedOutSuffixes are the endings of the error text of a POST request
// that timed out.
var timedOutSuffixes = []string{
	": context deadline exceeded",
	": i/o timeout",
	" (Client.Timeout exceeded while awaiting headers)",
	": net/http: TLS handshake timeout",
}

// webhookCallOutcomeOf reads the error of the call to the webhook, the text
// after webhookCallFailed. A POST request that failed with no endpoints for
// the webhook's service did not reach the webhook. apimachinery's text for
// a 503 that is not a Status means a response from the webhook's server, or
// from something in front of it, that did not handle the request; it is
// treated as a request that did not reach the webhook. apimachinery's text
// for a 504 means a timeout.
func webhookCallOutcomeOf(inner string) webhookCallOutcome {
	switch {
	case inner == genericStatusText(http.StatusServiceUnavailable):
		return webhookCallNotReached
	case inner == genericStatusText(http.StatusGatewayTimeout):
		return webhookCallTimedOut
	case !strings.HasPrefix(inner, `Post "`):
		return webhookCallOther
	case strings.Contains(inner, `": dial tcp `) && strings.HasSuffix(inner, ": i/o timeout"):
		// The connection was never opened: the request did not reach the
		// webhook.
		return webhookCallNotReached
	case slices.ContainsFunc(notReachedSuffixes, func(end string) bool { return strings.HasSuffix(inner, end) }),
		strings.Contains(inner, `": no endpoints available for service "`) && strings.HasSuffix(inner, `"`):
		return webhookCallNotReached
	case slices.ContainsFunc(timedOutSuffixes, func(end string) bool { return strings.HasSuffix(inner, end) }):
		return webhookCallTimedOut
	}
	return webhookCallOther
}

// genericStatusText is apimachinery's text for a response with code that
// is not a Status and carries no text of its own.
func genericStatusText(code int) string {
	return apierrors.NewGenericServerResponse(code, http.MethodPost, schema.GroupResource{}, "", "", 0, true).ErrStatus.Message
}

// webhookCallError is the error for the API server's 500 for a failed
// admission webhook call. msg is the error's text as it was, own the Status
// message, and reason that message as the status table read it. The code
// is GENERIC. The error is retryable when the request did not reach the
// webhook (a timeout while connecting to the webhook's address included)
// or the call came back with a 503 that is not a Status. A call that timed
// out in any other way (a 504 that is not a Status included) is not
// retryable: a timeout does not show whether the webhook acted on the
// request. A call that did not reach the webhook, timed out, or came back
// with a 503 or 504 that is not a Status gets the hint that shows
// rancher-webhook's logs when the webhook is one of rancher-webhook's own
// and the CLI's request is to the local cluster. The logs are read on that
// cluster. Every other call, including one where the webhook responded
// with its own body, is not retryable and gets no hint.
//
// Every occurrence of the Status message in msg is replaced by the message
// the CLI shows. In the text around the occurrences each response body is
// replaced by "unknown" (an empty body stays as it is), and everything from
// the first "{" after the first replaced body is cut, with U+2026 added.
// When msg holds no occurrence, only the message the CLI shows is returned:
// the rest of msg is not shown.
func webhookCallError(msg, own, reason string, req *Request) *Error {
	name, quoted, cause, _ := webhookCallParts(reason)
	shown := webhookCallText(name, quoted, cause)
	copied := reason
	if strings.HasSuffix(msg, own) && strings.TrimSpace(own) == reason {
		copied = own
	}
	if parts := strings.Split(msg, copied); len(parts) > 1 {
		for i, p := range parts {
			parts[i] = errorTextWithoutResponseBodies(p, false)
		}
		shown = strings.Join(parts, shown)
	}
	outcome := webhookCallOther
	if inner, called := strings.CutPrefix(cause, webhookCallFailed); called {
		outcome = webhookCallOutcomeOf(inner)
	}
	ce := New(CodeGeneric, shown).WithRetryable(outcome == webhookCallNotReached)
	if outcome != webhookCallOther && strings.HasPrefix(name, rancherWebhookPrefix) && req != nil && req.Cluster == "local" {
		ce = ce.WithHintCommand(NewCommand("logs webhook"))
	}
	return ce
}

// webhookCallText is the message for the API server's 500 for a failed
// admission webhook call, built from the parts webhookCallParts read. When
// the webhook responded with its own message, the text is "admission
// webhook '<name>' failed: " followed by that message. In every other case
// it is the API server's text with the response bodies in the webhook
// error replaced, everything from the first "{" the webhook error still
// holds cut, and the webhook error cut to maxServerMessage bytes.
func webhookCallText(name, quoted, reason string) string {
	lead := webhookCallLead + quoted
	if reason == "" {
		return lead + webhookNoDetails
	}
	if inner, called := strings.CutPrefix(reason, webhookCallFailed); called {
		if text, body, found := webhookResponseBody(inner); found {
			if m := webhookResponseMessage(body); m != "" {
				return "admission webhook " + ShellQuote(name) + " failed: " + m
			}
			reason = webhookCallFailed + text
		}
	}
	text := withoutResponseBodies(reason)
	if before, _, cut := strings.Cut(text, "{"); cut {
		text = before + "\u2026"
	}
	return lead + ": " + bounded(text)
}

// webhookResponseBody finds the webhook's response body in the error of
// the call. The body is quoted into apimachinery's text for a 5xx response
// that is not a Status (whatever follows the quoted body is kept), or it
// is the whole text when that starts with "{", as apimachinery keeps the
// body of a 403, 406 or 415 response. text is the error with the body
// replaced by "unknown", or the error as it is when the body is empty.
// When the quoted body is unterminated or does not unquote, body is "".
// found is false when the error holds no response body.
func webhookResponseBody(inner string) (text, body string, found bool) {
	replaced := fmt.Sprintf(unexpectedResponseFormat, unknownMessage)
	if strings.HasPrefix(inner, responseBodyLead) {
		start, end, cut, _ := quotedResponseBody(inner, 0)
		if cut {
			return replaced, "", true
		}
		unquoted, err := strconv.Unquote(inner[start:end])
		if err != nil {
			return replaced, "", true
		}
		if strings.TrimSpace(unquoted) == "" {
			return inner, "", true
		}
		return responseBodyLead + quotedUnknown + inner[end:], unquoted, true
	}
	if strings.HasPrefix(inner, "{") {
		if len(inner) > maxWebhookBody {
			return unknownMessage, "", true
		}
		return unknownMessage, inner, true
	}
	return inner, "", false
}

// webhookResponseMessage is the webhook's own message in its response
// body, or "" when the body has none or cannot be used. The message is cut
// to maxServerMessage bytes. It is the Status message of an AdmissionReview
// response; or the message of a JSON error object that has no apiVersion and
// no metadata and whose kind is absent or Status; or the first line of a
// body that is not JSON and starts with neither "{" nor "[". A body that
// starts with "{" or "[" and does not decode has no message, and neither
// has a body longer than client-go keeps.
func webhookResponseMessage(body string) string {
	text := strings.TrimSpace(body)
	if text == "" || text == unknownMessage || len(text) > maxWebhookBody {
		return ""
	}
	if !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		return serverMessage([]byte(text))
	}
	var review struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Metadata   json.RawMessage `json:"metadata"`
		Response   *struct {
			Status *metav1.Status `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(text), &review) != nil {
		return ""
	}
	if review.Response != nil && review.Response.Status != nil {
		return bounded(review.Response.Status.Message)
	}
	if review.APIVersion != "" || review.Metadata != nil || (review.Kind != "" && review.Kind != "Status") {
		return ""
	}
	m, _ := objectMessage(text)
	return m
}
