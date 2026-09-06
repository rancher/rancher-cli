package access

import (
	"context"
	"errors"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

// candidateIdentity is what a SubjectAccessReview about a candidate
// carries, resolved once per distinct candidate.
type candidateIdentity struct {
	Subject    AccessSubject
	GroupsFrom GroupsFrom
	Groups     []string
}

// groupsCacheWarning states the limit of every group row's knownMembers.
const groupsCacheWarning = "group rows list only the members Rancher has cached (users who have logged in); groups are not expanded beyond that"

// resolveCandidate resolves a binding subject into the identity a
// SubjectAccessReview about it carries. Groups and service accounts use
// the fixed identity tables; system:anonymous and other system: usernames
// need no server. Any other username is looked up with ONE exact GET
// users/<name> on the management cluster: a hit attaches the display name
// and the same-named UserAttribute's groups; NOT_FOUND keeps the name
// under the arbitrary-user rule (system:authenticated only, so a denied
// answer is unknown); any other error is returned for the caller to turn
// into the candidate's request issue. The --as resolver is never used: a
// binding name is never matched against usernames or principal ids.
func resolveCandidate(ctx context.Context, local engine.ClusterClient, c candidate) (candidateIdentity, error) {
	switch c.Kind {
	case KindGroup:
		return candidateIdentity{Subject: AccessSubject{Kind: KindGroup, Name: c.Name}, GroupsFrom: GroupsGroupOnly, Groups: []string{c.Name}}, nil
	case KindServiceAccount:
		ns, _, _ := strings.Cut(strings.TrimPrefix(c.Name, serviceAccountPrefix), ":")
		return candidateIdentity{
			Subject:    AccessSubject{Kind: KindServiceAccount, Name: c.Name},
			GroupsFrom: GroupsServiceAccountIdentity,
			Groups:     []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, authenticatedGroup},
		}, nil
	}
	if c.Name == anonymousUser {
		return candidateIdentity{Subject: AccessSubject{Kind: KindUser, Name: anonymousUser}, GroupsFrom: GroupsAnonymousIdentity, Groups: []string{unauthenticatedGroup}}, nil
	}
	arbitrary := candidateIdentity{Subject: AccessSubject{Kind: KindUser, Name: c.Name}, GroupsFrom: GroupsAuthenticatedOnly, Groups: []string{authenticatedGroup}}
	if strings.HasPrefix(c.Name, "system:") {
		return arbitrary, nil
	}
	userKind, _ := kinds.Lookup("user")
	u, err := local.Get(ctx, userKind.GVR, "", c.Name)
	if err != nil {
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeNotFound {
			return arbitrary, nil
		}
		return candidateIdentity{}, err
	}
	groups, err := userGroups(ctx, local, u.GetName())
	if err != nil {
		return candidateIdentity{}, err
	}
	display, _, _ := unstructured.NestedString(u.Object, "displayName")
	return candidateIdentity{
		Subject:    AccessSubject{Kind: KindUser, Name: c.Name, ID: u.GetName(), DisplayName: display},
		GroupsFrom: GroupsRancherUserAttribute,
		Groups:     groups,
	}, nil
}

// groupMembers reads every UserAttribute on the management cluster once
// and indexes the user ids by group principal: Rancher's cache of the
// groups each user held when they last logged in. The index is enrichment
// only (a group row's knownMembers, a user row's "group <g>" marker), so a
// failed LIST is returned as a warning and never fails the command.
func groupMembers(ctx context.Context, local engine.ClusterClient) (map[string][]string, string) {
	list, err := local.List(ctx, rbac.UserAttrGVR, "", "")
	if err != nil {
		return nil, "group members not expanded: " + errorMessage(err)
	}
	members := map[string][]string{}
	for i := range list.Items {
		u := &list.Items[i]
		for _, g := range rbac.GroupPrincipalEntries(u) {
			members[g.Principal] = append(members[g.Principal], u.GetName())
		}
	}
	for g, ids := range members {
		slices.Sort(ids)
		members[g] = slices.Compact(ids)
	}
	return members, ""
}

// errorMessage is the message of a mapped error, or the plain error text.
func errorMessage(err error) string {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return ce.Message
	}
	return err.Error()
}
