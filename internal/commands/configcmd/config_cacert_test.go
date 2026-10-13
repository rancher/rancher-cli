package configcmd

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
)

// markedEntry seeds a server entry with no CA that records a login made with
// RANCHER_CLI_CACERT.
func markedEntry(t *testing.T, dir, name, url string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[name] = config.Server{URL: url, CAFromEnvironment: true}
	cfg.CurrentServer = name
	require.NoError(t, cfg.Write())
}

// TestUpdateServerClearsTheVariableRecord asserts the entry changes that set
// or clear the CA or URL drop the record that a login connected with
// RANCHER_CLI_CACERT, --clear-cacert on a marked entry with no CA is a
// change that clears it, and an update that changes neither keeps it.
func TestUpdateServerClearsTheVariableRecord(t *testing.T) {
	const url = "https://qa1.example.com"
	for _, tc := range []struct {
		name        string
		in          cmdtest.Input
		wantChanged []string
		wantMarked  bool
	}{
		{"--cacert", cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem1}}, []string{"ca"}, false},
		{"--clear-cacert", cmdtest.Input{Flag: map[string]bool{"clear-cacert": true}}, []string{"ca"}, false},
		{"--url", cmdtest.Input{Str: map[string]string{"url": "https://qa2.example.com"}}, []string{"url"}, false},
		{"the same --url", cmdtest.Input{Str: map[string]string{"url": url}}, []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := cmdtest.FilePolicyDir(t)
			markedEntry(t, dir, "qa1", url)
			tc.in.Pos = []string{"qa1"}
			res, err := newUpdateServerCmd(dir).Execute(t.Context(), tc.in, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantChanged, res.Data.(map[string]any)["changed"])
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			assert.Equal(t, tc.wantMarked, cfg.Servers["qa1"].CAFromEnvironment)
		})
	}

	t.Run("--clear-cacert on an entry that is not marked changes nothing", func(t *testing.T) {
		dir, _ := cmdtest.FilePolicyDir(t)
		addServerForUpdateTests(t, dir, "qa1", url)
		res, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"clear-cacert": true}, Pos: []string{"qa1"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, true, res.Data.(map[string]any)["unchanged"])
	})
}

// TestAddAndUpdateServerNeverReadTheVariable asserts neither command
// connects with RANCHER_CLI_CACERT: add-server preflights with system roots,
// update-server with the entry's saved CA, and the unknown-authority error
// is the same text with the variable set and unset.
func TestAddAndUpdateServerNeverReadTheVariable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	serverPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	var seen []string
	capture := func(_ context.Context, _, ca string) (string, error) {
		seen = append(seen, ca)
		return "", nil
	}
	t.Setenv(config.EnvCACert, serverPEM)

	dir, _ := cmdtest.FilePolicyDir(t)
	_, err := (&configAddServerCmd{dir: dir, tlsPreflight: capture}).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{""}, seen, "add-server connects with system roots")

	seen = nil
	saved := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	_, err = (&configUpdateServerCmd{dir: dir, tlsPreflight: capture}).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": saved}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	seen = nil
	_, err = (&configUpdateServerCmd{dir: dir, tlsPreflight: capture}).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{cmdtest.CAPem1}, seen, "update-server connects with the entry's saved CA, not the variable")

	failure := func(t *testing.T) *clierr.Error {
		t.Helper()
		d, _ := cmdtest.FilePolicyDir(t)
		_, err := (&configAddServerCmd{dir: d, tlsPreflight: register.TLSPreflight}).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": srv.URL}, Pos: []string{"qa1"}}, nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		return ce
	}
	withVariable := failure(t)
	t.Setenv(config.EnvCACert, "")
	withoutVariable := failure(t)
	assert.Equal(t, clierr.CodeTLSTrust, withVariable.Code)
	assert.Equal(t, withoutVariable.Message, withVariable.Message, "the variable changes nothing about add-server's failure")
	assert.Contains(t, withVariable.Message, "and pass it with --cacert")
}
