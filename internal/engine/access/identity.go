package access

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// callerIdentity is what SelfSubjectReview reports for the client in use.
type callerIdentity struct {
	Username string
	Groups   []string
}

// proxyDenial records that the Rancher proxy refused the subject's access
// to the cluster before any authorizer was asked.
type proxyDenial struct {
	Note string
}

// identityPlan is the protocol's outcome: the client that answers the
// reviews and how the answer is labelled.
type identityPlan struct {
	Subject    AccessSubject
	Identity   Identity
	GroupsFrom GroupsFrom
	Groups     []string
	Client     engine.ClusterClient
	AsCaller   bool
	Denied     *proxyDenial
	Warnings   []string
}

const noClusterAccessDetail = "a cluster owner grants it with a ClusterRoleTemplateBinding or ProjectRoleTemplateBinding"

// whoami posts a SelfSubjectReview on c and returns the identity it
// reports; the review result carries the classified failure otherwise.
func whoami(ctx context.Context, c engine.ClusterClient) (callerIdentity, reviewResult) {
	r := postReview(ctx, c, engine.ReviewSelfSubjectIdentity, identityReviewObject())
	if r.kind != outcomeAnswer {
		return callerIdentity{}, r
	}
	name, _, _ := unstructured.NestedString(r.resp.Object.Object, "status", "userInfo", "username")
	groups, _, _ := unstructured.NestedStringSlice(r.resp.Object.Object, "status", "userInfo", "groups")
	slices.Sort(groups)
	return callerIdentity{Username: name, Groups: groups}, r
}

// callerPreflight is step 0: the caller's own SelfSubjectReview on the
// target cluster. Commands call it right after obtaining the cluster
// client, before discovery and scope, so a caller without cluster access
// is refused before any other request can fail for a less clear reason.
func callerPreflight(ctx context.Context, callerClient engine.ClusterClient, clusterID string) (callerIdentity, error) {
	caller, r := whoami(ctx, callerClient)
	switch r.kind {
	case outcomeAnswer:
		return caller, nil
	case outcomeNoClusterAccess:
		return callerIdentity{}, clierr.New(clierr.CodeRBAC,
			fmt.Sprintf("you have no access to cluster %s; %s", clusterID, noClusterAccessDetail))
	default:
		return callerIdentity{}, r.err
	}
}

// planIdentity runs steps 1-5 of the identity protocol given the step-0
// caller identity. local is the management cluster client (for Rancher
// user resolution); callerClient is the caller's client on the target
// cluster.
func planIdentity(ctx context.Context, clients engine.ClusterClientFactory, callerClient, local engine.ClusterClient,
	clusterID string, caller callerIdentity, as ParsedSubject, forList bool) (identityPlan, error) {
	callerSubject := enrichCaller(ctx, local, caller.Username)
	if as.Raw == "" {
		return identityPlan{Subject: callerSubject, Identity: IdentityCaller, Client: callerClient, AsCaller: true}, nil
	}
	resolved, err := ResolveSubject(ctx, local, as)
	if err != nil {
		return identityPlan{}, err
	}
	if isCallerSubject(resolved.Subject, caller) {
		return identityPlan{Subject: resolved.Subject, Identity: IdentityCaller, Client: callerClient, AsCaller: true}, nil
	}
	sarPlan := identityPlan{Subject: resolved.Subject, Identity: IdentitySAR, GroupsFrom: resolved.GroupsFrom, Groups: resolved.Groups, Client: callerClient}
	if resolved.Subject.Kind == KindGroup || resolved.GroupsFrom == GroupsAnonymousIdentity {
		// Nothing to impersonate: a group is not an identity and the
		// anonymous identity's group set is known. list rejected both at
		// validation (groupListMessage, anonymousListMessage).
		return sarPlan, nil
	}
	if clusterID == engine.LocalCluster {
		if forList {
			return identityPlan{}, clierr.New(clierr.CodeRBAC,
				"the management cluster cannot impersonate; ask a point question with access can, or list on a downstream cluster")
		}
		return sarPlan, nil
	}
	impersonated, err := clients.ImpersonatedCluster(ctx, clusterID, engine.Impersonation{User: resolved.Subject.Name})
	if err != nil {
		return identityPlan{}, err
	}
	who, r := whoami(ctx, impersonated)
	switch r.kind {
	case outcomeAnswer:
		if who.Username != resolved.Subject.Name {
			// The route dropped the header and answered for the caller.
			return sarFallback(sarPlan, forList, resolved, clusterID)
		}
		return identityPlan{Subject: resolved.Subject, Identity: IdentityImpersonated, Client: impersonated}, nil
	case outcomeImpersonationRefused:
		return sarFallback(sarPlan, forList, resolved, clusterID)
	case outcomeNoClusterAccess:
		note := fmt.Sprintf("%s has no access to cluster %s", resolved.Subject.Name, clusterID)
		if resolved.IsRancherUser && noRancherBindingsFor(ctx, local, clusterID, resolved) {
			note += " (no ClusterRoleTemplateBinding or ProjectRoleTemplateBinding grants it)"
		}
		return identityPlan{Subject: resolved.Subject, Identity: IdentityImpersonated, Client: impersonated, Denied: &proxyDenial{Note: note}}, nil
	case outcomeSubjectNotFound:
		// The proxy could not impersonate the service account because it
		// (or its namespace) does not exist on the cluster: a refusal
		// naming the subject, never a decision and never retryable.
		return identityPlan{}, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("%s not found on cluster %s (%s)", resolved.Subject.Name, clusterID, r.err.Message))
	default:
		return identityPlan{}, r.err
	}
}

// isCallerSubject reports whether the named subject IS the caller: a user
// or service account whose Kubernetes username equals the caller's. A
// group is never the caller, even when a group is named like the caller's
// username; it stays on the SubjectAccessReview path so the question asked
// is the one the operator asked.
func isCallerSubject(subject AccessSubject, caller callerIdentity) bool {
	switch subject.Kind {
	case KindUser, KindServiceAccount:
		return subject.Name == caller.Username
	default:
		return false
	}
}

// enrichCaller adds the caller's Rancher user id and display name when the
// management cluster knows the username as a User; any failure leaves the
// bare name (best-effort, never an error).
func enrichCaller(ctx context.Context, local engine.ClusterClient, username string) AccessSubject {
	subject := AccessSubject{Kind: KindUser, Name: username}
	userKind, _ := kinds.Lookup("user")
	u, err := local.Get(ctx, userKind.GVR, "", username)
	if err != nil {
		return subject
	}
	subject.ID = u.GetName()
	subject.DisplayName, _, _ = unstructured.NestedString(u.Object, "displayName")
	return subject
}

// anonymousListMessage is the one wording for the anonymous refusal in
// list: the anonymous identity cannot be impersonated.
const anonymousListMessage = "no server operation lists the anonymous identity's rules; ask a point question with access can --as system:anonymous"

// groupListMessage is the one wording for the group refusal in list.
func groupListMessage(raw string) string {
	return fmt.Sprintf("no server operation lists a group's rules; ask a point question with: rancher access can <verb> <resource> --as %s", raw)
}

// sarFallback is step 5: a SubjectAccessReview posted as the caller. list
// has no SubjectAccessReview equivalent and refuses instead.
func sarFallback(plan identityPlan, forList bool, resolved ResolvedSubject, clusterID string) (identityPlan, error) {
	if forList {
		return identityPlan{}, impersonationRefusal(resolved, clusterID)
	}
	return plan, nil
}

// impersonationRefusal names the management-cluster right the caller
// lacks for the subject's kind. The hint is a discovery command; a grant
// needs a GlobalRole this code cannot know.
func impersonationRefusal(resolved ResolvedSubject, clusterID string) *clierr.Error {
	right := "impersonate on users"
	if resolved.Subject.Kind == KindServiceAccount {
		right = "impersonate on serviceaccounts"
	}
	return clierr.New(clierr.CodeRBAC,
		fmt.Sprintf("cannot act as %s on cluster %s: your GlobalRoles lack %s on the management cluster", resolved.Subject.Name, clusterID, right)).
		WithHint("rancher get globalroles")
}

// sarRefusal is step 6: the review endpoint refused the caller's
// SubjectAccessReview.
func sarRefusal(resolved ResolvedSubject, clusterID string) *clierr.Error {
	where := "cluster " + clusterID
	right := "create subjectaccessreviews on " + where
	switch {
	case clusterID == engine.LocalCluster || resolved.Subject.Kind == KindGroup:
		// local never impersonates; a group has nothing to impersonate.
	case resolved.Subject.Kind == KindServiceAccount:
		right += ", or impersonate on serviceaccounts on the management cluster"
	default:
		right += ", or impersonate on users on the management cluster"
	}
	return clierr.New(clierr.CodeRBAC,
		fmt.Sprintf("cannot ask about %s on %s: you lack %s", resolved.Subject.Name, where, right)).
		WithHint("rancher get globalroles")
}

// noRancherBindingsFor reports whether the management plane has no
// ClusterRoleTemplateBinding in the cluster's namespace and no
// ProjectRoleTemplateBinding anywhere naming the user, its principal ids,
// or its group principals. Any read error reports false so the caller
// never states the clause without evidence.
func noRancherBindingsFor(ctx context.Context, local engine.ClusterClient, clusterID string, resolved ResolvedSubject) bool {
	crtb, _ := kinds.Lookup("clusterroletemplatebinding")
	prtb, _ := kinds.Lookup("projectroletemplatebinding")
	crtbs, err := local.List(ctx, crtb.GVR, clusterID, "")
	if err != nil {
		return false
	}
	prtbs, err := local.List(ctx, prtb.GVR, "", "")
	if err != nil {
		return false
	}
	names := func(l *unstructured.UnstructuredList) bool {
		for i := range l.Items {
			u := &l.Items[i]
			user, _, _ := unstructured.NestedString(u.Object, "userName")
			principal, _, _ := unstructured.NestedString(u.Object, "userPrincipalName")
			group, _, _ := unstructured.NestedString(u.Object, "groupPrincipalName")
			if user == resolved.Subject.ID || (principal != "" && slices.Contains(resolved.PrincipalIDs, principal)) ||
				(group != "" && slices.Contains(resolved.Groups, group)) {
				return true
			}
		}
		return false
	}
	return !names(crtbs) && !names(prtbs)
}
