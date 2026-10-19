package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
)

// TestUserIDCompletionOffersTheRecordedOwners asserts auth get-token
// --user-id completes to the owner the entry the typed --name identifies
// records, by its name or the <entry>-<cluster> form, and for a call with
// no --name or one that identifies no entry to the distinct owners the
// entries for the typed --server (the current server's URL without one)
// record, sorted, those that begin with the word; and to nothing for an
// entry at another URL, an entry that records no owner or one that is not
// a user id. The site is owned: with live completion on, the live layer is
// not asked.
func TestUserIDCompletionOffersTheRecordedOwners(t *testing.T) {
	cfg := &config.Config{
		CurrentServer: "prod",
		Completion:    config.CompletionConfig{Network: true},
		Servers: map[string]config.Server{
			"prod":    {URL: "https://r.ex", TokenUserID: "u-def34"},
			"staging": {URL: "https://r.ex", TokenUserID: "u-abc12"},
			"again":   {URL: "https://r.ex", TokenUserID: "u-abc12"},
			"other":   {URL: "https://other.ex", TokenUserID: "u-other"},
			"blank":   {URL: "https://r.ex"},
			"strange": {URL: "https://r.ex", TokenUserID: "U/1"},
		},
	}
	setStoredConfigForTest(t, cfg, t.TempDir())
	d := token.NewTokenCommand("", nil, nil).Describe()

	for _, tc := range []struct {
		name string
		tail []string
		word string
		want []string
	}{
		{"by entry name", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod"}, "", []string{"u-def34"}},
		{"by kubeconfig name", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod-c-1"}, "", []string{"u-def34"}},
		{"current server", []string{"--cluster", "c-1", "--name=prod-c-1"}, "", []string{"u-def34"}},
		{"schemeless server", []string{"--server", "r.ex", "--cluster", "c-1", "--name", "prod"}, "u-", []string{"u-def34"}},
		{"word not a prefix", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod"}, "x", nil},
		{"no --name", []string{"--server", "https://r.ex", "--cluster", "c-1"}, "", []string{"u-abc12", "u-def34"}},
		{"no --name, a word", []string{"--server", "https://r.ex", "--cluster", "c-1"}, "u-d", []string{"u-def34"}},
		{"no --name, no --cluster", []string{"--server", "https://r.ex"}, "", []string{"u-abc12", "u-def34"}},
		{"no --name, the current server's URL", []string{"--cluster", "c-1"}, "", []string{"u-abc12", "u-def34"}},
		{"no --name, another server", []string{"--server", "https://other.ex"}, "", []string{"u-other"}},
		{"no --name, a server with no entry", []string{"--server", "https://none.ex"}, "", nil},
		{"name identifies nothing", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "kube"}, "", []string{"u-abc12", "u-def34"}},
		{"entry at another URL", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "other"}, "", nil},
		{"no owner recorded", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "blank"}, "", nil},
		{"owner not a user id", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "strange"}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := CompletionSite{Kind: SiteFlagValue, FlagName: "user-id", FlagSpelling: "--user-id", Partial: tc.word}
			got := dispatchSite(t.Context(), site, d, nil, noLiveSource{t}, append(tc.tail, "--user-id"))
			assert.Equal(t, tc.want, got)
			site.EqualsForm = true
			eq := dispatchSite(t.Context(), site, d, nil, noLiveSource{t}, append(tc.tail, "--user-id="+tc.word))
			require.Len(t, eq, len(tc.want))
			for i, c := range eq {
				assert.Equal(t, "--user-id="+tc.want[i], c)
			}
		})
	}
}

// TestUserIDCompletionThroughTheCommandTree asserts the completion
// protocol reaches the owners, read from the directory --config names: the
// identified entry's for a typed --name, the distinct owners at the URL
// for none, and each value as a word that completes to itself.
func TestUserIDCompletionThroughTheCommandTree(t *testing.T) {
	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv, rawCurrentWord, rawCompleting = origArgv, origWord, origProto
	})
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod":    {URL: "https://r.ex", TokenUserID: "u-def34"},
		"staging": {URL: "https://r.ex", TokenUserID: "u-abc12"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	for _, tc := range []struct {
		name string
		tail []string
		cur  string
		want string
	}{
		{"a typed --name", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod-c-1"}, "", "u-def34\n"},
		{"no --name", []string{"--server", "https://r.ex", "--cluster", "c-1"}, "", "u-abc12\nu-def34\n"},
		{"no --name, no --cluster, a word", []string{"--server", "https://r.ex"}, "u-d", "u-def34\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scriptArgs := append(append([]string{"--rancher-completion-proto=2", "auth", "get-token"}, tc.tail...),
				"--user-id", "--rancher-completion-cur="+tc.cur, "--generate-shell-completion")
			CaptureArgv(scriptArgs)
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out bytes.Buffer
			root.Writer = &out
			require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))
			assert.Equal(t, tc.want, out.String())
		})
	}
}
