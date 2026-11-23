package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// versionDocument is the part of /rancherversion the source reads.
type versionDocument struct {
	Version      string `json:"Version"`
	RancherPrime string `json:"RancherPrime"`
}

// repoList is the part of a ClusterRepo list the source reads: the name and
// the fields that tell where each repository reads charts from.
type repoList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			GitRepo string `json:"gitRepo"`
			URL     string `json:"url"`
		} `json:"spec"`
		Status struct {
			URL string `json:"url"`
		} `json:"status"`
	} `json:"items"`
}

// AddRepositorySource is the live source of the repository name `extension
// add-repository` takes, whose registered name is command: the Rancher
// extension repositories the command would add. partner-extensions is offered
// on every edition, rancher-ui-plugins only when the server says it is Rancher
// Prime; a repository whose URL is already read by a ClusterRepo, or whose name
// is already a ClusterRepo's, is not offered, because the command refuses it.
// Two reads of the local cluster, the version document then the ClusterRepos;
// nothing when the ClusterRepos cannot be read, and an unreadable edition
// offers only partner-extensions. It claims no other site.
func AddRepositorySource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command || req.Flag != "" || req.Index != 0 {
				return completion.Requirements{}, false
			}
			return completion.ManagementTwoListings(), true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
			edition := readEdition(ctx, conn)
			existing, ok := readClusterRepos(ctx, conn)
			if !ok {
				return completion.Result{}
			}
			var names []string
			for _, repo := range extension.Repositories() {
				if repo.PrimeOnly && edition != extension.EditionPrime {
					continue
				}
				if _, added := extension.AlreadyAdded(existing, repo.URL); added {
					continue
				}
				if taken(existing, repo.Name) {
					continue
				}
				names = append(names, repo.Name)
			}
			return completion.Result{Names: names}
		},
	}
}

// readEdition reads the server's edition from its version document; a failed
// or malformed read is the unknown edition.
func readEdition(ctx context.Context, conn *completion.Connection) extension.Edition {
	p, err := completion.RancherVersionPath()
	if err != nil {
		return extension.EditionUnknown
	}
	var doc versionDocument
	if err := conn.GetJSON(ctx, p, "", &doc); err != nil {
		return extension.EditionUnknown
	}
	return extension.ParseEdition(doc.RancherPrime)
}

// readClusterRepos lists the local cluster's ClusterRepos. It reports false
// when the list cannot be read.
func readClusterRepos(ctx context.Context, conn *completion.Connection) ([]extension.ClusterRepo, bool) {
	p, err := completion.ClusterPath("local", "catalog.cattle.io", "v1", "clusterrepos")
	if err != nil {
		return nil, false
	}
	var list repoList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, false
	}
	repos := make([]extension.ClusterRepo, 0, len(list.Items))
	for _, it := range list.Items {
		repos = append(repos, extension.ClusterRepo{Name: it.Metadata.Name, StatusURL: it.Status.URL, GitRepo: it.Spec.GitRepo, URL: it.Spec.URL})
	}
	return repos, true
}

// taken reports whether a ClusterRepo already has the name.
func taken(repos []extension.ClusterRepo, name string) bool {
	for _, r := range repos {
		if r.Name == name {
			return true
		}
	}
	return false
}
