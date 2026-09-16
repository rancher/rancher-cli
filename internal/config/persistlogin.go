package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
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
	// Policy and Pin are EffectivePolicy and ReadPin at decision time.
	Policy, Pin string
	// DisclosedPlaintextPath is the credentials file the flow warned about
	// before the login, or empty when no plaintext warning was given.
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
	// authenticated. Every
	// policy read under the lock uses Policy rather than the loaded
	// declaration, so a login that carries its own store opens that one
	// and RepinForLogin is a no-op unless this login's policy is auto.
	// Empty means the caller observed no policy of its own and the
	// declaration read under the lock is the login's policy, which is what
	// it is for a login that changes nothing.
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
	// OpenStore resolves the store from the policy read under the lock;
	// production passes OpenStoreWithNotice.
	OpenStore func(policy, dir string) (CredStore, string, error)
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
	ConfigRecovery *clierr.ConfigRecovery
	// Warnings are the store and pin notices, in order.
	Warnings           []string
	CurrentServer      string
	CurrentServerValid bool
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
		cfg, err := Load(FilePath(dir))
		if err != nil {
			return err
		}
		if in.PreCheck != nil {
			if err := in.PreCheck(cfg); err != nil {
				return err
			}
		}
		if reason := revalidate(dir, cfg, in); reason != "" {
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
		entry := newEntry(in, snap, now())
		cfg.Servers[name] = entry
		if werr := writeStep("entry", cfg); werr != nil {
			out.ConfigState = clierr.ConfigWriteFailed
			return werr
		}
		out.ConfigState = clierr.ConfigWritten

		rawStore, storeNotice, err := in.OpenStore(policy, dir)
		if err != nil {
			return rollback(&out, cfg, name, snap, declared, err)
		}
		if storeNotice != "" {
			out.Warnings = append(out.Warnings, storeNotice)
		}
		store, pinNotice, err := RepinForLogin(dir, policy, rawStore)
		if err != nil {
			return rollback(&out, cfg, name, snap, declared, err)
		}
		if pinNotice != "" {
			out.Warnings = append(out.Warnings, pinNotice)
		}

		ref := ServerRef{Name: name, URL: in.CanonicalURL}
		// A keychain write is addressed by the entry's identity, so the
		// identity is decided and recorded before the credential is
		// written and settled after it. The store the login actually
		// opened - after RepinForLogin, which can move an auto directory
		// from the file store to the keychain inside this very login -
		// is what decides whether that applies.
		var write *keychainWrite
		if ks, ok := keychainStoreFor(store); ok {
			w, werr := beginKeychainWrite(ctx, ks, ref, in.Bearer)
			if werr != nil {
				return rollback(&out, cfg, name, snap, declared, werr)
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
					return rollback(&out, cfg, name, snap, declared, idErr)
				}
				// The credential did land: the entry stays as the handle
				// for reaching it, and the record says a login for this
				// entry has not settled until the next one settles it.
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
				// accepted during this login is still persisted.
				restored := restoredEntry(snap, in.PendingTrust)
				if restored == nil {
					delete(cfg.Servers, name)
				} else {
					cfg.Servers[name] = *restored
				}
				if werr := writeStep("rollback", cfg); werr != nil {
					return rollbackFailed(&out, name, snap, werr, cerr)
				}
				if in.PendingTrust != nil {
					out.ConfigState = clierr.ConfigWritten
				} else {
					out.ConfigState = clierr.ConfigNotWritten
				}
				return cerr
			case commit.State == CommitPossiblyCommitted && snap == nil:
				// The store may hold the credential and the caller will
				// revoke it: keep the entry as the `auth logout` handle,
				// without naming a token that is about to be revoked.
				cfg.Servers[name] = Server{URL: in.CanonicalURL, CA: in.CA, CAFingerprint: in.CAFingerprint, Provenance: in.Provenance}
				if werr := writeStep("rollback", cfg); werr != nil {
					return rollbackFailed(&out, name, snap, werr, cerr)
				}
				out.ConfigState = clierr.ConfigWritten
				return cerr
			default:
				return rollback(&out, cfg, name, snap, declared, cerr)
			}
		}
		if commit.Notice != "" {
			out.Warnings = append(out.Warnings, commit.Notice)
		}
		// The declaration rides the trust write when there is one and is a
		// write of its own otherwise, but either way it is the FIRST config
		// write after the commit: until the credential is in the
		// destination store, a reader that takes no lock would resolve the
		// new declaration to a store still holding the previous login's
		// credential.
		declares := in.Declaration != "" && in.Declaration != declared
		if declares {
			cfg.CredentialStore = in.Declaration
		}
		if in.PendingTrust != nil {
			entry.CA, entry.CAFingerprint = in.PendingTrust.CA, in.PendingTrust.Fingerprint
			cfg.Servers[name] = entry
			if werr := writeStep("trust", cfg); werr != nil {
				if commit.Compensate != nil {
					if cerr := commit.Compensate(); cerr != nil {
						out.CredentialState, out.CompensationError = clierr.CredentialCompensationFailed, cerr.Error()
					} else {
						out.CredentialState = clierr.CredentialCompensated
					}
				}
				return rollback(&out, cfg, name, snap, declared,
					clierr.New(clierr.CodeGeneric, fmt.Sprintf("storing the accepted certificate for %s: %s", name, werr.Error())).
						WithHint(LoginHintFor(name)))
			}
			entry = cfg.Servers[name]
		} else if declares {
			if werr := writeStep("declaration", cfg); werr != nil {
				// Not a rollback: the credential is committed in the store
				// this login opened and is the directory's own. What is
				// missing is the record of where it now lives, which the
				// next login writes.
				out.ConfigState = clierr.ConfigDeclarationNotWritten
				return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
					"the credential for %s was stored, but recording that this directory keeps credentials in %q failed: %s",
					name, in.Declaration, werr.Error())).
					WithHint(LoginHintFor(name))
			}
		}

		// The entry's cache binding id advances after the commit and
		// before any cleanup. `auth get-token` takes no config lock: one
		// that read the old id also read a store still holding the old
		// credential and caches under the old id, and one that reads the
		// new id reads a store that already holds the new credential.
		// Advancing before the commit would let the second of those cache
		// the previous user's credential under the new id.
		if berr := AdvanceCacheBinding(&entry); berr != nil {
			return berr
		}
		cfg.Servers[name] = entry
		if werr := writeStep("cachebinding", cfg); werr != nil {
			// Not a rollback either: the credential is committed and
			// recorded. What did not happen is the invalidation of the
			// cache records the previous login left, which stay usable
			// until they expire.
			out.ConfigState = clierr.ConfigCacheBindingNotAdvanced
			return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
				"the credential for %s was stored, but the exec-credential cache records of the previous login could not be invalidated: %s",
				name, werr.Error())).
				WithHint("rancher auth delete-token --all")
		}

		out.SupersededTokens = entry.SupersededTokens
		out.CacheBindingID = entry.CacheBindingID
		out.CurrentServer = cfg.CurrentServer
		if out.CurrentServer != "" {
			_, out.CurrentServerValid = cfg.Servers[out.CurrentServer]
		}
		return nil
	})
	return out, err
}

// revalidate re-reads the declaration, the setting and the effective
// backend under the lock and returns the abort reason, or "" to proceed.
// Pure reads only: openStoreResolved and RepinForLogin probe and write the
// pin and must not run here.
func revalidate(dir string, cfg *Config, in PersistLoginInput) string {
	if in.Policy != "" && cfg.CredentialStore != in.ObservedDeclaration {
		return fmt.Sprintf("the credential store this directory declares changed during the login (%s); run the command again",
			declarationText(cfg.CredentialStore))
	}
	if in.Decision == nil {
		return ""
	}
	d := *in.Decision
	if d.RequireSetting && !cfg.TokenReuse {
		return "token reuse was turned off during the login (rancher config set-token-reuse off)"
	}
	if d.Policy == "" {
		return ""
	}
	// The login's own policy, never the loaded declaration: a login that
	// carries one disclosed the backend that policy names before the
	// sign-in, and this re-check exists to catch the directory becoming
	// a plaintext store while the sign-in was open. Asking about the
	// declaration instead would compare the wrong two things and skip
	// the disclosure the caller owes.
	policy := in.Policy
	if policy == "" {
		policy = cfg.CredentialStore
	}
	_, _, backend, berr := ResolvedBackend(dir, policy)
	if berr != nil {
		return berr.Error()
	}
	if backend == "file" && d.DisclosedPlaintextPath != CredentialsFilePath(dir) {
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

// ResolvedBackend resolves the credential-store policy, the pin an auto
// policy consults, and the backend the two name.
func ResolvedBackend(dir, cfgPolicy string) (policy, pin, backend string, err error) {
	policy, err = EffectivePolicy(cfgPolicy)
	if err != nil {
		return "", "", "", err
	}
	if policy == "auto" {
		pin = ReadPin(dir)
	}
	return policy, pin, BackendOf(policy, pin), nil
}

// BackendOf names the physical backend a policy and pin resolve to without
// probing: file, keychain, exec, or "" when auto is still unpinned. The
// plugin-created login uses it to decide, before its interactive login,
// whether to disclose the plaintext file.
func BackendOf(policy, pin string) string {
	switch {
	case policy == "file", policy == "auto" && pin == "file":
		return "file"
	case policy == "keychain", policy == "auto" && pin == "keychain":
		return "keychain"
	case strings.HasPrefix(policy, "exec:"):
		return "exec"
	}
	return ""
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
				SupersededAt: now.UTC().Format(time.RFC3339),
			})
		}
	}
	e.Provenance, e.TokenRef, e.TokenUserID, e.CredentialExpiresAt = in.Provenance, in.TokenRef, in.TokenUserID, in.ExpiresAt
	e.LoggedOutAt = ""
	return e
}

// restoredEntry is the snapshot with a pending trust applied; nil when the
// operation created the entry.
func restoredEntry(snap *Server, trust *PendingTrust) *Server {
	if snap == nil {
		return nil
	}
	r := snap.Clone()
	if trust != nil {
		r.CA, r.CAFingerprint = trust.CA, trust.Fingerprint
	}
	return &r
}

// rollback removes a created entry or restores an updated one and returns
// primary; a failed rollback write is reported instead. declared is the
// declaration this login loaded: a rollback undoes the attempt entirely,
// so the directory goes back to naming the store it named before, whether
// or not this login got as far as setting a new one on the in-memory
// config.
func rollback(out *PersistLoginOutcome, cfg *Config, name string, snap *Server, declared string, primary error) error {
	cfg.CredentialStore = declared
	if snap == nil {
		delete(cfg.Servers, name)
	} else {
		cfg.Servers[name] = snap.Clone()
	}
	if werr := writeStep("rollback", cfg); werr != nil {
		return rollbackFailed(out, name, snap, werr, primary)
	}
	out.ConfigState = clierr.ConfigNotWritten
	return primary
}

func rollbackFailed(out *PersistLoginOutcome, name string, snap *Server, werr, primary error) error {
	out.ConfigState = clierr.ConfigRollbackFailed
	out.ConfigRecovery = &clierr.ConfigRecovery{
		Entry: name,
		Prior: priorEntry(snap),
		// The entry name is operator-chosen and only prefix-checked, so it is
		// shell-quoted like every value a machine hint carries.
		Command: "rancher config delete-server " + clierr.ShellQuote(name),
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
