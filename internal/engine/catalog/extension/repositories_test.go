package extension

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite golden files")

const repositoriesGolden = "testdata/extension-repositories.golden.json"

// TestExtensionRepositoriesGolden asserts that the table of the two Rancher
// extension repositories equals its golden. The golden is the copy that
// scripts/extension-sources compares with the Rancher UI's source, so a
// repository added to the table, or a name, URL or branch changed, fails here
// until the golden is rewritten on purpose with `go test
// ./internal/engine/catalog/extension/ -run TestExtensionRepositoriesGolden
// -update`.
func TestExtensionRepositoriesGolden(t *testing.T) {
	got, err := json.MarshalIndent(Repositories(), "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	if *update {
		require.NoError(t, os.WriteFile(repositoriesGolden, got, 0o644))
	}
	raw, err := os.ReadFile(repositoriesGolden)
	require.NoError(t, err)
	var want, have []Repository
	require.NoError(t, json.Unmarshal(raw, &want))
	require.NoError(t, json.Unmarshal(got, &have))
	assert.Equal(t, want, have)
}

// TestRepositoryNamed asserts the lookup by ClusterRepo name finds the two
// repositories and nothing else, including the third repository the Rancher UI
// defines and never adds.
func TestRepositoryNamed(t *testing.T) {
	t.Parallel()
	prime, ok := RepositoryNamed("rancher-ui-plugins")
	require.True(t, ok)
	assert.True(t, prime.PrimeOnly)
	assert.Equal(t, "https://github.com/rancher/ui-plugin-charts", prime.URL)
	partner, ok := RepositoryNamed("partner-extensions")
	require.True(t, ok)
	assert.False(t, partner.PrimeOnly)
	for _, name := range []string{"community-extensions", "", "Partner-Extensions", "rancher-charts"} {
		_, ok := RepositoryNamed(name)
		assert.False(t, ok, name)
	}
}

// TestAlreadyAdded asserts the rule of the Rancher UI: the first non-empty of
// status.url, spec.gitRepo and spec.url is compared with the address by exact
// string equality.
func TestAlreadyAdded(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/rancher/partner-extensions"
	tests := []struct {
		name  string
		repos []ClusterRepo
		want  string
	}{
		{"empty list", nil, ""},
		{"status url", []ClusterRepo{{Name: "a", StatusURL: url}}, "a"},
		{"git repo", []ClusterRepo{{Name: "a", GitRepo: url}}, "a"},
		{"url", []ClusterRepo{{Name: "a", URL: url}}, "a"},
		{"status url differs from git repo", []ClusterRepo{{Name: "a", StatusURL: "https://example.com/x", GitRepo: url}}, ""},
		{"status url equals while git repo differs", []ClusterRepo{{Name: "a", StatusURL: url, GitRepo: "https://example.com/x"}}, "a"},
		{"git repo before url", []ClusterRepo{{Name: "a", GitRepo: "https://example.com/x", URL: url}}, ""},
		{"trailing slash", []ClusterRepo{{Name: "a", GitRepo: url + "/"}}, ""},
		{"git suffix", []ClusterRepo{{Name: "a", GitRepo: url + ".git"}}, ""},
		{"the first match is used", []ClusterRepo{{Name: "a", URL: "https://example.com/x"}, {Name: "b", GitRepo: url}, {Name: "c", URL: url}}, "b"},
		{"no address", []ClusterRepo{{Name: "a"}}, ""},
	}
	for _, tt := range tests {
		got, ok := AlreadyAdded(tt.repos, url)
		assert.Equal(t, tt.want != "", ok, tt.name)
		assert.Equal(t, tt.want, got.Name, tt.name)
	}
	_, ok := AlreadyAdded([]ClusterRepo{{Name: "a"}}, "")
	assert.False(t, ok, "an empty address matches no repository")
}
