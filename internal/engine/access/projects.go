package access

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const (
	refusedClusterNote            = "the proxy refuses the subject on this cluster; no namespace was asked about"
	unknownClusterNote            = "the cluster's probe failed; no namespace was asked about"
	membershipNotDeterminedPrefix = "membership not determined: "
	noBindingNote                 = "no matching Rancher binding names the subject; the server allowed the request"
	disagreementNote              = "binding present, the authorizer denies every namespace get"
	inheritedMark                 = "inherited"
)

// projectsCmd is `access projects`. It shares the cluster probe with
// `access clusters` and adds, per reachable cluster, the caller's steve
// namespace read and one SelfSubjectAccessReview per project namespace.
type projectsCmd struct {
	clustersCmd
}

// NewProjects builds `access projects`; see NewCan for the local argument.
func NewProjects(clients engine.ClusterClientFactory, roots engine.RootClientFactory, local engine.ClusterClient) runtime.Command {
	return &projectsCmd{clustersCmd{clients: clients, roots: roots, local: local}}
}

func (c *projectsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access projects",
		Usage: "List the projects a subject can see into, and the Rancher memberships behind them",
		Desc: "Lists the projects you can see, grouped by cluster, and asks each cluster's authorizer whether you (or an impersonated --as user) " +
			"may get every namespace a project owns: the grant Rancher gives a project's members. Visibility is that answer rolled up per project " +
			"(all, partial, none, unknown). Beside it, the ProjectRoleTemplateBindings that make the subject a member of the project, and the " +
			"cluster and global bindings that reach it without membership. Projects and namespaces are what steve lists for you; a member asking " +
			"about themselves sees their own projects only. Exit 0 when every cluster, project and namespace answered, 13 when any is unknown.",
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Desc: "restrict to one cluster, by id or unique display name; omitted = every cluster you can list"},
			{Name: flagAs, Type: "string", Desc: "user name, id, or principal; omitted = you"},
			{Name: flagType, Type: "string", Enum: []string{typeUser}, Desc: "kind of the --as value (only user is accepted here)"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access projects",
			"rancher access projects --cluster prod --as alice",
			"rancher access projects --as alice -o json",
		},
		CommandType:      runtime.ReadOnly,
		Auth:             true,
		SuccessExitCodes: []int{0, exitUnknown},
		Shape:            runtime.ShapeText,
	}
}

// LiveSource hands the surface the command's own --cluster source: the
// steve cluster collection the command resolves the flag against, so the
// values offered are the ones it accepts. Every other site of the command
// is declined.
func (*projectsCmd) LiveSource() completion.Source { return complete.ProjectsClusterSource() }

// projectRow is one project of the caller's set with the subject's answers.
type projectRow struct {
	Project     string          `json:"project"`
	DisplayName string          `json:"displayName"`
	Namespaces  []namespaceRow  `json:"namespaces"`
	AllowedIn   int             `json:"allowedIn"`
	UnknownIn   int             `json:"unknownIn"`
	Visibility  string          `json:"visibility"`
	Source      ResultSource    `json:"source"`
	Complete    bool            `json:"complete"`
	Bindings    []bindingRecord `json:"bindings"`
	// KubernetesBindings names the Kubernetes RBAC bindings the authorizer's
	// reasons credit when a visible project has no matching Rancher
	// binding and every namespace reason parses as an RBAC reason;
	// otherwise null.
	KubernetesBindings []string `json:"kubernetesBindings"`
	Note               string   `json:"note"`
}

// namespaceDiscovery is the outcome of one cluster's steve namespace read.
type namespaceDiscovery struct {
	Source RowSource `json:"source"`
	Issue  *Issue    `json:"issue"`
}

// unlistedProject groups the namespaces whose project annotation names a
// project the caller's project set does not carry.
type unlistedProject struct {
	Project    string   `json:"project"`
	Namespaces []string `json:"namespaces"`
}

// projectsClusterRow is a cluster row of AccessClusters extended with the
// cluster's namespace discovery and its project rows.
type projectsClusterRow struct {
	clusterRow
	NamespaceDiscovery        *namespaceDiscovery `json:"namespaceDiscovery"`
	UnlistedProjectNamespaces []unlistedProject   `json:"unlistedProjectNamespaces"`
	Projects                  []projectRow        `json:"projects"`
}

// visibleProject is the compact form of a project the subject can see into.
type visibleProject struct {
	Cluster    string `json:"cluster"`
	Project    string `json:"project"`
	Namespaces int    `json:"namespaces"`
	AllowedIn  int    `json:"allowedIn"`
	UnknownIn  int    `json:"unknownIn"`
	Complete   bool   `json:"complete"`
}

// membership is a visible project the subject is a Rancher member of,
// with the RoleTemplates its ProjectRoleTemplateBindings grant.
type membership struct {
	visibleProject
	Grants []grant `json:"grants"`
}

// memberOfSet is memberOf[] with its own completeness: true when every
// input the ProjectRoleTemplateBinding match needs was read; the inputs
// are listed where the condition is computed, at membershipComplete in
// assemble.
type memberOfSet struct {
	Projects []membership `json:"projects"`
	Complete bool         `json:"complete"`
}

// inheritedAccess is a visible project reached through cluster or global
// bindings only, never a membership.
type inheritedAccess struct {
	visibleProject
	Grants []grant `json:"grants"`
}

type projectsRollup struct {
	Clusters              int  `json:"clusters"`
	Reachable             int  `json:"reachable"`
	Refused               int  `json:"refused"`
	ClustersUnknown       int  `json:"clustersUnknown"`
	ClustersNotApplicable int  `json:"clustersNotApplicable"`
	Projects              int  `json:"projects"`
	All                   int  `json:"all"`
	Partial               int  `json:"partial"`
	None                  int  `json:"none"`
	ProjectsUnknown       int  `json:"projectsUnknown"`
	ProjectsNotApplicable int  `json:"projectsNotApplicable"`
	NamespacesAsked       int  `json:"namespacesAsked"`
	NamespacesUnknown     int  `json:"namespacesUnknown"`
	Members               int  `json:"members"`
	Complete              bool `json:"complete"`
}

// projectsResult is the AccessProjects payload.
type projectsResult struct {
	Subject           AccessSubject        `json:"subject"`
	Identity          Identity             `json:"identity"`
	Cluster           string               `json:"cluster,omitempty"`
	Discovery         discoveryStatus      `json:"discovery"`
	BindingsDiscovery bindingsDiscovery    `json:"bindingsDiscovery"`
	GroupAliases      groupAliases         `json:"groupAliases"`
	Clusters          []projectsClusterRow `json:"clusters"`
	VisibleProjects   []visibleProject     `json:"visibleProjects"`
	MemberOf          *memberOfSet         `json:"memberOf"`
	InheritedAccess   []inheritedAccess    `json:"inheritedAccess"`
	Rollup            projectsRollup       `json:"rollup"`
	ResultSource      ResultSource         `json:"resultSource"`
}

// resolveClusterArg picks one cluster of the caller's steve set, the set
// the command reports, with the shared match rule: an id wins over another
// cluster's display name, and warning names the others. "local" is always
// the management cluster's row. It sends no request: a cluster the set does
// not carry is not one the command can report.
func resolveClusterArg(clusters []discoveredCluster, arg string) (discoveredCluster, string, error) {
	if arg == engine.LocalCluster {
		if i := slices.IndexFunc(clusters, func(c discoveredCluster) bool { return c.ID == engine.LocalCluster }); i >= 0 {
			return clusters[i], "", nil
		}
	}
	list := make([]steve.Cluster, len(clusters))
	for i, c := range clusters {
		list[i] = steve.Cluster{ID: c.ID, DisplayName: c.DisplayName}
	}
	id, warning, err := steve.MatchCluster(list, arg, steve.ClusterText(arg))
	if err != nil {
		return discoveredCluster{}, "", err
	}
	return clusters[slices.IndexFunc(clusters, func(c discoveredCluster) bool { return c.ID == id })], warning, nil
}

func (c *projectsCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	as, err := ParseSubject(in.String(flagAs), in.String(flagType))
	if err != nil {
		return runtime.Result{}, err
	}
	switch as.Kind {
	case KindGroup:
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "no server operation lists a group's projects; "+pointQuestionPlaceholders+"; ask a point question with: "+groupAccessCan(as.Raw))
	case KindServiceAccount:
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "a service account belongs to one cluster; use "+clierr.Rancher+" access list --cluster <cluster> --as "+clierr.ShellQuote(as.Raw))
	}
	if as.Raw == anonymousUser {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "the anonymous identity cannot be impersonated; "+pointQuestionPlaceholders+"; ask a point question with: "+anonymousAccessCan())
	}
	budget, err := parseTimeout(in)
	if err != nil {
		return runtime.Result{}, err
	}
	ctx, cancel := fanOutContext(ctx, budget)
	defer cancel()

	local, err := localClient(ctx, c.clients, c.local)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the management cluster client")
	}
	caller, r := whoami(ctx, local)
	if r.kind != outcomeAnswer {
		if r.kind == outcomeNoClusterAccess {
			return runtime.Result{}, clierr.New(clierr.CodeRBAC, "you have no access to the management cluster; "+noClusterAccessDetail)
		}
		if ce := tlsTrustCause(r.err); ce != nil {
			return runtime.Result{}, ce
		}
		return runtime.Result{}, budgetError(ctx, r.err, "the caller's identity review")
	}

	root, err := c.roots.Root()
	if err != nil {
		return runtime.Result{}, err
	}
	clusters, err := listClusters(ctx, root)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "cluster discovery")
	}
	projects, err := listProjects(ctx, root)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "project discovery")
	}
	if err := projectsWithinClusters(projects, clusters); err != nil {
		return runtime.Result{}, err
	}
	clusterArg := in.String("cluster")
	var warning string
	if clusterArg != "" {
		chosen, w, err := resolveClusterArg(clusters, clusterArg)
		if err != nil {
			return runtime.Result{}, err
		}
		warning = w
		clusters = []discoveredCluster{chosen}
		projects = slices.DeleteFunc(slices.Clone(projects), func(p discoveredProject) bool { return p.Cluster != chosen.ID })
		clusterArg = chosen.ID
	}

	subject := AccessSubject{Kind: KindUser, Name: caller.Username}
	identity := IdentityCaller
	pin := provenanceInput{Subject: subject}
	if as.Raw != "" {
		u, err := findRancherUser(ctx, local, as.Raw)
		if err != nil {
			return runtime.Result{}, budgetError(ctx, err, "subject resolution")
		}
		display, _, _ := unstructured.NestedString(u.Object, "displayName")
		principals, _, _ := unstructured.NestedStringSlice(u.Object, "principalIds")
		resolved := AccessSubject{Kind: KindUser, Name: u.GetName(), ID: u.GetName(), DisplayName: display}
		pin = provenanceInput{Subject: resolved, PrincipalIDs: principals, Resolved: true}
		subject = resolved
		if resolved.Name != caller.Username {
			identity = IdentityImpersonated
		}
	}

	clusterRows, err := c.probe(ctx, clusters, subject, identity, caller)
	if err != nil {
		return runtime.Result{}, err
	}

	rows := make([]projectsClusterRow, len(clusterRows))
	for i, cr := range clusterRows {
		rows[i] = projectsClusterRow{clusterRow: cr, UnlistedProjectNamespaces: []unlistedProject{}, Projects: []projectRow{}}
		for _, p := range projects {
			if p.Cluster == cr.Cluster {
				rows[i].Projects = append(rows[i].Projects, projectRow{Project: p.Project, DisplayName: p.DisplayName, Namespaces: []namespaceRow{}, Bindings: []bindingRecord{}})
			}
		}
		if cr.Access != reachable {
			// A cluster no review answers for contributes its line and no
			// project work. Its projects carry the cluster's own answer:
			// refused means the proxy admits the subject nowhere on the
			// cluster (visibility none, the proxy's answer); not applicable
			// (local for another user) is not applicable for its projects
			// either and never counts against completeness; an unknown
			// cluster leaves its projects unknown.
			for j := range rows[i].Projects {
				pr := &rows[i].Projects[j]
				switch cr.Access {
				case refused:
					pr.Visibility, pr.Source, pr.Complete, pr.Note = visibilityNone, ResultSource(RowRancherProxy), true, refusedClusterNote
				case notApplicable:
					pr.Visibility, pr.Source, pr.Complete, pr.Note = visibilityNotApplicable, ResultSource(RowNotApplicable), true, localCannotImpersonateNote
				default:
					pr.Visibility, pr.Source, pr.Note = visibilityUnknown, ResultSource(cr.Source), unknownClusterNote
				}
			}
			continue
		}
		if err := c.reviewCluster(ctx, root, &rows[i], subject, identity); err != nil {
			return runtime.Result{}, err
		}
	}

	prov, err := readProvenance(ctx, local, pin)
	if err != nil {
		return runtime.Result{}, err
	}
	out := projectsResult{
		Subject: prov.Subject, Identity: identity, Cluster: clusterArg,
		Discovery:         discoveryStatus{Source: "rancher", Complete: true},
		BindingsDiscovery: prov.Discovery, GroupAliases: prov.GroupAliases,
		Clusters: rows, VisibleProjects: []visibleProject{}, InheritedAccess: []inheritedAccess{},
	}
	out.assemble(prov, pin.Resolved)
	exit := 0
	if !out.Rollup.Complete {
		exit = exitUnknown
	}
	res := runtime.Result{Kind: "AccessProjects", Data: out, ExitStatus: exit,
		Text: projectsText(out, false), WideText: projectsText(out, true)}
	if warning != "" {
		res.Warnings = []string{warning}
	}
	return res, nil
}

// reviewCluster reads the cluster's namespaces as the caller and asks the
// authorizer, as the subject, about every namespace of every listed
// project. Clusters are reviewed one after another, so the fan-out limit
// bounds the reviews in flight at any moment.
func (c *projectsCmd) reviewCluster(ctx context.Context, root engine.RootClient, row *projectsClusterRow, subject AccessSubject, identity Identity) error {
	namespaces, err := listNamespaces(ctx, root, row.Cluster)
	if err != nil {
		if ctx.Err() != nil {
			return budgetError(ctx, err, "namespace discovery on cluster "+row.Cluster)
		}
		// A 401 is the credential refused, whatever the path: terminal. A
		// 403 and every other failure are private to this cluster's read.
		if st, ok := clierr.HTTPStatus(err); ok && st == http.StatusUnauthorized {
			return err
		}
		ce := clierr.From(err)
		iss := issueFrom(ce)
		row.NamespaceDiscovery = &namespaceDiscovery{Source: RowScopeDiscovery, Issue: &iss}
		for j := range row.Projects {
			pr := &row.Projects[j]
			v := rollupVisibility(nil, true)
			pr.Visibility, pr.Source, pr.Complete, pr.Note = v.Visibility, v.Source, v.Complete, v.Note
		}
		return nil
	}
	row.NamespaceDiscovery = &namespaceDiscovery{Source: RowScopeDiscovery}
	listed := map[string]bool{}
	for _, pr := range row.Projects {
		listed[pr.Project] = true
	}
	byProject := map[string][]string{}
	unlisted := map[string][]string{}
	for _, ns := range namespaces {
		switch {
		case ns.Project == "":
			// Belongs to no project.
		case listed[ns.Project]:
			byProject[ns.Project] = append(byProject[ns.Project], ns.Name)
		default:
			unlisted[ns.Project] = append(unlisted[ns.Project], ns.Name)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(unlisted)) {
		names := unlisted[p]
		slices.Sort(names)
		row.UnlistedProjectNamespaces = append(row.UnlistedProjectNamespaces, unlistedProject{Project: p, Namespaces: names})
	}

	var client engine.ClusterClient
	if identity == IdentityCaller {
		client, err = c.clients.Cluster(ctx, row.Cluster)
	} else {
		client, err = c.clients.ImpersonatedCluster(ctx, row.Cluster, engine.Impersonation{User: subject.Name})
	}
	if err != nil {
		return budgetError(ctx, err, "the client for cluster "+row.Cluster)
	}
	for j := range row.Projects {
		pr := &row.Projects[j]
		names := byProject[pr.Project]
		slices.Sort(names)
		nsRows, failure := reviewNamespaces(ctx, client, names)
		if failure != nil {
			return budgetError(ctx, failure.Err, fmt.Sprintf("the namespace reviews of project %s/%s", row.Cluster, pr.Project))
		}
		pr.Namespaces = nsRows
		v := rollupVisibility(nsRows, false)
		pr.Visibility, pr.Source, pr.AllowedIn, pr.UnknownIn, pr.Complete, pr.Note = v.Visibility, v.Source, v.AllowedIn, v.UnknownIn, v.Complete, v.Note
	}
	return nil
}

// bindingsForProject returns the records that reach one project: its own
// ProjectRoleTemplateBindings, the cluster's ClusterRoleTemplateBindings
// and every GlobalRoleBinding kept.
func bindingsForProject(all []bindingRecord, clusterID, project string) []bindingRecord {
	out := []bindingRecord{}
	for _, b := range all {
		switch b.Kind {
		case bindingGRB:
			out = append(out, b)
		case bindingCRTB:
			if b.Cluster == clusterID {
				out = append(out, b)
			}
		case bindingPRTB:
			if b.Project != nil && b.Project.Cluster == clusterID && b.Project.Project == project {
				out = append(out, b)
			}
		}
	}
	return out
}

// assemble fills the per-project bindings, the compact lists, the rollup and
// the result source from the reviewed rows and the provenance read.
func (out *projectsResult) assemble(prov provenance, resolved bool) {
	memberships := []membership{}
	failed := map[string]bool{}
	for _, si := range prov.Discovery.Issues {
		failed[si.Stage] = true
	}
	prtbReadFailed := failed["prtb"]
	// Membership evidence is complete when every alias input the PRTB match
	// needs was read: the PRTBs themselves, the cached group principals,
	// and the caller's own principal ids (skipped when the subject was
	// resolved by name, which already carried them).
	membershipComplete := !prtbReadFailed && !failed["userattribute"] && (resolved || !failed["user"]) && prov.GroupAliases.Status == "available"
	// Inherited access is an exclusive classification too: it needs the
	// membership evidence (no hidden PRTB) and the two inherited-binding
	// reads (no hidden CRTB or GRB). Its grants also need the GlobalRole
	// read when a GlobalRoleBinding is among the entry's bindings.
	inheritedComplete := membershipComplete && !failed["crtb"] && !failed["grb"]
	leafSources := map[RowSource]bool{}
	for i := range out.Clusters {
		cr := &out.Clusters[i]
		cr.Bindings = bindingsFor(prov.Bindings, cr.Cluster)
		out.Rollup.Clusters++
		switch cr.Access {
		case reachable:
			out.Rollup.Reachable++
		case refused:
			out.Rollup.Refused++
		case unknown:
			out.Rollup.ClustersUnknown++
		case notApplicable:
			out.Rollup.ClustersNotApplicable++
		}
		if cr.Source != RowNotApplicable {
			leafSources[cr.Source] = true
		}
		if cr.NamespaceDiscovery != nil && cr.NamespaceDiscovery.Issue != nil {
			leafSources[RowScopeDiscovery] = true
		}
		for j := range cr.Projects {
			pr := &cr.Projects[j]
			pr.Bindings = bindingsForProject(prov.Bindings, cr.Cluster, pr.Project)
			out.Rollup.Projects++
			switch pr.Visibility {
			case visibilityAll:
				out.Rollup.All++
			case visibilityPartial:
				out.Rollup.Partial++
			case visibilityNone:
				out.Rollup.None++
			case visibilityNotApplicable:
				out.Rollup.ProjectsNotApplicable++
			default:
				out.Rollup.ProjectsUnknown++
			}
			out.Rollup.NamespacesAsked += len(pr.Namespaces)
			out.Rollup.NamespacesUnknown += pr.UnknownIn
			if cr.Access == reachable && cr.NamespaceDiscovery != nil && cr.NamespaceDiscovery.Issue == nil && len(pr.Namespaces) == 0 {
				leafSources[RowScopeDiscovery] = true
			}
			for _, ns := range pr.Namespaces {
				leafSources[ns.Source] = true
			}
			visible := pr.Visibility == visibilityAll || pr.Visibility == visibilityPartial
			var prtbs, others []bindingRecord
			for _, b := range pr.Bindings {
				if b.Kind == bindingPRTB {
					prtbs = append(prtbs, b)
				} else {
					others = append(others, b)
				}
			}
			switch {
			case visible:
				vp := visibleProject{Cluster: cr.Cluster, Project: pr.Project, Namespaces: len(pr.Namespaces), AllowedIn: pr.AllowedIn, UnknownIn: pr.UnknownIn, Complete: pr.Complete}
				out.VisibleProjects = append(out.VisibleProjects, vp)
				switch {
				case len(prtbs) > 0:
					// A matched PRTB proves the membership on its own; its
					// grants are the RoleTemplates it names, claimed only
					// when the membership evidence is complete.
					var grants []grant
					if membershipComplete {
						grants = prtbGrants(prtbs)
					}
					memberships = append(memberships, membership{visibleProject: vp, Grants: grants})
				case !inheritedComplete:
					// A PRTB, CRTB or GRB may be hidden by a failed read or
					// alias resolution: neither "inherited only" nor "no
					// binding" can be claimed.
				case len(others) > 0:
					out.InheritedAccess = append(out.InheritedAccess, inheritedAccess{visibleProject: vp, Grants: inheritedGrants(others, failed)})
				case prov.Discovery.Status == "complete":
					pr.Note = noBindingNote
					pr.KubernetesBindings = kubernetesBindings(pr.Namespaces)
				}
			case pr.Visibility == visibilityNone && cr.Access == reachable && len(prtbs) > 0:
				// The authorizer was asked and denied every namespace while
				// a Rancher binding names the subject: a disagreement. A
				// refused cluster's projects keep the proxy's note.
				pr.Note = disagreementNote
			}
		}
	}
	if prtbReadFailed {
		out.MemberOf = nil
	} else {
		out.MemberOf = &memberOfSet{Projects: memberships, Complete: membershipComplete}
	}
	if !inheritedComplete {
		out.InheritedAccess = nil
	}
	out.Rollup.Members = len(memberships)
	out.Rollup.Complete = out.Rollup.ClustersUnknown == 0 && out.Rollup.ProjectsUnknown == 0 && out.Rollup.NamespacesUnknown == 0 &&
		!slices.ContainsFunc(out.Clusters, func(c projectsClusterRow) bool {
			return c.NamespaceDiscovery != nil && c.NamespaceDiscovery.Issue != nil
		})
	switch {
	case len(leafSources) == 0 && out.Rollup.Clusters > 0 && out.Rollup.ClustersNotApplicable == out.Rollup.Clusters:
		out.ResultSource = ResultSource(RowNotApplicable)
	case len(leafSources) == 0:
		out.ResultSource = SourceScopeDiscovery
	case len(leafSources) == 1:
		for s := range leafSources {
			out.ResultSource = ResultSource(s)
		}
	default:
		out.ResultSource = SourceMixed
	}
}

// projectsText renders the default and the wide view.
func projectsText(out projectsResult, wide bool) string {
	who := output.Printable(out.Subject.Name)
	if out.Identity == IdentityCaller {
		who = "you"
	} else if out.Subject.DisplayName != "" {
		who = fmt.Sprintf("%s (%s)", output.Printable(out.Subject.DisplayName), who)
	}
	var b strings.Builder
	scope := "projects you can list"
	if out.Cluster != "" {
		scope = "projects you can list on " + output.Printable(out.Cluster)
	}
	fmt.Fprintf(&b, "%s, visibility for %s\n\n", scope, who)
	header := []string{"CLUSTER", "PROJECT", "NAME", "VISIBILITY", "GRANTED BY", "NOTE"}
	if wide {
		header = []string{"CLUSTER", "PROJECT", "NAME", "VISIBILITY", "NAMESPACES", "GRANTED BY", "NOTE"}
	}
	rows := make([][]string, 0, len(out.Clusters))
	for _, cr := range out.Clusters {
		if len(cr.Projects) == 0 || cr.Access != reachable {
			note := cr.Note
			if cr.Issue != nil {
				note = cr.Issue.Message
			}
			if cr.Access == reachable && note == "" {
				note = "no projects listed"
			}
			row := []string{cr.Cluster, "-", "-", "cluster " + cr.Access}
			if wide {
				row = append(row, "-")
			}
			row = append(row, grantedByCell(cr.Bindings, out.BindingsDiscovery), note)
			rows = append(rows, row)
			if cr.Access != reachable {
				continue
			}
		}
		if cr.NamespaceDiscovery != nil && cr.NamespaceDiscovery.Issue != nil {
			row := []string{cr.Cluster, "-", "-", "namespace discovery failed"}
			if wide {
				row = append(row, "-")
			}
			rows = append(rows, append(row, "-", runtime.PrintableRemedies(cr.NamespaceDiscovery.Issue.Message)))
		}
		for _, pr := range cr.Projects {
			v := visibility{Visibility: pr.Visibility, AllowedIn: pr.AllowedIn, UnknownIn: pr.UnknownIn}
			row := []string{cr.Cluster, pr.Project, output.Printable(pr.DisplayName), visibilityCell(v, len(pr.Namespaces))}
			if wide {
				cell := "-"
				if len(pr.Namespaces) > 0 {
					var ns []string
					for _, n := range pr.Namespaces {
						ns = append(ns, output.Printable(n.Namespace)+"="+n.Access)
					}
					cell = strings.Join(ns, " ")
				}
				row = append(row, cell)
			}
			row = append(row, projectGrantedByCell(pr.Bindings, out.BindingsDiscovery), runtime.PrintableRemedies(pr.Note))
			rows = append(rows, row)
		}
	}
	writeTable(&b, "", header, rows)
	b.WriteString("\n")
	switch {
	case out.MemberOf == nil:
		reason := "the ProjectRoleTemplateBinding read failed"
		for _, si := range out.BindingsDiscovery.Issues {
			if si.Stage == "prtb" {
				reason = runtime.PrintableRemedies(si.Issue.Message)
			}
		}
		fmt.Fprintf(&b, "%s%s\n", membershipNotDeterminedPrefix, reason)
	case len(out.MemberOf.Projects) == 0:
		fmt.Fprintf(&b, "%s a member of: none\n", memberPhrase(who))
	default:
		var items []string
		for _, m := range out.MemberOf.Projects {
			s := output.Printable(m.Cluster) + "/" + output.Printable(m.Project)
			if names := grantNames(m.Grants); names != "" {
				s += " (" + names + ")"
			}
			items = append(items, s)
		}
		fmt.Fprintf(&b, "%s a member of: %s\n", memberPhrase(who), strings.Join(items, ", "))
	}
	var noBinding []string
	for _, cr := range out.Clusters {
		for _, pr := range cr.Projects {
			if pr.Note == noBindingNote {
				entry := output.Printable(cr.Cluster) + "/" + output.Printable(pr.Project)
				if len(pr.KubernetesBindings) > 0 {
					names := make([]string, 0, len(pr.KubernetesBindings))
					for _, n := range pr.KubernetesBindings {
						names = append(names, output.Printable(n))
					}
					entry += " (" + strings.Join(names, ", ") + ")"
				}
				noBinding = append(noBinding, entry)
			}
		}
	}
	if len(noBinding) > 0 {
		fmt.Fprintf(&b, "visible through Kubernetes bindings, not a Rancher member: %s\n", strings.Join(noBinding, ", "))
	}
	fmt.Fprintf(&b, "%s\n", visibilityDefinition)
	if out.InheritedAccess == nil {
		b.WriteString("inherited access not determined: a binding read the classification depends on failed\n")
	}
	if len(out.InheritedAccess) > 0 {
		var items []string
		for _, ia := range out.InheritedAccess {
			s := output.Printable(ia.Cluster) + "/" + output.Printable(ia.Project)
			if names := grantNames(ia.Grants); names != "" {
				s += " (" + names + ")"
			}
			items = append(items, s)
		}
		fmt.Fprintf(&b, "inherited access (cluster or global grants, not membership): %s\n", strings.Join(items, ", "))
	}
	if out.GroupAliases.Status == "available" {
		fmt.Fprintf(&b, "%s\n", groupsCachedNote)
	}
	if out.BindingsDiscovery.Status != "complete" {
		var reasons []string
		for _, si := range out.BindingsDiscovery.Issues {
			reasons = append(reasons, si.Stage+": "+runtime.PrintableRemedies(si.Issue.Message))
		}
		fmt.Fprintf(&b, "bindings: %s (%s)\n", out.BindingsDiscovery.Status, strings.Join(reasons, "; "))
	}
	r := out.Rollup
	fmt.Fprintf(&b, "%d clusters: %d reachable, %d refused, %d unknown", r.Clusters, r.Reachable, r.Refused, r.ClustersUnknown)
	if r.ClustersNotApplicable > 0 {
		fmt.Fprintf(&b, ", %d not applicable", r.ClustersNotApplicable)
	}
	fmt.Fprintf(&b, "; %d projects: %d all, %d partial, %d none, %d unknown", r.Projects, r.All, r.Partial, r.None, r.ProjectsUnknown)
	if r.ProjectsNotApplicable > 0 {
		fmt.Fprintf(&b, ", %d not applicable", r.ProjectsNotApplicable)
	}
	fmt.Fprintf(&b, "; %d namespaces asked, %d unknown; %d memberships\n", r.NamespacesAsked, r.NamespacesUnknown, r.Members)
	fmt.Fprintf(&b, "source: %s | identity: %s\n", out.ResultSource, out.Identity)
	return b.String()
}

// projectGrantedByCell renders a project's bindings: its own
// ProjectRoleTemplateBindings, the cluster's ClusterRoleTemplateBindings
// marked inherited, and GlobalRoleBindings as in access clusters.
func projectGrantedByCell(records []bindingRecord, discovery bindingsDiscovery) string {
	if len(records) == 0 {
		if discovery.Status == "complete" {
			return "-"
		}
		return "?"
	}
	parts := make([]string, 0, len(records))
	for _, b := range records {
		switch b.Kind {
		case bindingPRTB:
			parts = append(parts, fmt.Sprintf("prtb/%s (%s)", b.Name, b.RoleTemplate))
		case bindingCRTB:
			parts = append(parts, fmt.Sprintf("crtb/%s (%s, %s)", b.Name, b.RoleTemplate, inheritedMark))
		case bindingGRB:
			parts = append(parts, fmt.Sprintf("grb/%s (%s)", b.Name, b.GlobalRole))
		}
	}
	return strings.Join(parts, "; ")
}

// prtbGrants is the RoleTemplates a project's ProjectRoleTemplateBindings
// name, deduplicated in order.
func prtbGrants(prtbs []bindingRecord) []grant {
	seen := map[grant]bool{}
	out := []grant{}
	for _, b := range prtbs {
		g := grant{Kind: "RoleTemplate", Name: b.RoleTemplate}
		if g.Name == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

// inheritedGrants is what a project's cluster and global bindings grant.
// The CRTB and GRB reads succeeded whenever this runs (inheritedAccess is
// null otherwise); the GlobalRole read must have succeeded too when a
// GlobalRoleBinding is among the records, since its grants come from the
// GlobalRole. nil when that read failed.
func inheritedGrants(records []bindingRecord, failed map[string]bool) []grant {
	for _, b := range records {
		if b.Kind == bindingGRB && (failed["globalrole"] || b.inheritedUnknown) {
			return nil
		}
	}
	return grantsFor(records, bindingsDiscovery{Status: "complete"})
}

// kubernetesBindings names the bindings the authorizer's reasons credit for
// a project's allowed namespaces, when every allowed namespace carries a
// reason that parses as a Kubernetes RBAC reason; nil otherwise. A
// RoleBinding is written <namespace>/<name>.
func kubernetesBindings(rows []namespaceRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if r.Access != nsAllowed {
			continue
		}
		b, ok := parseReason(r.Reason)
		if !ok {
			return nil
		}
		name := b.Kind + " " + b.Name
		if b.Namespace != "" {
			name = b.Kind + " " + b.Namespace + "/" + b.Name
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// memberPhrase opens the membership line: "you are" for the caller, "<who>
// is" for anyone else.
func memberPhrase(who string) string {
	if who == "you" {
		return "you are"
	}
	return who + " is"
}

func grantNames(gs []grant) string {
	if gs == nil {
		return ""
	}
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, output.Printable(g.Name))
	}
	return strings.Join(names, ", ")
}
