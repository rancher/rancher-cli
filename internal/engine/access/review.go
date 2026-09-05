package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// ReviewOutcomeKind classifies one PostReview call per the review outcome
// matrix: an answer, one of the three Rancher-side proxy facts (no
// cluster access, impersonation refused, impersonated service account
// not found), a terminal failure (refused endpoint, malformed answer,
// cancellation), or a degradable request failure (5xx, transport,
// per-request timeout).
type ReviewOutcomeKind int

const (
	outcomeAnswer ReviewOutcomeKind = iota
	outcomeNoClusterAccess
	outcomeImpersonationRefused
	outcomeSubjectNotFound
	outcomeTerminal
	outcomeDegradable
)

type reviewResult struct {
	kind ReviewOutcomeKind
	resp engine.ReviewResponse
	err  *clierr.Error
}

const (
	rancherImpersonateUserRefusal = "not allowed to impersonate user"
	rancherImpersonateSARefusal   = "not allowed to impersonate service account"
)

// saTokenFailurePrefix opens the proxy's answer when the impersonated
// service account's token cannot be created (the service account or its
// namespace does not exist): HTTP 500, text/plain.
const saTokenFailurePrefix = "unable to create token for impersonated ServiceAccount:"

// postReview posts one review and classifies the answer. It never
// evaluates the answer's content beyond the HTTP shape; decision mapping
// is classifyAccess.
//
// A per-request timeout the provider imposed arrives as a retryable error
// with the parent context still live, so it degrades; a parent deadline
// shows as ctx.Err() != nil and is terminal.
func postReview(ctx context.Context, c engine.ClusterClient, kind engine.ReviewKind, obj *unstructured.Unstructured) reviewResult {
	resp, err := c.PostReview(ctx, engine.ReviewRequest{Kind: kind, Object: obj})
	if err != nil {
		ce, ok := errors.AsType[*clierr.Error](err)
		if !ok {
			ce = clierr.New(clierr.CodeGeneric, err.Error())
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return reviewResult{kind: outcomeTerminal, err: ce}
		}
		// The provider marks every no-response failure (refused, reset,
		// per-request timeout) retryable and every protocol error
		// (malformed 2xx) non-retryable; that flag is the degradable line.
		if ce.Retryable {
			return reviewResult{kind: outcomeDegradable, err: ce}
		}
		return reviewResult{kind: outcomeTerminal, err: ce}
	}
	switch {
	case resp.Status >= 200 && resp.Status <= 299:
		return reviewResult{kind: outcomeAnswer, resp: resp}
	case resp.Status == http.StatusForbidden && isRancherImpersonationRefusal(resp.Body):
		return reviewResult{kind: outcomeImpersonationRefused, resp: resp, err: resp.Mapped}
	case resp.Status == http.StatusForbidden && isRancherClusterAccessRefusal(resp.Body):
		return reviewResult{kind: outcomeNoClusterAccess, resp: resp, err: resp.Mapped}
	case resp.Status == http.StatusInternalServerError && strings.HasPrefix(strings.TrimSpace(string(resp.Body)), saTokenFailurePrefix):
		// The proxy could not create the impersonated service account's
		// token: the subject (or its namespace) does not exist. The one
		// plain-text 500 on the proxy path that never clears on retry.
		return reviewResult{kind: outcomeSubjectNotFound, resp: resp,
			err: clierr.New(clierr.CodeNotFound, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(resp.Body)), saTokenFailurePrefix)))}
	case resp.Status >= 500 && resp.Status <= 599:
		// Degradable: the endpoint exists and the request was well formed;
		// the failure is on the path or the server.
		return reviewResult{kind: outcomeDegradable, resp: resp, err: resp.Mapped}
	default:
		// Any other 4xx, and anything outside 4xx/5xx (a 3xx, a 1xx), is a
		// systemic endpoint or protocol problem: terminal.
		return reviewResult{kind: outcomeTerminal, resp: resp, err: resp.Mapped}
	}
}

// isRancherClusterAccessRefusal recognizes the proxy's answer for an
// identity without access to the cluster: HTTP 403 whose body is Rancher's
// error object ({"Code":{"Code":"Forbidden",...},"Message":"clusters.management.cattle.io
// \"<id>\" is forbidden: User \"<subject>\" cannot get resource \"clusters\" ..."}),
// served as text/plain, or an empty body. It is never a Kubernetes Status
// (no kind), which is how the authorizer's own 403 stays terminal.
func isRancherClusterAccessRefusal(body []byte) bool {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return true
	}
	var obj struct {
		Kind string `json:"kind"`
		Code struct {
			Code string `json:"Code"`
		} `json:"Code"`
		Message string `json:"Message"`
	}
	if json.Unmarshal(body, &obj) != nil || obj.Kind != "" {
		return false
	}
	return obj.Code.Code == "Forbidden" && strings.Contains(obj.Message, "clusters.management.cattle.io") && strings.Contains(obj.Message, "is forbidden")
}

// isRancherImpersonationRefusal recognizes the proxy's refusal body: plain
// text, or a small JSON object whose message field is the refusal, both
// matched exactly after trimming. A Kubernetes Status body is never the
// proxy's refusal, whatever its message mentions.
func isRancherImpersonationRefusal(body []byte) bool {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "{") {
		var obj struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &obj) != nil || obj.Kind == "Status" {
			return false
		}
		text = strings.TrimSpace(obj.Message)
	}
	return text == rancherImpersonateUserRefusal || text == rancherImpersonateSARefusal
}

// classifyAccess maps a decoded SubjectAccessReview or
// SelfSubjectAccessReview status: allowed stays allowed (an evaluation
// error becomes a warning upstream), not allowed with an evaluation error
// is unknown, not allowed without one is denied.
func classifyAccess(obj *unstructured.Unstructured) Outcome {
	allowed, _, _ := unstructured.NestedBool(obj.Object, "status", "allowed")
	denied, _, _ := unstructured.NestedBool(obj.Object, "status", "denied")
	reason, _, _ := unstructured.NestedString(obj.Object, "status", "reason")
	evalErr, _, _ := unstructured.NestedString(obj.Object, "status", "evaluationError")
	out := Outcome{Reason: reason, EvaluationError: evalErr, Denied: denied}
	switch {
	case allowed:
		out.Decision = DecisionAllowed
	case evalErr != "":
		out.Decision = DecisionUnknown
	default:
		out.Decision = DecisionDenied
	}
	return out
}

// identityReviewObject is the SelfSubjectReview the protocol posts.
func identityReviewObject() *unstructured.Unstructured {
	gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectIdentity)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind, "spec": map[string]any{},
	}}
}

// accessReviewObject builds a SelfSubjectAccessReview for a question in a
// namespace ("" for cluster scope or all namespaces).
func accessReviewObject(q Question, namespace string) *unstructured.Unstructured {
	gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": reviewSpec(q, namespace),
	}}
}

// subjectAccessReviewObject builds a SubjectAccessReview about user and
// groups.
func subjectAccessReviewObject(q Question, namespace, user string, groups []string) *unstructured.Unstructured {
	gvk, _ := engine.ReviewGVK(engine.ReviewSubjectAccess)
	spec := reviewSpec(q, namespace)
	if user != "" {
		spec["user"] = user
	}
	gs := make([]any, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g)
	}
	spec["groups"] = gs
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind, "spec": spec,
	}}
}

// rulesReviewObject builds a SelfSubjectRulesReview for a namespace.
func rulesReviewObject(namespace string) *unstructured.Unstructured {
	gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectRules)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": map[string]any{"namespace": namespace},
	}}
}

func reviewSpec(q Question, namespace string) map[string]any {
	if q.NonResourceURL != "" {
		return map[string]any{"nonResourceAttributes": map[string]any{"verb": q.Verb, "path": q.NonResourceURL}}
	}
	ra := map[string]any{"verb": q.Verb, "resource": q.Resource, "group": q.Group, "version": q.Version}
	if namespace != "" {
		ra["namespace"] = namespace
	}
	if q.Name != "" {
		ra["name"] = q.Name
	}
	if q.Subresource != "" {
		ra["subresource"] = q.Subresource
	}
	return map[string]any{"resourceAttributes": ra}
}
