package access

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type listCmd struct {
	clients engine.ClusterClientFactory
	local   engine.ClusterClient
}

// NewList builds `access list`; see NewCan for the local argument.
func NewList(clients engine.ClusterClientFactory, local engine.ClusterClient) runtime.Command {
	return &listCmd{clients: clients, local: local}
}

func (c *listCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access list",
		Usage: "List the rules a subject holds in a namespace, as the cluster's authorizer reports them",
		Desc: "Posts a SelfSubjectRulesReview per namespace, as you or as an impersonated --as subject, and renders the " +
			"server's rules. Groups cannot be listed (no server operation exists). Exit 0 when every namespace answered " +
			"completely, 13 otherwise.",
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster name or id",
				MissingMessage: engine.ClusterRequiredMessage, MissingHint: engine.ClusterRequiredHint},
			{Name: "namespace", Type: "string", Aliases: []string{"n"}, Desc: "namespace to list rules in (default: default)"},
			{Name: "all-namespaces", Type: "bool", Aliases: []string{"A"}, Desc: "list rules in every namespace you can list"},
			{Name: "as", Type: "string", Desc: "user name, id, or principal, or system:serviceaccount:<ns>:<name>; omitted = you"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "kind of a bare --as value when ambiguous (group is rejected here)"},
		},
		Examples: []string{
			"rancher access list --cluster prod -n team-a",
			"rancher access list --cluster prod --project p-cvrp4 --as alice",
			"rancher access list --cluster prod -A -o json",
		},
		CommandType:      runtime.ReadOnly,
		Network:          true,
		Auth:             true,
		SuccessExitCodes: []int{0, 13},
		Shape:            runtime.ShapeText,
	}
}

// Rule is one authorizer rule in canonical form: every slice sorted and
// deduplicated.
type Rule struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

type listNamespace struct {
	Namespace        string       `json:"namespace"`
	Source           MemberSource `json:"source"`
	Evaluated        bool         `json:"evaluated"`
	Issue            *Issue       `json:"issue"`
	Incomplete       bool         `json:"incomplete"`
	EvaluationError  string       `json:"evaluationError"`
	ResourceRules    []Rule       `json:"resourceRules"`
	NonResourceRules []Rule       `json:"nonResourceRules"`
}

type listRollup struct {
	Namespaces int  `json:"namespaces"`
	Evaluated  int  `json:"evaluated"`
	Incomplete int  `json:"incomplete"`
	Issues     int  `json:"issues"`
	Complete   bool `json:"complete"`
}

type listResult struct {
	Subject      AccessSubject   `json:"subject"`
	Scope        Scope           `json:"scope"`
	Note         string          `json:"note"`
	Namespaces   []listNamespace `json:"namespaces"`
	Rollup       listRollup      `json:"rollup"`
	ResultSource ResultSource    `json:"resultSource"`
	Identity     Identity        `json:"identity"`
}

const defaultNamespace = "default"

func (c *listCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	as, err := ParseSubject(in.String(flagAs), in.String(flagType))
	if err != nil {
		return runtime.Result{}, err
	}
	if as.Kind == KindGroup {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, groupListMessage(as.Raw))
	}
	if as.Raw == anonymousUser {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, anonymousListMessage)
	}
	flags := scopeFlags{namespace: in.String(flagNamespace), project: in.String(flagProject), allNamespaces: in.Bool(flagAllNamespaces), allowAll: true}
	if err := validateSelectors(flags); err != nil {
		return runtime.Result{}, err
	}
	implicitDefault := flags.selectors() == 0
	if implicitDefault {
		flags.namespace = defaultNamespace
	}
	if flags.project != "" || flags.allNamespaces {
		var cancel context.CancelFunc
		ctx, cancel = fanOutContext(ctx)
		defer cancel()
	}
	// Step 0 before scope: the caller's own access to the cluster.
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	callerClient, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	caller, err := callerPreflight(ctx, callerClient, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	if flags.project != "" {
		id, err := projects.ResolveProject(ctx, c.clients, clusterID, flags.project)
		if err != nil {
			return runtime.Result{}, budgetError(ctx, err, "project resolution")
		}
		flags.project = id
	}
	scope, err := resolveScope(ctx, callerClient, clusterID, flags)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "namespace discovery")
	}
	if implicitDefault {
		// The implicit default namespace is a target, not wire scope: the
		// row names it.
		scope.Namespace = ""
	}
	local, err := localClient(ctx, c.clients, c.local)
	if err != nil {
		return runtime.Result{}, err
	}
	plan, err := planIdentity(ctx, c.clients, callerClient, local, clusterID, caller, as, true)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "subject resolution")
	}
	out := listResult{Subject: plan.Subject, Scope: scope, Identity: plan.Identity, Namespaces: []listNamespace{}}
	if scope.mode == scopeProject && len(scope.targets) == 0 {
		// A verified-empty project is a complete, empty listing: nothing
		// to evaluate, nothing missing.
		out.Note = fmt.Sprintf("project %s has no namespaces on cluster %s", scope.Project, clusterID)
		out.ResultSource = SourceScopeDiscovery
		out.Rollup = listRollup{Complete: true}
		return runtime.Result{Kind: "AccessRules", Data: out, ExitStatus: 0, Text: listText(out, implicitDefault), WideText: listWideText(out)}, nil
	}
	if plan.Denied != nil {
		// The proxy refused the subject before any rules review: report
		// the fact, evaluate nothing, exit 13.
		out.Note = plan.Denied.Note
		out.ResultSource = SourceRancherProxy
		out.Rollup = listRollup{Namespaces: len(scope.targets)}
		return runtime.Result{Kind: "AccessRules", Data: out, ExitStatus: exitUnknown, Text: listText(out, implicitDefault), WideText: listWideText(out)}, nil
	}
	ask := func(ctx context.Context, ns string) (listNamespace, reviewResult) {
		r := postReview(ctx, plan.Client, engine.ReviewSelfSubjectRules, rulesReviewObject(ns))
		if r.kind != outcomeAnswer {
			return listNamespace{}, r
		}
		return rulesFrom(ns, r.resp.Object), r
	}
	members, failure := runFanOut(ctx, scope.targets, ask)
	if failure != nil {
		return runtime.Result{}, failure.Err
	}
	for _, m := range members {
		row := m.Value
		row.Namespace, row.Source, row.Issue = m.Key, m.Source, m.Issue
		row.Evaluated = m.Source == MemberAuthorizer
		if row.ResourceRules == nil {
			row.ResourceRules = []Rule{}
		}
		if row.NonResourceRules == nil {
			row.NonResourceRules = []Rule{}
		}
		out.Rollup.Namespaces++
		if row.Evaluated {
			out.Rollup.Evaluated++
		}
		if row.Incomplete {
			out.Rollup.Incomplete++
		}
		if row.Issue != nil {
			out.Rollup.Issues++
		}
		out.Namespaces = append(out.Namespaces, row)
	}
	out.Rollup.Complete = out.Rollup.Evaluated == out.Rollup.Namespaces && out.Rollup.Incomplete == 0 && out.Rollup.Issues == 0
	out.ResultSource = deriveSource(members)
	exit := 0
	if !out.Rollup.Complete {
		exit = exitUnknown
	}
	return runtime.Result{Kind: "AccessRules", Data: out, ExitStatus: exit, Text: listText(out, implicitDefault), WideText: listWideText(out)}, nil
}

// rulesFrom canonicalizes one SelfSubjectRulesReview status.
func rulesFrom(ns string, obj *unstructured.Unstructured) listNamespace {
	row := listNamespace{Namespace: ns}
	row.Incomplete, _, _ = unstructured.NestedBool(obj.Object, "status", "incomplete")
	row.EvaluationError, _, _ = unstructured.NestedString(obj.Object, "status", "evaluationError")
	if raw, ok, _ := unstructured.NestedSlice(obj.Object, "status", "resourceRules"); ok {
		row.ResourceRules = canonicalRules(raw)
	}
	if raw, ok, _ := unstructured.NestedSlice(obj.Object, "status", "nonResourceRules"); ok {
		row.NonResourceRules = canonicalRules(raw)
	}
	return row
}

func canonicalRules(raw []any) []Rule {
	seen := map[string]bool{}
	var out []Rule
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		r := Rule{
			Verbs: sortedStrings(m["verbs"]), APIGroups: sortedStrings(m["apiGroups"]), Resources: sortedStrings(m["resources"]),
			ResourceNames: sortedStrings(m["resourceNames"]), NonResourceURLs: sortedStrings(m["nonResourceURLs"]),
		}
		key := r.canonical()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Rule) int { return strings.Compare(a.canonical(), b.canonical()) })
	return out
}

func sortedStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (r Rule) canonical() string {
	return strings.Join([]string{strings.Join(r.APIGroups, ","), strings.Join(r.Resources, ","), strings.Join(r.ResourceNames, ","),
		strings.Join(r.NonResourceURLs, ","), strings.Join(r.Verbs, ",")}, "|")
}

// listText renders one table per distinct rule set; namespaces with an
// identical set are listed once under "also in".
func listText(out listResult, implicitDefault bool) string {
	who := out.Subject.Name
	if out.Identity == IdentityCaller {
		who = "you"
	}
	var b strings.Builder
	if out.Note != "" {
		fmt.Fprintf(&b, "note: %s\n\n", out.Note)
	}
	printed := map[string]int{} // canonical set -> index into out.Namespaces
	for i, ns := range out.Namespaces {
		if !ns.Evaluated {
			fmt.Fprintf(&b, "namespace %s: not evaluated (%s: %s)\n\n", ns.Namespace, ns.Issue.Code, ns.Issue.Message)
			continue
		}
		key := listGroupKey(ns)
		if _, ok := printed[key]; ok {
			continue
		}
		printed[key] = i
		var also []string
		for _, other := range out.Namespaces[i+1:] {
			if other.Evaluated && listGroupKey(other) == key {
				also = append(also, other.Namespace)
			}
		}
		fmt.Fprintf(&b, "rules for %s in namespace %s on %s", who, ns.Namespace, out.Scope.Cluster)
		if implicitDefault {
			b.WriteString(", including every cluster-wide rule")
		}
		if len(also) > 0 {
			fmt.Fprintf(&b, " (also in: %s)", strings.Join(also, ", "))
		}
		b.WriteString("\n")
		if ns.Incomplete {
			fmt.Fprintf(&b, "  incomplete: the server could not enumerate every rule (%s)\n", ns.EvaluationError)
		}
		fmt.Fprintf(&b, "  %-40s %-40s %-24s %s\n", "RESOURCES", "VERBS", "API GROUP", "NAMES")
		for _, r := range ns.ResourceRules {
			fmt.Fprintf(&b, "  %-40s %-40s %-24s %s\n", strings.Join(r.Resources, ","), strings.Join(r.Verbs, ","), strings.Join(r.APIGroups, ","), strings.Join(r.ResourceNames, ","))
		}
		if len(ns.NonResourceRules) > 0 {
			fmt.Fprintf(&b, "  %-40s %s\n", "NON-RESOURCE URLS", "VERBS")
			for _, r := range ns.NonResourceRules {
				fmt.Fprintf(&b, "  %-40s %s\n", strings.Join(r.NonResourceURLs, ","), strings.Join(r.Verbs, ","))
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "source: %s | identity: %s", out.ResultSource, out.Identity)
	return b.String()
}

// listGroupKey identifies rows the text view may fold into one block:
// the same canonical rules AND the same completeness state. A namespace
// answered incomplete, or with an evaluation error, never hides under
// another namespace's "also in".
func listGroupKey(ns listNamespace) string {
	key := struct {
		Incomplete       bool
		EvaluationError  string
		ResourceRules    []Rule
		NonResourceRules []Rule
	}{ns.Incomplete, ns.EvaluationError, ns.ResourceRules, ns.NonResourceRules}
	b, err := json.Marshal(key)
	if err != nil {
		// Rule holds strings and string slices only; marshaling cannot fail.
		panic("list grouping key is not JSON-safe: " + err.Error())
	}
	return string(b)
}

// listWideText is the -o wide projection: one row per namespace, never
// grouped, a NAMESPACE column first, so every rule is attributable.
func listWideText(out listResult) string {
	var b strings.Builder
	if out.Note != "" {
		fmt.Fprintf(&b, "note: %s\n\n", out.Note)
	}
	for _, ns := range out.Namespaces {
		if !ns.Evaluated {
			fmt.Fprintf(&b, "namespace %s: not evaluated (%s: %s)\n", ns.Namespace, ns.Issue.Code, ns.Issue.Message)
		} else if ns.Incomplete {
			fmt.Fprintf(&b, "namespace %s: incomplete, the server could not enumerate every rule (%s)\n", ns.Namespace, ns.EvaluationError)
		}
	}
	fmt.Fprintf(&b, "%-24s %-40s %-40s %-24s %s\n", "NAMESPACE", "RESOURCES", "VERBS", "API GROUP", "NAMES")
	for _, ns := range out.Namespaces {
		for _, r := range ns.ResourceRules {
			fmt.Fprintf(&b, "%-24s %-40s %-40s %-24s %s\n", ns.Namespace, strings.Join(r.Resources, ","), strings.Join(r.Verbs, ","), strings.Join(r.APIGroups, ","), strings.Join(r.ResourceNames, ","))
		}
	}
	if slices.ContainsFunc(out.Namespaces, func(ns listNamespace) bool { return len(ns.NonResourceRules) > 0 }) {
		fmt.Fprintf(&b, "\n%-24s %-40s %s\n", "NAMESPACE", "NON-RESOURCE URLS", "VERBS")
		for _, ns := range out.Namespaces {
			for _, r := range ns.NonResourceRules {
				fmt.Fprintf(&b, "%-24s %-40s %s\n", ns.Namespace, strings.Join(r.NonResourceURLs, ","), strings.Join(r.Verbs, ","))
			}
		}
	}
	fmt.Fprintf(&b, "\nsource: %s | identity: %s", out.ResultSource, out.Identity)
	return b.String()
}
