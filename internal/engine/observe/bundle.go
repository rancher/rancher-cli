package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagTargets        = "targets"
	flagClusters       = "clusters"
	flagOutputFile     = "output-file"
	defaultBundleTail  = 10000
	defaultBundleSince = 24 * time.Hour

	statusCollected            = "collected"
	statusSkippedNotApplicable = "skipped: not applicable"
)

// settingsAllowlist is the ONLY settings support-bundle ever reads -
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

// errorStatus is the manifest status of a failed collector. The manifest is
// written into the archive, which no invocation expands remedy commands in,
// so the remedy markers an error message carries are removed here.
func errorStatus(err error) string {
	return clierr.ExpandRemedies(fmt.Sprintf("error: %s", err), "")
}

type supportBundleCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	schema  func() any
	now     func() time.Time
}

// NewSupportBundle builds `support-bundle [--targets ...|all] [--clusters
// a,b|*] [--since <dur>] [--tail N=10000] [--output-file out]`: packs every
// API-reachable diagnostic surface - curated component logs, cluster/node
// state, per-namespace events, and the CLI's own settings/features/schema
// snapshot - into one tar.gz, tri-state manifested so support can tell
// "not applicable" from "collector failed" at a glance.
func NewSupportBundle(clients engine.ClusterClientFactory, roots engine.RootClientFactory, schema func() any) runtime.Command {
	return &supportBundleCmd{clients: clients, roots: roots, schema: schema, now: time.Now}
}

func (c *supportBundleCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "support-bundle",
		Usage: "Collect diagnostics reachable through the API into one tar.gz for support",
		Flags: []runtime.ArgSpec{
			{Name: flagTargets, Type: "string", Desc: "comma-separated log `targets` (" + strings.Join(Names(), ", ") + "), or all (the default)"},
			{Name: flagClusters, Type: "string", Desc: "downstream cluster ids/names to include (comma-separated)"},
			{Name: flagSince, Type: "string", Desc: "collect logs newer than this `duration` (default 24h)"},
			{Name: flagTail, Type: "string", Desc: "max log `lines` per target (default 10000)"},
			{Name: flagOutputFile, Type: "string", Desc: "write the tar.gz archive to a new `file`; an existing file or - is refused"},
		},
		CommandType: runtime.ReadOnly,
		Examples: []string{
			"rancher support-bundle --output-file bundle.tar.gz",
			"rancher support-bundle --targets agent,fleet-agent --clusters prod,staging",
		},
		Auth:  true,
		Shape: runtime.ShapeText,
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
	path := in.String(flagOutputFile)
	if path == "" {
		path = fmt.Sprintf("rancher-support-%d.tar.gz", c.now().Unix())
	}
	if path == "-" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			`--output-file "-": support-bundle writes the archive to a file, not to stdout; pass a file path`)
	}
	if err := refuseExistingArchive(path); err != nil {
		return runtime.Result{}, err
	}

	local, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}

	get := steve.RootFactoryGetter(c.roots)
	clusters, discoveryErr, err := resolveBundleClusters(ctx, c.clients, get, in.String(flagClusters))
	if err != nil {
		return runtime.Result{}, err
	}

	bw, err := CreateArchive(path)
	if err != nil {
		return runtime.Result{}, err
	}

	var items []manifestItem

	if discoveryErr != nil {
		items = append(items, manifestItem{Name: "cluster-discovery", Status: errorStatus(discoveryErr)})
	}

	settingsValues, settingsData := collectSettings(ctx, local)
	_ = bw.Add(ArchiveEntry{Path: "rancher/settings.json", Data: settingsData})
	items = append(items, manifestItem{Name: "rancher/settings.json", Status: statusCollected})

	featuresData, ferr := collectFeatures(ctx, local)
	items = append(items, manifestItem{Name: "rancher/features.json", Status: itemStatus(ferr)})
	if ferr == nil {
		_ = bw.Add(ArchiveEntry{Path: "rancher/features.json", Data: featuresData})
	}

	schemaData, serr := json.Marshal(c.schema())
	items = append(items, manifestItem{Name: "rancher/schema.json", Status: itemStatus(serr)})
	if serr == nil {
		_ = bw.Add(ArchiveEntry{Path: "rancher/schema.json", Data: schemaData})
	}

	// Partial re-decode of the already-marshaled manifest, on purpose: it
	// reads the one field without coupling observe to runtime.Manifest's
	// concrete type. Failure leaves the version empty; the bundle is not
	// worth failing over a version stamp.
	var cliBuildVersion struct {
		CLIVersion string `json:"cliVersion"`
	}
	_ = json.Unmarshal(schemaData, &cliBuildVersion)
	cliVer := cliBuildVersion.CLIVersion

	versionData, _ := json.Marshal(map[string]string{
		"cliVersion":    cliVer,
		"serverVersion": settingsValues["server-version"],
	})
	_ = bw.Add(ArchiveEntry{Path: "rancher/version.json", Data: versionData})
	items = append(items, manifestItem{Name: "rancher/version.json", Status: statusCollected})

	var warnings []string
	for _, clusterID := range clusters {
		clusterItems, namespaces, clusterWarnings := collectCluster(ctx, c.clients, get, clusterID, targets, tail, since, bw)
		items = append(items, clusterItems...)
		warnings = append(warnings, clusterWarnings...)

		for _, ns := range namespaces {
			data, total, included, eerr := collectEvents(ctx, c.clients, clusterID, ns)
			status := itemStatus(eerr)
			if eerr == nil && included < total {
				status = fmt.Sprintf("collected (truncated to %d of %d, newest first)", included, total)
			}
			items = append(items, manifestItem{Name: "events-" + ns, Cluster: clusterID, Status: status})
			if eerr == nil {
				_ = bw.Add(ArchiveEntry{Path: fmt.Sprintf("clusters/%s/events-%s.json", clusterID, ns), Data: data})
			}
		}
	}

	_ = bw.Add(ArchiveEntry{Path: "README.txt", Data: []byte(readmeText)})

	manifest := bundleManifest{
		CLIVersion:    cliVer,
		ServerVersion: settingsValues["server-version"],
		Timestamp:     c.now().UTC().Format(time.RFC3339),
		Items:         items,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		bw.Discard()
		return runtime.Result{}, err
	}
	_ = bw.Add(ArchiveEntry{Path: "manifest.json", Data: manifestData})

	bytesWritten, err := bw.Close()
	if err != nil {
		return runtime.Result{}, err
	}

	var skipped, errored int
	for _, it := range items {
		switch {
		case strings.HasPrefix(it.Status, "skipped:"):
			skipped++
		case strings.HasPrefix(it.Status, "error:"):
			errored++
		}
	}

	res := renderBundleText(path, len(items), bytesWritten, skipped, errored)
	res.Kind = "SupportBundle"
	res.Data = bundleResult{Path: path, Items: len(items), Skipped: skipped, Errors: errored, Bytes: bytesWritten}
	res.Warnings = warnings
	return res, nil
}

// renderBundleText builds support-bundle's default-format text: the written
// path, an items/size summary (size IEC-formatted for operators - the raw
// byte count stays in Data for -o json), and a last line naming skipped/
// errored counts, present only when either is non-zero.
func renderBundleText(path string, items int, bytesWritten int64, skipped, errored int) runtime.Result {
	lines := []string{fmt.Sprintf("%d items, %s", items, formatIECBytes(bytesWritten))}
	if skipped != 0 || errored != 0 {
		lines = append(lines, fmt.Sprintf("skipped: %d, errors: %d", skipped, errored))
	}
	return runtime.TextLines("support bundle written: "+path, lines...)
}

// formatIECBytes renders n using IEC binary units (KiB/MiB/GiB/...), one
// decimal place, e.g. "3.2 MiB" - plain "<n> B" below 1024. Unit selection
// accounts for the print-time rounding: a value %.1f would round up to
// "1024.0" (anything >= 1023.95 in the current unit) rolls into the next
// unit instead, so 1048575 renders "1.0 MiB", never "1024.0 KiB". The roll
// is capped at the table's last unit.
func formatIECBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	const units = "KMGTPE"
	v, exp := float64(n)/unit, 0
	for v >= 1023.95 && exp < len(units)-1 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, units[exp])
}

func itemStatus(err error) string {
	if err != nil {
		return errorStatus(err)
	}
	return statusCollected
}

// parseTargetsFlag resolves --targets: "" or "all" means every target;
// otherwise a comma-separated list, each name validated against the table.
// "all" is accepted only as the whole value; inside a list it is refused.
func parseTargetsFlag(raw string) ([]Target, error) {
	if raw == "" || raw == "all" {
		return Targets, nil
	}
	var out []Target
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "all" {
			return nil, clierr.New(clierr.CodeValidation,
				"--"+flagTargets+": pass all as the whole value, or list targets without it")
		}
		t, ok := Lookup(tok)
		if !ok {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s=%q is not one of all|%s", flagTargets, tok, runtime.OneOf(Names())))
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveBundleClusters resolves --clusters: "" or "*" discovers local plus
// every management Cluster the caller can read, as steve lists them; an
// explicit comma-separated list is resolved token by token (the "local"
// literal short-circuits).
//
// The two error returns are NOT interchangeable. discoveryErr is a
// non-fatal signal on the default/"*" path: fleet-wide listing failed
// (RBAC denied listing Clusters, ...), so the returned clusters degrade to
// local-only - the caller must record discoveryErr as a manifest error
// entry rather than silently shipping a bundle that only covers local
// while claiming fleet coverage. err is fatal - an explicit --clusters
// token that fails to resolve aborts the whole command, same as any other
// VALIDATION-class failure.
func resolveBundleClusters(ctx context.Context, clients engine.ClusterClientFactory, get steve.Getter, raw string) (clusters []string, discoveryErr error, err error) {
	if raw == "" || raw == "*" {
		ids := map[string]bool{engine.LocalCluster: true}
		list, lerr := steve.Clusters(ctx, get)
		if lerr != nil {
			return []string{engine.LocalCluster}, lerr, nil
		}
		for _, c := range list {
			ids[c.ID] = true
		}
		out := make([]string, 0, len(ids))
		for id := range ids {
			out = append(out, id)
		}
		sort.Strings(out)
		return out, nil, nil
	}
	var out []string
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "local" {
			out = append(out, engine.LocalCluster)
			continue
		}
		id, rerr := engine.ResolveClusterArgument(ctx, clients, tok)
		if rerr != nil {
			return nil, nil, rerr
		}
		out = append(out, id)
	}
	return out, nil, nil
}

// collectSettings fetches every allowlisted setting; a setting absent on
// this Rancher version is silently skipped (not every version ships every
// allowlisted setting) - never a full dump, and a missing setting is not a
// collector failure.
func collectSettings(ctx context.Context, local engine.ClusterClient) (map[string]string, []byte) {
	values := map[string]string{}
	for _, name := range settingsAllowlist {
		obj, err := local.Get(ctx, settingKind.GVR, "", name)
		if err != nil {
			continue
		}
		// Key presence means "the server reports this setting": a Setting
		// carrying neither value nor default is omitted entirely, so a
		// presence check downstream stays meaningful.
		if v, ok := engine.SettingValue(obj); ok {
			values[name] = v
		}
	}
	data, _ := json.Marshal(values)
	return values, data
}

// featureRow's bool fields carry NO `omitempty`: "off" (false) and "not
// collected" (field absent) must not serialize to the same bytes in a
// triage bundle - a support engineer reading features.json needs to see
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
// applicable target's logs, streaming each into bw as it is collected, and
// returns the manifest items and the set of namespaces any attempted
// target resolved to (for the caller's per-namespace events collection) -
// cattle-system is always included.
func collectCluster(ctx context.Context, clients engine.ClusterClientFactory, get steve.Getter, clusterID string, targets []Target, tail *int64, since *int64, bw *ArchiveWriter) ([]manifestItem, []string, []string) {
	var items []manifestItem
	var warnings []string
	namespaces := map[string]bool{"cattle-system": true}

	if obj, err := steve.GetCluster(ctx, get, clusterID); err != nil {
		items = append(items, manifestItem{Name: "cluster.json", Cluster: clusterID, Status: errorStatus(err)})
	} else {
		clusterState := steve.ObjectState(obj)
		clusterConds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		data, _ := json.Marshal(map[string]any{
			"state":      clusterState,
			"conditions": clusterConds,
		})
		_ = bw.Add(ArchiveEntry{Path: fmt.Sprintf("clusters/%s/cluster.json", clusterID), Data: data})
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
				nodeConds, _, _ := unstructured.NestedSlice(list.Items[i].Object, "status", "conditions")
				rows = append(rows, map[string]any{
					"name":       list.Items[i].GetName(),
					"conditions": nodeConds,
				})
			}
			data, _ := json.Marshal(rows)
			_ = bw.Add(ArchiveEntry{Path: fmt.Sprintf("clusters/%s/nodes.json", clusterID), Data: data})
			items = append(items, manifestItem{Name: "nodes.json", Cluster: clusterID, Status: statusCollected})
		}
	}

	for _, t := range targets {
		if !Applicable(t, clusterID) {
			items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: statusSkippedNotApplicable})
			continue
		}
		resolved, err := ResolveOnCluster(ctx, clients, get, t, clusterID)
		if err != nil {
			if nf := clierr.AsNotFound(err); nf != nil && nf.Message == nonACEHint {
				items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: fmt.Sprintf("skipped: not applicable - %s", nonACEHint)})
			} else {
				items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: errorStatus(err)})
			}
			continue
		}
		namespaces[resolved.Namespace] = true
		outcome := collectTargetLogs(ctx, t, resolved, tail, since, bw)
		items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: outcome.status()})
		warnings = append(warnings, outcome.warnings(clusterID, t.Name)...)
	}

	nsList := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		nsList = append(nsList, ns)
	}
	sort.Strings(nsList)
	return items, nsList, warnings
}

// logOutcome is what collectTargetLogs established for one target: how
// many pod/container pairs it resolved to, which current logs did not
// reach the archive, and which previous-container logs the server refused
// (as opposed to not having). The manifest status and the result's
// warnings are both derived from it, so they cannot disagree.
type logOutcome struct {
	containers     int
	failed         []string
	previousDenied []string
}

// status is the manifest status the outcome earns: collected only when
// every current log reached the archive; otherwise an error status, which
// the Errors counter sees, naming the count and the first failure.
func (o logOutcome) status() string {
	if len(o.failed) == 0 {
		return statusCollected
	}
	return clierr.ExpandRemedies(fmt.Sprintf("error: %d of %d container logs not collected; %s", len(o.failed), o.containers, o.failed[0]), "")
}

// warnings names every missing log once, so a caller reading the result
// rather than the archive still sees what is absent.
func (o logOutcome) warnings(clusterID, target string) []string {
	var w []string
	for _, f := range o.failed {
		w = append(w, fmt.Sprintf("support-bundle: %s/%s: log not collected: %s", clusterID, target, f))
	}
	for _, p := range o.previousDenied {
		w = append(w, fmt.Sprintf("support-bundle: %s/%s: previous container log refused: %s", clusterID, target, p))
	}
	return w
}

// collectTargetLogs streams every matched pod/container's current log into
// bw and reports what it could not collect. The previous-container log is
// best effort: a container that never restarted has none, which is the
// common case and stays silent, but a refusal (RBAC, authentication) is
// missing evidence and is reported. A container whose current log failed
// is not asked for its previous log - the answer would be the same.
func collectTargetLogs(ctx context.Context, t Target, resolved Resolved, tail *int64, since *int64, bw *ArchiveWriter) logOutcome {
	var o logOutcome
	for i := range resolved.Pods {
		podName := resolved.Pods[i].GetName()
		for _, cname := range engine.ContainerNames(resolved.Pods[i].Object) {
			o.containers++
			where := podName + "/" + cname
			base := fmt.Sprintf("logs/%s/%s/%s-%s", resolved.ClusterID, t.Name, podName, cname)
			out, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since})
			if err != nil {
				o.failed = append(o.failed, where+": "+err.Error())
				continue
			}
			if err := bw.Add(ArchiveEntry{Path: base + ".log", Data: []byte(out)}); err != nil {
				o.failed = append(o.failed, where+": archive write failed: "+err.Error())
				continue
			}
			prev, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since, Previous: true})
			switch {
			case err == nil:
				_ = bw.Add(ArchiveEntry{Path: base + "-previous.log", Data: []byte(prev)})
			case isRefusal(err):
				o.previousDenied = append(o.previousDenied, where+": "+err.Error())
			}
		}
	}
	return o
}

// isRefusal reports whether err is the server refusing the read (RBAC or
// authentication) rather than reporting there is nothing to read.
func isRefusal(err error) bool {
	ce, ok := errors.AsType[*clierr.Error](err)
	return ok && (ce.Code == clierr.CodeRBAC || ce.Code == clierr.CodeAuth)
}

// defaultEventCap bounds the per-namespace event payload the same way logs
// are already bounded by tail lines: an unbounded event list on a noisy
// cluster must not blow up the bundle's memory or size.
const defaultEventCap = 500

// collectEvents fetches a namespace's events and keeps at most
// defaultEventCap of them, newest first by lastTimestamp/eventTime/creation
// (whichever is present), so a caller can tell whether the returned payload
// was truncated from total to included.
func collectEvents(ctx context.Context, clients engine.ClusterClientFactory, clusterID, namespace string) (data []byte, total, included int, err error) {
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return nil, 0, 0, err
	}
	list, err := client.List(ctx, eventKind.GVR, namespace, "")
	if err != nil {
		return nil, 0, 0, err
	}
	total = len(list.Items)
	items := list.Items
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return eventTimestamp(b).Compare(eventTimestamp(a))
	})
	if len(items) > defaultEventCap {
		items = items[:defaultEventCap]
	}
	included = len(items)
	rows := make([]map[string]any, 0, included)
	for i := range items {
		rows = append(rows, items[i].Object)
	}
	data, err = json.Marshal(rows)
	return data, total, included, err
}

// eventTimestamp reads the first present field among lastTimestamp,
// eventTime, and metadata.creationTimestamp - an Event may carry any subset
// of them depending on the emitting controller - falling back to the zero
// time (sorts last) when none parse as RFC3339.
func eventTimestamp(u unstructured.Unstructured) time.Time {
	for _, path := range [][]string{{"lastTimestamp"}, {"eventTime"}, {"metadata", "creationTimestamp"}} {
		s, ok, _ := unstructured.NestedString(u.Object, path...)
		if !ok || s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

const readmeText = `This is a Rancher CLI support bundle.

Contents:
  manifest.json            what was collected, skipped, or failed, and why
                            (a target whose container logs could not all be
                            read is an error item naming the first failure)
  logs/<cluster>/<target>/ curated component logs (see "rancher logs")
  clusters/<cluster>/      cluster/node state and per-namespace events
  rancher/                 CLI version, an allowlisted settings snapshot,
                            feature flags, and this CLI's own schema manifest

This bundle may contain sensitive log content (application output,
identifiers, timestamps) from the collected components. Review before
sharing outside your organization.

This bundle covers the API-reachable plane only. For node-level diagnostics
(journald, the rancher-system-agent unit, OS state) see
rancherlabs/support-tools' node collector - the two bundles are
complementary by design.
`
