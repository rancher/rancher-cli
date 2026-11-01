package cli

import (
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// assertNameRefused asserts a run ended with VALIDATION at exit code 2, with
// a message that names the code point, before any request reached the server
// and with no config file and no credential written in dir.
func assertNameRefused(t *testing.T, dir string, srv *factsServer, got recordedRun, codePoint string) {
	t.Helper()
	require.NotNil(t, got.res.err, "stderr: %s", got.stderr)
	assert.Equal(t, clierr.CodeValidation, got.res.err.Code)
	assert.Equal(t, 2, got.res.code)
	require.ErrorContains(t, got.res.err, codePoint)
	assert.Empty(t, srv.paths(), "no request reached the server")
	_, err := os.Stat(config.FilePath(dir))
	require.ErrorIs(t, err, os.ErrNotExist, "no config file is written")
	_, err = os.Stat(config.CredentialsFilePath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist, "no credential is written")
}

// TestNewServerNameWithAnInvisibleOrControlCharacterIsRefusedBeforeAnyRequest
// asserts config add-server and auth login refuse a new name a terminal
// prints as another name, before any request, so no token is created on the
// server, with nothing written; and that a name derived from a URL host is
// refused the same way.
func TestNewServerNameWithAnInvisibleOrControlCharacterIsRefusedBeforeAnyRequest(t *testing.T) {
	rows := []struct {
		label     string
		codePoint string
		argv      func(srv *factsServer) []string
		stdin     string
	}{
		{"add-server, an invisible separator ahead of a dash", "U+2063", func(srv *factsServer) []string {
			return []string{"config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", "\u2063-q"}
		}, ""},
		{"add-server, a zero width space", "U+200B", func(srv *factsServer) []string {
			return []string{"config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", "\u200bqa1"}
		}, ""},
		{"add-server, a newline", "U+000A", func(srv *factsServer) []string {
			return []string{"config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", "qa1\nx"}
		}, ""},
		{"login with --url, a direction override", "U+202E", func(srv *factsServer) []string {
			return []string{"auth", "login", "--url", srv.URL, "--token-stdin", "--", "\u202ex"}
		}, pastedToken + "\n"},
		{"login with --url, a name derived from the host", "U+200B", func(*factsServer) []string {
			return []string{"auth", "login", "--url", "https://a\u200bb.invalid", "--token-stdin"}
		}, pastedToken + "\n"},
	}
	for _, row := range rows {
		t.Run(row.label, func(t *testing.T) {
			dir, srv := loginEnv(t)
			got := runRecordedStdin(t, newRoot(t), row.stdin, row.argv(srv)...)
			assertNameRefused(t, dir, srv, got, row.codePoint)
		})
	}
}

// TestNewServerNameThatPrintsAsItselfIsRegistered asserts a leading dash and
// a name with a space still register.
func TestNewServerNameThatPrintsAsItselfIsRegistered(t *testing.T) {
	for _, name := range []string{"-q", "a b"} {
		t.Run(name, func(t *testing.T) {
			dir, srv := loginEnv(t)
			got := runRecorded(t, newRoot(t), "config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", name)
			require.Zero(t, got.res.code, got.stderr)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			assert.Contains(t, cfg.Servers, name)
		})
	}
}

// seedStoredEntry writes a config whose one server entry has the given name,
// at the server's URL with its CA, as a name made before the refusal existed.
func seedStoredEntry(t *testing.T, dir string, srv *factsServer, name string) {
	t.Helper()
	s := config.Server{URL: srv.canonical(t), CA: srv.ca}
	require.NoError(t, config.AdvanceCacheBinding(&s))
	cfg := &config.Config{CurrentServer: name, Servers: map[string]config.Server{name: s}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// loginPathsOfStoredEntry seeds an entry named name, logs it in with a piped
// token and returns the paths the server saw, sorted.
func loginPathsOfStoredEntry(t *testing.T, name string) []string {
	t.Helper()
	dir, srv := loginEnv(t)
	seedStoredEntry(t, dir, srv, name)
	got := runRecordedStdin(t, newRoot(t), pastedToken+"\n", "auth", "login", "--token-stdin", "--", name)
	require.Zero(t, got.res.code, got.stderr)
	paths := srv.paths()
	slices.Sort(paths)
	return paths
}

// TestAServerNameAlreadyStoredWithAnInvisibleCharacterKeepsWorking asserts an
// entry already stored under a name with an invisible character is still
// logged in by name, with the requests an ordinary name makes, and by --url
// at its own server, that add-server answers it is unchanged, and that
// use-server, logout and delete-server of it succeed.
func TestAServerNameAlreadyStoredWithAnInvisibleCharacterKeepsWorking(t *testing.T) {
	stored := "qa\u200b1"

	t.Run("login by name sends the requests of an ordinary name", func(t *testing.T) {
		ordinary := loginPathsOfStoredEntry(t, "qa1")
		assert.Equal(t, ordinary, loginPathsOfStoredEntry(t, stored))
	})

	t.Run("login by name and --url at its own server", func(t *testing.T) {
		dir, srv := loginEnv(t)
		seedStoredEntry(t, dir, srv, stored)
		got := runRecordedStdin(t, newRoot(t), pastedToken+"\n", "auth", "login", "--url", srv.URL, "--token-stdin", "--", stored)
		require.Zero(t, got.res.code, got.stderr)
		assert.NotEmpty(t, srv.paths())
	})

	t.Run("add-server of it is unchanged", func(t *testing.T) {
		dir, srv := loginEnv(t)
		seedStoredEntry(t, dir, srv, stored)
		got := runRecorded(t, newRoot(t), "config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", stored)
		require.Zero(t, got.res.code, got.stderr)
		assert.Contains(t, got.stdout, "unchanged")
	})

	t.Run("use-server, logout and delete-server succeed", func(t *testing.T) {
		dir, srv := loginEnv(t)
		seedStoredEntry(t, dir, srv, stored)
		got := runRecordedStdin(t, newRoot(t), pastedToken+"\n", "auth", "login", "--token-stdin", "--", stored)
		require.Zero(t, got.res.code, got.stderr)
		for _, argv := range [][]string{
			{"config", "use-server", "--", stored},
			{"auth", "logout", "--", stored},
			{"config", "delete-server", "--", stored},
		} {
			got = runRecorded(t, newRoot(t), argv...)
			require.Zero(t, got.res.code, "%v: %s", argv, got.stderr)
		}
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		assert.NotContains(t, cfg.Servers, stored)
	})

	t.Run("a new name with the character at the same server sends no request", func(t *testing.T) {
		dir, srv := loginEnv(t)
		seedStoredEntry(t, dir, srv, stored)
		got := runRecorded(t, newRoot(t), "config", "add-server", "--url", srv.URL, "--cacert", srv.ca, "--", "qa\u200b2")
		require.NotNil(t, got.res.err, got.stderr)
		assert.Equal(t, clierr.CodeValidation, got.res.err.Code)
		assert.Empty(t, srv.paths())
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		assert.Len(t, cfg.Servers, 1)
	})
}

// TestAuthLoginCompletionOffersNothingForANewNameWithAnInvisibleCharacter
// asserts auth login completes --auth-provider for a typed new name that is
// fine and for none that holds an invisible character, which the command
// refuses, and sends no request for it.
// NOT parallel: the environment and the stored config are process-wide.
func TestAuthLoginCompletionOffersNothingForANewNameWithAnInvisibleCharacter(t *testing.T) {
	t.Run("an ordinary new name completes", func(t *testing.T) {
		srv := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{}, nil, "auth", "login", "qa1", "--url", srv.srv.URL)
		assert.Equal(t, authConfigNames, got)
	})
	t.Run("a new name with a zero width space gives nothing", func(t *testing.T) {
		srv := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{}, nil, "auth", "login", "qa\u200b1", "--url", srv.srv.URL)
		assert.Empty(t, got)
		assert.Empty(t, srv.served())
	})
	t.Run("a stored name with a zero width space completes", func(t *testing.T) {
		srv := newAuthListServer(t, false)
		cfg := &config.Config{Servers: map[string]config.Server{"qa\u200b1": {URL: srv.srv.URL}}}
		got, _ := completeAuthProvider(t, cfg, nil, "auth", "login", "qa\u200b1")
		assert.Equal(t, authConfigNames, got)
	})
}
