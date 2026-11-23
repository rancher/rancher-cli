package complete

import (
	"context"
	"slices"

	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// extensionApp is an App of the extension namespace as the upgrade reads it.
type extensionApp struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Version     string            `json:"version"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"chart"`
	} `json:"spec"`
}

// upgradable reports whether Helm can upgrade the App's release.
func (a extensionApp) upgradable() bool {
	switch a.Spec.Info.Status {
	case "deployed", "failed", "superseded":
		return true
	}
	return false
}

// repo is the ClusterRepo the App came from: the annotation of its chart, else
// its label; empty when it has neither.
func (a extensionApp) repo() string {
	if repo := a.Spec.Chart.Metadata.Annotations[extension.AnnotationSourceRepo]; repo != "" {
		return repo
	}
	return a.Metadata.Labels[extension.AnnotationClusterRepoName]
}

// ExtensionUpgradeSource is the live source of `extension upgrade`, whose
// registered name is command: the chart name, --repo and --version, each only
// with a value the command accepts.
//
//   - <chart>: an App whose release Helm can upgrade; the App names the
//     ClusterRepo it came from (the typed --repo, when there is one); that
//     ClusterRepo is read by the Rancher UI and is active; the chart is an
//     extension chart in its index and installs one extension; its first version
//     that passes the checks of the Rancher UI is not the installed one (the
//     Apps, the ClusterRepos, one index per ClusterRepo the Apps name and the
//     two compatibility reads).
//   - --repo: the ClusterRepo the typed chart's App came from, when the command
//     accepts it (two requests: the App and the ClusterRepos).
//   - --version: the versions of the typed chart that pass the checks of the
//     Rancher UI, the installed one left out (the compatibility reads, the App,
//     the ClusterRepos and the index).
//
// An App that names more ClusterRepos than completion.ExtensionRepositoryLimit
// leaves the chart names empty. The source claims no other site.
func ExtensionUpgradeSource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command {
				return completion.Requirements{}, false
			}
			reqs := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement}
			switch {
			case req.Flag == "" && req.Index == 0:
				reqs.Allowance = completion.AllowanceExtension
			case req.Flag == flagRepo && len(req.Positionals) > 0:
				reqs.Allowance = completion.AllowanceTwo
			case req.Flag == flagVersion && len(req.Positionals) > 0:
				reqs.Allowance = completion.AllowanceExtension
			default:
				return completion.Requirements{}, false
			}
			return reqs, true
		},
		Complete: completeExtensionUpgrade,
	}
}

func completeExtensionUpgrade(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	switch req.Flag {
	case flagRepo:
		return completeUpgradeRepo(ctx, conn, req.Positionals[0])
	case flagVersion:
		return completeUpgradeVersions(ctx, conn, req)
	}
	return completeUpgradeCharts(ctx, conn, req)
}

// readExtensionApp reads the App of the typed chart: one request.
func readExtensionApp(ctx context.Context, conn *completion.Connection, chart string) (extensionApp, bool) {
	p, err := completion.NamedResourcePath("local", "catalog.cattle.io", "v1", extension.Namespace, "apps", chart)
	if err != nil {
		return extensionApp{}, false
	}
	var app extensionApp
	if err := conn.GetJSON(ctx, p, "", &app); err != nil {
		return extensionApp{}, false
	}
	return app, true
}

// readExtensionApps reads the Apps of the extension namespace: one request.
func readExtensionApps(ctx context.Context, conn *completion.Connection) ([]extensionApp, bool) {
	p, err := completion.NamespacedPath("local", "catalog.cattle.io", "v1", extension.Namespace, "apps")
	if err != nil {
		return nil, false
	}
	var list struct {
		Items []extensionApp `json:"items"`
	}
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, false
	}
	return list.Items, true
}

// acceptsRepo reports whether the command takes the ClusterRepo as the
// source of an upgrade: the Rancher UI reads it and it is active.
func acceptsRepo(repos []installRepo, name string) bool {
	i := slices.IndexFunc(repos, func(r installRepo) bool { return r.name == name })
	return i >= 0 && repos[i].reads && repos[i].active
}

// upgradeVersions returns the versions of the chart in the index that pass the
// checks of the Rancher UI, in index order, or nil when the chart is not an
// extension chart there or installs several extensions.
func upgradeVersions(idx repoIndex, chart string, inputs extension.Inputs) []string {
	versions := extensionVersions(idx, chart)
	if versions == nil || extension.IsBundleChart(annotationsOf(versions)) {
		return nil
	}
	var out []string
	for _, v := range versions {
		if extension.Compatibility(v.Annotations, inputs).Installable {
			out = append(out, v.Version)
		}
	}
	return out
}

// completeUpgradeRepo offers the ClusterRepo of the typed chart's App when the
// command accepts it.
func completeUpgradeRepo(ctx context.Context, conn *completion.Connection, chart string) completion.Result {
	app, ok := readExtensionApp(ctx, conn, chart)
	if !ok || !app.upgradable() || app.repo() == "" {
		return completion.Result{}
	}
	repos, ok := readInstallRepos(ctx, conn)
	if !ok || !acceptsRepo(repos, app.repo()) {
		return completion.Result{}
	}
	return completion.Result{Names: []string{app.repo()}}
}

// completeUpgradeVersions offers the versions of the typed chart that pass the
// checks, without the installed one. A typed --repo other than the App's
// ClusterRepo offers nothing.
func completeUpgradeVersions(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	chart := req.Positionals[0]
	app, ok := readExtensionApp(ctx, conn, chart)
	if !ok || !app.upgradable() || app.repo() == "" {
		return completion.Result{}
	}
	if typed := completion.TailFlag(req.Tail, flagRepo); typed != "" && typed != app.repo() {
		return completion.Result{}
	}
	repos, ok := readInstallRepos(ctx, conn)
	if !ok || !acceptsRepo(repos, app.repo()) {
		return completion.Result{}
	}
	idx, ok := readRepoIndex(ctx, conn, app.repo())
	if !ok {
		return completion.Result{}
	}
	inputs := readInputs(ctx, conn)
	if ctx.Err() != nil {
		return completion.Result{}
	}
	var names []string
	for _, v := range upgradeVersions(idx, chart, inputs) {
		if v != app.Spec.Chart.Metadata.Version {
			names = append(names, v)
		}
	}
	return completion.Result{Names: names}
}

// completeUpgradeCharts offers the Apps the command upgrades: those with a
// version other than the installed one as the default.
func completeUpgradeCharts(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	typed := completion.TailFlag(req.Tail, flagRepo)
	apps, ok := readExtensionApps(ctx, conn)
	if !ok {
		return completion.Result{}
	}
	repos, ok := readInstallRepos(ctx, conn)
	if !ok {
		return completion.Result{}
	}
	var candidates []extensionApp
	var sources []string
	for _, a := range apps {
		repo := a.repo()
		if !a.upgradable() || repo == "" || (typed != "" && repo != typed) || !acceptsRepo(repos, repo) {
			continue
		}
		candidates = append(candidates, a)
		if !slices.Contains(sources, repo) {
			sources = append(sources, repo)
		}
	}
	if len(candidates) == 0 || len(sources) > completion.ExtensionRepositoryLimit {
		return completion.Result{}
	}
	inputs := readInputs(ctx, conn)
	indexes := map[string]repoIndex{}
	for _, repo := range sources {
		if idx, ok := readRepoIndex(ctx, conn, repo); ok {
			indexes[repo] = idx
		}
	}
	if ctx.Err() != nil {
		return completion.Result{}
	}
	var names []string
	for _, a := range candidates {
		versions := upgradeVersions(indexes[a.repo()], a.Metadata.Name, inputs)
		if len(versions) > 0 && versions[0] != a.Spec.Chart.Metadata.Version {
			names = append(names, a.Metadata.Name)
		}
	}
	return completion.Result{Names: names}
}
