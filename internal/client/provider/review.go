package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sync"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/client/kube"
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
	hreq, err := kube.NewWriteRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return engine.ReviewResponse{}, clierr.From(err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(hreq)
	if err != nil {
		// No HTTP response at all (refused, reset, TLS, EOF, deadline):
		// retryable by construction so a fan-out can degrade one member
		// instead of aborting; a protocol error below is not.
		return engine.ReviewResponse{}, clierr.Retryable(clierr.CodeGeneric, "no response from the cluster: "+err.Error()).WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReviewBody))
	if err != nil {
		return engine.ReviewResponse{}, clierr.New(clierr.CodeGeneric, "reading review response: "+err.Error())
	}
	out := engine.ReviewResponse{Status: resp.StatusCode, Body: body}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.Mapped = clierr.FromResponse("", http.MethodPost, resp.StatusCode, body, &clierr.Request{Cluster: r.clusterID, Server: r.server()})
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

// maxDiscoveryBytes bounds one aggregated discovery document; the
// largest real clusters answer well under a megabyte. A variable so a
// test can lower it.
var maxDiscoveryBytes int64 = 16 << 20

// ServerResources lists every served resource in its group's preferred
// version, subresources included as "<resource>/<sub>" entries. It asks
// for aggregated discovery first: two requests (/api, /apis) bound to
// ctx. A server whose answer to either root is not a 200
// APIGroupDiscoveryList is walked the legacy way instead (the local
// cluster of Rancher v2.15.0 and v2.15.1 does not serve aggregated
// discovery for /apis through the proxy: it answers 406 to this header,
// and a legacy APIGroupList to plain JSON): the plain root documents name
// the groups and their preferred versions, then one document per group
// version is read, each request bound to ctx. Either way a partial answer is returned as the
// partial list TOGETHER with an *engine.DiscoveryIncomplete naming the
// group versions the server could not describe (a stale aggregated
// entry, a 503 discovery document), so a caller can tell a complete
// inventory from a survivor. The caller's own cancellation passes through
// unmapped.
func (r *dynamicClient) ServerResources(ctx context.Context) ([]engine.APIResource, error) {
	ctx = kube.WithAnswers(ctx)
	if err := ctx.Err(); err != nil {
		return nil, clierr.WrapUnlessCanceled(err, r.discoveryErr(ctx))
	}
	if out, failed, ok := r.aggregatedResources(ctx); ok {
		return out, engine.ErrDiscoveryIncompleteFor(failed)
	}
	if err := ctx.Err(); err != nil {
		return nil, clierr.WrapUnlessCanceled(err, r.discoveryErr(ctx))
	}
	lists, failed, err := r.legacyResources(ctx)
	if err != nil {
		return nil, clierr.WrapUnlessCanceled(err, r.discoveryErr(ctx))
	}
	return engine.ResourcesFromLegacy(lists), engine.ErrDiscoveryIncompleteFor(failed)
}

// discoveryErr maps the error of a discovery request of the call ctx
// belongs to. The walk has no bound of its own, only its caller's, so a
// deadline error names no time.
func (r *dynamicClient) discoveryErr(ctx context.Context) func(error) error {
	return func(err error) error {
		return mapRequestErrWithin(ctx, http.MethodGet, 0, err, &clierr.Request{Cluster: r.clusterID, Endpoint: true, Server: r.server()})
	}
}

// legacyWorkers bounds the group version documents read at once on the
// legacy walk.
const legacyWorkers = 8

// legacyResources walks the pre-aggregation discovery documents: /api
// (APIVersions, the preferred version first) and /apis (APIGroupList
// with each group's preferred version), then /api/<v> and
// /apis/<group>/<version> for the preferred version of every group, in
// parallel on a bounded set of goroutines, every request bound to ctx.
// A root that fails is an error; a group version document that fails is
// recorded in failed and the walk goes on, so the answer is partial, not
// absent. Lists come back in the root documents' order.
func (r *dynamicClient) legacyResources(ctx context.Context) ([]*metav1.APIResourceList, map[schema.GroupVersion]error, error) {
	var versions metav1.APIVersions
	if _, err := r.getDiscoveryJSON(ctx, "/api", &versions); err != nil {
		return nil, nil, err
	}
	var groups metav1.APIGroupList
	if _, err := r.getDiscoveryJSON(ctx, "/apis", &groups); err != nil {
		return nil, nil, err
	}
	var gvs []schema.GroupVersion
	if len(versions.Versions) > 0 {
		gvs = append(gvs, schema.GroupVersion{Group: "", Version: versions.Versions[0]})
	}
	for _, g := range groups.Groups {
		version := g.PreferredVersion.Version
		if version == "" && len(g.Versions) > 0 {
			version = g.Versions[0].Version
		}
		if g.Name == "" || version == "" {
			continue
		}
		gvs = append(gvs, schema.GroupVersion{Group: g.Name, Version: version})
	}

	lists := make([]*metav1.APIResourceList, len(gvs))
	errs := make([]error, len(gvs))
	slots := make(chan struct{}, legacyWorkers)
	var wg sync.WaitGroup
	for i, gv := range gvs {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			path := "/apis/" + gv.Group + "/" + gv.Version
			if gv.Group == "" {
				path = "/api/" + gv.Version
			}
			var list metav1.APIResourceList
			// These requests run at the same time: each records its own
			// answer, and none stands for another that ran out of time.
			if _, err := r.getDiscoveryJSON(kube.WithNewAnswers(ctx), path, &list); err != nil {
				errs[i] = err
				return
			}
			if list.GroupVersion == "" {
				list.GroupVersion = gv.String()
			}
			lists[i] = &list
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	failed := map[schema.GroupVersion]error{}
	out := make([]*metav1.APIResourceList, 0, len(gvs))
	for i, gv := range gvs {
		if errs[i] != nil {
			failed[gv] = errs[i]
			continue
		}
		out = append(out, lists[i])
	}
	return out, failed, nil
}

// getDiscoveryJSON reads one discovery document as plain JSON over the
// client's own HTTP client with ctx on the request, bounded by
// maxDiscoveryBytes, and decodes it into dst. A non-2xx answer is an error
// naming the status and the body's first line (a dead aggregated group
// answers 503 "service unavailable").
func (r *dynamicClient) getDiscoveryJSON(ctx context.Context, path string, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.host+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	_ = resp.Body.Close()
	if rerr != nil {
		return resp.StatusCode, rerr
	}
	if int64(len(body)) > maxDiscoveryBytes {
		return resp.StatusCode, fmt.Errorf("discovery document %s exceeds %d bytes", path, maxDiscoveryBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("GET %s: %w", path, clierr.ResponseError(http.MethodGet, resp.StatusCode, body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return resp.StatusCode, fmt.Errorf("decoding discovery document %s: %w", path, err)
	}
	return resp.StatusCode, nil
}

// aggregatedResources fetches /api and /apis with the aggregated discovery
// Accept header over the client's own HTTP client (the one carrying the
// bearer and the redirect policy), each request bound to ctx. ok is false
// when either root does not answer 200 with an APIGroupDiscoveryList, or a
// request fails; the caller then decides between the legacy path and
// giving up on a cancelled context.
func (r *dynamicClient) aggregatedResources(ctx context.Context) ([]engine.APIResource, map[schema.GroupVersion]error, bool) {
	if r.hc == nil || r.host == "" {
		return nil, nil, false
	}
	var out []engine.APIResource
	failed := map[schema.GroupVersion]error{}
	for _, root := range []string{"/api", "/apis"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.host+root, nil)
		if err != nil {
			return nil, nil, false
		}
		req.Header.Set("Accept", engine.AggregatedDiscoveryAccept)
		resp, err := r.hc.Do(req)
		if err != nil {
			return nil, nil, false
		}
		// One byte past the cap tells a truncated body from one that fit:
		// a document cut at the cap could still decode and pass as a
		// complete inventory, so an oversized answer is not aggregated
		// discovery for our purposes.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
		_ = resp.Body.Close()
		if rerr != nil || int64(len(body)) > maxDiscoveryBytes || resp.StatusCode != http.StatusOK || !engine.IsAggregatedDiscovery(resp.Header.Get("Content-Type")) {
			return nil, nil, false
		}
		var doc apidiscoveryv2.APIGroupDiscoveryList
		if json.Unmarshal(body, &doc) != nil || doc.Kind != "APIGroupDiscoveryList" {
			return nil, nil, false
		}
		res, f := engine.ResourcesFromAggregated(&doc)
		out = append(out, res...)
		maps.Copy(failed, f)
	}
	return out, failed, true
}
