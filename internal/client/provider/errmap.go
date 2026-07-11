package provider

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// deniedFallbackMessage substitutes for a Forbidden/Unauthorized error whose
// message is empty — Rancher's proxy paths reject an invalid/insufficient
// token with an empty-body 403, and apierrors' err.Error() passes that
// through verbatim, so the envelope would otherwise carry code without any
// explanation.
const deniedFallbackMessage = "server denied the request (403): the credential may be invalid or lack permission"

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
		return withDeniedFallback(clierr.New(clierr.CodeRBAC, err.Error()))
	case apierrors.IsUnauthorized(err):
		return withDeniedFallback(clierr.New(clierr.CodeAuth, err.Error()))
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		// Both are HTTP 409 but distinct predicates: Conflict = resourceVersion
		// race, AlreadyExists = duplicate name on create.
		return clierr.New(clierr.CodeConflict, err.Error())
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return clierr.New(clierr.CodeValidation, err.Error())
	default:
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
}

// withDeniedFallback substitutes deniedFallbackMessage and a hint pointing
// at `rancher auth check` when ce's message is empty or whitespace-only,
// otherwise returns ce untouched.
func withDeniedFallback(ce *clierr.Error) *clierr.Error {
	if strings.TrimSpace(ce.Message) != "" {
		return ce
	}
	ce.Message = deniedFallbackMessage
	return ce.WithHint("rancher auth check")
}
