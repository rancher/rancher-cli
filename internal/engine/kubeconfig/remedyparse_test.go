package kubeconfig_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kubeconfig"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// notice commands end at the words of the sentence that follows them; a
// placeholder stands in for the value the operator chooses.
var (
	noticeCommandRE    = regexp.MustCompile(`rancher --config '[^']*'(?: (?:[a-z0-9][a-z0-9.-]*|--[a-z-]*|'[^']*'|<[a-z]+>))+`)
	noticePlaceholders = strings.NewReplacer("<name>", "x", "<file>", "/etc/rancher/ca.pem", "<id>", "c-1")
)

// TestTheNoticesOfTheKubeconfigCommandRunInTheCommandTree asserts every
// command the kubeconfig command names for a call with no stored login, and
// for a --name that auth get-token refuses, reaches the command the notice
// means in the real command tree with --config naming the directory: a
// piped login, the entry made current, the kubeconfig written again, and the
// kubeconfig created with its own token.
func TestTheNoticesOfTheKubeconfigCommandRunInTheCommandTree(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	t.Setenv(config.EnvCredentialStore, "file")
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}}
	for _, tc := range []struct {
		name string
		text func() string
		want []string
	}{
		{"no stored login answers", func() string {
			return kubeconfig.NoStoredLoginNoticeForTest(cfg, "/x", "prod", "c-1", "https://r.example.com")
		}, []string{"auth login", "create kubeconfig"}},
		{"a name auth get-token refuses", func() string {
			return kubeconfig.ReservedNameRefusalForTest(config.LegacyCachePrefix+"x", "c-1", "https://r.example.com")
		}, []string{"auth login", "config use-server", "kubeconfig"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			text := rt.IO{RemedyDir: dir}.ExpandText(tc.text())
			var ran []string
			for _, line := range noticeCommandRE.FindAllString(text, -1) {
				words := noOwnerShellWords(t, noticePlaceholders.Replace(line))
				name, configDir := runInTree(t, words)
				assert.Equal(t, dir, configDir, line)
				ran = append(ran, name)
			}
			require.NotEmpty(t, ran, text)
			assert.Equal(t, tc.want, ran, text)
		})
	}
}
