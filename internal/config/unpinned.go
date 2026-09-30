package config

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// An `auto` directory with no pin has not written down where it keeps its
// credentials, and only a login writes the pin. Every reader of such a
// directory - a command opening the store, a login working out what it
// changes, a logout, completion, the status commands - asks the one
// question here: where are its credentials now, from what the entries in
// config.yaml have stored. Nothing is probed and nothing is written. An
// orphan token or identity record, whose entry is gone from config.yaml,
// is never served to anyone, so it does not count.

// UnpinnedRow is the answer.
type UnpinnedRow int

const (
	// UnpinnedNothing: no entry has anything stored. A read finds nothing
	// to serve; a login probes and its own pin step writes the pin.
	UnpinnedNothing UnpinnedRow = iota
	// UnpinnedFile: tokens in credentials.yaml and no identity record.
	UnpinnedFile
	// UnpinnedKeychain: identity records and no file token. A record
	// exists only for a keychain credential, so this is the keychain,
	// without a probe: a keychain the process cannot use fails loudly.
	UnpinnedKeychain
	// UnpinnedBoth: both. Nothing on disk says which copy is current, and
	// picking either can serve the wrong user.
	UnpinnedBoth
)

// Unpinned is where an `auto` directory with no pin keeps its
// credentials, and the entries behind the answer.
type Unpinned struct {
	Row UnpinnedRow
	// KeychainEntries have an identity record, pending ones included;
	// FileEntries have a token in credentials.yaml. Both are sorted.
	KeychainEntries, FileEntries []string
	dir                          string
	// cfg is the config the answer was read from, for the note Err adds
	// while a store change is unfinished.
	cfg *Config
}

// UnpinnedStoreOf answers for dir, counting the entries of cfg only. It
// costs one look at the names in credstore-id/ - no record is opened -
// and one read of credentials.yaml. A directory it cannot trust or a
// credentials file it cannot read is an error, never "nothing".
func UnpinnedStoreOf(dir string, cfg *Config) (Unpinned, error) {
	u := Unpinned{dir: dir, cfg: cfg}
	if cfg == nil || len(cfg.Servers) == 0 {
		return u, nil
	}
	records, err := identityRecordNames(dir)
	if err != nil {
		return u, err
	}
	tokens, err := newFileStore(dir, &noticeBuffer{}).read()
	if err != nil {
		return u, unreadableCredentialsFile(dir, err)
	}
	for name := range cfg.Servers {
		if records[identityFileName(name)] {
			u.KeychainEntries = append(u.KeychainEntries, name)
		}
		if _, ok := tokens[name]; ok {
			u.FileEntries = append(u.FileEntries, name)
		}
	}
	slices.Sort(u.KeychainEntries)
	slices.Sort(u.FileEntries)
	switch {
	case len(u.KeychainEntries) > 0 && len(u.FileEntries) > 0:
		u.Row = UnpinnedBoth
	case len(u.KeychainEntries) > 0:
		u.Row = UnpinnedKeychain
	case len(u.FileEntries) > 0:
		u.Row = UnpinnedFile
	}
	return u, nil
}

// Backend is the physical store the answer names: file, keychain, or
// empty when nothing is stored or the answer is both.
func (u Unpinned) Backend() string {
	switch u.Row {
	case UnpinnedFile:
		return backendFile
	case UnpinnedKeychain:
		return backendKeychain
	}
	return ""
}

// UsesKeychain says the keychain holds this directory's credentials: an
// identity record exists, with or without file tokens beside it. A delete
// path treats a keychain it cannot reach as a failure then, rather than a
// warning, so a logout that is the remedy for the both-stores error can
// always run and never exits 0 with the credential still there.
func (u Unpinned) UsesKeychain() bool {
	return u.Row == UnpinnedKeychain || u.Row == UnpinnedBoth
}

// errCredentialsInBothStores marks the both-stores error for a caller
// that has to tell it from every other VALIDATION error.
var errCredentialsInBothStores = errors.New("credentials in both the keychain and the file store")

// IsCredentialsInBothStores reports whether err is the both-stores error.
func IsCredentialsInBothStores(err error) bool { return errors.Is(err, errCredentialsInBothStores) }

// Err is the error every credential command gets for the both-stores
// answer, and nil for every other. It lists the entries in each store and
// the logouts that keep the keychain, which holds no plaintext. A logout
// stays the hint while a store change is unfinished - that change may be
// what left the second copy - and the message names the change; while the
// change record cannot be read the status report is the hint instead.
func (u Unpinned) Err() error {
	if u.Row != UnpinnedBoth {
		return nil
	}
	logouts := make([]string, 0, len(u.FileEntries))
	for _, e := range u.FileEntries {
		logouts = append(logouts, LogoutHintFor(e))
	}
	msg := fmt.Sprintf(
		"this config directory has credentials in the keychain (%s) and in the plaintext file %s (%s), and nothing records which of the two is current, so neither is read; log out the entries in the store you do not want to keep, then run this command again - to keep the keychain, which holds no plaintext, run %s; a keychain entry's logout needs the keychain reachable; or use a different --config",
		strings.Join(u.KeychainEntries, ", "), CredentialsFilePath(u.dir), strings.Join(u.FileEntries, ", "),
		strings.Join(logouts, ", then "))
	err := clierr.New(clierr.CodeValidation, msg).WithCause(errCredentialsInBothStores)
	if len(logouts) == 1 {
		err = err.WithHint(logouts[0])
	}
	return UnfinishedChangeOf(u.dir, u.cfg).Apply(err)
}

// identityRecordNames is the set of names in credstore-id/ spelled like a
// record. Only the names are read: whether a record exists is the
// question, and a record that cannot be read is its own reader's error.
func identityRecordNames(dir string) (map[string]bool, error) {
	root, err := openIdentityRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	observeIdentity("list")
	d, err := root.Open(".")
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, identityDirError(dir, "unreadable: "+err.Error())
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		if isLowerHex(e.Name(), identityNameLen) {
			names[e.Name()] = true
		}
	}
	return names, nil
}

// readerKeychainHint is appended to every keychain error of a directory
// whose keychain was chosen under `auto` - by the pin, or by the identity
// records of an unpinned directory - rather than declared: a keychain the
// process cannot use has to fail with a way out. Deleting the pin would
// change nothing, and the variable conflicts with every declared
// directory, so neither is offered.
func readerKeychainHint() string {
	return " (this directory keeps its credentials in the keychain; make the keychain reachable, or use a different --config)"
}
