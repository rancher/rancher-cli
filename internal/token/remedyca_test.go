package token_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
)

// loginCommandOf is the auth login a remedy text names, with <name> filled
// as name, split into words as a shell would.
func loginCommandOf(t *testing.T, text, dir, name string) []string {
	t.Helper()
	for _, c := range remedyCommands(rt.IO{RemedyDir: dir}.ExpandText(text)) {
		if strings.Contains(c, " auth login ") {
			return shellWords(t, strings.ReplaceAll(c, "<name>", name))
		}
	}
	t.Fatalf("no auth login in %q", text)
	return nil
}

// runPiped runs words through the real command tree with token on stdin
// and no terminal, and returns what was printed to stderr and the error.
func runPiped(t *testing.T, words []string, token string) (string, error) {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	root.Reader = strings.NewReader(token + "\n")
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	err = root.Run(t.Context(), words)
	return errOut.String(), err
}

// TestNewEntryLoginRemediesRunAgainstAPrivateCA asserts the login every
// remedy that names a new entry carries - the one for a plugin login
// entry of another user, for a login that records no owner where no user
// entry is the call's, and for a --name auth get-token would refuse - runs
// as written against a Rancher whose CA is private, with a piped token
// and no terminal: with the CA as a path it registers the entry and logs
// in; with the CA inline the text asks for the CA's file, and with that
// file added the same holds.
func TestNewEntryLoginRemediesRunAgainstAPrivateCA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the words are split by a POSIX shell, the one the remedies are quoted for")
	}
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCACert, "")
	const seed = "seed:secret"
	f := testrancher.New(testrancher.Options{SeedToken: seed, SelfSubjectReviews: true,
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rancherversion" {
				_, _ = w.Write([]byte(`{"Version":"v2.15.1"}`))
				return
			}
			http.NotFound(w, r)
		})})
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(caPEM), 0o600))

	builders := map[string]func(ca string) string{
		"another user's plugin login entry": func(ca string) string {
			return token.NewEntryLoginRemedy(nil, "u-abc12", "c-1", srv.URL, ca)
		},
		"a login that records no owner": func(ca string) string {
			return token.NewEntryLoginRemedy(nil, "", "c-1", srv.URL, ca)
		},
		"a refused --name": func(ca string) string {
			return token.NoTerminalLogin(nil, "", "c-1", srv.URL, ca, false).Text()
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			t.Run("the CA as a path", func(t *testing.T) {
				dir := t.TempDir()
				text := build(caPath)
				require.Contains(t, text, "--cacert '"+caPath+"'")
				stderr, err := runPiped(t, loginCommandOf(t, text, dir, "fresh"), seed)
				require.NoError(t, err, stderr)
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				assert.Equal(t, testrancher.SeedOwner, cfg.Servers["fresh"].TokenUserID, "logged in, owner recorded")
			})
			t.Run("the CA inline", func(t *testing.T) {
				dir := t.TempDir()
				text := build(caPEM)
				require.Contains(t, text, "adding --cacert with the path of a file that holds the server's CA certificate, which this exec entry carries inline")
				words := loginCommandOf(t, text, dir, "fresh")
				require.NotContains(t, words, "--cacert")
				stderr, err := runPiped(t, words, seed)
				require.Error(t, err)
				assert.Contains(t, stderr, "(TLS_TRUST)", "without the CA the private Rancher is not trusted")
				stderr, err = runPiped(t, append(words, "--cacert", caPath), seed)
				require.NoError(t, err, stderr)
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				assert.Equal(t, testrancher.SeedOwner, cfg.Servers["fresh"].TokenUserID)
			})
		})
	}
}
