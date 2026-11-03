package cli

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterValueGuard is the typed --cluster value of the guard: a DNS
// subdomain that is not "local", does not start with "c-" and matches no id
// or display name in the cluster list.
const clusterValueGuard = "nope"

// clusterValueSite is one claimed completion site of a command that takes
// --cluster, with the line that reaches it: the typed --cluster value and
// the positional words before the site.
type clusterValueSite struct {
	name string
	// base is the name without the partial word, and partial is the word
	// typed at the site.
	base    string
	partial string
	d       runtime.Descriptor
	lc      completion.Completer
	site    CompletionSite
	tail    []string
	src     completion.Source
	// wiring names where the claiming source is registered.
	wiring string
	reqs   completion.Requirements
}

// clusterValueFixture serves the clusters collection in two pages and
// answers a GET by id for clusterValueGuard with a valid cluster, so a
// request for the value succeeds and the guard cannot pass on a 404.
// Every other path answers one empty body that carries both shapes a
// source reads: items for a Kubernetes list, and data with a revision for
// a steve collection.
type clusterValueFixture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	uris []string
}

func newClusterValueFixture(t *testing.T) *clusterValueFixture {
	t.Helper()
	f := &clusterValueFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.uris = append(f.uris, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case steveClustersPath:
			page, ok := steveClustersPages[r.URL.Query().Get("continue")]
			if !ok {
				http.Error(w, "no such page", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
		case steveClusterNopePath:
			_ = json.NewEncoder(w).Encode(steveFixtureItem(clusterValueGuard, ""))
		default:
			_, _ = w.Write([]byte(`{"items":[],"data":[],"revision":"1"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *clusterValueFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uris = nil
}

func (f *clusterValueFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.uris)
}

// wordCombos returns every line of words the positionals in args can
// carry: each word is one value of the argument's enum when it has one, and
// the placeholder x otherwise.
func wordCombos(args []runtime.ArgSpec) [][]string {
	combos := [][]string{{}}
	for _, a := range args {
		options := a.Enum
		if len(options) == 0 {
			options = []string{"x"}
		}
		var next [][]string
		for _, c := range combos {
			for _, o := range options {
				next = append(next, append(slices.Clone(c), o))
			}
		}
		combos = next
	}
	return combos
}

// clusterValueTakesCluster reports whether the command accepts --cluster.
func clusterValueTakesCluster(d runtime.Descriptor) bool {
	return slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "cluster" })
}

// clusterValuePartials are the words typed at the site: none, and a letter,
// so a source that reads the partial word is reached both ways.
var clusterValuePartials = []string{"", "x"}

// clusterValueProject is the project id typed on the lines that carry
// --project: no project of the fixture has it.
const clusterValueProject = "p-unlisted"

// clusterValueLines returns the typed flags of each line the sites of a
// command run on: --cluster alone and, for a command that takes --project,
// --cluster with --project.
func clusterValueLines(d runtime.Descriptor) [][]string {
	lines := [][]string{{}}
	if runtime.TakesProject(d) {
		lines = append(lines, []string{"--project", clusterValueProject})
	}
	return lines
}

// clusterValueSites lists every site of every command that takes
// --cluster which a completion source claims, built from the commands'
// own descriptors and the sources' own claims: each value-taking flag, and
// each positional from the first to one past the declared arguments. Each
// site is listed once per entry of clusterValuePartials and once per line
// of clusterValueLines. The --project flag site runs on the line without
// --project only, since the site types the flag itself.
func clusterValueSites(t *testing.T, value string) []clusterValueSite {
	t.Helper()
	var out []clusterValueSite
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		d := probe.Describe()
		if !clusterValueTakesCluster(d) {
			continue
		}
		lc, _ := probe.(completion.Completer)

		add := func(name string, typed CompletionSite, words []string) {
			for _, line := range clusterValueLines(d) {
				if typed.FlagName == runtime.FlagProject && len(line) > 0 {
					continue
				}
				tail := slices.Concat([]string{"--cluster", value}, line, words)
				base := name + " [" + strings.Join(words, " ") + "]"
				if len(line) > 0 {
					base += " with " + strings.Join(line, " ")
				}
				for _, partial := range clusterValuePartials {
					site := typed
					site.Partial = partial
					req := completion.Request{
						Flag:        site.FlagName,
						Index:       site.Index,
						Partial:     site.Partial,
						Cluster:     completion.TailFlag(tail, "cluster"),
						Project:     completion.TailFlag(tail, "project"),
						Tail:        tail,
						Positionals: typedPositionals(site, tail, d.Flags),
					}
					src, reqs, ok := claimLiveSource(req, d, lc, flagSources, kindSources, positionalKinds)
					if !ok {
						continue
					}
					out = append(out, clusterValueSite{
						name:    base + " partial " + strconv.Quote(partial),
						base:    base,
						partial: partial,
						d:       d,
						lc:      lc,
						site:    site,
						tail:    tail,
						src:     src,
						wiring:  clusterValueWiring(req, d, lc),
						reqs:    reqs,
					})
				}
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

// packagesDeclaringAClusterNeed returns the packages of liveSourcePackages
// whose non-test files name ClusterResolved or ClusterListing as an
// identifier, read from the source tree. Comments and strings are not
// read.
func packagesDeclaringAClusterNeed(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for pkg := range liveSourcePackages {
		dir := filepath.Join("..", "..", filepath.FromSlash(strings.TrimPrefix(pkg, modulePath+"/")))
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, f.Name()), nil, 0)
			require.NoError(t, err)
			ast.Inspect(parsed, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (id.Name == "ClusterResolved" || id.Name == "ClusterListing") {
					out[pkg] = true
				}
				return true
			})
		}
	}
	return out
}

// clusterValueSilentSites are the claimed sites that send no request on a
// line that holds only --cluster and the words before the site: the guard
// runs them and asserts they stay silent, so a site that starts sending
// leaves this list and is checked for the typed value like every other.
var clusterValueSilentSites = map[string]string{
	"token --auth-provider": "its CompletionTarget names no server on such a line, so the source has none to ask",
}

// clusterValueWiringsWithoutSites are the registered wirings that no
// guarded site reaches, each with the reason: every command that carries
// the wiring refuses --cluster, so no line of it has a typed value, or the
// command's own source claims the site first and is guarded under its
// own wiring.
var clusterValueWiringsWithoutSites = map[string]string{
	"kind managementcluster":            "the commands that carry it take no --cluster",
	"kind user":                         "the commands that carry it take no --cluster",
	"command create cluster":            "the command takes no --cluster",
	"command create user":               "the command takes no --cluster",
	"command auth login":                "the command takes no --cluster",
	"command inspect cluster":           "the command takes no --cluster",
	"command cluster scale":             "the command takes no --cluster",
	"command cluster set-autoscaling":   "the command takes no --cluster",
	"command cluster clear-autoscaling": "the command takes no --cluster",
	"command oidc-client rotate-secret": "the command takes no --cluster",
	"command oidc-client remove-secret": "the command takes no --cluster",

	"flag --cluster on access projects": "the command's own source claims the flag",
}

// clusterValueKindKey returns the kind key whose source claims the first
// positional of the command: the key the positional table names, or the
// kind of a generated verb.
func clusterValueKindKey(d runtime.Descriptor) (string, bool) {
	if key, ok := positionalKinds[d.Name]; ok {
		return key, true
	}
	if isGeneratedVerb(d.Name) {
		if k, found := kindForDescriptor(d); found {
			return k.Key, true
		}
	}
	return "", false
}

// clusterValueKindWiring names the wiring of a kind source: the kind's own
// source when the kind table has one, the generic kind source otherwise.
func clusterValueKindWiring(key string) string {
	if _, bespoke := kindSources[key]; bespoke {
		return "kind " + key
	}
	return "generic kind " + key
}

// clusterValueKindCompletes reports whether the source of the kind offers
// names at all: the generic kind source declines a kind that does not
// complete.
func clusterValueKindCompletes(key string) bool {
	src, bespoke := kindSources[key]
	if !bespoke {
		src = kindscomplete.KindSource(key)
	}
	_, ok := src.Claim(completion.Request{})
	return ok
}

// clusterValueOwnSite names a site of a command's own source: the flag, or
// the position of the positional.
func clusterValueOwnSite(flag string, index int) string {
	if flag != "" {
		return "--" + flag
	}
	return "positional " + strconv.Itoa(index)
}

// clusterValueWiring names the wiring that claims the request, in the order
// claimLiveSource looks: the command's own source at that flag or position,
// the shared flag source of the flag on that command, or the kind source of
// a first positional. It returns "" when no source claims it.
func clusterValueWiring(req completion.Request, d runtime.Descriptor, lc completion.Completer) string {
	if req.Flag == runtime.FlagProject && !runtime.TakesProject(d) {
		return ""
	}
	if lc != nil {
		if src := lc.LiveSource(); src.Claim != nil {
			if _, ok := src.Claim(req); ok {
				return "command " + d.Name + " " + clusterValueOwnSite(req.Flag, req.Index)
			}
		}
	}
	switch {
	case req.Flag != "":
		if src, ok := flagSources[req.Flag]; ok {
			if _, ok := src.Claim(req); ok {
				return "flag --" + req.Flag + " on " + d.Name
			}
		}
	case req.Index == 0:
		if key, ok := clusterValueKindKey(d); ok {
			src, bespoke := kindSources[key]
			if !bespoke {
				src = kindscomplete.KindSource(key)
			}
			if _, ok := src.Claim(req); ok {
				return clusterValueKindWiring(key)
			}
		}
	}
	return ""
}

// registeredClusterValueWirings returns every wiring the surface can claim
// a site with, and for each the number of guarded sites it must have. It is
// built from the commands' descriptors and the wiring tables and not from
// the sites the walk found: each bespoke kind source is registered, and for
// every command that takes --cluster, each value-taking flag and each
// position from the first to one past the declared arguments is asked, with
// every combination of the words an argument can carry typed before it and
// on every line of clusterValueLines, which wiring claims it. A wiring
// counts one site per partial word for each combination and line that it
// claims. A command that refuses --cluster has one wiring for its whole
// source and no site.
//
// The request carries the flag or position, the words before it and the
// typed project. The claims read the request through Flag, Index and
// Positionals, and through the typed flags of the tail with TailFlag; the
// lines the guard builds hold no flag in the tail but --cluster and
// --project, and none of the claims depends on either.
func registeredClusterValueWirings() map[string]int {
	out := map[string]int{}
	for key := range kindSources {
		out["kind "+key] += 0
	}
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		d := probe.Describe()
		lc, _ := probe.(completion.Completer)
		if !clusterValueTakesCluster(d) {
			if lc != nil && lc.LiveSource().Claim != nil {
				out["command "+d.Name] += 0
			}
			continue
		}
		for _, f := range d.Flags {
			if _, shared := flagSources[f.Name]; shared && flagTakesValue(f.Name, d.Flags) {
				out["flag --"+f.Name+" on "+d.Name] += 0
			}
		}
		if key, ok := clusterValueKindKey(d); ok && clusterValueKindCompletes(key) {
			out[clusterValueKindWiring(key)] += 0
		}
		count := func(req completion.Request, line []string) {
			if req.Flag == runtime.FlagProject && len(line) > 0 {
				return
			}
			if len(line) > 0 {
				req.Project = clusterValueProject
			}
			if w := clusterValueWiring(req, d, lc); w != "" {
				out[w] += len(clusterValuePartials)
			}
		}
		for _, line := range clusterValueLines(d) {
			for _, f := range d.Flags {
				if !flagTakesValue(f.Name, d.Flags) {
					continue
				}
				for _, words := range wordCombos(d.Args) {
					count(completion.Request{Flag: f.Name, Positionals: words}, line)
				}
			}
			for n := 0; n <= len(d.Args); n++ {
				for _, words := range wordCombos(d.Args[:n]) {
					count(completion.Request{Index: n, Positionals: words}, line)
				}
			}
		}
	}
	return out
}

// TestCompletionSendsNoClusterValueNoListedClusterCarries asserts the rule
// for every claimed site of every command that takes --cluster: when the
// typed --cluster value is not "local", does not start with "c-" and
// matches no id or display name in the cluster list, no completion request
// carries the value in its path or query, though the server would answer a
// GET by id for it. A site whose source needs a resolved cluster reads the
// cluster list and nothing else. A "c-" or "local" value is a cluster id
// the line names and takes no resolution request (livedispatch_test.go
// asserts that); the source's own request then carries it as one path
// segment, and this test does not cover it.
//
// The lines it types are --cluster with the value, alone and, for a command
// that takes --project, with --project and an unlisted project id; the
// words before the site, in every combination the arguments allow; and at
// the site an empty partial word and a one-letter one. Other flags typed
// on the line (--namespace, --node, --as and the command's own) are not
// varied, so a source that sends the value only when one of them is typed
// is not reached. The fake server answers every list but the clusters
// collection with no items: a request a source sends only after it reads
// an item is not reached either.
// Not parallel: it sets the process environment and the stored config.
func TestCompletionSendsNoClusterValueNoListedClusterCarries(t *testing.T) {
	const value = clusterValueGuard
	require.False(t, isClusterIDShaped(value))
	for _, page := range steveClustersPages {
		for _, item := range page.Data {
			assert.NotEqual(t, value, item["id"], "the fixture lists the value as an id")
			spec, _ := item["spec"].(map[string]any)
			assert.NotEqual(t, value, spec["displayName"], "the fixture lists the value as a display name")
		}
	}

	require.Equal(t, steveClusterNopePath, steveClustersPath+"/"+value)

	fixture := newClusterValueFixture(t)
	liveFixture(t, fixture.srv)

	sites := clusterValueSites(t, value)
	require.NotEmpty(t, sites)

	wantResolution := []string{steveClustersPath, steveClustersPath + "?continue=n"}
	resolvedPackages := map[string]bool{}
	guardedByWiring := map[string]int{}
	seenSilent := map[string]bool{}
	require.Contains(t, clusterValuePartials, "", "a site runs with no partial word")
	require.True(t, slices.ContainsFunc(clusterValuePartials, func(p string) bool { return p != "" }), "a site runs with a partial word")
	partialsRun := map[string][]string{}
	var guarded, resolved, clusterNone, withProject int
	var violations []string
	for _, s := range sites {
		fixture.reset()
		_, owned := liveSourceCandidates(t.Context(), s.site, s.d, s.lc, s.tail, flagSources, kindSources, positionalKinds)
		require.True(t, owned, s.name)
		requests := fixture.requests()
		guarded++
		guardedByWiring[s.wiring]++
		if slices.Contains(s.tail, "--project") {
			withProject++
		}
		partialsRun[s.base] = append(partialsRun[s.base], s.partial)
		key := s.d.Name + " --" + s.site.FlagName
		if _, ok := clusterValueSilentSites[key]; ok {
			seenSilent[key] = true
			if len(requests) > 0 {
				violations = append(violations, s.name+": a listed silent site sends "+strings.Join(requests, " "))
			}
		}
		if s.reqs.Cluster == completion.ClusterNone {
			clusterNone++
		}
		for _, uri := range requests {
			if strings.Contains(uri, value) {
				violations = append(violations, s.name+": request "+uri)
			}
		}
		if s.reqs.Cluster != completion.ClusterResolved {
			continue
		}
		resolved++
		resolvedPackages[declaringPackage(s.src.Claim)] = true
		if !slices.Equal(wantResolution, requests) {
			violations = append(violations, s.name+": requests "+strings.Join(requests, " "))
		}
	}
	assert.Empty(t, violations)
	for base, ran := range partialsRun {
		assert.ElementsMatch(t, clusterValuePartials, ran, "%s runs once with each partial word", base)
	}
	assert.Positive(t, guarded)
	assert.Positive(t, withProject, "a site runs on a line with --project typed")
	assert.Positive(t, clusterNone, "a site that needs no resolved cluster is guarded too")

	for key := range clusterValueSilentSites {
		assert.True(t, seenSilent[key], "the list of silent sites names a site no source claims: %s", key)
	}

	registered := registeredClusterValueWirings()
	for wiring, want := range registered {
		got := guardedByWiring[wiring]
		if reason, listed := clusterValueWiringsWithoutSites[wiring]; listed {
			assert.Zero(t, got, "%s has guarded sites and is listed without them (%s)", wiring, reason)
			assert.Zero(t, want, "%s is listed without sites and the surface can claim %d (%s)", wiring, want, reason)
			continue
		}
		if assert.Positive(t, got, "%s has no guarded site", wiring) {
			assert.Equal(t, want, got, "%s has %d guarded sites and the surface can claim %d", wiring, got, want)
		}
	}
	for wiring := range clusterValueWiringsWithoutSites {
		_, ok := registered[wiring]
		assert.True(t, ok, "the list of wirings without sites names one nothing registers: %s", wiring)
	}
	for wiring := range guardedByWiring {
		_, ok := registered[wiring]
		assert.True(t, ok, "a guarded site is claimed by %s, which nothing registers", wiring)
	}

	registeredList := slices.Collect(maps.Keys(registered))
	for flag := range flagSources {
		assert.True(t, slices.ContainsFunc(registeredList, func(w string) bool {
			return strings.HasPrefix(w, "flag --"+flag+" on ")
		}), "no command that takes --cluster carries the shared source of --%s", flag)
	}

	declaring := packagesDeclaringAClusterNeed(t)
	require.NotEmpty(t, declaring)
	for pkg := range declaring {
		assert.True(t, resolvedPackages[pkg], "package %s declares a cluster need and no site of it is guarded", pkg)
	}
	for pkg := range resolvedPackages {
		assert.True(t, declaring[pkg], "package %s has a guarded resolved site and no cluster need in its source", pkg)
	}
	t.Logf("guarded %d sites, %d with a resolved cluster, %d with none, in %d packages", guarded, resolved, clusterNone, len(resolvedPackages))
}

// TestCommandResolvesAnUnlistedClusterThroughTheGetByID asserts the
// factory every command's --cluster resolves through, as a factory-level
// test and not a command run: a value the cluster list does not show yet,
// without a "c-" prefix, resolves through one GET by id after the list
// read.
func TestCommandResolvesAnUnlistedClusterThroughTheGetByID(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{
		items: []map[string]any{steveClusterItem("c-abc", "prod")},
		byID:  map[string]int{"fresh-cluster": http.StatusOK},
	}
	f := resolveFactory(t, root)
	got, err := f.ResolveCluster(t.Context(), "fresh-cluster")
	require.NoError(t, err)
	assert.Equal(t, "fresh-cluster", got)
	assert.Equal(t, []string{steveClustersPath, steveClustersPath + "/fresh-cluster"}, root.requests())
}
