package access

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
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
			clusterFlag("cluster name or id"),
			{Name: "namespace", Type: "string", Aliases: []string{"n"}, Desc: "namespace to list rules in (default: default)"},
			runtime.ProjectFlag(false, "ask about the project's namespaces"),
			{Name: "all-namespaces", Type: "bool", Aliases: []string{"A"}, Desc: "list rules in every namespace you can list"},
			{Name: "as", Type: "string", Desc: "user name, id, or principal, or system:serviceaccount:<ns>:<name>; omitted = you"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "kind of a bare --as value when ambiguous (group is rejected here)"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access list --cluster prod -n team-a",
			"rancher access list --cluster prod --project p-cvrp4 --as alice",
			"rancher access list --cluster prod -A -o json",
		},
		CommandType:      runtime.ReadOnly,
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
	flags := scopeFlags{namespace: in.String(flagNamespace), project: in.String(flagProject), allNamespaces: in.Bool(flagAllNamespaces), allowAll: true, takesProject: true}
	if err := validateSelectors(flags); err != nil {
		return runtime.Result{}, err
	}
	implicitDefault := flags.selectors() == 0
	if implicitDefault {
		flags.namespace = defaultNamespace
	}
	budget, err := parseTimeout(in)
	if err != nil {
		return runtime.Result{}, err
	}
	ctx, cancel := fanOutContext(ctx, budget)
	defer cancel()
	// Before scope: the caller's own access to the cluster.
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, true)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "cluster resolution")
	}
	callerClient, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the cluster client")
	}
	caller, err := callerPreflight(ctx, callerClient, clusterID)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the caller's identity review")
	}
	project, err := typedProject(ctx, in, clusterID)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "project resolution")
	}
	scope, err := resolveScope(ctx, callerClient, clusterID, flags, project)
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
		return runtime.Result{}, budgetError(ctx, err, "the management cluster client")
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

// resourceRow is one text row: a resource of one API group (or one
// non-resource URL, in Resource) with the verbs the rules grant on it, and
// the resource names the grant is limited to, comma-joined, when any.
type resourceRow struct {
	Group    string
	Resource string
	Names    string
	Verbs    []string
}

// resourceRows expands canonical rules into one row per API group,
// resource, and resource-name set, sorted by the group as printed (so the
// core group sorts as "core"), then resource, then names. Rules about the
// same group and resource with the same (possibly empty) name set merge
// their verbs: the union is exactly what the authorizer allows for that
// resource. A rule limited to resource names keeps its own row, since its
// verbs apply to those names only.
func resourceRows(rules []Rule) []resourceRow {
	byKey := map[string]*resourceRow{}
	var order []string
	for _, r := range rules {
		names := strings.Join(r.ResourceNames, ",")
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				key := g + "\x00" + res + "\x00" + names
				row := byKey[key]
				if row == nil {
					row = &resourceRow{Group: g, Resource: res, Names: names}
					byKey[key] = row
					order = append(order, key)
				}
				row.Verbs = append(row.Verbs, r.Verbs...)
			}
		}
	}
	out := make([]resourceRow, 0, len(order))
	for _, k := range order {
		row := byKey[k]
		slices.Sort(row.Verbs)
		row.Verbs = slices.Compact(row.Verbs)
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b resourceRow) int {
		return cmp.Or(strings.Compare(groupLabel(a.Group), groupLabel(b.Group)), strings.Compare(a.Resource, b.Resource), strings.Compare(a.Names, b.Names))
	})
	return out
}

// nonResourceRows expands non-resource rules to one row per URL, verbs
// merged per URL, sorted by URL.
func nonResourceRows(rules []Rule) []resourceRow {
	byURL := map[string]*resourceRow{}
	for _, r := range rules {
		for _, u := range r.NonResourceURLs {
			row := byURL[u]
			if row == nil {
				row = &resourceRow{Resource: u}
				byURL[u] = row
			}
			row.Verbs = append(row.Verbs, r.Verbs...)
		}
	}
	out := make([]resourceRow, 0, len(byURL))
	for _, u := range slices.Sorted(maps.Keys(byURL)) {
		row := byURL[u]
		slices.Sort(row.Verbs)
		row.Verbs = slices.Compact(row.Verbs)
		out = append(out, *row)
	}
	return out
}

// groupLabel prints the core API group, whose name is empty, as "core":
// the spelling --group and name.group already accept for it.
func groupLabel(g string) string {
	if g == "" {
		return "core"
	}
	return g
}

// tableLines renders a header and rows with every column fitted to its
// widest cell, two spaces between columns, no trailing spaces, each line
// prefixed with indent. The header is the first line and every later line
// is one row in order, so a caller that writes its own text between rows
// still measures the columns over the whole table.
func tableLines(indent string, header []string, rows [][]string) []string {
	// Cells carry server text (resource names, subject names, display
	// names); each passes the terminal-safety boundary before it is
	// measured or written, so widths are computed on what is printed.
	header = printableAll(header)
	safe := make([][]string, len(rows))
	for i, r := range rows {
		safe[i] = printableAll(r)
	}
	rows = safe
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string) string {
		var l strings.Builder
		l.WriteString(indent)
		for i, c := range cells {
			l.WriteString(c)
			if i < len(cells)-1 {
				l.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2))
			}
		}
		// An empty last cell (a row without NAMES) leaves no trailing pad.
		return strings.TrimRight(l.String(), " ")
	}
	out := make([]string, 0, len(rows)+1)
	out = append(out, line(header))
	for _, r := range rows {
		out = append(out, line(r))
	}
	return out
}

// printableAll returns cells with every entry passed through
// output.Printable, leaving the input untouched.
func printableAll(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = output.Printable(c)
	}
	return out
}

// writeTable writes the fitted table as one block.
func writeTable(b *strings.Builder, indent string, header []string, rows [][]string) {
	for _, l := range tableLines(indent, header, rows) {
		b.WriteString(l)
		b.WriteString("\n")
	}
}

// namespaceRows is one namespace's expanded rows, the unit the table
// writers take so the wide view can list several namespaces in one table.
type namespaceRows struct {
	namespace string
	rows      []resourceRow
}

// resourceTable writes the resource rows of one or several namespaces;
// namespaced, when set, prefixes a NAMESPACE column. The NAMES column
// appears only when some row carries resource names.
func resourceTable(b *strings.Builder, indent string, groups []namespaceRows, namespaced bool) {
	hasNames := false
	for _, g := range groups {
		for _, r := range g.rows {
			if r.Names != "" {
				hasNames = true
			}
		}
	}
	header := []string{"API GROUP", "RESOURCE", "VERBS"}
	if hasNames {
		header = append(header, "NAMES")
	}
	if namespaced {
		header = append([]string{"NAMESPACE"}, header...)
	}
	var rows [][]string
	for _, g := range groups {
		for _, r := range g.rows {
			cells := []string{groupLabel(r.Group), r.Resource, strings.Join(r.Verbs, ",")}
			if hasNames {
				cells = append(cells, r.Names)
			}
			if namespaced {
				cells = append([]string{g.namespace}, cells...)
			}
			rows = append(rows, cells)
		}
	}
	writeTable(b, indent, header, rows)
}

// nonResourceTable is resourceTable's twin for non-resource URLs.
func nonResourceTable(b *strings.Builder, indent string, groups []namespaceRows, namespaced bool) {
	header := []string{"NON-RESOURCE URL", "VERBS"}
	if namespaced {
		header = append([]string{"NAMESPACE"}, header...)
	}
	var rows [][]string
	for _, g := range groups {
		for _, r := range g.rows {
			cells := []string{r.Resource, strings.Join(r.Verbs, ",")}
			if namespaced {
				cells = append([]string{g.namespace}, cells...)
			}
			rows = append(rows, cells)
		}
	}
	writeTable(b, indent, header, rows)
}

// listText renders one block per distinct rule set; namespaces with an
// identical set are listed once under "also in". Each block is one row
// per API group and resource, then the non-resource URLs when any.
func listText(out listResult, implicitDefault bool) string {
	// The subject name, the note, namespace names, and issue messages are
	// the server's text: each passes the terminal-safety boundary at
	// insertion, as the cells do inside tableLines.
	who := output.Printable(out.Subject.Name)
	if out.Identity == IdentityCaller {
		who = "you"
	}
	var b strings.Builder
	if out.Note != "" {
		fmt.Fprintf(&b, "note: %s\n\n", output.Printable(out.Note))
	}
	printed := map[string]bool{}
	for i, ns := range out.Namespaces {
		if !ns.Evaluated {
			fmt.Fprintf(&b, "namespace %s: not evaluated (%s: %s)\n\n", output.Printable(ns.Namespace), ns.Issue.Code, output.Printable(ns.Issue.Message))
			continue
		}
		key := listGroupKey(ns)
		if printed[key] {
			continue
		}
		printed[key] = true
		var also []string
		for _, other := range out.Namespaces[i+1:] {
			if other.Evaluated && listGroupKey(other) == key {
				also = append(also, other.Namespace)
			}
		}
		fmt.Fprintf(&b, "rules for %s in namespace %s on %s", who, output.Printable(ns.Namespace), output.Printable(out.Scope.Cluster))
		if implicitDefault {
			b.WriteString(", including every cluster-wide rule")
		}
		if len(also) > 0 {
			fmt.Fprintf(&b, " (also in: %s)", output.Printable(strings.Join(also, ", ")))
		}
		b.WriteString("\n")
		if ns.Incomplete {
			fmt.Fprintf(&b, "  incomplete: the server could not enumerate every rule (%s)\n", output.Printable(ns.EvaluationError))
		}
		resourceTable(&b, "  ", []namespaceRows{{namespace: ns.Namespace, rows: resourceRows(ns.ResourceRules)}}, false)
		if len(ns.NonResourceRules) > 0 {
			nonResourceTable(&b, "  ", []namespaceRows{{namespace: ns.Namespace, rows: nonResourceRows(ns.NonResourceRules)}}, false)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "source: %s | identity: %s\n", out.ResultSource, out.Identity)
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

// listWideText is the -o wide projection: one row per namespace, API
// group, and resource, never grouped, a NAMESPACE column first, so every
// row is attributable.
func listWideText(out listResult) string {
	var b strings.Builder
	if out.Note != "" {
		fmt.Fprintf(&b, "note: %s\n\n", output.Printable(out.Note))
	}
	var resources, nonResources []namespaceRows
	for _, ns := range out.Namespaces {
		switch {
		case !ns.Evaluated:
			fmt.Fprintf(&b, "namespace %s: not evaluated (%s: %s)\n", output.Printable(ns.Namespace), ns.Issue.Code, output.Printable(ns.Issue.Message))
			continue
		case ns.Incomplete:
			fmt.Fprintf(&b, "namespace %s: incomplete, the server could not enumerate every rule (%s)\n", output.Printable(ns.Namespace), output.Printable(ns.EvaluationError))
		}
		resources = append(resources, namespaceRows{namespace: ns.Namespace, rows: resourceRows(ns.ResourceRules)})
		if len(ns.NonResourceRules) > 0 {
			nonResources = append(nonResources, namespaceRows{namespace: ns.Namespace, rows: nonResourceRows(ns.NonResourceRules)})
		}
	}
	resourceTable(&b, "", resources, true)
	if len(nonResources) > 0 {
		b.WriteString("\n")
		nonResourceTable(&b, "", nonResources, true)
	}
	fmt.Fprintf(&b, "\nsource: %s | identity: %s\n", out.ResultSource, out.Identity)
	return b.String()
}
