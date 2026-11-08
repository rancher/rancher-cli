package config

import (
	"errors"
	"io/fs"
	"net/url"
	"regexp"
	"strings"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// unusableURLText is what a display shows for a URL that has no scheme and
// host to show.
const unusableURLText = "<unusable URL>"

// unusableIdentityPrefix opens the identity of a URL that does not
// canonicalize, so it never equals the identity of a usable server.
const unusableIdentityPrefix = "unusable:"

var urlSchemePrefixRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+\-.]*://`)

// CheckServerURL is credhelper.CheckServerURL's verdict alone, for packages
// that have no other use for credhelper: nil for a URL with no userinfo that
// parses into a scheme and a host, else an error whose text holds no part of
// the URL.
func CheckServerURL(raw string) error {
	_, err := credhelper.CheckServerURL(raw)
	return err
}

// ErrServerURLScheme is the reason for a URL that parses into a scheme and a
// host and whose scheme is not http or https.
var ErrServerURLScheme = errors.New("its scheme is not http or https")

// ServerURLOrigin says where a server URL that cannot be used came from, and
// so which repair its refusal names.
type ServerURLOrigin struct {
	// Source is the flag or variable the URL was typed or set in: "--server",
	// "--url" or RANCHER_CLI_URL. It is empty for the current URL of a stored
	// entry.
	Source string
	// Entry is the name of the stored entry whose current URL it is.
	Entry string
	// ExecEntryArgument is set for a typed --server of a command that
	// kubectl runs from a kubeconfig exec block: the refusal then says where
	// that value is kept.
	ExecEntryArgument bool
}

// EntryOrigin is the origin of the current URL of the stored entry name.
func EntryOrigin(name string) ServerURLOrigin { return ServerURLOrigin{Entry: name} }

// source is the place the refusal names.
func (o ServerURLOrigin) source() string {
	if o.Entry != "" {
		return "server entry " + clierr.ShellQuote(o.Entry)
	}
	return o.Source
}

// Text is the refusal of a server URL from this origin for reason, the error
// credhelper.CheckServerURL returned or ErrServerURLScheme. The caller
// already knows the URL cannot be used. The text holds no part of the URL.
func (o ServerURLOrigin) Text(reason error) string {
	head := "the server URL in " + o.source() + " cannot be used: " + reason.Error() + "; "
	switch {
	case o.Entry == "":
		repair := "give a URL like https://host, without a user name or password"
		if errors.Is(reason, ErrServerURLScheme) {
			repair = "use http:// or https://"
		}
		if o.ExecEntryArgument {
			repair += "; when kubectl runs this command, the URL is the --server argument of the kubeconfig's exec block"
		}
		return head + repair
	case IsExecLoginEntryName(o.Entry):
		return head + "if the entry holds a login, run: " + clierr.Rancher + " auth logout" + clierr.ShellQuoteArgs("", o.Entry) +
			"; then remove the entry with: " + clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", o.Entry)
	}
	return head + "change the entry's URL with " + clierr.Rancher + " config update-server" +
		clierr.ShellQuoteArgs(" --url <url>", o.Entry)
}

// RemovedText is the refusal text for the current URL of an entry that the
// running command removes: nothing is left to repair.
func (o ServerURLOrigin) RemovedText(reason error) string {
	return "the server URL in " + o.source() + " cannot be used: " + reason.Error() + "; this command removes the entry"
}

// Refusal is Text as a validation error.
func (o ServerURLOrigin) Refusal(reason error) *clierr.Error {
	return clierr.New(clierr.CodeValidation, o.Text(reason))
}

// recordServerText is the text for the server recorded for the token ref when
// that server cannot be used and is not an entry's current URL: the token
// stays on the server that issued it, and the delete runs there, as the
// token's owner (owner, when known) or an administrator.
func recordServerText(ref clierr.TokenRef, reason error, owner string) string {
	return "the server recorded for token " + clierr.ShellQuote(ref.Name) + " cannot be used: " + reason.Error() +
		"; the token stays on the server that issued it; delete it there as " + recordPerson(owner) + " while signed in to that server: " +
		clierr.Rancher + " delete token" + clierr.ShellQuoteArgs(" --apply --yes", ref.Name)
}

// identityProblem is why the identity of a server cannot go into a command:
// it holds userinfo or does not parse into a scheme and a host. The reason is
// the one of the URL the identity stands for, not of its prefix.
func identityProblem(identity string) error {
	err := CheckServerURL(identity)
	if err == nil {
		return nil
	}
	if derr := CheckServerURL(DisplayServerIdentity(identity)); derr != nil {
		return derr
	}
	return err
}

// RecordOwnerOrAdministrator names who can delete an ext token whose owner is
// not known: the token's owner and an administrator are the only users Rancher
// lets read or delete it. A text about a record with no known owner uses these
// words.
const RecordOwnerOrAdministrator = "its owner or an administrator"

// recordPerson is who runs the delete of a token owned by owner: the user, or
// RecordOwnerOrAdministrator when the owner is not known.
func recordPerson(owner string) string {
	if owner == "" {
		return RecordOwnerOrAdministrator
	}
	return "user " + clierr.ShellQuote(clierr.IDText(owner))
}

// RecordAdvice is what to say about the server a token record names: the
// recovery sequence that revokes the token, or, when no command can name the
// server, the text that says what to do instead. Exactly one is set.
type RecordAdvice struct {
	Sequence []string
	// LoginAs is the user the sequence's first command logs in as: the
	// token's owner exactly as recorded, empty when the owner is unknown. A
	// text that names it passes it through clierr.IDText. Set only with
	// Sequence.
	LoginAs string
	Text    string
}

// noServerText is the text for a token record that names no server: no
// command can reach the server, and the token stays on it. The delete runs as
// the token's owner (owner, when known) or an administrator.
func noServerText(ref clierr.TokenRef, owner string) string {
	return "no server is recorded for token " + clierr.ShellQuote(ref.Name) +
		"; delete it as " + recordPerson(owner) + ", signed in to the server that issued it: " +
		clierr.Rancher + " delete token" + clierr.ShellQuoteArgs(" --apply --yes", ref.Name)
}

// RecordSite says where a token record is reported: on which entry, and
// whether that entry still exists.
type RecordSite struct {
	// Entry is the name of the stored entry the record is reported on; empty
	// when none is reported for it (the record was dropped, or has no entry).
	Entry string
	// EntryURL is the entry's current URL as stored; set for an entry that
	// was removed as well, so the reason stays the one of that URL.
	EntryURL string
	// Removed is set when the entry was removed: nothing is left to repair.
	Removed bool
}

// holdsUserinfo reports whether raw parses with a user name or password.
func holdsUserinfo(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User != nil
}

// AdviseRecord is the one place that decides what to say about a token record
// whose server is recordServer (the entry's own URL stands for the entry's own
// token, and a record may name no server). It decides in this order:
//   - no server: the token stays, and the text says no server is recorded;
//   - the record has the identity of the entry's URL, that URL fails the check
//     and the entry exists (not site.Removed): the entry's repair, or with no
//     entry reported the text for the record, with the reason of the entry's
//     own URL;
//   - the identity does not canonicalize: the token stays, with the reason of
//     the entry's own URL when the record is at it, else of the identity;
//   - the identity is a usable URL, the stored text held a user name or
//     password and the scheme is not http or https: the token stays (no login
//     takes such a URL), with the reason of the entry's own URL when that URL
//     fails, so one value gets one reason. A plain non-http(s) server is not
//     judged by its scheme;
//   - otherwise the recovery sequence, built from the identity and never from
//     the text as stored.
func AdviseRecord(site RecordSite, recordServer, userID string, ref clierr.TokenRef) RecordAdvice {
	owner := userID
	if recordServer == "" {
		return RecordAdvice{Text: noServerText(ref, owner)}
	}
	id := CanonicalServerIdentity(recordServer)
	atEntry := site.EntryURL != "" && id == CanonicalServerIdentity(site.EntryURL)
	var entryProblem error
	if atEntry {
		entryProblem = CheckServerURL(site.EntryURL)
	}
	if entryProblem != nil && !site.Removed {
		if site.Entry != "" {
			return RecordAdvice{Text: EntryOrigin(site.Entry).Text(entryProblem)}
		}
		return RecordAdvice{Text: recordServerText(ref, entryProblem, owner)}
	}
	if err := identityProblem(id); err != nil {
		if entryProblem != nil {
			err = entryProblem
		}
		return RecordAdvice{Text: recordServerText(ref, err, owner)}
	}
	if (holdsUserinfo(recordServer) || (atEntry && holdsUserinfo(site.EntryURL))) && !IsSupportedScheme(id) {
		reason := ErrServerURLScheme
		if entryProblem != nil {
			reason = entryProblem
		}
		return RecordAdvice{Text: recordServerText(ref, reason, owner)}
	}
	return RecordAdvice{Sequence: clierr.RecoverySequence(id, ref), LoginAs: owner}
}

// stripURLUserinfo is steps 1 to 3 of the identity of a URL that does not
// canonicalize: it keeps a leading scheme and "://", removes everything up
// to the last "@" of the rest, and cuts the rest at its first "/", "?" or
// "#". The result holds no "@" and no text a user information part could
// hold.
func stripURLUserinfo(raw string) string {
	scheme := urlSchemePrefixRE.FindString(raw)
	rest := raw[len(scheme):]
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return scheme + rest
}

// DisplayServerURL is the form of a stored, effective or prior server URL
// that output may show: scheme://host[:port] when the URL parses into a
// scheme and a host (userinfo, path, query and fragment dropped), the same
// form of the text left after the userinfo is removed when it does not parse
// and that text parses, else "<unusable URL>". An empty URL shows as empty.
// For a URL that does not parse, the userinfo is looked for only before the
// first "/", "?" or "#": an "@" in a path or query is not the end of a
// userinfo, and what follows it is no host.
func DisplayServerURL(raw string) string {
	if raw == "" {
		return ""
	}
	if shown, ok := schemeAndHost(raw); ok {
		return shown
	}
	scheme := urlSchemePrefixRE.FindString(raw)
	rest := raw[len(scheme):]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if shown, ok := schemeAndHost(stripURLUserinfo(scheme + rest)); ok {
		return shown
	}
	return unusableURLText
}

func schemeAndHost(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// unusableIdentity is the identity of a URL that does not canonicalize. An
// empty input stays empty (callers use "" for "no server recorded"); an input
// that already starts with the prefix and holds no "@" is returned as it is,
// which makes a second pass return the first pass's result.
func unusableIdentity(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, unusableIdentityPrefix) && !strings.Contains(raw, "@") {
		return raw
	}
	return unusableIdentityPrefix + strings.ToLower(stripURLUserinfo(raw))
}

// DisplayServerIdentity is the form of a server identity
// (CanonicalServerIdentity) that output may show: the identity of a URL that
// does not canonicalize loses its prefix, so a legacy record that names a
// bare host shows that host. An identity holds no user information.
func DisplayServerIdentity(identity string) string {
	return strings.TrimPrefix(identity, unusableIdentityPrefix)
}

// CAFileProblem says why a CA file could not be read, in words that name no
// part of the path: a command that takes secrets never echoes a typed value,
// and os.ReadFile's own error repeats the path.
func CAFileProblem(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "the file does not exist"
	}
	return "the file cannot be read"
}
