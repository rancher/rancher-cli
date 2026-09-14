package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// keychainWrite is one entry's credential write with the identity
// bookkeeping around it: the record written ahead of the credential, the
// record to go back to, and the prior the credential write compensates
// against. Every committed account stays addressable at every point of
// it: before the pending record nothing changed, between the pending
// record and its settlement the record names the account and the
// fingerprint the write intended, and after settlement the record names
// what the keychain holds.
type keychainWrite struct {
	dir   string
	entry string
	// Prior is what the credential write compensates back to.
	Prior PriorCredential
	// pending is the record written ahead of the credential write.
	pending identityRecord
	// previous is the active record the entry had before, nil when it had
	// none.
	previous *identityRecord
}

// beginKeychainWrite reads and settles the entry's identity, chooses the
// account the credential will be written under, and records that intent
// before the write. A fresh entry and an entry whose slot holds a
// credential this directory did not store both get a new account: the
// value already in the other directory's slot is never a prior, is never
// restored, and its account is left exactly as it is.
func beginKeychainWrite(ctx context.Context, k *keyringStore, ref ServerRef, token string) (*keychainWrite, error) {
	current, err := reconcileIdentity(ctx, k, ref)
	if err != nil {
		return nil, err
	}

	var (
		id    string
		prior PriorCredential
	)
	if current == nil {
		if id, err = newKeychainAccountID(); err != nil {
			return nil, err
		}
		prior = PriorCredential{Source: clierr.PriorNone}
	} else {
		stored, gerr := keyringGet(current.Account())
		switch {
		case errors.Is(gerr, keyring.ErrNotFound):
			// Logged out: the account is this directory's and empty.
			id = current.ID
			prior = PriorCredential{Source: clierr.PriorNone}
		case gerr != nil:
			return nil, k.getError(ref.Name, gerr)
		case credentialFingerprint(current.ID, stored) == current.Fingerprint:
			// An ordinary re-login: the same account, over this
			// directory's own credential.
			id = current.ID
			prior = PriorCredential{Source: clierr.PriorKeyring, Raw: stored}
		default:
			// The slot holds a credential this directory never stored, so
			// this entry has diverged from a copy: take a new account and
			// leave the other one alone.
			if id, err = newKeychainAccountID(); err != nil {
				return nil, err
			}
			prior = PriorCredential{Source: clierr.PriorNone}
		}
	}
	prior.Account = keychainAccount(id, ref.Name)

	pending := identityRecord{
		Version:     identityVersion,
		Entry:       ref.Name,
		State:       identityPending,
		ID:          id,
		Fingerprint: credentialFingerprint(id, token),
	}
	if current != nil {
		pending.Prior = &identityPrior{ID: current.ID, Fingerprint: current.Fingerprint}
	}
	if err := withIdentityLock(ctx, k.dir, func() error {
		return writeIdentityRecord(k.dir, pending)
	}); err != nil {
		return nil, err
	}
	return &keychainWrite{dir: k.dir, entry: ref.Name, Prior: prior, pending: pending, previous: current}, nil
}

// finish settles the record against what the credential write did. Both
// committed states mean the credential landed. A write whose outcome the
// keychain could not report leaves the record pending: the next login
// reads the account and the fingerprint it names and settles it then.
func (w *keychainWrite) finish(ctx context.Context, state CommitState) error {
	switch state {
	case CommitCommitted, CommitCommittedUncompensatable:
		active := w.pending
		active.State = identityActive
		active.Prior = nil
		return withIdentityLock(ctx, w.dir, func() error {
			return writeIdentityRecord(w.dir, active)
		})
	case CommitPossiblyCommitted:
		return nil
	default:
		return w.revert(ctx)
	}
}

// revert puts the entry's record back the way the write found it: the
// record the entry had before, or none when it had none.
func (w *keychainWrite) revert(ctx context.Context) error {
	return withIdentityLock(ctx, w.dir, func() error {
		if w.previous == nil {
			return removeIdentityRecord(w.dir, w.entry)
		}
		return writeIdentityRecord(w.dir, *w.previous)
	})
}

// accounts names the account this write targets and the one it would go
// back to, for a message about a compensation that could not finish.
func (w *keychainWrite) accounts() string {
	if w.previous == nil {
		return fmt.Sprintf("account %s", w.pending.Account())
	}
	return fmt.Sprintf("account %s, previous account %s", w.pending.Account(), w.previous.Account())
}

// reconcileIdentity settles a record an earlier login left pending, as
// bookkeeping only: if the account it names holds the credential it
// intended, that write landed and the record becomes active for it;
// otherwise the write did not land and the record goes back to the
// identity it named as previous, or away entirely. The verdict a login
// then reaches is the same one any active record gets: reconciliation
// never shortcuts it.
func reconcileIdentity(ctx context.Context, k *keyringStore, ref ServerRef) (*identityRecord, error) {
	rec, err := readIdentityRecord(k.dir, ref.Name)
	if err != nil || rec == nil || rec.State != identityPending {
		return rec, err
	}

	stored, gerr := keyringGet(rec.Account())
	switch {
	case gerr != nil && !errors.Is(gerr, keyring.ErrNotFound):
		return nil, k.getError(ref.Name, gerr)
	case gerr == nil && credentialFingerprint(rec.ID, stored) == rec.Fingerprint:
		active := *rec
		active.State = identityActive
		active.Prior = nil
		if err := withIdentityLock(ctx, k.dir, func() error {
			return writeIdentityRecord(k.dir, active)
		}); err != nil {
			return nil, err
		}
		return &active, nil
	}

	if rec.Prior == nil {
		if err := withIdentityLock(ctx, k.dir, func() error {
			return removeIdentityRecord(k.dir, ref.Name)
		}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	restored := identityRecord{
		Version:     identityVersion,
		Entry:       ref.Name,
		State:       identityActive,
		ID:          rec.Prior.ID,
		Fingerprint: rec.Prior.Fingerprint,
	}
	if err := withIdentityLock(ctx, k.dir, func() error {
		return writeIdentityRecord(k.dir, restored)
	}); err != nil {
		return nil, err
	}
	return &restored, nil
}

// keychainStoreFor reports the keychain store a login is about to write
// through, and false for a store that does not write to the keychain. The
// concrete store is the honest test of "this write goes to the keychain":
// a second policy lookup could disagree with the store actually opened,
// which an auto repin inside the same login can move. A lazy store is
// resolved, which a login that is about to write a credential does anyway.
func keychainStoreFor(store CredStore) (*keyringStore, bool) {
	switch s := store.(type) {
	case *keyringStore:
		return s, true
	case *lazyStore:
		inner, err := s.backend()
		if err != nil {
			return nil, false
		}
		return keychainStoreFor(inner)
	}
	return nil, false
}

// deleteKeychainCredential removes the entry's credential from the keychain
// and then its identity record, comparing before it deletes. With no
// record nothing was ever stored for the entry under this directory's
// identity: nothing is read, nothing is deleted and no record is created.
// declared says the directory's store resolves to the keychain, which
// decides whether a keychain it cannot reach is an error or a warning that
// leaves the record for the next attempt. pinHint rides only the error
// arms: a directory that did not declare the keychain has no pin to undo,
// so the warning arms have nothing to add.
func deleteKeychainCredential(ctx context.Context, dir string, ref ServerRef, declared bool, pinHint string) (deleted bool, notices []string, err error) {
	rec, err := readIdentityRecord(dir, ref.Name)
	if err != nil || rec == nil {
		return false, nil, err
	}

	// The accounts this directory may have stored the entry's credential
	// under: the record's own, and - while a write is unsettled - the one
	// it would go back to. A crash between the pending record and the
	// credential write leaves the credential under the previous account,
	// and a logout still has to remove it.
	type candidate struct {
		account     string
		fingerprint string
		id          string
	}
	candidates := []candidate{{account: rec.Account(), fingerprint: rec.Fingerprint, id: rec.ID}}
	if rec.Prior != nil {
		candidates = append(candidates, candidate{
			account:     rec.PriorAccount(),
			fingerprint: rec.Prior.Fingerprint,
			id:          rec.Prior.ID,
		})
	}

	mismatch := false
	for _, c := range candidates {
		stored, gerr := keyringGet(c.account)
		switch {
		case errors.Is(gerr, keyring.ErrNotFound):
			continue
		case gerr != nil:
			if declared {
				return deleted, notices, fmt.Errorf("keyring get %q: %w%s", ref.Name, gerr, pinHint)
			}
			// A directory that does not declare the keychain still has to
			// try, because the record says something is there; it must not
			// fail the logout of the store it does declare. The record
			// stays for the next attempt.
			notices = append(notices, fmt.Sprintf("keyring get %q: %v - keychain unreachable, the credential identity record was kept", ref.Name, gerr))
			return deleted, notices, nil
		case credentialFingerprint(c.id, stored) != c.fingerprint:
			mismatch = true
			continue
		}
		if derr := keyringDelete(c.account); derr != nil && !errors.Is(derr, keyring.ErrNotFound) {
			// The record stays: the credential is still there and the next
			// attempt must still reach it.
			if declared {
				return deleted, notices, fmt.Errorf("keyring delete %q: %w%s", ref.Name, derr, pinHint)
			}
			notices = append(notices, fmt.Sprintf("keyring delete %q: %v - the credential identity record was kept", ref.Name, derr))
			return deleted, notices, nil
		}
		deleted = true
	}

	if !deleted && mismatch {
		notices = append(notices, fmt.Sprintf(
			"another config directory sharing this identity now owns the stored credential for %s; nothing deleted from the keychain",
			ref.Name))
	}
	if rerr := withIdentityLock(ctx, dir, func() error {
		return removeIdentityRecord(dir, ref.Name)
	}); rerr != nil {
		return deleted, notices, rerr
	}
	return deleted, notices, nil
}

// IdentityEntries lists the server entries this directory holds a keychain
// identity record for. `logout --all` uses it to reach an entry whose
// config record was removed by hand: the credential is still addressable
// through the identity record, and nothing else names it.
func IdentityEntries(dir string) ([]string, error) {
	records, err := listIdentityRecords(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.Entry)
	}
	return out, nil
}
