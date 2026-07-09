//go:build e2e

package e2e

import (
	"context"
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
	"strings"
	"testing"
	"time"

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
	binPath string
	shimURL string
	// shimCACertPEM is the shim's TLS certificate, PEM-encoded: client-go's
	// exec-credential plugin (kubectl, `rancher auth get-token`) refuses to
	// wire in exec auth over a plaintext http:// server (the same
	// credential-hygiene rule that keeps bearer tokens off http), so the shim
	// must be TLS and scripts must be able to trust it. Scripts get this as
	// INLINE PEM, not a file path: config.Effective's CA and kube.RESTConfig
	// consume eff.CA as raw PEM bytes directly (only auth get-token's and
	// kubeconfig's own --cacert handling resolve a path via ResolveCA), so a
	// path here would break every other command.
	shimCACertPEM string
	shimClient    *http.Client // trusts the shim's cert; use for Go-side HTTP calls to shimURL
	dynCli        dynamic.Interface
)

// testToken must satisfy BOTH the shim bearer check AND auth login's
// id:secret format (the auth-login scenario stores and replays it).
const testToken = "e2e:secret"

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "rancher-e2e-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}
	binPath = filepath.Join(tmp, "rancher")

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
		return nil
	}
	startEnv := func() error {
		var err error
		if cfg, err = env.Start(); err != nil {
			return fmt.Errorf("envtest (run via `make e2e`, which sets KUBEBUILDER_ASSETS; otherwise run "+
				"`go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path` and export it): %w", err)
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
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport: transport,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
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
	mux.HandleFunc("PUT /_test/status/", handleStatusLever)
	srv := httptest.NewTLSServer(mux)
	shimURL = srv.URL
	shimClient = srv.Client()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	shimCACertPEM = string(caPEM)

	dynCli, err = dynamic.NewForConfig(cfg)
	if err != nil {
		panic("building dynamic client: " + err.Error())
	}
	seedManagementCluster()
	seedNamespaces()

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

	err := buildAndStartEnv(
		func() error {
			time.Sleep(20 * time.Millisecond) // finishes after startEnv below
			return buildErr
		},
		func() error {
			return nil // envtest starts successfully, faster than the failing build
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

func TestScenarios(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "scripts",
		Setup: func(e *testscript.Env) error {
			e.Setenv("PATH", filepath.Dir(binPath)+string(os.PathListSeparator)+e.Getenv("PATH"))
			e.Setenv("TEST_RANCHER_URL", shimURL)
			e.Setenv("TEST_RANCHER_TOKEN", testToken)
			e.Setenv("TEST_RANCHER_CACERT", shimCACertPEM)
			// Global CA trust for every script (env-only and config-flow alike):
			// RANCHER_CACERT is env-effective everywhere Effective() is consulted.
			// It does NOT reach `rancher kubeconfig`'s embedded CA, which reads
			// only the config FILE's server entry — config-flow scripts pass
			// --cacert on login for that.
			e.Setenv("RANCHER_CACERT", shimCACertPEM)
			e.Setenv("RANCHER_CONFIG_DIR", filepath.Join(e.WorkDir, "rancher-config"))
			// E2E-A-003: credstore-pin.txtar clears this itself as its first
			// line (`env RANCHER_CREDENTIAL_STORE=`) — testscript.Env carries
			// no script name, so the exemption lives in the script.
			e.Setenv("RANCHER_CREDENTIAL_STORE", "file")
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
			"flipstatus":    cmdFlipStatus,
			"seedexeccache": cmdSeedExecCache,
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
	if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		panic("seeding management cluster c-test: " + err.Error())
	}
}

// seedNamespaces creates the namespaces scripts rely on beyond envtest's
// auto-created "default". "c-test" is required too: management.cattle.io
// kinds namespaced by cluster id (project, clusterroletemplatebinding) need
// the namespace object to exist before a namespaced create succeeds.
func seedNamespaces() {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, name := range []string{"fleet-default", "team-a", "c-test"} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": name},
		}}
		if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			panic("seeding namespace " + name + ": " + err.Error())
		}
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
