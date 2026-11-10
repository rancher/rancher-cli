package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// missingNamespace is the namespace every row names; no server object
// exists in it.
const missingNamespace = "missing"

// missingClass says what a command does with a namespace that does not
// exist.
type missingClass string

const (
	// classChecks: the command ends in an error, because it reads a named
	// object (a storage 404), creates one (refused with 404 by the
	// namespace lifecycle admission), or reads the namespace itself.
	classChecks missingClass = "checks"
	// classKubectl: the command lists, and a missing namespace is an empty
	// list, as kubectl prints it.
	classKubectl missingClass = "kubectl"
	// classRefused: the command refuses before any request, whatever the
	// namespace.
	classRefused missingClass = "refused"
)

// missingRow is one command line run against a missing namespace. code is
// the error code a checks or refused row must end in (empty for a kubectl
// row); last is the request the command's last request must be (none for a
// refused row); reason says why a kubectl row keeps kubectl's answer.
type missingRow struct {
	name   string
	class  missingClass
	argv   []string
	code   string
	last   string
	reason string
}

const (
	reasonBuiltIn    = "a list of an autoscaling, core, networking or policy kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonWorkload   = "a list of a core, apps, batch or networking kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonFleet      = "a list of a Fleet kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonManagement = "a list of a Rancher management kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonCAPI       = "a list of a Cluster API kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonMachine    = "a list of a machine config kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonK3k        = "a list of a k3k cluster in a missing namespace is empty, as kubectl get -n prints it"
	reasonCatalog    = "a list of a catalog kind in a missing namespace is empty, as kubectl get -n prints it"
	reasonETCDSnap   = "a list of an etcd snapshot in a missing namespace is empty, as kubectl get -n prints it"
	reasonCRT        = "the root API list of registration tokens is filtered by namespace and a namespace with none is an empty list"
)

// missingKind is one generated kind with a namespace flag: where its
// objects live, the verbs the kind has (g get, p patch, d delete) and why
// its list keeps kubectl's answer. A kind with a cluster flag lives in the
// downstream cluster c-abc; the others are read through the local cluster.
type missingKind struct {
	kind     string
	cluster  bool
	api      string
	resource string
	verbs    string
	reason   string
}

var missingKinds = []missingKind{
	{"pod", true, "api/v1", "pods", "gpd", reasonWorkload},
	{"deployment", true, "apis/apps/v1", "deployments", "gpd", reasonWorkload},
	{"service", true, "api/v1", "services", "gpd", reasonWorkload},
	{"configmap", true, "api/v1", "configmaps", "gpd", reasonWorkload},
	{"secret", true, "api/v1", "secrets", "gpd", reasonWorkload},
	{"statefulset", true, "apis/apps/v1", "statefulsets", "gpd", reasonWorkload},
	{"daemonset", true, "apis/apps/v1", "daemonsets", "gpd", reasonWorkload},
	{"replicaset", true, "apis/apps/v1", "replicasets", "gpd", reasonWorkload},
	{"job", true, "apis/batch/v1", "jobs", "gpd", reasonWorkload},
	{"cronjob", true, "apis/batch/v1", "cronjobs", "gpd", reasonWorkload},
	{"ingress", true, "apis/networking.k8s.io/v1", "ingresses", "gpd", reasonWorkload},
	{"persistentvolumeclaim", true, "api/v1", "persistentvolumeclaims", "gpd", reasonWorkload},
	{"serviceaccount", true, "api/v1", "serviceaccounts", "gpd", reasonWorkload},
	{"event", true, "api/v1", "events", "gp", reasonWorkload},
	{"horizontalpodautoscaler", true, "apis/autoscaling/v2", "horizontalpodautoscalers", "gpd", reasonBuiltIn},
	{"limitrange", true, "api/v1", "limitranges", "gpd", reasonBuiltIn},
	{"networkpolicy", true, "apis/networking.k8s.io/v1", "networkpolicies", "gpd", reasonBuiltIn},
	{"poddisruptionbudget", true, "apis/policy/v1", "poddisruptionbudgets", "gpd", reasonBuiltIn},
	{"resourcequota", true, "api/v1", "resourcequotas", "gpd", reasonBuiltIn},
	{"k3k-cluster", true, "apis/k3k.io/v1beta1", "clusters", "gpd", reasonK3k},
	{"app", true, "apis/catalog.cattle.io/v1", "apps", "g", reasonCatalog},
	{"operation", true, "apis/catalog.cattle.io/v1", "operations", "g", reasonCatalog},
	{"gitrepo", false, "apis/fleet.cattle.io/v1alpha1", "gitrepos", "gpd", reasonFleet},
	{"helmop", false, "apis/fleet.cattle.io/v1alpha1", "helmops", "gpd", reasonFleet},
	{"bundle", false, "apis/fleet.cattle.io/v1alpha1", "bundles", "gp", reasonFleet},
	{"bundledeployment", false, "apis/fleet.cattle.io/v1alpha1", "bundledeployments", "gp", reasonFleet},
	{"clustergroup", false, "apis/fleet.cattle.io/v1alpha1", "clustergroups", "gp", reasonFleet},
	{"fleet-cluster", false, "apis/fleet.cattle.io/v1alpha1", "clusters", "gp", reasonFleet},
	{"projectroletemplatebinding", false, "apis/management.cattle.io/v3", "projectroletemplatebindings", "gpd", reasonManagement},
	{"provisioning-cluster", false, "apis/provisioning.cattle.io/v1", "clusters", "gpd", reasonManagement},
	{"etcd-snapshot", false, "apis/rke.cattle.io/v1", "etcdsnapshots", "g", reasonETCDSnap},
	{"capi-cluster", false, "apis/cluster.x-k8s.io/v1", "clusters", "gp", reasonCAPI},
	{"machine", false, "apis/cluster.x-k8s.io/v1", "machines", "gp", reasonCAPI},
	{"machineset", false, "apis/cluster.x-k8s.io/v1", "machinesets", "gp", reasonCAPI},
	{"machinedeployment", false, "apis/cluster.x-k8s.io/v1", "machinedeployments", "gp", reasonCAPI},
	{"amazonec2config", false, "apis/rke-machine-config.cattle.io/v1", "amazonec2configs", "gpd", reasonMachine},
	{"azureconfig", false, "apis/rke-machine-config.cattle.io/v1", "azureconfigs", "gpd", reasonMachine},
	{"digitaloceanconfig", false, "apis/rke-machine-config.cattle.io/v1", "digitaloceanconfigs", "gpd", reasonMachine},
	{"harvesterconfig", false, "apis/rke-machine-config.cattle.io/v1", "harvesterconfigs", "gpd", reasonMachine},
	{"vmwarevsphereconfig", false, "apis/rke-machine-config.cattle.io/v1", "vmwarevsphereconfigs", "gpd", reasonMachine},
}

const missingPatchBody = `[{"op":"add","path":"/metadata/labels","value":{"a":"b"}}]`

// rows returns the rows of one generated kind: the named get and the
// patch and delete that read the named object first are checks rows, the
// unnamed get is a kubectl row.
func (k missingKind) rows() []missingRow {
	cluster := "/k8s/clusters/local"
	var flags []string
	if k.cluster {
		cluster = "/k8s/clusters/c-abc"
		flags = []string{"--cluster", "c-abc"}
	}
	flags = append(flags, "--namespace", missingNamespace, "-o", "json")
	collection := cluster + "/" + k.api + "/namespaces/" + missingNamespace + "/" + k.resource
	var rows []missingRow
	if strings.Contains(k.verbs, "g") {
		rows = append(rows,
			missingRow{name: "get " + k.kind + "/list", class: classKubectl, argv: slices.Concat([]string{"get", k.kind}, flags),
				last: "GET " + collection, reason: k.reason},
			missingRow{name: "get " + k.kind + "/named", class: classChecks, argv: slices.Concat([]string{"get", k.kind, "x"}, flags),
				code: clierr.CodeNotFound, last: "GET " + collection + "/x"})
	}
	if strings.Contains(k.verbs, "p") {
		rows = append(rows, missingRow{name: "patch " + k.kind, class: classChecks,
			argv: slices.Concat([]string{"patch", k.kind, "x", "--patch", missingPatchBody, "--dry-run"}, flags),
			code: clierr.CodeNotFound, last: "GET " + collection + "/x"})
	}
	if strings.Contains(k.verbs, "d") {
		rows = append(rows, missingRow{name: "delete " + k.kind, class: classChecks,
			argv: slices.Concat([]string{"delete", k.kind, "x", "--dry-run"}, flags),
			code: clierr.CodeNotFound, last: "GET " + collection + "/x"})
	}
	return rows
}

const (
	crtCollection = "/v1/management.cattle.io.clusterregistrationtokens"
	namespaceGET  = "GET /k8s/clusters/c-abc/api/v1/namespaces/" + missingNamespace
	// repoPath is where the index and the chart info of the ClusterRepo r
	// are read, through the Rancher API of the downstream cluster.
	repoPath = "/k8s/clusters/c-abc/v1/catalog.cattle.io.clusterrepos/r"
)

// missingRows is every row: the generated kinds, the registration token
// kind, whose objects are read through the Rancher root API, and the
// hand-written commands.
func missingRows() []missingRow {
	var rows []missingRow
	for _, k := range missingKinds {
		rows = append(rows, k.rows()...)
	}
	rows = append(rows,
		missingRow{name: "get clusterregistrationtoken/list", class: classKubectl,
			argv: []string{"get", "clusterregistrationtoken", "--namespace", missingNamespace, "-o", "json"},
			last: "GET " + crtCollection, reason: reasonCRT},
		missingRow{name: "get clusterregistrationtoken/named", class: classChecks,
			argv: []string{"get", "clusterregistrationtoken", "x", "--namespace", missingNamespace, "-o", "json"},
			code: clierr.CodeNotFound, last: "GET " + crtCollection + "/" + missingNamespace + "/x"},
		missingRow{name: "patch app", class: classRefused,
			argv: []string{"patch", "app", "x", "--patch", missingPatchBody, "--dry-run", "--cluster", "c-abc", "--namespace", missingNamespace, "-o", "json"},
			code: clierr.CodeValidation},
		missingRow{name: "patch operation", class: classRefused,
			argv: []string{"patch", "operation", "x", "--patch", missingPatchBody, "--dry-run", "--cluster", "c-abc", "--namespace", missingNamespace, "-o", "json"},
			code: clierr.CodeValidation},
		missingRow{name: "patch etcd-snapshot", class: classRefused,
			argv: []string{"patch", "etcd-snapshot", "x", "--patch", missingPatchBody, "--dry-run", "--namespace", missingNamespace, "-o", "json"},
			code: clierr.CodeValidation},
		missingRow{name: "patch clusterregistrationtoken", class: classRefused,
			argv: []string{"patch", "clusterregistrationtoken", "x", "--patch", missingPatchBody, "--dry-run", "--namespace", missingNamespace, "-o", "json"},
			code: clierr.CodeValidation},
	)
	const downstream = "/k8s/clusters/c-abc"
	const provisioning = "GET /k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/" + missingNamespace + "/clusters/prod"
	ns := []string{"--namespace", missingNamespace, "-o", "json"}
	cluster := slices.Concat([]string{"--cluster", "c-abc"}, ns)
	preview := slices.Concat([]string{"--dry-run"}, ns)
	return append(rows,
		missingRow{name: "create secret", class: classChecks, argv: slices.Concat([]string{"create", "secret", "s", "--from-literal", "a=b", "--dry-run", "--cluster", "c-abc"}, ns),
			code: clierr.CodeNotFound, last: "POST " + downstream + "/api/v1/namespaces/" + missingNamespace + "/secrets"},
		missingRow{name: "create resource", class: classChecks, argv: slices.Concat([]string{"create", "resource", "-f", "{manifest}", "--dry-run", "--cluster", "c-abc"}, ns),
			code: clierr.CodeNotFound, last: "POST " + downstream + "/api/v1/namespaces/" + missingNamespace + "/configmaps"},
		missingRow{name: "cluster scale", class: classChecks, argv: slices.Concat([]string{"cluster", "scale", "prod", "--pool", "p", "--nodes", "2"}, preview),
			code: clierr.CodeNotFound, last: provisioning},
		missingRow{name: "cluster set-autoscaling", class: classChecks, argv: slices.Concat([]string{"cluster", "set-autoscaling", "prod", "--pool", "p", "--min", "1", "--max", "3"}, preview),
			code: clierr.CodeNotFound, last: provisioning},
		missingRow{name: "cluster clear-autoscaling", class: classChecks, argv: slices.Concat([]string{"cluster", "clear-autoscaling", "prod", "--pool", "p"}, preview),
			code: clierr.CodeNotFound, last: provisioning},
		missingRow{name: "cluster pause-autoscaling", class: classChecks, argv: slices.Concat([]string{"cluster", "pause-autoscaling", "prod"}, preview),
			code: clierr.CodeNotFound, last: provisioning},
		missingRow{name: "cluster resume-autoscaling", class: classChecks, argv: slices.Concat([]string{"cluster", "resume-autoscaling", "prod"}, preview),
			code: clierr.CodeNotFound, last: provisioning},
		missingRow{name: "inspect pod", class: classChecks, argv: slices.Concat([]string{"inspect", "pod", "x"}, cluster),
			code: clierr.CodeNotFound, last: "GET " + downstream + "/api/v1/namespaces/" + missingNamespace + "/pods/x"},
		missingRow{name: "inspect deployment", class: classChecks, argv: slices.Concat([]string{"inspect", "deployment", "x"}, cluster),
			code: clierr.CodeNotFound, last: "GET " + downstream + "/apis/apps/v1/namespaces/" + missingNamespace + "/deployments/x"},
		missingRow{name: "access can", class: classChecks, argv: slices.Concat([]string{"access", "can", "get", "pods"}, cluster),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "access explain", class: classChecks, argv: slices.Concat([]string{"access", "explain", "get", "pods"}, cluster),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "access list", class: classChecks, argv: slices.Concat([]string{"access", "list"}, cluster),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "access who-can", class: classChecks, argv: slices.Concat([]string{"access", "who-can", "get", "pods"}, cluster),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "usage", class: classChecks, argv: slices.Concat([]string{"usage"}, cluster),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "app install", class: classChecks, argv: slices.Concat([]string{"app", "install", "web", "--repo", "r", "--chart", "c"}, cluster, []string{"--dry-run"}),
			code: clierr.CodeNotFound, last: namespaceGET},
		missingRow{name: "app uninstall", class: classChecks, argv: slices.Concat([]string{"app", "uninstall", "web"}, cluster, []string{"--dry-run"}),
			code: clierr.CodeNotFound, last: "GET " + downstream + "/apis/catalog.cattle.io/v1/namespaces/" + missingNamespace + "/apps/web"},
	)
}

// missingServer is the facade's state for the guard: every request it
// received.
type missingServer struct {
	mu   sync.Mutex
	reqs []string
}

func (s *missingServer) record(r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
}

func (s *missingServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

// missingFallthrough answers what the facade does not serve. A request
// under the missing namespace is answered as Kubernetes answers it: a
// create and a named read are a Status 404, a list is empty, and the
// namespace itself is a Status 404. A registration token read through the
// root API answers the same way, by path. The index and the chart info of
// the ClusterRepo r are served, so app install reaches its namespace read. The API group list names the
// groups whose kinds leave their version to discovery. Every other request
// is answered as the caller-scope tests answer it: the version, discovery,
// access reviews and the objects of the kinds table.
func missingFallthrough(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(what string) {
		write(http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
			"status": "Failure", "message": what + " not found", "reason": "NotFound", "code": 404})
	}
	if p == crtCollection && r.Method == http.MethodGet {
		write(http.StatusOK, map[string]any{"type": "collection", "revision": "1", "count": 0, "data": []any{}})
		return
	}
	if rest, ok := strings.CutPrefix(p, crtCollection+"/"); ok {
		notFound("clusterregistrationtokens " + rest)
		return
	}
	if p == repoPath && r.Method == http.MethodGet {
		switch r.URL.Query().Get("link") {
		case "index":
			write(http.StatusOK, map[string]any{"entries": map[string]any{"c": []any{map[string]any{"version": "1.0.0", "digest": "d"}}}})
			return
		case "info":
			write(http.StatusOK, map[string]any{"values": map[string]any{}, "chart": map[string]any{"annotations": map[string]any{}}})
			return
		}
	}
	if r.Method == http.MethodGet && strings.HasSuffix(p, "/apis") {
		group := func(name, version string) map[string]any {
			gv := map[string]any{"groupVersion": name + "/" + version, "version": version}
			return map[string]any{"name": name, "versions": []any{gv}, "preferredVersion": gv}
		}
		write(http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1",
			"groups": []any{group("ext.cattle.io", "v1"), group("cluster.x-k8s.io", "v1")}})
		return
	}
	_, after, found := strings.Cut(p, "/namespaces/"+missingNamespace)
	if !found || (after != "" && !strings.HasPrefix(after, "/")) {
		callerFallthrough(w, r)
		return
	}
	segs := strings.Split(strings.Trim(after, "/"), "/")
	switch {
	case after == "":
		notFound("namespaces " + missingNamespace)
	case r.Method == http.MethodGet && len(segs) == 1:
		write(http.StatusOK, map[string]any{"kind": "List", "apiVersion": "v1", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	default:
		notFound(segs[0] + " " + segs[len(segs)-1])
	}
}

// missingHarness runs commands through a fresh root against a facade that
// records every request.
type missingHarness struct {
	srv      *missingServer
	manifest string
}

func newMissingHarness(t *testing.T) *missingHarness {
	t.Helper()
	keyring.MockInit()
	s := &missingServer{}
	clusters := []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}}
	f := testrancher.New(testrancher.Options{
		SeedToken:         callerBearer,
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) { return clusters, b == callerBearer },
		ProjectVisibility: func(context.Context, string, string) ([]testrancher.ProjectFixture, bool, error) {
			return nil, true, nil
		},
		NamespaceVisibility: func(string, string) ([]testrancher.NamespaceFixture, bool) { return nil, true },
		RequestHook:         s.record,
		Fallthrough:         http.HandlerFunc(missingFallthrough),
	})
	hs := httptest.NewTLSServer(f)
	t.Cleanup(hs.Close)

	cfgDir, home := t.TempDir(), t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfigDir, cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, hs.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, hs)))
	t.Setenv(config.EnvToken, callerBearer)
	h := &missingHarness{srv: s, manifest: filepath.Join(t.TempDir(), "cm.yaml")}
	require.NoError(t, os.WriteFile(h.manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: guard\ndata:\n  a: b\n"), 0o600))
	t.Cleanup(func() { CaptureArgv(nil) })
	return h
}

// run runs argv through a fresh root and returns the error code the
// envelope carries (empty on success) and the requests the server saw.
func (h *missingHarness) run(t *testing.T, argv []string) (code string, requests []string) {
	t.Helper()
	argv = slices.Clone(argv)
	for i, a := range argv {
		if a == "{manifest}" {
			argv[i] = h.manifest
		}
	}
	h.srv.take()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	CaptureArgv(argv)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_ = root.Run(ctx, append([]string{"rancher"}, argv...))
	requests = h.srv.take()
	for _, stream := range []string{errOut.String(), out.String()} {
		i := strings.Index(stream, "{")
		if i < 0 {
			continue
		}
		var env struct {
			Data struct {
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(stream[i:]), &env) == nil && env.Data.Error != nil {
			return env.Data.Error.Code, requests
		}
	}
	return "", requests
}

// TestEveryNamespaceCommandHasAMissingNamespaceRow asserts that every
// command that declares --namespace has a row naming what it does with a
// namespace that does not exist: a command that reads a named object, a
// create, a namespace read ends in an error (checks), a list is empty as
// kubectl prints it (kubectl), and a command that refuses before any
// request refuses whatever the namespace (refused). Each row asserts the
// error code and the last request, so a row cannot pass on a discovery
// failure. A new command with --namespace and no row fails the test: add
// its row to missingRows. Not parallel: it sets the process environment
// and the captured command line.
func TestEveryNamespaceCommandHasAMissingNamespaceRow(t *testing.T) {
	rows := missingRows()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	declaring := map[string]bool{}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "namespace" }) {
			declaring[d.Name] = true
		}
	}
	covered := map[string]bool{}
	for _, row := range rows {
		command, _, _ := strings.Cut(row.name, "/")
		covered[command] = true
		assert.True(t, declaring[command], "row %q names a command that does not declare --namespace", row.name)
	}
	var uncovered []string
	for _, name := range slices.Sorted(maps.Keys(declaring)) {
		if covered[name] {
			continue
		}
		uncovered = append(uncovered, name)
		table := "missingRows"
		if verb, _, _ := strings.Cut(name, " "); slices.Contains([]string{"get", "patch", "delete"}, verb) {
			table = "missingKinds"
		}
		assert.Fail(t, name+" declares --namespace and has no row",
			"add its row to "+table+" in this file, saying what it does with a namespace that does not exist")
	}
	commands, rowCount := len(declaring), len(rows)
	assert.Equal(t, 126, commands, "the commands that declare --namespace; uncovered: %v", uncovered)
	assert.Equal(t, 167, rowCount, "the rows; uncovered commands: %v", uncovered)

	h := newMissingHarness(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			code, requests := h.run(t, row.argv)
			switch row.class {
			case classRefused:
				assert.Equal(t, row.code, code)
				assert.Empty(t, requests, "a refusal makes no request")
				return
			case classChecks:
				assert.Equal(t, row.code, code, "requests: %v", requests)
			case classKubectl:
				assert.Empty(t, code, "a list in a missing namespace succeeds; requests: %v", requests)
				assert.NotEmpty(t, row.reason, "a kubectl row says why")
			}
			if assert.NotEmpty(t, requests) {
				assert.Equal(t, row.last, requests[len(requests)-1], "the last request, of %v", requests)
			}
		})
	}
}
