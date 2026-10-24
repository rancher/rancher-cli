package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestWriteThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := FilePath(dir)
	c := &Config{
		CurrentServer: "acme",
		Servers:       map[string]Server{"acme": {URL: "https://rancher.example.com", CA: "PEM"}},
		path:          path,
	}
	require.NoError(t, c.Write())

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (fsacl.Harden is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.CurrentServer)
	assert.Equal(t, "https://rancher.example.com", got.Servers["acme"].URL)
}

func TestReadOnlyFieldRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := FilePath(dir)
	c := &Config{ReadOnly: true, Servers: map[string]Server{}, path: path}
	require.NoError(t, c.Write())

	got, err := Load(path)
	require.NoError(t, err)
	assert.True(t, got.ReadOnly)
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Empty(t, got.Servers)
}

func TestCurrentServerEntryUnsetIsNotLoggedIn(t *testing.T) {
	_, _, err := (&Config{}).CurrentServerEntry()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

// TestCompletionTimeoutWithRaw asserts the floor/cap/default contract for
// CompletionTimeoutWithRaw, and indirectly EffectiveCompletionTimeout (a
// thin wrapper).
func TestCompletionTimeoutWithRaw(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		effective time.Duration
		diverges  bool
	}{
		{"empty uses default", "", CompletionTimeoutDefault, false},
		{"in-range is unchanged", "1s", time.Second, false},
		{"non-canonical equal suppressed", "1000ms", time.Second, false},
		{"at cap is unchanged", "2s", CompletionTimeoutMax, false},
		{"at floor is unchanged", "500ms", CompletionTimeoutDefault, false},
		{"over cap is clamped", "10s", CompletionTimeoutMax, true},
		{"unparseable falls to default", "bad", CompletionTimeoutDefault, true},
		{"zero falls to default", "0", CompletionTimeoutDefault, true},
		{"negative falls to default", "-1s", CompletionTimeoutDefault, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Completion: CompletionConfig{Timeout: tc.raw}}
			eff, raw, div := CompletionTimeoutWithRaw(cfg)
			assert.Equal(t, tc.effective, eff)
			assert.Equal(t, tc.raw, raw)
			assert.Equal(t, tc.diverges, div)
			// EffectiveCompletionTimeout must agree - it is now a thin wrapper.
			assert.Equal(t, tc.effective, EffectiveCompletionTimeout(cfg))
		})
	}

	t.Run("nil config uses default no divergence", func(t *testing.T) {
		eff, raw, div := CompletionTimeoutWithRaw(nil)
		assert.Equal(t, CompletionTimeoutDefault, eff)
		assert.Equal(t, "", raw)
		assert.False(t, div)
	})
}

func TestResolveDirPrecedence(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "/env/dir")
		got, err := ResolveDir("/flag/dir")
		require.NoError(t, err)
		assert.Equal(t, "/flag/dir", got)
	})
	t.Run("env over xdg", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "/env/dir")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, "/env/dir", got)
	})
	t.Run("xdg when no env", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/xdg", "rancher"), got)
	})
}

// TestServerMetadataRoundTrips asserts the login-credential metadata a
// server entry can carry survives Write and Load field for field, that an
// entry without it reads back with zero values, and that the setting is
// omitted from the file when off.
func TestServerMetadataRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{
		"plain": {URL: "https://a.ex"},
		"exec-login-b.ex": {URL: "https://b.ex", Provenance: clierr.ProvenanceExecLogin,
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-h2qcs"}, TokenUserID: "user-tx66l",
			CredentialExpiresAt: "2026-09-25T10:00:00Z", LoggedOutAt: "2026-09-26T00:00:00Z",
			SupersededTokens: []SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-2ww4s"},
				UserID: "user-tx66l", ExpiresAt: "2026-09-20T08:00:00Z", SupersededAt: "2026-09-04T18:00:00Z",
				LastAttempt: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "503 from the server"}}}},
	}, TokenReuse: true}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	got, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, cfg.Servers["exec-login-b.ex"], got.Servers["exec-login-b.ex"])
	assert.Equal(t, Server{URL: "https://a.ex"}, got.Servers["plain"])
	assert.True(t, got.TokenReuse)

	raw, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "tokenUserId: user-tx66l")
	assert.Contains(t, string(raw), "lastAttempt:")
	assert.NotContains(t, string(raw), "provenance: \"\"", "absent metadata is omitted, never written empty")

	off := &Config{Servers: map[string]Server{}}
	off.SetPath(FilePath(t.TempDir()))
	require.NoError(t, off.Write())
	raw, err = os.ReadFile(off.Path())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "tokenReuse", "the setting is omitted when off")
}

// TestServerCLICreatedOwnedAndHoldsCLITokens asserts which entries the
// durable surfaces list: a CLI-created provenance (with or without a named
// token), or any superseded record; a user-supplied or pre-provenance entry
// with no records is neither. CLIOwned additionally requires the name, and
// records never make a pasted token read as CLI-created.
func TestServerCLICreatedOwnedAndHoldsCLITokens(t *testing.T) {
	t.Parallel()
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}
	rec := []SupersededToken{{Ref: *ref, SupersededAt: "2026-09-04T18:00:00Z"}}
	cases := []struct {
		name                  string
		s                     Server
		created, owned, holds bool
	}{
		{"pre-provenance", Server{URL: "u"}, false, false, false},
		{"userSupplied", Server{URL: "u", Provenance: clierr.ProvenanceUserSupplied}, false, false, false},
		{"userSupplied with records", Server{URL: "u", Provenance: clierr.ProvenanceUserSupplied, TokenRef: ref, SupersededTokens: rec}, false, false, true},
		{"derived", Server{URL: "u", Provenance: clierr.ProvenanceDerived, TokenRef: ref}, true, true, true},
		{"legacySession", Server{URL: "u", Provenance: clierr.ProvenanceLegacySession, TokenRef: ref}, true, true, true},
		{"execLogin", Server{URL: "u", Provenance: clierr.ProvenanceExecLogin, TokenRef: ref}, true, true, true},
		{"derived without ref", Server{URL: "u", Provenance: clierr.ProvenanceDerived}, true, false, true},
		{"records only", Server{URL: "u", SupersededTokens: rec}, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, c.created, c.s.CLICreated())
			assert.Equal(t, c.owned, c.s.CLIOwned())
			assert.Equal(t, c.holds, c.s.HoldsCLITokens())
		})
	}
}

// TestServerCloneIsDeep asserts a cloned entry shares no record storage
// with its source, so a snapshot survives the source's later mutation.
func TestServerCloneIsDeep(t *testing.T) {
	t.Parallel()
	src := Server{URL: "u", SupersededTokens: []SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "a"}}}}
	c := src.Clone()
	src.SupersededTokens[0].Ref.Name = "changed"
	src.SupersededTokens = append(src.SupersededTokens, SupersededToken{})
	assert.Equal(t, "a", c.SupersededTokens[0].Ref.Name)
	assert.Len(t, c.SupersededTokens, 1)
}

// TestRecordOwnerState asserts the display-time derivation: same owner
// only when both sides are non-empty and equal; an empty owner on either
// side is unknown, never different.
func TestRecordOwnerState(t *testing.T) {
	t.Parallel()
	rec := func(u string) SupersededToken { return SupersededToken{UserID: u} }
	assert.Equal(t, OwnerSame, RecordOwnerState(rec("user-a"), "user-a"))
	assert.Equal(t, OwnerDifferent, RecordOwnerState(rec("user-a"), "user-b"))
	assert.Equal(t, OwnerUnknown, RecordOwnerState(rec(""), "user-b"))
	assert.Equal(t, OwnerUnknown, RecordOwnerState(rec("user-a"), ""))
	assert.Equal(t, OwnerUnknown, RecordOwnerState(rec(""), ""))
}

// TestTelemetryKeyWrites asserts Write keeps the telemetry key as the text
// it holds: "true" and "false" as quoted texts, an invalid text unchanged,
// and nothing for an absent key, whatever else is written.
func TestTelemetryKeyWrites(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		value  *string
		want   any
		absent bool
	}{
		{name: "absent", absent: true},
		{name: "true", value: text("true"), want: "true"},
		{name: "false", value: text("false"), want: "false"},
		{name: "invalid", value: text("maybe"), want: "maybe"},
		{name: "empty", value: text(""), want: ""},
		{name: "spelled off", value: text("off"), want: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := FilePath(t.TempDir())
			c := &Config{Telemetry: tc.value, TokenReuse: true, Servers: map[string]Server{}, path: path}
			require.NoError(t, c.Write())

			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &decoded))
			got, present := decoded["telemetry"]
			assert.Equal(t, !tc.absent, present)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, true, decoded["tokenReuse"])

			loaded, err := Load(path)
			require.NoError(t, err)
			if tc.absent {
				assert.Nil(t, loaded.Telemetry)
				return
			}
			require.NotNil(t, loaded.Telemetry)
			assert.Equal(t, tc.want, *loaded.Telemetry)

			require.NoError(t, loaded.Write())
			again, err := Load(path)
			require.NoError(t, err)
			require.NotNil(t, again.Telemetry)
			assert.Equal(t, tc.want, *again.Telemetry, "a second write keeps the text")
		})
	}
}
