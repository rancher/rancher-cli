// saveetcdsnapshot.go implements `cluster save-etcd-snapshot <cluster>`: an
// etcd snapshot of a provisioned RKE2 or K3s cluster, requested the way the
// Rancher UI requests it, with a server-side dry-run preview and a wait until
// Rancher reports the snapshot done.
package provisioning

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maxSavedSnapshots limits the snapshots the result lists.
const maxSavedSnapshots = 10

// etcdSnapshotOperation is the etcd snapshot as Rancher runs it: requested by
// spec.rkeConfig.etcdSnapshotCreate, copied into the status of the
// RKEControlPlane, with its phase in etcdSnapshotCreatePhase.
var etcdSnapshotOperation = day2Operation{
	kind:       clusterops.OperationSnapshot,
	field:      "etcdSnapshotCreate",
	phaseField: "etcdSnapshotCreatePhase",
	doneAt:     phaseFinished,
	requested:  "etcd snapshot requested",
	done:       "etcd snapshot taken",
}

type saveEtcdSnapshotCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewSaveEtcdSnapshot builds `cluster save-etcd-snapshot <cluster>`
// (mutating): requests an etcd snapshot of a provisioned RKE2 or K3s cluster
// and waits until Rancher reports it done.
func NewSaveEtcdSnapshot(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &saveEtcdSnapshotCmd{clients: clients, roots: roots}
}

func (saveEtcdSnapshotCmd) Describe() runtime.Descriptor {
	flags := []runtime.ArgSpec{clusterNamespaceFlag()}
	flags = append(flags, clusterWaitFlags()...)
	return runtime.Descriptor{
		Name:  "cluster save-etcd-snapshot",
		Usage: "Take an etcd snapshot of an RKE2 or K3s cluster",
		Desc: "Requests an etcd snapshot of a provisioned cluster by writing the next generation of " +
			"spec.rkeConfig.etcdSnapshotCreate on its provisioning cluster, as the Rancher UI does, then waits until " +
			"Rancher reports the snapshot taken and lists the cluster's snapshots. Rancher takes the snapshot " +
			"with its own controllers on every etcd node. The command refuses while another operation is in progress " +
			"on the cluster, because Rancher runs one operation at a time, and while the cluster is not ready. " +
			"Running the command again while its own snapshot is in progress changes nothing and waits for that snapshot. " +
			"--wait=false returns once the server accepts the request. Imported clusters, hosted " +
			"clusters and the local cluster are refused.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       flags,
		Examples: []string{
			"rancher cluster save-etcd-snapshot my-cluster --dry-run",
			"rancher cluster save-etcd-snapshot my-cluster --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

var _ runtime.MutatingCommand = (*saveEtcdSnapshotCmd)(nil)

func (c *saveEtcdSnapshotCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	wait, err := readClusterWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	target, err := readDay2Target(ctx, c.clients, c.roots, in, etcdSnapshotOperation)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	p := &day2Plan{op: etcdSnapshotOperation, target: target, wait: wait}
	warning := saveWarning(target)
	if target.waiting {
		warning = p.waitingRunWarning()
	}
	return p.buildEffect(ctx, []string{warning})
}

// saveEtcdSnapshotRecovery is the command that previews the request again:
// the same command with --dry-run, in the same namespace.
func saveEtcdSnapshotRecovery(name, ns string) clierr.Command {
	cmd := clierr.NewCommand("cluster save-etcd-snapshot").Arg(name)
	if ns != clusterNamespace {
		cmd = cmd.FlagValue("--namespace", ns)
	}
	return cmd.Flag("--dry-run")
}

// saveWarning says what Rancher does for the request: where it runs the
// snapshot, whether it also sends the file to S3 because
// spec.rkeConfig.etcd.s3 is set, and where the etcd snapshot object of each
// file appears.
func saveWarning(t *day2Target) string {
	runtimeName := "rke2"
	if version, _, _ := unstructured.NestedString(t.prov.Object, "spec", "kubernetesVersion"); strings.Contains(version, "k3s") {
		runtimeName = "k3s"
	}
	msg := fmt.Sprintf("Rancher runs `%s etcd-snapshot save` on every etcd node of cluster %s; each etcd node keeps its snapshot file", runtimeName, t.name)
	if s3, found, _ := unstructured.NestedFieldNoCopy(t.prov.Object, "spec", "rkeConfig", "etcd", "s3"); found && s3 != nil {
		bucket, _, _ := unstructured.NestedString(t.prov.Object, "spec", "rkeConfig", "etcd", "s3", "bucket")
		if bucket == "" {
			msg += ", and because spec.rkeConfig.etcd.s3 is set, Rancher also sends the file to S3 (the bucket of the cloud credential)"
		} else {
			msg += ", and because spec.rkeConfig.etcd.s3 is set, Rancher also sends the file to S3 bucket " + bucket
		}
	}
	return msg + fmt.Sprintf("; Rancher creates an etcd snapshot object in namespace %s for each snapshot file once it reads the file from the cluster", t.ns)
}

// listClusterSnapshots adds the cluster's snapshots to the result, newest
// first. Rancher creates an etcd snapshot object for a snapshot file once it
// reads the file from the cluster. The List goes through the Kubernetes API
// and, only when that responds with 403, through Steve as the caller: a Cluster
// Owner lists the objects it may read there. When the List finds no etcd
// snapshot object of the cluster (refused both ways, failed, or the object for
// the snapshot just taken is not readable yet), the result has a warning and
// no snapshots. The command does not tell which object this run created.
func listClusterSnapshots(ctx context.Context, t *day2Target, data map[string]any) []string {
	var records []map[string]any
	items, err := listEtcdSnapshots(ctx, t)
	if err == nil {
		for _, item := range items {
			if clusterops.SnapshotOfCluster(item.Object, t.name) {
				records = append(records, item.Object)
			}
		}
	}
	if len(records) == 0 {
		return []string{clierr.Text(
			"Rancher creates an etcd snapshot object for each snapshot file once it reads the file from the cluster, and an object it created moments ago may not be readable yet",
			clierr.Lead("; `"),
			clierr.NewCommand("get etcd-snapshot").FlagValue("--namespace", t.ns),
			"` lists them for an admin or a user with the Create Clusters role")}
	}
	clusterops.SortSnapshotsNewestFirst(records)
	records = records[:min(len(records), maxSavedSnapshots)]
	entries := make([]any, 0, len(records))
	for _, rec := range records {
		name, _, _ := unstructured.NestedString(rec, "metadata", "name")
		node, _, _ := unstructured.NestedString(rec, "snapshotFile", "nodeName")
		created, _, _ := unstructured.NestedString(rec, "snapshotFile", "createdAt")
		status, _, _ := unstructured.NestedString(rec, "snapshotFile", "status")
		entries = append(entries, map[string]any{"name": name, "nodeName": node, "createdAt": created, "status": status})
	}
	data["snapshots"] = entries
	return nil
}

// listEtcdSnapshots lists the etcd snapshot objects of the cluster's
// namespace through the Kubernetes API and, only when that responds with 403,
// through Steve as the caller.
func listEtcdSnapshots(ctx context.Context, t *day2Target) ([]unstructured.Unstructured, error) {
	list, err := t.client.List(ctx, kindRow("etcdsnapshot").GVR, t.ns, "")
	if err != nil {
		return resources.EtcdSnapshotsAfterForbidden(ctx, t.roots, t.ns, err)
	}
	return list.Items, nil
}
