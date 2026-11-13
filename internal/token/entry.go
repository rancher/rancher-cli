package token

import (
	"errors"
	"fmt"
	"slices"
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
	if id.Entry != "" {
		// An entry whose URL cannot be used is refused whatever URL the call
		// runs for: its stored login is never read or sent for the host read
		// from the URL.
		if err := unusableEntryError(cfg, id.Entry, server, clusterID, true); err != nil {
			return "", err
		}
	}
	return id.Entry, nil
}

// usableEntry reports whether the URL of the entry passes the server URL
// check, so the entry's stored login may be read for a call.
func usableEntry(cfg *config.Config, name string) bool {
	return config.CheckServerURL(cfg.Servers[name].URL) == nil
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
	// An entry whose URL cannot be used answers no call, and does not make one
	// ambiguous: its login is never read for the host read from that URL.
	found = slices.DeleteFunc(found, func(n string) bool { return !usableEntry(cfg, n) })
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
	var hint clierr.Command
	if identified != "" && config.ValidateEntryName(identified) == nil {
		hint = clierr.NewCommand(config.LoginWords).Arg(identified).Flag("--token-stdin")
		msg = clierr.Text(msg+"; to log in to "+clierr.ShellQuote(identified)+", "+piped, clierr.Lead(" to: "), hint)
	} else {
		url := user.serverID
		if url == "" {
			url = server
		}
		login := NoTerminalLogin(cfg, "", user.clusterID, url, user.caSource, user.caFrom, false)
		msg += "; " + login.Instruction("to log in an entry for "+clierr.ShellQuote(url)+", "+piped)
	}
	removal := clierr.NewSequence().
		Then("", clierr.NewCommand("auth logout").Placeholder("<entry>")).
		Then("then ", clierr.NewCommand(config.DeleteServerWords).Placeholder("<entry>"))
	msg = clierr.Text(msg+"; or keep one: for each other entry", clierr.Lead(" run "), removal)
	return clierr.New(clierr.CodeAmbiguousSelection, clierr.NoteOnce(msg)).WithCandidates(cands).WithCause(errLookupRefused).WithHintCommand(hint)
}

// useServerCommand is the command that selects the entry name; an empty name
// gives the placeholder <entry>.
func useServerCommand(name string) clierr.Command {
	use := clierr.NewCommand("config use-server")
	if name == "" {
		return use.Placeholder("<entry>")
	}
	return use.Arg(name)
}

// The note that ends a sentence whose commands select an entry: what that
// does to the directory's current server, and, when there is one, the
// command that switches back.
const (
	switchUnsetNote             = "; selecting an entry sets the directory's current server, which is unset now"
	switchNote                  = "; selecting an entry switches the directory's current server"
	switchBackLead  clierr.Lead = "; to switch back, run: "
)

// useServerNote is the side effect of selecting an entry on the directory
// and how to undo it, for the end of a sentence that holds the command:
// current is the directory's current server, empty when it has none. The
// command that switches back is its own, left out alone.
func useServerNote(current string) string {
	if current == "" {
		return switchUnsetNote
	}
	return clierr.Text(switchNote, switchBackLead, useServerCommand(current))
}

// kubeconfigRemedy is the command that writes the kubeconfig for a cluster;
// an empty clusterID gives the placeholder <id>.
func kubeconfigRemedy(clusterID string) clierr.Command {
	kubeconfig := clierr.NewCommand("kubeconfig")
	if clusterID == "" {
		return kubeconfig.FlagText(" --cluster <id>")
	}
	return kubeconfig.FlagValue("--cluster", clusterID)
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
	idNote := "<id> stands for the id of the cluster; "
	if clusterID != "" {
		idNote = ""
	}
	regenerate := clierr.NewSequence().
		Then("", useServerCommand("")).
		Then("then ", kubeconfigRemedy(clusterID))
	var msg string
	if ignoreCurrentContext {
		msg = clierr.Text(fmt.Sprintf(
			"more than one server entry for %s has a stored login (%s) and the legacy token command cannot name one; %sto replace this kubeconfig, regenerate it from the entry you want, and use the one it prints",
			config.DisplayServerURL(hits[0].entry.URL), quotedList(names), idNote), clierr.Lead(": "), regenerate)
	} else {
		instruction := "regenerate the kubeconfig from the entry you want"
		fragment, ok := entryNameFragment(clusterID)
		if ok {
			instruction = "give the exec entry " + fragment + "; or " + instruction
		}
		msg = clierr.Text(fmt.Sprintf(
			"more than one server entry for %s has a stored login (%s) and this exec entry names none of them; %s%s",
			config.DisplayServerURL(hits[0].entry.URL), quotedList(names), idNote, instruction), clierr.Lead(" with: "), regenerate)
		if !ok {
			msg = clierr.WithLeftOutNote(msg)
		}
	}
	msg = clierr.NoteOnce(msg + useServerNote(cfg.CurrentServer))
	return clierr.New(clierr.CodeAmbiguousSelection, msg).WithCandidates(cands).WithCause(errLookupRefused)
}

// entryNameFragment is the --name an exec entry is given to name an entry for
// every cluster's kubeconfig: <entry>-<cluster>, with the cluster id as the
// shell reads it.
func entryNameFragment(clusterID string) (string, bool) {
	if clusterID == "" {
		return clierr.FlagFragmentText("--name", "<entry>-<id>"), true
	}
	return clierr.FlagFragmentAfterWord("--name", "<entry>-", clusterID)
}

// entryOnOtherServerError is the refusal when the entry name a call names
// is registered in cfg at a different URL than server, the one the call
// runs for. The named entry is the first route to offer: its kubeconfig
// regenerated from where it is now. Regenerating from an entry registered
// at the call's URL is the other route, offered only when one exists.
func entryOnOtherServerError(cfg *config.Config, name, server, clusterID string) *clierr.Error {
	entry := clierr.ShellQuote(name)
	if err := unusableEntryError(cfg, name, server, clusterID, true); err != nil {
		return err
	}
	entryURL, current := config.DisplayServerURL(cfg.Servers[name].URL), cfg.CurrentServer
	// auth login refuses a plugin login entry's name, so its route logs in
	// a new entry at its URL, with its CA, and regenerates from that one.
	plugin := config.IsExecLoginEntryName(name)
	ownEntry := ""
	if plugin {
		s := cfg.Servers[name]
		serverID := config.DisplayServerURL(s.URL)
		if serverID == config.UnusableURLDisplay {
			// Not a URL the login can name: the command is left out.
			serverID = ""
		}
		ownEntry = "log in there under a server entry of your own, since auth login refuses the exec plugin's own login entries; " +
			userCheck{clusterID: clusterID, serverID: serverID, caSource: s.CA, caFrom: config.CASource{Kind: config.CASourceEntry, Entry: name}}.loginOfNewEntry(cfg)
	}
	steps, _ := namedEntryRoute(name, clusterID, current)
	if len(cfg.MatchingServerNames(server)) == 0 {
		route := ownEntry + useServerNote(current)
		if !plugin {
			route = clierr.Text("regenerate the kubeconfig from "+entry, clierr.Lead(": "), steps) + namedEntryNote(name, current)
		}
		return clierr.New(clierr.CodeConflict, clierr.NoteOnce(fmt.Sprintf(
			"the exec entry names server entry %s, which is registered at %s, but this call runs for %s and no entry is registered there; %s",
			entry, entryURL, server, route)))
	}
	route := ownEntry
	if !plugin {
		route = clierr.Text("regenerate the kubeconfig from it", clierr.Lead(": "), steps)
	}
	regenerate := clierr.NewSequence().
		Then("", useServerCommand("")).
		Then("then ", kubeconfigRemedy(clusterID))
	msg := clierr.Text(fmt.Sprintf(
		"the exec entry names server entry %s, which is registered at %s, but this call runs for %s; to use %s, %s; to use %s, regenerate it from an entry registered there",
		entry, entryURL, server, entry, route, server), clierr.Lead(" with: "), regenerate)
	return clierr.New(clierr.CodeConflict, clierr.NoteOnce(msg+useServerNote(current)))
}

// unusableEntryError is the refusal when the entry a call names holds a URL
// that cannot be used, nil when it can; named is set when the call named the
// entry, and the text then says so, else it says the call runs for the server
// the entry is registered at. No command can name the entry's URL:
// its refusal and repair come first, and no login or regeneration is suggested
// from it. The repair of a plugin login entry removes the entry, so that form
// gives the reason alone and leaves the entry as it is. The route that
// regenerates the kubeconfig from an entry at the call's URL is kept when a
// usable entry is registered there.
func unusableEntryError(cfg *config.Config, name, server, clusterID string, named bool) *clierr.Error {
	err := config.CheckServerURL(cfg.Servers[name].URL)
	if err == nil {
		return nil
	}
	text := config.EntryOrigin(name).Text(err)
	if config.IsExecLoginEntryName(name) {
		text = config.EntryOrigin(name).RefusalWithoutRepair(err)
	}
	vouched := usableEntryAtServer(cfg, server)
	shown := server
	if masked, hidden := maskedCacheServer(cfg, server); hidden && !vouched {
		// The call's server is the host read from the entry's URL: the start of
		// a user name and password, shown as the list of cached tokens shows it.
		shown = masked
	}
	msg := fmt.Sprintf("the call names server entry %s and runs for %s; %s", clierr.ShellQuote(name), shown, text)
	if !named {
		msg = fmt.Sprintf("the call runs for %s, where server entry %s is registered; %s", shown, clierr.ShellQuote(name), text)
	}
	if vouched {
		regenerate := clierr.NewSequence().
			Then("", useServerCommand("")).
			Then("then ", kubeconfigRemedy(clusterID))
		msg = clierr.NoteOnce(clierr.Text(msg+"; to use "+server+", regenerate it from an entry registered there", clierr.Lead(" with: "), regenerate) +
			useServerNote(cfg.CurrentServer))
	}
	return clierr.New(clierr.CodeConflict, msg)
}

// usableEntryAtServer reports whether a usable entry is registered at the
// identity of server: the server is then a host its own entry's URL names.
func usableEntryAtServer(cfg *config.Config, server string) bool {
	return slices.ContainsFunc(cfg.MatchingServerNames(server), func(n string) bool { return usableEntry(cfg, n) })
}

// unusableEntryAtServer is the first entry (by name) at the identity of
// server whose URL cannot be used, "" when there is none. The lookups leave
// such an entry out of their candidates; a call none of the others answers
// is refused with this entry's refusal, never sent on to a sign-in at the
// host read from its URL.
func unusableEntryAtServer(cfg *config.Config, server string) string {
	for _, n := range cfg.MatchingServerNames(server) {
		if !usableEntry(cfg, n) {
			return n
		}
	}
	return ""
}

// namedEntryRoute is the steps that regenerate the kubeconfig from the named
// user entry where it is registered now: select it unless it is already the
// current server, log in to it if a URL change cleared its login, then write
// the kubeconfig. A plugin login entry's name gets none (ok is false): auth
// login refuses it, and the route for one is a login of a new entry. A route
// that selects an entry ends with namedEntryNote.
func namedEntryRoute(name, clusterID, current string) (steps clierr.Sequence, ok bool) {
	if config.IsExecLoginEntryName(name) {
		return clierr.Sequence{}, false
	}
	login := clierr.NewCommand(config.LoginWords).Arg(name)
	steps = clierr.NewSequence()
	if name == current {
		steps = steps.Then("if it has no login, run: ", login)
	} else {
		steps = steps.Then("", useServerCommand(name)).Then("then if it has no login, run: ", login)
	}
	return steps.Then("then ", kubeconfigRemedy(clusterID)), true
}

// namedEntryNote is the note that ends a route of namedEntryRoute that
// selects an entry, empty for a route that does not.
func namedEntryNote(name, current string) string {
	if config.IsExecLoginEntryName(name) || name == current {
		return ""
	}
	return useServerNote(current)
}

// quotedList renders entry names as shell-quoted words for a message.
func quotedList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = clierr.ShellQuote(n)
	}
	return strings.Join(q, ", ")
}
