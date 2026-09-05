package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// maxReviewBody bounds what a review answer may carry; a rules review on a
// large cluster stays well under it.
const maxReviewBody = 4 << 20

// PostReview posts one review object and preserves the answer. See
// engine.ClusterClient for the contract.
func (r *dynamicClient) PostReview(ctx context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	gvr, ok := engine.ReviewGVR(req.Kind)
	if !ok {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%q is not a review kind; only authorization and authentication reviews may be posted", req.Kind))
	}
	want, _ := engine.ReviewGVK(req.Kind)
	if req.Object == nil {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeValidation, "review object is nil")
	}
	if got := req.Object.GroupVersionKind(); got != want {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("review object kind %s does not match review kind %s (%s)", got.String(), req.Kind, want.String()))
	}
	payload, err := json.Marshal(req.Object.Object)
	if err != nil {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeGeneric, "encoding review: "+err.Error())
	}
	url := r.host + "/apis/" + gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
	ctx, cancel := opCtx(ctx)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeGeneric, err.Error())
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(hreq)
	if err != nil {
		// No HTTP response at all (refused, reset, TLS, EOF, deadline):
		// retryable by construction so a fan-out can degrade one member
		// instead of aborting; a protocol error below is not.
		return engine.ReviewResponse{}, clierr.Retryable(clierr.CodeGeneric, "no response from the cluster: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReviewBody))
	if err != nil {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeGeneric, "reading review response: "+err.Error())
	}
	out := engine.ReviewResponse{Status: resp.StatusCode, Body: body}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.Mapped = mapStatusBody(resp.StatusCode, body)
		return out, nil
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(body); err != nil {
		return engine.ReviewResponse{}, protocolError("review response is not valid JSON: " + err.Error())
	}
	if got := obj.GroupVersionKind(); got != want {
		return engine.ReviewResponse{}, protocolError(fmt.Sprintf("review response kind %s, want %s", got.String(), want.String()))
	}
	if err := validateReviewStatus(req.Kind, obj); err != nil {
		return engine.ReviewResponse{}, err
	}
	out.Object = obj
	return out, nil
}

func protocolError(msg string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, msg)
}

// validateReviewStatus checks the fields a review kind must carry on the
// untyped object, so an absent boolean is never read as false later.
func validateReviewStatus(kind engine.ReviewKind, obj *unstructured.Unstructured) error {
	status, ok, _ := unstructured.NestedMap(obj.Object, "status")
	if !ok {
		return protocolError("review response has no status")
	}
	isBool := func(k string) bool { _, ok := status[k].(bool); return ok }
	isStringOrAbsent := func(k string) bool { v, ok := status[k]; _, s := v.(string); return !ok || s }
	switch kind {
	case engine.ReviewSubjectAccess, engine.ReviewSelfSubjectAccess:
		if !isBool("allowed") {
			return protocolError("review response status.allowed is missing or not a boolean")
		}
		if v, ok := status["denied"]; ok {
			if _, b := v.(bool); !b {
				return protocolError("review response status.denied is not a boolean")
			}
		}
		if !isStringOrAbsent("reason") || !isStringOrAbsent("evaluationError") {
			return protocolError("review response status.reason or evaluationError is not a string")
		}
	case engine.ReviewSelfSubjectRules:
		if !isBool("incomplete") {
			return protocolError("rules review status.incomplete is missing or not a boolean")
		}
		if err := rulesWellFormed(status, "resourceRules", "verbs", "apiGroups", "resources", "resourceNames"); err != nil {
			return err
		}
		if err := rulesWellFormed(status, "nonResourceRules", "verbs", "nonResourceURLs"); err != nil {
			return err
		}
	case engine.ReviewSelfSubjectIdentity:
		name, _, _ := unstructured.NestedString(obj.Object, "status", "userInfo", "username")
		if name == "" {
			return protocolError("identity review status.userInfo.username is missing or empty")
		}
		info, _, _ := unstructured.NestedMap(obj.Object, "status", "userInfo")
		if !stringArrayOrAbsent(info, "groups") {
			return protocolError("identity review status.userInfo.groups is not an array of strings")
		}
	}
	return nil
}

// rulesWellFormed checks that status[key], when present, is an array of
// objects whose listed fields, when present, are arrays of strings. The
// engine's canonicalization later reads exactly those fields, so nothing
// malformed can be silently dropped there.
func rulesWellFormed(status map[string]any, key string, fields ...string) error {
	v, ok := status[key]
	if !ok {
		return nil
	}
	rules, ok := v.([]any)
	if !ok {
		return protocolError("rules review status." + key + " is not an array")
	}
	for i, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			return protocolError(fmt.Sprintf("rules review status.%s[%d] is not an object", key, i))
		}
		for _, f := range fields {
			if !stringArrayOrAbsent(rule, f) {
				return protocolError(fmt.Sprintf("rules review status.%s[%d].%s is not an array of strings", key, i, f))
			}
		}
	}
	return nil
}

// stringArrayOrAbsent reports whether m[key] is absent or an array whose
// every element is a string.
func stringArrayOrAbsent(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return true
	}
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if _, s := e.(string); !s {
			return false
		}
	}
	return true
}

// mapStatusBody maps a non-2xx review answer the way mapErr maps a
// client-go error: a Kubernetes Status body becomes the equivalent
// StatusError; anything else is classified by HTTP status and body text.
// The transport status is authoritative: a Status body whose code field
// disagrees with the HTTP status is mapped by the HTTP status.
func mapStatusBody(status int, body []byte) *clierr.Error {
	var st metav1.Status
	if len(body) > 0 && json.Unmarshal(body, &st) == nil && st.Kind == "Status" {
		// A reason is a known string apierrors classifies on ahead of the
		// code, so a body disagreeing with the transport is stripped of it
		// and classified by the HTTP status alone; an agreeing body keeps
		// its reason and maps exactly as a client-go StatusError would.
		if st.Code != int32(status) {
			st.Reason = ""
			st.Code = int32(status)
		}
		ce, _ := errors.AsType[*clierr.Error](mapErr(&apierrors.StatusError{ErrStatus: st}))
		return ce
	}
	text := strings.TrimSpace(string(body))
	switch {
	case status == http.StatusUnauthorized:
		return withDeniedFallback(clierr.New(clierr.CodeAuth, text), status)
	case status == http.StatusForbidden:
		return withDeniedFallback(clierr.New(clierr.CodeRBAC, text), status)
	case status == http.StatusNotFound:
		return clierr.New(clierr.CodeNotFound, nonEmpty(text, "not found"))
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return clierr.Retryable(clierr.CodeGeneric, nonEmpty(text, http.StatusText(status))+" - retrying may succeed")
	case status == http.StatusInternalServerError:
		// The body SHAPE classifies, as in mapErr: an apiserver answers
		// 500 with a Kubernetes Status (handled above, non-retryable); a
		// plain-text 500 can only have come from the proxy path, whose
		// failures (agent disconnected, tunnel reset) clear on retry. The
		// substring below refines the wording only, never the class.
		if strings.Contains(text, "cluster agent disconnected") {
			return clierr.Retryable(clierr.CodeGeneric, text+" - retry once the cluster reconnects")
		}
		return clierr.Retryable(clierr.CodeGeneric, nonEmpty(text, http.StatusText(status))+" - often a proxy-path failure; retrying may succeed")
	case status >= 400 && status < 500:
		return clierr.New(clierr.CodeValidation, nonEmpty(text, http.StatusText(status)))
	default:
		return clierr.New(clierr.CodeGeneric, nonEmpty(text, http.StatusText(status)))
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ServerResources lists the preferred version of every served resource.
// A partial discovery answer (one aggregated API down) is returned as the
// partial list without error, as kubectl does; the missing group's
// resources are then unknown to resolution.
func (r *dynamicClient) ServerResources(ctx context.Context) ([]engine.APIResource, error) {
	lists, err := r.disc.ServerPreferredResources()
	if err != nil && len(lists) == 0 {
		return nil, mapErr(err)
	}
	var out []engine.APIResource
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, res := range l.APIResources {
			out = append(out, engine.APIResource{
				Group: gv.Group, Version: gv.Version, Name: res.Name, SingularName: res.SingularName,
				Kind: res.Kind, ShortNames: res.ShortNames, Namespaced: res.Namespaced, Verbs: res.Verbs,
			})
		}
	}
	return out, nil
}
