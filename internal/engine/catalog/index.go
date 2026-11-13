// Package catalog reads what Rancher's catalog offers: the Helm repository
// index of a ClusterRepo, as the caller, through the server's steve link
// read. Nothing here writes to a server.
package catalog

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// ClusterRepoType is the steve type of a ClusterRepo.
const ClusterRepoType = "catalog.cattle.io.clusterrepos"

// ChartVersion is one version of a chart as the Helm repository index
// lists it: the fields the commands show or decide on.
type ChartVersion struct {
	Version     string            `json:"version"`
	AppVersion  string            `json:"appVersion"`
	Description string            `json:"description"`
	Deprecated  bool              `json:"deprecated"`
	Annotations map[string]string `json:"annotations"`
	// Digest is empty for an OCI entry until Rancher has fetched the chart.
	Digest string `json:"digest"`
}

// Index is a Helm repository index: the versions of each chart, by chart
// name, in the order Rancher serves them (newest first, as Helm sorts them).
type Index struct {
	Entries map[string][]ChartVersion `json:"entries"`
}

// ReadIndex reads the Helm repository index Rancher serves for the
// ClusterRepo repo of cluster, a cluster id. Rancher answers with the
// versions whose annotations allow the cluster's Kubernetes and Rancher
// versions; the request carries no parameter that changes that.
//
// Every error names the repository. A server answer keeps the status
// table's code; an answer that does not decode is GENERIC and never quotes
// the body.
func ReadIndex(ctx context.Context, root engine.RootClient, cluster, repo string) (Index, error) {
	req := steve.Request{Type: ClusterRepoType, Name: repo}
	if cluster != engine.LocalCluster {
		req.Cluster = cluster
	}
	body, err := root.GetJSON(ctx, req.Path()+"?link=index")
	if err != nil {
		return Index{}, repoError(repo, err)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return Index{}, clierr.New(clierr.CodeGeneric,
			"the index of ClusterRepo "+clierr.ShellQuote(repo)+" is not valid JSON").WithCause(err)
	}
	return idx, nil
}

// Info is what Rancher's info link returns for one chart version: the
// chart's default values and its Chart.yaml annotations.
type Info struct {
	Values map[string]any `json:"values"`
	Chart  InfoChart      `json:"chart"`
}

// InfoChart is the part of a chart's Chart.yaml the info link answer carries
// that the commands use.
type InfoChart struct {
	Annotations map[string]string `json:"annotations"`
	// Version is the chart's own version from Chart.yaml. Rancher looks a
	// version up in the index as Helm does, which can answer another build
	// of the same base version.
	Version string `json:"version"`
}

// ReadInfo reads the info of one chart version from the ClusterRepo repo of
// cluster, a cluster id. Errors are shaped as ReadIndex's are: every one
// names the repository, a server answer keeps the status table's code, and
// an answer that does not decode is GENERIC and never quotes the body.
func ReadInfo(ctx context.Context, root engine.RootClient, cluster, repo, chart, version string) (Info, error) {
	req := steve.Request{Type: ClusterRepoType, Name: repo}
	if cluster != engine.LocalCluster {
		req.Cluster = cluster
	}
	path := req.Path() + "?link=info&chartName=" + url.QueryEscape(chart) + "&version=" + url.QueryEscape(version)
	body, err := root.GetJSON(ctx, path)
	if err != nil {
		return Info{}, repoError(repo, err)
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return Info{}, clierr.New(clierr.CodeGeneric,
			"the info of chart "+clierr.ShellQuote(chart)+" version "+clierr.ShellQuote(version)+
				" in ClusterRepo "+clierr.ShellQuote(repo)+" is not valid JSON").WithCause(err)
	}
	return info, nil
}

// repoError puts the repository name in front of err's message and keeps
// its code, retry advice and cause. A 401 is the exception: the status table
// words it as a rejected credential, but Rancher also answers 401 for an
// index ConfigMap the ClusterRepo does not own, so it is a GENERIC error that
// says Rancher refused to serve the index and sends no one to sign in.
func repoError(repo string, err error) error {
	ce := clierr.From(err)
	name := "ClusterRepo " + clierr.ShellQuote(repo)
	if ce.Code == clierr.CodeAuth {
		return clierr.New(clierr.CodeGeneric, name+": Rancher refused to serve its index (the server answered 401)").WithCause(err)
	}
	return ce.WithMessage(name + ": " + ce.Message)
}
