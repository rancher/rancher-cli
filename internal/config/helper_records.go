package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// The helper-records file remembers which exec helpers were asked to store
// which credential identities, so a later delete can erase through every
// helper that may hold a token - regardless of what the credential-store
// policy says by then. Machine-local state (like the credstore pin), never
// portable config. Records are written BEFORE the helper store runs
// (write-ahead): a stale record after a failed store is harmless because
// erase is idempotent, while a missing record after a successful store
// would orphan a live credential.
const (
	// The on-disk names predate the plain-words rename and are kept so
	// existing installs keep their records.
	helperRecordsFileName = "credhelper-provenance.yaml"
	helperRecordsLockName = "credhelper-provenance.lock"
)

// HelperRecord is one recorded store intent: helper × credential identity.
type HelperRecord struct {
	Helper       string `json:"helper"`
	Name         string `json:"name"`
	CanonicalURL string `json:"canonicalURL"`
}

type helperRecords struct {
	Entries []HelperRecord `json:"entries"`
	dirty   bool
}

func (s *helperRecords) add(e HelperRecord) {
	if slices.Contains(s.Entries, e) {
		return
	}
	s.Entries = append(s.Entries, e)
	s.dirty = true
}

func (s *helperRecords) remove(e HelperRecord) {
	out := s.Entries[:0]
	for _, x := range s.Entries {
		if x != e {
			out = append(out, x)
		}
	}
	if len(out) != len(s.Entries) {
		s.dirty = true
	}
	s.Entries = out
}

// forServer returns the recorded entries for a config-entry name. Helper names
// are validated in withHelperRecordsLock's load loop before any callback runs -
// entries that reach a callback have already passed ValidHelperName.
func (s *helperRecords) forServer(name string) []HelperRecord {
	var out []HelperRecord
	for _, e := range s.Entries {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// readHelperRecords loads the records file without taking the lock. A
// directory with no file has no records. A corrupt file, or a record
// naming an invalid helper, is an error: no record is acted on from a file
// that cannot be trusted. Writes are atomic renames, so a lock-free read
// sees a whole file.
func readHelperRecords(dir string) (*helperRecords, error) {
	st := &helperRecords{}
	b, rerr := readFile(filepath.Join(dir, helperRecordsFileName))
	switch {
	case rerr == nil:
		if uerr := yaml.Unmarshal(b, st); uerr != nil {
			return nil, fmt.Errorf("credential-helper records file %s is corrupt: %w", helperRecordsFileName, uerr)
		}
	case !os.IsNotExist(rerr):
		return nil, rerr
	}
	for _, e := range st.Entries {
		if !credhelper.ValidHelperName(e.Helper) {
			return nil, fmt.Errorf("credential-helper records file names invalid helper %q; refusing to execute it", e.Helper)
		}
	}
	return st, nil
}

// withHelperRecordsLock serializes a whole operation on the helper-records file
// across processes: flock (LockFileEx on Windows; released by the OS on
// process death) with a bounded wait, load, run fn - which may also invoke
// helper store/erase while holding the lock, by design - then persist if
// dirty. The lock spanning the helper calls is what makes a concurrent
// login's re-store never race a logout's erase-then-unrecord.
func withHelperRecordsLock(ctx context.Context, dir string, fn func(*helperRecords) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// ctx is the caller's context where one exists (the deletes, so Ctrl-C
	// interrupts a contended wait even while the config lock is held);
	// interface-bound store paths pass context.Background() and stay
	// bounded by the timeout alone.
	waitCtx, cancel := context.WithTimeout(ctx, 2*execHelperTimeout)
	defer cancel()
	release, err := waitForFlock(waitCtx, filepath.Join(dir, helperRecordsLockName), 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return clierr.Retryable(clierr.CodeLockTimeout,
			"locking the credential-helper records (another rancher process may be logging in/out): "+err.Error()).
			WithCause(err)
	}
	defer release()

	st, lerr := readHelperRecords(dir)
	if lerr != nil {
		return lerr
	}

	// Run fn, then persist dirty mutations EVEN WHEN fn errored: a delete that
	// erased three targets and failed a fourth must persist the three
	// removals (failed entries stay for retry), and SetToken's unsupported
	// path must persist its intent removal while returning the sentinel.
	// Both errors surface, neither masked.
	fnErr := fn(st)
	if st.dirty {
		if perr := persistHelperRecords(dir, st); perr != nil {
			return errors.Join(fnErr, fmt.Errorf("persisting credential-helper records: %w", perr))
		}
	}
	return fnErr
}

// DeleteOrphanedHelperCredentials erases EVERY remaining recorded helper credential,
// independently of what config.yaml still lists. `logout --all` calls it after
// deleting the configured servers so a credential whose server name never
// reached config.yaml - a crash (or failed config write) between a successful
// helper store and the config write leaves the helper holding a live token
// plus a helper record no configured-server logout can reach - is still
// deleted. Runs under the helper-records lock; each entry's helper name has
// already passed ValidHelperName in the lock loader. Entries whose erase
// succeeds are removed; failures are retained for a later retry and their
// errors joined. Returns the records whose credential was actually deleted -
// structured, so callers never re-parse a name that may itself contain any
// separator character.
func DeleteOrphanedHelperCredentials(ctx context.Context, dir string) ([]HelperRecord, error) {
	var deleted []HelperRecord
	lerr := withHelperRecordsLock(ctx, dir, func(st *helperRecords) error {
		// Snapshot the entries: st.remove mutates st.Entries mid-loop.
		remaining := make([]HelperRecord, len(st.Entries))
		copy(remaining, st.Entries)
		return deleteHelperRecords(dir, remaining, st, func(e HelperRecord) {
			deleted = append(deleted, e)
		})
	})
	return deleted, lerr
}

// persistHelperRecords is a swappable seam over persistHelperRecordsToDisk so
// tests can inject persistence failures at chosen call ordinals.
var persistHelperRecords = persistHelperRecordsToDisk

// setHelperRecordsPersistForTest swaps the persistence function; returns restore.
func setHelperRecordsPersistForTest(fn func(string, *helperRecords) error) (restore func()) {
	old := persistHelperRecords
	persistHelperRecords = fn
	return func() { persistHelperRecords = old }
}

// persistHelperRecordsToDisk marshals and atomically writes st, clearing dirty.
// Called by withHelperRecordsLock's tail and mid-callback by SetToken (for the
// write-ahead intent and the unsupported-removal, which must be durable
// BEFORE the sentinel is returned).
func persistHelperRecordsToDisk(dir string, st *helperRecords) error {
	out, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(filepath.Join(dir, helperRecordsFileName), out, 0o600); err != nil {
		return err
	}
	st.dirty = false
	return nil
}
