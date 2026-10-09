package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// sweepRow is one generated login and the directory it runs on.
type sweepRow struct {
	name string
	// declared is config.yaml's credentialStore, pin is credstore-pin and
	// loginVar is RANCHER_CLI_CREDENTIAL_STORE while the login runs; empty
	// for none.
	declared, pin, loginVar string
	// readerVars are the values further readers of every entry run with,
	// beside the one with the variable unset.
	readerVars []string
	// login is empty for auth login with no --credential-store, a store
	// for auth login with that flag, or loginPlugin.
	login string
	// probe is the keychain probe's answer; reachable is false for a
	// keychain no call can reach.
	probe, reachable bool
	// prodNew leaves prod out of config.yaml: the plugin's login creates
	// it, and prod's copies are credentials stored under a name no entry
	// has.
	prodNew bool
	// prod and staging are the stores holding each entry's copy; staging
	// is in config.yaml only with hasStaging.
	prod, staging []string
	hasStaging    bool
	// start is what an earlier store change of prod left.
	start string
}

var sweepURLs = map[string]string{"prod": "https://r.ex", "staging": "https://staging.ex"}

// state is the row's directory in the rules' model.
func (r sweepRow) state() sweepState {
	s := sweepState{declared: r.declared, pin: r.pin, probe: r.probe, reachable: r.reachable,
		copies: map[string][]string{"prod": slices.Clone(r.prod), "staging": slices.Clone(r.staging)}}
	if !r.prodNew {
		s.entries = append(s.entries, "prod")
	}
	if r.hasStaging {
		s.entries = append(s.entries, "staging")
	}
	if r.start == startDeleteBlocked {
		s.blocked = "prod"
	}
	return s
}

func (r sweepRow) usesHelper() bool {
	return r.declared == sweepHelper || r.loginVar == sweepHelper || slices.Contains(r.readerVars, sweepHelper) ||
		r.login == sweepHelper || slices.Contains(r.prod, sweepHelper) || slices.Contains(r.staging, sweepHelper)
}

// sweepValue turns an axis value into the row's spelling of it: "none" is
// empty and "helper" is the helper's store.
func sweepValue(v string) string {
	switch v {
	case "none":
		return ""
	case "helper":
		return sweepHelper
	}
	return v
}

func sweepStores(v string) []string {
	if v == "none" {
		return nil
	}
	var out []string
	for s := range strings.SplitSeq(v, "+") {
		out = append(out, sweepValue(s))
	}
	return out
}

// sweepRowOf applies the chosen value of every axis the row has chosen to
// base. An axis left at -1 keeps base's value.
func sweepRowOf(axes []pairwiseAxis, row []int, base sweepRow) sweepRow {
	r := base
	for i, ax := range axes {
		if row[i] < 0 {
			continue
		}
		v := ax.values[row[i]]
		switch ax.name {
		case "declared":
			r.declared = sweepValue(v)
		case "pin":
			r.pin = sweepValue(v)
		case "loginVar":
			r.loginVar = sweepValue(v)
		case "readerVar":
			r.readerVars = nil
			if v != "none" {
				r.readerVars = []string{sweepValue(v)}
			}
		case "login":
			r.login = sweepValue(v)
		case "probe":
			r.probe = v == "usable"
		case "keychain":
			r.reachable = v == "reachable"
		case "prod", "orphan":
			r.prod = sweepStores(v)
		case "staging":
			r.hasStaging = v != "absent"
			if r.hasStaging {
				r.staging = sweepStores(v)
			}
		case "start":
			r.start = v
		}
	}
	return r
}

var (
	sweepStoreValues   = []string{"none", "auto", "file", "keychain"}
	sweepHelperValues  = []string{"none", "auto", "file", "keychain", "helper"}
	sweepStagingValues = []string{"absent", "none", "file", "keychain", "file+keychain"}
	sweepStartValues   = []string{startNone, startDeleteLeft, startDeleteBlocked, startPinLeft, startNeverStarted, startUnreadable}
)

// sweepFamily is one family of generated rows.
type sweepFamily struct {
	name string
	rows []sweepRow
}

// generatedSweep builds the rows of one family: every pair of values of
// two axes in some row, among the rows valid allows.
func generatedSweep(name string, axes []pairwiseAxis, base sweepRow, constrained []string, valid func(r sweepRow) bool) sweepFamily {
	idx := make([]int, len(constrained))
	for n, c := range constrained {
		idx[n] = slices.IndexFunc(axes, func(ax pairwiseAxis) bool { return ax.name == c })
	}
	f := sweepFamily{name: name}
	for _, row := range pairwiseRows(axes, idx, func(row []int) bool { return valid(sweepRowOf(axes, row, base)) }) {
		r := sweepRowOf(axes, row, base)
		r.name = pairwiseRowName(axes, row)
		f.rows = append(f.rows, r)
	}
	return f
}

// policyResolves reports whether the login's policy resolves. The rows
// it refuses are pruned from the generated families and asserted by
// the conflicts family, which holds every one of them.
func policyResolves(r sweepRow) bool {
	_, _, refusal := sweepLoginPolicy(r)
	return refusal == ""
}

// sweepFamilies are the generated families and the targeted products.
//
// Two entries: prod logs in beside staging, over the file store and the
// keychain, with an earlier change of prod left in each state it can be.
// Helper: prod alone, with the helper added to every store axis. Orphans:
// the plugin's login creates prod while credentials stored under that name
// are left in any of the three stores. Conflicts: every combination the
// generated families prune because the login's policy refuses. Both
// stores: declared and pin in full, each read with every value of the
// reader's variable, over a directory whose entries keep credentials in
// both stores. Store changes: where readers are, the store the login asks
// for and where staging has copies, in full - the three that decide
// whether a change is refused.
// Earlier changes: each state an earlier change can leave against each
// kind of login, on an auto directory whose pin the prelude may move.
// Helper orphans: the plugin's login creating prod on a directory that
// declares nothing, over a credential left under prod in the helper, read
// through the helper, with a commit that lands and one that fails.
func sweepFamilies() []sweepFamily {
	twoEntry := generatedSweep("two entries", []pairwiseAxis{
		{"start", sweepStartValues},
		{"staging", sweepStagingValues},
		{"login", []string{"none", "auto", "file", "keychain", loginPlugin}},
		{"readerVar", sweepHelperValues},
		{"declared", sweepStoreValues},
		{"loginVar", sweepStoreValues},
		{"prod", []string{"none", "file", "keychain", "file+keychain"}},
		{"pin", []string{"none", "file", "keychain"}},
		{"probe", []string{"usable", "unusable"}},
	}, sweepRow{reachable: true},
		[]string{"start", "staging", "login", "declared", "loginVar", "prod", "pin"},
		func(r sweepRow) bool { return policyResolves(r) && startPossible(r.start, r.state()) })

	helper := generatedSweep("helper", []pairwiseAxis{
		{"prod", []string{"none", "file", "keychain", "helper", "file+keychain", "file+helper", "keychain+helper", "file+keychain+helper"}},
		{"login", []string{"none", "auto", "file", "keychain", "helper", loginPlugin}},
		{"declared", sweepHelperValues},
		{"loginVar", sweepHelperValues},
		{"readerVar", sweepHelperValues},
		{"pin", []string{"none", "file", "keychain"}},
		{"probe", []string{"usable", "unusable"}},
	}, sweepRow{reachable: true, start: startNone},
		[]string{"login", "declared", "loginVar"}, policyResolves)

	orphans := generatedSweep("orphans", []pairwiseAxis{
		{"orphan", []string{"file", "keychain", "helper", "file+keychain", "file+helper", "keychain+helper", "file+keychain+helper"}},
		{"staging", sweepStagingValues},
		{"declared", sweepHelperValues},
		{"loginVar", sweepHelperValues},
		{"readerVar", sweepHelperValues},
		{"pin", []string{"none", "file", "keychain"}},
		{"probe", []string{"usable", "unusable"}},
		{"keychain", []string{"reachable", "unreachable"}},
	}, sweepRow{login: loginPlugin, prodNew: true, start: startNone},
		[]string{"declared", "loginVar"}, policyResolves)

	// A flag is weighed against the variable alone, never the
	// declaration, so its rows take two declarations rather than all.
	conflicts := sweepFamily{name: "conflicts"}
	for _, declared := range sweepHelperValues {
		for _, loginVar := range sweepHelperValues {
			for _, login := range []string{"none", "auto", "file", "keychain", "helper", loginPlugin} {
				r := sweepRow{declared: sweepValue(declared), loginVar: sweepValue(loginVar), login: sweepValue(login),
					probe: true, reachable: true, start: startNone,
					name: fmt.Sprintf("declared=%s,loginVar=%s,login=%s", declared, loginVar, login)}
				flag := r.login != "" && r.login != loginPlugin
				if !policyResolves(r) && (!flag || declared == "none" || declared == "file") {
					conflicts.rows = append(conflicts.rows, r)
				}
			}
		}
	}

	both := sweepFamily{name: "both stores"}
	for _, declared := range sweepStoreValues {
		for _, pin := range []string{"none", "file", "keychain"} {
			both.rows = append(both.rows, sweepRow{declared: sweepValue(declared), pin: sweepValue(pin),
				readerVars: []string{"auto", backendFile, backendKeychain, sweepHelper},
				probe:      true, reachable: true, prod: []string{backendKeychain}, staging: []string{backendFile}, hasStaging: true,
				start: startNone, name: fmt.Sprintf("declared=%s,pin=%s", declared, pin)})
		}
	}
	changes := sweepFamily{name: "store changes"}
	for _, dir := range []struct{ name, declared, pin string }{
		{"declared file", backendFile, ""}, {"declared keychain", backendKeychain, ""},
		{"auto pinned to file", "auto", backendFile}, {"auto pinned to keychain", "auto", backendKeychain},
	} {
		readers := dir.declared
		if dir.pin != "" {
			readers = dir.pin
		}
		for _, login := range []string{"auto", "file", "keychain"} {
			for _, staging := range []string{"none", "file", "keychain", "file+keychain"} {
				changes.rows = append(changes.rows, sweepRow{declared: dir.declared, pin: dir.pin, login: login,
					probe: true, reachable: true, prod: []string{readers}, staging: sweepStores(staging), hasStaging: true,
					start: startNone, name: fmt.Sprintf("%s,login=%s,staging=%s", dir.name, login, staging)})
			}
		}
	}
	earlier := sweepFamily{name: "earlier changes"}
	for _, dir := range []struct {
		pin   string
		probe bool
	}{{backendKeychain, false}, {backendFile, true}} {
		for _, start := range sweepStartValues[1:] {
			for _, login := range []string{"none", "auto", "file", "keychain", loginPlugin} {
				r := sweepRow{declared: "auto", pin: dir.pin, login: sweepValue(login), probe: dir.probe, reachable: true,
					prod: []string{backendFile, backendKeychain}, hasStaging: true, start: start,
					name: fmt.Sprintf("pin=%s,probe=%t,start=%s,login=%s", dir.pin, dir.probe, start, login)}
				if startPossible(start, r.state()) {
					earlier.rows = append(earlier.rows, r)
				}
			}
		}
	}
	helperOrphans := sweepFamily{name: "helper orphans"}
	for _, keychain := range []string{"reachable", "unreachable"} {
		for _, loginVar := range []string{"none", "file"} {
			for _, orphan := range []string{"helper", "file+helper"} {
				helperOrphans.rows = append(helperOrphans.rows, sweepRow{login: loginPlugin, prodNew: true, start: startNone,
					loginVar: sweepValue(loginVar), readerVars: []string{sweepHelper}, probe: true, reachable: keychain == "reachable",
					prod: sweepStores(orphan), name: fmt.Sprintf("keychain=%s,loginVar=%s,orphan=%s", keychain, loginVar, orphan)})
			}
		}
	}
	return []sweepFamily{twoEntry, helper, orphans, conflicts, both, changes, earlier, helperOrphans}
}

// TestAGeneratedLoginKeepsEveryReaderOnTheOldOrTheNewCredential asserts,
// for every row of the families sweepFamilies generates, what a login does
// and what a process that takes no lock and runs no probe reads while it
// runs.
//
// The check before the sign-in and the login under the lock, with that
// check skipped, run on twin directories: each answers as the rules say,
// and the two give the same answer - allowed, or refused the same way -
// except for the plugin's abort on a plaintext file it did not disclose,
// which only the lock can meet, and the known violations.
//
// Before the login every reader gets what the rules say. After every write
// the login makes that such a process can observe - config.yaml, the pin,
// the store change record, an identity record, a keychain set or delete,
// credentials.yaml, a helper store or erase - readers of each entry, with
// the variable unset, with the login's own value and with each of the
// row's reader values, never probe or
// write, never get the both-stores error unless it was their answer before
// the login, and get the conflict error exactly when their variable
// disagrees with the declaration config.yaml has at that moment. Otherwise
// a reader of staging gets its answer from before the login. A reader of
// prod gets that answer, the new credential, or an error a rule allows:
// "not logged in" once its old copy is gone from the store it was read
// from, LOGIN_INCOMPLETE between the pending identity record and its
// settling, CREDENTIAL_STORE_UNAVAILABLE when the row's keychain cannot be
// reached. It gets its previous credential only while the binding id has
// not advanced or its cache writes are paused, and until the binding id
// advances a reader of prod with the variable unset gets exactly its
// answer from before, unless the prelude is finishing an earlier change
// that left its pin step.
//
// Afterwards the new credential is where the rules say, prod's copy in the
// store it left is gone, every other copy is where it was, no unpinned
// directory keeps credentials in both stores, and a credential left under
// a name the login creates is gone from every store, each removal with a
// notice. A refused login leaves the
// declaration, the entry and the pin as the prelude left them.
//
// Readers whose config load and credential read fall on two sides of one
// write are out of scope here; the cache binding tests own them.
func TestAGeneratedLoginKeepsEveryReaderOnTheOldOrTheNewCredential(t *testing.T) {
	for _, f := range sweepFamilies() {
		t.Run(f.name, func(t *testing.T) {
			for _, r := range f.rows {
				t.Run(r.name, func(t *testing.T) { runSweepRow(t, r) })
			}
		})
	}
}

// sweepKnownViolations are rule breaks the sweep finds at this commit and
// does not accept. A row the rules say shows one logs it rather than
// failing, and fails if it no longer shows it, so a fix removes the entry
// here; TestEveryKnownSweepViolationHasARow fails for an entry no row
// shows. matches picks out the entry's violations among a row's reader
// and after-login ones; an entry without it is a disagreement of the
// check before the sign-in and the login under the lock.
var sweepKnownViolations = []struct {
	rule    string
	applies func(r sweepRow, want sweepOutcome) bool
	matches func(violation string) bool
}{
	{
		rule: "the check before the sign-in refuses a store change while an earlier change has work left that the login's own prelude finishes, " +
			"with the retryable CREDENTIAL_TRANSITION_INCOMPLETE and \"run this command again\"; the same command is refused again on every run, " +
			"while REFERENCE.md says only a second store change is refused while some of that work remains, and the login under the lock goes on",
		applies: func(_ sweepRow, want sweepOutcome) bool { return want.knownPrecheck != "" },
	},
}

// TestEveryKnownSweepViolationHasARow asserts each known violation
// applies to at least one generated row: a change to the axes or the
// generator that drops every row showing one would otherwise leave its
// entry checked by nothing.
func TestEveryKnownSweepViolationHasARow(t *testing.T) {
	families := sweepFamilies()
	for _, k := range sweepKnownViolations {
		rows := 0
		for _, f := range families {
			for _, r := range f.rows {
				s := r.state()
				leftFor, _ := s.readerStore("")
				if k.applies(r, expectLogin(r, s, leftFor)) {
					rows++
				}
			}
		}
		assert.Positive(t, rows, "no row shows the known violation: %s", k.rule)
	}
}

// sweepMode is one reader: the entry it reads and its variable.
type sweepMode struct{ entry, v string }

func (m sweepMode) String() string {
	if m.v == "" {
		return "a reader of " + m.entry
	}
	return fmt.Sprintf("a reader of %s with %s=%s", m.entry, EnvCredentialStore, m.v)
}

// sweepDir is one of a row's twin directories.
type sweepDir struct {
	path, helperState string
	// blocked is the keychain account whose slot the row makes unreachable.
	blocked string
}

func runSweepRow(t *testing.T, r sweepRow) {
	t.Helper()
	if r.usesHelper() {
		useFakeHelper(t, "ok")
	}
	t.Setenv(EnvCredentialStore, r.loginVar)
	keyring.MockInit()
	reading := false
	var violations []string
	stubMatrixProbe(t, r.probe, &reading, &violations)

	s := r.state()
	leftFor, _ := s.readerStore("")
	precheckDir := seedSweepDir(t, r, s, leftFor)
	lockDir := seedSweepDir(t, r, s, leftFor)
	if !r.reachable || r.start == startDeleteBlocked {
		t.Cleanup(SetKeyringBackendForTest(sweepKeyring{
			unreachable: !r.reachable, blocked: []string{precheckDir.blocked, lockDir.blocked}}))
	}
	want := expectLogin(r, s, leftFor)

	var modes []sweepMode
	entries := s.entries
	if r.prodNew {
		entries = append(slices.Clone(entries), "prod")
	}
	// Every row also reads with the login's own variable: the process
	// beside a login most often runs in the same environment.
	vars := []string{""}
	for _, v := range append([]string{r.loginVar}, r.readerVars...) {
		if !slices.Contains(vars, v) {
			vars = append(vars, v)
		}
	}
	for _, e := range entries {
		for _, v := range vars {
			modes = append(modes, sweepMode{e, v})
		}
	}
	read := func(m sweepMode) sweepAnswer {
		reading = true
		t.Setenv(EnvCredentialStore, m.v)
		tok, err := readMatrixEntry(lockDir.path, m.entry)
		t.Setenv(EnvCredentialStore, r.loginVar)
		reading = false
		return sweepAnswerOf(tok, err)
	}

	// stored is what a store holds, looked at directly through a keychain
	// every call reaches: the row's unreachable keychain is the login's
	// and the readers' condition, not the test's.
	stored := func(entry, store string) string {
		defer SetKeyringBackendForTest(goKeyring{})()
		return matrixStored(t, lockDir.path, entry, sweepURLs[entry], store)
	}

	setHelperState(t, r, lockDir)
	baseline := map[sweepMode]sweepAnswer{}
	baseStore := map[sweepMode]string{}
	for _, m := range modes {
		got := read(m)
		exp, store := s.read(m.entry, m.v)
		assert.Equal(t, exp, got, "before the login %s gets what the rules say", m)
		baseline[m], baseStore[m] = got, store
	}

	setHelperState(t, r, precheckDir)
	pre, policy, disclosed := sweepPreCheck(t, r, precheckDir.path)
	wantPre := want.precheck
	if want.knownPrecheck != "" {
		wantPre = want.knownPrecheck
	}
	assert.Equal(t, wantPre, pre, "the check before the sign-in")
	if pre == "" {
		assert.Equal(t, want.disclosed, disclosed, "the check before the sign-in discloses the plaintext file when the login goes there")
	}

	setHelperState(t, r, lockDir)
	writes, pending := 0, false
	judge := func(label string) {
		cur, lerr := Load(FilePath(lockDir.path))
		if lerr != nil {
			violations = append(violations, fmt.Sprintf("%s: config.yaml cannot be read: %v", label, lerr))
			return
		}
		moment := sweepMoment{r: r, cur: cur, pending: pending, baseline: baseline, baseStore: baseStore,
			stored: func(store string) string { return stored("prod", store) }}
		for _, m := range modes {
			if v := moment.judge(m, read(m)); v != "" {
				violations = append(violations, fmt.Sprintf("%s: %s %s", label, m, v))
			}
		}
	}
	entryWritten := false
	t.Cleanup(SetPersistWriteHookForTest(func(step string) error {
		if step == "entry" {
			entryWritten = true
		}
		return nil
	}))
	restoreHook := SetAfterWriteHookForTest(func(what string) {
		if reading {
			violations = append(violations, "a reader wrote "+what)
			return
		}
		writes++
		switch what {
		case "credstore-id prod " + string(identityPending):
			pending = true
		case "credstore-id prod " + string(identityActive), "credstore-id prod removed":
			pending = false
		}
		judge(fmt.Sprintf("after write %d (%s)", writes, what))
	})
	out, lerr := sweepPersist(t, r, lockDir.path, pre, policy, disclosed)
	restoreHook()

	lock := ""
	if lerr != nil && !entryWritten {
		lock = classifySweepError(lerr)
	}
	assert.Equal(t, want.lock, lock, "the login under the lock: %v", lerr)

	cur, err := Load(FilePath(lockDir.path))
	require.NoError(t, err)
	switch {
	case lock != "":
		assert.Equal(t, r.declared, cur.CredentialStore, "a refused login declares nothing")
		if r.prodNew {
			assert.NotContains(t, cur.Servers, "prod", "a refused login creates no entry")
		} else {
			assert.Equal(t, "b0", cur.Servers["prod"].CacheBindingID, "a refused login leaves prod's entry as it was")
		}
		pin := r.pin
		if want.preludeRan {
			pin = want.settled.pin
		}
		assert.Equal(t, pin, ReadPin(lockDir.path), "a refused login leaves the pin as the prelude left it")
	case want.commitFails != "":
		require.Error(t, lerr, "a commit to a store that cannot be reached fails")
		assert.Equal(t, want.commitFails, classifySweepError(lerr))
		assert.Contains(t, cur.Servers, "prod", "the entry stays as the handle for the credential that may have landed")
	default:
		require.NoError(t, lerr)
		assert.Positive(t, writes, "the login wrote something a reader could observe")
		assert.Equal(t, sweepAnswer{token: matrixNewToken}, read(sweepMode{"prod", ""}), "after the login a reader gets the new credential")
		assert.Equal(t, matrixNewToken, stored("prod", want.b.commitsTo), "in the store the rules name")
		if want.b.changesBackend() {
			assert.NotEqual(t, sweepToken("prod", want.b.readsNow), stored("prod", want.b.readsNow),
				"prod's copy in the store it left is gone")
		}
		if cur.CredentialStore == "" || cur.CredentialStore == "auto" {
			if ReadPin(lockDir.path) == "" {
				u, uerr := UnpinnedStoreOf(lockDir.path, cur)
				require.NoError(t, uerr)
				assert.NotEqual(t, UnpinnedBoth, u.Row, "no login leaves credentials in both stores with no pin")
			}
		}
	}
	if r.prodNew && lock == "" {
		// The removal runs before the entry write, so it has run whether
		// the commit then lands or fails.
		removed := 0
		for _, store := range r.prod {
			if stored("prod", store) == sweepToken("prod", store) {
				violations = append(violations, fmt.Sprintf("after the login: the credential left under prod in %s is still there", store))
				continue
			}
			removed++
		}
		notices := slices.DeleteFunc(slices.Clone(out.Warnings), func(w string) bool { return !strings.Contains(w, "that no entry named") })
		assert.Len(t, notices, removed, "one notice for each credential left under prod that was removed: %v", out.Warnings)
	}
	for _, store := range r.staging {
		assert.Equal(t, sweepToken("staging", store), stored("staging", store), "staging's copy in %s is where it was", store)
	}
	_, statErr := os.Lstat(TransitionPath(lockDir.path))
	manifest := statErr == nil
	switch {
	case r.start == startUnreadable:
		assert.True(t, manifest, "an unreadable record is left for the user")
	case want.preludeRan:
		assert.Equal(t, want.stepsLeft, manifest, "a record stays only while it has work left")
	}
	judge("after the login")
	for _, k := range sweepKnownViolations {
		if k.matches == nil || !k.applies(r, want) {
			continue
		}
		n := len(violations)
		if violations = slices.DeleteFunc(violations, k.matches); len(violations) < n {
			t.Logf("known violation, %d checks: %s", n-len(violations), k.rule)
		} else {
			assert.Fail(t, "known violation no longer occurs; remove it from sweepKnownViolations", k.rule)
		}
	}
	assert.Empty(t, violations)

	compareSweepPaths(t, r, want, pre, lock)
}

// compareSweepPaths asserts the check before the sign-in and the login
// under the lock agree. Allowed: the plugin's login aborts under the lock
// on the plaintext file the check did not disclose, which only the lock
// can see once its prelude has finished an earlier change.
func compareSweepPaths(t *testing.T, r sweepRow, want sweepOutcome, pre, lock string) {
	t.Helper()
	known := ""
	for _, k := range sweepKnownViolations {
		if k.matches == nil && k.applies(r, want) {
			known = k.rule
		}
	}
	switch {
	case known != "" && pre != lock:
		t.Logf("known violation: %s (check before the sign-in: %q, login under the lock: %q)", known, pre, orAllowed(lock))
	case known != "":
		assert.Fail(t, "known violation no longer occurs; remove it from sweepKnownViolations", known)
	case pre == lock:
	case pre == "" && lock == answerUndisclosed:
	default:
		assert.Fail(t, "the check before the sign-in and the login under the lock disagree",
			"check before the sign-in: %s; login under the lock: %s", orAllowed(pre), orAllowed(lock))
	}
}

func orAllowed(kind string) string {
	if kind == "" {
		return "allowed"
	}
	return kind
}

// sweepMoment is the state a reader's answer is judged against.
type sweepMoment struct {
	r         sweepRow
	cur       *Config
	pending   bool
	baseline  map[sweepMode]sweepAnswer
	baseStore map[sweepMode]string
	stored    func(store string) string
}

// judge returns the rule a reader's answer breaks, or "".
func (c sweepMoment) judge(m sweepMode, got sweepAnswer) string {
	if got.kind == answerBothStores && c.baseline[m].kind != answerBothStores {
		return "gets the both-stores error in the middle of the login"
	}
	_, configured := c.cur.Servers[m.entry]
	conflict := configured && m.v != "" && c.cur.CredentialStore != "" && m.v != c.cur.CredentialStore
	switch {
	case conflict && got.kind != answerConflict:
		return fmt.Sprintf("gets %s, where its variable disagrees with the declaration %q", got, c.cur.CredentialStore)
	case conflict:
		return ""
	case got.kind == answerConflict:
		return fmt.Sprintf("gets the conflict error, though the declaration is %q", c.cur.CredentialStore)
	}
	ref, refStore := c.baseline[m], c.baseStore[m]
	if ref.kind == answerConflict {
		// The variable now names the store the directory declares, so
		// this reader reads what one with no variable does.
		ref, refStore = c.baseline[sweepMode{m.entry, ""}], c.baseStore[sweepMode{m.entry, ""}]
	}
	if m.entry != "prod" {
		if got != ref {
			return fmt.Sprintf("gets %s, not its answer from before the login, %s", got, ref)
		}
		return ""
	}
	prod, configured := c.cur.Servers["prod"]
	advanced := configured && prod.CacheBindingID != "b0"
	if m.v == "" && !advanced && c.r.start != startPinLeft && got != ref {
		return fmt.Sprintf("gets %s before the login wrote its entry, not its answer from before, %s", got, ref)
	}
	earlier := ""
	if c.r.start == startPinLeft {
		earlier = sweepToken("prod", otherStore(c.r.pin))
	}
	switch {
	case got.token == matrixNewToken:
	case got.token != "" && (got.token == ref.token || got.token == earlier):
		if advanced && !prod.Paused() {
			return fmt.Sprintf("gets the previous credential %s after the binding id advanced, with cache writes not paused", got.token)
		}
	case got.token != "":
		return fmt.Sprintf("gets %s, which is neither its old nor its new credential", got)
	case got.kind == ref.kind:
	case got.kind == answerNotLoggedIn:
		if ref.token != "" && c.stored(refStore) == ref.token {
			return fmt.Sprintf("gets \"not logged in\" while its old credential is still in %s", refStore)
		}
	case got.kind == answerIncomplete && c.pending:
	case got.kind == answerUnavailable && (!c.r.reachable || c.r.start == startDeleteBlocked):
	default:
		return fmt.Sprintf("gets %s, which no rule allows", got)
	}
	return ""
}

// sweepKeyring is a keychain whose calls fail as unreachable: every call,
// or those on the blocked accounts.
type sweepKeyring struct {
	unreachable bool
	blocked     []string
}

func (k sweepKeyring) refuses(account string) bool {
	return k.unreachable || slices.Contains(k.blocked, account)
}

func (k sweepKeyring) Set(service, account, password string) error {
	if k.refuses(account) {
		return errUnreachableKeyring
	}
	return goKeyring{}.Set(service, account, password)
}

func (k sweepKeyring) Get(service, account string) (string, error) {
	if k.refuses(account) {
		return "", errUnreachableKeyring
	}
	return goKeyring{}.Get(service, account)
}

func (k sweepKeyring) Delete(service, account string) error {
	if k.refuses(account) {
		return errUnreachableKeyring
	}
	return goKeyring{}.Delete(service, account)
}

// setHelperState points the fake helper at d's own state: the twin
// directories never share what a helper holds.
func setHelperState(t *testing.T, r sweepRow, d sweepDir) {
	t.Helper()
	if r.usesHelper() {
		t.Setenv("FAKEHELPER_STATE", d.helperState)
	}
}

// seedSweepDir writes the row's directory: config.yaml, the pin, every
// copy, and the record an earlier change of prod left. leftFor is the
// store readers use, which the earlier change's work is recorded against.
func seedSweepDir(t *testing.T, r sweepRow, s sweepState, leftFor string) sweepDir {
	t.Helper()
	d := sweepDir{path: t.TempDir()}
	if r.usesHelper() {
		d.helperState = filepath.Join(t.TempDir(), "helper.json")
		setHelperState(t, r, d)
	}
	cfg := &Config{CredentialStore: r.declared, TokenReuse: r.login == loginPlugin, Servers: map[string]Server{}}
	if !r.prodNew {
		prod := Server{URL: sweepURLs["prod"], CacheBindingID: "b0"}
		if r.start == startPinLeft {
			// The change that left its pin step keeps prod's cache writes
			// paused until a later login finishes it.
			prod.CacheWritesPaused = PauseLocked
		}
		cfg.Servers["prod"], cfg.CurrentServer = prod, "prod"
	}
	if r.hasStaging {
		cfg.Servers["staging"] = Server{URL: sweepURLs["staging"]}
	}
	cfg.SetPath(FilePath(d.path))
	require.NoError(t, cfg.Write())
	if r.pin != "" {
		require.NoError(t, WritePin(d.path, r.pin))
	}
	accounts := map[string]string{}
	for _, e := range []string{"prod", "staging"} {
		for _, store := range s.copies[e] {
			if id := seedMatrixCopy(t, d.path, e, sweepURLs[e], store, sweepToken(e, store)); id != "" {
				accounts[e] = id
			}
		}
	}

	deleteStep := func(entry, store string) transitionStep {
		vid, err := newTransitionID()
		require.NoError(t, err)
		st := transitionStep{Kind: transitionDelete, Entry: entry, Source: store,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, sweepToken(entry, store))}
		switch {
		case store == backendKeychain && accounts[entry] != "":
			st.SourceAccountID = accounts[entry]
		case store == backendKeychain:
			st.SourceAccountID = mustAccount(t)
		case isExecBackend(store):
			st.CanonicalURL = sweepURLs[entry]
		}
		return st
	}
	record := func(named bool, steps ...transitionStep) {
		id, err := newTransitionID()
		require.NoError(t, err)
		require.NoError(t, writeTransition(d.path, transitionManifest{Version: transitionVersion, ID: id, Steps: steps}))
		if named {
			cfg.CredentialTransitionID = id
			require.NoError(t, cfg.Write())
		}
	}
	switch r.start {
	case startDeleteLeft:
		record(true, deleteStep("prod", otherStore(leftFor)))
	case startDeleteBlocked:
		st := deleteStep("prod", backendKeychain)
		d.blocked = keychainAccount(st.SourceAccountID, "prod")
		record(true, st)
	case startPinLeft:
		var steps []transitionStep
		if s.has("prod", leftFor) {
			steps = append(steps, deleteStep("prod", leftFor))
		}
		record(true, append(steps, transitionStep{Kind: transitionPin, Target: otherStore(leftFor)})...)
	case startNeverStarted:
		// A record nothing names is removed unread. Its delete names a
		// credential readers are served now, so one that ran would show.
		live := leftFor
		if live != backendFile && live != backendKeychain {
			live = backendFile
		}
		entry := "prod"
		if s.has("staging", live) {
			entry = "staging"
		}
		record(false, deleteStep(entry, live))
	case startUnreadable:
		id, err := newTransitionID()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(TransitionPath(d.path), []byte("not a store change record\n"), 0o600))
		cfg.CredentialTransitionID = id
		require.NoError(t, cfg.Write())
	}
	return d
}

// sweepPreCheck runs the check a login makes before its sign-in and
// returns its refusal, the policy it resolved and whether it disclosed the
// plaintext file. It writes nothing, which a write hook asserts.
//
// The plugin's check is its storeDecision, rebuilt here from config's
// exported pieces in the order it runs them, since this package cannot
// import the token package: the policy, the orphan check and the store
// decision for the entry the login will write.
func sweepPreCheck(t *testing.T, r sweepRow, dir string) (refusal, policy string, disclosed bool) {
	t.Helper()
	writes := 0
	defer SetAfterWriteHookForTest(func(string) { writes++ })()
	defer func() { assert.Zero(t, writes, "the check before the sign-in writes nothing") }()

	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	if r.login == loginPlugin {
		policy, err = EffectivePolicy(cfg.CredentialStore)
		if err != nil {
			return classifySweepError(err), "", false
		}
		target := CollisionFreeName(cfg, "prod", sweepURLs["prod"])
		if err := CheckOrphanCredentials(dir, cfg, target); err != nil {
			return classifySweepError(err), policy, false
		}
		backend, err := LoginStoreChange(dir, cfg, target, policy, DeclarationFor(cfg.CredentialStore, "", policy))
		if err != nil {
			return classifySweepError(err), policy, false
		}
		return "", policy, backend == backendFile
	}
	policy, _, err = LoginPolicy(r.login, cfg.CredentialStore)
	if err != nil {
		return classifySweepError(err), "", false
	}
	if _, err := LoginStoreChange(dir, cfg, "prod", policy, DeclarationFor(cfg.CredentialStore, r.login, policy)); err != nil {
		return classifySweepError(err), policy, false
	}
	return "", policy, false
}

// sweepPersist runs the login under the lock on dir, the way auth login
// or the plugin's login calls it. A plugin login gets the decision the
// check before the sign-in made, with the disclosure naming this twin's
// file; when that check refused, the decision discloses the file, so the
// abort for an undisclosed plaintext file cannot stand in for the refusal
// the two paths are compared on. Where the check could not resolve a
// policy, auth login passes its flag and the plugin passes none.
func sweepPersist(t *testing.T, r sweepRow, dir, pre, policy string, disclosed bool) (PersistLoginOutcome, error) {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	if r.login == loginPlugin {
		d := StoreDecision{RequireSetting: true, Policy: policy}
		if disclosed || pre != "" {
			d.DisclosedPlaintextPath = CredentialsFilePath(dir)
		}
		return PersistLoginCredential(t.Context(), dir, PersistLoginInput{
			Name: "prod", CollisionSuffix: true, CanonicalURL: sweepURLs["prod"],
			Provenance: clierr.ProvenanceExecLogin, Bearer: matrixNewToken,
			OwnsEntry: true, Decision: &d, ReadOnlyStoreIsFailure: true,
			Policy: d.Policy, ObservedDeclaration: cfg.CredentialStore,
			Declaration: DeclarationFor(cfg.CredentialStore, "", d.Policy),
			OpenStore:   OpenResolvedStore,
		})
	}
	if policy == "" {
		policy = r.login
	}
	return PersistLoginCredential(t.Context(), dir, PersistLoginInput{
		Name: "prod", CanonicalURL: sweepURLs["prod"], Bearer: matrixNewToken,
		Provenance:          clierr.ProvenanceDerived,
		Policy:              policy,
		ObservedDeclaration: cfg.CredentialStore,
		Declaration:         DeclarationFor(cfg.CredentialStore, r.login, policy),
		OpenStore:           OpenResolvedStore,
	})
}
