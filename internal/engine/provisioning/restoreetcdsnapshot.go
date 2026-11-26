// restoreetcdsnapshot.go implements `cluster restore-etcd-snapshot <cluster>
// --snapshot <name>`: a restore of a provisioned RKE2 or K3s cluster from one of
// its etcd snapshot objects, requested the way the Rancher UI requests it, after
// the checks of the Rancher UI and of Rancher's planner, with a server-side
// dry-run preview and a wait until Rancher reports the restore finished.
package provisioning

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	restoreSnapshotFlag = "snapshot"
	restoreModeFlag     = "restore-mode"
)

// etcdSnapshotRestoreOperation is the etcd snapshot restore as Rancher runs
// it: requested by spec.rkeConfig.etcdSnapshotRestore, copied into the status
// of the RKEControlPlane, with its phase in etcdSnapshotRestorePhase.
var etcdSnapshotRestoreOperation = day2Operation{
	kind:       clusterops.OperationRestore,
	field:      "etcdSnapshotRestore",
	phaseField: "etcdSnapshotRestorePhase",
	doneAt:     phaseFinished,
	requested:  "etcd snapshot restore requested",
	done:       "restored from etcd snapshot",
}

type restoreEtcdSnapshotCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewRestoreEtcdSnapshot builds `cluster restore-etcd-snapshot <cluster>
// --snapshot <name>` (mutating): restores a provisioned RKE2 or K3s cluster from
// one of its etcd snapshot objects and waits until Rancher reports the restore
// finished.
func NewRestoreEtcdSnapshot(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &restoreEtcdSnapshotCmd{clients: clients, roots: roots}
}

func (restoreEtcdSnapshotCmd) Describe() runtime.Descriptor {
	flags := []runtime.ArgSpec{
		{Name: restoreSnapshotFlag, Type: "string", Required: true, Desc: "`name` of the etcd-snapshot to restore, the NAME column of `rancher get etcd-snapshots`"},
		{Name: restoreModeFlag, Type: "string", Enum: []string{clusterops.RestoreModeNone, clusterops.RestoreModeKubernetesVersion, clusterops.RestoreModeAll},
			Desc: "restore mode: none|kubernetesVersion|all (default: the Rancher UI's default for the snapshot)"},
		clusterNamespaceFlag(),
	}
	flags = append(flags, clusterWaitFlags()...)
	return runtime.Descriptor{
		Name:  "cluster restore-etcd-snapshot",
		Usage: "Restore an RKE2 or K3s cluster from one of its etcd snapshots",
		Desc: "Restores a provisioned cluster from one of its etcd snapshots by writing the next generation of " +
			"spec.rkeConfig.etcdSnapshotRestore on its provisioning cluster, as the Rancher UI does, then waits until Rancher " +
			"reports the restore finished. Rancher stops Kubernetes on every node of the cluster (its API server is down until the " +
			"restore ends), replaces the cluster's etcd data with the snapshot and starts the nodes again; every Kubernetes object " +
			"created or changed after the snapshot was taken is lost, and a restore cannot be undone. --snapshot takes the name " +
			"of an etcd-snapshot, the NAME column of `rancher get etcd-snapshots`. --restore-mode sets restoreRKEConfig " +
			"in that field: none restores etcd only, kubernetesVersion also sets the Kubernetes version recorded in the snapshot, " +
			"and all also restores the cluster configuration recorded in it. Without --restore-mode the command writes the " +
			"Rancher UI's default mode for the snapshot. Before the preview the command refuses an etcd snapshot that is " +
			"not a successful snapshot of this cluster taken after the cluster was created, a restore mode the snapshot does " +
			"not offer, and a snapshot taken before Kubernetes 1.25 for a cluster at 1.25 or newer with restore mode none. It also " +
			"refuses while another operation is in progress on the cluster, because Rancher runs one operation at a time, and " +
			"while the cluster is not ready. Running the command again with --apply --yes while its own restore is in progress " +
			"changes nothing and waits for that restore; once that restore has ended, running it again requests a new restore. " +
			"A run with --dry-run shows the phase of a restore in progress and sends no write. --wait=false returns once " +
			"the server accepts the request. Imported clusters, hosted " +
			"clusters and the local cluster are refused.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       flags,
		Examples: []string{
			"rancher cluster restore-etcd-snapshot my-cluster --snapshot my-snapshot --dry-run",
			"rancher cluster restore-etcd-snapshot my-cluster --snapshot my-snapshot --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

var _ runtime.MutatingCommand = (*restoreEtcdSnapshotCmd)(nil)

// LiveEnumFlags names the flags whose values the command's live source
// answers in place of the flag's static enum: the restore modes offered
// depend on the snapshot.
func (restoreEtcdSnapshotCmd) LiveEnumFlags() []string { return []string{restoreModeFlag} }

func (c *restoreEtcdSnapshotCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	snapshot, typed := in.String(restoreSnapshotFlag), in.String(restoreModeFlag)
	wait, err := readClusterWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	target, err := readDay2Target(ctx, c.clients, c.roots, in, etcdSnapshotRestoreOperation, day2Match{snapshot: snapshot, mode: typed})
	if err != nil {
		return safety.Effect{}, nil, err
	}
	p := &day2Plan{op: etcdSnapshotRestoreOperation, target: target, wait: wait, snapshot: snapshot, mode: typed}

	if target.waiting {
		name, _ := target.running.Request["name"].(string)
		mode := runningRestoreMode(target.running)
		p.data = map[string]any{"snapshot": name, "restoreMode": mode}
		p.done = "restored from etcd snapshot " + name
		warning := p.waitingRunWarning() + fmt.Sprintf("; the restore in progress is of etcd-snapshot %s with restore mode %s", name, mode)
		return p.buildEffect(ctx, []string{warning}, nil)
	}

	object, err := c.readSnapshot(ctx, target, snapshot, typed)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	check := clusterops.CheckProvisionedRestore(object.Object, target.prov.Object, typed)
	if err := restoreCheckError(check, object.Object, target, snapshot, typed); err != nil {
		return safety.Effect{}, nil, err
	}
	p.data = map[string]any{"snapshot": snapshot, "restoreMode": check.Mode}
	p.done = "restored from etcd snapshot " + snapshot

	warnings := []string{restoreWarning(object.Object, target, snapshot), restoreModeWarning(target, check)}
	if typed == "" {
		warnings = append(warnings, fmt.Sprintf("--restore-mode was not given: the Rancher UI's default for this snapshot, %s", check.Mode))
	}
	if restoreEnded(target, snapshot, check.Mode) {
		warnings = append(warnings, fmt.Sprintf("etcd-snapshot %s is the last restore of cluster %s and it has ended; this restores it again", snapshot, target.name))
	}
	if warning := restoreS3Warning(object.Object, target, snapshot); warning != "" {
		warnings = append(warnings, warning)
	}
	if check.Spec.State == clusterops.SnapshotSpecTooLarge {
		warnings = append(warnings, fmt.Sprintf("the cluster spec recorded in etcd-snapshot %s is larger than 16 MiB and was not read: the restore modes offered are those the annotation etcdsnapshot.rke.io/restore-mode-options lists (all three when the snapshot has no such annotation), and the check for a snapshot taken before Kubernetes 1.25 was not made", snapshot))
	}
	fields := map[string]any{"name": snapshot, "restoreRKEConfig": check.Mode}
	return p.buildEffect(ctx, warnings, fields)
}

// restoreEtcdSnapshotRecovery is the command that previews the request again:
// the same command with --dry-run, in the same namespace, with the restore
// mode as typed.
func restoreEtcdSnapshotRecovery(name, ns, snapshot, mode string) clierr.Command {
	cmd := clierr.NewCommand("cluster restore-etcd-snapshot").Arg(name)
	if ns != clusterNamespace {
		cmd = cmd.FlagValue("--namespace", ns)
	}
	if mode == "" {
		return cmd.FlagValue("--snapshot", snapshot).Flag("--dry-run")
	}
	return cmd.FlagValue("--snapshot", snapshot).FlagValue("--restore-mode", mode).Flag("--dry-run")
}

// readSnapshot reads the etcd snapshot object the restore names. A missing
// object is NOT_FOUND with the etcd snapshot objects of the cluster that the
// command would restore as candidates; a 403 is RBAC; any other error is
// returned as it is.
func (c *restoreEtcdSnapshotCmd) readSnapshot(ctx context.Context, t *day2Target, name, mode string) (*unstructured.Unstructured, error) {
	object, err := t.client.Get(ctx, kindRow("etcdsnapshot").GVR, t.ns, name)
	if err == nil {
		return object, nil
	}
	switch {
	case clierr.IsNotFound(err):
		return nil, c.noSnapshotError(ctx, t, name, mode).WithCause(err)
	case httpForbidden(err):
		return nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf("the etcd-snapshot %s in namespace %s could not be read (RBAC); Cluster Owner of the cluster grants get on its snapshots; an object Rancher created moments ago may not be readable yet", name, t.ns)).WithCause(err)
	}
	return nil, err
}

func httpForbidden(err error) bool {
	status, ok := clierr.HTTPStatus(err)
	return ok && status == 403
}

// noSnapshotError is the refusal for a name with no etcd snapshot object. One
// List of the namespace finds an object of the cluster whose snapshot file has
// that name, when the caller took the file name for the object name, and the
// restorable objects of the cluster otherwise. A List that fails adds neither.
func (c *restoreEtcdSnapshotCmd) noSnapshotError(ctx context.Context, t *day2Target, name, mode string) *clierr.Error {
	msg := fmt.Sprintf("no etcd-snapshot %s in namespace %s", name, t.ns)
	items, err := listEtcdSnapshots(ctx, t)
	if err != nil {
		return clierr.New(clierr.CodeNotFound, msg)
	}
	var own []map[string]any
	for _, item := range items {
		if clusterops.SnapshotOfCluster(item.Object, t.name) {
			own = append(own, item.Object)
		}
	}
	clusterops.SortSnapshotsNewestFirst(own)
	for _, object := range own {
		if file, _, _ := unstructured.NestedString(object, "snapshotFile", "name"); file == name {
			objectName, _, _ := unstructured.NestedString(object, "metadata", "name")
			return clierr.New(clierr.CodeNotFound, fmt.Sprintf("%s; %s is the snapshot file name of etcd-snapshot %s; --%s takes the etcd-snapshot name", msg, name, objectName, restoreSnapshotFlag))
		}
	}
	var candidates []clierr.Candidate
	for _, object := range own {
		if clusterops.CheckProvisionedRestore(object, t.prov.Object, mode).Problem != clusterops.ProvisionedRestoreAccepted {
			continue
		}
		id, _, _ := unstructured.NestedString(object, "metadata", "name")
		file, _, _ := unstructured.NestedString(object, "snapshotFile", "name")
		candidates = append(candidates, clierr.Candidate{ID: id, Name: file})
	}
	out := clierr.New(clierr.CodeNotFound, msg)
	if len(candidates) == 0 {
		return out
	}
	hint := clierr.NewCommand("cluster restore-etcd-snapshot").Arg(t.name)
	if t.ns != clusterNamespace {
		hint = hint.FlagValue("--namespace", t.ns)
	}
	return out.WithCandidatesCut(candidates, clierr.CandidateLimit).
		WithHintCommand(hint.FlagValue("--snapshot", candidates[0].ID).Flag("--dry-run"))
}

// restoreCheckError turns the first problem of the checks into its refusal; nil
// when there is none.
func restoreCheckError(check clusterops.ProvisionedRestore, object map[string]any, t *day2Target, name, typed string) *clierr.Error {
	field := func(path ...string) string {
		v, _, _ := unstructured.NestedString(object, path...)
		return v
	}
	validation := func(msg string) *clierr.Error {
		return clierr.New(clierr.CodeValidation, msg)
	}
	switch check.Problem {
	case clusterops.ProvisionedRestoreNoClusterName:
		return validation(fmt.Sprintf("etcd-snapshot %s has no spec.clusterName; the Rancher UI restores only a snapshot of cluster %s", name, t.name))
	case clusterops.ProvisionedRestoreOtherCluster:
		return validation(fmt.Sprintf("etcd-snapshot %s belongs to cluster %s (spec.clusterName); restore a snapshot of cluster %s", name, field("spec", "clusterName"), t.name))
	case clusterops.ProvisionedRestoreOtherLabel:
		label := field("metadata", "labels", "rke.cattle.io/cluster-name")
		return validation(fmt.Sprintf("etcd-snapshot %s has the label rke.cattle.io/cluster-name %s and spec.clusterName %s; the Rancher UI lists it under cluster %s, not under cluster %s", name, label, t.name, label, t.name))
	case clusterops.ProvisionedRestoreNotSuccessful:
		status := field("snapshotFile", "status")
		msg := fmt.Sprintf("etcd-snapshot %s has status %s; the Rancher UI restores only a successful snapshot", name, status)
		if status == "" {
			msg = fmt.Sprintf("etcd-snapshot %s has no snapshotFile.status; the Rancher UI restores only a successful snapshot", name)
		}
		if message := decodedSnapshotMessage(field("snapshotFile", "message")); message != "" {
			msg += ": " + message
		}
		return clierr.New(clierr.CodeValidation, msg)
	case clusterops.ProvisionedRestoreNoCreatedAt:
		return validation(fmt.Sprintf("etcd-snapshot %s has no snapshotFile.createdAt; the Rancher UI does not restore it", name))
	case clusterops.ProvisionedRestoreBeforeCluster:
		created, _, _ := unstructured.NestedString(t.prov.Object, "metadata", "creationTimestamp")
		return validation(fmt.Sprintf("etcd-snapshot %s was taken at %s, at or before the time cluster %s was created (%s); the Rancher UI does not restore a snapshot taken before the cluster was created",
			name, field("snapshotFile", "createdAt"), t.name, created))
	case clusterops.ProvisionedRestoreBadVersion:
		if check.Spec.KubernetesVersion == "" {
			return validation(fmt.Sprintf("the metadata of etcd-snapshot %s has no Kubernetes version; Rancher would fail the restore on every attempt", name))
		}
		return validation(fmt.Sprintf("the metadata of etcd-snapshot %s has Kubernetes version \"%s\", which Rancher cannot parse; Rancher would fail the restore on every attempt", name, cutVersion(check.Spec.KubernetesVersion)))
	case clusterops.ProvisionedRestoreModeNotOffered:
		return validation(fmt.Sprintf("--%s %s is not offered for etcd-snapshot %s; the modes offered are: %s", restoreModeFlag, typed, name, strings.Join(check.Modes, ", ")))
	case clusterops.ProvisionedRestoreBelow125:
		current, _, _ := unstructured.NestedString(t.prov.Object, "spec", "kubernetesVersion")
		msg := fmt.Sprintf("Rancher does not restore a snapshot taken before Kubernetes 1.25 onto a cluster at 1.25 or newer with restore type Only etcd (--restore-mode none); etcd-snapshot %s has Kubernetes version %s and cluster %s is at %s", name, check.Spec.KubernetesVersion, t.name, current)
		if passing := slices.DeleteFunc(slices.Clone(check.Modes), func(mode string) bool { return mode == clusterops.RestoreModeNone }); len(passing) > 0 {
			msg += fmt.Sprintf("; the --%s values that restore it are: %s", restoreModeFlag, strings.Join(passing, ", "))
		}
		return validation(msg)
	}
	return nil
}

// cutVersion is a version for a message: at most clusterops.MaxVersionLen
// bytes, cut at a character boundary, followed by "..." when it was cut.
func cutVersion(version string) string {
	if len(version) <= clusterops.MaxVersionLen {
		return version
	}
	end := clusterops.MaxVersionLen
	for end > 0 && !utf8.RuneStart(version[end]) {
		end--
	}
	return version[:end] + "..."
}

// decodedSnapshotMessage is snapshotFile.message as the Rancher UI shows it:
// the base64 decoded text when the value is valid base64 that decodes to valid
// UTF-8, else the value as stored.
func decodedSnapshotMessage(message string) string {
	if decoded, err := base64.StdEncoding.DecodeString(message); err == nil && utf8.Valid(decoded) {
		return string(decoded)
	}
	return message
}

// snapshotWhere is the parenthesis of the restore warning after the time: the
// bucket of an S3 snapshot, else the node that has the file, else nothing.
func snapshotWhere(object map[string]any) string {
	if s3, found, _ := unstructured.NestedFieldNoCopy(object, "snapshotFile", "s3"); found && s3 != nil {
		if bucket, _, _ := unstructured.NestedString(object, "snapshotFile", "s3", "bucket"); bucket != "" {
			return "in S3 bucket " + bucket
		}
		return "in S3"
	}
	if node, _, _ := unstructured.NestedString(object, "snapshotFile", "nodeName"); node != "" {
		return "on node " + node
	}
	return ""
}

// restoreWarning says what Rancher does to the cluster and what the restore
// loses.
func restoreWarning(object map[string]any, t *day2Target, name string) string {
	taken, _, _ := unstructured.NestedString(object, "snapshotFile", "createdAt")
	when := "taken " + taken
	if where := snapshotWhere(object); where != "" {
		when += ", " + where
	}
	return fmt.Sprintf("Rancher stops Kubernetes on every node of cluster %s (its API server is down until the restore ends), removes the server certificate files on the etcd and control plane nodes, replaces the cluster's etcd data with etcd-snapshot %s (%s), then starts the nodes again: every Kubernetes object created or changed after that time is lost, objects deleted since then come back, and Rancher deletes the Node objects of nodes that are no longer part of the cluster; a restore cannot be undone, only followed by the restore of another snapshot",
		t.name, name, when)
}

// restoreModeWarning says what the restore mode replaces, with the labels of
// the Rancher UI.
func restoreModeWarning(t *day2Target, check clusterops.ProvisionedRestore) string {
	current, _, _ := unstructured.NestedString(t.prov.Object, "spec", "kubernetesVersion")
	target := "the version recorded in the snapshot"
	if check.Spec.State == clusterops.SnapshotSpecRead {
		target = check.Spec.KubernetesVersion
	}
	switch check.Mode {
	case clusterops.RestoreModeKubernetesVersion:
		if check.Spec.State != clusterops.SnapshotSpecRead {
			return fmt.Sprintf("restore type Kubernetes version and etcd: Rancher first sets spec.kubernetesVersion from %s to %s", current, target)
		}
		return fmt.Sprintf("restore type Kubernetes version and etcd: Rancher first sets spec.kubernetesVersion from %s to %s, the version recorded in the snapshot", current, target)
	case clusterops.RestoreModeAll:
		return fmt.Sprintf("restore type Cluster config, Kubernetes version and etcd: Rancher first replaces spec.kubernetesVersion (%s to %s) and, from the snapshot, spec.rkeConfig machineGlobalConfig, machineSelectorConfig, chartValues, registries, upgradeStrategy, additionalManifest and networking, spec.defaultPodSecurityAdmissionConfigurationTemplateName, and the cluster agent, Fleet agent and webhook deployment customizations; machine pools are not restored", current, target)
	}
	return "restore type Only etcd: the cluster's configuration and Kubernetes version stay as they are"
}

// restoreEnded reports whether the request on the provisioning Cluster is a
// restore of the same etcd snapshot object with the same mode that Rancher
// ended: the status copy on the RKEControlPlane equals it and its phase is
// Finished.
func restoreEnded(t *day2Target, snapshot, mode string) bool {
	if t.controlPlane == nil {
		return false
	}
	modeOf := func(request map[string]any) string {
		if value, _ := request["restoreRKEConfig"].(string); value != "" {
			return value
		}
		return clusterops.RestoreModeNone
	}
	request, found, _ := unstructured.NestedMap(t.prov.Object, "spec", "rkeConfig", "etcdSnapshotRestore")
	if !found {
		return false
	}
	applied, found, _ := unstructured.NestedMap(t.controlPlane.Object, "status", "etcdSnapshotRestore")
	if !found {
		return false
	}
	phase, _, _ := unstructured.NestedString(t.controlPlane.Object, "status", "etcdSnapshotRestorePhase")
	name, _ := request["name"].(string)
	return phase == phaseFinished && name == snapshot && modeOf(request) == mode &&
		modeOf(applied) == mode && applied["name"] == request["name"] && numberOf(applied["generation"]) == numberOf(request["generation"])
}

// restoreS3Warning is the warning for a snapshot stored in S3 when neither the
// etcd snapshot object nor the cluster names a cloud credential: Rancher then
// restores it with no S3 credentials. "" when the snapshot is not in S3 or a
// credential is named.
func restoreS3Warning(object map[string]any, t *day2Target, name string) string {
	s3, found, _ := unstructured.NestedFieldNoCopy(object, "snapshotFile", "s3")
	if !found || s3 == nil {
		return ""
	}
	if credential, _, _ := unstructured.NestedString(object, "snapshotFile", "s3", "cloudCredentialName"); credential != "" {
		return ""
	}
	if credential, _, _ := unstructured.NestedString(t.prov.Object, "spec", "rkeConfig", "etcd", "s3", "cloudCredentialName"); credential != "" {
		return ""
	}
	where := "in S3"
	if bucket, _, _ := unstructured.NestedString(object, "snapshotFile", "s3", "bucket"); bucket != "" {
		where += " bucket " + bucket
	}
	return fmt.Sprintf("etcd-snapshot %s is %s; cluster %s names no S3 cloud credential in spec.rkeConfig.etcd.s3: Rancher restores it with no S3 credentials", name, where, t.name)
}
