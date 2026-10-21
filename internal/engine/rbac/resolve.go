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
)

// ResolvedSubject is the binding-ready form of a Subject.
type ResolvedSubject struct {
	UserID    string
	Principal string
	// PrincipalIDs holds ALL of the resolved user's principalIds (nil for
	// groups and provisioning) - grant's duplicate detection uses them to
	// match identity aliases of the same user.
	PrincipalIDs []string
	Provision    bool
}

const candidateCap = 10

// IsSystemUser reports whether u is a system user. It mirrors rancher's
// User.IsSystem: any principalIds entry matching the shared system-principal
// predicate (engine.IsSystemPrincipal).
func IsSystemUser(u *unstructured.Unstructured) bool {
	pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
	return slices.ContainsFunc(pids, engine.IsSystemPrincipal)
}

// preferredPrincipal picks the principal userPrincipalName is set from
// for bare-name resolution: the first "local://" entry if present (the
// canonical provider-independent principal), else the first entry.
func preferredPrincipal(principalIDs []string) string {
	for _, id := range principalIDs {
		if strings.HasPrefix(id, "local://") {
			return id
		}
	}
	return principalIDs[0]
}

// ResolveSubject resolves a parsed Subject to binding fields:
// groups pass through; bare user names resolve against existing users
// only; fully qualified user principals resolve via deterministic-id and
// principalIds lookups and fall through to the provisioning path when no
// user exists. Only structured NOT_FOUND advances a lookup step.
func ResolveSubject(ctx context.Context, clients engine.ClusterClientFactory, sub Subject) (ResolvedSubject, error) {
	if sub.Kind == SubjectGroup {
		return ResolvedSubject{Principal: sub.Principal}, nil
	}
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return ResolvedSubject{}, err
	}
	userKind, _ := kinds.Lookup("user")

	if sub.Principal != "" {
		isLocal := strings.HasPrefix(sub.Principal, "local://")
		id := strings.TrimPrefix(sub.Principal, "local://")
		if !isLocal {
			id = ProvisionedUserID(sub.Principal)
		}
		obj, err := client.Get(ctx, userKind.GVR, "", id)
		if err != nil && !clierr.IsNotFound(err) {
			return ResolvedSubject{}, err
		}
		if err == nil {
			// A successful Get hit on a system user is TERMINAL for both
			// local and external paths: never fall through to a scan or
			// provisioning (the caller addressed a system account).
			if IsSystemUser(obj) {
				return ResolvedSubject{}, systemUserError(sub.Principal)
			}
			if isLocal {
				// Local: id IS the user id (TrimPrefix, not a hash). The
				// Get is direct ownership proof - no scan needed.
				pids, _, _ := unstructured.NestedStringSlice(obj.Object, "principalIds")
				return ResolvedSubject{UserID: obj.GetName(), Principal: sub.Principal, PrincipalIDs: pids}, nil
			}
		}

		if isLocal {
			// Local principal, hash-id was NOT_FOUND: the user doesn't exist.
			return ResolvedSubject{}, clierr.New(clierr.CodeNotFound,
				fmt.Sprintf("no user found for principal %q", sub.Principal)).
				WithHint("rancher get users")
		}

		// External principal: always run the authoritative ownership scan.
		// A verified hash-id hit is not sufficient - a second user may also
		// carry the same principal (GetUserByPrincipalID rejects duplicates
		// regardless of names). Record whether the hash id was occupied by a
		// non-owning user; provisioning cannot succeed in that case.
		var hashIDOccupied string // non-empty: the id that is occupied
		if err == nil {
			pids, _, _ := unstructured.NestedStringSlice(obj.Object, "principalIds")
			if !principalOwned(pids, sub.Principal) {
				// Hash id found but principal not in its principalIds.
				hashIDOccupied = obj.GetName()
			}
		}

		list, lerr := client.List(ctx, userKind.GVR, "", "")
		if lerr != nil {
			return ResolvedSubject{}, lerr
		}
		var owners []*unstructured.Unstructured
		for i := range list.Items {
			u := &list.Items[i]
			if IsSystemUser(u) {
				continue
			}
			pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
			if slices.Contains(pids, sub.Principal) {
				owners = append(owners, u)
			}
		}
		switch len(owners) {
		case 0:
			if hashIDOccupied != "" {
				// The deterministic id is occupied by a user that no
				// longer owns this principal. EnsureUser would call
				// Create and receive AlreadyExists - provisioning cannot
				// succeed. Return a structured conflict.
				return ResolvedSubject{}, clierr.New(clierr.CodeConflict,
					fmt.Sprintf("principal %q cannot be provisioned: the user id Rancher would give the new user, %q, already belongs to a user without this principal; find the right user, or pass its id directly",
						sub.Principal, hashIDOccupied)).
					WithHint("rancher get users")
			}
			return ResolvedSubject{Principal: sub.Principal, Provision: true}, nil
		case 1:
			pids, _, _ := unstructured.NestedStringSlice(owners[0].Object, "principalIds")
			return ResolvedSubject{UserID: owners[0].GetName(), Principal: sub.Principal, PrincipalIDs: pids}, nil
		default:
			cands := make([]clierr.Candidate, 0, len(owners))
			for _, u := range owners {
				cands = append(cands, UserCandidate(u))
			}
			return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
				fmt.Sprintf("%d users share principal %q", len(owners), sub.Principal)).
				WithCandidates(cands)
		}
	}

	// Bare name/id: exact-id Get wins outright, with no warning naming a
	// user whose display name or username is the same value.
	// On Get miss, list scan matches exact equality on displayName OR
	// username; a single user matching both fields counts once;
	// >1 match → AMBIGUOUS_SELECTION; 0 → NOT_FOUND.
	obj, err := client.Get(ctx, userKind.GVR, "", sub.Bare)
	if err == nil {
		if IsSystemUser(obj) {
			return ResolvedSubject{}, systemUserError(sub.Bare)
		}
		return resolvedFromUser(obj)
	}
	if !clierr.IsNotFound(err) {
		return ResolvedSubject{}, err
	}

	list, err := client.List(ctx, userKind.GVR, "", "")
	if err != nil {
		return ResolvedSubject{}, err
	}
	// nameMatches collects users where displayName == input OR username == input.
	// Each list item is evaluated once, so a user whose both fields equal the
	// input appends exactly once.
	var nameMatches []*unstructured.Unstructured
	var all []*unstructured.Unstructured
	for i := range list.Items {
		u := &list.Items[i]
		if IsSystemUser(u) {
			continue
		}
		all = append(all, u)
		display, _, _ := unstructured.NestedString(u.Object, "displayName")
		username, _, _ := unstructured.NestedString(u.Object, "username")
		if display == sub.Bare || username == sub.Bare {
			nameMatches = append(nameMatches, u)
		}
	}
	switch len(nameMatches) {
	case 1:
		return resolvedFromUser(nameMatches[0])
	case 0:
		return ResolvedSubject{}, clierr.New(clierr.CodeNotFound, "user not found: "+sub.Bare).
			WithCandidates(SubjectCandidates(all, sub.Bare)).
			WithHint("rancher get users")
	default:
		cands := make([]clierr.Candidate, 0, len(nameMatches))
		for _, u := range nameMatches {
			cands = append(cands, UserCandidate(u))
		}
		return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d users match %q", len(nameMatches), sub.Bare)).
			WithCandidates(cands)
	}
}

// systemUserError is the terminal result for a lookup that lands on a
// system account: never a fallback, never provisioning.
func systemUserError(subject string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%q resolves to a system user; system users cannot be granted or revoked roles; pass a regular user or group", subject))
}

func resolvedFromUser(u *unstructured.Unstructured) (ResolvedSubject, error) {
	pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
	if len(pids) == 0 {
		return ResolvedSubject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("user %q has no principalIds; fix the user in Rancher, or pass a fully qualified principal", u.GetName()))
	}
	return ResolvedSubject{UserID: u.GetName(), Principal: preferredPrincipal(pids), PrincipalIDs: pids}, nil
}

// UserCandidate returns u as a selectable candidate: its id (the object name)
// and its display name, else its username.
func UserCandidate(u *unstructured.Unstructured) clierr.Candidate {
	display, _, _ := unstructured.NestedString(u.Object, "displayName")
	if display == "" {
		display, _, _ = unstructured.NestedString(u.Object, "username")
	}
	return clierr.Candidate{ID: u.GetName(), Name: display}
}

// principalOwned reports whether principal appears in pids.
func principalOwned(pids []string, principal string) bool {
	return slices.Contains(pids, principal)
}

// SubjectCandidates builds the candidate slice for a user not-found error:
// the users whose id, display name or username near-matches needle, else
// the first few users. Each user's fields are extracted once: username for
// the haystack and UserCandidate() for the display-name-fallback Candidate.
// The haystack is id + candidate.Name + username; the display-name-fallback
// rule is owned by UserCandidate(), so it is not restated here.
func SubjectCandidates(users []*unstructured.Unstructured, needle string) []clierr.Candidate {
	type row struct {
		haystack string
		cand     clierr.Candidate
	}
	rows := make([]row, len(users))
	for i, u := range users {
		un, _, _ := unstructured.NestedString(u.Object, "username")
		cand := UserCandidate(u)
		rows[i] = row{
			haystack: cand.ID + " " + cand.Name + " " + un,
			cand:     cand,
		}
	}
	return candidatesWithFallback(len(rows),
		func(i int) string { return rows[i].haystack },
		func(i int) clierr.Candidate { return rows[i].cand },
		needle)
}
