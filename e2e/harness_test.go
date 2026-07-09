//go:build e2e

package e2e

import (
	"context"
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

	"github.com/rogpeppe/go-internal/testscript"
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
	dynCli  dynamic.Interface
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
	// cwd during `go test` is this package directory (e2e/); the module root
	// with the main package is one level up.
	if out, err := exec.Command("go", "build", "-o", binPath, "..").CombinedOutput(); err != nil {
		panic("building rancher: " + string(out))
	}

	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("testdata", "crds")}}
	cfg, err := env.Start()
	if err != nil {
		panic("envtest (run via `make e2e`, which sets KUBEBUILDER_ASSETS; otherwise run " +
			"`go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path` and export it): " + err.Error())
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
	srv := httptest.NewServer(mux)
	shimURL = srv.URL

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

func TestScenarios(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "scripts",
		Setup: func(e *testscript.Env) error {
			e.Setenv("PATH", filepath.Dir(binPath)+string(os.PathListSeparator)+e.Getenv("PATH"))
			e.Setenv("TEST_RANCHER_URL", shimURL)
			e.Setenv("TEST_RANCHER_TOKEN", testToken)
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
			"jsonquery":  cmdJSONQuery,
			"flipstatus": cmdFlipStatus,
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
// auto-created "default".
func seedNamespaces() {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, name := range []string{"fleet-default", "team-a"} {
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
