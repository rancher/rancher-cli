package inspect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const flagAllClusters = "all-clusters"

// imagesFanOutLimit bounds --all-clusters concurrency: the factory's
// mutex/memoization make concurrent Cluster() calls safe, but an unbounded
// fan-out over a large fleet would still open too many connections at once.
const imagesFanOutLimit = 5

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
		Name:        "images",
		Usage:       "List container images in use, deduplicated and counted",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string"},
			{Name: flagAllClusters, Type: "bool"},
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

func (c *imagesCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(in, "images"); err != nil {
		return runtime.Result{}, err
	}
	clusterFlag := in.String(resources.FlagCluster)
	allClusters := in.Bool(flagAllClusters)
	if allClusters && clusterFlag != "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cluster and --all-clusters are mutually exclusive").
			WithHint("pass exactly one of --cluster or --all-clusters")
	}
	if allClusters {
		return c.allClusters(ctx)
	}

	clusterID, err := resources.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	imgs, err := imagesFor(ctx, client)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "ClusterImages", Data: imgs}, nil
}

func (c *imagesCmd) allClusters(ctx context.Context) (runtime.Result, error) {
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
	perCluster := clusterImages{}
	var warns []string
	for _, id := range ids {
		id := id
		g.Go(func() error {
			client, cerr := c.clients.Cluster(gctx, id)
			if cerr == nil {
				imgs, ierr := imagesFor(gctx, client)
				if ierr == nil {
					mu.Lock()
					perCluster[id] = imgs
					mu.Unlock()
					return nil
				}
				cerr = ierr
			}
			mu.Lock()
			warns = append(warns, fmt.Sprintf("cluster %s: %v", id, cerr))
			mu.Unlock()
			return nil // degrade, never abort the group
		})
	}
	_ = g.Wait()
	return runtime.Result{Kind: "ClusterImages", Data: perCluster, Warnings: warns}, nil
}

// imagesFor lists a cluster's pods across all namespaces and dedupes every
// container and init-container image into a sorted, counted slice.
func imagesFor(ctx context.Context, client engine.ClusterClient) (imageRows, error) {
	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, "", "")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for i := range list.Items {
		countImages(counts, list.Items[i].Object, "containers")
		countImages(counts, list.Items[i].Object, "initContainers")
	}
	out := make(imageRows, 0, len(counts))
	for img, n := range counts {
		out = append(out, imageCount{Image: img, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

func countImages(counts map[string]int, pod map[string]any, field string) {
	items, _, _ := unstructured.NestedSlice(pod, "spec", field)
	for _, item := range items {
		cm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if img, _ := cm["image"].(string); img != "" {
			counts[img]++
		}
	}
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
