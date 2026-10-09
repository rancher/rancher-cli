package config

import (
	"errors"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// The expected outcomes of the generated reader sweep. Each rule restates
// a documented behavior of the credential store - REFERENCE.md's
// credential store section and the doc comments of the functions named
// beside it - in terms of the sweep's own small model of a directory, and
// none of it calls the code it judges. A rule and the code can only agree
// by both being right or both being wrong in the same way.

// sweepHelper is the credential helper the sweep uses.
const sweepHelper = "exec:fake"

// loginPlugin is the login axis value for the kubectl plugin's own login.
const loginPlugin = "plugin"

// The kinds of answer a reader or a login gets, besides a credential.
const (
	answerNotLoggedIn   = "not logged in"
	answerNotConfigured = "no such entry"
	answerBothStores    = "credentials in both stores"
	answerConflict      = "the variable disagrees with the store asked for"
	answerUnavailable   = "credential store unavailable"
	answerIncomplete    = "login incomplete"
	answerRecord        = "store change record unreadable"
	answerUnfinished    = "store change unfinished"
	answerStoreChange   = "store change refused"
	answerUndisclosed   = "plaintext file not disclosed"
)

// sweepToken is the credential seeded for entry in store: a distinct value
// per entry and store, so a reader served another store's copy, or another
// entry's, reads a credential that is neither its old one nor its new one.
func sweepToken(entry, store string) string {
	if isExecBackend(store) {
		return entry + "-helper"
	}
	return entry + "-" + store
}

// sweepAnswer is what a reader got: a credential, or the kind of error.
type sweepAnswer struct{ token, kind string }

func (a sweepAnswer) String() string {
	if a.kind == "" {
		return "credential " + a.token
	}
	return a.kind
}

// classifySweepError names the kind of a reader's or a login's error.
// Anything outside the kinds above keeps its code and message, so it can
// never compare equal to an expected kind.
//
// A kind with a sentinel cause or an error code of its own is told by
// that. The rest share VALIDATION with no cause, and only their message
// tells them apart: the flag-versus-variable conflict, the unreadable
// store change record and the store change refusal. The aborts under
// ErrPersistAborted are told by the sentinel alone: the sweep changes
// neither the declaration nor the token reuse setting during a login, so
// the undisclosed plaintext file is the only abort it can meet.
func classifySweepError(err error) string {
	switch {
	case errors.Is(err, ErrNoToken):
		return answerNotLoggedIn
	case errors.Is(err, errMatrixNotConfigured):
		return answerNotConfigured
	case IsCredentialsInBothStores(err):
		return answerBothStores
	case errors.Is(err, ErrCredentialStoreConflict):
		return answerConflict
	case errors.Is(err, ErrPersistAborted):
		return answerUndisclosed
	}
	ce, _ := errors.AsType[*clierr.Error](err)
	code := ""
	if ce != nil {
		code = ce.Code
	}
	msg := err.Error()
	switch code {
	case clierr.CodeCredentialStoreUnavailable:
		return answerUnavailable
	case clierr.CodeLoginIncomplete:
		return answerIncomplete
	case clierr.CodeCredentialTransitionIncomplete:
		return answerUnfinished
	case clierr.CodeValidation:
		switch {
		case strings.Contains(msg, "(--credential-store); "+EnvCredentialStore+" asks for"):
			return answerConflict
		case strings.Contains(msg, "the credential store change recorded in "):
			return answerRecord
		case strings.Contains(msg, "a login changes the store of its own credential only"):
			return answerStoreChange
		}
	}
	return "unexpected error (" + code + "): " + msg
}

func sweepAnswerOf(tok string, err error) sweepAnswer {
	if err != nil {
		return sweepAnswer{kind: classifySweepError(err)}
	}
	return sweepAnswer{token: tok}
}

// sweepState is the sweep's model of a directory: what config.yaml
// declares and names, the pin, and where each entry has a copy of its
// credential. Every copy holds sweepToken(entry, store).
type sweepState struct {
	declared, pin string
	entries       []string
	copies        map[string][]string
	probe         bool
	// reachable is false for a keychain no call can reach; blocked names
	// the entry whose keychain copy alone cannot be reached.
	reachable bool
	blocked   string
}

func (s sweepState) configured(entry string) bool { return slices.Contains(s.entries, entry) }

func (s sweepState) has(entry, store string) bool { return slices.Contains(s.copies[entry], store) }

func (s sweepState) clone() sweepState {
	c := s
	c.entries = slices.Clone(s.entries)
	c.copies = map[string][]string{}
	for e, stores := range s.copies {
		c.copies[e] = slices.Clone(stores)
	}
	return c
}

func (s *sweepState) drop(entry, store string) {
	s.copies[entry] = slices.DeleteFunc(s.copies[entry], func(v string) bool { return v == store })
}

// others is every configured entry but entry.
func (s sweepState) others(entry string) []string {
	return slices.DeleteFunc(slices.Clone(s.entries), func(e string) bool { return e == entry })
}

// otherHas reports whether another configured entry has a copy in store.
func (s sweepState) otherHas(entry, store string) bool {
	return slices.ContainsFunc(s.others(entry), func(e string) bool { return s.has(e, store) })
}

func otherStore(store string) string {
	if store == backendFile {
		return backendKeychain
	}
	return backendFile
}

// readerStore is the store a process that takes no lock resolves, with
// RANCHER_CLI_CREDENTIAL_STORE set to v ("" for unset), or the error it
// gets instead (EffectivePolicyWithSource, resolveBackend,
// UnpinnedStoreOf):
//   - a variable naming a store other than the one config.yaml declares
//     is the conflict error;
//   - otherwise the policy is the variable, the declaration, or auto;
//   - a named store is read as named; auto reads the pin, and with no pin
//     what the configured entries have stored: the keychain when one has
//     an identity record, the file store when only credentials.yaml holds
//     them, the both-stores error for both, and the file store, which
//     answers "not logged in", for neither. An entry config.yaml does not
//     name counts for nothing.
func (s sweepState) readerStore(v string) (store, kind string) {
	if v != "" && s.declared != "" && v != s.declared {
		return "", answerConflict
	}
	policy := v
	if policy == "" {
		policy = s.declared
	}
	if policy != "" && policy != "auto" {
		return policy, ""
	}
	if s.pin != "" {
		return s.pin, ""
	}
	store, kind = s.unpinned()
	if store == "" && kind == "" {
		store = backendFile
	}
	return store, kind
}

// unpinned is where an auto directory with no pin keeps its credentials,
// from the configured entries' copies: empty when none has one.
func (s sweepState) unpinned() (store, kind string) {
	inKeychain := slices.ContainsFunc(s.entries, func(e string) bool { return s.has(e, backendKeychain) })
	inFile := slices.ContainsFunc(s.entries, func(e string) bool { return s.has(e, backendFile) })
	switch {
	case inKeychain && inFile:
		return "", answerBothStores
	case inKeychain:
		return backendKeychain, ""
	case inFile:
		return backendFile, ""
	}
	return "", ""
}

// read is what that process gets for entry: "no such entry" for one
// config.yaml does not name; the entry's copy in the store it resolves, or
// "not logged in" when there is none. A keychain read needs the entry's
// identity record first, so an entry with none answers "not logged in"
// without a keychain call; with one, a keychain that cannot be reached is
// CREDENTIAL_STORE_UNAVAILABLE.
func (s sweepState) read(entry, v string) (answer sweepAnswer, store string) {
	if !s.configured(entry) {
		return sweepAnswer{kind: answerNotConfigured}, ""
	}
	store, kind := s.readerStore(v)
	switch {
	case kind != "":
		return sweepAnswer{kind: kind}, ""
	case !s.has(entry, store):
		return sweepAnswer{kind: answerNotLoggedIn}, store
	case store == backendKeychain && (!s.reachable || s.blocked == entry):
		return sweepAnswer{kind: answerUnavailable}, store
	}
	return sweepAnswer{token: sweepToken(entry, store)}, store
}

// sweepLoginPolicy is the policy a login uses, what it declares once its
// credential is stored, and the conflict that refuses it instead
// (LoginPolicy, EffectivePolicy, DeclarationFor):
//   - a store named by the flag is the policy, and the variable, when set,
//     has to name the same one;
//   - without a flag, and for the plugin's login, the policy is the
//     variable on a directory that declares nothing, else the declaration,
//     else auto, and a variable that disagrees with a declaration refuses;
//   - the login declares the flag's store, or its policy on a directory
//     that declares nothing, and otherwise nothing.
func sweepLoginPolicy(r sweepRow) (policy, declaration, refusal string) {
	if r.login != "" && r.login != loginPlugin {
		if r.loginVar != "" && r.loginVar != r.login {
			return "", "", answerConflict
		}
		return r.login, r.login, ""
	}
	switch {
	case r.loginVar != "" && r.declared != "" && r.loginVar != r.declared:
		return "", "", answerConflict
	case r.loginVar != "":
		policy = r.loginVar
	case r.declared != "":
		policy = r.declared
	default:
		policy = "auto"
	}
	if r.declared == "" {
		declaration = policy
	}
	return policy, declaration, ""
}

// sweepBackends is what a login decides about the directory's store.
type sweepBackends struct {
	// readsNow is the store readers use before the login, empty for an
	// auto directory with no pin and nothing stored; commitsTo is where
	// the login stores its credential.
	readsNow, commitsTo string
	// changesStore: the login moves its entry's credential out of the
	// store readers use, or ends with a pin other than the one the
	// directory has. A second change is refused while an earlier one has
	// work left (MutationIntent, refusedBy).
	changesStore bool
}

func (b sweepBackends) changesBackend() bool { return b.readsNow != "" && b.readsNow != b.commitsTo }

// backends decides for a login of entry with policy that declares
// declaration (REFERENCE.md "Credential-store resolution" and the store
// change paragraph):
//   - readers now: the declaration when it names a store; under auto the
//     pin, else what the configured entries have stored, where both stores
//     refuse every login;
//   - the commit: a named policy is the store; auto keeps a keychain pin,
//     and otherwise asks the probe, which upgrades a file pin, and decides
//     an unpinned directory: keychain when usable, else the file store;
//   - an auto login that could move to the keychain from the file store
//     stays on the file store while another entry has a credential there,
//     or one left in the keychain;
//   - auto never moves a keychain or helper directory into plaintext: when
//     it would, the login is refused as the keychain being unavailable;
//   - readers afterwards resolve the declaration the login leaves, through
//     the pin it ends with: under auto that pin is the store committed to.
func (s sweepState) backends(entry, policy, declaration string) (sweepBackends, string) {
	var b sweepBackends
	declared := s.declared
	if declared == "" {
		declared = "auto"
	}
	switch {
	case declared != "auto":
		b.readsNow = declared
	case s.pin != "":
		b.readsNow = s.pin
	default:
		store, kind := s.unpinned()
		if kind != "" {
			return b, kind
		}
		b.readsNow = store
	}
	switch {
	case policy != "auto":
		b.commitsTo = policy
	case s.pin == backendKeychain, s.probe:
		b.commitsTo = backendKeychain
	default:
		b.commitsTo = backendFile
	}
	if policy == "auto" && b.readsNow == backendFile && b.commitsTo == backendKeychain &&
		(s.otherHas(entry, backendFile) || s.otherHas(entry, backendKeychain)) {
		b.commitsTo = backendFile
	}
	if policy == "auto" && b.commitsTo == backendFile && b.readsNow != "" && b.readsNow != backendFile {
		return b, answerUnavailable
	}
	after := declaration
	if after == "" {
		after = s.declared
	}
	b.changesStore = b.changesBackend() || (after == "auto" && b.commitsTo != s.pin)
	return b, ""
}

// storeChangeRefusal is the refusal a change of the store readers use
// gets (REFERENCE.md, the store change paragraph; CheckTransitionSources,
// CheckHelperDestination). A change takes only this entry's credential, so
// while another entry exists:
//   - leaving a helper, or moving into one, is refused, since a helper may
//     answer for any entry;
//   - another entry with a credential in the store being left, or one left
//     in the store being moved to, refuses; a keychain credential being
//     left has to be read, and one that cannot be is the keychain being
//     unavailable;
//   - an unpinned directory with nothing stored that moves to a helper is
//     refused the same way.
func (s sweepState) storeChangeRefusal(entry string, b sweepBackends) string {
	others := s.others(entry)
	if len(others) == 0 {
		return ""
	}
	switch {
	case b.changesBackend():
		if isExecBackend(b.readsNow) || isExecBackend(b.commitsTo) {
			return answerStoreChange
		}
		refused := false
		for _, o := range others {
			if s.has(o, b.readsNow) {
				if b.readsNow == backendKeychain && (!s.reachable || s.blocked == o) {
					return answerUnavailable
				}
				refused = true
			}
			if s.has(o, b.commitsTo) {
				refused = true
			}
		}
		if refused {
			return answerStoreChange
		}
	case b.readsNow == "" && isExecBackend(b.commitsTo):
		return answerStoreChange
	}
	return ""
}

// The states an earlier store change of prod can leave a directory in.
const (
	startNone = "none"
	// startDeleteLeft: the change moved readers, and the delete of prod's
	// copy in the store they left is still recorded; it can run now.
	startDeleteLeft = "deleteLeft"
	// startDeleteBlocked: the same, with a keychain copy whose slot cannot
	// be reached, so the delete cannot run.
	startDeleteBlocked = "deleteBlocked"
	// startPinLeft: the change committed prod's credential to the store
	// the pin will name, and the pin step - after the delete of prod's
	// copy in the store readers still use, when there is one - did not run.
	startPinLeft = "pinLeft"
	// startNeverStarted: a record config.yaml does not name, left by a
	// login that died before it named it.
	startNeverStarted = "neverStarted"
	// startUnreadable: config.yaml names a record that cannot be read.
	startUnreadable = "unreadable"
)

// startPossible reports whether an earlier change can have left the
// directory s in state start. A delete is recorded only for the store
// readers left, so it needs readers on a physical store; a blocked delete
// is the keychain's, so readers are on the file store. A pin step is left
// only under auto, by a change that committed to the other store while
// the pin named the one readers use, and that change was refused while
// another entry had a credential in either.
func startPossible(start string, s sweepState) bool {
	readers, kind := s.readerStore("")
	switch start {
	case startDeleteLeft:
		return kind == "" && (readers == backendFile || readers == backendKeychain)
	case startDeleteBlocked:
		return kind == "" && readers == backendFile
	case startPinLeft:
		return s.declared == "auto" && s.pin != "" && s.has("prod", otherStore(s.pin)) &&
			len(s.copies["staging"]) == 0
	}
	return true
}

// prelude is what the login's prelude under the lock does to s before
// anything else (prepareMutation): an unreadable record refuses every
// login; a record config.yaml does not name is removed unread; a named
// one is replayed in order, and work it cannot finish stays. leftFor
// names the store readers used when the state was made.
func (s sweepState) prelude(start, leftFor string) (settled sweepState, refusal string, stepsLeft bool) {
	settled = s.clone()
	switch start {
	case startUnreadable:
		return settled, answerRecord, false
	case startDeleteLeft:
		settled.drop("prod", otherStore(leftFor))
	case startDeleteBlocked:
		return settled, "", true
	case startPinLeft:
		settled.drop("prod", leftFor)
		settled.pin = otherStore(leftFor)
	}
	return settled, "", false
}

// leftoverWork reports whether a start leaves an earlier change with steps
// recorded in a record config.yaml names, readable.
func leftoverWork(start string) bool {
	return start == startDeleteLeft || start == startDeleteBlocked || start == startPinLeft
}

// sweepOutcome is what the rules say a row's login does.
type sweepOutcome struct {
	// precheck and lock are the refusals of the check before the sign-in
	// and of the login under the lock with that check skipped, empty when
	// each goes on. Both come before the login writes its entry.
	precheck, lock string
	// disclosed: the check before the sign-in told the plugin's user the
	// credential goes to the plaintext file.
	disclosed bool
	// preludeRan: the lock got as far as its prelude.
	preludeRan bool
	// settled is the directory as the prelude leaves it, and b the
	// login's decision on it.
	settled sweepState
	b       sweepBackends
	// stepsLeft: an earlier change still has work recorded afterwards.
	stepsLeft bool
	// commitFails is the error of a commit that cannot reach its store.
	commitFails string
}

// expectLogin applies the rules to row r on directory s, in the order
// each path runs them. The check before the sign-in: the login's policy,
// for the plugin's login the orphan check, the decision, the earlier
// change, the store change. The login under the lock: the policy, the
// decision made before the prelude (which says whether this login changes
// the store), the prelude, the decision again on what the prelude left,
// the store change, the plugin's disclosure, the orphan removal, the
// commit.
func expectLogin(r sweepRow, s sweepState, leftFor string) sweepOutcome {
	var o sweepOutcome
	policy, declaration, refusal := sweepLoginPolicy(r)
	o.precheck = func() string {
		if refusal != "" {
			return refusal
		}
		// An orphan in a keychain that cannot be reached refuses the
		// plugin's login before the sign-in, since the removal under the
		// lock would (CheckOrphanCredentials).
		if r.prodNew && s.has("prod", backendKeychain) && !s.reachable {
			return answerUnavailable
		}
		b, kind := s.backends("prod", policy, declaration)
		if kind != "" {
			return kind
		}
		if r.start == startUnreadable {
			return answerRecord
		}
		// The rule: while an earlier change has work left, the check
		// before the sign-in leaves both the second-change refusal and the
		// store rules to the login under the lock. The lock replays the
		// work first and judges the directory as the replay left it; the
		// check can only see the directory before that, and would refuse
		// what the lock accepts. The decision for the plugin's disclosure
		// is still made on the directory as it is.
		if !leftoverWork(r.start) {
			if kind := s.storeChangeRefusal("prod", b); kind != "" {
				return kind
			}
		}
		o.disclosed = r.login == loginPlugin && b.commitsTo == backendFile
		return ""
	}()
	// The plugin's login under the lock aborts when it would commit to the
	// plaintext file it did not disclose (undisclosedPlaintext). The lock
	// run of a row whose check refused is given the disclosure, so that
	// abort cannot stand in for the refusal it is compared with.
	disclosedToLock := o.disclosed || o.precheck != ""
	o.lock = func() string {
		if refusal != "" {
			return refusal
		}
		intent, kind := s.backends("prod", policy, declaration)
		if kind != "" {
			return kind
		}
		o.preludeRan = true
		settled, kind, left := s.prelude(r.start, leftFor)
		o.settled, o.stepsLeft = settled, left
		switch {
		case kind != "":
			return kind
		case left && intent.changesStore:
			return answerUnfinished
		}
		b, kind := settled.backends("prod", policy, declaration)
		o.b = b
		if kind != "" {
			return kind
		}
		if kind := settled.storeChangeRefusal("prod", b); kind != "" {
			return kind
		}
		if r.login == loginPlugin && b.commitsTo == backendFile && !disclosedToLock {
			return answerUndisclosed
		}
		// The orphan removal deletes a keychain orphan, which needs the
		// keychain (RemoveOrphanCredentials).
		if r.prodNew && settled.has("prod", backendKeychain) && !s.reachable {
			return answerUnavailable
		}
		return ""
	}()
	if o.lock == "" && o.b.commitsTo == backendKeychain &&
		(!s.reachable || (s.blocked == "prod" && o.settled.has("prod", backendKeychain))) {
		o.commitFails = answerUnavailable
	}
	return o
}
