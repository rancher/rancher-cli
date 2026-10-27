package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// directoryCommandRE matches a rancher command that depends on the config
// directory, capturing what follows the word rancher.
var directoryCommandRE = sync.OnceValue(func() *regexp.Regexp {
	words, err := remedytest.DirectoryCommandWords(remedyGolden)
	if err != nil {
		panic(err)
	}
	return regexp.MustCompile(`\brancher( --config '[^']*')? (?:` + words + `)(?:[^A-Za-z0-9_-]|$)`)
})

// remedyGolden is the manifest golden, found before any test changes the
// working directory.
var remedyGolden, _ = filepath.Abs(filepath.Join("testdata", "schema.golden.json"))

// runRemedyCLI runs the real command tree from the working directory with
// the given arguments and returns stderr.
func runRemedyCLI(t *testing.T, args ...string) string {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	_ = root.Run(t.Context(), append([]string{"rancher"}, args...))
	return errOut.String()
}

// writeRemedyConfig writes a config.yaml into dir declaring the file store.
func writeRemedyConfig(t *testing.T, dir string, cfg *config.Config) {
	t.Helper()
	cfg.CredentialStore = "file"
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// withDirectory is s with each remedy command "rancher ..." written as a
// run against dir prints it, with --config and the directory, shell-quoted,
// after the word rancher.
func withDirectory(dir, s string) string {
	return strings.ReplaceAll(s, "rancher ", "rancher --config "+clierr.ShellQuote(dir)+" ")
}

// assertEveryCommandNames asserts s holds at least one directory command
// and that every one of them carries want (the --config part, "" for
// none).
func assertEveryCommandNames(t *testing.T, s, want string) {
	t.Helper()
	assert.NotContains(t, s, clierr.RemedyMarker, "no marker reaches the terminal")
	all := directoryCommandRE().FindAllStringSubmatch(s, -1)
	require.NotEmpty(t, all, "no command in %q", s)
	for _, m := range all {
		assert.Equal(t, want, m[1], "%q in %q", m[0], s)
	}
}

// TestTheExecPluginAndKubeconfigRemediesNameTheirDirectory asserts the
// real error paths of auth get-token and kubeconfig, run with a relative
// --config, name that directory, absolute and shell-quoted, in every
// command they print - message, hint and JSON alike - while the legacy
// rancher token command prints its plain and another command names its
// own directory.
func TestTheExecPluginAndKubeconfigRemediesNameTheirDirectory(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(config.EnvConfigDir, "")
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	named := " --config '" + filepath.Join(realWD, "acct") + "'"

	t.Run("kubeconfig with no server", func(t *testing.T) {
		require.NoError(t, os.RemoveAll("acct"))
		got := runRemedyCLI(t, "--config", "acct", "kubeconfig", "--cluster", "c-1")
		assert.Contains(t, got, "error (NOT_LOGGED_IN)")
		assertEveryCommandNames(t, got, named)
	})

	t.Run("kubeconfig for an entry with no login", func(t *testing.T) {
		require.NoError(t, os.RemoveAll("acct"))
		writeRemedyConfig(t, "acct", &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}})
		got := runRemedyCLI(t, "--config", "acct", "kubeconfig", "--cluster", "c-1")
		assert.Contains(t, got, `not logged in to "prod"`)
		assert.Contains(t, got, "hint: rancher"+named+" auth login 'prod'")
		assertEveryCommandNames(t, got, named)
	})

	t.Run("get-token for an entry at another URL", func(t *testing.T) {
		require.NoError(t, os.RemoveAll("acct"))
		writeRemedyConfig(t, "acct", &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://other.example.com"}}})
		args := []string{"--config", "acct", "auth", "get-token", "--server", "https://r.example.com", "--cluster", "c-1", "--name", "prod"}
		got := runRemedyCLI(t, args...)
		assert.Contains(t, got, "error (CONFLICT)")
		assertEveryCommandNames(t, got, named)

		var env struct {
			Data struct {
				Error struct{ Message string } `json:"error"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(runRemedyCLI(t, append(args, "-o", "json")...)), &env))
		assertEveryCommandNames(t, env.Data.Error.Message, named)
	})

	// A file Rancher writes names a user and no --name and carries no env,
	// and the environment that ran kubectl chose its directory, which a
	// remedy run in the same environment resolves too: its remedies name no
	// --config. With a --name the file is the CLI's, and they do.
	t.Run("get-token with --user-id and no --name names no directory", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"local","type":"localProvider"}]}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		require.NoError(t, os.RemoveAll("acct"))
		writeRemedyConfig(t, "acct", &config.Config{})
		base := []string{"--config", "acct", "auth", "get-token", "--server", srv.URL, "--user-id", "u-abc12", "--auth-provider", "local"}
		for name, extra := range map[string][]string{
			"with a cluster":    {"--cluster", "c-1"},
			"without a cluster": nil,
		} {
			got := runRemedyCLI(t, append(append([]string{}, base...), extra...)...)
			assert.Contains(t, got, "error (AUTH)", name)
			assertEveryCommandNames(t, got, "")
			assert.NotContains(t, got, "--config", name)
		}
		got := runRemedyCLI(t, append(append([]string{}, base...), "--cluster", "c-1", "--name", "prod")...)
		assert.Contains(t, got, "error (AUTH)")
		assertEveryCommandNames(t, got, named)
	})

	t.Run("the legacy token command's tie is plain", func(t *testing.T) {
		require.NoError(t, os.RemoveAll("acct"))
		writeRemedyConfig(t, "acct", &config.Config{Servers: map[string]config.Server{
			"prod": {URL: "https://r.example.com"}, "staging": {URL: "https://r.example.com"}}})
		store := config.NewFileStore("acct")
		require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: "https://r.example.com"}, "tok-a"))
		require.NoError(t, store.SetToken(config.ServerRef{Name: "staging", URL: "https://r.example.com"}, "tok-b"))
		got := runRemedyCLI(t, "--config", "acct", "token", "--server", "https://r.example.com", "--user", "alice")
		assert.Contains(t, got, "error (AMBIGUOUS_SELECTION)")
		assertEveryCommandNames(t, got, "")
	})

	// The command tree keeps process-wide state of its own (the read-only
	// override, the loaded config completion reads), so two trees cannot
	// run at once; runtime asserts the concurrent case per invocation.
	// Here the invocations alternate in one process: nothing one leaves
	// behind reaches the next.
	t.Run("another command in the same process names its own directory", func(t *testing.T) {
		require.NoError(t, os.RemoveAll("acct"))
		require.NoError(t, os.MkdirAll("blank", 0o700))
		blank := " --config '" + filepath.Join(realWD, "blank") + "'"
		for range 3 {
			assertEveryCommandNames(t, runRemedyCLI(t, "--config", "acct", "kubeconfig", "--cluster", "c-1"), named)
			assertEveryCommandNames(t, runRemedyCLI(t, "--config", "blank", "auth", "status"), blank)
			// JSON is written as encoded, with nothing like output.Printable
			// between the text and the terminal, so a marker left there leaks.
			var doc any
			require.NoError(t, json.Unmarshal([]byte(runRemedyCLI(t, "--config", "blank", "auth", "status", "-o", "json")), &doc))
			assertEveryCommandNames(t, strings.Join(jsonStrings(doc), "\n"), blank)
		}
	})
}

// TestADirectoryFailureNamesTheDirectory asserts the failure to open the
// invocation's directory, rendered before the command runs, names that
// directory in its remedies like the command's own errors.
func TestADirectoryFailureNamesTheDirectory(t *testing.T) {
	t.Parallel()
	var errOut bytes.Buffer
	c := &ucli.Command{Name: "rancher", ErrWriter: &errOut}
	err := renderDepsError(c, runtime.IO{RemedyDir: "/d"}, errors.New("config.yaml cannot be read; run "+clierr.Rancher+" config status"))
	require.Error(t, err)
	assert.Contains(t, errOut.String(), "run rancher --config '/d' config status")
	assert.NotContains(t, errOut.String(), clierr.RemedyMarker)
}

// jsonStrings returns every string a decoded JSON document holds.
func jsonStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, jsonStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, jsonStrings(e)...)
		}
		return out
	}
	return nil
}

// TestALoadFailureNamesTheDirectoryThroughTheCommandTree asserts, through
// the real command tree, that the directory is on the invocation before the
// config load: a load error that carries a remedy names it.
func TestALoadFailureNamesTheDirectoryThroughTheCommandTree(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	prev := openDirectory
	openDirectory = func(string) (config.CredStore, *config.CacheAnchor, error) {
		return nil, nil, errors.New("config.yaml cannot be read; run " + clierr.Rancher + " config status")
	}
	t.Cleanup(func() { openDirectory = prev })
	dir := filepath.Join(t.TempDir(), "acct")
	got := runRemedyCLI(t, "--config", dir, "kubeconfig", "--cluster", "c-1")
	assert.Contains(t, got, "run rancher --config "+clierr.ShellQuote(dir)+" config status")
	assert.NotContains(t, got, clierr.RemedyMarker)
}
