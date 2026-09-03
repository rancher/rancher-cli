package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// keyringRead is one scripted answer to Get.
type keyringRead struct {
	value string
	err   error
}

// fakeKeyring drives every keyring failure shape the store must classify.
// Reads are scripted per call so a test can make the pre-write read fail
// and the post-error read-back succeed, which one persistent error cannot
// express; when the script is exhausted Get answers from items.
type fakeKeyring struct {
	items         map[string]string
	reads         []keyringRead
	setErr        error
	commitThenErr bool
}

func (f *fakeKeyring) Set(service, user, password string) error {
	if f.setErr != nil && !f.commitThenErr {
		return f.setErr
	}
	f.items[user] = password
	return f.setErr
}

func (f *fakeKeyring) Get(service, user string) (string, error) {
	if len(f.reads) > 0 {
		r := f.reads[0]
		f.reads = f.reads[1:]
		return r.value, r.err
	}
	v, ok := f.items[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Delete(service, user string) error { delete(f.items, user); return nil }

// withFakeKeyring swaps the package-global seam; the caller must not run in
// parallel with any other test in this package.
func withFakeKeyring(t *testing.T, fk *fakeKeyring) {
	t.Helper()
	keyring.MockInit()
	prev := keyringBackend
	keyringBackend = fk
	t.Cleanup(func() { keyringBackend = prev })
}

// TestKeyringSetTokenWithOutcomeClassifies asserts the four normalized
// keyring outcomes and the prior-source table.
func TestKeyringSetTokenWithOutcomeClassifies(t *testing.T) {
	ref := ServerRef{Name: "prod", URL: "https://r.example"}
	boom := errors.New("dbus: reply lost")
	locked := errors.New("keyring locked")
	cases := []struct {
		name       string
		fk         *fakeKeyring
		fileToken  string
		wantState  clierr.CredentialState
		wantPrior  clierr.PriorSource
		wantErr    bool
		wantNotice bool
		wantComp   bool
	}{
		{"error before write, no prior", &fakeKeyring{items: map[string]string{}, setErr: boom}, "", clierr.CredentialNotWritten, clierr.PriorNone, true, false, false},
		{"commit then error, prior in keyring", &fakeKeyring{items: map[string]string{"prod": "old"}, setErr: boom, commitThenErr: true}, "", clierr.CredentialCommitted, clierr.PriorKeyring, false, true, true},
		{"commit then error, prior unreadable", &fakeKeyring{items: map[string]string{}, setErr: boom, commitThenErr: true, reads: []keyringRead{{err: locked}, {value: "new"}}}, "", clierr.CredentialCommittedUncompensatable, clierr.PriorUnknown, false, true, false},
		{"error before write, read-back unreachable", &fakeKeyring{items: map[string]string{}, setErr: boom, reads: []keyringRead{{err: keyring.ErrNotFound}, {err: locked}}}, "", clierr.CredentialPossiblyCommitted, clierr.PriorNone, true, false, false},
		{"commit then error, read-back shows a third value", &fakeKeyring{items: map[string]string{"prod": "old"}, setErr: boom, commitThenErr: true, reads: []keyringRead{{value: "old"}, {value: "other"}}}, "", clierr.CredentialPossiblyCommitted, clierr.PriorKeyring, true, false, false},
		{"success, prior in file only", &fakeKeyring{items: map[string]string{}}, "filetok", clierr.CredentialCommitted, clierr.PriorFile, false, false, true},
		{"success, prior unreadable", &fakeKeyring{items: map[string]string{}, reads: []keyringRead{{err: locked}}}, "", clierr.CredentialCommittedUncompensatable, clierr.PriorUnknown, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fs := newFileStore(dir, &noticeBuffer{})
			if tc.fileToken != "" {
				require.NoError(t, fs.SetToken(ref, tc.fileToken))
			}
			withFakeKeyring(t, tc.fk)
			ks := newPinnedKeyringStore(dir, false)
			out, err := ks.SetTokenWithOutcome(ref, "new")
			assert.Equal(t, clierr.BackendKeyring, out.Backend)
			assert.Equal(t, tc.wantState, out.State)
			assert.Equal(t, tc.wantPrior, out.PriorSource)
			assert.Equal(t, tc.wantErr, err != nil, "err=%v", err)
			assert.Equal(t, tc.wantNotice, out.Notice != "")
			assert.Equal(t, tc.wantComp, out.Compensate != nil)
		})
	}
}

// TestKeyringCompensationTouchesOnlyTheKeyring asserts the keyring-miss/
// file-hit case: compensation deletes the new keyring item and leaves the
// file entry untouched.
func TestKeyringCompensationTouchesOnlyTheKeyring(t *testing.T) {
	ref := ServerRef{Name: "prod", URL: "https://r.example"}
	dir := t.TempDir()
	fs := newFileStore(dir, &noticeBuffer{})
	require.NoError(t, fs.SetToken(ref, "old-file"))
	fk := &fakeKeyring{items: map[string]string{}}
	withFakeKeyring(t, fk)
	ks := newPinnedKeyringStore(dir, false)
	out, err := ks.SetTokenWithOutcome(ref, "new")
	require.NoError(t, err)
	require.Equal(t, clierr.PriorFile, out.PriorSource)
	require.NotNil(t, out.Compensate)
	require.NoError(t, out.Compensate())
	_, has := fk.items["prod"]
	assert.False(t, has, "the new keyring item is deleted")
	tok, err := fs.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "old-file", tok, "the file entry is untouched")
}

// TestFileSetTokenWithOutcomeUnreadablePriorIsNotWritten asserts the file
// store's refusal to write over a file it cannot parse: without the parsed
// map every other server's entry would be lost, so the outcome is
// notWritten with an unknown prior and the file's bytes are unchanged.
func TestFileSetTokenWithOutcomeUnreadablePriorIsNotWritten(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	fs := newFileStore(dir, &noticeBuffer{})
	ref := ServerRef{Name: "prod", URL: "https://r.example"}
	require.NoError(t, fs.SetToken(ref, "old"))
	path := fs.path
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	out, err := fs.SetTokenWithOutcome(ref, "new")
	require.Error(t, err)
	assert.Equal(t, clierr.BackendFile, out.Backend)
	assert.Equal(t, clierr.CredentialNotWritten, out.State)
	assert.Equal(t, clierr.PriorUnknown, out.PriorSource)
	assert.Nil(t, out.Compensate)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused write leaves the file untouched")
}

// TestExecSetTokenWithOutcomeBoundaries asserts the exec store's outcome at
// each boundary, the on-disk helper record, and the helper's own state
// file, which is what tells a commit-then-fail apart from a fail-before-
// store even though the protocol classifies both as possiblyCommitted.
func TestExecSetTokenWithOutcomeBoundaries(t *testing.T) {
	warmFakeHelper(t)
	restoreTimeout := SetExecHelperTimeoutForTest(500 * time.Millisecond)
	defer restoreTimeout()
	ref := execRef()
	cases := []struct {
		name          string
		mode          string
		wantState     clierr.CredentialState
		wantRetained  bool
		wantErr       bool
		wantUnsupport bool
		wantStored    bool // the fake helper's state file holds the credential
	}{
		{"helper commits then exits non-zero", "commit-fail", clierr.CredentialPossiblyCommitted, true, true, false, true},
		{"helper commits then answers garbage", "commit-garbage", clierr.CredentialPossiblyCommitted, true, true, false, true},
		{"helper commits then hangs", "commit-hang", clierr.CredentialPossiblyCommitted, true, true, false, true},
		{"helper exits non-zero before storing", "fail", clierr.CredentialPossiblyCommitted, true, true, false, false},
		{"helper reports unsupported", "unsupported", clierr.CredentialNotWritten, false, true, true, false},
		{"helper success", "ok", clierr.CredentialHelperNonAtomic, true, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statePath := useFakeHelper(t, tc.mode)
			dir := t.TempDir()
			st, err := OpenStore("exec:fake", dir)
			require.NoError(t, err)
			out, err := st.SetTokenWithOutcome(ref, "id:s")
			assert.Equal(t, tc.wantErr, err != nil, "err=%v", err)
			assert.Equal(t, tc.wantUnsupport, errors.Is(err, ErrTokenStoreUnsupported))
			assert.Equal(t, clierr.BackendExec, out.Backend)
			assert.Equal(t, tc.wantState, out.State)
			assert.Equal(t, clierr.PriorUnknown, out.PriorSource)
			assert.Equal(t, tc.wantRetained, out.HelperRecordRetained)
			assert.Nil(t, out.Compensate)
			require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
				if tc.wantRetained {
					assert.Len(t, s.forServer("prod"), 1)
				} else {
					assert.Empty(t, s.forServer("prod"))
				}
				return nil
			}))
			assert.Equal(t, tc.wantStored, fakeHelperHolds(t, statePath, ref), "helper state file")
		})
	}
}

// warmFakeHelper runs the freshly built helper binary once at the default
// timeout. The operating system's first exec of a new binary (loader,
// signature check) costs far more than a warm one, and a test that shrinks
// the timeout to sub-second must measure the helper's behavior rather than
// that cold start.
func warmFakeHelper(t *testing.T) {
	t.Helper()
	useFakeHelper(t, "notfound")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, _ = st.GetToken(execRef())
}

// fakeHelperHolds reports whether the fake helper's JSON state file holds
// a credential for ref (the file's shape is the fake's own; see
// testdata/fakehelper/main.go).
func fakeHelperHolds(t *testing.T, statePath string, ref ServerRef) bool {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	var st struct {
		Creds map[string]string `json:"creds"`
	}
	require.NoError(t, json.Unmarshal(b, &st))
	for k := range st.Creds {
		if k == ref.URL+"\n"+ref.Name {
			return true
		}
	}
	return false
}

// TestExecSetTokenWithOutcomeRecordFailures asserts the boundaries before
// and after the helper runs: a corrupt records file fails the lock/load
// step, a failed intent persist never invokes the helper, and a failed
// removal persist after an unsupported answer is a hard error rather than
// the unsupported sentinel.
func TestExecSetTokenWithOutcomeRecordFailures(t *testing.T) {
	ref := execRef()
	t.Run("corrupt records file", func(t *testing.T) {
		useFakeHelper(t, "ok")
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, helperRecordsFileName), []byte(":\n  - not yaml"), 0o600))
		st, err := OpenStore("exec:fake", dir)
		require.NoError(t, err)
		out, err := st.SetTokenWithOutcome(ref, "id:s")
		require.ErrorContains(t, err, "corrupt")
		assert.Equal(t, clierr.CredentialNotWritten, out.State)
		assert.False(t, out.HelperRecordRetained)
	})
	t.Run("intent persist fails", func(t *testing.T) {
		useFakeHelper(t, "ok")
		dir := t.TempDir()
		restore := setHelperRecordsPersistForTest(func(string, *helperRecords) error { return errors.New("disk full") })
		defer restore()
		st, err := OpenStore("exec:fake", dir)
		require.NoError(t, err)
		out, err := st.SetTokenWithOutcome(ref, "id:s")
		require.ErrorContains(t, err, "disk full")
		assert.Equal(t, clierr.CredentialNotWritten, out.State)
		assert.False(t, out.HelperRecordRetained)
	})
	t.Run("unsupported, removal persist fails", func(t *testing.T) {
		useFakeHelper(t, "unsupported")
		dir := t.TempDir()
		calls := 0
		restore := setHelperRecordsPersistForTest(func(dir string, s *helperRecords) error {
			calls++
			if calls >= 2 {
				return errors.New("disk full")
			}
			return persistHelperRecordsToDisk(dir, s)
		})
		defer restore()
		st, err := OpenStore("exec:fake", dir)
		require.NoError(t, err)
		out, err := st.SetTokenWithOutcome(ref, "id:s")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrTokenStoreUnsupported)
		assert.Equal(t, clierr.CredentialNotWritten, out.State)
		assert.True(t, out.HelperRecordRetained, "the stale intent is still on disk")
	})
}

// TestCommitTokenPassesBackendOutcomeThrough asserts that CommitToken never
// builds its own snapshot: the backend's outcome and compensation are
// returned as is, and GetToken is never called.
func TestCommitTokenPassesBackendOutcomeThrough(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	fs := newFileStore(dir, &noticeBuffer{})
	ref := ServerRef{Name: "prod", URL: "https://r.example"}
	require.NoError(t, fs.SetToken(ref, "old"))
	out, err := CommitToken(fs, ref, "new")
	require.NoError(t, err)
	assert.Equal(t, clierr.BackendFile, out.Backend)
	assert.Equal(t, clierr.CredentialCommitted, out.State)
	assert.Equal(t, clierr.PriorFile, out.PriorSource)
	require.NotNil(t, out.Compensate)
	require.NoError(t, out.Compensate())
	tok, err := fs.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "old", tok)
}
