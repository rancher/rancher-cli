package inspect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
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

// matchesAny is IM-3: case-sensitive substring over the raw ref, values
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

// imagesResult is the by-discriminated envelope (IM-7): one Result Kind,
// agents branch on "by". Exactly one of RowData/Clusters is set.
type imagesResult struct {
	By       string         `json:"by"`
	RowData  output.Tabular `json:"rows,omitempty"`
	Clusters output.Tabular `json:"clusters,omitempty"`
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
}

// NewImages builds `images [--cluster X | --all-clusters]`: every
// container+init-container image across a cluster's pods, deduplicated and
// counted. --all-clusters fans out per management cluster and degrades a
// single cluster's failure to a warning — partial results beat none for a
// fleet-wide scan.
func NewImages(clients engine.ClusterClientFactory) runtime.Command {
	return &imagesCmd{clients: clients}
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
			"first-level reference plus a warning.",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string", Desc: "target cluster `id|name` (defaults to the current context)"},
			{Name: flagAllClusters, Type: "bool", Desc: "scan all downstream clusters"},
			{Name: flagBy, Type: "string", Enum: []string{byImage, byWorkload},
				Desc: "Group by unique image or by owning workload (default: image)"},
			{Name: flagMatch, Type: "stringSlice",
				Desc: "Show only images whose reference contains `substr` (repeatable, OR)"},
		},
		Examples: []string{"images --all-clusters --match library/elasticsearch: --by workload"},
		Network:  true,
		Auth:     true,
	}
}

func (c *imagesCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(in, "images"); err != nil {
		return runtime.Result{}, err
	}
	needles := in.Strings(flagMatch)
	for _, n := range needles {
		if n == "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"--match value must not be empty").
				WithHint("pass a non-empty substring, e.g. --match registry.old.corp/")
		}
	}
	by := in.String(flagBy)
	if by == "" {
		by = byImage
	}
	clusterFlag := in.String(resources.FlagCluster)
	allClusters := in.Bool(flagAllClusters)
	if allClusters && clusterFlag != "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cluster and --all-clusters are mutually exclusive").
			WithHint("pass exactly one of --cluster or --all-clusters")
	}
	if allClusters {
		return c.allClusters(ctx, by, needles)
	}

	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	if by == byWorkload {
		rows, warns, werr := workloadImagesFor(ctx, client, needles)
		if werr != nil {
			return runtime.Result{}, werr
		}
		sort.Strings(warns)
		return runtime.Result{Kind: "ClusterImages",
			Data: imagesResult{By: byWorkload, RowData: rows}, Warnings: warns}, nil
	}
	imgs, err := imagesFor(ctx, client, needles)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "ClusterImages", Data: imagesResult{By: byImage, RowData: imgs}}, nil
}

func (c *imagesCmd) allClusters(ctx context.Context, by string, needles []string) (runtime.Result, error) {
	mgmt, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	clusterKind, _ := kinds.Lookup("cluster")
	list, err := mgmt.List(ctx, clusterKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	ids := make([]string, 0, len(list.Items))
	for i := range list.Items {
		ids = append(ids, list.Items[i].GetName())
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(imagesFanOutLimit)
	var mu sync.Mutex
	perImages := clusterImages{}
	perWorkloads := clusterImageWorkloads{}
	var warns []string
	for _, id := range ids {
		id := id
		g.Go(func() error {
			client, cerr := c.clients.Cluster(gctx, id)
			if cerr != nil {
				mu.Lock()
				warns = append(warns, fmt.Sprintf("cluster %s: %v", id, cerr))
				mu.Unlock()
				return nil // degrade, never abort the group
			}
			if by == byWorkload {
				rows, ws, ierr := workloadImagesFor(gctx, client, needles)
				mu.Lock()
				defer mu.Unlock()
				if ierr != nil {
					warns = append(warns, fmt.Sprintf("cluster %s: %v", id, ierr))
					return nil
				}
				perWorkloads[id] = rows
				for _, w := range ws {
					warns = append(warns, fmt.Sprintf("cluster %s: %s", id, w))
				}
				return nil
			}
			imgs, ierr := imagesFor(gctx, client, needles)
			mu.Lock()
			defer mu.Unlock()
			if ierr != nil {
				warns = append(warns, fmt.Sprintf("cluster %s: %v", id, ierr))
				return nil
			}
			perImages[id] = imgs
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(warns)
	if by == byWorkload {
		return runtime.Result{Kind: "ClusterImages",
			Data: imagesResult{By: byWorkload, Clusters: perWorkloads}, Warnings: warns}, nil
	}
	return runtime.Result{Kind: "ClusterImages",
		Data: imagesResult{By: byImage, Clusters: perImages}, Warnings: warns}, nil
}

// podActive reports whether a pod counts toward the image inventory. Only
// Running and Pending pods are active (IM-14): completed-job images are not
// blast radius; a Pending rollout pod's images are. Terminating pods retain
// their phase until they leave the List, so they stay included.
func podActive(pod map[string]any) bool {
	phase, _, _ := unstructured.NestedString(pod, "status", "phase")
	return phase == "Running" || phase == "Pending"
}

var podImageFields = []string{"containers", "initContainers", "ephemeralContainers"}

// podImageRefs returns every image reference in the pod spec, in field
// order, duplicates preserved: --by image counts references (IM-18) while
// --by workload dedupes per pod (IM-17).
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
// slice. needles filters by substring (IM-3); nil matches everything.
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
