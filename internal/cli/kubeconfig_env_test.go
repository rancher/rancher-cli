package cli

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestKubeconfigRecordsTheDirectoryTheCommandOpened asserts, through the
// real command tree, that the exec entry's RANCHER_CLI_CONFIG_DIR is the
// directory the command resolved and opened - from --config (relative),
// RANCHER_CLI_CONFIG_DIR, XDG_CONFIG_HOME and the platform default - made
// absolute, and never the working directory or another spelling; and that
// the remedies the same run prints name that one directory.
func TestKubeconfigRecordsTheDirectoryTheCommandOpened(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/management.cattle.io.clusters/local" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"local","type":"management.cattle.io.cluster","metadata":{"name":"local"},"spec":{}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	home := t.TempDir()
	xdg := t.TempDir()
	envDir := filepath.Join(t.TempDir(), "fromenv")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvToken, "tok")
	t.Setenv(config.EnvCACert, caPEM)
	t.Setenv(config.EnvCredentialStore, "file")

	for _, tc := range []struct {
		name, flag, env, xdg, want string
	}{
		{name: "a relative --config", flag: "acct", want: filepath.Join(realWD, "acct")},
		{name: "RANCHER_CLI_CONFIG_DIR", env: envDir, want: envDir},
		{name: "XDG_CONFIG_HOME", xdg: xdg, want: filepath.Join(xdg, "rancher")},
		{name: "the platform default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvConfigDir, tc.env)
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			if tc.name == "the platform default" {
				want, err := config.ResolveDir("")
				require.NoError(t, err)
				tc.want = want
			}
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
			var out, errOut bytes.Buffer
			root.Writer, root.ErrWriter = &out, &errOut
			args := []string{"rancher"}
			if tc.flag != "" {
				args = append(args, "--config", tc.flag)
			}
			require.NoError(t, root.Run(t.Context(), append(args, "kubeconfig", "--cluster", "local")), "stderr: %s", errOut.String())
			kc, err := clientcmd.Load(out.Bytes())
			require.NoError(t, err)
			require.Len(t, kc.AuthInfos, 1)
			for _, ai := range kc.AuthInfos {
				require.NotEmpty(t, ai.Exec.Env)
				assert.Equal(t, config.EnvConfigDir, ai.Exec.Env[0].Name)
				assert.Equal(t, tc.want, ai.Exec.Env[0].Value)
				assert.True(t, filepath.IsAbs(ai.Exec.Env[0].Value))
			}
			assert.Contains(t, errOut.String(), "this kubeconfig records this machine's config directory "+tc.want+",")
			// Nothing is stored for the session, so the lookup notice names
			// the login to run - in the directory the file records.
			assert.Contains(t, errOut.String(), "pipe an API token to: rancher --config "+clierr.ShellQuote(tc.want)+" auth login")
		})
	}
}
