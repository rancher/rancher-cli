package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/credhelper"
)

// Provenance records which exec helpers were asked to store which credential
// identities, so a later drain can erase through every helper that may hold
// a token — regardless of what the credential-store policy says by then.
// Machine-local state (like the credstore pin), never portable config.
// Entries are written BEFORE the helper store runs (write-ahead): a stale
// intent after a failed store is harmless because erase is idempotent, while
// a missing record after a successful store would orphan a live credential.
const (
	provenanceFileName = "credhelper-provenance.yaml"
	provenanceLockName = "credhelper-provenance.lock"
)

// ProvenanceEntry is one recorded store intent: helper × credential identity.
type ProvenanceEntry struct {
	Helper       string `json:"helper"`
	Name         string `json:"name"`
	CanonicalURL string `json:"canonicalURL"`
}

type provenanceState struct {
	Entries []ProvenanceEntry `json:"entries"`
	dirty   bool
}

func (s *provenanceState) add(e ProvenanceEntry) {
	for _, x := range s.Entries {
		if x == e {
			return
		}
	}
	s.Entries = append(s.Entries, e)
	s.dirty = true
}

func (s *provenanceState) remove(e ProvenanceEntry) {
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
// are validated in withProvenanceLock's load loop before any callback runs —
// entries that reach a callback have already passed ValidHelperName.
func (s *provenanceState) forServer(name string) []ProvenanceEntry {
	var out []ProvenanceEntry
	for _, e := range s.Entries {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// withProvenanceLock serializes a whole operation on the provenance file
// across processes: flock (LockFileEx on Windows; released by the OS on
// process death) with a bounded wait, load, run fn — which may also invoke
// helper store/erase while holding the lock, by design — then persist if
// dirty. The lock spanning the helper calls is what makes a concurrent
// login's re-store never race a logout's erase-then-unrecord.
func withProvenanceLock(dir string, fn func(*provenanceState) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fl := flock.New(filepath.Join(dir, provenanceLockName))
	ctx, cancel := context.WithTimeout(context.Background(), 2*execHelperTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !ok {
		return fmt.Errorf("locking credential-helper provenance (another rancher process may be logging in/out): %v", err)
	}
	defer func() { _ = fl.Unlock() }()

	st := &provenanceState{}
	b, rerr := os.ReadFile(filepath.Join(dir, provenanceFileName))
	switch {
	case rerr == nil:
		if uerr := yaml.Unmarshal(b, st); uerr != nil {
			return fmt.Errorf("credential-helper provenance %s is corrupt: %w", provenanceFileName, uerr)
		}
	case !os.IsNotExist(rerr):
		return rerr
	}
	for _, e := range st.Entries {
		if !credhelper.ValidHelperName(e.Helper) {
			return fmt.Errorf("credential-helper provenance names invalid helper %q; refusing to execute it", e.Helper)
		}
	}

	// Run fn, then persist dirty mutations EVEN WHEN fn errored: a drain that
	// erased three targets and failed a fourth must persist the three
	// removals (failed entries stay for retry), and SetToken's unsupported
	// path must persist its intent removal while returning the sentinel.
	// Both errors surface, neither masked.
	fnErr := fn(st)
	if st.dirty {
		if perr := persistProvenance(dir, st); perr != nil {
			return errors.Join(fnErr, fmt.Errorf("persisting credential-helper provenance: %w", perr))
		}
	}
	return fnErr
}

// persistProvenance is a swappable seam over persistProvenanceToDisk so
// tests can inject persistence failures at chosen call ordinals.
var persistProvenance = persistProvenanceToDisk

// setProvenancePersistForTest swaps the persistence function; returns restore.
func setProvenancePersistForTest(fn func(string, *provenanceState) error) (restore func()) {
	old := persistProvenance
	persistProvenance = fn
	return func() { persistProvenance = old }
}

// persistProvenanceToDisk marshals and atomically writes st, clearing dirty.
// Called by withProvenanceLock's tail and mid-callback by SetToken (for the
// write-ahead intent and the unsupported-removal, which must be durable
// BEFORE the sentinel is returned).
func persistProvenanceToDisk(dir string, st *provenanceState) error {
	out, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(filepath.Join(dir, provenanceFileName), out, 0o600); err != nil {
		return err
	}
	st.dirty = false
	return nil
}
