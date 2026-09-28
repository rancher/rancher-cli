package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is a model of the exec-credential cache across a change of an
// entry's credential. It does not run the package's code: it states the
// protocol as a small state machine - the files a directory holds, the
// processes that read and write them, and the order of each process's
// durable writes - enumerates every interleaving of a few processes, with
// a crash after any durable write of a process that changes a
// credential, and checks one invariant for every entry:
//
//   - a cache hit never serves a token made from a credential other than
//     the one a reader of the entry resolves at that moment, and
//   - no record left on disk is one a later reader would serve while the
//     entry resolves a different credential - including after another
//     command finishes whatever steps a change left.
//
// Two protocols are modelled. modelIDOnly is the entry's cacheBindingId
// alone: a change advances the id at one point of its order, and a cache
// writer writes under the id its load returned. modelPaused adds these
// rules:
//
//   - the config write that starts a change advances the entry's id and
//     marks its cache writes paused, before any of its credential is
//     written; a change made without the config lock marks the pause as
//     such;
//   - a cache writer whose load shows the pause writes nothing, and one
//     writes only when the store a reader resolves from its load - the
//     declaration there, the pin read after it - is the store it read the
//     credential from;
//   - every other config write carries the pause as it found it;
//   - a pause is cleared only under the config lock, only while no pin
//     step is left that would move readers - one whose store readers
//     already resolve moves none - and a pause marked as made without the
//     lock only by the process that made it.
//
// The first protocol is asserted to break the invariant for every change
// it models, which is what shows the model can see the windows; the
// second is asserted to keep it everywhere; and each of the second's
// rules, weakened on its own, is asserted to break it again.

type modelProtocol int8

const (
	modelIDOnly modelProtocol = iota
	modelPaused
	// Each protocol below is modelPaused with one of its rules weakened.
	// The model has to find a violation in every one of them; one it
	// does not find is a rule the model cannot tell is needed.
	modelWriterIgnoresPause
	modelWriterSkipsStoreCheck
	modelAdvanceAfterCommit
	modelRollbackRestoresID
	modelResumeWithoutLock
	modelPauseWithoutAdvance
	modelRewriteDropsPause
	modelResumeDespitePin
	modelResumeUnlockedPause
)

func (p modelProtocol) String() string {
	return [...]string{
		"binding id only",
		"paused writes",
		"a writer that ignores the pause",
		"a writer that skips the store check",
		"the advance after the commit",
		"a rollback that restores the id",
		"a resume without the config lock",
		"a pause without the id advance",
		"a config rewrite that drops the pause",
		"a resume while a pin step remains",
		"a locked resume of a pause made without the lock",
	}[p]
}

func (p modelProtocol) pauses() bool   { return p != modelIDOnly }
func (p modelProtocol) advances() bool { return p != modelPauseWithoutAdvance }

// modelStore is a credential store an entry's credential can be kept in.
type modelStore int8

const (
	modelFile modelStore = iota
	modelKeychain
)

// modelPolicy is a credentialStore declaration or a pin: none, a store,
// or auto.
type modelPolicy int8

const (
	modelNoPolicy modelPolicy = iota
	modelFilePolicy
	modelKeychainPolicy
	modelAutoPolicy
)

func (s modelStore) policy() modelPolicy {
	if s == modelKeychain {
		return modelKeychainPolicy
	}
	return modelFilePolicy
}

func (s modelStore) String() string {
	if s == modelKeychain {
		return backendKeychain
	}
	return backendFile
}

// modelUser is whose credential a store holds or a token was made from;
// modelNobody is none.
type modelUser int8

const (
	modelNobody modelUser = iota
	modelAlice
	modelBob
	modelTom
	// modelSignedIn is whoever signs in when the exec plugin falls back to
	// an interactive login.
	modelSignedIn
)

func (u modelUser) String() string {
	return [...]string{"nobody", "alice", "bob", "tom", "an interactive sign-in"}[u]
}

// servable reports whether a record made from src may be served to a
// reader that resolves the entry to resolved: the same credential, or an
// interactive sign-in's token while the entry holds no credential, which
// is what the fallback is for.
func servable(src, resolved modelUser) bool {
	return src == resolved || (src == modelSignedIn && resolved == modelNobody)
}

// modelStep is one step a store change still has to do: delete an entry's
// old copy from a store, or write the pin.
type modelStep struct {
	kind  int8
	store modelStore
	entry int8
}

const (
	modelNoStep int8 = iota
	modelDeleteStep
	modelPinStep
)

// The states of an entry's pause.
const (
	modelNotPaused int8 = iota
	modelPausedLocked
	modelPausedUnlocked
)

// modelRecord is the one exec-credential cache record the model tracks
// for an entry, for one cluster. src is the user whose credential the
// token was made from.
type modelRecord struct {
	present bool
	src     modelUser
	id      int8
}

// modelEntry is one server entry: the fields of config.yaml the protocol
// reads, what each store holds for it, and its cache record.
type modelEntry struct {
	exists bool
	id     int8
	paused int8
	cred   [2]modelUser
	record modelRecord
}

// modelDir is everything durable in the directory: the binding stamp
// file, the directory-wide fields of config.yaml, the change record's
// remaining steps, the pin, and two entries. lock is the config lock's
// holder (a process index plus one), zero when free.
type modelDir struct {
	stampFile int8
	cfgStamp  int8
	decl      modelPolicy
	steps     [2]modelStep
	pin       modelPolicy
	e         [2]modelEntry

	seq  int8
	lock int8
}

// fresh is a value no earlier write produced, as a random stamp or id is.
// The starting values are negative, so none of them is ever produced.
func (d *modelDir) fresh() int8 {
	d.seq++
	return d.seq
}

// readerStore is the store a reader that takes no lock resolves: the
// declaration when it names one, else the pin, else the unpinned decision.
func (d modelDir) readerStore() (modelStore, bool) { return d.storeFor(d.decl, d.pin) }

// storeFor resolves a declaration and a pin as openStoreResolved does. An
// `auto` directory with no pin takes the unpinned decision from what its
// entries have stored (UnpinnedStoreOf): the keychain when any entry has a
// keychain credential, the file store otherwise, and no store at all - the
// both-stores error - when some entry has each.
func (d modelDir) storeFor(decl, pin modelPolicy) (modelStore, bool) {
	for _, p := range []modelPolicy{decl, pin} {
		switch p {
		case modelFilePolicy:
			return modelFile, true
		case modelKeychainPolicy:
			return modelKeychain, true
		}
	}
	var inKeychain, inFile bool
	for _, e := range d.e {
		inKeychain = inKeychain || (e.exists && e.cred[modelKeychain] != modelNobody)
		inFile = inFile || (e.exists && e.cred[modelFile] != modelNobody)
	}
	switch {
	case inKeychain && inFile:
		return 0, false
	case inKeychain:
		return modelKeychain, true
	}
	return modelFile, true
}

// resolved is the user whose credential a reader of entry i gets now, or
// modelNobody when it gets none.
func (d modelDir) resolved(i int) modelUser {
	if !d.e[i].exists {
		return modelNobody
	}
	s, ok := d.readerStore()
	if !ok {
		return modelNobody
	}
	return d.e[i].cred[s]
}

func (d *modelDir) nextStep() modelStep { return d.steps[0] }

func (d *modelDir) dropStep() { d.steps = [2]modelStep{d.steps[1]} }

func (d modelDir) hasSteps() bool { return d.steps[0].kind != modelNoStep }

func (d modelDir) pinPending() bool {
	for _, s := range d.steps {
		if s.kind == modelPinStep {
			return true
		}
	}
	return false
}

// pinMovesReaders reports whether a pending pin step would move readers:
// its store is not the one they resolve now, or they resolve none.
func (d modelDir) pinMovesReaders() bool {
	for _, s := range d.steps {
		if s.kind != modelPinStep {
			continue
		}
		now, ok := d.readerStore()
		if !ok || now != s.store {
			return true
		}
	}
	return false
}

// runStep performs a step's effect; a replay removes it afterwards.
func (d *modelDir) runStep(s modelStep) {
	switch s.kind {
	case modelDeleteStep:
		d.e[s.entry].cred[s.store] = modelNobody
	case modelPinStep:
		d.pin = s.store.policy()
	}
}

// finished is the directory after some later command replays every
// remaining step successfully.
func (d modelDir) finished() modelDir {
	for d.hasSteps() {
		d.runStep(d.nextStep())
		d.dropStep()
	}
	return d
}

// mayResume reports whether a resumer may clear entry i's pause. setter
// is true for the process whose change set it.
func (d modelDir) mayResume(p modelProtocol, i int, setter bool) bool {
	switch d.e[i].paused {
	case modelNotPaused:
		return false
	case modelPausedUnlocked:
		if !setter && p != modelResumeUnlockedPause {
			return false
		}
	}
	return !d.pinMovesReaders() || p == modelResumeDespitePin
}

// modelLocals is one process's memory: its position, and what it read.
type modelLocals struct {
	pc      int8
	done    bool
	crashed bool

	decl        modelPolicy
	bindDecl    modelPolicy
	recheck     modelStore
	recheckOK   bool
	store       modelStore
	storeOK     bool
	anchor      int8
	loadID      int8
	loadStamp   int8
	loadEntry   bool
	loadPaused  int8
	cred        modelUser
	served      modelUser
	reloaded    bool
	failed      bool
	oldCopyLeft bool
	outcomeID   int8
	priorID     int8
	pausedAfter bool
	hasPrior    bool
	stamp       int8
}

// modelAction is one atomic action of a process. A durable action is a
// write a crash can follow. A locked action runs under the config lock,
// taking it when the process does not hold it yet; release gives it back
// afterwards, and so does the process ending. branches is the number of
// outcomes the action can have (success, failure), zero meaning one; when
// is a condition under which the action runs at all.
type modelAction struct {
	name     string
	locked   bool
	release  bool
	durable  bool
	branches int
	when     func(d modelDir, l modelLocals) bool
	run      func(d *modelDir, l *modelLocals, branch int, c *modelCheck)
}

type modelProgram struct {
	name    string
	changer bool
	actions []modelAction
}

// modelOpts is what a change is modelled with: the protocol, the entry it
// changes, the user it logs in, and whether it runs without the config
// lock, as RANCHER_CLI_CONFIG_NO_LOCK runs one.
type modelOpts struct {
	p      modelProtocol
	x      int
	user   modelUser
	noLock bool
}

// modelCheck collects violations of the invariant with the trace that led
// to each; only the first few traces are kept.
type modelCheck struct {
	count  int
	traces []string
	trace  []string
}

func (c *modelCheck) violate(why string) {
	c.count++
	if len(c.traces) < 3 {
		c.traces = append(c.traces, why+"\n    after: "+strings.Join(c.trace, "\n           "))
	}
}

// stampedWrite is a config write that changes an entry's binding id or its
// existence: Config.Write writes the stamp file first, then config.yaml
// with the same value, and a crash can fall between the two.
func stampedWrite(o modelOpts, name string, mutate func(d *modelDir, l *modelLocals)) []modelAction {
	return []modelAction{
		{name: name + " (stamp file)", locked: !o.noLock, durable: true, run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.stamp = d.fresh()
			d.stampFile = l.stamp
		}},
		{name: name + " (config.yaml)", locked: !o.noLock, durable: true, run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			mutate(d, l)
			d.cfgStamp = l.stamp
		}},
	}
}

// plainWrite is a config write that leaves every entry's binding id and
// existence alone, which Config.Write makes without touching the stamp.
func plainWrite(o modelOpts, name string, when func(d modelDir, l modelLocals) bool, mutate func(d *modelDir, l *modelLocals)) modelAction {
	return modelAction{name: name, locked: !o.noLock, durable: true, when: when,
		run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) { mutate(d, l) }}
}

// advance is the change's id advance. Under the paused protocols it is
// the change's first write and pauses the entry's cache writes in the
// same write.
func advance(o modelOpts) []modelAction {
	name := "advance the binding id"
	if o.p.pauses() {
		name = "advance the binding id and pause cache writes"
	}
	mutate := func(d *modelDir, l *modelLocals) {
		e := &d.e[o.x]
		l.priorID, l.hasPrior = e.id, true
		if o.p.advances() {
			e.id = d.fresh()
		}
		l.outcomeID = e.id
		if o.p.pauses() {
			e.paused = modelPausedLocked
			if o.noLock {
				e.paused = modelPausedUnlocked
			}
		}
	}
	if !o.p.advances() {
		return one(plainWrite(o, "pause cache writes", nil, mutate))
	}
	return stampedWrite(o, name, mutate)
}

// resume clears every pause this process may clear. A change clears its
// own entry's and, holding the lock, every other entry's a resumer may;
// without the lock it clears only its own. setter is false for a process
// that changed no credential of its own.
func resume(o modelOpts, name string, setter bool) modelAction {
	targets := func(d modelDir) []int {
		var out []int
		for i := range d.e {
			own := setter && i == o.x
			if !own && o.noLock {
				continue
			}
			if d.mayResume(o.p, i, own) {
				out = append(out, i)
			}
		}
		return out
	}
	a := plainWrite(o, name,
		func(d modelDir, _ modelLocals) bool { return len(targets(d)) > 0 },
		func(d *modelDir, l *modelLocals) {
			for _, i := range targets(*d) {
				if o.p == modelRollbackRestoresID && setter && i == o.x && l.failed && l.hasPrior {
					d.e[i].id = l.priorID
				}
				d.e[i].paused = modelNotPaused
			}
		})
	if o.p == modelResumeWithoutLock && !setter {
		a.locked = false
	}
	return a
}

func notFailed(_ modelDir, l modelLocals) bool { return !l.failed }

// commit writes the change's user's credential to a store. Branch 1 is a
// commit that did not write, after which the credential is what it was.
// Branch 2 is a commit that landed, followed by a failed write of the
// login's own and a compensation that failed too: the login rolls back
// with the new credential still stored.
func commit(o modelOpts, name string, store func(d modelDir) modelStore, onFailure func(l *modelLocals)) modelAction {
	return modelAction{name: "commit " + o.user.String() + "'s credential to " + name, locked: !o.noLock, durable: true, branches: 3,
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch != 1 {
				d.e[o.x].cred[store(*d)] = o.user
			}
			if branch != 0 {
				onFailure(l)
			}
		}}
}

func readersStore(d modelDir) modelStore {
	s, _ := d.readerStore()
	return s
}

func fixedStore(s modelStore) func(modelDir) modelStore {
	return func(modelDir) modelStore { return s }
}

// stopOnFailure ends the process: the change stopped on an error it
// reports, and makes no further write.
func stopOnFailure(l *modelLocals) { l.done = true }

// skipRestOnFailure marks the failure so the steps that only a committed
// credential needs are skipped and the process goes on to its last
// write.
func skipRestOnFailure(l *modelLocals) { l.failed = true }

// purge removes the entry's cache records under the cache lock.
func purge(o modelOpts) modelAction {
	return modelAction{name: "purge the cache records", locked: !o.noLock,
		run: func(d *modelDir, _ *modelLocals, _ int, _ *modelCheck) { d.e[o.x].record = modelRecord{} }}
}

// deleteCredential deletes the entry's credential from every store; a
// failure stops the command with the credential in place.
func deleteCredential(o modelOpts) modelAction {
	return modelAction{name: "delete the credential", locked: !o.noLock, durable: true, branches: 2,
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch == 1 {
				l.done = true
				return
			}
			d.e[o.x].cred = [2]modelUser{}
		}}
}

// replayStep runs the change's next remaining step; a failure keeps it
// and stops the pass, as replayPass does.
func replayStep(o modelOpts, name string) modelAction {
	return modelAction{name: name, locked: !o.noLock, durable: true, branches: 2,
		when: func(d modelDir, l modelLocals) bool { return d.hasSteps() && !l.failed },
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch == 1 {
				l.failed = true
				return
			}
			d.runStep(d.nextStep())
			d.dropStep()
		}}
}

func release(o modelOpts) modelAction {
	return modelAction{name: "release the config lock", locked: !o.noLock, release: true,
		run: func(*modelDir, *modelLocals, int, *modelCheck) {}}
}

func withRelease(o modelOpts, actions ...[]modelAction) []modelAction {
	var out []modelAction
	for _, a := range actions {
		out = append(out, a...)
	}
	return append(out, release(o))
}

func one(a modelAction) []modelAction { return []modelAction{a} }

// refuseOthers is a store change's refusal under the lock, before any
// write (checkStoreChange): it does not start while another entry holds a
// credential in the store being left or in the one it moves to.
func refuseOthers(o modelOpts) modelAction {
	return modelAction{name: "refuse while another entry holds a credential", locked: !o.noLock,
		run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			for i, e := range d.e {
				if i != o.x && e.cred != [2]modelUser{} {
					l.done = true
				}
			}
		}}
}

// modelLogin is a login that changes no store: the user's credential
// replaces the entry's in the store its readers use.
func modelLogin(o modelOpts) modelProgram {
	return modelLoginTo(o, "the store readers use", readersStore)
}

// modelLoginToPinnedStore is a login made while a pin step is pending
// that commits to the store the pin will name rather than the one readers
// use now; with no pin pending it is modelLogin. The code refuses such a
// login (a login that leaves the readers' store is a store change, and a
// store change is refused while a step remains); the model runs it anyway,
// as a state the protocol must survive if that refusal ever goes.
func modelLoginToPinnedStore(o modelOpts) modelProgram {
	prog := modelLoginTo(o, "the store the pending pin names", func(d modelDir) modelStore {
		for _, s := range d.steps {
			if s.kind == modelPinStep {
				return s.store
			}
		}
		return readersStore(d)
	})
	prog.name = "login to the pinned store"
	return prog
}

func modelLoginTo(o modelOpts, where string, store func(modelDir) modelStore) modelProgram {
	switch o.p {
	case modelIDOnly:
		return modelProgram{name: "login", changer: true, actions: withRelease(o,
			one(commit(o, where, store, stopOnFailure)),
			advance(o),
		)}
	case modelAdvanceAfterCommit:
		return modelProgram{name: "login", changer: true, actions: withRelease(o,
			one(commit(o, where, store, skipRestOnFailure)),
			advance(o),
			one(resume(o, "resume cache writes", true)),
		)}
	}
	return modelProgram{name: "login", changer: true, actions: withRelease(o,
		advance(o),
		one(commit(o, where, store, skipRestOnFailure)),
		one(resume(o, "resume cache writes", true)),
	)}
}

// modelPluginLogin is the exec plugin's own login: a login, then the cache
// record for the token it made from the new credential, bound to the id
// the login's outcome reports.
func modelPluginLogin(o modelOpts) modelProgram {
	login := modelLogin(o)
	login.name = "plugin-created login"
	last := len(login.actions) - 1
	// The outcome reports whether the login left the pause set.
	login.actions[last].run = func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
		l.pausedAfter = d.e[o.x].paused != modelNotPaused
	}
	login.actions = append(login.actions, modelAction{name: "write its own cache record",
		when: func(_ modelDir, l modelLocals) bool { return !l.failed },
		run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if o.p.pauses() && l.pausedAfter {
				return
			}
			d.e[o.x].record = modelRecord{present: true, src: o.user, id: l.outcomeID}
		}})
	return login
}

// modelLogout is `auth logout` of the entry, and equally `config
// update-server --url`, which advances, deletes and purges in the same
// order and differs only in the address it writes last.
func modelLogout(o modelOpts) modelProgram {
	if o.p == modelIDOnly {
		return modelProgram{name: "logout", changer: true, actions: withRelease(o,
			advance(o),
			one(deleteCredential(o)),
			one(purge(o)),
		)}
	}
	return modelProgram{name: "logout", changer: true, actions: withRelease(o,
		advance(o),
		one(deleteCredential(o)),
		one(purge(o)),
		one(resume(o, "resume cache writes", true)),
	)}
}

// modelDeleteServer is `config delete-server`: the credential and the
// records go first, then the entry.
func modelDeleteServer(o modelOpts) modelProgram {
	remove := stampedWrite(o, "remove the entry", func(d *modelDir, _ *modelLocals) {
		d.e[o.x] = modelEntry{}
	})
	if o.p == modelIDOnly {
		return modelProgram{name: "delete-server", changer: true, actions: withRelease(o,
			one(deleteCredential(o)),
			one(purge(o)),
			remove,
		)}
	}
	return modelProgram{name: "delete-server", changer: true, actions: withRelease(o,
		advance(o),
		one(deleteCredential(o)),
		one(purge(o)),
		remove,
	)}
}

// modelDeclaredChange is a login that moves a directory that declares
// the keychain to the file store: the credential commits to the file
// store, the declaration moves readers, and the old keychain copy is
// deleted by the change's replay.
func modelDeclaredChange(o modelOpts) modelProgram {
	declare := plainWrite(o, "declare the file store and name the change", notFailed, func(d *modelDir, _ *modelLocals) {
		d.decl = modelFilePolicy
		d.steps = [2]modelStep{{kind: modelDeleteStep, store: modelKeychain, entry: int8(o.x)}}
	})
	if o.p == modelIDOnly {
		return modelProgram{name: "declared store change", changer: true, actions: withRelease(o,
			one(refuseOthers(o)),
			one(commit(o, "the file store", fixedStore(modelFile), stopOnFailure)),
			one(declare),
			advance(o),
			one(replayStep(o, "replay: delete the old keychain copy")),
		)}
	}
	return modelProgram{name: "declared store change", changer: true, actions: withRelease(o,
		one(refuseOthers(o)),
		advance(o),
		one(commit(o, "the file store", fixedStore(modelFile), skipRestOnFailure)),
		one(declare),
		one(replayStep(o, "replay: delete the old keychain copy")),
		one(resume(o, "resume cache writes when no pin step is left", true)),
	)}
}

// modelPinnedChange is a login that moves an `auto` directory pinned to
// the file store to the keychain: the credential commits to the keychain;
// the old file copy is deleted before the id advances, and when that
// fails the pin flips instead; when both fail the login stops with the
// steps left for a later command.
func modelPinnedChange(o modelOpts) modelProgram {
	name := plainWrite(o, "name the change", notFailed, func(d *modelDir, _ *modelLocals) {
		d.steps = [2]modelStep{{kind: modelDeleteStep, store: modelFile, entry: int8(o.x)}, {kind: modelPinStep, store: modelKeychain}}
	})
	oldCopy := modelAction{name: "delete the old file copy", locked: !o.noLock, durable: true, branches: 2,
		when: notFailed,
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch == 1 {
				l.oldCopyLeft = true
				return
			}
			d.e[o.x].cred[modelFile] = modelNobody
			d.dropStep()
		}}
	pinInstead := modelAction{name: "write the pin instead", locked: !o.noLock, durable: true, branches: 2,
		when: func(_ modelDir, l modelLocals) bool { return !l.failed && l.oldCopyLeft },
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch == 1 {
				// Neither could be done: the login stops and says so.
				l.done = true
				return
			}
			d.pin = modelKeychainPolicy
			d.steps = [2]modelStep{{kind: modelDeleteStep, store: modelFile, entry: int8(o.x)}}
		}}
	rest := replayStep(o, "replay the remaining step")
	keychain := fixedStore(modelKeychain)
	if o.p == modelIDOnly {
		return modelProgram{name: "pinned store change", changer: true, actions: withRelease(o,
			one(refuseOthers(o)),
			one(commit(o, "the keychain", keychain, stopOnFailure)),
			one(name),
			one(oldCopy),
			one(pinInstead),
			advance(o),
			one(rest),
		)}
	}
	return modelProgram{name: "pinned store change", changer: true, actions: withRelease(o,
		one(refuseOthers(o)),
		advance(o),
		one(commit(o, "the keychain", keychain, skipRestOnFailure)),
		one(name),
		one(oldCopy),
		one(pinInstead),
		one(rest),
		one(resume(o, "resume cache writes when no pin step is left", true)),
	)}
}

// modelDeclaredUnreadChange is modelDeclaredChange when the old keychain
// copy could not be read: no step names it, so the change records nothing
// and the copy stays where it is.
func modelDeclaredUnreadChange(o modelOpts) modelProgram {
	declare := plainWrite(o, "declare the file store", notFailed, func(d *modelDir, _ *modelLocals) {
		d.decl = modelFilePolicy
	})
	prog := modelProgram{name: "declared store change, old copy unread", changer: true}
	if o.p == modelIDOnly {
		prog.actions = withRelease(o,
			one(refuseOthers(o)),
			one(commit(o, "the file store", fixedStore(modelFile), stopOnFailure)),
			one(declare),
			advance(o),
		)
		return prog
	}
	prog.actions = withRelease(o,
		one(refuseOthers(o)),
		advance(o),
		one(commit(o, "the file store", fixedStore(modelFile), skipRestOnFailure)),
		one(declare),
		one(resume(o, "resume cache writes when no pin step is left", true)),
	)
	return prog
}

// modelPinnedUnreadChange is modelPinnedChange when the old file copy
// could not be read: the change records only the pin, which runs before
// the id write; when it fails the login stops with the pin left.
func modelPinnedUnreadChange(o modelOpts) modelProgram {
	name := plainWrite(o, "name the change", notFailed, func(d *modelDir, _ *modelLocals) {
		d.steps = [2]modelStep{{kind: modelPinStep, store: modelKeychain}}
	})
	pinEarly := modelAction{name: "write the pin before the id write", locked: !o.noLock, durable: true, branches: 2,
		when: notFailed,
		run: func(d *modelDir, l *modelLocals, branch int, _ *modelCheck) {
			if branch == 1 {
				l.done = true
				return
			}
			d.pin = modelKeychainPolicy
			d.steps = [2]modelStep{}
		}}
	prog := modelProgram{name: "pinned store change, old copy unread", changer: true}
	keychain := fixedStore(modelKeychain)
	if o.p == modelIDOnly {
		prog.actions = withRelease(o,
			one(refuseOthers(o)),
			one(commit(o, "the keychain", keychain, stopOnFailure)),
			one(name),
			one(pinEarly),
			advance(o),
		)
		return prog
	}
	prog.actions = withRelease(o,
		one(refuseOthers(o)),
		advance(o),
		one(commit(o, "the keychain", keychain, skipRestOnFailure)),
		one(name),
		one(pinEarly),
		one(resume(o, "resume cache writes when no pin step is left", true)),
	)
	return prog
}

// modelFirstLogin is the first login on an `auto` directory with no pin,
// the plugin-created login's on a bare install included: nothing is
// stored, so readers take the unpinned decision; the credential commits
// to the keychain, and the change's one step writes the pin readers will
// then resolve by that decision anyway. A pin write that fails leaves the
// step for a later login.
func modelFirstLogin(o modelOpts) modelProgram {
	name := plainWrite(o, "name the change", notFailed, func(d *modelDir, _ *modelLocals) {
		d.steps = [2]modelStep{{kind: modelPinStep, store: modelKeychain}}
	})
	keychain := fixedStore(modelKeychain)
	prog := modelProgram{name: "first login on an unpinned directory", changer: true}
	if o.p == modelIDOnly {
		prog.actions = withRelease(o,
			one(refuseOthers(o)),
			one(commit(o, "the keychain", keychain, stopOnFailure)),
			one(name),
			advance(o),
			one(replayStep(o, "replay: write the pin")),
		)
		return prog
	}
	prog.actions = withRelease(o,
		one(refuseOthers(o)),
		advance(o),
		one(commit(o, "the keychain", keychain, skipRestOnFailure)),
		one(name),
		one(replayStep(o, "replay: write the pin")),
		one(resume(o, "resume cache writes when no pin step would move readers", true)),
	)
	return prog
}

// modelReplay is another command's prelude and last write: under the
// config lock it runs whatever steps the directory's change still has,
// in order, stopping at the first that fails, then resumes every pause it
// may. With every step failing it is also the exec plugin that finds a
// pause on a miss, takes the free lock and resumes what it may.
func modelReplay(p modelProtocol) modelProgram {
	o := modelOpts{p: p, x: -1}
	if p == modelResumeWithoutLock {
		// A process that clears a pause it sees without taking the lock
		// can land inside a change another process is making.
		return modelProgram{name: "another process", actions: one(resume(o, "another process resumes cache writes", false))}
	}
	actions := []modelAction{
		replayStep(o, "another command replays a step"),
		replayStep(o, "another command replays the next step"),
	}
	if p.pauses() {
		actions = append(actions, resume(o, "another command resumes cache writes", false))
	}
	return modelProgram{name: "another command's replay", changer: true, actions: withRelease(o, actions)}
}

// modelRewrite is a config write that changes no credential - a CA-only
// `config update-server`, `config use-server`, a retirement record - and
// rebuilds the entry as it writes it.
func modelRewrite(p modelProtocol, x int) modelProgram {
	return modelProgram{name: "a config rewrite", actions: []modelAction{{
		name: "rewrite the entry", locked: true, release: true, durable: true,
		run: func(d *modelDir, _ *modelLocals, _ int, _ *modelCheck) {
			if p == modelRewriteDropsPause {
				d.e[x].paused = modelNotPaused
			}
		}}}}
}

// modelWriter is the exec plugin on a cache miss with a stored credential:
// its store comes from an early config load and the pin read at its first
// credential call; the binding it writes the record under comes from a
// later load; it reads the credential after that load, makes a token over
// the network, resolves the store again the way a reader would from the
// binding load, and writes the record under the cache lock.
func modelWriter(p modelProtocol, x int) modelProgram { return modelCacheWriter(p, x, false) }

// modelFallbackWriter is modelWriter when its store holds nothing for the
// entry and a terminal is attached: the plugin signs in interactively and
// writes that token under the same binding.
func modelFallbackWriter(p modelProtocol, x int) modelProgram {
	w := modelCacheWriter(p, x, true)
	w.name = "cache writer with the interactive fallback"
	return w
}

func modelCacheWriter(p modelProtocol, x int, fallback bool) modelProgram {
	return modelProgram{name: "cache writer", actions: []modelAction{
		{name: "writer: load the declaration", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.decl = d.decl
		}},
		{name: "writer: resolve its store", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.store, l.storeOK = d.storeFor(l.decl, d.pin)
		}},
		{name: "writer: load the binding", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			e := d.e[x]
			l.loadID, l.loadEntry, l.loadPaused, l.bindDecl = e.id, e.exists, e.paused, d.decl
		}},
		{name: "writer: read the credential", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if !l.storeOK || !l.loadEntry {
				l.done = true
				return
			}
			l.cred = d.e[x].cred[l.store]
			if l.cred == modelNobody {
				if !fallback {
					l.done = true
					return
				}
				l.cred = modelSignedIn
			}
		}},
		{name: "writer: resolve the store again from the binding load", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.recheck, l.recheckOK = d.storeFor(l.bindDecl, d.pin)
		}},
		{name: "writer: write the record", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if p.pauses() && p != modelWriterIgnoresPause && l.loadPaused != modelNotPaused {
				return
			}
			if p.pauses() && p != modelWriterSkipsStoreCheck && (!l.recheckOK || l.recheck != l.store) {
				return
			}
			d.e[x].record = modelRecord{present: true, src: l.cred, id: l.loadID}
		}},
	}}
}

// modelReader is a cache hit as CachedHit runs it: the stamp, then the
// load, then the record judged against the load, then the stamp again; a
// stale view or a moved stamp gets exactly one second look. The check is
// made where the credential is handed back.
func modelReader(x int) modelProgram {
	return modelProgram{name: "cache reader", actions: []modelAction{
		{name: "reader: read the stamp", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.anchor = d.stampFile
		}},
		{name: "reader: load the config", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.loadID, l.loadStamp, l.loadEntry = d.e[x].id, d.cfgStamp, d.e[x].exists
		}},
		{name: "reader: judge the record", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if l.anchor != l.loadStamp {
				if l.reloaded {
					l.done = true
					return
				}
				l.reloaded = true
				l.pc = 0
				return
			}
			r := d.e[x].record
			if !r.present || !l.loadEntry || r.id != l.loadID {
				l.done = true
				return
			}
			l.served = r.src
		}},
		{name: "reader: confirm the stamp", run: func(d *modelDir, l *modelLocals, _ int, c *modelCheck) {
			if d.stampFile != l.anchor {
				if l.reloaded {
					l.done = true
					return
				}
				l.reloaded = true
				l.anchor = d.stampFile
				l.pc = 1
				return
			}
			if got := d.resolved(x); !servable(l.served, got) {
				c.violate(fmt.Sprintf("a hit for entry %d served a token made from %s's credential while it resolves %s's", x, l.served, got))
			}
		}},
	}}
}

// modelWorld is the whole state the enumeration visits: the directory and
// every process's memory. It is comparable, so a state reached twice is
// explored once.
type modelWorld struct {
	d modelDir
	p [4]modelLocals
}

type modelRun struct {
	programs []modelProgram
	seen     map[modelWorld]struct{}
	check    modelCheck
	states   int
	// firstOnly stops the enumeration at the first violation, for a
	// caller that only asks whether there is one.
	firstOnly bool
}

// settle marks a process done past its last action, and gives the config
// lock back when a process that ended still holds it.
func settle(w *modelWorld, i int, n int) {
	l := &w.p[i]
	if int(l.pc) >= n {
		l.done = true
	}
	if l.done && w.d.lock == int8(i+1) {
		w.d.lock = 0
	}
}

func (r *modelRun) explore(w modelWorld) {
	if r.firstOnly && r.check.count > 0 {
		return
	}
	if _, ok := r.seen[w]; ok {
		return
	}
	r.seen[w] = struct{}{}
	r.states++

	moved := false
	for i, prog := range r.programs {
		l := w.p[i]
		if l.done {
			continue
		}
		a := prog.actions[l.pc]
		if a.locked && w.d.lock != 0 && w.d.lock != int8(i+1) {
			continue
		}
		moved = true
		if a.when != nil && !a.when(w.d, l) {
			// The condition is read where the action would run: under
			// the config lock for a locked action, so a process that
			// waits on the lock decides only once it holds it.
			next := w
			if a.locked {
				next.d.lock = int8(i + 1)
			}
			next.p[i].pc++
			if a.release {
				next.d.lock = 0
			}
			settle(&next, i, len(prog.actions))
			r.explore(next)
			continue
		}
		branches := max(a.branches, 1)
		for b := range branches {
			next := w
			nl := &next.p[i]
			if a.locked {
				next.d.lock = int8(i + 1)
			}
			nl.pc++
			label := prog.name + ": " + a.name
			if b > 0 {
				label += fmt.Sprintf(" - outcome %d", b)
			}
			r.check.trace = append(r.check.trace, label)
			a.run(&next.d, nl, b, &r.check)
			if a.release {
				next.d.lock = 0
			}
			settle(&next, i, len(prog.actions))
			r.explore(next)
			if a.durable && prog.changer && !nl.done {
				crashed := next
				crashed.p[i].done, crashed.p[i].crashed = true, true
				if crashed.d.lock == int8(i+1) {
					crashed.d.lock = 0
				}
				r.check.trace = append(r.check.trace, prog.name+": crashes")
				r.explore(crashed)
				r.check.trace = r.check.trace[:len(r.check.trace)-1]
			}
			r.check.trace = r.check.trace[:len(r.check.trace)-1]
		}
	}
	if !moved {
		r.final(w)
	}
}

// final checks the records the run left behind, now and after a later
// command finishes the change: a record a reader would serve must be
// made from the credential its entry then resolves.
func (r *modelRun) final(w modelWorld) {
	for _, d := range []modelDir{w.d, w.d.finished()} {
		for i, e := range d.e {
			rec := e.record
			if rec.present && e.exists && rec.id == e.id && !servable(rec.src, d.resolved(i)) {
				r.check.violate(fmt.Sprintf("a record for entry %d made from %s's credential stays valid while it resolves %s's", i, rec.src, d.resolved(i)))
				return
			}
		}
	}
}

// modelStart is the directory before the change: prod (entry 0) holds
// alice's credential in the store its readers use, and, when seeded, a
// cache record made from it under the current id; staging (entry 1)
// exists with no credential.
func modelStart(decl, pin modelPolicy, store modelStore, seeded bool) modelDir {
	d := modelDir{stampFile: -1, cfgStamp: -1, decl: decl, pin: pin}
	d.e[0] = modelEntry{exists: true, id: -2}
	d.e[0].cred[store] = modelAlice
	if seeded {
		d.e[0].record = modelRecord{present: true, src: modelAlice, id: -2}
	}
	d.e[1] = modelEntry{exists: true, id: -3}
	return d
}

func keychainDeclared(seeded bool) modelDir {
	return modelStart(modelKeychainPolicy, modelNoPolicy, modelKeychain, seeded)
}

// bareAuto is an `auto` directory with no pin and nothing stored: prod
// and staging exist and neither holds a credential. It has no record
// made from a credential to seed, so seeded makes no difference.
func bareAuto(bool) modelDir {
	d := modelStart(modelAutoPolicy, modelNoPolicy, modelFile, false)
	d.e[0].cred = [2]modelUser{}
	return d
}

func autoPinnedFile(seeded bool) modelDir {
	return modelStart(modelAutoPolicy, modelFilePolicy, modelFile, seeded)
}

type modelChange struct {
	program func(modelOpts) modelProgram
	start   func(seeded bool) modelDir
	// storeChange marks a change of the directory's credential store,
	// which the fallback, rewrite and staging scenarios run against.
	storeChange bool
}

func modelChanges() []modelChange {
	return []modelChange{
		{modelLogin, keychainDeclared, false},
		{modelPluginLogin, keychainDeclared, false},
		{modelLogout, keychainDeclared, false},
		{modelDeleteServer, keychainDeclared, false},
		{modelDeclaredChange, keychainDeclared, true},
		{modelPinnedChange, autoPinnedFile, true},
		{modelDeclaredUnreadChange, keychainDeclared, true},
		{modelPinnedUnreadChange, autoPinnedFile, true},
		{modelFirstLogin, bareAuto, true},
	}
}

func storeChanges() []modelChange {
	return slices.DeleteFunc(modelChanges(), func(c modelChange) bool { return !c.storeChange })
}

// modelScenario is one set of processes run against one starting
// directory, with and without a record made before the change.
type modelScenario struct {
	name     string
	start    func(seeded bool) modelDir
	programs []modelProgram
}

// modelScenarios is every combination the invariant is checked on:
//
//   - each change of prod against a cache writer, a cache reader and
//     another command's replay;
//   - each store change of prod against a writer that falls back to an
//     interactive sign-in, a reader and a replay;
//   - each store change of prod against a config rewrite of prod;
//   - each store change of prod against a login of staging, to the store
//     readers use or the one a pending pin names, with staging's writer
//     and reader;
//   - each change of prod made without the config lock, against a writer,
//     a reader and a locked replay;
//
// except that the binding-id-only protocol has no pause to check, so it
// gets only the first.
func modelScenarios(p modelProtocol) []modelScenario {
	prod := modelOpts{p: p, x: 0, user: modelBob}
	var out []modelScenario
	for _, c := range modelChanges() {
		change := c.program(prod)
		out = append(out, modelScenario{change.name, c.start,
			[]modelProgram{change, modelWriter(p, 0), modelReader(0), modelReplay(p)}})
	}
	if !p.pauses() {
		return out
	}
	for _, c := range storeChanges() {
		change := c.program(prod)
		out = append(out, modelScenario{change.name + " with the interactive fallback", c.start,
			[]modelProgram{change, modelFallbackWriter(p, 0), modelReader(0), modelReplay(p)}})
		out = append(out, modelScenario{change.name + " with a config rewrite", c.start,
			[]modelProgram{change, modelRewrite(p, 0), modelWriter(p, 0), modelReader(0)}})
		staging := modelOpts{p: p, x: 1, user: modelTom}
		for _, login := range []func(modelOpts) modelProgram{modelLogin, modelLoginToPinnedStore} {
			other := login(staging)
			out = append(out, modelScenario{change.name + " with staging's " + other.name, c.start,
				[]modelProgram{change, other, modelWriter(p, 1), modelReader(1)}})
		}
	}
	unlocked := prod
	unlocked.noLock = true
	for _, c := range modelChanges() {
		change := c.program(unlocked)
		out = append(out, modelScenario{change.name + " without the lock", c.start,
			[]modelProgram{change, modelWriter(p, 0), modelReader(0), modelReplay(p)}})
	}
	return out
}

func (s modelScenario) run(firstOnly bool) (check modelCheck, states int) {
	for _, seeded := range []bool{false, true} {
		r := &modelRun{programs: s.programs, seen: map[modelWorld]struct{}{}, firstOnly: firstOnly}
		r.explore(modelWorld{d: s.start(seeded)})
		check.count += r.check.count
		check.traces = append(check.traces, r.check.traces...)
		states += r.states
		if firstOnly && check.count > 0 {
			break
		}
	}
	return check, states
}

// TestCacheModelBindingIDOnlyBreaksTheInvariant asserts that the model
// finds a window in the binding-id-only protocol for every change it
// models from a directory with a credential to change: a logout or
// delete-server racing a writer that read the credential before the
// delete, a login that crashes between its commit and its id write, and a
// store change whose steps another command finishes after the id was
// written. A first login on a bare directory has no previous credential,
// so no window.
func TestCacheModelBindingIDOnlyBreaksTheInvariant(t *testing.T) {
	t.Parallel()
	for _, s := range modelScenarios(modelIDOnly) {
		if s.start(true).resolved(0) == modelNobody {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			check, states := s.run(false)
			require.NotEmpty(t, check.traces, "the model found no window in the binding-id-only protocol")
			t.Logf("%d states, %d violations; the first:\n  %s", states, check.count, check.traces[0])
		})
	}
}

// TestCacheModelPausedWritesKeepTheInvariant asserts that with cache
// writes paused from a change's first write until nothing it left can
// still move the entry's readers, no interleaving of the scenarios, with
// a crash after any write of a change, serves or leaves a record made
// from a credential its entry does not resolve.
func TestCacheModelPausedWritesKeepTheInvariant(t *testing.T) {
	t.Parallel()
	for _, s := range modelScenarios(modelPaused) {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			check, states := s.run(false)
			t.Logf("%d states, %d violations", states, check.count)
			require.Zero(t, check.count, "\n  %s", strings.Join(check.traces, "\n  "))
		})
	}
}

// TestCacheModelTwoChangesKeepTheInvariant asserts the paused protocol for
// a login and a logout of the same entry racing each other, a cache
// writer and a cache reader: each change pauses and resumes inside its own
// config lock span, so neither can resume cache writes the other paused.
func TestCacheModelTwoChangesKeepTheInvariant(t *testing.T) {
	t.Parallel()
	prod := modelOpts{p: modelPaused, x: 0, user: modelBob}
	r := &modelRun{
		programs: []modelProgram{modelLogin(prod), modelLogout(prod), modelWriter(modelPaused, 0), modelReader(0)},
		seen:     map[modelWorld]struct{}{},
	}
	r.explore(modelWorld{d: keychainDeclared(true)})
	t.Logf("login and logout: %d states, %d violations", r.states, r.check.count)
	require.Zero(t, r.check.count, "\n  %s", strings.Join(r.check.traces, "\n  "))
}

// TestCacheModelFindsEveryWeakenedRule asserts that each rule of the
// paused protocol is one the model can tell is needed: with any one of
// them weakened, some interleaving serves or leaves a record made from a
// credential its entry does not resolve.
func TestCacheModelFindsEveryWeakenedRule(t *testing.T) {
	t.Parallel()
	for p := modelWriterIgnoresPause; p <= modelResumeUnlockedPause; p++ {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			found := ""
			for _, s := range modelScenarios(p) {
				if check, _ := s.run(true); check.count > 0 {
					found = s.name + ":\n  " + check.traces[0]
					break
				}
			}
			require.NotEmpty(t, found, "the model found no violation")
			t.Logf("first found in %s", found)
		})
	}
}

// TestCacheModelFirstLoginResumesWithItsPinLeft asserts that a first login
// on an unpinned directory whose pin write fails still ends with cache
// writes resumed, with no later command: its pin would move no reader.
// On a directory only the exec plugin uses, nothing else would ever
// resume it.
func TestCacheModelFirstLoginResumesWithItsPinLeft(t *testing.T) {
	t.Parallel()
	change := modelFirstLogin(modelOpts{p: modelPaused, x: 0, user: modelBob})
	r := &modelRun{programs: []modelProgram{change}, seen: map[modelWorld]struct{}{}}
	r.explore(modelWorld{d: bareAuto(false)})
	ended := 0
	for w := range r.seen {
		l := w.p[0]
		if !l.done || l.crashed || !w.d.pinPending() {
			continue
		}
		ended++
		assert.Equal(t, modelNotPaused, w.d.e[0].paused, "a first login left paused by its pin step: %+v", w.d)
	}
	require.Positive(t, ended, "no end state with the pin step left")
}

// TestCacheModelEveryPauseIsResumable asserts that every state a change
// made under the config lock can end in - finished, stopped on an error,
// or crashed - is one another command takes back to cache writes resumed
// once the change's steps succeed, so no entry keeps its cache off for
// good. A pause made without the lock is left to the entry's next change.
func TestCacheModelEveryPauseIsResumable(t *testing.T) {
	t.Parallel()
	prod := modelOpts{p: modelPaused, x: 0, user: modelBob}
	for _, c := range modelChanges() {
		change := c.program(prod)
		r := &modelRun{programs: []modelProgram{change, modelWriter(modelPaused, 0)}, seen: map[modelWorld]struct{}{}}
		r.explore(modelWorld{d: c.start(true)})
		for w := range r.seen {
			if !w.p[0].done || !w.p[1].done {
				continue
			}
			replay := &modelRun{programs: []modelProgram{modelReplay(modelPaused)}, seen: map[modelWorld]struct{}{}}
			replay.explore(modelWorld{d: w.d})
			resumed := false
			for end := range replay.seen {
				if end.p[0].done && !end.p[0].crashed && !end.d.hasSteps() && end.d.e[0].paused == modelNotPaused {
					resumed = true
				}
			}
			assert.True(t, resumed, "%s: a pause no replay clears: %+v", change.name, w.d)
		}
	}
}
