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

// CommitToken persists token through store and returns the backend's own
// account of what happened. It never reads a prior value itself: only the
// backend knows where its prior state physically lived. The backends'
// sentinel errors (ErrTokenStoreUnsupported) are returned unwrapped so
// callers keep using errors.Is.
func CommitToken(store CredStore, ref ServerRef, token string) (CommitOutcome, error) {
	return store.SetTokenWithOutcome(ref, token)
}
