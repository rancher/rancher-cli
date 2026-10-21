package rbac

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResolveInspectUser resolves the `inspect user <user>` positional to a
// user. This is an INSPECT-SPECIFIC resolution mode, built from the same
// rbac resolution pieces as ResolveSubject but deliberately divergent from
// it: ResolveSubject rejects system users terminally, skips them in scans,
// uses grant-vocabulary error messages, and can PROVISION an unowned
// principal - all wrong for a read-only inspection. Here: system users
// resolve normally (their card carries the system marker); nothing is ever
// provisioned (an unowned external principal is NOT_FOUND); a group
// principal or an unrecognized principal prefix is VALIDATION-rejected
// (inspect has no --type to disambiguate one).
func ResolveInspectUser(ctx context.Context, clients engine.ClusterClientFactory, arg string) (ResolvedSubject, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return ResolvedSubject{}, err
	}
	userKind, _ := kinds.Lookup("user")

	if strings.Contains(arg, "://") {
		return resolveInspectUserPrincipal(ctx, client, userKind.GVR, arg)
	}
	return resolveInspectUserBare(ctx, client, userKind.GVR, arg)
}

// resolveInspectUserPrincipal resolves a fully qualified principal argument.
// Ownership is proven by scanning Users' principalIds - the only
// authoritative source; a deterministic predicted-user-id GET is not used,
// since the scan alone already covers every case the prediction would
// only optimize.
func resolveInspectUserPrincipal(ctx context.Context, client engine.ClusterClient, userGVR schema.GroupVersionResource, principal string) (ResolvedSubject, error) {
	prefix, remainder, _ := strings.Cut(principal, "://")
	if remainder == "" {
		return ResolvedSubject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("principal %q has empty remainder after ://; provide the full principal id, e.g. local://<user-id>", principal))
	}

	switch {
	case engine.IsSystemPrincipal(principal):
		// A system principal is still a USER-owned principal (Rancher's
		// system users), so it takes the ownership-scan path below like any
		// other qualified principal - never rejected as an unknown prefix.
	case prefix == "local" || strings.HasSuffix(prefix, "_user"):
		// Recognized user-principal form.
	case strings.HasSuffix(prefix, "_group") || prefix == "github_org" || prefix == "github_team":
		return ResolvedSubject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%q is a group principal; inspect user only supports individual users; use grant/revoke to work with group role bindings", principal))
	default:
		return ResolvedSubject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cannot infer subject type from principal %q; pass a user id, username, displayName, or a local:// / <provider>_user:// principal", principal))
	}

	list, err := client.List(ctx, userGVR, "", "")
	if err != nil {
		return ResolvedSubject{}, err
	}
	var owners []*unstructured.Unstructured
	for i := range list.Items {
		u := &list.Items[i]
		pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
		if slices.Contains(pids, principal) {
			owners = append(owners, u)
		}
	}
	switch len(owners) {
	case 0:
		return ResolvedSubject{}, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("no user found for principal %q", principal)).
			WithHint("rancher get users")
	case 1:
		pids, _, _ := unstructured.NestedStringSlice(owners[0].Object, "principalIds")
		return ResolvedSubject{UserID: owners[0].GetName(), Principal: principal, PrincipalIDs: pids}, nil
	default:
		cands := make([]clierr.Candidate, 0, len(owners))
		for _, u := range owners {
			cands = append(cands, UserCandidate(u))
		}
		return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d users share principal %q", len(owners), principal)).
			WithCandidates(cands)
	}
}

// resolveInspectUserBare resolves a user id, username, or displayName.
// Exact-id Get wins outright (ResolveSubject's precedence; unlike the
// project and cluster lookups of engine.MatchOne, it matches username as
// well as displayName and names no other user in a warning); on a Get
// miss, username/displayName collisions become AMBIGUOUS_SELECTION. System
// users resolve at every step - never excluded, never terminal.
func resolveInspectUserBare(ctx context.Context, client engine.ClusterClient, userGVR schema.GroupVersionResource, bare string) (ResolvedSubject, error) {
	obj, err := client.Get(ctx, userGVR, "", bare)
	if err == nil {
		return resolvedInspectUser(obj), nil
	}
	if !clierr.IsNotFound(err) {
		return ResolvedSubject{}, err
	}

	list, err := client.List(ctx, userGVR, "", "")
	if err != nil {
		return ResolvedSubject{}, err
	}
	var matches []*unstructured.Unstructured
	for i := range list.Items {
		u := &list.Items[i]
		display, _, _ := unstructured.NestedString(u.Object, "displayName")
		username, _, _ := unstructured.NestedString(u.Object, "username")
		if display == bare || username == bare {
			matches = append(matches, u)
		}
	}
	switch len(matches) {
	case 1:
		return resolvedInspectUser(matches[0]), nil
	case 0:
		return ResolvedSubject{}, clierr.New(clierr.CodeNotFound, "user not found: "+bare).
			WithCandidates(SubjectCandidates(toPointerSlice(list.Items), bare)).
			WithHint("rancher get users")
	default:
		cands := make([]clierr.Candidate, 0, len(matches))
		for _, u := range matches {
			cands = append(cands, UserCandidate(u))
		}
		return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d users match %q", len(matches), bare)).
			WithCandidates(cands)
	}
}

// resolvedInspectUser builds a ResolvedSubject from a fetched User object.
// Unlike grant/revoke's resolvedFromUser, this never errors on an empty
// principalIds: inspect must be able to resolve and later render a
// malformed user rather than refuse it outright. PreferredPrincipalOrID
// applies the same principal-selection rule ResolveSubject uses, extended
// with a final user-id fallback for the empty-principalIds case.
func resolvedInspectUser(u *unstructured.Unstructured) ResolvedSubject {
	pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
	id := u.GetName()
	return ResolvedSubject{UserID: id, Principal: PreferredPrincipalOrID(id, pids), PrincipalIDs: pids}
}

// PreferredPrincipalOrID is the shared own-principal selection rule: the
// first local:// principal, else the first principalIds element, else the
// user id itself. Both ResolveInspectUser and the held-bindings matcher use
// this rule for a direct binding's row principal - never a second,
// independently invented one.
func PreferredPrincipalOrID(userID string, principalIDs []string) string {
	if len(principalIDs) == 0 {
		return userID
	}
	return preferredPrincipal(principalIDs)
}

// toPointerSlice adapts an UnstructuredList's Items into the []*Unstructured
// shape SubjectCandidates expects.
func toPointerSlice(items []unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}
