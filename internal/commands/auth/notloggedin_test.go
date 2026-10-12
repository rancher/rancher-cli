package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// runRoot runs words through the real command tree with stdin and returns
// stdout, stderr and the error.
func runRoot(t *testing.T, stdin string, words ...string) (string, string, error) {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	root.Reader = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	err = root.Run(t.Context(), append([]string{"rancher"}, words...))
	return out.String(), errOut.String(), err
}

// shellWords splits a command of plain and single-quoted words, the form
// every remedy is written in.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	for f := range strings.FieldsSeq(s) {
		unq, ok := strings.CutPrefix(f, "'")
		if ok {
			unq, ok = strings.CutSuffix(unq, "'")
			require.True(t, ok, "a quoted word holds no space in these remedies: %q", s)
		}
		words = append(words, unq)
	}
	return words
}

// TestNotLoggedInHintForALonePluginLoginEntryRuns asserts the NOT_LOGGED_IN
// hint of a directory whose one server entry is the exec plugin's own
// login entry runs through auth login for an IPv6 server, whose host auth
// login derives no name from and whose loopback form would begin with a
// dash: the hint, as auth status prints it, run against the directory
// with the token on stdin, logs in under the name it names.
func TestNotLoggedInHintForALonePluginLoginEntryRuns(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here: " + err.Error())
	}
	const seed = "seed:secret"
	srv := httptest.NewUnstartedServer(testrancher.New(testrancher.Options{SeedToken: seed, SelfSubjectReviews: true}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")

	dir := t.TempDir()
	plugin := config.ExecLoginEntryName(config.CanonicalServerIdentity(srv.URL))
	cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{plugin: {URL: srv.URL, CA: caPEM, Provenance: clierr.ProvenanceExecLogin}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	_, stderr, err := runRoot(t, "", "--config", dir, "auth", "status", "-o", "json")
	require.Error(t, err)
	var env struct {
		Data struct {
			Error struct {
				Code string `json:"code"`
				Hint string `json:"hint"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &env), stderr)
	require.Equal(t, clierr.CodeNotLoggedIn, env.Data.Error.Code)
	words := shellWords(t, env.Data.Error.Hint)
	require.Equal(t, []string{"rancher", "auth", "login"}, words[:3], env.Data.Error.Hint)
	name := words[3]
	assert.False(t, strings.HasPrefix(name, "-"), "a name with a leading dash reads as a flag: %q", name)

	_, stderr, err = runRoot(t, seed+"\n", append(append([]string{"--config", dir}, words[1:]...), "--token-stdin", "--cacert", caPEM)...)
	require.NoError(t, err, stderr)
	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, srv.URL, after.Servers[name].URL, "the hint's entry is registered at the server")
	assert.Equal(t, testrancher.SeedOwner, after.Servers[name].TokenUserID, "and logged in")
}
