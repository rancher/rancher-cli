package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
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
// record written by an older binary does not carry what the record is bound
// to in this shape, so it cannot be judged and is a miss.
const cacheRecordVersion = 2

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
// directory it was created in, the entry the call's --name identified (or
// none), and what the exec plugin's lookup for that call reads - the
// entries the record is bound to, each with its cache binding id, and,
// where the lookup depends on them, the current server and the
// token-reuse setting. A record is served only while the lookup, run again
// over the reader's own config load, would read the same entries.
//
// Which entries a record is bound to follows from how the call's lookup
// reads them:
//
//   - a record made from the identified entry's own credential (or signed
//     in after the server rejected it) is bound to that entry alone, and,
//     for a plugin login entry, to the setting, without which that entry is
//     never read (CacheBindingFor);
//   - any other record of a call that identifies an entry is bound to that
//     entry, to the setting, and, while the setting is on, exactly to the
//     server's plugin login entries, the one entry that lookup may read
//     after it (CacheBindingForIdentified);
//   - the record of a call that identifies none is bound exactly to every
//     entry that answers for the server, to the current server, which the
//     lookup prefers among them, and to the setting
//     (CacheBindingForServer).
//
// "Exactly" means the list may neither gain nor lose an entry.
//
// All of it comes from the same config load as the credential it belongs
// to, and is carried to the write rather than re-read there: a re-read
// could pair a credential this process acquired for one login with the id
// of the login that replaced it in the meantime.
//
// A binding is built only by its constructors. The zero value names no
// load and is refused by every write, so a producer that does not build
// one writes nothing, rather than a record the pause would have stopped.
//
// load and the store are what CheckStore compares: the config the
// binding was built from, and the store the credential was read from -
// from, asked at the check, or source, named where no store is at hand.
type CacheBinding struct {
	dir        string
	kind       string
	identified string
	// entry is the entry the record is bound to by its id alone: the
	// identified entry, empty for a call that identifies none.
	entry boundEntry
	// identifiedIsPlugin reports that the identified entry is a plugin
	// login entry, whose lookup reads no other.
	identifiedIsPlugin bool
	listed             bool
	list               []boundEntry
	currentBound       bool
	current            string
	reuseBound         bool
	reuse              bool
	built              bool
	// refused marks a binding no record is written under on purpose: the
	// call's lookup, over the binding's load, refuses the call.
	refused bool
	load    *Config
	from    CredStore
	source  string
}

// The kinds of cache record, one per rule above, which a record names so
// a hit judges it by that kind's rule and requires everything the kind is
// bound to.
const (
	recordOwnCredential = "ownCredential"
	recordIdentified    = "identifiedEntry"
	recordServer        = "server"
)

// boundEntry is one entry a record is bound to: its name, its cache binding
// id in the load, and whether its cache writes were paused there.
type boundEntry struct {
	name, id string
	paused   bool
}

func boundEntryOf(cfg *Config, name string) boundEntry {
	s := cfg.Servers[name]
	return boundEntry{name: name, id: s.CacheBindingID, paused: s.Paused()}
}

func boundEntriesOf(cfg *Config, names []string) []boundEntry {
	out := make([]boundEntry, len(names))
	for i, n := range names {
		out[i] = boundEntryOf(cfg, n)
	}
	return out
}

// CacheBindingFor is the binding of a record made from entry's own stored
// credential by a call whose --name identified entry, from cfg, the config
// load the credential was read after: bound to entry alone, and, when entry
// is a plugin login entry, to the token-reuse setting. The same binding
// covers the sign-in that follows a rejection of that credential, since
// the lookup read that entry and nothing else. An entry cfg does not have
// gives the zero binding.
func CacheBindingFor(dir string, cfg *Config, entry string) CacheBinding {
	if cfg == nil {
		return CacheBinding{}
	}
	s, ok := cfg.Servers[entry]
	if !ok {
		return CacheBinding{}
	}
	b := CacheBinding{dir: dir, kind: recordOwnCredential, identified: entry, entry: boundEntryOf(cfg, entry), built: true, load: cfg}
	if s.Provenance == clierr.ProvenanceExecLogin {
		b.reuseBound, b.reuse = true, cfg.TokenReuse
	}
	return b
}

// CacheBindingForIdentified is the binding of any other record of a call
// whose --name identified entry, for server: the plugin's own login entry
// answering for it, the plugin-created login, or an interactive sign-in.
// It is bound to entry, to the token-reuse setting, and, while the setting
// is on, exactly to the server's plugin login entries; not to the other
// entries for the server, nor to the current server, which that lookup
// never reads. An entry cfg does not have gives the zero binding, and so
// does a list the lookup refuses (see refusesPluginEntries).
func CacheBindingForIdentified(dir string, cfg *Config, server, entry string) CacheBinding {
	if cfg == nil {
		return CacheBinding{}
	}
	s, ok := cfg.Servers[entry]
	if !ok {
		return CacheBinding{}
	}
	b := CacheBinding{dir: dir, kind: recordIdentified, identified: entry, entry: boundEntryOf(cfg, entry),
		identifiedIsPlugin: s.Provenance == clierr.ProvenanceExecLogin,
		reuseBound:         true, reuse: cfg.TokenReuse, built: true, load: cfg}
	if cfg.TokenReuse {
		b.listed, b.list = true, boundEntriesOf(cfg, cfg.PluginLoginEntries(server))
	}
	if b.refusesPluginEntries() {
		return CacheBinding{refused: true}
	}
	return b
}

// refusesPluginEntries reports that the lookup of the call this binding
// is for refuses it over the binding's list (PluginFallbackRefused).
func (b CacheBinding) refusesPluginEntries() bool {
	return b.kind == recordIdentified && PluginFallbackRefused(b.identifiedIsPlugin, len(b.list))
}

// PluginFallbackRefused reports whether the exec plugin's lookup refuses a
// call whose --name identified an entry with no credential of its own,
// given how many plugin login entries answer for the call's server: a user
// entry falls back to the server's one plugin login entry, and more than
// one is refused, since the fallback is one entry and never a choice among
// several; a plugin login entry reads no other. The lookup, the cache
// writers and a cache hit all ask this.
func PluginFallbackRefused(identifiedIsPlugin bool, pluginLoginEntries int) bool {
	return !identifiedIsPlugin && pluginLoginEntries > 1
}

// CacheBindingForLookup is the binding of the record a call writes,
// picked from what its lookup found in cfg, the config load its
// credentials were read after: identified is the entry the call's --name
// identified, empty for none, and answered the entry whose stored
// credential made the token, empty for a sign-in. The identified entry's
// own credential gives CacheBindingFor, any other answer of a call that
// identifies an entry CacheBindingForIdentified, and a call that
// identifies none CacheBindingForServer (legacyShim for the legacy
// `rancher token` shim's lookup). Every writer picks its binding here.
func CacheBindingForLookup(dir string, cfg *Config, server, identified, answered string, legacyShim bool) CacheBinding {
	switch {
	case identified != "" && answered == identified:
		return CacheBindingFor(dir, cfg, identified)
	case identified != "":
		return CacheBindingForIdentified(dir, cfg, server, identified)
	}
	return CacheBindingForServer(dir, cfg, server, legacyShim)
}

// CacheBindingForServer is the binding of a record of a call whose --name
// identifies no entry, for server: bound exactly to every entry that
// answers for server, to the current server - its name when it is one of
// them, else none - and to the token-reuse setting. The legacy `rancher
// token` shim's lookup ignores the current server, so its records
// (legacyShim) are not bound to it. A server cfg has no entry for gives
// the empty list, the bare-install case.
func CacheBindingForServer(dir string, cfg *Config, server string, legacyShim bool) CacheBinding {
	if cfg == nil {
		return CacheBinding{}
	}
	names := cfg.MatchingServerNames(server)
	b := CacheBinding{dir: dir, kind: recordServer, listed: true, list: boundEntriesOf(cfg, names), reuseBound: true, reuse: cfg.TokenReuse, built: true, load: cfg}
	if !legacyShim {
		b.currentBound, b.current = true, currentAmong(cfg.CurrentServer, names)
	}
	return b
}

// currentAmong is the current server as a record is bound to it: its name
// when it is one of names, else empty. The lookup only asks whether a
// candidate is the current server.
func currentAmong(current string, names []string) string {
	if slices.Contains(names, current) {
		return current
	}
	return ""
}

// CacheBindingForTest is a binding for a record a test seeds directly,
// with no config load: for a non-empty entry, a record made from that
// entry's own credential by a call that identified it, with the given id;
// for an empty one, a record of a call that identifies no entry, made
// while the directory had no entry for the server, no current server and
// the setting off. Production code builds its bindings from a config
// load.
func CacheBindingForTest(dir, entry, bindingID string) CacheBinding {
	if entry == "" {
		return CacheBinding{dir: dir, kind: recordServer, listed: true, currentBound: true, reuseBound: true, built: true}
	}
	return CacheBinding{dir: dir, kind: recordOwnCredential, identified: entry, entry: boundEntry{name: entry, id: bindingID}, built: true}
}

// CacheBindingForSeed is the binding the exec plugin writes for a call
// with --name name, for server and cluster, over cfg: the identified
// entry alone when the name identifies one at server - the record of a
// call its own credential answered - else what the lookup of a call that
// identifies none reads. For a test or a harness that seeds the cache as
// a real call would; the exec plugin itself picks its binding from what
// its lookup read.
func CacheBindingForSeed(dir string, cfg *Config, name, server, cluster string) CacheBinding {
	legacyShim := IsLegacyCacheSlot(name)
	identified := ""
	if id := IdentifyEntry(cfg, server, name, cluster, legacyShim); !id.OtherURL {
		identified = id.Entry
	}
	return CacheBindingForLookup(dir, cfg, server, identified, identified, legacyShim)
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
// itself refuses it. A binding bound to no entry has nothing to compare
// either: its record is served only while the directory has no entry for
// the server, so no credential of an entry can be stale in it. Nor does
// one whose store was never read - a lazy store no credential call has
// resolved - since no copy was read to be stale. A store whose
// resolution failed, and a named source that is empty, refuse.
func (b CacheBinding) CheckStore() error {
	if !b.built || (b.entry.name == "" && len(b.list) == 0) {
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

// Entry is the entry the record is bound to by its id alone - the entry the
// call identified - or empty for a call that identifies none.
func (b CacheBinding) Entry() string { return b.entry.name }

// BindingID is that entry's cache binding id in the load.
func (b CacheBinding) BindingID() string { return b.entry.id }

// PausedEntry names an entry the record is bound to whose cache writes were
// paused in the load, or is empty when none was.
func (b CacheBinding) PausedEntry() string {
	if b.entry.paused {
		return b.entry.name
	}
	for _, e := range b.list {
		if e.paused {
			return e.name
		}
	}
	return ""
}

// Paused reports whether an entry the record is bound to had its cache
// writes paused in the load.
func (b CacheBinding) Paused() bool { return b.PausedEntry() != "" }

// withWritten is b with the entry a login wrote bound to the id and pause
// that write left, in place of what b's load showed for it: added to the
// list when the login created it, and the load CheckStore resolves from
// replaced by the config that write wrote. A binding whose list cannot
// hold the entry - one not bound to a list - gives the zero binding, since
// its record would not be bound to the entry its credential came from, and
// so does a list the call's lookup now refuses: a login that added a
// second plugin login entry for the server of a call that identifies a
// user entry.
func (b CacheBinding) withWritten(written *Config, name string, paused bool) CacheBinding {
	s, ok := written.Servers[name]
	if !b.built || !ok || !b.listed {
		return CacheBinding{}
	}
	wrote := boundEntry{name: name, id: s.CacheBindingID, paused: paused || s.Paused()}
	list := slices.Clone(b.list)
	i, found := slices.BinarySearchFunc(list, name, func(e boundEntry, n string) int { return strings.Compare(e.name, n) })
	if found {
		list[i] = wrote
	} else {
		list = slices.Insert(list, i, wrote)
	}
	b.list, b.load, b.from = list, written, nil
	if b.refusesPluginEntries() {
		return CacheBinding{refused: true}
	}
	return b
}

// ErrCacheWritesPaused matches every CacheWritesPausedError, for a caller
// that asks only whether a write was refused for a pause.
var ErrCacheWritesPaused = errors.New("exec-credential cache writes are paused while a change of a credential finishes")

// CacheWritesPausedError is what a cache write returns for a binding whose
// load showed an entry it is bound to with its cache writes paused: a
// change of that entry's credential has not finished, and nothing is
// written. It names the entry, so the exec plugin can try to resume that
// entry's writes and say why they stay paused.
type CacheWritesPausedError struct{ Entry string }

func (e *CacheWritesPausedError) Error() string {
	return fmt.Sprintf("exec-credential cache writes for %s are paused while a change of its credential finishes", e.Entry)
}

// Is makes a CacheWritesPausedError match ErrCacheWritesPaused.
func (e *CacheWritesPausedError) Is(target error) bool { return target == ErrCacheWritesPaused }

// errCacheBindingUnset is what a cache write returns for a binding no
// constructor built, or one with no directory: there is nothing a record
// could later be judged by. The exec plugin reaches it when the config
// directory could not be resolved or config.yaml could not be read.
var errCacheBindingUnset = errors.New("the config directory could not be resolved or its config.yaml could not be read when the credential was made")

// errCacheRecordRefused is what a cache write returns for a binding whose
// call its lookup refuses (Refused): no record is written for it.
var errCacheRecordRefused = errors.New("no cache record is written for a call its credential lookup refuses")

// Refused reports that no record is written under b on purpose: the call's
// lookup, over the binding's load, refuses the call as the directory now
// stands. The next call's own refusal says why and what to do, so the
// skipped write needs no notice of its own.
func (b CacheBinding) Refused() bool { return b.refused }

// writable reports whether a record may be written under b: never when an
// entry it is bound to was paused in the load.
func (b CacheBinding) writable() error {
	switch {
	case b.refused:
		return errCacheRecordRefused
	case !b.built || b.dir == "":
		return errCacheBindingUnset
	case b.Paused():
		return &CacheWritesPausedError{Entry: b.PausedEntry()}
	}
	return nil
}

// record is the cache record b writes for a credential.
func (b CacheBinding) record(token string, expiry time.Time, name, server, cluster string) cachedCred {
	rec := cachedCred{Token: token, Expiry: expiry, Name: name, Server: server, Cluster: cluster,
		Version: cacheRecordVersion, Dir: b.dir, Kind: b.kind, Identified: b.identified, Entry: b.entry.name, BindingID: b.entry.id}
	if b.listed {
		entries := make([]cachedEntry, len(b.list))
		for i, e := range b.list {
			entries[i] = cachedEntry{Name: e.name, BindingID: e.id}
		}
		rec.Entries = &entries
	}
	if b.currentBound {
		rec.CurrentServer = &b.current
	}
	if b.reuseBound {
		rec.TokenReuse = &b.reuse
	}
	return rec
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
	return a.Cfg.servesRecord(rec)
}

// servesRecord reports whether the exec plugin's lookup for the record's
// call, run over c, would read what the record is bound to: the same
// identification and no refusal, then, by the record's kind, the one
// entry and its id, the exact list with every id, and the current server
// and the token-reuse setting where the kind binds them. A record of no
// known kind, or missing or carrying a binding its kind does not have, is a
// miss, never judged by another kind's rule.
//
// Ids compare by plain equality, the two empties included. An entry whose
// login predates the binding id carries none, and a record made from it
// carries none either; refusing that pair would make the cache useless for
// such an entry until its next login, and it is not needed: every path
// that removes or replaces the entry's credential, config delete-server
// included, writes a fresh id first, and every entry a command creates
// starts with one.
func (c *Config) servesRecord(rec cachedCred) bool {
	legacyShim := IsLegacyCacheSlot(rec.Name)
	id := IdentifyEntry(c, rec.Server, rec.Name, rec.Cluster, legacyShim)
	if id.OtherURL || id.Entry != rec.Identified {
		return false
	}
	if rec.TokenReuse != nil && *rec.TokenReuse != c.TokenReuse {
		return false
	}
	switch rec.Kind {
	case recordServer:
		if rec.Identified != "" || rec.Entry != "" || rec.Entries == nil || rec.TokenReuse == nil || (rec.CurrentServer == nil) != legacyShim {
			return false
		}
		names := c.MatchingServerNames(rec.Server)
		if rec.CurrentServer != nil && currentAmong(c.CurrentServer, names) != *rec.CurrentServer {
			return false
		}
		return c.sameEntries(names, *rec.Entries)
	case recordOwnCredential:
		s, ok := c.identifiedEntryOf(rec)
		if !ok || rec.Entries != nil {
			return false
		}
		// A plugin login entry is read only with the setting on, so a
		// record of one is bound to the setting; a user entry's is not.
		return (rec.TokenReuse != nil) == (s.Provenance == clierr.ProvenanceExecLogin)
	case recordIdentified:
		s, ok := c.identifiedEntryOf(rec)
		if !ok || rec.TokenReuse == nil || (rec.Entries != nil) != *rec.TokenReuse {
			return false
		}
		if rec.Entries == nil {
			return true
		}
		plugins := c.PluginLoginEntries(rec.Server)
		if PluginFallbackRefused(s.Provenance == clierr.ProvenanceExecLogin, len(plugins)) {
			return false
		}
		return c.sameEntries(plugins, *rec.Entries)
	}
	return false
}

// identifiedEntryOf is the entry a record of an identified call is bound to
// by its id: the identified entry, present with the id the record carries.
// Such a record is never bound to the current server.
func (c *Config) identifiedEntryOf(rec cachedCred) (Server, bool) {
	if rec.Identified == "" || rec.Entry != rec.Identified || rec.CurrentServer != nil {
		return Server{}, false
	}
	s, ok := c.Servers[rec.Entry]
	if !ok || s.CacheBindingID != rec.BindingID {
		return Server{}, false
	}
	return s, true
}

// sameEntries reports whether names, sorted, are exactly the entries a
// record lists, each with the cache binding id the record carries for it.
func (c *Config) sameEntries(names []string, listed []cachedEntry) bool {
	if len(names) != len(listed) {
		return false
	}
	for i, n := range names {
		if listed[i].Name != n || c.Servers[n].CacheBindingID != listed[i].BindingID {
			return false
		}
	}
	return true
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

// PluginLoginEntries lists the plugin's own login entries - the execLogin
// provenance - among the entries answering for server, sorted. The exec
// plugin reads one of them after an identified entry with no credential,
// and more than one refuses the call.
func (c *Config) PluginLoginEntries(server string) []string {
	return slices.DeleteFunc(c.MatchingServerNames(server), func(n string) bool {
		return c.Servers[n].Provenance != clierr.ProvenanceExecLogin
	})
}

// MatchingServerNames lists the entries answering for a server, compared
// by scheme and host, sorted for determinism. One matching policy for
// every caller: the credential lookup, the CA lookup and the lists a cache
// record is bound to.
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

// entryInputRow is the fields of one server entry a cache record's validity
// depends on.
type entryInputRow struct {
	url       string
	bindingID string
}

// recordInputs is the config.yaml content an exec-credential cache record
// is judged against: which entries exist, which URL each one answers for,
// the cache binding id each one carries, the current server and the
// token-reuse setting. The last two decide which entry the exec plugin's
// lookup reads for a call that names none, and whether it reads the
// plugin's own login entries at all. A config write that leaves all of it
// unchanged cannot make any record invalid, so it needs no fresh stamp;
// one that changes any of it can, so it writes one.
//
// The predicate is evaluated rather than listed, so a config writer added
// later cannot forget to stamp.
type recordInputs struct {
	entries       map[string]entryInputRow
	currentServer string
	tokenReuse    bool
}

func (c *Config) recordInputs() recordInputs {
	out := recordInputs{entries: make(map[string]entryInputRow, len(c.Servers)), currentServer: c.CurrentServer, tokenReuse: c.TokenReuse}
	for name, s := range c.Servers {
		out.entries[name] = entryInputRow{url: s.URL, bindingID: s.CacheBindingID}
	}
	return out
}

// equal reports whether two loads give a cache record the same inputs.
func (r recordInputs) equal(o recordInputs) bool {
	return r.currentServer == o.currentServer && r.tokenReuse == o.tokenReuse && maps.Equal(r.entries, o.entries)
}
