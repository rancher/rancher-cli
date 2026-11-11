package observe

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The names of the reads a fake server answers or fails. A read the fake
// serves has no entry in its reads value.
const (
	readDiscovery     = "discovery"
	readSettings      = "settings"
	readFeatures      = "features"
	readClusterObject = "clusterObject"
	readNodes         = "nodes"
	readPods          = "pods"
	readEvents        = "events"
)

var allReads = []string{readDiscovery, readSettings, readFeatures, readClusterObject, readNodes, readPods, readEvents}

// reads maps the name of a read to the error the fake server answers it with.
type reads map[string]error

// failAll fails every read with err.
func failAll(err error) reads {
	r := reads{}
	for _, name := range allReads {
		r[name] = err
	}
	return r
}

// failExcept fails every read but the named ones with err.
func failExcept(err error, served ...string) reads {
	r := failAll(err)
	for _, name := range served {
		delete(r, name)
	}
	return r
}

// failOnly fails the named reads with err and serves the others.
func failOnly(err error, failed ...string) reads {
	r := reads{}
	for _, name := range failed {
		r[name] = err
	}
	return r
}

// statusFailure is the error a request answered with status gives.
func statusFailure(status int) error {
	return clierr.FromResponse("rancher GET /v1/example", http.MethodGet, status, nil, &clierr.Request{})
}

// exitFactory is a server that answers each read as r says: the local cluster
// and the downstream cluster c-1 (which has an authorized cluster endpoint)
// serve one pod named p-1 in cattle-system.
func exitFactory(r reads) *fakeFactory {
	newClient := func() *fakeClient {
		c := newFakeClient().
			withListNS(podKind.GVR, "cattle-system", podList("p-1")).
			withLog("main", "log text\n")
		if err := r[readSettings]; err != nil {
			c.withDeniedKind(settingKind.GVR, err)
		}
		if err := r[readFeatures]; err != nil {
			c.withDeniedKind(featureKind.GVR, err)
		}
		if err := r[readNodes]; err != nil {
			c.withDeniedKind(nodeKind.GVR, err)
		}
		if err := r[readPods]; err != nil {
			c.withDeniedKind(podKind.GVR, err).withDeniedKind(deployKind.GVR, err).
				withDeniedKind(provKind.GVR, err).withDeniedKind(fleetKind.GVR, err)
		}
		if err := r[readEvents]; err != nil {
			c.withDeniedKind(eventKind.GVR, err)
		}
		return c
	}
	root := &fakeRoot{}
	for _, id := range []string{engine.LocalCluster, "c-1"} {
		root.withCluster(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": id},
			"spec":     map[string]any{"localClusterAuthEndpoint": map[string]any{"enabled": true}},
		}})
		if err := r[readClusterObject]; err != nil {
			root.withError(clusterPath(id), err)
		}
	}
	if err := r[readDiscovery]; err != nil {
		root.withError((steve.Request{Type: steve.ClustersType}).Path(), err)
	}
	return &fakeFactory{
		local:      newClient(),
		root:       root,
		perCluster: map[string]engine.ClusterClient{"c-1": newClient()},
	}
}

// runExit runs support-bundle on factory and returns the result, the archive
// path and the error. An archive that exists is read: the names it holds are
// local names.
func runExit(t *testing.T, ctx context.Context, factory *fakeFactory, clusters, targets string, opts ...func(*supportBundleCmd)) (runtime.Result, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	cmd := NewSupportBundle(factory, factory, func() any { return map[string]string{"cliVersion": "v9.9.9"} }).(*supportBundleCmd)
	for _, opt := range opts {
		opt(cmd)
	}
	res, err := cmd.Execute(ctx, input{s: map[string]string{flagOutputFile: path, flagClusters: clusters, flagTargets: targets}}, nil)
	if _, statErr := os.Stat(path); statErr == nil {
		requireLocalNames(t, tarEntryNames(t, path))
	}
	return res, path, err
}

// exitKind is one kind of manifest item and what the rule does with it.
type exitKind struct {
	// counted is true when a collected item of the kind keeps a run from
	// failing and a failed one is among the failures.
	counted bool
	// reads are the reads that make the item collected; none for a kind that
	// has no read of the server.
	reads []string
	// clusters and targets are the flags of the rows of the kind.
	clusters, targets string
	// failureOnly is true for a kind whose item exists only when it failed.
	failureOnly bool
	// item is the item (name, cluster) of the kind in the rows of the kind.
	item [2]string
}

// exitKinds is every kind of manifest item. A target is a kind of its own.
// A test below maps each item of the runs of every shape to a kind; an item
// with no row here fails it.
var exitKinds = map[string]exitKind{
	"cluster-discovery":    {counted: true, failureOnly: true},
	"settings":             {reads: []string{readSettings}},
	"features":             {counted: true, reads: []string{readFeatures}, item: [2]string{"rancher/features.json", ""}},
	"schema":               {},
	"version":              {},
	"cluster":              {counted: true, failureOnly: true},
	"cluster.json":         {counted: true, reads: []string{readClusterObject}, item: [2]string{"cluster.json", "local"}},
	"nodes.json":           {counted: true, reads: []string{readNodes}, item: [2]string{"nodes.json", "local"}},
	"target server":        {counted: true, reads: []string{readPods}, clusters: "local", targets: "server", item: [2]string{"server", "local"}},
	"target agent":         {counted: true, reads: []string{readPods}, clusters: "c-1", targets: "agent", item: [2]string{"agent", "c-1"}},
	"target fleet-agent":   {counted: true, reads: []string{readPods}, clusters: "local", targets: "fleet-agent", item: [2]string{"fleet-agent", "local"}},
	"target kube-api-auth": {counted: true, reads: []string{readPods, readClusterObject}, clusters: "c-1", targets: "kube-api-auth", item: [2]string{"kube-api-auth", "c-1"}},
	"target webhook":       {counted: true, reads: []string{readPods}, clusters: "local", targets: "webhook", item: [2]string{"webhook", "local"}},
	"target api-extension": {counted: true, reads: []string{readPods}, clusters: "local", targets: "api-extension", item: [2]string{"api-extension", "local"}},
	"events":               {counted: true, reads: []string{readEvents}, item: [2]string{"events-cattle-system", "local"}},
}

// exitKindOf is the kind of a manifest item.
func exitKindOf(name string) (string, bool) {
	switch name {
	case "cluster-discovery", "cluster", "cluster.json", "nodes.json":
		return name, true
	case "rancher/settings.json":
		return "settings", true
	case "rancher/features.json":
		return "features", true
	case "rancher/schema.json":
		return "schema", true
	case "rancher/version.json":
		return "version", true
	}
	if slices.Contains(Names(), name) {
		return "target " + name, true
	}
	if strings.HasPrefix(name, "events-") {
		return "events", true
	}
	return "", false
}

// TestSupportBundleEveryItemHasAKindInTheExitTable asserts the table of kinds
// matches what a run makes: runs of every target over the local cluster and a
// downstream cluster, of a failed discovery, of an unsafe cluster id and of an
// unsafe namespace give items that each map to a kind, and every kind is made
// by one of them. An item added without its row, or a row no run makes, fails.
// Mutation: add an item that is always collected and counted, with no row.
func TestSupportBundleEveryItemHasAKindInTheExitTable(t *testing.T) {
	t.Parallel()
	fleet, _ := fleetAgentBundleFactory("a/b")
	runs := []struct {
		name              string
		factory           *fakeFactory
		clusters, targets string
	}{
		{"every target on two clusters", exitFactory(nil), "local,c-1", ""},
		{"a failed discovery", exitFactory(failOnly(statusFailure(530), readDiscovery)), "", "webhook"},
		{"an unsafe cluster id", exitFactory(nil), "local,a/b", "webhook"},
		{"an unsafe namespace", fleet, "c-1", "fleet-agent"},
	}
	produced := map[string]bool{}
	for _, run := range runs {
		_, path, err := runExit(t, t.Context(), run.factory, run.clusters, run.targets)
		require.NoError(t, err, run.name)
		for _, it := range manifestFrom(t, path).Items {
			kind, ok := exitKindOf(it.Name)
			if assert.True(t, ok, "%s: the item %q has no kind", run.name, it.Name) {
				_, listed := exitKinds[kind]
				assert.True(t, listed, "%s: the kind %q of the item %q has no row", run.name, kind, it.Name)
				produced[kind] = true
			}
		}
	}
	for kind := range exitKinds {
		assert.True(t, produced[kind], "no run makes an item of the kind %q", kind)
	}
	for _, name := range Names() {
		assert.Contains(t, exitKinds, "target "+name)
	}
}

// exitRow is one row of the table that asserts when a run fails.
type exitRow struct {
	name              string
	factory           *fakeFactory
	clusters, targets string
	opts              []func(*supportBundleCmd)

	// wantCode is the code of the error; empty when the run succeeds.
	wantCode string
	// collected names the items (name, cluster) whose status is not an error
	// when the run succeeds, and failed those whose status is.
	collected, failed [][2]string
}

func exitRows() []exitRow {
	var rows []exitRow
	row := func(r exitRow) { rows = append(rows, r) }
	const local, webhook = "local", "webhook"

	row(exitRow{name: "nothing failed", factory: exitFactory(nil), clusters: local, targets: webhook,
		collected: [][2]string{{"rancher/features.json", ""}, {"cluster.json", local}, {"nodes.json", local}, {webhook, local}}})

	for _, kind := range slices.Sorted(maps.Keys(exitKinds)) {
		k := exitKinds[kind]
		clusters, targets := cmp.Or(k.clusters, local), cmp.Or(k.targets, webhook)
		switch {
		case k.counted && !k.failureOnly:
			row(exitRow{name: "one failed alone: " + kind, factory: exitFactory(failOnly(statusFailure(530), k.reads...)), clusters: clusters, targets: targets,
				failed: [][2]string{k.item}})
			row(exitRow{name: "one succeeded alone: " + kind, factory: exitFactory(failExcept(statusFailure(530), k.reads...)), clusters: clusters, targets: targets,
				collected: [][2]string{k.item}})
		case !k.counted && len(k.reads) > 0:
			row(exitRow{name: "an item that is not counted collected alone: " + kind,
				factory: exitFactory(failExcept(statusFailure(530), k.reads...)), clusters: clusters, targets: targets, wantCode: clierr.CodeGeneric})
		}
	}

	certificate := certificateFailure()
	for _, f := range []struct {
		name string
		err  error
		code string
	}{
		{"530", statusFailure(530), clierr.CodeGeneric},
		{"401", statusFailure(401), clierr.CodeAuth},
		{"403", statusFailure(403), clierr.CodeRBAC},
		{"certificate", certificate, clierr.CodeTLSTrust},
	} {
		row(exitRow{name: "every item failed: " + f.name, factory: exitFactory(failAll(f.err)), clusters: local, targets: webhook, wantCode: f.code})
		row(exitRow{name: "every item of every cluster failed: " + f.name, factory: exitFactory(failAll(f.err)), clusters: "local,c-1", wantCode: f.code})
		row(exitRow{name: "every item failed with the default clusters: " + f.name, factory: exitFactory(failAll(f.err)), wantCode: f.code})
	}
	mixed := failAll(statusFailure(401))
	mixed[readNodes] = statusFailure(403)
	row(exitRow{name: "every item failed with two codes", factory: exitFactory(mixed), clusters: local, targets: webhook, wantCode: clierr.CodeGeneric})
	row(exitRow{name: "every item failed and the schema failed", factory: exitFactory(failAll(statusFailure(530))), clusters: local, targets: webhook,
		wantCode: clierr.CodeGeneric,
		opts:     []func(*supportBundleCmd){failingSchema}})
	row(exitRow{name: "every item failed with 403 and the schema failed", factory: exitFactory(failAll(statusFailure(403))), clusters: local, targets: webhook,
		wantCode: clierr.CodeRBAC, opts: []func(*supportBundleCmd){failingSchema}})
	row(exitRow{name: "every item failed and every target was skipped", factory: exitFactory(failAll(statusFailure(530))), clusters: local, targets: "agent", wantCode: clierr.CodeGeneric})

	row(exitRow{name: "a target skipped on a cluster without an authorized cluster endpoint is an answer",
		factory: exitFactory(failAll(statusFailure(530))), clusters: "c-1", targets: "kube-api-auth",
		opts:      []func(*supportBundleCmd){nonACEAfterTheFirstRead},
		collected: [][2]string{{"kube-api-auth", "c-1"}}})

	partial := exitFactory(failExcept(statusFailure(530), readPods))
	partial.local = newFakeClient().
		withListNS(podKind.GVR, "cattle-system", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{unstructuredPodWithContainers("p-1", "main", "side")}}).
		withDeniedKind(featureKind.GVR, statusFailure(530)).withDeniedKind(nodeKind.GVR, statusFailure(530)).withDeniedKind(eventKind.GVR, statusFailure(530)).
		withLog("main", "log text\n").withLogErr("side", statusFailure(530))
	row(exitRow{name: "a target with one of two logs collected is collected", factory: partial, clusters: local, targets: webhook,
		failed: [][2]string{{webhook, local}}})

	offline := exitFactory(nil)
	offline.resolve = map[string]string{"prod": "c-abc"}
	offline.perClusterErr = map[string]error{"c-abc": statusFailure(530)}
	row(exitRow{name: "a requested cluster that fails while the local cluster answers", factory: offline, clusters: "local,prod", targets: "webhook,agent",
		collected: [][2]string{{"rancher/features.json", ""}, {"cluster.json", local}},
		failed:    [][2]string{{"cluster.json", "c-abc"}, {"nodes.json", "c-abc"}, {"agent", "c-abc"}}})
	return rows
}

// failingSchema makes the schema fail to marshal.
func failingSchema(c *supportBundleCmd) { c.schema = func() any { return make(chan int) } }

// nonACEAfterTheFirstRead gives the server a downstream cluster whose
// management Cluster fails the first read of it and then answers, without an
// authorized cluster endpoint: cluster.json fails, and the target that needs
// the endpoint is skipped.
func nonACEAfterTheFirstRead(c *supportBundleCmd) {
	inner := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"}, "spec": map[string]any{},
	}})
	c.roots = rootOf{root: &failFirstRead{fakeRoot: inner, err: statusFailure(530), seen: map[string]bool{}}}
}

type rootOf struct{ root engine.RootClient }

func (r rootOf) Root() (engine.RootClient, error) { return r.root, nil }

// failFirstRead fails the first GET of each path and then answers it.
type failFirstRead struct {
	*fakeRoot
	err  error
	mu   sync.Mutex
	seen map[string]bool
}

func (r *failFirstRead) GetJSON(ctx context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	first := !r.seen[path]
	r.seen[path] = true
	r.mu.Unlock()
	if first {
		return nil, r.err
	}
	return r.fakeRoot.GetJSON(ctx, path)
}

// errorStatuses is how many manifest items carry an error status, and
// whether the item (name, cluster) does.
func errorStatuses(m bundleManifest) (count int, byItem map[[2]string]bool) {
	byItem = map[[2]string]bool{}
	for _, it := range m.Items {
		failed := isErrorStatus(it.Status)
		byItem[[2]string{it.Name, it.Cluster}] = failed
		if failed {
			count++
		}
	}
	return count, byItem
}

// errorEnvelope is the error as an agent reads it: its JSON form.
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
	Details   struct {
		OutputFile string          `json:"outputFile"`
		Attempted  int             `json:"attempted"`
		Failed     int             `json:"failed"`
		First      json.RawMessage `json:"first"`
	} `json:"details"`
}

func decodeError(t *testing.T, err error) (*clierr.Error, errorEnvelope) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "the error is a structured error: %v", err)
	raw, merr := json.Marshal(ce)
	require.NoError(t, merr)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))
	return ce, env
}

// TestSupportBundleFailsWhenNoServerItemWasCollected is the guard of the rule:
// a run that collects no item from the server ends with an error whose code is
// the one every failure shares, GENERIC when they differ, and leaves the
// archive with every failure in its manifest; a run that collects any
// attempted item succeeds, whatever else failed. One table holds every row:
// each kind of item failing alone and succeeding alone, every item failing
// with each failure a server gives, and the shapes between. A kind collected
// alone that is not counted (the settings file) fails the run. The rule has a
// bypass the table does not see: an item whose kind no run of the
// completeness test makes is not seen.
// Mutations: return the result when the rule fires; count a skipped target as
// collected; leave events out of the count; count the settings file; count a
// target with a log in the archive as failed.
func TestSupportBundleFailsWhenNoServerItemWasCollected(t *testing.T) {
	t.Parallel()
	for _, row := range exitRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			res, path, err := runExit(t, t.Context(), row.factory, row.clusters, row.targets, row.opts...)
			m := manifestFrom(t, path)
			errored, byItem := errorStatuses(m)

			if row.wantCode == "" {
				require.NoError(t, err)
				data, ok := res.Data.(bundleResult)
				require.True(t, ok)
				assert.Equal(t, errored, data.Errors)
				for _, id := range row.collected {
					failed, found := byItem[id]
					assert.True(t, found && !failed, "the item %v is collected", id)
				}
				for _, id := range row.failed {
					assert.True(t, byItem[id], "the item %v failed", id)
				}
				return
			}

			ce, env := decodeError(t, err)
			assert.Equal(t, row.wantCode, ce.Code)
			assert.NotZero(t, clierr.ExitCode(ce.Code), "the code exits non-zero")
			assert.Equal(t, path, env.Details.OutputFile)
			// An item of a kind the rule does not count (the settings and
			// schema files) can carry an error status; it is in the manifest's
			// error count but is neither attempted nor failed.
			counted := errored
			for _, it := range m.Items {
				kind, ok := exitKindOf(it.Name)
				require.True(t, ok, "the item %q has no kind", it.Name)
				if isErrorStatus(it.Status) && !exitKinds[kind].counted {
					counted--
				}
			}
			assert.Equal(t, counted, env.Details.Attempted, "every attempted item failed")
			assert.Equal(t, counted, env.Details.Failed)
			assert.Contains(t, ce.Message, "is kept at "+path)
			assert.Nil(t, res.Data, "a failed run has no result")
		})
	}
}

// TestSupportBundleErrorOfNoServerItemKeepsTheFirstFailure asserts the error
// carries the first failure's classification, hint, retryability and details,
// names the counts and the archive path, ends with the first failure's own
// message, and rides the warnings of the run.
func TestSupportBundleErrorOfNoServerItemKeepsTheFirstFailure(t *testing.T) {
	t.Parallel()
	certificate := clierr.New(clierr.CodeTLSTrust, "the server presented a certificate signed by an unknown authority; "+
		"save it for this entry with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>").
		WithHint("trust the certificate authority").
		WithDetails(tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Fingerprint: "ab:cd"})
	retryable := statusFailure(530)
	retryable = clierr.From(retryable).WithMessage(retryable.Error()).WithRetryable(true)

	for _, tc := range []struct {
		name    string
		failure error
		code    string
	}{
		{"530", retryable, clierr.CodeGeneric},
		{"401", statusFailure(401), clierr.CodeAuth},
		{"403", statusFailure(403), clierr.CodeRBAC},
		{"certificate", certificate, clierr.CodeTLSTrust},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			factory := exitFactory(failAll(tc.failure))
			_, path, err := runExit(t, t.Context(), factory, "local", "webhook")
			ce, env := decodeError(t, err)
			first := clierr.From(tc.failure)

			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, first.Retryable, env.Retryable)
			assert.Equal(t, first.Hint, env.Hint)
			assert.True(t, strings.HasSuffix(ce.Message, tc.failure.Error()), "the first failure's message is last: %q", ce.Message)
			assert.Contains(t, ce.Message, `"rancher/features.json"`)
			assert.Contains(t, ce.Message, "all 5 items it attempted failed")
			assert.Equal(t, path, env.Details.OutputFile)
			if first.Details != nil {
				want, merr := json.Marshal(first.Details)
				require.NoError(t, merr)
				assert.JSONEq(t, string(want), string(env.Details.First))
			} else {
				assert.Empty(t, env.Details.First)
			}
			assert.Contains(t, readArchive(t, path), "manifest.json")
		})
	}
}

// TestNoServerItemErrorShowsHostileValuesWithEscapes asserts an item name and
// a cluster id reach the message only through quoteValue: a control
// character, a bidirectional mark and a leading dash show as escapes or
// inside quotes, and a long value is cut.
func TestNoServerItemErrorShowsHostileValuesWithEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value, shown, absent string }{
		{"newline", "a\nb", `"a\nb"`, "\n"},
		{"right-to-left override", "a\u202eb", `"a\u202eb"`, "\u202e"},
		{"leading dash", "-rf", `"-rf"`, ""},
		{"over 64 bytes", strings.Repeat("a", 99) + "/b", `"` + strings.Repeat("a", 64) + `"...`, strings.Repeat("a", 65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failed := []itemFailure{{name: tc.value, cluster: tc.value, err: statusFailure(530)}}
			ce := noServerItemError("b.tar.gz", 1, failed)
			assert.Equal(t, 2, strings.Count(ce.Message, tc.shown), "the item and the cluster are quoted: %q", ce.Message)
			if tc.absent != "" {
				assert.NotContains(t, ce.Message, tc.absent)
			}
		})
	}
}

// TestNoServerItemErrorNamesTheWholePath asserts the archive path is in the
// message of the rule and of an interrupted run as the operator typed it, once,
// whatever its length and whatever separator it uses, and that a path with a
// rune the CLI never shows raw appears with escapes. The expected texts are
// written out.
func TestNoServerItemErrorNamesTheWholePath(t *testing.T) {
	t.Parallel()
	long := "/home/operator/support/" + strings.Repeat("d", 80) + "/bundle.tar.gz"
	failed := []itemFailure{{name: "nodes.json", cluster: "local", err: statusFailure(530)}}
	for _, tc := range []struct{ name, path, shown string }{
		{"over 64 bytes", long, long},
		{"windows separators", `C:\Users\op\support\b.tar.gz`, `C:\Users\op\support\b.tar.gz`},
		{"with a space", "my bundle.tar.gz", "my bundle.tar.gz"},
		{"newline", "a\nb.tar.gz", `"a\nb.tar.gz"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule := noServerItemError(tc.path, 1, failed)
			assert.Equal(t, 1, strings.Count(rule.Message, tc.shown), "%q", rule.Message)
			assert.Contains(t, rule.Message, "the archive is kept at "+tc.shown+" and its manifest.json lists each failure")
			stopped := interruptedError(tc.path)
			assert.Equal(t, "support-bundle stopped before it finished; the archive of what was collected until then is kept at "+tc.shown, stopped.Message)
		})
	}
}

// TestSupportBundleCountsOnlyItemsReadFromTheServer asserts the counts of the
// error leave out the items that never went to the server, and that such an
// item never changes the code: an item of a cluster id that is not a plain file
// name, and the events of a namespace that is not one. The code is the one the
// server's failures share. Mutation: build either item as a server item.
func TestSupportBundleCountsOnlyItemsReadFromTheServer(t *testing.T) {
	t.Parallel()
	denied := statusFailure(403)

	t.Run("an unsafe cluster id", func(t *testing.T) {
		t.Parallel()
		_, path, err := runExit(t, t.Context(), exitFactory(failAll(denied)), "local,a/b", "webhook")
		ce, env := decodeError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, 5, env.Details.Attempted, "features, cluster.json, nodes.json, webhook and events of the local cluster")
		assert.Equal(t, 5, env.Details.Failed)
		assert.Contains(t, ce.Message, "all 5 items it attempted failed")
		errored, byItem := errorStatuses(manifestFrom(t, path))
		assert.Equal(t, 7, errored, "the 5 failed items, the unsafe cluster id and the settings item, which reads 403 and is not attempted")
		assert.True(t, byItem[[2]string{"rancher/settings.json", ""}])
		assert.True(t, byItem[[2]string{"cluster", "a/b"}])
	})

	t.Run("an unsafe namespace", func(t *testing.T) {
		t.Parallel()
		factory, downstream := fleetAgentBundleFactory("a/b")
		factory.root.withError(clusterPath("c-1"), denied)
		local := factory.local.(*fakeClient)
		local.withDeniedKind(featureKind.GVR, denied).withDeniedKind(nodeKind.GVR, denied)
		downstream.withDeniedKind(nodeKind.GVR, denied).withDeniedKind(eventKind.GVR, denied).withLogErr("main", denied)
		_, path, err := runExit(t, t.Context(), factory, "c-1", "fleet-agent")
		ce, env := decodeError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, 5, env.Details.Attempted, "features, cluster.json, nodes.json, fleet-agent and the events of cattle-system")
		assert.Equal(t, 5, env.Details.Failed)
		errored, byItem := errorStatuses(manifestFrom(t, path))
		assert.Equal(t, 6, errored)
		assert.True(t, byItem[[2]string{"events-a/b", "c-1"}])
	})

	t.Run("the counts of a list of items", func(t *testing.T) {
		t.Parallel()
		items := []manifestItem{
			serverItem("rancher/features.json", "", statusFailure(403)),
			serverItem("nodes.json", "c-1", nil),
			localItem("cluster", "a/b", errors.New("not a plain file name")),
			outsideItem("rancher/schema.json", nil),
		}
		attempted, failed := serverOutcome(items)
		assert.Equal(t, 2, attempted)
		require.Len(t, failed, 1)
		assert.Equal(t, "rancher/features.json", failed[0].name)
	})
}

// TestSupportBundleInterruptedRunKeepsAndNamesTheArchive asserts a run whose
// context ended closes the archive, keeps it with its manifest, and returns an
// error that names the path in its message and in its details and does not
// wrap context.Canceled, whether the server still answered or the requests
// failed with the cancellation. When the requests fail, the manifest reads the
// settings item as an error and the archive has no settings file. Mutation:
// drop the context check.
func TestSupportBundleInterruptedRunKeepsAndNamesTheArchive(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]reads{
		"the server answers":              nil,
		"the requests fail as canceled":   failAll(context.Canceled),
		"the requests fail with a status": failAll(statusFailure(530)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			res, path, err := runExit(t, ctx, exitFactory(r), "local", "webhook")
			require.Error(t, err)
			require.NotErrorIs(t, err, context.Canceled, "the pipeline keeps the message of an error that does not wrap the cancellation")
			ce, env := decodeError(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Contains(t, ce.Message, "is kept at "+path)
			assert.Equal(t, path, env.Details.OutputFile)
			assert.Nil(t, res.Data)
			archive := readArchive(t, path)
			assert.Contains(t, archive, "manifest.json")
			settings := findItem(t, manifestFrom(t, path), "rancher/settings.json", "")
			if r == nil {
				assert.Equal(t, statusCollected, settings.Status)
				return
			}
			assert.True(t, strings.HasPrefix(settings.Status, "error: "+settingsNotRead(settingsAllowlist)+"; first failure: "), settings.Status)
			assert.True(t, strings.HasSuffix(settings.Status, clierr.ExpandRemedies(r[readSettings].Error(), "")), settings.Status)
			assert.NotContains(t, archive, "rancher/settings.json")
		})
	}
}

// failToWrite makes the archive fail when it is closed.
func failToWrite(c *supportBundleCmd) {
	c.create = func(path string) (*ArchiveWriter, error) {
		return createArchiveWith(path, func(io.Writer) io.Writer { return failingWriter{err: errors.New("disk full")} })
	}
}

// TestSupportBundleReturnsTheCloseErrorWhenTheArchiveCannotBeWritten
// asserts that when the archive cannot be written the error is the write
// failure, with the server's first
// failure in its message, and no archive is left or named as kept.
func TestSupportBundleReturnsTheCloseErrorWhenTheArchiveCannotBeWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		r         reads
		cancelled bool
		want      string
	}{
		{"no item collected", failAll(statusFailure(403)), false, statusFailure(403).Error()},
		{"interrupted", nil, true, "stopped before it finished"},
		{"a run that collected items", nil, false, "disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			if !tc.cancelled {
				defer cancel()
			} else {
				cancel()
			}
			_, path, err := runExit(t, ctx, exitFactory(tc.r), "local", "webhook", failToWrite)
			require.ErrorContains(t, err, "disk full")
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "is kept at")
			assert.NotContains(t, err.Error(), path)
			assert.NoFileExists(t, path)
		})
	}
}

// TestSupportBundleLogOutcomeItemOutcome asserts the part a target takes in
// the rule: collected without a failure and with one log in the archive, a
// failure of the server's read when none was written, and a local failure
// when the server's answers were all read.
func TestSupportBundleLogOutcomeItemOutcome(t *testing.T) {
	t.Parallel()
	denied := statusFailure(403)

	assert.Equal(t, itemOutcome{}, logOutcome{containers: 2}.itemOutcome())
	assert.Equal(t, itemOutcome{}, logOutcome{containers: 2, written: 1, failed: []string{"p/b: x"}, readErr: denied}.itemOutcome())
	assert.Equal(t, itemOutcome{failure: denied}, logOutcome{containers: 1, failed: []string{"p/a: x"}, readErr: denied}.itemOutcome())

	got := logOutcome{containers: 1, failed: []string{"pod \"a\" container \"b\": log entry left out"}}.itemOutcome()
	assert.True(t, got.local)
	assert.ErrorContains(t, got.failure, "log entry left out")
}

// TestBundleFailureDetailsTextDetailsProjectsThePathAndTheCounts asserts the
// text form lists the path and the counts on their own lines, then the
// certificate facts of the first failure.
func TestBundleFailureDetailsTextDetailsProjectsThePathAndTheCounts(t *testing.T) {
	t.Parallel()
	cert := tlstrust.Details{Kind: tlstrust.KindExpired}
	d := BundleFailureDetails{OutputFile: "b.tar.gz", Attempted: 5, Failed: 5, First: &cert}
	assert.Equal(t, slices.Concat([]string{"archive: b.tar.gz", "items attempted: 5, failed: 5"}, cert.TextDetails()), d.TextDetails())
	assert.Equal(t, []string{"archive: b.tar.gz"}, BundleFailureDetails{OutputFile: "b.tar.gz"}.TextDetails())
}

// TestBundleFailureDetailsTextShowsAnUnsafePathWithEscapes asserts a path
// that holds a control character or a bidirectional mark is shown escaped in
// the text form, as the renderer prints it, while the JSON form keeps the
// exact value.
func TestBundleFailureDetailsTextShowsAnUnsafePathWithEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, path, shown string }{
		{"newline", "a\nb.tar.gz", `"a\nb.tar.gz"`},
		{"escape", "a\x1b[2Jb.tar.gz", `"a\x1b[2Jb.tar.gz"`},
		{"right-to-left override", "a\u202eb.tar.gz", `"a\u202eb.tar.gz"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := BundleFailureDetails{OutputFile: tc.path}
			lines := d.TextDetails()
			require.Len(t, lines, 1)
			assert.Equal(t, "archive: "+tc.shown, lines[0])
			assert.Equal(t, lines[0], output.Printable(lines[0]), "the renderer removes nothing from the escaped path")

			raw, err := json.Marshal(d)
			require.NoError(t, err)
			var env struct {
				OutputFile string `json:"outputFile"`
			}
			require.NoError(t, json.Unmarshal(raw, &env))
			assert.Equal(t, tc.path, env.OutputFile)
		})
	}
}

// TestSupportBundleKeepsOnlyCertificateDetailsOfTheFirstFailure asserts a
// first failure whose details are of another type leaves `first` empty and
// still gives the error that failure's code, message and hint; a certificate
// failure's details are kept.
func TestSupportBundleKeepsOnlyCertificateDetailsOfTheFirstFailure(t *testing.T) {
	t.Parallel()
	other := clierr.New(clierr.CodeRBAC, "the server refused the read").
		WithHint("ask an administrator for read access").
		WithDetails(clierr.CapabilityUnsupportedDetails{Feature: "x", Server: "s"})
	_, path, err := runExit(t, t.Context(), exitFactory(failAll(other)), "local", "webhook")
	ce, env := decodeError(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "ask an administrator for read access", ce.Hint)
	assert.True(t, strings.HasSuffix(ce.Message, "the server refused the read"), "%q", ce.Message)
	assert.Empty(t, env.Details.First)
	assert.Equal(t, path, env.Details.OutputFile)
	details, ok := ce.Details.(BundleFailureDetails)
	require.True(t, ok)
	assert.Nil(t, details.First)

	cert := tlstrust.Details{Kind: tlstrust.KindExpired}
	assert.Equal(t, &cert, firstCertificate(cert))
	assert.Nil(t, firstCertificate(&cert), "a pointer is not the value the clients return")
	assert.Nil(t, firstCertificate(nil))
}
