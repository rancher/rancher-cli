package projects

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// creatorIDAnnotation names the user who created a project. Rancher adds the
// creator's owner binding to the project only when it is present.
const creatorIDAnnotation = "field.cattle.io/creatorId"

// noCreatorRBACAnnotation, with any value, tells Rancher to add no creator
// binding. The server refuses a project that carries both annotations.
const noCreatorRBACAnnotation = "field.cattle.io/no-creator-rbac"

// creatorWarningPrefix is the shared prefix of every warning for a creatorId
// annotation the command could not set. Test assertions and the e2e txtar
// substrings are anchored by this const on the code side.
const creatorWarningPrefix = "creatorId annotation not set"

// validUserIDRe is DNS-1123-ish: lowercase alphanumerics, dots, and hyphens,
// starting with a lowercase alphanumeric - the shape of Rancher User object
// names stored in status.userID.
var validUserIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

var (
	errNoUserID      = errors.New("the SelfUser response has no user ID")
	errInvalidUserID = errors.New("the SelfUser response has an invalid user ID")
)

// callerUserID gets the user ID the request runs as with a SelfUser request.
// The request is a real (non-dry-run) create of a create-only "whoami"
// object, so it is a network write during Plan, before any --apply gate. The
// error is the client's own for a failed request, errNoUserID or
// errInvalidUserID otherwise.
func callerUserID(ctx context.Context, client engine.ClusterClient) (string, error) {
	answer, err := client.Create(ctx, selfuser.GVR, "", selfuser.New(), false)
	if err != nil {
		return "", err
	}
	userID := selfuser.UserID(answer)
	if userID == "" {
		return "", errNoUserID
	}
	if !validUserIDRe.MatchString(userID) {
		return "", errInvalidUserID
	}
	return userID, nil
}

// SetCreatorID sets metadata.annotations["field.cattle.io/creatorId"] on a
// Project to the user the create runs as, before the dry-run, so that Rancher
// adds the creator's owner binding. It returns a warning, or "" when there is
// nothing to say.
//
//   - A project with the no-creator-rbac annotation gets no request and no
//     change.
//   - A non-empty creatorId equal to the caller's user ID is kept. A different
//     one is refused with a VALIDATION error, and so is any non-empty one when
//     the CLI cannot get the user ID: the comparison cannot run, and the CLI
//     never changes a value the user wrote.
//   - Otherwise (no creatorId, or an empty one) the annotation is set to the
//     caller's user ID. When the CLI cannot get the user ID the command warns
//     and goes on with no annotation.
//
// Other annotations stay as they are.
func SetCreatorID(ctx context.Context, client engine.ClusterClient, obj *unstructured.Unstructured) (string, error) {
	annotations, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "metadata", "annotations")
	existing, _ := annotations.(map[string]any)
	if _, ok := existing[noCreatorRBACAnnotation]; ok {
		return "", nil
	}
	manifestValue := ""
	if v, ok := existing[creatorIDAnnotation]; ok && v != nil {
		manifestValue = fmt.Sprint(v)
	}

	userID, err := callerUserID(ctx, client)
	if manifestValue != "" {
		return "", checkManifestCreatorID(manifestValue, userID, err)
	}
	if err != nil {
		return creatorWarning(err.Error()), nil
	}
	if err := unstructured.SetNestedField(obj.Object, userID, "metadata", "annotations", creatorIDAnnotation); err != nil {
		return creatorWarning("could not add it to the project: " + err.Error()), nil
	}
	return "", nil
}

// checkManifestCreatorID returns nil when the manifest's creatorId equals the
// caller's user ID and a refusal otherwise; lookupErr is the failure of the
// user ID lookup, when there was one.
func checkManifestCreatorID(manifestValue, userID string, lookupErr error) error {
	const compareFailed = "the manifest sets " + creatorIDAnnotation +
		" and the CLI could not get your user ID to compare it; remove the annotation, or log in again and retry"
	if lookupErr != nil {
		if errors.Is(lookupErr, errNoUserID) || errors.Is(lookupErr, errInvalidUserID) {
			return clierr.New(clierr.CodeGeneric, lookupErr.Error()+": "+compareFailed)
		}
		if ce, ok := errors.AsType[*clierr.Error](lookupErr); ok {
			return ce.WithWarnings(compareFailed)
		}
		return clierr.New(clierr.CodeGeneric, lookupErr.Error()+": "+compareFailed)
	}
	if manifestValue == userID {
		return nil
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the CLI sets the creator of a project to you, and the manifest names another user in %s (%s); remove the annotation, or set it to your own user ID (%s)",
		creatorIDAnnotation, clierr.IDText(manifestValue), userID))
}

// creatorWarning is the warning for a creatorId annotation the command could
// not set. It states the consequence: Rancher adds no owner binding when the
// annotation is missing.
func creatorWarning(cause string) string {
	return creatorWarningPrefix + " (" + cause + "): Rancher adds no owner binding for you on this project and you may not be able to see or manage it after it is created; " +
		"log in again, or check that you may create selfusers.ext.cattle.io, before you run --apply; " +
		"a project already created keeps no owner binding: a cluster owner or an administrator grants the role"
}
