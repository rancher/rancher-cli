package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// badRepoNames are --repo values that are not a ClusterRepo name, including
// a carriage return and a backslash.
var badRepoNames = map[string]string{
	"slash":               "a/b",
	"upper case":          "A",
	"underscore":          "a_b",
	"space":               "a b",
	"dots only":           "..",
	"dash after a dot":    "a.-b",
	"254 characters":      strings.Repeat("a", 254),
	"zero width space":    "a\u200bb",
	"carriage return":     "a\rb",
	"backslash":           `a\b`,
	"empty part":          "a..b",
	"ends with a dot":     "a.",
	"starts with a dash":  "-a",
	"ends with a dash":    "a-",
	"dash before a dot":   "a-.b",
	"upper case in a dot": "a.B",
}

// TestChartsRefuseANameThatIsNotAClusterRepoBeforeAnyRequest asserts
// `charts --repo <value>` is VALIDATION naming --repo, before the cluster is
// resolved or anything is read.
func TestChartsRefuseANameThatIsNotAClusterRepoBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	for name, repo := range badRepoNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cluster := &fakeCluster{}
			clients := &fakeClients{cluster: cluster}
			root := newFakeRoot(nil)
			_, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": "c-test", "repo": repo}})
			require.ErrorContains(t, err, "--repo")
			require.ErrorContains(t, err, "is not a ClusterRepo name")
			assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
			assert.Empty(t, clients.resolved, "the cluster is not resolved")
			assert.Empty(t, clients.asked, "no cluster client is built")
			assert.Empty(t, cluster.gets)
			assert.Empty(t, cluster.gvrs)
			assert.Empty(t, root.requests())
		})
	}
}

// TestChartsRepoNameMessage asserts the whole refusal text and that the
// value is shown quoted and in full.
func TestChartsRepoNameMessage(t *testing.T) {
	t.Parallel()
	_, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, newFakeRoot(nil), input{s: map[string]string{"cluster": "c-test", "repo": "a/b"}})
	ce := clierr.From(err)
	assert.Equal(t, `--repo 'a/b' is not a ClusterRepo name: a ClusterRepo name has at most 253 characters of lowercase letters, digits, "-" and ".", each part between dots starting and ending with a letter or digit`, ce.Message)
	assert.Empty(t, ce.Hint)
}

// TestChartsAcceptAClusterRepoName asserts a valid name the server does not
// have is the existing NOT_FOUND, and one it has is read.
func TestChartsAcceptAClusterRepoName(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"a", "0abc", "a.b", "a.b-c", "rancher-charts", strings.Repeat("a", 253)} {
		t.Run(repo[:min(len(repo), 20)], func(t *testing.T) {
			t.Parallel()
			cluster := &fakeCluster{missing: []string{repo}}
			clients := &fakeClients{cluster: cluster}
			_, err := runCharts(t, clients, newFakeRoot(nil), input{s: map[string]string{"cluster": "c-test", "repo": repo}})
			require.Error(t, err)
			assert.Equal(t, clierr.CodeNotFound, clierr.From(err).Code)
			assert.Equal(t, []string{repo}, cluster.gets)
		})
	}
}

// countingClients counts the cluster resolves an app command makes.
type countingClients struct {
	*appFake
	resolved []string
}

func (c *countingClients) ResolveCluster(ctx context.Context, v string) (string, error) {
	c.resolved = append(c.resolved, v)
	return c.appFake.ResolveCluster(ctx, v)
}

var _ engine.ClusterClientFactory = (*countingClients)(nil)

// TestInstallRefuseANameThatIsNotAClusterRepoBeforeAnyRequest asserts
// `app install --repo <value>` is VALIDATION naming --repo, before the
// cluster is resolved, a values file is read or anything is requested.
func TestInstallRefuseANameThatIsNotAClusterRepoBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	for name, repo := range badRepoNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			cc := &countingClients{appFake: f}
			cmd := newTestInstall(f, nil)
			cmd.clients = cc
			_, _, err := cmd.Plan(t.Context(), installInput(func(in *appInput) {
				in.s[flagRepo] = repo
				in.strs[flagValues] = []string{"-"}
				in.terminal = true
			}))
			require.ErrorContains(t, err, "--repo")
			require.ErrorContains(t, err, "is not a ClusterRepo name")
			assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
			assert.Empty(t, cc.resolved, "the cluster is not resolved")
			assert.Empty(t, f.reads)
			assert.Empty(t, f.root.requests())
		})
	}
}

// TestInstallRepoNameMessage asserts the whole refusal text and that the
// value is shown quoted and in full.
func TestInstallRepoNameMessage(t *testing.T) {
	t.Parallel()
	_, _, err := newTestInstall(newAppFake(t), nil).Plan(t.Context(), installInput(func(in *appInput) { in.s[flagRepo] = "a/b" }))
	ce := clierr.From(err)
	assert.Equal(t, `--repo 'a/b' is not a ClusterRepo name: a ClusterRepo name has at most 253 characters of lowercase letters, digits, "-" and ".", each part between dots starting and ending with a letter or digit`, ce.Message)
	assert.Empty(t, ce.Hint)
}
