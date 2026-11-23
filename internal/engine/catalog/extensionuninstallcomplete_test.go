package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const extUninstallName = "extension uninstall"

// TestExtensionUninstallAcceptsExactlyWhatCompletionOffers asserts, on the
// mixed world of the upgrade tests, that the chart names the completion source
// offers are the Apps of the namespace, whatever their status or ClusterRepo,
// that the plan of `extension uninstall` accepts each of them, and that it
// refuses a name with no App.
func TestExtensionUninstallAcceptsExactlyWhatCompletionOffers(t *testing.T) {
	t.Parallel()
	w := extUpgradeMixedWorld()
	src := catalogcomplete.ExtensionUninstallSource(extUninstallName)
	req := completion.Request{Command: extUninstallName}
	_, claimed := src.Claim(req)
	require.True(t, claimed)
	got := src.Complete(t.Context(), w.server(t).Conn(1), req).Names

	var apps []string
	for _, a := range w.apps {
		apps = append(apps, a.name)
	}
	assert.ElementsMatch(t, apps, got)
	for _, chart := range append(apps, "ghost") {
		_, _, err := newTestExtensionUninstall(w.fake(t)).Plan(t.Context(), extInput(chart))
		assert.Equal(t, chart != "ghost", err == nil, "chart %s: %v", chart, err)
	}
}

// TestExtensionUninstallCompletionRequestCountAndSites asserts the one request
// of the chart name, the allowance the source claims for it, and that no other
// site is claimed: a flag value, a second positional and another command.
func TestExtensionUninstallCompletionRequestCountAndSites(t *testing.T) {
	t.Parallel()
	src := catalogcomplete.ExtensionUninstallSource(extUninstallName)
	req := completion.Request{Command: extUninstallName}
	reqs, claimed := src.Claim(req)
	require.True(t, claimed)
	assert.Equal(t, completion.AllowanceOne, reqs.Allowance)
	srv := extUpgradeMixedWorld().server(t)
	src.Complete(t.Context(), srv.Conn(1), req)
	assert.Len(t, srv.Requested(), 1)

	for _, other := range []completion.Request{
		{Command: extUninstallName, Flag: "timeout", Positionals: []string{"good"}},
		{Command: extUninstallName, Index: 1},
		{Command: "other"},
	} {
		_, claimed := src.Claim(other)
		assert.False(t, claimed, "%+v", other)
	}
}
