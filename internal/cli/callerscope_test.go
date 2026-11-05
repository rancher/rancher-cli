package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// callerBearer is the one credential of the caller-scope tests: a user who
// may not get cluster local.
const callerBearer = "u-member:secret"

// callerTarget is where a row's request for the resolved cluster lands.
type callerTarget string

const (
	// targetDownstream: a request under /k8s/clusters/<id>/.
	targetDownstream callerTarget = "downstream"
	// targetManagement: a management.cattle.io request through the local
	// cluster in the cluster's namespace, or the steve projects
	// collection of the cluster.
	targetManagement callerTarget = "management"
	// targetLocalExt: an ext.cattle.io create through the local cluster
	// whose body names the cluster.
	targetLocalExt callerTarget = "local-ext"
	// targetClusterObject: the steve GET of the cluster by id.
	targetClusterObject callerTarget = "cluster-object"
	targetNone          callerTarget = "none"
)

// localClustersPath is the local cluster's management clusters API, which
// Rancher refuses to a user who may not get cluster local.
const localClustersPath = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"

// callerRow is one command line. args may hold {base} (the server URL) and
// {manifest} (a ConfigMap manifest). kindErr, set on a generated verb that
// keeps kubectl semantics on the local proxy, is the error it must end in;
// own is the request the row may send under localClustersPath.
type callerRow struct {
	args    string
	target  callerTarget
	plugin  bool
	kindErr *callerError
	own     string
}

type callerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

const localRefusal = " (403) and gave no reason; through the local cluster's proxy, Rancher answers some management.cattle.io resources, clusters among them, only for users with access to the local cluster"

// callerBespokeRows are the hand-written commands that take --cluster or
// --clusters, with the declared positionals and required flags they need,
// plus the generated cluster verbs and inspect cluster, which name the
// cluster without the flag.
var callerBespokeRows = map[string]callerRow{
	"access can":               {args: "access can get pods --cluster prod --namespace default -o json", target: targetDownstream},
	"access explain":           {args: "access explain get pods --cluster prod --namespace default -o json", target: targetDownstream},
	"access list":              {args: "access list --cluster prod -o json", target: targetDownstream},
	"access projects":          {args: "access projects --cluster prod -o json", target: targetDownstream},
	"access who-can":           {args: "access who-can get pods --cluster prod --namespace default -o json", target: targetDownstream},
	"auth get-token":           {args: "auth get-token --server {base} --cluster prod --name kube -o json", target: targetLocalExt, plugin: true},
	"token":                    {args: "token --server {base} --user kube --cluster prod -o json", target: targetLocalExt, plugin: true},
	"create kubeconfig":        {args: "create kubeconfig --clusters prod --dry-run", target: targetLocalExt},
	"create namespace":         {args: "create namespace ns1 --cluster prod --dry-run -o json", target: targetDownstream},
	"create project":           {args: "create project web2 --cluster prod --dry-run -o json", target: targetManagement},
	"create resource":          {args: "create resource -f {manifest} --cluster prod --dry-run -o json", target: targetDownstream},
	"create secret":            {args: "create secret s --cluster prod --namespace default --from-literal a=b --dry-run -o json", target: targetDownstream},
	"create token":             {args: "create token --cluster prod --dry-run -o json", target: targetLocalExt},
	"grant":                    {args: "grant u-1 --role cluster-member --cluster prod --dry-run -o json", target: targetManagement},
	"revoke":                   {args: "revoke u-1 --cluster prod --dry-run -o json", target: targetManagement},
	"images":                   {args: "images --cluster prod -o json", target: targetDownstream},
	"inspect deployment":       {args: "inspect deployment x --cluster prod --namespace default -o json", target: targetDownstream},
	"inspect pod":              {args: "inspect pod x --cluster prod --namespace default -o json", target: targetDownstream},
	"inspect project":          {args: "inspect project web --cluster prod -o json", target: targetManagement},
	"kubeconfig":               {args: "kubeconfig --cluster prod", target: targetClusterObject},
	"logs":                     {args: "logs kube-api-auth --cluster prod -o json", target: targetDownstream},
	"project add-namespace":    {args: "project add-namespace default --project web --cluster prod --dry-run -o json", target: targetDownstream},
	"project remove-namespace": {args: "project remove-namespace default --cluster prod --dry-run -o json", target: targetDownstream},
	"support-bundle":           {args: "support-bundle --clusters prod -o json", target: targetDownstream},
	"usage":                    {args: "usage --cluster prod -o json", target: targetDownstream},
	"inspect cluster":          {args: "inspect cluster prod -o json", target: targetClusterObject},
	"get cluster": {args: "get cluster -o json", target: targetNone, own: "GET " + localClustersPath,
		kindErr: &callerError{"RBAC", "the server refused list clusters.management.cattle.io" + localRefusal, "rancher access clusters"}},
	"get cluster c-abc": {args: "get cluster c-abc -o json", target: targetNone, own: "GET " + localClustersPath + "/c-abc",
		kindErr: &callerError{"RBAC", "the server refused get clusters.management.cattle.io 'c-abc'" + localRefusal, "rancher inspect cluster 'c-abc'"}},
	"patch cluster": {args: `patch cluster c-abc --patch [{"op":"add","path":"/metadata/labels","value":{"a":"b"}}] --dry-run -o json`, target: targetNone,
		own:     "GET " + localClustersPath + "/c-abc",
		kindErr: &callerError{"RBAC", "the server refused get clusters.management.cattle.io 'c-abc'" + localRefusal, "rancher inspect cluster 'c-abc'"}},
}

// generatedRow builds the row of a generated get, patch or delete that
// takes --cluster: a placeholder for each required positional and
// required flag, --dry-run on a mutation, JSON output. A downstream kind
// reaches the cluster's proxy; a management kind, the cluster's namespace
// through local.
func generatedRow(d runtime.Descriptor, k kinds.Kind) callerRow {
	args := strings.Fields(d.Name)
	for _, a := range d.Args {
		if a.Required {
			args = append(args, "x")
		}
	}
	for _, f := range d.Flags {
		switch {
		case f.Name == "cluster":
			args = append(args, "--cluster", "prod")
		case f.Name == "namespace" && f.Required:
			args = append(args, "--namespace", "default")
		case f.Name == "patch":
			args = append(args, "--patch", `[{"op":"add","path":"/metadata/labels","value":{"a":"b"}}]`)
		}
	}
	if d.CommandType == runtime.Mutating {
		args = append(args, "--dry-run")
	}
	row := callerRow{args: strings.Join(append(args, "-o", "json"), " "), target: targetDownstream}
	if k.Scope == kinds.Management {
		row.target = targetManagement
	}
	if d.Name == "get project" {
		row.kindErr = &callerError{"RBAC", projectListForbidden, "rancher access projects --cluster '<id>'"}
	}
	return row
}

// projectListForbidden is the kube Status message the fake answers the
// project LIST in a cluster's namespace with.
const projectListForbidden = `projects.management.cattle.io is forbidden: User "u-member" cannot list resource "projects" in API group "management.cattle.io" in the namespace "<id>"`

type callerRequest struct {
	method, path string
	body         []byte
}

func (q callerRequest) String() string { return q.method + " " + q.path }

// callerScopeServer is the facade's state for one pass: the clusters the
// caller sees on steve (c-abc, displayed as prod, and in the collision pass
// prod, displayed as c-abc) and every request it received.
type callerScopeServer struct {
	base    string
	collide bool
	// projectMiss makes the local proxy answer a GET of one named project
	// with NOT_FOUND, as it does for a name that is no project's id.
	projectMiss atomic.Bool
	mu          sync.Mutex
	reqs        []callerRequest
}

func (s *callerScopeServer) clusters() []testrancher.ClusterFixture {
	out := []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}}
	if s.collide {
		out = append(out, testrancher.ClusterFixture{ID: "prod", DisplayName: "c-abc"})
	}
	return out
}

func (s *callerScopeServer) record(r *http.Request) {
	q := callerRequest{method: r.Method, path: r.URL.Path}
	if r.Body != nil && r.Method != http.MethodGet {
		q.body, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(q.body))
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, q)
	s.mu.Unlock()
}

func (s *callerScopeServer) take() []callerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

// callerFallthrough answers what the facade does not serve: the version,
// discovery, access reviews, dry-run creates and every kind of the kinds
// table (kubeAnswer). The project LIST in a cluster's namespace through
// local is a kube Status 403 with a message.
func callerFallthrough(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	decoded := func() map[string]any {
		obj := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&obj)
		return obj
	}
	if rest, ok := strings.CutPrefix(p, "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/"); ok {
		segs := strings.Split(rest, "/")
		switch {
		case len(segs) == 2 && segs[1] == "projects" && r.Method == http.MethodGet:
			write(http.StatusForbidden, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
				"message": strings.ReplaceAll(projectListForbidden, "<id>", segs[0]), "reason": "Forbidden", "code": 403})
			return
		case len(segs) == 3 && segs[1] == "projects":
			write(http.StatusOK, map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Project",
				"metadata": map[string]any{"name": segs[2], "namespace": segs[0], "resourceVersion": "1"},
				"spec":     map[string]any{"displayName": "web", "clusterName": segs[0]}})
			return
		}
	}
	switch {
	case p == "/rancherversion":
		write(http.StatusOK, map[string]any{"Version": "v2.13.0", "GitCommit": "x", "RancherPrime": "false"})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis"):
		gv := map[string]any{"groupVersion": "ext.cattle.io/v1", "version": "v1"}
		write(http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "ext.cattle.io", "versions": []any{gv}, "preferredVersion": gv}}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/api/v1"):
		write(http.StatusOK, map[string]any{"kind": "APIResourceList", "groupVersion": "v1", "resources": []any{
			map[string]any{"name": "pods", "singularName": "pod", "namespaced": true, "kind": "Pod", "verbs": []any{"get", "list"}}}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/api"):
		write(http.StatusOK, map[string]any{"kind": "APIVersions", "versions": []any{"v1"}, "serverAddressByClientCIDRs": []any{}})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/authentication.k8s.io/v1/selfsubjectreviews"):
		write(http.StatusCreated, map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{"username": "u-member", "groups": []any{"system:authenticated"}}}})
	case r.Method == http.MethodPost && strings.Contains(p, "/authorization.k8s.io/v1/"):
		obj := decoded()
		obj["status"] = map[string]any{"allowed": true, "incomplete": false, "resourceRules": []any{}, "nonResourceRules": []any{}}
		write(http.StatusCreated, obj)
	case r.Method == http.MethodPost && r.URL.Query().Get("dryRun") != "":
		obj := decoded()
		md, _ := obj["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		if md["name"] == nil || md["name"] == "" {
			md["name"] = "x"
		}
		md["resourceVersion"] = "1"
		obj["metadata"] = md
		write(http.StatusCreated, obj)
	default:
		if obj, ok := kubeAnswer(r); ok {
			write(http.StatusOK, obj)
			return
		}
		write(http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
			"status": "Failure", "message": "not found", "reason": "NotFound", "code": 404})
	}
}

// callerHarness is one pass: a facade whose local proxy refuses the
// caller the management resources outside Rancher's allow list, and a
// config directory holding the caller's login in the file store.
type callerHarness struct {
	srv      *callerScopeServer
	manifest string
	cfgDir   string
}

func newCallerHarness(t *testing.T, collide bool) *callerHarness {
	t.Helper()
	return newCallerHarnessWith(t, collide, nil)
}

// newCallerHarnessWith is newCallerHarness with adjust applied to the
// facade's options before the facade is built.
func newCallerHarnessWith(t *testing.T, collide bool, adjust func(*testrancher.Options)) *callerHarness {
	t.Helper()
	keyring.MockInit()
	s := &callerScopeServer{collide: collide}
	known := func(b string) bool { return b == callerBearer }
	opts := testrancher.Options{
		SeedToken:           callerBearer,
		LocalManagementGate: func(string) bool { return false },
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return s.clusters(), known(b)
		},
		// The kube-api-auth logs target needs a cluster with the
		// authorized cluster endpoint enabled.
		ClusterObject: func(id string) (map[string]any, bool) {
			i := slices.IndexFunc(s.clusters(), func(c testrancher.ClusterFixture) bool { return c.ID == id })
			if i < 0 {
				return nil, false
			}
			return map[string]any{"metadata": map[string]any{"name": id},
				"spec":   map[string]any{"displayName": s.clusters()[i].DisplayName, "localClusterAuthEndpoint": map[string]any{"enabled": true}},
				"status": map[string]any{"conditions": []any{}}}, true
		},
		// Every cluster has p-1 (web) and p-2, whose display name is p-1's
		// id.
		ProjectVisibility: func(_ context.Context, b, _ string) ([]testrancher.ProjectFixture, bool, error) {
			var out []testrancher.ProjectFixture
			for _, c := range s.clusters() {
				out = append(out, testrancher.ProjectFixture{Cluster: c.ID, ID: "p-1", DisplayName: "web"},
					testrancher.ProjectFixture{Cluster: c.ID, ID: "p-2", DisplayName: "p-1"})
			}
			return out, known(b), nil
		},
		NamespaceVisibility: func(b, _ string) ([]testrancher.NamespaceFixture, bool) {
			return []testrancher.NamespaceFixture{{Name: "default", Project: "p-1"}}, known(b)
		},
		RequestHook: s.record,
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rest, ok := strings.CutPrefix(r.URL.Path, "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/")
			if segs := strings.Split(rest, "/"); ok && s.projectMiss.Load() && r.Method == http.MethodGet && len(segs) == 3 && segs[1] == "projects" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
					"status": "Failure", "message": "projects.management.cattle.io \"" + segs[2] + "\" not found", "reason": "NotFound", "code": 404})
				return
			}
			callerFallthrough(w, r)
		}),
	}
	if adjust != nil {
		adjust(&opts)
	}
	f := testrancher.New(opts)
	hs := httptest.NewTLSServer(f)
	t.Cleanup(hs.Close)
	s.base = hs.URL

	cfgDir, home := t.TempDir(), t.TempDir()
	// support-bundle writes its archive where it runs.
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfigDir, cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	require.NoError(t, os.Unsetenv(config.EnvURL))
	require.NoError(t, os.Unsetenv(config.EnvToken))
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "x", Servers: map[string]config.Server{"x": {URL: s.base, CA: string(pemFromServer(t, hs))}}}
	cfg.SetPath(config.FilePath(cfgDir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(cfgDir).SetToken(config.ServerRef{Name: "x", URL: s.base}, callerBearer))

	h := &callerHarness{srv: s, manifest: filepath.Join(t.TempDir(), "cm.yaml"), cfgDir: cfgDir}
	require.NoError(t, os.WriteFile(h.manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: caller\n  namespace: default\ndata:\n  a: b\n"), 0o600))
	t.Cleanup(func() { CaptureArgv(nil) })
	return h
}

type callerRun struct {
	stdout, stderr string
	requests       []callerRequest
	warnings       []string
	err            *callerError
}

// run runs args through a fresh root. A JSON envelope, on either stream
// and after any notice lines, gives the error and the warnings.
func (h *callerHarness) run(t *testing.T, args []string) callerRun {
	t.Helper()
	h.srv.take()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	CaptureArgv(args)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_ = root.Run(ctx, append([]string{"rancher"}, args...))
	r := callerRun{stdout: out.String(), stderr: errOut.String(), requests: h.srv.take()}
	for _, stream := range []string{r.stderr, r.stdout} {
		i := strings.Index(stream, "{")
		if i < 0 {
			continue
		}
		var env struct {
			Warnings []string        `json:"warnings"`
			Data     json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(stream[i:]), &env) != nil {
			continue
		}
		r.warnings = append(r.warnings, env.Warnings...)
		var data struct {
			Error *callerError `json:"error"`
		}
		if json.Unmarshal(env.Data, &data) == nil && data.Error != nil {
			r.err = data.Error
		}
	}
	return r
}

// reached reports whether a request of r lands on cluster id as target
// says.
func (r callerRun) reached(target callerTarget, id string) bool {
	for _, q := range r.requests {
		switch target {
		case targetDownstream:
			if strings.HasPrefix(q.path, "/k8s/clusters/"+id+"/") {
				return true
			}
		case targetManagement:
			if strings.HasPrefix(q.path, "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/"+id+"/") ||
				q.path == testrancher.ProjectsCollectionPath+"/"+id {
				return true
			}
		case targetLocalExt:
			var obj struct {
				Spec struct {
					ClusterName string   `json:"clusterName"`
					Clusters    []string `json:"clusters"`
				} `json:"spec"`
			}
			if q.method == http.MethodPost && strings.HasPrefix(q.path, "/k8s/clusters/local/apis/ext.cattle.io/") &&
				json.Unmarshal(q.body, &obj) == nil && (obj.Spec.ClusterName == id || slices.Equal(obj.Spec.Clusters, []string{id})) {
				return true
			}
		case targetClusterObject:
			if q.method == http.MethodGet && q.path == testrancher.ClustersCollectionPath+"/"+id {
				return true
			}
		case targetNone:
			return true
		}
	}
	return false
}

// callerRows is every row: the generated verbs that take --cluster, from
// the registry, and the bespoke rows. It fails on a command with a
// cluster or clusters flag that has no row, and on a row with no declared
// target.
func callerRows(t *testing.T) map[string]callerRow {
	t.Helper()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	rows := map[string]callerRow{}
	taking := 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if !slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "cluster" || f.Name == "clusters" }) {
			continue
		}
		taking++
		if k, ok := kindForDescriptor(d); ok && isGeneratedVerb(d.Name) {
			if k.NoPatchReason != "" && d.CommandType == runtime.Mutating {
				// A refused patch does not look up the target cluster; it refuses before any request.
				continue
			}
			rows[d.Name] = generatedRow(d, k)
			continue
		}
		row, ok := callerBespokeRows[d.Name]
		if assert.True(t, ok, "%s takes a cluster and has no row", d.Name) {
			rows[d.Name] = row
		}
	}
	assert.Equal(t, 88, taking, "the commands that take a cluster or clusters flag")
	for name, row := range callerBespokeRows {
		if _, ok := rows[name]; !ok {
			rows[name] = row
		}
	}
	for name, row := range rows {
		assert.NotEmpty(t, row.target, "%s: the row declares no target", name)
	}
	return rows
}

// TestEveryClusterCommandResolvesAsTheCaller asserts, for every command
// that takes --cluster or --clusters and for the generated cluster verbs,
// against a server whose local proxy refuses the caller the management
// resources outside Rancher's allow list: the cluster value is resolved
// through the steve clusters collection, nothing reaches the local
// cluster's management clusters API but a generated verb's own request,
// the declared target is reached on the resolved cluster, no command ends
// in the local proxy's refusal except the generated verbs that keep
// kubectl semantics there, whose error names the refused request and the
// caller-scoped command. In the collision pass a second cluster has the
// id prod and the display name c-abc: the id wins with a warning, which
// names --cluster for a flag value and no flag for the positional of get,
// inspect and patch cluster and for a --clusters or --current-context
// entry; a project id that is also another project's display name warns
// first, and a store notice comes after both.
// Not parallel: it sets env and the captured argv.
func TestEveryClusterCommandResolvesAsTheCaller(t *testing.T) {
	rows := callerRows(t)
	for _, pass := range []struct {
		name    string
		collide bool
		id      string
	}{{"display-name", false, "c-abc"}, {"id-collision", true, "prod"}} {
		t.Run(pass.name, func(t *testing.T) {
			h := newCallerHarness(t, pass.collide)
			for _, name := range slices.Sorted(maps.Keys(rows)) {
				t.Run(name, func(t *testing.T) { h.checkRow(t, rows[name], pass.id, pass.collide) })
			}
			if !pass.collide {
				return
			}
			t.Run("project-and-cluster-warnings", func(t *testing.T) {
				r := h.run(t, strings.Fields("get pod --cluster prod --project p-1 -o json"))
				require.Nil(t, r.err, r.stderr)
				assert.Equal(t, []string{
					"--project 'p-1' is the id of project 'p-1' and the display name of project 'p-2'; the command uses the one with that id. To use the other one, pass --project 'p-2'",
					callerClusterWarning,
				}, r.warnings, "the project's warning first, then the cluster's")
			})
			t.Run("project-cluster-and-store-notices", func(t *testing.T) {
				creds := config.CredentialsFilePath(h.cfgDir)
				require.NoError(t, os.Chmod(creds, 0o644))
				t.Cleanup(func() { _ = os.Chmod(creds, 0o600) })
				r := h.run(t, strings.Fields("get pod --cluster prod --project p-1 -o json"))
				require.Nil(t, r.err, r.stderr)
				assert.Equal(t, []string{
					"--project 'p-1' is the id of project 'p-1' and the display name of project 'p-2'; the command uses the one with that id. To use the other one, pass --project 'p-2'",
					callerClusterWarning,
					creds + " is group/world-readable (644); run: chmod 600 " + creds,
				}, r.warnings, "the project's warning, then the cluster's, then the store's")
			})
			t.Run("positional-warnings", func(t *testing.T) {
				for _, args := range []string{
					"get cluster prod -o json",
					"inspect cluster prod -o json",
					`patch cluster prod --patch [{"op":"add","path":"/metadata/labels","value":{"a":"b"}}] --dry-run -o json`,
					"create kubeconfig --clusters prod --dry-run",
					"create kubeconfig --clusters * --current-context prod --dry-run",
					"support-bundle --clusters prod --output-file positional-warnings.tar.gz -o json",
				} {
					r := h.run(t, strings.Fields(args))
					// A text-mode run prints its warnings as "# warning:" lines.
					warnings := r.warnings
					for line := range strings.Lines(r.stderr) {
						if w, ok := strings.CutPrefix(strings.TrimSpace(line), "# warning: "); ok {
							warnings = append(warnings, w)
						}
					}
					assert.Equal(t, []string{callerPositionalWarning}, warnings, "%s: the warning names no flag", args)
				}
				r := h.run(t, strings.Fields("get pod --cluster prod -o json"))
				assert.Equal(t, []string{callerClusterWarning}, r.warnings, "a --cluster value's warning names the flag")
			})
		})
	}
}

// callerClusterWarning is the collision pass's warning: the id prod wins
// over c-abc's display name.
const callerClusterWarning = "--cluster 'prod' is the id of cluster 'prod' and the display name of cluster 'c-abc'; the command uses the one with that id. To use the other one, pass --cluster 'c-abc'"

// callerPositionalWarning is callerClusterWarning for a cluster named by
// the command's positional argument.
const callerPositionalWarning = "'prod' is the id of cluster 'prod' and the display name of cluster 'c-abc'; the command uses the one with that id. To use the other one, pass 'c-abc'"

// checkRow runs row and asserts it resolved its cluster value to id as
// the caller and reached its target there.
func (h *callerHarness) checkRow(t *testing.T, row callerRow, id string, collide bool) {
	t.Helper()
	args := strings.Fields(strings.NewReplacer("{base}", h.srv.base, "{manifest}", h.manifest).Replace(row.args))
	r := h.run(t, args)
	var sent []string
	for _, q := range r.requests {
		sent = append(sent, q.String())
	}
	resolves := slices.Contains(args, "--cluster") || slices.Contains(args, "--clusters") || args[0] == "inspect" && args[1] == "cluster"
	if resolves {
		assert.Contains(t, sent, "GET "+testrancher.ClustersCollectionPath, "the cluster is resolved through steve")
	}
	for _, q := range r.requests {
		if q.path == localClustersPath || strings.HasPrefix(q.path, localClustersPath+"/") {
			assert.Equal(t, row.own, q.String(), "nothing reaches the local cluster's clusters API but the verb's own request")
		}
		if row.target != targetDownstream {
			continue
		}
		// A downstream request sent through local instead lands outside
		// Rancher's own groups there.
		if rest, ok := strings.CutPrefix(q.path, "/k8s/clusters/local/"); ok {
			group, _, _ := strings.Cut(strings.TrimPrefix(rest, "apis/"), "/")
			assert.True(t, strings.HasSuffix(group, ".cattle.io") || strings.HasSuffix(rest, "/selfsubjectreviews"),
				"a downstream row sends nothing through local but Rancher's resources and the SelfSubjectReview: %s", q)
		}
	}
	switch {
	case row.kindErr != nil:
		want := *row.kindErr
		want.Message = strings.ReplaceAll(want.Message, "<id>", id)
		want.Hint = strings.ReplaceAll(want.Hint, "<id>", id)
		// The run's directory is not the platform default, so its hint names it.
		want.Hint = withDirectory(h.cfgDir, want.Hint)
		if assert.NotNil(t, r.err, r.stderr) {
			assert.Equal(t, want, *r.err)
		}
	case r.err != nil:
		assert.NotContains(t, r.err.Message, "gave no reason", "the local proxy's refusal")
	case !slices.Contains(args, "-o"):
		for line := range strings.Lines(r.stderr) {
			if strings.HasPrefix(line, "error") {
				assert.NotContains(t, line, "gave no reason", "the local proxy's refusal")
			}
		}
	}
	assert.True(t, r.reached(row.target, id), "reaches %s on %s: %v", row.target, id, sent)
	if row.plugin {
		var cred struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &cred), "stdout is one JSON document: %q %s", r.stdout, r.stderr)
		assert.Equal(t, "ExecCredential", cred.Kind)
	}
	if !collide || !resolves {
		return
	}
	want := callerClusterWarning
	if args[0] == "inspect" && args[1] == "cluster" || slices.Contains(args, "--clusters") {
		want = callerPositionalWarning
	}
	if row.plugin || !slices.Contains(args, "-o") {
		assert.Contains(t, r.stderr, want, "the id wins with a warning")
		return
	}
	assert.Contains(t, r.warnings, want, "the id wins with a warning")
}
