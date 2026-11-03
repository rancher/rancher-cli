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
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// The GRANTED BY side of access clusters: what the management plane says
// grants a subject its cluster access. It is read once per run, after the
// clusters are known, and it never changes the reachability answer or the
// exit code; its own completeness travels in bindingsDiscovery.

// Binding kinds as the records name them.
const (
	bindingCRTB = "ClusterRoleTemplateBinding"
	bindingPRTB = "ProjectRoleTemplateBinding"
	bindingGRB  = "GlobalRoleBinding"
)

// Provenance read stages, in the order issues are reported.
const (
	stageUser          = "user"
	stageUserAttribute = "userattribute"
	stageCRTB          = "crtb"
	stagePRTB          = "prtb"
	stageGRB           = "grb"
	stageGlobalRole    = "globalrole"
)

var stageOrder = []string{stageUser, stageUserAttribute, stageCRTB, stagePRTB, stageGRB, stageGlobalRole}

// Built-in GlobalRoles whose cluster access is not expressed as inherited
// RoleTemplates: their holders reach every cluster.
var allClusterGlobalRoles = map[string]bool{"admin": true, "restricted-admin": true}

// viaEntry names the identity a binding matched: the user id, one of the
// user's principal ids, or one of the group principals Rancher has cached.
type viaEntry struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// projectRef is a project as the (cluster, project) pair every comparison
// in the access suite uses; the spelled forms differ between steve, the
// bindings and the namespace labels.
type projectRef struct {
	Cluster string `json:"cluster"`
	Project string `json:"project"`
}

// bindingRecord is one management binding naming the subject. The fields
// present depend on Kind: a ClusterRoleTemplateBinding carries Cluster and
// RoleTemplate; a ProjectRoleTemplateBinding carries Project (and Cluster,
// the project's) and RoleTemplate; a GlobalRoleBinding carries GlobalRole,
// InheritedRoleTemplates (JSON null when the GlobalRole could not be read,
// an empty list when it was read and inherits nothing) and AllClusters.
// A binding a list returns twice is one record when the copies agree on
// every field but via, whose entries are merged; copies that disagree are
// a changed collection.
type bindingRecord struct {
	Kind                   string      `json:"kind"`
	Name                   string      `json:"name"`
	Namespace              string      `json:"namespace,omitempty"`
	Cluster                string      `json:"cluster,omitempty"`
	Project                *projectRef `json:"project,omitempty"`
	RoleTemplate           string      `json:"roleTemplate,omitempty"`
	GlobalRole             string      `json:"globalRole,omitempty"`
	InheritedRoleTemplates []string    `json:"inheritedRoleTemplates"`
	AllClusters            bool        `json:"allClusters,omitempty"`
	Via                    []viaEntry  `json:"via"`
	// inheritedUnknown is true when the GlobalRole read failed, so the
	// record's grants cannot be claimed.
	inheritedUnknown bool
}

// grant is one tagged entry of a compact row's grants: a RoleTemplate a
// binding names or inherits, or a GlobalRole that reaches every cluster.
type grant struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// stageIssue is one failed provenance read.
type stageIssue struct {
	Stage string `json:"stage"`
	Issue Issue  `json:"issue"`
}

// bindingsDiscovery says how much of the management plane could be read:
// complete (every read succeeded), partial (some failed; what was read is
// kept), unavailable (nothing could be read).
type bindingsDiscovery struct {
	Status string       `json:"status"`
	Issues []stageIssue `json:"issues"`
}

// groupAliases records the group principals Rancher has cached for the
// subject. Complete is always false: Rancher's cache is what it observed at
// the user's logins, never a directory expansion.
type groupAliases struct {
	Source   GroupsFrom `json:"source"`
	Status   string     `json:"status"`
	Count    *int       `json:"count"`
	Complete bool       `json:"complete"`
}

// provenance is everything GRANTED BY read for one run.
type provenance struct {
	Subject      AccessSubject
	PrincipalIDs []string
	Groups       []string
	Bindings     []bindingRecord
	Discovery    bindingsDiscovery
	GroupAliases groupAliases
}

// provenanceInput is what the reads need to know about the subject before
// they start. When the subject was resolved by name, ID and PrincipalIDs
// are already known and the User read is skipped.
type provenanceInput struct {
	Subject      AccessSubject
	PrincipalIDs []string
	Resolved     bool
}

// terminalProvenanceError reports whether a provenance read failure ends
// the run: an expired or revoked credential (401), the caller's
// cancellation, or the fan-out budget. Everything else (a 403, a 404, a
// server failure private to that read) is a provenance issue.
func terminalProvenanceError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	ce, ok := errors.AsType[*clierr.Error](err)
	return ok && ce.Code == clierr.CodeAuth
}

// readProvenance performs the GRANTED BY reads on the management cluster
// and matches the bindings to the subject. The returned error is only a
// terminal one (see terminalProvenanceError); every other failure is an
// issue inside the result.
func readProvenance(ctx context.Context, local engine.ClusterClient, attrs *steve.UserAttributes, in provenanceInput) (provenance, error) {
	out := provenance{Subject: in.Subject, PrincipalIDs: in.PrincipalIDs}
	issues := map[string]Issue{}
	fail := func(stage string, err error) error {
		if terminalProvenanceError(ctx, err) {
			return budgetError(ctx, err, "the "+stage+" read")
		}
		ce := clierr.From(err)
		issues[stage] = issueFrom(ce)
		return nil
	}

	userKind, _ := kinds.Lookup("user")
	if !in.Resolved {
		u, err := local.Get(ctx, userKind.GVR, "", in.Subject.Name)
		switch {
		case err == nil:
			out.Subject.ID = u.GetName()
			out.Subject.DisplayName, _, _ = unstructured.NestedString(u.Object, "displayName")
			out.PrincipalIDs, _, _ = unstructured.NestedStringSlice(u.Object, "principalIds")
		case clierr.IsNotFound(err):
			// Not a Rancher User (a service account, or an identity Rancher
			// never materialized): the review username is the only alias.
		default:
			if terr := fail(stageUser, err); terr != nil {
				return provenance{}, terr
			}
		}
	}
	userID := out.Subject.ID
	if userID == "" {
		userID = out.Subject.Name
	}

	out.GroupAliases = groupAliases{Source: GroupsRancherUserAttribute, Status: "unavailable"}
	attr, found, err := attrs.Get(ctx, userID)
	switch {
	case err != nil:
		if terr := fail(stageUserAttribute, err); terr != nil {
			return provenance{}, terr
		}
	case found:
		for _, g := range rbac.GroupPrincipalEntries(attr) {
			out.Groups = append(out.Groups, g.Principal)
		}
		slices.Sort(out.Groups)
		out.Groups = slices.Compact(out.Groups)
		n := len(out.Groups)
		out.GroupAliases = groupAliases{Source: GroupsRancherUserAttribute, Status: "available", Count: &n}
	default:
		zero := 0
		out.GroupAliases = groupAliases{Source: GroupsRancherUserAttribute, Status: "available", Count: &zero}
	}

	own := rbac.OwnIdentitySet(userID, out.PrincipalIDs)
	groupSet := map[string]bool{}
	for _, g := range out.Groups {
		groupSet[g] = true
	}
	match := func(b *unstructured.Unstructured) []viaEntry {
		return bindingVia(b, userID, own, groupSet)
	}

	records := map[[3]string]bindingRecord{}
	var conflict error
	add := func(rec bindingRecord) {
		key := [3]string{rec.Kind, rec.Namespace, rec.Name}
		prev, dup := records[key]
		if !dup {
			records[key] = rec
			return
		}
		if !sameBinding(prev, rec) {
			conflict = clierr.Retryable(clierr.CodeGeneric, fmt.Sprintf(
				"the management cluster listed %s %s twice with different fields; retry the command", rec.Kind, rec.Name))
			return
		}
		prev.Via = mergeVia(prev.Via, rec.Via)
		records[key] = prev
	}

	crtbKind, _ := kinds.Lookup("clusterroletemplatebinding")
	if list, err := local.List(ctx, crtbKind.GVR, "", ""); err == nil {
		for i := range list.Items {
			b := &list.Items[i]
			via := match(b)
			if len(via) == 0 {
				continue
			}
			rt, _, _ := unstructured.NestedString(b.Object, "roleTemplateName")
			add(bindingRecord{Kind: bindingCRTB, Name: b.GetName(), Namespace: b.GetNamespace(),
				Cluster: b.GetNamespace(), RoleTemplate: rt, Via: via})
		}
	} else if terr := fail(stageCRTB, err); terr != nil {
		return provenance{}, terr
	}

	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	if list, err := local.List(ctx, prtbKind.GVR, "", ""); err == nil {
		for i := range list.Items {
			b := &list.Items[i]
			via := match(b)
			if len(via) == 0 {
				continue
			}
			rt, _, _ := unstructured.NestedString(b.Object, "roleTemplateName")
			projectName, _, _ := unstructured.NestedString(b.Object, "projectName")
			ref := parseProjectName(projectName)
			rec := bindingRecord{Kind: bindingPRTB, Name: b.GetName(), Namespace: b.GetNamespace(), RoleTemplate: rt, Via: via}
			if ref != nil {
				rec.Cluster, rec.Project = ref.Cluster, ref
			}
			add(rec)
		}
	} else if terr := fail(stagePRTB, err); terr != nil {
		return provenance{}, terr
	}

	grbKind, _ := kinds.Lookup("globalrolebinding")
	grbRecords := map[[3]string]bindingRecord{}
	if list, err := local.List(ctx, grbKind.GVR, "", ""); err == nil {
		for i := range list.Items {
			b := &list.Items[i]
			via := match(b)
			if len(via) == 0 {
				continue
			}
			gr, _, _ := unstructured.NestedString(b.Object, "globalRoleName")
			rec := bindingRecord{Kind: bindingGRB, Name: b.GetName(), GlobalRole: gr, AllClusters: allClusterGlobalRoles[gr], Via: via}
			key := [3]string{rec.Kind, "", rec.Name}
			if prev, dup := grbRecords[key]; dup {
				if !sameBinding(prev, rec) {
					conflict = clierr.Retryable(clierr.CodeGeneric, fmt.Sprintf(
						"the management cluster listed %s %s twice with different fields; retry the command", rec.Kind, rec.Name))
					continue
				}
				prev.Via = mergeVia(prev.Via, rec.Via)
				grbRecords[key] = prev
				continue
			}
			grbRecords[key] = rec
		}
	} else if terr := fail(stageGRB, err); terr != nil {
		return provenance{}, terr
	}
	if conflict != nil {
		return provenance{}, conflict
	}
	grbs := make([]bindingRecord, 0, len(grbRecords))
	for _, g := range grbRecords {
		grbs = append(grbs, g)
	}
	if len(grbs) > 0 {
		grKind, _ := kinds.Lookup("globalrole")
		if list, err := local.List(ctx, grKind.GVR, "", ""); err == nil {
			inherited := map[string][]string{}
			for i := range list.Items {
				names, _, _ := unstructured.NestedStringSlice(list.Items[i].Object, "inheritedClusterRoles")
				slices.Sort(names)
				inherited[list.Items[i].GetName()] = slices.Compact(names)
			}
			for i := range grbs {
				rts := inherited[grbs[i].GlobalRole]
				if rts == nil {
					rts = []string{}
				}
				grbs[i].InheritedRoleTemplates = rts
			}
		} else if terr := fail(stageGlobalRole, err); terr != nil {
			return provenance{}, terr
		} else {
			for i := range grbs {
				grbs[i].inheritedUnknown = true
			}
		}
	}
	// A GlobalRoleBinding is cluster provenance only when its GlobalRole
	// reaches clusters: a built-in that reaches them all, or one with
	// inherited RoleTemplates. When the GlobalRole could not be read the
	// binding is kept, its reach unknown.
	for _, g := range grbs {
		if g.AllClusters || len(g.InheritedRoleTemplates) > 0 || g.inheritedUnknown {
			records[[3]string{g.Kind, "", g.Name}] = g
		}
	}
	out.Bindings = make([]bindingRecord, 0, len(records))
	for _, r := range records {
		out.Bindings = append(out.Bindings, r)
	}
	slices.SortFunc(out.Bindings, cmpBindings)
	out.Discovery = bindingsDiscovery{Status: "complete", Issues: []stageIssue{}}
	for _, stage := range stageOrder {
		if iss, ok := issues[stage]; ok {
			out.Discovery.Issues = append(out.Discovery.Issues, stageIssue{Stage: stage, Issue: iss})
		}
	}
	switch {
	case len(issues) == 0:
	case issues[stageCRTB] != (Issue{}) && issues[stagePRTB] != (Issue{}) && issues[stageGRB] != (Issue{}):
		out.Discovery.Status = "unavailable"
	default:
		out.Discovery.Status = "partial"
	}
	return out, nil
}

// sameBinding reports whether two copies of one binding agree on every
// consumed field but via.
func sameBinding(a, b bindingRecord) bool {
	if (a.Project == nil) != (b.Project == nil) || (a.Project != nil && *a.Project != *b.Project) {
		return false
	}
	return a.Kind == b.Kind && a.Name == b.Name && a.Namespace == b.Namespace && a.Cluster == b.Cluster &&
		a.RoleTemplate == b.RoleTemplate && a.GlobalRole == b.GlobalRole && a.AllClusters == b.AllClusters
}

// mergeVia unions two via lists, deduplicated by kind and value, in the
// fixed order user, principal, group and then by value.
func mergeVia(a, b []viaEntry) []viaEntry {
	seen := map[viaEntry]bool{}
	var out []viaEntry
	for _, v := range append(append([]viaEntry{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	rank := map[string]int{"user": 0, "principal": 1, "group": 2}
	slices.SortFunc(out, func(x, y viaEntry) int {
		if rank[x.Kind] != rank[y.Kind] {
			return rank[x.Kind] - rank[y.Kind]
		}
		return strings.Compare(x.Value, y.Value)
	})
	return out
}

// cmpBindings orders binding records by kind, namespace, name.
func cmpBindings(a, b bindingRecord) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// bindingVia names every identity a binding matched, in the fixed order
// user, principal, group; nil when it names none of them.
func bindingVia(b *unstructured.Unstructured, userID string, own map[string]bool, groups map[string]bool) []viaEntry {
	var via []viaEntry
	userName, _, _ := unstructured.NestedString(b.Object, "userName")
	userPrincipal, _, _ := unstructured.NestedString(b.Object, "userPrincipalName")
	groupPrincipal, _, _ := unstructured.NestedString(b.Object, "groupPrincipalName")
	if userName != "" && userName == userID {
		via = append(via, viaEntry{Kind: "user", Value: userName})
	}
	if userPrincipal != "" && own[userPrincipal] && userPrincipal != userID {
		via = append(via, viaEntry{Kind: "principal", Value: userPrincipal})
	}
	if groupPrincipal != "" && groups[groupPrincipal] {
		via = append(via, viaEntry{Kind: "group", Value: groupPrincipal})
	}
	return via
}

// parseProjectName splits a binding's projectName (<cluster>:<project>)
// into the pair; nil when the field does not have that shape.
func parseProjectName(s string) *projectRef {
	cluster, project, ok := strings.Cut(s, ":")
	if !ok || cluster == "" || project == "" {
		return nil
	}
	return &projectRef{Cluster: cluster, Project: project}
}

// bindingsFor returns the records that grant access to one cluster: the
// ClusterRoleTemplateBindings in its namespace, the
// ProjectRoleTemplateBindings on its projects, and every GlobalRoleBinding
// kept (its reach is cluster-wide by construction).
func bindingsFor(all []bindingRecord, clusterID string) []bindingRecord {
	var out []bindingRecord
	for _, b := range all {
		switch b.Kind {
		case bindingGRB:
			out = append(out, b)
		default:
			if b.Cluster == clusterID {
				out = append(out, b)
			}
		}
	}
	if out == nil {
		out = []bindingRecord{}
	}
	return out
}

// grantsFor is the compact union of what a cluster's records grant: the
// RoleTemplates they name or inherit, and the GlobalRole itself when it
// reaches every cluster. nil (JSON null) unless provenance is complete,
// because a partial read cannot claim the set is the whole of it.
func grantsFor(records []bindingRecord, discovery bindingsDiscovery) []grant {
	if discovery.Status != "complete" {
		return nil
	}
	seen := map[grant]bool{}
	var out []grant
	add := func(g grant) {
		if g.Name == "" || seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
	}
	for _, b := range records {
		switch b.Kind {
		case bindingGRB:
			if b.AllClusters {
				add(grant{Kind: "GlobalRole", Name: b.GlobalRole})
			}
			for _, rt := range b.InheritedRoleTemplates {
				add(grant{Kind: "RoleTemplate", Name: rt})
			}
		default:
			add(grant{Kind: "RoleTemplate", Name: b.RoleTemplate})
		}
	}
	slices.SortFunc(out, func(a, b grant) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if out == nil {
		out = []grant{}
	}
	return out
}
