package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	memberK8sBase       = "/k8s/clusters/local/apis/management.cattle.io/v3"
	memberSteveClusters = "/v1/management.cattle.io.clusters"
	memberSteveProjects = "/v1/management.cattle.io.projects"
)

// memberServer is a Rancher whose Kubernetes API for clusters and projects
// either returns the objects (an administrator) or responds with 403 (a
// member), and whose Steve returns what the caller can read. It records every
// request.
type memberServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string
	// member makes the Kubernetes reads of clusters and projects respond with 403.
	member bool
	// refuseCollection and refuseRest make Steve respond with 403 for the
	// clusters collection and for every other cluster and project path.
	refuseCollection, refuseRest bool
	// oneObjectAsItem makes Steve return one object by id as it returns a
	// collection item, with _id as well as id. Without it the object has id and
	// no _id.
	oneObjectAsItem bool
}

func (s *memberServer) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *memberServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = nil
}

func kubernetesCluster(id, display string) map[string]any {
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": id, "resourceVersion": "10"},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
}

func kubernetesProject(cluster, id, display string) map[string]any {
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": map[string]any{"name": id, "namespace": cluster, "resourceVersion": "11"},
		"spec":     map[string]any{"clusterName": cluster, "displayName": display},
	}
}

// withSteveFields is obj as Steve returns it: the Kubernetes object plus the
// fields Steve adds.
func withSteveFields(obj map[string]any, id, typ string) map[string]any {
	out := map[string]any{}
	maps.Copy(out, obj)
	meta := map[string]any{}
	maps.Copy(meta, obj["metadata"].(map[string]any))
	meta["fields"] = []any{id}
	meta["state"] = map[string]any{"name": "active"}
	meta["relationships"] = []any{}
	out["metadata"] = meta
	out["id"], out["_id"], out["type"] = id, id, typ
	out["links"], out["actions"] = map[string]any{"self": "x"}, map[string]any{}
	return out
}

// oneObject is item as Steve returns one object by id: without _id, which only
// a collection item has. With asItem it is item unchanged.
func oneObject(item map[string]any, asItem bool) map[string]any {
	if asItem {
		return item
	}
	out := maps.Clone(item)
	delete(out, "_id")
	return out
}

func steveClusterObject(id, display string) map[string]any {
	return withSteveFields(kubernetesCluster(id, display), id, "management.cattle.io.cluster")
}

func steveProjectObject(cluster, id, display string) map[string]any {
	return withSteveFields(kubernetesProject(cluster, id, display), cluster+"/"+id, "management.cattle.io.project")
}

func steveCollectionOf(items ...map[string]any) map[string]any {
	return map[string]any{"count": len(items), "revision": "1", "type": "collection", "data": items}
}

func newMemberServer(t *testing.T, member bool) *memberServer {
	t.Helper()
	s := &memberServer{member: member}
	clusters := []map[string]any{kubernetesCluster("c-a", "Alpha"), kubernetesCluster("c-b", "Beta")}
	projects := []map[string]any{kubernetesProject("c-a", "p-1", "One"), kubernetesProject("c-a", "p-2", "Two"), kubernetesProject("c-b", "p-3", "Three")}
	forbidden := func(w http.ResponseWriter) {
		grWriteStatus(w, http.StatusForbidden, "Forbidden", "forbidden for this user")
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, r.URL.Path)
		s.mu.Unlock()
		p := r.URL.Path
		kubernetes := strings.HasPrefix(p, memberK8sBase)
		steveRefused := (p == memberSteveClusters && s.refuseCollection) ||
			(p != memberSteveClusters && strings.HasPrefix(p, "/v1/") && s.refuseRest)
		switch {
		case kubernetes && s.member:
			forbidden(w)
		case steveRefused:
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "steve refuses")
		case p == memberK8sBase+"/clusters":
			grWriteJSON(w, grKubeList(clusters))
		case p == memberK8sBase+"/clusters/c-a":
			grWriteJSON(w, clusters[0])
		case p == memberK8sBase+"/projects":
			grWriteJSON(w, grKubeList(projects))
		case p == memberK8sBase+"/namespaces/c-a/projects":
			grWriteJSON(w, grKubeList(projects[:2]))
		case p == memberK8sBase+"/namespaces/c-a/projects/p-1":
			grWriteJSON(w, projects[0])
		case p == memberSteveClusters:
			grWriteJSON(w, steveCollectionOf(steveClusterObject("c-b", "Beta"), steveClusterObject("c-a", "Alpha")))
		case p == memberSteveClusters+"/c-a":
			grWriteJSON(w, oneObject(steveClusterObject("c-a", "Alpha"), s.oneObjectAsItem))
		case p == memberSteveProjects:
			grWriteJSON(w, steveCollectionOf(steveProjectObject("c-b", "p-3", "Three"), steveProjectObject("c-a", "p-2", "Two"), steveProjectObject("c-a", "p-1", "One")))
		case p == memberSteveProjects+"/c-a":
			grWriteJSON(w, steveCollectionOf(steveProjectObject("c-a", "p-2", "Two"), steveProjectObject("c-a", "p-1", "One")))
		case p == memberSteveProjects+"/c-a/p-1":
			grWriteJSON(w, oneObject(steveProjectObject("c-a", "p-1", "One"), s.oneObjectAsItem))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// getAs runs one command line against s with a token in a config directory of
// its own and returns what reached stdout, how the run ended and the config
// directory. The tests that call it are not parallel: they set the
// environment.
func getAs(t *testing.T, s *memberServer, args ...string) (stdout []byte, res runResult, stderr, dir string) {
	t.Helper()
	restoreRawArgv(t)
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir = grConfigDir(t, s.srv)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
	res = run(t.Context(), root, append([]string{"rancher", "--config", dir}, args...))
	return out.Bytes(), res, errOut.String(), dir
}

func decodedData(t *testing.T, stdout []byte) any {
	t.Helper()
	var env struct {
		Data any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env), "stdout: %s", stdout)
	return env.Data
}

// dataNames lists the <namespace>/<name> or name of each decoded object in a
// list.
func dataNames(t *testing.T, data any) []string {
	t.Helper()
	list, ok := data.([]any)
	require.True(t, ok, "data is an array")
	var out []string
	for _, item := range list {
		meta := item.(map[string]any)["metadata"].(map[string]any)
		name := meta["name"].(string)
		if ns, ok := meta["namespace"].(string); ok {
			name = ns + "/" + name
		}
		out = append(out, name)
	}
	return out
}

func assertNoSteveFields(t *testing.T, obj map[string]any) {
	t.Helper()
	for _, k := range []string{"id", "type", "links", "actions", "_id", "_type", "_links", "_actions"} {
		assert.NotContains(t, obj, k)
	}
	meta := obj["metadata"].(map[string]any)
	for _, k := range []string{"fields", "state", "relationships"} {
		assert.NotContains(t, meta, k)
	}
}

// TestGetAnAdministratorSendsTheSameRequests asserts that for an
// administrator get clusters, get cluster and get projects send exactly one
// Kubernetes request for the list or the object, no Steve request for the
// object or for a projects collection, and return the objects as the server
// returned them.
func TestGetAnAdministratorSendsTheSameRequests(t *testing.T) {
	s := newMemberServer(t, false)
	for _, tc := range []struct {
		args       []string
		k8sPath    string
		wantNames  []string
		wantObject bool
	}{
		{[]string{"get", "clusters", "-o", "json"}, memberK8sBase + "/clusters", []string{"c-a", "c-b"}, false},
		{[]string{"get", "cluster", "c-a", "-o", "json"}, memberK8sBase + "/clusters/c-a", nil, true},
		{[]string{"get", "projects", "-o", "json"}, memberK8sBase + "/projects", []string{"c-a/p-1", "c-a/p-2", "c-b/p-3"}, false},
	} {
		s.reset()
		stdout, res, stderr, _ := getAs(t, s, tc.args...)
		require.Nil(t, res.err, "%v: %s", tc.args, stderr)
		assert.Equal(t, 0, res.code)
		data := decodedData(t, stdout)
		if tc.wantObject {
			assert.Equal(t, kubernetesCluster("c-a", "Alpha"), data)
		} else {
			assert.Equal(t, tc.wantNames, dataNames(t, data))
		}
		var k8s, steveObjects int
		for _, p := range s.requested() {
			switch {
			case p == tc.k8sPath:
				k8s++
			case strings.HasPrefix(p, memberSteveClusters+"/"), strings.HasPrefix(p, memberSteveProjects):
				steveObjects++
			}
		}
		assert.Equal(t, 1, k8s, "%v: the Kubernetes path is requested once", tc.args)
		assert.Zero(t, steveObjects, "%v: no Steve request for an object or a projects collection", tc.args)
	}
}

// TestGetAMemberReadsThroughSteve asserts that when the Kubernetes API
// refuses with 403, get clusters, get cluster, get projects and get projects
// --cluster return the objects Steve lists for the caller, in the Kubernetes
// shape.
func TestGetAMemberReadsThroughSteve(t *testing.T) {
	s := newMemberServer(t, true)

	stdout, res, stderr, _ := getAs(t, s, "get", "clusters", "-o", "json")
	require.Nil(t, res.err, stderr)
	data := decodedData(t, stdout)
	assert.Equal(t, []string{"c-a", "c-b"}, dataNames(t, data))
	for _, item := range data.([]any) {
		assertNoSteveFields(t, item.(map[string]any))
	}

	stdout, res, stderr, _ = getAs(t, s, "get", "projects", "-o", "json")
	require.Nil(t, res.err, stderr)
	assert.Equal(t, []string{"c-a/p-1", "c-a/p-2", "c-b/p-3"}, dataNames(t, decodedData(t, stdout)))

	stdout, res, stderr, _ = getAs(t, s, "get", "projects", "--cluster", "c-a", "-o", "json")
	require.Nil(t, res.err, stderr)
	assert.Equal(t, []string{"c-a/p-1", "c-a/p-2"}, dataNames(t, decodedData(t, stdout)))

	for _, asItem := range []bool{false, true} {
		s.oneObjectAsItem = asItem

		stdout, res, stderr, _ = getAs(t, s, "get", "cluster", "c-a", "-o", "json")
		require.Nil(t, res.err, stderr)
		obj, ok := decodedData(t, stdout).(map[string]any)
		require.True(t, ok)
		assertNoSteveFields(t, obj)
		assert.Equal(t, kubernetesCluster("c-a", "Alpha")["spec"], obj["spec"], "one object returned as a collection item: %t", asItem)

		stdout, res, stderr, _ = getAs(t, s, "get", "project", "p-1", "--cluster", "c-a", "-o", "json")
		require.Nil(t, res.err, stderr)
		obj, ok = decodedData(t, stdout).(map[string]any)
		require.True(t, ok)
		assertNoSteveFields(t, obj)
		assert.Equal(t, "p-1", obj["metadata"].(map[string]any)["name"], "one object returned as a collection item: %t", asItem)
	}
}

// TestGetAMemberWhomSteveAlsoRefusesGetsTheKubernetesError asserts that when
// Steve refuses too, the run ends with exit 6, code RBAC and the hint the
// Kubernetes refusal carries, for a list and for one object; a refused get of
// one project carries no hint.
func TestGetAMemberWhomSteveAlsoRefusesGetsTheKubernetesError(t *testing.T) {
	s := newMemberServer(t, true)
	s.refuseCollection, s.refuseRest = true, true
	for _, tc := range []struct {
		args []string
		hint string
	}{
		{[]string{"get", "clusters", "-o", "json"}, "rancher access clusters"},
		{[]string{"get", "projects", "--cluster", "c-a", "-o", "json"}, "rancher access projects --cluster 'c-a'"},
		{[]string{"get", "projects", "-o", "json"}, "rancher access projects"},
		{[]string{"get", "cluster", "c-a", "-o", "json"}, "rancher inspect cluster 'c-a'"},
		{[]string{"get", "project", "p-1", "--cluster", "c-a", "-o", "json"}, ""},
	} {
		s.refuseCollection = tc.args[1] == "clusters"
		_, res, stderr, dir := getAs(t, s, tc.args...)
		require.NotNil(t, res.err, "%v", tc.args)
		assert.Equal(t, 6, res.code, "%v", tc.args)
		ce := renderedError(t, "json", stderr)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Contains(t, ce.Message, "forbidden for this user", "the Kubernetes API's own message")
		assert.Equal(t, tc.hint, withoutConfigDir(ce.Hint, dir), "%v", tc.args)
	}
}

// TestGetClusterTabOffersAMembersClusters asserts that completing the name of
// get cluster for a member, with the Kubernetes clusters list refused, offers
// the clusters Steve lists with their unique display names, that every
// offered value resolves through the command's factory, and that patch
// cluster completes from the same pages.
func TestGetClusterTabOffersAMembersClusters(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case mgmtClustersPath:
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "forbidden for this user")
		case steveClustersPath:
			grWriteJSON(w, map[string]any{"count": 3, "revision": "1", "data": []map[string]any{
				steveFixtureItem("c-m-b", "Beta"), steveFixtureItem("c-m-a", "Alpha"), steveFixtureItem("c-m-c", "Beta")}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	completionFixture(t, srv.URL, true)

	for _, verb := range []string{"get", "patch"} {
		mu.Lock()
		hits = nil
		mu.Unlock()
		got := runCompletion(t, []string{verb, "cluster"}, "")
		assert.ElementsMatch(t, []string{"c-m-a:Alpha", "Alpha", "c-m-b:Beta", "c-m-c:Beta"}, got, verb)
		mu.Lock()
		assert.NotContains(t, hits, mgmtClustersPath, "%s makes no request on the Kubernetes clusters path", verb)
		mu.Unlock()

		f := factoryFor(t, srv.URL)
		for _, candidate := range got {
			value, _, _ := strings.Cut(candidate, ":")
			id, err := f.ResolveCluster(t.Context(), value)
			require.NoError(t, err, "%s: the offered value %q resolves", verb, value)
			assert.Contains(t, []string{"c-m-a", "c-m-b", "c-m-c"}, id)
		}
	}
}
