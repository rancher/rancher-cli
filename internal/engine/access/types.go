package access

import (
	"github.com/rancher/rancher-cli/internal/clierr"
)

// Decision is the authorizer's answer to one question.
type Decision string

const (
	DecisionAllowed Decision = "allowed"
	DecisionDenied  Decision = "denied"
	DecisionUnknown Decision = "unknown"
)

// ResultSource says who produced a result: the Kubernetes authorizer, the
// Rancher proxy's cluster-access gate, scope discovery (no member to ask),
// failed requests only, or a mix of member sources.
type ResultSource string

const (
	SourceAuthorizer     ResultSource = "authorizer"
	SourceRancherProxy   ResultSource = "rancherProxy"
	SourceScopeDiscovery ResultSource = "scopeDiscovery"
	SourceRequest        ResultSource = "request"
	SourceMixed          ResultSource = "mixed"
)

// MemberSource is a fan-out member's own source; the result source is
// derived from the members.
type MemberSource string

const (
	MemberAuthorizer   MemberSource = "authorizer"
	MemberRequest      MemberSource = "request"
	MemberRancherProxy MemberSource = "rancherProxy"
)

// Identity says whose credentials answered: the caller's own, an
// impersonated subject's, or a SubjectAccessReview the caller posted about
// the subject.
type Identity string

const (
	IdentityCaller       Identity = "caller"
	IdentityImpersonated Identity = "impersonated"
	IdentitySAR          Identity = "subjectAccessReview"
)

// GroupsFrom says where a SubjectAccessReview's groups came from.
type GroupsFrom string

const (
	GroupsRancherUserAttribute   GroupsFrom = "rancherUserAttribute"
	GroupsServiceAccountIdentity GroupsFrom = "serviceAccountIdentity"
	GroupsGroupOnly              GroupsFrom = "groupOnly"
	GroupsAnonymousIdentity      GroupsFrom = "anonymousIdentity"
	GroupsAuthenticatedOnly      GroupsFrom = "authenticatedOnly"
)

// SubjectKind is the closed set every access output uses; Kubernetes'
// title-case RBAC subject kinds never appear in output.
type SubjectKind string

const (
	KindUser           SubjectKind = "user"
	KindGroup          SubjectKind = "group"
	KindServiceAccount SubjectKind = "serviceaccount"
)

// AccessSubject is the subject as Kubernetes sees it. Name is the Rancher
// user id for a Rancher user, the group name, the service-account name,
// or an arbitrary username; ID is set only for Rancher users (equal to
// Name); DisplayName is the Rancher User's display name when resolved.
type AccessSubject struct {
	Kind        SubjectKind `json:"kind"`
	Name        string      `json:"name"`
	ID          string      `json:"id,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
}

// Question is one authorization question in the server's own terms.
// Version is the API version discovery resolved; it is empty for a
// non-resource URL.
type Question struct {
	Verb           string `json:"verb"`
	Resource       string `json:"resource"`
	Group          string `json:"group"`
	Version        string `json:"version"`
	Name           string `json:"name"`
	Subresource    string `json:"subresource"`
	NonResourceURL string `json:"nonResourceURL"`
}

// Issue is a request failure that did not reach the authorizer, copied
// field for field from the provider's mapped error.
type Issue struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

func issueFrom(err *clierr.Error) Issue {
	return Issue{Code: err.Code, Retryable: err.Retryable, Message: err.Message}
}

// Outcome is one classified SubjectAccessReview answer.
type Outcome struct {
	Decision        Decision
	Reason          string
	Note            string
	EvaluationError string
	Denied          bool
}
