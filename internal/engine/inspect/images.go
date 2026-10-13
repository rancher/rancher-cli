package inspect

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const flagAllClusters = "all-clusters"

const (
	flagBy    = "by"
	flagMatch = "match"
)

// matchesAny does a case-sensitive substring over the raw ref, values
// OR, no needles matches everything. Precision comes from delimiters in
// the needle (registry.old.corp/, /nginx:), not pattern syntax.
func matchesAny(ref string, needles []string) bool {
	if len(needles) == 0 {
		return true
	}
	for _, n := range needles {
		if strings.Contains(ref, n) {
			return true
		}
	}
	return false
}

// imagesFanOutLimit bounds --all-clusters concurrency: the factory's
// mutex/memoization make concurrent Cluster() calls safe, but an unbounded
// fan-out over a large fleet would still open too many connections at once.
const imagesFanOutLimit = 5

const (
	byImage    = "image"
	byWorkload = "workload"
)

// imagesResult is the by-discriminated envelope: one Result Kind,
// agents branch on "by". Exactly one of RowData/Clusters is set.
type imagesResult struct {
	By       string         `json:"by"`
	RowData  output.Tabular `json:"rows,omitempty"`
	Clusters output.Tabular `json:"clusters,omitempty"`
	// Degraded names the collectors this run could not read in full. Absent
	// when every collector succeeded.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
}

func (r imagesResult) Columns() []string {
	if r.Clusters != nil {
		return r.Clusters.Columns()
	}
	if r.RowData == nil {
		return nil
	}
	return r.RowData.Columns()
}

func (r imagesResult) Rows() [][]string {
	if r.Clusters != nil {
		return r.Clusters.Rows()
	}
	if r.RowData == nil {
		return nil
	}
	return r.RowData.Rows()
}

type imagesCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewImages builds `images [--cluster X | --all-clusters]`: every
// container+init-container image across a cluster's pods, deduplicated and
// counted. --all-clusters fans out per management cluster and degrades a
// single cluster's failure to a warning - partial results beat none for a
// fleet-wide scan. The clusters are the ones the caller can read, as steve
// lists them.
func NewImages(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &imagesCmd{clients: clients, roots: roots}
}

func (c *imagesCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "images",
		Usage: "List container images in use, grouped by image or owning workload",
		Desc: "Lists container images referenced by active pods (status.phase Running or Pending; " +
			"Succeeded and Failed pods are excluded, terminating pods remain until removed), " +
			"reading spec.containers, spec.initContainers, and spec.ephemeralContainers. " +
			"--match filters by case-sensitive substring over the image reference exactly as " +
			"written in the pod spec: repeatable, values OR, no OCI normalization (a bare " +
			"nginx:1.25 does not match docker.io); anchor with delimiters, e.g. " +
			"--match registry.old.corp/ for a registry. --match never inspects image contents. " +
			"--by workload resolves each pod's controller owner chain and reports one row per " +
			"image and owning workload object (apiVersion, kind, name, uid) with the count of " +
			"pods controlled by exactly that object; owner-resolution failures degrade to the " +
			"first-level reference plus a warning. " +
			degradedContractDesc +
			"Cluster resolution and the pod list are required and hard-error; with --all-clusters the " +
			"management cluster list is required, zero listed clusters is a verified-empty success, and " +
			"every listed cluster failing its scan is an error. Degradable sections: ownerResolution " +
			"(partial, target = the canonical owner identity apiVersion|kind|namespace|name|uid, " +
			"cluster-id prefixed under --all-clusters), clusters (partial, target = cluster id, " +
			"--all-clusters only).",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string", Desc: "target cluster `id|name`; pass exactly one of --cluster or --all-clusters"},
			{Name: flagAllClusters, Type: "bool", Desc: "scan every cluster you can list"},
			{Name: flagBy, Type: "string", Enum: []string{byImage, byWorkload},
				Desc: "Group by unique image or by owning workload (default: image)"},
			{Name: flagMatch, Type: "stringSlice",
				Desc: "Show only images whose reference contains `substr` (repeatable, OR)"},
		},
		Examples: []string{"images --all-clusters --match library/elasticsearch: --by workload"},
		Auth:     true,
		Shape:    runtime.ShapeTabular,
	}
}

func (c *imagesCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	needles := in.Strings(flagMatch)
	if slices.Contains(needles, "") {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--match value must not be empty; pass a non-empty substring, e.g. --match registry.old.corp/")
	}
	by := in.String(flagBy)
	if by == "" {
		by = byImage
	}
	clusterFlag := in.String(resources.FlagCluster)
	allClusters := in.Bool(flagAllClusters)
	if allClusters && clusterFlag != "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cluster and --all-clusters are mutually exclusive")
	}
	if allClusters {
		return c.allClusters(ctx, by, needles)
	}
	if clusterFlag == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cluster or --all-clusters is required; pass --cluster <id|name> for one cluster, or --all-clusters for every cluster you can list").
			WithHint(engine.ClusterRequiredHint)
	}

	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, false)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	if by == byWorkload {
		sec := newSections(ctx, sectionOwnerResolution)
		rows, werr := workloadImagesFor(ctx, client, sec, "", needles)
		if werr != nil {
			return runtime.Result{}, werr
		}
		degraded, warns := sec.Finalize()
		return runtime.Result{Kind: "ClusterImages",
			Data: imagesResult{By: byWorkload, RowData: rows, Degraded: degraded}, Warnings: warns}, nil
	}
	imgs, err := imagesFor(ctx, client, needles)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "ClusterImages", Data: imagesResult{By: byImage, RowData: imgs}}, nil
}

func (c *imagesCmd) allClusters(ctx context.Context, by string, needles []string) (runtime.Result, error) {
	list, err := steve.Clusters(ctx, steve.RootFactoryGetter(c.roots))
	if err != nil {
		return runtime.Result{}, err
	}
	ids := make([]string, 0, len(list))
	for _, cl := range list {
		ids = append(ids, cl.ID)
	}

	sec := newSections(ctx, sectionClusters, sectionOwnerResolution)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(imagesFanOutLimit)
	var mu sync.Mutex
	perImages := clusterImages{}
	perWorkloads := clusterImageWorkloads{}
	failures := map[string]error{}
	// Degrade the cluster, never abort the group: partial results beat none
	// for a fleet-wide scan. The accumulator returns the error only when the
	// caller canceled, which is the group's abort.
	scanFailed := func(id string, err error) error {
		if herr := sec.Partial(sectionClusters, id, err); herr != nil {
			return herr
		}
		mu.Lock()
		defer mu.Unlock()
		failures[id] = err
		return nil
	}
	for _, id := range ids {
		g.Go(func() error {
			client, cerr := c.clients.Cluster(gctx, id)
			if cerr != nil {
				return scanFailed(id, cerr)
			}
			if by == byWorkload {
				rows, ierr := workloadImagesFor(gctx, client, sec, id, needles)
				if ierr != nil {
					return scanFailed(id, ierr)
				}
				mu.Lock()
				defer mu.Unlock()
				perWorkloads[id] = rows
				return nil
			}
			imgs, ierr := imagesFor(gctx, client, needles)
			if ierr != nil {
				return scanFailed(id, ierr)
			}
			mu.Lock()
			defer mu.Unlock()
			perImages[id] = imgs
			return nil
		})
	}
	if werr := g.Wait(); werr != nil {
		return runtime.Result{}, werr
	}
	// A scan that ended because the caller went away is not a fleet-wide
	// availability answer, whatever the individual errors said.
	if ctx.Err() != nil {
		return runtime.Result{}, ctx.Err()
	}
	// Zero listed clusters is a verified-empty success. Every listed cluster
	// failing is not partial access to a fleet - there is no fleet-wide
	// answer left to return.
	if len(ids) > 0 && len(failures) == len(ids) {
		return runtime.Result{}, everyScanFailed(failures)
	}
	degraded, warns := sec.Finalize()
	if by == byWorkload {
		return runtime.Result{Kind: "ClusterImages",
			Data: imagesResult{By: byWorkload, Clusters: perWorkloads, Degraded: degraded}, Warnings: warns}, nil
	}
	return runtime.Result{Kind: "ClusterImages",
		Data: imagesResult{By: byImage, Clusters: perImages, Degraded: degraded}, Warnings: warns}, nil
}

// everyScanFailed is the hard error for a fleet-wide scan that reached no
// cluster at all: partial results beat none, but here there are none. The
// shape is deterministic - the shared code when every cluster failed the
// same way, GENERIC when they disagree, and causes in cluster id order.
func everyScanFailed(failures map[string]error) error {
	ids := make([]string, 0, len(failures))
	for id := range failures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	code := degradedCode(failures[ids[0]])
	for _, id := range ids[1:] {
		if degradedCode(failures[id]) != code {
			code = clierr.CodeGeneric
			break
		}
	}
	causes := make([]string, 0, len(ids))
	for _, id := range ids {
		causes = append(causes, id+": "+failures[id].Error())
	}
	return clierr.New(code, "no cluster could be scanned: "+strings.Join(causes, "; "))
}

// podActive reports whether a pod counts toward the image inventory. Only
// Running and Pending pods are active: completed-job images are not
// blast radius; a Pending rollout pod's images are. Terminating pods retain
// their phase until they leave the List, so they stay included.
func podActive(pod map[string]any) bool {
	phase, _, _ := unstructured.NestedString(pod, "status", "phase")
	return phase == "Running" || phase == "Pending"
}

var podImageFields = []string{"containers", "initContainers", "ephemeralContainers"}

// podImageRefs returns every image reference in the pod spec, in field
// order, duplicates preserved: --by image counts references while
// --by workload dedupes per pod.
func podImageRefs(pod map[string]any) []string {
	var out []string
	for _, field := range podImageFields {
		items, _, _ := unstructured.NestedSlice(pod, "spec", field)
		for _, item := range items {
			cm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if img, _ := cm["image"].(string); img != "" {
				out = append(out, img)
			}
		}
	}
	return out
}

// imagesFor lists a cluster's pods across all namespaces, skips inactive
// pods, and dedupes every container image reference into a sorted, counted
// slice. needles filters by substring; nil matches everything.
func imagesFor(ctx context.Context, client engine.ClusterClient, needles []string) (imageRows, error) {
	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, "", "")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for i := range list.Items {
		pod := list.Items[i].Object
		if !podActive(pod) {
			continue
		}
		for _, img := range podImageRefs(pod) {
			if matchesAny(img, needles) {
				counts[img]++
			}
		}
	}
	out := make(imageRows, 0, len(counts))
	for img, n := range counts {
		out = append(out, imageCount{Image: img, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

type imageCount struct {
	Image string `json:"image"`
	Count int    `json:"count"`
}

// imageRows is single-cluster `images`' payload: a sorted, deduplicated
// image/count list. It implements output.Tabular for `-o table`.
type imageRows []imageCount

func (r imageRows) Columns() []string { return []string{"IMAGE", "COUNT"} }

func (r imageRows) Rows() [][]string {
	out := make([][]string, 0, len(r))
	for _, c := range r {
		out = append(out, []string{c.Image, strconv.Itoa(c.Count)})
	}
	return out
}

// clusterImages is --all-clusters' payload: per-cluster image/count lists.
type clusterImages map[string]imageRows

func (c clusterImages) Columns() []string { return []string{"CLUSTER", "IMAGE", "COUNT"} }

func (c clusterImages) Rows() [][]string {
	ids := make([]string, 0, len(c))
	for id := range c {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out [][]string
	for _, id := range ids {
		for _, img := range c[id] {
			out = append(out, []string{id, img.Image, strconv.Itoa(img.Count)})
		}
	}
	return out
}
