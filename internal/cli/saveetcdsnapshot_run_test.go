package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	savePrefix  = "/k8s/clusters/local/apis/"
	saveCluster = savePrefix + "provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"
	saveList    = "/v1/provisioning.cattle.io.clusters/fleet-default"
	savePlane   = savePrefix + "rke.cattle.io/v1/namespaces/fleet-default/rkecontrolplanes/prod"
	saveSnaps   = savePrefix + "rke.cattle.io/v1/namespaces/fleet-default/etcdsnapshots"
	saveMgmt    = "/v1/management.cattle.io.clusters/c-m-prod1"
)

// savePut is one PUT the server received.
type savePut struct {
	path         string
	dryRun       bool
	fieldManager string
	body         map[string]any
}

// saveServer serves a provisioned cluster whose spec.rkeConfig names a cloud
// credential, its RKEControlPlane, its ready management Cluster and its
// etcd snapshot objects, and records every request. A request whose path has a
// secrets segment fails the test. A PUT that is not a dry run makes Rancher
// finish the request: the RKEControlPlane's status copy takes the written
// generation and its phase is Finished.
type saveServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	puts     []savePut
	cluster  map[string]any
	plane    map[string]any
}

func newSaveServer(t *testing.T) (*saveServer, *httptest.Server) {
	t.Helper()
	s := &saveServer{t: t,
		cluster: map[string]any{
			"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "prod", "namespace": "fleet-default", "resourceVersion": "7", "generation": 1},
			"spec": map[string]any{"kubernetesVersion": "v1.31.4+rke2r1", "rkeConfig": map[string]any{
				"etcd": map[string]any{"s3": map[string]any{"bucket": "backups", "cloudCredentialName": "cattle-global-data:cc-1"}},
			}},
			"status": map[string]any{"clusterName": "c-m-prod1"},
		},
		plane: map[string]any{
			"apiVersion": "rke.cattle.io/v1", "kind": "RKEControlPlane",
			"metadata": map[string]any{"name": "prod", "namespace": "fleet-default", "resourceVersion": "3"},
			"status":   map[string]any{},
		},
	}
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *saveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if slices.Contains(strings.Split(r.URL.Path, "/"), "secrets") {
		s.t.Errorf("the command read a Secret: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		return
	}
	reply := func(obj any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == saveCluster:
		reply(s.cluster)
	case r.Method == http.MethodPut && r.URL.Path == saveCluster:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		q := r.URL.Query()
		put := savePut{path: r.URL.Path, dryRun: q.Get("dryRun") != "", fieldManager: q.Get("fieldManager"), body: body}
		s.puts = append(s.puts, put)
		if !put.dryRun {
			s.cluster = body
			generation := int64(0)
			if f, ok := body["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotCreate"].(map[string]any); ok {
				if n, ok := f["generation"].(float64); ok {
					generation = int64(n)
				}
			}
			s.plane["status"] = map[string]any{"etcdSnapshotCreate": map[string]any{"generation": generation}, "etcdSnapshotCreatePhase": "Finished"}
		}
		reply(body)
	case r.Method == http.MethodGet && r.URL.Path == savePlane:
		reply(s.plane)
	case r.URL.Path == strings.TrimSuffix(savePlane, "/prod"):
		http.Error(w, `{"message":"no list or watch"}`, http.StatusBadRequest)
	case r.Method == http.MethodGet && r.URL.Path == saveMgmt:
		reply(map[string]any{
			"id": "c-m-prod1", "type": "management.cattle.io.cluster",
			"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
			"metadata": map[string]any{"name": "c-m-prod1", "resourceVersion": "9"},
			"status":   map[string]any{"driver": "rke2", "conditions": []any{map[string]any{"type": "Connected", "status": "True"}}},
		})
	case r.Method == http.MethodGet && r.URL.Path == saveSnaps:
		reply(map[string]any{"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshotList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{map[string]any{
			"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
			"metadata":     map[string]any{"name": "snap-1"},
			"spec":         map[string]any{"clusterName": "prod"},
			"snapshotFile": map[string]any{"nodeName": "n1", "status": "successful", "createdAt": "2026-11-18T10:00:00Z"},
		}}})
	case r.Method == http.MethodGet && r.URL.Path == saveList:
		reply(map[string]any{"data": []any{
			map[string]any{"metadata": map[string]any{"name": "prod"}, "spec": s.cluster["spec"]},
			map[string]any{"metadata": map[string]any{"name": "edge"}, "spec": map[string]any{}},
			map[string]any{"metadata": map[string]any{"name": "imported"}, "spec": map[string]any{"rkeConfig": nil}},
		}})
	default:
		http.NotFound(w, r)
	}
}

func (s *saveServer) putsOf() []savePut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.puts)
}

func (s *saveServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.requests
	s.requests = nil
	return out
}

// saveEnv points the CLI at the server with a token. Not for a parallel test:
// it sets the process environment.
func saveEnv(t *testing.T) (*saveServer, *httptest.Server) {
	t.Helper()
	restoreRawArgv(t)
	extraArgsEnv(t)
	s, srv := newSaveServer(t)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return s, srv
}

// saveEnvelope decodes the JSON envelope of a run.
func saveEnvelope(t *testing.T, stdout string) (data map[string]any, warnings []string) {
	t.Helper()
	var env struct {
		Data     map[string]any `json:"data"`
		Warnings []string       `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
	return env.Data, env.Warnings
}

// TestSaveEtcdSnapshotPreviewReportsNotReversible asserts the preview of the
// real command run is a server dry-run with reversible false and no write.
func TestSaveEtcdSnapshotPreviewReportsNotReversible(t *testing.T) {
	s, _ := saveEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "save-etcd-snapshot", "prod", "--dry-run", "-o", "json"})
	require.Zero(t, code, stderr)
	data, warnings := saveEnvelope(t, stdout)
	assert.Equal(t, false, data["reversible"])
	assert.Equal(t, true, data["destructive"])
	assert.Equal(t, "serverDryRun", data["previewSource"])
	assert.NotEmpty(t, warnings)
	puts := s.putsOf()
	require.Len(t, puts, 1)
	assert.True(t, puts[0].dryRun)
}

// TestSaveEtcdSnapshotNeedsApplyAndYes asserts --apply without --yes is
// CONFIRMATION_REQUIRED and sends no update.
func TestSaveEtcdSnapshotNeedsApplyAndYes(t *testing.T) {
	s, _ := saveEnv(t)
	_, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "save-etcd-snapshot", "prod", "--apply", "-o", "json"})
	assert.NotZero(t, code)
	assert.Equal(t, clierr.CodeConfirmationRequired, renderedError(t, "json", stderr).Code)
	for _, put := range s.putsOf() {
		assert.True(t, put.dryRun, "no write without --yes")
	}
}

// TestSaveEtcdSnapshotAppliedRun asserts the applied run of the real command
// writes once, sends the field manager on every PUT, waits until the status
// copy and phase say done, lists the cluster's snapshots, and reads no Secret
// although spec.rkeConfig.etcd.s3 names a cloud credential.
func TestSaveEtcdSnapshotAppliedRun(t *testing.T) {
	s, _ := saveEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "save-etcd-snapshot", "prod", "--apply", "--yes", "-o", "json"})
	require.Zero(t, code, stderr)
	data, warnings := saveEnvelope(t, stdout)
	assert.Equal(t, true, data["waited"])
	assert.Equal(t, true, data["written"])
	assert.Equal(t, "spec.rkeConfig.etcdSnapshotCreate", data["field"])
	assert.Equal(t, float64(1), data["generation"])
	assert.Equal(t, "Finished", data["phase"])
	assert.Equal(t, "c-m-prod1", data["clusterId"])
	assert.Equal(t, []any{map[string]any{"name": "snap-1", "nodeName": "n1", "createdAt": "2026-11-18T10:00:00Z", "status": "successful"}}, data["snapshots"])
	assert.NotContains(t, strings.Join(warnings, "\n"), "may not be readable yet", "the etcd snapshot objects were read")

	puts := s.putsOf()
	require.Len(t, puts, 2)
	assert.True(t, puts[0].dryRun)
	assert.False(t, puts[1].dryRun)
	for _, put := range puts {
		assert.Equal(t, "cli.rancher.io", put.fieldManager)
		field := put.body["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotCreate"]
		assert.Equal(t, map[string]any{"generation": float64(1)}, field)
	}
}

// TestSaveEtcdSnapshotCompletionReadsNoSecret asserts the TAB callback for
// the <cluster> offers the cluster with spec.rkeConfig from one Steve request
// and reads no Secret.
func TestSaveEtcdSnapshotCompletionReadsNoSecret(t *testing.T) {
	s, srv := saveEnv(t)
	completionFixture(t, srv.URL, true)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	s.take()
	got := runCompletion(t, []string{"cluster", "save-etcd-snapshot"}, "")
	assert.Equal(t, []string{"prod"}, got)
	assert.Equal(t, []string{"GET " + saveList}, s.take())
}

// TestUpgradeRefusalHintIsACommandOfTheSchema asserts the refusal of cluster
// upgrade while an etcd snapshot is in progress has a hint that is a command
// of the schema manifest, with only flags that command declares, and that
// running the hint previews the waiting run.
func TestUpgradeRefusalHintIsACommandOfTheSchema(t *testing.T) {
	s, _ := saveEnv(t)
	s.mu.Lock()
	s.cluster["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotCreate"] = map[string]any{"generation": 1}
	s.mu.Unlock()
	_, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "upgrade", "prod", "--k8s-version", "v1.31.5+rke2r1", "--dry-run", "-o", "json"})
	require.NotZero(t, code)
	refusal := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeGeneric, refusal.Code)
	require.NotEmpty(t, refusal.Hint)

	words := strings.Fields(refusal.Hint)
	require.Equal(t, "rancher", words[0], refusal.Hint)
	words = words[1:]
	if len(words) > 1 && words[0] == "--config" {
		words = words[2:]
	}
	for i := range words {
		words[i] = strings.Trim(words[i], "'")
	}
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	var command runtime.Command
	for _, c := range wr.reg.Commands() {
		name := strings.Fields(c.Describe().Name)
		if len(words) >= len(name) && slices.Equal(name, words[:len(name)]) && (command == nil || len(name) > len(strings.Fields(command.Describe().Name))) {
			command = c
		}
	}
	require.NotNil(t, command, "the hint names no command of the schema: %s", refusal.Hint)
	assert.Equal(t, "cluster save-etcd-snapshot", command.Describe().Name)
	declared := declaredFlags(command)
	for _, word := range words {
		if flag, ok := strings.CutPrefix(word, "--"); ok && flag != "dry-run" {
			assert.Contains(t, declared, flag, "the hint has a flag the command does not declare: %s", refusal.Hint)
		}
	}
	assert.Equal(t, "--dry-run", words[len(words)-1])

	stdout, stderr, code := runBounded(t, newRoot(t), slices.Concat(words, []string{"-o", "json"}))
	require.Zero(t, code, stderr)
	data, warnings := saveEnvelope(t, stdout)
	assert.Equal(t, false, data["reversible"])
	assert.True(t, slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "an etcd snapshot of cluster prod is in progress") }), "%v", warnings)
}
