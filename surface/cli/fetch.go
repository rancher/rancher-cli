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

// isValidSegment reports whether s is safe to interpolate into a REST path
// segment (finding 1). Reuses completionCharset: cluster IDs and namespace
// names are DNS-safe; a slash, dot-dot, or shell metacharacter fails here.
// A value failing this check must cause the whole live dispatch to return nil
// (fail closed, zero requests).
//
// Round-4 finding 1: bare-dot segments ("..", "...") satisfy the charset but
// path-normalize at any proxy layer (HTTP stacks strip dot segments), silently
// dropping the intended scope. Reject a segment composed entirely of dots so
// no ".", "..", "..." reaches a URL; the charset already blocks "/", so a
// single such segment is the only dot-normalization risk left.
func isValidSegment(s string) bool {
	if !completionCharset.MatchString(s) {
		return false
	}
	for _, c := range s {
		if c != '.' {
			return true
		}
	}
	return false // all-dots segment: ".", "..", "...", etc.
}

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
// tokens (AC-21a). sanitizeCandidates is applied to values first; it returns
// the values sorted, and the constant prefix preserves that order.
func equalsFormCandidates(spelling string, values []string) []string {
	safe := sanitizeCandidates(values)
	out := make([]string, 0, len(safe))
	for _, v := range safe {
		out = append(out, spelling+"="+v)
	}
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
func completionTimeout() time.Duration {
	return config.EffectiveCompletionTimeout(storedConfigSnapshot())
}

// completionNetworkEnabled reports whether live completion is configured
// (AC-1).
func completionNetworkEnabled() bool {
	cfg := storedConfigSnapshot()
	return cfg != nil && cfg.Completion.Network
}

// newCompletionClient builds an http.Client for completion requests. It
// is constructed directly from httpx, never through resolveDeps (AC-20/AC-22).
// Called exactly once per live dispatch (finding 9).
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
// hc must be the single per-dispatch client (finding 9).
func fetchClusters(ctx context.Context, hc *http.Client, cap TokenCapability, _ string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil // exec-cred-only: cluster list requires management token
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

// fetchProjects lists projects for the resolved cluster via the management K8s
// API and returns name/id candidates (AC-8, finding 10). Projects are listed in
// the cluster's namespace, exactly as internal/engine/projects/resolve.go does.
// Returns nil on any fetch error.
// hc must be the single per-dispatch client (finding 9).
func fetchProjects(ctx context.Context, hc *http.Client, cap TokenCapability, clusterID string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	if clusterID == "" {
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
func projectCandidates(items []map[string]any) []string {
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
	return dedupe(sanitizeCandidates(out))
}

// fetchRoleTemplates lists management RoleTemplates and returns the names
// of those whose top-level context field equals roleContext ("project" or
// "cluster"). Management list: requires a management token — exec-cred-only
// capabilities (IDOnly) return nil without a request, same boundary as
// fetchClusters/fetchProjects. The filter is client-side: the RoleTemplate
// CRD (management.cattle.io_roletemplates) declares no spec.selectableFields
// entry for the context field, so the Kubernetes fieldSelector mechanism only
// covers metadata.name and metadata.namespace — any context= filter must be
// applied after the full list is fetched.
func fetchRoleTemplates(ctx context.Context, hc *http.Client, cap TokenCapability, roleContext string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	var names []string
	for _, item := range col.Items {
		if rc, _ := item["context"].(string); rc != roleContext {
			continue
		}
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		if name, _ := meta["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return dedupe(sanitizeCandidates(names))
}

// fetchGlobalRoles lists management GlobalRoles and returns their names.
// Same management-token boundary as fetchRoleTemplates.
func fetchGlobalRoles(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	var names []string
	for _, item := range col.Items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		if name, _ := meta["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return dedupe(sanitizeCandidates(names))
}

// fetchK8sResources lists a Kubernetes resource in a cluster via the
// management proxy (/k8s/clusters/<id>/...) and returns object names (AC-9,
// AC-10, AC-11). ns may be "" for cluster-scoped resources. Returns nil on
// any error.
// hc must be the single per-dispatch client (finding 9).
func fetchK8sResources(ctx context.Context, hc *http.Client, cap TokenCapability, clusterID, apiPath, ns string) []string {
	if cap.Token == "" {
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

// fetchUsers lists management Users and returns candidates suitable for the
// grant/revoke subject positional. System users (any principalIds entry with
// prefix "system:") are excluded.
//
// Candidates:
//   - metadata.name id — always emitted; Get-by-id resolution is deterministic.
//   - username — emitted ONLY when it uniquely resolves: no other non-system user
//     carries the same string as their username OR displayName. A user whose own
//     displayName equals their own username is not double-counted (self-collision
//     does not suppress). This mirrors the ambiguity check in ResolveSubject so
//     that a candidate offered by completion will not fail with AMBIGUOUS_SELECTION
//     at execution time.
//
// Management list: requires a management token — exec-cred-only capabilities
// (IDOnly) return nil without a request, same boundary as fetchRoleTemplates.
func fetchUsers(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/users"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}

	type userEntry struct {
		id          string
		username    string
		displayName string
	}

	// First pass: collect non-system users, deduped by metadata.name id.
	seenID := map[string]bool{}
	var entries []userEntry
	for _, item := range col.Items {
		if isSystemUserItem(item) {
			continue
		}
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		if id == "" || seenID[id] {
			continue
		}
		seenID[id] = true
		username, _ := item["username"].(string)
		displayName, _ := item["displayName"].(string)
		entries = append(entries, userEntry{id: id, username: username, displayName: displayName})
	}

	// Build string frequency: count distinct users whose username OR displayName
	// equals each string. A user whose username == displayName contributes only
	// once to the count (set-per-user avoids double-counting self-collision).
	freq := make(map[string]int)
	for _, e := range entries {
		perUser := map[string]struct{}{}
		if e.username != "" {
			perUser[e.username] = struct{}{}
		}
		if e.displayName != "" {
			perUser[e.displayName] = struct{}{}
		}
		for s := range perUser {
			freq[s]++
		}
	}

	// Second pass: emit id unconditionally; emit username only when it uniquely
	// resolves (freq == 1: no other user would produce AMBIGUOUS_SELECTION).
	var names []string
	for _, e := range entries {
		names = append(names, e.id)
		if e.username != "" && freq[e.username] == 1 {
			names = append(names, e.username)
		}
	}
	return dedupe(sanitizeCandidates(names))
}

// isSystemUserItem reports whether the user item represents a system account:
// any entry in principalIds with the "system:" prefix.
func isSystemUserItem(item map[string]any) bool {
	pids, ok := item["principalIds"].([]interface{})
	if !ok {
		return false
	}
	for _, pid := range pids {
		if s, ok := pid.(string); ok && strings.HasPrefix(s, "system:") {
			return true
		}
	}
	return false
}

// dedupe removes duplicates from a sorted slice (input must be sorted).
func dedupe(s []string) []string {
	if len(s) == 0 {
		return s
	}
	// A fresh slice: appending into a shared backing array would silently
	// mutate the caller's input.
	out := []string{s[0]}
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
