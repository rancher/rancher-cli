package catalog

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// bundleChartText is the refusal of a chart that installs several extensions
// together.
const bundleChartText = "this chart installs several extensions together; install it from the Rancher UI"

// registryRepositoryPrefix is the start of the image repository of a chart
// whose image the system default registry replaces, as in the Rancher UI.
const registryRepositoryPrefix = "rancher/"

// extensionTarget is the release of an extension: always in the extension
// namespace of the local cluster, with the chart's name as its release name.
func extensionTarget(chart, requestID string) actionTarget {
	return actionTarget{typedCluster: engine.LocalCluster, namespace: extension.Namespace, release: chart, requestID: requestID}
}

// extensionActionBody is the body of the install and upgrade actions the
// Rancher UI's Extensions page sends: no timeout, wait or Helm option.
type extensionActionBody struct {
	Namespace string           `json:"namespace"`
	Charts    []extensionChart `json:"charts"`
}

// extensionChart is the one chart entry of an extensionActionBody. Values is
// always sent, as {} when empty.
type extensionChart struct {
	ChartName   string            `json:"chartName"`
	Version     string            `json:"version"`
	ReleaseName string            `json:"releaseName"`
	Values      map[string]any    `json:"values"`
	Annotations map[string]string `json:"annotations"`
}

// newExtensionBody is the body that installs or upgrades chart to version from
// the ClusterRepo repo. registry is the system default registry the values
// carry, or "" for none.
func newExtensionBody(t actionTarget, repo, version, registry string) extensionActionBody {
	values := map[string]any{}
	if registry != "" {
		values["global"] = map[string]any{"cattle": map[string]any{"systemDefaultRegistry": registry}}
	}
	return extensionActionBody{
		Namespace: t.namespace,
		Charts: []extensionChart{{
			ChartName:   t.release,
			Version:     version,
			ReleaseName: t.release,
			Values:      values,
			Annotations: map[string]string{
				annotationRequestID:      t.requestID,
				annotationSourceRepoType: sourceRepoTypeCluster,
				annotationSourceRepo:     repo,
			},
		}},
	}
}

// getRepoHint is the command that shows a ClusterRepo of the local cluster.
func getRepoHint(repo string) clierr.Command {
	return clierr.NewCommand("get clusterrepo").Arg(repo).FlagValue("--cluster", engine.LocalCluster)
}

// extensionListHint is the command that lists the extensions.
func extensionListHint() clierr.Command { return clierr.NewCommand("extension list") }

// extensionChoice is the repository an extension comes from and the versions
// of the chart in its index, newest first as Rancher serves them.
type extensionChoice struct {
	repo     repoRow
	versions []ChartVersion
	warnings []string
}

// extensionVersionsOf returns the versions of an extension chart of the index
// that the Rancher UI lists, or nil when the chart is absent, not an extension
// or hidden.
func extensionVersionsOf(idx Index, chart string) []ChartVersion {
	versions := idx.Entries[chart]
	annotations := versionAnnotations(versions)
	if len(versions) == 0 || !extension.IsExtensionChart(annotations) || extension.IsHiddenExtension(annotations) {
		return nil
	}
	return versions
}

// chooseExtensionRepo finds the ClusterRepo that offers the extension chart. A
// typed repository must be one the Rancher UI reads; without one, exactly one
// repository the Rancher UI reads must offer the chart.
func chooseExtensionRepo(ctx context.Context, root engine.RootClient, repos []repoRow, chart, typed string) (extensionChoice, error) {
	if typed != "" {
		return namedExtensionRepo(ctx, root, repos, chart, typed)
	}
	scan, err := scanRepos(ctx, root, repos)
	if err != nil {
		return extensionChoice{}, err
	}
	var offering []string
	for _, c := range scan.charts {
		if c.Name == chart {
			offering = append(offering, c.Repo)
		}
	}
	switch len(offering) {
	case 0:
		msg := "no ClusterRepo the Rancher UI reads offers an extension named " + clierr.ShellQuote(chart)
		if len(scan.warnings) > 0 {
			msg += "; " + strings.Join(scan.warnings, "; ")
		}
		return extensionChoice{}, clierr.New(clierr.CodeValidation, msg).WithHintCommand(extensionListHint())
	case 1:
		i := slices.IndexFunc(repos, func(r repoRow) bool { return r.Name == offering[0] })
		return extensionChoice{repo: repos[i], versions: extensionVersionsOf(scan.indexes[offering[0]], chart), warnings: scan.warnings}, nil
	}
	quoted := make([]string, len(offering))
	for i, name := range offering {
		quoted[i] = clierr.ShellQuote(name)
	}
	return extensionChoice{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"extension %s is offered by more than one ClusterRepo: %s; name one with --%s",
		clierr.ShellQuote(chart), strings.Join(quoted, ", "), flagRepo)).
		WithHintCommand(clierr.NewCommand("extension install").Arg(chart).FlagValue("--"+flagRepo, offering[0]).Flag("--dry-run"))
}

// namedExtensionRepo checks the ClusterRepo --repo names against what the
// Rancher UI reads, reads its index and takes the chart from it.
func namedExtensionRepo(ctx context.Context, root engine.RootClient, repos []repoRow, chart, name string) (extensionChoice, error) {
	i := slices.IndexFunc(repos, func(r repoRow) bool { return r.Name == name })
	if i < 0 {
		return extensionChoice{}, clierr.New(clierr.CodeNotFound, "ClusterRepo "+clierr.ShellQuote(name)+" not found").
			WithHintCommand(clierr.NewCommand("get clusterrepos").FlagValue("--cluster", engine.LocalCluster))
	}
	repo := repos[i]
	q := clierr.ShellQuote(name)
	switch {
	case repo.Hidden:
		return extensionChoice{}, clierr.New(clierr.CodeValidation, "ClusterRepo "+q+" is hidden, and the Rancher UI does not read extensions from it")
	case !repo.Enabled:
		return extensionChoice{}, clierr.New(clierr.CodeValidation, "ClusterRepo "+q+" is disabled").WithHintCommand(getRepoHint(name))
	case repo.State != stateActive:
		return extensionChoice{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("ClusterRepo %s is not ready: its state is %s", q, stateText(repo.State))).WithHintCommand(getRepoHint(name))
	}
	idx, err := ReadIndex(ctx, root, engine.LocalCluster, name)
	if err != nil {
		if status, ok := clierr.HTTPStatus(err); ok && status == http.StatusServiceUnavailable {
			return extensionChoice{}, clierr.New(clierr.CodeValidation,
				"ClusterRepo "+q+" is not ready: Rancher is still downloading it").WithHintCommand(getRepoHint(name))
		}
		return extensionChoice{}, err
	}
	versions := extensionVersionsOf(idx, chart)
	if versions == nil {
		return extensionChoice{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("chart %s in ClusterRepo %s is not a Rancher UI extension", clierr.ShellQuote(chart), q)).
			WithHintCommand(clierr.NewCommand("charts").FlagValue("--cluster", engine.LocalCluster).FlagValue("--"+flagRepo, name))
	}
	return extensionChoice{repo: repo, versions: versions}, nil
}

// refuseBundle refuses a chart that installs several extensions together.
func refuseBundle(versions []ChartVersion) error {
	if extension.IsBundleChart(versionAnnotations(versions)) {
		return clierr.New(clierr.CodeValidation, bundleChartText)
	}
	return nil
}

// chosenVersion is the version an extension command acts on, with what the
// preview says about the choice.
type chosenVersion struct {
	index    int
	entry    ChartVersion
	warnings []string
}

// chooseExtensionVersion picks the version of an extension chart as the
// Rancher UI does: the exact --version when typed, else the first version in
// the index that passes the UI's checks. A version that fails a check is
// refused with the check's reason.
func chooseExtensionVersion(ec extensionContext, chart, repo string, versions []ChartVersion, want string) (chosenVersion, error) {
	q := clierr.ShellQuote(chart)
	outcomes := make([]extension.Outcome, len(versions))
	for i, v := range versions {
		outcomes[i] = extension.Compatibility(v.Annotations, ec.inputs)
	}
	var chosen int
	if want != "" {
		chosen = slices.IndexFunc(versions, func(v ChartVersion) bool { return v.Version == want })
		if chosen < 0 {
			return chosenVersion{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"version %s of extension %s is not in the index ClusterRepo %s serves; --%s names one exact version; the index has %s",
				clierr.ShellQuote(want), q, clierr.ShellQuote(repo), flagVersion, versionList(versions)))
		}
		if !outcomes[chosen].Installable {
			return chosenVersion{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"version %s of extension %s is not compatible with this server: %s",
				clierr.ShellQuote(want), q, outcomes[chosen].Reason)).WithHintCommand(extensionListHint())
		}
	} else if chosen = slices.IndexFunc(outcomes, func(o extension.Outcome) bool { return o.Installable }); chosen < 0 {
		return chosenVersion{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"no version of extension %s in ClusterRepo %s is compatible with this server; the newest, %s, %s",
			q, clierr.ShellQuote(repo), versions[0].Version, outcomes[0].Reason)).WithHintCommand(extensionListHint())
	}
	var warnings []string
	if want == "" {
		v := versions[chosen]
		switch {
		case IsPrerelease(v.Version):
			warnings = append(warnings, fmt.Sprintf("%s is a prerelease version of extension %s; name another with --%s", v.Version, chart, flagVersion))
		case v.Deprecated:
			warnings = append(warnings, fmt.Sprintf("%s of extension %s is deprecated; name another with --%s", v.Version, chart, flagVersion))
		}
	}
	first := chosen
	if want == "" {
		first = 0
	}
	item := listItem{outcomes: outcomes[first : chosen+1]}
	warnings = append(warnings, extensionWarnings([]listItem{item}, ec.inputs, ec.rancherVersion, ec.apiKnown)...)
	return chosenVersion{index: chosen, entry: versions[chosen], warnings: warnings}, nil
}

// registryHost is the system default registry the Rancher UI sends in the
// values of an extension: the setting's value, only when it is not empty and
// the chart's default image.repository starts with rancher/. The setting is
// read first and the chart's info only when the setting has a value. A failed
// read, including one the server refuses, is returned.
func registryHost(ctx context.Context, root engine.RootClient, repo, chart, version string) (string, error) {
	host, denied, err := readSetting(ctx, root, settingSystemDefaultRegistry)
	switch {
	case err != nil:
		return "", err
	case denied:
		return "", clierr.New(clierr.CodeRBAC, "you cannot get the setting "+clierr.ShellQuote(settingSystemDefaultRegistry)+
			", which the command reads to find the system default registry it sends in the values; ask for permission to get it")
	case host == "":
		return "", nil
	}
	info, err := readChartInfo(ctx, root, infoTarget{
		noun:    "extension",
		cluster: engine.LocalCluster, typedCluster: engine.LocalCluster, repo: repo, chart: chart, version: version,
		listHint: extensionListHint(), versionsHint: extensionListHint(),
	})
	if err != nil {
		return "", err
	}
	if v, st, _ := lookup(info.Values, valuePath{"image", "repository"}); st == pathPresent {
		if s, ok := v.(string); ok && strings.HasPrefix(s, registryRepositoryPrefix) {
			return host, nil
		}
	}
	return "", nil
}

// repoIdentity is who Helm runs as for a request from the repository: its
// service account when the ClusterRepo has one, else the caller (""), as for app install.
func repoIdentity(repo repoRow) string {
	return serviceAccountIdentity(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"serviceAccount": repo.ServiceAccount, "serviceAccountNamespace": repo.ServiceAccountNamespace,
	}}})
}

// registryValuePath is the path of the registry in the values, as the preview
// shows it.
const registryValuePath = "values.global.cattle.systemDefaultRegistry"

// extensionOutcome is what an accepted request of an extension command needs
// to report its end.
type extensionOutcome struct {
	client   engine.ClusterClient
	interval time.Duration
	wait     waitRequest
	canRead  bool
	target   actionTarget
	mode     waitMode
	// recorded is the App's spec.version before an upgrade, else 0.
	recorded int64
	// planStatus is the App's status before an uninstall, which its wait
	// compares with the status at the end.
	planStatus string
}

// report is the result of an accepted request: the operation Rancher created,
// or, with --wait, how Helm's run ended.
func (o extensionOutcome) report(ctx context.Context, a actionAnswer, warnings []string) (runtime.Result, error) {
	verb := o.mode.verb()
	if !o.wait.on {
		res := actionResult(verb, o.target, a, o.canRead)
		res.Warnings = append(warnings, res.Warnings...)
		return res, nil
	}
	w := &appWait{client: o.client, interval: o.interval, timeout: o.wait.timeout, t: o.target, op: a,
		filter: Values{}.filterFunc(), mode: o.mode, recorded: o.recorded, planStatus: o.planStatus, skip: !o.canRead}
	end, err := w.run(ctx)
	if err != nil {
		return runtime.Result{}, err
	}
	res := waitedResult(verb, o.target, a, end)
	res.Warnings = append(warnings, res.Warnings...)
	return res, nil
}
