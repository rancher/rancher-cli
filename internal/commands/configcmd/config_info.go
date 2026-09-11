package configcmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configInfoCmd struct{ dir string }

// NewConfigInfo builds `config status` (local, no network/auth): the resolved
// config file path plus the EFFECTIVE credential-store policy and its source
// (env > config > default auto), with the auto-probe pin when one exists.
// This is the one place that answers "which credential store am I actually
// using": an env-supplied store never persists into config.yaml, so neither
// the file nor `config list-servers` can reveal it (vault-helper live
// exercise, S-05). dir carries the same flag > env > XDG resolution every
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
			"Under auto, also reports the probed backend pin when present, or, " +
			"before any pin exists, credentialStoreDefault, the OS-native " +
			"keychain auto will try first on this platform (named from the " +
			"platform alone, never probed). " +
			"Also reports a nested completion section (network: live API " +
			"completion on/off; timeout: effective per-TAB budget after " +
			"clamping/default; timeoutRaw: only when the stored value was " +
			"clamped or unparseable). " +
			"Also reports tokenReuse and, under servers, every entry whose current token is CLI-owned or that names superseded tokens: provenance, owner, expiry, logged-out state, and for each token the recovery sequence that revokes it. " +
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
	policy, source := config.EffectivePolicyWithSource(cfg.CredentialStore)
	// The pin is auto's probed backend choice - meaningless (and, when
	// stale, actively misleading) under any explicit policy, so it is
	// reported only when the effective policy is auto.
	pin := ""
	if policy == "auto" {
		pin = config.ReadPin(c.dir)
	}
	// Before any pin exists (no login yet, no prior probe), name what auto
	// will try first on this machine - a static GOOS-derived label, never a
	// live probe, so `config status` stays read-only.
	defaultBackend := ""
	if policy == "auto" && pin == "" {
		defaultBackend = config.PlatformKeychainLabel()
	}

	fields := []field{
		{"path", path},
		{"exists", exists},
		{"currentServer", cfg.CurrentServer},
		{"credentialStore", policy},
		{"credentialStoreSource", source},
	}
	if pin != "" {
		fields = append(fields, field{"pin", pin})
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
		serverSections = append(serverSections, field{n, entrySection(s)})
	}
	var text strings.Builder
	data := renderFields(fields, "", &text)

	// The entries render from their own projections: entryReport owns the
	// JSON shape (list-shaped records, shared with set-token-reuse and
	// delete-server), entrySection the text.
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

// entrySection is the text form of entryReport: provenance, owner, expiry,
// logged-out state, the current token, and one section per superseded
// record keyed by the token, each with state, last attempt, and recovery.
func entrySection(s config.Server) []field {
	var out []field
	if s.Provenance != "" {
		out = append(out, field{"provenance", string(s.Provenance)})
	}
	if s.TokenUserID != "" {
		out = append(out, field{"tokenUserId", s.TokenUserID})
	}
	if s.CredentialExpiresAt != "" {
		out = append(out, field{"credentialExpiresAt", s.CredentialExpiresAt})
	}
	if s.LoggedOutAt != "" {
		out = append(out, field{"loggedOutAt", s.LoggedOutAt}, field{"credential", "no stored credential"})
	}
	switch {
	case s.CLIOwned():
		out = append(out, field{"currentToken", tokenSection(s.URL, *s.TokenRef, s.TokenUserID, s.CredentialExpiresAt)})
	case s.CLICreated():
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
				sec = append(sec, field{"userId", r.UserID})
			}
			if r.ExpiresAt != "" {
				sec = append(sec, field{"expiresAt", r.ExpiresAt})
			}
			sec = append(sec, field{"supersededAt", r.SupersededAt}, field{"state", string(config.RecordOwnerState(r, s.TokenUserID))})
			if r.LastAttempt != nil {
				sec = append(sec, field{"lastAttempt", string(r.LastAttempt.Result) + " (" + r.LastAttempt.Error + ")"})
			}
			sec = append(sec, recoveryField(s.URL, r.UserID, r.Ref))
			recs = append(recs, field{r.Ref.String(), sec})
		}
		out = append(out, field{"supersededTokens", recs})
	}
	return out
}

// tokenSection renders an entry's current token: ref, owner, expiry, then
// the recovery sequence that revokes it.
func tokenSection(serverURL string, ref clierr.TokenRef, userID, expiresAt string) []field {
	sec := []field{{"ref", ref.String()}}
	if userID != "" {
		sec = append(sec, field{"userId", userID})
	}
	if expiresAt != "" {
		sec = append(sec, field{"expiresAt", expiresAt})
	}
	return append(sec, recoveryField(serverURL, userID, ref))
}

// recoveryField is a recovery sequence with the assumption its commands
// depend on as a trailing line, the same sentence delete-server and
// set-token-reuse print. Text only: the JSON side is entryReport, whose
// recovery value stays the commands alone.
func recoveryField(serverURL, userID string, ref clierr.TokenRef) field {
	seq := clierr.RecoverySequence(serverURL, userID, ref)
	return field{"recovery", append(seq, "("+clierr.RecoverySequenceAssumption+")")}
}

// configInfoSectionHeaders is the exact line config status always emits for
// its nested sections: the completion block is unconditional (see
// Execute's field list above), so "completion:" needs no presence check;
// "servers:" appears only when there are CLI-owned tokens.
var configInfoSectionHeaders = map[string]bool{"completion:": true, "servers:": true}

// styleConfigInfoText applies config status's bold span to text, the plain
// rendering built above: the "completion:" and "servers:" section labels,
// bold, whole line. Every flat "key: value" line stays plain.
func styleConfigInfoText(text string, s style.Style) string {
	return runtime.BoldExactLines(text, configInfoSectionHeaders, s)
}
