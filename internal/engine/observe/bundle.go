package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagTargets        = "targets"
	flagClusters       = "clusters"
	flagFilename       = "filename"
	defaultBundleTail  = 10000
	defaultBundleSince = 24 * time.Hour

	statusCollected            = "collected"
	statusSkippedNotApplicable = "skipped: not applicable"
)

// nowFunc is the support-bundle default filename's clock; overridden in
// tests for a deterministic name.
var nowFunc = time.Now

// settingsAllowlist is the ONLY settings support-bundle ever reads —
// diagnostic identifiers, never a full settings dump (cacerts, registry pull
// secrets, and friends are secret-adjacent and must never ship).
var settingsAllowlist = []string{
	"server-version",
	"server-url",
	"install-uuid",
	"agent-image",
	"agent-tls-mode",
	"system-agent-version",
	"k8s-version",
	"fleet-default-workspace-name",
}

type manifestItem struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster,omitempty"`
	Status  string `json:"status"`
}

type bundleManifest struct {
	CLIVersion    string         `json:"cliVersion"`
	ServerVersion string         `json:"serverVersion,omitempty"`
	Timestamp     string         `json:"timestamp"`
	Items         []manifestItem `json:"items"`
}

func errorStatus(err error) string { return fmt.Sprintf("error: %s", err) }

type supportBundleCmd struct {
	clients engine.ClusterClientFactory
	schema  func() any
}

// NewSupportBundle builds `support-bundle [--targets ...|all] [--clusters
// a,b|*] [--since <dur>] [--tail N=10000] [-f out]` (SB-1): packs every
// API-reachable diagnostic surface — curated component logs, cluster/node
// state, per-namespace events, and the CLI's own settings/features/schema
// snapshot — into one tar.gz, tri-state manifested so support can tell
// "not applicable" from "collector failed" at a glance.
func NewSupportBundle(clients engine.ClusterClientFactory, schema func() any) runtime.Command {
	return &supportBundleCmd{clients: clients, schema: schema}
}

func (c *supportBundleCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "support-bundle",
		Usage: "Pack the API-reachable triage plane into one tar.gz for support",
		Flags: []runtime.ArgSpec{
			{Name: flagTargets, Type: "string"},
			{Name: flagClusters, Type: "string"},
			{Name: flagSince, Type: "string"},
			{Name: flagTail, Type: "string"},
			{Name: flagFilename, Type: "string", Aliases: []string{"f"}},
		},
		CommandType: runtime.ReadOnly,
		Examples: []string{
			"rancher support-bundle -f bundle.tar.gz",
			"rancher support-bundle --targets agent,fleet-agent --clusters prod,staging",
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

type bundleResult struct {
	Path    string `json:"path"`
	Items   int    `json:"items"`
	Skipped int    `json:"skipped"`
	Errors  int    `json:"errors"`
	Bytes   int64  `json:"bytes"`
}

func (c *supportBundleCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	targets, err := parseTargetsFlag(in.String(flagTargets))
	if err != nil {
		return runtime.Result{}, err
	}
	tail, err := parseTailFlag(in.String(flagTail))
	if err != nil {
		return runtime.Result{}, err
	}
	since, err := parseSinceFlag(in.String(flagSince))
	if err != nil {
		return runtime.Result{}, err
	}
	if in.String(flagSince) == "" {
		secs := int64(defaultBundleSince.Seconds())
		since = &secs
	}
	if in.String(flagTail) == "" {
		n := int64(defaultBundleTail)
		tail = &n
	}
	path := in.String(flagFilename)
	if path == "" {
		path = fmt.Sprintf("rancher-support-%d.tar.gz", nowFunc().Unix())
	}

	local, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}

	clusters, discoveryErr, err := resolveBundleClusters(ctx, c.clients, local, in.String(flagClusters))
	if err != nil {
		return runtime.Result{}, err
	}

	var entries []ArchiveEntry
	var items []manifestItem

	if discoveryErr != nil {
		items = append(items, manifestItem{Name: "cluster-discovery", Status: errorStatus(discoveryErr)})
	}

	settingsValues, settingsData := collectSettings(ctx, local)
	entries = append(entries, ArchiveEntry{Path: "rancher/settings.json", Data: settingsData})
	items = append(items, manifestItem{Name: "rancher/settings.json", Status: statusCollected})

	featuresData, ferr := collectFeatures(ctx, local)
	items = append(items, manifestItem{Name: "rancher/features.json", Status: itemStatus(ferr)})
	if ferr == nil {
		entries = append(entries, ArchiveEntry{Path: "rancher/features.json", Data: featuresData})
	}

	schemaData, serr := json.Marshal(c.schema())
	items = append(items, manifestItem{Name: "rancher/schema.json", Status: itemStatus(serr)})
	if serr == nil {
		entries = append(entries, ArchiveEntry{Path: "rancher/schema.json", Data: schemaData})
	}

	versionData, _ := json.Marshal(map[string]string{
		"cliVersion":    cliVersionFrom(schemaData),
		"serverVersion": settingsValues["server-version"],
	})
	entries = append(entries, ArchiveEntry{Path: "rancher/version.json", Data: versionData})
	items = append(items, manifestItem{Name: "rancher/version.json", Status: statusCollected})

	for _, clusterID := range clusters {
		clusterEntries, clusterItems, namespaces := collectCluster(ctx, c.clients, local, clusterID, targets, tail, since)
		entries = append(entries, clusterEntries...)
		items = append(items, clusterItems...)

		for _, ns := range namespaces {
			data, eerr := collectEvents(ctx, c.clients, clusterID, ns)
			items = append(items, manifestItem{Name: "events-" + ns, Cluster: clusterID, Status: itemStatus(eerr)})
			if eerr == nil {
				entries = append(entries, ArchiveEntry{Path: fmt.Sprintf("clusters/%s/events-%s.json", clusterID, ns), Data: data})
			}
		}
	}

	entries = append(entries, ArchiveEntry{Path: "README.txt", Data: []byte(readmeText)})

	manifest := bundleManifest{
		CLIVersion:    cliVersionFrom(schemaData),
		ServerVersion: settingsValues["server-version"],
		Timestamp:     nowFunc().UTC().Format(time.RFC3339),
		Items:         items,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return runtime.Result{}, err
	}
	entries = append(entries, ArchiveEntry{Path: "manifest.json", Data: manifestData})

	bytesWritten, err := WriteArchive(path, entries)
	if err != nil {
		return runtime.Result{}, err
	}

	var skipped, errored int
	for _, it := range items {
		switch {
		case it.Status == statusSkippedNotApplicable:
			skipped++
		case strings.HasPrefix(it.Status, "error:"):
			errored++
		}
	}

	return runtime.Result{
		Kind: "SupportBundle",
		Data: bundleResult{Path: path, Items: len(items), Skipped: skipped, Errors: errored, Bytes: bytesWritten},
	}, nil
}

func itemStatus(err error) string {
	if err != nil {
		return errorStatus(err)
	}
	return statusCollected
}

// cliVersionFrom extracts "cliVersion" from the already-marshaled schema
// manifest JSON, keeping this package decoupled from runtime.Manifest's
// concrete type (the constructor's schema func returns `any`).
func cliVersionFrom(schemaJSON []byte) string {
	var v struct {
		CLIVersion string `json:"cliVersion"`
	}
	_ = json.Unmarshal(schemaJSON, &v)
	return v.CLIVersion
}

// parseTargetsFlag resolves --targets: "" or "all" means every target;
// otherwise a comma-separated list, each name validated against the table.
func parseTargetsFlag(raw string) ([]Target, error) {
	if raw == "" || raw == "all" {
		return Targets, nil
	}
	var out []Target
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		t, ok := Lookup(tok)
		if !ok {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown log target %q", tok)).
				WithHint(fmt.Sprintf("choose from: %s", strings.Join(Names(), ", ")))
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveBundleClusters resolves --clusters: "" or "*" discovers local plus
// every management Cluster the caller can list (via the already-fetched
// local client — the caller has no separate local-client failure mode to
// worry about, since it needed local before calling this anyway); an
// explicit comma-separated list is resolved token by token (the "local"
// literal short-circuits).
//
// The two error returns are NOT interchangeable. discoveryErr is a
// non-fatal signal on the default/"*" path: fleet-wide listing failed
// (RBAC denied listing Clusters, ...), so the returned clusters degrade to
// local-only — the caller must record discoveryErr as a manifest error
// entry rather than silently shipping a bundle that only covers local
// while claiming fleet coverage. err is fatal — an explicit --clusters
// token that fails to resolve aborts the whole command, same as any other
// VALIDATION-class failure.
func resolveBundleClusters(ctx context.Context, clients engine.ClusterClientFactory, local engine.ClusterClient, raw string) (clusters []string, discoveryErr error, err error) {
	if raw == "" || raw == "*" {
		ids := map[string]bool{engine.LocalCluster: true}
		list, lerr := local.List(ctx, clusterKind.GVR, "", "")
		if lerr != nil {
			return []string{engine.LocalCluster}, lerr, nil
		}
		for i := range list.Items {
			ids[list.Items[i].GetName()] = true
		}
		out := make([]string, 0, len(ids))
		for id := range ids {
			out = append(out, id)
		}
		sort.Strings(out)
		return out, nil, nil
	}
	var out []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "local" {
			out = append(out, engine.LocalCluster)
			continue
		}
		id, rerr := clients.ResolveCluster(ctx, tok)
		if rerr != nil {
			return nil, nil, rerr
		}
		out = append(out, id)
	}
	return out, nil, nil
}

// collectSettings fetches every allowlisted setting; a setting absent on
// this Rancher version is silently skipped (not every version ships every
// allowlisted setting) — never a full dump, and a missing setting is not a
// collector failure.
func collectSettings(ctx context.Context, local engine.ClusterClient) (map[string]string, []byte) {
	values := map[string]string{}
	for _, name := range settingsAllowlist {
		obj, err := local.Get(ctx, settingKind.GVR, "", name)
		if err != nil {
			continue
		}
		if v, found, _ := unstructured.NestedString(obj.Object, "value"); found && v != "" {
			values[name] = v
			continue
		}
		if v, found, _ := unstructured.NestedString(obj.Object, "default"); found {
			values[name] = v
		}
	}
	data, _ := json.Marshal(values)
	return values, data
}

// featureRow's bool fields carry NO `omitempty`: "off" (false) and "not
// collected" (field absent) must not serialize to the same bytes in a
// triage bundle — a support engineer reading features.json needs to see
// `"value":false` explicitly, not a missing key.
type featureRow struct {
	Name        string `json:"name"`
	Value       bool   `json:"value"`
	Default     bool   `json:"default"`
	Locked      bool   `json:"lockedValue"`
	Description string `json:"description,omitempty"`
}

func collectFeatures(ctx context.Context, local engine.ClusterClient) ([]byte, error) {
	list, err := local.List(ctx, featureKind.GVR, "", "")
	if err != nil {
		return nil, err
	}
	rows := make([]featureRow, 0, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].Object
		value, _, _ := unstructured.NestedBool(obj, "spec", "value")
		def, _, _ := unstructured.NestedBool(obj, "status", "default")
		locked, _, _ := unstructured.NestedBool(obj, "status", "lockedValue")
		desc, _, _ := unstructured.NestedString(obj, "status", "description")
		rows = append(rows, featureRow{Name: list.Items[i].GetName(), Value: value, Default: def, Locked: locked, Description: desc})
	}
	return json.Marshal(rows)
}

// collectCluster gathers one cluster's cluster.json/nodes.json plus every
// applicable target's logs, returning the archive entries, their manifest
// items, and the set of namespaces any attempted target resolved to (for
// the caller's per-namespace events collection) — cattle-system is always
// included, per the design spec.
func collectCluster(ctx context.Context, clients engine.ClusterClientFactory, local engine.ClusterClient, clusterID string, targets []Target, tail *int64, since *int64) ([]ArchiveEntry, []manifestItem, []string) {
	var entries []ArchiveEntry
	var items []manifestItem
	namespaces := map[string]bool{"cattle-system": true}

	if obj, err := local.Get(ctx, clusterKind.GVR, "", clusterID); err != nil {
		items = append(items, manifestItem{Name: "cluster.json", Cluster: clusterID, Status: errorStatus(err)})
	} else {
		data, _ := json.Marshal(map[string]any{
			"state":      firstString(obj.Object, "status", "state"),
			"conditions": firstSlice(obj.Object, "status", "conditions"),
		})
		entries = append(entries, ArchiveEntry{Path: fmt.Sprintf("clusters/%s/cluster.json", clusterID), Data: data})
		items = append(items, manifestItem{Name: "cluster.json", Cluster: clusterID, Status: statusCollected})
	}

	if client, err := clients.Cluster(ctx, clusterID); err != nil {
		items = append(items, manifestItem{Name: "nodes.json", Cluster: clusterID, Status: errorStatus(err)})
	} else {
		if list, err := client.List(ctx, nodeKind.GVR, "", ""); err != nil {
			items = append(items, manifestItem{Name: "nodes.json", Cluster: clusterID, Status: errorStatus(err)})
		} else {
			rows := make([]map[string]any, 0, len(list.Items))
			for i := range list.Items {
				rows = append(rows, map[string]any{
					"name":       list.Items[i].GetName(),
					"conditions": firstSlice(list.Items[i].Object, "status", "conditions"),
				})
			}
			data, _ := json.Marshal(rows)
			entries = append(entries, ArchiveEntry{Path: fmt.Sprintf("clusters/%s/nodes.json", clusterID), Data: data})
			items = append(items, manifestItem{Name: "nodes.json", Cluster: clusterID, Status: statusCollected})
		}
	}

	for _, t := range targets {
		if !Applicable(t, clusterID) {
			items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: statusSkippedNotApplicable})
			continue
		}
		resolved, err := ResolveOnCluster(ctx, clients, t, clusterID)
		if err != nil {
			items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: errorStatus(err)})
			continue
		}
		namespaces[resolved.Namespace] = true
		items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: statusCollected})
		entries = append(entries, collectTargetLogs(ctx, t, resolved, tail, since)...)
	}

	nsList := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		nsList = append(nsList, ns)
	}
	sort.Strings(nsList)
	return entries, items, nsList
}

// collectTargetLogs fetches every matched pod/container's current log, plus
// a best-effort previous-container log (silently omitted when there is
// none — a fresh container has never restarted, which is the common case,
// not a failure).
func collectTargetLogs(ctx context.Context, t Target, resolved Resolved, tail *int64, since *int64) []ArchiveEntry {
	var entries []ArchiveEntry
	for i := range resolved.Pods {
		podName := resolved.Pods[i].GetName()
		for _, cname := range engine.ContainerNames(resolved.Pods[i].Object) {
			base := fmt.Sprintf("logs/%s/%s/%s-%s", resolved.ClusterID, t.Name, podName, cname)
			if out, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since}); err == nil {
				entries = append(entries, ArchiveEntry{Path: base + ".log", Data: []byte(out)})
			}
			if out, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since, Previous: true}); err == nil {
				entries = append(entries, ArchiveEntry{Path: base + "-previous.log", Data: []byte(out)})
			}
		}
	}
	return entries
}

func collectEvents(ctx context.Context, clients engine.ClusterClientFactory, clusterID, namespace string) ([]byte, error) {
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	list, err := client.List(ctx, eventKind.GVR, namespace, "")
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		rows = append(rows, list.Items[i].Object)
	}
	return json.Marshal(rows)
}

func firstString(obj map[string]any, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj, fields...)
	return v
}

func firstSlice(obj map[string]any, fields ...string) []any {
	v, _, _ := unstructured.NestedSlice(obj, fields...)
	return v
}

const readmeText = `This is a Rancher CLI support bundle.

Contents:
  manifest.json            what was collected, skipped, or failed, and why
  logs/<cluster>/<target>/ curated component logs (see "rancher logs")
  clusters/<cluster>/      cluster/node state and per-namespace events
  rancher/                 CLI version, an allowlisted settings snapshot,
                            feature flags, and this CLI's own schema manifest

This bundle may contain sensitive log content (application output,
identifiers, timestamps) from the collected components. Review before
sharing outside your organization.

This bundle covers the API-reachable plane only. For node-level diagnostics
(journald, the rancher-system-agent unit, OS state) see
rancherlabs/support-tools' node collector — the two bundles are
complementary by design.
`
