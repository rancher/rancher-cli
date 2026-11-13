package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// chartPlan is what app install and app upgrade share about the chart a
// request sends: where it comes from, the release it is for, and the values
// the checks and the composition read.
type chartPlan struct {
	client  engine.ClusterClient
	root    engine.RootClient
	cluster string
	target  actionTarget
	repo    string
	chart   string
	version string
	// values are the values the composition starts from: the --values files
	// merged for an install, the release's values with the files merged over
	// them for an upgrade.
	values Values
	// files are the --values files alone, for an upgrade, whose values start
	// from the release's values: a message about a path names where it came
	// from.
	files Values
	// current is the chart version an upgrade starts from; an install has
	// none. It selects the wording of the refusals that name a way out.
	current string
}

// installCommand is the whole command that installs chart as a new release:
// the release name is the reader's to choose. An empty repo stands for a
// repository the reader names.
func installCommand(typedCluster, namespace, repo, chart string) clierr.Command {
	if repo == "" {
		return clierr.NewCommand("app install").Placeholder("<release>").
			FlagValue("--cluster", typedCluster).FlagValue("--namespace", namespace).
			FlagText(" --repo <repo>").FlagValue("--chart", chart).Flag("--dry-run")
	}
	return clierr.NewCommand("app install").Placeholder("<release>").
		FlagValue("--cluster", typedCluster).FlagValue("--namespace", namespace).
		FlagValue("--repo", repo).FlagValue("--chart", chart).Flag("--dry-run")
}

// chartsHint is the command that lists the repository's charts, with the
// flags it is given.
func (p chartPlan) chartsHint(flags ...clierr.CommandWord) clierr.Command {
	c := clierr.NewCommand("charts").FlagValue("--cluster", p.target.typedCluster).FlagValue("--repo", p.repo)
	for _, f := range flags {
		c = c.Flag(f)
	}
	return c
}

// checkChart applies the refusals the chart's annotations call for, and
// returns the warnings of the checks it could not make. mgmt is the
// management Cluster, nil when the caller cannot read it. kept is true for
// the installed version of an upgrade's release: the release is in that
// namespace under that name, so the namespace and release-name annotations
// are not checked.
func (p chartPlan) checkChart(ctx context.Context, entry ChartVersion, info Info, repo, mgmt *unstructured.Unstructured, kept bool) ([]string, error) {
	name := fmt.Sprintf("chart %s %s", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version))
	upgrade := p.current != ""
	// The Rancher UI reads the annotations of the index entry; the info
	// link's chart metadata stands in only for an entry that carries none.
	ann := entry.Annotations
	if len(ann) == 0 {
		ann = info.Chart.Annotations
	}
	if first := autoInstallCharts(ann); len(first) > 0 {
		if upgrade {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"%s names %s to install first (annotation %s); rancher-cli does not upgrade a chart that names another chart to install first; upgrade it from the Rancher UI, which upgrades the chart it names too",
				name, strings.Join(first, ", "), firstAnnotation(ann)))
		}
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"%s names %s to install first (annotation %s); rancher-cli does not install a chart that names another chart to install first; install it from the Rancher UI",
			name, strings.Join(first, ", "), firstAnnotation(ann)))
	}
	if want := ann[annotationNamespace]; want != "" && want != p.target.namespace && !kept {
		if upgrade {
			return nil, clierr.New(clierr.CodeValidation, clierr.Text(fmt.Sprintf(
				"%s installs only in namespace %s (annotation %s), and release %s is in namespace %s; stay on the installed version %s, or install %[1]s as a new release in %[7]s",
				name, clierr.ShellQuote(want), annotationNamespace, clierr.ShellQuote(p.target.release), clierr.ShellQuote(p.target.namespace),
				clierr.ShellQuote(p.current), clierr.ShellQuote(want)),
				clierr.Lead(" with "), installCommand(p.target.typedCluster, want, p.repo, p.chart)))
		}
		msg := fmt.Sprintf("%s installs only in namespace %s (annotation %s)", name, clierr.ShellQuote(want), annotationNamespace)
		if fragment, ok := clierr.FlagFragment("--namespace", want); ok {
			msg += "; run it with " + fragment
		} else {
			msg = clierr.WithLeftOutNote(msg)
		}
		return nil, clierr.New(clierr.CodeValidation, msg)
	}
	if want := ann[annotationReleaseName]; want != "" && want != p.target.release && !kept {
		if upgrade {
			return nil, clierr.New(clierr.CodeValidation, clierr.Text(fmt.Sprintf(
				"%s installs only as release %s (annotation %s); stay on the installed version %s, or install %[1]s as release %[5]s",
				name, clierr.ShellQuote(want), annotationReleaseName, clierr.ShellQuote(p.current), clierr.ShellQuote(want)),
				clierr.Lead(" with "), installCommand(p.target.typedCluster, p.target.namespace, p.repo, p.chart)))
		}
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"%s installs only as release %s (annotation %s); run it with that release name",
			name, clierr.ShellQuote(want), annotationReleaseName))
	}
	var warnings []string
	if entries := trimmedList(ann[annotationRequiresGVR]); len(entries) > 0 {
		w, err := p.checkRequires(ctx, name, entries)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, w...)
	}
	w, err := p.checkPermitsOS(name, ann, repo, mgmt)
	if err != nil {
		return nil, err
	}
	return append(warnings, w...), nil
}

// checkRequires refuses the chart when the cluster serves no resource for
// an entry of catalog.cattle.io/requires-gvr: <schema id>[/<version>],
// where the schema id is the lowercased <group>.<kind>, or the lowercased
// kind of the core group, and the version is compared with the group's
// preferred version, as the Rancher UI compares it.
func (p chartPlan) checkRequires(ctx context.Context, name string, entries []string) ([]string, error) {
	resources, err := p.client.ServerResources(ctx)
	incomplete, partial := errorsAsIncomplete(err)
	if err != nil && !partial {
		return nil, err
	}
	var warnings []string
	for _, e := range entries {
		id, version, _ := strings.Cut(e, "/")
		id = strings.ToLower(id)
		if served(resources, id, version) {
			continue
		}
		if partial && incomplete.hasGroupOf(id) {
			warnings = append(warnings, fmt.Sprintf("the cluster could not list every API group; %s needs %s (annotation %s), which was not checked", name, e, annotationRequiresGVR))
			continue
		}
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"%s needs the resource %s on the cluster (annotation %s), and the cluster does not serve it", name, e, annotationRequiresGVR))
	}
	return warnings, nil
}

// checkPermitsOS refuses the chart when the cluster has workers of an OS
// that catalog.cattle.io/permits-os does not list. Without the annotation a
// chart from a Rancher repository permits linux only and any other chart
// every OS. The workers' OSes come from the management cluster mgmt, nil
// when the caller cannot read it: the check is then skipped with a warning.
func (p chartPlan) checkPermitsOS(name string, ann map[string]string, repo, mgmt *unstructured.Unstructured) ([]string, error) {
	var permitted []string
	if v, ok := ann[annotationPermitsOS]; ok && v != "" {
		for e := range strings.SplitSeq(v, ",") {
			if e != "" {
				permitted = append(permitted, e)
			}
		}
	} else if rancherSource(repo) {
		permitted = []string{"linux"}
	}
	if len(permitted) == 0 {
		return nil, nil
	}
	if mgmt == nil {
		return []string{fmt.Sprintf("cannot read cluster %s; %s permits only %s nodes (annotation %s), which was not checked against its workers",
			clierr.ShellQuote(p.target.typedCluster), name, strings.Join(permitted, ", "), annotationPermitsOS)}, nil
	}
	for _, workerOS := range workerOSes(mgmt) {
		if !slices.Contains(permitted, workerOS) {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"%s permits only %s nodes (annotation %s), and cluster %s has %s workers",
				name, strings.Join(permitted, ", "), annotationPermitsOS, clierr.ShellQuote(p.target.typedCluster), workerOS))
		}
	}
	return nil, nil
}

// checkRegistryFlags refuses a registry flag that cannot apply: either flag
// on a chart whose default values declare neither registry path, and
// --system-default-registry where the values files set a parent of a
// registry path to anything but a mapping.
func (p chartPlan) checkRegistryFlags(reg registryFlags, info Info) error {
	flag := ""
	switch {
	case reg.host != "":
		flag = flagSystemDefaultRegistry
	case reg.none:
		flag = flagNoSystemDefaultRegistry
	default:
		return nil
	}
	if !declaresRegistry(info.Values) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"chart %s %s declares neither global.cattle.systemDefaultRegistry nor global.systemDefaultRegistry in its default values; --%s applies only to a chart that declares one; run the command without it",
			clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version), flag))
	}
	if reg.host == "" {
		return nil
	}
	for _, path := range registryPaths {
		if st, parent := p.values.state(path); st == pathBlocked {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"--%s cannot set %s: %s set %s to a value that is not a mapping",
				flagSystemDefaultRegistry, path, p.sourceOf(path), parent))
		}
	}
	return nil
}

// composeValues adds the cluster values and the registry values to the
// files' values. cluster is the management Cluster, nil when the caller
// cannot read it. A source the caller cannot read leaves its values out
// with a warning; a Setting or the project list that is not found counts
// as "".
func (p chartPlan) composeValues(ctx context.Context, defaults map[string]any, cluster *unstructured.Unstructured, reg registryFlags) (composition, error) {
	src := clusterSources{id: p.cluster, cluster: cluster}
	var rows []clusterValue
	need := map[valueSource]bool{}
	for _, row := range clusterValues {
		if st, _ := p.values.state(row.path); st == pathAbsent {
			rows = append(rows, row)
			need[row.source] = true
		}
	}
	denied := map[valueSource]bool{sourceCluster: cluster == nil}
	var err error
	if need[sourceServerURL] {
		if src.serverURL, denied[sourceServerURL], err = readSetting(ctx, p.root, settingServerURL); err != nil {
			return composition{}, err
		}
	}
	if need[sourceProjects] {
		if src.systemProject, denied[sourceProjects], err = systemProject(ctx, p.root, p.cluster); err != nil {
			return composition{}, err
		}
	}
	var add []addedValue
	notSent := map[valueSource][]string{}
	for _, row := range rows {
		if denied[row.source] {
			notSent[row.source] = append(notSent[row.source], row.path.String())
			continue
		}
		if v, ok := row.value(src); ok {
			add = append(add, addedValue{path: row.path, value: v})
		}
	}

	var drop []valuePath
	clusterRegistryNotRead, settingDenied := false, false
	switch {
	case reg.none:
		drop = registryPaths
	case reg.host != "":
		add = append(add, registryValues(reg.host)...)
	case declaresRegistry(defaults) && !p.registryBlocked():
		host := ""
		for _, path := range registryPaths {
			if host = p.values.mergedString(defaults, path); host != "" {
				break
			}
		}
		if host == "" && cluster == nil {
			clusterRegistryNotRead = true
		}
		if host == "" && cluster != nil {
			host = clusterRegistry(cluster)
		}
		if host == "" {
			if host, settingDenied, err = readSetting(ctx, p.root, settingSystemDefaultRegistry); err != nil {
				return composition{}, err
			}
		}
		add = append(add, registryValues(host)...)
	}

	sent, added, dropped := p.values.compose(add, drop)
	c := composition{sent: sent, added: added}
	if declaresRegistry(defaults) {
		c.shown = registryPaths
	}
	if len(added) > 0 {
		c.warnings = append(c.warnings, "rancher-cli adds the values the Rancher UI adds when it installs or upgrades a chart: "+strings.Join(added, ", "))
	}
	typed := clierr.ShellQuote(p.target.typedCluster)
	if paths := notSent[sourceCluster]; len(paths) > 0 || clusterRegistryNotRead {
		w := "cannot read cluster " + typed + "; "
		if len(paths) > 0 {
			w += strings.Join(paths, ", ") + notSentVerb(paths)
			if clusterRegistryNotRead {
				w += ", and "
			}
		}
		if clusterRegistryNotRead {
			w += "the cluster's registry was not read"
		}
		c.warnings = append(c.warnings, w)
	}
	if paths := notSent[sourceServerURL]; len(paths) > 0 {
		c.warnings = append(c.warnings, "cannot read the server-url setting; "+strings.Join(paths, ", ")+notSentVerb(paths))
	}
	if paths := notSent[sourceProjects]; len(paths) > 0 {
		c.warnings = append(c.warnings, "cannot list the projects of cluster "+typed+"; "+strings.Join(paths, ", ")+notSentVerb(paths))
	}
	if settingDenied {
		c.warnings = append(c.warnings, "cannot read the system-default-registry setting; the registry values are sent empty; name a registry with --"+flagSystemDefaultRegistry)
	}
	if len(dropped) > 0 {
		c.warnings = append(c.warnings, "--"+flagNoSystemDefaultRegistry+" leaves out "+p.droppedText(dropped))
	}
	return c, nil
}

// sourceOf names where the value at path came from: the values files when one
// holds it (or a parent of it), else, for an upgrade, the release's values.
func (p chartPlan) sourceOf(path valuePath) string {
	if p.current == "" {
		return "the values files"
	}
	if st, _ := p.files.state(path); st != pathAbsent {
		return "the values files"
	}
	return "the release's values"
}

// droppedText lists the paths --no-system-default-registry left out, each
// group with the source it was set in.
func (p chartPlan) droppedText(dropped []string) string {
	var fromFiles, fromStored []string
	for _, path := range registryPaths {
		if !slices.Contains(dropped, path.String()) {
			continue
		}
		if p.sourceOf(path) == "the values files" {
			fromFiles = append(fromFiles, path.String())
		} else {
			fromStored = append(fromStored, path.String())
		}
	}
	var clauses []string
	if len(fromFiles) > 0 {
		clauses = append(clauses, strings.Join(fromFiles, ", ")+" set in the values files")
	}
	if len(fromStored) > 0 {
		clauses = append(clauses, strings.Join(fromStored, ", ")+" set in the release's values")
	}
	return strings.Join(clauses, " and ")
}

// registryBlocked reports whether the values files set a parent of either
// registry path to anything but a mapping: the CLI then adds neither.
func (p chartPlan) registryBlocked() bool {
	for _, path := range registryPaths {
		if st, _ := p.values.state(path); st == pathBlocked {
			return true
		}
	}
	return false
}

// recheckIndex reads the filtered index again before the request and compares
// the digest of the target version with the preview's: a version that left the
// index or a content that changed is a retryable conflict, and an index that
// gives no digest is a warning.
func (p chartPlan) recheckIndex(ctx context.Context, digest string) ([]string, error) {
	idx, err := ReadIndex(ctx, p.root, p.cluster, p.repo)
	if err != nil {
		return nil, changedBeforeRequest(fmt.Sprintf("the index of ClusterRepo %s could not be read again (%s)",
			clierr.ShellQuote(p.repo), clierr.From(err).Message))
	}
	i := slices.IndexFunc(idx.Entries[p.chart], func(v ChartVersion) bool { return v.Version == p.version })
	if i < 0 {
		return nil, changedBeforeRequest(fmt.Sprintf("version %s of chart %s left the index", clierr.ShellQuote(p.version), clierr.ShellQuote(p.chart)))
	}
	now := idx.Entries[p.chart][i].Digest
	switch {
	case digest != "" && now != "" && digest != now:
		return nil, changedBeforeRequest(fmt.Sprintf("the content of chart %s %s changed in the index", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version)))
	case digest == "" || now == "":
		return []string{fmt.Sprintf("the index gives no digest for chart %s %s; its content was not compared with the preview's", p.chart, p.version)}, nil
	}
	return nil, nil
}
