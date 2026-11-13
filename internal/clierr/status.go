package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// AuthStatusCommand is the command a refused credential names.
var AuthStatusCommand = NewCommand("auth status")

// AuthStatusRemedy is AuthStatusCommand as the text of a hint.
var AuthStatusRemedy = AuthStatusCommand.Field()

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
// marks it.
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
	return se
}

// causeServerMessageTaken marks an unexpected server response whose
// quoted message is already the server's message as the CLI shows it, not
// the body client-go quotes.
const causeServerMessageTaken metav1.CauseType = "ServerMessageTaken"

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
	return decodeMessage(body, func(s string) string { return strings.TrimSpace(filter(s)) })
}

// keepText is the filter of the callers that show the message as sent.
func keepText(s string) string { return s }

// decodeMessage is serverMessage with filter run on the decoded text before
// it is bounded.
func decodeMessage(body []byte, filter func(string) string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	if m, ok := objectMessageFiltered(text, filter); ok {
		return m
	}
	if json.Valid([]byte(text)) {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	return bounded(strings.TrimRight(filter(line), "\r"))
}

// objectMessage decodes text as a JSON error object.
func objectMessage(text string) (string, bool) { return objectMessageFiltered(text, keepText) }

// objectMessageFiltered is objectMessage with filter run on the message
// before it is trimmed and bounded.
func objectMessageFiltered(text string, filter func(string) string) (string, bool) {
	if !strings.HasPrefix(text, "{") {
		return "", false
	}
	var obj errorObject
	if json.Unmarshal([]byte(text), &obj) != nil {
		return "", false
	}
	return bounded(strings.TrimSpace(filter(obj.Message))), true
}

// bounded cuts s to maxServerMessage bytes on a character boundary.
func bounded(s string) string {
	if len(s) <= maxServerMessage {
		return s
	}
	cut := maxServerMessage
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
			if req != nil && !req.NoCredential {
				ce = ce.WithHintCommand(AuthStatusCommand)
			}
			return ce
		}
		return New(CodeAuth, msg)
	case apierrors.IsConflict(se) && req != nil && req.Precondition:
		// A resourceVersion precondition race: another writer updated the
		// object between the read and this write; a fresh read-modify-write
		// resolves it. The reason alone cannot tell this from a webhook's
		// refusal. A webhook 409 on a write that sent a resourceVersion
		// therefore gets the same message.
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
		case strings.Contains(msg, "cluster agent disconnected"):
			return Retryable(CodeGeneric, msg+" - retry once the cluster reconnects")
		case throughCluster(req):
			return Retryable(CodeGeneric, msg+" - often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed")
		}
		return Retryable(CodeGeneric, msg+" - retrying may succeed")
	case named && isCloudflareCode(code):
		// A Status body that carries a proxy's own code.
		return Retryable(CodeGeneric, answeredMessage(prefix, req, code, serverDetail(se, reason, noReason), false))
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
			text = cause
		default:
			text = serverMessage([]byte(cause))
		}
	} else if !noReason {
		text = bounded(reason)
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
		text = cause
	default:
		text = serverMessage([]byte(cause))
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
