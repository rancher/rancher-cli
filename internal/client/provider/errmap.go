package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// request describes the call an error answers, so a refusal that carries
// no reason can name what was refused. cluster is the --cluster value the
// client targets.
type request struct {
	verb, group, resource, namespace, name, cluster string
}

// what is "<verb> <resource>.<group>", the group left out for the core
// group.
func (q *request) what() string {
	gr := q.resource
	if q.group != "" {
		gr += "." + q.group
	}
	return q.verb + " " + gr
}

// localManagementRefusal is why a management.cattle.io request through the
// local cluster's proxy can be refused with no reason.
const localManagementRefusal = "; through the local cluster's proxy, Rancher answers some management.cattle.io resources, clusters among them, only for users with access to the local cluster"

// unauthorizedDefault is apimachinery's message for a 401 whose body
// carried none.
const unauthorizedDefault = "the server has asked for the client to provide credentials"

// forbiddenText words a 403 that gave no reason: client-go leaves the
// message empty for the dynamic client's AbsPath requests, and Rancher's
// proxy answers a user without access with an empty body.
func forbiddenText(q *request) string {
	if q == nil {
		return "the server refused the request (403) and gave no reason"
	}
	name := ""
	if q.name != "" {
		name = " " + clierr.ShellQuote(q.name)
	}
	msg := "the server refused " + q.what() + name + " (403) and gave no reason"
	if q.cluster == "local" && q.group == "management.cattle.io" {
		msg += localManagementRefusal
	}
	return msg
}

// unauthorizedText words a 401 by its status alone.
func unauthorizedText(q *request) string {
	if q == nil {
		return "the server rejected the credential (401)"
	}
	return "the server rejected the credential (401) for " + q.what()
}

// mapErr translates a Kubernetes API error into a structured *clierr.Error so the
// pipeline renders it with the right exit code. Returns nil for nil.
func mapErr(err error) error { return mapRequestErr(err, nil) }

// mapRequestErr is mapErr for the call q describes; q is nil when there is
// no request to name.
func mapRequestErr(err error, q *request) error {
	if err == nil {
		return nil
	}
	// An error that already carries the structured shape (a refused
	// cross-origin redirect arrives from the transport wrapped in a
	// *url.Error) is returned as is: rebuilding it from its text would drop
	// its code and retryability.
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return ce
	}
	switch {
	case apierrors.IsNotFound(err):
		return clierr.New(clierr.CodeNotFound, err.Error())
	case apierrors.IsForbidden(err):
		return withDeniedFallback(clierr.New(clierr.CodeRBAC, err.Error()), http.StatusForbidden, q)
	case apierrors.IsUnauthorized(err):
		return withDeniedFallback(clierr.New(clierr.CodeAuth, err.Error()), http.StatusUnauthorized, q)
	case apierrors.IsConflict(err):
		// A resourceVersion precondition race: some other writer (e.g. a
		// reconciling controller) updated the object between the read and
		// this write. The object itself is fine - a fresh
		// read-modify-write against the current version resolves it, so
		// this is retryable, and the raw Kubernetes wording ("the object
		// has been modified") never reaches the caller.
		return clierr.Retryable(clierr.CodeConflict,
			"the resource changed on the server since it was last read; retry the command")
	case apierrors.IsAlreadyExists(err):
		// A duplicate name on create - retrying without changing the name
		// never succeeds.
		return clierr.New(clierr.CodeConflict, err.Error())
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return clierr.New(clierr.CodeValidation, err.Error())
	case apierrors.IsServiceUnavailable(err), isGatewayError(err):
		return clierr.Retryable(clierr.CodeGeneric,
			err.Error()+" - its agent may not be registered yet; retry once the cluster is active")
	case errors.Is(err, context.DeadlineExceeded):
		// A hung request expires the client timeout without any HTTP status -
		// client-go hands back a url.Error wrapping context.DeadlineExceeded.
		// This arm serves management-plane reads and downstream agent-tunnel
		// reads alike, and the two causes are indistinguishable here, so the
		// message names both; which cluster to inspect is unknown here, so
		// no hint names one.
		return clierr.Retryable(clierr.CodeGeneric,
			err.Error()+" - the server may be slow, or, if the target is a downstream cluster, its agent may be disconnected (check with: "+InspectClusterRemedy+")")
	case isUnexpectedServerError(err):
		msg := err.Error()
		// The substring refines the message only - it is never the classifier,
		// so a reworded proxy body still lands retryable with the generic
		// proxy-path wording instead of silently flipping non-retryable.
		if strings.Contains(msg, "cluster agent disconnected") {
			msg += " - retry once the cluster reconnects"
		} else {
			msg += " - often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed"
		}
		return clierr.Retryable(clierr.CodeGeneric, msg)
	default:
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
}

// InspectClusterRemedy and AuthStatusRemedy are the commands the error
// mapping names: the cluster's own state when a read timed out, the
// session's when the server refused it.
const (
	InspectClusterRemedy = clierr.Rancher + " inspect cluster <cluster-id>"
	AuthStatusRemedy     = clierr.Rancher + " auth status"
)

// withDeniedFallback words a 403 or 401 that gave no reason: an empty or
// whitespace-only message, and for a 401 also apimachinery's default one.
// A 401 names `rancher auth status`; a 403's hint, when there is one, is
// the caller's (kinds.ForbiddenHint). Any other message is kept.
func withDeniedFallback(ce *clierr.Error, status int, q *request) *clierr.Error {
	msg := strings.TrimSpace(ce.Message)
	switch {
	case status == http.StatusUnauthorized && (msg == "" || msg == unauthorizedDefault):
		ce.Message = unauthorizedText(q)
		return ce.WithHint(AuthStatusRemedy)
	case status == http.StatusForbidden && msg == "":
		ce.Message = forbiddenText(q)
	}
	return ce
}

// isGatewayError reports a 502 or 504 StatusError; both are retryable
// like 503, while a plain 500 stays non-retryable.
func isGatewayError(err error) bool {
	se, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok {
		return false
	}
	return se.ErrStatus.Code == http.StatusBadGateway || se.ErrStatus.Code == http.StatusGatewayTimeout
}

// isUnexpectedServerError reports client-go's wrapping of a non-Status
// 5xx body (InternalError reason + UnexpectedServerResponse cause) - the
// shape every Rancher proxy-path failure takes, because the proxy answers
// with plain text (e.g. "error trying to reach service: cluster agent
// disconnected"). A genuine k8s-Status 500, like Rancher's
// stale-precondition delete in fidelity/probes.go, carries no such cause
// and stays out of this arm.
func isUnexpectedServerError(err error) bool {
	if !apierrors.IsInternalError(err) {
		return false
	}
	se, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok {
		return false
	}
	if d := se.ErrStatus.Details; d != nil {
		for _, c := range d.Causes {
			if c.Type == metav1.CauseTypeUnexpectedServerResponse {
				return true
			}
		}
	}
	return false
}

// restoreStatus returns the Kubernetes Status a raw request's error lost.
// client-go builds a raw request's error from the response body only when
// the body is text/plain; the application/json Status an API server answers
// a refused pods/log read with is dropped, and the error reads "unknown".
// When body decodes as a Status the error is rebuilt from it, so the
// server's own message reaches the caller. Any other body - empty, log text,
// or Rancher's proxy error object for a caller without cluster access -
// leaves err as it was: an empty body keeps client-go's generic wording,
// and a text/plain proxy failure keeps its retryable mapping. A 5xx whose
// body is a Status is the API server's own answer, not a proxy-path
// failure, and maps like one from every typed request.
func restoreStatus(err error, body []byte) error {
	if err == nil || len(body) == 0 {
		return err
	}
	var st metav1.Status
	if json.Unmarshal(body, &st) != nil || st.Kind != "Status" || st.Code == 0 {
		return err
	}
	return &apierrors.StatusError{ErrStatus: st}
}
