package clusterops

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const (
	// versionManagementAnnotation is the annotation of an imported RKE2 or
	// K3s management Cluster that turns Rancher's management of its
	// Kubernetes version on, off, or leaves it to a setting.
	versionManagementAnnotation = "rancher.io/imported-cluster-version-management"

	// versionManagementPausedAnnotation is set to "true" while Rancher restores
	// an etcd snapshot of an imported cluster.
	versionManagementPausedAnnotation = "management.cattle.io/imported-cluster-version-management-paused"

	versionManagementSetting = "imported-cluster-version-management"
)

// SettingRead is the imported-cluster-version-management Setting as read.
type SettingRead struct {
	Value string
	// Read is false when the read failed; Code is then the code of the error.
	Read bool
	Code string
}

// UpgradeCluster has the facts about a cluster that the version rules need,
// read from its objects by CheckUpgradeCluster.
type UpgradeCluster struct {
	// Name is the name of the provisioning Cluster; ID is the id of its
	// management Cluster, empty when the cluster is provisioned.
	Name string
	ID   string
	// Provisioned is true when Rancher provisions the cluster (it has
	// spec.rkeConfig), false for an imported one.
	Provisioned bool
	// Distro is "rke2" or "k3s".
	Distro string
	// Current is the version the cluster runs as the Rancher UI reads it: a
	// provisioned cluster's spec.kubernetesVersion; an imported cluster's
	// status.version.gitVersion, else the version of its config.
	Current string
	// Stored is the version the write sets: spec.kubernetesVersion of the
	// provisioning Cluster, or the kubernetesVersion of the imported
	// cluster's config.
	Stored string
	// ManuallyUpgraded is true for an imported cluster whose config version
	// is lower than the version it runs.
	ManuallyUpgraded bool
	// AddAnnotation is true when the write also sets the version management
	// annotation to system-default: an imported cluster that has none.
	AddAnnotation bool
	// AzureInTree is true for a provisioned cluster that uses the in-tree
	// azure cloud provider.
	AzureInTree bool
}

// NeedsVersionManagementSetting reports whether the annotation
// rancher.io/imported-cluster-version-management of the management Cluster
// leaves the decision to the setting of the same name: it is system-default,
// empty or absent. The caller reads the Setting only then.
func NeedsVersionManagementSetting(managementCluster map[string]any) bool {
	switch nestedString(managementCluster, "metadata", "annotations", versionManagementAnnotation) {
	case "", "system-default":
		return true
	}
	return false
}

// versionManagementHint is the command that turns version management on for an
// imported cluster, as a preview.
func versionManagementHint(id string) clierr.Command {
	return clierr.NewCommand("patch cluster").Arg(id).
		FlagText(` --type merge --patch '{"metadata":{"annotations":{"rancher.io/imported-cluster-version-management":"true"}}}'`).
		Flag("--dry-run")
}

func versionsHint() clierr.Command {
	return clierr.NewCommand("get kubernetes-versions")
}

// hostedConfigs are the hosted-cluster config fields of a management Cluster
// spec, with the driver each one belongs to.
var hostedConfigs = []struct{ field, driver, config string }{
	{"eksConfig", "EKS", "eksConfig"},
	{"aksConfig", "AKS", "aksConfig"},
	{"gkeConfig", "GKE", "gkeConfig"},
	{"aliConfig", "Alibaba", "aliConfig"},
}

// hostedCluster returns the driver name and config field of a hosted
// management Cluster: recognized by a non-null config first, else by its
// driver.
func hostedCluster(managementCluster map[string]any) (driver, config string, ok bool) {
	for _, h := range hostedConfigs {
		if v, found := nested(managementCluster, "spec", h.field); found && v != nil {
			return h.driver, h.config, true
		}
	}
	driver = nestedString(managementCluster, "status", "driver")
	for _, h := range hostedConfigs {
		if driver == h.driver {
			return h.driver, h.config, true
		}
	}
	return "", "", false
}

// CheckUpgradeCluster checks from the objects of the cluster alone, before
// any release row is read, whether the cluster can be upgraded. cluster
// is the provisioning Cluster, managementCluster its management Cluster (nil
// for a provisioned cluster), setting the version management Setting as read
// (used only when NeedsVersionManagementSetting holds). It returns the facts
// of the cluster, or the first refusal in this order: the cluster Rancher
// runs in; for a provisioned cluster the current version; for an imported
// cluster the link to its management Cluster, hosted clusters, the driver,
// the pause annotation, version management, readiness and then the current
// version.
func CheckUpgradeCluster(cluster, managementCluster map[string]any, setting SettingRead) (UpgradeCluster, *clierr.Error) {
	name := nestedString(cluster, "metadata", "name")
	id := nestedString(cluster, "status", "clusterName")
	if err := refuseLocal(id); err != nil {
		return UpgradeCluster{}, err
	}

	if HasRKEConfig(cluster) {
		stored := nestedString(cluster, "spec", "kubernetesVersion")
		c := UpgradeCluster{Name: name, Provisioned: true, Distro: provisionedDistro(stored), Current: stored, Stored: stored, AzureInTree: azureInTree(cluster)}
		if err := checkCurrentVersion(c, name, true, "provisioning cluster "+name+" in namespace "+nestedString(cluster, "metadata", "namespace")); err != nil {
			return UpgradeCluster{}, err
		}
		return c, nil
	}

	driver, err := checkImportedDriver(name, id, managementCluster)
	if err != nil {
		return UpgradeCluster{}, err
	}

	hint := versionManagementHint(id)
	annotation, annotated := nested(managementCluster, "metadata", "annotations", versionManagementAnnotation)
	value, _ := annotation.(string)
	switch {
	case nestedString(managementCluster, "metadata", "annotations", versionManagementPausedAnnotation) == "true":
		return UpgradeCluster{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("Rancher paused version management of cluster %s for an etcd snapshot restore; try again after the restore", name)).WithHintCommand(hint)
	case value == "false":
		return UpgradeCluster{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("version management is off for cluster %s: annotation %s is false", name, versionManagementAnnotation)).WithHintCommand(hint)
	case unknownAnnotationValue(value):
		return UpgradeCluster{}, unknownAnnotationError(name, value, hint)
	case value == "" || value == "system-default":
		if !setting.Read {
			return UpgradeCluster{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("cannot read the setting %s (%s); set the annotation to true or false on cluster %s", versionManagementSetting, setting.Code, name)).WithHintCommand(hint)
		}
		if setting.Value != "true" {
			annotationText := "absent"
			switch {
			case value == "system-default":
				annotationText = value
			case annotated:
				annotationText = "empty"
			}
			settingText := setting.Value
			if settingText == "" {
				settingText = "empty"
			}
			return UpgradeCluster{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("version management is off for cluster %s: its annotation %s is %s and the setting %s is %s", name, versionManagementAnnotation, annotationText, versionManagementSetting, settingText)).WithHintCommand(hint)
		}
	}

	configVersion, err := checkImportedReported(name, driver, managementCluster)
	if err != nil {
		return UpgradeCluster{}, err
	}

	current := nestedString(managementCluster, "status", "version", "gitVersion")
	if current == "" {
		current = configVersion
	}
	c := UpgradeCluster{
		Name: name, ID: id, Distro: driver, Current: current, Stored: configVersion,
		ManuallyUpgraded: CompareVersions(configVersion, current) < 0,
		AddAnnotation:    value == "",
	}
	if err := checkCurrentVersion(c, name, false, "management cluster "+id); err != nil {
		return UpgradeCluster{}, err
	}
	return c, nil
}

// refuseLocal refuses the cluster Rancher runs in, by the id of its management
// Cluster.
func refuseLocal(id string) *clierr.Error {
	if id == localCluster {
		return clierr.New(clierr.CodeValidation, "this CLI does not upgrade the local cluster")
	}
	return nil
}

// checkImportedDriver is the part of the checks of an imported cluster that
// reads its link and its driver, in this order: the link to the management
// Cluster, a hosted cluster, an empty driver and a driver other than rke2 and
// k3s. It returns the driver.
func checkImportedDriver(name, id string, managementCluster map[string]any) (string, *clierr.Error) {
	if id == "" {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("Rancher has not linked cluster %s to its management cluster yet; try again later", name)).WithRetryable(true)
	}
	if driver, config, hosted := hostedCluster(managementCluster); hosted {
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cluster %s is a hosted %s cluster; its Kubernetes version is set in spec.%s, which this command does not change yet", name, driver, config)).
			WithHintCommand(clierr.NewCommand("get cluster").Arg(id))
	}
	driver := nestedString(managementCluster, "status", "driver")
	if driver == "" {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("Rancher has not detected the distribution of cluster %s yet; try again later", name)).WithRetryable(true)
	}
	if driver != distroRKE2 && driver != distroK3s {
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("Rancher manages the Kubernetes version only of RKE2 and K3s clusters; cluster %s has driver %s", name, driver))
	}
	return driver, nil
}

// unknownAnnotationValue reports whether a value of the version management
// annotation is one Rancher refuses: anything but true, false, system-default
// and empty.
func unknownAnnotationValue(value string) bool {
	return value != "" && value != "true" && value != "false" && value != "system-default"
}

func unknownAnnotationError(name, value string, hint clierr.Command) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("the annotation %s of cluster %s is %s; Rancher accepts only true, false or system-default", versionManagementAnnotation, name, value)).WithHintCommand(hint)
}

// checkImportedReported refuses an imported cluster that has not reported its
// version: no config of its distribution, an empty version in it, or no
// status.version. It returns the version of the config.
func checkImportedReported(name, driver string, managementCluster map[string]any) (string, *clierr.Error) {
	config := nestedMap(managementCluster, "spec", driver+"Config")
	configVersion := nestedString(config, "kubernetesVersion")
	running, _ := nested(managementCluster, "status", "version")
	if config == nil || configVersion == "" || running == nil {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("cluster %s has not reported its version yet; try again later", name)).WithRetryable(true)
	}
	return configVersion, nil
}

// provisionedDistro is the distribution of a provisioned cluster: k3s when its
// version names it, else rke2, as Rancher decides it.
func provisionedDistro(version string) string {
	if strings.Contains(version, "k3s") {
		return distroK3s
	}
	return distroRKE2
}

// azureInTree reports whether the provisioning Cluster sets the in-tree azure
// cloud provider in the machine selector config that has no label selector.
func azureInTree(cluster map[string]any) bool {
	v, _ := nested(cluster, "spec", "rkeConfig", "machineSelectorConfig")
	entries, _ := v.([]any)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["machineLabelSelector"] != nil {
			continue
		}
		return nestedString(entry, "config", "cloud-provider-name") == "azure"
	}
	return false
}

// preRelease reports whether a version has a pre-release part: a "-" in the
// text before the build suffix.
func preRelease(version string) bool {
	core, _, _ := strings.Cut(version, "+")
	return strings.Contains(core, "-")
}

// checkCurrentVersion refuses a current version the CLI cannot compare: a
// pre-release, an empty one, or one that does not parse. where names the
// field and object a person changes it on.
func checkCurrentVersion(c UpgradeCluster, name string, provisioned bool, where string) *clierr.Error {
	field := "spec.kubernetesVersion"
	if !provisioned {
		field = "spec." + c.Distro + "Config.kubernetesVersion"
	}
	if _, _, _, _, ok := ParseVersionKey(c.Current); preRelease(c.Current) || !ok {
		shown := c.Current
		if shown == "" {
			shown = "none"
		}
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("the current version of cluster %s (%s) is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the version to upgrade to; change %s of %s", name, shown, field, where))
	}
	return nil
}

// CheckUpgradeTarget returns the refusal of a target version for a cluster
// CheckUpgradeCluster accepted, or nil when the target is accepted. releases
// are the rows of the cluster's distribution. The order of the checks: a
// target that cannot be compared (pre-release, unparseable), another
// distribution, a version the server does not list, a lower version, for an
// imported cluster a version Rancher would not upgrade to, and for a
// provisioned cluster the removed azure in-tree cloud provider.
func CheckUpgradeTarget(c UpgradeCluster, releases []Release, target string) *clierr.Error {
	if preRelease(target) {
		return notComparable(target, c.Current)
	}
	major, minor, patch, _, ok := ParseVersionKey(target)
	if !ok {
		return notComparable(target, c.Current)
	}
	if provisionedDistro(target) != c.Distro {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cluster %s runs %s; Rancher does not change the distribution of a cluster", c.Name, DistroName(c.Distro))).WithHintCommand(versionsHint())
	}
	if !slices.ContainsFunc(releases, func(r Release) bool { return r.Version == target && r.HasServerArgs() }) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("%s is not %s release this server offers for an upgrade", target, distroNameWithArticle(c.Distro))).WithHintCommand(versionsHint())
	}
	if CompareVersions(target, c.Current) < 0 {
		return clierr.New(clierr.CodeValidation, lowerVersionText(c, target)).WithHintCommand(versionsHint())
	}
	if !c.Provisioned && textLower(target, c.Current) {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("Rancher compares %s with %s as text, where %s is lower than %s, and would not upgrade an imported cluster to it",
				target, c.Current, buildSuffix(target), buildSuffix(c.Current)))
	}
	if c.Provisioned && c.AzureInTree && CompareTriple(major, minor, patch, 1, 30, 0, false) >= 0 {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cluster %s uses the azure in-tree cloud provider, which Kubernetes 1.30 removed; the Rancher UI does not offer 1.30 or newer for it", c.Name)).WithHintCommand(versionsHint())
	}
	return nil
}

// notComparable is the refusal of a version typed that is a pre-release or
// does not parse.
func notComparable(target, current string) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the current version %s", target, current)).
		WithHintCommand(versionsHint())
}

// buildSuffix is the text after the plus sign of a version.
func buildSuffix(version string) string {
	_, build, _ := strings.Cut(version, "+")
	return build
}

// DistroName is the distribution as it reads in text: RKE2 or K3s.
func DistroName(distro string) string {
	if distro == distroK3s {
		return "K3s"
	}
	return "RKE2"
}

// distroNameWithArticle is DistroName with the article the name takes.
func distroNameWithArticle(distro string) string {
	if distro == distroK3s {
		return "a K3s"
	}
	return "an RKE2"
}

func lowerVersionText(c UpgradeCluster, target string) string {
	if c.Provisioned {
		return fmt.Sprintf("%s is lower than the current version %s; the Rancher UI does not offer a lower version (it lists older versions disabled). "+
			"To return to an earlier version, restore an etcd snapshot of type Kubernetes version and etcd; it sets the version recorded in the snapshot and restores etcd to its state when the snapshot was taken",
			target, c.Current)
	}
	return fmt.Sprintf("%s is lower than the current version %s; the Rancher UI does not offer a lower version (it lists older versions disabled). "+
		"To return to an earlier version on Rancher v2.16 and later, restore an etcd snapshot of type Kubernetes version and etcd; "+
		"it sets the version recorded in the snapshot and restores etcd to its state when the snapshot was taken (earlier servers cannot restore the version of an imported cluster)",
		target, c.Current)
}

// textLower reports whether Rancher's text comparison of an imported
// cluster's versions puts target below current while CompareVersions puts it
// above: the same semver core, and a build suffix that is lower as a string
// ("rke2r10" is lower than "rke2r9").
func textLower(target, current string) bool {
	if CompareVersions(target, current) <= 0 {
		return false
	}
	targetCore, targetBuild, _ := strings.Cut(target, "+")
	currentCore, currentBuild, _ := strings.Cut(current, "+")
	return targetCore == currentCore && strings.Compare(targetBuild, currentBuild) < 0
}

// AcceptedTargets returns the versions of releases for which CheckUpgradeTarget
// returns nil, newest first by CompareVersions, verbatim, each once. It never
// returns the version equal to the current one, whether or not the cluster
// finished the upgrade to it. A caller offers exactly this list.
func AcceptedTargets(c UpgradeCluster, releases []Release) []string {
	var out []string
	for _, r := range releases {
		if !r.HasServerArgs() || CompareVersions(r.Version, c.Current) == 0 {
			continue
		}
		if CheckUpgradeTarget(c, releases, r.Version) == nil {
			out = append(out, r.Version)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int {
		return cmp.Or(CompareVersions(b, a), strings.Compare(a, b))
	})
	return slices.Compact(out)
}

// NewestSuccessfulSnapshot returns the newest etcd snapshot object of the
// cluster whose snapshotFile.status is successful. clusterName is the value of
// spec.clusterName the objects carry: the provisioning Cluster's name for a
// provisioned cluster, and for an imported cluster the id of its management
// Cluster.
func NewestSuccessfulSnapshot(items []map[string]any, clusterName string) (map[string]any, bool) {
	var own []map[string]any
	for _, item := range items {
		if SnapshotOfCluster(item, clusterName) && nestedString(item, "snapshotFile", "status") == "successful" {
			own = append(own, item)
		}
	}
	if len(own) == 0 {
		return nil, false
	}
	SortSnapshotsNewestFirst(own)
	return own[0], true
}
