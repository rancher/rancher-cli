package cli

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inspectClusterValueSites lists every value-taking flag of inspect cluster
// with the arguments typed before its value. A value-taking flag added with no
// row fails TestEveryValueFlagOfInspectClusterHasACompletion.
var inspectClusterValueSites = map[string]appCompletionSite{
	"--node-name":        {completion: "none", reason: "free text", words: []string{"inspect", "cluster", "c-aaa11", "--registration", "--node-name"}},
	"--address":          {completion: "none", reason: "free text", words: []string{"inspect", "cluster", "c-aaa11", "--registration", "--address"}},
	"--internal-address": {completion: "none", reason: "free text", words: []string{"inspect", "cluster", "c-aaa11", "--registration", "--internal-address"}},
	"--label":            {completion: "none", reason: "free text", words: []string{"inspect", "cluster", "c-aaa11", "--registration", "--label"}},
	"--taint":            {completion: "none", reason: "free text", words: []string{"inspect", "cluster", "c-aaa11", "--registration", "--taint"}},
}

// TestEveryValueFlagOfInspectClusterHasACompletion asserts every
// value-taking flag of inspect cluster has a row and no row names a flag that
// is gone, and that through the real TAB callback each row offers no
// candidates and sends no request. NOT parallel: the completion fixture mutates env globals
// and the stored config.
func TestEveryValueFlagOfInspectClusterHasACompletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)

	var declared []string
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		if d.Name != "inspect cluster" {
			continue
		}
		for _, f := range d.Flags {
			if f.Type != "bool" {
				declared = append(declared, "--"+f.Name)
			}
		}
	}
	require.NotEmpty(t, declared, "inspect cluster has value-taking flags")
	assert.ElementsMatch(t, declared, slices.Collect(maps.Keys(inspectClusterValueSites)),
		"every value-taking flag has a row, and every row names one")

	s := newCommandSitesServer(t)
	for _, key := range slices.Sorted(maps.Keys(inspectClusterValueSites)) {
		site := inspectClusterValueSites[key]
		t.Run(key, func(t *testing.T) {
			require.Equal(t, "none", site.completion, "a node command option value is free text")
			assert.NotEmpty(t, site.reason)
			completionFixture(t, s.srv.URL, true)
			s.reset()
			got := runCompletion(t, site.words, "")
			hits := s.reset()
			assert.Empty(t, got, "no candidates")
			assert.Empty(t, hits, "no request")
		})
	}
}
