// upgradestrategy.go implements `cluster set-upgrade-strategy <cluster>`: the
// concurrency and drain settings Rancher upgrades the nodes of a provisioned
// or imported RKE2 or K3s cluster with, previewed by a server dry-run.
package provisioning

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// strategyGroup is one of the two groups of nodes the strategy covers, with the
// names of its flags and of its fields in both kinds of cluster.
type strategyGroup struct {
	// prefix starts every flag of the group; label is how a text names its nodes.
	prefix, label string
	// provConcurrency and provDrain are the fields of spec.rkeConfig.upgradeStrategy
	// of a provisioned cluster.
	provConcurrency, provDrain string
	// impConcurrency and impDrain are the fields of the upgrade strategy of an
	// imported cluster's config.
	impConcurrency, impDrain string
}

var strategyGroups = []strategyGroup{
	{prefix: "control-plane", label: "control plane", provConcurrency: "controlPlaneConcurrency", provDrain: "controlPlaneDrainOptions", impConcurrency: "serverConcurrency", impDrain: "drainServerNodes"},
	{prefix: "worker", label: "worker", provConcurrency: "workerConcurrency", provDrain: "workerDrainOptions", impConcurrency: "workerConcurrency", impDrain: "drainWorkerNodes"},
}

// Flag name endings of a group, after its prefix and a hyphen.
const (
	strategyConcurrency = "concurrency"
	strategyDrain       = "drain"
	strategyForce       = "drain-force"
	strategyEmptyDir    = "delete-emptydir"
	strategyGrace       = "grace-period"
	strategyTimeout     = "drain-timeout"
)

func (g strategyGroup) flag(ending string) string { return g.prefix + "-" + ending }

// drainDefaults are the values the Rancher UI gives the drain options when it
// turns drain on, in the order the command writes them. enabled is the typed
// flag and has no default.
var drainDefaults = []struct {
	field string
	value any
}{
	{"deleteEmptyDirData", true},
	{"disableEviction", false},
	{"force", false},
	{"gracePeriod", int64(-1)},
	{"ignoreDaemonSets", true},
	{"skipWaitForDeleteTimeoutSeconds", int64(0)},
	{"timeout", int64(120)},
}

// detailFlag is a typed drain option other than enabled.
type detailFlag struct {
	flag, field string
	value       any
}

// groupFlags are the typed flags of one group.
type groupFlags struct {
	group strategyGroup
	// concurrency is --<group>-concurrency as typed, "" when not typed.
	concurrency string
	// drainSet is true when --<group>-drain is typed, drain its value.
	drainSet, drain bool
	details         []detailFlag
}

func (f groupFlags) typed() bool {
	return f.concurrency != "" || f.drainSet || len(f.details) > 0
}

// detail returns the typed value of a drain option, and whether it is typed.
func (f groupFlags) detail(field string) (any, bool) {
	for _, d := range f.details {
		if d.field == field {
			return d.value, true
		}
	}
	return nil, false
}

type upgradeStrategyCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewUpgradeStrategy builds `cluster set-upgrade-strategy <cluster>`
// (mutating): sets the concurrency and drain options Rancher upgrades the
// nodes of a provisioned or imported RKE2 or K3s cluster with.
func NewUpgradeStrategy(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &upgradeStrategyCmd{clients: clients, roots: roots}
}

func (upgradeStrategyCmd) Describe() runtime.Descriptor {
	var flags []runtime.ArgSpec
	for _, g := range strategyGroups {
		flags = append(flags,
			runtime.ArgSpec{Name: g.flag(strategyConcurrency), Type: "string", Desc: "`number` of " + g.label + " nodes to upgrade at a time, or a percentage such as 10% (provisioned cluster only); an imported cluster takes a whole number of 1 or more"},
			runtime.ArgSpec{Name: g.flag(strategyDrain), Type: "bool", Desc: "drain " + g.label + " nodes before they are upgraded (--" + g.flag(strategyDrain) + "=false turns it off)"},
			runtime.ArgSpec{Name: g.flag(strategyForce), Type: "bool", Desc: "when draining " + g.label + " nodes, also delete standalone pods that no workload controller manages (Rancher field force; provisioned cluster only)"},
			runtime.ArgSpec{Name: g.flag(strategyEmptyDir), Type: "bool", Desc: "delete the data of pods that use an emptyDir volume when draining " + g.label + " nodes: kubectl --delete-emptydir-data, Rancher field deleteEmptyDirData (provisioned cluster only)"},
			runtime.ArgSpec{Name: g.flag(strategyGrace), Type: "string", Desc: "`seconds` each pod gets to stop when draining " + g.label + " nodes: 1 or more, or -1 for the pod's default (provisioned cluster only)"},
			runtime.ArgSpec{Name: g.flag(strategyTimeout), Type: "string", Desc: "longest `duration` a drain of " + g.label + " nodes may take: 1s or more in whole seconds, for example 90s or 5m (provisioned cluster only)"},
		)
	}
	flags = append(flags, clusterNamespaceFlag())
	return runtime.Descriptor{
		Name:  "cluster set-upgrade-strategy",
		Usage: "Set how Rancher upgrades the nodes of an RKE2 or K3s cluster",
		Desc: "Sets the upgrade strategy of a provisioned cluster (spec.rkeConfig.upgradeStrategy of its provisioning cluster) or of an " +
			"imported RKE2 or K3s cluster (spec.rke2Config.rke2upgradeStrategy or spec.k3sConfig.k3supgradeStrategy of its " +
			"management cluster): how many control plane and worker nodes Rancher upgrades at a time and whether it drains them first. " +
			"A provisioned cluster takes every flag. An imported cluster takes the two concurrency flags, whole numbers of 1 or more, " +
			"and --control-plane-drain and --worker-drain: the upgrade plan of Rancher's system-upgrade-controller has no other drain option. " +
			"When the command turns drain on for a provisioned cluster, it also writes the Rancher UI's value for each drain option " +
			"the cluster does not have yet (deleteEmptyDirData true, disableEviction false, force false, gracePeriod -1 (the pod's default), " +
			"ignoreDaemonSets true, skipWaitForDeleteTimeoutSeconds 0, timeout 120 seconds). A drain option the cluster already has keeps its value, " +
			"as in the Rancher UI; the preview warns when the command turns drain on and the cluster has gracePeriod 0 or deleteEmptyDirData false. " +
			"The command does not wait for an upgrade: the strategy applies to the next upgrade, for a provisioned cluster also to machine rollouts, " +
			"and to an upgrade that is running. A provisioned cluster needs " +
			"--yes with --apply; an imported cluster needs --apply only. The local cluster and hosted clusters are refused.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       flags,
		Examples: []string{
			"rancher cluster set-upgrade-strategy my-cluster --control-plane-concurrency 1 --worker-concurrency 10% --dry-run",
			"rancher cluster set-upgrade-strategy my-cluster --worker-drain --worker-grace-period 60 --apply --yes",
		},
		Destructive: "dependsOnInput",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

var _ runtime.MutatingCommand = (*upgradeStrategyCmd)(nil)

// parseGracePeriod reads a grace period flag: whole seconds, 1 or more, or -1
// for the pod's default.
func parseGracePeriod(flag, text string) (int64, error) {
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil || (seconds < 1 && seconds != -1) {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %q must be a whole number of seconds, 1 or more, or -1 for the pod's default", flag, text))
	}
	return seconds, nil
}

// parseDrainTimeout reads a drain timeout flag: a duration in whole seconds of
// 1s or more, returned in seconds. Rancher treats a timeout of 0 as 600
// seconds.
func parseDrainTimeout(flag, text string) (int64, error) {
	d, err := time.ParseDuration(text)
	if err != nil || d%time.Second != 0 || d < time.Second {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s %q must be a duration of 1s or more in whole seconds, for example 90s or 5m", flag, text))
	}
	return int64(d / time.Second), nil
}

// readStrategyFlags reads and checks every flag of the command, before any
// request.
func readStrategyFlags(in runtime.Input) ([]groupFlags, error) {
	var all []groupFlags
	typed := false
	for _, g := range strategyGroups {
		f := groupFlags{group: g, concurrency: in.String(g.flag(strategyConcurrency))}
		if in.IsSet(g.flag(strategyDrain)) {
			f.drainSet, f.drain = true, in.Bool(g.flag(strategyDrain))
		}
		if flag := g.flag(strategyForce); in.IsSet(flag) {
			f.details = append(f.details, detailFlag{flag, "force", in.Bool(flag)})
		}
		if flag := g.flag(strategyEmptyDir); in.IsSet(flag) {
			f.details = append(f.details, detailFlag{flag, "deleteEmptyDirData", in.Bool(flag)})
		}
		if flag := g.flag(strategyGrace); in.String(flag) != "" {
			seconds, err := parseGracePeriod(flag, in.String(flag))
			if err != nil {
				return nil, err
			}
			f.details = append(f.details, detailFlag{flag, "gracePeriod", seconds})
		}
		if flag := g.flag(strategyTimeout); in.String(flag) != "" {
			seconds, err := parseDrainTimeout(flag, in.String(flag))
			if err != nil {
				return nil, err
			}
			f.details = append(f.details, detailFlag{flag, "timeout", seconds})
		}
		typed = typed || f.typed()
		all = append(all, f)
	}
	if !typed {
		return nil, clierr.New(clierr.CodeValidation, "give at least one flag that sets the upgrade strategy, for example --worker-concurrency 1")
	}
	for _, f := range all {
		if f.drainSet && !f.drain && len(f.details) > 0 {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s conflicts with --%s=false; remove one", f.details[0].flag, f.group.flag(strategyDrain)))
		}
	}
	return all, nil
}

// checkImportedFlags refuses the flags an imported cluster cannot take: a drain
// option other than enabled, and a concurrency that is not a whole number of 1
// or more. It returns the concurrency values as integers.
func checkImportedFlags(all []groupFlags) (map[string]int64, error) {
	for _, f := range all {
		if len(f.details) > 0 {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s is for a provisioned cluster only; the upgrade plan of an imported cluster (Rancher's system-upgrade-controller) drains with force and has no other drain option; only --control-plane-drain and --worker-drain apply", f.details[0].flag))
		}
	}
	values := map[string]int64{}
	for _, f := range all {
		if f.concurrency == "" {
			continue
		}
		n, err := strconv.ParseUint(f.concurrency, 10, 63)
		if err != nil || n < 1 {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s %q must be a whole number of 1 or more for an imported cluster", f.group.flag(strategyConcurrency), f.concurrency))
		}
		values[f.group.prefix] = int64(n)
	}
	return values, nil
}

func (c *upgradeStrategyCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	flags, err := readStrategyFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, prov, ns, name, err := fetchClusterFamilyTarget(ctx, c.clients, in, "")
	if err != nil {
		if clierr.IsNotFound(err) {
			return safety.Effect{}, nil, noProvisioningClusterError(in.Args()[0], resolveClusterNamespace(in),
				"its upgrade strategy is in its management cluster, in spec.rke2Config.rke2upgradeStrategy or spec.k3sConfig.k3supgradeStrategy").WithCause(err)
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
	facts, cerr := clusterops.CheckStrategyCluster(prov.Object, mgmtObject)
	if cerr != nil {
		return safety.Effect{}, nil, cerr
	}

	var warnings []string
	var concurrency map[string]int64
	if facts.Provisioned {
		w, rerr := checkRestoreRunning(ctx, client, prov, ns, name)
		if rerr != nil {
			return safety.Effect{}, nil, rerr
		}
		warnings = append(warnings, w...)
	} else if concurrency, err = checkImportedFlags(flags); err != nil {
		return safety.Effect{}, nil, err
	}

	var (
		before, after *unstructured.Unstructured
		row           = kindRow("provisioningcluster")
		gvr           = provisioningClusterGVR
		objNS         = ns
		reversible    = true
	)
	if facts.Provisioned {
		before, after = prov, prov.DeepCopy()
		err = buildProvisionedStrategy(after, flags)
		warnings = append(warnings, "the strategy applies to the next upgrade and to machine rollouts")
	} else {
		row, gvr, objNS = kindRow("managementcluster"), kindRow("managementcluster").GVR, ""
		before, after = mgmt, mgmt.DeepCopy()
		err = buildImportedStrategy(after, facts, flags, concurrency)
		reversible = !turnsDrainOn(mgmt, facts, flags)
	}
	if err != nil {
		return safety.Effect{}, nil, err
	}

	diff := engine.DiffObjects(before.Object, after.Object)
	eff, exec, err := engine.PlanUpdate(ctx, client, gvr, objNS, after, row.Redaction(), engine.LocalCluster, row.Name, "Cluster", diff, row.Sensitive)
	if err != nil {
		return eff, exec, err
	}
	eff.Reversible = reversible
	eff.Warnings = append(eff.Warnings, warnings...)
	eff.Warnings = append(eff.Warnings, strategyWarnings(before, after, facts, flags, name, mgmt, setting, len(diff) == 0)...)
	return eff, strategyResult(exec, name), nil
}

// checkRestoreRunning refuses while Rancher runs an etcd snapshot restore that
// sets the upgrade strategy from the snapshot (restoreRKEConfig all), and warns
// when the RKEControlPlane cannot be read: the refusal then cannot be made.
func checkRestoreRunning(ctx context.Context, client engine.ClusterClient, prov *unstructured.Unstructured, ns, name string) ([]string, error) {
	cp, err := fetchControlPlane(ctx, client, ns, name)
	if err != nil {
		return []string{fmt.Sprintf("cannot read the RKEControlPlane of cluster %s (%s) to check for an etcd snapshot restore in progress; such a restore would replace the upgrade strategy this command writes", name, clierr.From(err).Code)}, nil
	}
	for _, op := range clusterops.RunningOperations(prov.Object, cp.Object) {
		if op.Kind == clusterops.OperationRestore && op.Request["restoreRKEConfig"] == "all" {
			return nil, restoreRefusal(name, op)
		}
	}
	return nil, nil
}

// restoreRefusal is the refusal for an etcd snapshot restore in progress that
// replaces the upgrade strategy: GENERIC and retryable, with no hint. It has
// its own text because the command lets every other operation through.
func restoreRefusal(clusterName string, op clusterops.Operation) *clierr.Error {
	head := fmt.Sprintf("cluster %s has an etcd snapshot restore in progress", clusterName)
	if state := operationState(op); state != "" {
		head += " (" + state + ")"
	}
	return clierr.New(clierr.CodeGeneric, head+"; it restores the cluster configuration (restoreRKEConfig all) and would replace the upgrade strategy this command writes; try again after it ends").WithRetryable(true)
}

// buildProvisionedStrategy sets the typed flags in spec.rkeConfig.upgradeStrategy
// of the provisioning Cluster. When a flag turns drain on for a group whose
// drain the cluster has not enabled, every drain option the cluster does not
// have and the flags do not type gets the Rancher UI's value.
func buildProvisionedStrategy(obj *unstructured.Unstructured, all []groupFlags) error {
	path := []string{"spec", "rkeConfig", "upgradeStrategy"}
	set := func(value any, rest ...string) error {
		return unstructured.SetNestedField(obj.Object, value, append(slices.Clone(path), rest...)...)
	}
	for _, f := range all {
		g := f.group
		if f.concurrency != "" {
			if err := set(f.concurrency, g.provConcurrency); err != nil {
				return err
			}
		}
		stored, _, _ := unstructured.NestedMap(obj.Object, append(slices.Clone(path), g.provDrain)...)
		storedEnabled, _ := stored["enabled"].(bool)
		if f.drainSet {
			if err := set(f.drain, g.provDrain, "enabled"); err != nil {
				return err
			}
		}
		if f.drainSet && f.drain && !storedEnabled {
			for _, d := range drainDefaults {
				if _, has := stored[d.field]; has {
					continue
				}
				if _, typed := f.detail(d.field); typed {
					continue
				}
				if err := set(d.value, g.provDrain, d.field); err != nil {
					return err
				}
			}
		}
		for _, d := range f.details {
			if err := set(d.value, g.provDrain, d.field); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildImportedStrategy sets the typed flags in the upgrade strategy of the
// management Cluster's config, and the version management annotation when the
// cluster has none.
func buildImportedStrategy(obj *unstructured.Unstructured, facts clusterops.StrategyCluster, all []groupFlags, concurrency map[string]int64) error {
	path := []string{"spec", facts.Distro + "Config", facts.Distro + "upgradeStrategy"}
	set := func(value any, field string) error {
		return unstructured.SetNestedField(obj.Object, value, append(slices.Clone(path), field)...)
	}
	stored, _, _ := unstructured.NestedMap(obj.Object, path...)
	for _, f := range all {
		// A number read through Steve decodes as a float64: a typed value equal
		// to the one the cluster already has leaves the field as read.
		if n, ok := concurrency[f.group.prefix]; ok {
			if current, isNumber := drainNumber(stored[f.group.impConcurrency]); !isNumber || current != n {
				if err := set(n, f.group.impConcurrency); err != nil {
					return err
				}
			}
		}
		// Rancher reads an absent drain field as false: turning drain off when
		// the field is absent writes nothing.
		if _, hasDrain := stored[f.group.impDrain]; f.drainSet && (f.drain || hasDrain) {
			if err := set(f.drain, f.group.impDrain); err != nil {
				return err
			}
		}
	}
	if facts.AddAnnotation {
		return unstructured.SetNestedField(obj.Object, "system-default", "metadata", "annotations", versionManagementAnnotation)
	}
	return nil
}

// importedStrategy is the upgrade strategy an imported cluster already has.
func importedStrategy(mgmt *unstructured.Unstructured, facts clusterops.StrategyCluster) map[string]any {
	strategy, _, _ := unstructured.NestedMap(mgmt.Object, "spec", facts.Distro+"Config", facts.Distro+"upgradeStrategy")
	return strategy
}

// turnsDrainOn reports whether a flag turns drain on for an imported cluster
// whose drain is not on. Rancher keeps the drain in its existing upgrade plan,
// and turning drain off again with this command does not remove it.
func turnsDrainOn(mgmt *unstructured.Unstructured, facts clusterops.StrategyCluster, all []groupFlags) bool {
	strategy := importedStrategy(mgmt, facts)
	for _, f := range all {
		if stored, _ := strategy[f.group.impDrain].(bool); f.drainSet && f.drain && !stored {
			return true
		}
	}
	return false
}

// strategyWarnings are the warnings of the preview that depend on the cluster
// and the flags; unchanged is true when the update changes nothing.
func strategyWarnings(before, after *unstructured.Unstructured, facts clusterops.StrategyCluster, all []groupFlags, name string, mgmt *unstructured.Unstructured, setting clusterops.SettingRead, unchanged bool) []string {
	var out []string
	if _, running := upgradeRunning(before, facts.Provisioned); running {
		out = append(out, "an upgrade is in progress; the new strategy applies to it at once")
	}
	if facts.Provisioned {
		strategy, _, _ := unstructured.NestedMap(after.Object, "spec", "rkeConfig", "upgradeStrategy")
		_, controlPlaneZero := concurrencyText(strategy["controlPlaneConcurrency"])
		_, workerZero := concurrencyText(strategy["workerConcurrency"])
		out = append(out, zeroWarnings(controlPlaneZero, workerZero)...)
		stored, _, _ := unstructured.NestedMap(before.Object, "spec", "rkeConfig", "upgradeStrategy")
		for _, f := range all {
			storedDrain, _ := stored[f.group.provDrain].(map[string]any)
			enabled, _ := storedDrain["enabled"].(bool)
			if f.drainSet && f.drain && !enabled {
				out = append(out, storedDrainWarnings(f, storedDrain, name)...)
			}
			if f.drainSet || enabled {
				continue
			}
			for _, d := range f.details {
				out = append(out, fmt.Sprintf("--%s applies once drain is on for the %s nodes of cluster %s", d.flag, f.group.label, name))
			}
		}
	} else {
		out = append(out, importedWarnings(mgmt, facts, all, name, setting)...)
	}
	if unchanged {
		out = append(out, fmt.Sprintf("cluster %s already has the values you typed; the command changes nothing", name))
	}
	return out
}

// storedDrainWarnings are the warnings for a group whose drain the command turns
// on while the cluster has gracePeriod 0 or deleteEmptyDirData false. The
// command keeps gracePeriod 0 and deleteEmptyDirData false that the cluster
// already has, as the Rancher UI does, unless a flag types them.
func storedDrainWarnings(f groupFlags, storedDrain map[string]any, name string) []string {
	var out []string
	if _, typed := f.detail("gracePeriod"); !typed {
		if n, isNumber := drainNumber(storedDrain["gracePeriod"]); isNumber && n == 0 {
			out = append(out, fmt.Sprintf("cluster %s has gracePeriod 0 for draining the %s nodes; a pod gets no time to stop (the command keeps it; --%s changes it)", name, f.group.label, f.group.flag(strategyGrace)))
		}
	}
	if _, typed := f.detail("deleteEmptyDirData"); !typed {
		if deleteData, isBool := storedDrain["deleteEmptyDirData"].(bool); isBool && !deleteData {
			out = append(out, fmt.Sprintf("cluster %s has deleteEmptyDirData false for draining the %s nodes; the drain fails when a pod uses an emptyDir volume (the command keeps it; --%s changes it)", name, f.group.label, f.group.flag(strategyEmptyDir)))
		}
	}
	return out
}

// importedWarnings are the warnings that apply to an imported cluster only.
func importedWarnings(mgmt *unstructured.Unstructured, facts clusterops.StrategyCluster, all []groupFlags, name string, setting clusterops.SettingRead) []string {
	var out []string
	switch clusterops.VersionManagementOf(mgmt.Object, setting) {
	case clusterops.VersionManagementDisabled:
		out = append(out, fmt.Sprintf("version management is off for cluster %s; the strategy applies once it is on", name))
	case clusterops.VersionManagementUnknown:
		out = append(out, fmt.Sprintf("cannot tell whether version management is on for cluster %s (the setting %s cannot be read: %s); the strategy applies once it is on", name, versionManagementSetting, setting.Code))
	}
	if facts.AddAnnotation {
		out = append(out, annotationWarning(name))
	}
	strategy := importedStrategy(mgmt, facts)
	var turnsOn, turnsOff bool
	for _, f := range all {
		stored, _ := strategy[f.group.impDrain].(bool)
		turnsOn = turnsOn || (f.drainSet && f.drain && !stored)
		turnsOff = turnsOff || (f.drainSet && !f.drain && stored)
	}
	if turnsOn {
		out = append(out, "turning drain on adds the drain to Rancher's upgrade plan; turning it off later does not remove it")
	}
	if turnsOff {
		out = append(out, "Rancher keeps the drain in its existing upgrade plan; this change does not remove it")
	}
	return out
}

// strategyResult reports the cluster as the command's result text, keeping the
// written object as the result data.
func strategyResult(exec runtime.ExecuteFunc, name string) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		out := runtime.MutationText("cluster", name, "upgrade strategy set")
		out.Kind, out.Data = res.Kind, res.Data
		return out, nil
	}
}
