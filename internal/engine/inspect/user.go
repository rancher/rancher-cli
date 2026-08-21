package inspect

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// systemPrincipalPrefix is Rancher's user-store hide-system predicate
// (user_store.go, "filter system users out of the api") — deliberately
// narrower than engine.IsSystemPrincipal ("system:"), which carries RBAC
// subject semantics for a different purpose. Mirrors kinds.go's user row
// SystemFilter.
var systemPrincipalPrefix = func() string {
	if k, ok := kinds.Lookup("user"); ok && k.SystemFilter != nil {
		return k.SystemFilter.Prefix
	}
	panic("user kind must declare SystemFilter")
}()

type userCmd struct {
	clients engine.ClusterClientFactory
}

// NewInspectUser builds `inspect user <user>`: one document answering who a
// user is and what roles they hold everywhere — never what they can
// actually do (that is effective access, out of scope here and everywhere
// else in this CLI today).
func NewInspectUser(clients engine.ClusterClientFactory) runtime.Command {
	return &userCmd{clients: clients}
}

func (c *userCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("user")
	return runtime.Descriptor{
		Name:    "inspect user",
		Aliases: k.Aliases,
		Usage:   "Inspect a user: identity, groups, and held roles across scopes",
		Desc: degradedContractDesc +
			"The user is required and hard-errors when unreadable; every other section degrades. " +
			"The positional accepts a user id, username, displayName, or a fully qualified user " +
			"principal (local://…, <provider>_user://…) — the principal form is the one way to name " +
			"a user unambiguously when ids and display names collide. " +
			"data.user carries identity (id, username, displayName, enabled, principals, created, " +
			"system) plus everything Rancher records per user in UserAttributes: groups (memberships " +
			"as last synced by Rancher, refreshed at login, a flat array of {principal, displayName, " +
			"provider}), lastLogin, lastRefresh and needsRefresh (the group-sync cache's own state), " +
			"disableAfter and deleteAfter (per-user inactivity overrides), and extra (per-provider " +
			"audit extras, [provider][key] -> values; Rancher writes principalid and username today). " +
			"All of them share one fate: when the UserAttributes read failed for any reason other " +
			"than a verified absence (a user who has never logged in has none), every one is omitted " +
			"from the wire — never an empty list — with the failure recorded in data.degraded; an " +
			"omitted optional field on a healthy read simply is not set. " +
			"data.held.{global,clusters,projects} lists role bindings that EXIST for the user or any " +
			"of their group principals — HELD roles, never a computed statement of what the user can " +
			"do. Each row is the shared member shape (principal, displayName, role, binding, system); " +
			"clusters and projects group rows by scope instance, with a best-effort displayName looked " +
			"up per entry. Because held completeness depends on the user's group principals, a failed " +
			"groups read blocks all three held sections too (their completeness would otherwise be " +
			"unverifiable) — recorded in data.degraded alongside groups.",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "user", Type: "string", Required: true, Desc: "id|username|displayName|principal"}},
		Network:     true,
		Auth:        true,
	}
}

func (c *userCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(in, "inspect user"); err != nil {
		return runtime.Result{}, err
	}
	rs, err := rbac.ResolveInspectUser(ctx, c.clients, in.Args()[0])
	if err != nil {
		return runtime.Result{}, err
	}
	mgmt, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}

	userKind, _ := kinds.Lookup("user")
	obj, err := mgmt.Get(ctx, userKind.GVR, "", rs.UserID)
	if err != nil {
		return runtime.Result{}, err
	}
	identity := userIdentityFrom(obj)

	sec := newSections(ctx, sectionGroups, sectionHeldGlobal, sectionHeldClusters, sectionHeldProjects)

	groupPrincipals, blocked, err := userAttributes(ctx, mgmt, sec, rs.UserID, &identity)
	if err != nil {
		return runtime.Result{}, err
	}

	var held heldResult
	var extraWarnings []string
	if !blocked {
		var heldWarnings []string
		held, heldWarnings, err = collectHeld(ctx, mgmt, sec, rs.UserID, rs.PrincipalIDs, groupPrincipals,
			heldDisplayNames(identity, rs.UserID, rs.PrincipalIDs, identity.Groups))
		if err != nil {
			return runtime.Result{}, err
		}
		extraWarnings = heldWarnings
	}

	degraded, warnings := sec.Finalize()
	warnings = append(warnings, extraWarnings...)
	result := userResult{User: identity, Held: held, Degraded: degraded}
	return runtime.Result{
		Kind:     "UserInspection",
		Data:     result,
		Text:     renderUserText(result, rbac.OwnIdentitySet(rs.UserID, rs.PrincipalIDs)),
		Warnings: warnings,
	}, nil
}

// userAttributes reads the UserAttribute GET-BY-ID (never a list) and
// fills identity's UserAttribute-sourced fields in place — one field
// list, no copy step to keep in sync. It returns the flat
// group-principal set the held collectors match against and whether
// the held sections are blocked. NotFound is a verified-empty user
// (never logged in): groups is a non-nil empty slice, every other
// field stays absent, held collectors run normally. Any other failure
// degrades groups (nil, omitted from the wire) AND blocks all three
// held collectors — their completeness is unknowable without the
// user's group principals, so a direct-only result would misrepresent
// a group-held-only user as verified-complete.
func userAttributes(ctx context.Context, mgmt engine.ClusterClient, sec *sections, userID string, identity *userIdentity) (groupPrincipals []string, blocked bool, err error) {
	obj, gerr := mgmt.Get(ctx, rbac.UserAttrGVR, "", userID)
	switch {
	case gerr != nil && clierr.IsNotFound(gerr):
		empty := []userGroup{}
		identity.Groups = &empty
		return nil, false, nil
	case gerr != nil:
		if herr := sec.Unavailable(sectionGroups, gerr); herr != nil {
			return nil, false, herr
		}
		if herr := sec.Blocked(sectionHeldGlobal, sectionGroups, gerr); herr != nil {
			return nil, false, herr
		}
		if herr := sec.Blocked(sectionHeldClusters, sectionGroups, gerr); herr != nil {
			return nil, false, herr
		}
		if herr := sec.Blocked(sectionHeldProjects, sectionGroups, gerr); herr != nil {
			return nil, false, herr
		}
		return nil, true, nil
	default:
		rows := parseUserAttributeGroups(obj)
		principals := make([]string, len(rows))
		for i, r := range rows {
			principals[i] = r.Principal
		}
		// lastLogin, disableAfter, and deleteAfter carry json tags on the
		// UserAttribute type; the untagged siblings (GroupPrincipals,
		// LastRefresh, NeedsRefresh, ExtraByProvider, UserName) serialize
		// under their Go field names — verified against a live server.
		identity.Groups = &rows
		identity.LastLogin, _, _ = unstructured.NestedString(obj.Object, "lastLogin")
		identity.LastRefresh, _, _ = unstructured.NestedString(obj.Object, "LastRefresh")
		if v, found, _ := unstructured.NestedBool(obj.Object, "NeedsRefresh"); found {
			identity.NeedsRefresh = &v
		}
		identity.DisableAfter, _, _ = unstructured.NestedString(obj.Object, "disableAfter")
		identity.DeleteAfter, _, _ = unstructured.NestedString(obj.Object, "deleteAfter")
		identity.Extra = parseUserAttributeExtra(obj)
		return principals, false, nil
	}
}

// parseUserAttributeExtra maps ExtraByProvider — per-provider audit
// extras, [provider → [key → values]] — dropping non-conforming leaves
// rather than failing the read. Rancher writes exactly two inner keys
// today (principalid, username); the map is passed through as-is so a
// future key surfaces without a CLI change.
func parseUserAttributeExtra(obj *unstructured.Unstructured) map[string]map[string][]string {
	raw, found, _ := unstructured.NestedMap(obj.Object, "ExtraByProvider")
	if !found || len(raw) == 0 {
		return nil
	}
	out := map[string]map[string][]string{}
	for provider, v := range raw {
		inner, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for key, lv := range inner {
			list, ok := lv.([]any)
			if !ok {
				continue
			}
			values := make([]string, 0, len(list))
			for _, item := range list {
				if s, ok := item.(string); ok {
					values = append(values, s)
				}
			}
			if len(values) == 0 {
				continue
			}
			if out[provider] == nil {
				out[provider] = map[string][]string{}
			}
			out[provider][key] = values
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseUserAttributeGroups maps the shared rbac walk of GroupPrincipals
// into the composite's wire rows — flat, totally ordered (principal,
// provider), duplicates across providers retained (distinct provider
// caches, no merge or conflict logic).
func parseUserAttributeGroups(obj *unstructured.Unstructured) []userGroup {
	entries := rbac.GroupPrincipalEntries(obj)
	rows := make([]userGroup, len(entries))
	for i, e := range entries {
		rows[i] = userGroup{Principal: e.Principal, DisplayName: e.DisplayName, Provider: e.Provider}
	}
	return rows
}

// droppedScopeWarning names role bindings omitted because their scope id
// was empty (a malformed binding with no projectName/namespace): the roles
// are real but unattributable to a scope — dropping them silently would
// misreport, merging them under a blank heading would be worse.
func droppedScopeWarning(scope string, bindings []string) []string {
	if len(bindings) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d %s role binding(s) with no scope id omitted: %s",
		len(bindings), scope, strings.Join(bindings, ", "))}
}

// heldDisplayNames builds the enrichment map heldRow consumes: the user's
// own display name for every own-identity principal (and the id itself),
// and each group principal's cached display name — blanked when two
// provider caches disagree, never chosen arbitrarily (the same
// blank-on-disagreement rule ResolveMembers applies).
func heldDisplayNames(identity userIdentity, userID string, principalIDs []string, groups *[]userGroup) map[string]string {
	names := map[string]string{}
	if identity.DisplayName != "" {
		names[userID] = identity.DisplayName
		for _, p := range principalIDs {
			names[p] = identity.DisplayName
		}
	}
	if groups != nil {
		disagreed := map[string]bool{}
		for _, g := range *groups {
			if g.DisplayName == "" {
				continue
			}
			if seen, ok := names[g.Principal]; ok && seen != g.DisplayName {
				disagreed[g.Principal] = true
				continue
			}
			names[g.Principal] = g.DisplayName
		}
		for p := range disagreed {
			delete(names, p)
		}
	}
	return names
}

// collectHeld runs the three held-binding collectors sequentially — global,
// then clusters (with per-entry displayName enrichment), then projects
// (same enrichment) — so a caller cancellation during either enrichment
// aborts before the next collector issues a single request. Each collector
// degrades independently on its own list failure.
func collectHeld(ctx context.Context, mgmt engine.ClusterClient, sec *sections, userID string, principalIDs, groupPrincipals []string, displayNames map[string]string) (heldResult, []string, error) {
	var held heldResult
	var warnings []string

	globalRows, err := rbac.HeldGlobalBindings(ctx, mgmt, userID, principalIDs, groupPrincipals, displayNames)
	switch {
	case err != nil:
		if herr := sec.Unavailable(sectionHeldGlobal, err); herr != nil {
			return heldResult{}, nil, herr
		}
	default:
		held.Global = &globalRows
	}

	clusterGroups, droppedClusters, err := rbac.HeldClusterBindings(ctx, mgmt, userID, principalIDs, groupPrincipals, displayNames)
	warnings = append(warnings, droppedScopeWarning("cluster", droppedClusters)...)
	switch {
	case err != nil:
		if herr := sec.Unavailable(sectionHeldClusters, err); herr != nil {
			return heldResult{}, nil, herr
		}
	default:
		entries := make([]heldCluster, 0, len(clusterGroups))
		for _, g := range clusterGroups {
			displayName, derr := clusterDisplayName(ctx, mgmt, g.ID)
			if derr != nil {
				return heldResult{}, nil, derr
			}
			entries = append(entries, heldCluster{Cluster: g.ID, DisplayName: displayName, Roles: g.Rows})
		}
		held.Clusters = &entries
	}

	projectGroups, droppedProjects, err := rbac.HeldProjectBindings(ctx, mgmt, userID, principalIDs, groupPrincipals, displayNames)
	warnings = append(warnings, droppedScopeWarning("project", droppedProjects)...)
	switch {
	case err != nil:
		if herr := sec.Unavailable(sectionHeldProjects, err); herr != nil {
			return heldResult{}, nil, herr
		}
	default:
		entries := make([]heldProject, 0, len(projectGroups))
		for _, g := range projectGroups {
			clusterID, projectID, _ := strings.Cut(g.ID, ":")
			displayName, derr := projectDisplayName(ctx, mgmt, clusterID, projectID)
			if derr != nil {
				return heldResult{}, nil, derr
			}
			entries = append(entries, heldProject{Project: g.ID, DisplayName: displayName, Roles: g.Rows})
		}
		held.Projects = &entries
	}

	return held, warnings, nil
}

// clusterDisplayName is a best-effort per-entry GET: an id is never
// replaced by a display name, so an unreadable cluster silently omits
// DisplayName rather than failing the section. Caller cancellation
// propagates — enrichment never swallows ctx.Err() or keeps issuing
// requests after cancellation.
func clusterDisplayName(ctx context.Context, mgmt engine.ClusterClient, clusterID string) (string, error) {
	clusterKind, _ := kinds.Lookup("cluster")
	obj, err := mgmt.Get(ctx, clusterKind.GVR, "", clusterID)
	if err != nil {
		if lookupCanceled(ctx, err) {
			return "", err
		}
		return "", nil
	}
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	return name, nil
}

// projectDisplayName is clusterDisplayName's project sibling: same
// silent-omission and cancellation-propagation contract.
func projectDisplayName(ctx context.Context, mgmt engine.ClusterClient, clusterID, projectID string) (string, error) {
	projectKind, _ := kinds.Lookup("project")
	obj, err := mgmt.Get(ctx, projectKind.GVR, clusterID, projectID)
	if err != nil {
		if lookupCanceled(ctx, err) {
			return "", err
		}
		return "", nil
	}
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	return name, nil
}

// userIdentityFrom reads exactly what the User CRD exposes at its root
// (norman-style: no spec/status nesting). Enabled preserves the raw
// presence/absence distinction as an omitempty *bool on the wire; the
// text card always states the effective value (absent means enabled);
// only explicit false renders the disabled marker. System is the
// user-store hide-system predicate (system:// principal), deliberately
// narrower than the RBAC-facing engine.IsSystemPrincipal.
func userIdentityFrom(obj *unstructured.Unstructured) userIdentity {
	displayName, _, _ := unstructured.NestedString(obj.Object, "displayName")
	username, _, _ := unstructured.NestedString(obj.Object, "username")
	principals, _, _ := unstructured.NestedStringSlice(obj.Object, "principalIds")
	if principals == nil {
		principals = []string{}
	}
	var enabled *bool
	if v, found, _ := unstructured.NestedBool(obj.Object, "enabled"); found {
		enabled = &v
	}
	return userIdentity{
		ID:          obj.GetName(),
		Username:    username,
		DisplayName: displayName,
		Enabled:     enabled,
		Principals:  principals,
		Created:     obj.GetCreationTimestamp().UTC().Format(time.RFC3339),
		System:      slices.ContainsFunc(principals, func(p string) bool { return strings.HasPrefix(p, systemPrincipalPrefix) }),
	}
}

// userGroup is one row of the groups flat array: a group principal from
// one provider's UserAttributes cache.
type userGroup struct {
	Principal   string `json:"principal"`
	DisplayName string `json:"displayName,omitempty"`
	Provider    string `json:"provider"`
}

// userIdentity is data.user. Groups and every field after it come from
// the same UserAttributes collector and share its tri-state: Groups nil
// (omitted) means degraded, in which case the rest are also omitted —
// never a fabricated absence. LastRefresh/NeedsRefresh describe the
// group-sync cache; DisableAfter/DeleteAfter are per-user inactivity
// overrides; Extra is Rancher's per-provider audit extras
// ([provider → [key → values]], principalid and username today).
type userIdentity struct {
	ID           string                         `json:"id"`
	Username     string                         `json:"username"`
	DisplayName  string                         `json:"displayName,omitempty"`
	Enabled      *bool                          `json:"enabled,omitempty"`
	Principals   []string                       `json:"principals"`
	Created      string                         `json:"created"`
	System       bool                           `json:"system,omitempty"`
	Groups       *[]userGroup                   `json:"groups,omitempty"`
	LastLogin    string                         `json:"lastLogin,omitempty"`
	LastRefresh  string                         `json:"lastRefresh,omitempty"`
	NeedsRefresh *bool                          `json:"needsRefresh,omitempty"`
	DisableAfter string                         `json:"disableAfter,omitempty"`
	DeleteAfter  string                         `json:"deleteAfter,omitempty"`
	Extra        map[string]map[string][]string `json:"extra,omitempty"`
}

// heldCluster and heldProject are one scope-instance entry: the stable
// scope id is never replaced by DisplayName, only accompanied by it.
type heldCluster struct {
	Cluster     string           `json:"cluster"`
	DisplayName string           `json:"displayName,omitempty"`
	Roles       []rbac.MemberRow `json:"roles"`
}

type heldProject struct {
	Project     string           `json:"project"`
	DisplayName string           `json:"displayName,omitempty"`
	Roles       []rbac.MemberRow `json:"roles"`
}

// heldResult is data.held. Each field is nil (omitted) exactly when its
// collector failed or was blocked by a failed groups read; a non-nil
// pointer to an empty slice is a verified-empty section.
type heldResult struct {
	Global   *[]rbac.MemberRow `json:"global,omitempty"`
	Clusters *[]heldCluster    `json:"clusters,omitempty"`
	Projects *[]heldProject    `json:"projects,omitempty"`
}

// userResult is `inspect user`'s payload; the default rendering is
// renderUserText.
type userResult struct {
	User     userIdentity     `json:"user"`
	Held     heldResult       `json:"held"`
	Degraded []sectionOutcome `json:"degraded,omitempty"`
}

// renderUserText is the default rendering: an IDENTITY block, a GROUPS
// block, then a tri-state HELD ROLES block per collector.
// renderUserText receives the RESOLVE-TIME own-identity set — the same
// snapshot heldRow classified against — never a set rebuilt from the
// identity section's own, later Get: two reads of the same user can
// disagree (a concurrent IdP login changing principalIds) and a rebuilt
// set would mislabel a direct row as group-held.
func renderUserText(r userResult, own map[string]bool) string {
	var b strings.Builder
	renderUserIdentity(&b, r.User)
	renderUserGroups(&b, r.User)
	renderHeldRoles(&b, r, own)
	return b.String()
}

func renderUserIdentity(b *strings.Builder, u userIdentity) {
	header := "USER"
	if u.System {
		header += " [system]"
	}
	if u.Enabled != nil && !*u.Enabled {
		header += " [disabled]"
	}
	fmt.Fprintf(b, "%s\n  id: %s\n  username: %s\n  displayName: %s\n",
		header, u.ID, engine.Printable(u.Username), engine.Printable(u.DisplayName))
	// Text always states the effective value: an absent wire field means
	// enabled in Rancher, and the card should say so rather than stay
	// silent. The wire keeps the raw presence/absence tri-state.
	fmt.Fprintf(b, "  enabled: %t\n", u.Enabled == nil || *u.Enabled)
	fmt.Fprintf(b, "  principals: %s\n  created: %s\n",
		engine.Printable(strings.Join(u.Principals, ", ")), u.Created)
	switch {
	case u.Groups == nil:
		// The UserAttribute read degraded: lastLogin is unknown, never
		// silently absent. The optional siblings (disableAfter,
		// deleteAfter, extra) are usually unset even on a healthy read,
		// so no unavailable marker distinguishes anything for them.
		b.WriteString("  lastLogin: unavailable\n")
	case u.LastLogin != "":
		fmt.Fprintf(b, "  lastLogin: %s\n", u.LastLogin)
	}
	if u.DisableAfter != "" {
		fmt.Fprintf(b, "  disableAfter: %s\n", engine.Printable(u.DisableAfter))
	}
	if u.DeleteAfter != "" {
		fmt.Fprintf(b, "  deleteAfter: %s\n", engine.Printable(u.DeleteAfter))
	}
	for _, provider := range slices.Sorted(maps.Keys(u.Extra)) {
		inner := u.Extra[provider]
		pairs := make([]string, 0, len(inner))
		for _, key := range slices.Sorted(maps.Keys(inner)) {
			pairs = append(pairs, fmt.Sprintf("%s=%s", key, strings.Join(inner[key], ",")))
		}
		fmt.Fprintf(b, "  extra[%s]: %s\n", engine.Printable(provider), engine.Printable(strings.Join(pairs, "  ")))
	}
}

func renderUserGroups(b *strings.Builder, u userIdentity) {
	b.WriteString("\nGROUPS\n")
	switch {
	case u.Groups == nil:
		b.WriteString("  groups: unavailable\n")
		return
	case len(*u.Groups) == 0:
		b.WriteString("  none\n")
	default:
		for _, g := range *u.Groups {
			if g.DisplayName != "" {
				fmt.Fprintf(b, "  %s (%s)  [%s]\n", engine.Printable(g.DisplayName), engine.Printable(g.Principal), engine.Printable(g.Provider))
			} else {
				fmt.Fprintf(b, "  %s  [%s]\n", engine.Printable(g.Principal), engine.Printable(g.Provider))
			}
		}
	}
	if u.LastRefresh != "" {
		fmt.Fprintf(b, "  lastRefresh: %s\n", engine.Printable(u.LastRefresh))
	}
	if u.NeedsRefresh != nil {
		fmt.Fprintf(b, "  needsRefresh: %t\n", *u.NeedsRefresh)
	}
}

func renderHeldRoles(b *strings.Builder, r userResult, own map[string]bool) {
	b.WriteString("\nHELD ROLES\n")
	allKnown := r.Held.Global != nil && r.Held.Clusters != nil && r.Held.Projects != nil
	allEmpty := allKnown && len(*r.Held.Global) == 0 && len(*r.Held.Clusters) == 0 && len(*r.Held.Projects) == 0
	if allEmpty {
		b.WriteString("  none\n")
		return
	}

	switch {
	case r.Held.Global == nil:
		b.WriteString("  GLOBAL: unavailable\n")
	case len(*r.Held.Global) > 0:
		b.WriteString("  GLOBAL\n")
		for _, row := range *r.Held.Global {
			renderHeldRow(b, row, own)
		}
	}

	switch r.Held.Clusters {
	case nil:
		b.WriteString("  CLUSTER: unavailable\n")
	default:
		for _, entry := range *r.Held.Clusters {
			fmt.Fprintf(b, "  CLUSTER %s\n", scopeLabel(entry.Cluster, entry.DisplayName))
			for _, row := range entry.Roles {
				renderHeldRow(b, row, own)
			}
		}
	}

	switch r.Held.Projects {
	case nil:
		b.WriteString("  PROJECT: unavailable\n")
	default:
		for _, entry := range *r.Held.Projects {
			fmt.Fprintf(b, "  PROJECT %s\n", scopeLabel(entry.Project, entry.DisplayName))
			for _, row := range entry.Roles {
				renderHeldRow(b, row, own)
			}
		}
	}
}

// scopeLabel renders "displayName (id)" when enriched, "id" otherwise — an
// id is never replaced by a display name, only accompanied by it.
func scopeLabel(id, displayName string) string {
	if displayName == "" {
		return engine.Printable(id)
	}
	return fmt.Sprintf("%s (%s)", engine.Printable(displayName), engine.Printable(id))
}

// renderHeldRow prints one memberRow. The parenthetical names the holding
// principal only when it is a GROUP's — a direct row's principal is
// already the user's own and would otherwise state the obvious (or, for
// the empty-principalIds fallback row, misleadingly show the user's own
// id as if it were a separate holder).
func renderHeldRow(b *strings.Builder, row rbac.MemberRow, own map[string]bool) {
	suffix := ""
	if row.System {
		suffix = " [system]"
	}
	if !own[row.Principal] {
		fmt.Fprintf(b, "    %s  (via %s)  [%s]%s\n", engine.Printable(row.Role), engine.Printable(row.Principal), engine.Printable(row.Binding), suffix)
		return
	}
	fmt.Fprintf(b, "    %s  [%s]%s\n", engine.Printable(row.Role), engine.Printable(row.Binding), suffix)
}
