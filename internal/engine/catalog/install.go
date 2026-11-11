package catalog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The flags of app install that app uninstall does not take.
const (
	flagChart                    = "chart"
	flagVersion                  = "version"
	flagHelmWait                 = "helm-wait"
	flagDisableOpenAPIValidation = "disable-openapi-validation"
	flagDescription              = "description"
)

// The chart annotations app install enforces. Rancher's server reads none
// of them; the Rancher UI does, and so does this command.
const (
	annotationNamespace      = "catalog.cattle.io/namespace"
	annotationReleaseName    = "catalog.cattle.io/release-name"
	annotationRequiresGVR    = "catalog.cattle.io/requires-gvr"
	annotationPermitsOS      = "catalog.cattle.io/permits-os"
	annotationAutoInstall    = "catalog.cattle.io/auto-install"
	annotationAutoInstallGVR = "catalog.cattle.io/auto-install-gvr"
)

// maxReleaseName and releaseNameText are Helm's rule for a release name.
const maxReleaseName = 53

var releaseNameText = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// validateReleaseName refuses a name Helm refuses.
func validateReleaseName(name string) error {
	if len(name) > maxReleaseName || !releaseNameText.MatchString(name) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"release name %s is not valid: Helm takes at most 53 characters of lowercase letters, digits, \"-\" and \".\", "+
				"each part between dots starting and ending with a letter or digit", clierr.ShellQuote(name)))
	}
	return nil
}

type installCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	actions engine.ActionClientFactory
	src     engine.FileSource
	now     func() time.Time
	newID   func() string
}

// NewInstall builds `app install <release> --cluster <c> --namespace <ns>
// --repo <r> --chart <chart>`: a chart install through the install action
// of a ClusterRepo, previewed from server reads.
func NewInstall(clients engine.ClusterClientFactory, roots engine.RootClientFactory, actions engine.ActionClientFactory, src engine.FileSource) runtime.Command {
	return &installCmd{clients: clients, roots: roots, actions: actions, src: src, now: time.Now, newID: randomID}
}

// randomID is a new request id: 16 random bytes in hex.
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (*installCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "app install",
		Usage: "Install a chart from a ClusterRepo as a Helm release",
		Desc: "Asks Rancher to install --chart from the ClusterRepo --repo on --cluster as the Helm release <release> in --namespace, " +
			"through the ClusterRepo's install action: Rancher fetches the chart and runs Helm in a pod, and records the release as an App. " +
			"The server has no dry-run for this action; the preview comes from server reads (previewSource serverReads): " +
			"the ClusterRepo (it must be enabled), the chart and version in the index Rancher serves for the cluster, " +
			"the chart's default values and annotations, the App (it must not exist), the operations of the namespace " +
			"(none may still be running for the release) and the namespace (it must exist). " +
			"It carries no resourceVersion precondition: --apply reads the App, the operations and the index entry again " +
			"before it sends the request, and nothing checks for a change after that. " +
			"--version names one exact version from the index; without it the version is the one `rancher charts` shows. " +
			"Values come only from files (--values, repeatable, merged in order; - reads stdin once); " +
			"the preview lists their paths, never the values. " +
			"Neither Rancher nor rancher-cli checks the values against the chart's values schema (Helm does, in the pod) or the chart's signature. " +
			"Refused: a chart whose default values declare global.cattle (install it from the Rancher UI), " +
			"a chart that names another chart to install first (catalog.cattle.io/auto-install or auto-install-gvr), " +
			"and a chart whose catalog.cattle.io/namespace, release-name, requires-gvr or permits-os annotation the target does not meet. " +
			"--helm-wait (default true; --helm-wait=false sends false) makes Helm wait for the chart's resources; " +
			"--helm-timeout (default 10m, at least 1s, at most 15m) is Helm's timeout; the chart's hooks can make a run longer, and Rancher removes a run's access one hour after the operation starts. " +
			"The command returns once Rancher accepts the request, with the operation's name and the commands that follow it.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "release", Type: "string", Required: true, Desc: "Helm release name"},
		},
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, true),
			withDesc(engine.NamespaceFlag(true), "`namespace` of the release; it must exist"),
			{Name: flagRepo, Type: "string", Required: true, Desc: "ClusterRepo `name` to install from"},
			{Name: flagChart, Type: "string", Required: true, Desc: "chart `name` in the repository"},
			{Name: flagVersion, Type: "string", Desc: "exact chart `version` (default: the version `rancher charts` shows)"},
			{Name: flagValues, Type: "stringSlice", Desc: "values `file`, repeatable, merged in order; - reads stdin"},
			{Name: flagHelmWait, Type: "bool", Desc: "Helm waits for the chart's resources (default true; --helm-wait=false sends false)"},
			{Name: flagHelmTimeout, Type: "string", Desc: "Helm `timeout` for each step, 1s to 15m (default 10m)"},
			{Name: flagNoHooks, Type: "bool", Desc: "do not run the chart's hooks"},
			{Name: flagDisableOpenAPIValidation, Type: "bool", Desc: "do not validate the rendered resources against the OpenAPI schema"},
			{Name: flagDescription, Type: "string", Desc: "release `description`"},
		},
		Examples: []string{
			"rancher app install my-nginx --cluster prod --namespace web --repo bitnami --chart nginx --values values.yaml --dry-run",
			"rancher app install my-nginx --cluster prod --namespace web --repo bitnami --chart nginx --values values.yaml --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func withDesc(a runtime.ArgSpec, desc string) runtime.ArgSpec {
	a.Desc = desc
	return a
}

// stdinTerminal reads the pipeline's stdin flag; an Input that does not
// carry it is not a terminal.
func stdinTerminal(in runtime.Input) bool {
	si, ok := in.(runtime.StdinInput)
	return ok && si.StdinTerminal()
}

// installPlan is what Plan read and decided, for Execute.
type installPlan struct {
	client  engine.ClusterClient
	root    engine.RootClient
	cluster string
	target  actionTarget
	repo    string
	chart   string
	version string
	digest  string
	body    installBody
	values  Values
	canRead bool
}

func (c *installCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	release := in.Args()[0]
	if err := validateReleaseName(release); err != nil {
		return safety.Effect{}, nil, err
	}
	timeout, err := helmTimeout(in.String(flagHelmTimeout), defaultHelmTimeout)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	values, err := ReadValues(c.src, in.Strings(flagValues), stdinTerminal(in))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	p := installPlan{
		repo:   in.String(flagRepo),
		chart:  in.String(flagChart),
		values: values,
		target: actionTarget{typedCluster: in.String(engine.FlagCluster), namespace: in.String(engine.FlagNamespace), release: release, requestID: c.newID()},
	}
	var warnings []string

	if p.cluster, err = engine.ResolveDownstreamCluster(ctx, c.clients, in, true); err != nil {
		return safety.Effect{}, nil, err
	}
	if p.client, err = c.clients.Cluster(ctx, p.cluster); err != nil {
		return safety.Effect{}, nil, err
	}
	repoObj, err := p.readRepo(ctx)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if p.root, err = c.roots.Root(); err != nil {
		return safety.Effect{}, nil, err
	}

	entry, w, err := p.resolveVersion(ctx, in.String(flagVersion))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings = append(warnings, w...)
	p.version, p.digest = entry.Version, entry.Digest

	info, err := ReadInfo(ctx, p.root, p.cluster, p.repo, p.chart, p.version)
	if err != nil {
		return safety.Effect{}, nil, withRepoRemedy(err, p.target.typedCluster)
	}
	w, err = p.checkChart(ctx, entry, info, repoObj)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	warnings = append(warnings, w...)

	if err := p.checkNoApp(ctx); err != nil {
		return safety.Effect{}, nil, err
	}
	op, canList, err := runningOperation(ctx, p.client, p.target.namespace, release, c.now())
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if op != "" {
		return safety.Effect{}, nil, operationRunning(p.target, op)
	}
	if !canList {
		warnings = append(warnings, operationsNotChecked(p.target))
	}
	if err := p.checkNamespace(ctx); err != nil {
		return safety.Effect{}, nil, err
	}

	idWarning, canRead := identityOf(ctx, p.client, serviceAccountIdentity(repoObj), p.target.namespace)
	p.canRead = canRead

	p.body = installBody{
		Timeout:                  timeout,
		Wait:                     !in.IsSet(flagHelmWait) || in.Bool(flagHelmWait),
		NoHooks:                  in.Bool(flagNoHooks),
		DisableOpenAPIValidation: in.Bool(flagDisableOpenAPIValidation),
		Namespace:                p.target.namespace,
		Charts: []installChart{{
			ChartName:   p.chart,
			Version:     p.version,
			ReleaseName: release,
			Description: in.String(flagDescription),
			Values:      values.request(),
			Annotations: map[string]string{
				annotationRequestID:      p.target.requestID,
				annotationSourceRepoType: sourceRepoTypeCluster,
				annotationSourceRepo:     p.repo,
			},
		}},
	}

	eff := safety.Effect{
		Action:            safety.ActionCreate,
		Target:            kind("app").GVR.Resource + "/" + release,
		DisplayKind:       "app",
		DisplayName:       release,
		PreviewSource:     safety.PreviewServerReads,
		Cluster:           p.cluster,
		Diff:              p.diff(),
		ConflictDetection: false,
		Reversible:        false,
		Destructive:       false,
		Warnings: append([]string{
			idWarning,
			warnServerReads,
			"uninstalling the release leaves the CRDs of the chart's crds/ directory and the resources marked helm.sh/resource-policy: keep",
		}, warnings...),
	}
	if pullSecretsWarningApplies(ctx, c.clients) {
		eff.Warnings = append(eff.Warnings, warnPullSecretsInstall)
	}
	exec := func(ctx context.Context) (runtime.Result, error) { return c.execute(ctx, p) }
	return eff, exec, nil
}

// diff lists what the request sends: the source, the release, the Helm
// options and the value paths, never a value.
func (p installPlan) diff() []safety.DiffEntry {
	d := []safety.DiffEntry{
		{Path: "repo", To: p.repo},
		{Path: "chart", To: p.chart},
		{Path: "version", To: p.version},
		{Path: "namespace", To: p.target.namespace},
		{Path: "releaseName", To: p.target.release},
		{Path: "wait", To: p.body.Wait},
		{Path: "timeout", To: p.body.Timeout},
	}
	if p.body.NoHooks {
		d = append(d, safety.DiffEntry{Path: "noHooks", To: true})
	}
	if p.body.DisableOpenAPIValidation {
		d = append(d, safety.DiffEntry{Path: "disableOpenAPIValidation", To: true})
	}
	if desc := p.body.Charts[0].Description; desc != "" {
		d = append(d, safety.DiffEntry{Path: "description", To: desc})
	}
	for _, path := range p.values.paths() {
		d = append(d, safety.DiffEntry{Path: path, To: hiddenText})
	}
	return d
}

// readRepo reads the ClusterRepo the plan installs from.
func (p installPlan) readRepo(ctx context.Context) (*unstructured.Unstructured, error) {
	return readClusterRepo(ctx, p.client, p.repo, p.target.typedCluster)
}

// readClusterRepo reads the ClusterRepo a command takes with --repo; a missing
// one names the command that lists them, and a disabled one is refused.
// typedCluster is the cluster as the user typed it.
func readClusterRepo(ctx context.Context, client engine.ClusterClient, repo, typedCluster string) (*unstructured.Unstructured, error) {
	obj, err := client.Get(ctx, kind("clusterrepo").GVR, "", repo)
	if err != nil {
		if notFound(err) {
			return nil, clierr.New(clierr.CodeNotFound, "ClusterRepo "+clierr.ShellQuote(repo)+" not found").
				WithHint(clierr.Rancher + " get clusterrepos --cluster " + clierr.ShellQuote(typedCluster))
		}
		return nil, err
	}
	if enabled, found, _ := unstructured.NestedBool(obj.Object, "spec", "enabled"); found && !enabled {
		return nil, clierr.New(clierr.CodeValidation, "ClusterRepo "+clierr.ShellQuote(repo)+" is disabled").
			WithHint(clierr.Rancher + " get clusterrepo" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(typedCluster), repo))
	}
	return obj, nil
}

// chartsHint is the command that lists the repository's charts.
func (p installPlan) chartsHint(extra string) string {
	return clierr.Rancher + " charts --cluster " + clierr.ShellQuote(p.target.typedCluster) + " --repo " + clierr.ShellQuote(p.repo) + extra
}

// resolveVersion finds the chart version to install in the index Rancher
// serves for the cluster: the exact --version, or the version rancher
// charts shows.
func (p installPlan) resolveVersion(ctx context.Context, want string) (ChartVersion, []string, error) {
	idx, err := ReadIndex(ctx, p.root, p.cluster, p.repo)
	if err != nil {
		return ChartVersion{}, nil, withRepoRemedy(err, p.target.typedCluster)
	}
	versions, ok := idx.Entries[p.chart]
	if !ok || len(versions) == 0 {
		return ChartVersion{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"chart %s is not in the index ClusterRepo %s serves for this cluster", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo))).
			WithHint(p.chartsHint(""))
	}
	if want != "" {
		for _, v := range versions {
			if v.Version == want {
				return v, nil, nil
			}
		}
		return ChartVersion{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"version %s of chart %s is not in the index ClusterRepo %s serves for this cluster; --version names one exact version; the index has %s",
			clierr.ShellQuote(want), clierr.ShellQuote(p.chart), clierr.ShellQuote(p.repo), versionList(versions))).
			WithHint(p.chartsHint(" --all-versions"))
	}
	usable := WithoutDeprecated(versions)
	v, ok := DefaultVersion(usable)
	if !ok {
		return ChartVersion{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"every version of %s is deprecated; name one with --version; the index has %s",
			clierr.ShellQuote(p.chart), versionList(versions))).
			WithHint(p.chartsHint(" --all-versions --include-deprecated"))
	}
	var warnings []string
	if IsPrerelease(v.Version) {
		warnings = append(warnings, fmt.Sprintf("%s is a prerelease; every version of %s that is not deprecated is one", v.Version, p.chart))
	}
	return v, warnings, nil
}

func versionList(versions []ChartVersion) string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Version
	}
	return strings.Join(out, ", ")
}

// checkChart applies the refusals the chart's default values and
// annotations call for, and returns the warnings of the checks it could not
// make.
func (p installPlan) checkChart(ctx context.Context, entry ChartVersion, info Info, repo *unstructured.Unstructured) ([]string, error) {
	name := fmt.Sprintf("chart %s %s", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version))
	if global, ok := info.Values["global"].(map[string]any); ok {
		if _, ok := global["cattle"]; ok {
			return nil, clierr.New(clierr.CodeValidation, name+
				" reads cluster values under global.cattle that the Rancher UI sets when it installs a chart; rancher-cli cannot install it; install it from the Rancher UI")
		}
	}
	// The Rancher UI reads the annotations of the index entry; the info
	// link's chart metadata stands in only for an entry that carries none.
	ann := entry.Annotations
	if len(ann) == 0 {
		ann = info.Chart.Annotations
	}
	if first := autoInstallCharts(ann); len(first) > 0 {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"%s names %s to install first (annotation %s); rancher-cli does not install a chart that names another chart to install first; install it from the Rancher UI",
			name, strings.Join(first, ", "), firstAnnotation(ann)))
	}
	if want := ann[annotationNamespace]; want != "" && want != p.target.namespace {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"%s installs only in namespace %s (annotation %s); run it with --namespace %s",
			name, clierr.ShellQuote(want), annotationNamespace, clierr.ShellQuote(want)))
	}
	if want := ann[annotationReleaseName]; want != "" && want != p.target.release {
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
	w, err := p.checkPermitsOS(ctx, name, ann, repo)
	if err != nil {
		return nil, err
	}
	return append(warnings, w...), nil
}

// autoInstallCharts names what a chart asks to install first: the chart of
// each catalog.cattle.io/auto-install entry (<chart>=<constraint>), else
// each catalog.cattle.io/auto-install-gvr entry.
func autoInstallCharts(ann map[string]string) []string {
	if out := autoInstallChartNames(ann); len(out) > 0 {
		return out
	}
	return trimmedList(ann[annotationAutoInstallGVR])
}

// autoInstallChartNames names the chart of each catalog.cattle.io/auto-install
// entry (<chart>=<constraint>): the charts the Rancher UI installs with this
// one.
func autoInstallChartNames(ann map[string]string) []string {
	var out []string
	for _, e := range trimmedList(ann[annotationAutoInstall]) {
		chart, _, _ := strings.Cut(e, "=")
		out = append(out, chart)
	}
	return out
}

func firstAnnotation(ann map[string]string) string {
	if len(trimmedList(ann[annotationAutoInstall])) > 0 {
		return annotationAutoInstall
	}
	return annotationAutoInstallGVR
}

// checkRequires refuses the chart when the cluster serves no resource for
// an entry of catalog.cattle.io/requires-gvr: <schema id>[/<version>],
// where the schema id is the lowercased <group>.<kind>, or the lowercased
// kind of the core group, and the version is compared with the group's
// preferred version, as the Rancher UI compares it.
func (p installPlan) checkRequires(ctx context.Context, name string, entries []string) ([]string, error) {
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

// served reports whether one discovered resource gives the schema id, and
// the version when one is given.
func served(resources []engine.APIResource, id, version string) bool {
	for _, r := range resources {
		if strings.Contains(r.Name, "/") {
			continue
		}
		rid := strings.ToLower(r.Kind)
		if r.Group != "" {
			rid = strings.ToLower(r.Group + "." + r.Kind)
		}
		if rid == id && (version == "" || version == r.Version) {
			return true
		}
	}
	return false
}

// incompleteGroups is the set of API groups discovery could not list.
type incompleteGroups map[string]bool

func errorsAsIncomplete(err error) (incompleteGroups, bool) {
	di, ok := errors.AsType[*engine.DiscoveryIncomplete](err)
	if !ok {
		return nil, false
	}
	out := incompleteGroups{}
	for gv := range di.Failed {
		out[strings.ToLower(gv.Group)] = true
	}
	return out, true
}

// hasGroupOf reports whether the group of a schema id is one discovery
// could not list; a core-group id has no dot.
func (g incompleteGroups) hasGroupOf(id string) bool {
	group, _, ok := strings.CutLast(id, ".")
	if !ok {
		return g[""]
	}
	return g[group]
}

// checkPermitsOS refuses the chart when the cluster has workers of an OS
// that catalog.cattle.io/permits-os does not list. Without the annotation a
// chart from a Rancher repository permits linux only and any other chart
// every OS. The workers' OSes come from the management cluster, which the
// caller may not be able to read: the check is then skipped with a warning.
func (p installPlan) checkPermitsOS(ctx context.Context, name string, ann map[string]string, repo *unstructured.Unstructured) ([]string, error) {
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
	mgmt, err := steve.GetCluster(ctx, steve.RootGetter(p.root), p.cluster)
	if err != nil && !forbidden(err) {
		return nil, err
	}
	if err != nil {
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

// workerOSes reads the OSes of a management cluster's workers as the
// Rancher UI does: the worker counts in its status, else the provider,
// else linux.
func workerOSes(cluster *unstructured.Unstructured) []string {
	windows, _, _ := unstructured.NestedFieldNoCopy(cluster.Object, "status", "windowsWorkerCount")
	linux, _, _ := unstructured.NestedFieldNoCopy(cluster.Object, "status", "linuxWorkerCount")
	if positive(windows) || positive(linux) {
		var out []string
		if positive(windows) {
			out = append(out, "windows")
		}
		if positive(linux) {
			out = append(out, "linux")
		}
		return out
	}
	if provider, _, _ := unstructured.NestedString(cluster.Object, "status", "provider"); strings.HasSuffix(provider, ".windows") {
		return []string{"windows"}
	}
	return []string{"linux"}
}

func positive(v any) bool {
	switch n := v.(type) {
	case int64:
		return n > 0
	case float64:
		return n > 0
	case int:
		return n > 0
	}
	return false
}

// rancherSource reports whether a ClusterRepo serves Rancher's own charts,
// as the Rancher UI decides it: its URL or Git repository is on rancher.io
// or a subdomain of it, and it does not set both.
func rancherSource(repo *unstructured.Unstructured) bool {
	u, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	g, _, _ := unstructured.NestedString(repo.Object, "spec", "gitRepo")
	if u != "" && g != "" {
		return false
	}
	for _, raw := range []string{u, g} {
		parsed, err := url.Parse(raw)
		if raw == "" || err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "rancher.io" || strings.HasSuffix(host, ".rancher.io") {
			return true
		}
	}
	return false
}

// checkNoApp refuses an install over an App of the release name.
func (p installPlan) checkNoApp(ctx context.Context) error {
	app, err := p.client.Get(ctx, kind("app").GVR, p.target.namespace, p.target.release)
	if err != nil {
		if notFound(err) {
			return nil
		}
		return err
	}
	status, _, _ := unstructured.NestedString(app.Object, "spec", "info", "status")
	t := p.target
	if status == "uninstalled" {
		return clierr.New(clierr.CodeConflict, fmt.Sprintf(
			"release %s in namespace %s is uninstalled and Helm keeps its history; Helm does not install a release name with kept history; choose another release name",
			clierr.ShellQuote(t.release), clierr.ShellQuote(t.namespace)))
	}
	msg := fmt.Sprintf("app %s already exists in namespace %s (status %s)",
		clierr.ShellQuote(t.release), clierr.ShellQuote(t.namespace), statusText(status))
	if status == "failed" {
		msg += "; to remove the failed release, add --apply --yes after reading the preview of " +
			clierr.Rancher + " app uninstall" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(t.typedCluster)+" --namespace "+clierr.ShellQuote(t.namespace)+" --dry-run", t.release)
	}
	return clierr.New(clierr.CodeConflict, msg).
		WithHint(clierr.Rancher + " get app" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(t.typedCluster)+" --namespace "+clierr.ShellQuote(t.namespace), t.release))
}

func statusText(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// checkNamespace refuses a namespace that does not exist; the CLI does not
// create it here.
func (p installPlan) checkNamespace(ctx context.Context) error {
	_, err := p.client.Get(ctx, kind("namespace").GVR, "", p.target.namespace)
	if err == nil {
		return nil
	}
	if !notFound(err) {
		return err
	}
	t := p.target
	return clierr.New(clierr.CodeNotFound, fmt.Sprintf(
		"namespace %s not found on cluster %s; create it first; a project member creates it in the project (<project> is its name) with %s",
		clierr.ShellQuote(t.namespace), clierr.ShellQuote(t.typedCluster),
		clierr.Rancher+" create namespace"+clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(t.typedCluster)+" --project <project> --dry-run", t.namespace))).
		WithHint(clierr.Rancher + " create namespace" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(t.typedCluster)+" --dry-run", t.namespace))
}

// execute reads the App, the operations and the index entry again, then
// sends the request once.
func (c *installCmd) execute(ctx context.Context, p installPlan) (runtime.Result, error) {
	t := p.target
	if _, err := p.client.Get(ctx, kind("app").GVR, t.namespace, t.release); err == nil {
		return runtime.Result{}, changedBeforeRequest("app " + clierr.ShellQuote(t.release) + " was created")
	} else if !notFound(err) {
		return runtime.Result{}, err
	}
	if err := recheckOperations(ctx, p.client, t, c.now()); err != nil {
		return runtime.Result{}, err
	}
	var warnings []string
	idx, err := ReadIndex(ctx, p.root, p.cluster, p.repo)
	if err != nil {
		return runtime.Result{}, changedBeforeRequest(fmt.Sprintf("the index of ClusterRepo %s could not be read again (%s)",
			clierr.ShellQuote(p.repo), clierr.From(err).Message))
	}
	i := slices.IndexFunc(idx.Entries[p.chart], func(v ChartVersion) bool { return v.Version == p.version })
	if i < 0 {
		return runtime.Result{}, changedBeforeRequest(fmt.Sprintf("version %s of chart %s left the index", clierr.ShellQuote(p.version), clierr.ShellQuote(p.chart)))
	}
	now := idx.Entries[p.chart][i].Digest
	switch {
	case p.digest != "" && now != "" && p.digest != now:
		return runtime.Result{}, changedBeforeRequest(fmt.Sprintf("the content of chart %s %s changed in the index", clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version)))
	case p.digest == "" || now == "":
		warnings = append(warnings, fmt.Sprintf("the index gives no digest for chart %s %s; its content was not compared with the preview's", p.chart, p.version))
	}
	a, err := postAction(ctx, c.actions, installPath(p.cluster, p.repo), p.body, t, p.values)
	if err != nil {
		return runtime.Result{}, err
	}
	res := actionResult("install", t, a, p.canRead)
	res.Warnings = append(warnings, res.Warnings...)
	return res, nil
}
