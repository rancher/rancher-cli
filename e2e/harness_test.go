//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
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
	"strings"
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
	// per-script router newRouter builds — only the bearer check varies per
	// instance, never the transport.
	proxy *httputil.ReverseProxy
)

// testToken must satisfy BOTH the shim bearer check AND auth login's
// id:secret format (the auth-login scenario stores and replays it).
const testToken = "e2e:secret"

// legacyScripts marks per-script façade instances that must run in legacy
// mode (the ext.cattle.io create route 404s), keyed by txtar base name
// (matched against the testscript WorkDir, "script-<name>").
var legacyScripts = map[string]bool{
	"get-token-legacy": true,
}

// newRouter builds the shim's cluster-proxy handler: a bearer check via
// validBearer, then the /k8s/clusters/<id> prefix rewrite into the shared
// envtest reverse proxy. It is a constructor, not a single shared instance,
// because auth scenarios need their own façade's ValidBearer composed in
// without a union of issued-token sets across scripts (FA-2/FA-3):
// deterministic per-instance token names/secrets mean two scripts can hold
// the literal same bearer, and a shared validity set would keep one
// script's revoked bearer alive via another script's set.
func newRouter(validBearer func(string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) || !validBearer(strings.TrimPrefix(auth, prefix)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var trimmed string
		switch {
		case strings.HasPrefix(r.URL.Path, "/k8s/clusters/local/"):
			trimmed = strings.TrimPrefix(r.URL.Path, "/k8s/clusters/local")
		case strings.HasPrefix(r.URL.Path, "/k8s/clusters/c-test/"):
			trimmed = strings.TrimPrefix(r.URL.Path, "/k8s/clusters/c-test")
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.URL.Path = trimmed
		r.Header.Del("Authorization") // transport injects envtest creds
		proxy.ServeHTTP(w, r)
	})
}

func TestMain(m *testing.M) {
	// legacyScripts keys must resolve to real txtars, loudly, for the same
	// reason Setup's own scriptName derivation fails loudly: a typo'd key or
	// a renamed txtar must be an obvious harness-drift error, never a
	// confusing scenario failure (e.g. get-token-legacy.txtar's stderr
	// assertion failing because its façade silently ran non-legacy).
	for name := range legacyScripts {
		if _, err := os.Stat(filepath.Join("scripts", name+".txtar")); err != nil {
			panic(fmt.Sprintf("legacyScripts references missing scripts/%s.txtar: %v", name, err))
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
	// each other — run them concurrently so `make e2e` pays max(build,
	// envtest-start) instead of their sum.
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("testdata", "crds")}}
	var cfg *rest.Config
	build := func() error {
		// cwd during `go test` is this package directory (e2e/); the module
		// root with the main package is one level up.
		if out, err := exec.Command("go", "build", "-o", binPath, "..").CombinedOutput(); err != nil {
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
			// The hint mirrors scripts/e2e's pinned invocation — keep the two
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

	// The global TLS server stays alive for its own narrow job — flipstatus
	// mutates shared envtest state through it (commands_test.go) — but no
	// script's TEST_RANCHER_URL points at it anymore; TestScenarios' Setup
	// gives every script its own façade-wrapped router instead.
	mux := http.NewServeMux()
	mux.Handle("/", newRouter(func(t string) bool { return t == testToken }))
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

// buildAndStartEnv runs build and startEnv concurrently — they have no data
// dependency on each other, so `make e2e` pays max(build, envtest-start)
// instead of their sum. If either fails, stopEnv runs first when startEnv
// actually succeeded: errgroup.Wait blocks until BOTH goroutines return, so
// envStarted is settled by the time it's read here regardless of which
// goroutine failed. A build failure after envtest already started must
// still stop it — otherwise the etcd/kube-apiserver child processes outlive
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
// must still stop envtest — the goroutine leaked etcd/kube-apiserver
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
// "seed OR this instance's façade.ValidBearer" authorizes both the static
// seed token and a façade-created bearer through the cluster proxy, and stops
// authorizing the created bearer the moment it is revoked.
func TestRouterValidatesSeedAndFacadeBearersAcrossRevocation(t *testing.T) {
	var facade *testrancher.Facade
	router := newRouter(func(tok string) bool {
		return tok == testToken || (facade != nil && facade.ValidBearer(tok))
	})
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

	assert.Equal(t, http.StatusOK, getClusters(created.Status.BearerToken), "a façade-created bearer must authorize")

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
			router := newRouter(func(t string) bool {
				return t == testToken || (facade != nil && facade.ValidBearer(t))
			})
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
			facade = testrancher.New(testrancher.Options{
				Fallthrough: router,
				Legacy:      legacyScripts[scriptName],
				SeedToken:   testToken,
				EnableIdP:   true,
				IdPURL:      idpSrv.URL,
			})
			facadeSrv := httptest.NewTLSServer(facade)
			e.Defer(facadeSrv.Close)
			// client-go's exec-credential plugin (kubectl, `rancher auth
			// get-token`) refuses to wire in exec auth over a plaintext
			// http:// server (the same credential-hygiene rule that keeps
			// bearer tokens off http), so the façade must be TLS and scripts
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
			// only the config FILE's server entry — config-flow scripts pass
			// --cacert on login for that.
			e.Setenv("RANCHER_CLI_CACERT", facadeCACertPEM)
			e.Setenv("RANCHER_CLI_CONFIG_DIR", filepath.Join(e.WorkDir, "rancher-config"))
			// E2E-A-003: credstore-pin.txtar clears this itself as its first
			// line (`env RANCHER_CLI_CREDENTIAL_STORE=`) — testscript.Env carries
			// no script name, so the exemption lives in the script.
			e.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
			// Go reads SSL_CERT_FILE into the system pool on Linux only; the
			// device-code script skips itself elsewhere. Written per script
			// so concurrent scripts never share a file.
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

// seedNamespaces creates the namespaces scripts rely on beyond envtest's
// auto-created "default". "c-test" is required too: management.cattle.io
// kinds namespaced by cluster id (project, clusterroletemplatebinding) need
// the namespace object to exist before a namespaced create succeeds.
// "cattle-system" is seeded here (not per-script) because the logs and
// support-bundle target tables both hardcode it, and per-script creation
// races across scripts sharing this one apiserver. "cattle-global-data" is
// Task 7's cloud-credential namespace (seedCloudCredential's Secret lives
// there) — seeded here for the same reason.
func seedNamespaces() {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, name := range []string{"fleet-default", "team-a", "c-test", "cattle-system", "cattle-global-data"} {
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
// cattle-global-data — global and one-time (Task 7, review R2-2):
// `create cluster`'s node-driver e2e scenarios resolve --cloud-credential
// by display name, and a per-script Setup creating these same fixtures
// would race AlreadyExists across every script sharing this one envtest
// apiserver. cc-e2etest ("e2e-cred") carries the amazonec2 driver key;
// cc-e2edo ("e2e-do-cred") the digitalocean one, so the machine-config
// two-step flow pairs a DO config with a DO credential. The data
// key/value mirror cloudcred.go's driver-detection contract
// (<driver>credentialConfig-<key>); "ZHVtbXk=" is the base64 encoding of
// "dummy" — ResolveCloudCredential never reads a data VALUE (spec
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
// context field — fetchGlobalRoles lists by name only.
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
	}}
	if _, err := dynCli.Resource(grGVR).Create(context.Background(), grObj, metav1.CreateOptions{}); err != nil { // survives the test body: runs from TestMain, no *testing.T in scope
		panic("seeding globalrole e2e-global-role: " + err.Error())
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
// enrichment source, not an operator-facing resource — `create resource -f`
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
// matches the harness credential id ("e2e" — the id portion of
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
