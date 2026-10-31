package access

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

// ParsedSubject is the --as value classified into the closed kind set
// before any server call.
type ParsedSubject struct {
	Kind SubjectKind
	Raw  string
}

// ResolvedSubject is a subject as Kubernetes sees it plus the groups a
// SubjectAccessReview about it would carry.
type ResolvedSubject struct {
	Subject       AccessSubject
	IsRancherUser bool
	PrincipalIDs  []string // a Rancher user's principalIds, for binding matches
	GroupsFrom    GroupsFrom
	Groups        []string
	SANamespace   string
}

const (
	anonymousUser        = "system:anonymous"
	serviceAccountPrefix = "system:serviceaccount:"
	authenticatedGroup   = "system:authenticated"
	unauthenticatedGroup = "system:unauthenticated"
	cattleAuthenticated  = "system:cattle:authenticated"
	typeUser             = "user"
	typeGroup            = "group"
)

// ParseSubject classifies the --as value. --type qualifies a named
// subject only; a bare name with --type group is a Kubernetes group taken
// verbatim.
func ParseSubject(as, typeFlag string) (ParsedSubject, error) {
	if typeFlag != "" && as == "" {
		return ParsedSubject{}, clierr.New(clierr.CodeValidation,
			"--type qualifies a named subject; pass --as <subject> with it")
	}
	if typeFlag != "" && typeFlag != typeUser && typeFlag != typeGroup {
		return ParsedSubject{}, clierr.New(clierr.CodeValidation, "--type must be user or group")
	}
	switch {
	case as == "":
		return ParsedSubject{}, nil
	case strings.HasPrefix(as, serviceAccountPrefix):
		rest := strings.TrimPrefix(as, serviceAccountPrefix)
		ns, name, ok := strings.Cut(rest, ":")
		if !ok || ns == "" || name == "" || strings.Contains(name, ":") {
			return ParsedSubject{}, clierr.New(clierr.CodeValidation,
				"a service account subject is system:serviceaccount:<namespace>:<name>")
		}
		if typeFlag != "" {
			return ParsedSubject{}, clierr.New(clierr.CodeValidation, "a service account subject is neither a user nor a group; drop --type")
		}
		return ParsedSubject{Kind: KindServiceAccount, Raw: as}, nil
	case strings.Contains(as, "://"):
		// A qualified principal follows grant/revoke's grammar exactly
		// (closed inference: local and *_user are users; *_group,
		// github_org, github_team are groups; any other scheme needs an
		// explicit --type; a contradicting --type is a validation error).
		sub, err := rbac.ParseSubject(as, typeFlag)
		if err != nil {
			return ParsedSubject{}, err
		}
		kind := KindUser
		if sub.Kind == rbac.SubjectGroup {
			kind = KindGroup
		}
		return ParsedSubject{Kind: kind, Raw: as}, nil
	case typeFlag == typeGroup:
		return ParsedSubject{Kind: KindGroup, Raw: as}, nil
	default:
		return ParsedSubject{Kind: KindUser, Raw: as}, nil
	}
}

// ResolveSubject turns a parsed subject into the identity Kubernetes
// sees. Only Rancher users need the management cluster.
func ResolveSubject(ctx context.Context, local engine.ClusterClient, p ParsedSubject) (ResolvedSubject, error) {
	switch p.Kind {
	case KindGroup:
		return ResolvedSubject{Subject: AccessSubject{Kind: KindGroup, Name: p.Raw}, GroupsFrom: GroupsGroupOnly, Groups: []string{p.Raw}}, nil
	case KindServiceAccount:
		ns, _, _ := strings.Cut(strings.TrimPrefix(p.Raw, serviceAccountPrefix), ":")
		return ResolvedSubject{
			Subject: AccessSubject{Kind: KindServiceAccount, Name: p.Raw}, SANamespace: ns,
			GroupsFrom: GroupsServiceAccountIdentity,
			Groups:     []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, authenticatedGroup},
		}, nil
	}
	if p.Raw == anonymousUser {
		return ResolvedSubject{Subject: AccessSubject{Kind: KindUser, Name: anonymousUser}, GroupsFrom: GroupsAnonymousIdentity, Groups: []string{unauthenticatedGroup}}, nil
	}
	if strings.HasPrefix(p.Raw, "system:") {
		return ResolvedSubject{Subject: AccessSubject{Kind: KindUser, Name: p.Raw}, GroupsFrom: GroupsAuthenticatedOnly, Groups: []string{authenticatedGroup}}, nil
	}
	user, err := findRancherUser(ctx, local, p.Raw)
	if err != nil {
		return ResolvedSubject{}, err
	}
	id := user.GetName()
	display, _, _ := unstructured.NestedString(user.Object, "displayName")
	principals, _, _ := unstructured.NestedStringSlice(user.Object, "principalIds")
	groups, err := userGroups(ctx, local, id)
	if err != nil {
		return ResolvedSubject{}, err
	}
	return ResolvedSubject{
		Subject:       AccessSubject{Kind: KindUser, Name: id, ID: id, DisplayName: display},
		IsRancherUser: true,
		PrincipalIDs:  principals,
		GroupsFrom:    GroupsRancherUserAttribute,
		Groups:        groups,
	}, nil
}

// findRancherUser resolves a user by exact id, then username, then
// principal id. It never provisions and never guesses an id.
func findRancherUser(ctx context.Context, local engine.ClusterClient, raw string) (*unstructured.Unstructured, error) {
	userKind, _ := kinds.Lookup("user")
	if !strings.Contains(raw, "://") {
		if u, err := local.Get(ctx, userKind.GVR, "", raw); err == nil {
			return u, nil
		} else if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeNotFound {
			return nil, err
		}
	}
	list, err := local.List(ctx, userKind.GVR, "", "")
	if err != nil {
		return nil, err
	}
	var matches []*unstructured.Unstructured
	for i := range list.Items {
		u := &list.Items[i]
		username, _, _ := unstructured.NestedString(u.Object, "username")
		principals, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
		if username == raw || slices.Contains(principals, raw) {
			matches = append(matches, u)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		e := clierr.New(clierr.CodeNotFound, fmt.Sprintf("user %s not found", raw)).WithHint(clierr.Rancher + " get users")
		// A principal or a system: value has no name to match near.
		if !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "system:") {
			var nonSystem []*unstructured.Unstructured
			for i := range list.Items {
				if !rbac.IsSystemUser(&list.Items[i]) {
					nonSystem = append(nonSystem, &list.Items[i])
				}
			}
			e = e.WithCandidatesCut(rbac.SubjectCandidates(nonSystem, raw), rbac.CandidateLimit)
		}
		return nil, e
	default:
		cands := make([]clierr.Candidate, 0, len(matches))
		for _, u := range matches {
			cands = append(cands, rbac.UserCandidate(u))
		}
		return nil, clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d users match %s; pass the user id", len(matches), raw)).
			WithHint(clierr.Rancher + " get users").
			WithCandidates(cands)
	}
}

// userGroups reads the user's cached group principals. A missing
// UserAttribute means the user has none (a user who never logged in).
func userGroups(ctx context.Context, local engine.ClusterClient, userID string) ([]string, error) {
	groups := []string{}
	attr, err := local.Get(ctx, rbac.UserAttrGVR, "", userID)
	if err != nil {
		if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeNotFound {
			return nil, err
		}
	} else {
		for _, g := range rbac.GroupPrincipalEntries(attr) {
			groups = append(groups, g.Principal)
		}
	}
	groups = append(groups, authenticatedGroup, cattleAuthenticated)
	slices.Sort(groups)
	return slices.Compact(groups), nil
}
