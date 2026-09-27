package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// A login decides three things about the directory's credential store:
// what a reader resolves to now, what this login's credential is written
// to, and what a reader resolves to once the login is done. Every
// condition in the login order is derived from those three, answered
// once, in one place. A second spelling of the same question is how a
// login ends up committing to one store and pinning another.

// loginBackends is that answer. Its fields are physical backends - file,
// keychain, exec:<name> - never the policy `auto`, which is a question.
type loginBackends struct {
	// ReadsNow is what a reader resolves to before this login. Empty for
	// an `auto` directory with no pin and nothing stored: nothing has
	// decided yet and there is nothing to read.
	ReadsNow string
	// CommitsTo is what this login writes its credential to. The store is
	// opened from this value and every manifest step is built from it.
	CommitsTo string
	// ReadsAfter is what a reader resolves to once the declaration this
	// login writes is on disk and the pin is what step 0 leaves it as.
	ReadsAfter string
	// Pin is credstore-pin as this login read it.
	Pin string
	// PrePin is the pin step 0 writes, before the commit, or empty. A
	// directory with no pin that this login's credential leaves the file
	// store from gets `file`, and an unpinned `auto` directory that readers
	// find in the keychain gets `keychain`: it states what readers already
	// use, stays true if the login fails, and keeps readers where they are
	// until the pin step or the declaration moves them. Without it, a
	// reader would find this entry's new credential in one store and a
	// credential in the other with no pin to choose, and every entry would
	// fail in the middle of the login. With PinsFirst it is the store this
	// login commits to.
	PrePin string
	// PinsFirst says the login writes the pin it ends with in step 0,
	// before the commit. It is set only when the declaration the login
	// leaves makes readers ask the pin while the pin names a third store,
	// neither the one readers use now nor the one this login commits to.
	// That can only happen under an explicit declaration, which no
	// reader resolves through the pin, so the early write is invisible
	// until the declaration changes, and harmless if the login fails.
	PinsFirst bool
	// Notice is what an `auto` login owes the operator about plaintext:
	// the disclosure when it found the keychain unusable, or the warning
	// when it stays on the file store because other entries are there.
	Notice string
}

// ChangesBackend reports whether this entry's credential goes to a store
// other than the one readers use now. A directory with nothing to read
// from changes nothing.
func (b loginBackends) ChangesBackend() bool { return b.ReadsNow != "" && b.ReadsNow != b.CommitsTo }

// WritesPin reports whether the change ends with a pin write: without one
// a reader after this login resolves to a store that does not hold the
// credential the login just committed.
func (b loginBackends) WritesPin() bool { return b.ReadsAfter != b.CommitsTo }

// ChangesStore is the question the mutation prelude asks. Two store
// changes cannot stack: the second would forget the first, and an old copy
// the first had still to delete would be left in a backend nothing names.
func (b loginBackends) ChangesStore() bool { return b.ChangesBackend() || b.WritesPin() || b.PinsFirst }

// physicalBackend names the backend a policy and a pin resolve to,
// keeping a credential helper's name. Only `auto` asks the pin;
// everything else is already an answer. Empty means `auto` with no pin.
func physicalBackend(policy, pin string) string {
	if policy == "auto" {
		return pin
	}
	return policy
}

// loginBackendsOf answers what a login does to the directory's credential
// store, without writing anything: no pin, no store side effect. The
// keychain probe it may run is the memoized one a login already pays.
//
// entry is the entry logging in. policy is the login's own, as LoginPolicy
// reports it. declaration is what the login will write to config.yaml
// after its credential commits, empty when it changes nothing.
func loginBackendsOf(dir string, cfg *Config, entry, policy, declaration string) (loginBackends, error) {
	var b loginBackends
	resolved, err := EffectivePolicy(policy)
	if err != nil {
		return b, err
	}
	b.Pin = ReadPin(dir)

	declared := declaredPolicy(cfg)
	b.ReadsNow = physicalBackend(declared, b.Pin)
	if b.ReadsNow == "" {
		// An `auto` directory with no pin: what its entries have stored
		// is where readers find them. With nothing stored there is
		// nothing to read, and the change is the pin alone.
		u, uerr := UnpinnedStoreOf(dir, cfg)
		if uerr != nil {
			return b, uerr
		}
		if berr := u.Err(); berr != nil {
			return b, berr
		}
		b.ReadsNow = u.Backend()
	}

	b.CommitsTo = physicalBackend(resolved, b.Pin)
	switch {
	case resolved != "auto":
		// An explicit policy is already the answer and never asks the pin.
	case b.CommitsTo == "":
		if keyringUsableOnce() {
			b.CommitsTo = backendKeychain
			break
		}
		b.CommitsTo = backendFile
		b.Notice = plaintextStoreNotice(dir)
	case b.CommitsTo == backendFile && keyringUsableOnce():
		// Upgrade only: a keychain pin is never re-probed, so a keychain
		// that hiccups can never move a directory to plaintext behind the
		// operator's back.
		b.CommitsTo = backendKeychain
	}
	if resolved == "auto" && b.ReadsNow == backendFile && b.CommitsTo == backendKeychain {
		// The upgrade takes this entry's credential only. While another
		// entry's credential is in the file store, moving this one alone
		// would leave the directory reading two stores, so the login stays
		// where the directory already reads - no downgrade - and says how
		// to leave: log the others out, and the next login upgrades.
		others, oerr := otherFileCredentials(dir, cfg, entry)
		if oerr != nil {
			return b, oerr
		}
		if len(others) > 0 {
			b.CommitsTo = backendFile
			b.Notice = plaintextKeptNotice(dir, others)
		}
	}
	if resolved == "auto" && b.CommitsTo == backendFile && b.ReadsNow != "" && b.ReadsNow != backendFile {
		// The keychain cannot be used right now, on a directory whose
		// credentials are in the keychain or a helper. `auto` falling back
		// to the plaintext file here would move all of them out on a
		// probe - a locked keychain, a session over SSH - which is the
		// silent downgrade a credential store must never make. Plaintext
		// is something a login is asked for by name.
		return b, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"the keychain cannot be used right now, and a login that asks for auto never moves this directory's credentials out of %s into the plaintext file %s; unlock or fix the keychain and run this command again, or log in with --credential-store file to keep this entry's credential in plaintext on purpose",
			b.ReadsNow, CredentialsFilePath(dir)))
	}

	after := cfg.CredentialStore
	if declaration != "" {
		after = declaration
	}
	afterPolicy, err := EffectivePolicy(after)
	if err != nil {
		return b, err
	}
	b.ReadsAfter = physicalBackend(afterPolicy, b.Pin)
	if b.Pin == "" && b.ChangesBackend() {
		switch {
		case b.ReadsNow == backendFile:
			b.PrePin = backendFile
		case b.ReadsNow == backendKeychain && declared == "auto":
			// Only an unpinned `auto` directory's readers find the
			// keychain without a pin; a declared keychain never asks it.
			b.PrePin = backendKeychain
		}
	}
	// What a reader resolves to right after the declaration write, with
	// the pin as step 0 leaves it.
	pinAtActivation := b.Pin
	if b.PrePin != "" {
		pinAtActivation = b.PrePin
	}
	readsAtActivation := physicalBackend(afterPolicy, pinAtActivation)
	if declared != "auto" && readsAtActivation != b.ReadsNow && readsAtActivation != b.CommitsTo {
		// Readers between the declaration write and a pin step at the end
		// would resolve to a third store: one the pin names, where an
		// empty store reads "not logged in" for every entry and a stale
		// token is served under the new binding id; or, with no pin at
		// all, whatever the stored credentials answer, which is both
		// stores while this login's new credential sits beside a stale
		// one. Readers ignore the pin until the declaration changes, so it
		// is written first instead.
		b.PinsFirst = true
		b.PrePin = b.CommitsTo
		b.ReadsAfter = b.CommitsTo
	}
	return b, nil
}

// declaredPolicy is what says where this directory's credentials are
// before the login: its declaration, and `auto` when it has none. The
// variable is not part of it. It chooses where a login writes and is
// recorded by that login, but where the credentials already are is the
// directory's own record - the pin and what is stored - so a variable
// naming another store is a change asked for by name like the flag.
func declaredPolicy(cfg *Config) string {
	if cfg.CredentialStore == "" {
		return "auto"
	}
	return cfg.CredentialStore
}

// otherFileCredentials names the entries in config.yaml other than entry
// whose credential is in credentials.yaml. An unreadable file is an error
// rather than "none": a login deciding on the belief that the file is
// empty would leave whatever is in it behind. A directory with no other
// entry has nothing to ask the file about.
func otherFileCredentials(dir string, cfg *Config, entry string) ([]string, error) {
	var others []string
	for name := range cfg.Servers {
		if name != entry {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		return nil, nil
	}
	m, err := newFileStore(dir, &noticeBuffer{}).read()
	if err != nil {
		return nil, unreadableCredentialsFile(dir, err)
	}
	others = slices.DeleteFunc(others, func(name string) bool {
		_, ok := m[name]
		return !ok
	})
	slices.Sort(others)
	return others, nil
}

// unreadableCredentialsFile refuses a login that has to know what
// credentials.yaml holds and cannot read it.
func unreadableCredentialsFile(dir string, err error) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"%s cannot be read: %v; make it readable, or use a different --config, then run this command again",
		CredentialsFilePath(dir), err)).
		WithCause(err)
}

// plaintextKeptNotice is the warning an `auto` login gives when it could
// use the keychain and stays on the file store because other entries'
// credentials are there.
func plaintextKeptNotice(dir string, others []string) string {
	count, which := "1 other credential", "log that entry out"
	if len(others) > 1 {
		count, which = fmt.Sprintf("%d other credentials", len(others)), "log those entries out"
	}
	return fmt.Sprintf(
		"this directory keeps %s in the plaintext file %s (%s), so this login stores its credential there too; to move the directory to the keychain, %s, then log in again",
		count, CredentialsFilePath(dir), strings.Join(others, ", "), which)
}

// LoginStoreChange reports what would stop a login before it asks anyone
// to authenticate: a store change while another is unfinished, another
// entry still holding a credential in the store this entry's credential
// leaves, or a directory whose credentials are in both stores with no pin.
// It also returns the store the login's credential would go to, the
// answer a plaintext disclosure before the sign-in is made from. It takes
// no lock and writes nothing, so the answer can be stale by the time the
// login holds the config lock; the plan there runs the same rules and is
// the authority. This exists so an operator is not asked to sign in for a
// login that was never going to be accepted.
func LoginStoreChange(dir string, cfg *Config, entry, policy, declaration string) (commitsTo string, err error) {
	b, err := loginBackendsOf(dir, cfg, entry, policy, declaration)
	if err != nil {
		return "", err
	}
	if err := RefusedByTransition(dir, cfg, MutationIntent{ChangesStore: b.ChangesStore()}); err != nil {
		return "", err
	}
	if b.ChangesBackend() {
		if err := CheckTransitionSources(dir, cfg, entry, b.ReadsNow); err != nil {
			return "", err
		}
	}
	return b.CommitsTo, nil
}

// plaintextStoreNotice is the disclosure a directory gets when the
// keychain cannot be used and the credential goes to the plaintext file.
func plaintextStoreNotice(dir string) string {
	return fmt.Sprintf("keychain unavailable; credentials will be stored in plaintext %s", CredentialsFilePath(dir))
}

// planStoreChange records what a login that changes the directory's
// credential store leaves behind: this entry's old copy, then the pin the
// change ends with - last, so no reader resolves through the new pin while
// the old copy is still where the old one points.
//
// The manifest is written before the credential commits and is inert
// until a config write names its id, so a login that dies between the two
// leaves nothing to finish. A plan with no steps at all is no change and
// is not written; a plan whose only step is the pin is a change like any
// other, or the pin would never be written.
func planStoreChange(dir string, cfg *Config, entry, url string, b loginBackends) (storeChange, error) {
	var ch storeChange
	if b.ChangesBackend() {
		if err := CheckTransitionSources(dir, cfg, entry, b.ReadsNow); err != nil {
			return ch, err
		}
	}
	plan, err := newTransitionPlan()
	if err != nil {
		return ch, err
	}
	if b.ChangesBackend() {
		old, notice, oerr := oldCopyOf(dir, entry, url, b.ReadsNow)
		if oerr != nil {
			return ch, oerr
		}
		ch.notices = appendNotice(ch.notices, notice)
		ch.oldCopyUnread = old.unread
		if aerr := plan.AddOldCopyDelete(entry, b.ReadsNow, old.token, old.url, old.accountID); aerr != nil {
			return ch, aerr
		}
	}
	if b.WritesPin() {
		plan.AddPin(b.CommitsTo)
	}
	if b.PinsFirst {
		ch.notices = append(ch.notices, noticePinned(b.Pin, b.CommitsTo))
	}
	if plan.Empty() {
		// Nothing to record, but a pin written first is still written: it
		// is the whole of what this change does to the store.
		if b.PrePin != "" {
			return ch, WritePin(dir, b.PrePin)
		}
		return ch, nil
	}
	if werr := plan.Write(dir); werr != nil {
		return ch, werr
	}
	// From here the record is on disk: an undo has to remove it even if
	// the step below fails.
	ch.plan = plan
	if b.PrePin != "" {
		// Written before the commit. It names the store readers already
		// use, or it is written under a declaration no reader resolves
		// through the pin, so a login that fails leaves nothing a reader
		// sees change.
		if perr := WritePin(dir, b.PrePin); perr != nil {
			return ch, perr
		}
	}
	return ch, nil
}

// storeChange is what step 0 left behind: the record of the change, and
// the one thing step 2 needs that the record cannot say - whether this
// entry's old copy could be read at all.
type storeChange struct {
	// plan is nil when the login records no change, and set as soon as the
	// record reaches the disk, so an undo removes one that did.
	plan *TransitionPlan
	// oldCopyUnread says the backend holding this entry's old copy could
	// not be read, so no delete step names it: a delete is bound to the
	// credential's digest and there was none to compute. Step 2 cannot
	// remove that copy and flips the pin instead, which is the other way
	// to stop a reader resolving to it.
	oldCopyUnread bool
	notices       []string
}

// oldCopy is the logging-in entry's credential in the backend it is
// leaving, and what a step needs to delete exactly that one.
type oldCopy struct {
	token, accountID, url string
	// unread says the backend could not answer at all. There is no digest
	// to bind a delete to, so no step is recorded and the copy stays
	// where it is; the caller has to stop readers resolving to it another
	// way.
	unread bool
}

// oldCopyOf reads it. An absent credential adds no step and is not a
// failure; neither is a keychain slot another config directory now owns,
// which is not this directory's to delete. A backend that cannot be read
// at all leaves the old copy where it is with a notice, because a delete
// step is bound to the credential's digest and there is none to record.
func oldCopyOf(dir, entry, url, from string) (oldCopy, string, error) {
	unread := func(err error) (oldCopy, string, error) {
		return oldCopy{unread: true}, fmt.Sprintf(
			"the old copy of %s's credential could not be read and was left where it is: %s", entry, err.Error()), nil
	}
	ref := ServerRef{Name: entry, URL: url}
	switch from {
	case backendFile:
		tok, err := newFileStore(dir, &noticeBuffer{}).GetToken(ref)
		switch {
		case errors.Is(err, ErrNoToken):
			return oldCopy{}, "", nil
		case err != nil:
			return unread(err)
		}
		return oldCopy{token: tok}, "", nil
	case backendKeychain:
		rec, err := readIdentityRecord(dir, entry)
		switch {
		case err != nil:
			return unread(err)
		case rec == nil:
			return oldCopy{}, "", nil
		}
		tok, gerr := keyringGet(rec.Account())
		switch {
		case errors.Is(gerr, keyring.ErrNotFound):
			return oldCopy{}, "", nil
		case gerr != nil:
			return unread(gerr)
		case credentialFingerprint(rec.ID, tok) != rec.Fingerprint:
			// The slot holds another config directory's credential now.
			// That directory addresses it; this one has no claim to
			// remove it.
			return oldCopy{}, "", nil
		}
		return oldCopy{token: tok, accountID: rec.ID}, "", nil
	}
	name, ok := execHelperName(from)
	if !ok {
		return oldCopy{}, "", fmt.Errorf("unknown credential backend %q", from)
	}
	store := newExecStore(name, dir)
	tok, gerr := store.GetToken(ref)
	switch {
	case errors.Is(gerr, ErrNoToken):
		return oldCopy{}, "", nil
	case gerr != nil:
		return unread(gerr)
	}
	if _, short := store.ShortLivedTokenExpiry(ref); short {
		// The helper made this on the way out rather than returning one
		// it stored, so there is nothing left behind to remove.
		return oldCopy{}, "", nil
	}
	return oldCopy{token: tok, url: url}, "", nil
}

// runOldCopyBeforeBinding performs step 2: the logging-in entry's old
// copy goes, and if it cannot go the pin flips instead, both before the
// binding id advances. ok is false only when neither could be done, which
// is the one state that stops the id from advancing.
func runOldCopyBeforeBinding(ctx context.Context, dir string, cfg *Config, ch storeChange, entry string) (notices []string, ok bool) {
	m := &ch.plan.manifest
	i := stepIndex(m, func(s transitionStep) bool {
		return s.Kind == transitionDelete && s.Entry == entry
	})
	switch {
	case i < 0 && !ch.oldCopyUnread:
		// No old copy at all: there is nothing a reader could resolve to
		// and pair with the new id, so the pin keeps its place last.
		return nil, true
	case i < 0:
		// There is one, and it could not be read, so no step names it.
		// The pin is the only thing left that stops a reader resolving to
		// it, so it runs here rather than last.
		return runPinBeforeBinding(ctx, dir, cfg, m, nil)
	}
	done, ns, retained, err := replayStepAt(ctx, dir, cfg, m, i)
	notices = append(notices, ns...)
	notices = append(notices, retained...)
	if done && err == nil {
		return notices, true
	}
	if err != nil {
		notices = append(notices, fmt.Sprintf(
			"the credential store change could not be recorded as it ran: %s", err.Error()))
	}
	return runPinBeforeBinding(ctx, dir, cfg, m, notices)
}

// runPinBeforeBinding runs the change's pin step early, which is what
// stops a reader resolving to an old copy that is still there.
func runPinBeforeBinding(ctx context.Context, dir string, cfg *Config, m *transitionManifest, notices []string) ([]string, bool) {
	j := stepIndex(m, func(s transitionStep) bool { return s.Kind == transitionPin })
	if j < 0 {
		return notices, false
	}
	pinned, pns, pretained, perr := replayStepAt(ctx, dir, cfg, m, j)
	notices = append(notices, pns...)
	notices = append(notices, pretained...)
	if perr != nil {
		notices = append(notices, fmt.Sprintf(
			"the credential store change could not be recorded as it ran: %s", perr.Error()))
	}
	return notices, pinned && perr == nil
}

// stepIndex is the position of the first step matching want, or -1.
func stepIndex(m *transitionManifest, want func(transitionStep) bool) int {
	return slices.IndexFunc(m.Steps, want)
}
