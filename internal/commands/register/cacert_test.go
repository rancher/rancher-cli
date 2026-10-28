package register

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestPrepareConnectingPreflightsWithTheVariableAndKeepsTheSavedCA asserts
// the probe of a login that connects with RANCHER_CLI_CACERT: the preflight
// runs with the variable's bytes, while the saved selector and its own
// bytes, which the commit rechecks, are the entry's.
func TestPrepareConnectingPreflightsWithTheVariableAndKeepsTheSavedCA(t *testing.T) {
	saved := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	variable := cmdtest.WriteCAFile(t, cmdtest.CAPem2)
	var got []string
	preflight := func(_ context.Context, _, ca string) (string, error) {
		got = append(got, ca)
		return "v2.20.0", nil
	}

	p, err := PrepareConnecting(t.Context(), preflight, "https://r.ex", saved, variable)
	require.NoError(t, err)
	assert.Equal(t, []string{cmdtest.CAPem2}, got)
	assert.Equal(t, saved, p.CASelector)
	assert.Equal(t, cmdtest.CAPem1, p.CABytes, "the saved CA's own bytes")
	assert.Equal(t, cmdtest.CAPem2, p.ConnectionBytes)
	assert.True(t, p.FromEnvironment)

	got = nil
	p, err = PrepareConnecting(t.Context(), preflight, "https://r.ex", "", variable)
	require.NoError(t, err)
	assert.Equal(t, []string{cmdtest.CAPem2}, got)
	assert.Empty(t, p.CASelector, "the variable is never the saved CA")
	assert.Empty(t, p.CABytes)
	assert.True(t, p.FromEnvironment)

	got = nil
	p, err = Prepare(t.Context(), preflight, "https://r.ex", saved)
	require.NoError(t, err)
	assert.Equal(t, []string{cmdtest.CAPem1}, got, "no variable: the saved CA connects")
	assert.Equal(t, cmdtest.CAPem1, p.ConnectionBytes)
	assert.False(t, p.FromEnvironment)
}

// TestPrepareConnectingReadsTheVariableOnlyForHTTPS asserts the variable is
// not resolved for an http URL, where no CA applies: nothing was connected
// with it.
func TestPrepareConnectingReadsTheVariableOnlyForHTTPS(t *testing.T) {
	p, err := PrepareConnecting(t.Context(), nil, "http://r.ex", "", "/nonexistent/ca.pem")
	require.NoError(t, err)
	assert.Empty(t, p.ConnectionBytes)
	assert.False(t, p.FromEnvironment)
}

// TestPrepareConnectingNamesTheUnreadableCA asserts an unreadable variable
// is "reading RANCHER_CLI_CACERT: ...", and an unreadable saved CA fails
// first with its own text, even when the variable applies.
func TestPrepareConnectingNamesTheUnreadableCA(t *testing.T) {
	good := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	_, err := PrepareConnecting(t.Context(), nil, "https://r.ex", good, "/nonexistent/variable.pem")
	require.ErrorContains(t, err, "reading RANCHER_CLI_CACERT: ")
	require.ErrorContains(t, err, "the file does not exist")
	assert.NotContains(t, err.Error(), "variable.pem")

	_, err = PrepareConnecting(t.Context(), nil, "https://r.ex", "/nonexistent/saved.pem", good)
	require.ErrorContains(t, err, "reading --cacert: ")
	require.ErrorContains(t, err, "the file does not exist")
	assert.NotContains(t, err.Error(), "saved.pem")
}

// TestCommitCreateRecordsAVariableConnectedLoginOnAnEntryWithNoCA asserts
// the new entry's record: set when the login connected with the variable and
// the entry saves no CA, never set when it saves one.
func TestCommitCreateRecordsAVariableConnectedLoginOnAnEntryWithNoCA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe Probe
		want  bool
	}{
		{"no CA, variable", Probe{CanonicalURL: "https://r.ex", FromEnvironment: true}, true},
		{"no CA, no variable", Probe{CanonicalURL: "https://r.ex"}, false},
		{"a CA, variable", Probe{CanonicalURL: "https://r.ex", CASelector: cmdtest.CAPem1, CABytes: cmdtest.CAPem1, FromEnvironment: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.probe.CASelector != "" {
				tc.probe.CASelector = cmdtest.WriteCAFile(t, cmdtest.CAPem1)
			}
			cfg := writableCfg(t, map[string]config.Server{}, "")
			_, err := commitCreate(cfg, "qa1", tc.probe)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Servers["qa1"].CAFromEnvironment)
		})
	}
}

// TestCommitUpdateClearsTheVariableRecord asserts every write that replaces
// or clears the entry's CA or URL drops the record that a login connected
// with the variable, that --clear-cacert on a marked entry with no CA is a
// change, and that an update that touches neither keeps the record.
func TestCommitUpdateClearsTheVariableRecord(t *testing.T) {
	marked := config.Server{URL: "https://qa1.example.com", CAFromEnvironment: true}
	for _, tc := range []struct {
		name        string
		probe       Probe
		clear       bool
		wantChanged []string
		wantMarked  bool
	}{
		{"a new CA", Probe{CanonicalURL: marked.URL, CASelector: cmdtest.CAPem1, CABytes: cmdtest.CAPem1}, false, []string{"ca"}, false},
		{"--clear-cacert on a marked entry with no CA", Probe{CanonicalURL: marked.URL}, true, []string{"ca"}, false},
		{"a URL change", Probe{CanonicalURL: "https://qa2.example.com"}, false, []string{"url"}, false},
		{"nothing asked for", Probe{CanonicalURL: marked.URL}, false, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writableCfg(t, map[string]config.Server{"qa1": marked}, "qa1")
			src := srcOf(marked)
			src.ClearCA = tc.clear
			if tc.probe.CASelector != "" {
				tc.probe.CASelector = cmdtest.WriteCAFile(t, cmdtest.CAPem1)
			}
			out, err := commitUpdate(cfg, "qa1", src, tc.probe)
			require.NoError(t, err)
			assert.Equal(t, tc.wantChanged, out.ChangedFields)
			assert.Equal(t, tc.wantMarked, cfg.Servers["qa1"].CAFromEnvironment)
		})
	}

	t.Run("--clear-cacert on an entry that is not marked is no change", func(t *testing.T) {
		plain := config.Server{URL: "https://qa1.example.com"}
		cfg := writableCfg(t, map[string]config.Server{"qa1": plain}, "qa1")
		src := srcOf(plain)
		src.ClearCA = true
		out, err := commitUpdate(cfg, "qa1", src, Probe{CanonicalURL: plain.URL})
		require.NoError(t, err)
		assert.True(t, out.Unchanged)
	})
}
