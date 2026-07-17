package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
)

// completionCharset is the allowlist for candidate values (AC-21).
// First character must not be a hyphen (would parse as a flag).
var completionCharset = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// sanitizeCandidates drops values that do not conform to the shell-safe
// charset and returns the survivors lexicographically sorted (AC-21, AC-26).
func sanitizeCandidates(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if completionCharset.MatchString(c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// equalsFormCandidates reassembles value candidates as "--flag=<value>" full
// tokens (AC-21a). sanitizeCandidates is applied to values first; the
// assembled tokens are then sorted.
func equalsFormCandidates(spelling string, values []string) []string {
	safe := sanitizeCandidates(values)
	out := make([]string, 0, len(safe))
	for _, v := range safe {
		out = append(out, spelling+"="+v)
	}
	sort.Strings(out)
	return out
}

// resolveStaticEnum returns enum candidates for a flag-value site, sorted.
// Called for both space-form (bare values) and equals-form (full tokens).
func resolveStaticEnum(enum []string, site CompletionSite) []string {
	if site.EqualsForm {
		return equalsFormCandidates(site.FlagSpelling, enum)
	}
	return sanitizeCandidates(enum)
}

// completionTimeout returns the effective completion timeout from the stored
// config, delegating to config.EffectiveCompletionTimeout for default/floor/cap
// semantics (AC-2, finding 6). Hand-edited values outside [1ms, 2s] are clamped
// here at use rather than at write time.
func completionTimeout() time.Duration { //nolint:unused
	return config.EffectiveCompletionTimeout(storedConfigSnapshot())
}

// completionNetworkEnabled reports whether live completion is configured
// (AC-1).
func completionNetworkEnabled() bool { //nolint:unused
	cfg := storedConfigSnapshot()
	return cfg != nil && cfg.Completion.Network
}

// newCompletionClient builds an http.Client for completion requests. It
// is constructed directly from httpx, never through resolveDeps (AC-20/AC-22).
func newCompletionClient(caPEM string) (*http.Client, error) {
	return httpx.New(httpx.Options{CACert: caPEM})
}

// k8sItemsList is the minimal envelope for a Kubernetes-style list response
// (used by the Rancher management API — /k8s/clusters/... endpoints).
// Finding 10: Norman /v3 endpoints return {"data":[...]}, but the management K8s
// proxy returns the standard k8s {"items":[...]} envelope.
type k8sItemsList struct {
	Items []map[string]any `json:"items"`
}

// getJSON performs one authenticated GET and decodes the JSON body into dst.
// Returns a non-nil error on non-2xx status or any I/O failure (AC-18:
// caller degrades to empty on any error).
func getJSON(ctx context.Context, hc *http.Client, token, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	resp, body, err := httpx.Do(hc, req)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, dst)
}

// fetchClusters lists management clusters via the management K8s API and
// returns id+display-name candidates (AC-7, finding 10). Uses the standard
// Rancher management API path; decodes the k8s items envelope.
// Returns nil on any fetch error (AC-18).
func fetchClusters(ctx context.Context, cap TokenCapability, _ string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil // exec-cred-only: cluster list requires management token
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return nil
	}
	// Management K8s API: same convention as surface/cli/engine.go clusters().
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	return clusterCandidates(col.Items)
}

// clusterCandidates extracts cluster ids and uniquely-resolving display names
// from a list response (AC-7: same MatchByIDOrDisplay semantics).
func clusterCandidates(items []map[string]any) []string {
	type clusterEntry struct {
		id      string
		display string
	}
	entries := make([]clusterEntry, 0, len(items))
	for _, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		spec, _ := item["spec"].(map[string]any)
		display := ""
		if spec != nil {
			display, _ = spec["displayName"].(string)
		}
		entries = append(entries, clusterEntry{id: id, display: display})
	}
	// Build a set of ids and a display-name frequency map for uniqueness.
	idSet := map[string]bool{}
	displayFreq := map[string]int{}
	for _, e := range entries {
		idSet[e.id] = true
		if e.display != "" {
			displayFreq[e.display]++
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.id != "" && !seen[e.id] {
			seen[e.id] = true
			out = append(out, e.id)
		}
		// Include display name only when it uniquely resolves and does not
		// collide with any cluster id (MatchByIDOrDisplay semantics, AC-7).
		if e.display != "" && displayFreq[e.display] == 1 && !idSet[e.display] && !seen[e.display] {
			seen[e.display] = true
			out = append(out, e.display)
		}
	}
	return sanitizeCandidates(out)
}

// resolveClusterDisplayName resolves a display name to a cluster ID by
// fetching the cluster list and matching. Returns "" when not found or on
// any error. When clusterRef is already an ID (no match by display name),
// returns clusterRef unchanged.
func resolveClusterDisplayName(ctx context.Context, cap TokenCapability, clusterRef string) string {
	if clusterRef == "" {
		return ""
	}
	if cap.Token == "" || cap.IDOnly {
		return clusterRef
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return clusterRef
	}
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return clusterRef
	}
	for _, item := range col.Items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		spec, _ := item["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		display, _ := spec["displayName"].(string)
		if display == clusterRef {
			return id
		}
	}
	return clusterRef
}

// fetchProjects lists projects for the resolved cluster via the management K8s
// API and returns name/id candidates (AC-8, finding 10). Projects are listed in
// the cluster's namespace, exactly as internal/engine/projects/resolve.go does.
// Returns nil on any fetch error.
func fetchProjects(ctx context.Context, cap TokenCapability, clusterID string) []string { //nolint:unused
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	if clusterID == "" {
		return nil
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return nil
	}
	// Management K8s API: project GVR in the cluster's namespace
	// (mirrors internal/engine/projects/resolve.go:81-104).
	url := fmt.Sprintf("%s/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/%s/projects",
		cap.ServerURL, clusterID)
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	return projectCandidates(col.Items)
}

// projectCandidates extracts project ids and uniquely-resolving display names.
func projectCandidates(items []map[string]any) []string { //nolint:unused
	seen := map[string]bool{}
	displayFreq := map[string]int{}
	type pEntry struct{ id, display string }
	entries := make([]pEntry, 0, len(items))
	for _, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		spec, _ := item["spec"].(map[string]any)
		display := ""
		if spec != nil {
			display, _ = spec["displayName"].(string)
		}
		entries = append(entries, pEntry{id: id, display: display})
		if display != "" {
			displayFreq[display]++
		}
	}
	idSet := map[string]bool{}
	for _, e := range entries {
		idSet[e.id] = true
	}
	var out []string
	for _, e := range entries {
		if e.id != "" && !seen[e.id] {
			seen[e.id] = true
			out = append(out, e.id)
		}
		if e.display != "" && displayFreq[e.display] == 1 && !idSet[e.display] && !seen[e.display] {
			seen[e.display] = true
			out = append(out, e.display)
		}
	}
	return sanitizeCandidates(out)
}

// fetchK8sResources lists a Kubernetes resource in a cluster via the
// management proxy (/k8s/clusters/<id>/...) and returns object names (AC-9,
// AC-10, AC-11). ns may be "" for cluster-scoped resources. Returns nil on
// any error.
func fetchK8sResources(ctx context.Context, cap TokenCapability, clusterID, apiPath, ns string) []string {
	if cap.Token == "" {
		return nil
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/" + clusterID + apiPath
	if ns != "" {
		url = cap.ServerURL + "/k8s/clusters/" + clusterID +
			strings.Replace(apiPath, "{namespace}", ns, 1)
	}
	var col struct {
		Items []map[string]any `json:"items"`
	}
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	out := make([]string, 0, len(col.Items))
	for _, item := range col.Items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		name, _ := meta["name"].(string)
		if name != "" {
			out = append(out, name)
		}
	}
	return sanitizeCandidates(out)
}

// dedupe removes duplicates from a sorted slice (input must be sorted).
func dedupe(s []string) []string { //nolint:unused
	if len(s) == 0 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
