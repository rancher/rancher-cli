package configcmd

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

func execConfigInfoResult(t *testing.T, dir string) runtime.Result {
	t.Helper()
	res, err := NewConfigInfo(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ConfigInfo", res.Kind)
	return res
}

func execConfigInfo(t *testing.T, dir string) map[string]any {
	t.Helper()
	return execConfigInfoResult(t, dir).Data.(map[string]any)
}

// assertCompletion extracts the nested completion map from res, asserts the
// nested JSON structure, guards against both dead key shapes (flat dotted and
// camelCase), and verifies the text indentation header. Called by every
// completion sub-test so the contract holds for all cases.
func assertCompletion(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	data := res.Data.(map[string]any)

	c, ok := data["completion"].(map[string]any)
	require.True(t, ok, "completion must be a nested map[string]any")

	// Regression guard: the completion fields must appear only in the nested
	// map, never as flat-dotted (completion.network etc.) or camelCase
	// (completionNetwork etc.) top-level keys.
	for _, dead := range []string{
		"completion.network", "completion.timeout", "completion.timeoutRaw",
		"completionNetwork", "completionTimeout", "completionTimeoutRaw",
	} {
		_, present := data[dead]
		assert.False(t, present, "%q must not appear as a top-level key (nested shape required)", dead)
	}

	// Text must open the indented section.
	assert.Contains(t, res.Text, "completion:\n")

	return c
}

// TestStyleConfigInfoTextStripInvariant asserts config status's bold span: the
// "completion:" section label is bold, whole line; every flat "key: value"
// line stays plain - plus the strip invariant and the inactive-style
// identity.
func TestStyleConfigInfoTextStripInvariant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := execConfigInfoResult(t, dir)
	enabled := style.Style{Enabled: true}

	styled := styleConfigInfoText(res.Text, enabled)
	assert.NotEqual(t, res.Text, styled)
	assert.Equal(t, res.Text, style.Strip(styled))
	assert.Equal(t, res.Text, styleConfigInfoText(res.Text, style.Style{}), "disabled style returns text unchanged")
	assert.Contains(t, styled, "\x1b[1mcompletion:\x1b[0m\n")
	assert.NotContains(t, styled, "\x1b[1mpath:", "a flat key: value line must not be styled")
}

// TestConfigInfoCompletionBlock asserts the completion section as a nested object
// in JSON data and an indented block in text. Both views are asserted for every
// scenario (auth-check precedent: parity is guaranteed by tests, not a shared
// loop). No t.Parallel(): t.Setenv.
func TestConfigInfoCompletionBlock(t *testing.T) {
	t.Run("defaults when no completion config set", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, false, c["network"])
		assert.Equal(t, "500ms", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when no config is set")

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 500ms\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("configured network true and in-range timeout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Network = true
		cfg.Completion.Timeout = "1s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, true, c["network"])
		assert.Equal(t, "1s", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when raw equals effective")

		assert.Contains(t, res.Text, "  network: true\n")
		assert.Contains(t, res.Text, "  timeout: 1s\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("clamped timeout reports both effective and raw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "10s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, false, c["network"])
		assert.Equal(t, "2s", c["timeout"])
		assert.Equal(t, "10s", c["timeoutRaw"])

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 2s\n")
		assert.Contains(t, res.Text, "  timeoutRaw: 10s\n")
	})

	t.Run("non-canonical but equal raw suppresses timeoutRaw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "1000ms" // same as 1s after parse
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, "1s", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when raw parses to the effective duration")

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 1s\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("unparseable raw surfaces as timeoutRaw with default effective", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "malformed"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, "500ms", c["timeout"])
		assert.Equal(t, "malformed", c["timeoutRaw"])

		assert.Contains(t, res.Text, "  timeout: 500ms\n")
		assert.Contains(t, res.Text, "  timeoutRaw: malformed\n")
	})
}

// TestConfigInfoEffectiveStoreSources asserts the env > config > default
// resolution the command exists to make visible (an env-supplied store
// is invisible in config.yaml and list-servers).
// No t.Parallel(): t.Setenv.
func TestConfigInfoEffectiveStoreSources(t *testing.T) {
	t.Run("a variable that disagrees with the declaration is reported, not applied", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "exec:vault")

		data := execConfigInfo(t, dir)
		// The directory's own answer is what the report shows: this
		// command never opens a store, so it can show both sides
		// rather than failing the way every credential path does.
		assert.Equal(t, "file", data["credentialStore"])
		assert.Equal(t, "config", data["credentialStoreSource"])
		assert.Equal(t, true, data["exists"])
		conflict, ok := data["credentialStoreConflict"].(string)
		require.True(t, ok, "the conflict is reported")
		assert.Contains(t, conflict, `"file"`)
		assert.Contains(t, conflict, `"exec:vault"`)
		assert.Contains(t, conflict, config.EnvCredentialStore)
	})

	t.Run("a variable that agrees with the declaration is no conflict", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "file")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "file", data["credentialStore"])
		assert.Equal(t, "env", data["credentialStoreSource"], "the variable is set and is what applies")
		_, ok := data["credentialStoreConflict"]
		assert.False(t, ok)
	})

	t.Run("a variable over a directory that declares nothing applies", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "file")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "file", data["credentialStore"])
		assert.Equal(t, "env", data["credentialStoreSource"])
		_, ok := data["credentialStoreConflict"]
		assert.False(t, ok)
	})

	t.Run("config file value with no env", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "file", data["credentialStore"])
		assert.Equal(t, "config", data["credentialStoreSource"])
	})

	t.Run("default auto with pin, missing config file", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		require.NoError(t, config.WritePin(dir, "file"))

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		assert.Equal(t, "default", data["credentialStoreSource"])
		assert.Equal(t, "file", data["pin"])
		assert.Equal(t, false, data["exists"])
		assert.Equal(t, config.FilePath(dir), data["path"])
		_, ok := data["credentialStoreDefault"]
		assert.False(t, ok, "once pinned, the platform default is moot - the pin is the actual answer")
	})

	t.Run("default auto with no pin yet names the platform default, read-only", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		_, pinned := data["pin"]
		assert.False(t, pinned, "no pin exists yet - reporting one would imply a probe ran")
		assert.Equal(t, config.PlatformKeychainLabel(), data["credentialStoreDefault"])
		_, err := os.Stat(config.PinPath(dir))
		assert.True(t, os.IsNotExist(err), "config status must never write the pin itself")
	})

	t.Run("explicit policy hides a stale auto pin", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "keychain"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		require.NoError(t, config.WritePin(dir, "file"))
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "keychain", data["credentialStore"])
		_, ok := data["pin"]
		assert.False(t, ok, "a stale auto pin must not be reported under an explicit policy")
	})

	t.Run("env-supplied auto still reports the pin", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "auto")
		require.NoError(t, config.WritePin(dir, "keychain"))

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		assert.Equal(t, "env", data["credentialStoreSource"])
		assert.Equal(t, "keychain", data["pin"])
	})

	t.Run("no pin omits the field", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		data := execConfigInfo(t, dir)
		_, ok := data["pin"]
		assert.False(t, ok, "absent pin must not appear as an empty field")
	})
}

// TestConfigInfoReportsTokenReuseAndCLIOwnedEntries asserts the setting is
// always reported and that every entry with a CLI-owned current token or
// superseded records gets a section with owner, expiry, the records with
// their derived state, last attempt, and recovery sequence; a logged-out
// entry's own token is listed the same way; a user-supplied entry with
// superseded records lists the records without a current-token note;
// entries without CLI tokens have no section.
func TestConfigInfoReportsTokenReuseAndCLIOwnedEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"plain":         {URL: "https://p.ex"},
		"user-supplied": {URL: "https://u.ex", Provenance: clierr.ProvenanceUserSupplied},
		"user-supplied-records": {URL: "https://us.ex", Provenance: clierr.ProvenanceUserSupplied,
			SupersededTokens: []config.SupersededToken{
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, UserID: "user-c", SupersededAt: "2026-09-01T00:00:00Z"},
			}},
		"exec-login-r.ex": {URL: "https://r.ex:8443", Provenance: clierr.ProvenanceExecLogin,
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live"}, TokenUserID: "user-a", CredentialExpiresAt: "2026-10-01T00:00:00Z",
			SupersededTokens: []config.SupersededToken{
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-b"}, UserID: "user-b", ExpiresAt: "2026-09-20T00:00:00Z", SupersededAt: "2026-09-02T00:00:00Z"},
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-x"}, UserID: "user-a", SupersededAt: "2026-09-03T00:00:00Z",
					LastAttempt: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "503"}},
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-nobody"}, SupersededAt: "2026-09-04T00:00:00Z"},
			}},
		"prod": {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-out"},
			TokenUserID: "user-a", LoggedOutAt: "2026-09-04T12:00:00Z"},
		"noref": {URL: "https://n.ex", Provenance: clierr.ProvenanceDerived},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res := execConfigInfoResult(t, dir)
	data := res.Data.(map[string]any)
	assert.Equal(t, true, data["tokenReuse"])
	servers := data["servers"].(map[string]any)
	assert.Len(t, servers, 4)
	noref := servers["noref"].(map[string]any)
	assert.Equal(t, "cannot revoke by name: no tokenRef recorded", noref["currentToken"].(map[string]any)["note"])
	_, hasPlain := servers["plain"]
	assert.False(t, hasPlain)
	_, hasUser := servers["user-supplied"]
	assert.False(t, hasUser)

	// A user-supplied token is never CLI-created: the entry is listed for
	// its superseded records only, with no current-token section and no
	// "cannot revoke by name" note.
	us := servers["user-supplied-records"].(map[string]any)
	_, hasCurrent := us["currentToken"]
	assert.False(t, hasCurrent)
	usRecs := us["supersededTokens"].([]map[string]any)
	require.Len(t, usRecs, 1)
	assert.Equal(t, "token-old", usRecs[0]["ref"].(map[string]any)["name"])
	assert.Equal(t, 1, strings.Count(res.Text, "cannot revoke by name"), "only the noref entry carries the note")

	e := servers["exec-login-r.ex"].(map[string]any)
	assert.Equal(t, "execLogin", e["provenance"])
	assert.Equal(t, "user-a", e["tokenUserId"])
	assert.Equal(t, "2026-10-01T00:00:00Z", e["credentialExpiresAt"])
	recs := e["supersededTokens"].([]map[string]any)
	require.Len(t, recs, 3)
	assert.Equal(t, "differentOwner", recs[0]["state"])
	assert.Equal(t, "sameOwner", recs[1]["state"])
	assert.Equal(t, "failed", recs[1]["lastAttempt"].(map[string]any)["result"])
	assert.Equal(t, "unknownOwner", recs[2]["state"])
	assert.Equal(t, []string{"rancher delete management-token kubeconfig-u-x --apply --yes --allow-deprecated"}, recs[1]["recovery"],
		"a v3 token's recovery is the administrator's delete alone")
	ext := recs[0]["recovery"].([]string)
	assert.Equal(t, "rancher auth login 'recover-r.ex-8443' --url 'https://r.ex:8443'   # log in as user 'user-b'", ext[0])

	p := servers["prod"].(map[string]any)
	assert.Equal(t, "2026-09-04T12:00:00Z", p["loggedOutAt"])
	assert.Equal(t, "no stored credential", p["credential"])
	cur := p["currentToken"].(map[string]any)
	assert.Equal(t, "token-out", cur["ref"].(map[string]any)["name"])
	assert.NotEmpty(t, cur["recovery"], "a logged-out entry's own token is listed like a superseded record")

	// Text: two spaces per level. servers → entry → supersededTokens →
	// one section per record keyed by the token → its fields; recovery
	// commands are list items one level under "recovery:".
	assert.Contains(t, res.Text, "tokenReuse: true\n")
	assert.Contains(t, res.Text, "servers:\n  exec-login-r.ex:\n    provenance: execLogin\n")
	assert.Contains(t, res.Text, "    supersededTokens:\n      ext/token-b:\n        userId: user-b\n")
	assert.Contains(t, res.Text, "        state: differentOwner\n")
	assert.Contains(t, res.Text, "        recovery:\n          rancher auth login 'recover-r.ex-8443' --url 'https://r.ex:8443'   # log in as user 'user-b'\n          rancher config use-server 'recover-r.ex-8443'\n")
	assert.Contains(t, res.Text, "      v3/kubeconfig-u-x:\n        userId: user-a\n")
	assert.Contains(t, res.Text, "        recovery:\n          an administrator logged in to https://r.ex:8443 can delete it with: rancher delete management-token kubeconfig-u-x --apply --yes --allow-deprecated\n")
	assert.Contains(t, res.Text, "  prod:\n")
	assert.Contains(t, res.Text, "    credential: no stored credential\n")
	assert.Contains(t, res.Text, "  noref:\n    provenance: derived\n    currentToken:\n      note: cannot revoke by name: no tokenRef recorded\n")
}

// TestConfigInfoReportsWhereAnUnpinnedDirectoryKeepsItsCredentials asserts
// `config status` gives the answer every reader of an `auto` directory
// with no pin gets, without probing or pinning: the store the entries'
// stored credentials are in, the both-stores error as a field rather than
// a failure, and the platform default only when nothing is stored.
func TestConfigInfoReportsWhereAnUnpinnedDirectoryKeepsItsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name             string
		file, keychain   map[string]string
		inUse, errSubstr string
		platformDefault  bool
	}{
		{name: "nothing stored", platformDefault: true},
		{name: "file tokens", file: map[string]string{"prod": "a"}, inUse: "file"},
		{name: "identity records", keychain: map[string]string{"prod": "a"}, inUse: "keychain"},
		{name: "both", file: map[string]string{"staging": "b"}, keychain: map[string]string{"prod": "a"},
			errSubstr: "credentials in the keychain (prod) and in the plaintext file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(config.EnvCredentialStore, "")
			dir := t.TempDir()
			cmdtest.SeedUnpinned(t, dir, tc.file, tc.keychain)

			data := execConfigInfo(t, dir)
			assert.Equal(t, "auto", data["credentialStore"])
			if tc.inUse != "" {
				assert.Equal(t, tc.inUse, data["credentialStoreInUse"])
			} else {
				assert.NotContains(t, data, "credentialStoreInUse")
			}
			if tc.errSubstr != "" {
				assert.Contains(t, data["credentialStoreError"], tc.errSubstr)
			} else {
				assert.NotContains(t, data, "credentialStoreError")
			}
			if tc.platformDefault {
				assert.Equal(t, config.PlatformKeychainLabel(), data["credentialStoreDefault"])
			} else {
				assert.NotContains(t, data, "credentialStoreDefault", "something is stored, so the default is moot")
			}
			assert.NoFileExists(t, config.PinPath(dir), "config status never writes the pin")
		})
	}
}

// TestSetCompletionNoteOnAnUnpinnedDirectory asserts the capability note
// `config set-completion --network true` gives an `auto` directory with no
// pin: the answer every reader gets, with no probe and no pin, and "log
// in" as the way to settle a directory that has stored nothing, since a
// read no longer pins.
func TestSetCompletionNoteOnAnUnpinnedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name           string
		file, keychain map[string]string
		want           string
	}{
		{name: "nothing stored", want: "(log in to pin it)"},
		{name: "file tokens", file: map[string]string{"prod": "a"}, want: "full capability (file credential store)"},
		{name: "identity records", keychain: map[string]string{"prod": "a"}, want: "needs RANCHER_CLI_TOKEN or the file credential store"},
		{name: "both", file: map[string]string{"staging": "b"}, keychain: map[string]string{"prod": "a"},
			want: "live completion enabled, but this config directory has credentials in the keychain (prod)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(config.EnvCredentialStore, "")
			t.Setenv(config.EnvToken, "")
			dir := t.TempDir()
			cmdtest.SeedUnpinned(t, dir, tc.file, tc.keychain)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)

			assert.Contains(t, completionCapabilityNote(dir, cfg), tc.want)
			assert.NoFileExists(t, config.PinPath(dir))
		})
	}
}

// TestConfigInfoShowsTheRecordedAuthConfig asserts config info lists the
// auth config an entry's login recorded as tokenAuthConfig beside
// tokenUserId, in the data and in the text, and nothing for an entry that
// recorded none.
func TestConfigInfoShowsTheRecordedAuthConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a", TokenAuthConfig: "corp-ldap",
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live"}},
		"exec-login-r.ex-2": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a",
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live-2"}},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res := execConfigInfoResult(t, dir)
	servers := res.Data.(map[string]any)["servers"].(map[string]any)
	with := servers["exec-login-r.ex"].(map[string]any)
	assert.Equal(t, "user-a", with["tokenUserId"])
	assert.Equal(t, "corp-ldap", with["tokenAuthConfig"])
	without := servers["exec-login-r.ex-2"].(map[string]any)
	assert.NotContains(t, without, "tokenAuthConfig")
	assert.Equal(t, 1, strings.Count(res.Text, "tokenAuthConfig: corp-ldap"), res.Text)
}
