package provisioning

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const versionManagementSetting = "imported-cluster-version-management"

// nodeVersion is one downstream Node as the wait reads it.
type nodeVersion struct {
	Name           string `json:"name"`
	KubeletVersion string `json:"kubeletVersion"`
}

// nodeRuns reports whether a Node runs the target version. A kubelet version
// with a build suffix must equal the target in full, because a change of the
// release number alone leaves major, minor and patch equal; one without a
// build suffix must equal the target's major, minor and patch, as Rancher's
// planner compares them.
func nodeRuns(kubelet, target string) bool {
	if strings.Contains(kubelet, "+") {
		return kubelet == target
	}
	a, b, c, _, ok := clusterops.ParseVersionKey(kubelet)
	d, e, f, _, okTarget := clusterops.ParseVersionKey(target)
	return ok && okTarget && clusterops.CompareTriple(a, b, c, d, e, f, false) == 0
}

func nodesRunning(nodes []nodeVersion, target string) (running int) {
	for _, n := range nodes {
		if nodeRuns(n.KubeletVersion, target) {
			running++
		}
	}
	return running
}

// listDownstreamNodes lists the Nodes of the downstream cluster as the caller.
// A failed List is a warning, not a refusal: the Nodes feed the preview's
// number of nodes and the wait's node check only.
func listDownstreamNodes(ctx context.Context, clients engine.ClusterClientFactory, r upgradeRead) (nodes []nodeVersion, listed bool, warning string) {
	nodes, err := readNodes(ctx, clients, r.clusterID)
	if err == nil {
		return nodes, true, ""
	}
	warning = fmt.Sprintf("cannot list the nodes of cluster %s (%s); the preview does not show how many nodes run the current version", r.name, clierr.From(err).Code)
	if r.facts.Provisioned {
		warning += " and the wait does not check the nodes"
	}
	return nil, false, warning
}

// readNodes lists the Nodes of the downstream cluster with the id.
func readNodes(ctx context.Context, clients engine.ClusterClientFactory, clusterID string) ([]nodeVersion, error) {
	downstream, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	list, err := downstream.List(ctx, kindRow("node").GVR, "", "")
	if err != nil {
		return nil, err
	}
	nodes := make([]nodeVersion, 0, len(list.Items))
	for _, item := range list.Items {
		version, _, _ := unstructured.NestedString(item.Object, "status", "nodeInfo", "kubeletVersion")
		nodes = append(nodes, nodeVersion{Name: item.GetName(), KubeletVersion: version})
	}
	return nodes, nil
}

// snapshotRead is the newest successful etcd snapshot of the cluster, or why
// there is none.
type snapshotRead struct {
	found     bool
	name      string
	createdAt string
	err       error
}

// readSnapshots lists the etcd snapshot objects of the cluster: in the
// namespace of its provisioning Cluster, or for an imported cluster in the
// namespace named by the cluster's id, where Rancher records them with that id
// as the cluster name. It lists them through the Kubernetes API and, only when
// that responds with 403, through Steve as the caller.
func readSnapshots(ctx context.Context, client engine.ClusterClient, roots engine.RootClientFactory, r upgradeRead) snapshotRead {
	ns, cluster := r.ns, r.name
	if !r.facts.Provisioned {
		ns, cluster = r.facts.ID, r.facts.ID
	}
	var list []unstructured.Unstructured
	listed, err := client.List(ctx, kindRow("etcdsnapshot").GVR, ns, "")
	if err != nil {
		list, err = resources.EtcdSnapshotsAfterForbidden(ctx, roots, ns, err)
		if err != nil {
			return snapshotRead{err: err}
		}
	} else {
		list = listed.Items
	}
	items := make([]map[string]any, 0, len(list))
	for _, item := range list {
		items = append(items, item.Object)
	}
	newest, ok := clusterops.NewestSuccessfulSnapshot(items, cluster)
	if !ok {
		return snapshotRead{}
	}
	name, _, _ := unstructured.NestedString(newest, "metadata", "name")
	created, _, _ := unstructured.NestedString(newest, "snapshotFile", "createdAt")
	return snapshotRead{found: true, name: name, createdAt: created}
}

// warning is the line of the preview that says which etcd snapshot to restore
// to return to an earlier version. When the command runs again while the
// upgrade is not finished, the snapshot may have been taken after the upgrade
// was requested.
func (s snapshotRead) warning(cluster string, rerun bool) string {
	switch {
	case s.err != nil:
		return fmt.Sprintf("cannot list the etcd snapshots of cluster %s (%s)", cluster, clierr.From(s.err).Code)
	case !s.found:
		return fmt.Sprintf("No successful etcd snapshot of cluster %s is recorded; take one first, in the Rancher UI", cluster)
	}
	text := fmt.Sprintf("Newest successful snapshot of cluster %s: %s, taken %s", cluster, s.name, s.createdAt)
	if rerun {
		text += "; it may have been taken after the upgrade was requested"
	}
	return text
}

// returnWarning says that the upgrade cannot be reversed with this command and
// how to return to an earlier version.
func returnWarning(f clusterops.UpgradeCluster) string {
	restore := "restore an etcd snapshot of type Kubernetes version and etcd: it sets the Kubernetes version recorded in that snapshot, which can differ from " +
		f.Current + ", and restores etcd to its state when the snapshot was taken"
	if f.Provisioned {
		return "the upgrade cannot be reversed with this command. To return to an earlier version, " + restore
	}
	return "the upgrade cannot be reversed with this command. On Rancher v2.16 and later, to return to an earlier version, " + restore +
		"; earlier servers cannot restore the version of an imported cluster"
}

// conditionOf returns the status and message of the status condition of the
// given type, and whether the object has it.
func conditionOf(obj *unstructured.Unstructured, condType string) (status, message string, found bool) {
	engine.EachCondition(obj.Object, func(typ, s, _, m string) bool {
		if typ != condType {
			return true
		}
		status, message, found = s, m, true
		return false
	})
	return status, message, found
}

// upgradeFinished reports whether Rancher is done with an upgrade to the
// target, by the rule that ends the wait, and whether it checked the Nodes: a
// provisioned cluster is done when Rancher has seen the object's generation,
// its Updated condition is True and every listed Node runs the target; an
// imported cluster when its Upgraded condition is True and it runs the
// target or newer.
func upgradeFinished(r upgradeRead, target string) (done, nodesChecked bool) {
	if !r.facts.Provisioned {
		status, _, _ := conditionOf(r.mgmt, "Upgraded")
		running, _, _ := unstructured.NestedString(r.mgmt.Object, "status", "version", "gitVersion")
		return status == "True" && clusterops.CompareVersions(running, target) >= 0, false
	}
	status, _, _ := conditionOf(r.prov, "Updated")
	generation, _, _ := unstructured.NestedFieldNoCopy(r.prov.Object, "metadata", "generation")
	observed, _, _ := unstructured.NestedFieldNoCopy(r.prov.Object, "status", "observedGeneration")
	done = status == "True" && numberOf(observed) >= numberOf(generation)
	if !r.listed {
		return done, false
	}
	for _, n := range r.nodes {
		if strings.Contains(n.KubeletVersion, "+") {
			nodesChecked = true
		}
	}
	return done && nodesRunning(r.nodes, target) == len(r.nodes), nodesChecked
}

// drainNumber returns a drain option that is a number, whatever its Go type.
func drainNumber(v any) (int64, bool) {
	switch v.(type) {
	case int, int64, float64:
		return numberOf(v), true
	}
	return 0, false
}

// numberOf returns the value of a number decoded from JSON whatever its Go
// type, 0 for a value that is not a number.
func numberOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// finishedError is the refusal for a target whose upgrade is finished.
func finishedError(r upgradeRead, target string, nodesChecked bool) *clierr.Error {
	text := fmt.Sprintf("cluster %s is already upgraded to %s: Rancher reports the upgrade done", r.name, target)
	if r.facts.Provisioned {
		if nodesChecked {
			text += " and every listed node runs it"
		} else {
			text += " (its Updated condition is True); the nodes were not checked"
		}
	}
	return clierr.New(clierr.CodeValidation, text).WithHintCommand(clierr.NewCommand("get kubernetes-versions"))
}

// concurrencyText returns a concurrency value as written, 1 when it is unset
// or empty, and whether it is 0 (every node at once).
func concurrencyText(v any) (text string, zero bool) {
	switch n := v.(type) {
	case nil:
		return "1", false
	case string:
		if n == "" {
			return "1", false
		}
		return n, n == "0"
	}
	text = fmt.Sprint(v)
	return text, text == "0"
}

// drainFields are the fields of a provisioned cluster's drain options the
// preview names, in the order it names them.
var drainFields = []string{"force", "ignoreDaemonSets", "deleteEmptyDirData", "disableEviction", "gracePeriod", "timeout", "skipWaitForDeleteTimeoutSeconds"}

// drainText is "on" with the drain options that are set, or "off". A timeout
// and a grace period carry their unit in seconds; a negative grace period is
// the pod's default, and a timeout of 0 is 600 seconds for Rancher.
func drainText(options map[string]any) string {
	if enabled, _ := options["enabled"].(bool); !enabled {
		return "off"
	}
	var parts []string
	for _, f := range drainFields {
		v, ok := options[f]
		if !ok {
			continue
		}
		n, isNumber := drainNumber(v)
		switch {
		case f == "gracePeriod" && isNumber && n < 0:
			parts = append(parts, fmt.Sprintf("%s %v (the pod's default)", f, v))
		case f == "timeout" && isNumber && n == 0:
			parts = append(parts, fmt.Sprintf("%s %v (Rancher uses 600 seconds)", f, v))
		case (f == "gracePeriod" || f == "timeout") && isNumber:
			parts = append(parts, fmt.Sprintf("%s %v seconds", f, v))
		default:
			parts = append(parts, fmt.Sprintf("%s %v", f, v))
		}
	}
	if len(parts) == 0 {
		return "on"
	}
	return "on (" + strings.Join(parts, ", ") + ")"
}

// upgradeWarnings are the warnings of the preview that depend on the cluster,
// the target and the wait.
func upgradeWarnings(r upgradeRead, target string, again bool, wait clusterWait, rows []Version, releases []clusterops.Release) []string {
	var out []string
	f := r.facts
	if again {
		out = append(out, fmt.Sprintf("cluster %s already has version %s set and its upgrade is not finished: nothing changes; the command waits for the upgrade", r.name, target))
	}
	if message, running := upgradeRunning(upgradeObject(r), f.Provisioned); running {
		out = append(out, runningUpgradeWarning(message))
	}
	if f.ManuallyUpgraded {
		out = append(out, fmt.Sprintf("cluster %s runs %s while the version in its config is %s; the Rancher UI shows it as manually upgraded; this command compares the new version with %s", r.name, f.Current, f.Stored, f.Current))
	}
	if f.AddAnnotation {
		out = append(out, annotationWarning(r.name))
	}
	out = append(out, orderWarnings(r)...)

	if major, minor, _, _, ok := clusterops.ParseVersionKey(target); ok {
		if cMajor, cMinor, _, _, cok := clusterops.ParseVersionKey(f.Current); cok && cMajor == major && minor > cMinor+1 {
			skipped := fmt.Sprintf("1.%d", cMinor+1)
			if minor > cMinor+2 {
				skipped += fmt.Sprintf(" to 1.%d", minor-1)
			}
			out = append(out, fmt.Sprintf("%s skips Kubernetes %s; Kubernetes supports upgrading one minor at a time", target, skipped))
		}
		if newest, ok := clusterops.NewestPatches(releases)[fmt.Sprintf("%d.%d", major, minor)]; ok && newest != target {
			out = append(out, fmt.Sprintf("%s is not the newest patch of %d.%d; the Rancher UI lists it as deprecated", target, major, minor))
		}
	}
	for _, v := range rows {
		if v.Distro == f.Distro && v.Default && clusterops.CompareVersions(target, v.Version) > 0 {
			setting := rke2DefaultVersionSetting
			if f.Distro == "k3s" {
				setting = k3sDefaultVersionSetting
			}
			out = append(out, fmt.Sprintf("%s is newer than the default version %s (setting %s); the Rancher UI labels it experimental", target, v.Version, setting))
		}
	}
	if r.listed {
		out = append(out, fmt.Sprintf("%d of %d nodes run %s", nodesRunning(r.nodes, f.Current), len(r.nodes), f.Current))
	}
	if wait.On && wait.Timeout < upgradeShortTimeout {
		out = append(out, "an upgrade usually takes longer; the upgrade goes on after the wait ends")
	}
	return out
}

// upgradeShortTimeout is the shortest --timeout that does not warn.
const upgradeShortTimeout = 10 * time.Minute

// annotationWarning says the command adds the version management annotation,
// which Rancher's admission webhook requires on an update of an imported
// cluster.
func annotationWarning(name string) string {
	return fmt.Sprintf("cluster %s has no annotation %s; Rancher's admission webhook denies an update without it, and the command sets it to system-default", name, versionManagementAnnotation)
}

// upgradeRunning reports whether Rancher runs an upgrade of the cluster, and
// the message of the condition that says so: a provisioning Cluster whose
// Updated condition is present and not True, or a management Cluster whose
// Upgraded condition is Unknown. cluster is the provisioning Cluster of a
// provisioned cluster, else the management Cluster.
func upgradeRunning(cluster *unstructured.Unstructured, provisioned bool) (message string, running bool) {
	if provisioned {
		status, message, found := conditionOf(cluster, "Updated")
		return message, found && status != "True"
	}
	status, message, found := conditionOf(cluster, "Upgraded")
	return message, found && status == "Unknown"
}

func runningUpgradeWarning(message string) string {
	if message == "" {
		return "an upgrade is in progress; the new version replaces the version it upgrades to"
	}
	return "an upgrade is in progress: " + message + "; the new version replaces the version it upgrades to"
}

// orderWarnings say how Rancher runs the upgrade, with the concurrency and
// drain settings the cluster stores.
func orderWarnings(r upgradeRead) []string {
	var out []string
	if r.facts.Provisioned {
		strategy, _, _ := unstructured.NestedMap(r.prov.Object, "spec", "rkeConfig", "upgradeStrategy")
		cp, cpZero := concurrencyText(strategy["controlPlaneConcurrency"])
		worker, workerZero := concurrencyText(strategy["workerConcurrency"])
		out = append(out, fmt.Sprintf("the server-side dry-run checks the request; it does not run the upgrade. Rancher upgrades etcd nodes one at a time, then control plane nodes (%s at a time), then worker nodes (%s at a time)", cp, worker))
		out = append(out, zeroWarnings(cpZero, workerZero)...)
		cpDrain, _ := strategy["controlPlaneDrainOptions"].(map[string]any)
		workerDrain, _ := strategy["workerDrainOptions"].(map[string]any)
		out = append(out, fmt.Sprintf("drain before upgrade: control plane nodes %s, worker nodes %s", drainText(cpDrain), drainText(workerDrain)))
		return out
	}
	strategy, _, _ := unstructured.NestedMap(r.mgmt.Object, "spec", r.facts.Distro+"Config", r.facts.Distro+"upgradeStrategy")
	cp, cpZero := concurrencyText(strategy["serverConcurrency"])
	worker, workerZero := concurrencyText(strategy["workerConcurrency"])
	out = append(out, fmt.Sprintf("Rancher's system-upgrade-controller upgrades the control plane nodes (%s at a time), then the worker nodes (%s at a time)", cp, worker))
	out = append(out, zeroWarnings(cpZero, workerZero)...)
	cpDrain, _ := strategy["drainServerNodes"].(bool)
	workerDrain, _ := strategy["drainWorkerNodes"].(bool)
	return append(out, fmt.Sprintf("drain before upgrade: control plane nodes %s, worker nodes %s", onForce(cpDrain), onForce(workerDrain)))
}

// onForce is "on (force true)" for a group that drains, else "off": Rancher
// sets drain force to true for every group of an imported cluster that drains.
func onForce(on bool) string {
	if on {
		return "on (force true)"
	}
	return "off"
}

func zeroWarnings(controlPlane, worker bool) []string {
	var out []string
	if controlPlane {
		out = append(out, "every control plane node is upgraded at once")
	}
	if worker {
		out = append(out, "every worker node is upgraded at once")
	}
	return out
}
