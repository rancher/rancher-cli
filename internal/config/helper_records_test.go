package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelperRecordsAddRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	e := HelperRecord{Helper: "vault", Name: "prod", CanonicalURL: "https://r.example.com"}
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		s.add(e)
		return nil
	}))
	// Re-read in a fresh lock scope: entry persisted.
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		got := s.forServer("prod")
		require.Len(t, got, 1)
		assert.Equal(t, e, got[0])
		s.remove(e)
		return nil
	}))
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"))
		return nil
	}))
}

func TestHelperRecordsConcurrentAddsLoseNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := HelperRecord{Helper: "vault", Name: fmt.Sprintf("s%d", i), CanonicalURL: "https://r.example.com"}
			_ = withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error { s.add(e); return nil })
		}(i)
	}
	wg.Wait()
	total := 0
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		for i := range 20 {
			total += len(s.forServer(fmt.Sprintf("s%d", i)))
		}
		return nil
	}))
	assert.Equal(t, 20, total, "no concurrent add may be lost")
}

// Cross-PROCESS lock tests: goroutine tests cannot prove flock semantics.
// The fake helper's lockhold mode (Task 7's main.go) acquires the flock at a
// caller-supplied path; the tests build it from helperRecordsLockName so the
// lock and prints "locked" AFTER acquisition; waitForLocked (fixtures file)
// blocks on that stdout line, so the parent can never race the child to the
// lock. Real compiled-binary runs — actual OS lock behavior on every
// platform including Windows LockFileEx.

func TestHelperRecordsLockCrossProcessContention(t *testing.T) {
	dir := t.TempDir()
	child := exec.Command(fakeHelperBin(t), "lockhold", filepath.Join(dir, helperRecordsLockName), "2s")
	waitForLocked(t, child) // child provably owns the lock now

	start := time.Now()
	err := withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		s.add(HelperRecord{Helper: "vault", Name: "p", CanonicalURL: "https://x"})
		return nil
	})
	require.NoError(t, err, "blocked acquisition must succeed once the child releases")
	assert.Greater(t, time.Since(start), 500*time.Millisecond, "must actually have waited on the child's lock")
}

func TestHelperRecordsLockBoundedWait(t *testing.T) {
	restore := SetExecHelperTimeoutForTest(200 * time.Millisecond) // wait bound = 2×
	defer restore()
	dir := t.TempDir()
	child := exec.Command(fakeHelperBin(t), "lockhold", filepath.Join(dir, helperRecordsLockName), "30s")
	waitForLocked(t, child)

	err := withHelperRecordsLock(t.Context(), dir, func(*helperRecords) error { return nil })
	require.Error(t, err, "acquisition must give up after the bounded wait")
	assert.ErrorContains(t, err, "another rancher process")
}

func TestHelperRecordsLockReleasedOnProcessDeath(t *testing.T) {
	dir := t.TempDir()
	child := exec.Command(fakeHelperBin(t), "lockhold", filepath.Join(dir, helperRecordsLockName), "30s")
	waitForLocked(t, child) // owns the lock…
	require.NoError(t, child.Process.Kill(), "…and dies holding it")
	_, _ = child.Process.Wait()

	err := withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error { return nil })
	assert.NoError(t, err, "OS must release a dead process's lock — no stale-lock recovery")
}

func TestExecStoreSetTokenWritesIntentBeforeStore(t *testing.T) {
	useFakeHelper(t, "fail") // store fails; intent must survive for a later delete
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	err = st.SetToken(execRef(), "id:s")
	require.Error(t, err)
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		require.Len(t, s.forServer("prod"), 1, "write-ahead intent must survive a failed store")
		return nil
	}))
}

func TestExecStoreSetTokenUnsupportedRemovesIntent(t *testing.T) {
	useFakeHelper(t, "unsupported")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	err = st.SetToken(execRef(), "id:s")
	assert.ErrorIs(t, err, ErrTokenStoreUnsupported)
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "unsupported store recorded nothing")
		return nil
	}))
}

func TestExecStoreUnsupportedPersistFailureIsHardError(t *testing.T) {
	// The unsupported sentinel may ONLY be returned once its intent removal
	// is durably persisted — otherwise login would warn-and-succeed while a
	// stale intent keeps a nothing-storing helper on every future delete.
	useFakeHelper(t, "unsupported")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	// Fail the SECOND persist (intent write succeeds, removal write fails).
	calls := 0
	restore := setHelperRecordsPersistForTest(func(dir string, s *helperRecords) error {
		calls++
		if calls >= 2 {
			return fmt.Errorf("disk full")
		}
		return persistHelperRecordsToDisk(dir, s)
	})
	defer restore()
	err = st.SetToken(execRef(), "id:s")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrTokenStoreUnsupported,
		"a hard persistence failure must not read as clean read-only success")
}

func TestExecStoreDeleteTokenClearsHelperRecord(t *testing.T) {
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:s"))
	require.NoError(t, st.DeleteToken(execRef()))
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "a direct DeleteToken clears its helper record")
		return nil
	}))
}

func TestExecStoreDeleteTokenFailureKeepsHelperRecord(t *testing.T) {
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:s"))
	t.Setenv("FAKEHELPER_MODE", "fail")
	require.Error(t, st.DeleteToken(execRef()))
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		require.Len(t, s.forServer("prod"), 1, "failed erase keeps the entry for retry")
		return nil
	}))
}

// The three spec-mandated safety boundaries:

func TestExecStoreFirstPersistFailureAbortsBeforeHelper(t *testing.T) {
	// Write-ahead contract: if the INTENT cannot be recorded, the helper is
	// never invoked — no remote credential without a cleanup record.
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	restore := setHelperRecordsPersistForTest(func(string, *helperRecords) error {
		return fmt.Errorf("disk full")
	})
	defer restore()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	err = st.SetToken(execRef(), "id:s")
	require.Error(t, err)
	assert.ErrorContains(t, err, "token NOT stored")
	// The fake helper logs every invocation; no log = never invoked.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr), "helper must never have been invoked")
}

// Deterministic interleaving tests: execStoreLockedHook fires while the
// helper-records lock is HELD, so the test can pin exactly which operation owns
// the lock first, start the competitor (which must block on the lock), then
// release — both orders proven, both operations required to succeed, exact
// final helper/record state asserted. Not parallel (package-level hook).

func TestExecStoreStoreThenEraseInterleaving(t *testing.T) {
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)

	inStore := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	execStoreLockedHook = func(op string) {
		if op == "store" {
			close(inStore) // announce: store owns the lock
			<-release      // hold it until the test says go
		}
	}
	t.Cleanup(func() { execStoreLockedHook = nil; unblock() }) // every failure path unblocks both goroutines

	storeErr := make(chan error, 1)
	eraseErr := make(chan error, 1)
	go func() { storeErr <- st.SetToken(execRef(), "id:s") }()
	select { // bounded: a store failing BEFORE the hook reports its error, never hangs
	case <-inStore: // store provably holds the lock now
	case err := <-storeErr:
		t.Fatalf("store finished before reaching the lock hook: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("store never reached the lock hook")
	}
	go func() { eraseErr <- st.DeleteToken(execRef()) }()
	unblock() // store completes; erase acquires next and runs after

	require.NoError(t, <-storeErr)
	require.NoError(t, <-eraseErr)
	assert.Empty(t, fakeHelperCreds(t, statePath), "store→erase: no credential survives")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "store→erase: no helper record survives")
		return nil
	}))
}

func TestExecStoreEraseThenStoreInterleaving(t *testing.T) {
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:old")) // pre-existing credential

	inErase := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	execStoreLockedHook = func(op string) {
		if op == "erase" {
			close(inErase) // announce: erase owns the lock
			<-release
		}
	}
	t.Cleanup(func() { execStoreLockedHook = nil; unblock() })

	eraseErr := make(chan error, 1)
	storeErr := make(chan error, 1)
	go func() { eraseErr <- st.DeleteToken(execRef()) }()
	select {
	case <-inErase: // erase provably holds the lock now
	case err := <-eraseErr:
		t.Fatalf("erase finished before reaching the lock hook: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("erase never reached the lock hook")
	}
	go func() { storeErr <- st.SetToken(execRef(), "id:new") }()
	unblock() // erase completes; store acquires next and runs after

	require.NoError(t, <-eraseErr)
	require.NoError(t, <-storeErr)
	creds := fakeHelperCreds(t, statePath)
	require.Len(t, creds, 1, "erase→store: the re-stored credential is live")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		require.Len(t, s.forServer("prod"), 1,
			"erase→store: the live credential has its helper record")
		return nil
	}))
}
