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
// whose message is empty — Rancher's proxy paths reject an
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
	switch {
	case apierrors.IsNotFound(err):
		return clierr.New(clierr.CodeNotFound, err.Error())
	case apierrors.IsForbidden(err):
		return withDeniedFallback(clierr.New(clierr.CodeRBAC, err.Error()), http.StatusForbidden)
	case apierrors.IsUnauthorized(err):
		return withDeniedFallback(clierr.New(clierr.CodeAuth, err.Error()), http.StatusUnauthorized)
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		// Both are HTTP 409 but distinct predicates: Conflict = resourceVersion
		// race, AlreadyExists = duplicate name on create.
		return clierr.New(clierr.CodeConflict, err.Error())
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return clierr.New(clierr.CodeValidation, err.Error())
	case apierrors.IsServiceUnavailable(err):
		return clierr.Retryable(clierr.CodeGeneric, err.Error()).
			WithHint("the target cluster is unavailable — its agent may not be registered yet; retry once the cluster is active")
	case errors.Is(err, context.DeadlineExceeded):
		// A hung request expires the client timeout without any HTTP status —
		// client-go hands back a url.Error wrapping context.DeadlineExceeded.
		// This arm serves management-plane reads and downstream agent-tunnel
		// reads alike, and the two causes are indistinguishable here, so the
		// hint names both.
		return clierr.Retryable(clierr.CodeGeneric, err.Error()).
			WithHint("the request timed out — the server may be slow, or, if the target is a downstream cluster, its agent may be disconnected (check with: rancher inspect cluster <cluster>)")
	case isUnexpectedServerError(err):
		ce := clierr.Retryable(clierr.CodeGeneric, err.Error())
		// The substring refines the hint only — it is never the classifier,
		// so a reworded proxy body still lands retryable with the generic
		// proxy-path hint instead of silently flipping non-retryable.
		if strings.Contains(err.Error(), "cluster agent disconnected") {
			return ce.WithHint("the cluster agent is disconnected — retry once the cluster reconnects")
		}
		return ce.WithHint("the server answered with an unstructured 5xx — often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed")
	default:
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
}

// withDeniedFallback substitutes deniedFallbackMessage(status) and a hint
// pointing at `rancher auth check` when ce's message is empty or
// whitespace-only, otherwise returns ce untouched.
func withDeniedFallback(ce *clierr.Error, status int) *clierr.Error {
	if strings.TrimSpace(ce.Message) != "" {
		return ce
	}
	ce.Message = deniedFallbackMessage(status)
	return ce.WithHint("rancher auth check")
}

// isUnexpectedServerError reports client-go's wrapping of a non-Status
// 5xx body (InternalError reason + UnexpectedServerResponse cause) — the
// shape every Rancher proxy-path failure takes, because the proxy answers
// with plain text (e.g. "error trying to reach service: cluster agent
// disconnected"). A genuine k8s-Status 500, like Rancher's
// stale-precondition delete in fidelity/probes.go, carries no such cause
// and stays out of this arm.
func isUnexpectedServerError(err error) bool {
	if !apierrors.IsInternalError(err) {
		return false
	}
	var se *apierrors.StatusError
	if !errors.As(err, &se) {
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
