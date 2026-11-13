package steve

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// ClustersType is the steve type of Rancher's management clusters.
const ClustersType = "management.cattle.io.clusters"

// Cluster is one cluster the caller can read.
type Cluster struct{ ID, DisplayName string }

type clusterItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

// Clusters reads the clusters the caller can read, as steve lists them.
func Clusters(ctx context.Context, get Getter) ([]Cluster, error) {
	return clusters(ctx, get, ReadCollection[Cluster])
}

// ClustersOnce is Clusters in one walk of the collection: a collection that
// moves under the read is an error at once, never read again.
func ClustersOnce(ctx context.Context, get Getter) ([]Cluster, error) {
	return clusters(ctx, get, readOnce[Cluster])
}

func clusters(ctx context.Context, get Getter, read func(context.Context, Getter, Request, Decoder[Cluster]) ([]Cluster, error)) ([]Cluster, error) {
	r := Request{Type: ClustersType}
	rows, err := read(ctx, get, r, decodeCluster)
	if err != nil {
		return nil, collectionRefusal(r, err)
	}
	return rows, nil
}

func decodeCluster(path string, index int, raw json.RawMessage) (string, Cluster, error) {
	var item clusterItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", Cluster{}, protocolError(path, fmt.Sprintf("item %d is not an object: %s", index, err))
	}
	if item.ID == "" {
		return "", Cluster{}, protocolError(path, fmt.Sprintf("item %d has no id", index))
	}
	if item.Metadata.Name != "" && item.Metadata.Name != item.ID {
		return "", Cluster{}, protocolError(path, fmt.Sprintf("item %q has metadata.name %q, which differs from its id", item.ID, item.Metadata.Name))
	}
	return item.ID, Cluster{ID: item.ID, DisplayName: item.Spec.DisplayName}, nil
}

// ResolveProjectFlag resolves a typed --project v on the cluster that
// cluster, the typed --cluster, names. With no --cluster it never picks a
// cluster: it refuses, naming as candidates the clusters on which a
// project the caller can read has v as its id or display name.
func ResolveProjectFlag(ctx context.Context, get Getter, clients engine.ClusterClientFactory, cluster, v string) (p Project, warning string, err error) {
	if cluster == "" {
		return Project{}, "", clusterRequired(ctx, get, v)
	}
	clusterID, err := clients.ResolveCluster(ctx, cluster)
	if err != nil {
		return Project{}, "", err
	}
	local, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return Project{}, "", err
	}
	return ResolveProject(ctx, get, local, clusterID, v)
}

// clusterRequired is the refusal of a --project typed without --cluster.
func clusterRequired(ctx context.Context, get Getter, v string) error {
	list, err := Projects(ctx, get, "")
	if err != nil {
		return err
	}
	var on []string
	byDisplay, byID := 0, false
	for _, p := range list {
		switch {
		case p.ID == v:
			byID = true
		case p.DisplayName == v:
			byDisplay++
		default:
			continue
		}
		if !slices.Contains(on, p.Cluster) {
			on = append(on, p.Cluster)
		}
	}
	q := clierr.ShellQuote(v)
	if len(on) == 0 {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"--cluster is required with --project; no project you can read has the id or display name %s", q)).
			WithHintCommand(clierr.NewCommand("access projects"))
	}
	slices.Sort(on)
	clusters, err := Clusters(ctx, get)
	if err != nil {
		return err
	}
	name := map[string]string{}
	for _, c := range clusters {
		name[c.ID] = c.DisplayName
	}
	cands := make([]clierr.Candidate, len(on))
	quoted := make([]string, len(on))
	for i, id := range on {
		cands[i] = clierr.Candidate{ID: id, Name: name[id]}
		quoted[i] = clierr.ShellQuote(id)
	}
	hint := clierr.NewCommand("access clusters")
	pass, passOK := clierr.FlagFragment("--cluster", on[0])
	msg := fmt.Sprintf("--cluster is required with --project; the project with the id or display name %s is on cluster %s",
		q, quoted[0])
	if passOK {
		msg += " - pass " + pass
	}
	switch {
	case len(on) > 1:
		msg = fmt.Sprintf("--cluster is required with --project; projects with the id or display name %s are on clusters %s - pass --cluster with one of them",
			q, strings.Join(quoted, ", "))
	case !byID && byDisplay > 1:
		// On the one cluster the display name still names several projects,
		// so --cluster alone is not enough: the caller needs an id, which
		// the projects listing shows.
		msg = fmt.Sprintf("--cluster is required with --project; %d projects with the display name %s are on cluster %s",
			byDisplay, q, quoted[0])
		if passOK {
			msg += " - pass " + pass + " and one of their ids as --project"
		}
		hint = clierr.NewCommand("access projects").FlagValue("--cluster", on[0])
	}
	if !passOK && len(on) == 1 {
		msg = clierr.WithLeftOutNote(msg)
	}
	return clierr.New(clierr.CodeValidation, msg).WithCandidates(cands).WithHintCommand(hint)
}
