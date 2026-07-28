package provider

import (
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
		return clierr.New(clierr.CodeGeneric, err.Error()).
			WithRetryable(true).
			WithHint("the target cluster is unavailable — its agent may not be registered yet; retry once the cluster is active")
	case isAgentDisconnected(err):
		return clierr.New(clierr.CodeGeneric, err.Error()).
			WithRetryable(true).
			WithHint("the cluster agent is disconnected — retry once the cluster reconnects")
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

// isAgentDisconnected reports the Rancher proxy's post-registration
// agent-disconnect failure. The proxy answers a request to a downstream
// cluster whose agent has dropped with a plain-text 5xx body ("error
// trying to reach service: cluster agent disconnected"); client-go wraps
// any such non-Status 5xx as a generic InternalError StatusError with an
// UnexpectedServerResponse cause. There is no typed discriminator for the
// disconnect itself, so after the typed narrowing (InternalError reason +
// UnexpectedServerResponse cause — a genuine k8s Status 500, e.g.
// Rancher's stale-precondition delete in fidelity/probes.go, carries
// neither) the message substring is the last-resort test.
func isAgentDisconnected(err error) bool {
	if !apierrors.IsInternalError(err) {
		return false
	}
	var se *apierrors.StatusError
	if !errors.As(err, &se) {
		return false
	}
	unexpected := false
	if d := se.ErrStatus.Details; d != nil {
		for _, c := range d.Causes {
			if c.Type == metav1.CauseTypeUnexpectedServerResponse {
				unexpected = true
				break
			}
		}
	}
	return unexpected && strings.Contains(se.ErrStatus.Message, "cluster agent disconnected")
}
