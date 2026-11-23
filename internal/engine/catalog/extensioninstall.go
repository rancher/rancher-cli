package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type extensionInstallCmd struct {
	clients  engine.ClusterClientFactory
	roots    engine.RootClientFactory
	actions  engine.ActionClientFactory
	now      func() time.Time
	newID    func() string
	interval time.Duration
}

// NewExtensionInstall builds `extension install <chart> [--repo <repo>]
// [--version <v>]`: the install of a Rancher UI extension through the install
// action of the ClusterRepo that offers it, as the Rancher UI's Extensions page
// does, previewed from server reads.
func NewExtensionInstall(clients engine.ClusterClientFactory, roots engine.RootClientFactory, actions engine.ActionClientFactory) runtime.Command {
	return &extensionInstallCmd{clients: clients, roots: roots, actions: actions, now: time.Now, newID: randomID, interval: defaultPollInterval}
}

func (*extensionInstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "extension install",
		Usage: "Install a Rancher UI extension from a ClusterRepo",
		Desc: "Asks Rancher to install the extension <chart> as the Rancher UI's Extensions page does: " +
			"through the install action of the ClusterRepo that offers it, as the Helm release <chart> in the namespace cattle-ui-plugin-system of the local cluster, " +
			"with the request body the Rancher UI sends, plus three annotations (a request ID and the type and name of the source ClusterRepo), no Helm options, and no values except the system default registry, which is sent when the setting system-default-registry has a value and the chart's default image.repository starts with rancher/. " +
			"The server has no dry-run for this action; the preview comes from server reads (previewSource serverReads): " +
			"the ClusterRepos the Rancher UI reads and their indexes, the chart's versions with the checks of the Rancher UI, " +
			"the UIPlugins, the App (it must not exist) and the operations of the namespace (none may still be running for the release). " +
			"It carries no resourceVersion precondition: --apply reads the App, the operations and the index entry again before it sends the request, " +
			"and without --wait nothing checks for a change after that. " +
			"Without --repo exactly one ClusterRepo the Rancher UI reads must offer the chart. " +
			"--version names one exact version from the index; without it the version is the first one in the index that passes the checks of the Rancher UI. " +
			"Refused: a version that fails a check, a chart that installs several extensions together, an extension that is already installed or loaded from a URL, " +
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
			{Name: flagRepo, Type: "string", Desc: "ClusterRepo `name` that offers the chart (default: the only ClusterRepo the Rancher UI reads that offers it)"},
			{Name: flagVersion, Type: "string", Desc: "exact chart `version` (default: the first version that passes the checks of the Rancher UI)"},
		}, waitFlags()...),
		Examples: []string{
			"rancher extension install epinio --dry-run",
			"rancher extension install epinio --repo partner-extensions --version 1.14.3 --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// extensionInstall is what Plan read and decided, for Execute.
type extensionInstall struct {
	client  engine.ClusterClient
	root    engine.RootClient
	target  actionTarget
	repo    string
	version string
	digest  string
	body    extensionActionBody
	canRead bool
	wait    waitRequest
}

func (c *extensionInstallCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
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
	ec, err := readExtensionContext(ctx, c.clients, root)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	repos, err := readRepoRows(ctx, root)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	choice, err := chooseExtensionRepo(ctx, root, repos, chart, typedRepo)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if err := refuseBundle(choice.versions); err != nil {
		return safety.Effect{}, nil, err
	}
	t := extensionTarget(chart, c.newID())
	if err := checkNotInstalled(ctx, client, t); err != nil {
		return safety.Effect{}, nil, err
	}
	chosen, err := chooseExtensionVersion(ec, chart, choice.repo.Name, choice.versions, in.String(flagVersion))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings := append(choice.warnings, chosen.warnings...)

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
		warnings = append(warnings, skipWarning(modeInstall, reviewed, t.namespace))
	}
	registry, err := registryHost(ctx, root, choice.repo.Name, chart, chosen.entry.Version)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	p := extensionInstall{
		client: client, root: root, target: t, repo: choice.repo.Name, version: chosen.entry.Version, digest: chosen.entry.Digest,
		body: newExtensionBody(t, choice.repo.Name, chosen.entry.Version, registry), canRead: canRead, wait: wait,
	}
	diff := []safety.DiffEntry{
		{Path: "repo", To: p.repo},
		{Path: "chart", To: chart},
		{Path: "version", To: p.version},
		{Path: "namespace", To: t.namespace},
		{Path: "releaseName", To: chart},
	}
	if registry != "" {
		diff = append(diff, safety.DiffEntry{Path: registryValuePath, To: registry})
	}
	eff := installEffect(engine.LocalCluster, chart, diff, append([]string{idWarning, warnServerReads}, warnings...))
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsInstall)
		p.target.pullSecretNote = notePullSecretsInstall
	}
	exec := func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, p) }
	return eff, exec, nil
}

// checkNotInstalled refuses an install over an App of the chart's name, and
// over a UIPlugin of that name that has no App: the Rancher UI shows both as
// installed or loaded.
func checkNotInstalled(ctx context.Context, client engine.ClusterClient, t actionTarget) error {
	plugins, err := listUIPlugins(ctx, client)
	if err != nil {
		return err
	}
	app, err := client.Get(ctx, kind("app").GVR, t.namespace, t.release)
	switch {
	case err == nil:
		status, _, _ := unstructured.NestedString(app.Object, "spec", "info", "status")
		chart := clierr.ShellQuote(t.release)
		switch status {
		case "deployed", "failed", "superseded":
			return clierr.New(clierr.CodeConflict, fmt.Sprintf(
				"extension %s is already installed: App %s in namespace %s (status %s)", chart, chart, t.namespace, status)).
				WithHintCommand(clierr.NewCommand("extension upgrade").Arg(t.release).Flag("--dry-run"))
		}
		return clierr.New(clierr.CodeConflict, fmt.Sprintf(
			"App %s in namespace %s has the status %s, and Helm does not install a release of that name over an App with that status", chart, t.namespace, statusText(status))).
			WithHintCommand(t.getApp())
	case !notFound(err):
		return err
	}
	for _, p := range plugins {
		if p.PluginName == t.release {
			return clierr.New(clierr.CodeConflict, fmt.Sprintf(
				"an extension named %s is already loaded (UIPlugin %s); an extension loaded from a URL is uninstalled in the Rancher UI",
				clierr.ShellQuote(t.release), clierr.ShellQuote(p.Namespace+"/"+p.Name)))
		}
	}
	return nil
}

// execute reads the App, the operations and the index entry again, then sends
// the request once.
func (c *extensionInstallCmd) execute(ctx context.Context, p extensionInstall) (runtime.Result, error) {
	t := p.target
	if _, err := p.client.Get(ctx, kind("app").GVR, t.namespace, t.release); err == nil {
		return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was created")
	} else if !notFound(err) {
		return runtime.Result{}, err
	}
	if err := recheckOperations(ctx, p.client, t, c.now()); err != nil {
		return runtime.Result{}, err
	}
	index := chartPlan{root: p.root, cluster: engine.LocalCluster, repo: p.repo, chart: t.release, version: p.version}
	warnings, err := index.recheckIndex(ctx, p.digest)
	if err != nil {
		return runtime.Result{}, err
	}
	a, err := postAction(ctx, c.actions, installPath(engine.LocalCluster, p.repo), p.body, t, Values{})
	if err != nil {
		return runtime.Result{}, err
	}
	out := extensionOutcome{client: p.client, interval: c.interval, wait: p.wait, canRead: p.canRead, target: t, mode: modeInstall}
	return out.report(ctx, a, warnings)
}

// LiveSource hands the surface the source of the chart name, --repo and
// --version. Every other site is declined.
func (c *extensionInstallCmd) LiveSource() completion.Source {
	return catalogcomplete.ExtensionInstallSource(c.Describe().Name)
}
