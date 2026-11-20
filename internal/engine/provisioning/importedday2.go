// importedday2.go has the checks `create resource` runs for a manifest of the
// kinds through which Rancher takes etcd snapshots, restores and rotations of an
// imported RKE2 or K3s cluster (ETCDSnapshotSave, ETCDSnapshotRestore,
// EncryptionKeyRotation, CertificateRotation): the reads before the dry-run
// that tell whether Rancher can run the operation, and the comparison after the
// dry-run that tells whether the server kept every field the manifest sets.
package provisioning

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// operationsEnabledAnnotation turns day-2 operations on, off or, with any
	// other value or none, leaves the decision to the Setting of the next name.
	operationsEnabledAnnotation = "rancher.io/operations-enabled"
	day2OpsSetting              = "imported-cluster-day2-ops-enabled"

	// administratedAnnotation is "true" on the management Cluster of a cluster
	// Rancher provisions.
	administratedAnnotation = "provisioning.cattle.io/administrated"

	importedDay2OpsFeature = "imported-day-2-ops"
	provisioningV2Feature  = "provisioningv2"

	// certificateRotationResource is the resource that a server serves only
	// together with spec.args.restoreMode of ETCDSnapshotRestore: both arrive in
	// Rancher v2.16.
	certificateRotationResource = "certificaterotations"
)

// turtlesLabels are the labels of a management Cluster that Rancher Turtles
// manages as a Cluster API cluster.
var turtlesLabels = []string{"cluster-api.cattle.io/capi-cluster-owner", "cluster-api.cattle.io/capi-cluster-owner-ns"}

// ImportedDay2Check returns the create check of the kind row with the key: the
// reads of Rancher's preconditions before the dry-run and the comparison of the
// dry-run answer with the manifest after it. roots reads the management
// Cluster through Steve and the group version document; server returns the
// URL of the connected server, "" when it cannot be resolved.
func ImportedDay2Check(key string, roots engine.RootClientFactory, server func() string) resources.CreateCheck {
	return resources.CreateCheck{Key: key, Roots: roots, Server: server, Before: checkImportedDay2Create, After: checkImportedDay2Observed}
}

// checkImportedDay2Create reads what decides whether Rancher runs the
// operation the manifest names and refuses what the Rancher UI does not offer
// and Rancher cannot run, in the order: the cluster (exists, ready, an
// imported RKE2 or K3s cluster, day-2 operations on), the features, then the
// checks of the kind. A read that fails is a warning and the checks that
// needed it are left out; only a management Cluster that does not exist, and
// an etcd snapshot that does not exist, are errors.
func checkImportedDay2Create(ctx context.Context, in resources.CreateCheckInput) ([]string, error) {
	id, _, _ := unstructured.NestedString(in.Obj.Object, "spec", "clusterRef", "name")
	var warnings []string

	mgmtObj, err := resources.ManagementClusterThroughSteve(ctx, in.Roots, id)
	clusterRead := err == nil
	switch {
	case clierr.IsNotFound(err):
		return nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("cluster %s not found", id)).
			WithHintCommand(clierr.NewCommand("access clusters")).WithCause(err)
	case err != nil:
		warnings = append(warnings, fmt.Sprintf("could not read cluster %s to check that Rancher runs day-2 operations on it: %v", id, err))
	}
	var mgmt map[string]any
	if clusterRead {
		mgmt = mgmtObj.Object
		clusterWarnings, err := checkDay2Cluster(ctx, in.Local, id, mgmt)
		warnings = append(warnings, clusterWarnings...)
		if err != nil {
			return nil, err
		}
	}

	featureWarnings, err := checkDay2Features(ctx, in.Local)
	warnings = append(warnings, featureWarnings...)
	if err != nil {
		return nil, err
	}
	if !clusterRead {
		return warnings, nil
	}

	switch in.Obj.GetKind() {
	case "ETCDSnapshotRestore":
		name, _, _ := unstructured.NestedString(in.Obj.Object, "spec", "args", "name")
		snapshot, err := in.Local.Get(ctx, kindRow("etcdsnapshot").GVR, id, name)
		switch {
		case clierr.IsNotFound(err):
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("no etcd-snapshot %s in namespace %s", name, id)).
				WithHintCommand(clierr.NewCommand("get etcd-snapshot").FlagValue("--namespace", in.Obj.GetNamespace())).WithCause(err)
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("could not read etcd-snapshot %s to check it: %v", name, err))
		default:
			restoreWarnings, err := checkDay2Restore(ctx, in, mgmt, snapshot.Object, id, name)
			warnings = append(warnings, restoreWarnings...)
			if err != nil {
				return nil, err
			}
		}
	case "EncryptionKeyRotation":
		keyWarnings, err := checkDay2KeyRotation(ctx, in, id, time.Now())
		warnings = append(warnings, keyWarnings...)
		if err != nil {
			return nil, err
		}
	case "CertificateRotation":
		if err := checkDay2Services(in.Obj, mgmt, id); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

// checkDay2Cluster refuses a cluster the Rancher UI does not offer day-2
// operations on and Rancher does not run them on: a cluster that is not ready,
// the local cluster, a cluster Rancher provisions, a Turtles or Harvester
// cluster, one that is not an imported RKE2 or K3s cluster, and one on which
// day-2 operations are off. The Setting is read only when the annotation does
// not decide.
func checkDay2Cluster(ctx context.Context, local engine.ClusterClient, id string, mgmt map[string]any) ([]string, error) {
	if ready, condType, status, message := clusterops.Ready(mgmt); !ready {
		state := "it reports no Connected or Ready condition"
		if condType != "" {
			state = fmt.Sprintf("condition %s is %s", condType, status)
			if message != "" {
				state += ": " + message
			}
		}
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cluster %s is not ready (%s); the Rancher UI offers day-2 operations only on a ready cluster", id, state))
	}
	if err := checkDay2ClusterKind(id, mgmt); err != nil {
		return nil, err
	}

	annotations, _, _ := unstructured.NestedStringMap(mgmt, "metadata", "annotations")
	switch annotations[operationsEnabledAnnotation] {
	case "false":
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"day-2 operations are off for cluster %s: its annotation %s is \"false\"; Rancher runs them once the annotation is \"true\", and then installs its system agent on the cluster's nodes",
			id, operationsEnabledAnnotation))
	case "true":
		return nil, nil
	}
	setting, err := local.Get(ctx, settingsGVR, "", day2OpsSetting)
	if err != nil {
		return []string{fmt.Sprintf("could not read the setting %s to check that Rancher runs day-2 operations on cluster %s: %v", day2OpsSetting, id, err)}, nil
	}
	if value, _ := engine.SettingValue(setting); value != "true" {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"day-2 operations are off for cluster %s: the setting %s is \"%s\", and the cluster's annotation %s follows it",
			id, day2OpsSetting, value, operationsEnabledAnnotation))
	}
	return nil, nil
}

// checkDay2ClusterKind refuses a management Cluster that Rancher does not run
// day-2 operations on, in the order of Rancher's own rule.
func checkDay2ClusterKind(id string, mgmt map[string]any) error {
	annotations, _, _ := unstructured.NestedStringMap(mgmt, "metadata", "annotations")
	labels, _, _ := unstructured.NestedStringMap(mgmt, "metadata", "labels")
	switch {
	case id == engine.LocalCluster:
		return clierr.New(clierr.CodeValidation, "Rancher runs day-2 operations on imported clusters, not on the local cluster")
	case annotations[administratedAnnotation] == "true":
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"cluster %s is a provisioned cluster; Rancher takes its etcd snapshots and rotations through spec.rkeConfig of its provisioning cluster", id))
	}
	for _, label := range turtlesLabels {
		if _, ok := labels[label]; ok {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"cluster %s carries the Rancher Turtles label %s; this CLI does not create day-2 operations for it", id, label))
		}
	}
	if provider, _, _ := unstructured.NestedString(mgmt, "status", "info", "machineProvider"); provider == "harvester" {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("cluster %s is a Harvester cluster; Rancher turns day-2 operations off for it", id))
	}
	if _, imported := clusterops.ImportedDistribution(mgmt, true); !imported {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"cluster %s is not an imported RKE2 or K3s cluster; Rancher runs day-2 operations only on those", id))
	}
	return nil
}

// checkDay2Features refuses a server where the feature imported-day-2-ops or
// provisioningv2 is off, and warns for a feature it cannot read.
func checkDay2Features(ctx context.Context, local engine.ClusterClient) ([]string, error) {
	var warnings []string
	for _, f := range []struct{ name, consequence string }{
		{importedDay2OpsFeature, "Rancher runs no day-2 operations on imported clusters while it is off"},
		{provisioningV2Feature, "no Rancher controller acts on day-2 operation objects while it is off"},
	} {
		obj, err := local.Get(ctx, kindRow("feature").GVR, "", f.name)
		// Rancher never creates the provisioningv2 Feature object, so a 404 is the
		// answer of every version, and the feature is on by default: a missing
		// object says nothing against it.
		if clierr.IsNotFound(err) && f.name == provisioningV2Feature {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read the feature %s to check that it is on: %v", f.name, err))
			continue
		}
		on := engine.FeatureEffectiveValue(obj)
		switch {
		case on == nil:
			warnings = append(warnings, fmt.Sprintf("could not check that the feature %s is on: it has neither spec.value nor status.default", f.name))
		case !*on:
			return warnings, clierr.New(clierr.CodeValidation, fmt.Sprintf("the %s feature is off on the connected server; %s", f.name, f.consequence))
		}
	}
	return warnings, nil
}

// checkDay2Restore checks the etcd snapshot a restore names, read as snapshot:
// the snapshot is one the Rancher UI restores to the cluster, and the restore
// mode is one it offers. The mode rules apply only where the server serves
// CertificateRotation, which arrives with spec.args.restoreMode; the closing
// warning says what the restore does.
func checkDay2Restore(ctx context.Context, in resources.CreateCheckInput, mgmt, snapshot map[string]any, id, name string) ([]string, error) {
	switch clusterops.CheckRestorableSnapshot(snapshot, mgmt, id) {
	case clusterops.SnapshotNoClusterName:
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("etcd-snapshot %s has no spec.clusterName; the Rancher UI restores only a snapshot of cluster %s", name, id))
	case clusterops.SnapshotOtherCluster:
		other, _, _ := unstructured.NestedString(snapshot, "spec", "clusterName")
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("etcd-snapshot %s belongs to cluster %s, not to cluster %s", name, other, id))
	case clusterops.SnapshotNotSuccessful:
		status, _, _ := unstructured.NestedString(snapshot, "snapshotFile", "status")
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("etcd-snapshot %s has status %s; the Rancher UI restores only a successful snapshot", name, status))
	case clusterops.SnapshotNoCreatedAt:
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("etcd-snapshot %s has no snapshotFile.createdAt; the Rancher UI does not restore it", name))
	case clusterops.SnapshotBeforeImport:
		taken, _, _ := unstructured.NestedString(snapshot, "snapshotFile", "createdAt")
		imported, _, _ := unstructured.NestedString(mgmt, "metadata", "creationTimestamp")
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"etcd-snapshot %s was taken at %s, at or before the time cluster %s was imported into Rancher (%s); the Rancher UI does not restore a snapshot taken before the import",
			name, taken, id, imported))
	}

	var warnings []string
	mode, _, _ := unstructured.NestedString(in.Obj.Object, "spec", "args", "restoreMode")
	modesApply, warning := restoreModesApply(ctx, in.Roots)
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if modesApply {
		versionManagement, warning := versionManagementOn(ctx, in.Local, mgmt)
		if warning != "" {
			warnings = append(warnings, warning)
		} else {
			offered := clusterops.RestoreModes(versionManagement, snapshot)
			switch {
			case mode != "" && !slices.Contains(offered, mode):
				return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
					"spec.args.restoreMode %s is not offered for etcd-snapshot %s on cluster %s; the modes offered are: %s", mode, name, id, strings.Join(offered, ", ")))
			case mode == "":
				if def := clusterops.DefaultRestoreMode(offered); def != clusterops.RestoreModeNone {
					warnings = append(warnings, fmt.Sprintf(
						"spec.args.restoreMode is empty: Rancher restores etcd only; the Rancher UI's default for this snapshot is %s, which also writes the Kubernetes version and agent settings back", def))
				}
			}
		}
	}

	taken, _, _ := unstructured.NestedString(snapshot, "snapshotFile", "createdAt")
	closing := fmt.Sprintf("Rancher restores etcd of cluster %s to the snapshot taken at %s", id, taken)
	if where := snapshotLocation(snapshot); where != "" {
		closing += " " + where
	}
	if mode == clusterops.RestoreModeKubernetesVersion || mode == clusterops.RestoreModeAll {
		closing += " and writes the Kubernetes version and agent settings recorded in the snapshot back onto the cluster"
	}
	closing += "; every change after that time is lost"
	return append(warnings, closing), nil
}

// snapshotLocation says where an etcd snapshot is kept: in S3, or on the node
// that took it; "" when the object names neither.
func snapshotLocation(snapshot map[string]any) string {
	storage, _, _ := unstructured.NestedString(snapshot, "metadata", "annotations", "etcdsnapshot.rke.io/storage")
	if s3, found, _ := unstructured.NestedFieldNoCopy(snapshot, "snapshotFile", "s3"); storage == "s3" || (found && s3 != nil) {
		return "in S3"
	}
	if node, _, _ := unstructured.NestedString(snapshot, "snapshotFile", "nodeName"); node != "" {
		return "on node " + node
	}
	return ""
}

// restoreModesApply reads the resource list of the group version that
// ETCDSnapshotRestore is served in and reports whether the restore mode rules
// apply: only when the server lists CertificateRotation, which arrives with
// spec.args.restoreMode. A read that fails, or an answer that is not the
// resource list of that group version, gives the warning and no rules.
func restoreModesApply(ctx context.Context, roots engine.RootClientFactory) (bool, string) {
	gvr := kindRow("etcdsnapshotrestore").GVR
	groupVersion := gvr.Group + "/" + gvr.Version
	root, err := roots.Root()
	var body []byte
	if err == nil {
		body, err = root.GetJSON(ctx, "/k8s/clusters/"+engine.LocalCluster+"/apis/"+groupVersion)
	}
	if err != nil {
		return false, fmt.Sprintf("could not read the %s API version to check spec.args.restoreMode: %v", gvr.Group, err)
	}
	names, readable := engine.ResourceNames(body, groupVersion)
	if !readable {
		return false, fmt.Sprintf("could not read the %s API version to check spec.args.restoreMode: the answer is not the resource list of %s", gvr.Group, groupVersion)
	}
	return slices.Contains(names, certificateRotationResource), ""
}

// versionManagementOn reads whether Rancher manages the Kubernetes version of
// the cluster: the annotation decides, and the Setting is read only when it
// does not. A Setting that cannot be read gives the warning.
func versionManagementOn(ctx context.Context, local engine.ClusterClient, mgmt map[string]any) (bool, string) {
	value := ""
	if clusterops.NeedsVersionManagementSetting(mgmt) {
		setting, err := local.Get(ctx, settingsGVR, "", versionManagementSetting)
		if err != nil {
			return false, fmt.Sprintf("could not read the setting %s to check spec.args.restoreMode: %v", versionManagementSetting, err)
		}
		value, _ = engine.SettingValue(setting)
	}
	return clusterops.VersionManagementOn(mgmt, value), ""
}

// checkDay2KeyRotation requires an etcd snapshot of the cluster, as the
// Rancher UI does before it rotates the encryption keys, and warns when the
// newest is a day old or more. The snapshots are listed through the Kubernetes
// API and, only when that responds with 403, through Steve as the caller.
func checkDay2KeyRotation(ctx context.Context, in resources.CreateCheckInput, id string, now time.Time) ([]string, error) {
	items, err := listDay2Snapshots(ctx, in, id)
	if err != nil {
		return []string{fmt.Sprintf("could not list the etcd snapshots of cluster %s to check that one exists: %v", id, err)}, nil
	}
	objects := make([]map[string]any, 0, len(items))
	for _, item := range items {
		objects = append(objects, item.Object)
	}
	age := clusterops.NewestSnapshotAge(objects, id, now)
	if age.Count == 0 {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"cluster %s has no etcd snapshot; take one with an etcd-snapshot-save and rotate the keys once it has completed, to be able to restore the cluster if rotation fails", id))
	}
	if !age.Dated || age.Days < 1 {
		return nil, nil
	}
	old := fmt.Sprintf("%d days old", age.Days)
	if age.Days == 1 {
		old = "1 day old"
	}
	return []string{fmt.Sprintf("the newest etcd snapshot of cluster %s is %s; changes made since then cannot be restored if rotation fails", id, old)}, nil
}

// listDay2Snapshots lists the etcd snapshot objects in the cluster's namespace.
func listDay2Snapshots(ctx context.Context, in resources.CreateCheckInput, id string) ([]unstructured.Unstructured, error) {
	list, err := in.Local.List(ctx, kindRow("etcdsnapshot").GVR, id, "")
	if err != nil {
		return resources.EtcdSnapshotsAfterForbidden(ctx, in.Roots, id, err)
	}
	return list.Items, nil
}

// checkDay2Services refuses a service the cluster's distribution has no
// certificate for, in a certificate rotation's spec.args.services.
func checkDay2Services(obj *unstructured.Unstructured, mgmt map[string]any, id string) error {
	entries, _, _ := unstructured.NestedSlice(obj.Object, "spec", "args", "services")
	services := make([]string, 0, len(entries))
	for _, entry := range entries {
		if s, ok := entry.(string); ok {
			services = append(services, s)
		}
	}
	distro := clusterops.CommandDistribution(mgmt)
	if unknown, found := clusterops.FirstUnknownService(distro, services); found {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("cluster %s runs %s; it has no service %s; its services are: %s",
			id, clusterops.DistroName(distro), unknown, strings.Join(clusterops.CertificateServices(distro), ", ")))
	}
	return nil
}

// checkImportedDay2Observed compares the manifest with the server's dry-run
// answer: every field under spec that the manifest sets to a value must be in
// the answer. The API server removes the fields its version of the CRD does
// not declare, and the create sends no field validation, so a server that
// predates spec.args.restoreMode of ETCDSnapshotRestore or spec.cancel would
// store the object without the field and Rancher would ignore the setting.
func checkImportedDay2Observed(_ context.Context, in resources.CreateCheckInput, observed *unstructured.Unstructured) error {
	if observed == nil {
		return nil
	}
	spec, _, _ := unstructured.NestedMap(in.Obj.Object, "spec")
	for _, path := range specValuePaths(spec, []string{"spec"}) {
		if _, found, _ := unstructured.NestedFieldNoCopy(observed.Object, path...); found {
			continue
		}
		field := strings.Join(path, ".")
		kind := in.Obj.GetKind()
		if field == "spec.cancel" || (field == "spec.args.restoreMode" && kind == "ETCDSnapshotRestore") {
			server := ""
			if in.Server != nil {
				server = in.Server()
			}
			return clierr.New(clierr.CodeCapabilityUnsupported,
				fmt.Sprintf("the connected server does not support %s of %s, which Rancher v2.16 added", field, kind)).
				WithDetails(clierr.CapabilityUnsupportedDetails{Feature: removedFieldFeature(in.Obj, field), Server: server})
		}
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("the server removed %s from the object: its version of %s has no such field", field, kind))
	}
	return nil
}

// removedFieldFeature is the identifier of a field a server removed, in the
// form <plural>.<path>, as the capability errors of the kind rows name one.
func removedFieldFeature(obj *unstructured.Unstructured, field string) string {
	resource := obj.GetKind()
	if k, ok := kinds.LookupGVK(obj.GetAPIVersion(), obj.GetKind()); ok {
		resource = k.GVR.Resource
	}
	return resource + "." + field
}

// specValuePaths returns the paths, in sorted order, of the values a map holds
// below prefix: a scalar is a value; a list is one value, never one per entry;
// an empty map and an empty list are values; a null is not.
func specValuePaths(m map[string]any, prefix []string) [][]string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var paths [][]string
	for _, key := range keys {
		path := append(slices.Clone(prefix), key)
		switch v := m[key].(type) {
		case nil:
		case map[string]any:
			if len(v) == 0 {
				paths = append(paths, path)
				continue
			}
			paths = append(paths, specValuePaths(v, path)...)
		default:
			paths = append(paths, path)
		}
	}
	return paths
}
