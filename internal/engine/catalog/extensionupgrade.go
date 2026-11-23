package catalog

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type extensionUpgradeCmd struct {
	clients  engine.ClusterClientFactory
	roots    engine.RootClientFactory
	actions  engine.ActionClientFactory
	now      func() time.Time
	newID    func() string
	interval time.Duration
}

// NewExtensionUpgrade builds `extension upgrade <chart> [--repo <repo>]
// [--version <v>]`: the upgrade of an installed Rancher UI extension through the
// upgrade action of the ClusterRepo it came from, as the Rancher UI's
// Extensions page does, previewed from server reads.
func NewExtensionUpgrade(clients engine.ClusterClientFactory, roots engine.RootClientFactory, actions engine.ActionClientFactory) runtime.Command {
	return &extensionUpgradeCmd{clients: clients, roots: roots, actions: actions, now: time.Now, newID: randomID, interval: defaultPollInterval}
}

func (*extensionUpgradeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "extension upgrade",
		Usage: "Upgrade an installed Rancher UI extension",
		Desc: "Asks Rancher to upgrade the installed extension <chart> as the Rancher UI's Extensions page does: " +
			"through the upgrade action of the ClusterRepo the App came from, for the Helm release <chart> in the namespace cattle-ui-plugin-system of the local cluster, " +
			"with the request body the Rancher UI sends, plus three annotations (a request ID and the type and name of the source ClusterRepo), no Helm options, and no values except the system default registry, which is sent when the setting system-default-registry has a value and the chart's default image.repository starts with rancher/. " +
			"The server has no dry-run for this action; the preview comes from server reads (previewSource serverReads): " +
			"the App, the ClusterRepo and its index, the chart's versions with the checks of the Rancher UI " +
			"and the operations of the namespace (none may still be running for the release). " +
			"It carries no resourceVersion precondition on the request itself: --apply reads the App, the operations and the index entry again before it sends the request, " +
			"and refuses when the App changed, and without --wait nothing checks for a change after that. " +
			"--repo, when typed, must be the ClusterRepo the App came from. " +
			"--version names one exact version from the index, an older one included (a downgrade, which Helm runs as any upgrade); " +
			"without it the version is the first one in the index that passes the checks of the Rancher UI. " +
			"Refused: a version that fails a check, the version that is installed, a chart that installs several extensions together, " +
			"an extension that is not installed or was loaded from a URL, an App whose status is other than deployed, failed or superseded, " +
			"and a server where the feature uiextension is turned off or the namespace does not exist. " +
			"Without --wait the command returns once Rancher accepts the request, with the operation and the commands that follow it. " +
			"With --wait (or --timeout, default 55m, at most 55m) it returns when Helm's run has finished: with the release's revision and status, " +
			"or with Helm's error, the release's state and the command to run next. " +
			"It does not wait for the Rancher UI to load the extension; `rancher extension list` shows the UIPlugin's state.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "chart", Type: "string", Required: true, Desc: "name of the extension chart, which is also its release name"},
		},
		Flags: append([]runtime.ArgSpec{
			{Name: flagRepo, Type: "string", Desc: "ClusterRepo `name` the extension came from (default: the ClusterRepo named in the App; another one is refused)"},
			{Name: flagVersion, Type: "string", Desc: "exact chart `version` (default: the first version that passes the checks of the Rancher UI)"},
		}, waitFlags()...),
		Examples: []string{
			"rancher extension upgrade epinio --dry-run",
			"rancher extension upgrade epinio --version 1.14.3 --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// extensionUpgrade is what Plan read and decided, for Execute.
type extensionUpgrade struct {
	client          engine.ClusterClient
	root            engine.RootClient
	target          actionTarget
	repo            string
	version         string
	digest          string
	body            extensionActionBody
	canRead         bool
	wait            waitRequest
	resourceVersion string
	revision        int64
}

func (c *extensionUpgradeCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	chart := in.Args()[0]
	if err := validateReleaseName(chart); err != nil {
		return safety.Effect{}, nil, err
	}
	typedRepo := in.String(flagRepo)
	if typedRepo != "" {
		if err := validateRepoName(typedRepo); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	wait, err := readWaitFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := checkExtensionsAvailable(ctx, client); err != nil {
		return safety.Effect{}, nil, err
	}
	root, err := c.roots.Root()
	if err != nil {
		return safety.Effect{}, nil, err
	}
	t := extensionTarget(chart, c.newID())
	appObj, err := client.Get(ctx, kind("app").GVR, t.namespace, chart)
	if err != nil {
		if notFound(err) {
			return safety.Effect{}, nil, noExtensionApp(ctx, client, t, "this command does not upgrade it",
				clierr.NewCommand("extension install").Arg(chart).Flag("--dry-run"))
		}
		return safety.Effect{}, nil, err
	}
	app := readAppRecord(appObj)
	q := clierr.ShellQuote(chart)
	switch app.status {
	case "deployed", "failed", "superseded":
	default:
		return safety.Effect{}, nil, clierr.New(clierr.CodeConflict, fmt.Sprintf(
			"App %s in namespace %s has the status %s, and Helm upgrades only a deployed, failed or superseded release",
			q, t.namespace, statusText(app.status))).WithHintCommand(t.getApp())
	}
	appRepo := extensionAppRepo(appObj)
	if appRepo == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"the App of extension %s does not name the ClusterRepo it came from, and the Rancher UI does not upgrade it either", q)).
			WithHintCommand(extensionListHint())
	}
	if typedRepo != "" && typedRepo != appRepo {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"extension %s came from ClusterRepo %s; an extension is upgraded from the ClusterRepo it came from", q, clierr.ShellQuote(appRepo))).
			WithHintCommand(clierr.NewCommand("extension upgrade").Arg(chart).FlagValue("--"+flagRepo, appRepo).Flag("--dry-run"))
	}
	ec, err := readExtensionContext(ctx, c.clients, root)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	repos, err := readRepoRows(ctx, root)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	choice, err := namedExtensionRepo(ctx, root, repos, chart, appRepo)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := refuseBundle(choice.versions); err != nil {
		return safety.Effect{}, nil, err
	}
	chosen, err := chooseExtensionVersion(ec, chart, choice.repo.Name, choice.versions, in.String(flagVersion))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if chosen.entry.Version == app.version {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"version %s of extension %s is already installed", clierr.ShellQuote(chosen.entry.Version), q)).WithHintCommand(extensionListHint())
	}
	warnings := slices.Clone(chosen.warnings)
	installed := slices.IndexFunc(choice.versions, func(v ChartVersion) bool { return v.Version == app.version })
	switch {
	case installed < 0:
		warnings = append(warnings, fmt.Sprintf(
			"the index does not list the installed version %s; whether %s is older was not checked; Helm runs any upgrade, and the chart may not support downgrades",
			app.version, chosen.entry.Version))
	case chosen.index > installed:
		warnings = append(warnings, fmt.Sprintf(
			"%s is older than the installed %s: a downgrade; Helm runs it as any upgrade, and the chart may not support downgrades", chosen.entry.Version, app.version))
	}

	op, canList, err := runningOperation(ctx, client, t.namespace, chart, c.now())
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if op != "" {
		return safety.Effect{}, nil, operationRunning(t, op)
	}
	if !canList {
		warnings = append(warnings, operationsNotChecked(t))
	}
	idWarning, canRead, reviewed := identityOf(ctx, client, repoIdentity(choice.repo), t.namespace)
	if wait.on && !canRead {
		warnings = append(warnings, skipWarning(modeUpgrade, reviewed, t.namespace))
	}
	registry, err := registryHost(ctx, root, choice.repo.Name, chart, chosen.entry.Version)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	p := extensionUpgrade{
		client: client, root: root, target: t, repo: choice.repo.Name, version: chosen.entry.Version, digest: chosen.entry.Digest,
		body: newExtensionBody(t, choice.repo.Name, chosen.entry.Version, registry), canRead: canRead, wait: wait,
		resourceVersion: app.resourceVersion, revision: app.revision,
	}
	diff := []safety.DiffEntry{
		{Path: "repo", To: p.repo},
		{Path: "chart", To: chart},
		fromTo("version", app.version, p.version),
		{Path: "namespace", To: t.namespace},
		{Path: "releaseName", To: chart},
		{Path: "revision", From: app.revision, To: app.revision + 1},
	}
	if registry != "" {
		diff = append(diff, safety.DiffEntry{Path: registryValuePath, To: registry})
	}
	eff := upgradeEffect(engine.LocalCluster, chart, diff, append([]string{idWarning, warnServerReads, warnUpgradeRisk}, warnings...))
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsInstall)
		p.target.pullSecretNote = notePullSecretsInstall
	}
	exec := func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, p) }
	return eff, exec, nil
}

// noExtensionApp is the refusal of a command that acts on an installed
// extension when the App does not exist: an extension loaded from a URL (a
// UIPlugin of that name) or no extension at all. urlClause ends the first
// text, and hint is the command the second one points to.
func noExtensionApp(ctx context.Context, client engine.ClusterClient, t actionTarget, urlClause string, hint clierr.Command) error {
	plugins, err := listUIPlugins(ctx, client)
	if err != nil {
		return err
	}
	q := clierr.ShellQuote(t.release)
	for _, p := range plugins {
		if p.PluginName == t.release {
			return clierr.New(clierr.CodeNotFound, fmt.Sprintf(
				"extension %s has no App in namespace %s; it was loaded from a URL (UIPlugin %s), and %s",
				q, t.namespace, clierr.ShellQuote(p.Namespace+"/"+p.Name), urlClause))
		}
	}
	return clierr.New(clierr.CodeNotFound, fmt.Sprintf("extension %s is not installed: no App %s in namespace %s", q, q, t.namespace)).
		WithHintCommand(hint)
}

// execute reads the App, the operations and the index entry again, then sends
// the request once.
func (c *extensionUpgradeCmd) execute(ctx context.Context, p extensionUpgrade) (runtime.Result, error) {
	t := p.target
	again, err := p.client.Get(ctx, kind("app").GVR, t.namespace, t.release)
	if err != nil {
		if notFound(err) {
			return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was deleted")
		}
		return runtime.Result{}, err
	}
	if again.GetResourceVersion() != p.resourceVersion {
		return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " changed")
	}
	if err := recheckOperations(ctx, p.client, t, c.now()); err != nil {
		return runtime.Result{}, err
	}
	index := chartPlan{root: p.root, cluster: engine.LocalCluster, repo: p.repo, chart: t.release, version: p.version}
	warnings, err := index.recheckIndex(ctx, p.digest)
	if err != nil {
		return runtime.Result{}, err
	}
	a, err := postAction(ctx, c.actions, upgradePath(engine.LocalCluster, p.repo), p.body, t, Values{})
	if err != nil {
		return runtime.Result{}, err
	}
	out := extensionOutcome{client: p.client, interval: c.interval, wait: p.wait, canRead: p.canRead, target: t, mode: modeUpgrade, recorded: p.revision}
	return out.report(ctx, a, warnings)
}

// LiveSource hands the surface the source of the chart name, --repo and
// --version. Every other site is declined.
func (c *extensionUpgradeCmd) LiveSource() completion.Source {
	return catalogcomplete.ExtensionUpgradeSource(c.Describe().Name)
}
