package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The binding stamp anchors an exec-credential cache hit in time. A reader
// loads config.yaml to learn which cache binding id each entry carries,
// and that load can be overtaken: a login that completes in between makes
// the loaded ids stale, and the reader would validate a record against
// them and serve the previous user's credential. The stamp is read once
// before that load and once immediately before the credential is
// returned, and a hit needs both reads and the value the loaded config
// carries to agree.
//
// It is a fresh random value, never a counter. The check needs equality
// only, so there is nothing to read before writing and therefore nothing
// to race, and two writers that escaped the flock under
// RANCHER_CLI_CONFIG_NO_LOCK still write values that cannot be mistaken
// for each other.
const bindingStampFileName = "cachebinding-stamp"

// randomIDLen is the length in hex characters of the two random values
// this file generates: the directory's binding stamp and a server entry's
// cache binding id. 128 bits each, so neither is guessable and two draws
// never collide in practice.
const randomIDLen = 32

// BindingStampPath is the directory's binding stamp file.
func BindingStampPath(dir string) string { return filepath.Join(dir, bindingStampFileName) }

// newBindingStamp generates a binding stamp: 128 random bits in
// lower-case hex.
func newBindingStamp() (string, error) { return randomHex(randomIDLen / 2) }

// newCacheBindingID generates a server entry's cache binding id: 128
// random bits in lower-case hex. A random value rather than a per-entry
// counter, which would restart when `config delete-server` removes the
// entry and a later login recreates it.
func newCacheBindingID() (string, error) { return randomHex(randomIDLen / 2) }

// AdvanceCacheBinding gives an entry a fresh cache binding id, so every
// exec-credential cache record made under the previous one becomes a
// miss. The caller writes the config; this only changes the value.
func AdvanceCacheBinding(e *Server) error {
	id, err := newCacheBindingID()
	if err != nil {
		return err
	}
	e.CacheBindingID = id
	return nil
}

// readBindingStamp returns the directory's binding stamp and whether the
// file could be used at all. It never fails.
//
// Not an error, because the alternative is worse than a cold cache: the
// stamp is read at the start of every command, so an error would fail the
// command - including the login and logout that would rewrite the file,
// and including the exec plugin, which must keep working from a bare
// install where there is nothing to read. A damaged stamp file costs the
// directory its warm cache and nothing else.
//
// The damage is reported beside the value rather than encoded in it. A
// sentinel string would be a value a hand-edited config.yaml could carry
// in its bindingStamp field, and the two would then compare equal and
// records would be served: the one thing this must not do.
//
// An absent file is ok with an empty stamp, not damage: a directory that
// has never written one - the bare-install state - has nothing a reader
// could be out of date with, and it matches a config.yaml that carries no
// stamp either. Readers never create it.
func readBindingStamp(dir string) (stamp string, ok bool) {
	observeIdentity("stampread")
	b, err := os.ReadFile(BindingStampPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", true
		}
		return "", false
	}
	stamp = strings.TrimSpace(string(b))
	if !isLowerHex(stamp, randomIDLen) {
		return "", false
	}
	return stamp, true
}

// ReadBindingStamp returns the directory's binding stamp, and false when
// the file cannot be used. Exported for the command packages' tests,
// which assert what a write left behind; every reader in production goes
// through NewCacheAnchor.
func ReadBindingStamp(dir string) (string, bool) { return readBindingStamp(dir) }

// writeBindingStamp writes the stamp file by atomic rename, so a reader
// never sees a partial value.
//
// A path that is not a regular file - a directory, a symlink, whatever a
// restore or a dotfile manager left there - is removed first. An atomic
// rename cannot replace a directory, and without this every config write
// in the directory would fail: the login and the logout that are the way
// out of a damaged stamp would fail with it, which is the state the read
// side is careful not to create.
func writeBindingStamp(dir, stamp string) error {
	path := BindingStampPath(dir)
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		if rerr := os.RemoveAll(path); rerr != nil {
			return fmt.Errorf("replacing the cache binding stamp %s: %w", path, rerr)
		}
	}
	return atomicWriteFile(path, []byte(stamp+"\n"), 0o600)
}

// cacheRecordVersion is the current exec-credential cache record shape. A
// record written by an older binary carries none of the binding fields
// below, so it cannot be judged and is a miss.
const cacheRecordVersion = 1

// CanonicalDir is the directory value a cache record is written with and
// judged by: absolute and cleaned, which is C0's form.
//
// The spelling the caller typed will not do. Two working directories with
// the same relative `--config .rancher` name two different directories,
// and plain string equality would call them one: a `token-cache.yaml`
// copied from the first to the second would be served there, which is the
// one thing the directory in a record exists to prevent. Conversely `./x`,
// `x/../x` and `x/` are one directory and have to compare as one.
//
// A path that cannot be made absolute yields the empty value, which no
// record and no anchor can be judged by: valid refuses an empty directory
// on either side. Returning the caller's spelling instead would be the
// bug this function exists to close - two processes whose working
// directory has gone, both given the same relative --config, would record
// and compare that spelling and match.
func CanonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return abs
}

// CacheBinding is what a cache record is written with and judged by: the
// directory it was created in, the config entry it was created from, that
// entry's cache binding id at the time, and whether the entry's cache
// writes were paused then.
//
// All of it comes from the same config load as the credential it belongs
// to, and is carried to the write rather than re-read there: a re-read
// could pair a credential this process acquired for one login with the id
// of the login that replaced it in the meantime.
//
// A binding is built only by its constructors, from an entry of a loaded
// config or, for a server that config has no entry for, by
// CacheBindingWithoutEntry. The zero value names no load and is refused
// by every write, so a producer that does not build one writes nothing,
// rather than a record the pause would have stopped.
//
// No entry means the credential was acquired for a server the directory
// has no entry for. Such a record is a hit only while that stays true.
//
// load and the store are what CheckStore compares: the config the
// binding was built from, and the store the credential was read from -
// from, asked at the check, or source, named where no store is at hand.
type CacheBinding struct {
	dir, entry, bindingID string
	paused                bool
	built                 bool
	load                  *Config
	from                  CredStore
	source                string
}

// CacheBindingFor is the binding of a record made for entry, from cfg,
// the config load the credential was read after. An entry cfg does not
// have gives the zero binding.
func CacheBindingFor(dir string, cfg *Config, entry string) CacheBinding {
	if cfg == nil {
		return CacheBinding{}
	}
	s, ok := cfg.Servers[entry]
	if !ok {
		return CacheBinding{}
	}
	return CacheBinding{dir: dir, entry: entry, bindingID: s.CacheBindingID, paused: s.Paused(), built: true, load: cfg}
}

// CacheBindingWithoutEntry is the binding of a record made for a server
// cfg has no entry for.
func CacheBindingWithoutEntry(dir string, cfg *Config) CacheBinding {
	if cfg == nil {
		return CacheBinding{}
	}
	return CacheBinding{dir: dir, built: true, load: cfg}
}

// CacheBindingForTest is a binding for a record a test seeds directly:
// dir, entry and id as given, not paused. Production code builds its
// bindings from a config load.
func CacheBindingForTest(dir, entry, bindingID string) CacheBinding {
	return CacheBinding{dir: dir, entry: entry, bindingID: bindingID, built: true}
}

// ReadFrom records the store the binding's credential is read from, for
// CheckStore. The store is asked which backend it reads at the check,
// not here: a lazy store fixes its backend at its first credential call,
// which can come after the load a binding is built from.
func (b CacheBinding) ReadFrom(store CredStore) CacheBinding {
	b.from = store
	return b
}

// ErrCacheStoreChanged is what CheckStore's refusal is: the store a reader
// of the binding's load resolves now is not the store the credential was
// read from, or cannot be resolved at all.
var ErrCacheStoreChanged = errors.New("the credential store changed while the credential was read")

// CacheStoreChangedError is CheckStore's refusal with its cause - the two
// stores, or why a reader's store could not be resolved - so the notice a
// skipped record gets can say what happened.
type CacheStoreChangedError struct{ Cause string }

func (e *CacheStoreChangedError) Error() string { return ErrCacheStoreChanged.Error() + ": " + e.Cause }

// Is makes a CacheStoreChangedError match ErrCacheStoreChanged.
func (e *CacheStoreChangedError) Is(target error) bool { return target == ErrCacheStoreChanged }

// CheckStore reports whether a record may be written for a credential
// read after the binding's load: only when the store a reader resolves
// from that load - its declaration, then the pin or the unpinned decision
// read now - is the store the credential came from. A process fixes its
// store at its first credential call, which can precede a change the
// load already shows, and whatever copy the change left in the store
// readers left is still there to be read; a record made from it would
// carry the entry's current id. A resolution that fails refuses too. A
// binding no constructor built has nothing to compare, and the write
// itself refuses it. A binding to no entry has nothing to compare
// either: its record is served only while the directory has no entry for
// the server, so no credential of an entry can be stale in it. Nor does
// one whose store was never read - a lazy store no credential call has
// resolved - since no copy was read to be stale. A store whose
// resolution failed, and a named source that is empty, refuse.
func (b CacheBinding) CheckStore() error {
	if !b.built || b.entry == "" {
		return nil
	}
	source := b.source
	if b.from != nil {
		backend, fixed := b.from.ResolvedBackend()
		if !fixed {
			return nil
		}
		source = backend
	}
	if source == "" {
		return &CacheStoreChangedError{Cause: "the store it was read from is not known"}
	}
	now, err := readerBackend(b.dir, b.load)
	switch {
	case err != nil:
		return &CacheStoreChangedError{Cause: "no store could be resolved: " + err.Error()}
	case now != source:
		return &CacheStoreChangedError{Cause: fmt.Sprintf("it was read from %s, and the directory now reads %s", source, now)}
	}
	return nil
}

// Dir is the directory the record is bound to.
func (b CacheBinding) Dir() string { return b.dir }

// Entry is the config entry the record is bound to, or empty for none.
func (b CacheBinding) Entry() string { return b.entry }

// BindingID is the entry's cache binding id in the load.
func (b CacheBinding) BindingID() string { return b.bindingID }

// Paused reports whether the entry's cache writes were paused in the load.
func (b CacheBinding) Paused() bool { return b.paused }

// ErrCacheWritesPaused is what a cache write returns for a binding whose
// load showed its entry's cache writes paused: a change of the entry's
// credential has not finished, and nothing is written for it.
var ErrCacheWritesPaused = errors.New("exec-credential cache writes for this entry are paused while a change of its credential finishes")

// errCacheBindingUnset is what a cache write returns for a binding no
// constructor built, or one with no directory: there is nothing a record
// could later be judged by.
var errCacheBindingUnset = errors.New("the exec-credential cache record has no binding to a config load")

// writable reports whether a record may be written under b.
func (b CacheBinding) writable() error {
	switch {
	case !b.built || b.dir == "":
		return errCacheBindingUnset
	case b.paused:
		return ErrCacheWritesPaused
	}
	return nil
}

// CacheAnchor is what an exec-credential cache hit is judged against: the
// directory the reader is running in, the binding stamp read BEFORE Cfg
// was loaded, and Cfg itself.
//
// The stamp must precede the load. A reader whose load returned entries
// that a login has since moved would otherwise validate a record against
// them; because the stamp it read is the pre-login one and the config it
// loaded is the pre-login one too, only a second read of the file - taken
// when the credential is about to be returned - can tell it so. Hence
// Confirm.
//
// A nil anchor, or one with no Cfg, is a miss. That is the answer for an
// unreadable or unparseable config.yaml: a record whose binding cannot be
// checked must not be served, because after a re-login it is the previous
// user's credential. An ABSENT config.yaml is not that case - it loads as
// an empty config, which is the bare-install state the exec plugin has to
// serve.
type CacheAnchor struct {
	Dir   string
	Stamp string
	Cfg   *Config
	// damaged records that the stamp file could not be used at all. It is
	// kept apart from Stamp so no config.yaml content can reproduce it:
	// a reader whose directory has a damaged stamp file judges nothing,
	// whatever the config happens to say.
	damaged bool
}

// NewCacheAnchor reads the directory's binding stamp and pairs it with a
// config loaded after it. The caller must load Cfg after this returns.
// The anchor is the only thing that stores the directory, and it resolves
// it once: every reader and writer downstream takes the value from here
// rather than resolving again. SameDirectory resolves a caller's own
// spelling to compare against it, which is what lets a caller holding
// what the user typed match this; that costs a Clean for an absolute path
// and one os.Getwd for a relative one.
func NewCacheAnchor(dir string) *CacheAnchor {
	stamp, ok := readBindingStamp(dir)
	return &CacheAnchor{Dir: CanonicalDir(dir), Stamp: stamp, damaged: !ok}
}

// SameDirectory reports whether this anchor was built for dir. The
// comparison is between canonical forms, so a caller holding the spelling
// the user typed still matches an anchor holding the resolved one. A
// directory that cannot be resolved matches nothing, including another
// unresolvable one.
func (a *CacheAnchor) SameDirectory(dir string) bool {
	canon := CanonicalDir(dir)
	return a != nil && canon != "" && a.Dir == canon
}

// WithConfig returns the anchor with its config attached, so the stamp
// read and the load stay paired in one value.
func (a *CacheAnchor) WithConfig(cfg *Config) *CacheAnchor {
	if a == nil {
		return nil
	}
	out := *a
	out.Cfg = cfg
	return &out
}

// Damaged reports whether the directory's stamp file could not be used,
// so a caller that wants to say so - `config status` - can.
func (a *CacheAnchor) Damaged() bool { return a != nil && a.damaged }

// staleView reports that this reader's two halves are from different
// moments: the stamp it read before its config load and the stamp that
// load returned disagree, so something wrote a binding in between.
//
// This is not a verdict on any record. The records may be perfectly good
// and the entry they name untouched; what is out of date is the reader's
// own view of the directory. The answer is to look again - once - rather
// than to miss, which would send the exec plugin off to derive a fresh
// credential every time an unrelated entry changed.
func (a *CacheAnchor) staleView() bool {
	return a != nil && !a.damaged && a.Cfg != nil && a.Stamp != a.Cfg.BindingStamp
}

// judgeable reports whether this anchor can decide a record's fate at
// all. A reader with no config cannot - that is the broken-config case -
// and neither can one whose stamp disagrees with the config it loaded,
// because the two halves of its own view are from different moments.
//
// It says nothing about what the reader should do next. Either way no
// record is served and none is deleted on this anchor's word; whether
// looking again could help is staleView's question, and CachedHit's to
// act on.
func (a *CacheAnchor) judgeable() bool {
	return a != nil && !a.damaged && a.Cfg != nil && a.Stamp == a.Cfg.BindingStamp
}

// valid reports whether a cache record may be served. It performs no I/O:
// every value it compares was read before the credential was looked up.
func (a *CacheAnchor) valid(rec cachedCred) bool {
	if !a.judgeable() {
		return false
	}
	if rec.Version != cacheRecordVersion {
		return false
	}
	if rec.Dir == "" || a.Dir == "" || rec.Dir != a.Dir {
		return false
	}
	if rec.Entry == "" {
		// Written for a server this directory had no entry for: valid
		// while that holds, a miss the moment one exists, because the
		// entry is then what decides who the credential belongs to.
		return !a.Cfg.HasEntryForServer(rec.Server)
	}
	entry, ok := a.Cfg.Servers[rec.Entry]
	if !ok {
		return false
	}
	// Plain equality, the two empties included. An entry whose login
	// predates the binding id carries none, and a record made from it
	// carries none either; refusing that pair would make the cache
	// useless for such an entry until its next login, and it is not
	// needed: every path that removes or replaces the entry's credential,
	// config delete-server included, writes a fresh id first, and every
	// entry a command creates starts with one.
	return entry.CacheBindingID == rec.BindingID
}

// staleRecord reports whether this record is one the reader can prove is
// not its own to serve, which is the only case a reader deletes as
// cleanup.
func (a *CacheAnchor) staleRecord(rec cachedCred) bool {
	return a.judgeable() && !a.valid(rec)
}

// Confirm re-reads the binding stamp and reports whether it is still the
// value valid judged against. It is the last operation before a cached
// credential is returned: a binding change that completed between the
// load and here is exactly what it catches.
func (a *CacheAnchor) Confirm() bool {
	_, confirmed := a.confirm()
	return confirmed
}

// confirm is Confirm, also returning what it read. A reader that has to
// look again re-anchors on that value rather than reading the file a
// fourth time: the protocol is three reads - the invocation's, the
// confirmation's, and the one that confirms the second judgement.
//
// An empty stamp is reported for a read that failed and for a directory
// that legitimately has none, and the caller treats both as nothing in
// hand and reads again. That costs one read in the second case, which is
// the bare-install state where there is no warm cache to lose anyway.
func (a *CacheAnchor) confirm() (stamp string, confirmed bool) {
	if a == nil || a.damaged {
		return "", false
	}
	stamp, ok := readBindingStamp(a.Dir)
	if !ok {
		return "", false
	}
	return stamp, stamp == a.Stamp
}

// processCacheAnchor is the anchor the root hook built for this
// invocation, held here rather than in the CLI layer because the two
// readers that cannot be handed one through a constructor need it: the
// legacy `token` shim, whose entry receives no directory and no store,
// and live completion, which runs outside any command. Both live in
// packages that cannot import the CLI layer.
//
// One process, one invocation, one anchor: the stamp is read once, before
// the root hook's config load, and every reader that has no anchor of its
// own validates against that pair.
var (
	processAnchorMu sync.RWMutex
	processAnchor   *CacheAnchor
)

// SetProcessCacheAnchor records the invocation's anchor. The root hook
// calls it once, with the stamp it read before its own config load.
func SetProcessCacheAnchor(a *CacheAnchor) {
	processAnchorMu.Lock()
	defer processAnchorMu.Unlock()
	processAnchor = a
}

// ProcessCacheAnchor returns the invocation's anchor, or nil when the root
// hook could not build one - an unresolvable directory or a config.yaml it
// could not read. A nil anchor is a miss, which is the answer a reader
// that cannot check a record's binding must give.
func ProcessCacheAnchor() *CacheAnchor {
	processAnchorMu.RLock()
	defer processAnchorMu.RUnlock()
	return processAnchor
}

// reload re-reads the binding stamp and loads the config after it,
// returning an anchor whose two halves are from the same moment again.
// retry is what a first attempt learned: whether a second look could
// help, and the stamp it already read, so the second look does not pay
// for a read it has in hand. A stale view reads nothing, so it carries
// no stamp and the reload takes one.
type retry struct {
	again bool
	stamp string
	seen  bool
}

func (a *CacheAnchor) reload(r retry) (*CacheAnchor, error) {
	if a == nil || a.Dir == "" {
		// Two readers hand CachedHit a nil anchor deliberately, for a
		// directory they could not resolve or a config they could not
		// read, and an unresolvable directory is the empty value. There
		// is nothing to re-anchor on - and re-resolving the empty value
		// would silently produce the working directory, which answers
		// for nobody.
		return nil, errNoAnchorToReload
	}
	// The confirmation's read succeeded, so the value is current and the
	// file is readable: re-anchor on it rather than reading it again.
	next := &CacheAnchor{Dir: a.Dir, Stamp: r.stamp}
	if !r.seen {
		next = NewCacheAnchor(a.Dir)
	}
	cfg, err := Load(FilePath(a.Dir))
	if err != nil {
		return nil, err
	}
	return next.WithConfig(cfg), nil
}

// errNoAnchorToReload is reload's answer for an anchor that does not
// exist: a miss, never a panic.
var errNoAnchorToReload = errors.New("no cache anchor to reload")

// CachedHit runs the cache-hit protocol for one reader. read looks the
// record up and judges it against the anchor it is given, returning
// whether it may be served.
//
// A hit needs the binding stamp to be unchanged from before the config
// load to after the answer is known, so a login, logout or credential
// clearing that completed in between turns it into a miss. That is the
// whole point: a reader whose config load preceded a completed identity
// change must not serve what it read.
//
// A stamp that does not line up is not by itself a miss, at either end.
// Before the read it means the load was overtaken - the reader's own two
// halves are from different moments - and after it means something landed
// while the reader worked. Both times the record itself may be perfectly
// good, so the reader re-anchors on the newer stamp, loads once more and
// asks again. Exactly one reload: a directory something is actively
// rewriting has no stable answer to give, and kubectl is better served a
// fresh credential than a reader spinning against a moving target.
func CachedHit[T any](anchor *CacheAnchor, read func(*CacheAnchor) (T, bool, error)) (T, bool, error) {
	var zero T
	value, hit, again, err := cachedAttempt(anchor, read)
	switch {
	case err != nil:
		return zero, false, err
	case hit:
		return value, true, nil
	case !again.again:
		return zero, false, nil
	}

	next, rerr := anchor.reload(again)
	if rerr != nil {
		return zero, false, nil
	}
	value, hit, _, err = cachedAttempt(next, read)
	if err != nil || !hit {
		return zero, false, err
	}
	return value, true, nil
}

// cachedAttempt is one pass of the protocol: check that the reader's view
// holds together, read, and confirm the stamp has not moved since. again
// reports that looking once more could still produce a hit, which is the
// one case CachedHit reloads for.
func cachedAttempt[T any](a *CacheAnchor, read func(*CacheAnchor) (T, bool, error)) (value T, hit bool, again retry, err error) {
	var zero T
	if a.staleView() {
		// Nothing is read at all: judging a record against entries this
		// reader already knows are out of date would be guessing.
		return zero, false, retry{again: true}, nil
	}
	value, hit, err = read(a)
	switch {
	case err != nil:
		return zero, false, retry{}, err
	case !hit:
		// Judged, and not this reader's to serve. Looking again changes
		// nothing.
		return zero, false, retry{}, nil
	}
	stamp, confirmed := a.confirm()
	if confirmed {
		return value, true, retry{}, nil
	}
	return zero, false, retry{again: true, stamp: stamp, seen: stamp != ""}, nil
}

// HasEntryForServer reports whether any entry answers for this server.
// The comparison is the one the token package's candidate set uses, so a
// record's "the directory had no entry for this server" can never mean
// something different from the credential lookup's.
func (c *Config) HasEntryForServer(server string) bool {
	return len(c.MatchingServerNames(server)) > 0
}

// MatchingServerNames lists the entries answering for a server, compared
// by scheme and host, sorted for determinism. One matching policy for
// every caller: the credential lookup, the CA lookup and the cache
// record's entry rule.
func (c *Config) MatchingServerNames(server string) []string {
	var names []string
	for n, s := range c.Servers {
		if SameServerURL(s.URL, server) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// entryLookupRow is the part of one server entry a cache record's validity
// depends on.
type entryLookupRow struct {
	url       string
	bindingID string
}

// entryLookup is the part of config.yaml an exec-credential cache record
// is judged against: which entries exist, which URL each one answers for,
// and the cache binding id each one carries. A config write that leaves
// all three unchanged cannot make any record invalid, so it needs no
// fresh stamp; one that adds, removes or re-addresses an entry, or
// advances an entry's binding id, can, so it writes one.
//
// The predicate is evaluated rather than listed, so a config writer added
// later cannot forget to stamp.
func (c *Config) entryLookup() map[string]entryLookupRow {
	out := make(map[string]entryLookupRow, len(c.Servers))
	for name, s := range c.Servers {
		out[name] = entryLookupRow{url: s.URL, bindingID: s.CacheBindingID}
	}
	return out
}
