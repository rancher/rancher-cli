package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// contractServer is a Rancher fake for the --project contract: two
// clusters, c-abc and c-def, each with a project whose display name is
// "web" (p-xyz on c-abc, p-def on c-def), so a project read on the wrong
// cluster is visible. It serves the steve project and cluster collections
// the lookup reads, the management reads the commands make on the way, and
// records every request.
type contractServer struct {
	mu   sync.Mutex
	reqs []string
}

func (s *contractServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

func (s *contractServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	line := r.Method + " " + p
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, line)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	mgmtCluster := func(id, display string) map[string]any {
		return map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
			"metadata": map[string]any{"name": id, "resourceVersion": "1"},
			"spec":     map[string]any{"displayName": display}, "status": map[string]any{"conditions": []any{}}}
	}
	steveProject := func(cluster, id string) map[string]any {
		return map[string]any{"id": cluster + "/" + id, "type": "management.cattle.io.project",
			"metadata": map[string]any{"name": id}, "spec": map[string]any{"clusterName": cluster, "displayName": "web"}}
	}
	collection := func(items ...any) map[string]any {
		return map[string]any{"type": "collection", "revision": "1", "count": len(items), "data": items}
	}
	switch {
	case r.Method == http.MethodGet && p == "/v1/management.cattle.io.projects":
		write(collection(steveProject("c-abc", "p-xyz"), steveProject("c-def", "p-def")))
	case r.Method == http.MethodGet && p == "/v1/management.cattle.io.projects/c-abc":
		write(collection(steveProject("c-abc", "p-xyz")))
	case r.Method == http.MethodGet && p == "/v1/management.cattle.io.projects/c-def":
		write(collection(steveProject("c-def", "p-def")))
	case r.Method == http.MethodGet && p == "/v1/management.cattle.io.clusters":
		write(collection(
			map[string]any{"id": "c-abc", "metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
			map[string]any{"id": "c-def", "metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "edge"}}))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis"):
		gv := map[string]any{"groupVersion": "ext.cattle.io/v1", "version": "v1"}
		write(map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "ext.cattle.io", "versions": []any{gv}, "preferredVersion": gv}}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/api/v1"):
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod","verbs":["get","list","create","delete"]},{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","list"]}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/api"):
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`))
	case r.Method == http.MethodPost && strings.Contains(p, "/authorization.k8s.io/v1/") && strings.HasSuffix(p, "reviews"):
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		if obj == nil {
			obj = map[string]any{}
		}
		obj["status"] = map[string]any{"allowed": true, "incomplete": false, "resourceRules": []any{}, "nonResourceRules": []any{}}
		w.WriteHeader(http.StatusCreated)
		write(obj)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/authentication.k8s.io/v1/selfsubjectreviews"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfoStatus":{},"userInfo":{"username":"u-contract","groups":["system:authenticated"]}}}`))
	case r.Method == http.MethodPost && r.URL.Query().Get("dryRun") != "":
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		if obj == nil {
			obj = map[string]any{}
		}
		md, _ := obj["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		if md["name"] == nil || md["name"] == "" {
			md["name"] = "x"
		}
		md["resourceVersion"] = "1"
		obj["metadata"] = md
		w.WriteHeader(http.StatusCreated)
		write(obj)
	case r.Method == http.MethodGet && p == "/rancherversion":
		_, _ = w.Write([]byte(`{"Version":"v2.13.0","GitCommit":"x","RancherPrime":"false"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis/management.cattle.io/v3/clusters"):
		write(map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "ClusterList", "metadata": map[string]any{"resourceVersion": "1"},
			"items": []any{mgmtCluster("c-abc", "prod"), mgmtCluster("c-def", "edge")}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis/management.cattle.io/v3/clusters/c-abc"):
		write(mgmtCluster("c-abc", "prod"))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis/management.cattle.io/v3/namespaces/c-abc/projects/p-xyz"):
		write(map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Project",
			"metadata": map[string]any{"name": "p-xyz", "namespace": "c-abc", "resourceVersion": "1"},
			"spec":     map[string]any{"displayName": "web", "clusterName": "c-abc"}})
	case r.Method == http.MethodGet && p == "/k8s/clusters/c-abc/api/v1/namespaces":
		ns := map[string]any{"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": "default", "resourceVersion": "1",
				"labels":      map[string]any{"field.cattle.io/projectId": "p-xyz"},
				"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-xyz"}}}
		write(map[string]any{"apiVersion": "v1", "kind": "NamespaceList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{ns}})
	case r.Method == http.MethodGet && p == "/k8s/clusters/c-abc/api/v1/pods":
		write(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	default:
		if obj, ok := kubeAnswer(r); ok {
			write(obj)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","details":{},"code":404}`))
	}
}

// kubeAnswer answers a Kubernetes request for any kind of the kinds table
// through a cluster's proxy: a list is empty, a named object exists (a
// namespace carries the project label of p-xyz), and a dry-run write
// echoes the object.
func kubeAnswer(r *http.Request) (map[string]any, bool) {
	_, rest, ok := strings.Cut(r.URL.Path, "/k8s/clusters/")
	if !ok {
		return nil, false
	}
	segs := strings.Split(rest, "/")[1:]
	var group, version string
	switch {
	case len(segs) >= 2 && segs[0] == "api":
		version, segs = segs[1], segs[2:]
	case len(segs) >= 3 && segs[0] == "apis":
		group, version, segs = segs[1], segs[2], segs[3:]
	default:
		return nil, false
	}
	ns, name := "", ""
	switch {
	case len(segs) >= 3 && segs[0] == "namespaces":
		ns, segs = segs[1], segs[2:]
	case len(segs) == 0:
		return nil, false
	}
	resource := segs[0]
	if len(segs) > 1 {
		name = segs[1]
	}
	if group == "rbac.authorization.k8s.io" && name == "" && r.Method == http.MethodGet {
		return map[string]any{"apiVersion": group + "/" + version, "kind": "List", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}}, true
	}
	k, found := kinds.ByGVR(schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
	if !found || resource == "projects" {
		return nil, false
	}
	apiVersion := version
	if group != "" {
		apiVersion = group + "/" + version
	}
	if name == "" && r.Method == http.MethodGet {
		items := []any{}
		if resource == "projectroletemplatebindings" && ns == "p-xyz" {
			items = append(items, map[string]any{"apiVersion": apiVersion, "kind": k.K8sKind,
				"metadata":    map[string]any{"name": "prtb-1", "namespace": ns, "resourceVersion": "1"},
				"projectName": "c-abc:p-xyz", "roleTemplateName": "project-member",
				"userName": "x", "userPrincipalName": "local://x"})
		}
		return map[string]any{"apiVersion": apiVersion, "kind": k.K8sKind + "List", "metadata": map[string]any{"resourceVersion": "1"}, "items": items}, true
	}
	if name == "" {
		return nil, false
	}
	md := map[string]any{"name": name, "resourceVersion": "1", "uid": "u-1"}
	if ns != "" {
		md["namespace"] = ns
	}
	obj := map[string]any{"apiVersion": apiVersion, "kind": k.K8sKind, "metadata": md}
	switch resource {
	case "namespaces":
		md["labels"] = map[string]any{"field.cattle.io/projectId": "p-xyz"}
		md["annotations"] = map[string]any{"field.cattle.io/projectId": "c-abc:p-xyz"}
	case "roletemplates":
		obj["context"] = "project"
	case "users":
		obj["principalIds"] = []any{"local://" + name}
	}
	return obj, true
}

var validationExit = clierr.ExitCode(clierr.CodeValidation)

const (
	contractProjectPath = "/v1/management.cattle.io.projects/c-abc"
	contractLocalList   = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/projects"
)

// bespokeProjectCommands is the reviewed inventory of the hand-written
// commands that take --project; every other command that takes it is a
// generated get, patch or delete of a project scopable kind.
var bespokeProjectCommands = []string{
	"create resource", "create namespace", "create secret",
	"project add-namespace", "grant", "revoke", "usage",
	"access can", "access list", "access who-can",
}

// contractPositional and contractOptionalPositional fill the positionals a
// declaring command needs to get past its own input checks to its project
// read.
var contractPositional = map[string]string{"access can": "get", "access who-can": "get", "project add-namespace": "default",
	"get namespace": "default", "delete namespace": "default", "patch namespace": "default"}

var contractOptionalPositional = map[string]string{"access can": "pods", "access who-can": "pods"}

type contractRun struct {
	stdout, stderr string
	exit           int
	requests       []string
	reads          []string
	err            *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Hint       string `json:"hint"`
		Candidates []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"candidates"`
	}
}

// contractHarness runs commands through a fresh root against one fake. A
// root keeps a flag's set state from an earlier run, so every run builds
// its own; the config directory, the credential and the fake are shared.
type contractHarness struct {
	srv      *contractServer
	base     string
	manifest string
	cfgDir   string
	mu       sync.Mutex
	reads    []string
}

func newContractHarness(t *testing.T) *contractHarness {
	t.Helper()
	keyring.MockInit()
	h := &contractHarness{srv: &contractServer{}}
	hs := httptest.NewTLSServer(h.srv)
	t.Cleanup(hs.Close)
	h.base = hs.URL
	cfgDir, home := t.TempDir(), t.TempDir()
	h.cfgDir = cfgDir
	// A command that writes a file where it runs (support-bundle) writes
	// it here, never into the package directory.
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfigDir, cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, h.base)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, hs)))
	t.Setenv(config.EnvToken, "token-abc:secret")
	seed := &config.Config{CurrentServer: "x", Servers: map[string]config.Server{"x": {URL: h.base}}}
	seed.SetPath(config.FilePath(cfgDir))
	require.NoError(t, seed.Write())
	h.manifest = filepath.Join(t.TempDir(), "cm.yaml")
	require.NoError(t, os.WriteFile(h.manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: contract\n  namespace: default\ndata:\n  a: b\n"), 0o600))
	t.Cleanup(runtime.SetProjectReadObserverForTest(func(clusterID string) {
		h.mu.Lock()
		h.reads = append(h.reads, clusterID)
		h.mu.Unlock()
	}))
	t.Cleanup(func() { CaptureArgv(nil) })
	return h
}

func (h *contractHarness) run(t *testing.T, args ...string) contractRun {
	t.Helper()
	h.srv.take()
	h.mu.Lock()
	h.reads = nil
	h.mu.Unlock()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	CaptureArgv(args)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	runErr := root.Run(ctx, append([]string{"rancher"}, args...))
	r := contractRun{stdout: out.String(), stderr: errOut.String(), requests: h.srv.take()}
	if ec, ok := errors.AsType[ucli.ExitCoder](runErr); ok {
		r.exit = ec.ExitCode()
	} else if runErr != nil {
		r.exit = -1
	}
	h.mu.Lock()
	r.reads = slices.Clone(h.reads)
	h.mu.Unlock()
	var env struct {
		Data struct {
			Error json.RawMessage `json:"error"`
		} `json:"data"`
	}
	for _, stream := range []string{r.stderr, r.stdout} {
		if json.Unmarshal([]byte(strings.TrimSpace(stream)), &env) == nil && len(env.Data.Error) > 0 {
			require.NoError(t, json.Unmarshal(env.Data.Error, &r.err))
			break
		}
	}
	return r
}

// count is how many requests went to path, whatever their query.
func (r contractRun) count(path string) int {
	n := 0
	for _, line := range r.requests {
		_, rest, _ := strings.Cut(line, " ")
		if p, _, _ := strings.Cut(rest, "?"); p == path {
			n++
		}
	}
	return n
}

// contractArgs carries a declaring command past its own input checks to
// its project read: required positionals and flags, --cluster c-abc unless
// withCluster is false, --namespace default where the command takes one
// (not with create secret or an access question), a preview for a
// mutation, and JSON output.
func (h *contractHarness) contractArgs(d runtime.Descriptor, withCluster bool) []string {
	args := strings.Fields(d.Name)
	for i, a := range d.Args {
		switch {
		case i == 0 && contractPositional[d.Name] != "":
			args = append(args, contractPositional[d.Name])
		case a.Required:
			args = append(args, "x")
		case contractOptionalPositional[d.Name] != "":
			args = append(args, contractOptionalPositional[d.Name])
		}
	}
	for _, f := range d.Flags {
		switch {
		case f.Name == "cluster":
			if withCluster {
				args = append(args, "--cluster", "c-abc")
			}
		case f.Name == "namespace":
			// --namespace is another way to name what --project names for
			// create secret and the access questions; the rest need it.
			if d.Name != "create secret" && !strings.HasPrefix(d.Name, "access ") {
				args = append(args, "--namespace", "default")
			}
		case f.Name == runtime.FlagProject:
		case f.Name == "patch":
			args = append(args, "--patch", `[{"op":"add","path":"/metadata/labels","value":{"contract":"1"}}]`)
		case f.Name == "filename" && f.Required:
			args = append(args, "--filename", h.manifest)
		case f.Required && f.Type == "bool":
			args = append(args, "--"+f.Name)
		case f.Required:
			v := "x"
			if len(f.Enum) > 0 {
				v = f.Enum[0]
			}
			args = append(args, "--"+f.Name, v)
		}
	}
	if d.Name == "create secret" {
		args = append(args, "--from-literal", "a=b")
	}
	if d.CommandType == runtime.Mutating {
		args = append(args, "--dry-run")
	}
	args = append(args, "--no-input")
	if d.OutputPolicy.Allows("json") {
		args = append(args, "-o", "json")
	}
	return args
}

// before moves a trailing "--project <v>" in front of the command name.
func before(args []string, words int) []string {
	n := len(args)
	return slices.Concat(args[n-2:], args[:words], args[words:n-2])
}

// splitHint splits a hint the CLI printed into argv: words separated by
// spaces, single-quoted values (clierr.ShellQuote always quotes) kept
// whole, the leading program name dropped.
func splitHint(t *testing.T, hint string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	quoted, inWord := false, false
	for _, r := range hint {
		switch {
		case r == '\'':
			quoted, inWord = !quoted, true
		case r == ' ' && !quoted:
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	require.False(t, quoted, "unbalanced quote in %q", hint)
	if inWord {
		out = append(out, cur.String())
	}
	require.NotEmpty(t, out)
	require.Equal(t, "rancher", out[0], "a hint names the rancher command: %q", hint)
	return out[1:]
}

// TestProjectContract asserts, for every registered command, that a typed
// --project reaches exactly the commands that list it: every other command
// refuses it before any request, whether typed after or before the command
// name; every command that takes it reads the project once, on the cluster
// --cluster names, through the steve collection; without --cluster the
// refusal names the cluster the project is on; and every hint those
// refusals print runs through the real command tree without a VALIDATION
// error.
func TestProjectContract(t *testing.T) {
	h := newContractHarness(t)
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var hints []string
	declaring := 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		words := len(strings.Fields(d.Name))
		_, k, generated := strings.Cut(d.Name, " ")
		kind, isKind := kinds.Lookup(k)
		wantTakes := slices.Contains(bespokeProjectCommands, d.Name) ||
			(generated && isGeneratedVerb(d.Name) && isKind && kinds.ProjectScopable(kind))
		require.Equal(t, wantTakes, runtime.TakesProject(d), "%s: takes --project only when it is in the reviewed inventory or a generated verb of a project scopable kind", d.Name)

		t.Run(strings.ReplaceAll(d.Name, " ", "-"), func(t *testing.T) {
			if !runtime.TakesProject(d) {
				args := strings.Fields(d.Name)
				if d.Name == "project remove-namespace" {
					args = append(args, "x", "--cluster", "c-abc")
				}
				if d.OutputPolicy.Allows("json") {
					args = append(args, "-o", "json")
				}
				args = append(args, "--project", "web")
				for _, argv := range [][]string{args, before(args, words)} {
					r := h.run(t, argv...)
					assert.Empty(t, r.requests, "%v: refused before any request", argv)
					assert.Empty(t, r.reads, "%v", argv)
					if !d.OutputPolicy.Allows("json") {
						assert.Equal(t, validationExit, r.exit, "%v", argv)
						assert.Equal(t, "error (VALIDATION): --project does not apply to "+d.Name, strings.TrimSpace(r.stderr), "%v", argv)
						continue
					}
					assert.Equal(t, validationExit, r.exit, "%v", argv)
					require.NotNil(t, r.err, "%v: %s", argv, r.stderr)
					assert.Equal(t, "VALIDATION", r.err.Code, "%v", argv)
					assert.Equal(t, "--project does not apply to "+d.Name, r.err.Message, "%v", argv)
					if d.Name == "project remove-namespace" {
						assert.Equal(t, withDirectory(h.cfgDir, "rancher project remove-namespace 'x' --cluster 'c-abc' --dry-run"), r.err.Hint)
						hints = append(hints, r.err.Hint)
					} else {
						assert.Empty(t, r.err.Hint, "%v", argv)
					}
				}
				return
			}
			declaring++
			args := append(h.contractArgs(d, true), "--project", "web")
			if verb, _, _ := strings.Cut(d.Name, " "); verb == "patch" && kind.NoPatchReason != "" {
				// A refused patch has a --project flag and refuses before it reads
				// the project; TestEveryRefusedVerbIsATypedValidationError asserts
				// the whole message.
				for _, argv := range [][]string{args, before(args, words)} {
					r := h.run(t, argv...)
					assert.Empty(t, r.requests, "%v: refused before any request", argv)
					require.NotNil(t, r.err, "%v: %s", argv, r.stderr)
					assert.Equal(t, "VALIDATION", r.err.Code, "%v", argv)
				}
				return
			}
			for _, argv := range [][]string{args, before(args, words)} {
				r := h.run(t, argv...)
				assert.Equal(t, 1, r.count(contractProjectPath), "%v: one project collection read on the typed cluster: %v", argv, r.requests)
				assert.Zero(t, r.count(contractLocalList), "%v: no project LIST through local", argv)
				assert.Nil(t, r.err, "%v: the command runs to the end against the fake", argv)
				assert.Zero(t, r.exit, "%v", argv)
				require.NotEmpty(t, r.reads, "%v: the command read the project it takes", argv)
				for _, c := range r.reads {
					assert.Equal(t, "c-abc", c, "%v: the project is read on the cluster given by --cluster", argv)
				}
			}

			r := h.run(t, append(h.contractArgs(d, false), "--project", "p-xyz")...)
			require.NotNil(t, r.err, "%s without --cluster: %s %s", d.Name, r.stdout, r.stderr)
			assert.Equal(t, "VALIDATION", r.err.Code)
			assert.Equal(t, "--cluster is required with --project; the project with the id or display name 'p-xyz' is on cluster 'c-abc' - pass --cluster 'c-abc'", r.err.Message)
			require.Len(t, r.err.Candidates, 1)
			assert.Equal(t, "c-abc", r.err.Candidates[0].ID)
			assert.Equal(t, "prod", r.err.Candidates[0].Name)
			assert.Equal(t, withDirectory(h.cfgDir, "rancher access clusters"), r.err.Hint)
			assert.Equal(t, []string{"GET /v1/management.cattle.io.projects", "GET /v1/management.cattle.io.clusters"}, r.requests,
				"%s without --cluster: only the two steve collection reads", d.Name)
			hints = append(hints, r.err.Hint)
		})
	}
	assert.Equal(t, 61, declaring, "the commands that take --project")

	t.Run("usage --by project", func(t *testing.T) {
		r := h.run(t, "usage", "--cluster", "c-abc", "--by", "project", "--project", "web", "-o", "json")
		assert.Nil(t, r.err)
		assert.Equal(t, 1, r.count(contractProjectPath), "the lookup's read is the only collection read: %v", r.requests)
		var env struct {
			Data struct {
				Rows map[string]json.RawMessage `json:"rows"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &env))
		assert.Equal(t, []string{"p-xyz"}, slices.Sorted(maps.Keys(env.Data.Rows)), "one bucket, the project's")
		table := h.run(t, "usage", "--cluster", "c-abc", "--by", "project", "--project", "web", "-o", "table")
		assert.Contains(t, table.stdout, "web", "the bucket is named by the project's display name")
		assert.Equal(t, 1, table.count(contractProjectPath))

		bare := h.run(t, "usage", "--cluster", "c-abc", "--by", "project", "-o", "json")
		assert.Nil(t, bare.err)
		assert.Equal(t, 1, bare.count(contractProjectPath), "without --project the buckets' names come from one collection read")
		assert.Empty(t, bare.reads)
	})

	t.Run("hints", func(t *testing.T) {
		ns := filepath.Join(t.TempDir(), "ns.yaml")
		require.NoError(t, os.WriteFile(ns, []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns1\n"), 0o600))
		for _, argv := range [][]string{
			{"create", "resource", "-f", ns, "--cluster", "c-abc", "--project", "web", "--dry-run", "-o", "json"},
			{"get", "pod", "--project", "nosuch", "-o", "json"},
			{"get", "pod", "--cluster", "c-abc", "--project", "nosuch", "-o", "json"},
			{"create", "secret", "s", "--cluster", "c-abc", "--dry-run", "-o", "json"},
		} {
			r := h.run(t, argv...)
			require.NotNil(t, r.err, "%v", argv)
			require.NotEmpty(t, r.err.Hint, "%v: %+v", argv, r.err)
			hints = append(hints, r.err.Hint)
		}
		secret := h.run(t, "create", "secret", "s", "--cluster", "c-abc", "--dry-run", "-o", "json")
		require.NotNil(t, secret.err)
		assert.Equal(t, "VALIDATION", secret.err.Code)
		assert.Equal(t, "pass --namespace <namespace> to create a secret in a namespace, or --project <project> to create a project-scoped secret", secret.err.Message)
		assert.Equal(t, withDirectory(h.cfgDir, "rancher access projects --cluster 'c-abc'"), secret.err.Hint)
		assert.Equal(t, validationExit, secret.exit)
		assert.Empty(t, secret.requests, "the flag combination is refused before any request")
		assert.Contains(t, hints, withDirectory(h.cfgDir, "rancher create namespace 'ns1' --cluster 'c-abc' --project 'web' --dry-run"))
		assert.Contains(t, hints, withDirectory(h.cfgDir, "rancher access projects"))
		assert.Contains(t, hints, withDirectory(h.cfgDir, "rancher access projects --cluster 'c-abc'"))
		slices.Sort(hints)
		for _, hint := range slices.Compact(hints) {
			r := h.run(t, append(splitHint(t, hint), "-o", "json")...)
			if r.err != nil {
				assert.NotEqual(t, "VALIDATION", r.err.Code, "the hint %q: %+v", hint, r.err)
			}
		}
	})
}

// TestNotLoggedInIsTheSameWhicheverClientReadsFirst asserts a command that
// needs the server answers one not-logged-in error, whichever client
// reaches the server first: with --project and no --cluster the root
// client reads first (the clusters the project is on), with --cluster a
// cluster client does. On a blank machine and with a server whose
// credential is gone, both paths give the same code, message and remedy,
// and nothing is sent.
func TestNotLoggedInIsTheSameWhicheverClientReadsFirst(t *testing.T) {
	h := newContractHarness(t)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	require.NoError(t, os.Unsetenv(config.EnvURL))
	require.NoError(t, os.Unsetenv(config.EnvToken))
	commands := [][]string{
		{"get", "pod"},
		{"usage"},
		{"grant", "u-1", "--role", "project-member"},
		{"access", "can", "get", "pods"},
	}
	for _, tc := range []struct {
		name    string
		servers map[string]config.Server
		code    string
		message string
		hint    string
	}{
		{"blank machine", nil, "NOT_LOGGED_IN",
			"not logged in and no RANCHER_CLI_URL set; <url> is the address of your Rancher server; add a server with: rancher auth login --url <url>", ""},
		{"server without a credential", map[string]config.Server{"x": {URL: h.base}}, "NOT_LOGGED_IN",
			`not logged in to "x"`, "rancher auth login 'x'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(config.EnvConfigDir, dir)
			seed := &config.Config{Servers: tc.servers}
			if len(tc.servers) > 0 {
				seed.CurrentServer = "x"
			}
			seed.SetPath(config.FilePath(dir))
			require.NoError(t, seed.Write())
			for _, cmd := range commands {
				for _, argv := range [][]string{
					append(slices.Clone(cmd), "--project", "web", "-o", "json"),
					append(slices.Clone(cmd), "--cluster", "c-abc", "-o", "json"),
				} {
					r := h.run(t, argv...)
					require.NotNil(t, r.err, "%v: %s %s", argv, r.stdout, r.stderr)
					assert.Equal(t, tc.code, r.err.Code, "%v", argv)
					assert.Equal(t, withDirectory(dir, tc.message), r.err.Message, "%v", argv)
					assert.Equal(t, withDirectory(dir, tc.hint), r.err.Hint, "%v", argv)
					assert.Equal(t, clierr.ExitCode(tc.code), r.exit, "%v", argv)
					assert.Empty(t, r.requests, "%v: nothing is sent", argv)
				}
			}
		})
	}
}
