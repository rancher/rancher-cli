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

	rows := make([]memberRow, 0, len(bindings))
	// Only principals the input bindings actually reference are indexed:
	// directory corruption on unrelated principals must never attach a
	// warning to (or influence) this card.
	userRefs := map[string]bool{}  // referenced user subjects: principal ids AND bare userNames
	groupRefs := map[string]bool{} // referenced group principal ids
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

	var warnings []string
	display := map[string]string{} // referenced id → display name
	collided := map[string]bool{}  // referenced id → ambiguous, stays unresolved

	if len(userRefs) > 0 {
		// Pre-flight, not just post-error: a prior call may have canceled
		// the caller context without this LIST erroring — never issue a
		// request after cancellation.
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		userKind, _ := kinds.Lookup("user")
		list, err := mgmt.List(ctx, userKind.GVR, "", "")
		switch {
		case err != nil && lookupCanceled(ctx, err):
			return nil, nil, err
		case err != nil:
			warnings = append(warnings, "users unavailable: "+err.Error())
		default:
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
			// refusal). The display-disagreement rule below is for cached
			// GROUP principals only.
			for p, n := range owners {
				if n > 1 {
					collided[p] = true
					delete(display, p)
				}
			}
		}
	}

	if len(groupRefs) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		list, err := mgmt.List(ctx, userAttrGVR, "", "")
		switch {
		case err != nil && lookupCanceled(ctx, err):
			return nil, nil, err
		case err != nil:
			warnings = append(warnings, "userattributes unavailable: "+err.Error())
		default:
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
