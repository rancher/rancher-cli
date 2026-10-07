package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
)

// TestUserIDCompletionOffersTheIdentifiedEntrysOwner asserts auth
// get-token --user-id completes to the owner the entry the typed --name
// identifies records, by its name or the <entry>-<cluster> form, when it
// begins with the word; and to nothing for a call with no --name, a name
// that identifies no entry or an entry at another URL, an entry that
// records no owner or one that is not a user id. The site is owned: with
// live completion on, the live layer is not asked.
func TestUserIDCompletionOffersTheIdentifiedEntrysOwner(t *testing.T) {
	cfg := &config.Config{
		CurrentServer: "prod",
		Completion:    config.CompletionConfig{Network: true},
		Servers: map[string]config.Server{
			"prod":    {URL: "https://r.ex", TokenUserID: "u-abc12"},
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
		{"by entry name", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod"}, "", []string{"u-abc12"}},
		{"by kubeconfig name", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod-c-1"}, "", []string{"u-abc12"}},
		{"current server", []string{"--cluster", "c-1", "--name=prod-c-1"}, "", []string{"u-abc12"}},
		{"schemeless server", []string{"--server", "r.ex", "--cluster", "c-1", "--name", "prod"}, "u-", []string{"u-abc12"}},
		{"word not a prefix", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "prod"}, "x", nil},
		{"no --name", []string{"--server", "https://r.ex", "--cluster", "c-1"}, "", nil},
		{"name identifies nothing", []string{"--server", "https://r.ex", "--cluster", "c-1", "--name", "kube"}, "", nil},
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
// protocol reaches the owner for a typed --name, read from the directory
// --config names.
func TestUserIDCompletionThroughTheCommandTree(t *testing.T) {
	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv, rawCurrentWord, rawCompleting = origArgv, origWord, origProto
	})
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", TokenUserID: "u-abc12"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	scriptArgs := []string{"--rancher-completion-proto=2", "auth", "get-token", "--server", "https://r.ex", "--cluster", "c-1",
		"--name", "prod-c-1", "--user-id", "--rancher-completion-cur=", "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))
	assert.Equal(t, "u-abc12\n", out.String())
}
