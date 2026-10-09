package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rancher/rancher-cli/credhelper"
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
// keychain, the file store, and every credential helper this directory's
// helper records name for that entry name - whichever helper the
// creating process uses, and whether or not the directory declares a
// store. A helper is asked directly, never through its records, and a
// reader's credential store variable can name any helper, so a credential
// left in a recorded helper would be served to a reader of the new entry.
// A helper that cannot erase it (not on PATH, or failing), or a keychain
// that cannot be reached, refuses with the error; erasures that finished
// before it stay done, and the caller then writes nothing more. The caller
// holds the config lock and writes the entry after this returns.
func RemoveOrphanCredentials(ctx context.Context, dir string, cfg *Config, name string) ([]string, error) {
	if _, ok := cfg.Servers[name]; ok {
		return nil, nil
	}
	var notices []string
	// Every helper record is written under the config lock the caller
	// holds, so a directory without a records file has none to erase and
	// needs neither the records lock nor a lock file.
	if _, serr := os.Stat(filepath.Join(dir, helperRecordsFileName)); !errors.Is(serr, fs.ErrNotExist) {
		if err := withHelperRecordsLock(ctx, dir, func(st *helperRecords) error {
			for _, e := range st.forServer(name) {
				if err := checkRecordedHelperOnPath(dir, name, e.Helper); err != nil {
					return err
				}
				if _, eerr := eraseHelperCredential(dir, e); eerr != nil {
					return orphanHelperError(dir, name, e.Helper, eerr)
				}
				st.remove(e)
				notices = append(notices, orphanRemovedNotice(name, "the credential helper "+e.Helper))
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

// CheckOrphanCredentials is RemoveOrphanCredentials' refusals alone, for the
// check before the sign-in: a helper this directory's records name for
// name that is not on PATH, or an orphan in a keychain this process cannot
// reach, refuses the login, since the removal under the lock would. It
// reads the helper records without the lock, creating no directory and no
// lock file, and the orphan's keychain slot, and writes nothing. A helper
// that is on PATH but fails to erase is found only by the removal itself.
func CheckOrphanCredentials(dir string, cfg *Config, name string) error {
	if _, ok := cfg.Servers[name]; ok {
		return nil
	}
	st, err := readHelperRecords(dir)
	if err != nil {
		return err
	}
	for _, e := range st.forServer(name) {
		if err := checkRecordedHelperOnPath(dir, name, e.Helper); err != nil {
			return err
		}
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

// checkRecordedHelperOnPath refuses with orphanHelperError when helper's
// binary is not on PATH, found the way an erase finds it.
func checkRecordedHelperOnPath(dir, name, helper string) error {
	binary := helperBinary(helper)
	if _, err := exec.LookPath(binary); err != nil {
		return orphanHelperError(dir, name, helper,
			helperUnavailable(binary, credhelper.VerbErase, name, "it is not installed in PATH"))
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

// orphanHelperError refuses a command creating an entry whose name has a
// leftover credential in a helper this directory's records name, when that
// helper will not erase it: left in place, it would be served to a reader
// of the entry the command writes. A helper that is unavailable keeps its
// own code. The remedy is the same for every recorded helper, declared or
// not: make the helper work, use another directory, or drop the record when
// the helper is gone.
func orphanHelperError(dir, name, helper string, err error) error {
	cause := err.Error()
	ce := asStoreUnavailable(err)
	if ce != nil {
		cause = ce.Message
	}
	msg := fmt.Sprintf(
		"the credential helper %s holds a credential stored under %s that no entry names and could not erase it (%s); make %s available on PATH (or working) and run this command again, or use a different --config; if %s is gone for good, remove the %s record for %s from %s",
		helper, clierr.ShellQuote(name), cause, helper, helper, clierr.ShellQuote(name), helper,
		filepath.Join(CanonicalDir(dir), helperRecordsFileName))
	if ce != nil {
		return ce.WithMessage(msg)
	}
	return clierr.New(clierr.CodeValidation, msg).WithCause(err)
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
