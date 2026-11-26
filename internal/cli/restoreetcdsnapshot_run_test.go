package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
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
	restoreSnapshotPath  = savePrefix + "rke.cattle.io/v1/namespaces/fleet-default/etcdsnapshots/s1"
	restoreSnapshotsList = "/v1/rke.cattle.io.etcdsnapshots/fleet-default"
	restoreCreated       = "2026-01-01T00:00:00Z"
	restoreTaken         = "2026-06-01T00:00:00Z"
)

// restoreMetadata is snapshotFile.metadata of a snapshot of a cluster spec.
func restoreMetadata(t *testing.T, spec string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write([]byte(spec))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	outer, err := json.Marshal(map[string]string{"provisioning-cluster-spec": base64.StdEncoding.EncodeToString(buf.Bytes())})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(outer)
}

// restoreServer serves a provisioned cluster whose spec.rkeConfig names a cloud
// credential, its RKEControlPlane, its ready management Cluster and one etcd
// snapshot object s1 stored in S3, and records every request. A request whose
// path has a secrets segment fails the test. A PUT that is not a dry run makes
// Rancher finish the request: the RKEControlPlane's status copy takes the
// written request and its phase is Finished.
type restoreServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	puts     []savePut
	cluster  map[string]any
	plane    map[string]any
	snapshot map[string]any
}

func newRestoreServer(t *testing.T, metadata string) (*restoreServer, *httptest.Server) {
	t.Helper()
	s := &restoreServer{t: t,
		cluster: map[string]any{
			"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "prod", "namespace": "fleet-default", "resourceVersion": "7", "generation": 1, "creationTimestamp": restoreCreated},
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
		snapshot: map[string]any{
			"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
			"metadata": map[string]any{"name": "s1", "namespace": "fleet-default", "labels": map[string]any{"rke.cattle.io/cluster-name": "prod"}},
			"spec":     map[string]any{"clusterName": "prod"},
			"snapshotFile": map[string]any{
				"name": "s1.zip", "status": "successful", "createdAt": restoreTaken, "metadata": metadata,
				"s3": map[string]any{"bucket": "backups", "cloudCredentialName": "cattle-global-data:cc-1"},
			},
		},
	}
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *restoreServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			request, _ := body["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotRestore"].(map[string]any)
			s.plane["status"] = map[string]any{"etcdSnapshotRestore": request, "etcdSnapshotRestorePhase": "Finished"}
		}
		reply(body)
	case r.Method == http.MethodGet && r.URL.Path == savePlane:
		reply(s.plane)
	case r.Method == http.MethodGet && r.URL.Path == saveMgmt:
		reply(map[string]any{
			"id": "c-m-prod1", "type": "management.cattle.io.cluster",
			"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
			"metadata": map[string]any{"name": "c-m-prod1", "resourceVersion": "9"},
			"status":   map[string]any{"driver": "rke2", "conditions": []any{map[string]any{"type": "Connected", "status": "True"}}},
		})
	case r.Method == http.MethodGet && r.URL.Path == restoreSnapshotPath:
		reply(s.snapshot)
	case r.Method == http.MethodGet && r.URL.Path == saveSnaps:
		reply(map[string]any{"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshotList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{s.snapshot}})
	case r.Method == http.MethodGet && r.URL.Path == restoreSnapshotsList:
		reply(map[string]any{"data": []any{s.snapshot}})
	case r.Method == http.MethodGet && r.URL.Path == saveList:
		reply(map[string]any{"data": []any{
			map[string]any{"metadata": map[string]any{"name": "prod"}, "spec": s.cluster["spec"]},
			map[string]any{"metadata": map[string]any{"name": "edge"}, "spec": map[string]any{}},
		}})
	default:
		http.NotFound(w, r)
	}
}

func (s *restoreServer) putsOf() []savePut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.puts)
}

func (s *restoreServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.requests
	s.requests = nil
	return out
}

// restoreEnv points the CLI at the server. Not for a parallel test: it sets
// the process environment.
func restoreEnv(t *testing.T) (*restoreServer, *httptest.Server) {
	t.Helper()
	restoreRawArgv(t)
	extraArgsEnv(t)
	s, srv := newRestoreServer(t, restoreMetadata(t, `{"kubernetesVersion":"v1.31.4+rke2r1","rkeConfig":{}}`))
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return s, srv
}

var restoreEtcdSnapshotArgv = []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "s1"}

// TestRestoreEtcdSnapshotPreviewReportsNotReversible asserts the preview of
// the real command run is a server dry-run with reversible false and no write,
// and that it reads no Secret although the cluster and the etcd snapshot
// object name a cloud credential.
func TestRestoreEtcdSnapshotPreviewReportsNotReversible(t *testing.T) {
	s, _ := restoreEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), slices.Concat(restoreEtcdSnapshotArgv, []string{"--dry-run", "-o", "json"}))
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

// TestRestoreEtcdSnapshotNeedsApplyAndYes asserts --apply without --yes is
// CONFIRMATION_REQUIRED and sends no update.
func TestRestoreEtcdSnapshotNeedsApplyAndYes(t *testing.T) {
	s, _ := restoreEnv(t)
	_, stderr, code := runBounded(t, newRoot(t), slices.Concat(restoreEtcdSnapshotArgv, []string{"--apply", "-o", "json"}))
	assert.NotZero(t, code)
	assert.Equal(t, clierr.CodeConfirmationRequired, renderedError(t, "json", stderr).Code)
	for _, put := range s.putsOf() {
		assert.True(t, put.dryRun, "no write without --yes")
	}
}

// TestRestoreEtcdSnapshotAppliedRun asserts the applied run of the real
// command writes once, sends the field manager on every PUT, writes the
// request the Plan built, waits until the status copy and phase say done and
// reads no Secret.
func TestRestoreEtcdSnapshotAppliedRun(t *testing.T) {
	s, _ := restoreEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), slices.Concat(restoreEtcdSnapshotArgv, []string{"--restore-mode", "kubernetesVersion", "--apply", "--yes", "-o", "json"}))
	require.Zero(t, code, stderr)
	data, _ := saveEnvelope(t, stdout)
	assert.Equal(t, true, data["waited"])
	assert.Equal(t, true, data["written"])
	assert.Equal(t, "spec.rkeConfig.etcdSnapshotRestore", data["field"])
	assert.Equal(t, float64(1), data["generation"])
	assert.Equal(t, "Finished", data["phase"])
	assert.Equal(t, "c-m-prod1", data["clusterId"])
	assert.Equal(t, "s1", data["snapshot"])
	assert.Equal(t, "kubernetesVersion", data["restoreMode"])

	puts := s.putsOf()
	require.Len(t, puts, 2)
	assert.True(t, puts[0].dryRun)
	assert.False(t, puts[1].dryRun)
	for _, put := range puts {
		assert.Equal(t, "cli.rancher.io", put.fieldManager)
		field := put.body["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotRestore"]
		assert.Equal(t, map[string]any{"generation": float64(1), "name": "s1", "restoreRKEConfig": "kubernetesVersion"}, field)
	}
}

// TestRestoreEtcdSnapshotRefusesBeforeAnyRequest asserts a missing --snapshot,
// a --restore-mode outside the three values and --wait=false with --timeout
// are VALIDATION refusals with no request.
func TestRestoreEtcdSnapshotRefusesBeforeAnyRequest(t *testing.T) {
	s, _ := restoreEnv(t)
	rows := map[string][]string{
		"no snapshot":        {"cluster", "restore-etcd-snapshot", "prod", "--dry-run", "-o", "json"},
		"unknown mode":       slices.Concat(restoreEtcdSnapshotArgv, []string{"--restore-mode", "bogus", "--dry-run", "-o", "json"}),
		"wait false timeout": slices.Concat(restoreEtcdSnapshotArgv, []string{"--wait=false", "--timeout", "5m", "--dry-run", "-o", "json"}),
	}
	for name, argv := range rows {
		s.take()
		_, stderr, code := runBounded(t, newRoot(t), argv)
		assert.NotZero(t, code, name)
		assert.Equal(t, clierr.CodeValidation, renderedError(t, "json", stderr).Code, name)
		assert.Empty(t, s.take(), name+": no request")
	}
}

// TestRestoreEtcdSnapshotNotFoundReadsNoSecret asserts the refusal for a name
// with no etcd snapshot object lists the etcd snapshot objects and reads no
// Secret.
func TestRestoreEtcdSnapshotNotFoundReadsNoSecret(t *testing.T) {
	s, _ := restoreEnv(t)
	_, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "missing", "--dry-run", "-o", "json"})
	require.NotZero(t, code)
	assert.Equal(t, clierr.CodeNotFound, renderedError(t, "json", stderr).Code)
	assert.Contains(t, s.take(), "GET "+saveSnaps)
}

// hintCommand finds the command of the schema that a hint names, with its
// words after "rancher".
func hintCommand(t *testing.T, hint string) (runtime.Command, []string) {
	t.Helper()
	words := strings.Fields(hint)
	require.Equal(t, "rancher", words[0], hint)
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
	require.NotNil(t, command, "the hint names no command of the schema: %s", hint)
	return command, words
}

// TestRunningRestoreRefusalHintsAreCommandsOfTheSchema asserts the refusal of
// the restore of another snapshot, of cluster save-etcd-snapshot and of
// cluster upgrade while a restore is in progress has a hint that is the
// restore command of the schema manifest, with only flags the command
// declares, ending --dry-run, that the refusal is not retryable and advises no
// run with --apply --yes to wait, and that running the hint previews the
// waiting run with the phase.
func TestRunningRestoreRefusalHintsAreCommandsOfTheSchema(t *testing.T) {
	rows := map[string][]string{
		"restore of another snapshot": {"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "s2", "--dry-run", "-o", "json"},
		"save":                        {"cluster", "save-etcd-snapshot", "prod", "--dry-run", "-o", "json"},
		"upgrade":                     {"cluster", "upgrade", "prod", "--k8s-version", "v1.31.5+rke2r1", "--dry-run", "-o", "json"},
	}
	for name, argv := range rows {
		t.Run(name, func(t *testing.T) {
			s, _ := restoreEnv(t)
			s.mu.Lock()
			s.cluster["spec"].(map[string]any)["rkeConfig"].(map[string]any)["etcdSnapshotRestore"] = map[string]any{"name": "s1", "generation": 1, "restoreRKEConfig": "all"}
			s.mu.Unlock()
			_, stderr, code := runBounded(t, newRoot(t), argv)
			require.NotZero(t, code)
			refusal := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeGeneric, refusal.Code)
			var retry struct {
				Data struct {
					Error struct {
						Retryable bool `json:"retryable"`
					} `json:"error"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal([]byte(stderr), &retry), "stderr: %s", stderr)
			assert.False(t, retry.Data.Error.Retryable)
			assert.NotContains(t, refusal.Message, "to wait for it")
			require.NotEmpty(t, refusal.Hint)
			command, words := hintCommand(t, refusal.Hint)
			assert.Equal(t, "cluster restore-etcd-snapshot", command.Describe().Name)
			declared := declaredFlags(command)
			for _, word := range words {
				if flag, ok := strings.CutPrefix(word, "--"); ok && flag != "dry-run" {
					assert.Contains(t, declared, flag, "the hint has a flag the command does not declare: %s", refusal.Hint)
				}
			}
			assert.Equal(t, "--dry-run", words[len(words)-1])
			assert.Equal(t, []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "s1", "--restore-mode", "all", "--dry-run"}, words)

			stdout, stderr, code := runBounded(t, newRoot(t), slices.Concat(words, []string{"-o", "json"}))
			require.Zero(t, code, stderr)
			data, warnings := saveEnvelope(t, stdout)
			assert.Equal(t, false, data["reversible"])
			assert.True(t, slices.ContainsFunc(warnings, func(w string) bool {
				return strings.Contains(w, "an etcd snapshot restore of cluster prod is in progress (phase ")
			}), "%v", warnings)
		})
	}
}

// TestRestoreEtcdSnapshotCompletionReadsNoSecret asserts the TAB callback of
// each of the three sites offers only what the command accepts, with the
// requests it needs and no read of a Secret: the <cluster> from one Steve
// request, --snapshot from the provisioning Cluster and one Steve page of etcd
// snapshot objects, --restore-mode from the provisioning Cluster and the etcd
// snapshot object.
func TestRestoreEtcdSnapshotCompletionReadsNoSecret(t *testing.T) {
	s, srv := restoreEnv(t)
	completionFixture(t, srv.URL, true)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	s.take()
	got := runCompletion(t, []string{"cluster", "restore-etcd-snapshot"}, "")
	assert.Equal(t, []string{"prod"}, got)
	assert.Equal(t, []string{"GET " + saveList}, s.take())

	got = runCompletion(t, []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot"}, "")
	assert.Equal(t, []string{"s1"}, got)
	assert.Equal(t, []string{"GET " + saveCluster, "GET " + restoreSnapshotsList}, s.take())

	got = runCompletion(t, []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "s1", "--restore-mode"}, "")
	assert.Equal(t, []string{"all", "kubernetesVersion", "none"}, got)
	assert.Equal(t, []string{"GET " + saveCluster, "GET " + restoreSnapshotPath}, s.take())

	got = runCompletion(t, []string{"cluster", "restore-etcd-snapshot", "--restore-mode"}, "")
	assert.Empty(t, got, "no snapshot is typed")
	assert.Empty(t, s.take())
}

// TestRestoreModeOffersNothingWithTheNetworkOffOrTheReadRefused asserts --restore-mode
// offers nothing, never the static list of its three values, with the network
// off and with the server refusing the read.
func TestRestoreModeOffersNothingWithTheNetworkOffOrTheReadRefused(t *testing.T) {
	words := []string{"cluster", "restore-etcd-snapshot", "prod", "--snapshot", "s1", "--restore-mode"}
	t.Run("network off", func(t *testing.T) {
		s, srv := restoreEnv(t)
		completionFixture(t, srv.URL, false)
		t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
		s.take()
		assert.Empty(t, runCompletion(t, words, ""))
		assert.Empty(t, s.take(), "no request")
	})
	t.Run("read refused", func(t *testing.T) {
		restoreRawArgv(t)
		extraArgsEnv(t)
		refusing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		}))
		t.Cleanup(refusing.Close)
		completionFixture(t, refusing.URL, true)
		t.Setenv(config.EnvCACert, string(pemFromServer(t, refusing)))
		assert.Empty(t, runCompletion(t, words, ""))
	})
}
