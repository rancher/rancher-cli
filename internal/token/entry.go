package token

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// identifyEntry returns the server entry an exec entry's --name means, or
// "" when it means none, as config.IdentifyEntry decides it. An identified
// entry registered at another URL is an error, never a reason to use the
// URL's other entries. The shim (ignoreCurrentContext) never identifies an
// entry.
func identifyEntry(cfg *config.Config, server, wantName, clusterID string, ignoreCurrentContext bool) (string, error) {
	id := config.IdentifyEntry(cfg, server, wantName, clusterID, ignoreCurrentContext)
	if id.OtherURL {
		return "", entryOnOtherServerError(cfg, id.Entry, server, clusterID)
	}
	return id.Entry, nil
}

// entryChoice is an identification made at one moment: the entry a call's
// --name identified ("" for none) when config.yaml was read to choose the CA,
// or unknown when the CA was not chosen from it.
type entryChoice struct {
	known bool
	name  string
}

// configChangedError is the refusal when the two reads of config.yaml in one
// call identify different entries: the file changed under the call, most
// likely a login or a server entry written by another process. Nothing has
// been sent or created, so trying again is safe.
func configChangedError(was, now string) *clierr.Error {
	describe := func(name string) string {
		if name == "" {
			return "no entry"
		}
		return "entry " + clierr.ShellQuote(name)
	}
	return clierr.Retryable(clierr.CodeConflict, fmt.Sprintf(
		"config.yaml changed while this call was running: the exec entry's --name identified %s when the connection was set up and identifies %s now; run the command again",
		describe(was), describe(now)))
}

// readEntryCredential reads one entry's stored login credential. A clean
// miss - nothing stored, or a blank value - is (zero, false, nil); every
// other store failure is returned as it came.
func readEntryCredential(cfg *config.Config, store CredentialReader, name string) (storedCredential, bool, error) {
	s := cfg.Servers[name]
	ref := config.ServerRef{Name: name, URL: s.URL}
	tok, err := store.GetToken(ref)
	switch {
	case err == nil && strings.TrimSpace(tok) != "":
		return storedCredential{name: name, token: tok, ref: ref, entry: s}, true, nil
	case err == nil, errors.Is(err, config.ErrNoToken):
		return storedCredential{}, false, nil
	}
	return storedCredential{}, false, err
}

// identifiedCredential is the lookup for a call that names an entry. The
// entry's own credential is the only one read; a store failure on it, of
// any kind, is returned rather than answered by another entry. The one
// fallback is the plugin's own login entry for the server, while token
// reuse is on; for a call with --user-id, the one that records that user.
// A plugin login entry is never read with the setting off.
func identifiedCredential(cfg *config.Config, store CredentialReader, server, identified string, user userCheck) (storedCredential, bool, error) {
	isPluginEntry := cfg.Servers[identified].Provenance == clierr.ProvenanceExecLogin
	if isPluginEntry && !cfg.TokenReuse {
		return storedCredential{}, false, nil
	}
	if cred, ok, err := readEntryCredential(cfg, store, identified); ok || err != nil {
		return cred, ok, err
	}
	if isPluginEntry || !cfg.TokenReuse {
		return storedCredential{}, false, nil
	}
	plugin, err := pluginEntryFor(cfg, server, identified, user)
	if err != nil || plugin == "" {
		return storedCredential{}, false, err
	}
	return readEntryCredential(cfg, store, plugin)
}

// pluginEntryFor returns the plugin's own login entry for server, the
// fallback of a call whose identified user entry holds no credential: the
// one entry with the execLogin provenance that answers for it, "" when
// there is none. For a call with --user-id only the entries that record
// that user count and, for a call with an auth config, the group that
// answers: the entries that recorded that auth config, else the ones that
// recorded none. A plugin login entry has no fallback, so its caller
// never asks. The refusal of several (config.PluginFallbackRefused for a
// user entry) is an error.
func pluginEntryFor(cfg *config.Config, server, identified string, user userCheck) (string, error) {
	found := cfg.AnsweringPluginEntries(server, user.userID, user.authConfig)
	switch {
	case config.PluginFallbackRefused(false, len(found)):
		return "", pluginEntriesError(server, found, cfg, identified, user)
	case len(found) == 0:
		return "", nil
	}
	return found[0], nil
}

// pluginEntriesError is the refusal when more than one login entry the exec
// plugin created answers for one URL (for a call with --user-id, records
// that user): the plugin's own login is one entry, so the extras go before
// anything can be chosen. The first remedy is a login that makes the call
// answer without them: for identified, the user entry the call's --name
// identifies, a login of that entry, which is the hint; otherwise a login
// of a new entry. Removing all but one entry comes last, since removing
// another user's login breaks that user's calls.
func pluginEntriesError(server string, names []string, cfg *config.Config, identified string, user userCheck) *clierr.Error {
	cands := make([]clierr.Candidate, len(names))
	for i, n := range names {
		cands[i] = clierr.Candidate{ID: n, Name: config.DisplayServerURL(cfg.Servers[n].URL)}
	}
	msg := fmt.Sprintf("more than one login entry the exec plugin created answers for %s (%s)", server, quotedList(names))
	piped := "pipe an API token"
	if user.userID != "" {
		piped += " of " + clierr.ShellQuote(user.userID)
		if user.authConfig != "" {
			piped += " made through auth config " + clierr.ShellQuote(user.authConfig)
		}
	} else {
		msg += ", and this exec entry names no Rancher user"
	}
	hint := ""
	if identified != "" && config.ValidateEntryName(identified) == nil {
		hint = clierr.Rancher + " auth login" + clierr.ShellQuoteArgs(" --token-stdin", identified)
		msg += "; to log in to " + clierr.ShellQuote(identified) + ", " + piped + " to: " + hint
	} else {
		url := user.serverID
		if url == "" {
			url = server
		}
		login := NoTerminalLogin(cfg, "", user.clusterID, url, user.caSource, user.caFrom, false)
		msg += "; " + login.Instruction("to log in an entry for "+clierr.ShellQuote(url)+", "+piped+" to: ")
	}
	msg += "; or keep one: for each other entry run " + clierr.Rancher + " auth logout <entry>; then " + clierr.Rancher + " config delete-server <entry>"
	return clierr.New(clierr.CodeAmbiguousSelection, msg).WithCandidates(cands).WithCause(errLookupRefused).WithHint(hint)
}

// useServerCommand is the command that selects the entry name; an empty name
// gives the placeholder <entry>.
func useServerCommand(name string) string {
	if name == "" {
		return clierr.Rancher + " config use-server <entry>"
	}
	return clierr.Rancher + " config use-server" + clierr.ShellQuoteArgs("", name)
}

// useServerNote is the side effect of selecting an entry on the directory
// and how to undo it, for the end of a sentence that holds the command:
// current is the directory's current server, empty when it has none.
func useServerNote(current string) string {
	if current == "" {
		return "; selecting an entry sets the directory's current server, which is unset now"
	}
	return "; selecting an entry switches the directory's current server; to switch back, run: " +
		clierr.Rancher + " config use-server" + clierr.ShellQuoteArgs("", current)
}

// kubeconfigRemedy is the command that writes the kubeconfig for a cluster;
// an empty clusterID gives the placeholder <id>.
func kubeconfigRemedy(clusterID string) string {
	if clusterID == "" {
		return clierr.Rancher + " kubeconfig --cluster <id>"
	}
	return clierr.Rancher + " kubeconfig" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(clusterID))
}

// entryNotNamedError is the refusal when a call names no entry and more than
// one entry that answers for its URL has a stored login. Choosing one would
// authenticate as whichever user the choice happened to be. The first
// remedy changes only the exec entry; the second rewrites the kubeconfig
// from the entry the caller picks and switches the directory's current
// server on the way. Only user entries tie: several plugin login entries
// are refused by pluginEntriesError before any is read.
func entryNotNamedError(cfg *config.Config, hits []storedCredential, clusterID string, ignoreCurrentContext bool) *clierr.Error {
	names := make([]string, len(hits))
	cands := make([]clierr.Candidate, len(hits))
	for i, h := range hits {
		names[i] = h.name
		cands[i] = clierr.Candidate{ID: h.name, Name: config.DisplayServerURL(h.entry.URL)}
	}
	clusterSuffix, idNote := "<id>", "<id> stands for the id of the cluster; "
	if clusterID != "" {
		clusterSuffix, idNote = shellWordIfNeeded(clusterID), ""
	}
	regenerate := useServerCommand("") + "; then " + kubeconfigRemedy(clusterID) + useServerNote(cfg.CurrentServer)
	var msg string
	if ignoreCurrentContext {
		msg = fmt.Sprintf(
			"more than one server entry for %s has a stored login (%s) and the legacy token command cannot name one; %sto replace this kubeconfig, regenerate it from the entry you want, and use the one it prints: %s",
			config.DisplayServerURL(hits[0].entry.URL), quotedList(names), idNote, regenerate)
	} else {
		msg = fmt.Sprintf(
			"more than one server entry for %s has a stored login (%s) and this exec entry names none of them; %sgive the exec entry --name <entry>-%s; or regenerate the kubeconfig from the entry you want with: %s",
			config.DisplayServerURL(hits[0].entry.URL), quotedList(names), idNote, clusterSuffix, regenerate)
	}
	return clierr.New(clierr.CodeAmbiguousSelection, msg).WithCandidates(cands).WithCause(errLookupRefused)
}

// entryOnOtherServerError is the refusal when the entry name a call names
// is registered in cfg at a different URL than server, the one the call
// runs for. The named entry is the first route to offer: its kubeconfig
// regenerated from where it is now. Regenerating from an entry registered
// at the call's URL is the other route, offered only when one exists.
func entryOnOtherServerError(cfg *config.Config, name, server, clusterID string) *clierr.Error {
	entry := clierr.ShellQuote(name)
	entryURL, current := config.DisplayServerURL(cfg.Servers[name].URL), cfg.CurrentServer
	// auth login refuses a plugin login entry's name, so its route logs in
	// a new entry at its URL, with its CA, and regenerates from that one.
	plugin := config.IsExecLoginEntryName(name)
	ownEntry := ""
	if plugin {
		s := cfg.Servers[name]
		ownEntry = "log in there under a server entry of your own, since auth login refuses the exec plugin's own login entries; " +
			userCheck{clusterID: clusterID, serverID: config.DisplayServerURL(s.URL), caSource: s.CA, caFrom: config.CASource{Kind: config.CASourceEntry, Entry: name}}.loginOfNewEntry(cfg)
	}
	if len(cfg.MatchingServerNames(server)) == 0 {
		route := "regenerate the kubeconfig from " + entry + ": " + namedEntryRoute(name, clusterID, current) + namedEntryNote(name, current)
		if plugin {
			route = ownEntry + useServerNote(current)
		}
		return clierr.New(clierr.CodeConflict, fmt.Sprintf(
			"the exec entry names server entry %s, which is registered at %s, but this call runs for %s and no entry is registered there; %s",
			entry, entryURL, server, route))
	}
	route := "regenerate the kubeconfig from it: " + namedEntryRoute(name, clusterID, current)
	if plugin {
		route = ownEntry
	}
	return clierr.New(clierr.CodeConflict, fmt.Sprintf(
		"the exec entry names server entry %s, which is registered at %s, but this call runs for %s; to use %s, %s; to use %s, regenerate it from an entry registered there with: ",
		entry, entryURL, server, entry, route, server)+
		useServerCommand("")+"; then "+kubeconfigRemedy(clusterID)+useServerNote(current))
}

// namedEntryRoute is the steps that regenerate the kubeconfig from the named
// user entry where it is registered now: select it unless it is already the
// current server, log in to it if a URL change cleared its login, then write
// the kubeconfig. A plugin login entry's name gets none: auth login refuses
// it, and the route for one is a login of a new entry. A route that selects
// an entry ends with namedEntryNote.
func namedEntryRoute(name, clusterID, current string) string {
	if config.IsExecLoginEntryName(name) {
		return ""
	}
	steps := "if it has no login, run: " + clierr.Rancher + " auth login" + clierr.ShellQuoteArgs("", name) + "; then " + kubeconfigRemedy(clusterID)
	if name == current {
		return steps
	}
	return useServerCommand(name) + "; then " + steps
}

// namedEntryNote is the note that ends a route of namedEntryRoute that
// selects an entry, empty for a route that does not.
func namedEntryNote(name, current string) string {
	if config.IsExecLoginEntryName(name) || name == current {
		return ""
	}
	return useServerNote(current)
}

// shellWordIfNeeded quotes s for a shell only when it holds a character a
// shell would split or expand, so a plain value reads as it is.
func shellWordIfNeeded(s string) string {
	for _, r := range s {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)
		if !plain {
			return clierr.ShellQuote(s)
		}
	}
	return s
}

// quotedList renders entry names as shell-quoted words for a message.
func quotedList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = clierr.ShellQuote(n)
	}
	return strings.Join(q, ", ")
}
