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

// explainCmd answers one question for one subject on one cluster with the
// authorizer's decision and reason, the binding and role the reason names,
// the rules in that role covering the request, and the Rancher chain
// behind the binding.
type explainCmd struct {
	clients engine.ClusterClientFactory
	local   engine.ClusterClient
}

// NewExplain builds `access explain`. local is the management-cluster
// client the Rancher layer reads through; nil resolves it from clients.
func NewExplain(clients engine.ClusterClientFactory, local engine.ClusterClient) runtime.Command {
	return &explainCmd{clients: clients, local: local}
}

func (c *explainCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "access explain",
		Usage: "Explain the cluster's authorizer's decision for a subject, a verb, and a resource",
		Desc: "Asks the same review as access can (SelfSubjectAccessReview as you or the impersonated subject, SubjectAccessReview " +
			"for a group or on the management cluster) and explains the answer: on allow, the one binding and role the server's " +
			"reason names, the rules in that role covering the request, and the Rancher chain behind the binding " +
			"(ClusterRoleTemplateBinding or ProjectRoleTemplateBinding, GlobalRoleBinding and GlobalRole, RoleTemplate " +
			"inheritance) read from its labels and annotations; on deny, the bindings on the cluster naming the subject or its " +
			"groups. The decision is the server's; the chain is provenance. Exit 0 allowed, 12 denied, 13 unknown; a partial or " +
			"unknown chain never changes the exit and is reported in chain and chainIssues.",
		Args: []runtime.ArgSpec{
			{Name: "verb", Type: "string", Required: true, Desc: "Kubernetes verb (get, list, create, ...) or, with --non-resource-url, an HTTP verb"},
			{Name: "resource", Type: "string", Desc: "resource plural, singular, short name, or resource.group; optionally /<name>; omitted with --non-resource-url"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster name or id the question is about",
				MissingMessage: engine.ClusterRequiredMessage, MissingHint: engine.ClusterRequiredHint},
			{Name: "namespace", Type: "string", Aliases: []string{"n"}, Desc: "namespace the question is about (one namespace; explain takes no --project or -A)"},
			{Name: "group", Type: "string", Desc: "API group when the resource name exists in several groups"},
			{Name: "subresource", Type: "string", Desc: "subresource such as status or log"},
			{Name: "non-resource-url", Type: "string", Desc: "ask about a non-resource path such as /healthz instead of a resource"},
			{Name: "as", Type: "string", Desc: "subject to explain instead of you: a Rancher user (id, username, or principal), a group principal, or a service account"},
			{Name: "type", Type: "string", Enum: []string{typeUser, typeGroup}, Desc: "how to read a bare --as value: user or group"},
			timeoutFlag(),
		},
		Examples: []string{
			"rancher access explain delete pods --cluster prod -n team-a",
			"rancher access explain delete pods --cluster prod -n team-a --as u-abc12 -o json",
			"rancher access explain get nodes --cluster prod --as okta_group://eng",
			"rancher access explain get --non-resource-url /healthz --cluster prod",
		},
		CommandType:      runtime.ReadOnly,
		Auth:             true,
		SuccessExitCodes: []int{0, exitDenied, exitUnknown},
		Shape:            runtime.ShapeText,
	}
}

// explainResult is the JSON kind AccessExplanation.
type explainResult struct {
	Subject         AccessSubject `json:"subject"`
	Question        Question      `json:"question"`
	Scope           Scope         `json:"scope"`
	Decision        Decision      `json:"decision"`
	Reason          string        `json:"reason"`
	Note            string        `json:"note"`
	EvaluationError string        `json:"evaluationError,omitempty"`
	Chain           chainState    `json:"chain"`
	ChainIssues     []chainIssue  `json:"chainIssues"`
	Tree            *chainNode    `json:"tree"`
	ResultSource    ResultSource  `json:"resultSource"`
	Identity        Identity      `json:"identity"`
	GroupsFrom      GroupsFrom    `json:"groupsFrom,omitempty"`
}

const (
	deniedNote = "the CLI found no binding above that the server accepted for this request"
)

func (c *explainCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	p, err := parseQuestion(in, false, false)
	if err != nil {
		return runtime.Result{}, err
	}
	// One budget for the whole explanation: discovery, identity, the
	// review, and every read of the chain.
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
		if err := r.rejectSelectors(p.scope); err != nil {
			return runtime.Result{}, err
		}
	}
	scope, err := resolveScope(ctx, callerClient, clusterID, p.scope, runtime.Project{})
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "namespace discovery")
	}
	if ref != nil && ref.Namespaced && scope.mode == scopeCluster && (p.name != "" || !isCollectionVerb(p.question.Verb)) {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s %s needs a namespace: add -n", p.question.Verb, p.resource))
	}
	local, err := localClient(ctx, c.clients, c.local)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "the management cluster client")
	}
	plan, err := planIdentity(ctx, c.clients, callerClient, local, clusterID, caller, p.as, false)
	if err != nil {
		return runtime.Result{}, budgetError(ctx, err, "subject resolution")
	}
	out := explainResult{Subject: plan.Subject, Question: p.question, Scope: scope, Identity: plan.Identity, GroupsFrom: plan.GroupsFrom,
		Chain: chainComplete, ChainIssues: []chainIssue{}}
	if plan.Denied != nil {
		// The proxy refused the subject's cluster access before any
		// authorizer was asked: one denial, nothing to explain further.
		out.Decision, out.ResultSource, out.GroupsFrom, out.Note = DecisionDenied, SourceRancherProxy, "", plan.Denied.Note
		res := runtime.Result{Kind: "AccessExplanation", Data: out, ExitStatus: exitDenied, Text: explainText(out)}
		return withDiscoveryWarning(res, discoveryWarning), nil
	}
	var r reviewResult
	if plan.Identity == IdentitySAR {
		r = postReview(ctx, plan.Client, engine.ReviewSubjectAccess, subjectAccessReviewObject(p.question, scope.Namespace, sarUser(plan.Subject), plan.Groups))
	} else {
		r = postReview(ctx, plan.Client, engine.ReviewSelfSubjectAccess, accessReviewObject(p.question, scope.Namespace))
	}
	if r.kind != outcomeAnswer {
		if r.kind == outcomeTerminal && r.resp.Status == 403 && plan.Identity == IdentitySAR {
			return runtime.Result{}, sarRefusal(ResolvedSubject{Subject: plan.Subject}, clusterID)
		}
		return runtime.Result{}, budgetError(ctx, r.err, "the review")
	}
	outcome := classifyAccess(r.resp.Object)
	if plan.GroupsFrom == GroupsAuthenticatedOnly && outcome.Decision == DecisionDenied {
		outcome.Decision, outcome.Note = DecisionUnknown, groupsUnknownNote
	}
	out.Decision, out.Reason, out.EvaluationError, out.Note = outcome.Decision, outcome.Reason, outcome.EvaluationError, outcome.Note
	out.ResultSource = SourceAuthorizer

	groups := plan.Groups
	if plan.AsCaller && len(groups) == 0 {
		groups = caller.Groups
	}
	subject := chainSubject{Kind: plan.Subject.Kind, Name: plan.Subject.Name, ID: plan.Subject.ID, PrincipalIDs: plan.PrincipalIDs, Groups: groups}
	// The chain is read with the caller's own client on the downstream and
	// the management client: what the caller may read, never more.
	walker := newChainWalker(ctx, callerClient, local, clusterID, subject, p.question)
	switch out.Decision {
	case DecisionAllowed:
		out.Tree, out.Chain = walker.explainAllowed(out.Reason)
	case DecisionDenied:
		root, state := walker.explainDenied(scope.Namespace)
		out.Tree, out.Chain = &root, state
		out.Note = strings.TrimPrefix(deniedNote+"; "+out.Note, "; ")
		out.Note = strings.TrimSuffix(out.Note, "; ")
	default:
		out.Chain = chainUnknown
	}
	if walker.issues != nil {
		out.ChainIssues = walker.issues
	}
	res := runtime.Result{Kind: "AccessExplanation", Data: out, ExitStatus: exitFor(out.Decision), Text: explainText(out)}
	if outcome.EvaluationError != "" && outcome.Decision == DecisionAllowed {
		res.Warnings = append(res.Warnings, "the authorizer reported an evaluation error: "+outcome.EvaluationError)
	}
	return withDiscoveryWarning(res, discoveryWarning), nil
}

// explainText renders the decision line, the reason and note, the tree,
// the chain block when the chain is not complete, and the source line.
// Authored text is plain ASCII; server text passes the terminal-safety
// boundary at insertion.
func explainText(out explainResult) string {
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
	if out.Scope.Namespace != "" {
		where = " in " + out.Scope.Namespace + where
	}
	verbPhrase := map[Decision]string{DecisionAllowed: "can", DecisionDenied: "cannot", DecisionUnknown: "may or may not"}[out.Decision]
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s %s %s %s%s\n", out.Decision, output.Printable(who), verbPhrase, out.Question.Verb, output.Printable(target), output.Printable(where))
	if out.Reason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", output.Printable(out.Reason))
	}
	if out.Note != "" {
		fmt.Fprintf(&b, "  note: %s\n", output.Printable(out.Note))
	}
	if out.EvaluationError != "" {
		fmt.Fprintf(&b, "  evaluation error: %s\n", output.Printable(out.EvaluationError))
	}
	if out.Tree != nil {
		mark := "[" + string(out.Decision) + "]"
		for _, line := range renderTree(*out.Tree, mark) {
			b.WriteString(output.Printable(line) + "\n")
		}
	}
	if out.Chain != chainComplete {
		noun := "issues"
		if len(out.ChainIssues) == 1 {
			noun = "issue"
		}
		fmt.Fprintf(&b, "chain: %s (%d %s)\n", out.Chain, len(out.ChainIssues), noun)
		for _, i := range out.ChainIssues {
			object := ""
			if i.Object != "" {
				object = " " + i.Object
			}
			fmt.Fprintf(&b, "  %s%s: %s - %s\n", i.Stage, output.Printable(object), i.Code, output.Printable(i.Message))
		}
	}
	fmt.Fprintf(&b, "source: %s | identity: %s", out.ResultSource, out.Identity)
	if out.GroupsFrom != "" {
		fmt.Fprintf(&b, " | groups from %s", out.GroupsFrom)
	}
	b.WriteString("\n")
	return b.String()
}
