package cli

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steveReadKubernetesListSites lists the completion sites that may list a
// management kind that sets SteveReadOnForbidden through the Kubernetes API:
// the wiring of the site, with the reason. An entry that no site reaches fails
// the test. A downstream kind that sets the flag (the namespace kind) is listed
// through the Kubernetes API by every site, and the test asserts the Steve read
// after a 403 instead.
var steveReadKubernetesListSites = map[string]string{}

// steveReadTypedNamespace is the namespace typed on the lines of a command that
// declares --namespace; with none typed, a workspace kind lists its default
// namespace.
const steveReadTypedNamespace = "team-a"

// steveReadSite is one claimed completion site of a command, with the line that
// reaches it.
type steveReadSite struct {
	name   string
	d      runtime.Descriptor
	lc     completion.Completer
	site   CompletionSite
	tail   []string
	wiring string
}

// steveReadSites lists every site of every command that a completion source
// claims, built from the commands' own descriptors and the sources' own
// claims: each value-taking flag, and each positional from the first to one
// past the declared arguments, with every combination of the words the
// arguments can carry typed before it. A site is run with no flag typed, with
// --namespace typed when the command declares it, and with --cluster typed when
// the command declares it; a flag site does not run on the line that types the
// same flag. The partial word is empty.
func steveReadSites(t *testing.T) []steveReadSite {
	t.Helper()
	var out []steveReadSite
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		d := probe.Describe()
		lc, _ := probe.(completion.Completer)
		declares := func(name string) bool {
			return slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == name })
		}
		lines := [][]string{{}}
		if declares("namespace") {
			lines = append(lines, []string{"--namespace", steveReadTypedNamespace})
		}
		if declares("cluster") {
			lines = append(lines, []string{"--cluster", "c-guard"})
		}
		add := func(name string, typed CompletionSite, words []string) {
			for _, line := range lines {
				if typed.FlagName != "" && slices.Contains(line, "--"+typed.FlagName) {
					continue
				}
				tail := slices.Concat(line, words)
				req := completion.Request{
					Command:     d.Name,
					Flag:        typed.FlagName,
					Index:       typed.Index,
					Cluster:     completion.TailFlag(tail, "cluster"),
					Project:     completion.TailFlag(tail, "project"),
					Tail:        tail,
					Positionals: typedPositionals(typed, tail, d.Flags),
				}
				if _, _, ok := claimLiveSource(req, d, lc, flagSources, kindSources, positionalKinds); !ok {
					continue
				}
				out = append(out, steveReadSite{
					name:   name + " [" + strings.Join(words, " ") + "] on [" + strings.Join(line, " ") + "]",
					d:      d,
					lc:     lc,
					site:   typed,
					tail:   tail,
					wiring: clusterValueWiring(req, d, lc),
				})
			}
		}
		for _, f := range d.Flags {
			if !flagTakesValue(f.Name, d.Flags) {
				continue
			}
			for _, words := range wordCombos(d.Args) {
				add(d.Name+" --"+f.Name, CompletionSite{Kind: SiteFlagValue, FlagName: f.Name}, words)
			}
		}
		for n := 0; n <= len(d.Args); n++ {
			for _, words := range wordCombos(d.Args[:n]) {
				add(d.Name+" positional "+strconv.Itoa(n), CompletionSite{Kind: SitePositional, Index: n, Args: words}, words)
			}
		}
	}
	return out
}

// steveReadKubernetesListPath returns the pattern of a Kubernetes collection
// path of k on any cluster: the group and version, the resource, and either no
// namespace or one namespace segment, with no name segment after the resource.
func steveReadKubernetesListPath(k kinds.Kind) *regexp.Regexp {
	prefix := `^/k8s/clusters/[^/]+/api`
	if k.GVR.Group != "" {
		prefix += `s/` + regexp.QuoteMeta(k.GVR.Group)
	}
	return regexp.MustCompile(prefix + `/` + regexp.QuoteMeta(k.GVR.Version) + `/(namespaces/[^/]+/)?` + regexp.QuoteMeta(k.GVR.Resource) + `$`)
}

// TestNoCompletionSiteListsAKindThatSetsSteveReadOnForbiddenThroughKubernetes asserts that no
// completion site of any command lists a management kind that sets
// SteveReadOnForbidden through the Kubernetes API (a member or owner is refused
// that list, and the site would offer nothing), except a site of
// steveReadKubernetesListSites, and that every such kind that completes
// through the generic source has a site that lists it through the Steve API, in
// the row's default namespace and in a typed one. A downstream kind that sets
// the flag is listed through the Kubernetes API, and each site that lists it
// that way, run again with a fixture that responds to that list with 403, then
// requests the Steve collection of the same cluster.
//
// The fake server responds to every list but the clusters collection with no
// items: a source that lists a kind only after it reads an item is not
// reached. The partial word is empty: a source that reads the partial word
// to choose what it lists (the machine config kind a pool names) is not
// reached.
// Not parallel: it sets the process environment and the stored config.
func TestNoCompletionSiteListsAKindThatSetsSteveReadOnForbiddenThroughKubernetes(t *testing.T) {
	fixture := newClusterValueFixture(t)
	liveFixture(t, fixture.srv)

	var readThroughSteve []kinds.Kind
	var viaGenericSource, downstreamViaGenericSource []kinds.Kind
	for _, k := range kinds.All() {
		if !k.SteveReadOnForbidden {
			continue
		}
		readThroughSteve = append(readThroughSteve, k)
		_, bespoke := kindSources[k.Key]
		if _, claims := kindscomplete.KindSource(k.Key).Claim(completion.Request{}); claims && !bespoke {
			if k.Scope == kinds.Downstream {
				downstreamViaGenericSource = append(downstreamViaGenericSource, k)
			} else {
				viaGenericSource = append(viaGenericSource, k)
			}
		}
	}
	require.NotEmpty(t, viaGenericSource)
	require.NotEmpty(t, downstreamViaGenericSource)

	sites := steveReadSites(t)
	require.NotEmpty(t, sites)
	listPaths := map[string]*regexp.Regexp{}
	for _, k := range readThroughSteve {
		listPaths[k.Key] = steveReadKubernetesListPath(k)
	}

	reachedAllowance := map[string]bool{}
	steveRequests := map[string]bool{}
	var violations []string
	// forbiddenLists are the Kubernetes list paths of a downstream kind that
	// sets the flag that a site requested, with the site.
	type forbiddenList struct {
		site steveReadSite
		path string
	}
	var forbiddenLists []forbiddenList
	for _, s := range sites {
		fixture.reset()
		_, owned := liveSourceCandidates(t.Context(), s.site, s.d, s.lc, s.tail, flagSources, kindSources, positionalKinds)
		require.True(t, owned, s.name)
		for _, uri := range fixture.requests() {
			u, err := url.Parse(uri)
			require.NoError(t, err, uri)
			steveRequests[u.Path] = true
			for _, k := range readThroughSteve {
				if !listPaths[k.Key].MatchString(u.Path) {
					continue
				}
				if k.Scope == kinds.Downstream {
					forbiddenLists = append(forbiddenLists, forbiddenList{site: s, path: u.Path})
					continue
				}
				if _, allowed := steveReadKubernetesListSites[s.wiring]; allowed {
					reachedAllowance[s.wiring] = true
					continue
				}
				violations = append(violations, s.name+": lists "+k.Key+" through the Kubernetes API with "+uri+" (wiring "+strconv.Quote(s.wiring)+")")
			}
		}
	}
	require.NotEmpty(t, forbiddenLists, "no site lists a downstream kind that sets SteveReadOnForbidden through the Kubernetes API")
	for _, l := range forbiddenLists {
		fixture.reset()
		fixture.forbidden = func(path string) bool { return path == l.path }
		_, owned := liveSourceCandidates(t.Context(), l.site.site, l.site.d, l.site.lc, l.site.tail, flagSources, kindSources, positionalKinds)
		require.True(t, owned, l.site.name)
		steveRead := strings.Replace(l.path, "/api/v1/", "/v1/", 1)
		assert.Equal(t, []string{l.path, steveRead}, requestPaths(t, fixture.requests()), "%s: after a 403 of %s the site reads the Steve collection of the same cluster", l.site.name, l.path)
	}
	fixture.forbidden = nil
	assert.Empty(t, violations)
	for wiring, reason := range steveReadKubernetesListSites {
		assert.NotEmpty(t, reason, wiring)
		assert.True(t, reachedAllowance[wiring], "the allowed wiring %q is reached by no site that lists a kind that sets SteveReadOnForbidden through the Kubernetes API", wiring)
	}
	for _, k := range viaGenericSource {
		for _, ns := range []string{k.DefaultNamespace, steveReadTypedNamespace} {
			path := "/v1/" + k.SteveType() + "/" + ns
			assert.True(t, steveRequests[path], "no site lists %s through the Steve API at %s", k.Key, path)
		}
	}
}

// requestPaths returns the path of each request URI, in order.
func requestPaths(t *testing.T, uris []string) []string {
	t.Helper()
	var out []string
	for _, uri := range uris {
		u, err := url.Parse(uri)
		require.NoError(t, err, uri)
		out = append(out, u.Path)
	}
	return out
}
