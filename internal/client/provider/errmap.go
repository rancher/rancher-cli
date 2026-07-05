package provider

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

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
		return clierr.New(clierr.CodeRBAC, err.Error())
	case apierrors.IsUnauthorized(err):
		return clierr.New(clierr.CodeAuth, err.Error())
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
