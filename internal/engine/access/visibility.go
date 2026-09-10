package access

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine"
)

// Project visibility values: the rollup of a project's namespace answers.
const (
	visibilityAll           = "all"
	visibilityPartial       = "partial"
	visibilityNone          = "none"
	visibilityUnknown       = "unknown"
	visibilityNotApplicable = "notApplicable"
)

// Namespace access values: the authorizer's answer to "may the subject get
// this Namespace object", or unknown when no answer was had.
const (
	nsAllowed = "allowed"
	nsDenied  = "denied"
	nsUnknown = "unknown"
)

const (
	emptyProjectNote     = "project has no namespaces; nothing to ask the authorizer"
	discoveryFailedNote  = "namespace discovery failed on this cluster; see the cluster's namespaceDiscovery issue"
	visibilityDefinition = "visibility = may get the project's namespaces, the grant Rancher gives members; rules inside a namespace are access list"
)

// namespaceRow is the authorizer's answer for one namespace a project owns.
// Reason is the server's own text, verbatim: Kubernetes RBAC names the
// binding that allowed the request, other authorizers may say nothing.
type namespaceRow struct {
	Namespace       string    `json:"namespace"`
	Access          string    `json:"access"`
	Source          RowSource `json:"source"`
	Issue           *Issue    `json:"issue"`
	Reason          string    `json:"reason"`
	EvaluationError string    `json:"evaluationError"`
}

// visibility is the rollup of a project's namespace rows.
type visibility struct {
	Visibility string
	Source     ResultSource
	AllowedIn  int
	UnknownIn  int
	Complete   bool
	Note       string
}

// namespaceGetQuestion is the one question access projects asks the
// authorizer: may the subject get this Namespace object. Rancher grants
// exactly that to every member of the project that owns the namespace.
// The review carries the namespace as its namespace attribute too, as the
// apiserver derives it for a GET of a Namespace object, so a RoleBinding
// inside the namespace counts the way it does for the real request.
func namespaceGetQuestion(namespace string) Question {
	return Question{Verb: "get", Resource: "namespaces", Version: "v1", Name: namespace}
}

// reviewNamespaces asks the authorizer, as the subject the client acts as,
// whether it may get each namespace, under the fan-out limit. A request
// failure is the namespace's issue (access unknown, source request); the
// first terminal outcome aborts the fan-out and is returned.
func reviewNamespaces(ctx context.Context, client engine.ClusterClient, namespaces []string) ([]namespaceRow, *fanOutFailure) {
	members, failure := runFanOut(ctx, namespaces, func(ctx context.Context, ns string) (Outcome, reviewResult) {
		r := postReview(ctx, client, engine.ReviewSelfSubjectAccess, accessReviewObject(namespaceGetQuestion(ns), ns))
		if r.kind != outcomeAnswer {
			return Outcome{}, r
		}
		return classifyAccess(r.resp.Object), r
	})
	if failure != nil {
		return nil, failure
	}
	rows := make([]namespaceRow, len(members))
	for i, m := range members {
		row := namespaceRow{Namespace: m.Key}
		switch m.Source {
		case MemberAuthorizer:
			row.Source = RowAuthorizer
			row.Reason, row.EvaluationError = m.Value.Reason, m.Value.EvaluationError
			switch m.Value.Decision {
			case DecisionAllowed:
				row.Access = nsAllowed
			case DecisionDenied:
				row.Access = nsDenied
			default:
				row.Access = nsUnknown
			}
		default:
			row.Source, row.Access, row.Issue = RowRequest, nsUnknown, m.Issue
		}
		rows[i] = row
	}
	return rows, nil
}

// rollupVisibility derives a project's visibility from its namespace rows:
// all when every namespace is allowed, partial when some are, none when
// none is and none is unknown, unknown when none is allowed and at least
// one is unknown. A project that owns no namespace, or whose cluster's
// namespace discovery failed, is unknown with source scopeDiscovery and
// never complete.
func rollupVisibility(rows []namespaceRow, discoveryFailed bool) visibility {
	if discoveryFailed {
		return visibility{Visibility: visibilityUnknown, Source: SourceScopeDiscovery, Note: discoveryFailedNote}
	}
	if len(rows) == 0 {
		return visibility{Visibility: visibilityUnknown, Source: SourceScopeDiscovery, Note: emptyProjectNote}
	}
	v := visibility{}
	seen := map[RowSource]bool{}
	for _, r := range rows {
		seen[r.Source] = true
		switch r.Access {
		case nsAllowed:
			v.AllowedIn++
		case nsUnknown:
			v.UnknownIn++
		}
	}
	switch {
	case v.AllowedIn == len(rows):
		v.Visibility = visibilityAll
	case v.AllowedIn > 0:
		v.Visibility = visibilityPartial
	case v.UnknownIn == 0:
		v.Visibility = visibilityNone
	default:
		v.Visibility = visibilityUnknown
	}
	if len(seen) == 1 {
		for s := range seen {
			v.Source = ResultSource(s)
		}
	} else {
		v.Source = SourceMixed
	}
	v.Complete = v.UnknownIn == 0
	return v
}

// visibilityCell renders the VISIBILITY column: all, partial (2 of 3),
// partial (2 of 3, 1 unknown), none, unknown.
func visibilityCell(v visibility, namespaces int) string {
	if v.Visibility != visibilityPartial {
		return v.Visibility
	}
	if v.UnknownIn > 0 {
		return fmt.Sprintf("partial (%d of %d, %d unknown)", v.AllowedIn, namespaces, v.UnknownIn)
	}
	return fmt.Sprintf("partial (%d of %d)", v.AllowedIn, namespaces)
}
