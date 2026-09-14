package config

import "github.com/rancher/rancher-cli/internal/clierr"

type CredentialBackend = clierr.CredentialBackend

type PriorSource = clierr.PriorSource

type CommitState = clierr.CredentialState

const (
	CommitNotWritten               = clierr.CredentialNotWritten
	CommitCommitted                = clierr.CredentialCommitted
	CommitCommittedUncompensatable = clierr.CredentialCommittedUncompensatable
	CommitPossiblyCommitted        = clierr.CredentialPossiblyCommitted
	CommitHelperNonAtomic          = clierr.CredentialHelperNonAtomic
)

// CommitOutcome is what a backend did with a token write, reported by the
// backend itself. A committed state never travels with a non-nil error.
type CommitOutcome struct {
	Backend CredentialBackend
	State   CommitState
	// PriorSource explains why Compensate is or is not safe.
	PriorSource PriorSource
	// HelperRecordRetained is true when the exec store kept its
	// write-ahead record; false for every other backend.
	HelperRecordRetained bool
	// Notice is an advisory a successful outcome wants surfaced (a
	// keyring Set error that a read-back proved harmless). Empty on
	// failure paths, which return an error instead.
	Notice string
	// Compensate restores the prior physical state of THIS backend; nil
	// unless State is CommitCommitted.
	Compensate func() error
}

// PriorCredential is what a credential write compensates back to. The
// caller classifies it and the store writes against it, so the account a
// login recorded as the entry's identity is the account the write targets,
// with no second read of anything in between. Raw carries a value only
// when Source is PriorKeyring; a new account's prior is none by
// construction, so a value read from a slot that turned out to belong to
// another config directory is never a prior and is never written anywhere.
type PriorCredential struct {
	// Account is the keychain account this write targets. Empty for a
	// backend that does not address credentials by account.
	Account string
	Source  PriorSource
	Raw     string
}

// CommitToken persists token through store and returns the backend's own
// account of what happened. prior is the state a compensation restores,
// classified by the caller. The backends' sentinel errors
// (ErrTokenStoreUnsupported) are returned unwrapped so callers keep using
// errors.Is.
func CommitToken(store CredStore, ref ServerRef, token string, prior PriorCredential) (CommitOutcome, error) {
	return store.SetTokenWithOutcome(ref, token, prior)
}
