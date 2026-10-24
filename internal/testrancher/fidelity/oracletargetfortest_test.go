package fidelity

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTB is a testing.TB that keeps the cleanup functions and the
// reported errors instead of acting on them, so a test can run the cleanups
// itself and observe a failure without failing. Only the methods
// NewOracleTargetForTest calls are implemented; any other call panics on the
// nil embedded interface.
type recordingTB struct {
	testing.TB
	cleanups []func()
	errors   []string
	fatals   []string
}

type recordedFatal struct{}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
	panic(recordedFatal{})
}

func (r *recordingTB) runCleanups() {
	for _, cleanup := range slices.Backward(r.cleanups) {
		cleanup()
	}
}

// oracleCleanupServer serves a TLS oracle that lists the standard user until
// it receives that user's DELETE, then lists none, and has no member user and
// no bindings. The DELETE answers deleteStatus; only a 200 removes the user.
// It sets every variable NewOracleTargetFromEnv reads, so an ambient value
// never reaches a real oracle.
func oracleCleanupServer(t *testing.T, deleteStatus int) *atomic.Int32 {
	t.Helper()
	var deletes atomic.Int32
	var removed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("username") == standardUsername && !removed.Load() {
			_, _ = w.Write([]byte(`{"data":[{"id":"u-std","username":"e2e-fid-standard"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("DELETE /v3/users/u-std", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		if deleteStatus == http.StatusOK {
			removed.Store(true)
		}
		w.WriteHeader(deleteStatus)
	})
	empty := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	mux.HandleFunc("GET /v3/globalrolebindings", empty)
	mux.HandleFunc("GET /v3/projectroletemplatebindings", empty)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("E2E_FIDELITY_URL", srv.URL)
	t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
	t.Setenv("E2E_FIDELITY_CACERT", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	t.Setenv("E2E_FIDELITY_DOWNSTREAM", "")
	return &deletes
}

// TestNewOracleTargetForTestRemovesTheUsersAtTheEndOfTheTest asserts the
// helper removes the provisioned user when the test that built the target
// ends, and not before.
func TestNewOracleTargetForTestRemovesTheUsersAtTheEndOfTheTest(t *testing.T) {
	deletes := oracleCleanupServer(t, http.StatusOK)

	t.Run("test using the target", func(t *testing.T) {
		tgt := NewOracleTargetForTest(t)
		require.NotNil(t, tgt)
		assert.Zero(t, deletes.Load(), "nothing is deleted while the test runs")
	})

	assert.Equal(t, int32(1), deletes.Load(), "the standard user is deleted when the test ends")
}

// TestNewOracleTargetForTestReportsACleanupFailure asserts a user the server
// refuses to delete is reported as a test error, not dropped.
func TestNewOracleTargetForTestReportsACleanupFailure(t *testing.T) {
	deletes := oracleCleanupServer(t, http.StatusInternalServerError)

	rec := &recordingTB{}
	require.NotNil(t, NewOracleTargetForTest(rec))
	assert.Empty(t, rec.errors, "nothing is reported before the cleanups run")
	require.Len(t, rec.cleanups, 1)

	rec.runCleanups()
	assert.Equal(t, int32(1), deletes.Load())
	require.Len(t, rec.errors, 1)
	assert.Contains(t, rec.errors[0], "cleaning up e2e-fid-standard")
	assert.Contains(t, rec.errors[0], "status 500")
	assert.Empty(t, rec.fatals)
}

// TestNewOracleTargetForTestFailsOnAnIncompleteEnvironment asserts an
// environment that cannot build a target stops the test and registers no
// cleanup.
func TestNewOracleTargetForTestFailsOnAnIncompleteEnvironment(t *testing.T) {
	t.Setenv("E2E_FIDELITY_URL", "")
	t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "")
	t.Setenv("E2E_FIDELITY_CACERT", "")
	t.Setenv("E2E_FIDELITY_DOWNSTREAM", "")

	rec := &recordingTB{}
	require.Panics(t, func() { NewOracleTargetForTest(rec) }, "Fatalf stops the test")
	require.Len(t, rec.fatals, 1)
	assert.Contains(t, rec.fatals[0], "E2E_FIDELITY_URL")
	assert.Empty(t, rec.cleanups)
}

// TestOracleCleanupTimeoutCoversEveryWait asserts the cleanup bound is at
// least the sum of the waits Cleanup can spend on its two users.
func TestOracleCleanupTimeoutCoversEveryWait(t *testing.T) {
	assert.Greater(t, oracleCleanupTimeout(), 2*(standardUserGoneTimeout+bindingsGoneTimeout))
}
