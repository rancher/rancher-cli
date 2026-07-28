package inspect

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// userAttrGVR is the UserAttribute CRD (cluster-scoped, name = user id).
// Not a kinds-table row: it is an enrichment source, not an operator-facing
// resource.
var userAttrGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "userattributes"}

// memberRow is the resolver's internal row — each card defines its own
// serialized projection (project keeps user/roleTemplate; cluster uses
// principal/displayName/role).
type memberRow struct {
	Principal   string
	DisplayName string
	Role        string
}

// resolveMembers enriches crtb/prtb bindings with display names from two
// management-plane LISTs — Users (user display names; EnsureUser sets
// User.displayName from the provider principal) and UserAttributes (group
// display names via GroupPrincipals.<provider>.Items[]). No Norman.
//
// Lazy: empty input returns immediately; the users LIST runs only when a
// user subject needs resolution, the userattributes LIST only when a group
// subject does. LIST failures degrade to a warning (display names lost,
// rows kept) EXCEPT caller cancellation/deadline, which returns an error
// (lookupCanceled rule). Duplicate principal ownership and disagreeing
// cached group names stay unresolved with a warning — never
// last-listed-wins. Rows sort by (principal, role); collision warnings by
// principal; source-failure warnings users-before-userattributes.
func resolveMembers(ctx context.Context, mgmt engine.ClusterClient, bindings []unstructured.Unstructured) ([]memberRow, []string, error) {
	if len(bindings) == 0 {
		return []memberRow{}, nil, nil
	}

	rows, userRefs, groupRefs := collectPrincipalRefs(bindings)

	display := map[string]string{}
	collided := map[string]bool{}
	var warnings []string

	if len(userRefs) > 0 {
		// Pre-flight, not just post-error: a prior call may have canceled
		// the caller context without this LIST erroring — never issue a
		// request after cancellation.
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		d, c, w, err := resolveUserDisplayNames(ctx, mgmt, userRefs)
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
		for k, v := range d {
			display[k] = v
		}
		for k, v := range c {
			collided[k] = v
		}
	}

	if len(groupRefs) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		d, c, w, err := resolveGroupDisplayNames(ctx, mgmt, groupRefs)
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
		for k, v := range d {
			display[k] = v
		}
		for k, v := range c {
			collided[k] = v
		}
	}

	for i := range rows {
		if collided[rows[i].Principal] {
			continue
		}
		if d, ok := display[rows[i].Principal]; ok && d != "" {
			rows[i].DisplayName = d
		}
	}

	collisions := make([]string, 0, len(collided))
	for p := range collided {
		collisions = append(collisions, p)
	}
	sort.Strings(collisions)
	for _, p := range collisions {
		warnings = append(warnings, fmt.Sprintf("display name for %s is ambiguous (multiple owners); left unresolved", p))
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Principal != rows[j].Principal {
			return rows[i].Principal < rows[j].Principal
		}
		return rows[i].Role < rows[j].Role
	})
	return rows, warnings, nil
}

// collectPrincipalRefs extracts an initial row per binding and indexes the
// user and group principals it references. Only referenced principals are
// indexed — directory corruption on unrelated principals must never attach
// a warning to (or influence) this card.
//
// Priority order here (userPrincipalName → groupPrincipalName → userName)
// differs deliberately from the thin get-crtb/prtb table columns
// (userName → groupPrincipalName → userPrincipalName defined in
// internal/engine/kinds/kinds.go). The get table prefers the plain bare
// userName for operator convenience; inspect cards key on the full principal
// id because display-name resolution uses principalIds as the join key.
// See kinds.go for the reciprocal comment.
func collectPrincipalRefs(bindings []unstructured.Unstructured) (rows []memberRow, userRefs map[string]bool, groupRefs map[string]bool) {
	rows = make([]memberRow, 0, len(bindings))
	userRefs = map[string]bool{}
	groupRefs = map[string]bool{}

	for i := range bindings {
		o := bindings[i].Object
		userName, _, _ := unstructured.NestedString(o, "userName")
		userPrincipal, _, _ := unstructured.NestedString(o, "userPrincipalName")
		groupPrincipal, _, _ := unstructured.NestedString(o, "groupPrincipalName")
		role, _, _ := unstructured.NestedString(o, "roleTemplateName")

		principal := userPrincipal
		if groupPrincipal != "" {
			principal = groupPrincipal
			groupRefs[groupPrincipal] = true
		} else if userName != "" || userPrincipal != "" {
			if principal == "" {
				principal = userName
			}
			if userPrincipal != "" {
				userRefs[userPrincipal] = true
			}
			if userName != "" {
				userRefs[userName] = true
			}
		}
		rows = append(rows, memberRow{Principal: principal, Role: role})
	}
	return rows, userRefs, groupRefs
}

// resolveUserDisplayNames LISTs management-plane Users and resolves display
// names for the given user principals. Returns the display map, collision
// set, a non-empty warning string on a degraded (non-cancellation) LIST
// failure, and a non-nil error only on cancellation.
func resolveUserDisplayNames(ctx context.Context, mgmt engine.ClusterClient, userRefs map[string]bool) (display map[string]string, collided map[string]bool, warning string, err error) {
	display = map[string]string{}
	collided = map[string]bool{}

	userKind, _ := kinds.Lookup("user")
	list, listErr := mgmt.List(ctx, userKind.GVR, "", "")
	switch {
	case listErr != nil && lookupCanceled(ctx, listErr):
		return nil, nil, "", listErr
	case listErr != nil:
		return display, collided, "users unavailable: " + listErr.Error(), nil
	}

	owners := map[string]int{} // referenced principal id → count of Users advertising it
	for i := range list.Items {
		u := list.Items[i].Object
		d, _, _ := unstructured.NestedString(u, "displayName")
		// User object names are unique — no ownership ambiguity for a
		// bare userName reference.
		if name := list.Items[i].GetName(); userRefs[name] {
			display[name] = d
		}
		principals, _, _ := unstructured.NestedStringSlice(u, "principalIds")
		// Ambiguity means multiple USERS advertising the id — a
		// malformed principalIds slice repeating the id inside ONE
		// User is a single owner, so de-duplicate per User first.
		seenInUser := map[string]bool{}
		for _, p := range principals {
			if !userRefs[p] || seenInUser[p] {
				continue
			}
			seenInUser[p] = true
			owners[p]++
			if owners[p] == 1 {
				display[p] = d
			}
		}
	}
	// ANY duplicate User ownership is ambiguous — even with equal
	// display names (mirrors the RBAC resolver's duplicate-owner
	// refusal). The display-disagreement rule for cached GROUP
	// principals is handled separately in resolveGroupDisplayNames.
	for p, n := range owners {
		if n > 1 {
			collided[p] = true
			delete(display, p)
		}
	}
	return display, collided, "", nil
}

// resolveGroupDisplayNames LISTs management-plane UserAttributes and
// resolves display names for the given group principals. Returns the
// display map, collision set, a non-empty warning string on a degraded
// (non-cancellation) LIST failure, and a non-nil error only on cancellation.
func resolveGroupDisplayNames(ctx context.Context, mgmt engine.ClusterClient, groupRefs map[string]bool) (display map[string]string, collided map[string]bool, warning string, err error) {
	display = map[string]string{}
	collided = map[string]bool{}

	list, listErr := mgmt.List(ctx, userAttrGVR, "", "")
	switch {
	case listErr != nil && lookupCanceled(ctx, listErr):
		return nil, nil, "", listErr
	case listErr != nil:
		return display, collided, "userattributes unavailable: " + listErr.Error(), nil
	}

	for i := range list.Items {
		// Wire shape: UserAttribute's top-level fields carry no json
		// tags — GroupPrincipals.<provider>.Items[].metadata.name /
		// .displayName (Principal's own fields ARE tagged).
		groups, ok, _ := unstructured.NestedMap(list.Items[i].Object, "GroupPrincipals")
		if !ok {
			continue
		}
		for _, v := range groups {
			wrap, ok := v.(map[string]any)
			if !ok {
				continue
			}
			items, ok := wrap["Items"].([]any)
			if !ok {
				continue
			}
			for _, item := range items {
				p, ok := item.(map[string]any)
				if !ok {
					continue
				}
				id, _, _ := unstructured.NestedString(p, "metadata", "name")
				d, _, _ := unstructured.NestedString(p, "displayName")
				if id == "" || d == "" || !groupRefs[id] {
					continue
				}
				setGroupResolved(display, collided, id, d)
			}
		}
	}
	return display, collided, "", nil
}

// setGroupResolved records a cached GROUP principal's display name,
// detecting disagreement: a cache entry with a DIFFERENT name marks the id
// collided (unresolved). The same name cached by many users' attributes is
// the normal case, not a conflict — unlike duplicate User ownership, which
// is ambiguous even with equal names.
func setGroupResolved(display map[string]string, collided map[string]bool, id, d string) {
	if collided[id] {
		return
	}
	if prev, ok := display[id]; ok && prev != d {
		collided[id] = true
		delete(display, id)
		return
	}
	display[id] = d
}
