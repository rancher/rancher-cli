package complete

import (
	"context"
	"slices"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// The annotations that name an App's source ClusterRepo.
const (
	annotationSourceRepoType = "catalog.cattle.io/ui-source-repo-type"
	annotationSourceRepo     = "catalog.cattle.io/ui-source-repo"
	sourceRepoTypeCluster    = "cluster"
)

// app is the part of an App the app upgrade sources decide on.
type app struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name        string            `json:"name"`
				Version     string            `json:"version"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"chart"`
	} `json:"spec"`
}

// appList is the list of the Apps of a namespace.
type appList struct {
	Items []app `json:"items"`
}

// upgradable reports whether Helm can upgrade the App by its status:
// deployed, failed or superseded.
func (a app) upgradable() bool {
	return slices.Contains([]string{"deployed", "failed", "superseded"}, a.Spec.Info.Status)
}

// sourceRepo is the ClusterRepo the App's chart came from: the source
// annotation when the source type is a ClusterRepo, else "".
func (a app) sourceRepo() string {
	ann := a.Spec.Chart.Metadata.Annotations
	if ann[annotationSourceRepoType] != sourceRepoTypeCluster {
		return ""
	}
	return ann[annotationSourceRepo]
}

// namesChartToInstallFirst reports whether the annotations name a chart to
// install first, which app upgrade refuses.
func namesChartToInstallFirst(ann map[string]string) bool {
	return namesAChart(ann[annotationAutoInstall]) || namesAChart(ann[annotationAutoInstallGVR])
}

// upgradeVersionOffered reports whether app upgrade accepts the version with
// these annotations as the target for the release in the namespace: it names
// no chart to install first, and a fixed catalog.cattle.io/namespace or
// release-name equals the release's, unless the version is the installed one.
func upgradeVersionOffered(ann map[string]string, namespace, release string, kept bool) bool {
	if namesChartToInstallFirst(ann) {
		return false
	}
	if kept {
		return true
	}
	if want := ann[annotationNamespace]; want != "" && want != namespace {
		return false
	}
	if want := ann[annotationReleaseName]; want != "" && want != release {
		return false
	}
	return true
}

// typedApp reads the App the line names: <release> of --namespace on the
// cluster, one request. It reports false for a line without either, an App
// that cannot be read, an App with a status Helm cannot upgrade, and, when
// matchChart is true, an App whose chart is not the typed --chart.
func typedApp(ctx context.Context, conn *completion.Connection, req completion.Request, matchChart bool) (app, bool) {
	k, known := kinds.ByKey("app")
	namespace := completion.TailFlag(req.Tail, "namespace")
	if !known || req.ClusterID == "" || namespace == "" || len(req.Positionals) == 0 {
		return app{}, false
	}
	p, err := completion.NamedResourcePath(req.ClusterID, k.GVR.Group, k.GVR.Version, namespace, k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return app{}, false
	}
	var a app
	if err := conn.GetJSON(ctx, p, "", &a); err != nil || !a.upgradable() {
		return app{}, false
	}
	if chart := completion.TailFlag(req.Tail, "chart"); matchChart && chart != "" && chart != a.Spec.Chart.Metadata.Name {
		return app{}, false
	}
	return a, true
}

// clusterReads is the declaration of a source that makes plain reads through
// the typed cluster: the cluster resolved, any credential, allowance requests.
func clusterReads(allowance completion.Allowance) completion.Requirements {
	return completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: allowance}
}

// UpgradeSource is the live source of app upgrade, whose registered name is
// command. It answers the values the command accepts for what the line
// shows, and nothing else:
//
//   - <release>: the Apps of --namespace (two requests: the Apps and the
//     ClusterRepos) with a status Helm can upgrade; without --repo, those with
//     an enabled ClusterRepo as their source; without --version, those whose
//     chart names no chart to install first; with --chart, those of that
//     chart; nothing when a typed --repo is absent or disabled.
//   - --repo: the App's source ClusterRepo when it is enabled (two requests).
//   - --chart: the App's chart (one request).
//   - --version: the versions of the App's chart in the filtered index of the
//     typed or source ClusterRepo, and the installed version (three requests:
//     the App, the ClusterRepo, the index).
func UpgradeSource(command string) completion.Source {
	pick := func(req completion.Request) (completion.Requirements, func(context.Context, *completion.Connection, completion.Request) completion.Result, bool) {
		if req.Command != command {
			return completion.Requirements{}, nil, false
		}
		switch {
		case req.Flag == "" && req.Index == 0:
			return clusterReads(completion.AllowanceTwo), completeRelease, true
		case req.Flag == "repo":
			return clusterReads(completion.AllowanceTwo), completeUpgradeRepo, true
		case req.Flag == "chart":
			return clusterReads(completion.AllowanceOne), completeUpgradeChart, true
		case req.Flag == "version":
			return completion.ClusterThreeReads(), completeUpgradeVersion, true
		}
		return completion.Requirements{}, nil, false
	}
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			reqs, _, ok := pick(req)
			return reqs, ok
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			_, complete, ok := pick(req)
			if !ok {
				return completion.Result{}
			}
			return complete(ctx, conn, req)
		},
	}
}

// completeRelease lists the Apps of --namespace that the command would not
// refuse for what the line shows.
func completeRelease(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	apps, ok := kinds.ByKey("app")
	repos, okRepos := kinds.ByKey("clusterrepo")
	namespace := completion.TailFlag(req.Tail, "namespace")
	if !ok || !okRepos || req.ClusterID == "" || namespace == "" {
		return completion.Result{}
	}
	appsPath, err := completion.NamespacedPath(req.ClusterID, apps.GVR.Group, apps.GVR.Version, namespace, apps.GVR.Resource)
	if err != nil {
		return completion.Result{}
	}
	reposPath, err := completion.ClusterPath(req.ClusterID, repos.GVR.Group, repos.GVR.Version, repos.GVR.Resource)
	if err != nil {
		return completion.Result{}
	}
	var appItems appList
	if err := conn.GetJSON(ctx, appsPath, "", &appItems); err != nil {
		return completion.Result{}
	}
	var repoItems struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Enabled *bool `json:"enabled"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := conn.GetJSON(ctx, reposPath, "", &repoItems); err != nil {
		return completion.Result{}
	}
	enabled := map[string]bool{}
	for _, r := range repoItems.Items {
		if r.Spec.Enabled == nil || *r.Spec.Enabled {
			enabled[r.Metadata.Name] = true
		}
	}
	repo := completion.TailFlag(req.Tail, "repo")
	version := completion.TailFlag(req.Tail, "version")
	chart := completion.TailFlag(req.Tail, "chart")
	if repo != "" && !enabled[repo] {
		return completion.Result{}
	}
	var names []string
	for _, a := range appItems.Items {
		if a.Metadata.Name == "" || !a.upgradable() {
			continue
		}
		if repo == "" && !enabled[a.sourceRepo()] {
			continue
		}
		if version == "" && namesChartToInstallFirst(a.Spec.Chart.Metadata.Annotations) {
			continue
		}
		if chart != "" && chart != a.Spec.Chart.Metadata.Name {
			continue
		}
		names = append(names, a.Metadata.Name)
	}
	return completion.Result{Names: names}
}

// completeUpgradeRepo offers the App's source ClusterRepo when it is
// enabled: the chart is the App's, and only that repository is known to
// serve it.
func completeUpgradeRepo(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	a, ok := typedApp(ctx, conn, req, true)
	if !ok {
		return completion.Result{}
	}
	repo := a.sourceRepo()
	if !clusterRepoEnabled(ctx, conn, req.ClusterID, repo) {
		return completion.Result{}
	}
	return completion.Result{Names: []string{repo}}
}

// completeUpgradeChart offers exactly the App's chart: an upgrade cannot
// change it.
func completeUpgradeChart(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	a, ok := typedApp(ctx, conn, req, false)
	if !ok || a.Spec.Chart.Metadata.Name == "" {
		return completion.Result{}
	}
	return completion.Result{Names: []string{a.Spec.Chart.Metadata.Name}}
}

// completeUpgradeVersion offers the versions of the App's chart in the
// filtered index of the typed --repo, else the App's source ClusterRepo, in
// the order Rancher serves them, and the installed version. Each passes the
// checks the command makes on the annotations of its target; the installed
// version is exempt from the namespace and release-name checks, and comes
// from the App when the index no longer lists it, only when the repository is
// the one the App came from.
func completeUpgradeVersion(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	a, ok := typedApp(ctx, conn, req, true)
	if !ok {
		return completion.Result{}
	}
	repo := completion.TailFlag(req.Tail, "repo")
	if repo == "" {
		repo = a.sourceRepo()
	}
	// Only the repository the app came from accepts the installed version when
	// the index no longer lists it.
	source := repo == a.sourceRepo()
	if !clusterRepoEnabled(ctx, conn, req.ClusterID, repo) {
		return completion.Result{}
	}
	idx, ok := readRepoIndex(ctx, conn, req.ClusterID, repo)
	if !ok {
		return completion.Result{}
	}
	namespace := completion.TailFlag(req.Tail, "namespace")
	release := req.Positionals[0]
	current := a.Spec.Chart.Metadata.Version
	var names []string
	listed := false
	for _, v := range idx.Entries[a.Spec.Chart.Metadata.Name] {
		if v.Version == "" {
			continue
		}
		kept := source && v.Version == current
		listed = listed || kept
		if upgradeVersionOffered(v.Annotations, namespace, release, kept) {
			names = append(names, v.Version)
		}
	}
	if source && !listed && current != "" && upgradeVersionOffered(a.Spec.Chart.Metadata.Annotations, namespace, release, true) {
		names = append(names, current)
	}
	return completion.Result{Names: names}
}
