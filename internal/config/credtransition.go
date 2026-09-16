package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"sigs.k8s.io/yaml"
)

// Changing which store a directory keeps its credentials in moves them one
// entry at a time, across backends that can each fail on their own. The
// manifest is the write-ahead record of that work: written before the new
// credential commits, inert until a config write names its id, and
// replayed by the next credential-mutating command until every step is
// done.
//
// There is exactly one, at a fixed path. An id-derived path would let two
// transitions exist at once, and the point of the single file is that a
// second store change cannot start while the first has work left.
const (
	transitionFileName = "credential-transition.yaml"
	transitionLockName = "credential-transition.lock"
	transitionVersion  = 1
	transitionIDLen    = 32
	transitionFilePerm = 0o600
)

// Step kinds. A step is one backend operation with everything needed to
// verify it, so a replay in a later process can tell "already done" from
// "someone else's credential" without trusting anything but the digest.
const (
	transitionMove   = "move"
	transitionDelete = "delete"
	transitionPin    = "pin"
)

// Backend names a step operates on. These are physical backends, never the
// policy `auto`: they are resolved before the manifest is written, so a pin
// that flips later cannot retarget work already scheduled.
const (
	backendFile     = "file"
	backendKeychain = "keychain"
	backendExecPre  = "exec:"
)

// transitionManifest is the write-ahead record of a credential store
// change.
type transitionManifest struct {
	Version int              `json:"version"`
	ID      string           `json:"id"`
	Steps   []transitionStep `json:"steps"`
}

// transitionStep is one backend operation.
//
// The verifier is backend-neutral: a random VerificationID per step and
// CredentialDigest = HMAC-SHA256(VerificationID, token). It is not a
// password hash and does not need to be - Rancher bearer tokens are
// high-entropy values, and the manifest is owner-only local state. What it
// buys is that replay can prove a destination holds the credential it was
// supposed to receive, and that a source still holds the one it is allowed
// to delete, without any backend-specific notion of identity and without
// the same credential being recognizable across two manifests.
type transitionStep struct {
	Kind        string `json:"kind"`
	Entry       string `json:"entry,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	// Target is the pin step's backend.
	Target           string `json:"target,omitempty"`
	VerificationID   string `json:"verificationId,omitempty"`
	CredentialDigest string `json:"credentialDigest,omitempty"`
	// SourceAccountID and DestAccountID name the keychain accounts a step
	// reads and writes, so a replay addresses the account the step was
	// scheduled against rather than whatever the entry's identity record
	// happens to say later.
	SourceAccountID string `json:"sourceAccountId,omitempty"`
	DestAccountID   string `json:"destAccountId,omitempty"`
	// CanonicalURL is recorded for a step whose source or destination is a
	// credential helper: the helper protocol keys a credential on canonical
	// URL plus name, and `config update-server --url` or `config
	// delete-server` may change or remove the entry's URL before the step
	// is replayed.
	CanonicalURL string `json:"canonicalUrl,omitempty"`
	// LastError is the cause of the most recent failure this step reported
	// - the backend's own words, not a rendered sentence - so `config
	// status` and the next command's refusal can each say why the work is
	// still there in their own frame.
	LastError string `json:"lastError,omitempty"`
	// Terminal records that the last attempt found the step impossible to
	// finish: the credential is in neither backend the step names, or a
	// destination refused the write for good. A terminal step is reported
	// with its own non-retryable error and remedy; a retained one with the
	// retryable code. Cleared when a later attempt gets further.
	Terminal bool `json:"terminal,omitempty"`
}

// TransitionPath is the directory's transition manifest.
func TransitionPath(dir string) string { return filepath.Join(dir, transitionFileName) }

// newTransitionID generates a manifest id: 128 random bits in lower-case
// hex.
func newTransitionID() (string, error) { return randomHex(transitionIDLen / 2) }

// transitionError is the terminal verdict on a manifest that cannot be
// trusted. It names the file, says no backend operation ran, and leaves
// the file alone: a manifest is the only record of where credentials were
// being moved, and rewriting one this release cannot read would destroy
// that record.
func transitionError(dir, what string) error {
	path := TransitionPath(dir)
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential store change recorded in %s is %s; no credential was read or written; inspect the file and restore a valid copy, or move it aside to abandon the change",
		path, what)).
		WithHint("rancher config status")
}

// transitionIncomplete reports work the directory still has to do. It is
// retryable: the step completes on its own once the backend it needs
// answers, and the next credential-mutating command retries it.
func transitionIncomplete(step transitionStep) error {
	msg := fmt.Sprintf("a credential store change is unfinished for this directory (%s)", stepWhat(step))
	if step.LastError != "" {
		msg += ": " + step.LastError + "; run this command again once that is resolved"
	} else {
		msg += "; run this command again"
	}
	return clierr.Retryable(clierr.CodeCredentialTransitionIncomplete, msg).
		WithHint("rancher config status")
}

// transitionAlreadyRecorded refuses to write a manifest over a different
// one. The file is the only record of where credentials were being moved,
// so a second change may start only once the first has finished or been
// abandoned - the callers that are allowed to remove one go through
// removeTransition, never through a write.
func transitionAlreadyRecorded(dir string, present transitionStep) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"a credential store change is already recorded in %s (%s); it must finish, or be abandoned, before another is recorded",
		TransitionPath(dir), stepWhat(present))).
		WithHint("rancher config status")
}

// stepWhat names a step for a message: its kind and, when it has one, its
// entry.
func stepWhat(step transitionStep) string {
	if step.Entry != "" {
		return fmt.Sprintf("%s %s", step.Kind, step.Entry)
	}
	return step.Kind
}

// validateTransition checks everything a replay would otherwise have to
// trust. Strict on the way in: a manifest drives deletes across backends,
// so a field this release does not understand is a reason to stop, never
// to guess.
func validateTransition(dir string, m *transitionManifest) error {
	if m.Version != transitionVersion {
		return transitionError(dir, fmt.Sprintf("version %d, which this release does not understand", m.Version))
	}
	if !isLowerHex(m.ID, transitionIDLen) {
		return transitionError(dir, "missing or malformed in its identifier")
	}
	if len(m.Steps) == 0 {
		return transitionError(dir, "empty")
	}
	for i, s := range m.Steps {
		if err := validateTransitionStep(dir, i, s); err != nil {
			return err
		}
	}
	return nil
}

func validateTransitionStep(dir string, i int, s transitionStep) error {
	bad := func(what string) error {
		return transitionError(dir, fmt.Sprintf("unusable: step %d is %s", i+1, what))
	}
	switch s.Kind {
	case transitionPin:
		if s.Target != backendFile && s.Target != backendKeychain {
			return bad("a pin to no known backend")
		}
		if s.Terminal {
			// No attempt records a pin as impossible: writing the pin
			// either succeeds or is retried. A manifest that says
			// otherwise was not written by this code, and a terminal
			// step is rendered with the entry it names, which a pin has
			// none of.
			return bad("a pin recorded as impossible, which no attempt produces")
		}
		return nil
	case transitionMove, transitionDelete:
	default:
		return bad("of no known kind")
	}
	if s.Entry == "" {
		return bad("for no entry")
	}
	if !isLowerHex(s.VerificationID, transitionIDLen) || !isLowerHex(s.CredentialDigest, identityFingerprintLen) {
		return bad("missing the verifier that proves which credential it may touch")
	}
	backends := []string{s.Source}
	if s.Kind == transitionMove {
		backends = append(backends, s.Destination)
		// A move into a helper cannot be verified: `get` may create a
		// credential rather than return the one `store` was given, so a
		// replay could never prove the destination received it.
		if !isPhysicalBackend(s.Destination) || isExecBackend(s.Destination) {
			return bad("a move into a backend this release cannot verify")
		}
	}
	for _, b := range backends {
		if !isPhysicalBackend(b) {
			return bad("addressed at no known backend")
		}
		if isExecBackend(b) && s.CanonicalURL == "" {
			return bad("a credential-helper step with no server URL to address it by")
		}
	}
	if s.Source == backendKeychain && !isLowerHex(s.SourceAccountID, identityIDLen) {
		return bad("a keychain step naming no source account")
	}
	if s.Kind == transitionMove && s.Destination == backendKeychain && !isLowerHex(s.DestAccountID, identityIDLen) {
		return bad("a move into the keychain naming no destination account")
	}
	return nil
}

// isPhysicalBackend reports whether b names a backend a step can address:
// never the policy `auto`, which is a question rather than an answer.
func isPhysicalBackend(b string) bool {
	if b == backendFile || b == backendKeychain {
		return true
	}
	name, ok := isExecName(b)
	return ok && name != ""
}

func isExecBackend(b string) bool {
	_, ok := isExecName(b)
	return ok
}

func isExecName(b string) (string, bool) {
	name, ok := execHelperName(b)
	return name, ok
}

// readTransition returns the manifest at the fixed path.
//
// The file is opened without following symlinks and must be a regular,
// owner-only file physically beneath the directory: os.Root confines the
// name, and the Lstat before the read refuses a link os.Root would happily
// follow inside the root. An absent file is (nil, nil) - the directory has
// no change recorded - and every other unreadable state is terminal.
func readTransition(dir string) (*transitionManifest, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, transitionError(dir, "in a directory that cannot be opened: "+err.Error())
	}
	defer func() { _ = root.Close() }()

	observeIdentity("manifeststat")
	fi, err := root.Lstat(transitionFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, transitionError(dir, "unreadable: "+err.Error())
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, transitionError(dir, "a symlink")
	}
	if !fi.Mode().IsRegular() {
		return nil, transitionError(dir, "not a regular file")
	}
	if err := checkIdentityDirOwnership(fi); err != nil {
		return nil, transitionError(dir, err.Error())
	}

	data, err := root.ReadFile(transitionFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, transitionError(dir, "unreadable: "+err.Error())
	}
	if len(data) == 0 {
		return nil, transitionError(dir, "empty")
	}
	var m transitionManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, transitionError(dir, "unreadable")
	}
	if err := validateTransition(dir, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeTransition persists the manifest by atomic rename through the
// directory's own root, so a reader sees one whole version and no step of
// the write follows a link out of the directory. Callers hold the
// transition lock.
//
// It never replaces a different manifest. A valid file with another id is
// work the directory still has, and overwriting it would forget every
// cross-backend delete it recorded; a file this release cannot read is
// terminal and is left for the operator. Only the same id may be
// rewritten, which is what a replay does as steps complete.
func writeTransition(dir string, m transitionManifest) error {
	if err := validateTransition(dir, &m); err != nil {
		return err
	}
	present, perr := readTransition(dir)
	if perr != nil {
		return perr
	}
	if present != nil && present.ID != m.ID {
		return transitionAlreadyRecorded(dir, present.Steps[0])
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, identityDirPerm); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	switch fi, lerr := root.Lstat(transitionFileName); {
	case lerr == nil && fi.Mode()&os.ModeSymlink != 0:
		return transitionError(dir, "a symlink")
	case lerr == nil && !fi.Mode().IsRegular():
		return transitionError(dir, "not a regular file")
	case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
		return transitionError(dir, "unreadable: "+lerr.Error())
	}

	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := "." + transitionFileName + "-" + suffix
	f, err := root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, transitionFilePerm)
	if err != nil {
		return err
	}
	// Close before removing: Windows cannot remove a file that is still
	// open, so remove-before-close leaks the temporary file there.
	closeAndRemove := func() {
		_ = f.Close()
		_ = root.Remove(tmp)
	}
	if err := hardenACLHook(filepath.Join(dir, tmp)); err != nil {
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
	if err := root.Rename(tmp, transitionFileName); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// removeTransition deletes the manifest; an absent one is not an error.
// Callers hold the transition lock.
func removeTransition(dir string) error {
	err := os.Remove(TransitionPath(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// transitionFileAbsent answers the common case with one stat and no open:
// a directory that has never had a store change.
//
// Only a clean "nothing is there" short-circuits. Anything else - a
// regular file, a symlink, a directory, a stat that failed - falls
// through to readTransition, which is what classifies an unsafe path and
// says so. Treating "not a regular file" as absent here would make a
// manifest replaced by a symlink invisible on a directory whose config
// names no change, which is the one state that most needs saying out
// loud.
func transitionFileAbsent(dir string) bool {
	observeIdentity("manifeststat")
	_, err := os.Lstat(TransitionPath(dir))
	return errors.Is(err, fs.ErrNotExist)
}

// TransitionState is what the directory's recorded store change amounts to
// right now.
type TransitionState int

const (
	// TransitionNone: the config names no change and there is no file.
	TransitionNone TransitionState = iota
	// TransitionActive: the config names the id of the file that is there.
	// This is work the next credential-mutating command must retry.
	TransitionActive
	// TransitionAbandoned: a valid file the config does not name. A login
	// died between preparing the manifest and the config write that would
	// have activated it, so nothing was ever moved and the file is
	// removable.
	TransitionAbandoned
	// TransitionFinished: the config names an id and no file is there. The
	// last step completed and the file was deleted before the config write
	// that would have cleared the name. Nothing is pending; the next config
	// write clears it.
	TransitionFinished
)

// LoadTransition reports the directory's transition state and, when there
// is a file, the manifest.
//
// "Active" means the loaded config names the id of a valid file that
// exists - both halves, which is what keeps a finished change from looking
// pending forever and an abandoned file from being replayed. A file whose
// id the config does not name, and which is otherwise valid, is abandoned
// work; one that is invalid is terminal whether or not the config names
// it, because a manifest that cannot be read is a set of cross-backend
// deletes nobody can account for.
func LoadTransition(dir string, cfg *Config) (TransitionState, *transitionManifest, error) {
	named := ""
	if cfg != nil {
		named = cfg.CredentialTransitionID
	}
	// One stat answers the common case - a directory that names no
	// change and has no manifest - without an open or a parse.
	if named == "" && transitionFileAbsent(dir) {
		return TransitionNone, nil, nil
	}
	m, err := readTransition(dir)
	if err != nil {
		return TransitionNone, nil, err
	}
	switch {
	case m == nil && named == "":
		return TransitionNone, nil, nil
	case m == nil:
		return TransitionFinished, nil, nil
	case named == "":
		return TransitionAbandoned, m, nil
	case named != m.ID:
		return TransitionNone, nil, transitionError(dir,
			"a change this directory does not name; the directory expects "+named)
	default:
		return TransitionActive, m, nil
	}
}

// HasPendingTransition reports whether the directory has store-change work
// left. It performs no file access at all when the config names no change,
// which is what keeps the hot paths free: a directory that has never had a
// transition never opens the manifest.
func HasPendingTransition(dir string, cfg *Config) bool {
	if cfg == nil || cfg.CredentialTransitionID == "" {
		return false
	}
	state, _, err := LoadTransition(dir, cfg)
	return err != nil || state == TransitionActive
}

// stepsNaming returns the remaining steps of the given kinds that name the
// entry.
func (m *transitionManifest) stepsNaming(entry string, kinds ...string) []transitionStep {
	if m == nil {
		return nil
	}
	var out []transitionStep
	for _, s := range m.Steps {
		if s.Entry == entry && slices.Contains(kinds, s.Kind) {
			out = append(out, s)
		}
	}
	return out
}

// keychainAccounts returns the keychain accounts the remaining steps name
// for the entry, so a delete path can reach an account the entry's own
// identity record no longer points at.
func (m *transitionManifest) keychainAccounts(entry string) []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, s := range m.Steps {
		if s.Entry != entry {
			continue
		}
		if s.Source == backendKeychain && s.SourceAccountID != "" {
			out = append(out, keychainAccount(s.SourceAccountID, entry))
		}
		if s.Kind == transitionMove && s.Destination == backendKeychain && s.DestAccountID != "" {
			out = append(out, keychainAccount(s.DestAccountID, entry))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// transitionLockSem serializes transition work inside one process, the way
// configLockSem does for the config lock.
var transitionLockSem = make(chan struct{}, 1)

// withTransitionLock runs fn while holding
// <dir>/credential-transition.lock. It is held through preparing a
// manifest, activating it, and every replayed step, so two commands cannot
// each decide the same step still needs doing.
//
// It sits between the config lock and the narrow backend locks: config
// outermost, then this one, then exactly one of credstore-id.lock or the
// helper-records lock, never both. That is what lets a replay take the
// identity lock for a keychain write and the helper-records lock for a
// helper erase without either one re-entering this.
//
// RANCHER_CLI_CONFIG_NO_LOCK skips the flock and keeps the in-process
// semaphore, exactly as it does for every other lock here. The residual is
// real and stated: without the flock a concurrent mutating command can
// replace or remove the one manifest, which is the same lost-update class
// the variable already accepts for config.yaml, and the variable stays the
// escape hatch for filesystems whose advisory locking does not work.
func withTransitionLock(ctx context.Context, dir string, fn func() error) error {
	return withDirLock(ctx, dir, transitionLockName, transitionLockSem, nil, nil, fn)
}
