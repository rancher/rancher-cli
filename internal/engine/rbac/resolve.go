package rbac

import (
	"context"
	"errors"
	"fmt"
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
	// groups and provisioning) — grant's duplicate detection uses them to
	// match identity aliases of the same user.
	PrincipalIDs []string
	Provision    bool
}

const candidateCap = 10

// isNotFound reports whether err is the structured NOT_FOUND — the ONLY
// error that advances a lookup chain (spec: fallback discipline).
func isNotFound(err error) bool {
	var ce *clierr.Error
	return errors.As(err, &ce) && ce.Code == clierr.CodeNotFound
}

// isSystemUser mirrors rancher's User.IsSystem: any principalIds entry
// with the literal prefix "system:".
func isSystemUser(u *unstructured.Unstructured) bool {
	pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
	for _, p := range pids {
		if strings.HasPrefix(p, "system:") {
			return true
		}
	}
	return false
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

// ResolveSubject resolves a parsed Subject to binding fields per the spec:
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
		if err != nil && !isNotFound(err) {
			return ResolvedSubject{}, err
		}
		if err == nil {
			// A successful Get hit on a system user is TERMINAL for both
			// local and external paths: never fall through to a scan or
			// provisioning (the caller addressed a system account).
			if isSystemUser(obj) {
				return ResolvedSubject{}, systemUserError(sub.Principal)
			}
			if isLocal {
				// Local: id IS the user id (TrimPrefix, not a hash). The
				// Get is direct ownership proof — no scan needed.
				pids, _, _ := unstructured.NestedStringSlice(obj.Object, "principalIds")
				return ResolvedSubject{UserID: obj.GetName(), Principal: sub.Principal, PrincipalIDs: pids}, nil
			}
		}

		if isLocal {
			// Local principal, hash-id was NOT_FOUND: the user doesn't exist.
			return ResolvedSubject{}, clierr.New(clierr.CodeNotFound,
				fmt.Sprintf("no user found for principal %q", sub.Principal)).
				WithHint("run: rancher get users")
		}

		// External principal: always run the authoritative ownership scan.
		// A verified hash-id hit is not sufficient — a second user may also
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
			if isSystemUser(u) {
				continue
			}
			pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
			for _, p := range pids {
				if p == sub.Principal {
					owners = append(owners, u)
					break
				}
			}
		}
		switch len(owners) {
		case 0:
			if hashIDOccupied != "" {
				// The deterministic id is occupied by a user that no
				// longer owns this principal. EnsureUser would call
				// Create and receive AlreadyExists — provisioning cannot
				// succeed. Return a structured conflict.
				return ResolvedSubject{}, clierr.New(clierr.CodeConflict,
					fmt.Sprintf("principal %q cannot be provisioned: predicted user id %q is already occupied by a different identity",
						sub.Principal, hashIDOccupied)).
					WithHint("run: rancher get users, or pass the user id directly")
			}
			return ResolvedSubject{Principal: sub.Principal, Provision: true}, nil
		case 1:
			pids, _, _ := unstructured.NestedStringSlice(owners[0].Object, "principalIds")
			return ResolvedSubject{UserID: owners[0].GetName(), Principal: sub.Principal, PrincipalIDs: pids}, nil
		default:
			cands := make([]clierr.Candidate, 0, len(owners))
			for _, u := range owners {
				cands = append(cands, candidate(u))
			}
			return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
				fmt.Sprintf("%d users share principal %q", len(owners), sub.Principal)).
				WithCandidates(cands).
				WithHint("pass the user id")
		}
	}

	// Bare name/id: exact-id Get wins outright (deliberate precedence
	// change from MatchOneOrError's ambiguity detection — spec'd).
	obj, err := client.Get(ctx, userKind.GVR, "", sub.Bare)
	if err == nil {
		if isSystemUser(obj) {
			return ResolvedSubject{}, systemUserError(sub.Bare)
		}
		return resolvedFromUser(obj)
	}
	if !isNotFound(err) {
		return ResolvedSubject{}, err
	}

	list, err := client.List(ctx, userKind.GVR, "", "")
	if err != nil {
		return ResolvedSubject{}, err
	}
	var displayMatches []*unstructured.Unstructured
	var all []*unstructured.Unstructured
	for i := range list.Items {
		u := &list.Items[i]
		if isSystemUser(u) {
			continue
		}
		all = append(all, u)
		display, _, _ := unstructured.NestedString(u.Object, "displayName")
		if display == sub.Bare {
			displayMatches = append(displayMatches, u)
		}
	}
	switch len(displayMatches) {
	case 1:
		return resolvedFromUser(displayMatches[0])
	case 0:
		return ResolvedSubject{}, clierr.New(clierr.CodeNotFound, "user not found: "+sub.Bare).
			WithCandidates(nearCandidates(sub.Bare, all)).
			WithHint("run: rancher get users")
	default:
		cands := make([]clierr.Candidate, 0, len(displayMatches))
		for _, u := range displayMatches {
			cands = append(cands, candidate(u))
		}
		return ResolvedSubject{}, clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d users have display name %q", len(displayMatches), sub.Bare)).
			WithCandidates(cands).
			WithHint("pass the user id")
	}
}

// systemUserError is the terminal result for a lookup that lands on a
// system account: never a fallback, never provisioning.
func systemUserError(subject string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%q resolves to a system user; system users cannot be granted or revoked roles", subject)).
		WithHint("pass a regular user or group")
}

func resolvedFromUser(u *unstructured.Unstructured) (ResolvedSubject, error) {
	pids, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
	if len(pids) == 0 {
		return ResolvedSubject{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("user %q has no principal ids", u.GetName())).
			WithHint("fix the user object, or pass a fully qualified principal")
	}
	return ResolvedSubject{UserID: u.GetName(), Principal: preferredPrincipal(pids), PrincipalIDs: pids}, nil
}

func candidate(u *unstructured.Unstructured) clierr.Candidate {
	display, _, _ := unstructured.NestedString(u.Object, "displayName")
	if display == "" {
		display, _, _ = unstructured.NestedString(u.Object, "username")
	}
	return clierr.Candidate{ID: u.GetName(), Name: display}
}

// principalOwned reports whether principal appears in pids.
func principalOwned(pids []string, principal string) bool {
	for _, p := range pids {
		if p == principal {
			return true
		}
	}
	return false
}

// nearCandidates filters users to case-insensitive substring matches of
// input against id, displayName, and username, capped at candidateCap.
func nearCandidates(input string, users []*unstructured.Unstructured) []clierr.Candidate {
	needle := strings.ToLower(input)
	var out []clierr.Candidate
	for _, u := range users {
		if len(out) == candidateCap {
			break
		}
		display, _, _ := unstructured.NestedString(u.Object, "displayName")
		username, _, _ := unstructured.NestedString(u.Object, "username")
		hay := strings.ToLower(u.GetName() + " " + display + " " + username)
		if strings.Contains(hay, needle) {
			out = append(out, candidate(u))
		}
	}
	return out
}
