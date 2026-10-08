package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

const flagShowDenied = "show-denied"

// fixedMastersNote explains the candidate included independently of
// bindings.
const fixedMastersNote = "fixed candidate: the apiserver allows system:masters before RBAC is consulted, so its answer carries no reason and its members are not visible to the CLI"

type whoCanCmd struct {
	clients engine.ClusterClientFactory
	local   engine.ClusterClient
}

// NewWhoCan builds `access who-can`; see NewCan for the local argument.
func NewWhoCan(clients engine.ClusterClientFactory, local engine.ClusterClient) runtime.Command {
	return &whoCanCmd{clients: clients, local: local}
}

func (c *whoCanCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access who-can",
		Usage: "List the subjects the cluster's authorizer allows to perform a verb on a resource",
		Desc: "Reads the subjects named in the cluster's RoleBindings and ClusterRoleBindings, adds system:masters, and posts one " +
			"SubjectAccessReview per subject as you (per namespace with --project or --all-namespaces). Every row is the server's " +
			"decision; a subject whose groups Rancher does not know is unknown when denied, never denied. Exit 13 when a row has a " +
			"request issue or is unknown because the authorizer returned an evaluation error; an allowed answer with an evaluation " +
			"error stays allowed and warns.",
		Args: []runtime.ArgSpec{
			{Name: "verb", Type: "string", Required: true, Desc: "Kubernetes verb (get, list, create, ...) or, with --non-resource-url, an HTTP verb"},
			{Name: "resource", Type: "string", Desc: "resource plural, singular, short name, or resource.group; optionally /<name>; omitted with --non-resource-url"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster name or id the question is about",
				MissingMessage: engine.ClusterRequiredMessage, MissingHint: engine.ClusterRequiredHint},
			{Name: "namespace", Type: "string", Aliases: []string{"n"}, Desc: "namespace the question is about"},
			runtime.ProjectFlag(false, "ask about the project's namespaces"),
			{Name: "all-namespaces", Type: "bool", Aliases: []string{"A"}, Desc: "ask in every namespace you can list, one row per subject per namespace"},
			{Name: "group", Type: "string", Desc: "API group when the resource name exists in several groups"},
			{Name: "subresource", Type: "string", Desc: "subresource such as status or log"},
			{Name: "non-resource-url", Type: "string", Desc: "ask about a non-resource path such as /healthz instead of a resource"},
			{Name: "show-denied", Type: "bool", Desc: "list the denied subjects in the text output too"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access who-can delete pods --cluster prod -n team-a",
			"rancher access who-can get secrets --cluster prod --project p-cvrp4 -o json",
			"rancher access who-can get nodes --cluster prod --show-denied",
			"rancher access who-can post --non-resource-url /healthz --cluster prod",
		},
		CommandType:      runtime.ReadOnly,
		Auth:             true,
		SuccessExitCodes: []int{0, 13},
		Shape:            runtime.ShapeText,
	}
}

// subjectRow is one candidate's decision, in one namespace when the scope
// has several. Name is the binding's subject name byte for byte; ID and
// DisplayName are set for a Rancher user. Via marks how a binding-derived
// subject is reachable ("direct", "group <g>" for a cached member of a
// bound group); KnownMembers lists the users Rancher has cached in a group.
type subjectRow struct {
	Kind            SubjectKind  `json:"kind"`
	Name            string       `json:"name"`
	ID              string       `json:"id,omitempty"`
	DisplayName     string       `json:"displayName,omitempty"`
	Namespace       string       `json:"namespace,omitempty"`
	Source          MemberSource `json:"source"`
	Decision        Decision     `json:"decision"`
	GroupsFrom      GroupsFrom   `json:"groupsFrom,omitempty"`
	Reason          string       `json:"reason"`
	Note            string       `json:"note,omitempty"`
	EvaluationError string       `json:"evaluationError,omitempty"`
	Via             []string     `json:"via,omitempty"`
	KnownMembers    []string     `json:"knownMembers,omitempty"`
	Issue           *Issue       `json:"issue,omitempty"`
}

// subjectsRollup counts every subject x namespace pair the fan-out
// produced (before any collapse). GroupsUnknown is the subset of Unknown
// whose only cause is a subject with unknown groups.
type subjectsRollup struct {
	Candidates          int      `json:"candidates"`
	Rows                int      `json:"rows"`
	Evaluated           int      `json:"evaluated"`
	Allowed             int      `json:"allowed"`
	Denied              int      `json:"denied"`
	Unknown             int      `json:"unknown"`
	GroupsUnknown       int      `json:"groupsUnknown"`
	Issues              int      `json:"issues"`
	OutsideCandidateSet []string `json:"outsideCandidateSet"`
}

type subjectsResult struct {
	Question        Question       `json:"question"`
	Scope           Scope          `json:"scope"`
	CandidateSource string         `json:"candidateSource"`
	Note            string         `json:"note,omitempty"`
	Subjects        []subjectRow   `json:"subjects"`
	Rollup          subjectsRollup `json:"rollup"`
	ResultSource    ResultSource   `json:"resultSource"`
	Identity        Identity       `json:"identity"`
}

// outsideCandidateSet names what no candidate row can show.
func outsideCandidateSet() []string {
	return []string{"system:masters members", "non-RBAC authorizers"}
}

// rowSpec is one fan-out row: a candidate asked in one namespace ("" for
// the cluster scope or all namespaces).
type rowSpec struct {
	cand      int
	namespace string
}

func (c *whoCanCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	p, err := parseQuestion(in, true, true)
	if err != nil {
		return runtime.Result{}, err
	}
	if p.as.Raw != "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "who-can takes no --as: it asks about every subject the bindings name")
	}
	showDenied := in.Bool(flagShowDenied)
	// One budget for the whole operation: candidate discovery, identity
	// reads, group members, reviews.
	budget, err := parseTimeout(in)
	if err != nil {
		return runtime.Result{}, err
	}
	ctx, cancel := fanOutContext(ctx, budget)
	defer cancel()
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "cluster resolution")
	}
	callerClient, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the cluster client")
	}
	// The identity protocol's first check, as in every access command: no
	// subject is named, but a caller without cluster access is refused here
	// for the one reason every command gives, so a refused binding LIST
	// below always means the list right is missing.
	if _, err := callerPreflight(ctx, callerClient, clusterID); err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the caller's identity review")
	}
	var ref *ResourceRef
	var discoveryWarning string
	if p.question.NonResourceURL == "" {
		r, warn, err := ResolveResource(ctx, callerClient, p.resource, in.String(flagGroup))
		if err != nil {
			return runtime.Result{}, budgetError(ctx, err, "resource discovery")
		}
		ref, discoveryWarning = &r, warn
		p.question.Resource, p.question.Group, p.question.Version = r.Resource, r.Group, r.Version
		if p.question.Subresource != "" && !slices.Contains(r.Subresources, p.question.Subresource) {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s has no subresource %s; discovered: %s", r.Resource, p.question.Subresource, strings.Join(r.Subresources, ", ")))
		}
		if err := r.rejectSelectors(p.scope); err != nil {
			return runtime.Result{}, err
		}
	}
	project, err := typedProject(ctx, in, clusterID)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "project resolution")
	}
	scope, err := resolveScope(ctx, callerClient, clusterID, p.scope, project)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "namespace discovery")
	}
	allNamespaces := false
	if ref != nil && ref.Namespaced && scope.mode == scopeCluster {
		if p.name != "" || !isCollectionVerb(p.question.Verb) {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s %s needs a namespace: add -n, --project, or --all-namespaces", p.question.Verb, p.resource))
		}
		allNamespaces = true
	}
	out := subjectsResult{
		Question: p.question, Scope: scope, CandidateSource: candidateSourceFor(clusterID),
		Subjects: []subjectRow{}, Rollup: subjectsRollup{OutsideCandidateSet: outsideCandidateSet()}, Identity: IdentitySAR,
	}
	if scope.mode == scopeProject && len(scope.targets) == 0 {
		// A verified-empty project: no candidate is read and no question
		// is evaluated for anyone.
		out.Note = fmt.Sprintf("project %s has no namespaces on cluster %s", scope.Project, clusterID)
		out.ResultSource = SourceScopeDiscovery
		res := runtime.Result{Kind: "AccessSubjects", Data: out, ExitStatus: 0, Text: whoCanText(out, showDenied, allNamespaces)}
		return withDiscoveryWarning(res, discoveryWarning), nil
	}
	cands, warnings, err := listCandidates(ctx, callerClient, scope)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "candidate discovery")
	}
	if discoveryWarning != "" {
		warnings = append([]string{discoveryWarning}, warnings...)
	}
	local, err := localClient(ctx, c.clients, c.local)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the management cluster client")
	}
	// One identity per distinct candidate, under the same limit and budget
	// as the reviews. A read failure private to one candidate is that
	// candidate's issue; the rows about it are unknown and post no review.
	identities, failure := runFanOut(ctx, indexKeys(len(cands)), func(ctx context.Context, key string) (candidateIdentity, reviewResult) {
		id, err := resolveCandidate(ctx, local, cands[atoi(key)])
		if err != nil {
			return candidateIdentity{}, acquisitionIssue(ctx, err)
		}
		return id, reviewResult{kind: outcomeAnswer}
	})
	if failure != nil {
		return runtime.Result{}, failure.Err
	}
	var groupCands []candidate
	for _, cand := range cands {
		if cachedGroup(cand) {
			groupCands = append(groupCands, cand)
		}
	}
	// Rancher's membership cache matters only when a binding names a group
	// Rancher can hold members for (knownMembers under it, "group <g>"
	// markers on its members); without one, no management request is spent
	// and no cache warning raised.
	members := map[string][]string{}
	if len(groupCands) > 0 {
		var memberWarning string
		members, memberWarning = groupMembers(ctx, local)
		if memberWarning != "" {
			warnings = append(warnings, memberWarning)
		}
		warnings = append(warnings, groupsCacheWarning)
	}
	targets := scope.targets
	multi := scope.mode == scopeProject || scope.mode == scopeAllNamespaces
	if len(targets) == 0 {
		// Cluster scope, or a collection verb on a namespaced resource: one
		// review with the empty namespace.
		targets = []string{""}
	}
	specs := make([]rowSpec, 0, len(cands)*len(targets))
	for ci := range cands {
		for _, ns := range targets {
			specs = append(specs, rowSpec{cand: ci, namespace: ns})
		}
	}
	rows, failure := runFanOut(ctx, indexKeys(len(specs)), func(ctx context.Context, key string) (Outcome, reviewResult) {
		spec := specs[atoi(key)]
		id := identities[spec.cand]
		if id.Issue != nil {
			return Outcome{}, reviewResult{kind: outcomeDegradable, err: clierr.New(id.Issue.Code, id.Issue.Message).WithRetryable(id.Issue.Retryable)}
		}
		r := postReview(ctx, callerClient, engine.ReviewSubjectAccess,
			subjectAccessReviewObject(p.question, spec.namespace, sarUser(id.Value.Subject), id.Value.Groups))
		if r.kind != outcomeAnswer {
			return Outcome{}, r
		}
		outcome := classifyAccess(r.resp.Object)
		if id.Value.GroupsFrom == GroupsAuthenticatedOnly && outcome.Decision == DecisionDenied {
			outcome.Decision, outcome.Note = DecisionUnknown, groupsUnknownNote
		}
		return outcome, r
	})
	if failure != nil {
		if failure.Kind == outcomeTerminal && failure.Status == 403 {
			return runtime.Result{}, whoCanSARRefusal(clusterID)
		}
		return runtime.Result{}, failure.Err
	}
	out.Rollup.Candidates, out.Rollup.Rows = len(cands), len(rows)
	byCandidate := make([][]subjectRow, len(cands))
	for i, m := range rows {
		spec := specs[i]
		cand := cands[spec.cand]
		id := identities[spec.cand].Value
		row := subjectRow{Kind: cand.Kind, Name: cand.Name, ID: id.Subject.ID, DisplayName: id.Subject.DisplayName,
			Source: m.Source, Issue: m.Issue, GroupsFrom: id.GroupsFrom}
		if multi {
			row.Namespace = spec.namespace
		}
		if m.Source == MemberAuthorizer {
			out.Rollup.Evaluated++
			row.Decision, row.Reason, row.Note, row.EvaluationError = m.Value.Decision, m.Value.Reason, m.Value.Note, m.Value.EvaluationError
			if row.Decision == DecisionAllowed && row.EvaluationError != "" {
				// Allowed stays allowed (one authorizer said yes); the
				// error another authorizer reported is a warning, as in
				// every access command.
				where := ""
				if multi {
					where = " in " + spec.namespace
				}
				warnings = append(warnings, fmt.Sprintf("the authorizer reported an evaluation error for %s%s: %s", cand.Name, where, row.EvaluationError))
			}
		} else {
			row.Decision = DecisionUnknown
			out.Rollup.Issues++
		}
		if cand.Fixed {
			row.Note = joinNotes(fixedMastersNote, row.Note)
		} else {
			row.Via = []string{"direct"}
		}
		if cachedGroup(cand) {
			row.KnownMembers = members[cand.Name]
		}
		if cand.Kind == KindUser && id.Subject.ID != "" {
			for _, g := range groupCands {
				if slices.Contains(members[g.Name], id.Subject.ID) {
					row.Via = append(row.Via, "group "+g.Name)
				}
			}
		}
		switch row.Decision {
		case DecisionAllowed:
			out.Rollup.Allowed++
		case DecisionDenied:
			out.Rollup.Denied++
		default:
			out.Rollup.Unknown++
			if m.Source == MemberAuthorizer && m.Value.Note == groupsUnknownNote {
				out.Rollup.GroupsUnknown++
			}
		}
		byCandidate[spec.cand] = append(byCandidate[spec.cand], row)
	}
	for _, group := range byCandidate {
		out.Subjects = append(out.Subjects, collapseClusterWide(group, multi)...)
	}
	out.ResultSource = deriveSource(rows)
	exit := 0
	if out.Rollup.Issues > 0 || out.Rollup.Unknown > out.Rollup.GroupsUnknown {
		exit = exitUnknown
	}
	return runtime.Result{Kind: "AccessSubjects", Data: out, ExitStatus: exit, Warnings: warnings, Text: whoCanText(out, showDenied, allNamespaces)}, nil
}

// indexKeys are the fan-out keys for n slice positions: decimal indexes,
// so no subject name can ever split or collide with a key.
func indexKeys(n int) []string {
	keys := make([]string, n)
	for i := range n {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func atoi(key string) int {
	i, err := strconv.Atoi(key)
	if err != nil {
		panic("fan-out key is not an index: " + key)
	}
	return i
}

// acquisitionIssue classifies a failed identity read of one candidate: the
// candidate's own request issue (degradable), unless the context is
// already dead, which is terminal like everywhere else.
func acquisitionIssue(ctx context.Context, err error) reviewResult {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		ce = clierr.New(clierr.CodeGeneric, err.Error())
	}
	if ctx.Err() != nil {
		return reviewResult{kind: outcomeTerminal, err: ce}
	}
	return reviewResult{kind: outcomeDegradable, err: ce}
}

func joinNotes(a, b string) string {
	if b == "" {
		return a
	}
	return a + "; " + b
}

// whoCanSARRefusal is the authorizer refusing the caller's
// SubjectAccessReview: the caller lacks the create right there.
func whoCanSARRefusal(clusterID string) *clierr.Error {
	where := "cluster " + clusterID
	if clusterID == engine.LocalCluster {
		where = "the management cluster"
	}
	return clierr.New(clierr.CodeRBAC,
		fmt.Sprintf("cannot ask the authorizer about other subjects on %s: you lack create subjectaccessreviews there", where)).
		WithHint("rancher get globalroles")
}

// cachedGroup reports whether Rancher may hold members for a candidate
// group. It caches the principals of identity-provider groups only, never
// Kubernetes' own system: groups (which every cluster's bootstrap bindings
// name), and the fixed system:masters row's members are not visible to the
// CLI at all: a cached principal that happens to carry that name is not
// evidence of them. Such a group still gets its own decision, just no
// members under it and no membership request spent on it.
func cachedGroup(c candidate) bool {
	return c.Kind == KindGroup && !c.Fixed && !strings.HasPrefix(c.Name, "system:")
}

// crbReason extracts the ClusterRoleBinding an allowed reason names, in
// the authorizer's own words.
var crbReason = regexp.MustCompile(`^RBAC: allowed by ClusterRoleBinding "([^"]+)"`)

func clusterRoleBindingIn(reason string) string {
	m := crbReason.FindStringSubmatch(reason)
	if m == nil {
		return ""
	}
	return m[1]
}

// collapseClusterWide folds one subject's per-namespace rows into a single
// "*" row when every row is allowed with no evaluation error and the
// server's reason for each names the same ClusterRoleBinding: server
// evidence of a cluster-wide grant. Any other mix, any absent or
// unparseable reason, and any evaluation error (which must stay
// attributable to its namespace) keeps the rows; the fixed system:masters
// row is allowed without a reason and so never collapses.
func collapseClusterWide(rows []subjectRow, multi bool) []subjectRow {
	if !multi || len(rows) < 2 {
		return rows
	}
	crb := clusterRoleBindingIn(rows[0].Reason)
	if crb == "" {
		return rows
	}
	for _, r := range rows {
		if r.Source != MemberAuthorizer || r.Decision != DecisionAllowed || r.EvaluationError != "" || clusterRoleBindingIn(r.Reason) != crb {
			return rows
		}
	}
	one := rows[0]
	one.Namespace = "*"
	return []subjectRow{one}
}

// whoCanText renders the question, then the allowed rows, then the
// unknown rows that carry an issue or an evaluation error, then one folded
// line for the subjects whose groups are unknown, then (with --show-denied)
// the denied rows or their count, then the source line. Authored text is
// plain ASCII; names and reasons are printed verbatim.
func whoCanText(out subjectsResult, showDenied, allNamespaces bool) string {
	target := out.Question.Resource
	if out.Question.Name != "" {
		target += "/" + out.Question.Name
	}
	if out.Question.Subresource != "" {
		target += " (" + out.Question.Subresource + ")"
	}
	if out.Question.NonResourceURL != "" {
		target = out.Question.NonResourceURL
	}
	where := " on " + out.Scope.Cluster
	switch {
	case out.Scope.Project != "":
		where = " in project " + out.Scope.Project + where
	case out.Scope.Namespace != "":
		where = " in " + out.Scope.Namespace + where
	case out.Scope.AllNamespaces || allNamespaces:
		where = " in all namespaces" + where
	}
	var b strings.Builder
	fmt.Fprintf(&b, "who can %s %s%s\n", out.Question.Verb, target, where)
	if out.Note != "" {
		fmt.Fprintf(&b, "  note: %s\n", output.Printable(out.Note))
	}
	multi := out.Scope.Project != "" || out.Scope.AllNamespaces
	var shown []subjectRow
	var groupsUnknown []string
	hiddenDenied := 0
	for _, r := range out.Subjects {
		switch {
		case r.Decision == DecisionAllowed:
			shown = append(shown, r)
		case r.Decision == DecisionDenied:
			if showDenied {
				shown = append(shown, r)
			} else {
				hiddenDenied++
			}
		case r.Issue == nil && r.EvaluationError == "" && r.Note == groupsUnknownNote:
			if !slices.Contains(groupsUnknown, r.Name) {
				groupsUnknown = append(groupsUnknown, r.Name)
			}
		default:
			shown = append(shown, r)
		}
	}
	slices.SortStableFunc(shown, func(x, y subjectRow) int { return decisionRank(x.Decision) - decisionRank(y.Decision) })
	header := []string{"KIND", "NAME", "DISPLAY NAME", "DECISION", "VIA"}
	if multi {
		header = []string{"KIND", "NAME", "DISPLAY NAME", "NAMESPACE", "DECISION", "VIA"}
	}
	fixedShown := false
	cells := make([][]string, 0, len(shown))
	for _, r := range shown {
		via := strings.Join(r.Via, ", ")
		note := r.Note
		// The fixed candidate's note is long enough to break the table: the
		// cell says "fixed" and the note is printed once, under the table.
		if rest, ok := strings.CutPrefix(note, fixedMastersNote); ok {
			fixedShown = true
			via = strings.TrimSpace(via + " fixed")
			note = strings.TrimPrefix(rest, "; ")
		}
		if note != "" {
			via = strings.TrimSpace(via + " (" + note + ")")
		}
		if r.EvaluationError != "" {
			via = strings.TrimSpace(via + " (evaluation error: " + r.EvaluationError + ")")
		}
		if r.Issue != nil {
			via = strings.TrimSpace(via + fmt.Sprintf(" (%s: %s)", r.Issue.Code, r.Issue.Message))
		}
		row := []string{string(r.Kind), r.Name, r.DisplayName, string(r.Decision), via}
		if multi {
			row = []string{string(r.Kind), r.Name, r.DisplayName, r.Namespace, string(r.Decision), via}
		}
		cells = append(cells, row)
	}
	if len(shown) > 0 {
		lines := tableLines("  ", header, cells)
		b.WriteString(lines[0] + "\n")
		for i, r := range shown {
			b.WriteString(lines[i+1] + "\n")
			if len(r.KnownMembers) > 0 {
				fmt.Fprintf(&b, "  known members: %s\n", output.Printable(strings.Join(r.KnownMembers, ", ")))
			}
		}
		if fixedShown {
			fmt.Fprintf(&b, "  fixed: %s\n", strings.TrimPrefix(fixedMastersNote, "fixed candidate: "))
		}
	}
	if len(groupsUnknown) > 0 {
		fmt.Fprintf(&b, "  %d subject(s) with unknown groups could not be ruled out: %s\n", len(groupsUnknown), output.Printable(strings.Join(groupsUnknown, ", ")))
	}
	if hiddenDenied > 0 {
		fmt.Fprintf(&b, "  %d denied not shown; pass --show-denied to list them\n", hiddenDenied)
	}
	fmt.Fprintf(&b, "source: %s | identity: %s\n", out.ResultSource, out.Identity)
	return b.String()
}

func decisionRank(d Decision) int {
	switch d {
	case DecisionAllowed:
		return 0
	case DecisionUnknown:
		return 1
	default:
		return 2
	}
}
