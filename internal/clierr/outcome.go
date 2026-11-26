package clierr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RetryableInOutput is whether output says the error is retryable: Retryable,
// and never for an error with OutcomeUnknown, whatever a caller set the field
// to. The JSON and YAML envelopes and the text block read it.
func (e *Error) RetryableInOutput() bool { return e.Retryable && !e.OutcomeUnknown }

// MarshalJSON writes the fields of the error with retryable taken from
// RetryableInOutput. The YAML envelope is made from this JSON. It escapes
// no HTML character: the output encoder decides that, and a text already
// escaped here would stay escaped.
func (e Error) MarshalJSON() ([]byte, error) {
	type fields Error
	f := fields(e)
	f.Retryable = e.RetryableInOutput()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// unknownHead is the start of the message of a write whose result the CLI
// does not know: sent names the request, cause what ended the call.
func unknownHead(sent, cause string) string {
	return sent + " was sent, but whether the server applied the change is unknown (" + cause + "); "
}

// beforeAgain leads the step to take before the command runs again.
const beforeAgain = "before you run the command again, "

// UnknownOutcome is the error for a write whose request was sent and whose
// result the CLI does not know. sent names the request ("the request to
// create configmaps 'web'"), cause says what ended the call (OutcomeCause
// words it), err is that error and read is the command that shows whether
// the change is on the server, the zero Command for none. With a command
// the message ends "check with: <read>" and the hint is the command; without
// one, or with one that cannot be printed, the message says to check whether
// the change is on the server and the error has no hint. The error is
// GENERIC, not retryable, has OutcomeUnknown set and keeps err as its cause:
// HTTPStatus and errors.Is still read it.
func UnknownOutcome(sent, cause string, err error, read Command) *Error {
	text := unknownHead(sent, cause) + beforeAgain + "check whether the change is on the server"
	if read.start != "" {
		if read.OK() {
			text = Text(unknownHead(sent, cause), Lead(beforeAgain+"check with: "), read)
		} else {
			text = WithLeftOutNote(text)
		}
	}
	e := New(CodeGeneric, text).WithCause(err)
	e.OutcomeUnknown = true
	if read.start != "" && read.OK() {
		e = e.WithHintCommand(read)
	}
	return e
}

// UnknownOutcomeUnreadable is UnknownOutcome for a write of an object the
// caller cannot get: a get command would be refused. The message says so and
// to have a user who can get the object check it, or to list the objects of
// its kind (list) when the caller can; the error has no hint, because the
// list may be refused too.
func UnknownOutcomeUnreadable(sent, cause string, err error, list Command) *Error {
	ask := unknownHead(sent, cause) + "you cannot get the object: " + beforeAgain + "ask a user who can get it to check whether the change is on the server"
	text := ask
	if list.start != "" {
		text = Text(ask, Lead(", or list the objects of its kind if you can: "), list)
	}
	e := New(CodeGeneric, text).WithCause(err)
	e.OutcomeUnknown = true
	return e
}

// OutcomeCause words what ended a write whose result is unknown: the status
// the server responded with and the text it gave, a timeout, a stopped
// command or a failed connection. The URL of the request is left out, because
// it names nothing the reader needs. The server's text is kept as the message
// of an answered status shows it (serverDetail); a response with none is
// worded by its status alone.
func OutcomeCause(err error) string {
	if status, ok := HTTPStatus(err); ok {
		cause := "the server responded with " + strconv.Itoa(status)
		if text := serverText(err); text != "" {
			cause += ": " + text
		}
		return cause
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "the command was stopped"
	}
	return "the connection failed"
}

// serverText is the text the server gave in the status of err, "" for an
// error with no status or a response with no text.
func serverText(err error) string {
	se, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok {
		return ""
	}
	reading := readStatus(err, se)
	return serverDetail(se, reading.reason, reading.noReason)
}

// NotAKubernetesStatus reports whether err holds a *apierrors.StatusError
// that client-go built for a response whose body is not a Kubernetes Status:
// a proxy's text, or a JSON object that is not a Status, such as
// Rancher's own error objects. It reads the unexpected-server-response cause
// client-go records for such a body, whatever the status code. A Status that
// carries that cause reads true too: an API server built on
// k8s.io/apiserver sends the details of a StatusError unchanged, and a
// decoded Status cannot be told from a body client-go did not decode. That
// is the cautious side for a write: its result is reported as unknown. It
// is false for an error that holds no StatusError, for a StatusError with no
// details and for one whose details hold no such cause.
func NotAKubernetesStatus(err error) bool {
	se, ok := errors.AsType[*apierrors.StatusError](err)
	return ok && apierrors.IsUnexpectedServerError(se)
}
