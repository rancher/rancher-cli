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
)

type extensionUninstallCmd struct {
	clients  engine.ClusterClientFactory
	actions  engine.ActionClientFactory
	now      func() time.Time
	interval time.Duration
}

// NewExtensionUninstall builds `extension uninstall <chart>`: the uninstall of
// an installed Rancher UI extension through the uninstall action of its App,
// as the Rancher UI's Extensions page does, previewed from server reads.
func NewExtensionUninstall(clients engine.ClusterClientFactory, actions engine.ActionClientFactory) runtime.Command {
	return &extensionUninstallCmd{clients: clients, actions: actions, now: time.Now, interval: defaultPollInterval}
}

func (*extensionUninstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "extension uninstall",
		Usage: "Uninstall a Rancher UI extension",
		Desc: "Asks Rancher to uninstall the extension <chart> as the Rancher UI's Extensions page does: " +
			"through the uninstall action of the App <chart> in the namespace cattle-ui-plugin-system of the local cluster, with the same empty request the Rancher UI sends; " +
			"Helm deletes the UIPlugin the chart created. " +
			"The server has no dry-run for this action; the preview comes from server reads (previewSource serverReads): " +
			"the App and the operations of the namespace (none may still be running for the release). " +
			"It carries no resourceVersion precondition on the request itself: --apply reads the App and the operations again before it sends the request, " +
			"and refuses when the App changed, and without --wait nothing checks for a change after that. " +
			"Refused: an extension that is not installed or was loaded from a URL (the Rancher UI uninstalls it), " +
			"and a server where the feature uiextension is turned off or the namespace does not exist. " +
			"An App with the status uninstalled (Helm kept its history) is accepted, with a warning that the command removes that history. " +
			"Without --wait the command returns once Rancher accepts the request, with the operation and the commands that follow it. " +
			"With --wait (or --timeout, default 55m, at most 55m) it returns when Helm's run has finished: with the release's status, " +
			"or with Helm's error, the release's state and the command to run next.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "chart", Type: "string", Required: true, Desc: "name of the extension chart, which is also its release name"},
		},
		Flags: waitFlags(),
		Examples: []string{
			"rancher extension uninstall epinio --dry-run",
			"rancher extension uninstall epinio --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c *extensionUninstallCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	chart := in.Args()[0]
	if err := validateReleaseName(chart); err != nil {
		return safety.Effect{}, nil, err
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
	t := extensionTarget(chart, "")
	appObj, err := client.Get(ctx, kind("app").GVR, t.namespace, chart)
	if err != nil {
		if notFound(err) {
			return safety.Effect{}, nil, noExtensionApp(ctx, client, t, "it is uninstalled in the Rancher UI", extensionListHint())
		}
		return safety.Effect{}, nil, err
	}
	app := readAppRecord(appObj)
	release := t
	release.release = app.name
	var warnings []string
	if app.status == "uninstalled" {
		warnings = append(warnings, fmt.Sprintf("release %s is already uninstalled; this removes its kept history", clierr.ShellQuote(release.release)))
	}
	op, canList, err := runningOperation(ctx, client, t.namespace, release.release, c.now())
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if op != "" {
		return safety.Effect{}, nil, operationRunning(release, op)
	}
	if !canList {
		warnings = append(warnings, operationsNotChecked(release))
	}
	sa, w := uninstallIdentity(ctx, client, appObj)
	warnings = append(warnings, w...)
	idWarning, canRead, reviewed := identityOf(ctx, client, sa, t.namespace)
	if wait.on && !canRead {
		warnings = append(warnings, skipWarning(modeUninstall, reviewed, t.namespace))
	}

	diff := []safety.DiffEntry{
		{Path: "releaseName", From: release.release},
		{Path: "namespace", From: t.namespace},
		{Path: "chart", From: app.chart},
		{Path: "version", From: app.version},
		{Path: "resources", From: "the release's resources"},
	}
	eff := uninstallEffect(engine.LocalCluster, chart, diff, append([]string{idWarning, warnServerReadsUninstall}, warnings...))
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsUninstall)
		t.pullSecretNote = notePullSecretsUninstall
	}
	rv := app.resourceVersion
	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, t, release, rv, extensionOutcome{
			client: client, interval: c.interval, wait: wait, canRead: canRead, target: t, mode: modeUninstall, planStatus: app.status})
	}
	return eff, exec, nil
}

// execute reads the App and the operations again, then sends the request once.
func (c *extensionUninstallCmd) execute(ctx context.Context, client engine.ClusterClient, t, release actionTarget, rv string, out extensionOutcome) (runtime.Result, error) {
	again, err := client.Get(ctx, kind("app").GVR, t.namespace, t.release)
	if err != nil {
		if notFound(err) {
			return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was deleted")
		}
		return runtime.Result{}, err
	}
	if again.GetResourceVersion() != rv {
		return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " changed")
	}
	if err := recheckOperations(ctx, client, release, c.now()); err != nil {
		return runtime.Result{}, err
	}
	a, err := postAction(ctx, c.actions, uninstallPath(engine.LocalCluster, t.namespace, t.release), uninstallBody{}, t, Values{})
	if err != nil {
		return runtime.Result{}, err
	}
	return out.report(ctx, a, nil)
}

// LiveSource hands the surface the source of the chart name. Every other site
// is declined.
func (c *extensionUninstallCmd) LiveSource() completion.Source {
	return catalogcomplete.ExtensionUninstallSource(c.Describe().Name)
}
