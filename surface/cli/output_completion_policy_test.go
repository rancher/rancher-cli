package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOutputCompletionRespectsOutputPolicy is a manifest-wide conformance
// test, not per-command cases: for every registered command, every
// candidate the -o/--output flag-value completion offers must be a value
// that command's own OutputPolicy actually accepts. Completion must never
// suggest a format the command would then reject or resolve ambiguously.
func TestOutputCompletionRespectsOutputPolicy(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "output"}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		for _, cand := range enumCandidates(site, d) {
			assert.True(t, d.OutputPolicy.Allows(cand),
				"%s: -o completion offered %q, which its own OutputPolicy rejects", d.Name, cand)
		}
	}
}
