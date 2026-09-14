package config

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const (
	identityDirName  = "credstore-id"
	identityLockName = "credstore-id.lock"
	identityVersion  = 1
	identityDirPerm  = 0o700
	identityFilePerm = 0o600
	// identityIDLen is a keychain account id in lower-case hex: 128 random
	// bits, wide enough that two directories that never shared a file never
	// share an account.
	identityIDLen = 32
	// identityFingerprintLen is a hex HMAC-SHA256.
	identityFingerprintLen = 64
	// identityNameLen is a hex SHA-256 file name.
	identityNameLen = 64
)

// identityState is what a record says about the credential write it
// describes: pending means a write was started and its outcome is not yet
// known, active means the keychain slot holds what this record fingerprints.
type identityState string

const (
	identityActive  identityState = "active"
	identityPending identityState = "pending"
)

// identityObserverFn counts the filesystem operations the identity records
// cost, so a test can assert the credential read path's operation count
// rather than its wall time. Nil in production; set via
// SetIdentityObserverForTest. Operations: "dirstat", "diropen",
// "recordstat", "recordread", "recordwrite", "recordremove", "list".
var identityObserverFn func(op string)

// SetIdentityObserverForTest installs the identity-operation observer for
// the current test run. The calling test must not use t.Parallel(): the
// observer is a package-level seam.
func SetIdentityObserverForTest(fn func(op string)) (restore func()) {
	prev := identityObserverFn
	identityObserverFn = fn
	return func() { identityObserverFn = prev }
}

// identityWriteHook lets a test fail one record write in the middle of a
// login, which is the only way to reach the paths that settle or revert a
// record after the credential write has already been decided. Nil in
// production; set via SetIdentityWriteHookForTest.
var identityWriteHook func() error

// SetIdentityWriteHookForTest installs the record-write failure hook for
// the current test run. The calling test must not use t.Parallel(): the
// hook is a package-level seam.
func SetIdentityWriteHookForTest(fn func() error) (restore func()) {
	prev := identityWriteHook
	identityWriteHook = fn
	return func() { identityWriteHook = prev }
}

func observeIdentity(op string) {
	if identityObserverFn != nil {
		identityObserverFn(op)
	}
}

// identityRecord is one server entry's keychain identity, written ahead of
// the credential it describes. It is the only thing that names the entry's
// keychain account, so an absent record means nothing was ever stored for
// the entry under this directory's identity, and a record that does not
// parse is an error before any keychain access rather than a reason to
// create a new one.
type identityRecord struct {
	Version     int            `json:"version"`
	Entry       string         `json:"entry"`
	State       identityState  `json:"state"`
	ID          string         `json:"id"`
	Fingerprint string         `json:"fingerprint"`
	Prior       *identityPrior `json:"prior,omitempty"`
}

// identityPrior is the identity a pending write reverts to when the write
// turns out not to have landed: the account that held the entry's
// credential before, and the fingerprint of what it held.
type identityPrior struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// Account is the keychain account this record's credential lives under.
func (r identityRecord) Account() string { return keychainAccount(r.ID, r.Entry) }

// PriorAccount is the keychain account the entry's credential lived under
// before a pending write, empty when the write had nothing to revert to.
func (r identityRecord) PriorAccount() string {
	if r.Prior == nil {
		return ""
	}
	return keychainAccount(r.Prior.ID, r.Entry)
}

// identityDirPath is the directory of per-entry identity records. It sits
// beside credstore-pin and is machine-local for the same reason: it binds
// this copy of the directory to accounts in this machine's keychain.
func identityDirPath(dir string) string { return filepath.Join(dir, identityDirName) }

// identityFileName is the record file name for an entry: the lower-case hex
// SHA-256 of the entry name. Entry names are free positionals, so a
// name-derived file name would collide on a case-insensitive filesystem,
// could nest or traverse, and could name an alternate data stream.
func identityFileName(entry string) string {
	sum := sha256.Sum256([]byte(entry))
	return hex.EncodeToString(sum[:])
}

// keychainAccount is the keychain user field for an entry's credential:
// the entry's identity, then the entry name. One constructor, so every raw
// keyring call in this package spells the account the same way.
func keychainAccount(id, entry string) string { return id + ":" + entry }

// randomHex returns n random bytes in lower-case hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newKeychainAccountID generates a keychain account id: 128 random bits in
// lower-case hex.
func newKeychainAccountID() (string, error) { return randomHex(identityIDLen / 2) }

// credentialFingerprint is HMAC-SHA256 of the token under the entry's
// account id, in hex. A keyed hash of a high-entropy bearer token in an
// owner-only machine-local file discloses nothing usable, and it is what
// lets a directory tell a credential it stored from one another directory
// sharing the same account stored since.
func credentialFingerprint(id, token string) string {
	mac := hmac.New(sha256.New, []byte(id))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// isLowerHex reports whether s is exactly n lower-case hex characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// ErrCredentialIdentity marks every error the identity layer raises for
// an entry: the record could not be read, parsed or trusted, or the
// directory holding it could not be. It rides as the cause of the typed
// error a caller renders, so a reader can tell "this directory cannot say
// who this entry is" from "the store could not be reached". The two are
// opposite verdicts at a credential read: the second may be retried
// against another candidate, the first must not be, because the next
// candidate is a different user.
var ErrCredentialIdentity = errors.New("the credential identity for this entry cannot be read")

// IsCredentialIdentityError reports whether err came from the identity
// layer. Callers outside this package branch on it rather than on an
// error code: the code these errors carry is VALIDATION, which unrelated
// failures use too.
func IsCredentialIdentityError(err error) bool { return errors.Is(err, ErrCredentialIdentity) }

// identityError renders a broken identity state. The remedy names the
// keychain step before the file step because the file is the only place
// the id is written: once it is gone, no command can address the accounts
// it named.
func identityError(dir, entry, problem string) error {
	path := filepath.Join(identityDirPath(dir), identityFileName(entry))
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential identity record for %s is %s: %s; note the id it holds, delete any %s keychain entry whose account name starts with that id, then delete the file and log in again",
		clierr.ShellQuote(entry), problem, path, keyringService)).
		WithHint("rancher auth login " + clierr.ShellQuote(entry)).
		WithCause(ErrCredentialIdentity)
}

// identityListError renders a file in credstore-id/ that is named like a
// record and cannot be read as one. The enumerator is what `logout --all`
// reaches every entry through, so one such file stops it for the whole
// directory rather than leaving a credential nothing else names.
func identityListError(dir, name, problem string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential identity record %s %s; move it aside and log in again - until then no server in this directory can be logged out",
		filepath.Join(identityDirPath(dir), name), problem)).
		WithHint("rancher auth login").
		WithCause(ErrCredentialIdentity)
}

// identityDirError renders a credstore-id directory that cannot be trusted.
// It is refused before any keychain access: the records inside name the
// accounts this directory's credentials live under.
func identityDirError(dir, problem string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential identity directory %s is %s; move it aside and log in again",
		identityDirPath(dir), problem)).
		WithHint("rancher auth login").
		WithCause(ErrCredentialIdentity)
}

// openIdentityRoot opens credstore-id for contained access. os.OpenRoot
// follows symlinks in its own argument, so the refusal has to happen
// first: one Lstat rejects a symlink, a non-directory, a foreign owner and
// permissions anyone else can use, and the open then confines every
// later name to that directory. The caller closes the root; an absent
// directory returns an error matching fs.ErrNotExist.
func openIdentityRoot(dir string) (*os.Root, error) {
	path := identityDirPath(dir)
	observeIdentity("dirstat")
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Absent is not a failure: the caller reads it as "nothing was
		// stored here", so it stays the bare filesystem error.
		return nil, err
	}
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, identityDirError(dir, "a symlink")
	}
	if !fi.IsDir() {
		return nil, identityDirError(dir, "not a directory")
	}
	if err := checkIdentityDirOwnership(fi); err != nil {
		return nil, identityDirError(dir, err.Error())
	}
	observeIdentity("diropen")
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Removed between the stat and the open, by a logout in another
		// process: absent, which the caller reads as "nothing stored
		// here", not a directory that cannot be trusted.
		return nil, err
	}
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}
	return root, nil
}

// createIdentityRoot opens credstore-id, creating it owner-only when it is
// not there yet. Only a credential write reaches it: a read never creates
// the directory, so a file-store directory never grows one.
func createIdentityRoot(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, identityDirPerm); err != nil {
		return nil, err
	}
	switch err := os.Mkdir(identityDirPath(dir), identityDirPerm); {
	case err == nil, errors.Is(err, fs.ErrExist):
	default:
		return nil, err
	}
	// The mode bits above are what Unix enforces; the access control list
	// is what Windows enforces, and a directory that inherits its parent's
	// is not owner-only there. Applied on every write, not only at
	// creation: the call is idempotent, and a directory left behind by a
	// creation whose hardening failed would otherwise never be repaired.
	if err := hardenACLHook(identityDirPath(dir)); err != nil {
		return nil, err
	}
	return openIdentityRoot(dir)
}

// parseIdentityRecord validates raw record bytes for the entry whose file
// they came from. Every state that is not a valid version-1 record for that
// entry is an error, never a reason to write a fresh one.
func parseIdentityRecord(dir, entry string, data []byte) (*identityRecord, error) {
	if len(data) == 0 {
		return nil, identityError(dir, entry, "empty")
	}
	var rec identityRecord
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return nil, identityError(dir, entry, "unreadable")
	}
	if rec.Version != identityVersion {
		return nil, identityError(dir, entry, fmt.Sprintf("version %d, which this release does not understand", rec.Version))
	}
	if rec.Entry != entry {
		return nil, identityError(dir, entry, "a record for another entry")
	}
	if rec.State != identityActive && rec.State != identityPending {
		return nil, identityError(dir, entry, "in no known state")
	}
	if !isLowerHex(rec.ID, identityIDLen) {
		return nil, identityError(dir, entry, "missing its identity")
	}
	if !isLowerHex(rec.Fingerprint, identityFingerprintLen) {
		return nil, identityError(dir, entry, "missing its fingerprint")
	}
	if rec.Prior != nil {
		if rec.State != identityPending {
			return nil, identityError(dir, entry, "active and still carrying an earlier identity")
		}
		if !isLowerHex(rec.Prior.ID, identityIDLen) || !isLowerHex(rec.Prior.Fingerprint, identityFingerprintLen) {
			return nil, identityError(dir, entry, "carrying an unreadable earlier identity")
		}
	}
	return &rec, nil
}

// readIdentityRecord returns the entry's record, or (nil, nil) when the
// directory or the file is absent - nothing was ever stored for the entry
// under this directory's identity. Reads take no lock: a record is written
// by an atomic rename, so a reader sees one whole version or the previous
// one.
func readIdentityRecord(dir, entry string) (*identityRecord, error) {
	root, err := openIdentityRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	name := identityFileName(entry)
	observeIdentity("recordstat")
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, identityError(dir, entry, "unreadable")
	}
	// os.Root follows a symlink that stays inside the root, so the link has
	// to be refused by name here; a record is always a regular file.
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, identityError(dir, entry, "a symlink")
	}
	if fi.IsDir() {
		return nil, identityError(dir, entry, "a directory")
	}
	if !fi.Mode().IsRegular() {
		return nil, identityError(dir, entry, "not a regular file")
	}
	observeIdentity("recordread")
	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		// Removed between the stat and the read, by a logout in another
		// process: the same answer this read would give a moment later,
		// and not the broken identity whose remedy names keychain
		// entries that are still live.
		return nil, nil
	}
	if err != nil {
		return nil, identityError(dir, entry, "unreadable")
	}
	return parseIdentityRecord(dir, entry, data)
}

// listIdentityRecords returns every readable record in the directory. Only
// names of exactly the hex length a SHA-256 produces are considered, so the
// writer's temporary files and anything else left beside them are skipped
// rather than parsed as a record.
func listIdentityRecords(dir string) ([]identityRecord, error) {
	root, err := openIdentityRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	observeIdentity("list")
	d, err := root.Open(".")
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}

	var out []identityRecord
	for _, e := range entries {
		name := e.Name()
		if !isLowerHex(name, identityNameLen) {
			continue
		}
		// A name spelled like a record is a record: refusing it here is
		// the verdict a reader of the same file gets, so one file cannot
		// stop a command and be skipped by an enumeration of the same
		// directory.
		if !e.Type().IsRegular() {
			return nil, identityListError(dir, name, "is not a regular file")
		}
		observeIdentity("recordread")
		data, err := root.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between the directory read and this one: an entry
			// another process has just logged out is not an entry this
			// one has to reach.
			continue
		}
		if err != nil {
			return nil, identityListError(dir, name, fmt.Sprintf("is unreadable: %v", err))
		}
		var probe identityRecord
		if err := yaml.Unmarshal(data, &probe); err != nil {
			return nil, identityListError(dir, name, "does not parse")
		}
		if identityFileName(probe.Entry) != name {
			return nil, identityListError(dir, name, "names an entry it is not the record for")
		}
		rec, err := parseIdentityRecord(dir, probe.Entry, data)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// writeIdentityRecord writes the entry's record atomically inside the
// contained directory. atomicWriteFile is not enough here: it refuses a
// symlink at the final path only, and its directory creation and temporary
// file follow a symlinked parent, which is exactly the directory this
// record's containment has to hold. Callers hold the identity lock.
func writeIdentityRecord(dir string, rec identityRecord) error {
	if identityWriteHook != nil {
		if err := identityWriteHook(); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(rec)
	if err != nil {
		return err
	}
	// Refuse to write a record this package could not read back, so a
	// caller's mistake fails at the write rather than leaving a directory
	// that cannot be used again.
	if _, err := parseIdentityRecord(dir, rec.Entry, data); err != nil {
		return err
	}

	root, err := createIdentityRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	name := identityFileName(rec.Entry)
	// A record that is not a regular file is refused on the way in as it
	// is on the way out: os.Root would rename over a symlink that stays
	// inside the root, destroying whatever it names instead of saying
	// what is wrong.
	switch fi, lerr := root.Lstat(name); {
	case lerr == nil && fi.Mode()&os.ModeSymlink != 0:
		return identityError(dir, rec.Entry, "a symlink")
	case lerr == nil && !fi.Mode().IsRegular():
		return identityError(dir, rec.Entry, "not a regular file")
	case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
		return identityError(dir, rec.Entry, "unreadable")
	}

	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := "." + name + "-" + suffix

	observeIdentity("recordwrite")
	f, err := root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, identityFilePerm)
	if err != nil {
		return err
	}
	// Close before removing: Windows cannot remove a file that is still
	// open, so remove-before-close silently leaks the temporary file there.
	closeAndRemove := func() {
		_ = f.Close()
		_ = root.Remove(tmp)
	}
	if err := hardenACLHook(filepath.Join(identityDirPath(dir), tmp)); err != nil {
		closeAndRemove()
		return err
	}
	if _, err := f.Write(data); err != nil {
		closeAndRemove()
		return err
	}
	if err := f.Sync(); err != nil {
		closeAndRemove()
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// removeIdentityRecord deletes the entry's record; an absent record is not
// an error. Callers hold the identity lock.
func removeIdentityRecord(dir, entry string) error {
	root, err := openIdentityRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	observeIdentity("recordremove")
	if err := root.Remove(identityFileName(entry)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// identityLockSem serializes this process's identity-record writers, the
// way configLockSem does for the config lock, and for the same reason: the
// wait has to be cancellable.
var identityLockSem = make(chan struct{}, 1)

// withIdentityLock runs fn while holding <dir>/credstore-id.lock. It is the
// innermost lock in the directory: it is taken while the config lock is
// held, because logins and logouts run under that one, and code holding it
// never acquires another. Reads take it never - only a record write does:
// login, compensation and the delete paths. RANCHER_CLI_CONFIG_NO_LOCK
// skips the flock and keeps the in-process semaphore, exactly as it does
// for the config lock; because records are one file per entry, running
// without the flock can never lose another entry's record.
func withIdentityLock(ctx context.Context, dir string, fn func() error) error {
	return withDirLock(ctx, dir, identityLockName, identityLockSem, nil, nil, fn)
}
