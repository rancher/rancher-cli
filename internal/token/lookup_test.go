package token

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// mapReader is a CredentialReader over a map of entry name to token; an
// entry named in fail answers with that error.
type mapReader struct {
	tokens map[string]string
	fail   map[string]error
	reads  *[]string
}

func (m mapReader) GetToken(ref config.ServerRef) (string, error) {
	if m.reads != nil {
		*m.reads = append(*m.reads, ref.Name)
	}
	if err, ok := m.fail[ref.Name]; ok {
		return "", err
	}
	if tok, ok := m.tokens[ref.Name]; ok {
		return tok, nil
	}
	return "", config.ErrNoToken
}

// TestLookupStoredLoginOutcomes asserts the one lookup the exec plugin and
// the kubeconfig command share: which entry answers, for each way a call
// can identify an entry or not, and how a refusal is told from a store
// failure.
func TestLookupStoredLoginOutcomes(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	plugin := config.Server{URL: url, Provenance: clierr.ProvenanceExecLogin}
	for _, tc := range []struct {
		name         string
		cfg          config.Config
		tokens       map[string]string
		fail         map[string]error
		callName     string
		want         LookupOutcome
		identified   string
		entry        string
		pluginEntry  bool
		refusalCode  string
		wantReadOnly []string
	}{
		{name: "the identified entry's own login", callName: "prod-c-1",
			cfg:    config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}},
			tokens: map[string]string{"prod": "t"}, want: LookupAnswered, identified: "prod", entry: "prod", wantReadOnly: []string{"prod"}},
		{name: "an entry named exactly like the --name wins", callName: "prod-c-1",
			cfg:    config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}, "prod-c-1": {URL: url}}},
			tokens: map[string]string{"prod": "t", "prod-c-1": "u"}, want: LookupAnswered, identified: "prod-c-1", entry: "prod-c-1", wantReadOnly: []string{"prod-c-1"}},
		{name: "the plugin's own login entry, token reuse on", callName: "prod-c-1",
			cfg:    config.Config{TokenReuse: true, Servers: map[string]config.Server{"prod": {URL: url}, "exec-login-r": plugin}},
			tokens: map[string]string{"exec-login-r": "p"}, want: LookupAnswered, identified: "prod", entry: "exec-login-r", pluginEntry: true},
		{name: "no plugin fallback with token reuse off", callName: "prod-c-1",
			cfg:    config.Config{Servers: map[string]config.Server{"prod": {URL: url}, "exec-login-r": plugin}},
			tokens: map[string]string{"exec-login-r": "p"}, want: LookupNone, identified: "prod", wantReadOnly: []string{"prod"}},
		{name: "the ranked winner for a --name that identifies none", callName: "",
			cfg:    config.Config{CurrentServer: "staging", Servers: map[string]config.Server{"prod": {URL: url}, "staging": {URL: url}}},
			tokens: map[string]string{"prod": "a", "staging": "b"}, want: LookupAnswered, entry: "staging"},
		{name: "nothing stored", callName: "prod-c-1",
			cfg:  config.Config{Servers: map[string]config.Server{"prod": {URL: url}}},
			want: LookupNone, identified: "prod"},
		{name: "an entry at another URL", callName: "prod-c-1",
			cfg:  config.Config{Servers: map[string]config.Server{"prod": {URL: "https://other.example.com"}}},
			want: LookupRefused, refusalCode: clierr.CodeConflict},
		{name: "a tie", callName: "",
			cfg:    config.Config{Servers: map[string]config.Server{"prod": {URL: url}, "staging": {URL: url}}},
			tokens: map[string]string{"prod": "a", "staging": "b"}, want: LookupRefused, refusalCode: clierr.CodeAmbiguousSelection},
		{name: "several plugin login entries", callName: "prod-c-1",
			cfg:  config.Config{TokenReuse: true, Servers: map[string]config.Server{"prod": {URL: url}, "exec-login-a": plugin, "exec-login-b": plugin}},
			want: LookupRefused, identified: "prod", refusalCode: clierr.CodeAmbiguousSelection},
		{name: "a store that cannot be read", callName: "prod-c-1",
			cfg:  config.Config{Servers: map[string]config.Server{"prod": {URL: url}}},
			fail: map[string]error{"prod": errors.New("keychain locked")}, want: LookupReadFailed, identified: "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reads []string
			got := LookupStoredLogin(&tc.cfg, mapReader{tokens: tc.tokens, fail: tc.fail, reads: &reads}, url, tc.callName, "c-1")
			assert.Equal(t, tc.want, got.Outcome)
			assert.Equal(t, tc.identified, got.Identified)
			assert.Equal(t, tc.entry, got.Entry)
			assert.Equal(t, tc.pluginEntry, got.PluginEntry)
			switch tc.want {
			case LookupRefused:
				ce, ok := errors.AsType[*clierr.Error](got.Err)
				require.True(t, ok)
				assert.Equal(t, tc.refusalCode, ce.Code)
			case LookupReadFailed:
				require.ErrorContains(t, got.Err, "keychain locked")
			default:
				require.NoError(t, got.Err)
			}
			if tc.wantReadOnly != nil {
				assert.Equal(t, tc.wantReadOnly, reads, "the lookup reads only what the plugin reads")
			}
		})
	}
}

// TestNoTerminalLoginNamesTheEntryTheKubeconfigIdentifies asserts which
// entry the no-terminal login names: the identified entry; for a --name
// of the form <entry>-<cluster> that identifies none, <entry>, so one
// login serves every cluster's kubeconfig; else the --name; and never a
// name auth login refuses or one registered at another URL.
func TestNoTerminalLoginNamesTheEntryTheKubeconfigIdentifies(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	for _, tc := range []struct {
		name, callName string
		cfg            *config.Config
		shim           bool
		want           string
		named          bool
	}{
		{name: "a bare install, entry and cluster", callName: "prod-c-1", want: "'prod'", named: true},
		{name: "an empty directory, entry and cluster", callName: "prod-c-1", cfg: &config.Config{}, want: "'prod'", named: true},
		{name: "the identified entry", callName: "prod-c-1", cfg: &config.Config{Servers: map[string]config.Server{"prod-c-1": {URL: url}}}, want: "'prod-c-1'", named: true},
		{name: "a --name of another form", callName: "mine", want: "'mine'", named: true},
		{name: "the entry part registered elsewhere names the --name", callName: "prod-c-1", cfg: &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://other.example.com"}}}, want: "'prod-c-1'", named: true},
		{name: "the --name registered elsewhere", callName: "prod", cfg: &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://other.example.com"}}}, want: "<name>"},
		{name: "the legacy shim's slot", callName: config.LegacyCachePrefix + "alice", shim: true, want: "<name>"},
		{name: "no --name", want: "<name>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NoTerminalLogin(tc.cfg, tc.callName, "c-1", url, "", config.CASource{}, tc.shim)
			assert.Equal(t, clierr.Rancher+" auth login "+tc.want+" --url '"+url+"' --token-stdin", got.Command)
			assert.Equal(t, tc.named, got.Named)
			if tc.named {
				assert.Equal(t, got.Command, got.Hint())
			} else {
				assert.Empty(t, got.Hint())
				assert.Contains(t, got.Text(), "with a name of your choice for <name>")
			}
		})
	}
}

// TestGetTokenStoreConflictNamesTheRegenerateSteps asserts the exec
// plugin's refusal of a directory whose declared store its variable
// contradicts adds the steps that write the kubeconfig again without the
// variable, from the entry it was written from, and keeps the hint that
// unsets it; the legacy shim, whose kubeconfig records no variable, does
// not add them.
func TestGetTokenStoreConflictNamesTheRegenerateSteps(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(execInfoEnv, "")
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvCredentialStore, "keychain")
	store, err := config.NewLazyStore(cfg, dir)
	require.NoError(t, err)

	cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	_, err = cmd.Execute(t.Context(), tin{s: map[string]string{"server": "https://r.example.com", "cluster": "c-1", "name": "prod-c-1"}}, noTerminal())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorIs(t, err, config.ErrCredentialStoreConflict)
	require.ErrorContains(t, err, "if a kubeconfig's exec entry sets it, regenerate the kubeconfig with "+config.EnvCredentialStore+" unset: "+
		RegenerateSteps(clierr.Rancher, "prod", "c-1"))
	assert.Equal(t, "unset "+config.EnvCredentialStore, ce.Hint)

	shim := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	_, err = shim.Execute(t.Context(), tin{s: map[string]string{"server": "https://r.example.com", "user": "alice"}}, noTerminal())
	require.ErrorIs(t, err, config.ErrCredentialStoreConflict)
	require.NotContains(t, err.Error(), "regenerate the kubeconfig")
}

// TestGetTokenStoreConflictStepsNameTheEntryPartOfTheName asserts the
// conflict's regenerate steps name the <entry> of a --name of the form
// <entry>-<cluster> when no entry by either name is registered: the entry
// the kubeconfig was written from.
func TestGetTokenStoreConflictStepsNameTheEntryPartOfTheName(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(execInfoEnv, "")
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"other": {URL: "https://r.example.com"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvCredentialStore, "keychain")
	store, err := config.NewLazyStore(cfg, dir)
	require.NoError(t, err)

	cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	_, err = cmd.Execute(t.Context(), tin{s: map[string]string{"server": "https://r.example.com", "cluster": "c-1", "name": "prod-c-1"}}, noTerminal())
	require.ErrorIs(t, err, config.ErrCredentialStoreConflict)
	require.ErrorContains(t, err, RegenerateSteps(clierr.Rancher, "prod", "c-1"))
}
