package cli

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeNodesPath is the Steve API collection of the management nodes of the
// downstream cluster c-aaa11.
const nodeNodesPath = "/v1/management.cattle.io.nodes/c-aaa11"

// steveNodesBody is a Steve API collection of management nodes: a worker that is
// active, a draining worker, a drained worker, a worker whose node is not
// ready and a control plane node with no worker role.
const steveNodesBody = `{"data":[
 {"metadata":{"name":"m-active","state":{"name":"active"}},"status":{"nodeName":"active-node"},"spec":{"worker":true}},
 {"metadata":{"name":"m-draining","state":{"name":"draining"}},"status":{"nodeName":"draining-node","conditions":[{"type":"Drained","status":"Unknown"}]},"spec":{"worker":true,"desiredNodeUnschedulable":"drain","internalNodeSpec":{"unschedulable":true}}},
 {"metadata":{"name":"m-drained","state":{"name":"active"}},"status":{"nodeName":"drained-node","conditions":[{"type":"Drained","status":"True"}]},"spec":{"worker":true,"internalNodeSpec":{"unschedulable":true}}},
 {"metadata":{"name":"m-unavailable","state":{"name":"unavailable"}},"status":{"nodeName":"unavailable-node"},"spec":{"worker":true}},
 {"metadata":{"name":"m-control","state":{"name":"active"}},"status":{"nodeName":"control-node"},"spec":{"controlPlane":true}}
]}`

func nodeCompletionServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{nodeNodesPath: steveNodesBody}, steveClustersPages)
}

// TestNodeCompletionOffersAllowedNodes asserts, through the real TAB callback
// with --cluster typed first, that the node positional of node drain offers
// the active node only and that of node stop-drain the draining node only, a
// drained node, a node that is not ready and a node with no worker role to
// neither, with one request, and that --cluster of both commands is still
// offered by the shared cluster source; and that the same nodes are offered,
// with no request through the local cluster's route, when that route answers
// every request with 403. NOT parallel: the completion fixture
// mutates env globals and the stored config.
func TestNodeCompletionOffersAllowedNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	s := nodeCompletionServer(t)
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"drain", []string{"active-node"}},
		{"stop-drain", []string{"draining-node"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			completionFixture(t, s.srv.URL, true)
			s.reset()
			got := runCompletion(t, []string{"node", tc.command, "--cluster", "c-aaa11"}, "")
			assert.Equal(t, tc.want, got)
			assert.Equal(t, map[string]int{nodeNodesPath: 1}, s.reset(), "one request")

			completionFixture(t, s.srv.URL, true)
			s.reset()
			got = runCompletion(t, []string{"node", tc.command, "worker-1", "--cluster"}, "")
			hits := s.reset()
			assert.NotEmpty(t, got, "--cluster offers clusters")
			assert.NotContains(t, got, "active-node")
			assert.NotContains(t, hits, nodeNodesPath, "--cluster is not claimed by the node source")
		})
	}

	// A Cluster Owner or Manage Nodes user is refused on every request through
	// the local cluster's route: the node source reads through the Steve API
	// and offers the same nodes.
	var local atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/k8s/clusters/local/") {
			local.Add(1)
			http.Error(w, "", http.StatusForbidden)
			return
		}
		s.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"drain", []string{"active-node"}},
		{"stop-drain", []string{"draining-node"}},
	} {
		t.Run(tc.command+" for a caller the local cluster refuses", func(t *testing.T) {
			completionFixture(t, front.URL, true)
			s.reset()
			got := runCompletion(t, []string{"node", tc.command, "--cluster", "c-aaa11"}, "")
			assert.Equal(t, tc.want, got)
			assert.Equal(t, map[string]int{nodeNodesPath: 1}, s.reset(), "one request")
			assert.Zero(t, local.Load(), "nothing goes through the local cluster's route")
		})
	}
}

// TestManagementNodeNameCompletionOffersEveryID asserts get management-node
// completes with the ids of the typed cluster's management nodes, with no
// state filter. NOT parallel: the completion fixture mutates env globals and
// the stored config.
func TestManagementNodeNameCompletionOffersEveryID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	s := nodeCompletionServer(t)
	completionFixture(t, s.srv.URL, true)
	got := runCompletion(t, []string{"get", "management-node", "--cluster", "c-aaa11"}, "")
	assert.ElementsMatch(t, []string{"m-active:active-node", "m-draining:draining-node", "m-drained:drained-node", "m-unavailable:unavailable-node", "m-control:control-node"}, got)
	assert.Equal(t, map[string]int{nodeNodesPath: 1}, s.reset())
}

// nodeCompletionSite is one value-taking flag or argument of a node command:
// "live" (a source offers its candidates), or "static" (a fixed list, offered with no
// request), with the exact candidates of a static site.
type nodeCompletionSite struct {
	completion string
	words      []string
	want       []string
}

var nodeCompletionSites = map[string]nodeCompletionSite{
	"node drain <node>":          {completion: "live", words: []string{"node", "drain", "--cluster", "c-aaa11"}},
	"node drain --cluster":       {completion: "live", words: []string{"node", "drain", "worker-1", "--cluster"}},
	"node drain --grace-period":  {completion: "static", words: []string{"node", "drain", "worker-1", "--cluster", "c-aaa11", "--grace-period"}, want: []string{"10", "30", "60", "120", "300"}},
	"node drain --drain-timeout": {completion: "static", words: []string{"node", "drain", "worker-1", "--cluster", "c-aaa11", "--drain-timeout"}, want: []string{"30s", "1m", "2m", "5m", "10m", "30m", "1h"}},
	"node drain --timeout":       {completion: "static", words: []string{"node", "drain", "worker-1", "--cluster", "c-aaa11", "--timeout"}, want: []string{"30s", "1m", "2m", "5m", "10m"}},
	"node stop-drain <node>":     {completion: "live", words: []string{"node", "stop-drain", "--cluster", "c-aaa11"}},
	"node stop-drain --cluster":  {completion: "live", words: []string{"node", "stop-drain", "worker-1", "--cluster"}},
	"node stop-drain --timeout":  {completion: "static", words: []string{"node", "stop-drain", "worker-1", "--cluster", "c-aaa11", "--timeout"}, want: []string{"30s", "1m", "2m", "5m", "10m"}},
}

// TestEveryValueOfANodeCommandHasACompletion asserts every value-taking flag
// and argument of node drain and node stop-drain has a row in
// nodeCompletionSites and no row names one that is gone, and that through the
// real TAB callback a live row offers names after a request and a static row
// offers its list with no request. NOT parallel: the completion fixture
// mutates env globals and the stored config.
func TestEveryValueOfANodeCommandHasACompletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)

	declared := map[string]bool{}
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		if !strings.HasPrefix(d.Name, "node ") {
			continue
		}
		for _, a := range d.Args {
			declared[d.Name+" <"+a.Name+">"] = true
		}
		for _, f := range d.Flags {
			if f.Type != "bool" {
				declared[d.Name+" --"+f.Name] = true
			}
		}
	}
	require.NotEmpty(t, declared, "the node commands are registered")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(declared)), slices.Collect(maps.Keys(nodeCompletionSites)),
		"every value-taking flag and argument has a row, and every row names one")

	s := nodeCompletionServer(t)
	for _, key := range slices.Sorted(maps.Keys(nodeCompletionSites)) {
		site := nodeCompletionSites[key]
		t.Run(key, func(t *testing.T) {
			completionFixture(t, s.srv.URL, true)
			s.reset()
			got := runCompletion(t, site.words, "")
			hits := s.reset()
			switch site.completion {
			case "live":
				assert.NotEmpty(t, got, "a live site offers names")
				assert.NotEmpty(t, hits, "a live site reads the server")
			case "static":
				assert.ElementsMatch(t, site.want, got)
				assert.Empty(t, hits, "a static site sends no request")
			default:
				require.Failf(t, "unknown completion", "%q", site.completion)
			}
		})
	}
}
