// upgrade.go implements `cluster upgrade <cluster> --k8s-version <version>`:
// the Kubernetes version upgrade of a provisioned or imported RKE2 or K3s
// cluster, with the version rules of the Rancher UI, a server dry-run
// preview, and a wait until Rancher reports the upgrade done.
package provisioning

import (
	"context"
	"fmt"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	upgradeVersionFlag = "k8s-version"

	// versionManagementAnnotation is the annotation of an imported cluster
	// that Rancher's admission webhook denies an update without; the command
	// sets it to system-default when the cluster has none.
	versionManagementAnnotation = "rancher.io/imported-cluster-version-management"
)

type upgradeCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewUpgrade builds `cluster upgrade <cluster> --k8s-version <version>`
// (mutating): sets the Kubernetes version of a provisioned or imported RKE2
// or K3s cluster after the checks of the Rancher UI, and waits until Rancher
// reports the upgrade done.
func NewUpgrade(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &upgradeCmd{clients: clients, roots: roots}
}

func (upgradeCmd) Describe() runtime.Descriptor {
	flags := []runtime.ArgSpec{
		{Name: upgradeVersionFlag, Type: "string", Required: true, Desc: "Kubernetes `version` to upgrade to, for example v1.31.4+rke2r1"},
		clusterNamespaceFlag(),
	}
	flags = append(flags, clusterWaitFlags()...)
	return runtime.Descriptor{
		Name:  "cluster upgrade",
		Usage: "Upgrade the Kubernetes version of an RKE2 or K3s cluster",
		Desc: "Sets the Kubernetes version of a provisioned cluster (spec.kubernetesVersion of its provisioning cluster) or of an " +
			"imported RKE2 or K3s cluster (spec.rke2Config or spec.k3sConfig of its management cluster), then waits until Rancher " +
			"reports the upgrade done. It accepts the versions the Rancher UI offers: a release of the cluster's own distribution that " +
			"the server lists and that is not lower than the current version. The preview names the newest successful etcd snapshot, " +
			"because the upgrade cannot be reversed with this command: to return to an earlier version, restore an etcd snapshot of type Kubernetes " +
			"version and etcd. Rancher runs the upgrade with its own controllers. Running the command again with the same version " +
			"while the upgrade is not finished changes nothing and waits. --wait=false returns once the server accepts the request. " +
			"The local cluster, hosted clusters and imported clusters with version management off are refused.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       flags,
		Examples: []string{
			"rancher cluster upgrade my-cluster --k8s-version v1.31.4+rke2r1 --dry-run",
			"rancher cluster upgrade my-cluster --k8s-version v1.31.4+rke2r1 --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

var _ runtime.MutatingCommand = (*upgradeCmd)(nil)

// kindRow returns the kind row for key; a missing row is a build error.
func kindRow(key string) kinds.Kind {
	row, ok := kinds.Lookup(key)
	if !ok {
		panic(key + " kind row not found in kinds.go - renamed or removed without updating internal/engine/provisioning")
	}
	return row
}

// upgradeRead is what the Plan read about the cluster before it built the
// update.
type upgradeRead struct {
	prov  *unstructured.Unstructured
	mgmt  *unstructured.Unstructured // nil for a provisioned cluster
	facts clusterops.UpgradeCluster
	ns    string
	name  string
	// clusterID is the id of the management Cluster: the downstream cluster
	// the Nodes are listed on.
	clusterID string
	nodes     []nodeVersion
	// listed is true when the Node List succeeded.
	listed bool
}

func (c *upgradeCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	target := in.String(upgradeVersionFlag)
	if target == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--"+upgradeVersionFlag+" is required")
	}
	wait, err := readClusterWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, prov, ns, name, err := fetchClusterFamilyTarget(ctx, c.clients, in)
	if err != nil {
		if clierr.IsNotFound(err) {
			return safety.Effect{}, nil, noProvisioningClusterError(in.Args()[0], resolveClusterNamespace(in), "its version is set in spec.rke2Config or spec.k3sConfig of its management cluster").WithCause(err)
		}
		return safety.Effect{}, nil, err
	}

	mgmt, setting, err := readManagementCluster(ctx, client, c.roots, prov)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	var mgmtObject map[string]any
	if mgmt != nil {
		mgmtObject = mgmt.Object
	}
	facts, cerr := clusterops.CheckUpgradeCluster(prov.Object, mgmtObject, setting)
	if cerr != nil {
		return safety.Effect{}, nil, cerr
	}

	var warnings []string
	if facts.Provisioned {
		w, rerr := checkRunningOperations(ctx, client, prov, ns, name)
		if rerr != nil {
			return safety.Effect{}, nil, rerr
		}
		warnings = append(warnings, w...)
	}

	rows, _, _, missing, err := fetchVersionsDetailed(ctx, c.roots, c.clients)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if slices.Contains(missing, facts.Distro) {
		return safety.Effect{}, nil, releaseListMissingError(ctx, c.clients, facts.Distro)
	}
	releases := releasesOf(rows, facts.Distro)
	if terr := clusterops.CheckUpgradeTarget(facts, releases, target); terr != nil {
		return safety.Effect{}, nil, terr
	}

	read := upgradeRead{prov: prov, mgmt: mgmt, facts: facts, ns: ns, name: name, clusterID: upgradeClusterID(prov, facts)}
	var nodeWarning string
	read.nodes, read.listed, nodeWarning = listDownstreamNodes(ctx, c.clients, read)
	snap := readSnapshots(ctx, client, c.roots, read)

	done, nodesChecked := upgradeFinished(read, target)
	if clusterops.CompareVersions(target, facts.Current) == 0 && done {
		return safety.Effect{}, nil, finishedError(read, target, nodesChecked)
	}
	again := target == facts.Stored && !done

	after, gvr, objNS, row, err := buildUpgradeUpdate(read, target, again)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	diff := engine.DiffObjects(upgradeObject(read).Object, after.Object)
	eff, exec, err := engine.PlanUpdate(ctx, client, gvr, objNS, after, row.Redaction(), engine.LocalCluster, row.Name, "Cluster", diff, true)
	if err != nil {
		return eff, exec, err
	}
	eff.Reversible = false
	eff.Warnings = append(eff.Warnings, warnings...)
	eff.Warnings = append(eff.Warnings, upgradeWarnings(read, target, again, wait, rows, releases)...)
	if nodeWarning != "" {
		eff.Warnings = append(eff.Warnings, nodeWarning)
	}
	eff.Warnings = append(eff.Warnings, returnWarning(facts))
	eff.Warnings = append(eff.Warnings, snap.warning(name, again))

	p := upgradePlan{read: read, target: target, wait: wait, gvr: gvr, objNS: objNS, row: row, client: client, clients: c.clients}
	return eff, p.wrap(exec), nil
}

// upgradeObject is the object the command writes: the provisioning Cluster
// of a provisioned cluster, the management Cluster of an imported one.
func upgradeObject(r upgradeRead) *unstructured.Unstructured {
	if r.facts.Provisioned {
		return r.prov
	}
	return r.mgmt
}

// upgradeClusterID is the id of the cluster's management Cluster, "" when the
// provisioning Cluster has none yet.
func upgradeClusterID(prov *unstructured.Unstructured, facts clusterops.UpgradeCluster) string {
	if facts.ID != "" {
		return facts.ID
	}
	id, _, _ := unstructured.NestedString(prov.Object, "status", "clusterName")
	return id
}

// buildUpgradeUpdate returns the object to write and where to write it. Running
// the command again with the version already stored writes the object as read.
func buildUpgradeUpdate(r upgradeRead, target string, again bool) (after *unstructured.Unstructured, gvr schema.GroupVersionResource, ns string, row kinds.Kind, err error) {
	if r.facts.Provisioned {
		row = kindRow("provisioningcluster")
		after = r.prov.DeepCopy()
		gvr, ns = provisioningClusterGVR, r.ns
		if !again {
			err = unstructured.SetNestedField(after.Object, target, "spec", "kubernetesVersion")
		}
		return after, gvr, ns, row, err
	}
	row = kindRow("managementcluster")
	after = r.mgmt.DeepCopy()
	gvr = row.GVR
	if again {
		return after, gvr, "", row, nil
	}
	if err = unstructured.SetNestedField(after.Object, target, "spec", r.facts.Distro+"Config", "kubernetesVersion"); err != nil {
		return nil, gvr, "", row, err
	}
	if r.facts.AddAnnotation {
		err = unstructured.SetNestedField(after.Object, "system-default", "metadata", "annotations", versionManagementAnnotation)
	}
	return after, gvr, "", row, err
}

// noProvisioningClusterError is the refusal for a cluster with no provisioning
// Cluster in the namespace: an imported cluster whose management Cluster
// names no Fleet workspace has none. setIn says where the command's setting is
// made on such a cluster instead.
func noProvisioningClusterError(name, ns, setIn string) *clierr.Error {
	return clierr.New(clierr.CodeNotFound,
		fmt.Sprintf("cluster %s has no provisioning cluster in namespace %s; an imported cluster whose management cluster names no Fleet workspace has none, and %s", name, ns, setIn)).
		WithHintCommand(clierr.NewCommand("get provisioning-clusters").FlagValue("--namespace", ns))
}

// releaseListMissingError is the refusal for a server that has no release
// list route for the distribution: the CLI cannot check the version.
func releaseListMissingError(ctx context.Context, clients engine.ClusterClientFactory, distro string) *clierr.Error {
	route := rke2ReleasePath
	if distro == "k3s" {
		route = k3sReleasePath
	}
	server := "unknown"
	if versions, ok := clients.(engine.ServerVersionProvider); ok {
		if v, err := versions.ServerVersion(ctx); err == nil && v != "" {
			server = v
		}
	}
	return clierr.New(clierr.CodeCapabilityUnsupported,
		fmt.Sprintf("%s was not found: the server does not list %s releases; this CLI cannot check the version", route, clusterops.DistroName(distro))).
		WithDetails(clierr.CapabilityUnsupportedDetails{Feature: "cluster upgrade", Server: server})
}

// releasesOf returns the rows of one distribution as release rows.
func releasesOf(rows []Version, distro string) []clusterops.Release {
	var out []clusterops.Release
	for _, v := range rows {
		if v.Distro != distro {
			continue
		}
		r := clusterops.Release{Version: v.Version}
		if v.HasServerArgs {
			r.ServerArgs = map[string]any{}
		}
		out = append(out, r)
	}
	return out
}

// readManagementCluster reads the management Cluster of an imported cluster
// through Steve, in the shape the Kubernetes API returns it,
// and, when its annotation leaves the decision to the Setting, the Setting.
// A provisioned cluster, the cluster Rancher runs in and a cluster with no
// link yet need no read: nil is returned for them.
func readManagementCluster(ctx context.Context, client engine.ClusterClient, roots engine.RootClientFactory, prov *unstructured.Unstructured) (*unstructured.Unstructured, clusterops.SettingRead, error) {
	id, _, _ := unstructured.NestedString(prov.Object, "status", "clusterName")
	if clusterops.HasRKEConfig(prov.Object) || id == "" || id == engine.LocalCluster {
		return nil, clusterops.SettingRead{}, nil
	}
	mgmt, err := resources.ManagementClusterThroughSteve(ctx, roots, id)
	if err != nil {
		return nil, clusterops.SettingRead{}, err
	}
	if !clusterops.NeedsVersionManagementSetting(mgmt.Object) {
		return mgmt, clusterops.SettingRead{}, nil
	}
	obj, err := client.Get(ctx, settingsGVR, "", versionManagementSetting)
	if err != nil {
		return mgmt, clusterops.SettingRead{Code: clierr.From(err).Code}, nil
	}
	value, _ := engine.SettingValue(obj)
	return mgmt, clusterops.SettingRead{Value: value, Read: true}, nil
}

// checkRunningOperations refuses while Rancher runs an operation other than an
// upgrade on a provisioned cluster, and warns when the control plane cannot be
// read: the refusal then cannot be made.
func checkRunningOperations(ctx context.Context, client engine.ClusterClient, prov *unstructured.Unstructured, ns, name string) ([]string, error) {
	cp, err := fetchControlPlane(ctx, client, ns, name)
	if err != nil {
		return []string{fmt.Sprintf("cannot read the RKEControlPlane of cluster %s (%s); an etcd snapshot restore in progress would replace the version this command writes", name, clierr.From(err).Code)}, nil
	}
	for _, op := range clusterops.RunningOperations(prov.Object, cp.Object) {
		if op.Kind != clusterops.OperationUpgrade {
			return nil, runningOperationError(name, ns, op)
		}
	}
	return nil, nil
}
