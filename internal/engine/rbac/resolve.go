package rbac

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
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
	// UserReadRefused is set when the server refused the read of the user:
	// UserID is the value as typed, and Principal and PrincipalIDs hold
	// nothing the server said.
	UserReadRefused bool
}

// userIDShape is the form of the ids that generateName gives to users at
// creation: u- and a suffix of characters from the set Kubernetes uses to
// generate names (bcdfghjklmnpqrstvwxz2456789, no vowels); m- and user- are
// also accepted. The id Rancher derives from an external principal (u- and ten
// lowercase base32 characters) need not match it; pass such an id as
// local://<id>.
//
// A refused read says nothing about whether a value is a user id: Rancher
// answers 403 for a user that exists, for one that does not and for a
// username alike. The shape, and the local:// form, decide only whether the
// command continues with the value as typed or fails; they classify nothing,
// and the shape cannot tell a username from an id by anything but its
// characters. A wrong guess is a binding for a user that does not exist,
// which the warning says; a refusal is recovered by passing local://<id>.
var userIDShape = regexp.MustCompile(`^(u|user|m)-[bcdfghjklmnpqrstvwxz2456789]{5,}$`)

// continuesUnread reports whether a user whose read was refused is used as
// typed. id must be a valid object name; a local:// principal always
// continues, a value without local:// only when it looks like u-lpnxw
// (userIDShape).
func continuesUnread(id string, isLocal bool) bool {
	if len(validation.IsDNS1123Subdomain(id)) > 0 {
		return false
	}
	return isLocal || userIDShape.MatchString(id)
}

// isRefusedRead reports whether err is the structured RBAC error of a
// refused read.
func isRefusedRead(err error) bool {
	ce, ok := errors.AsType[*clierr.Error](err)
	return ok && ce.Code == clierr.CodeRBAC
}

// userReadRefusedWarning is the preview warning for a subject used as typed
// because the server refused the read of the user.
func userReadRefusedWarning(rs ResolvedSubject) string {
	return fmt.Sprintf("cannot read user %[1]q (Rancher refused the read); using %[1]q as the user id, exactly as typed; "+
		"the CLI could not check that a user with this id exists, and a binding for a user that does not exist has no effect; "+
		"the CLI does not find bindings made with another principal of the same user", rs.UserID)
}

// unreadableUserError is the error for a refused read of users when the
// subject cannot be used as typed: the server's own error with one added
// sentence that says to pass the user id as local://<id>. Any other error is
// returned as it is.
func unreadableUserError(err error, subject string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeRBAC {
		return err
	}
	return ce.WithMessage(clierr.Text(
		fmt.Sprintf("%s; the CLI could not read users to find %q; pass the user id as local://<id> (the id is the name of the User object, for example u-lpnxw); an administrator can list ids", ce.Message, subject),
		clierr.Lead(" with: "), clierr.NewCommand("get users")))
}

// CandidateLimit is how many candidates a NOT_FOUND error lists; a longer
// list is cut to its first entries and the error records the full size.
const CandidateLimit = 10

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
// user exists. Only structured NOT_FOUND advances a lookup step. When the
// server refuses the read of a user (RBAC), a user id typed as local://<id>,
// or a value without local:// that looks like u-lpnxw, is returned as typed
// with UserReadRefused set; every other refused read is the RBAC error with a
// sentence that says to pass the user id.
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
			if isLocal && isRefusedRead(err) && continuesUnread(id, true) {
				return ResolvedSubject{UserID: id, Principal: sub.Principal, UserReadRefused: true}, nil
			}
			return ResolvedSubject{}, unreadableUserError(err, sub.Principal)
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
				WithHintCommand(clierr.NewCommand("get users"))
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
			return ResolvedSubject{}, unreadableUserError(lerr, sub.Principal)
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
					WithHintCommand(clierr.NewCommand("get users"))
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
		if isRefusedRead(err) && continuesUnread(sub.Bare, false) {
			return ResolvedSubject{UserID: sub.Bare, UserReadRefused: true}, nil
		}
		return ResolvedSubject{}, unreadableUserError(err, sub.Bare)
	}

	list, err := client.List(ctx, userKind.GVR, "", "")
	if err != nil {
		return ResolvedSubject{}, unreadableUserError(err, sub.Bare)
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
			WithCandidatesCut(SubjectCandidates(all, sub.Bare), CandidateLimit).
			WithHintCommand(clierr.NewCommand("get users"))
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
// every user whose id, display name or username contains needle, else all
// of the users. The list is not cut; the caller attaches it with
// WithCandidatesCut and CandidateLimit. Each user's fields are extracted
// once: username for the haystack and UserCandidate() for the
// display-name-fallback Candidate. The haystack is id + candidate.Name +
// username; the display-name-fallback rule is owned by UserCandidate(), so
// it is not restated here.
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
