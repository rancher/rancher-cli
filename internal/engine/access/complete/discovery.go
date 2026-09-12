package complete

import (
	"context"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// aggregatedDiscoveryAccept asks a Kubernetes API server for aggregated
// discovery, the one document shape the source reads: v2 first, v2beta1
// for a server that predates it. A server answering with plain discovery
// instead contributes nothing.
const aggregatedDiscoveryAccept = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList," +
	"application/json;g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList"

// discoveryListKind is the kind an aggregated discovery root answers with.
const discoveryListKind = "APIGroupDiscoveryList"

// QuestionSource is the source access can, who-can and explain hand the
// surface for the sites the typed cluster's discovery answers: the
// resource positional (the second, after the verb), --group and
// --subresource (resolving the typed <resource>[/<name>] as the commands
// read it). Each needs the typed cluster resolved, a management
// token and the two discovery roots. Beside --non-resource-url the
// question has no resource, so nothing is claimed; --subresource is
// claimed only once a resource is typed to resolve; every other site of
// the commands is declined (--as belongs to the shared subject source).
func QuestionSource() completion.Source {
	return completion.Source{Claim: claimQuestion, Complete: completeQuestion}
}

// discoveryListing is the declaration of a discovery-backed site.
func discoveryListing() completion.Requirements {
	return completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialManagement, Allowance: completion.AllowanceDiscovery}
}

func claimQuestion(req completion.Request) (completion.Requirements, bool) {
	if completion.TailFlag(req.Tail, "non-resource-url") != "" {
		return completion.Requirements{}, false
	}
	switch {
	case req.Flag == "group", req.Flag == "" && req.Index == 1:
		return discoveryListing(), true
	case req.Flag == "subresource" && len(req.Positionals) > 1:
		return discoveryListing(), true
	}
	return completion.Requirements{}, false
}

func completeQuestion(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	all := readDiscovery(ctx, conn, req.ClusterID)
	switch req.Flag {
	case "group":
		return completion.Result{Names: groupNames(all)}
	case "subresource":
		resource, ok := typedResource(req.Positionals[1])
		if !ok {
			return completion.Result{}
		}
		return completion.Result{Names: subresourceNames(all, resource, completion.TailFlag(req.Tail, "group"))}
	}
	return completion.Result{Names: baseResourceNames(all)}
}

// typedResource reads the resource out of the typed <resource>[/<name>]
// positional the way the commands do: the token before the first "/";
// a name that is empty or carries another "/" is a spelling the commands
// reject, so nothing is offered for it.
func typedResource(arg string) (string, bool) {
	resource, name, hasName := strings.Cut(arg, "/")
	if hasName && (name == "" || strings.Contains(name, "/")) {
		return "", false
	}
	return resource, true
}

// resource is one served resource of the cluster in its preferred
// version, with the names the commands resolve it by and its
// subresources.
type resource struct {
	group, name, singular string
	shortNames            []string
	subresources          []string
}

// discoveryList is the aggregated discovery document's shape, the fields
// the source reads.
type discoveryList struct {
	Kind  string `json:"kind"`
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Versions []struct {
			Version   string `json:"version"`
			Freshness string `json:"freshness"`
			Resources []struct {
				Resource         string   `json:"resource"`
				SingularResource string   `json:"singularResource"`
				ShortNames       []string `json:"shortNames"`
				Subresources     []struct {
					Subresource string `json:"subresource"`
				} `json:"subresources"`
			} `json:"resources"`
		} `json:"versions"`
	} `json:"items"`
}

// readDiscovery reads the cluster's two aggregated discovery roots and
// returns every group's resources from its preferred version: the first
// version the server did not mark stale, as the commands' own discovery
// picks it. A root that fails or answers another document contributes
// nothing; a stale version contributes nothing, since a suggestion must
// be valid verbatim.
func readDiscovery(ctx context.Context, conn *completion.Connection, clusterID string) []resource {
	core, groups, err := completion.DiscoveryPaths(clusterID)
	if err != nil {
		return nil
	}
	var all []resource
	for _, root := range []completion.Path{core, groups} {
		var doc discoveryList
		if err := conn.GetJSON(ctx, root, aggregatedDiscoveryAccept, &doc); err != nil || doc.Kind != discoveryListKind {
			continue
		}
		for _, g := range doc.Items {
			for _, v := range g.Versions {
				if v.Freshness == "Stale" {
					continue
				}
				for _, r := range v.Resources {
					res := resource{group: g.Metadata.Name, name: r.Resource, singular: r.SingularResource, shortNames: r.ShortNames}
					for _, sub := range r.Subresources {
						res.subresources = append(res.subresources, sub.Subresource)
					}
					all = append(all, res)
				}
				break
			}
		}
	}
	return all
}

// baseResourceNames answers every served resource's plural name.
func baseResourceNames(all []resource) []string {
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, r.name)
	}
	return names
}

// groupNames answers the named API groups; the core group has no name to
// offer.
func groupNames(all []resource) []string {
	var names []string
	for _, r := range all {
		if r.group != "" {
			names = append(names, r.group)
		}
	}
	return names
}

// subresourceNames resolves the typed resource with the commands' own
// rules - plural, singular or short name; a name.group spelling or
// --group selects the group, "core" naming the core group, and the two
// must agree; a bare name several groups serve resolves to core when
// core serves it and to nothing otherwise - and answers that one
// resource's subresources, so every suggestion is valid verbatim.
func subresourceNames(all []resource, arg, group string) []string {
	name, dotGroup := arg, ""
	if i := strings.IndexByte(arg, '.'); i > 0 && !strings.Contains(arg, "/") {
		name, dotGroup = arg[:i], arg[i+1:]
	}
	wantGroup, groupSet := group, group != ""
	if dotGroup != "" {
		if groupSet && dotGroup != group {
			return nil
		}
		wantGroup, groupSet = dotGroup, true
	}
	if wantGroup == "core" {
		wantGroup = ""
	}
	var matches []resource
	seen := map[string]bool{}
	for _, r := range all {
		key := r.group + "/" + r.name
		if seen[key] {
			continue
		}
		seen[key] = true
		if groupSet && r.group != wantGroup {
			continue
		}
		if r.name == name || r.singular == name || slices.Contains(r.shortNames, name) {
			matches = append(matches, r)
		}
	}
	if len(matches) > 1 && !groupSet {
		for _, m := range matches {
			if m.group == "" {
				matches = []resource{m}
				break
			}
		}
	}
	if len(matches) != 1 {
		return nil
	}
	return matches[0].subresources
}
