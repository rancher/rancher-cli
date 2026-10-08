package access

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

const (
	flagAs             = "as"
	flagType           = "type"
	flagGroup          = "group"
	flagSubresource    = "subresource"
	flagNonResourceURL = "non-resource-url"
	flagNamespace      = "namespace"
	flagProject        = "project"
	flagAllNamespaces  = "all-namespaces"

	exitDenied  = 12
	exitUnknown = 13

	oneDenialNote = "one cluster-access denial applies to all listed namespaces"

	// groupsUnknownNote marks a negative answer about a subject whose real
	// groups are unknown: it proves nothing, so the row is unknown.
	groupsUnknownNote = "groups unknown"
)

type canCmd struct {
	clients engine.ClusterClientFactory
	local   engine.ClusterClient
}

// NewCan builds `access can`. local is the management cluster client used
// to resolve --as; nil means resolve it from clients on first use.
func NewCan(clients engine.ClusterClientFactory, local engine.ClusterClient) runtime.Command {
	return &canCmd{clients: clients, local: local}
}

func (c *canCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access can",
		Usage: "Ask the cluster's authorizer whether a subject can perform a verb on a resource",
		Desc: "Posts a SelfSubjectAccessReview (or a SubjectAccessReview about --as) through the Rancher proxy and reports " +
			"the server's decision: allowed, denied, or unknown. Nothing is evaluated client-side. Exit 0 allowed, 12 denied, 13 unknown.",
		Args: []runtime.ArgSpec{
			{Name: "verb", Type: "string", Required: true, Desc: "Kubernetes verb (get, list, create, ...) or, with --non-resource-url, an HTTP verb"},
			{Name: "resource", Type: "string", Desc: "resource plural, singular, short name, or resource.group; optionally /<name>; omitted with --non-resource-url"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster name or id the question is about",
				MissingMessage: engine.ClusterRequiredMessage, MissingHint: engine.ClusterRequiredHint},
			{Name: "namespace", Type: "string", Aliases: []string{"n"}, Desc: "namespace the question is about"},
			runtime.ProjectFlag(false, "ask about the project's namespaces"),
			{Name: "as", Type: "string", Desc: "subject to ask about: user name, id, or principal; group principal; system:serviceaccount:<ns>:<name>; omitted = you"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "kind of a bare --as value when ambiguous"},
			{Name: "group", Type: "string", Desc: "API group when the resource name exists in several groups"},
			{Name: "subresource", Type: "string", Desc: "subresource such as status or log"},
			{Name: "non-resource-url", Type: "string", Desc: "ask about a non-resource path such as /healthz instead of a resource"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access can delete pods --cluster prod -n team-a",
			"rancher access can delete pods --cluster prod -n team-a --as alice",
			"rancher access can get --non-resource-url /healthz --cluster prod --as github_org://eng",
			"rancher access can list pods --cluster prod --project p-cvrp4 -o json",
		},
		CommandType:      runtime.ReadOnly,
		Auth:             true,
		SuccessExitCodes: []int{0, 12, 13},
		Shape:            runtime.ShapeText,
	}
}

type canResult struct {
	Subject         AccessSubject  `json:"subject"`
	Question        Question       `json:"question"`
	Scope           Scope          `json:"scope"`
	Decision        Decision       `json:"decision"`
	Reason          string         `json:"reason"`
	Note            string         `json:"note"`
	EvaluationError string         `json:"evaluationError"`
	Namespaces      []canNamespace `json:"namespaces,omitempty"`
	Rollup          *canRollup     `json:"rollup,omitempty"`
	ResultSource    ResultSource   `json:"resultSource"`
	Identity        Identity       `json:"identity"`
	GroupsFrom      GroupsFrom     `json:"groupsFrom,omitempty"`
}

type canNamespace struct {
	Namespace       string       `json:"namespace"`
	Source          MemberSource `json:"source"`
	Decision        Decision     `json:"decision"`
	Reason          string       `json:"reason"`
	Note            string       `json:"note"`
	EvaluationError string       `json:"evaluationError"`
	Issue           *Issue       `json:"issue"`
}

type canRollup struct {
	Namespaces int `json:"namespaces"`
	Evaluated  int `json:"evaluated"`
	Allowed    int `json:"allowed"`
	Denied     int `json:"denied"`
	Unknown    int `json:"unknown"`
	Issues     int `json:"issues"`
}

// parsedCan is the validated command line, built before any network call.
type parsedCan struct {
	question Question
	resource string // raw resource token, before discovery
	name     string
	as       ParsedSubject
	scope    scopeFlags
}

// parseCan validates the access can command line: a question that never
// accepts --all-namespaces.
func parseCan(in runtime.Input) (parsedCan, error) { return parseQuestion(in, false, true) }

// parseQuestion validates a question command line (access can, access
// who-can, access explain). Every rule here fires before the first request;
// allowAll says whether --all-namespaces is accepted, takesProject whether
// the command takes --project (access explain does not, and never reads it).
func parseQuestion(in runtime.Input, allowAll, takesProject bool) (parsedCan, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return parsedCan{}, clierr.New(clierr.CodeValidation, "a verb is required")
	}
	p := parsedCan{}
	nonResource := in.String(flagNonResourceURL)
	if nonResource != "" {
		if len(args) > 1 {
			return parsedCan{}, clierr.New(clierr.CodeValidation, "--non-resource-url replaces the resource argument; pass the verb only")
		}
		scoping := []string{flagGroup, flagSubresource, flagNamespace}
		if takesProject {
			scoping = append(scoping, flagProject)
		}
		for _, f := range scoping {
			if in.String(f) != "" {
				return parsedCan{}, clierr.New(clierr.CodeValidation, "--non-resource-url cannot be combined with --"+f)
			}
		}
		if in.Bool(flagAllNamespaces) {
			return parsedCan{}, clierr.New(clierr.CodeValidation, "--non-resource-url cannot be combined with --all-namespaces")
		}
		if !strings.HasPrefix(nonResource, "/") {
			return parsedCan{}, clierr.New(clierr.CodeValidation, "a non-resource URL must begin with /")
		}
		verb, err := NormalizeVerb(args[0], true)
		if err != nil {
			return parsedCan{}, err
		}
		p.question = Question{Verb: verb, NonResourceURL: nonResource}
	} else {
		if len(args) < 2 || args[1] == "" {
			return parsedCan{}, clierr.New(clierr.CodeValidation, "a resource is required (or --non-resource-url)")
		}
		verb, err := NormalizeVerb(args[0], false)
		if err != nil {
			return parsedCan{}, err
		}
		res, name, hasName := strings.Cut(args[1], "/")
		if hasName && (name == "" || strings.Contains(name, "/")) {
			return parsedCan{}, clierr.New(clierr.CodeValidation, "write the resource as <resource> or <resource>/<name>; the name is one segment")
		}
		p.question = Question{Verb: verb, Subresource: in.String(flagSubresource), Name: name}
		p.resource, p.name = res, name
	}
	as, err := ParseSubject(in.String(flagAs), in.String(flagType))
	if err != nil {
		return parsedCan{}, err
	}
	p.as = as
	p.scope = scopeFlags{namespace: in.String(flagNamespace), allNamespaces: in.Bool(flagAllNamespaces), allowAll: allowAll}
	if takesProject {
		p.scope.project = in.String(flagProject)
	}
	if err := validateSelectors(p.scope); err != nil {
		return parsedCan{}, err
	}
	return p, nil
}

// localClient is the management cluster client a command resolves --as
// with: the injected one when a test supplied it, else the factory's.
func localClient(ctx context.Context, clients engine.ClusterClientFactory, local engine.ClusterClient) (engine.ClusterClient, error) {
	if local != nil {
		return local, nil
	}
	return clients.Cluster(ctx, engine.LocalCluster)
}

func (c *canCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	p, err := parseCan(in)
	if err != nil {
		return runtime.Result{}, err
	}
	// One budget for the whole call: target discovery, subject
	// resolution, reviews.
	budget, err := parseTimeout(in)
	if err != nil {
		return runtime.Result{}, err
	}
	ctx, cancel := fanOutContext(ctx, budget)
	defer cancel()
	// Order: cluster client, caller preflight, discovery, scope,
	// then the rest of the identity protocol.
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
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
		// Decidable from discovery alone: before any project lookup.
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
				fmt.Sprintf("%s %s needs a namespace: add -n or --project", p.question.Verb, p.resource))
		}
		allNamespaces = true
	}
	local, err := localClient(ctx, c.clients, c.local)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the management cluster client")
	}
	plan, err := planIdentity(ctx, c.clients, callerClient, local, clusterID, caller, p.as, false)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "subject resolution")
	}
	out := canResult{Subject: plan.Subject, Question: p.question, Scope: scope, Identity: plan.Identity, GroupsFrom: plan.GroupsFrom}
	if scope.mode == scopeProject && len(scope.targets) == 0 {
		// A verified-empty project: no question was evaluated for anyone,
		// so nothing (not even the proxy's cluster-wide refusal of the
		// subject) can turn this into an answer.
		out.Decision, out.ResultSource = DecisionUnknown, SourceScopeDiscovery
		out.Note = fmt.Sprintf("project %s has no namespaces on cluster %s", scope.Project, clusterID)
		out.Namespaces, out.Rollup = []canNamespace{}, &canRollup{}
		res := runtime.Result{Kind: "AccessDecision", Data: out, ExitStatus: exitUnknown, Text: canText(out, false)}
		return withDiscoveryWarning(res, discoveryWarning), nil
	}
	if plan.Denied != nil {
		return withDiscoveryWarning(c.renderDenial(out, scope, plan, clusterID), discoveryWarning), nil
	}
	ask := func(ctx context.Context, namespace string) (Outcome, reviewResult) {
		var r reviewResult
		if plan.Identity == IdentitySAR {
			r = postReview(ctx, plan.Client, engine.ReviewSubjectAccess, subjectAccessReviewObject(p.question, namespace, sarUser(plan.Subject), plan.Groups))
		} else {
			r = postReview(ctx, plan.Client, engine.ReviewSelfSubjectAccess, accessReviewObject(p.question, namespace))
		}
		if r.kind != outcomeAnswer {
			return Outcome{}, r
		}
		outcome := classifyAccess(r.resp.Object)
		if plan.GroupsFrom == GroupsAuthenticatedOnly && outcome.Decision == DecisionDenied {
			// The subject's real groups are unknown; a negative answer
			// proves nothing.
			outcome.Decision, outcome.Note = DecisionUnknown, groupsUnknownNote
		}
		return outcome, r
	}
	if scope.mode != scopeProject {
		ns := scope.Namespace
		if allNamespaces {
			ns = ""
		}
		outcome, r := ask(ctx, ns)
		switch r.kind {
		case outcomeAnswer:
		case outcomeTerminal:
			if r.resp.Status == 403 && plan.Identity == IdentitySAR {
				return runtime.Result{}, sarRefusal(ResolvedSubject{Subject: plan.Subject}, clusterID)
			}
			return runtime.Result{}, budgetError(ctx, r.err, "the review")
		default:
			// Degradation is fan-out only; a single review's request
			// failure is the mapped error, except an exhausted budget,
			// which is named as such like every other stage.
			return runtime.Result{}, budgetError(ctx, r.err, "the review")
		}
		out.Decision, out.Reason, out.EvaluationError, out.Note = outcome.Decision, outcome.Reason, outcome.EvaluationError, outcome.Note
		out.ResultSource = SourceAuthorizer
		res := withDiscoveryWarning(runtime.Result{Kind: "AccessDecision", Data: out, ExitStatus: exitFor(out.Decision)}, discoveryWarning)
		if outcome.EvaluationError != "" && outcome.Decision == DecisionAllowed {
			res.Warnings = append(res.Warnings, "the authorizer reported an evaluation error: "+outcome.EvaluationError)
		}
		res.Text = canText(out, allNamespaces)
		return res, nil
	}
	members, failure := runFanOut(ctx, scope.targets, ask)
	if failure != nil {
		// Exactly the single-review rule: a SubjectAccessReview the
		// authorizer refused (403 with a Status body) is the caller's
		// missing right. Mid-run cluster-access loss (its own kind),
		// budget, cancellation, and protocol errors pass through as they
		// are.
		if failure.Kind == outcomeTerminal && failure.Status == 403 && plan.Identity == IdentitySAR {
			return runtime.Result{}, sarRefusal(ResolvedSubject{Subject: plan.Subject}, clusterID)
		}
		return runtime.Result{}, failure.Err
	}
	rollup := canRollup{Namespaces: len(members)}
	for _, m := range members {
		row := canNamespace{Namespace: m.Key, Source: m.Source, Issue: m.Issue}
		switch m.Source {
		case MemberAuthorizer:
			rollup.Evaluated++
			row.Decision, row.Reason, row.EvaluationError, row.Note = m.Value.Decision, m.Value.Reason, m.Value.EvaluationError, m.Value.Note
		default:
			row.Decision = DecisionUnknown
			rollup.Issues++
		}
		switch row.Decision {
		case DecisionAllowed:
			rollup.Allowed++
		case DecisionDenied:
			rollup.Denied++
		default:
			rollup.Unknown++
		}
		out.Namespaces = append(out.Namespaces, row)
	}
	out.Rollup = &rollup
	out.ResultSource = deriveSource(members)
	switch {
	case rollup.Unknown > 0:
		out.Decision = DecisionUnknown
	case rollup.Denied > 0:
		out.Decision = DecisionDenied
	default:
		out.Decision = DecisionAllowed
	}
	res := runtime.Result{Kind: "AccessDecision", Data: out, ExitStatus: exitFor(out.Decision), Text: canText(out, false)}
	return withDiscoveryWarning(res, discoveryWarning), nil
}

// renderDenial expands the proxy's one cluster-wide refusal into the
// project shape without asking any authorizer.
func (c *canCmd) renderDenial(out canResult, scope Scope, plan identityPlan, clusterID string) runtime.Result {
	out.Decision, out.ResultSource, out.GroupsFrom = DecisionDenied, SourceRancherProxy, ""
	out.Note = plan.Denied.Note
	if scope.mode == scopeProject {
		out.Note = oneDenialNote + ": " + plan.Denied.Note
		rollup := canRollup{Namespaces: len(scope.targets), Denied: len(scope.targets)}
		for _, ns := range scope.targets {
			out.Namespaces = append(out.Namespaces, canNamespace{Namespace: ns, Source: MemberRancherProxy, Decision: DecisionDenied})
		}
		out.Rollup = &rollup
	}
	return runtime.Result{Kind: "AccessDecision", Data: out, ExitStatus: exitDenied, Text: canText(out, false)}
}

// sarUser is the SubjectAccessReview's spec.user for a subject: empty for
// a group, whose question is asked by groups alone.
func sarUser(subject AccessSubject) string {
	if subject.Kind == KindGroup {
		return ""
	}
	return subject.Name
}

func isCollectionVerb(v string) bool {
	return v == "list" || v == "watch" || v == "deletecollection"
}

func exitFor(d Decision) int {
	switch d {
	case DecisionAllowed:
		return 0
	case DecisionDenied:
		return exitDenied
	default:
		return exitUnknown
	}
}

// canText renders the decision first, then the reason or note, one line
// per namespace for a project, then the source line. Authored text is
// plain ASCII; subject names and reasons are printed verbatim.
func canText(out canResult, allNamespaces bool) string {
	who := out.Subject.Name
	if out.Identity == IdentityCaller {
		who = "you"
	}
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
	case allNamespaces:
		where = " in all namespaces" + where
	}
	verbPhrase := map[Decision]string{DecisionAllowed: "can", DecisionDenied: "cannot", DecisionUnknown: "may or may not"}[out.Decision]
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s %s %s %s%s\n", out.Decision, who, verbPhrase, out.Question.Verb, target, where)
	// The reason, note, evaluation error, namespace names, and issue
	// messages are the server's text: each passes the terminal-safety
	// boundary at insertion.
	if out.Reason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", output.Printable(out.Reason))
	}
	if out.Note != "" {
		fmt.Fprintf(&b, "  note: %s\n", output.Printable(out.Note))
	}
	if out.EvaluationError != "" {
		fmt.Fprintf(&b, "  evaluation error: %s\n", output.Printable(out.EvaluationError))
	}
	for _, ns := range out.Namespaces {
		fmt.Fprintf(&b, "  %-24s %s", output.Printable(ns.Namespace), ns.Decision)
		if ns.Issue != nil {
			fmt.Fprintf(&b, "  (%s: %s)", ns.Issue.Code, output.Printable(ns.Issue.Message))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "source: %s | identity: %s", out.ResultSource, out.Identity)
	if out.GroupsFrom != "" {
		fmt.Fprintf(&b, " | groups from %s", out.GroupsFrom)
	}
	b.WriteString("\n")
	return b.String()
}
