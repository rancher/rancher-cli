package config

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// An orphan is a credential stored under a name config.yaml has no entry
// for - left by a hand edit, or a rollback that did not finish. No reader
// is served it, so where an unpinned directory keeps its credentials
// ignores it. A command that creates an entry of that name - a login, or
// `config add-server` - would make it count the moment the entry is
// written: a reader of the new entry would be served it, a bearer perhaps
// another server issued, and an unpinned directory would read as keeping
// credentials in both stores for every entry. So the command removes it
// first, under the config lock and before the entry write, and says so.

// RemoveOrphanCredentials removes the credentials stored under name when
// config.yaml has no entry of that name, and returns one notice per
// removal. It removes nothing when the entry exists. It covers the
// keychain, the file store, and each credential helper in helpers - the
// ones this directory's readers ask and the one the login commits to
// (OrphanHelpers) - where that helper's records name the orphan: a helper
// is asked directly, never through its records, so its orphan would be
// served. A helper no reader asks is left to `auth logout --all`'s sweep.
// A helper that cannot erase it, or a keychain that cannot be reached,
// refuses with the error; the caller then writes nothing. The caller holds
// the config lock and writes the entry after this returns.
func RemoveOrphanCredentials(ctx context.Context, dir string, cfg *Config, name string, helpers []string) ([]string, error) {
	if _, ok := cfg.Servers[name]; ok {
		return nil, nil
	}
	var notices []string
	for _, h := range helpers {
		helper, ok := execHelperName(h)
		if !ok {
			continue
		}
		if err := withHelperRecordsLock(ctx, dir, func(st *helperRecords) error {
			for _, e := range st.forServer(name) {
				if e.Helper != helper {
					continue
				}
				if _, eerr := eraseHelperCredential(dir, e); eerr != nil {
					return orphanHelperError(name, helper, eerr)
				}
				st.remove(e)
				notices = append(notices, orphanRemovedNotice(name, "the credential helper "+helper))
			}
			return nil
		}); err != nil {
			return notices, err
		}
	}
	rec, err := readIdentityRecord(dir, name)
	if err != nil {
		return notices, err
	}
	if rec != nil {
		deleted, ns, derr := deleteKeychainCredential(ctx, dir, ServerRef{Name: name}, true, orAnotherConfig)
		notices = append(notices, ns...)
		if derr != nil {
			return notices, orphanKeychainError(name, derr)
		}
		if deleted {
			notices = append(notices, orphanRemovedNotice(name, "the keychain"))
		} else {
			notices = append(notices, fmt.Sprintf(
				"removed the keychain identity record of a credential stored under %s that no entry named; nothing was deleted from the keychain", name))
		}
	}
	fs := newFileStore(dir, &noticeBuffer{})
	tokens, err := fs.read()
	if err != nil {
		return notices, unreadableCredentialsFile(dir, err)
	}
	if _, ok := tokens[name]; ok {
		if derr := fs.DeleteToken(ServerRef{Name: name}); derr != nil {
			return notices, fmt.Errorf("removing the credential stored under %s that no entry named, from %s: %w", name, CredentialsFilePath(dir), derr)
		}
		notices = append(notices, orphanRemovedNotice(name, "the plaintext file "+CredentialsFilePath(dir)))
	}
	return notices, nil
}

// CheckOrphanCredentials is RemoveOrphanCredentials' refusal alone, for the
// check before the sign-in: an orphan under name in a keychain this
// process cannot reach refuses the login, since the removal under the lock
// would. It reads the orphan's slot and writes nothing.
func CheckOrphanCredentials(dir string, cfg *Config, name string) error {
	if _, ok := cfg.Servers[name]; ok {
		return nil
	}
	rec, err := readIdentityRecord(dir, name)
	if err != nil || rec == nil {
		return err
	}
	if _, gerr := keyringGet(rec.Account()); gerr != nil && !errors.Is(gerr, keyring.ErrNotFound) {
		return orphanKeychainError(name, keychainError("get", name, gerr, orAnotherConfig))
	}
	return nil
}

// orphanKeychainError refuses a command creating an entry whose name has an
// orphan credential in a keychain the process cannot reach: the orphan has
// to go before the entry is written, and it cannot. A keychain that is
// unavailable keeps its own code and remedy.
func orphanKeychainError(name string, err error) error {
	if ce := asStoreUnavailable(err); ce != nil {
		return ce.WithMessage(fmt.Sprintf(
			"a credential stored under %s that no entry names is in the keychain and has to be removed before this command writes the entry: %s",
			clierr.ShellQuote(name), ce.Message))
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"a credential stored under %s that no entry names is in the keychain, which cannot be reached to remove it before this command writes the entry: %v; make the keychain reachable, or use a different --config, then run this command again",
		clierr.ShellQuote(name), err)).
		WithCause(err)
}

// orphanHelperError refuses a command creating an entry whose name has an
// orphan credential a helper the directory reads from will not erase: left
// in place, it would be served to a reader of the entry the command writes.
// A helper that is unavailable keeps its own code and remedy.
func orphanHelperError(name, helper string, err error) error {
	if ce := asStoreUnavailable(err); ce != nil {
		return ce.WithMessage(fmt.Sprintf(
			"a credential stored under %s that no entry names is held by the credential helper %s and has to be erased before this command writes the entry: %s",
			clierr.ShellQuote(name), helper, ce.Message))
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"a credential stored under %s that no entry names is held by the credential helper %s, which could not erase it before this command writes the entry: %v; fix the helper, or use a different --config, then run this command again",
		clierr.ShellQuote(name), helper, err)).
		WithCause(err)
}

// OrphanHelpers is the credential helpers whose orphans a login creating
// an entry removes: the one this directory's readers ask (its effective
// policy) and the one the login commits to (loginPolicy), when either is
// a helper.
func OrphanHelpers(cfg *Config, loginPolicy string) []string {
	var out []string
	if p, err := EffectivePolicy(cfg.CredentialStore); err == nil && isExecBackend(p) {
		out = append(out, p)
	}
	if isExecBackend(loginPolicy) && !slices.Contains(out, loginPolicy) {
		out = append(out, loginPolicy)
	}
	return out
}

// WithNotices carries the work a command did before it failed - the
// removals, the store change's steps - on the error it returns, so none of
// it goes unannounced. err is returned as it is when there are none, and a
// cancellation passes through unchanged so it still reads as one.
func WithNotices(err error, notices []string) error {
	if err == nil || len(notices) == 0 {
		return err
	}
	return clierr.WrapUnlessCanceled(err, func(err error) error {
		return clierr.Wrap(err, err.Error()).WithWarnings(notices...)
	})
}

func orphanRemovedNotice(name, store string) string {
	return fmt.Sprintf("removed a credential stored under %s that no entry named, from %s", name, store)
}
