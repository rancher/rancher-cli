package rbac

import (
	"context"
	"sort"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ScopeBindings is one scope instance's held rows: a cluster or a project
// that has at least one matching binding. ID is the stable scope id (a
// cluster id, or a project's `<cluster>:<id>` form) - never a display name.
type ScopeBindings struct {
	ID   string
	Rows []MemberRow
}

// OwnIdentitySet is the user id plus every principalIds element (the
// local:// principal, when the user has one, is already among them). A
// binding field equal to any member of this set is the user's OWN, never a
// group's - used both to classify a binding as direct here and, by the
// composite's rendering, to tell a held row's principal apart from a
// group's. Reuse this set; do not test principalIds alone, which would
// mislabel the empty-principalIds fallback row as group-held.
func OwnIdentitySet(userID string, principalIDs []string) map[string]bool {
	set := make(map[string]bool, len(principalIDs)+1)
	set[userID] = true
	for _, p := range principalIDs {
		set[p] = true
	}
	return set
}

// HeldGlobalBindings enumerates the user's held GlobalRoleBindings
// (cluster-scoped: one management-wide list, no grouping).
func HeldGlobalBindings(ctx context.Context, mgmt engine.ClusterClient, userID string, principalIDs, groupPrincipals []string, displayNames map[string]string) ([]MemberRow, error) {
	grbKind, _ := kinds.Lookup("globalrolebinding")
	list, err := mgmt.List(ctx, grbKind.GVR, "", "")
	if err != nil {
		return nil, err
	}
	ownIdentity := OwnIdentitySet(userID, principalIDs)
	groupSet := toSet(groupPrincipals)
	rows := make([]MemberRow, 0, len(list.Items))
	for i := range list.Items {
		if row, ok := heldRow(&list.Items[i], ownIdentity, userID, principalIDs, groupSet, displayNames, "globalRoleName"); ok {
			rows = append(rows, row)
		}
	}
	sortMemberRows(rows)
	return rows, nil
}

// HeldClusterBindings enumerates the user's held ClusterRoleTemplateBindings
// across every management namespace, grouped by cluster id (the crtb's own
// namespace). Only clusters with at least one matching row are returned.
func HeldClusterBindings(ctx context.Context, mgmt engine.ClusterClient, userID string, principalIDs, groupPrincipals []string, displayNames map[string]string) ([]ScopeBindings, []string, error) {
	crtbKind, _ := kinds.Lookup("clusterroletemplatebinding")
	list, err := mgmt.List(ctx, crtbKind.GVR, "", "")
	if err != nil {
		return nil, nil, err
	}
	groups, dropped := groupHeldRows(list.Items, userID, principalIDs, groupPrincipals, displayNames, "roleTemplateName",
		func(b *unstructured.Unstructured) string { return b.GetNamespace() })
	return groups, dropped, nil
}

// HeldProjectBindings enumerates the user's held ProjectRoleTemplateBindings
// across every management namespace, grouped by the stable `<cluster>:<id>`
// project id carried in the prtb's own projectName field - never the prtb's
// namespace, which is the project's backing namespace, a different value.
// Only projects with at least one matching row are returned.
func HeldProjectBindings(ctx context.Context, mgmt engine.ClusterClient, userID string, principalIDs, groupPrincipals []string, displayNames map[string]string) ([]ScopeBindings, []string, error) {
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	list, err := mgmt.List(ctx, prtbKind.GVR, "", "")
	if err != nil {
		return nil, nil, err
	}
	groups, dropped := groupHeldRows(list.Items, userID, principalIDs, groupPrincipals, displayNames, "roleTemplateName",
		func(b *unstructured.Unstructured) string {
			projectName, _, _ := unstructured.NestedString(b.Object, "projectName")
			return projectName
		})
	return groups, dropped, nil
}

// groupHeldRows drops a matched binding whose scopeID is EMPTY (a malformed
// prtb missing projectName, or a crtb with no namespace): merging such rows
// under one blank scope heading would report roles from unrelated broken
// objects as a single anonymous scope, and downstream enrichment would issue
// nonsense empty-segment GETs. The dropped binding names are returned so the
// caller can surface them in a warning rather than lose them silently.
func dropEmptyScopes(byScope map[string][]MemberRow) (dropped []string) {
	for _, row := range byScope[""] {
		dropped = append(dropped, row.Binding)
	}
	delete(byScope, "")
	sort.Strings(dropped)
	return dropped
}

// groupHeldRows matches every binding against the user, groups the rows it
// produces by scopeID, and drops scopes with zero matching rows - a
// binding belonging to some other subject must never manufacture an empty
// scope entry. Scopes sort by id; rows within a scope sort by (principal,
// role, binding).
func groupHeldRows(items []unstructured.Unstructured, userID string, principalIDs, groupPrincipals []string, displayNames map[string]string, roleField string, scopeID func(*unstructured.Unstructured) string) ([]ScopeBindings, []string) {
	ownIdentity := OwnIdentitySet(userID, principalIDs)
	groupSet := toSet(groupPrincipals)
	byScope := map[string][]MemberRow{}
	for i := range items {
		b := &items[i]
		row, ok := heldRow(b, ownIdentity, userID, principalIDs, groupSet, displayNames, roleField)
		if !ok {
			continue
		}
		id := scopeID(b)
		byScope[id] = append(byScope[id], row)
	}
	dropped := dropEmptyScopes(byScope)
	ids := make([]string, 0, len(byScope))
	for id := range byScope {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ScopeBindings, 0, len(ids))
	for _, id := range ids {
		rows := byScope[id]
		sortMemberRows(rows)
		out = append(out, ScopeBindings{ID: id, Rows: rows})
	}
	return out, dropped
}

// heldRow classifies one binding against the user, reporting the memberRow
// it produces and false when the binding belongs to neither the user nor
// one of their groups. Classification is separated from row provenance:
// DIRECT iff userName equals the user id or userPrincipalName is in the
// user's own-identity set; a direct match wins even when an unrelated
// groupPrincipalName is also populated (a malformed multi-subject binding
// never attributes to a group). GROUP-HELD only when no direct field
// matches and groupPrincipalName is one of the user's UserAttributes group
// principals.
//
// Row principal for a direct row is the populated, OWNED userPrincipalName -
// never an unrelated value a malformed binding happens to carry - else
// the fallback (local:// principal, then first principalIds element,
// then the user id); never the bare user id when a principal exists. Row
// principal for a group row is the matched groupPrincipalName as-is.
// DisplayName is enrichment from the caller's displayNames map (the
// inspected user's own name for own-identity principals, cached group
// names for group principals; blank on absence or disagreement) - the
// same blank-when-unresolvable contract MemberRow rules everywhere.
func heldRow(b *unstructured.Unstructured, ownIdentity map[string]bool, userID string, principalIDs []string, groupPrincipals map[string]bool, displayNames map[string]string, roleField string) (MemberRow, bool) {
	userName, _, _ := unstructured.NestedString(b.Object, "userName")
	userPrincipal, _, _ := unstructured.NestedString(b.Object, "userPrincipalName")
	groupPrincipal, _, _ := unstructured.NestedString(b.Object, "groupPrincipalName")
	role, _, _ := unstructured.NestedString(b.Object, roleField)

	ownedPrincipal := userPrincipal != "" && ownIdentity[userPrincipal]
	direct := (userName != "" && userName == userID) || ownedPrincipal
	switch {
	case direct:
		principal := PreferredPrincipalOrID(userID, principalIDs)
		if ownedPrincipal {
			principal = userPrincipal
		}
		return MemberRow{
			Principal:   principal,
			DisplayName: displayNames[principal],
			Role:        role,
			Binding:     b.GetName(),
			System:      engine.IsSystemPrincipal(principal),
		}, true
	case groupPrincipal != "" && groupPrincipals[groupPrincipal]:
		return MemberRow{
			Principal:   groupPrincipal,
			DisplayName: displayNames[groupPrincipal],
			Role:        role,
			Binding:     b.GetName(),
			System:      engine.IsSystemPrincipal(groupPrincipal),
		}, true
	default:
		return MemberRow{}, false
	}
}

// sortMemberRows applies the (principal, role, binding) order -
// binding distinguishes rows sharing a principal and role, keeping
// duplicate out-of-band bindings deterministic. A local copy of the
// comparator ResolveMembers applies to its own rows.
func sortMemberRows(rows []MemberRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Principal != rows[j].Principal {
			return rows[i].Principal < rows[j].Principal
		}
		if rows[i].Role != rows[j].Role {
			return rows[i].Role < rows[j].Role
		}
		return rows[i].Binding < rows[j].Binding
	})
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, s := range items {
		set[s] = true
	}
	return set
}
