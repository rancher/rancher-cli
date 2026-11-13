package catalog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const flagKeepHistory = "keep-history"

type uninstallCmd struct {
	clients engine.ClusterClientFactory
	actions engine.ActionClientFactory
	now     func() time.Time
	// interval is the spacing of the wait's reads.
	interval time.Duration
}

// NewUninstall builds `app uninstall <release> --cluster <c> --namespace
// <ns>`: a Helm uninstall through the uninstall action of the App,
// previewed from server reads.
func NewUninstall(clients engine.ClusterClientFactory, actions engine.ActionClientFactory) runtime.Command {
	return &uninstallCmd{clients: clients, actions: actions, now: time.Now, interval: defaultPollInterval}
}

func (*uninstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "app uninstall",
		Usage: "Uninstall a Helm release that Rancher shows as an App",
		Desc: "Asks Rancher to uninstall the Helm release <release> in --namespace on --cluster through the App's uninstall action: " +
			"Rancher runs helm uninstall in a pod, which removes the release's resources. " +
			"The CLI never sends the uninstall action's dryRun option; the preview comes from server reads (previewSource serverReads): " +
			"the App (it must exist) and the operations of the namespace (none may still be running for the release). " +
			"It carries no resourceVersion precondition: --apply reads the App and the operations again before it sends the request " +
			"and refuses when the App changed, and without --wait nothing checks for a change after that. " +
			"--keep-history keeps the release's history (status uninstalled); an App already uninstalled with its history kept is removed by running without it. " +
			"--helm-timeout (at least 1s, at most 15m; sent only when given) is Helm's timeout; the chart's hooks can make a run longer, and Rancher removes a run's access one hour after the operation starts. " +
			"Without --wait the command returns once Rancher accepts the request, with the operation and the commands that follow it. " +
			"With --wait (or --timeout, default 55m, at most 55m) it returns when Helm's run has finished: when the App is gone, or with --keep-history reads uninstalled, " +
			"or with Helm's exit code, the release's state and the command to run next.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "release", Type: "string", Required: true, Desc: "Helm release name"},
		},
		Flags: append([]runtime.ArgSpec{
			engine.ClusterFlag(true, true),
			withDesc(engine.NamespaceFlag(true), "`namespace` of the release"),
			{Name: flagKeepHistory, Type: "bool", Desc: "keep the release's history"},
			{Name: flagNoHooks, Type: "bool", Desc: "do not run the chart's hooks"},
			{Name: flagHelmTimeout, Type: "string", Desc: "Helm `timeout` for each step, 1s to 15m"},
		}, waitFlags()...),
		Examples: []string{
			"rancher app uninstall my-nginx --cluster prod --namespace web --dry-run",
			"rancher app uninstall my-nginx --cluster prod --namespace web --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c *uninstallCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t := actionTarget{typedCluster: in.String(engine.FlagCluster), namespace: in.String(engine.FlagNamespace), release: in.Args()[0]}
	timeout, err := helmTimeout(in.String(flagHelmTimeout), 0)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	keep := in.Bool(flagKeepHistory)
	wait, err := readWaitFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	app, err := client.Get(ctx, kind("app").GVR, t.namespace, t.release)
	if err != nil {
		if notFound(err) {
			return safety.Effect{}, nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("app %s not found in namespace %s",
				clierr.ShellQuote(t.release), clierr.ShellQuote(t.namespace))).
				WithHintCommand(clierr.NewCommand("get apps").FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace))
		}
		return safety.Effect{}, nil, err
	}
	status, _, _ := unstructured.NestedString(app.Object, "spec", "info", "status")
	// Rancher runs the uninstall for the App's spec.name and records that
	// name on the Operation; the App object is found by its own name.
	release := t
	if name, _, _ := unstructured.NestedString(app.Object, "spec", "name"); name != "" {
		release.release = name
	}
	var warnings []string
	if status == "uninstalled" {
		if keep {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"release %s is already uninstalled; run without --keep-history to remove its kept history", clierr.ShellQuote(release.release)))
		}
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

	sa, w := uninstallIdentity(ctx, client, app)
	warnings = append(warnings, w...)
	idWarning, canRead, reviewed := identityOf(ctx, client, sa, t.namespace)
	// The Effect's warnings reach the result too: the skip warning is added
	// here only.
	if wait.on && !canRead {
		warnings = append(warnings, skipWarning(false, reviewed, t.namespace))
	}

	ann, _, _ := unstructured.NestedStringMap(app.Object, "spec", "chart", "metadata", "annotations")
	if crd := autoInstallChartNames(ann); len(crd) > 0 {
		warnings = append(warnings, clierr.Text(fmt.Sprintf("the chart installed %s with it, which stays installed", strings.Join(crd, ", ")),
			clierr.Lead("; find its release with "),
			clierr.NewCommand("get apps").FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace)))
	}

	body := uninstallBody{NoHooks: in.Bool(flagNoHooks), KeepHistory: keep, Timeout: timeout}
	chart, _, _ := unstructured.NestedString(app.Object, "spec", "chart", "metadata", "name")
	version, _, _ := unstructured.NestedString(app.Object, "spec", "chart", "metadata", "version")
	diff := []safety.DiffEntry{
		{Path: "releaseName", From: release.release},
		{Path: "namespace", From: t.namespace},
		{Path: "chart", From: chart},
		{Path: "version", From: version},
		{Path: "resources", From: "the release's resources"},
	}
	if body.NoHooks {
		diff = append(diff, safety.DiffEntry{Path: "noHooks", To: true})
	}
	if body.KeepHistory {
		diff = append(diff, safety.DiffEntry{Path: "keepHistory", To: true})
	}
	if body.Timeout != "" {
		diff = append(diff, safety.DiffEntry{Path: "timeout", To: body.Timeout})
	}
	eff := safety.Effect{
		Action:            safety.ActionDelete,
		Target:            kind("app").GVR.Resource + "/" + t.release,
		DisplayKind:       "app",
		DisplayName:       t.release,
		PreviewSource:     safety.PreviewServerReads,
		Cluster:           clusterID,
		Diff:              diff,
		ConflictDetection: false,
		Reversible:        false,
		Destructive:       true,
		Warnings:          append([]string{idWarning, warnServerReadsUninstall}, warnings...),
	}
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsUninstall)
	}
	rv := app.GetResourceVersion()
	exec := func(ctx context.Context) (runtime.Result, error) {
		again, err := client.Get(ctx, kind("app").GVR, t.namespace, t.release)
		if err != nil {
			if notFound(err) {
				return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was removed")
			}
			return runtime.Result{}, err
		}
		if again.GetResourceVersion() != rv {
			return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " changed")
		}
		if err := recheckOperations(ctx, client, release, c.now()); err != nil {
			return runtime.Result{}, err
		}
		a, err := postAction(ctx, c.actions, uninstallPath(clusterID, t.namespace, t.release), body, t, Values{})
		if err != nil {
			return runtime.Result{}, err
		}
		if !wait.on {
			return actionResult("uninstall", t, a, canRead), nil
		}
		w := &appWait{client: client, interval: c.interval, timeout: wait.timeout, t: t, op: a,
			filter: Values{}.filterFunc(), keepHistory: keep, planStatus: status, skip: !canRead}
		end, err := w.run(ctx)
		if err != nil {
			return runtime.Result{}, err
		}
		return waitedResult("uninstall", t, a, end), nil
	}
	return eff, exec, nil
}

// uninstallIdentity is who Helm runs as for an uninstall: the service
// account of the ClusterRepo the App's chart names in its source
// annotations, in the App's namespace, else the caller (""). A ClusterRepo
// the caller cannot read leaves the identity unknown, with a warning.
func uninstallIdentity(ctx context.Context, client engine.ClusterClient, app *unstructured.Unstructured) (string, []string) {
	ann, _, _ := unstructured.NestedStringMap(app.Object, "spec", "chart", "metadata", "annotations")
	repo := ann[annotationSourceRepo]
	if ann[annotationSourceRepoType] != sourceRepoTypeCluster || repo == "" {
		return "", nil
	}
	obj, err := client.Get(ctx, kind("clusterrepo").GVR, "", repo)
	if err != nil {
		if notFound(err) {
			return "", nil
		}
		return "", []string{fmt.Sprintf("cannot read ClusterRepo %s, which the app names as its source; when it sets a service account, Helm runs as that account instead",
			clierr.ShellQuote(repo))}
	}
	sa, _, _ := unstructured.NestedString(obj.Object, "spec", "serviceAccount")
	saNS, _, _ := unstructured.NestedString(obj.Object, "spec", "serviceAccountNamespace")
	if sa == "" || strings.Contains(saNS, ":") {
		return "", nil
	}
	return "system:serviceaccount:" + app.GetNamespace() + ":" + sa, nil
}
