package configcmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configInfoCmd struct{ dir string }

// NewConfigInfo builds `config status` (local, no network/auth): the resolved
// config file path plus the EFFECTIVE credential-store policy and its source
// (env > config > default auto), with the auto-probe pin when one exists.
// This is the one place that answers "which credential store am I actually
// using": an env-supplied store never persists into config.yaml, so neither
// the file nor `config list-servers` can reveal it.
// dir carries the same flag > env > XDG resolution every
// config command receives from resolveDeps.
func NewConfigInfo(dir string) runtime.Command { return &configInfoCmd{dir: dir} }

func (configInfoCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config status",
		Usage: "Print config diagnostics: file path and effective credential store",
		Desc: "Reports the resolved config file path (and whether it exists), " +
			"the current server (empty when not logged in), " +
			"and the effective credential-store policy with its source: the " +
			"RANCHER_CLI_CREDENTIAL_STORE environment variable, the config " +
			"file's credentialStore key, or the built-in default (auto). " +
			"Under auto, also reports the backend pin a login recorded when present. " +
			"With no pin it reports where the entries' stored credentials are: " +
			"credentialStoreInUse, keychain when an entry has an identity record " +
			"or file when only credentials.yaml holds them; credentialStoreError " +
			"when they are in both, which every credential command refuses; or, " +
			"when nothing is stored, credentialStoreDefault, the OS-native " +
			"keychain a login will try first on this platform (named from the " +
			"platform alone, never probed). " +
			"Also reports a nested completion section (network: live API " +
			"completion on/off; timeout: effective per-TAB budget after " +
			"clamping/default; timeoutRaw: only when the stored value was " +
			"clamped or unparseable). " +
			"Also reports tokenReuse and, under servers, every entry whose current token is CLI-owned or that names superseded tokens: provenance, owner, expiry, logged-out state, and for each token the recovery sequence that revokes it. " +
			"Also reports cacheWritesPaused: every entry whose exec-credential cache writes are paused while a change of its credential finishes, with the reason the pause stays - " +
			"pinStep (a credential store change still has to write its pin, which switches where credentials are read from; the next auth login, auth logout, config delete-server or config update-server --url finishes it), " +
			"changeRecordUnreadable (the store change record cannot be read, as credentialStoreChange reports; restore or move it aside), " +
			"noLock (the change ran with RANCHER_CLI_CONFIG_NO_LOCK; a login or logout of the entry clears it), " +
			"or none (the next exec-credential cache miss or credential change resumes it). " +
			"Also reports credentialStoreChange when the directory has a credential store change record, read the way every " +
			"`rancher auth login`, `rancher auth logout`, `rancher config delete-server` and `rancher config update-server --url` reads it before its own work: " +
			"state active (unfinished; those commands retry the work left), abandoned (a change that never started; the next of them removes it) " +
			"or damaged (the record cannot be read or trusted, named by config.yaml or not; error names the file and what to do, and those commands refuse until it is restored or moved aside), " +
			"the record's id, the id config.yaml names, the file, and under remaining each piece of work left: " +
			"kind (delete or pin), entry, store, cannotFinish when no retry can finish it, and lastError. " +
			"A change whose last step finished is not reported. " +
			"The first place to check when " +
			"tab-completion shows no candidates.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config status"},
		Shape:       runtime.ShapeText,
	}
}

func (c configInfoCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	path := config.FilePath(c.dir)
	exists, err := configFileExists(path)
	if err != nil {
		return runtime.Result{}, err
	}

	cfg, err := config.Load(path)
	if err != nil {
		return runtime.Result{}, err
	}
	// A declaration the variable disagrees with is reported, not resolved:
	// this command never opens the store, so it is the one place that can
	// show the operator both answers instead of failing.
	policy, source, storeConflict := config.EffectivePolicyWithSource(cfg.CredentialStore)
	// The pin is auto's recorded backend choice - meaningless (and, when
	// stale, actively misleading) under any explicit policy, so it is
	// reported only when the effective policy is auto.
	pin := ""
	if policy == "auto" {
		pin = config.ReadPin(c.dir)
	}
	// With no pin, the answer every reader of the directory gets: where the
	// entries' stored credentials are, or the error that both stores are.
	// With nothing stored, name what a login will try first on this
	// machine - a static GOOS-derived label, never a live probe, so
	// `config status` stays read-only.
	defaultBackend, inUse, storeError := "", "", ""
	if policy == "auto" && pin == "" {
		switch u, uerr := config.UnpinnedStoreOf(c.dir, cfg); {
		case uerr != nil:
			storeError = uerr.Error()
		case u.Err() != nil:
			storeError = u.Err().Error()
		case u.Backend() != "":
			inUse = u.Backend()
		default:
			defaultBackend = config.PlatformKeychainLabel()
		}
	}

	fields := []field{
		{"path", path},
		{"exists", exists},
		{"currentServer", cfg.CurrentServer},
		{"credentialStore", policy},
		{"credentialStoreSource", source},
	}
	if storeConflict != nil {
		fields = append(fields, field{"credentialStoreConflict", storeConflict.Error()})
	}
	// A binding stamp that is unreadable, or that does not agree with
	// the one config.yaml carries, costs the directory every warm
	// exec-credential hit on every command and changes nothing else - so
	// nothing else would ever say it out loud.
	if state := cacheBindingStampState(c.dir, cfg); state != "" {
		fields = append(fields, field{"cacheBindingStamp", state})
	}
	if pin != "" {
		fields = append(fields, field{"pin", pin})
	}
	if inUse != "" {
		fields = append(fields, field{"credentialStoreInUse", inUse})
	}
	if storeError != "" {
		fields = append(fields, field{"credentialStoreError", storeError})
	}
	if defaultBackend != "" {
		fields = append(fields, field{"credentialStoreDefault", defaultBackend})
	}

	// Completion settings - always present so `config status` can answer
	// "why does TAB show no candidates?" without knowing whether completion
	// was ever configured. CompletionTimeoutWithRaw resolves effective vs.
	// raw in one pass so the two cannot disagree.
	effectiveTimeout, rawTimeout, diverges := config.CompletionTimeoutWithRaw(cfg)
	completionFields := []field{
		{"network", cfg.Completion.Network},
		{"timeout", effectiveTimeout.String()},
	}
	if diverges {
		completionFields = append(completionFields, field{"timeoutRaw", rawTimeout})
	}
	fields = append(fields, field{"completion", completionFields})
	fields = append(fields, field{"tokenReuse", cfg.TokenReuse})

	// Servers: every entry with CLI-owned tokens (current or records).
	var serverSections []field
	serverNames := make([]string, 0, len(cfg.Servers))
	for n, s := range cfg.Servers {
		if s.HoldsCLITokens() {
			serverNames = append(serverNames, n)
		}
	}
	slices.Sort(serverNames)
	for _, n := range serverNames {
		s := cfg.Servers[n]
		serverSections = append(serverSections, field{n, entrySection(n, s)})
	}
	var text strings.Builder
	data := renderFields(fields, "", &text)

	// The credential store change record, read the way every credential
	// command's prelude reads it, so the refusal or warning a command gave
	// can be looked at here. A report only: it never fails the command and
	// opens no credential store.
	if change := config.ChangeRecordStatusOf(c.dir, cfg); change != nil {
		text.WriteString("credentialStoreChange:\n")
		for _, line := range change.TextLines() {
			fmt.Fprintf(&text, "  %s\n", line)
		}
		data["credentialStoreChange"] = change
	}

	// Entries whose cache writes a change left paused. No other report
	// shows them, and every kubectl call for them makes a fresh token
	// until the pause is cleared.
	if paused := config.PausedEntries(c.dir, cfg); len(paused) > 0 {
		text.WriteString("cacheWritesPaused:\n")
		for _, p := range paused {
			fmt.Fprintf(&text, "  %s: %s\n", p.Entry, pauseReasonText(c.dir, p.Entry, p.Reason))
		}
		data["cacheWritesPaused"] = paused
	}

	// The entries render from their own projections: entryReport owns the
	// JSON shape (list-shaped records, shared with set-token-reuse),
	// entrySection the text.
	if len(serverSections) > 0 {
		text.WriteString("servers:\n")
		renderFields(serverSections, "  ", &text)
		serverData := make(map[string]any, len(serverSections))
		for _, n := range serverNames {
			serverData[n] = entryReport(n, cfg.Servers[n])
		}
		data["servers"] = serverData
	}

	rendered := text.String()
	return runtime.Result{
		Kind:       "ConfigInfo",
		Data:       data,
		Text:       rendered,
		StyledText: func(s style.Style) string { return styleConfigInfoText(rendered, s) },
	}, nil
}

// field is one key with a scalar, a nested section ([]field), or a list of
// strings ([]string). Sections nest to any depth; each level indents two
// spaces; list items sit one level under their key.
type field struct {
	key string
	val any
}

// renderFields writes fields as text at indent and returns their JSON
// projection, so -o json and the text view cannot drift for any level.
func renderFields(fields []field, indent string, text *strings.Builder) map[string]any {
	data := make(map[string]any, len(fields))
	for _, f := range fields {
		switch v := f.val.(type) {
		case []field:
			fmt.Fprintf(text, "%s%s:\n", indent, f.key)
			data[f.key] = renderFields(v, indent+"  ", text)
		case []string:
			fmt.Fprintf(text, "%s%s:\n", indent, f.key)
			for _, item := range v {
				fmt.Fprintf(text, "%s  %s\n", indent, item)
			}
			data[f.key] = v
		default:
			data[f.key] = v
			fmt.Fprintf(text, "%s%s: %v\n", indent, f.key, v)
		}
	}
	return data
}

// entrySection is the text form of entryReport: provenance, owner, auth config, expiry,
// logged-out state, the current token, and one section per superseded
// record keyed by the token, each with state, last attempt, and recovery.
func entrySection(name string, s config.Server) []field {
	var out []field
	if s.Provenance != "" {
		out = append(out, field{"provenance", string(s.Provenance)})
	}
	if s.TokenUserID != "" {
		out = append(out, field{"tokenUserId", output.Printable(s.TokenUserID)})
	}
	if s.TokenAuthConfig != "" {
		out = append(out, field{"tokenAuthConfig", output.Printable(s.TokenAuthConfig)})
	}
	if s.CredentialExpiresAt != "" {
		out = append(out, field{"credentialExpiresAt", output.Printable(s.CredentialExpiresAt)})
	}
	if s.LoggedOutAt != "" {
		out = append(out, field{"loggedOutAt", s.LoggedOutAt}, field{"credential", "no stored credential"})
	}
	switch {
	case s.CLIOwned():
		out = append(out, field{"currentToken", tokenSection(name, s.URL, *s.TokenRef, s.TokenUserID, s.CredentialExpiresAt)})
	case s.CLICreated() && s.LoggedOutAt == "":
		// The CLI created the token but never recorded its name: nothing
		// can be revoked by name, and the surface says so rather than
		// hiding the entry.
		out = append(out, field{"currentToken", []field{{"note", "cannot revoke by name: no tokenRef recorded"}}})
	}
	if len(s.SupersededTokens) > 0 {
		var recs []field
		for _, r := range s.SupersededTokens {
			// The record's section is keyed by the token, so its fields
			// do not repeat the ref.
			var sec []field
			if r.UserID != "" {
				sec = append(sec, field{"userId", output.Printable(r.UserID)})
			}
			if r.Server != "" {
				sec = append(sec, field{"server", config.DisplayServerURL(r.Server)})
			}
			if r.ExpiresAt != "" {
				sec = append(sec, field{"expiresAt", output.Printable(r.ExpiresAt)})
			}
			sec = append(sec, field{"supersededAt", r.SupersededAt}, field{"state", recordState(s, r)})
			if r.LastAttempt != nil {
				sec = append(sec, field{"lastAttempt", string(r.LastAttempt.Result) + " (" + output.Printable(r.LastAttempt.Error) + ")"})
			}
			sec = append(sec, recoveryField(name, s.URL, cmp.Or(r.Server, s.URL), r.UserID, r.Ref))
			recs = append(recs, field{output.Printable(r.Ref.String()), sec})
		}
		out = append(out, field{"supersededTokens", recs})
	}
	return out
}

// tokenSection renders an entry's current token: ref, owner, expiry, then
// the recovery sequence that revokes it.
func tokenSection(entry, serverURL string, ref clierr.TokenRef, userID, expiresAt string) []field {
	sec := []field{{"ref", output.Printable(ref.String())}}
	if userID != "" {
		sec = append(sec, field{"userId", output.Printable(userID)})
	}
	if expiresAt != "" {
		sec = append(sec, field{"expiresAt", output.Printable(expiresAt)})
	}
	return append(sec, recoveryField(entry, serverURL, serverURL, userID, ref))
}

// recoveryField is a recovery sequence with the assumption its commands
// depend on as a trailing line, the same sentence delete-server and
// set-token-reuse print, and a leading line that says who logs in (the
// token's owner when known, else its owner or an administrator); for a v3
// token it is the administrator's delete alone. Text only: the JSON side is
// entryReport, whose recovery value stays the commands alone.
func recoveryField(entry, entryURL, serverURL, userID string, ref clierr.TokenRef) field {
	ref.Name = output.Printable(ref.Name)
	userID = output.Printable(userID)
	if ref.Kind == clierr.TokenKindV3 {
		return field{"recovery", []string{administratorLine(serverURL, ref)}}
	}
	advice := config.AdviseRecord(config.RecordSite{Entry: entry, EntryURL: entryURL}, serverURL, userID, ref)
	if advice.Text != "" {
		return field{"recovery", []string{advice.Text}}
	}
	lines := append(advice.Sequence, "("+clierr.RecoverySequenceAssumption+")")
	login := "log in as the token's owner or an administrator, then run:"
	if advice.LoginAs != "" {
		login = "log in as user " + clierr.ShellQuote(advice.LoginAs) + " (the token's owner), then run:"
	}
	lines = append([]string{login}, lines...)
	return field{"recovery", lines}
}

// pauseReasonText is the reason entry's pause stays, as config status
// says it, with what clears it.
func pauseReasonText(dir, entry, reason string) string {
	switch reason {
	case config.PauseReasonPinStep:
		return "pinStep - a credential store change still has to write its pin, which switches where credentials are read from; the next auth login, auth logout, config delete-server or config update-server --url finishes it"
	case config.PauseReasonChangeRecordUnreadable:
		return "changeRecordUnreadable - the credential store change record " + config.TransitionPath(dir) + " cannot be read; inspect it, then restore a valid copy or move it aside; credentialStoreChange says why it cannot be read"
	case config.PauseReasonNoLock:
		text := "noLock - the change ran with " + config.EnvConfigNoLock + "; a login or logout of the entry clears it"
		if hint := config.LoginHintFor(entry); hint != "" {
			text += ": " + hint
		}
		return text
	}
	return "none - the next exec-credential cache miss or credential change resumes it"
}

// configInfoSectionHeaders is the exact line config status always emits for
// its nested sections: the completion block is unconditional (see
// Execute's field list above), so "completion:" needs no presence check;
// "servers:" appears only when there are CLI-owned tokens,
// "cacheWritesPaused:" only when an entry's cache writes are paused, and
// "credentialStoreChange:" only when the directory has a change record to
// report.
var configInfoSectionHeaders = map[string]bool{"completion:": true, "servers:": true, "cacheWritesPaused:": true, "credentialStoreChange:": true}

// styleConfigInfoText applies config status's bold span to text, the plain
// rendering built above: the "completion:", "servers:",
// "cacheWritesPaused:" and "credentialStoreChange:" section labels, bold,
// whole line. Every flat "key: value" line stays plain.
func styleConfigInfoText(text string, s style.Style) string {
	return runtime.BoldExactLines(text, configInfoSectionHeaders, s)
}

// cacheBindingStampState names what is wrong with the directory's binding
// stamp, or the empty string when nothing is. The two failures look the
// same from outside - no cached credential is ever served - and have the
// same remedy, so both are reported here and nowhere else.
//
// The disagreement is what a crash between the two halves of a config
// write leaves behind: the stamp file moved and config.yaml did not, and
// no reader's two halves line up again until a write that stamps puts
// them back in step.
func cacheBindingStampState(dir string, cfg *config.Config) string {
	const remedy = "cached credentials are not served until the next login or config change"
	stamp, ok := config.ReadBindingStamp(dir)
	switch {
	case !ok:
		return "unreadable; " + remedy
	case cfg != nil && stamp != cfg.BindingStamp:
		return "disagrees with config.yaml; " + remedy
	}
	return ""
}
