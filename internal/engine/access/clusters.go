package access

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// RowSource is where an access clusters row's answer came from: the
// Rancher proxy (admitted or refused), a failed request, or nowhere,
// because the operation does not apply to that row. Namespace and project
// rows add the cluster's authorizer and, for a row no request answered
// because discovery did not reach it, scope discovery.
type RowSource string

const (
	RowRancherProxy   RowSource = "rancherProxy"
	RowRequest        RowSource = "request"
	RowNotApplicable  RowSource = "notApplicable"
	RowAuthorizer     RowSource = "authorizer"
	RowScopeDiscovery RowSource = "scopeDiscovery"
)

// Reachability values.
const (
	reachable     = "reachable"
	refused       = "refused"
	unknown       = "unknown"
	notApplicable = "notApplicable"
)

var localCannotImpersonateNote = "the management cluster cannot impersonate; <verb> and <resource> are what to ask; ask with: " + clierr.Rancher + " access can <verb> <resource> --cluster local"

// reachabilitySentence is printed once under the table: what the answer
// proves and what it does not.
const reachabilitySentence = "reachable = Rancher's proxy admits the identity to this cluster; what it may do there is access list"

const groupsCachedNote = "group matches are limited to the groups Rancher has cached for this user"

type clustersCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	local   engine.ClusterClient
}

// NewClusters builds `access clusters`; see NewCan for the local argument.
func NewClusters(clients engine.ClusterClientFactory, roots engine.RootClientFactory, local engine.ClusterClient) runtime.Command {
	return &clustersCmd{clients: clients, roots: roots, local: local}
}

func (c *clustersCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access clusters",
		Usage: "List the clusters Rancher lets a subject into, and the bindings that grant it",
		Desc: "Lists the management clusters you can see and, for each, whether Rancher's proxy admits you (or an impersonated --as user) to the cluster's API: " +
			"reachable, refused, or unknown when the probe failed. Beside it, the ClusterRoleTemplateBindings, ProjectRoleTemplateBindings and " +
			"GlobalRoleBindings on the management cluster that name the subject, its principals, or a group Rancher has cached for it. " +
			"Reachability is the proxy's answer before any Kubernetes rule is consulted; what the identity may do inside a cluster is `rancher access list`'s question. " +
			"Exit 0 when every cluster answered, 13 when any is unknown.",
		Flags: []runtime.ArgSpec{
			{Name: flagAs, Type: "string", Desc: "user name, id, or principal; omitted = you"},
			{Name: flagType, Type: "string", Enum: []string{typeUser}, Desc: "kind of the --as value (only user is accepted here)"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access clusters",
			"rancher access clusters --as alice",
			"rancher access clusters --as alice -o json",
		},
		CommandType:      runtime.ReadOnly,
		Auth:             true,
		SuccessExitCodes: []int{0, exitUnknown},
		Shape:            runtime.ShapeText,
	}
}

// clusterRow is one cluster of the caller's set with the subject's answer.
type clusterRow struct {
	Cluster     string          `json:"cluster"`
	DisplayName string          `json:"displayName"`
	Returned    bool            `json:"returned"`
	Access      string          `json:"access"`
	Source      RowSource       `json:"source"`
	Issue       *Issue          `json:"issue"`
	Identity    *string         `json:"identity"`
	Bindings    []bindingRecord `json:"bindings"`
	Note        string          `json:"note"`
}

// reachableEntry is the compact form of a reachable row.
type reachableEntry struct {
	Cluster string  `json:"cluster"`
	Grants  []grant `json:"grants"`
}

type clustersRollup struct {
	Clusters      int  `json:"clusters"`
	Reachable     int  `json:"reachable"`
	Refused       int  `json:"refused"`
	Unknown       int  `json:"unknown"`
	NotApplicable int  `json:"notApplicable"`
	Complete      bool `json:"complete"`
}

type discoveryStatus struct {
	Source   string `json:"source"`
	Complete bool   `json:"complete"`
}

// clustersResult is the AccessClusters payload.
type clustersResult struct {
	Subject           AccessSubject     `json:"subject"`
	Identity          Identity          `json:"identity"`
	Discovery         discoveryStatus   `json:"discovery"`
	BindingsDiscovery bindingsDiscovery `json:"bindingsDiscovery"`
	GroupAliases      groupAliases      `json:"groupAliases"`
	Clusters          []clusterRow      `json:"clusters"`
	Reachable         []reachableEntry  `json:"reachable"`
	Rollup            clustersRollup    `json:"rollup"`
	ResultSource      ResultSource      `json:"resultSource"`
}

// probeOutcome is the classified answer of one cluster's SelfSubjectReview.
type probeOutcome struct {
	access   string
	source   RowSource
	issue    *Issue
	identity *string
	note     string
	terminal error
}

// classifyProbe applies the probe classifier to one cluster's review.
// The subject's Kubernetes username is what a successful review must
// report; a review answering as someone else means the proxy ignored the
// impersonation and the run cannot answer for the subject anywhere.
func classifyProbe(clusterID string, subjectUsername string, who callerIdentity, r reviewResult, evaluated, total int) probeOutcome {
	switch r.kind {
	case outcomeAnswer:
		if who.Username != subjectUsername {
			return probeOutcome{terminal: clierr.New(clierr.CodeRBAC, fmt.Sprintf(
				"the proxy for cluster %s answered as %s, not %s; impersonation was not applied (evaluated %d of %d clusters)",
				clusterID, who.Username, subjectUsername, evaluated, total))}
		}
		name := who.Username
		return probeOutcome{access: reachable, source: RowRancherProxy, identity: &name}
	case outcomeNoClusterAccess:
		return probeOutcome{access: refused, source: RowRancherProxy,
			note: fmt.Sprintf("%s has no access to cluster %s; %s", subjectUsername, clusterID, noClusterAccessDetail)}
	case outcomeDegradable:
		iss := issueFrom(r.err)
		return probeOutcome{access: unknown, source: RowRequest, issue: &iss}
	default:
		return probeOutcome{terminal: clierr.Wrap(r.err, fmt.Sprintf("%s (evaluated %d of %d clusters)", r.err.Message, evaluated, total)).WithRetryable(r.err.Retryable)}
	}
}

func (c *clustersCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	as, err := ParseSubject(in.String(flagAs), in.String(flagType))
	if err != nil {
		return runtime.Result{}, err
	}
	switch as.Kind {
	case KindGroup:
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "no server operation lists a group's clusters; "+pointQuestionPlaceholders+"; ask a point question with: "+groupAccessCan(as.Raw))
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
	// The caller's own identity on the management cluster. It is
	// also the local row's answer in caller mode.
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

	// Identity: the caller, or a named user resolved once (identity only;
	// group aliases are a GRANTED BY input).
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

	rows, err := c.probe(ctx, clusters, subject, identity, caller)
	if err != nil {
		return runtime.Result{}, err
	}

	prov, err := readProvenance(ctx, local, steve.NewUserAttributes(steve.RootFactoryGetter(c.roots), local), pin)
	if err != nil {
		return runtime.Result{}, err
	}
	out := clustersResult{
		Subject: prov.Subject, Identity: identity,
		Discovery:         discoveryStatus{Source: "rancher", Complete: true},
		BindingsDiscovery: prov.Discovery, GroupAliases: prov.GroupAliases,
		Clusters: rows, Reachable: []reachableEntry{},
	}
	for i := range out.Clusters {
		row := &out.Clusters[i]
		row.Bindings = bindingsFor(prov.Bindings, row.Cluster)
		out.Rollup.Clusters++
		switch row.Access {
		case reachable:
			out.Rollup.Reachable++
			out.Reachable = append(out.Reachable, reachableEntry{Cluster: row.Cluster, Grants: grantsFor(row.Bindings, prov.Discovery)})
		case refused:
			out.Rollup.Refused++
		case unknown:
			out.Rollup.Unknown++
		case notApplicable:
			out.Rollup.NotApplicable++
		}
	}
	out.Rollup.Complete = out.Rollup.Unknown == 0
	out.ResultSource = clustersResultSource(out.Clusters)
	exit := 0
	if !out.Rollup.Complete {
		exit = exitUnknown
	}
	return runtime.Result{Kind: "AccessClusters", Data: out, ExitStatus: exit,
		Text: clustersText(out, false), WideText: clustersText(out, true)}, nil
}

// probe runs the per-cluster SelfSubjectReview under the fan-out limit and
// returns one row per cluster in cluster order. In caller mode the local
// row reuses the management identity review; for a named user the local
// row is not applicable.
func (c *clustersCmd) probe(parent context.Context, clusters []discoveredCluster, subject AccessSubject, identity Identity, caller callerIdentity) ([]clusterRow, error) {
	// The probes run under their own cancellable context: the first
	// terminal outcome cancels every sibling, so a review that would
	// otherwise hang until the budget expires cannot turn a definitive
	// answer (a refused impersonation, a proxy answering as someone else)
	// into a budget failure.
	ctx, cancelProbes := context.WithCancel(parent)
	defer cancelProbes()
	rows := make([]clusterRow, len(clusters))
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, max(1, min(fanOutLimit, len(clusters))))
		terminal  error
		evaluated int
	)
	// The first pass settles the rows no review answers and the counts the
	// goroutines read: total is the number of clusters a review answers for
	// (every cluster in caller mode, local by the identity review already
	// taken; every cluster but local for a named user).
	total := len(clusters)
	var probes []int
	for i, cl := range clusters {
		rows[i] = clusterRow{Cluster: cl.ID, DisplayName: cl.DisplayName, Returned: cl.Returned, Bindings: []bindingRecord{}}
		if cl.ID == engine.LocalCluster {
			if identity == IdentityCaller {
				name := caller.Username
				rows[i].Access, rows[i].Source, rows[i].Identity = reachable, RowRancherProxy, &name
				evaluated++
			} else {
				rows[i].Access, rows[i].Source, rows[i].Note = notApplicable, RowNotApplicable, localCannotImpersonateNote
				total--
			}
			continue
		}
		probes = append(probes, i)
	}
	for _, i := range probes {
		id := clusters[i].ID
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			mu.Lock()
			stop := terminal != nil
			mu.Unlock()
			if stop {
				return
			}
			var (
				client engine.ClusterClient
				err    error
			)
			if identity == IdentityCaller {
				client, err = c.clients.Cluster(ctx, id)
			} else {
				client, err = c.clients.ImpersonatedCluster(ctx, id, engine.Impersonation{User: subject.Name})
			}
			if err != nil {
				mu.Lock()
				if terminal == nil {
					terminal = budgetError(parent, err, "the client for cluster "+id)
					cancelProbes()
				}
				mu.Unlock()
				return
			}
			who, r := whoami(ctx, client)
			mu.Lock()
			defer mu.Unlock()
			if terminal != nil {
				return
			}
			out := classifyProbe(id, subject.Name, who, r, evaluated, total)
			if out.terminal != nil {
				terminal = out.terminal
				if r.kind != outcomeAnswer {
					// A probe the budget cut short is the budget's
					// message, not the transport's.
					terminal = budgetErrorWithProgress(parent, out.terminal, "the cluster probes", fmt.Sprintf("evaluated %d of %d clusters", evaluated, total))
				}
				cancelProbes()
				return
			}
			evaluated++
			rows[i].Access, rows[i].Source, rows[i].Issue, rows[i].Identity, rows[i].Note = out.access, out.source, out.issue, out.identity, out.note
		})
	}
	wg.Wait()
	// A recorded terminal answer is the run's answer, whatever cancelled
	// the siblings afterwards; only a run without one reports the caller's
	// cancellation or the budget.
	if terminal != nil {
		return nil, terminal
	}
	if err := parent.Err(); err != nil {
		progress := fmt.Sprintf("evaluated %d of %d clusters", evaluated, total)
		return nil, budgetErrorWithProgress(parent, clierr.New(clierr.CodeGeneric, "cancelled; "+progress), "the cluster probes", progress)
	}
	return rows, nil
}

// clustersResultSource is the canonical rollup over the rows whose source
// is not notApplicable.
func clustersResultSource(rows []clusterRow) ResultSource {
	seen := map[RowSource]bool{}
	for _, r := range rows {
		if r.Source != RowNotApplicable {
			seen[r.Source] = true
		}
	}
	switch len(seen) {
	case 0:
		return ResultSource(RowNotApplicable)
	case 1:
		for s := range seen {
			return ResultSource(s)
		}
	}
	return SourceMixed
}

// grantedByCell renders a cluster's bindings for the table.
func grantedByCell(records []bindingRecord, discovery bindingsDiscovery) string {
	if len(records) == 0 {
		if discovery.Status == "complete" {
			return "-"
		}
		return "?"
	}
	parts := make([]string, 0, len(records))
	for _, b := range records {
		switch b.Kind {
		case bindingCRTB:
			parts = append(parts, fmt.Sprintf("crtb/%s (%s)", b.Name, b.RoleTemplate))
		case bindingPRTB:
			p := ""
			if b.Project != nil {
				p = ", project " + b.Project.Project
			}
			parts = append(parts, fmt.Sprintf("prtb/%s (%s%s)", b.Name, b.RoleTemplate, p))
		case bindingGRB:
			parts = append(parts, fmt.Sprintf("grb/%s (%s)", b.Name, b.GlobalRole))
		}
	}
	return strings.Join(parts, "; ")
}

// clustersText renders the default and the wide view.
func clustersText(out clustersResult, wide bool) string {
	who := output.Printable(out.Subject.Name)
	if out.Identity == IdentityCaller {
		who = "you"
	} else if out.Subject.DisplayName != "" {
		who = fmt.Sprintf("%s (%s)", output.Printable(out.Subject.DisplayName), who)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "clusters you can list, reachability for %s\n\n", who)
	header := []string{"CLUSTER", "NAME", "REACHABILITY", "GRANTED BY", "NOTE"}
	if wide {
		header = []string{"CLUSTER", "NAME", "REACHABILITY", "IDENTITY", "GRANTED BY", "PROJECTS", "NOTE"}
	}
	rows := make([][]string, 0, len(out.Clusters))
	for _, r := range out.Clusters {
		note := r.Note
		if r.Issue != nil {
			note = r.Issue.Message
			if r.Issue.Retryable {
				note += " (retryable)"
			}
		}
		access := r.Access
		row := []string{r.Cluster, r.DisplayName, access}
		if wide {
			id := "-"
			if r.Identity != nil {
				id = *r.Identity
			}
			row = append(row, id)
		}
		row = append(row, grantedByCell(r.Bindings, out.BindingsDiscovery))
		if wide {
			var projects []string
			for _, bd := range r.Bindings {
				if bd.Kind == bindingPRTB && bd.Project != nil {
					projects = append(projects, bd.Project.Project)
				}
			}
			slices.Sort(projects)
			row = append(row, cmp.Or(strings.Join(slices.Compact(projects), ", "), "-"))
		}
		row = append(row, note)
		rows = append(rows, row)
	}
	writeTable(&b, "", header, rows)
	b.WriteString("\n")
	if len(out.Reachable) == 0 {
		b.WriteString("reachable: none\n")
	} else {
		var names []string
		for _, e := range out.Reachable {
			s := output.Printable(e.Cluster)
			if e.Grants != nil {
				var gs []string
				for _, g := range e.Grants {
					gs = append(gs, output.Printable(g.Name))
				}
				if len(gs) > 0 {
					s += " (" + strings.Join(gs, ", ") + ")"
				}
			}
			names = append(names, s)
		}
		fmt.Fprintf(&b, "reachable: %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "%s\n", reachabilitySentence)
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
	fmt.Fprintf(&b, "%d clusters: %d reachable, %d refused, %d unknown", out.Rollup.Clusters, out.Rollup.Reachable, out.Rollup.Refused, out.Rollup.Unknown)
	if out.Rollup.NotApplicable > 0 {
		fmt.Fprintf(&b, ", %d not applicable", out.Rollup.NotApplicable)
	}
	fmt.Fprintf(&b, "\nsource: %s | identity: %s\n", out.ResultSource, out.Identity)
	return b.String()
}
