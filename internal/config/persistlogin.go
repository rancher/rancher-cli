package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// StoreDecision is what a plugin-created login decided before its
// interactive login, re-checked under the config lock before any write:
// the setting had to be on, and the credential store the user was told
// about is the one the commit will reach.
type StoreDecision struct {
	// RequireSetting aborts when TokenReuse is off at commit time.
	RequireSetting bool
	// Policy is the login's EffectivePolicy at decision time.
	Policy string
	// DisclosedPlaintextPath is the credentials file the flow warned about
	// before the sign-in's first prompt, or empty when no plaintext warning
	// was given.
	DisclosedPlaintextPath string
}

// PendingTrust is a guided-trust CA accepted during this login, written
// only after the credential commit succeeded, as auth login does today.
type PendingTrust struct {
	CA, Fingerprint string
}

// PersistLoginInput is everything the primitive needs to persist one login
// credential and its entry. It performs no network call: revocation of the
// tokens the caller created is the caller's, after this returns.
type PersistLoginInput struct {
	// Name is the entry to write. With CollisionSuffix, a name held by a
	// different-URL entry takes the next free -2, -3, … suffix; without it
	// that collision is a CONFLICT.
	Name            string
	CollisionSuffix bool
	CanonicalURL    string
	// CA and CAFingerprint are persisted only on a create; an update never
	// changes trust (PendingTrust is the one exception).
	CA, CAFingerprint string
	Provenance        clierr.Provenance
	TokenRef          *clierr.TokenRef
	TokenUserID       string
	Bearer, ExpiresAt string
	// OwnsEntry allows creating an absent entry; auth login never does.
	OwnsEntry bool
	// Decision, when non-nil, is revalidated under the lock first.
	Decision *StoreDecision
	// ReadOnlyStoreIsFailure makes ErrTokenStoreUnsupported a failure with
	// the entry rolled back; false keeps auth login's warn-and-succeed.
	ReadOnlyStoreIsFailure bool
	// Policy is the credential-store policy this login resolved before it
	// authenticated. Every policy read under the lock uses Policy rather
	// than the loaded declaration, so a login that carries its own store
	// resolves its backend from that one. Empty means the caller observed
	// no policy of its own and the declaration read under the lock is the
	// login's policy, which is what it is for a login that changes
	// nothing.
	Policy string
	// ObservedDeclaration is config.yaml's credentialStore as this login
	// read it before authenticating. With Policy set, the value under the
	// lock must still equal it: a concurrent config edit or a login with a
	// different store means this login resolved against a directory that
	// no longer exists as it read it.
	ObservedDeclaration string
	// Declaration is written to config.yaml after the credential commits,
	// never before: readers that take no lock would otherwise select the
	// destination store while it still holds the previous login's
	// credential. Empty writes nothing.
	Declaration string
	// OpenStore opens the store the credential commits to. It is called
	// with an ALREADY-PHYSICAL backend - file, keychain or exec:<name> -
	// resolved under the lock, never with a policy: an answer cannot be
	// re-resolved against the variable, and a physical value never reads
	// or writes the pin. Production passes OpenResolvedStore.
	OpenStore func(backend, dir string) (CredStore, string, error)
	// PreCheck runs under the lock on the freshly loaded config, before
	// revalidation and any write; auth login's snapshot check lives here.
	PreCheck     func(cfg *Config) error
	PendingTrust *PendingTrust
	// Now stamps supersededAt; nil means time.Now.
	Now func() time.Time
}

// PersistLoginOutcome is what config.yaml and the store hold when the
// primitive returns, plus the components the caller composes into a
// LoginFailureDetails or a success result.
type PersistLoginOutcome struct {
	// Name is the entry written, after collision suffixing.
	Name string
	// Aborted is the revalidation reason when the primitive stopped
	// before any write.
	Aborted     string
	ConfigState clierr.ConfigState
	// Commit is the backend's own account; nil when the store was never
	// reached.
	Commit *CommitOutcome
	// CredentialState is Commit.State, or compensated/compensationFailed
	// when a trust write failed after a committed credential.
	CredentialState   clierr.CredentialState
	CompensationError string
	// SupersededTokens is the written entry's record list, for the
	// caller's retirement pass.
	SupersededTokens []SupersededToken
	// CacheBindingID is the entry's cache binding id after this login, so
	// a caller that goes on to write a cache record binds it to the login
	// it just made without loading the config a second time.
	CacheBindingID string
	// CacheWritesPaused says the entry's cache writes are still paused
	// after this login: a pin step it left would move readers, or its last
	// write failed.
	CacheWritesPaused bool
	ConfigRecovery    *clierr.ConfigRecovery
	// written is the config as this login's last write wrote it, which
	// is what a cache record for the credential it stored is bound to, and
	// committedTo is the store that credential went to.
	written     *Config
	committedTo string
	// Warnings are the store and pin notices, in order.
	Warnings           []string
	CurrentServer      string
	CurrentServerValid bool
}

// CacheBindingOver is the binding of the cache record for the credential
// this login stored, for a call whose own lookup found none: lookup is
// that lookup's binding, from the config load it made after the
// plugin-login lock was taken, before the credentials it read came up
// empty. Every entry it is bound to keeps the id that load showed, except
// the entry this login wrote, which is bound with the id its last write
// wrote, and added to the list when the login created it. A call that
// identified the entry this login wrote gets a record made from the
// identified entry's own credential, bound to it alone. The store checked
// is the one the commit reached, resolved from the config that write
// wrote. A login that returned before that write, or a lookup binding no
// constructor built, gives the zero binding, which no cache write accepts.
func (o PersistLoginOutcome) CacheBindingOver(dir string, lookup CacheBinding) CacheBinding {
	if o.written == nil || !lookup.built {
		return CacheBinding{}
	}
	var b CacheBinding
	if lookup.identified == o.Name {
		b = CacheBindingForLookup(dir, o.written, "", o.Name, o.Name, false)
		b.entry.paused = b.entry.paused || o.CacheWritesPaused
	} else {
		b = lookup.withWritten(o.written, o.Name, o.CacheWritesPaused)
	}
	b.source = o.committedTo
	return b
}

// ErrPersistAborted wraps a revalidation abort: the setting was turned
// off, or the backend became the plaintext file without the disclosure the
// flow gave.
var ErrPersistAborted = errors.New("login persistence aborted")

// persistWriteHook lets a test fail one of the primitive's config writes
// by step, without touching disk. The step is whatever name the write
// site passes, so a new write is reachable by naming it rather than by
// adding a second hook. Nil in production.
var persistWriteHook func(step string) error

// SetPersistWriteHookForTest sets persistWriteHook; call the returned
// restore in cleanup. Tests using it must not run in parallel.
func SetPersistWriteHookForTest(fn func(step string) error) (restore func()) {
	old := persistWriteHook
	persistWriteHook = fn
	return func() { persistWriteHook = old }
}

func writeStep(step string, cfg *Config) error {
	if persistWriteHook != nil {
		if err := persistWriteHook(step); err != nil {
			return err
		}
	}
	return cfg.Write()
}

// WriteStep is a config write outside the login primitive that a test
// can fail by step name, so a command can exercise what it does when its
// own write fails - a warning it reports, or a state it names.
func WriteStep(step string, cfg *Config) error { return writeStep(step, cfg) }

// PersistLoginCredential persists a login credential and its server entry
// under the config lock, metadata first, then the credential commit, then
// the outcome handling: on a nil-error commit nothing more is written (a
// pending trust write excepted); on an error-carrying commit the entry is
// removed (create) or restored to its snapshot (update), and a failed
// restore is reported as rollbackFailed with the complete prior state.
// A CLI-created prior token becomes a supersededTokens record; the caller
// retires it after this returns. CurrentServer is never changed.
func PersistLoginCredential(ctx context.Context, dir string, in PersistLoginInput) (PersistLoginOutcome, error) {
	out := PersistLoginOutcome{ConfigState: clierr.ConfigNotWritten, CredentialState: clierr.CredentialNotWritten}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	err := WithLock(ctx, dir, func() error {
		// One take of the transition lock, around the whole body: the
		// prelude, the manifest write, every replayed step and the config
		// writes between them are one change to this directory, and
		// withDirLock is not reentrant, so anything nested would wait on
		// this very process until the lock deadline.
		return withTransitionLock(ctx, dir, func() error {
			return persistUnderLocks(ctx, dir, in, &out, now)
		})
	})
	return out, err
}

// persistUnderLocks is PersistLoginCredential's body, with the config lock
// and the transition lock held.
func persistUnderLocks(ctx context.Context, dir string, in PersistLoginInput, out *PersistLoginOutcome, now func() time.Time) error {
	cfg, err := Load(FilePath(dir))
	if err != nil {
		return err
	}
	if in.PreCheck != nil {
		if err := in.PreCheck(cfg); err != nil {
			return err
		}
	}
	if reason := revalidate(cfg, in); reason != "" {
		out.Aborted = reason
		return fmt.Errorf("%w: %s", ErrPersistAborted, reason)
	}
	// The declaration loaded here is what a rollback restores and what
	// the post-commit write replaces.
	declared := cfg.CredentialStore
	policy := in.Policy
	if policy == "" {
		policy = declared
	}

	// The entry name is resolved before the store is: whether other
	// entries keep this login on the file store depends on which entry
	// this login writes, and for a caller that suffixes a collision that
	// is not the name it asked for.
	name, existing, exists, err := resolveEntryName(cfg, in)
	if err != nil {
		return err
	}
	out.Name = name
	var snap *Server
	if exists {
		s := existing.Clone()
		snap = &s
	}
	rb := loginRollback{dir: dir, name: name, snap: snap, declared: declared}

	// The prelude retries the change this directory already has, and
	// decides whether this login may proceed at all. Its answer to
	// "does this login change the store" comes from a backend set
	// resolved before the retry; every other use comes from the one
	// resolved after it, because a retry can write the pin and delete an
	// old copy.
	intent, err := loginBackendsOf(dir, cfg, name, policy, in.Declaration)
	if err != nil {
		return err
	}
	notices, err := prepareMutationLocked(ctx, dir, cfg, MutationIntent{ChangesStore: intent.ChangesStore()})
	out.Warnings = append(out.Warnings, notices...)
	if err != nil {
		return err
	}
	// What a rollback puts back is the change's name as the prelude left
	// it: a finished one is already forgotten, and restoring it would make
	// the record this login writes next, if a rollback cannot remove it,
	// read as a change this directory does not name.
	rb.transition = cfg.CredentialTransitionID
	b, err := loginBackendsOf(dir, cfg, name, policy, in.Declaration)
	if err != nil {
		return err
	}
	// The authority for the refusals a change of the store readers use
	// gets; the same check before the sign-in only saves a sign-in.
	if err := checkStoreChange(dir, cfg, name, b); err != nil {
		return err
	}
	if reason := undisclosedPlaintext(dir, in, b); reason != "" {
		out.Aborted = reason
		return fmt.Errorf("%w: %s", ErrPersistAborted, reason)
	}
	if b.Notice != "" {
		out.Warnings = append(out.Warnings, b.Notice)
	}

	// A login that creates its entry removes any credential left under
	// that name first, from the keychain, the file store and every helper
	// this directory's records name for it: written next to it, the entry
	// would make that orphan count, a reader whose store variable names
	// that helper would be served it, and an unpinned directory would read
	// as keeping credentials in both stores for every entry until the
	// login ends.
	if !exists {
		ns, oerr := RemoveOrphanCredentials(ctx, dir, cfg, name)
		out.Warnings = append(out.Warnings, ns...)
		if oerr != nil {
			return oerr
		}
	}

	// The change's first write: the entry's cache binding id advances and
	// its cache writes pause before any of its credential is written, so
	// every cache record made so far is a miss - one a writer that loaded
	// earlier writes later included - and none is written until the last
	// write resumes them. The prelude and the orphan removal above touch no
	// credential a reader of this entry is served: a replay deletes from a
	// store readers already left, and an orphan has no entry.
	entry := newEntry(in, snap, now())
	if berr := StartCredentialChange(&entry); berr != nil {
		return berr
	}
	cfg.Servers[name] = entry
	if werr := writeStep("entry", cfg); werr != nil {
		out.ConfigState = clierr.ConfigWriteFailed
		return werr
	}
	out.ConfigState = clierr.ConfigWritten

	// The pre-commit step. The record of what this change has to do is written
	// before the credential commits and is inert until a config write
	// names its id, so a login that dies here leaves nothing to
	// finish and nothing half-done.
	var change storeChange
	if b.ChangesStore() {
		ch, perr := planStoreChange(dir, cfg, name, in.CanonicalURL, b)
		out.Warnings = append(out.Warnings, ch.notices...)
		// Before the error: the plan comes back as soon as the record
		// reached the disk, and an undo has to remove one that did even
		// when the step it failed on came after the write.
		rb.manifest = ch.plan != nil
		if perr != nil {
			return rb.undo(out, cfg, perr)
		}
		change = ch
	}
	plan := change.plan

	// The store is opened from the backend resolved above, never from
	// the policy: an already-physical value never reads or writes the
	// pin, so the only pin this login writes is its own last step.
	store, storeNotice, err := in.OpenStore(b.CommitsTo, dir)
	if err != nil {
		return rb.undo(out, cfg, err)
	}
	if storeNotice != "" {
		out.Warnings = append(out.Warnings, storeNotice)
	}

	ref := ServerRef{Name: name, URL: in.CanonicalURL}
	// A keychain write is addressed by the entry's identity, so the
	// identity is decided and recorded before the credential is
	// written and settled after it. The store the login opened - the
	// destination of the change when there is one, which can move an
	// auto directory from the file store to the keychain inside this
	// very login - is what decides whether that applies.
	var write *keychainWrite
	if ks, ok := keychainStoreFor(store); ok {
		w, werr := beginKeychainWrite(ctx, ks, ref, in.Bearer)
		if werr != nil {
			return rb.undo(out, cfg, werr)
		}
		write = w
	}
	var prior PriorCredential
	if write != nil {
		prior = write.Prior
	}

	commit, cerr := CommitToken(store, ref, in.Bearer, prior)
	if write != nil {
		if ferr := write.finish(ctx, commit.State); ferr != nil {
			out.Commit = &commit
			out.CredentialState = commit.State
			idErr := errors.Join(cerr, fmt.Errorf("recording the credential identity for %s: %w", name, ferr))
			if commit.State == CommitNotWritten {
				// Nothing was stored, so the entry goes back the way
				// every other empty-handed credential failure sends
				// it; leaving the rewritten entry would replace a
				// working login's URL, CA and provenance with this
				// failed one's.
				return rb.undo(out, cfg, idErr)
			}
			// The credential did land: the entry stays as the handle
			// for reaching it, and the record says a login for this
			// entry has not settled until the next one settles it.
			// The change is still unnamed, and nothing will ever name
			// it now, so it goes.
			out.Warnings = appendNotice(out.Warnings, rb.dropManifest())
			return idErr
		}
		if commit.Compensate != nil {
			// A compensation has to undo both halves: the credential
			// and the identity that names it. Either half failing is
			// one failed compensation, and the message names both
			// accounts because neither is addressable from the other
			// once the record is gone.
			undo := commit.Compensate
			commit.Compensate = func() error {
				if err := undo(); err != nil {
					return fmt.Errorf("%w (%s)", err, write.accounts())
				}
				if err := write.revert(ctx); err != nil {
					return fmt.Errorf("%w (%s)", err, write.accounts())
				}
				return nil
			}
		}
	}
	out.Commit = &commit
	out.CredentialState = commit.State
	if cerr != nil {
		switch {
		case errors.Is(cerr, ErrTokenStoreUnsupported) && !in.ReadOnlyStoreIsFailure:
			// auth login's warn-and-succeed: nothing was stored, the
			// entry goes back to its snapshot, and trust the user
			// accepted during this login is still persisted. A login
			// that named a store still declares it, and the change
			// it recorded - this entry's old copy, which the helper
			// will never hold - is put into effect by that same
			// write, so the restored entry stops naming a token that
			// is scheduled for deletion and that the helper creates
			// on demand anyway.
			declares := in.Declaration != "" && in.Declaration != declared
			restored := restoredEntry(snap, in.PendingTrust, declares && plan != nil)
			if restored == nil {
				delete(cfg.Servers, name)
			} else {
				cfg.Servers[name] = keepsTheChangesID(cfg, name, *restored)
			}
			if declares {
				cfg.CredentialStore = in.Declaration
				if plan != nil {
					cfg.CredentialTransitionID = plan.ID()
				}
			} else {
				out.Warnings = appendNotice(out.Warnings, rb.dropManifest())
			}
			// The change this write puts into effect has delete steps only,
			// which hold no pause: this write is its last.
			ResumeCacheWrites(dir, cfg, name)
			// Named by what it carries, like every other write after the
			// commit: a test that fails the declaration write has to be
			// able to fail this one. Trust is not what names it - this
			// write restores an entry rather than settling a commit, and
			// it has carried an accepted certificate since before it
			// carried anything else - so a write with nothing new in it
			// is still the rollback it always was.
			step := activationStep(false, declares, declares && plan != nil)
			if step == "" {
				step = "rollback"
			}
			if werr := writeStep(step, cfg); werr != nil {
				return rollbackFailed(out, name, snap, werr, cerr)
			}
			if in.PendingTrust != nil || declares {
				out.ConfigState = clierr.ConfigWritten
			} else {
				out.ConfigState = clierr.ConfigNotWritten
			}
			return cerr
		case commit.State == CommitPossiblyCommitted && snap == nil:
			// The store may hold the credential and the caller will
			// revoke it: keep the entry as the `auth logout` handle,
			// without naming a token that is about to be revoked. Its id is
			// the change's, never an earlier one, and this last write
			// resumes it.
			cur := cfg.Servers[name]
			cfg.Servers[name] = Server{URL: in.CanonicalURL, CA: in.CA, CAFingerprint: in.CAFingerprint, Provenance: in.Provenance,
				CacheBindingID: cur.CacheBindingID, CacheWritesPaused: cur.CacheWritesPaused}
			out.Warnings = appendNotice(out.Warnings, rb.dropManifest())
			ResumeCacheWrites(dir, cfg, name)
			if werr := writeStep("rollback", cfg); werr != nil {
				return rollbackFailed(out, name, snap, werr, cerr)
			}
			out.ConfigState = clierr.ConfigWritten
			return cerr
		default:
			return rb.undo(out, cfg, cerr)
		}
	}
	if commit.Notice != "" {
		out.Warnings = append(out.Warnings, commit.Notice)
	}
	// The declaration write. The first config write after the commit carries
	// the declaration, the trust and the id of the change, and is what puts all
	// three into effect: until the credential is in the destination store, a
	// reader that takes no lock would resolve a new declaration to a store
	// still holding the previous login's credential, and a change named before
	// the commit would be replayed for a login that never happened.
	declares := in.Declaration != "" && in.Declaration != declared
	if declares {
		cfg.CredentialStore = in.Declaration
	}
	if plan != nil {
		cfg.CredentialTransitionID = plan.ID()
	}
	switch step := activationStep(in.PendingTrust != nil, declares, plan != nil); step {
	case "":
	case "trust":
		entry.CA, entry.CAFingerprint = in.PendingTrust.CA, in.PendingTrust.Fingerprint
		cfg.Servers[name] = entry
		if werr := writeStep(step, cfg); werr != nil {
			if commit.Compensate != nil {
				if cerr := commit.Compensate(); cerr != nil {
					out.CredentialState, out.CompensationError = clierr.CredentialCompensationFailed, cerr.Error()
				} else {
					out.CredentialState = clierr.CredentialCompensated
				}
			}
			return rb.undo(out, cfg,
				clierr.New(clierr.CodeGeneric, fmt.Sprintf("storing the accepted certificate for %s: %s", name, werr.Error())).
					WithHint(LoginHintFor(name)))
		}
		entry = cfg.Servers[name]
	default:
		if werr := writeStep(step, cfg); werr != nil {
			// Not a rollback: the credential is committed in the store
			// this login opened and is the directory's own. What is
			// missing is the record of where it now lives - and, when
			// the login was changing the store, the change itself,
			// which never started and is removed rather than left for
			// a config write that will not come.
			out.Warnings = appendNotice(out.Warnings, rb.dropManifest())
			cfg.CredentialStore, cfg.CredentialTransitionID = rb.declared, rb.transition
			if declares {
				out.ConfigState = clierr.ConfigDeclarationNotWritten
				return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
					"the credential for %s was stored, but recording that this directory keeps credentials in %q failed: %s",
					name, in.Declaration, werr.Error())).
					WithHint(LoginHintFor(name))
			}
			out.ConfigState = clierr.ConfigTransitionNotRecorded
			return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
				"the credential for %s was stored, but starting the credential store change failed: %s; nothing was moved",
				name, werr.Error())).
				WithHint(LoginHintFor(name))
		}
	}
	if plan != nil {
		// The write put the change into effect. If the record of what it
		// has to do is not the one that was named - gone, or replaced by
		// another process with the flock skipped - no command can finish
		// work nobody has the record of, and every arm of the switch above
		// wrote the name, so every arm has to look.
		state, _, terr := LoadTransition(dir, cfg)
		switch {
		case terr != nil:
			// The record is there and cannot be read. Running the login
			// again meets the same file, so the remedy is the one the
			// read itself names, and the hint is the report that shows it.
			out.ConfigState = clierr.ConfigTransitionLost
			return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
				"the credential for %s was stored, but the record of the credential store change cannot be read: %s; the change did not run",
				name, terr.Error())).
				WithCause(terr).WithHint(StatusCommand)
		case state != TransitionActive:
			// Finished: none of its steps can run, and none is left to
			// hold a pause. A writer that read the store readers have left
			// is refused by the store check, so this write resumes.
			out.ConfigState = clierr.ConfigTransitionLost
			out.Warnings = appendNotice(out.Warnings, resumeLast(dir, cfg, name))
			return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
				"the credential for %s was stored, but the record of the credential store change is no longer the one this login wrote; the change did not run",
				name)).
				WithHint(LoginHintFor(name))
		}
	}

	// The old-copy step. When a reader still resolves to the store this login
	// is leaving, the old copy of this entry's credential goes now, and if it
	// cannot go the pin flips instead, so readers stop resolving to it. When
	// neither can be done the login says so and stops: readers still resolve
	// the previous credential, and the pin step left undone keeps this entry's
	// cache writes paused until a later login finishes it.
	if plan != nil && b.WritesPin() {
		ns, ok := runOldCopyOrPin(ctx, dir, cfg, change, name)
		out.Warnings = append(out.Warnings, ns...)
		if !ok {
			out.ConfigState = clierr.ConfigStoreChangeNotApplied
			return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
				"the credential for %s was stored, but neither the old copy nor the credential store pin could be changed, so this directory still reads the previous credential",
				name)).
				WithHint(LoginHintFor(name))
		}
	}

	// The replay. Everything the change still has to do, in order, under
	// the lock this body already holds. A step that could not finish
	// is a warning and stays for the next credential-mutating
	// command: the credential is committed and recorded, and the work
	// left over is the directory's, not this login's to fail on.
	if plan != nil && !plan.Empty() {
		ns, retained, rerr := replayPass(ctx, dir, cfg, &plan.manifest)
		out.Warnings = append(out.Warnings, ns...)
		out.Warnings = append(out.Warnings, retained...)
		if rerr != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"the credential store change could not be recorded as it ran: %s", rerr.Error()))
		}
	}

	// The change's last write. It resumes every entry's cache writes a
	// resumer may clear - this one's unless a pin step it left would move
	// readers - and forgets the name of a change whose last step
	// completed. A failure leaves the pause for the next miss or change.
	notice := resumeLast(dir, cfg, name)
	out.Warnings = appendNotice(out.Warnings, notice)
	entry = cfg.Servers[name]
	out.SupersededTokens = entry.SupersededTokens
	out.CacheBindingID = entry.CacheBindingID
	out.CacheWritesPaused = entry.Paused() || notice != ""
	out.written, out.committedTo = cfg, b.CommitsTo
	out.CurrentServer = cfg.CurrentServer
	if out.CurrentServer != "" {
		_, out.CurrentServerValid = cfg.Servers[out.CurrentServer]
	}
	return nil
}

// revalidate re-reads the declaration and the setting under the lock and
// returns the abort reason, or "" to proceed. Pure reads only: nothing
// here opens a store or writes a pin.
func revalidate(cfg *Config, in PersistLoginInput) string {
	if in.Policy != "" && cfg.CredentialStore != in.ObservedDeclaration {
		return fmt.Sprintf("the credential store this directory declares changed during the login (%s); run the command again",
			declarationText(cfg.CredentialStore))
	}
	if in.Decision == nil {
		return ""
	}
	d := *in.Decision
	if d.RequireSetting && !cfg.TokenReuse {
		return "token reuse was turned off during the login (" + TokenReuseOffCommand + ")"
	}
	return ""
}

// undisclosedPlaintext is the abort reason for a login about to commit to
// the plaintext file store without having said so before the sign-in's
// first prompt, or "" to proceed. b is the login's own answer under the
// lock, the same one its check before the sign-in gave from what it read
// then: this catches the directory becoming a plaintext store while the
// sign-in was open. Anything else that answer refuses - a keychain it
// cannot use on a directory that reads the keychain, credentials in both
// stores - was already returned as that answer's own typed error, never
// folded into this abort.
func undisclosedPlaintext(dir string, in PersistLoginInput, b loginBackends) string {
	if in.Decision == nil || in.Decision.Policy == "" {
		return ""
	}
	if b.CommitsTo == backendFile && in.Decision.DisclosedPlaintextPath != CredentialsFilePath(dir) {
		return fmt.Sprintf("the credential store is now the plaintext file %s, which this login did not disclose before the interactive sign-in; run the command again", CredentialsFilePath(dir))
	}
	return ""
}

// DeclarationFor names what a login writes to config.yaml after its
// credential commits: the store it was asked for when it carries one,
// nothing when the directory already declares a store and this login does
// not change it, and otherwise the policy this login used, so a directory
// records where its credentials live from its first login on.
func DeclarationFor(declared, requested, policy string) string {
	switch {
	case requested != "":
		return requested
	case declared != "":
		return ""
	default:
		return policy
	}
}

// declarationText names a declaration for a message, including the state
// of having none.
func declarationText(declared string) string {
	if declared == "" {
		return "it now declares none"
	}
	return "it now declares " + declared
}

// CollisionFreeName is the entry a login that suffixes a collision writes
// for name at canonicalURL: name itself when it is free or already at that
// URL, else the next free -2, -3, ... suffix. A check before the sign-in
// asks about this entry, the one the login under the lock will write, so
// the two agree on which entries are "the other entries".
func CollisionFreeName(cfg *Config, name, canonicalURL string) string {
	resolved, _, _, err := resolveEntryName(cfg, PersistLoginInput{
		Name: name, CanonicalURL: canonicalURL, CollisionSuffix: true, OwnsEntry: true,
	})
	if err != nil {
		return name
	}
	return resolved
}

// resolveEntryName applies the collision policy under the lock.
func resolveEntryName(cfg *Config, in PersistLoginInput) (name string, existing Server, exists bool, err error) {
	name = in.Name
	for i := 2; ; i++ {
		existing, exists = cfg.Servers[name]
		if !exists || SameServerURL(existing.URL, in.CanonicalURL) {
			break
		}
		if !in.CollisionSuffix {
			return "", Server{}, false, clierr.New(clierr.CodeConflict,
				fmt.Sprintf("server entry %q is registered at %s, not %s", name, existing.URL, in.CanonicalURL))
		}
		name = fmt.Sprintf("%s-%d", in.Name, i)
	}
	if !exists && !in.OwnsEntry {
		return "", Server{}, false, clierr.New(clierr.CodeNotFound, fmt.Sprintf("server entry %q not found", in.Name))
	}
	return name, existing, exists, nil
}

// newEntry builds the entry write: a create carries the connection's trust;
// an update keeps the snapshot's trust and appends the snapshot's token as
// a record when the CLI created it.
func newEntry(in PersistLoginInput, snap *Server, now time.Time) Server {
	var e Server
	if snap == nil {
		e = Server{URL: in.CanonicalURL, CA: in.CA, CAFingerprint: in.CAFingerprint}
	} else {
		e = snap.Clone()
		if snap.CLIOwned() {
			e.SupersededTokens = append(e.SupersededTokens, SupersededToken{
				Ref: *snap.TokenRef, UserID: snap.TokenUserID, ExpiresAt: snap.CredentialExpiresAt,
				SupersededAt: now.UTC().Format(time.RFC3339), Server: CanonicalServerIdentity(snap.URL),
			})
		}
	}
	e.Provenance, e.TokenRef, e.TokenUserID, e.CredentialExpiresAt = in.Provenance, in.TokenRef, in.TokenUserID, in.ExpiresAt
	e.LoggedOutAt = ""
	return e
}

// restoredEntry is the snapshot with a pending trust applied; nil when the
// operation created the entry. dropToken clears the fields naming a
// credential the directory is about to stop having: the old copy is
// scheduled for deletion, and a helper that creates credentials on demand
// never held one to name.
func restoredEntry(snap *Server, trust *PendingTrust, dropToken bool) *Server {
	if snap == nil {
		return nil
	}
	r := snap.Clone()
	if trust != nil {
		r.CA, r.CAFingerprint = trust.CA, trust.Fingerprint
	}
	if dropToken {
		r.TokenRef, r.TokenUserID, r.CredentialExpiresAt = nil, "", ""
	}
	return &r
}

// loginRollback is everything a failed login puts back: the entry as it
// found it, the two config fields it may have set, and the record of the
// store change it may have written.
//
// The transition id matters as much as the declaration. Config.Write
// marshals it, so a rollback write that left it set would put into effect
// the very change the rollback is undoing, and the next command would
// replay it for a login that never committed.
type loginRollback struct {
	dir        string
	name       string
	snap       *Server
	declared   string
	transition string
	// manifest records that this login wrote one, so an undo removes it.
	manifest bool
}

// undo restores the entry and the config fields, removes the manifest and
// returns primary; a failed rollback write is reported instead. The
// entry keeps the id the change's first write gave it, and the rollback
// write is the change's last: records made after it under that id come
// from the credential it leaves, which is the one readers resolve.
func (r loginRollback) undo(out *PersistLoginOutcome, cfg *Config, primary error) error {
	out.Warnings = appendNotice(out.Warnings, r.dropManifest())
	cfg.CredentialStore, cfg.CredentialTransitionID = r.declared, r.transition
	if r.snap == nil {
		delete(cfg.Servers, r.name)
	} else {
		cfg.Servers[r.name] = keepsTheChangesID(cfg, r.name, *r.snap)
	}
	ResumeCacheWrites(r.dir, cfg, r.name)
	if werr := writeStep("rollback", cfg); werr != nil {
		return rollbackFailed(out, r.name, r.snap, werr, primary)
	}
	out.ConfigState = clierr.ConfigNotWritten
	return primary
}

// keepsTheChangesID is s as the entry a rollback writes: everything as it
// was before the login, but the cache binding id and the pause the
// change's first write gave it, which cfg still holds. An id never moves
// back: an earlier one would make every record the previous credential's
// writers made valid again, beside whatever credential the rollback
// could not remove.
func keepsTheChangesID(cfg *Config, name string, s Server) Server {
	cur := cfg.Servers[name]
	r := s.Clone()
	r.CacheBindingID, r.CacheWritesPaused = cur.CacheBindingID, cur.CacheWritesPaused
	return r
}

// resumeLast is a change's last write for a login that got as far as its
// commit: every pause a resumer may clear is cleared, a finished change's
// name is forgotten, and the notice for a write that failed is returned.
func resumeLast(dir string, cfg *Config, name string) string {
	ClearFinishedTransition(dir, cfg)
	ResumeCacheWrites(dir, cfg, name)
	if werr := writeStep("resume", cfg); werr != nil {
		return fmt.Sprintf("cache writes for %s stay paused (%s); the next exec-credential cache miss or credential change resumes them", name, werr.Error())
	}
	return ""
}

// dropManifest removes a manifest this login wrote and never named,
// returning the notice for a removal that did not work. Leaving one
// behind is not a failure of the login: an unnamed manifest is inert, and
// the next command's prelude removes it as abandoned work.
func (r loginRollback) dropManifest() string {
	if !r.manifest {
		return ""
	}
	if err := removeTransition(r.dir); err != nil {
		return fmt.Sprintf("the record of the credential store change that did not happen could not be removed: %s", err.Error())
	}
	return ""
}

// activationStep names the first config write after the commit by what it
// carries, so a test can fail exactly the write it means and every
// existing step name keeps the meaning it had.
func activationStep(trust, declares, activates bool) string {
	switch {
	case trust:
		return "trust"
	case declares:
		return "declaration"
	case activates:
		return "transition"
	}
	return ""
}

func rollbackFailed(out *PersistLoginOutcome, name string, snap *Server, werr, primary error) error {
	out.ConfigState = clierr.ConfigRollbackFailed
	out.ConfigRecovery = &clierr.ConfigRecovery{
		Entry: name,
		Prior: priorEntry(snap),
		// The entry name is operator-chosen and only prefix-checked, so it is
		// shell-quoted like every value a machine hint carries.
		Command: DeleteServerCommand + " " + clierr.ShellQuote(name),
	}
	return fmt.Errorf("%w (restoring the config entry also failed: %v)", primary, werr)
}

// priorEntry projects the snapshot onto the recovery component; the
// fingerprint is the stored one, else computed from the CA so an operator
// can compare it against the server.
func priorEntry(snap *Server) *clierr.PriorEntry {
	if snap == nil {
		return nil
	}
	fp := snap.CAFingerprint
	if fp == "" {
		fp = caFingerprintOf(snap.CA)
	}
	return &clierr.PriorEntry{URL: snap.URL, CA: snap.CA, CAFingerprint: fp, Provenance: snap.Provenance,
		TokenRef: snap.TokenRef, TokenUserID: snap.TokenUserID, CredentialExpiresAt: snap.CredentialExpiresAt,
		SupersededTokens: snap.SupersededTokens}
}

// caFingerprintOf is the SHA-256 of the first certificate in selector
// (inline PEM or a readable path, read the one way ResolveCA reads it),
// lowercase hex, the encoding tlstrust.Fingerprint produces; "" when
// nothing can be read.
func caFingerprintOf(selector string) string {
	pemText, err := ResolveCA(selector)
	if err != nil || pemText == "" {
		return ""
	}
	data := []byte(pemText)
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return ""
		}
		if block.Type == "CERTIFICATE" {
			sum := sha256.Sum256(block.Bytes)
			return hex.EncodeToString(sum[:])
		}
	}
}

// UpdateServerEntry reloads the config under the lock, lets fn change one
// entry, and writes only when fn reports a change. The retirement pass
// uses it for its one record write; auth logout stamps loggedOutAt with
// the same shape.
func UpdateServerEntry(ctx context.Context, dir, name string, fn func(s *Server) (changed bool)) error {
	return WithLock(ctx, dir, func() error {
		cfg, err := Load(FilePath(dir))
		if err != nil {
			return err
		}
		s, ok := cfg.Servers[name]
		if !ok {
			return clierr.New(clierr.CodeNotFound, fmt.Sprintf("server entry %q not found", name))
		}
		if !fn(&s) {
			return nil
		}
		cfg.Servers[name] = s
		return writeStep("record", cfg)
	})
}
