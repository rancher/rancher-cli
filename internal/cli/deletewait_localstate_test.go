package cli

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// lsSecret is the secret half of every bearer the store holds; no
	// output may carry it.
	lsSecret = "secret"

	lsManagementTokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"
	lsExtTokensPath        = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

	// lsUnauthorizedBody is the Kubernetes Status Rancher returns for a
	// request whose bearer it does not accept.
	lsUnauthorizedBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`
)

// extTokenDeletedBearerStatus and extTokenDeletedBearerBody are Rancher's
// response on the ext tokens path to a request whose bearer is an ext token
// that was just deleted: the same 401 a bearer that was never issued gets.
const (
	extTokenDeletedBearerStatus = http.StatusUnauthorized
	extTokenDeletedBearerBody   = lsUnauthorizedBody
)

// lsStore is a credential store that serves one bearer, counts the reads
// of it, and fails the test on any write or delete.
type lsStore struct {
	t      *testing.T
	token  string
	mu     sync.Mutex
	reads  int
	writes []string
}

func (s *lsStore) wrote(method string) {
	s.mu.Lock()
	s.writes = append(s.writes, method)
	s.mu.Unlock()
	s.t.Errorf("the credential store was written: %s", method)
}

func (s *lsStore) GetToken(config.ServerRef) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.token, nil
}
func (s *lsStore) SetToken(config.ServerRef, string) error { s.wrote("SetToken"); return nil }
func (s *lsStore) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	s.wrote("SetTokenWithOutcome")
	return config.CommitOutcome{}, nil
}
func (s *lsStore) DeleteToken(config.ServerRef) error { s.wrote("DeleteToken"); return nil }
func (s *lsStore) GetCachedCred(string, *config.CacheAnchor) (string, time.Time, error) {
	return "", time.Time{}, config.ErrNoToken
}
func (s *lsStore) GetCachedCredDetail(string, *config.CacheAnchor) (config.CachedCredDetail, error) {
	return config.CachedCredDetail{}, config.ErrNoToken
}
func (s *lsStore) MarkCachedCredReady(string, string, string) (bool, error) {
	s.wrote("MarkCachedCredReady")
	return false, nil
}
func (s *lsStore) SetCachedCred(string, string, time.Time, string, string, string, config.CacheBinding) error {
	s.wrote("SetCachedCred")
	return nil
}
func (s *lsStore) DeleteAllCachedCreds() error                       { s.wrote("DeleteAllCachedCreds"); return nil }
func (s *lsStore) ListCachedCreds() ([]config.CachedCredInfo, error) { return nil, nil }
func (s *lsStore) HoldCachedCreds(func([]config.CachedRecord) error) error {
	s.wrote("HoldCachedCreds")
	return nil
}
func (s *lsStore) DeleteCachedCredsIfUnchanged(map[string]string) ([]string, []string, error) {
	s.wrote("DeleteCachedCredsIfUnchanged")
	return nil, nil, nil
}
func (s *lsStore) ResolvedBackend() (string, bool) { return "", true }

var _ config.CredStore = (*lsStore)(nil)

// lsServer responds as Rancher does around the delete of one token: the
// token to a GET, 200 to the dry-run and the real delete, and after the
// real delete status and body to a request on afterPath and 401 to every
// other request. It records every request.
type lsServer struct {
	tokenPath string // the token's path, without a query
	object    string
	afterPath string
	status    int
	body      string

	mu      sync.Mutex
	reqs    []string
	deleted bool
}

func (s *lsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A delete carries its DeleteOptions, the dry run among them, in the
	// body.
	body, _ := io.ReadAll(r.Body)
	var opts struct {
		DryRun []string `json:"dryRun"`
	}
	_ = json.Unmarshal(body, &opts)
	dryRun := len(opts.DryRun) > 0 || r.URL.Query().Get("dryRun") != ""
	s.mu.Lock()
	line := r.Method + " " + r.URL.Path
	if dryRun {
		line += " (dry run)"
	}
	s.reqs = append(s.reqs, line)
	deleted := s.deleted
	if r.Method == http.MethodDelete && r.URL.Path == s.tokenPath && !dryRun {
		s.deleted = true
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case deleted && strings.HasPrefix(r.URL.Path, s.afterPath):
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	case deleted:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(lsUnauthorizedBody))
	case r.URL.Path == "/k8s/clusters/local/apis":
		_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"ext.cattle.io","versions":[{"groupVersion":"ext.cattle.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ext.cattle.io/v1","version":"v1"}}]}`))
	case r.URL.Path == s.tokenPath && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(s.object))
	case r.URL.Path == s.tokenPath && r.Method == http.MethodDelete:
		_, _ = w.Write([]byte(s.object))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","code":404}`))
	}
}

func (s *lsServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// afterDelete is the requests after the real delete of the token.
func (s *lsServer) afterDelete(t *testing.T) []string {
	t.Helper()
	reqs := s.requests()
	for i, r := range reqs {
		if r == http.MethodDelete+" "+s.tokenPath {
			return reqs[i+1:]
		}
	}
	t.Fatalf("no delete of %s in %v", s.tokenPath, reqs)
	return nil
}

// lsTree is every file and directory under dir with each file's bytes.
func lsTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(b)
		return nil
	}))
	return out
}

// lsRun is one run of a delete through the pipeline with the real client
// factory.
type lsRun struct {
	status         int
	ce             *clierr.Error
	stdout, stderr string
	store          *lsStore
}

func runLocalStateDelete(t *testing.T, srv *lsServer, bearer, command string, in fieldCoverageInput) lsRun {
	t.Helper()
	ts := httptest.NewTLSServer(srv)
	t.Cleanup(ts.Close)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: "acme", Servers: map[string]config.Server{"acme": {URL: ts.URL, CA: string(certPEM)}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	before := lsTree(t, dir)

	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var e *entry
	for _, candidate := range buildEntries() {
		if candidate.make(runDeps{}).Describe().Name == command {
			e = &candidate
		}
	}
	require.NotNil(t, e, "no entry builds %s", command)

	store := &lsStore{t: t, token: bearer}
	f := newClientFactory(dir, store)
	deps := runDeps{clients: f, roots: f, actions: f, serverBase: f, store: store, dir: dir, sessionNamer: f}
	var out, errOut bytes.Buffer
	status, ce := runtime.Run(t.Context(), e.make(deps), in, runtime.IO{
		Out: &out, Err: &errOut, Format: "json",
		Apply: true, Yes: true, AllowDeprecated: true,
		Capability: scrubAllowAll{}, Projects: scrubProjects{},
	}, wr.reg.Binding(command))

	assert.GreaterOrEqual(t, store.reads, 1, "the command read its token")
	assert.Empty(t, store.writes, "the credential store is not written")
	assert.Equal(t, before, lsTree(t, dir), "the config directory is unchanged")
	assert.NotContains(t, out.String(), lsSecret, "stdout never holds the secret")
	assert.NotContains(t, errOut.String(), lsSecret, "stderr never holds the secret")
	return lsRun{status: status, ce: ce, stdout: out.String(), stderr: errOut.String(), store: store}
}

// lsEnvelope is the part of the JSON envelope the rows read.
type lsEnvelope struct {
	Data struct {
		Deleted string `json:"deleted"`
		Waited  *bool  `json:"waited"`
	} `json:"data"`
	Warnings []string `json:"warnings"`
}

// TestDeleteOfTheCommandsOwnDerivedTokenLeavesLocalStateAlone asserts that
// the delete of the token the command uses, when Rancher marks it as
// derived, sends the read, the dry-run delete and the delete and nothing
// after, with the wait on or off; says the deletion started with the note
// to log in again; and changes no local state: no credential store write,
// the config directory unchanged.
func TestDeleteOfTheCommandsOwnDerivedTokenLeavesLocalStateAlone(t *testing.T) {
	t.Parallel()
	const name = "my-token-abc"
	for _, tc := range []struct {
		name  string
		bools map[string]bool
	}{
		{"default", nil},
		{"--wait=false", map[string]bool{"wait": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &lsServer{
				tokenPath: lsManagementTokensPath + "/" + name,
				object:    `{"apiVersion":"management.cattle.io/v3","kind":"Token","metadata":{"name":"` + name + `","resourceVersion":"7","uid":"u-tok"},"isDerived":true,"userId":"u-1"}`,
				// The tokens path responds to a deleted bearer with 403 and an
				// empty body; every other path responds with 401.
				afterPath: lsManagementTokensPath, status: http.StatusForbidden, body: "",
			}
			run := runLocalStateDelete(t, srv, name+":"+lsSecret, "delete management-token", fieldCoverageInput{args: []string{name}, bools: tc.bools})
			require.Nil(t, run.ce, "%v", run.ce)
			assert.Equal(t, 0, run.status)
			var env lsEnvelope
			require.NoError(t, json.Unmarshal([]byte(run.stdout), &env))
			assert.Equal(t, name, env.Data.Deleted)
			require.NotNil(t, env.Data.Waited)
			assert.False(t, *env.Data.Waited)
			assert.Equal(t, []string{dwOwnTokenNote}, env.Warnings)
			assert.Equal(t, []string{
				"GET " + srv.tokenPath,
				"DELETE " + srv.tokenPath + " (dry run)",
				"DELETE " + srv.tokenPath,
			}, srv.requests(), "the read, the dry-run delete and the delete, and nothing after")
		})
	}
}

// TestDeleteOfATokenEndingInAnAuthenticationFailureLeavesLocalStateAlone
// asserts that a delete whose wait ends at an authentication failure ends
// with AUTH and the committed details, after one request that follows the
// delete, and writes nothing to the credential store or the config
// directory. The first row's server responds with 401 after the delete of a
// token that is not the token the command uses, which Rancher does not do;
// the row uses it to reach the authentication failure after a delete. The
// second row deletes the ext token the command uses; before the delete, the
// command does not check whether an ext token is the token it uses.
func TestDeleteOfATokenEndingInAnAuthenticationFailureLeavesLocalStateAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, token, bearer string
		status              int
		body                string
	}{
		{"another token", "other-token", "my-token-abc:" + lsSecret, http.StatusUnauthorized, lsUnauthorizedBody},
		{"the ext token the command uses", "token-own", "ext/token-own:" + lsSecret, extTokenDeletedBearerStatus, extTokenDeletedBearerBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &lsServer{
				tokenPath: lsExtTokensPath + "/" + tc.token,
				object:    `{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"` + tc.token + `","resourceVersion":"3","uid":"u-ext"},"spec":{"userID":"u-1"}}`,
				afterPath: lsExtTokensPath, status: tc.status, body: tc.body,
			}
			run := runLocalStateDelete(t, srv, tc.bearer, "delete token", fieldCoverageInput{args: []string{tc.token}})
			require.NotNil(t, run.ce)
			assert.Equal(t, clierr.CodeAuth, run.ce.Code)
			assert.Equal(t, 3, run.ce.ExitCode())
			details, ok := run.ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok, "committed details: %#v", run.ce.Details)
			assert.True(t, details.Committed)
			assert.Len(t, srv.afterDelete(t), 1, "one request after the delete")
		})
	}
}
