package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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
// Static enums are compiled-in literals, not server data, so the AC-21
// shell-safe charset does not gate them - "template={{.}}" must survive.
// Live candidates keep sanitizeCandidates at their own call sites.
func resolveStaticEnum(enum []string, site CompletionSite) []string {
	sorted := append([]string{}, enum...)
	sort.Strings(sorted)
	if site.EqualsForm {
		out := make([]string, 0, len(sorted))
		for _, v := range sorted {
			out = append(out, site.FlagSpelling+"="+v)
		}
		return out
	}
	return sorted
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
	return httpx.New(httpx.Options{CACert: caPEM, Timeout: completionTimeout()})
}

// k8sItemsList is the minimal envelope for a Kubernetes-style list response
// (used by the Rancher management API - /k8s/clusters/... endpoints).
// Finding 10: Norman /v3 endpoints return {"data":[...]}, but the management K8s
// proxy returns the standard k8s {"items":[...]} envelope.
type k8sItemsList struct {
	Items []map[string]any `json:"items"`
}

// getJSON performs one authenticated GET and decodes the JSON body into dst.
// Returns a non-nil error on non-2xx status or any I/O failure (AC-18:
// caller degrades to empty on any error).
func getJSON(ctx context.Context, hc *http.Client, token, url string, dst any) error {
	return getJSONWithAccept(ctx, hc, token, url, "application/json", dst)
}

// getJSONWithAccept is getJSON with a caller-chosen Accept header, for an
// endpoint that serves more than one document shape (the aggregated
// discovery roots). Same token boundary, timeout, and error contract.
func getJSONWithAccept(ctx context.Context, hc *http.Client, token, url, accept string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	start := time.Now()
	resp, body, err := httpx.Do(hc, req)
	if err != nil {
		completionDebugf("GET %s: %v (%s)", url, err, time.Since(start).Round(time.Millisecond))
		return err
	}
	completionDebugf("GET %s: HTTP %d, %d bytes (%s)", url, resp.StatusCode, len(body), time.Since(start).Round(time.Millisecond))
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
func fetchClusters(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil // exec-cred-only: cluster list requires management token
	}
	// Management K8s API: same convention as internal/cli/engine.go clusters().
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	return clusterCandidates(col.Items)
}

// fetchSteveClusters lists the clusters the caller can see through the steve
// collection access projects resolves --cluster against; the management
// LIST fetchClusters reads is refused for callers who are not administrators.
// The read is the command's own single-pass reader over the production
// root client, so wherever the command would refuse the collection the
// completer offers nothing, the local row follows the command's rule, and
// unique display names are decided over the whole set.
func fetchSteveClusters(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	rows, err := access.ListClustersOnce(ctx, &rootClient{base: cap.ServerURL, token: cap.Token, hc: hc})
	if err != nil {
		completionDebugf("steve clusters: %v", err)
		return nil
	}
	items := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": c.ID},
			"spec":     map[string]any{"displayName": c.DisplayName},
		})
	}
	return clusterCandidates(items)
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

// fetchPods lists pods in namespace, filtered by selector, via the
// management K8s proxy - for `logs --pod` completion (logsPodCandidates).
// namespace and selector come from the curated observe.Targets table, never
// user input, so no path-injection guard is needed on them the way
// isValidSegment guards clusterID. Returns nil on any error.
func fetchPods(ctx context.Context, hc *http.Client, cap TokenCapability, clusterID, namespace, selector string) []string {
	apiPath := "/api/v1/namespaces/{namespace}/pods"
	if selector != "" {
		apiPath += "?labelSelector=" + neturl.QueryEscape(selector)
	}
	return fetchK8sResources(ctx, hc, cap, clusterID, apiPath, namespace)
}

// fetchCloudCredentials lists the Secrets in cattle-global-data and returns
// candidates for create cluster's --cloud-credential flag. The conventions
// are mirrored from internal/engine/provisioning/cloudcred.go - the same
// path create cluster's Plan resolves the flag through - because completion
// reads over raw HTTP (AC-20/AC-22), never a ClusterClient: the fixed
// cattle-global-data namespace, the "cc-" secret-name prefix, the
// field.cattle.io/name display annotation, and the "credentialConfig-"
// data-key marker (fetchProjects mirrors projects/resolve.go the same way).
//
// SECRET HYGIENE (cloudcred.go spec D-R2-1): only metadata and `data` KEY
// NAMES are read - a `data` VALUE must never flow into a candidate.
//
// Candidates:
//   - secret name (cc-xxxxx id) - always; ResolveCloudCredential's exact-ID
//     fast path accepts it deterministically.
//   - display name - ONLY when it resolves unambiguously (MatchOneOrError's
//     doctrine): unique among the credentials' display names and not equal
//     to any credential id. A suggestion must never fail with
//     AMBIGUOUS_SELECTION at execution time.
//
// Management list: requires a management token - exec-cred-only capabilities
// (IDOnly) return nil without a request, same boundary as fetchClusters.
func fetchCloudCredentials(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
	return cloudCredentialCandidates(col.Items)
}

// cloudCredentialCandidates extracts credential ids and uniquely-resolving
// display names from a cattle-global-data secrets list. Reads metadata and
// data KEY NAMES only - never a data value (see fetchCloudCredentials).
func cloudCredentialCandidates(items []map[string]any) []string {
	type ccEntry struct{ id, display string }
	var entries []ccEntry
	seenID := map[string]bool{}
	for _, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		if id == "" || seenID[id] {
			continue
		}
		// Credential-shaped only (cloudcred.go credentialObjects): named
		// cc-*, or carrying a *credentialConfig-* data KEY.
		if !strings.HasPrefix(id, "cc-") && !hasCredentialConfigKey(item) {
			continue
		}
		seenID[id] = true
		display := ""
		if anns, _ := meta["annotations"].(map[string]any); anns != nil {
			display, _ = anns["field.cattle.io/name"].(string)
		}
		entries = append(entries, ccEntry{id: id, display: display})
	}

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
		if !seen[e.id] {
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

// hasCredentialConfigKey scans `data` KEY NAMES ONLY - never a value - for
// the credentialConfig- marker (cloudcred.go driverFromDataKeys).
func hasCredentialConfigKey(item map[string]any) bool {
	data, _ := item["data"].(map[string]any)
	for k := range data {
		if strings.Contains(k, "credentialConfig-") {
			return true
		}
	}
	return false
}

// userEntry is a single non-system management user returned by fetchUserEntries.
type userEntry struct {
	id          string
	username    string
	displayName string
}

// fetchUserEntries fetches the management user list and returns all non-system
// users deduped by metadata.name id. Both fetchUsers and fetchUserIDs ride
// this first pass. Returns nil on any error or when the capability boundary
// prevents the request.
func fetchUserEntries(ctx context.Context, hc *http.Client, cap TokenCapability) []userEntry {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/users"
	var col k8sItemsList
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return nil
	}
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
	return entries
}

// fetchUsers lists management Users and returns candidates suitable for the
// grant/revoke subject positional. System users (any principalIds entry with
// prefix "system:") are excluded.
//
// Candidates:
//   - metadata.name id - always emitted; Get-by-id resolution is deterministic.
//   - username - emitted ONLY when it uniquely resolves: no other non-system user
//     carries the same string as their username OR displayName. A user whose own
//     displayName equals their own username is not double-counted (self-collision
//     does not suppress). This mirrors the ambiguity check in ResolveSubject so
//     that a candidate offered by completion will not fail with AMBIGUOUS_SELECTION
//     at execution time.
//
// Management list: requires a management token - exec-cred-only capabilities
// (IDOnly) return nil without a request, same boundary as fetchClusters.
func fetchUsers(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	entries := fetchUserEntries(ctx, hc, cap)
	if entries == nil {
		return nil
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

	// Emit id unconditionally; emit username only when it uniquely resolves
	// (freq == 1: no other user would produce AMBIGUOUS_SELECTION).
	var names []string
	for _, e := range entries {
		names = append(names, e.id)
		if e.username != "" && freq[e.username] == 1 {
			names = append(names, e.username)
		}
	}
	return dedupe(sanitizeCandidates(names))
}

// fetchUserIDs lists management Users and returns only their metadata.name ids.
// Used for kind-positional completion (get/delete/patch user) where the command
// addresses users by metadata.name only - bare usernames are rejected verbatim.
// System-account filtering matches fetchUsers. Management-token boundary: same
// as fetchUsers (IDOnly or no token → nil, zero requests).
func fetchUserIDs(ctx context.Context, hc *http.Client, cap TokenCapability) []string {
	entries := fetchUserEntries(ctx, hc, cap)
	if entries == nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.id)
	}
	return sanitizeCandidates(ids)
}

// isSystemUserItem reports whether the user item represents a system account:
// any entry in principalIds that engine.IsSystemPrincipal matches.
func isSystemUserItem(item map[string]any) bool {
	pids, ok := item["principalIds"].([]any)
	if !ok {
		return false
	}
	for _, pid := range pids {
		if s, ok := pid.(string); ok && engine.IsSystemPrincipal(s) {
			return true
		}
	}
	return false
}

// fetchOIDCClientSecretNames GETs the single named OIDCClient and returns
// its status.clientSecrets key names - the dependent secret-name
// completion for oidc-client rotate-secret/remove-secret. Requires a full
// management token, same boundary as the client-name completion above; any
// fetch error (including a not-found client) degrades to nil.
func fetchOIDCClientSecretNames(ctx context.Context, hc *http.Client, cap TokenCapability, k kinds.Kind, clientName string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local" + gvrPath(k) + "/" + clientName
	var obj struct {
		Status struct {
			ClientSecrets map[string]any `json:"clientSecrets"`
		} `json:"status"`
	}
	if err := getJSON(ctx, hc, cap.Token, url, &obj); err != nil {
		return nil
	}
	names := make([]string, 0, len(obj.Status.ClientSecrets))
	for name := range obj.Status.ClientSecrets {
		names = append(names, name)
	}
	return sanitizeCandidates(names)
}

// fetchClusterFamilyPoolNames GETs the single named provisioning Cluster in
// ns and returns its spec.rkeConfig.machinePools[].name entries - the
// dependent --pool completion every cluster-family verb shares, keyed off
// the typed <cluster> positional. Any fetch error (including a not-found
// cluster) degrades to nil, same as fetchOIDCClientSecretNames.
func fetchClusterFamilyPoolNames(ctx context.Context, hc *http.Client, cap TokenCapability, k kinds.Kind, ns, clusterName string) []string {
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	url := cap.ServerURL + "/k8s/clusters/local" + strings.Replace(gvrNamespacedPath(k), "{namespace}", ns, 1) + "/" + clusterName
	var obj struct {
		Spec struct {
			RKEConfig struct {
				MachinePools []struct {
					Name string `json:"name"`
				} `json:"machinePools"`
			} `json:"rkeConfig"`
		} `json:"spec"`
	}
	if err := getJSON(ctx, hc, cap.Token, url, &obj); err != nil {
		return nil
	}
	names := make([]string, 0, len(obj.Spec.RKEConfig.MachinePools))
	for _, p := range obj.Spec.RKEConfig.MachinePools {
		names = append(names, p.Name)
	}
	return sanitizeCandidates(names)
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

// fetchBaseResources lists the cluster's served base resources (no
// "<resource>/<sub>" entries) by plural name, core and grouped alike.
func fetchBaseResources(ctx context.Context, hc *http.Client, cap TokenCapability, cluster string) []string {
	var out []string
	for _, r := range fetchDiscovery(ctx, hc, cap, cluster) {
		if !strings.Contains(r.Name, "/") {
			out = append(out, r.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// fetchSubresources resolves the typed resource token with the command's
// own rules (plural, singular, short name, resource.group, --group; core
// wins a bare-name tie; a non-core tie offers nothing) and returns that
// exact resource's subresources, so every suggestion is valid verbatim.
func fetchSubresources(ctx context.Context, hc *http.Client, cap TokenCapability, cluster, resource, group string) []string {
	if resource == "" {
		return nil
	}
	ref, err := access.ResolveResourceIn(fetchDiscovery(ctx, hc, cap, cluster), resource, group)
	if err != nil {
		return nil
	}
	return slices.Clone(ref.Subresources)
}

// fetchAPIGroups lists API group names for --group (the core group has
// no name and is not offered).
func fetchAPIGroups(ctx context.Context, hc *http.Client, cap TokenCapability, cluster string) []string {
	var out []string
	for _, r := range fetchDiscovery(ctx, hc, cap, cluster) {
		if r.Group != "" {
			out = append(out, r.Group)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// fetchDiscovery returns the cluster's served resources in the port's
// shape from exactly two requests: GET /api (core) and GET /apis (every
// group), both with the aggregated discovery Accept header, through
// /k8s/clusters/<cluster>. Each answer is the preferred version's
// resources per group, with "<resource>/<sub>" entries for
// subresources, exactly what the provider's ServerResources yields for
// the command. A server that answers either root with anything but an
// APIGroupDiscoveryList contributes nothing (completion fails closed;
// the command itself still works through client-go discovery). No
// per-group fallback: the discovery itself is two requests, and a
// display-name --cluster adds the one resolution request the layer
// already makes for every cluster-dependent site (three in all; see the
// liveCandidates doc comment).
func fetchDiscovery(ctx context.Context, hc *http.Client, cap TokenCapability, cluster string) []engine.APIResource {
	if cap.Token == "" || cap.IDOnly || !isValidSegment(cluster) {
		return nil
	}
	var out []engine.APIResource
	for _, root := range []string{"/api", "/apis"} {
		var doc apidiscoveryv2.APIGroupDiscoveryList
		url := cap.ServerURL + "/k8s/clusters/" + cluster + root
		if err := getJSONWithAccept(ctx, hc, cap.Token, url, engine.AggregatedDiscoveryAccept, &doc); err != nil || doc.Kind != "APIGroupDiscoveryList" {
			continue
		}
		// Completion offers only what the server could describe: a stale
		// group's resources are dropped with the failure, since a suggestion
		// must be valid verbatim.
		res, _ := engine.ResourcesFromAggregated(&doc)
		out = append(out, res...)
	}
	return out
}
