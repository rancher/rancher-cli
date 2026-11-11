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

	// outcome is what the rule for a run that collected no server item reads
	// of the item. The manifest does not carry it.
	outcome itemOutcome `json:"-"`
}

// itemOutcome is an item's part in the rule that fails a run which collected
// no item from the server. The zero value is an item the server answered:
// it counts as collected. A new item counts unless it is marked outside.
type itemOutcome struct {
	// outside marks an item that is not a request to the server, or was never
	// attempted: the schema and version files and a target skipped before
	// any request. The settings file is outside too: it is an optional
	// diagnostic. Its failure is reported in the manifest and counts in
	// errors, and does not count toward the rule that fails a run which
	// collected no item.
	outside bool
	// failure is why an attempted item was not collected; nil when it was.
	failure error
	// local marks a failure that came from the archive or from an entry
	// name, not from the server.
	local bool
}

// serverItem is the item of a read from the server that ended with err.
func serverItem(name, cluster string, err error) manifestItem {
	return manifestItem{Name: name, Cluster: cluster, Status: itemStatus(err), outcome: itemOutcome{failure: err}}
}

// localItem is the item of a step that did not read the server and ended with
// err: a refused entry name or a failed archive write.
func localItem(name, cluster string, err error) manifestItem {
	return manifestItem{Name: name, Cluster: cluster, Status: itemStatus(err), outcome: itemOutcome{failure: err, local: err != nil}}
}

// outsideItem is an item the rule does not count.
func outsideItem(name string, err error) manifestItem {
	return manifestItem{Name: name, Status: itemStatus(err), outcome: itemOutcome{outside: true}}
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
	create  func(path string) (*ArchiveWriter, error)
}

// NewSupportBundle builds `support-bundle [--targets ...|all] [--clusters
// a,b|*] [--since <dur>] [--tail N=10000] [--output-file out]`: packs every
// API-reachable diagnostic surface - curated component logs, cluster/node
// state, per-namespace events, and the CLI's own settings/features/schema
// snapshot - into one tar.gz, tri-state manifested so support can tell
// "not applicable" from "collector failed" at a glance.
func NewSupportBundle(clients engine.ClusterClientFactory, roots engine.RootClientFactory, schema func() any) runtime.Command {
	return &supportBundleCmd{clients: clients, roots: roots, schema: schema, now: time.Now, create: CreateArchive}
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

	bw, err := c.create(path)
	if err != nil {
		return runtime.Result{}, err
	}

	var items []manifestItem

	if discoveryErr != nil {
		items = append(items, serverItem("cluster-discovery", "", discoveryErr))
	}

	settingsValues, settingsData, settingsErr := collectSettings(ctx, local)
	if settingsData != nil {
		_ = bw.Add(ArchiveEntry{Path: staticEntryPath("rancher/settings.json"), Data: settingsData})
	}
	items = append(items, outsideItem("rancher/settings.json", settingsErr))

	featuresData, ferr := collectFeatures(ctx, local)
	items = append(items, serverItem("rancher/features.json", "", ferr))
	if ferr == nil {
		_ = bw.Add(ArchiveEntry{Path: staticEntryPath("rancher/features.json"), Data: featuresData})
	}

	schemaData, serr := json.Marshal(c.schema())
	items = append(items, outsideItem("rancher/schema.json", serr))
	if serr == nil {
		_ = bw.Add(ArchiveEntry{Path: staticEntryPath("rancher/schema.json"), Data: schemaData})
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
	_ = bw.Add(ArchiveEntry{Path: staticEntryPath("rancher/version.json"), Data: versionData})
	items = append(items, outsideItem("rancher/version.json", nil))

	var warnings []string
	for _, clusterID := range clusters {
		clusterItems, namespaces, clusterWarnings := collectCluster(ctx, c.clients, get, clusterID, targets, tail, since, bw)
		items = append(items, clusterItems...)
		warnings = append(warnings, clusterWarnings...)

		eventItems, eventWarnings := archiveEvents(ctx, c.clients, clusterID, namespaces, bw)
		items = append(items, eventItems...)
		warnings = append(warnings, eventWarnings...)
	}

	_ = bw.Add(ArchiveEntry{Path: staticEntryPath("README.txt"), Data: append([]byte(readmeText), readmeEntriesText...)})

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
	_ = bw.Add(ArchiveEntry{Path: staticEntryPath("manifest.json"), Data: manifestData})

	// A run ended by its context, or one that collected no item from the
	// server, still closes the archive: its manifest.json lists every item
	// and why it failed.
	var stop *clierr.Error
	var failures []itemFailure
	if ctx.Err() != nil {
		stop = interruptedError(path).WithWarnings(warnings...)
	} else if attempted, failed := serverOutcome(items); attempted > 0 && len(failed) == attempted {
		failures = failed
		stop = noServerItemError(path, attempted, failed).WithWarnings(warnings...)
	}

	bytesWritten, err := bw.Close()
	if err != nil {
		switch {
		case failures != nil:
			return runtime.Result{}, clierr.Wrap(err, fmt.Sprintf(
				"support-bundle could not write the archive: %v; it collected no item from the server either, the first failure was: %v",
				err, firstFailure(failures).err)).WithWarnings(warnings...)
		case stop != nil:
			return runtime.Result{}, clierr.Wrap(err, fmt.Sprintf(
				"support-bundle stopped before it finished and could not write the archive: %v", err)).WithWarnings(warnings...)
		}
		return runtime.Result{}, err
	}
	if stop != nil {
		return runtime.Result{}, stop
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
// "all" is accepted only as the whole value; inside a list it is refused. A
// target named twice is used once, in the order of its first appearance.
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
				"--"+flagTargets+": pass all by itself, or list targets without all")
		}
		t, ok := Lookup(tok)
		if !ok {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s=%q is not one of all|%s", flagTargets, tok, runtime.OneOf(Names())))
		}
		if !slices.ContainsFunc(out, func(seen Target) bool { return seen.Name == t.Name }) {
			out = append(out, t)
		}
	}
	return out, nil
}

// resolveBundleClusters resolves --clusters: "" or "*" discovers local plus
// every management Cluster the caller can read, as steve lists them; an
// explicit comma-separated list is resolved token by token (the "local"
// literal short-circuits). A cluster named twice, by id or by name and id, is
// used once, in the order of its first appearance.
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
		id := engine.LocalCluster
		if tok != "local" {
			resolved, rerr := engine.ResolveClusterArgument(ctx, clients, tok)
			if rerr != nil {
				return nil, nil, rerr
			}
			id = resolved
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil, nil
}

// collectSettings fetches every allowlisted setting - never a full dump. A
// setting the server answers not found is skipped silently (not every Rancher
// version ships every allowlisted setting). Any other failure is returned in
// the error, which names the settings not read and wraps the first failure;
// the reads continue after it, so the settings that were read stay in values.
// data is the settings file; it is nil when every read failed, because an
// empty file would read as a server that reports none of these settings.
func collectSettings(ctx context.Context, local engine.ClusterClient) (values map[string]string, data []byte, err error) {
	values = map[string]string{}
	var failed []string
	var first error
	for _, name := range settingsAllowlist {
		obj, getErr := local.Get(ctx, settingKind.GVR, "", name)
		if getErr != nil {
			if !clierr.IsNotFound(getErr) {
				failed = append(failed, name)
				if first == nil {
					first = getErr
				}
			}
			continue
		}
		// Key presence means "the server reports this setting": a Setting
		// carrying neither value nor default is omitted entirely, so a
		// presence check downstream stays meaningful.
		if v, ok := engine.SettingValue(obj); ok {
			values[name] = v
		}
	}
	if first != nil {
		err = fmt.Errorf("%d of %d settings not read (%s); first failure: %w",
			len(failed), len(settingsAllowlist), strings.Join(failed, ", "), first)
	}
	if len(failed) < len(settingsAllowlist) {
		data, _ = json.Marshal(values)
	}
	return values, data, err
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

// archiveEvents writes each namespace's events into bw and returns one
// manifest item per namespace. A namespace whose events file name is not a
// plain file name is left out before any request; a failed write is an error
// item with a warning.
func archiveEvents(ctx context.Context, clients engine.ClusterClientFactory, clusterID string, namespaces []string, bw *ArchiveWriter) ([]manifestItem, []string) {
	var items []manifestItem
	var warnings []string
	for _, ns := range namespaces {
		eventsName, nerr := entryPath("clusters", clusterID, "events-"+ns+".json")
		if nerr != nil {
			items = append(items, localItem("events-"+ns, clusterID, nerr))
			warnings = append(warnings, fmt.Sprintf("support-bundle: events of cluster %s left out: %v", quoteValue(clusterID), nerr))
			continue
		}
		data, total, included, eerr := collectEvents(ctx, clients, clusterID, ns)
		item := serverItem("events-"+ns, clusterID, eerr)
		if eerr == nil && included < total {
			item.Status = fmt.Sprintf("collected (truncated to %d of %d, newest first)", included, total)
		}
		if eerr == nil {
			if aerr := bw.Add(ArchiveEntry{Path: eventsName, Data: data}); aerr != nil {
				item = localItem("events-"+ns, clusterID, aerr)
				warnings = append(warnings, fmt.Sprintf("support-bundle: events of cluster %s namespace %s not written: %v", quoteValue(clusterID), quoteValue(ns), aerr))
			}
		}
		items = append(items, item)
	}
	return items, warnings
}

// collectCluster gathers one cluster's cluster.json/nodes.json plus every
// applicable target's logs, streaming each into bw as it is collected, and
// returns the manifest items and the set of namespaces any attempted
// target resolved to (for the caller's per-namespace events collection) -
// cattle-system is always included. A cluster id that is not a plain file
// name is a whole path segment of every entry of the cluster: the cluster is
// left out before any request, with one error item named cluster, one
// warning and no namespaces.
func collectCluster(ctx context.Context, clients engine.ClusterClientFactory, get steve.Getter, clusterID string, targets []Target, tail *int64, since *int64, bw *ArchiveWriter) ([]manifestItem, []string, []string) {
	if err := checkComponent(clusterID); err != nil {
		return []manifestItem{localItem("cluster", clusterID, err)},
			nil,
			[]string{fmt.Sprintf("support-bundle: cluster left out: %v", err)}
	}
	var items []manifestItem
	var warnings []string
	namespaces := map[string]bool{"cattle-system": true}

	if obj, err := steve.GetCluster(ctx, get, clusterID); err != nil {
		items = append(items, serverItem("cluster.json", clusterID, err))
	} else {
		clusterState := steve.ObjectState(obj)
		clusterConds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		data, _ := json.Marshal(map[string]any{
			"state":      clusterState,
			"conditions": clusterConds,
		})
		clusterName, nerr := entryPath("clusters", clusterID, "cluster.json")
		if nerr == nil {
			nerr = bw.Add(ArchiveEntry{Path: clusterName, Data: data})
		}
		items = append(items, localItem("cluster.json", clusterID, nerr))
	}

	if client, err := clients.Cluster(ctx, clusterID); err != nil {
		items = append(items, serverItem("nodes.json", clusterID, err))
	} else {
		if list, err := client.List(ctx, nodeKind.GVR, "", ""); err != nil {
			items = append(items, serverItem("nodes.json", clusterID, err))
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
			nodesName, nerr := entryPath("clusters", clusterID, "nodes.json")
			if nerr == nil {
				nerr = bw.Add(ArchiveEntry{Path: nodesName, Data: data})
			}
			items = append(items, localItem("nodes.json", clusterID, nerr))
		}
	}

	for _, t := range targets {
		if !Applicable(t, clusterID) {
			items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: statusSkippedNotApplicable, outcome: itemOutcome{outside: true}})
			continue
		}
		resolved, err := ResolveOnCluster(ctx, clients, get, t, clusterID)
		if err != nil {
			if nf := clierr.AsNotFound(err); nf != nil && nf.Message == nonACEHint {
				items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: fmt.Sprintf("skipped: not applicable - %s", nonACEHint)})
			} else {
				items = append(items, serverItem(t.Name, clusterID, err))
			}
			continue
		}
		namespaces[resolved.Namespace] = true
		outcome := collectTargetLogs(ctx, t, resolved, tail, since, bw)
		items = append(items, manifestItem{Name: t.Name, Cluster: clusterID, Status: outcome.status(), outcome: outcome.itemOutcome()})
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

	// written is how many current logs reached the archive, and readErr is
	// the first error the server gave for a current log.
	written int
	readErr error
}

// itemOutcome is the target's part in the rule for a run that collected no
// server item. A target with at least one log in the archive is collected,
// whatever its status says.
func (o logOutcome) itemOutcome() itemOutcome {
	if len(o.failed) == 0 || o.written > 0 {
		return itemOutcome{}
	}
	if o.readErr != nil {
		return itemOutcome{failure: o.readErr}
	}
	return itemOutcome{failure: errors.New(o.failed[0]), local: true}
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
// is not asked for its previous log - the answer would be the same. Each pod
// has its own directory: two logs make one entry name only when the pod and
// container values are equal (or differ in letter case). The archive keeps
// the first and the second container is left out.
func collectTargetLogs(ctx context.Context, t Target, resolved Resolved, tail *int64, since *int64, bw *ArchiveWriter) logOutcome {
	var o logOutcome
	for i := range resolved.Pods {
		podName := resolved.Pods[i].GetName()
		for _, cname := range engine.ContainerNames(resolved.Pods[i].Object) {
			o.containers++
			where := podName + "/" + cname
			logName, nerr := entryPath("logs", resolved.ClusterID, t.Name, podName, cname+".log")
			if nerr != nil {
				o.failed = append(o.failed, fmt.Sprintf("pod %s container %s: log entry left out: %v", quoteValue(podName), quoteValue(cname), nerr))
				continue
			}
			out, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since})
			if err != nil {
				o.failed = append(o.failed, where+": "+err.Error())
				if o.readErr == nil {
					o.readErr = err
				}
				continue
			}
			if err := bw.Add(ArchiveEntry{Path: logName, Data: []byte(out)}); err != nil {
				if errors.Is(err, errEntryRepeated) {
					o.failed = append(o.failed, fmt.Sprintf("pod %s container %s: log entry left out: %v", quoteValue(podName), quoteValue(cname), err))
				} else {
					o.failed = append(o.failed, where+": archive write failed: "+err.Error())
				}
				continue
			}
			o.written++
			prev, err := resolved.Client.Logs(ctx, resolved.Namespace, podName,
				engine.LogOptions{Container: cname, TailLines: tail, SinceSeconds: since, Previous: true})
			switch {
			case err == nil:
				previousName, aerr := entryPath("logs", resolved.ClusterID, t.Name, podName, "previous", cname+".log")
				if aerr == nil {
					aerr = bw.Add(ArchiveEntry{Path: previousName, Data: []byte(prev)})
				}
				if aerr != nil {
					o.failed = append(o.failed, where+": archive write failed: "+aerr.Error())
				}
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

const readmeText = `This is a rancher-cli support bundle.

Contents:
  manifest.json            what was collected, skipped, or failed, and why
                            (a target whose container logs could not all be
                            read is an error item naming the first failure)
  logs/<cluster>/<target>/ logs of Rancher components (see "rancher logs")
  clusters/<cluster>/      cluster/node state and per-namespace events
  rancher/                 CLI version, a snapshot of a fixed list of
                            settings, feature flags, and this CLI's own
                            schema manifest

This bundle may contain sensitive log content (application output,
identifiers, timestamps) from the collected components. Review before
sharing outside your organization.

This bundle covers only what Rancher's Kubernetes API can reach. For
node-level diagnostics (journald, the rancher-system-agent unit, OS state)
see rancherlabs/support-tools' node collector; the two bundles complement
each other.
`

// readmeEntriesText follows readmeText in the archive's README.txt.
const readmeEntriesText = `
Each pod has its own directory under logs/<cluster>/<target>/. The current
log of a container is <pod>/<container>.log and the log of its previous run
is <pod>/previous/<container>.log. An entry whose name was already written
is left out and shows in manifest.json as an error.

An entry is left out of the bundle when its file name is not a plain file
name: it has a character other than letters, digits, ".", "_" and "-" (a
cluster id, namespace, pod or container name is part of it), it is over
255 bytes, or its part before the first "." is a Windows device name such
as con or aux. A cluster whose id is left out is listed as the item
"cluster". manifest.json shows each as an error.
`
