package complete

import (
	"context"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// The annotations the --chart and --version sources read from the index.
const (
	annotationDeprecated     = "catalog.cattle.io/deprecated"
	annotationHidden         = "catalog.cattle.io/hidden"
	annotationNamespace      = "catalog.cattle.io/namespace"
	annotationReleaseName    = "catalog.cattle.io/release-name"
	annotationAutoInstall    = "catalog.cattle.io/auto-install"
	annotationAutoInstallGVR = "catalog.cattle.io/auto-install-gvr"
)

// indexVersion is one version of a chart in a ClusterRepo's Helm repository
// index, with the fields the sources decide on.
type indexVersion struct {
	Version     string            `json:"version"`
	Deprecated  bool              `json:"deprecated"`
	Annotations map[string]string `json:"annotations"`
}

// repoIndex is a Helm repository index: the versions of each chart, in the
// order Rancher serves them.
type repoIndex struct {
	Entries map[string][]indexVersion `json:"entries"`
}

// deprecated reports whether the index entry's deprecated field is true or
// its annotation catalog.cattle.io/deprecated is "true".
func (v indexVersion) deprecated() bool {
	return v.Deprecated || v.Annotations[annotationDeprecated] == "true"
}

// hidden reports whether the annotation catalog.cattle.io/hidden is present
// and not empty, whatever its value.
func (v indexVersion) hidden() bool { return v.Annotations[annotationHidden] != "" }

// namesAChart reports whether an annotation value that lists charts holds at
// least one entry.
func namesAChart(v string) bool {
	for e := range strings.SplitSeq(v, ",") {
		if strings.TrimSpace(e) != "" {
			return true
		}
	}
	return false
}

// installable reports whether the index alone allows installing v as the
// release in the namespace, the last two as typed ("" when not typed yet): the
// chart is not hidden, names no chart to install first, and its fixed
// namespace and release name, when it has any, equal the typed ones.
func (v indexVersion) installable(namespace, release string) bool {
	if v.hidden() || namesAChart(v.Annotations[annotationAutoInstall]) || namesAChart(v.Annotations[annotationAutoInstallGVR]) {
		return false
	}
	if want := v.Annotations[annotationNamespace]; want != "" && namespace != "" && want != namespace {
		return false
	}
	if want := v.Annotations[annotationReleaseName]; want != "" && release != "" && want != release {
		return false
	}
	return true
}

// repoIndexOf reads the index of the typed --repo on the typed cluster: first
// the ClusterRepo, to leave a disabled repository out, then its filtered
// index. It reports false when anything fails or the repository is disabled.
func repoIndexOf(ctx context.Context, conn *completion.Connection, req completion.Request) (repoIndex, bool) {
	if !clusterRepoEnabled(ctx, conn, req.ClusterID, completion.TailFlag(req.Tail, "repo")) {
		return repoIndex{}, false
	}
	return readRepoIndex(ctx, conn, req.ClusterID, completion.TailFlag(req.Tail, "repo"))
}

// clusterRepoEnabled reads the ClusterRepo repo of the cluster and reports whether
// it exists and is not disabled (a missing spec.enabled counts as enabled).
// It is one request, none for an empty cluster id or an invalid name.
func clusterRepoEnabled(ctx context.Context, conn *completion.Connection, clusterID, repo string) bool {
	k, known := kinds.ByKey("clusterrepo")
	if !known || clusterID == "" || repo == "" {
		return false
	}
	obj, err := completion.NamedResourcePath(clusterID, k.GVR.Group, k.GVR.Version, "", k.GVR.Resource, repo)
	if err != nil {
		return false
	}
	var repoObj struct {
		Spec struct {
			Enabled *bool `json:"enabled"`
		} `json:"spec"`
	}
	if err := conn.GetJSON(ctx, obj, "", &repoObj); err != nil {
		return false
	}
	return repoObj.Spec.Enabled == nil || *repoObj.Spec.Enabled
}

// readRepoIndex reads the filtered index of the ClusterRepo repo on the
// cluster, one request. It reports false when the read fails.
func readRepoIndex(ctx context.Context, conn *completion.Connection, clusterID, repo string) (repoIndex, bool) {
	p, err := completion.ClusterRepoIndexPath(clusterID, repo)
	if err != nil {
		return repoIndex{}, false
	}
	var idx repoIndex
	if err := conn.GetJSON(ctx, p, "", &idx); err != nil {
		return repoIndex{}, false
	}
	return idx, true
}

// chartListing is the declaration of the --chart and --version sources: the
// typed cluster resolved, any credential (the reads go through the cluster's
// proxy), and two requests: the ClusterRepo, then its index.
func chartListing() completion.Requirements {
	return completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceTwo}
}

// ChartSource is the live source of the --chart flag of app install: the
// chart names of the filtered index of the typed --repo that the command
// would not refuse for what the index shows. A chart is offered when it has a
// version that is not deprecated and every such version is installable (not
// hidden, no auto-install or auto-install-gvr annotation, and a fixed
// catalog.cattle.io/namespace or release-name equal to the typed --namespace
// or <release>, when those are typed); with --version typed, when that exact
// version is installable. Nothing without --cluster and --repo, and nothing
// for a disabled repository. It claims no other site.
func ChartSource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command || req.Flag != "chart" {
				return completion.Requirements{}, false
			}
			return chartListing(), true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			idx, ok := repoIndexOf(ctx, conn, req)
			if !ok {
				return completion.Result{}
			}
			version := completion.TailFlag(req.Tail, "version")
			namespace := completion.TailFlag(req.Tail, "namespace")
			release := ""
			if len(req.Positionals) > 0 {
				release = req.Positionals[0]
			}
			var names []string
			for name, versions := range idx.Entries {
				if offered(versions, version, namespace, release) {
					names = append(names, name)
				}
			}
			return completion.Result{Names: names}
		},
	}
}

// offered reports whether a chart with these versions is offered: with a
// version typed, that exact version is installable; otherwise the chart has a
// version that is not deprecated and every such version is installable.
func offered(versions []indexVersion, typed, namespace, release string) bool {
	if typed != "" {
		for _, v := range versions {
			if v.Version == typed {
				return v.installable(namespace, release)
			}
		}
		return false
	}
	usable := 0
	for _, v := range versions {
		if v.deprecated() {
			continue
		}
		if !v.installable(namespace, release) {
			return false
		}
		usable++
	}
	return usable > 0
}

// VersionSource is the live source of the --version flag of app install: the
// versions of the typed --chart in the filtered index of the typed --repo, in
// the order Rancher serves them, deprecated ones left out and prereleases
// kept in place. Nothing without --cluster, --repo and --chart, and nothing
// for a disabled repository. It claims no other site.
func VersionSource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command || req.Flag != "version" {
				return completion.Requirements{}, false
			}
			return chartListing(), true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			chart := completion.TailFlag(req.Tail, "chart")
			if chart == "" {
				return completion.Result{}
			}
			idx, ok := repoIndexOf(ctx, conn, req)
			if !ok {
				return completion.Result{}
			}
			var names []string
			for _, v := range idx.Entries[chart] {
				if v.Version != "" && !v.deprecated() {
					names = append(names, v.Version)
				}
			}
			return completion.Result{Names: names}
		},
	}
}

// InstallSource is the live source of app install, whose registered name is
// command: --repo, --chart and --version, each answered by its own source,
// and nothing else.
func InstallSource(command string) completion.Source {
	repo, chart, version := ClusterRepoSourceFor(command), ChartSource(command), VersionSource(command)
	pick := func(req completion.Request) (completion.Source, bool) {
		switch req.Flag {
		case "repo":
			return repo, true
		case "chart":
			return chart, true
		case "version":
			return version, true
		}
		return completion.Source{}, false
	}
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			src, ok := pick(req)
			if !ok {
				return completion.Requirements{}, false
			}
			return src.Claim(req)
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			src, ok := pick(req)
			if !ok {
				return completion.Result{}
			}
			return src.Complete(ctx, conn, req)
		},
	}
}
