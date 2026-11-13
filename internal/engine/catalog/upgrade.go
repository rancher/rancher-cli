package catalog

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The flags of app upgrade that app install does not take.
const (
	flagResetValues   = "reset-values"
	flagForce         = "force"
	flagTakeOwnership = "take-ownership"
	flagCleanupOnFail = "cleanup-on-fail"
	flagHistoryMax    = "history-max"
)

// maxHistoryMax is the largest --history-max. Each revision is a Secret in the
// release's namespace, and a larger number is taken as a typo.
const maxHistoryMax = 100

// defaultHistoryMax is the number of revisions Rancher keeps when the body
// sends none, and for a history limit of 0, which Helm reads as no limit.
const defaultHistoryMax = 5

const (
	warnUpgradeRisk = "an upgrade can delete resources and run hooks the preview cannot show; Rancher's catalog has no rollback"
	warnTakeOwner   = "Rancher also sends --server-side=true and --force-conflicts; Helm then takes over conflicting fields that another field manager owns"
)

// flagsNotCounted are the flags of app upgrade whose presence does not make
// a change: the wait, the cluster and the namespace say how and where, and
// the version, the repository and the values count only when they differ
// from what the release has. Every other flag typed makes a change.
var flagsNotCounted = []string{flagWait, flagTimeout, engine.FlagCluster, engine.FlagNamespace, flagVersion, flagRepo, flagValues}

// pullSecretPaths are the values that name image pull secrets, as Rancher
// reads them.
var pullSecretPaths = []valuePath{
	{"global", "cattle", "imagePullSecrets"},
	{"global", "imagePullSecrets"},
	{"imagePullSecrets"},
}

type upgradeCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	actions engine.ActionClientFactory
	src     engine.FileSource
	now     func() time.Time
	newID   func() string
	// interval is the spacing of the wait's reads.
	interval time.Duration
}

// NewUpgrade builds `app upgrade <release> --cluster <c> --namespace <ns>`:
// a change of an installed release through the upgrade action of its
// ClusterRepo, previewed from server reads.
func NewUpgrade(clients engine.ClusterClientFactory, roots engine.RootClientFactory, actions engine.ActionClientFactory, src engine.FileSource) runtime.Command {
	return &upgradeCmd{clients: clients, roots: roots, actions: actions, src: src, now: time.Now, newID: randomID, interval: defaultPollInterval}
}

func (*upgradeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "app upgrade",
		Usage: "Upgrade, downgrade or change the values of a Helm release that Rancher shows as an App",
		Desc: "Asks Rancher to upgrade the Helm release <release> in --namespace on --cluster through the upgrade action of a ClusterRepo: " +
			"Rancher fetches the chart version and runs Helm in a pod. A newer version, an older version (a downgrade) and a change of values are all upgrades; Rancher has no rollback action. " +
			"The server has no dry-run for this action; the preview comes from server reads (previewSource serverReads): " +
			"the App (it must exist, with the status deployed, failed or superseded), the ClusterRepo (it must be enabled), the chart and version in the index Rancher serves for the cluster, " +
			"the chart's default values and annotations, the Helm release Secret the App's owner reference names (it holds the release's values), and the operations of the namespace (none may still be running for the release). " +
			"It carries no resourceVersion precondition: --apply reads the App, the operations and the index entry again before it sends the request, and without --wait nothing checks for a change after that. " +
			"The chart is the App's chart; --chart is accepted only with that name, and another chart is refused: install it as a new release with `rancher app install`. " +
			"--repo defaults to the ClusterRepo named in the App's catalog.cattle.io/ui-source-repo annotation, and --version to the App's installed version. " +
			"The upgrade accepts the installed version when the index Rancher serves for the cluster no longer lists it and the ClusterRepo is the one the App came from. " +
			"The preview and the result then warn that its chart content is not compared between the preview and the request. " +
			"An older --version than the installed version is a downgrade, which the preview names. " +
			"Values: the release's values are the values the App's revision was installed or upgraded with. " +
			releaseReadHelp() +
			"It merges each --values file over them in order (- reads stdin once; null removes a key) and sends the result. " +
			"That read needs permission to get Secrets in the release's namespace. " +
			"Without it the command stops and names --reset-values. " +
			"--reset-values sends only the --values files and the values rancher-cli adds, and Helm takes every other value from the chart's defaults. " +
			"rancher-cli never asks Helm to reuse the release's values. " +
			"After a failed upgrade the release's values are the values that failed, and the preview warns. " +
			"The preview lists the path of every value that is added, removed or changed, never the values, except the values rancher-cli adds and the registry host, and counts the release's values that it sends unchanged. " +
			"As the Rancher UI does, rancher-cli adds the cluster values and the registry values the release's values lack; --system-default-registry <host> sets both registry values over the release's values, and --no-system-default-registry sends neither. " +
			"The Rancher UI sends only the difference from the chart's defaults; rancher-cli sends the release's values in full. " +
			"Helm treats both requests the same, and the values Helm keeps with the next revision then differ from the ones the Rancher UI would keep. " +
			"Refused: an App with the status uninstalled, uninstalling, pending-install, pending-upgrade, pending-rollback or unknown, an operation still running for the release, a chart that names another chart to install first (catalog.cattle.io/auto-install or auto-install-gvr), " +
			"a version whose catalog.cattle.io/namespace or release-name annotation does not match the release (the installed version is exempt), a requires-gvr or permits-os annotation the target does not meet, " +
			"--force with --take-ownership, and a request that changes nothing. " +
			"These flags make no change: --wait, --timeout, --cluster, --namespace, --dry-run, --apply and --yes. " +
			"--version and --repo make a change when they differ from the App, and --values when the merge changes a value. " +
			"Every other flag makes a change. " +
			"--helm-wait (default true; --helm-wait=false sends false) makes Helm wait for the chart's resources; " +
			"--helm-timeout (default 10m, at least 1s, at most 15m) is Helm's timeout; the chart's hooks can make a run longer, and Rancher removes a run's access one hour after the operation starts. " +
			"--history-max (1 to 100; Rancher uses 5 when it is omitted) is the maximum number of revisions Helm keeps for the release; Helm deletes the oldest before the run, never the last deployed one. " +
			"Helm does not roll back a failed upgrade: the resources it already changed stay changed, and --cleanup-on-fail deletes only the resources the upgrade created. " +
			"Without --wait the command returns once Rancher accepts the request, with the operation and the commands that follow it. " +
			"With --wait (or --timeout, default 55m, at most 55m) it returns when Helm's run has finished: with the release's revision and status, or with Helm's error, the release's state and the command to run next.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "release", Type: "string", Required: true, Desc: "Helm release name"},
		},
		Flags: append([]runtime.ArgSpec{
			engine.ClusterFlag(true, true),
			withDesc(engine.NamespaceFlag(true), "`namespace` of the release"),
			{Name: flagRepo, Type: "string", Desc: "ClusterRepo `name` to upgrade from (default: the ClusterRepo the app came from)"},
			{Name: flagChart, Type: "string", Desc: "the app's chart `name`; an upgrade cannot change the chart"},
			{Name: flagVersion, Type: "string", Desc: "exact chart `version` (default: the app's installed version)"},
			{Name: flagValues, Type: "stringSlice", Desc: "values `file`, repeatable, merged in order over the release's values; - reads stdin"},
			{Name: flagResetValues, Type: "bool", Desc: "do not send the release's values; send only the --values files and the values rancher-cli adds"},
			{Name: flagForce, Type: "bool", Desc: "Helm replaces the release's resources instead of patching them; not with --take-ownership"},
			{Name: flagTakeOwnership, Type: "bool", Desc: "Helm takes ownership of existing resources; Rancher also sends --server-side=true and --force-conflicts"},
			{Name: flagCleanupOnFail, Type: "bool", Desc: "Helm deletes the resources this upgrade created when it fails"},
			{Name: flagHistoryMax, Type: "string", Desc: "maximum number of revisions Helm keeps for the release, 1 to 100 (Rancher uses 5 when it is omitted)"},
			{Name: flagHelmWait, Type: "bool", Desc: "Helm waits for the chart's resources (default true; --helm-wait=false sends false)"},
			{Name: flagHelmTimeout, Type: "string", Desc: "Helm `timeout` for each step, 1s to 15m (default 10m)"},
			{Name: flagNoHooks, Type: "bool", Desc: "do not run the chart's hooks"},
			{Name: flagDisableOpenAPIValidation, Type: "bool", Desc: "do not validate the rendered resources against the OpenAPI schema"},
			{Name: flagDescription, Type: "string", Desc: "release `description`"},
			{Name: flagSystemDefaultRegistry, Type: "string", Desc: "registry `host` to send at values.global.cattle.systemDefaultRegistry and values.global.systemDefaultRegistry"},
			{Name: flagNoSystemDefaultRegistry, Type: "bool", Desc: "send neither registry value, also when the release's values or a values file set one"},
		}, waitFlags()...),
		Examples: []string{
			"rancher app upgrade my-nginx --cluster prod --namespace web --values change.yaml --dry-run",
			"rancher app upgrade my-nginx --cluster prod --namespace web --version 1.2.3 --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// appRecord is what Plan read of the App: the facts the checks and the
// request use.
type appRecord struct {
	obj             *unstructured.Unstructured
	name            string
	namespace       string
	revision        int64
	status          string
	chart           string
	version         string
	annotations     map[string]string
	resourceVersion string
}

// readAppRecord takes the facts of an App. The release is the App's
// spec.name and spec.namespace, which the request names; an App with
// neither is the App's own name and namespace.
func readAppRecord(app *unstructured.Unstructured) appRecord {
	r := appRecord{obj: app, resourceVersion: app.GetResourceVersion()}
	r.name, _, _ = unstructured.NestedString(app.Object, "spec", "name")
	if r.name == "" {
		r.name = app.GetName()
	}
	r.namespace, _, _ = unstructured.NestedString(app.Object, "spec", "namespace")
	if r.namespace == "" {
		r.namespace = app.GetNamespace()
	}
	r.revision, _, _ = unstructured.NestedInt64(app.Object, "spec", "version")
	r.status, _, _ = unstructured.NestedString(app.Object, "spec", "info", "status")
	r.chart, _, _ = unstructured.NestedString(app.Object, "spec", "chart", "metadata", "name")
	r.version, _, _ = unstructured.NestedString(app.Object, "spec", "chart", "metadata", "version")
	r.annotations, _, _ = unstructured.NestedStringMap(app.Object, "spec", "chart", "metadata", "annotations")
	return r
}

// sourceRepo is the ClusterRepo the App's chart came from: the source
// annotation when the source type is a ClusterRepo, else "".
func (r appRecord) sourceRepo() string {
	if r.annotations[annotationSourceRepoType] != sourceRepoTypeCluster {
		return ""
	}
	return r.annotations[annotationSourceRepo]
}

// upgradeFlags are the flags of app upgrade that need reading before any
// request.
type upgradeFlags struct {
	timeout    string
	wait       waitRequest
	reg        registryFlags
	historyMax int
	reset      bool
	values     Values
}

// readUpgradeFlags reads and checks the flags of app upgrade that need no
// server read.
func readUpgradeFlags(in runtime.Input, src engine.FileSource) (upgradeFlags, error) {
	var f upgradeFlags
	var err error
	if f.timeout, err = helmTimeout(in.String(flagHelmTimeout), defaultHelmTimeout); err != nil {
		return f, err
	}
	if f.wait, err = readWaitFlags(in); err != nil {
		return f, err
	}
	f.reg = registryFlags{host: in.String(flagSystemDefaultRegistry), none: in.Bool(flagNoSystemDefaultRegistry)}
	if f.reg.host != "" && f.reg.none {
		return f, clierr.New(clierr.CodeValidation,
			"--"+flagSystemDefaultRegistry+" and --"+flagNoSystemDefaultRegistry+" are given together; give at most one of them")
	}
	if in.Bool(flagForce) && in.Bool(flagTakeOwnership) {
		return f, clierr.New(clierr.CodeValidation, "Helm refuses --force-replace (sent for --"+flagForce+") together with --force-conflicts (sent for --"+flagTakeOwnership+"); give one of --"+flagForce+" and --"+flagTakeOwnership)
	}
	if f.historyMax, err = readHistoryMax(in); err != nil {
		return f, err
	}
	f.reset = in.Bool(flagResetValues)
	f.values, err = ReadValues(src, in.Strings(flagValues), stdinTerminal(in))
	return f, err
}

// readHistoryMax reads --history-max: 0 when omitted, else an integer from 1
// to maxHistoryMax.
func readHistoryMax(in runtime.Input) (int, error) {
	if !in.IsSet(flagHistoryMax) {
		return 0, nil
	}
	n, err := strconv.Atoi(in.String(flagHistoryMax))
	if err != nil || n < 1 || n > maxHistoryMax {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"--%s must be a whole number from 1 to %d; Rancher replaces 0 with %d, although Helm reads 0 as no limit", flagHistoryMax, maxHistoryMax, defaultHistoryMax))
	}
	return n, nil
}

// upgradePlan is what Plan read and decided, for Execute.
type upgradePlan struct {
	chartPlan
	app      appRecord
	digest   string
	noEntry  bool
	body     upgradeBody
	filter   Values
	sent     composition
	canRead  bool
	wait     waitRequest
	opTarget actionTarget
	cleanup  bool
}

func (c *upgradeCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	release := in.Args()[0]
	if err := validateReleaseName(release); err != nil {
		return safety.Effect{}, nil, err
	}
	f, err := readUpgradeFlags(in, c.src)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	t := actionTarget{typedCluster: in.String(engine.FlagCluster), namespace: in.String(engine.FlagNamespace), release: release, requestID: c.newID()}
	p := upgradePlan{target: t, wait: f.wait, cleanup: in.Bool(flagCleanupOnFail)}
	var warnings []string

	if p.cluster, err = engine.ResolveDownstreamCluster(ctx, c.clients, in, true); err != nil {
		return safety.Effect{}, nil, err
	}
	if p.client, err = c.clients.Cluster(ctx, p.cluster); err != nil {
		return safety.Effect{}, nil, err
	}
	appObj, err := p.client.Get(ctx, kind("app").GVR, t.namespace, release)
	if err != nil {
		if notFound(err) {
			return safety.Effect{}, nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("app %s not found in namespace %s",
				clierr.ShellQuote(release), clierr.ShellQuote(t.namespace))).
				WithHintCommand(clierr.NewCommand("get apps").FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace))
		}
		return safety.Effect{}, nil, err
	}
	p.app = readAppRecord(appObj)
	p.opTarget = t
	p.opTarget.release = p.app.name
	p.chart, p.current = p.app.chart, p.app.version
	p.values, p.files = f.values, f.values
	if err := c.checkStatus(ctx, p); err != nil {
		return safety.Effect{}, nil, err
	}
	p.repo = in.String(flagRepo)
	if want := in.String(flagChart); want != "" && want != p.app.chart {
		return safety.Effect{}, nil, p.otherChart(want)
	}

	source := p.app.sourceRepo()
	if p.repo == "" {
		p.repo = source
	}
	if p.repo == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"app %s does not say which ClusterRepo it came from; name one with --%s", clierr.ShellQuote(release), flagRepo)).
			WithHintCommand(clierr.NewCommand("get clusterrepos").FlagValue("--cluster", t.typedCluster))
	}
	repoObj, err := readClusterRepo(ctx, p.client, p.repo, t.typedCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if p.root, err = c.roots.Root(); err != nil {
		return safety.Effect{}, nil, err
	}
	if p.repo != source {
		warnings = append(warnings, fmt.Sprintf("the app will record ClusterRepo %s as the one it came from; a later uninstall of this app runs Helm as the service account in spec.serviceAccount of that ClusterRepo, when it sets one",
			clierr.ShellQuote(p.repo)))
	}

	entry, kept, w, err := p.resolveVersion(ctx, in.String(flagVersion))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings = append(warnings, w...)
	p.version, p.digest = entry.Version, entry.Digest
	p.noEntry = kept && entry.Version == ""
	if p.noEntry {
		p.version = p.app.version
	}
	info, err := p.readInfo(ctx)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := p.checkRegistryFlags(f.reg, info); err != nil {
		return safety.Effect{}, nil, err
	}
	mgmt, err := steve.GetCluster(ctx, steve.RootGetter(p.root), p.cluster)
	if err != nil && !forbidden(err) {
		return safety.Effect{}, nil, err
	}
	w, err = p.checkChart(ctx, entry, info, repoObj, mgmt, kept)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings = append(warnings, w...)

	stored, failure := readReleaseValues(ctx, p.root, p.cluster, releaseRef{
		target: t, name: p.app.name, namespace: p.app.namespace, revision: p.app.revision, secret: ownerSecret(appObj),
	})
	var storedWarnings []string
	switch {
	case failure != nil && (!f.reset || failure.hard):
		return safety.Effect{}, nil, failure.err
	case failure != nil:
		storedWarnings = append(storedWarnings, fmt.Sprintf("cannot read the release's values (%s); the preview cannot list the paths --%s removes", failure.reason, flagResetValues))
	case f.reset:
		storedWarnings = append(storedWarnings, fmt.Sprintf("the preview lists the paths of the values --%s removes, read from revision %d (status %s), Secret %s",
			flagResetValues, stored.revision, statusText(stored.status), clierr.ShellQuote(stored.secret)))
	default:
		storedWarnings = append(storedWarnings, fmt.Sprintf("the release's values come from revision %d (status %s), Secret %s",
			stored.revision, statusText(stored.status), clierr.ShellQuote(stored.secret)))
		if stored.status == "failed" {
			storedWarnings = append(storedWarnings, fmt.Sprintf("revision %d failed; this upgrade sends the values that failed, with your changes", stored.revision))
		}
	}

	if op, canList, err := runningOperation(ctx, p.client, t.namespace, p.app.name, c.now()); err != nil {
		return safety.Effect{}, nil, err
	} else if op != "" {
		return safety.Effect{}, nil, operationRunning(p.opTarget, op)
	} else if !canList {
		warnings = append(warnings, operationsNotChecked(p.opTarget))
	}

	idWarning, canRead, reviewed := identityOf(ctx, p.client, serviceAccountIdentity(repoObj), t.namespace)
	p.canRead = canRead
	if p.wait.on && !canRead {
		warnings = append(warnings, skipWarning(modeUpgrade, reviewed, t.namespace))
	}

	if failure == nil && !f.reset {
		p.values = stored.values.merged(f.values)
	}
	p.filter = f.values
	if failure == nil {
		p.filter = filterValues(stored.values, f.values)
	}
	if err := p.checkRegistryFlags(f.reg, info); err != nil {
		return safety.Effect{}, nil, err
	}
	if p.sent, err = p.composeValues(ctx, info.Values, mgmt, f.reg); err != nil {
		return safety.Effect{}, nil, err
	}
	entries, unchanged := p.valueEntries(stored.values, failure == nil)
	if unchanged > 0 {
		warnings = append(warnings, unchangedWarning(unchanged, f.reset))
	}
	warnings = append(warnings, p.sent.warnings...)

	if failure == nil && kept && p.repo == source && !countedFlagTyped(in, c.Describe()) &&
		sameValues(stored.values, p.sent.sent) {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"release %s already has version %s of chart %s installed with these values; nothing to change",
			clierr.ShellQuote(release), clierr.ShellQuote(p.version), clierr.ShellQuote(p.chart))).
			WithHintCommand(t.getApp())
	}

	p.body = upgradeBody{
		Timeout:                  f.timeout,
		Wait:                     !in.IsSet(flagHelmWait) || in.Bool(flagHelmWait),
		NoHooks:                  in.Bool(flagNoHooks),
		DisableOpenAPIValidation: in.Bool(flagDisableOpenAPIValidation),
		Force:                    in.Bool(flagForce),
		TakeOwnership:            in.Bool(flagTakeOwnership),
		HistoryMax:               f.historyMax,
		CleanupOnFail:            p.cleanup,
		Namespace:                p.app.namespace,
		Charts: []upgradeChart{{
			ChartName:   p.app.chart,
			Version:     p.version,
			ReleaseName: p.app.name,
			ResetValues: f.reset,
			Description: in.String(flagDescription),
			Values:      p.sent.sent.request(),
			Annotations: map[string]string{
				annotationRequestID:      t.requestID,
				annotationSourceRepoType: sourceRepoTypeCluster,
				annotationSourceRepo:     p.repo,
			},
		}},
	}

	warnings = append(warnings, p.optionWarnings(f, info)...)
	eff := safety.Effect{
		Action:            safety.ActionUpdate,
		Target:            kind("app").GVR.Resource + "/" + release,
		DisplayKind:       "app",
		DisplayName:       release,
		PreviewSource:     safety.PreviewServerReads,
		Cluster:           p.cluster,
		Diff:              append(p.diff(source), entries...),
		ConflictDetection: false,
		Reversible:        false,
		Destructive:       true,
		Warnings:          append(append([]string{idWarning, warnServerReads, warnUpgradeRisk}, storedWarnings...), warnings...),
	}
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsInstall)
	}
	exec := func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, p) }
	return eff, exec, nil
}

// checkStatus refuses an App Helm cannot upgrade: the status must be
// deployed, failed or superseded. A pending status with an operation still
// running is the running operation's refusal.
func (c *upgradeCmd) checkStatus(ctx context.Context, p upgradePlan) error {
	if pending(p.app.status) {
		op, _, err := runningOperation(ctx, p.client, p.target.namespace, p.app.name, c.now())
		if err != nil {
			return err
		}
		if op != "" {
			return operationRunning(p.opTarget, op)
		}
	}
	return refuseStatus(p.target, p.app)
}

// refuseStatus is the refusal for an App Helm cannot upgrade, nil for a
// status it can. A CONFLICT here is not retryable: its remedy says what to
// do first.
func refuseStatus(t actionTarget, app appRecord) error {
	release := clierr.ShellQuote(app.name)
	getApp := t.getApp()
	switch app.status {
	case "deployed", "failed", "superseded":
		return nil
	case "uninstalled":
		return clierr.New(clierr.CodeConflict, clierr.Text(fmt.Sprintf(
			"release %s is uninstalled and Helm keeps its history; Helm would install it again rather than upgrade it; "+
				"install it under another release name", release),
			clierr.Lead(" with "), installCommand(t.typedCluster, t.namespace, app.sourceRepo(), app.chart))).WithHintCommand(getApp)
	case "uninstalling":
		return clierr.New(clierr.CodeConflict, clierr.Text(fmt.Sprintf(
			"release %s is uninstalling and Helm has no deployed revision to upgrade", release),
			clierr.Lead("; read it with "), getApp,
			clierr.Lead("; to remove it, add --apply --yes after reading the preview of "), uninstallPreview(t))).WithHintCommand(getApp)
	case "pending-install", "pending-upgrade", "pending-rollback":
		return clierr.New(clierr.CodeConflict, clierr.Text(fmt.Sprintf(
			"Helm refuses an upgrade while release %s is %s", release, app.status),
			clierr.Lead("; to remove it, add --apply --yes after reading the preview of "), uninstallPreview(t))).WithHintCommand(getApp)
	}
	return clierr.New(clierr.CodeConflict, fmt.Sprintf("release %s has the status %s, and Helm upgrades only a deployed, failed or superseded release",
		release, statusText(app.status))).WithHintCommand(getApp)
}

// otherChart is the refusal of a --chart that is not the App's chart: an
// upgrade cannot change the chart the release has installed, and a different
// chart is an install under another release name.
func (p upgradePlan) otherChart(want string) error {
	t := p.target
	return clierr.New(clierr.CodeValidation, clierr.Text(fmt.Sprintf(
		"release %s has chart %s installed, and an upgrade cannot change the chart; to install chart %s as a new release",
		clierr.ShellQuote(t.release), clierr.ShellQuote(p.app.chart), clierr.ShellQuote(want)),
		clierr.Lead(", run "), installCommand(t.typedCluster, t.namespace, p.repo, want)))
}

// keptVersionWarning is the warning for the installed version when the
// filtered index no longer lists it: the upgrade stays on it with no index
// entry.
func keptVersionWarning(version string) string {
	return fmt.Sprintf("version %s is no longer listed in the index for this cluster; the upgrade stays on it, and its chart content is not compared between the preview and the request", version)
}

// resolveVersion finds the target version in the index Rancher serves for the
// cluster: an exact --version, else the installed version. kept is true for
// the installed version when the repository is the one the App came from;
// when the index does not list it the entry is empty and the warning says so.
// Any other version, and the installed version in another repository, must be
// listed.
func (p upgradePlan) resolveVersion(ctx context.Context, want string) (ChartVersion, bool, []string, error) {
	idx, err := ReadIndex(ctx, p.root, p.cluster, p.repo)
	if err != nil {
		return ChartVersion{}, false, nil, withRepoRemedy(err, p.target.typedCluster)
	}
	versions := idx.Entries[p.chart]
	current := p.app.version
	if want == "" {
		want = current
	}
	index := slices.IndexFunc(versions, func(v ChartVersion) bool { return v.Version == want })
	currentIndex := slices.IndexFunc(versions, func(v ChartVersion) bool { return v.Version == current })
	if want == current && p.repo == p.app.sourceRepo() {
		if index >= 0 {
			return versions[index], true, nil, nil
		}
		return ChartVersion{}, true, []string{keptVersionWarning(current)}, nil
	}
	if len(versions) == 0 {
		return ChartVersion{}, false, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"chart %s is not in the index ClusterRepo %s serves for this cluster", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo))).
			WithHintCommand(p.chartsHint())
	}
	if index < 0 {
		return ChartVersion{}, false, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"version %s of chart %s is not in the index ClusterRepo %s serves for this cluster; --version names one exact version; the index has %s",
			clierr.ShellQuote(want), clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo), versionList(versions))).
			WithHintCommand(p.chartsHint("--all-versions"))
	}
	var warnings []string
	switch {
	case currentIndex < 0:
		warnings = append(warnings, fmt.Sprintf(
			"the index does not list the installed version %s; whether %s is older was not checked; Helm runs any upgrade, and the chart may not support downgrades",
			current, want))
	case index > currentIndex:
		warnings = append(warnings, fmt.Sprintf(
			"%s is older than the installed %s: a downgrade; Helm runs it as any upgrade, and the chart may not support downgrades", want, current))
	}
	return versions[index], false, warnings, nil
}

// readInfo reads the info of the target version with the mapping an upgrade
// needs: a version the repository no longer has is a VALIDATION error, not a
// server error, and the answer must be for exactly the version asked for.
func (p upgradePlan) readInfo(ctx context.Context) (Info, error) {
	info, err := ReadInfo(ctx, p.root, p.cluster, p.repo, p.chart, p.version)
	if err != nil {
		return Info{}, p.infoError(err)
	}
	if info.Chart.Version != p.version {
		return Info{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"version %s of chart %s is no longer in ClusterRepo %s (Rancher returned version %s instead); name one with --version",
			clierr.ShellQuote(p.version), clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo), clierr.ShellQuote(info.Chart.Version))).
			WithHintCommand(p.chartsHint("--all-versions"))
	}
	return info, nil
}

// The texts of Helm's index lookup that Rancher returns as a 500 when the
// repository no longer has a chart or a version.
const (
	lookupNoChart    = "no chart name found"
	lookupNoVersions = "no chart version found"
	lookupNoVersion  = "no chart version found for "
)

// infoError maps the 500 Rancher answers when its Helm index lookup finds no
// chart or no version to a VALIDATION error naming what is missing; any other
// error keeps the status table's code.
func (p upgradePlan) infoError(err error) error {
	status, ok := clierr.HTTPStatus(err)
	if !ok || status != 500 {
		return withRepoRemedy(err, p.target.typedCluster)
	}
	msg := clierr.From(err).Message
	chart, repo := clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo)
	switch {
	case strings.Contains(msg, lookupNoChart):
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("chart %s is no longer in ClusterRepo %s", chart, repo)).WithHintCommand(p.chartsHint())
	case strings.Contains(msg, lookupNoVersion):
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("version %s of chart %s is no longer in ClusterRepo %s; name one with --version",
			clierr.ShellQuote(p.version), chart, repo)).WithHintCommand(p.chartsHint("--all-versions"))
	case strings.Contains(msg, lookupNoVersions):
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("chart %s has no versions in ClusterRepo %s", chart, repo)).WithHintCommand(p.chartsHint())
	}
	return err
}

// countedFlagTyped reports whether a flag that makes a change was typed:
// every flag of the command except flagsNotCounted.
func countedFlagTyped(in runtime.Input, d runtime.Descriptor) bool {
	for _, f := range d.Flags {
		if !slices.Contains(flagsNotCounted, f.Name) && in.IsSet(f.Name) {
			return true
		}
	}
	return false
}

// sameValues reports whether the values an upgrade would send are the
// release's values.
func sameValues(stored, sent Values) bool {
	sent = sent.withoutNullsTheReleaseLacks(stored)
	if len(stored.tree) == 0 && len(sent.tree) == 0 {
		return true
	}
	return reflect.DeepEqual(stored.tree, sent.tree)
}

// withoutNullsTheReleaseLacks returns a copy of v without the null leaves at
// a path the release's values do not hold: a null removes a key, and there is
// nothing to remove. A mapping left empty by that is dropped too.
func (v Values) withoutNullsTheReleaseLacks(stored Values) Values {
	var strip func(m map[string]any, path valuePath) map[string]any
	strip = func(m map[string]any, path valuePath) map[string]any {
		out := make(map[string]any, len(m))
		for k, x := range m {
			at := append(slices.Clone(path), k)
			switch t := x.(type) {
			case nil:
				if _, st, _ := lookup(stored.tree, at); st != pathPresent {
					continue
				}
			case map[string]any:
				sub := strip(t, at)
				if len(sub) == 0 && len(t) > 0 {
					continue
				}
				x = sub
			}
			out[k] = x
		}
		return out
	}
	return Values{tree: strip(v.tree, nil)}
}

// merged returns a copy of v with the files' values merged over it in order:
// maps merge key by key, any other value replaces, and null stays as a value
// to send.
func (v Values) merged(files Values) Values {
	tree, _ := deepCopy(v.tree).(map[string]any)
	if tree == nil {
		tree = map[string]any{}
	}
	mergeValues(tree, files.tree)
	return Values{tree: tree}
}

// filterValues holds the release's values and the files' values side by side,
// for the server-text filter alone: a value of the release a file replaces is
// still one the server's text can hold.
func filterValues(stored, files Values) Values {
	return Values{tree: map[string]any{"stored": stored.tree, "files": files.tree}}
}

// diff lists what the request changes: the source, the chart version, the
// release and revision, the Helm options and, after it, the value entries.
func (p upgradePlan) diff(source string) []safety.DiffEntry {
	d := []safety.DiffEntry{fromTo("repo", source, p.repo), {Path: "chart", To: p.chart}, fromTo("version", p.app.version, p.version),
		{Path: "namespace", To: p.app.namespace}, {Path: "releaseName", To: p.app.name},
		{Path: "revision", From: p.app.revision, To: p.app.revision + 1},
		{Path: "wait", To: p.body.Wait}, {Path: "timeout", To: p.body.Timeout}}
	chart := p.body.Charts[0]
	for _, o := range []struct {
		path string
		on   bool
	}{
		{"resetValues", chart.ResetValues}, {"noHooks", p.body.NoHooks}, {"disableOpenAPIValidation", p.body.DisableOpenAPIValidation},
		{"force", p.body.Force}, {"takeOwnership", p.body.TakeOwnership}, {"cleanupOnFail", p.body.CleanupOnFail},
	} {
		if o.on {
			d = append(d, safety.DiffEntry{Path: o.path, To: true})
		}
	}
	if p.body.HistoryMax > 0 {
		d = append(d, safety.DiffEntry{Path: "historyMax", To: p.body.HistoryMax})
	}
	if chart.Description != "" {
		d = append(d, safety.DiffEntry{Path: "description", To: chart.Description})
	}
	return d
}

// fromTo is the entry of a value that changes (both sides) or stays (the
// value alone).
func fromTo(path, from, to string) safety.DiffEntry {
	if from == "" || from == to {
		return safety.DiffEntry{Path: path, To: to}
	}
	return safety.DiffEntry{Path: path, From: from, To: to}
}

// valueEntries compares the release's values with the values the request
// sends, leaf by leaf: a leaf only in the sent values is added, one only in
// the release's values is removed, and one in both with another value is
// changed. Equal leaves are counted, not listed. A value shows its text only
// when the CLI added it or it is a string at a registry path of a chart that
// declares one; every other value is hidden. read is false when the
// release's values could not be read: nothing is then listed as removed or
// changed.
func (p upgradePlan) valueEntries(stored Values, read bool) (entries []safety.DiffEntry, unchanged int) {
	was, now := map[string]any{}, map[string]any{}
	if read {
		stored.leaves(func(path string, v any) { was[path] = v })
	}
	p.sent.sent.leaves(func(path string, v any) { now[path] = v })
	for path, v := range now {
		if _, inOld := was[path]; v == nil && !inOld {
			delete(now, path)
		}
	}
	added := map[string]bool{}
	for _, a := range p.sent.added {
		added[a] = true
	}
	registry := map[string]bool{}
	for _, r := range p.sent.shown {
		registry[r.String()] = true
	}
	showSent := func(path string, v any) any {
		if _, isString := v.(string); added[path] || (registry[path] && isString) {
			return v
		}
		return hiddenText
	}
	showStored := func(path string, v any) any {
		if _, isString := v.(string); registry[path] && isString {
			return v
		}
		return hiddenText
	}
	paths := make(map[string]bool, len(was)+len(now))
	for path := range was {
		paths[path] = true
	}
	for path := range now {
		paths[path] = true
	}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		oldV, inOld := was[path]
		newV, inNew := now[path]
		switch {
		case inOld && inNew && newV == nil && oldV != nil:
			entries = append(entries, safety.DiffEntry{Path: path, From: showStored(path, oldV)})
		case inOld && inNew && reflect.DeepEqual(oldV, newV):
			unchanged++
		case inNew && !inOld:
			entries = append(entries, safety.DiffEntry{Path: path, To: showSent(path, newV)})
		case inOld && !inNew:
			entries = append(entries, safety.DiffEntry{Path: path, From: showStored(path, oldV)})
		default:
			entries = append(entries, safety.DiffEntry{Path: path, From: showStored(path, oldV), To: showSent(path, newV)})
		}
	}
	return entries, unchanged
}

// unchangedWarning counts the values the request sends as the release has
// them.
func unchangedWarning(n int, reset bool) string {
	switch {
	case reset && n == 1:
		return "1 value in the request is the same as the release's value"
	case reset:
		return fmt.Sprintf("%d values in the request are the same as the release's values", n)
	case n == 1:
		return "1 of the release's values is sent unchanged"
	}
	return fmt.Sprintf("%d of the release's values are sent unchanged", n)
}

// optionWarnings are the warnings of the options typed and of the History
// Helm keeps.
func (p upgradePlan) optionWarnings(f upgradeFlags, info Info) []string {
	var w []string
	if p.body.TakeOwnership {
		w = append(w, warnTakeOwner)
	}
	revision := p.app.revision
	switch {
	case f.historyMax > 0 && revision >= int64(f.historyMax):
		w = append(w, fmt.Sprintf("--%[1]s %[2]d: when the new revision would take the release over %[2]d revisions, Helm deletes the oldest before it runs, never the last deployed one",
			flagHistoryMax, f.historyMax))
	case f.historyMax == 0 && revision >= defaultHistoryMax:
		w = append(w, fmt.Sprintf("--%[1]s is not given and Rancher uses %[2]d: when the new revision would take the release over %[2]d revisions, Helm deletes the oldest before it runs, never the last deployed one; give --%[1]s to keep more",
			flagHistoryMax, defaultHistoryMax))
	}
	if f.reg.none && declaresPullSecrets(info.Values) {
		w = append(w, "--"+flagNoSystemDefaultRegistry+" sends no registry value; Rancher then deletes the image pull secrets it manages for this release")
	}
	return w
}

// declaresPullSecrets reports whether a chart's default values declare a
// path that names image pull secrets.
func declaresPullSecrets(defaults map[string]any) bool {
	for _, path := range pullSecretPaths {
		if _, st, _ := lookup(defaults, path); st == pathPresent {
			return true
		}
	}
	return false
}

// execute reads the App, the operations and the index entry again, then sends
// the request once.
func (c *upgradeCmd) execute(ctx context.Context, p upgradePlan) (runtime.Result, error) {
	t := p.target
	again, err := p.client.Get(ctx, kind("app").GVR, t.namespace, t.release)
	if err != nil {
		if notFound(err) {
			return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was removed")
		}
		return runtime.Result{}, err
	}
	if again.GetResourceVersion() != p.app.resourceVersion {
		return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " changed")
	}
	if err := recheckOperations(ctx, p.client, p.opTarget, c.now()); err != nil {
		return runtime.Result{}, err
	}
	var warnings []string
	if p.noEntry {
		warnings = append(warnings, keptVersionWarning(p.version))
	} else if warnings, err = p.recheckIndex(ctx, p.digest); err != nil {
		return runtime.Result{}, err
	}
	a, err := postAction(ctx, c.actions, upgradePath(p.cluster, p.repo), p.body, t, p.filter)
	if err != nil {
		return runtime.Result{}, err
	}
	if !p.wait.on {
		res := actionResult("upgrade", t, a, p.canRead)
		res.Warnings = append(warnings, res.Warnings...)
		return res, nil
	}
	w := &appWait{client: p.client, interval: c.interval, timeout: p.wait.timeout, t: t, op: a,
		filter: p.filter.filterFunc(), mode: modeUpgrade, recorded: p.app.revision, cleanupOnFail: p.cleanup, skip: !p.canRead}
	end, err := w.run(ctx)
	if err != nil {
		return runtime.Result{}, err
	}
	res := waitedResult("upgrade", t, a, end)
	res.Warnings = append(warnings, res.Warnings...)
	return res, nil
}
