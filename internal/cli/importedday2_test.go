package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/zalando/go-keyring"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

const day2Bearer = "u-owner:secret"

// day2Rows returns the rows of the operation.cattle.io group and asserts the four
// rows for the objects of an imported cluster are among them.
func day2Rows(t *testing.T) []kinds.Kind {
	t.Helper()
	var rows []kinds.Kind
	var keys []string
	for _, k := range kinds.All() {
		if k.GVR.Group == "operation.cattle.io" {
			rows = append(rows, k)
			keys = append(keys, k.Key)
		}
	}
	for _, want := range []string{"etcdsnapshotsave", "etcdsnapshotrestore", "encryptionkeyrotation", "certificaterotation"} {
		require.Contains(t, keys, want, "the day-2 operation rows are missing a row")
	}
	return rows
}

// day2ManifestExtras is the spec a row's manifest needs beyond spec.clusterRef
// and spec.ttl, by row key: a restore names its snapshot. The test that uses it
// asserts its keys equal the keys of the group, so a new row fails with "no
// fixture".
var day2ManifestExtras = map[string]string{
	"etcdsnapshotsave":      "",
	"etcdsnapshotrestore":   "  args:\n    name: snap\n",
	"encryptionkeyrotation": "",
	"certificaterotation":   "",
}

func requireDay2Fixtures(t *testing.T, rows []kinds.Kind) {
	t.Helper()
	var want []string
	for _, k := range rows {
		want = append(want, k.Key)
	}
	slices.Sort(want)
	require.Equal(t, want, slices.Sorted(maps.Keys(day2ManifestExtras)), "no fixture for a row of the group, or a fixture for a row that is not one")
}

// day2Manifest is the manifest of the row for cluster c-m-x, with the spec lines
// extra added; ttl is the spec.ttl line ("" leaves it out).
func day2Manifest(k kinds.Kind, ttl, extra string) string {
	return fmt.Sprintf("apiVersion: operation.cattle.io/v1alpha1\nkind: %s\nmetadata:\n  namespace: c-m-x\n  generateName: c-m-x-\nspec:\n%s  clusterRef:\n    apiVersion: management.cattle.io/v3\n    kind: Cluster\n    name: c-m-x\n%s%s",
		k.K8sKind, ttl, day2ManifestExtras[k.Key], extra)
}

// day2Server is a Rancher server for the checks of a day-2 operation create:
// the group list and the resource list of the local cluster's Kubernetes
// API, the management Cluster c-m-x through Steve, the two features, the two
// Settings, one etcd snapshot, and the dry-run and real create of the four
// kinds. It records every request.
type day2Server struct {
	mu   sync.Mutex
	reqs []string

	listGroup bool
	resources []string
	features  map[string]bool
	cluster   map[string]any
	// createStatus and createText, when set, are the answer to every create.
	createStatus int
	createText   string
	// dropSpecCancel makes a create answer without spec.cancel, as a server whose
	// CRD lacks the field does.
	dropSpecCancel bool
}

func newDay2Server() *day2Server {
	return &day2Server{
		listGroup: true,
		resources: []string{"etcdsnapshotsaves", "etcdsnapshotrestores", "encryptionkeyrotations", "certificaterotations"},
		features:  map[string]bool{"imported-day-2-ops": true, "provisioningv2": true},
		cluster: map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
			"metadata": map[string]any{"name": "c-m-x", "creationTimestamp": "2026-11-01T10:00:00Z"},
			"spec":     map[string]any{"displayName": "x"},
			"status": map[string]any{
				"driver": "k3s", "provider": "k3s",
				"conditions": []any{map[string]any{"type": "Connected", "status": "True"}},
			},
		},
	}
}

func (s *day2Server) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	s.reqs = append(s.reqs, line)
}

// requests returns what the server received since the last call.
func (s *day2Server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

func (s *day2Server) handler(w http.ResponseWriter, r *http.Request) {
	const local = "/k8s/clusters/local/apis"
	p := r.URL.Path
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func() {
		write(http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": "not found", "reason": "NotFound", "code": 404})
	}
	switch {
	case p == "/rancherversion":
		write(http.StatusOK, map[string]any{"Version": "v2.16.0", "GitCommit": "x", "RancherPrime": "false"})
	case r.Method == http.MethodGet && p == local:
		groups := []any{}
		if s.listGroup {
			gv := map[string]any{"groupVersion": "operation.cattle.io/v1alpha1", "version": "v1alpha1"}
			groups = append(groups, map[string]any{"name": "operation.cattle.io", "versions": []any{gv}, "preferredVersion": gv})
		}
		write(http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups})
	case r.Method == http.MethodGet && p == local+"/operation.cattle.io/v1alpha1":
		items := []any{}
		for _, name := range s.resources {
			items = append(items, map[string]any{"name": name})
		}
		write(http.StatusOK, map[string]any{"kind": "APIResourceList", "groupVersion": "operation.cattle.io/v1alpha1", "resources": items})
	case r.Method == http.MethodGet && strings.HasPrefix(p, local+"/management.cattle.io/v3/features/"):
		name := strings.TrimPrefix(p, local+"/management.cattle.io/v3/features/")
		on, ok := s.features[name]
		if !ok {
			notFound()
			return
		}
		write(http.StatusOK, map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Feature", "metadata": map[string]any{"name": name}, "spec": map[string]any{"value": on}})
	case r.Method == http.MethodGet && strings.HasPrefix(p, local+"/management.cattle.io/v3/settings/"):
		name := strings.TrimPrefix(p, local+"/management.cattle.io/v3/settings/")
		write(http.StatusOK, map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Setting", "metadata": map[string]any{"name": name}, "value": "true"})
	case r.Method == http.MethodGet && strings.HasPrefix(p, local+"/rke.cattle.io/v1/namespaces/c-m-x/etcdsnapshots"):
		snapshot := map[string]any{
			"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
			"metadata":     map[string]any{"name": "snap", "namespace": "c-m-x"},
			"spec":         map[string]any{"clusterName": "c-m-x"},
			"snapshotFile": map[string]any{"status": "successful", "nodeName": "node1", "createdAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		}
		if p == local+"/rke.cattle.io/v1/namespaces/c-m-x/etcdsnapshots" {
			write(http.StatusOK, map[string]any{"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshotList", "metadata": map[string]any{}, "items": []any{snapshot}})
			return
		}
		write(http.StatusOK, snapshot)
	case r.Method == http.MethodPost && strings.HasPrefix(p, local+"/operation.cattle.io/v1alpha1/namespaces/c-m-x/"):
		if s.createStatus != 0 {
			write(s.createStatus, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": s.createText, "reason": "Forbidden", "code": s.createStatus})
			return
		}
		obj := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&obj)
		meta, _ := obj["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		if meta["name"] == nil || meta["name"] == "" {
			suffix := "dryrn"
			if r.URL.Query().Get("dryRun") == "" {
				suffix = "reals"
			}
			meta["name"] = fmt.Sprint(meta["generateName"]) + suffix
		}
		meta["resourceVersion"] = "1"
		obj["metadata"] = meta
		if spec, ok := obj["spec"].(map[string]any); ok && s.dropSpecCancel {
			delete(spec, "cancel")
		}
		write(http.StatusCreated, obj)
	default:
		notFound()
	}
}

// day2Harness is a config directory holding a login to a day2Server, and the
// server.
type day2Harness struct {
	srv  *day2Server
	dir  string
	file string
	// url is the server's URL.
	url string
}

func newDay2Harness(t *testing.T, configure func(s *day2Server)) *day2Harness {
	t.Helper()
	keyring.MockInit()
	s := newDay2Server()
	if configure != nil {
		configure(s)
	}
	f := testrancher.New(testrancher.Options{
		SeedToken: day2Bearer,
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return []testrancher.ClusterFixture{{ID: "c-m-x", DisplayName: "x"}}, b == day2Bearer
		},
		ClusterObject: func(id string) (map[string]any, bool) {
			if id != "c-m-x" {
				return nil, false
			}
			return s.cluster, true
		},
		RequestHook: s.record,
		Fallthrough: http.HandlerFunc(s.handler),
	})
	hs := httptest.NewTLSServer(f)
	t.Cleanup(hs.Close)

	cfgDir, home := t.TempDir(), t.TempDir()
	t.Setenv(config.EnvConfigDir, cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	require.NoError(t, os.Unsetenv(config.EnvURL))
	require.NoError(t, os.Unsetenv(config.EnvToken))
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "x", Servers: map[string]config.Server{"x": {URL: hs.URL, CA: string(pemFromServer(t, hs))}}}
	cfg.SetPath(config.FilePath(cfgDir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(cfgDir).SetToken(config.ServerRef{Name: "x", URL: hs.URL}, day2Bearer))
	t.Cleanup(func() { CaptureArgv(nil) })
	return &day2Harness{srv: s, dir: cfgDir, file: filepath.Join(t.TempDir(), "m.yaml"), url: hs.URL}
}

// day2Run is one command run: its two streams, its exit status and the
// requests the server received.
type day2Run struct {
	stdout, stderr string
	requests       []string
	status         int
}

// run writes the manifest and runs `rancher create resource -f <file> args...`.
func (h *day2Harness) run(t *testing.T, manifest string, args ...string) day2Run {
	t.Helper()
	require.NoError(t, os.WriteFile(h.file, []byte(manifest), 0o600))
	h.srv.requests()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	argv := append([]string{"create", "resource", "-f", h.file, "-o", "json"}, args...)
	CaptureArgv(argv)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	status := 0
	if err := root.Run(ctx, append([]string{"rancher"}, argv...)); err != nil {
		status = 1
		if coder, ok := err.(exitCoder); ok {
			status = coder.ExitCode()
		}
	}
	return day2Run{stdout: out.String(), stderr: errOut.String(), requests: h.srv.requests(), status: status}
}

// day2Envelope is the part of a JSON envelope the tests read.
type day2Envelope struct {
	Warnings []string `json:"warnings"`
	Data     struct {
		Action      string `json:"action"`
		Target      string `json:"target"`
		Destructive bool   `json:"destructive"`
		Reversible  bool   `json:"reversible"`
		Error       struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Feature string `json:"feature"`
				Server  string `json:"server"`
			} `json:"details"`
		} `json:"error"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"data"`
}

func decodeDay2(t *testing.T, stream string) day2Envelope {
	t.Helper()
	i := strings.Index(stream, "{")
	require.GreaterOrEqual(t, i, 0, "no JSON in %q", stream)
	var env day2Envelope
	require.NoError(t, json.Unmarshal([]byte(stream[i:]), &env), stream)
	return env
}

// posts returns the create requests of a run: dry-run ones when dry is true,
// the others when it is false.
func posts(requests []string, dry bool) []string {
	var out []string
	for _, r := range requests {
		if strings.HasPrefix(r, "POST ") && strings.Contains(r, "/operation.cattle.io/") && strings.Contains(r, "dryRun=") == dry {
			out = append(out, r)
		}
	}
	return out
}

// TestImportedDay2CreateChecksRunBeforeTheDryRun asserts, for every row of the
// group, that a create resource of its manifest answers VALIDATION and sends no
// create when the feature imported-day-2-ops is off: the checks run before the
// dry-run.
func TestImportedDay2CreateChecksRunBeforeTheDryRun(t *testing.T) {
	rows := day2Rows(t)
	requireDay2Fixtures(t, rows)
	h := newDay2Harness(t, func(s *day2Server) { s.features["imported-day-2-ops"] = false })
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			run := h.run(t, day2Manifest(k, "  ttl: 60\n", ""), "--dry-run")
			env := decodeDay2(t, run.stderr)
			assert.Equal(t, "VALIDATION", env.Data.Error.Code, run.stderr)
			assert.Equal(t, "the imported-day-2-ops feature is off on the connected server; Rancher runs no day-2 operations on imported clusters while it is off", env.Data.Error.Message)
			assert.Equal(t, 2, run.status)
			assert.Empty(t, posts(run.requests, true), "no dry-run was sent")
			assert.Empty(t, posts(run.requests, false))
		})
	}
}

// TestImportedDay2CreateIsNotReversible asserts the preview of every row says
// the create is not reversible, destructive as the row says, and names the
// generateName prefix as the target.
func TestImportedDay2CreateIsNotReversible(t *testing.T) {
	rows := day2Rows(t)
	requireDay2Fixtures(t, rows)
	h := newDay2Harness(t, nil)
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			run := h.run(t, day2Manifest(k, "  ttl: 60\n", ""), "--dry-run")
			require.Zero(t, run.status, run.stderr)
			env := decodeDay2(t, run.stdout)
			assert.Equal(t, "create", env.Data.Action)
			assert.False(t, env.Data.Reversible, "reversible")
			assert.Equal(t, k.Sensitive, env.Data.Destructive, "destructive")
			assert.Equal(t, k.GVR.Resource+"/c-m-x-*", env.Data.Target)
			assert.Len(t, posts(run.requests, true), 1, "one dry-run create")
			assert.Empty(t, posts(run.requests, false))
		})
	}
}

// TestImportedDay2CreateWarningFollowsTheChecks asserts, for every row, that the
// warnings of a successful preview are the warnings of the checks and then the
// row's CreateWarning.
func TestImportedDay2CreateWarningFollowsTheChecks(t *testing.T) {
	rows := day2Rows(t)
	requireDay2Fixtures(t, rows)
	h := newDay2Harness(t, nil)
	const ttlWarning = "spec.ttl is 0: Rancher deletes this object a few seconds after the day-2 operation ends, and a wait can then answer NOT_FOUND; the Rancher UI sets spec.ttl: 60"
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			run := h.run(t, day2Manifest(k, "", ""), "--dry-run")
			require.Zero(t, run.status, run.stderr)
			env := decodeDay2(t, run.stdout)
			require.NotEmpty(t, env.Warnings)
			assert.Equal(t, ttlWarning, env.Warnings[0], "the warning of the manifest check comes first")
			assert.Equal(t, k.CreateWarning, env.Warnings[len(env.Warnings)-1], "the row's warning comes last")
			assert.Equal(t, k.Sensitive, env.Data.Destructive)
			if k.Key == "etcdsnapshotrestore" {
				require.Len(t, env.Warnings, 4, "the restore's own warnings sit between: %v", env.Warnings)
				assert.True(t, strings.HasPrefix(env.Warnings[1], "spec.args.restoreMode is empty: "), env.Warnings[1])
				assert.True(t, strings.HasPrefix(env.Warnings[2], "Rancher restores etcd of cluster c-m-x to the snapshot taken at "), env.Warnings[2])
			}
		})
	}
}

// TestImportedDay2ApplyNeedsYesForRestoreOnly asserts a restore with --apply and
// no --yes is CONFIRMATION_REQUIRED and sends no create that is not a dry-run,
// and that a snapshot with --apply is created.
func TestImportedDay2ApplyNeedsYesForRestoreOnly(t *testing.T) {
	h := newDay2Harness(t, nil)
	rows := map[string]kinds.Kind{}
	for _, k := range day2Rows(t) {
		rows[k.Key] = k
	}

	run := h.run(t, day2Manifest(rows["etcdsnapshotrestore"], "  ttl: 60\n", "    restoreMode: none\n"), "--apply")
	env := decodeDay2(t, run.stderr)
	assert.Equal(t, "CONFIRMATION_REQUIRED", env.Data.Error.Code, run.stderr)
	assert.Empty(t, posts(run.requests, false), "no real create without --yes")

	run = h.run(t, day2Manifest(rows["etcdsnapshotrestore"], "  ttl: 60\n", "    restoreMode: none\n"), "--apply", "--yes")
	require.Zero(t, run.status, run.stderr)
	assert.Len(t, posts(run.requests, false), 1, "the restore is created with --yes")

	run = h.run(t, day2Manifest(rows["etcdsnapshotsave"], "  ttl: 60\n", ""), "--apply")
	require.Zero(t, run.status, run.stderr)
	assert.Len(t, posts(run.requests, false), 1, "a snapshot needs --apply only")
	assert.Equal(t, "c-m-x-reals", decodeDay2(t, run.stdout).Data.Metadata.Name, "the result names the object Rancher created")
}

// TestImportedDay2CreateServerRefusals asserts a 403 of the dry-run whose text
// after "is forbidden: " is an admission text is VALIDATION at exit 2, and any
// other 403 is RBAC at exit 6 with the server's message.
func TestImportedDay2CreateServerRefusals(t *testing.T) {
	rows := day2Rows(t)
	rbac := `etcdsnapshotsaves.operation.cattle.io is forbidden: User "u-owner" cannot create resource "etcdsnapshotsaves" in API group "operation.cattle.io" in the namespace "c-m-x"`
	for _, tc := range []struct {
		name, text, code string
		status           int
	}{
		{"an admission text", "etcdsnapshotsaves.operation.cattle.io is forbidden: this namespace may not be deleted", "VALIDATION", 2},
		{"a missing permission", rbac, "RBAC", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDay2Harness(t, func(s *day2Server) { s.createStatus, s.createText = http.StatusForbidden, tc.text })
			var save kinds.Kind
			for _, k := range rows {
				if k.Key == "etcdsnapshotsave" {
					save = k
				}
			}
			run := h.run(t, day2Manifest(save, "  ttl: 60\n", ""), "--dry-run")
			env := decodeDay2(t, run.stderr)
			assert.Equal(t, tc.code, env.Data.Error.Code, run.stderr)
			assert.Equal(t, tc.status, run.status)
			assert.Contains(t, env.Data.Error.Message, strings.TrimPrefix(tc.text, "etcdsnapshotsaves.operation.cattle.io is forbidden: "))
		})
	}
}

// TestDiscoveryCapabilityTier asserts the checker of the discovery tier reads
// exactly the two paths of the local cluster's Kubernetes API, and decides:
// a readable group list without the group, or a readable resource list without
// the resource, is unsupported; a failed read, a body that is not a list and a
// resource list of another group version are unknown; the group and the
// resource listed is proceed.
func TestDiscoveryCapabilityTier(t *testing.T) {
	t.Parallel()
	const groupList = `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"operation.cattle.io","versions":[{"groupVersion":"operation.cattle.io/v1alpha1","version":"v1alpha1"}]}]}`
	const resourceList = `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1","resources":[{"name":"etcdsnapshotsaves"},{"name":"etcdsnapshotsaves/status"}]}`
	type answer struct {
		status int
		body   string
	}
	for _, tc := range []struct {
		name  string
		list  answer
		doc   answer
		want  runtime.CapabilityOutcome
		paths []string
	}{
		{"group and resource listed", answer{200, groupList}, answer{200, resourceList}, runtime.CapabilityProceed, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"group list without the group", answer{200, `{"kind":"APIGroupList","groups":[{"name":"apps"}]}`}, answer{}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis"}},
		{"group list with no groups", answer{200, `{"kind":"APIGroupList","groups":[]}`}, answer{}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis"}},
		{"a groups array with no kind", answer{200, `{"groups":[{"name":"apps"}]}`}, answer{}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis"}},
		{"a list of kind APIGroupList with no groups array", answer{200, `{"kind":"APIGroupList"}`}, answer{}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis"}},
		{"list 403", answer{403, ""}, answer{}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis"}},
		{"list 500", answer{500, ""}, answer{}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis"}},
		{"list is {}", answer{200, `{}`}, answer{}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis"}},
		{"list is an unrelated object", answer{200, `{"kind":"Status","status":"Failure"}`}, answer{}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis"}},
		{"list is not JSON", answer{200, `<html>`}, answer{}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis"}},
		{"resource list without the resource", answer{200, groupList}, answer{200, `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1","resources":[{"name":"etcdsnapshotrestores"}]}`}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"resource list with no resources", answer{200, groupList}, answer{200, `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1","resources":[]}`}, runtime.CapabilityUnsupported, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"resource list 403", answer{200, groupList}, answer{403, ""}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"resource list is {}", answer{200, groupList}, answer{200, `{}`}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"resource list of another group version", answer{200, groupList}, answer{200, `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1","resources":[{"name":"etcdsnapshotsaves"}]}`}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
		{"resource list with no resources array", answer{200, groupList}, answer{200, `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1"}`}, runtime.CapabilityUnknown, []string{"/k8s/clusters/local/apis", "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var paths []string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				a := tc.doc
				if r.URL.Path == "/k8s/clusters/local/apis" {
					a = tc.list
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(a.status)
				_, _ = w.Write([]byte(a.body))
			}))
			t.Cleanup(srv.Close)
			dec := checkerFor(t, srv).Check(t.Context(), runtime.CapabilityRequirement{
				Tier: runtime.CapabilityDiscovery, Group: "operation.cattle.io", Version: "v1alpha1", Feature: "etcdsnapshotsaves",
				Unsupported: "the ETCDSnapshotSave operation, which Rancher v2.15 added",
			})
			assert.Equal(t, tc.want, dec.Outcome)
			assert.Equal(t, srv.URL, dec.Server)
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tc.paths, paths, "the checker reads these paths and no other, never /apis at the server root")
		})
	}
}

// TestDay2RowsAnswerCapabilityUnsupportedOnAnOlderServer asserts every verb of
// every row answers CAPABILITY_UNSUPPORTED with the Rancher version that added
// the kind when the server's group list has no operation.cattle.io group, and
// that certificate rotation alone is unsupported on a v2.15 resource list.
func TestDay2RowsAnswerCapabilityUnsupportedOnAnOlderServer(t *testing.T) {
	h := newDay2Harness(t, func(s *day2Server) { s.listGroup = false })
	for _, k := range day2Rows(t) {
		t.Run(k.Key, func(t *testing.T) {
			for _, verb := range []string{"get", "patch", "delete"} {
				args := []string{verb, k.Name}
				if verb != "get" {
					args = append(args, "op", "--cluster", "c-m-x", "--dry-run")
					if verb == "patch" {
						args = append(args, "--patch", `{"spec":{"ttl":5}}`, "--type", "merge")
					}
				} else {
					args = append(args, "--cluster", "c-m-x")
				}
				env := runDay2Args(t, args)
				assert.Equal(t, "CAPABILITY_UNSUPPORTED", env.Data.Error.Code, "%s %s: %v", verb, k.Name, env)
				assert.Equal(t, "the connected server does not support "+k.Capability.Unsupported+"; upgrade the server to a version that does", env.Data.Error.Message)
				assert.Equal(t, k.GVR.Resource, env.Data.Error.Details.Feature)
			}
			env := decodeDay2(t, h.run(t, day2Manifest(k, "  ttl: 60\n", ""), "--dry-run").stderr)
			assert.Equal(t, "CAPABILITY_UNSUPPORTED", env.Data.Error.Code, "create resource of %s", k.Name)
		})
	}

	v215 := newDay2Harness(t, func(s *day2Server) {
		s.resources = []string{"etcdsnapshotsaves", "etcdsnapshotrestores", "encryptionkeyrotations"}
	})
	for _, k := range day2Rows(t) {
		run := v215.run(t, day2Manifest(k, "  ttl: 60\n", ""), "--dry-run")
		if k.Key == "certificaterotation" {
			assert.Equal(t, "CAPABILITY_UNSUPPORTED", decodeDay2(t, run.stderr).Data.Error.Code, k.Key)
		} else {
			assert.Zero(t, run.status, "%s on a v2.15 resource list: %s", k.Key, run.stderr)
		}
	}
}

// runDay2Args runs the command line in the current harness and decodes its error.
func runDay2Args(t *testing.T, args []string) day2Envelope {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	argv := append(slices.Clone(args), "-o", "json")
	CaptureArgv(argv)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = root.Run(ctx, append([]string{"rancher"}, argv...))
	require.NotEmpty(t, errOut.String(), "no error output for %v; stdout: %s", argv, out.String())
	return decodeDay2(t, errOut.String())
}

// TestRowWithObjectChecksNeedsCommandsThatRunThem asserts the build of the
// command tree refuses a row with ObjectChecks whose patch or delete command
// does not run them, and a refused patch, and accepts the generated commands.
func TestRowWithObjectChecksNeedsCommandsThatRunThem(t *testing.T) {
	t.Parallel()
	checks := &kinds.ObjectChecks{
		Patch:  func(orig, patched *unstructured.Unstructured) kinds.Verdict { return kinds.Verdict{} },
		Delete: func(obj *unstructured.Unstructured) kinds.Verdict { return kinds.Verdict{} },
	}
	row := kinds.Kind{Key: "widget", Name: "widget", K8sKind: "Widget", Aliases: []string{"widgets"}, Deletable: true, ObjectChecks: checks}
	foreign := func(k kinds.Kind, d runDeps) runtime.Command { return resourcesFakeCommand{} }
	for _, tc := range []struct {
		name      string
		row       kinds.Kind
		overrides map[string]kindOverride
		want      string
	}{
		{"generated commands", row, nil, ""},
		{"a patch that does not run them", row, map[string]kindOverride{"widget": {patch: foreign}}, `kind "widget" has ObjectChecks for patch but its patch command does not run them`},
		{"a delete that does not run them", row, map[string]kindOverride{"widget": {del: foreign}}, `kind "widget" has ObjectChecks for delete but its delete command does not run them`},
		{"a refused patch", func() kinds.Kind { k := row; k.NoPatchReason = "no"; return k }(), nil, `kind "widget" has ObjectChecks for patch but its patch command does not run them`},
		{"a row with no checks", kinds.Kind{Key: "widget", Name: "widget", Aliases: []string{"widgets"}}, map[string]kindOverride{"widget": {patch: foreign}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkObjectChecked([]kinds.Kind{tc.row}, tc.overrides)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
			assert.True(t, strings.HasPrefix(err.Error(), "internal error: kind definitions: "), err.Error())
		})
	}
}

// resourcesFakeCommand is a command that does not run the object checks of a
// row: it does not implement resources.ObjectChecked.
type resourcesFakeCommand struct{ runtime.Command }

// TestImportedDay2CreateRemovedFieldNamesTheServer asserts that a dry-run answer
// without spec.cancel is CAPABILITY_UNSUPPORTED for the field, with the feature
// and the server the command is connected to in its details.
func TestImportedDay2CreateRemovedFieldNamesTheServer(t *testing.T) {
	h := newDay2Harness(t, func(s *day2Server) { s.dropSpecCancel = true })
	for _, k := range day2Rows(t) {
		t.Run(k.Key, func(t *testing.T) {
			run := h.run(t, day2Manifest(k, "  ttl: 60\n", "  cancel: true\n"), "--dry-run")
			env := decodeDay2(t, run.stderr)
			assert.Equal(t, "CAPABILITY_UNSUPPORTED", env.Data.Error.Code, run.stderr)
			assert.Equal(t, fmt.Sprintf("the connected server does not support spec.cancel of %s, which Rancher v2.16 added", k.K8sKind), env.Data.Error.Message)
			assert.Equal(t, k.GVR.Resource+".spec.cancel", env.Data.Error.Details.Feature)
			assert.Equal(t, h.url, env.Data.Error.Details.Server)
			assert.Empty(t, posts(run.requests, false), "nothing was written")
		})
	}
}
