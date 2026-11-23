package complete

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const (
	clusterReposResource = "catalog.cattle.io.clusterrepos"
	clustersResource     = "management.cattle.io.clusters"
	hiddenRepoAnnotation = "clusterrepo.cattle.io/hidden"
	repoStateActive      = "active"
	flagRepo             = "repo"
	flagVersion          = "version"
)

// indexVersion and repoIndex are the parts of a ClusterRepo's index the
// source reads.
type indexVersion struct {
	Version     string            `json:"version"`
	Annotations map[string]string `json:"annotations"`
}

type repoIndex struct {
	Entries map[string][]indexVersion `json:"entries"`
}

// annotationsOf returns the annotations of each version of a chart.
func annotationsOf(versions []indexVersion) []map[string]string {
	out := make([]map[string]string, len(versions))
	for i, v := range versions {
		out[i] = v.Annotations
	}
	return out
}

// installRepo is a ClusterRepo as the Rancher UI reads it.
type installRepo struct {
	name   string
	reads  bool
	active bool
}

// installState is everything the source reads before it decides what the
// command accepts.
type installState struct {
	inputs    extension.Inputs
	repos     []installRepo
	installed map[string]bool
	// indexes has the index of each repository the Rancher UI reads that
	// responded.
	indexes map[string]repoIndex
}

// ExtensionInstallSource is the live source of `extension install`, whose
// registered name is command: the chart name, --repo and --version, each only
// with a value the command accepts. A chart is offered when one repository
// the Rancher UI reads offers it as an extension (or the typed --repo does),
// it installs one extension, it has a version that passes the checks of the
// Rancher UI, and neither an App nor a UIPlugin of its name exists. --repo
// offers the repositories where the typed chart is accepted, and --version
// the versions of the chart that pass the checks, in the repository --repo
// names or the only one that offers it; --repo and --version offer nothing
// until the chart is typed. On a server where the Rancher UI reads more
// ClusterRepos than completion.ExtensionRepositoryLimit, the source offers
// nothing, unless --repo is already typed; then only the index of that
// repository is read. It claims no other site.
func ExtensionInstallSource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command {
				return completion.Requirements{}, false
			}
			switch {
			case req.Flag == "" && req.Index == 0:
			case (req.Flag == flagRepo || req.Flag == flagVersion) && len(req.Positionals) > 0:
			default:
				return completion.Requirements{}, false
			}
			return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceExtension}, true
		},
		Complete: completeExtensionInstall,
	}
}

func completeExtensionInstall(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	typedRepo := completion.TailFlag(req.Tail, flagRepo)
	if req.Flag == flagRepo {
		typedRepo = ""
	}
	state, ok := readInstallState(ctx, conn, typedRepo)
	if !ok {
		return completion.Result{}
	}
	switch req.Flag {
	case "":
		return completion.Result{Names: state.chartNames(typedRepo)}
	case flagRepo:
		return completion.Result{Names: state.reposOffering(req.Positionals[0])}
	}
	return completion.Result{Names: state.versionsOf(req.Positionals[0], typedRepo)}
}

// readInstallState reads what the command reads: the version document, the
// local Cluster, the ClusterRepos, the UIPlugins and the Apps, then the index
// of the typed repository or of each repository the Rancher UI reads. An
// unreadable version document or Cluster leaves its inputs empty, as for the
// command; any other failed read, a repository the command refuses and a
// context that ended during the reads report false.
func readInstallState(ctx context.Context, conn *completion.Connection, typedRepo string) (installState, bool) {
	var st installState
	st.inputs = readInputs(ctx, conn)
	var ok bool
	if st.repos, ok = readInstallRepos(ctx, conn); !ok {
		return installState{}, false
	}
	if st.installed, ok = readInstalledNames(ctx, conn); !ok {
		return installState{}, false
	}
	st.indexes = map[string]repoIndex{}
	if typedRepo != "" {
		i := slices.IndexFunc(st.repos, func(r installRepo) bool { return r.name == typedRepo })
		if i < 0 || !st.repos[i].reads || !st.repos[i].active {
			return installState{}, false
		}
		idx, ok := readRepoIndex(ctx, conn, typedRepo)
		if !ok {
			return installState{}, false
		}
		st.indexes[typedRepo] = idx
		return st, true
	}
	var read []string
	for _, r := range st.repos {
		if r.reads {
			read = append(read, r.name)
		}
	}
	if len(read) > completion.ExtensionRepositoryLimit {
		return installState{}, false
	}
	for _, r := range st.repos {
		if !r.reads || !r.active {
			continue
		}
		if idx, ok := readRepoIndex(ctx, conn, r.name); ok {
			st.indexes[r.name] = idx
		}
	}
	if ctx.Err() != nil {
		return installState{}, false
	}
	return st, true
}

// readInputs reads the compatibility inputs: the version document and the
// local Cluster. An unreadable one leaves its inputs empty, as for the
// commands.
func readInputs(ctx context.Context, conn *completion.Connection) extension.Inputs {
	doc := readServerDocument(ctx, conn)
	in := extension.Inputs{RancherVersion: doc.Version, Edition: extension.ParseEdition(doc.RancherPrime), KubernetesVersion: readKubernetesVersion(ctx, conn)}
	if api, known := extension.ExtensionsAPIVersion(doc.Version); known {
		in.ExtensionsAPIVersion = api
	}
	return in
}

// readServerDocument reads /rancherversion; a failed read is the empty
// document.
func readServerDocument(ctx context.Context, conn *completion.Connection) versionDocument {
	var doc versionDocument
	if p, err := completion.RancherVersionPath(); err == nil {
		if err := conn.GetJSON(ctx, p, "", &doc); err != nil {
			return versionDocument{}
		}
	}
	return doc
}

// readKubernetesVersion reads the Kubernetes version of the local cluster as
// the Rancher UI compares it; "" when the read fails or has none.
func readKubernetesVersion(ctx context.Context, conn *completion.Connection) string {
	p, err := completion.SteveObjectPath(clustersResource, "local")
	if err != nil {
		return ""
	}
	var cluster struct {
		Status struct {
			Info struct {
				KubernetesVersion string `json:"kubernetesVersion"`
			} `json:"info"`
			Version struct {
				GitVersion string `json:"gitVersion"`
			} `json:"version"`
		} `json:"status"`
	}
	if err := conn.GetJSON(ctx, p, "", &cluster); err != nil {
		return ""
	}
	v, _ := extension.KubernetesVersion(cluster.Status.Info.KubernetesVersion, cluster.Status.Version.GitVersion)
	return v
}

// readInstallRepos reads the ClusterRepos of the local cluster through Steve,
// in name order.
func readInstallRepos(ctx context.Context, conn *completion.Connection) ([]installRepo, bool) {
	p, err := completion.SteveCollectionPath(clusterReposResource)
	if err != nil {
		return nil, false
	}
	var list struct {
		Data []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"metadata"`
			Spec struct {
				Enabled *bool `json:"enabled"`
			} `json:"spec"`
		} `json:"data"`
	}
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, false
	}
	repos := make([]installRepo, 0, len(list.Data))
	for _, it := range list.Data {
		if it.Metadata.Name == "" {
			return nil, false
		}
		repos = append(repos, installRepo{
			name:   it.Metadata.Name,
			reads:  it.Metadata.Annotations[hiddenRepoAnnotation] != "true" && (it.Spec.Enabled == nil || *it.Spec.Enabled),
			active: it.Metadata.State.Name == repoStateActive,
		})
	}
	slices.SortFunc(repos, func(a, b installRepo) int { return strings.Compare(a.name, b.name) })
	return repos, true
}

// readInstalledNames reads the names of the Apps of the extension namespace
// and the plugin names of the UIPlugins of every namespace: a chart with
// either is installed or loaded.
func readInstalledNames(ctx context.Context, conn *completion.Connection) (map[string]bool, bool) {
	plugins, err := completion.ClusterPath("local", "catalog.cattle.io", "v1", "uiplugins")
	if err != nil {
		return nil, false
	}
	apps, err := completion.NamespacedPath("local", "catalog.cattle.io", "v1", extension.Namespace, "apps")
	if err != nil {
		return nil, false
	}
	var pluginList struct {
		Items []struct {
			Spec struct {
				Plugin struct {
					Name string `json:"name"`
				} `json:"plugin"`
			} `json:"spec"`
		} `json:"items"`
	}
	var appList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := conn.GetJSON(ctx, plugins, "", &pluginList); err != nil {
		return nil, false
	}
	if err := conn.GetJSON(ctx, apps, "", &appList); err != nil {
		return nil, false
	}
	names := map[string]bool{}
	for _, it := range pluginList.Items {
		names[it.Spec.Plugin.Name] = true
	}
	for _, it := range appList.Items {
		names[it.Metadata.Name] = true
	}
	return names, true
}

// readRepoIndex reads the index of one ClusterRepo.
func readRepoIndex(ctx context.Context, conn *completion.Connection, repo string) (repoIndex, bool) {
	p, err := completion.ClusterRepoIndexPath("local", repo)
	if err != nil {
		return repoIndex{}, false
	}
	var idx repoIndex
	if err := conn.GetJSON(ctx, p, "", &idx); err != nil {
		return repoIndex{}, false
	}
	return idx, true
}

// extensionVersions returns the versions of an extension chart the Rancher UI
// lists, or nil when the chart is absent, not an extension or hidden.
func extensionVersions(idx repoIndex, chart string) []indexVersion {
	versions := idx.Entries[chart]
	annotations := annotationsOf(versions)
	if len(versions) == 0 || !extension.IsExtensionChart(annotations) || extension.IsHiddenExtension(annotations) {
		return nil
	}
	return versions
}

// installableVersions returns the versions of the chart in the repository that
// pass the checks of the Rancher UI, or nil when the chart cannot be
// installed from it: not an extension, a bundle, or installed already.
func (s installState) installableVersions(repo, chart string) []string {
	if s.installed[chart] {
		return nil
	}
	versions := extensionVersions(s.indexes[repo], chart)
	if versions == nil || extension.IsBundleChart(annotationsOf(versions)) {
		return nil
	}
	var out []string
	for _, v := range versions {
		if extension.Compatibility(v.Annotations, s.inputs).Installable {
			out = append(out, v.Version)
		}
	}
	return out
}

// offering returns the repositories whose index lists the chart as an
// extension, bundles and incompatible charts included, as the command counts
// them.
func (s installState) offering(chart string) []string {
	var out []string
	for _, r := range s.repos {
		if extensionVersions(s.indexes[r.name], chart) != nil {
			out = append(out, r.name)
		}
	}
	return out
}

// chartNames returns the chart names the command accepts without --repo, or
// with the typed repository.
func (s installState) chartNames(typedRepo string) []string {
	var names []string
	for _, repo := range slices.Sorted(maps.Keys(s.indexes)) {
		for chart := range s.indexes[repo].Entries {
			if slices.Contains(names, chart) {
				continue
			}
			if typedRepo == "" && len(s.offering(chart)) != 1 {
				continue
			}
			if len(s.installableVersions(repo, chart)) > 0 {
				names = append(names, chart)
			}
		}
	}
	return names
}

// reposOffering returns the repositories --repo accepts for the chart.
func (s installState) reposOffering(chart string) []string {
	var names []string
	for _, repo := range s.offering(chart) {
		if len(s.installableVersions(repo, chart)) > 0 {
			names = append(names, repo)
		}
	}
	return names
}

// versionsOf returns the versions --version accepts for the chart: from the
// typed repository, else from the only repository that offers the chart.
func (s installState) versionsOf(chart, typedRepo string) []string {
	repo := typedRepo
	if repo == "" {
		offering := s.offering(chart)
		if len(offering) != 1 {
			return nil
		}
		repo = offering[0]
	}
	return s.installableVersions(repo, chart)
}
