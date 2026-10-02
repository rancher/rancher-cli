package token_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// runCLI runs words through the real command tree with stdin and no
// terminal, and returns what it printed and the error.
func runCLI(t *testing.T, stdin string, words ...string) (stdout, stderr string, err error) {
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

// errorMessage is the message of the error envelope a command printed.
func errorMessage(t *testing.T, stderr string) string {
	t.Helper()
	var env struct {
		Data struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"data"`
	}
	// Notices come first, one per line; the envelope follows them.
	i := strings.Index(stderr, "{")
	require.GreaterOrEqual(t, i, 0, stderr)
	require.NoError(t, json.Unmarshal([]byte(stderr[i:]), &env), stderr)
	return env.Data.Error.Message
}

// TestNoTerminalRemedyForAPluginLoginEntryNameWorks asserts the remedy a
// call without a terminal gets when its --name stands for one of the exec
// plugin's own login entries, which auth login refuses: with token reuse
// off (nothing of that entry is read) and on (that entry holds no login),
// the message names a login of a new entry, that entry made current and
// the kubeconfig written again, and following the three commands as
// printed, a token piped and no terminal, makes the regenerated exec
// entry's call succeed.
func TestNoTerminalRemedyForAPluginLoginEntryNameWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the words are split by a POSIX shell, the one the remedies are quoted for")
	}
	const seed = "seed:secret"
	f := testrancher.New(testrancher.Options{SeedToken: seed, SelfSubjectReviews: true,
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rancherversion":
				_, _ = w.Write([]byte(`{"Version":"v2.15.1"}`))
				return
			case "/k8s/clusters/local/apis/management.cattle.io/v3/clusters/local":
				// The cluster rancher kubeconfig reads for its direct
				// endpoints; it has none.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"local"},"spec":{}}`))
				return
			}
			http.NotFound(w, r)
		})})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	plugin := config.ExecLoginEntryName(config.CanonicalServerIdentity(srv.URL))

	for _, reuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "token reuse off", true: "token reuse on"}[reuse], func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{CredentialStore: "file", TokenReuse: reuse, Servers: map[string]config.Server{
				plugin: {URL: srv.URL, Provenance: clierr.ProvenanceExecLogin, TokenUserID: testrancher.SeedOwner},
			}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			getToken := func(name string) (string, error) {
				_, stderr, err := runCLI(t, "", "--config", dir, "auth", "get-token", "--server", srv.URL, "--cluster", "local",
					"--name", name, "--user-id", testrancher.SeedOwner, "-o", "json")
				return stderr, err
			}

			stderr, err := getToken(plugin + "-local")
			require.Error(t, err)
			msg := errorMessage(t, stderr)
			commands := remedyCommands(msg)
			var login, useServer, kubeconfig string
			for _, c := range commands {
				switch {
				case strings.Contains(c, " auth login "):
					login = c
				case strings.Contains(c, " config use-server "):
					useServer = c
				case strings.Contains(c, " kubeconfig --cluster "):
					kubeconfig = c
				}
			}
			require.NotEmpty(t, login, msg)
			require.NotEmpty(t, useServer, "the new entry is made current: %s", msg)
			require.NotEmpty(t, kubeconfig, "the kubeconfig is written again from it: %s", msg)
			assert.NotContains(t, msg, "auth login '"+config.ExecLoginPrefix)

			for _, c := range []string{login, useServer} {
				words := shellWords(t, strings.ReplaceAll(c, "<name>", "mine"))
				_, stderr, err := runCLI(t, seed+"\n", words[1:]...)
				require.NoError(t, err, "%s: %s", c, stderr)
			}
			out, stderr, err := runCLI(t, "", shellWords(t, kubeconfig)[1:]...)
			require.NoError(t, err, stderr)
			kc, err := clientcmd.Load([]byte(out))
			require.NoError(t, err)
			require.Len(t, kc.AuthInfos, 1)
			var args []string
			for _, ai := range kc.AuthInfos {
				args = ai.Exec.Args
			}
			i := slices.Index(args, "--name")
			require.GreaterOrEqual(t, i, 0)
			assert.Equal(t, "mine-local", args[i+1], "the regenerated exec entry names the new entry")
			assert.Contains(t, args, "--user-id")

			_, err = getToken(args[i+1])
			require.NoError(t, err, "the regenerated exec entry's call succeeds without a terminal")
		})
	}
}
