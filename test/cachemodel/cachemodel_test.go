// Package cachemodel is the model of the exec-credential cache described
// below. It is a package of its own, apart from internal/config, because
// scripts/validate and scripts/test run it without the race detector: the
// model enumerates states on one goroutine per scenario, sharing nothing
// between them, so the detector has nothing to find and the run takes
// several times longer.
package cachemodel

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is a model of the exec-credential cache across a change of an
// entry's credential. It does not run internal/config's code: it states the
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
//     lock only by the process that made it;
//   - a kubeconfig call's record is bound to the entry the call's --name
//     identified, or to none, and is not served once the lookup identifies
//     another entry or refuses the call;
//   - a record made from the identified entry's own credential is bound
//     to that entry alone, and, for a plugin login entry, to the
//     token-reuse setting;
//   - any other record of a call that identifies an entry - the plugin's
//     own login entry answering for it, the plugin-created login, an
//     interactive sign-in - is bound to that entry, to the setting, and,
//     while the setting is on, to the URL's plugin login entry, which may
//     neither change nor appear;
//   - the record of a call that identifies no entry is bound to every
//     entry for the URL, which may neither change, appear nor go, and to
//     the current server and the setting;
//   - each id comes from a load made before that entry's credential was
//     read (for the entry a plugin-created login writes, the id its last
//     write wrote), a record is written only when none of its entries was
//     paused there, and a config write that changes the current server or
//     the setting moves the binding stamp.
//
// The first protocol is asserted to break the invariant for every change
// it models, which is what shows the model can see the windows; the
// second is asserted to keep it everywhere; and each of the second's
// rules, weakened on its own, is asserted to break it again.
//
// A call is modelled beside the entries. Its --name identifies prod
// (entry 0), staging (entry 1), or no entry at all; entry 2 is an entry
// named exactly like the --name, absent until a login creates it, which
// then wins the identification, or, registered at another URL, makes the
// lookup refuse the call. Staging is the plugin's own login entry unless
// the call identifies no entry; then it is another entry for the URL, read
// after the current server's. A miss with token reuse on goes to the
// plugin-created login; with it off, to an interactive sign-in. The call
// has one cache record, and the invariant is checked for it as for an
// entry: served or left on disk, it must be made from the credential the
// call's lookup gives, and none may be served where the lookup refuses.

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
	// modelCallBoundToOneEntry binds the call's record to the one entry
	// whose credential made it or, for an interactive sign-in, to the
	// entry the lookup prefers, so a login of another entry for the URL
	// leaves the record valid.
	modelCallBoundToOneEntry
	modelCallIgnoresOtherPause
	modelCallTakesIDsFromOutcome
	// modelCallUnboundRanking binds the call's record to its URL's entries
	// but not to what ranks them, and lets config use-server and config
	// set-token-reuse write without moving the stamp.
	modelCallUnboundRanking
	modelCallIgnoresRankingInputs
	modelRankingWriteWithoutStamp
	modelCallIgnoresIdentification
	modelIdentifiedCallDropsPluginEntries
	modelPluginEntryRecordIgnoresReuse
	modelUnnamedRecordNotExact
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
		"a call's record bound to one entry",
		"a call's record written while another entry it is bound to is paused",
		"a plugin-created login's record bound to the other entry's id from its outcome",
		"a call's record bound to no ranking input, and ranking writes that move no stamp",
		"a call's record bound to no ranking input",
		"a ranking write that moves no stamp",
		"a call's record not bound to the entry the call identified",
		"an identified call's record not bound to the plugin's login entry",
		"a plugin login entry's own record not bound to the token-reuse setting",
		"an unnamed call's record whose entry list is not exact",
	}[p]
}

// bindsRanking reports whether a call's record carries the current server
// and the token-reuse setting its load returned.
func (p modelProtocol) bindsRanking() bool {
	return p != modelCallUnboundRanking && p != modelCallIgnoresRankingInputs
}

// stampsRanking reports whether a write of the current server or the
// token-reuse setting moves the binding stamp.
func (p modelProtocol) stampsRanking() bool {
	return p != modelCallUnboundRanking && p != modelRankingWriteWithoutStamp
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
		return "keychain"
	}
	return "file"
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
	// modelRefused is the answer of a lookup that refuses the call: no
	// token may be served for it.
	modelRefused
)

func (u modelUser) String() string {
	return [...]string{"nobody", "alice", "bob", "tom", "an interactive sign-in", "a refusal"}[u]
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

// The call's kinds: which entry its --name identifies when no entry is
// named exactly like the --name.
const (
	callNamesProd int8 = iota
	callNamesStaging
	callNamesNone
)

// modelCallView is what one config load returns that the call's lookup
// and its record depend on: each entry's id, whether it exists and whether
// its cache writes are paused, the current server (an entry index, -1 for
// none) and the token-reuse setting.
type modelCallView struct {
	ids     [3]int8
	exists  [3]bool
	paused  [3]int8
	current int8
	reuse   bool
}

// modelCallRecord is the call's one exec-credential cache record: the user
// whose credential made it; the entries it is bound to, with the binding
// id each had; the entries whose existence at the call's URL must stay as
// it was (exact); and, where bound, the entry the call identified, the
// current server and the token-reuse setting it was written under. A
// record bound to nothing at all was made for a server the directory had
// no entry for.
type modelCallRecord struct {
	present      bool
	src          modelUser
	bound        [3]bool
	ids          [3]int8
	exact        [3]bool
	identBound   bool
	ident        int8
	currentBound bool
	current      int8
	reuseBound   bool
	reuse        bool
}

// modelEntry is one server entry: the fields of config.yaml the protocol
// reads, what each store holds for it, and its cache record.
type modelEntry struct {
	exists bool
	id     int8
	paused int8
	cred   [2]modelUser
	record modelRecord
	// otherURL is an entry registered at another URL than the call's.
	otherURL bool
}

// modelDir is everything durable in the directory: the binding stamp
// file, the directory-wide fields of config.yaml, the change record's
// remaining steps, the pin, three entries, the current server (an entry
// index, -1 for none), the token-reuse setting and the call's cache
// record. kind is what the call's --name identifies; it never changes.
// lock is the config lock's holder (a process index plus one), zero when
// free.
type modelDir struct {
	stampFile int8
	cfgStamp  int8
	decl      modelPolicy
	steps     [2]modelStep
	pin       modelPolicy
	e         [3]modelEntry
	current   int8
	reuse     bool
	kind      int8
	call      modelCallRecord

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

// callView is what a config load made now returns for the call.
func (d modelDir) callView() modelCallView {
	v := modelCallView{current: d.current, reuse: d.reuse}
	for i, e := range d.e {
		v.ids[i], v.exists[i], v.paused[i] = e.id, e.exists, e.paused
	}
	return v
}

// atURL reports whether entry i exists in v and answers for the call's
// URL.
func (d modelDir) atURL(i int8, v modelCallView) bool {
	return v.exists[i] && !d.e[i].otherURL
}

// callPlan is the exec plugin's entry lookup over one view: the entry the
// call identifies (-1 for none), whether the identification refuses the
// call, and the entries whose credentials it reads, in order.
//
//   - An entry named exactly like the call's --name - entry 2 - wins:
//     at the call's URL it is identified; at another URL the call is
//     refused.
//   - Else the call identifies prod or staging as its kind says, when that
//     entry exists.
//   - Staging is the plugin's own login entry unless the kind names no
//     entry; then it is another entry for the URL.
//   - An identified plugin login entry is read only while token reuse is
//     on, and nothing is read after it. Any other identified entry is read
//     alone, then, while the setting is on, the plugin's login entry.
//   - A call that identifies nothing reads the entries for its URL, the
//     current server's first; the plugin's login entry only while the
//     setting is on.
func (d modelDir) callPlan(v modelCallView) (ident int8, refused bool, order [2]int8, n int8) {
	stagingIsPlugin := d.kind != callNamesNone
	switch {
	case v.exists[2] && d.e[2].otherURL:
		return -1, true, order, 0
	case v.exists[2]:
		ident = 2
	case d.kind == callNamesProd && v.exists[0]:
		ident = 0
	case d.kind == callNamesStaging && v.exists[1]:
		ident = 1
	default:
		ident = -1
	}
	switch {
	case ident == 1:
		if v.reuse {
			order[0], n = 1, 1
		}
	case ident >= 0:
		order[0], n = ident, 1
		if stagingIsPlugin && v.reuse && d.atURL(1, v) {
			order[1], n = 1, 2
		}
	default:
		first, second := int8(0), int8(1)
		if v.current == 1 {
			first, second = 1, 0
		}
		for _, i := range [2]int8{first, second} {
			if !d.atURL(i, v) || (i == 1 && stagingIsPlugin && !v.reuse) {
				continue
			}
			order[n] = i
			n++
		}
	}
	return ident, false, order, n
}

// callCurrent is the current server as a record is bound to it: the
// entry when it answers for the call's URL, else none, since the lookup
// only asks whether a candidate is the current server.
func (d modelDir) callCurrent(v modelCallView) int8 {
	if v.current >= 0 && d.atURL(v.current, v) {
		return v.current
	}
	return -1
}

// resolvedCall is the user whose credential a reader of the call gets now:
// the first entry of the lookup that holds one, nobody's when none does,
// and a refusal when the identification refuses the call.
func (d modelDir) resolvedCall() modelUser {
	_, refused, order, n := d.callPlan(d.callView())
	if refused {
		return modelRefused
	}
	for _, i := range order[:n] {
		if u := d.resolved(int(i)); u != modelNobody {
			return u
		}
	}
	return modelNobody
}

// callMatches reports whether a reader whose load returned v would serve
// r: the identification, the current server and the setting it is bound
// to are unchanged; every entry it is bound to exists with the id it was
// written under; every exact entry exists at the URL exactly when it is
// bound; and a record bound to nothing is served only while no entry
// answers for the URL.
func (d modelDir) callMatches(r modelCallRecord, v modelCallView) bool {
	if !r.present {
		return false
	}
	if r.identBound {
		if ident, refused, _, _ := d.callPlan(v); refused || ident != r.ident {
			return false
		}
	}
	if r.currentBound && d.callCurrent(v) != r.current {
		return false
	}
	if r.reuseBound && v.reuse != r.reuse {
		return false
	}
	if r.bound == [3]bool{} && r.exact == [3]bool{} {
		return !d.atURL(0, v) && !d.atURL(1, v) && !d.atURL(2, v)
	}
	for i := range int8(3) {
		if r.bound[i] && (!v.exists[i] || v.ids[i] != r.ids[i]) {
			return false
		}
		if r.exact[i] && d.atURL(i, v) != r.bound[i] {
			return false
		}
	}
	return true
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

	// What a process that works for the call loaded and the lookup that
	// load gives, which entry's credential answered (-1 for none), and what
	// a plugin-created login's outcome reports of the identified entry.
	cv                modelCallView
	ident             int8
	refused           bool
	order             [2]int8
	n                 int8
	servedBy          int8
	outcomeIdentID    int8
	outcomeIdentPause int8
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
		// A login of an entry that does not exist creates it.
		e.exists = true
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

// loadCall is a config load made for the call, and the lookup it gives.
func loadCall(d *modelDir, l *modelLocals) {
	l.cv = d.callView()
	l.ident, l.refused, l.order, l.n = d.callPlan(l.cv)
}

// callBinding is the record a call writes for a token made from src, from
// the load l made (l.cv) and the entry whose credential answered
// (l.servedBy, -1 for none):
//
//   - made from the identified entry's own credential: bound to that
//     entry alone, and, for a plugin login entry, to the token-reuse
//     setting;
//   - otherwise, for a call that identifies an entry: bound to that entry,
//     to the plugin's login entry for the URL while the setting is on,
//     exactly, and to the setting;
//   - for a call that identifies none: bound to every entry for the URL,
//     exactly, to the current server and to the setting;
//   - and in every case to the identification itself.
//
// The weakened rules each drop one part; modelCallBoundToOneEntry binds
// only the entry whose credential made the token, or, for an interactive
// sign-in, the entry the lookup prefers. ok is false when an entry it
// binds was paused in the load; modelCallIgnoresOtherPause looks only at
// that one entry's pause.
func callBinding(p modelProtocol, d modelDir, l modelLocals, src modelUser) (rec modelCallRecord, ok bool) {
	v := l.cv
	rec = modelCallRecord{present: true, src: src}
	own := l.servedBy
	if own < 0 {
		own = l.ident
		if own < 0 && l.n > 0 {
			own = l.order[0]
		}
	}
	bind := func(i int8) {
		if i >= 0 && v.exists[i] {
			rec.bound[i], rec.ids[i] = true, v.ids[i]
		}
	}
	switch {
	case p == modelCallBoundToOneEntry:
		bind(own)
	case l.ident >= 0 && l.servedBy == l.ident:
		bind(l.ident)
		if l.ident == 1 && p.bindsRanking() && p != modelPluginEntryRecordIgnoresReuse {
			rec.reuseBound, rec.reuse = true, v.reuse
		}
	case l.ident >= 0:
		bind(l.ident)
		if d.kind != callNamesNone && v.reuse && l.ident != 1 && p != modelIdentifiedCallDropsPluginEntries {
			bind(1)
			rec.exact[1] = true
		}
		if p.bindsRanking() {
			rec.reuseBound, rec.reuse = true, v.reuse
		}
	default:
		for i := range int8(3) {
			if d.atURL(i, v) {
				bind(i)
			}
			rec.exact[i] = !d.e[i].otherURL && p != modelUnnamedRecordNotExact
		}
		if p.bindsRanking() {
			rec.currentBound, rec.current = true, d.callCurrent(v)
			rec.reuseBound, rec.reuse = true, v.reuse
		}
	}
	if p != modelCallBoundToOneEntry && p != modelCallIgnoresIdentification {
		rec.identBound, rec.ident = true, l.ident
	}
	if p == modelWriterIgnoresPause {
		return rec, true
	}
	for i := range int8(3) {
		if !rec.bound[i] || v.paused[i] == modelNotPaused {
			continue
		}
		if p == modelCallIgnoresOtherPause && i != own {
			continue
		}
		return rec, false
	}
	return rec, true
}

// modelCallWriter is the exec plugin on a cache miss for the call: its
// store comes from an early config load and the pin read after it, the
// binding and the lookup from a later load; then it reads the lookup's
// entries in order. With no credential, token reuse on sends it to the
// plugin-created login, which modelCallPluginLogin is, and off to an
// interactive sign-in. It resolves the store again from the binding load,
// as a reader would, and writes the record under the cache lock.
func modelCallWriter(p modelProtocol) modelProgram {
	read := func(k int8) func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
		return func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if k < l.n {
				i := l.order[k]
				if u := d.e[i].cred[l.store]; u != modelNobody {
					l.cred, l.servedBy = u, i
					return
				}
			}
			if k == 1 {
				if l.cv.reuse {
					l.done = true
					return
				}
				l.cred = modelSignedIn
			}
		}
	}
	return modelProgram{name: "call writer", actions: []modelAction{
		{name: "writer: load the declaration", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.decl = d.decl
		}},
		{name: "writer: resolve its store", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.store, l.storeOK = d.storeFor(l.decl, d.pin)
		}},
		{name: "writer: load the binding", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			loadCall(d, l)
			l.bindDecl, l.servedBy = d.decl, -1
			if !l.storeOK || l.refused {
				l.done = true
			}
		}},
		{name: "writer: read the first entry's credential", run: read(0)},
		{name: "writer: read the next entry's credential",
			when: func(_ modelDir, l modelLocals) bool { return l.servedBy < 0 },
			run:  read(1)},
		{name: "writer: resolve the store again from the binding load", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.recheck, l.recheckOK = d.storeFor(l.bindDecl, d.pin)
		}},
		{name: "writer: write the call's record", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if p != modelWriterSkipsStoreCheck && (!l.recheckOK || l.recheck != l.store) {
				return
			}
			if rec, ok := callBinding(p, *d, *l, l.cred); ok {
				d.call = rec
			}
		}},
	}}
}

// modelCallPluginLogin is the call's miss when token reuse is on and no
// entry of its lookup holds a credential: under the plugin-login lock the
// waiter loads the config - with the setting off it stops there - and
// reads the lookup's entries again through a store opened as a reader's;
// with none holding a credential the plugin logs in to staging, its own
// login entry, creating it when it does not exist, and writes the call's
// record for the token it made from the new credential. Staging's id
// comes from the login's outcome, as its last write wrote it; every other
// entry's from the waiter's load, which came before the read that found
// it empty. modelCallTakesIDsFromOutcome takes the identified entry's
// from the outcome too.
func modelCallPluginLogin(o modelOpts) modelProgram {
	login := modelLogin(o)
	login.name = "call's plugin-created login"
	pre := []modelAction{
		{name: "waiter: load the config", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			loadCall(d, l)
			l.store, l.storeOK = d.readerStore()
		}},
		{name: "waiter: read the lookup's credentials", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if !l.storeOK || l.refused || !l.cv.reuse {
				l.done = true
				return
			}
			for _, i := range l.order[:l.n] {
				if d.e[i].cred[l.store] != modelNobody {
					// A credential answers: that is a writer's case.
					l.done = true
				}
			}
		}},
	}
	last := len(login.actions) - 1
	login.actions[last].run = func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
		l.pausedAfter = d.e[o.x].paused != modelNotPaused
		if l.ident >= 0 {
			l.outcomeIdentID, l.outcomeIdentPause = d.e[l.ident].id, d.e[l.ident].paused
		}
	}
	login.actions = append(pre, login.actions...)
	login.actions = append(login.actions, modelAction{name: "write the call's record",
		when: func(_ modelDir, l modelLocals) bool { return !l.failed },
		run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if l.pausedAfter {
				return
			}
			// modelWriterIgnoresPause and modelWriterSkipsStoreCheck weaken
			// the plain writer only; this write keeps both rules.
			p := o.p
			if p == modelWriterIgnoresPause || p == modelWriterSkipsStoreCheck {
				p = modelPaused
			}
			x := int8(o.x)
			w := *l
			w.servedBy = x
			w.cv.ids[x], w.cv.exists[x], w.cv.paused[x] = l.outcomeID, true, modelNotPaused
			if p == modelCallTakesIDsFromOutcome && l.ident >= 0 && l.ident != x {
				w.cv.ids[l.ident], w.cv.paused[l.ident] = l.outcomeIdentID, l.outcomeIdentPause
			}
			if rec, ok := callBinding(p, *d, w, o.user); ok {
				d.call = rec
			}
		}})
	return login
}

// modelCallReader is a cache hit for the call, as CachedHit runs it: the
// stamp, the load, the record judged against the load, the stamp again,
// with one second look; the check is made where the credential is handed
// back.
func modelCallReader() modelProgram {
	return modelProgram{name: "call reader", actions: []modelAction{
		{name: "reader: read the stamp", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.anchor = d.stampFile
		}},
		{name: "reader: load the config", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			l.cv = d.callView()
			l.loadStamp = d.cfgStamp
		}},
		{name: "reader: judge the call's record", run: func(d *modelDir, l *modelLocals, _ int, _ *modelCheck) {
			if l.anchor != l.loadStamp {
				if l.reloaded {
					l.done = true
					return
				}
				l.reloaded = true
				l.pc = 0
				return
			}
			if !d.callMatches(d.call, l.cv) {
				l.done = true
				return
			}
			l.served = d.call.src
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
			if got := d.resolvedCall(); !servable(l.served, got) {
				c.violate(fmt.Sprintf("a hit for the call served a token made from %s's credential while the call's lookup gives %s", l.served, got))
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
		if c := d.call; d.callMatches(c, d.callView()) && !servable(c.src, d.resolvedCall()) {
			r.check.violate(fmt.Sprintf("the call's record made from %s's credential stays valid while the call's lookup gives %s", c.src, d.resolvedCall()))
			return
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

// callSetup is a starting directory for the call's scenarios: the call's
// kind, the token-reuse setting, the current server, what prod, staging
// and entry 2 hold in the keychain, which of prod and staging are absent,
// whether entry 2 - absent until a login creates it - answers for another
// URL, and the record seeded before the change: made from src, answered
// by servedBy (-1 for an interactive sign-in), in the protocol's binding.
// With src nobody there is no record to seed.
type callSetup struct {
	kind     int8
	reuse    bool
	current  int8
	cred     [3]modelUser
	absent   [2]bool
	otherURL bool
	src      modelUser
	servedBy int8
}

func (c callSetup) start(p modelProtocol) func(seeded bool) modelDir {
	return func(seeded bool) modelDir {
		d := keychainDeclared(false)
		for i := range d.e {
			d.e[i].cred = [2]modelUser{}
			d.e[i].cred[modelKeychain] = c.cred[i]
		}
		d.e[2].exists, d.e[2].id, d.e[2].otherURL = false, -4, c.otherURL
		for i, gone := range c.absent {
			if gone {
				d.e[i] = modelEntry{}
			}
		}
		d.kind, d.reuse, d.current = c.kind, c.reuse, c.current
		if seeded && c.src != modelNobody {
			l := modelLocals{servedBy: c.servedBy}
			loadCall(&d, &l)
			if rec, ok := callBinding(p, d, l, c.src); ok {
				d.call = rec
			}
		}
		return d
	}
}

// modelRankingWrite is a config write that changes what orders the call's
// lookup and no credential: config use-server or config set-token-reuse.
// It moves the binding stamp, as every write of a value a record is
// judged by does, unless the protocol's weakened rule says not.
func modelRankingWrite(p modelProtocol, name string, mutate func(d *modelDir)) modelProgram {
	o := modelOpts{p: p, x: -1}
	m := func(d *modelDir, _ *modelLocals) { mutate(d) }
	actions := []modelAction{plainWrite(o, name, nil, m)}
	if p.stampsRanking() {
		actions = stampedWrite(o, name, m)
	}
	return modelProgram{name: name, changer: true, actions: withRelease(o, actions)}
}

func useServerStaging(p modelProtocol) modelProgram {
	return modelRankingWrite(p, "config use-server staging", func(d *modelDir) { d.current = 1 })
}

func setTokenReuse(p modelProtocol, on bool) modelProgram {
	name := "config set-token-reuse off"
	if on {
		name = "config set-token-reuse on"
	}
	return modelRankingWrite(p, name, func(d *modelDir) { d.reuse = on })
}

// entryLogin is a login of entry x as bob, named for the scenario.
func entryLogin(p modelProtocol, x int, name string) modelProgram {
	login := modelLogin(modelOpts{p: p, x: x, user: modelBob})
	login.name = name
	return login
}

// callScenario is one of the call's scenarios: a change, and the call's
// writer, reader and a replay beside it.
func callScenario(p modelProtocol, name string, c callSetup, change modelProgram) modelScenario {
	return modelScenario{name, c.start(p), []modelProgram{change, modelCallWriter(p), modelCallReader(), modelReplay(p)}}
}

// rankingScenarios is each write of a ranking input against the call
// whose answer it changes:
//
//   - config use-server staging against a call that identifies no entry,
//     answered by prod's credential while prod is the current server;
//   - config set-token-reuse off against a call that identifies prod,
//     answered by staging, the plugin's own login entry;
//   - config set-token-reuse on against a call that identifies prod and
//     signed in interactively while the setting was off.
func rankingScenarios(p modelProtocol) []modelScenario {
	return []modelScenario{
		callScenario(p, "config use-server staging against the unnamed call answered by prod",
			callSetup{kind: callNamesNone, cred: [3]modelUser{modelAlice, modelTom}, src: modelAlice}, useServerStaging(p)),
		callScenario(p, "config set-token-reuse off against the call answered by the plugin's login entry",
			callSetup{reuse: true, cred: [3]modelUser{1: modelTom}, src: modelTom, servedBy: 1}, setTokenReuse(p, false)),
		callScenario(p, "config set-token-reuse on against the call signed in interactively",
			callSetup{cred: [3]modelUser{1: modelTom}, src: modelSignedIn, servedBy: -1}, setTokenReuse(p, true)),
	}
}

// identificationScenarios is each login that creates an entry named
// exactly like the call's --name, which then identifies it or, at another
// URL, refuses the call:
//
//   - against a call that identifies prod, answered by prod's own
//     credential, with the new entry at the call's URL and at another;
//   - against a call that identifies no entry, answered by prod's
//     credential, with the new entry at another URL and at the call's.
func identificationScenarios(p modelProtocol) []modelScenario {
	named := callSetup{cred: [3]modelUser{modelAlice}, src: modelAlice}
	unnamed := callSetup{kind: callNamesNone, cred: [3]modelUser{modelAlice, modelTom}, src: modelAlice}
	create := func(c callSetup, other bool) callSetup {
		c.otherURL = other
		return c
	}
	login := func(other bool) modelProgram {
		name := "login creating the entry named like the call's --name"
		if other {
			name += " at another URL"
		}
		return entryLogin(p, 2, name)
	}
	return []modelScenario{
		callScenario(p, login(false).name+", against the call answered by prod", create(named, false), login(false)),
		callScenario(p, login(true).name+", against the call answered by prod", create(named, true), login(true)),
		callScenario(p, login(true).name+", against the unnamed call answered by prod", create(unnamed, true), login(true)),
		callScenario(p, login(false).name+", against the unnamed call answered by prod", create(unnamed, false), login(false)),
	}
}

// callChanges is every change of prod or staging the call's scenarios run:
// a login, a logout and a delete-server of each.
func callChanges(p modelProtocol, noLock bool) []modelProgram {
	var out []modelProgram
	for x, name := range []string{"prod", "staging"} {
		o := modelOpts{p: p, x: x, user: modelBob, noLock: noLock}
		for _, change := range []func(modelOpts) modelProgram{modelLogin, modelLogout, modelDeleteServer} {
			prog := change(o)
			prog.name = prog.name + " of " + name
			out = append(out, prog)
		}
	}
	return out
}

// callScenarios is every combination the call's record is checked on:
//
//   - each change of prod or staging against a call answered by staging's
//     credential - one that identifies prod, which reaches staging as the
//     plugin's login entry, and one that identifies no entry - with the
//     config lock and without;
//   - a login of prod or staging against a call that identifies no entry
//     and signed in interactively;
//   - a login of prod against the call's plugin-created login, and a login
//     creating prod against a plugin-created login that creates its own
//     entry on a directory with none (the bare install);
//   - config use-server, a login and a logout of staging against a call
//     that identifies prod and signed in interactively with token reuse
//     off, and config use-server against one answered by the plugin's
//     login entry: changes that leave such a call's answer alone;
//   - config set-token-reuse off and a login of staging against a call
//     that identifies staging, the plugin's login entry itself;
//   - identificationScenarios, newEntryScenarios and rankingScenarios.
//
// Each has the call's writer, its reader and a replay beside the change,
// but the plugin-created logins, which are their own writers.
func callScenarios(p modelProtocol) []modelScenario {
	var out []modelScenario
	kinds := []struct {
		kind int8
		name string
	}{{callNamesProd, "the call"}, {callNamesNone, "the unnamed call"}}
	for _, k := range kinds {
		for _, noLock := range []bool{false, true} {
			for _, change := range callChanges(p, noLock) {
				name := change.name + " against " + k.name + " answered by staging"
				if noLock {
					name += " without the lock"
				}
				out = append(out, callScenario(p, name,
					callSetup{kind: k.kind, reuse: k.kind == callNamesProd, cred: [3]modelUser{1: modelTom}, src: modelTom, servedBy: 1}, change))
			}
		}
	}
	signedIn := callSetup{kind: callNamesNone, src: modelSignedIn, servedBy: -1}
	for x, name := range []string{"prod", "staging"} {
		out = append(out, callScenario(p, "login of "+name+" against the unnamed call signed in interactively",
			signedIn, entryLogin(p, x, "login of "+name)))
	}
	plugin := modelCallPluginLogin(modelOpts{p: p, x: 1, user: modelTom})
	out = append(out,
		modelScenario{"login of prod against the call's plugin-created login", callSetup{reuse: true}.start(p),
			[]modelProgram{plugin, entryLogin(p, 0, "login of prod"), modelCallReader(), modelReplay(p)}},
		modelScenario{"login creating prod against the call's plugin-created login creating its entry",
			callSetup{reuse: true, current: -1, absent: [2]bool{true, true}}.start(p),
			[]modelProgram{plugin, entryLogin(p, 0, "login creating prod"), modelCallReader(), modelReplay(p)}})
	namedSignedIn := callSetup{cred: [3]modelUser{1: modelTom}, src: modelSignedIn, servedBy: -1}
	out = append(out,
		callScenario(p, "config use-server staging against the call signed in interactively", namedSignedIn, useServerStaging(p)),
		callScenario(p, "login of staging against the call signed in interactively", namedSignedIn, entryLogin(p, 1, "login of staging")),
		callScenario(p, "logout of staging against the call signed in interactively", namedSignedIn, modelLogout(modelOpts{p: p, x: 1})),
		callScenario(p, "config use-server staging against the call answered by the plugin's login entry",
			callSetup{reuse: true, cred: [3]modelUser{1: modelTom}, src: modelTom, servedBy: 1}, useServerStaging(p)))
	pluginNamed := callSetup{kind: callNamesStaging, reuse: true, cred: [3]modelUser{1: modelTom}, src: modelTom, servedBy: 1}
	out = append(out,
		callScenario(p, "config set-token-reuse off against the call that identifies the plugin's login entry", pluginNamed, setTokenReuse(p, false)),
		callScenario(p, "login of staging against the call that identifies the plugin's login entry", pluginNamed, entryLogin(p, 1, "login of staging")))
	out = append(out, identificationScenarios(p)...)
	out = append(out, newEntryScenarios(p)...)
	return append(out, rankingScenarios(p)...)
}

// newEntryScenarios is a login creating prod, the entry the lookup would
// read first, against a call that identifies no entry, made while prod did
// not exist and no entry was the current server: once with the call's
// record answered by staging's credential, once with an interactive
// sign-in's. Nothing the call identifies changes; only the exact list of
// the URL's entries tells the record it no longer answers.
func newEntryScenarios(p modelProtocol) []modelScenario {
	base := callSetup{kind: callNamesNone, current: -1, absent: [2]bool{true, false}}
	staged, signedIn := base, base
	staged.cred, staged.src, staged.servedBy = [3]modelUser{1: modelTom}, modelTom, 1
	signedIn.src, signedIn.servedBy = modelSignedIn, -1
	return []modelScenario{
		callScenario(p, "login creating prod against the unnamed call answered by staging", staged, entryLogin(p, 0, "login creating prod")),
		callScenario(p, "login creating prod against the unnamed call signed in interactively", signedIn, entryLogin(p, 0, "login creating prod")),
	}
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
//   - the call's scenarios (callScenarios);
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
	return append(out, callScenarios(p)...)
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
	for p := modelWriterIgnoresPause; p <= modelUnnamedRecordNotExact; p++ {
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

// TestCacheModelCallRecordBoundToOneEntryBreaksTheInvariant asserts that
// the call's record bound only to the entry whose credential made it - or,
// for an interactive sign-in, to the entry the lookup prefers - is served
// after another entry for the URL gets a login: a record staging's
// credential made for a call that identifies prod, once prod logs in; an
// unnamed call's interactive record, once staging logs in; and the record
// of the call's plugin-created login, once prod logs in.
func TestCacheModelCallRecordBoundToOneEntryBreaksTheInvariant(t *testing.T) {
	t.Parallel()
	windows := []string{
		"login of prod against the call answered by staging",
		"login of prod against the unnamed call answered by staging",
		"login of staging against the unnamed call signed in interactively",
		"login of prod against the call's plugin-created login",
	}
	for _, name := range windows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			check, states := callScenarioNamed(t, modelCallBoundToOneEntry, name).run(false)
			require.NotEmpty(t, check.traces, "the model found no window in a call's record bound to one entry")
			t.Logf("%d states, %d violations; the first:\n  %s", states, check.count, check.traces[0])
			fixed, _ := callScenarioNamed(t, modelPaused, name).run(false)
			require.Zero(t, fixed.count, "\n  %s", strings.Join(fixed.traces, "\n  "))
		})
	}
}

// callScenarioNamed is the call's scenario of that name under p.
func callScenarioNamed(t *testing.T, p modelProtocol, name string) modelScenario {
	t.Helper()
	for _, s := range callScenarios(p) {
		if s.name == name {
			return s
		}
	}
	require.FailNow(t, "no scenario", name)
	return modelScenario{}
}

// TestCacheModelCallRecordWithoutRankingInputsBreaksTheInvariant asserts
// that a call's record bound to its URL's entries but not to the current
// server and the token-reuse setting, with those two written without a
// stamp, is served after a write that changes the call's answer: config
// use-server moving an unnamed call to another entry's login, and config
// set-token-reuse turning the plugin's login entry off or on as the
// fallback of a call that identifies prod. With the ranking inputs bound
// and stamped, no interleaving of the same scenarios serves or leaves
// such a record.
func TestCacheModelCallRecordWithoutRankingInputsBreaksTheInvariant(t *testing.T) {
	t.Parallel()
	for i, s := range rankingScenarios(modelCallUnboundRanking) {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			check, states := s.run(false)
			require.NotEmpty(t, check.traces, "the model found no window in a record bound to no ranking input")
			t.Logf("%d states, %d violations; the first:\n  %s", states, check.count, check.traces[0])
			fixed, fixedStates := rankingScenarios(modelPaused)[i].run(false)
			t.Logf("with the ranking inputs bound: %d states, %d violations", fixedStates, fixed.count)
			require.Zero(t, fixed.count, "\n  %s", strings.Join(fixed.traces, "\n  "))
		})
	}
}

// TestCacheModelCallRecordWithoutIdentificationBreaksTheInvariant asserts
// that a call's record not bound to the entry the call identified is
// served after a login creates an entry named exactly like the call's
// --name: a call that identified prod then identifies the new entry, or,
// with the entry at another URL, is refused; a call that identified no
// entry is refused. Bound to the identification, no interleaving of the
// same scenarios serves or leaves such a record. The fourth scenario, the
// new entry at the URL of a call that identified none, is caught by the
// exact list of the URL's entries either way.
func TestCacheModelCallRecordWithoutIdentificationBreaksTheInvariant(t *testing.T) {
	t.Parallel()
	weakened := identificationScenarios(modelCallIgnoresIdentification)
	fixed := identificationScenarios(modelPaused)
	for i, s := range weakened {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			check, states := s.run(false)
			if i < 3 {
				require.NotEmpty(t, check.traces, "the model found no window in a record not bound to the identification")
				t.Logf("%d states, %d violations; the first:\n  %s", states, check.count, check.traces[0])
			}
			got, fixedStates := fixed[i].run(false)
			t.Logf("with the identification bound: %d states, %d violations", fixedStates, got.count)
			require.Zero(t, got.count, "\n  %s", strings.Join(got.traces, "\n  "))
		})
	}
}

// TestCacheModelIdentifiedCallRecordOutlivesOtherChanges asserts that the
// record of a call that identifies an entry stays servable across changes
// that leave the call's answer alone - config use-server, and a login or
// logout of an entry the call does not read - so a sign-in made once in a
// terminal lasts until its token expires, as the exec plugin's
// no-terminal error says. With token reuse off the call reads only prod;
// with it on, staging's changes do matter and end the record.
func TestCacheModelIdentifiedCallRecordOutlivesOtherChanges(t *testing.T) {
	t.Parallel()
	start := callSetup{cred: [3]modelUser{1: modelTom}, src: modelSignedIn, servedBy: -1}.start(modelPaused)(true)
	require.True(t, start.call.present, "no record was seeded")
	for _, change := range []struct {
		name   string
		mutate func(d *modelDir)
	}{
		{"config use-server staging", func(d *modelDir) { d.current = 1 }},
		{"a login of staging", func(d *modelDir) { d.e[1].id = d.fresh() }},
		{"a logout of staging", func(d *modelDir) { d.e[1].id, d.e[1].cred = d.fresh(), [2]modelUser{} }},
		{"a delete-server of staging", func(d *modelDir) { d.e[1] = modelEntry{} }},
	} {
		d := start
		change.mutate(&d)
		assert.True(t, d.callMatches(d.call, d.callView()), "%s ended the record of a call it does not change", change.name)
		assert.True(t, servable(d.call.src, d.resolvedCall()), "%s: the record is not the call's answer", change.name)
	}
	withReuse := callSetup{reuse: true, cred: [3]modelUser{1: modelTom}, src: modelTom, servedBy: 1}.start(modelPaused)(true)
	d := withReuse
	d.current = 1
	assert.True(t, d.callMatches(d.call, d.callView()), "config use-server ended the record of a call that identifies prod")
	d = withReuse
	d.e[1].id = d.fresh()
	assert.False(t, d.callMatches(d.call, d.callView()), "a login of the plugin's login entry left its fallback record valid")
}

// TestCacheModelUnnamedRecordWithoutExactListBreaksTheInvariant asserts
// that the record of a call that identifies no entry, bound to the URL's
// entries but not to that list being exact, is served after a login
// creates the entry the lookup would read first; with the exact list no
// interleaving serves or leaves it.
func TestCacheModelUnnamedRecordWithoutExactListBreaksTheInvariant(t *testing.T) {
	t.Parallel()
	fixed := newEntryScenarios(modelPaused)
	for i, s := range newEntryScenarios(modelUnnamedRecordNotExact) {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			check, states := s.run(false)
			require.NotEmpty(t, check.traces, "the model found no window in a record whose entry list is not exact")
			t.Logf("%d states, %d violations; the first:\n  %s", states, check.count, check.traces[0])
			got, fixedStates := fixed[i].run(false)
			t.Logf("with the exact list: %d states, %d violations", fixedStates, got.count)
			require.Zero(t, got.count, "\n  %s", strings.Join(got.traces, "\n  "))
		})
	}
}
