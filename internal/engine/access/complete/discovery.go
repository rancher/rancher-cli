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

// groupList is the legacy /apis document's shape (APIGroupList), the
// fields the source reads: each group's name and preferred version.
type groupList struct {
	Groups []struct {
		Name             string `json:"name"`
		PreferredVersion struct {
			Version string `json:"version"`
		} `json:"preferredVersion"`
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	} `json:"groups"`
}

// coreVersions is the legacy /api document's shape (APIVersions): the
// core group's versions, the preferred one first.
type coreVersions struct {
	Versions []string `json:"versions"`
}

// resourceList is one group version's legacy discovery document
// (APIResourceList), the fields the source reads. A subresource is its
// own entry named "<resource>/<sub>".
type resourceList struct {
	Resources []struct {
		Name         string   `json:"name"`
		SingularName string   `json:"singularName"`
		ShortNames   []string `json:"shortNames"`
	} `json:"resources"`
}

// legacyWorkers bounds the goroutines reading group documents at once.
const legacyWorkers = 8

// readDiscovery reads the cluster's two discovery roots and returns every
// group's resources from its preferred version. A root that serves the
// aggregated document contributes the first version the server did not
// mark stale, as the commands' own discovery picks it; a stale version
// contributes nothing, since a suggestion must be valid verbatim. A root
// that does not serve it (the local cluster of Rancher v2.15.0 and
// v2.15.1 answers its /apis with the legacy list to plain JSON and with
// 406 to the aggregated-only header) is walked the way the commands' own
// discovery walks it: the plain root document, then one document per
// group in the server's preferred version, at most
// completion.DiscoveryGroupLimit of them. A root or a group
// document that fails contributes nothing.
func readDiscovery(ctx context.Context, conn *completion.Connection, clusterID string) []resource {
	core, groups, err := completion.DiscoveryPaths(clusterID)
	if err != nil {
		return nil
	}
	var all []resource
	for _, root := range []struct {
		path completion.Path
		core bool
	}{{core, true}, {groups, false}} {
		var doc discoveryList
		if err := conn.GetJSON(ctx, root.path, aggregatedDiscoveryAccept, &doc); err == nil && doc.Kind == discoveryListKind {
			all = append(all, fromAggregated(doc)...)
			continue
		}
		all = append(all, fromLegacy(ctx, conn, clusterID, root.path, root.core)...)
	}
	return all
}

// fromAggregated projects one aggregated document onto the source's
// resources.
func fromAggregated(doc discoveryList) []resource {
	var all []resource
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
	return all
}

// fromLegacy walks one root the legacy way: the plain root document
// names the groups (the core root names its versions), then each group's
// preferred version document is read, in parallel and bounded, and the
// results are kept in the root document's group order.
func fromLegacy(ctx context.Context, conn *completion.Connection, clusterID string, root completion.Path, core bool) []resource {
	type groupVersion struct{ group, version string }
	var gvs []groupVersion
	if core {
		var doc coreVersions
		if err := conn.GetJSON(ctx, root, "", &doc); err != nil || len(doc.Versions) == 0 {
			return nil
		}
		gvs = []groupVersion{{"", doc.Versions[0]}}
	} else {
		var doc groupList
		if err := conn.GetJSON(ctx, root, "", &doc); err != nil {
			return nil
		}
		for _, g := range doc.Groups {
			version := g.PreferredVersion.Version
			if version == "" && len(g.Versions) > 0 {
				version = g.Versions[0].Version
			}
			if g.Name == "" || version == "" {
				continue
			}
			gvs = append(gvs, groupVersion{g.Name, version})
		}
	}
	if len(gvs) > completion.DiscoveryGroupLimit {
		gvs = gvs[:completion.DiscoveryGroupLimit]
	}
	perGroup := make([][]resource, len(gvs))
	completion.Parallel(len(gvs), legacyWorkers, func(i int) {
		path, err := completion.DiscoveryVersionPath(clusterID, gvs[i].group, gvs[i].version)
		if err != nil {
			return
		}
		var doc resourceList
		if err := conn.GetJSON(ctx, path, "", &doc); err != nil {
			return
		}
		perGroup[i] = fromResourceList(gvs[i].group, doc)
	})
	var all []resource
	for _, rs := range perGroup {
		all = append(all, rs...)
	}
	return all
}

// fromResourceList projects one legacy group version document onto the
// source's resources: a plain entry is a resource, a "<resource>/<sub>"
// entry a subresource of the resource named before the slash, attached
// to that resource whichever order the server lists them in.
func fromResourceList(group string, doc resourceList) []resource {
	var all []resource
	index := map[string]int{}
	for _, r := range doc.Resources {
		if !strings.Contains(r.Name, "/") {
			index[r.Name] = len(all)
			all = append(all, resource{group: group, name: r.Name, singular: r.SingularName, shortNames: r.ShortNames})
		}
	}
	for _, r := range doc.Resources {
		base, sub, ok := strings.Cut(r.Name, "/")
		if !ok {
			continue
		}
		if i, known := index[base]; known {
			all[i].subresources = append(all[i].subresources, sub)
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
