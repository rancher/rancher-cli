package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deniedFallbackMessage substitutes for a Forbidden/Unauthorized error
// whose message is empty - Rancher's proxy paths reject an
// invalid/insufficient token with an empty-body 403 (and a direct k8s
// endpoint may do the same with a 401), and apierrors' err.Error() passes
// that through verbatim, so the envelope would otherwise carry a code
// without any explanation. One template parameterized by the actual HTTP
// status, so the 401 and 403 wording can never drift out of sync.
func deniedFallbackMessage(status int) string {
	return fmt.Sprintf("server denied the request (%d): the credential may be invalid or lack permission", status)
}

// mapErr translates a Kubernetes API error into a structured *clierr.Error so the
// pipeline renders it with the right exit code. Returns nil for nil.
func mapErr(err error) error {
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
		return withDeniedFallback(clierr.New(clierr.CodeRBAC, err.Error()), http.StatusForbidden)
	case apierrors.IsUnauthorized(err):
		return withDeniedFallback(clierr.New(clierr.CodeAuth, err.Error()), http.StatusUnauthorized)
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
			err.Error()+" - the server may be slow, or, if the target is a downstream cluster, its agent may be disconnected (check with: rancher inspect cluster <cluster-id>)")
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

// withDeniedFallback substitutes deniedFallbackMessage(status) and a hint
// pointing at `rancher auth status` when ce's message is empty or
// whitespace-only, otherwise returns ce untouched.
func withDeniedFallback(ce *clierr.Error, status int) *clierr.Error {
	if strings.TrimSpace(ce.Message) != "" {
		return ce
	}
	ce.Message = deniedFallbackMessage(status)
	return ce.WithHint("rancher auth status")
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
