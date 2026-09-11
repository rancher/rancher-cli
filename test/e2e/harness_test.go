//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	binPath    string
	shimURL    string
	shimClient *http.Client // trusts the shim's cert; use for Go-side HTTP calls to shimURL
	dynCli     dynamic.Interface
	// proxy rewrites into the envtest apiserver; built once in TestMain
	// (concurrency-safe) and shared by the global router AND every
	// per-script router newRouter builds - only the bearer check varies per
	// instance, never the transport.
	proxy *httputil.ReverseProxy
)

// testToken must satisfy BOTH the shim bearer check AND auth login's
// id:secret format (the auth-login scenario stores and replays it).
const testToken = "e2e:secret"

// legacyScripts marks per-script facade instances that must run in legacy
// mode (the ext.cattle.io create route 404s), keyed by txtar base name
// (matched against the testscript WorkDir, "script-<name>").
var legacyScripts = map[string]bool{
	"get-token-legacy": true,
}

// scriptOptions adjusts a script's own facade options before the facade is
// built, keyed the way legacyScripts is.
var scriptOptions = map[string]func(*testrancher.Options){
	"login-recovery":       func(o *testrancher.Options) { o.DropDerivedCreateResponseOnce = true },
	"auth-login-providers": func(o *testrancher.Options) { o.ExtraProviders = []string{"githubProvider"} },
	// One cluster per page, so the collection reader follows steve's
	// continue token and validates the count across pages.
	"access-clusters": func(o *testrancher.Options) { o.ClustersPageSize = 1 },
	"access-projects": func(o *testrancher.Options) { o.ClustersPageSize = 1 },
}

// shimClusters are the management Clusters the facade's steve collection
// lists: the envtest fixture cluster and the management cluster itself.
var shimClusters = []testrancher.ClusterFixture{
	{ID: "c-test", DisplayName: "testcluster"},
	{ID: "local", DisplayName: "local"},
}

// shimProjects are the management Projects the facade's steve collection
// lists: two on the envtest fixture cluster and one on the management
// cluster, whose projects a named user's run leaves not applicable.
var shimProjects = []testrancher.ProjectFixture{
	{Cluster: "c-test", ID: "p-e2e", DisplayName: "E2E"},
	{Cluster: "c-test", ID: "p-explain", DisplayName: "Explain"},
	{Cluster: "local", ID: "p-local", DisplayName: "Local"},
}

// shimNamespaces are the namespaces the facade's steve collection lists per
// cluster, with the project each carries: team-a and team-b belong to p-e2e,
// team-c to p-explain (the envtest RoleBindings decide who may get each),
// and the rest to no project.
var shimNamespaces = map[string][]testrancher.NamespaceFixture{
	"c-test": {{Name: "team-a", Project: "p-e2e"}, {Name: "team-b", Project: "p-e2e"}, {Name: "team-c", Project: "p-explain"}, {Name: "fleet-default"}},
	"local":  {{Name: "fleet-default"}, {Name: "cattle-system", Project: "p-local"}},
}

// seesProject reports whether the identity's steve view carries a project:
// every project of a reachable cluster for an identity with no project set,
// exactly the listed ones otherwise.
func (id shimIdentity) seesProject(cluster, project string) bool {
	if id.clusters != nil && !slices.Contains(id.clusters, cluster) {
		return false
	}
	return id.projects == nil || slices.Contains(id.projects, cluster+"/"+project)
}

// projectVisibility answers the facade's steve projects collection with the
// projects the identity sees.
func projectVisibility(resolve shimIdentityResolver) func(string) ([]testrancher.ProjectFixture, bool) {
	return func(bearer string) ([]testrancher.ProjectFixture, bool) {
		id, ok := resolve(bearer)
		if !ok {
			return nil, false
		}
		out := []testrancher.ProjectFixture{}
		for _, p := range shimProjects {
			if id.seesProject(p.Cluster, p.ID) {
				out = append(out, p)
			}
		}
		return out, true
	}
}

// namespaceVisibility answers a cluster's steve namespace collection: an
// administrator's view sees every namespace of a reachable cluster; a member
// sees the namespaces of its projects and nothing else, as steve partitions
// them; a cluster the identity may not reach answers 401.
func namespaceVisibility(resolve shimIdentityResolver) func(string, string) ([]testrancher.NamespaceFixture, bool) {
	return func(bearer, cluster string) ([]testrancher.NamespaceFixture, bool) {
		id, ok := resolve(bearer)
		if !ok {
			return nil, false
		}
		if id.clusters != nil && !slices.Contains(id.clusters, cluster) {
			return nil, false
		}
		if id.projects == nil {
			return slices.Clone(shimNamespaces[cluster]), true
		}
		// A member's view: the namespaces of its projects, and the
		// unowned namespaces its grants let it see (a real member sees
		// the management namespaces Rancher grants members on local); a
		// namespace of another project never.
		out := []testrancher.NamespaceFixture{}
		for _, ns := range shimNamespaces[cluster] {
			if ns.Project == "" || id.seesProject(cluster, ns.Project) {
				out = append(out, ns)
			}
		}
		return out, true
	}
}

// clusterVisibility answers the facade's steve clusters collection from
// the shim identities: an identity restricted to some clusters sees only
// those, every other identity sees them all, an unknown bearer is refused.
func clusterVisibility(resolve shimIdentityResolver) func(string) ([]testrancher.ClusterFixture, bool) {
	return func(bearer string) ([]testrancher.ClusterFixture, bool) {
		id, ok := resolve(bearer)
		if !ok {
			return nil, false
		}
		var out []testrancher.ClusterFixture
		for _, c := range shimClusters {
			if id.clusters == nil || slices.Contains(id.clusters, c.ID) {
				out = append(out, c)
			}
		}
		if id.twinCluster {
			out = append(out, testrancher.ClusterFixture{ID: "c-twin", DisplayName: "testcluster"})
		}
		return out, true
	}
}

// scriptChecks run after their script, with the facade it ran against, for
// the assertions a script cannot make about what the server recorded.
var scriptChecks = map[string]func(t testscript.T, f *testrancher.Facade){
	"login-recovery":   checkLoginRecovery,
	"exec-token-reuse": checkExecTokenReuse,
}

// scriptSkipped reports whether the script ended in a skip (a `[!unix]
// skip` line, for one), in which case the facade saw none of the requests
// a scriptChecks entry asserts on and the check must not run. Deferred
// functions run after a skip too, so this is the only place to tell the
// two teardowns apart. testscript's T is backed by *testing.T, whose
// Skipped carries the answer; a T of another shape reports false and the
// check runs as before.
func scriptSkipped(t testscript.T) bool {
	s, ok := t.(interface{ Skipped() bool })
	return ok && s.Skipped()
}

// checkLoginRecovery asserts what login-recovery.txtar cannot see: the
// delete verb ran a dry run and an execution against the leftover token,
// bound to one resource version, and the token the failed login left
// behind is the one that went away. The revokes a login performs carry
// neither a dry run nor a precondition, so the filter isolates the delete
// verb's two attempts.
func checkLoginRecovery(t testscript.T, f *testrancher.Facade) {
	var attempts []testrancher.DeleteRecord
	for _, r := range f.DeleteRequests() {
		if r.Kind == "ext" && (r.DryRun || r.Precondition != "") {
			attempts = append(attempts, r)
		}
	}
	if len(attempts) != 2 {
		t.Fatal(fmt.Sprintf("expected the delete verb's dry run and execution against the leftover, got %d ext attempts: %+v", len(attempts), attempts))
	}
	if !attempts[0].DryRun || attempts[1].DryRun {
		t.Fatal(fmt.Sprintf("expected dry run then execution, got %+v", attempts))
	}
	if attempts[0].Precondition == "" || attempts[0].Precondition != attempts[1].Precondition {
		t.Fatal(fmt.Sprintf("expected the same non-empty resourceVersion precondition in preview and execution, got %+v", attempts))
	}

	descs := f.LoginDescriptions()
	if len(descs) == 0 {
		t.Fatal("no login description recorded")
	}
	i := strings.Index(descs[0], "[op:")
	if i < 0 {
		t.Fatal(fmt.Sprintf("the first login description carries no operation marker: %q", descs[0]))
	}
	first := descs[0][i:]
	if n := len(f.TokensWithDescriptionContaining("ext", first)); n != 0 {
		t.Fatal(fmt.Sprintf("expected the first login's marked token to be gone, found %d", n))
	}
	if n := len(f.TokensWithDescriptionContaining("ext", "[op:")); n != 1 {
		t.Fatal(fmt.Sprintf("expected exactly the second login's derived token to remain, found %d marked ext tokens", n))
	}
}

// checkExecTokenReuse asserts what exec-token-reuse.txtar cannot see: the
// plugin login made exactly one marked derived create and one marked
// cluster create with that derived token as bearer, the silent second
// cluster's create was unmarked and used the same bearer, and the recovery
// sequence's delete carried a dry run and an execution with one
// resourceVersion. The scoped interactive logins the script runs with the
// setting off go through /v1-public/login, recorded with the session role,
// so they are outside both create lists.
func checkExecTokenReuse(t testscript.T, f *testrancher.Facade) {
	var derived, marked, unmarked []testrancher.CreateRecord
	for _, c := range f.CreateRequests() {
		switch {
		case c.Role == "derived":
			derived = append(derived, c)
		case c.Role == "cluster" && strings.Contains(c.Description, "[op:"):
			marked = append(marked, c)
		case c.Role == "cluster":
			unmarked = append(unmarked, c)
		}
	}
	// Two derived creates: the plugin login's D and the recovery login's D.
	if len(derived) != 2 {
		t.Fatal(fmt.Sprintf("expected two derived creates, got %d: %+v", len(derived), derived))
	}
	if len(marked) != 1 || marked[0].Bearer == "" {
		t.Fatal(fmt.Sprintf("expected one marked cluster create with the plugin login as bearer, got %+v", marked))
	}
	if len(unmarked) != 1 || unmarked[0].Bearer != marked[0].Bearer {
		t.Fatal(fmt.Sprintf("expected one unmarked silent cluster create with the same bearer, got %+v", unmarked))
	}
	var attempts []testrancher.DeleteRecord
	for _, r := range f.DeleteRequests() {
		if r.Kind == "ext" && (r.DryRun || r.Precondition != "") {
			attempts = append(attempts, r)
		}
	}
	if len(attempts) != 2 || !attempts[0].DryRun || attempts[1].DryRun || attempts[0].Precondition == "" || attempts[0].Precondition != attempts[1].Precondition {
		t.Fatal(fmt.Sprintf("expected the delete verb's dry run and execution with one precondition, got %+v", attempts))
	}
}

// shimIdentity is what a bearer maps to and the groups envtest must see for
// it. Groups are server-owned: the shim injects exactly these, never what
// the CLI sent.
type shimIdentity struct {
	user           string
	groups         []string
	canImpersonate bool     // holds impersonate on users/serviceaccounts, a Rancher GlobalRole right
	clusters       []string // cluster ids the identity may reach; nil = all
	// projects are the <cluster>/<project> ids the identity sees on the
	// steve project collection, and whose namespaces it sees on a cluster's
	// steve namespace collection; nil = every project of a reachable
	// cluster (an administrator's view).
	projects []string
	// twinCluster adds a second steve cluster row that shares testcluster's
	// display name, so a --cluster value by name is ambiguous for this
	// identity alone.
	twinCluster bool
}

// shimIdentities are the fixed bearers every router knows: the harness admin
// and the two named callers the access scenarios log in as.
var shimIdentities = map[string]shimIdentity{
	testToken:              {user: "e2e-admin", groups: []string{"system:authenticated", "system:cattle:authenticated"}, canImpersonate: true},
	"e2e-std:secret":       {user: "e2e-std", groups: []string{"system:authenticated", "system:cattle:authenticated"}},
	"e2e-nocluster:secret": {user: "e2e-nocluster", groups: []string{"system:authenticated", "system:cattle:authenticated"}, clusters: []string{"local"}},
	// A project member: steve shows it its one project and that project's
	// namespaces, and the management bindings are refused to it.
	"e2e-member:secret": {user: "e2e-member", groups: []string{"system:authenticated", "system:cattle:authenticated"}, projects: []string{"c-test/p-explain"}},
	// An administrator whose steve view carries a second cluster named
	// testcluster, for the ambiguous --cluster scenario.
	"e2e-twin:secret": {user: "e2e-admin", groups: []string{"system:authenticated", "system:cattle:authenticated"}, canImpersonate: true, twinCluster: true},
}

// shimSubjectGroups are the subjects an identity holding impersonate may act
// as, with the groups Rancher owns for each. A subject absent here is one the
// cluster does not know.
var shimSubjectGroups = map[string][]string{
	"e2e-std":                         {"system:authenticated", "system:cattle:authenticated"},
	"e2e-nocluster":                   {"system:authenticated", "system:cattle:authenticated"},
	"e2e-member":                      {"system:authenticated", "system:cattle:authenticated"},
	"e2e-compl-u1":                    {"system:authenticated", "system:cattle:authenticated"},
	"system:serviceaccount:team-a:ci": {"system:serviceaccounts", "system:serviceaccounts:team-a", "system:authenticated"},
	"system:anonymous":                {"system:unauthenticated"},
}

// shimClusterAccess restricts a subject to the listed clusters; a subject
// absent here reaches every cluster the shim serves.
var shimClusterAccess = map[string][]string{"e2e-nocluster": {"local"}}

// shimIdentityResolver maps a bearer to the identity the shim acts as. It
// runs per request, so a bearer the facade created moments ago is valid and
// a revoked one is refused without rebuilding the router.
type shimIdentityResolver func(bearer string) (shimIdentity, bool)

// staticIdentityResolver serves tests that need fixed identities only.
func staticIdentityResolver(identities map[string]shimIdentity) shimIdentityResolver {
	return func(bearer string) (shimIdentity, bool) {
		id, ok := identities[bearer]
		return id, ok
	}
}

// facadeIdentityResolver composes the fixed identities with a facade's
// bearer check: a bearer the facade accepts acts as the admin identity
// (every facade bearer was authorized as the testToken principal before),
// and stops being accepted the moment the facade revokes it. validBearer is
// a callback rather than the facade itself because every call site builds
// its router before the facade that wraps it exists.
func facadeIdentityResolver(identities map[string]shimIdentity, validBearer func(string) bool) shimIdentityResolver {
	return func(bearer string) (shimIdentity, bool) {
		if id, ok := identities[bearer]; ok {
			return id, true
		}
		if validBearer(bearer) {
			return identities[testToken], true
		}
		return shimIdentity{}, false
	}
}

// shimInjection is one proxied request as the downstream apiserver saw it:
// the identity the shim injected and the path it was injected on.
type shimInjection struct {
	identity shimIdentity
	path     string
}

// shimRecorder collects the identities the shim injected, for the assertions
// the CLI's own output cannot make (which user and which server-owned groups
// reached the downstream authorizer).
type shimRecorder struct {
	mu       sync.Mutex
	injected []shimInjection
}

func (r *shimRecorder) record(id shimIdentity, path string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injected = append(r.injected, shimInjection{identity: id, path: path})
}

// Injected returns a copy of what the shim has injected so far.
func (r *shimRecorder) Injected() []shimInjection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.injected)
}

// Reset drops the recording so one server can serve several runs.
func (r *shimRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injected = nil
}

// shimClusterAccessRefusal writes Rancher's answer for an identity without
// access to a cluster: 403 carrying its error object, served as text/plain -
// it is not a Kubernetes Status.
func shimClusterAccessRefusal(w http.ResponseWriter, cluster, user string) {
	// Marshalled rather than formatted: the message quotes the cluster id
	// and the user, and a hand-written JSON literal would emit those quotes
	// unescaped and make the body unparseable.
	body, err := json.Marshal(map[string]any{
		"Code":      map[string]any{"Code": "Forbidden", "Status": 403},
		"Message":   fmt.Sprintf(`clusters.management.cattle.io %q is forbidden: User %q cannot get resource "clusters" in API group "management.cattle.io" at the cluster scope`, cluster, user),
		"Cause":     nil,
		"FieldName": "",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}

// newRouter builds the shim's cluster-proxy handler: a bearer resolved to an
// identity, then the /k8s/clusters/<id> prefix rewrite into the shared
// envtest reverse proxy, with the resolved identity injected as impersonation
// headers. It is a constructor, not a single shared instance, because auth
// scenarios need their own facade's ValidBearer composed in without a union
// of issued-token sets across scripts (FA-2/FA-3): deterministic
// per-instance token names/secrets mean two scripts can hold the literal
// same bearer, and a shared validity set would keep one script's revoked
// bearer alive via another script's set.
func newRouter(resolve shimIdentityResolver) http.Handler {
	return newRecordingRouter(resolve, nil)
}

// newRecordingRouter is newRouter with a recorder of the identities it
// injected. Every impersonation decision lives here so the recording cannot
// drift from what the downstream apiserver actually saw.
func newRecordingRouter(resolve shimIdentityResolver, rec *shimRecorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id, ok := resolve(strings.TrimPrefix(auth, prefix))
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var cluster, trimmed string
		switch {
		case strings.HasPrefix(r.URL.Path, "/k8s/clusters/local/"):
			cluster, trimmed = "local", strings.TrimPrefix(r.URL.Path, "/k8s/clusters/local")
		case strings.HasPrefix(r.URL.Path, "/k8s/clusters/c-test/"):
			cluster, trimmed = "c-test", strings.TrimPrefix(r.URL.Path, "/k8s/clusters/c-test")
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Capture the requested subject, then strip every impersonation
		// header the CLI sent: only the shim injects identity.
		requested := r.Header.Get("Impersonate-User")
		for k := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(k), "Impersonate-") {
				r.Header.Del(k)
			}
		}
		acting := id
		// Rancher's local route drops the header silently: no impersonation
		// on local, whatever was requested.
		if requested != "" && requested != id.user && cluster != "local" {
			if !id.canImpersonate {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"not allowed to impersonate user"}`)
				return
			}
			groups, known := shimSubjectGroups[requested]
			if !known && strings.HasPrefix(requested, "system:serviceaccount:") {
				// Rancher cannot create the impersonated service account's
				// token: plain-text 500, the shape observed live.
				name := requested[strings.LastIndexByte(requested, ':')+1:]
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprintf(w, "unable to create token for impersonated ServiceAccount: can't create token: serviceaccounts %q not found", name)
				return
			}
			if !known {
				// A nonexistent user has no cluster access: the same refusal
				// an existing user without access gets.
				shimClusterAccessRefusal(w, cluster, requested)
				return
			}
			acting = shimIdentity{user: requested, groups: groups, clusters: shimClusterAccess[requested]}
		}
		if acting.clusters != nil && !slices.Contains(acting.clusters, cluster) {
			shimClusterAccessRefusal(w, cluster, acting.user)
			return
		}
		r.URL.Path = trimmed
		r.Header.Del("Authorization") // transport injects envtest creds
		r.Header.Set("Impersonate-User", acting.user)
		for _, g := range acting.groups {
			r.Header.Add("Impersonate-Group", g)
		}
		rec.record(acting, trimmed)
		proxy.ServeHTTP(w, r)
	})
}

func TestMain(m *testing.M) {
	// legacyScripts keys must resolve to real txtars, loudly, for the same
	// reason Setup's own scriptName derivation fails loudly: a typo'd key or
	// a renamed txtar must be an obvious harness-drift error, never a
	// confusing scenario failure (e.g. get-token-legacy.txtar's stderr
	// assertion failing because its facade silently ran non-legacy).
	for name := range legacyScripts {
		if _, err := os.Stat(filepath.Join("scripts", name+".txtar")); err != nil {
			panic(fmt.Sprintf("legacyScripts references missing scripts/%s.txtar: %v", name, err))
		}
	}
	for name := range scriptOptions {
		if _, err := os.Stat(filepath.Join("scripts", name+".txtar")); err != nil {
			panic(fmt.Sprintf("scriptOptions references missing scripts/%s.txtar: %v", name, err))
		}
	}
	for name := range scriptChecks {
		if _, err := os.Stat(filepath.Join("scripts", name+".txtar")); err != nil {
			panic(fmt.Sprintf("scriptChecks references missing scripts/%s.txtar: %v", name, err))
		}
	}

	tmp, err := os.MkdirTemp("", "rancher-e2e-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}
	// Scripts invoke "rancher" through PATH; on Windows the built binary
	// must carry the .exe suffix or exec never resolves it.
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	binPath = filepath.Join(tmp, "rancher"+ext)

	// Building the binary and starting envtest have no data dependency on
	// each other - run them concurrently so `make e2e` pays max(build,
	// envtest-start) instead of their sum.
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("testdata", "crds")}}
	var cfg *rest.Config
	build := func() error {
		if out, err := exec.Command("go", "build", "-o", binPath, "github.com/rancher/rancher-cli/cmd/rancher").CombinedOutput(); err != nil {
			return fmt.Errorf("building rancher: %s", out)
		}
		fakeBin := filepath.Join(tmp, "rancher-credential-fake"+ext)
		if out, err := exec.Command("go", "build", "-o", fakeBin,
			"github.com/rancher/rancher-cli/internal/config/testdata/fakehelper").CombinedOutput(); err != nil {
			return fmt.Errorf("building fake credential helper: %v\n%s", err, out)
		}
		return nil
	}
	startEnv := func() error {
		var err error
		if cfg, err = env.Start(); err != nil {
			// The hint mirrors scripts/e2e's exact invocation - keep the two
			// in lockstep so a direct `go test` resolves the same binaries.
			return fmt.Errorf("envtest (run via `make e2e`, which sets KUBEBUILDER_ASSETS; otherwise run "+
				"`go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.24.1 use 1.34.0 -p path` and export it): %w", err)
		}
		return nil
	}
	if err := buildAndStartEnv(build, startEnv, func() { _ = env.Stop() }); err != nil {
		panic(err.Error())
	}

	transport, err := rest.TransportFor(cfg)
	if err != nil {
		panic("building envtest transport: " + err.Error())
	}
	target, err := url.Parse(cfg.Host)
	if err != nil {
		panic("parsing envtest host: " + err.Error())
	}
	proxy = &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport: transport,
	}

	// The global TLS server stays alive for its own narrow job - flipstatus
	// mutates shared envtest state through it (commands_test.go) - but no
	// script's TEST_RANCHER_URL points at it anymore; TestScenarios' Setup
	// gives every script its own facade-wrapped router instead.
	mux := http.NewServeMux()
	mux.Handle("/", newRouter(staticIdentityResolver(shimIdentities)))
	mux.HandleFunc("PUT /_test/status/", handleStatusLever)
	srv := httptest.NewTLSServer(mux)
	shimURL = srv.URL
	shimClient = srv.Client()

	dynCli, err = dynamic.NewForConfig(cfg)
	if err != nil {
		panic("building dynamic client: " + err.Error())
	}
	seedManagementCluster()
	seedNamespaces()
	seedDirectEndpointFixtures()
	seedAccessRBAC()
	seedCloudCredential()
	seedRoleObjects()
	seedUserObjects()
	seedUserAttributeObjects()
	seedManagementToken()

	code := m.Run()
	srv.Close()
	_ = env.Stop()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// buildAndStartEnv runs build and startEnv concurrently - they have no data
// dependency on each other, so `make e2e` pays max(build, envtest-start)
// instead of their sum. If either fails, stopEnv runs first when startEnv
// actually succeeded: errgroup.Wait blocks until BOTH goroutines return, so
// envStarted is settled by the time it's read here regardless of which
// goroutine failed. A build failure after envtest already started must
// still stop it - otherwise the etcd/kube-apiserver child processes outlive
// this function.
func buildAndStartEnv(build, startEnv func() error, stopEnv func()) error {
	var envStarted bool
	g := new(errgroup.Group)
	g.Go(build)
	g.Go(func() error {
		if err := startEnv(); err != nil {
			return err
		}
		envStarted = true
		return nil
	})
	if err := g.Wait(); err != nil {
		if envStarted {
			stopEnv()
		}
		return err
	}
	return nil
}

// DR-011 repro: a build failure racing a slower, successful envtest start
// must still stop envtest - the goroutine leaked etcd/kube-apiserver
// processes before this fix, since env.Stop() was only reachable via the
// sequential path after m.Run(), never on this early-panic path.
func TestBuildAndStartEnvStopsEnvOnBuildFailureAfterEnvStarted(t *testing.T) {
	t.Parallel()
	var stopped bool
	buildErr := errors.New("build failed")

	envRunning := make(chan struct{})
	err := buildAndStartEnv(
		func() error {
			<-envRunning // deterministic: the build fails only after envtest started
			return buildErr
		},
		func() error {
			close(envRunning) // envtest starts successfully, before the failing build
			return nil
		},
		func() { stopped = true },
	)

	require.ErrorIs(t, err, buildErr)
	assert.True(t, stopped, "envtest must be stopped when the build fails after envtest already started")
}

func TestBuildAndStartEnvSkipsStopWhenEnvNeverStarted(t *testing.T) {
	t.Parallel()
	var stopped bool
	envErr := errors.New("envtest failed to start")

	err := buildAndStartEnv(
		func() error { return nil },
		func() error { return envErr },
		func() { stopped = true },
	)

	require.ErrorIs(t, err, envErr)
	assert.False(t, stopped, "there is nothing to stop when envtest itself never started")
}

func TestBuildAndStartEnvNoErrorDoesNotStop(t *testing.T) {
	t.Parallel()
	var stopped bool

	err := buildAndStartEnv(
		func() error { return nil },
		func() error { return nil },
		func() { stopped = true },
	)

	require.NoError(t, err)
	assert.False(t, stopped)
}

// TestRouterValidatesSeedAndFacadeBearersAcrossRevocation proves the
// per-script wiring TestScenarios' Setup builds: a router composed from
// "seed OR this instance's facade.ValidBearer" authorizes both the static
// seed token and a facade-created bearer through the cluster proxy, and stops
// authorizing the created bearer the moment it is revoked.
func TestRouterValidatesSeedAndFacadeBearersAcrossRevocation(t *testing.T) {
	var facade *testrancher.Facade
	router := newRouter(facadeIdentityResolver(shimIdentities, func(tok string) bool {
		return facade != nil && facade.ValidBearer(tok)
	}))
	facade = testrancher.New(testrancher.Options{Fallthrough: router, SeedToken: testToken})
	srv := httptest.NewTLSServer(facade)
	defer srv.Close()
	client := srv.Client()

	const clustersPath = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	getClusters := func(bearer string) int {
		req, err := http.NewRequest(http.MethodGet, srv.URL+clustersPath, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, getClusters(testToken), "the seed token must still authorize through a per-script instance")

	createReq, err := http.NewRequest(http.MethodPost, srv.URL+"/k8s/clusters/local/apis/ext.cattle.io/v1/tokens",
		strings.NewReader(`{"spec":{}}`))
	require.NoError(t, err)
	createReq.Header.Set("Authorization", "Bearer "+testToken)
	createResp, err := client.Do(createReq)
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	var created struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.NewDecoder(createResp.Body).Decode(&created))
	require.NotEmpty(t, created.Status.BearerToken)

	assert.Equal(t, http.StatusOK, getClusters(created.Status.BearerToken), "a facade-created bearer must authorize")

	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/k8s/clusters/local/apis/ext.cattle.io/v1/tokens/fac-token-1", nil)
	require.NoError(t, err)
	delReq.Header.Set("Authorization", "Bearer "+testToken)
	delResp, err := client.Do(delReq)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	require.Equal(t, http.StatusOK, delResp.StatusCode)

	assert.Equal(t, http.StatusUnauthorized, getClusters(created.Status.BearerToken), "the same bearer must 401 at the proxy after revocation")
}

func TestScenarios(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "scripts",
		Setup: func(e *testscript.Env) error {
			e.Setenv("PATH", filepath.Dir(binPath)+string(os.PathListSeparator)+e.Getenv("PATH"))

			// go-internal v1.14.1 names workdirs "script-<name>"
			// (testscript.go:453); "<name>" is the txtar base name. Failing
			// loudly on a mismatch (rather than silently defaulting to
			// non-legacy) means a future testscript upgrade that changes
			// this naming breaks Setup immediately instead of quietly
			// running every script non-legacy.
			scriptName := strings.TrimPrefix(filepath.Base(e.WorkDir), "script-")
			if _, err := os.Stat(filepath.Join("scripts", scriptName+".txtar")); err != nil {
				return fmt.Errorf("deriving script name from WorkDir %q: no scripts/%s.txtar: %w", e.WorkDir, scriptName, err)
			}

			// facade is assigned below, after router is built: validBearer
			// only runs once real requests arrive, well after Setup returns,
			// so the forward reference is safe.
			var facade *testrancher.Facade
			resolve := facadeIdentityResolver(shimIdentities, func(t string) bool {
				return facade != nil && facade.ValidBearer(t)
			})
			router := newRouter(resolve)
			// The identity-provider stub lives on its own TLS origin so the
			// binary must reach it with the operating system trust store, not
			// the Rancher CA it was given: the same split a self-signed
			// Rancher and a public provider present.
			idpSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				facade.IdPHandler().ServeHTTP(w, r)
			}))
			// A distinct certificate: every httptest TLS server otherwise
			// presents the same built-in one, and RANCHER_CLI_CACERT would
			// trust the IdP by accident, hiding a pin leak.
			idpCert, idpCertPEM, err := testrancher.LoopbackTLSCertificate()
			if err != nil {
				return err
			}
			idpSrv.TLS = &tls.Config{Certificates: []tls.Certificate{idpCert}}
			idpSrv.StartTLS()
			e.Defer(idpSrv.Close)
			opts := testrancher.Options{
				Fallthrough:         router,
				Legacy:              legacyScripts[scriptName],
				SeedToken:           testToken,
				EnableIdP:           true,
				IdPURL:              idpSrv.URL,
				ClusterVisibility:   clusterVisibility(resolve),
				ProjectVisibility:   projectVisibility(resolve),
				NamespaceVisibility: namespaceVisibility(resolve),
			}
			if adjust := scriptOptions[scriptName]; adjust != nil {
				adjust(&opts)
			}
			facade = testrancher.New(opts)
			if check := scriptChecks[scriptName]; check != nil {
				e.Defer(func() {
					if scriptSkipped(e.T()) {
						return
					}
					check(e.T(), facade)
				})
			}
			facadeSrv := httptest.NewTLSServer(facade)
			e.Defer(facadeSrv.Close)
			// client-go's exec-credential plugin (kubectl, `rancher auth
			// get-token`) refuses to wire in exec auth over a plaintext
			// http:// server (the same credential-hygiene rule that keeps
			// bearer tokens off http), so the facade must be TLS and scripts
			// must be able to trust it. Scripts get this as INLINE PEM, not
			// a file path: config.Effective's CA and kube.RESTConfig
			// consume eff.CA as raw PEM bytes directly (only auth
			// get-token's and kubeconfig's own --cacert handling resolve a
			// path via ResolveCA), so a path here would break every other
			// command.
			facadeCACertPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: facadeSrv.Certificate().Raw}))

			e.Setenv("TEST_RANCHER_URL", facadeSrv.URL)
			e.Setenv("TEST_RANCHER_TOKEN", testToken)
			e.Setenv("TEST_RANCHER_CACERT", facadeCACertPEM)
			// Global CA trust for every script (env-only and config-flow alike):
			// RANCHER_CLI_CACERT is env-effective everywhere Effective() is consulted.
			// It does NOT reach `rancher kubeconfig`'s embedded CA, which reads
			// only the config FILE's server entry - config-flow scripts pass
			// --cacert on login for that.
			e.Setenv("RANCHER_CLI_CACERT", facadeCACertPEM)
			e.Setenv("RANCHER_CLI_CONFIG_DIR", filepath.Join(e.WorkDir, "rancher-config"))
			// E2E-A-003: credstore-pin.txtar clears this itself as its first
			// line (`env RANCHER_CLI_CREDENTIAL_STORE=`) - testscript.Env carries
			// no script name, so the exemption lives in the script.
			e.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
			// Go reads SSL_CERT_FILE into the system pool on every platform
			// (darwin and windows since Go 1.27). Written per script so
			// concurrent scripts never share a file.
			idpCAFile := filepath.Join(e.WorkDir, "idp-ca.pem")
			if err := os.WriteFile(idpCAFile, []byte(idpCertPEM), 0o600); err != nil {
				return err
			}
			e.Setenv("SSL_CERT_FILE", idpCAFile)
			return nil
		},
		Condition: func(cond string) (bool, error) {
			if cond == "kubectl" {
				_, err := exec.LookPath("kubectl")
				return err == nil, nil
			}
			return false, fmt.Errorf("unknown condition %q", cond)
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"jsonquery":     cmdJSONQuery,
			"pathmatch":     cmdPathmatch,
			"flipstatus":    cmdFlipStatus,
			"rawget":        cmdRawGet,
			"seedexeccache": cmdSeedExecCache,
			"seedobj":       cmdSeedObj,
			"jsonget":       cmdJSONGet,
			"setenvfrom":    cmdSetenvFrom,
		},
	})
}

// seedManagementCluster creates the management cluster c-test so
// ResolveCluster's name-or-id paths work with the display name "testcluster".
func seedManagementCluster() {
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "c-test"},
		"spec":       map[string]any{"displayName": "testcluster"},
	}}
	if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding management cluster c-test: " + err.Error())
	}
}

// seedDirectEndpointFixtures creates the management objects the kubeconfig
// scenario reads for the Authorized Cluster Endpoint: three clusters with
// the endpoint enabled (one with an fqdn, one with control-plane nodes in
// its namespace, one with neither), the management Nodes of the second, and
// the multi-cluster-management Feature switched on. c-test stays without
// the endpoint: logs-target.txtar asserts that. Global and one-time, like
// every fixture shared across the parallel scripts.
func seedDirectEndpointFixtures() {
	clusters := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}
	nodes := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "nodes"}
	features := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "features"}
	create := func(gvr schema.GroupVersionResource, ns string, obj map[string]any, status map[string]any) {
		u := &unstructured.Unstructured{Object: obj}
		created, err := dynCli.Resource(gvr).Namespace(ns).Create(context.Background(), u, metav1.CreateOptions{})
		if err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic(fmt.Sprintf("seeding %s %s: %v", gvr.Resource, u.GetName(), err))
		}
		if status == nil {
			return
		}
		created.Object["status"] = status
		if _, err := dynCli.Resource(gvr).Namespace(ns).UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
			panic(fmt.Sprintf("seeding %s %s status: %v", gvr.Resource, u.GetName(), err))
		}
	}
	cluster := func(id, display string, endpoint map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
			"metadata": map[string]any{"name": id},
			"spec":     map[string]any{"displayName": display, "localClusterAuthEndpoint": endpoint},
		}
	}
	create(clusters, "", cluster("c-ace-fqdn", "acefqdn", map[string]any{"enabled": true, "fqdn": "ace.example.test", "caCerts": directEndpointCAPEM}), nil)
	create(clusters, "", cluster("c-ace-nodes", "acenodes", map[string]any{"enabled": true}), map[string]any{"caCert": base64.StdEncoding.EncodeToString([]byte(directEndpointCAPEM))})
	create(clusters, "", cluster("c-ace-empty", "aceempty", map[string]any{"enabled": true}), nil)
	node := func(name, hostname string, controlPlane, ready bool, ip string) {
		readyStatus := "False"
		if ready {
			readyStatus = "True"
		}
		create(nodes, "c-ace-nodes", map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "Node",
			"metadata": map[string]any{"name": name, "namespace": "c-ace-nodes"},
			"spec":     map[string]any{"controlPlane": controlPlane, "worker": !controlPlane, "requestedHostname": hostname},
		}, map[string]any{"internalNodeStatus": map[string]any{
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": ip}},
			"conditions": []any{map[string]any{"type": "Ready", "status": readyStatus}},
		}})
	}
	node("m-cp1", "c-ace-nodes-cp1", true, false, "10.0.0.1")
	node("m-cp2", "c-ace-nodes-cp2", true, true, "10.0.0.2")
	node("m-w1", "c-ace-nodes-w1", false, true, "10.0.0.9")
	create(features, "", map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Feature",
		"metadata": map[string]any{"name": "multi-cluster-management"},
		"spec":     map[string]any{"value": true},
	}, nil)
}

// directEndpointCAPEM is the certificate the fixture endpoints trust; the
// scenario asserts it is carried, never that it verifies anything.
const directEndpointCAPEM = `-----BEGIN CERTIFICATE-----
MIIBdzCCAR2gAwIBAgIUKr65J82c+1SqibViZ5cGxxIfrcgwCgYIKoZIzj0EAwIw
ETEPMA0GA1UEAwwGYWNlLWNhMB4XDTI2MDkwODA0NTYzNVoXDTM2MDkwNTA0NTYz
NVowETEPMA0GA1UEAwwGYWNlLWNhMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE
jn3wmG0sXQW1AkCIn0qaQK0seOW4x/7wEt3bf1SFXzII6bhfwbX5oHLSVww/SR/D
93zvzJfwMwhlixB6Tyc8y6NTMFEwHQYDVR0OBBYEFM5WVeOdyCfPT3tUqdpHB2RS
rcWMMB8GA1UdIwQYMBaAFM5WVeOdyCfPT3tUqdpHB2RSrcWMMA8GA1UdEwEB/wQF
MAMBAf8wCgYIKoZIzj0EAwIDSAAwRQIgRxKTqdpB9fbVb/NudK7izP+EeBcR/Ala
xTcaSBkjD+QCIQCer+Qsv1FU0Wm7T/W+ZaAIOos95Ps3Kt62v9A1srSvgQ==
-----END CERTIFICATE-----
`

// seedNamespaces creates the namespaces scripts rely on beyond envtest's
// auto-created "default". "c-test" is required too: management.cattle.io
// kinds namespaced by cluster id (project, clusterroletemplatebinding) need
// the namespace object to exist before a namespaced create succeeds.
// "team-b" is the access suite's second project member, where a different
// subject holds a different rule, so a --project rollup mixes decisions.
// "cattle-system" is seeded here (not per-script) because the logs and
// support-bundle target tables both hardcode it, and per-script creation
// races across scripts sharing this one apiserver. "cattle-global-data" is
// Task 7's cloud-credential namespace (seedCloudCredential's Secret lives
// there) - seeded here for the same reason. "cattle-local-user-passwords" is
// where `create user` writes the backing password Secret.
func seedNamespaces() {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, name := range []string{"fleet-default", "team-a", "team-b", "team-c", "c-test", "c-test-p-explain", "c-test-p-explain2", "c-ace-fqdn", "c-ace-nodes", "c-ace-empty", "cattle-system", "cattle-global-data", "cattle-local-user-passwords"} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": name},
		}}
		if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("seeding namespace " + name + ": " + err.Error())
		}
	}
}

// seedCloudCredential creates the fixture cloud-credential Secrets in
// cattle-global-data - global and one-time (Task 7, review R2-2):
// `create cluster`'s node-driver e2e scenarios resolve --cloud-credential
// by display name, and a per-script Setup creating these same fixtures
// would race AlreadyExists across every script sharing this one envtest
// apiserver. cc-e2etest ("e2e-cred") carries the amazonec2 driver key;
// cc-e2edo ("e2e-do-cred") the digitalocean one, so the machine-config
// two-step flow pairs a DO config with a DO credential. The data
// key/value mirror cloudcred.go's driver-detection contract
// (<driver>credentialConfig-<key>); "ZHVtbXk=" is the base64 encoding of
// "dummy" - ResolveCloudCredential never reads a data VALUE (spec
// D-R2-1), so the content is an arbitrary placeholder.
func seedCloudCredential() {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	for _, cred := range []struct {
		name    string
		display string
		dataKey string
	}{
		{"cc-e2etest", "e2e-cred", "amazonec2credentialConfig-accessKey"},
		{"cc-e2edo", "e2e-do-cred", "digitaloceancredentialConfig-accessToken"},
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":        cred.name,
				"namespace":   "cattle-global-data",
				"annotations": map[string]any{"field.cattle.io/name": cred.display},
			},
			"data": map[string]any{cred.dataKey: "ZHVtbXk="},
		}}
		if _, err := dynCli.Resource(gvr).Namespace("cattle-global-data").Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("seeding cloud credential secret " + cred.name + ": " + err.Error())
		}
	}
}

// seedRoleObjects creates deterministic RoleTemplate and GlobalRole fixtures
// for role completion e2e rows. RoleTemplates carry a top-level "context"
// field (cluster-scoped CRD with x-kubernetes-preserve-unknown-fields: true
// at root, so the field is retained). GlobalRole is cluster-scoped with no
// context field - fetchGlobalRoles lists by name only.
func seedRoleObjects() {
	rtGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "roletemplates"}
	for _, rt := range []struct {
		name    string
		context string
	}{
		{"e2e-project-role", "project"},
		{"e2e-cluster-role", "cluster"},
		// project-member and cluster-member are the canonical role names used
		// by project-members.txtar; seeded so grant's role pre-flight passes.
		{"project-member", "project"},
		{"cluster-member", "cluster"},
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "RoleTemplate",
			"metadata":   map[string]any{"name": rt.name},
			"context":    rt.context,
		}}
		if _, err := dynCli.Resource(rtGVR).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("seeding roletemplate " + rt.name + ": " + err.Error())
		}
	}

	grGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalroles"}
	grObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "GlobalRole",
		"metadata":   map[string]any{"name": "e2e-global-role"},
		// explain follows a GlobalRole's inheritedClusterRoles to the
		// RoleTemplates behind a GlobalRoleBinding's downstream access.
		"inheritedClusterRoles": []any{"cluster-member"},
	}}
	if _, err := dynCli.Resource(grGVR).Create(context.Background(), grObj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding globalrole e2e-global-role: " + err.Error())
	}
	grbGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalrolebindings"}
	grbObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":     "management.cattle.io/v3",
		"kind":           "GlobalRoleBinding",
		"metadata":       map[string]any{"name": "e2e-grb-admin"},
		"userName":       "e2e-admin",
		"globalRoleName": "e2e-global-role",
	}}
	if _, err := dynCli.Resource(grbGVR).Create(context.Background(), grbObj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding globalrolebinding e2e-grb-admin: " + err.Error())
	}
}

// seedUserObjects creates deterministic User fixtures for subject completion
// e2e rows. Usernames are prefixed with "e2e-compl-" to avoid collisions with
// the per-script u-alice fixture in project-members.txtar (displayName: alice).
// principalIds use the local:// prefix so they are not treated as system users.
func seedUserObjects() {
	userGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "users"}
	for _, u := range []struct {
		id       string
		username string
	}{
		{"e2e-compl-u1", "e2e-compl-user1"},
		{"e2e-compl-u2", "e2e-compl-user2"},
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion":   "management.cattle.io/v3",
			"kind":         "User",
			"metadata":     map[string]any{"name": u.id},
			"username":     u.username,
			"principalIds": []any{"local://" + u.id},
		}}
		if _, err := dynCli.Resource(userGVR).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("seeding user " + u.id + ": " + err.Error())
		}
	}
}

// seedUserAttributeObjects creates a UserAttribute fixture for
// inspect-user.txtar: UserAttribute carries no kind-table row (it is an
// enrichment source, not an operator-facing resource - `create resource -f`
// cannot address it), so it is seeded here directly like the role and user
// fixtures above. One group principal under one provider is enough for the
// composite's groups section and its held-bindings group matching to have
// something to read.
func seedUserAttributeObjects() {
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "userattributes"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "UserAttribute",
		"metadata":   map[string]any{"name": "u-bob"},
		"GroupPrincipals": map[string]any{
			"okta": map[string]any{
				"Items": []any{
					map[string]any{
						"metadata":    map[string]any{"name": "okta_group://eng"},
						"displayName": "Engineering",
					},
				},
			},
		},
	}}
	if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding userattribute u-bob: " + err.Error())
	}
}

// seedManagementToken creates the management.cattle.io/v3 Token whose name
// matches the harness credential id ("e2e" - the id portion of
// testToken "e2e:secret"). Seeding it globally (rather than per-script)
// ensures auth status's PS-2 v3 fallback probe returns deterministic metadata
// regardless of script execution order, and lets management-token.txtar's
// session-guard test rely on it without its own seedobj call racing other
// scripts on the shared apiserver.
func seedManagementToken() {
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "tokens"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":   "management.cattle.io/v3",
		"kind":         "Token",
		"metadata":     map[string]any{"name": "e2e"},
		"userId":       "u-session",
		"authProvider": "local",
	}}
	if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding management token e2e: " + err.Error())
	}
}

// seedAccessRBAC creates what the access scenarios ask the real authorizer
// about. The shim impersonates every caller now, so the harness admin is a
// named user rather than envtest's certificate identity and needs its
// cluster-admin binding here; the rest are the narrow grants the scenarios
// turn into allowed and denied answers, deliberately scoped so no identity
// is credited with more than the one rule under test. It also labels team-a
// and team-b into project p-e2e and creates the management-plane User and
// Project objects `--as` and `--project` resolve against.
func seedAccessRBAC() {
	rbacGVR := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: resource}
	}
	create := func(gvr schema.GroupVersionResource, namespace string, obj map[string]any) {
		u := &unstructured.Unstructured{Object: obj}
		if _, err := dynCli.Resource(gvr).Namespace(namespace).Create(context.Background(), u, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("seeding access rbac " + gvr.Resource + " " + u.GetName() + ": " + err.Error())
		}
	}
	rule := func(apiGroup string, resources []string, verbs []string) map[string]any {
		return map[string]any{
			"apiGroups": []any{apiGroup},
			"resources": toAny(resources),
			"verbs":     toAny(verbs),
		}
	}
	rules := func(apiGroup string, resources []string, verbs []string) []any {
		return []any{rule(apiGroup, resources, verbs)}
	}
	roleRef := func(kind, name string) map[string]any {
		return map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}
	}
	subject := func(kind, name string) []any {
		return []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}}
	}

	// The impersonated harness admin: every existing scenario runs as this
	// identity, so it must hold what envtest's certificate identity did.
	create(rbacGVR("clusterrolebindings"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "e2e-admin", "annotations": map[string]any{
			"authz.cluster.cattle.io/globalrolebinding": "e2e-grb-admin",
			"authz.cluster.cattle.io/globalrole":        "e2e-global-role",
		}},
		"roleRef":  roleRef("ClusterRole", "cluster-admin"),
		"subjects": subject("User", "e2e-admin"),
	})
	// Every Rancher user may list clusters on the management plane - the
	// proxy, not RBAC, decides which of them it will route to. `--cluster
	// testcluster` resolves a display name through that list before any
	// access question is asked, so a non-admin caller reaching the proxy's
	// refusal at all depends on this grant.
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-cluster-lister"},
		"rules":    rules("management.cattle.io", []string{"clusters"}, []string{"get", "list"}),
	})
	create(rbacGVR("clusterrolebindings"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "e2e-cluster-lister"},
		"roleRef":  roleRef("ClusterRole", "e2e-cluster-lister"),
		"subjects": []any{
			map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": "e2e-std"},
			map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": "e2e-nocluster"},
		},
	})
	// The namespaces rule is bound only through the team-a RoleBinding
	// below, so it authorizes a read of team-a and no other namespace: the
	// scope resolver reads the namespace before it asks anything about it.
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-pod-reader"},
		"rules": []any{
			rule("", []string{"pods"}, []string{"get", "list"}),
			rule("", []string{"namespaces"}, []string{"get"}),
		},
	})
	create(rbacGVR("rolebindings"), "team-a", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-std-pods", "namespace": "team-a"},
		"roleRef":  roleRef("ClusterRole", "e2e-pod-reader"),
		"subjects": subject("User", "e2e-std"),
	})
	// One resource in one namespace for system:authenticated: a cluster-wide
	// view grant would credit every authenticated identity with pods
	// everywhere and erase both the team-b denial and the mixed rollup.
	create(rbacGVR("roles"), "team-a", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "e2e-authenticated-deployments", "namespace": "team-a"},
		"rules":    rules("apps", []string{"deployments"}, []string{"get"}),
	})
	create(rbacGVR("rolebindings"), "team-a", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-authenticated-deployments", "namespace": "team-a"},
		"roleRef":  roleRef("Role", "e2e-authenticated-deployments"),
		"subjects": subject("Group", "system:authenticated"),
	})
	create(rbacGVR("roles"), "team-a", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "e2e-ci-configmaps", "namespace": "team-a"},
		"rules":    rules("", []string{"configmaps"}, []string{"get"}),
	})
	create(rbacGVR("rolebindings"), "team-a", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-ci-configmaps", "namespace": "team-a"},
		"roleRef":  roleRef("Role", "e2e-ci-configmaps"),
		"subjects": subject("Group", "system:serviceaccounts:team-a"),
	})
	create(rbacGVR("roles"), "team-b", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "e2e-team-b-secrets", "namespace": "team-b"},
		"rules":    rules("", []string{"secrets"}, []string{"get"}),
	})
	create(rbacGVR("rolebindings"), "team-b", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-team-b-secrets", "namespace": "team-b"},
		"roleRef":  roleRef("Role", "e2e-team-b-secrets"),
		"subjects": subject("User", "e2e-compl-u1"),
	})

	// who-can's two remaining candidate shapes, both in team-b: a group
	// principal (Rancher's UserAttribute cache names u-bob as a member of
	// okta_group://eng, so the group row lists a known member) and a
	// username with no Rancher User (asked with system:authenticated only:
	// an allowed answer is definite, a denied one is unknown).
	create(rbacGVR("roles"), "team-b", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "e2e-eng-configmaps", "namespace": "team-b"},
		"rules":    rules("", []string{"configmaps"}, []string{"get"}),
	})
	create(rbacGVR("rolebindings"), "team-b", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-eng-configmaps", "namespace": "team-b"},
		"roleRef":  roleRef("Role", "e2e-eng-configmaps"),
		"subjects": subject("Group", "okta_group://eng"),
	})
	create(rbacGVR("rolebindings"), "team-b", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-external-secrets", "namespace": "team-b"},
		"roleRef":  roleRef("Role", "e2e-team-b-secrets"),
		"subjects": subject("User", "oidc-external@corp"),
	})
	// explain's fixtures live in team-c and two PRTB namespaces of their
	// own so no other scenario's rules or member lists change: e2e-std
	// holds delete on pods in team-c through a
	// RoleBinding Rancher would have written for a ProjectRoleTemplateBinding
	// (classic label grammar), get on nodes through a ClusterRoleBinding of
	// the aggregation grammar owned by a ClusterRoleTemplateBinding, and get
	// on namespaces through a ClusterRoleBinding whose aggregation key is the
	// one every project's creator binding shares; the role names the
	// project, which is how the shared key is narrowed.
	mgmt := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: resource}
	}
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-explain-promoted"},
		"rules":    rules("", []string{"pods"}, []string{"get", "list", "delete"}),
	})
	create(rbacGVR("rolebindings"), "team-c", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-explain-rb", "namespace": "team-c",
			"labels": map[string]any{"authz.cluster.cattle.io/rtb-owner-updated": "c-test-p-explain_e2e-explain-prtb"}},
		"roleRef":  roleRef("ClusterRole", "e2e-explain-promoted"),
		"subjects": subject("User", "e2e-std"),
	})
	create(mgmt("projectroletemplatebindings"), "c-test-p-explain", map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "ProjectRoleTemplateBinding",
		"metadata":         map[string]any{"name": "e2e-explain-prtb", "namespace": "c-test-p-explain"},
		"userName":         "e2e-std",
		"roleTemplateName": "project-member",
		"projectName":      "c-test:p-explain",
	})
	// The project member the access projects scenario logs in as: it may
	// get its project's one namespace (what Rancher grants members) and
	// nothing else, so its own run answers visibility and no membership.
	create(rbacGVR("rolebindings"), "team-c", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "e2e-member-namespace", "namespace": "team-c"},
		"roleRef":  roleRef("ClusterRole", "e2e-pod-reader"),
		"subjects": subject("User", "e2e-member"),
	})
	create(mgmt("projectroletemplatebindings"), "c-test-p-explain", map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "ProjectRoleTemplateBinding",
		"metadata":         map[string]any{"name": "e2e-member-prtb", "namespace": "c-test-p-explain"},
		"userName":         "e2e-member",
		"roleTemplateName": "project-member",
		"projectName":      "c-test:p-explain",
	})
	// The aggregator carries the recorded structure: the clusterrole-owner
	// and aggregation labels, and an aggregationRule selecting the roles
	// labelled aggregates: cluster-member. envtest runs no aggregation
	// controller, so the aggregator also carries the rule it would
	// aggregate (as Rancher's own aggregator does on a real server).
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-explain-cluster-member", "labels": map[string]any{
			"authz.cluster.cattle.io/clusterrole-owner":     "cluster-member",
			"management.cattle.io/aggregates":               "cluster-member",
			"management.cattle.io/roletemplate-aggregation": "true",
		}},
		"rules": rules("", []string{"nodes"}, []string{"get"}),
	})
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-explain-nodes-aggregator", "labels": map[string]any{
			"authz.cluster.cattle.io/clusterrole-owner":     "cluster-member",
			"management.cattle.io/aggregates":               "e2e-explain-nodes-aggregator",
			"management.cattle.io/roletemplate-aggregation": "true",
		}},
		"aggregationRule": map[string]any{"clusterRoleSelectors": []any{
			map[string]any{"matchLabels": map[string]any{"management.cattle.io/aggregates": "cluster-member"}},
		}},
		"rules": rules("", []string{"nodes"}, []string{"get"}),
	})
	create(rbacGVR("clusterrolebindings"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "e2e-explain-agg-crb", "labels": map[string]any{
			"management.cattle.io/roletemplate-aggregation":       "true",
			"authz.cluster.cattle.io/crtb-owner-e2e-explain-crtb": "true",
		}},
		"roleRef":  roleRef("ClusterRole", "e2e-explain-nodes-aggregator"),
		"subjects": subject("User", "e2e-std"),
	})
	create(mgmt("clusterroletemplatebindings"), "c-test", map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "ClusterRoleTemplateBinding",
		"metadata":         map[string]any{"name": "e2e-explain-crtb", "namespace": "c-test"},
		"userName":         "e2e-std",
		"roleTemplateName": "cluster-member",
		"clusterName":      "c-test",
	})
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "p-explain-namespaces-edit"},
		"rules":    rules("", []string{"namespaces"}, []string{"get"}),
	})
	create(rbacGVR("clusterrolebindings"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "e2e-explain-shared-crb", "labels": map[string]any{
			"management.cattle.io/roletemplate-aggregation":            "true",
			"authz.cluster.cattle.io/prtb-owner-creator-project-owner": "true",
		}},
		"roleRef":  roleRef("ClusterRole", "p-explain-namespaces-edit"),
		"subjects": subject("User", "e2e-std"),
	})
	for _, project := range []string{"p-explain", "p-explain2"} {
		create(mgmt("projectroletemplatebindings"), "c-test-"+project, map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "ProjectRoleTemplateBinding",
			"metadata":         map[string]any{"name": "creator-project-owner", "namespace": "c-test-" + project},
			"userName":         "e2e-std",
			"roleTemplateName": "project-member",
			"projectName":      "c-test:" + project,
		})
	}
	// A Rancher user may read the management bindings that name it; the
	// shim grants e2e-std what Rancher grants every user, so the
	// held-bindings fallback has something to show when the downstream
	// refuses its RBAC objects.
	create(rbacGVR("clusterroles"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "e2e-explain-mgmt-reader"},
		"rules":    rules("management.cattle.io", []string{"clusterroletemplatebindings", "projectroletemplatebindings", "roletemplates"}, []string{"get", "list"}),
	})
	create(rbacGVR("clusterrolebindings"), "", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "e2e-explain-mgmt-reader"},
		"roleRef":  roleRef("ClusterRole", "e2e-explain-mgmt-reader"),
		"subjects": subject("User", "e2e-std"),
	})

	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, ns := range []string{"team-a", "team-b"} {
		patch := []byte(`{"metadata":{"labels":{"field.cattle.io/projectId":"p-e2e"}}}`)
		if _, err := dynCli.Resource(nsGVR).Patch(context.Background(), ns, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
			panic("labelling namespace " + ns + " into project p-e2e: " + err.Error())
		}
	}

	// The caller identities as Rancher users: `--as` resolves a name
	// against these, and the caller's own row enriches its subject.
	userGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "users"}
	for _, u := range []struct {
		id, username, display string
	}{
		{"e2e-admin", "admin", "E2E Admin"},
		{"e2e-std", "std", "Std User"},
		{"e2e-nocluster", "nocluster", "No Cluster"},
		{"e2e-member", "member", "Project Member"},
	} {
		create(userGVR, "", map[string]any{
			"apiVersion":   "management.cattle.io/v3",
			"kind":         "User",
			"metadata":     map[string]any{"name": u.id},
			"username":     u.username,
			"displayName":  u.display,
			"principalIds": []any{"local://" + u.id},
		})
	}

	projectGVR := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}
	for _, p := range []struct{ id, display string }{{"p-e2e", "E2E"}, {"p-empty", "Empty"}} {
		create(projectGVR, "c-test", map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "Project",
			"metadata":   map[string]any{"name": p.id, "namespace": "c-test"},
			"spec":       map[string]any{"displayName": p.display, "clusterName": "c-test"},
		})
	}
}

// toAny converts a string slice to the []any an unstructured object needs.
func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// handleStatusLever is the --wait lever: PUT /_test/status/<cluster>/<group>/<version>/<resource>/<ns>/<name>
// merge-patches the object's status subresource directly, standing in for a
// controller the shim does not emulate.
func handleStatusLever(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/_test/status/")
	parts := strings.SplitN(trimmed, "/", 6)
	if len(parts) != 6 {
		http.Error(w, "malformed status path: want /_test/status/<cluster>/<group>/<version>/<resource>/<ns>/<name>", http.StatusBadRequest)
		return
	}
	group, version, resource, ns, name := parts[1], parts[2], parts[3], parts[4], parts[5]
	// "core" is a sentinel for the empty API group (a bare group segment can't
	// round-trip through the URL path unambiguously): flipstatus local
	// core/v1/pods ... targets the core/v1 pods resource.
	if group == "core" {
		group = ""
	}
	// clusterScopedNSSegment (cmdFlipStatus) is the round-trip of "." for a
	// cluster-scoped resource: a literal "." or an empty segment are both
	// dot-segment/redundant-slash forms path normalization strips before
	// this handler ever sees them.
	if ns == clusterScopedNSSegment {
		ns = ""
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	patch := append([]byte(`{"status":`), append(body, '}')...)

	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	if _, err := dynCli.Resource(gvr).Namespace(ns).Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
