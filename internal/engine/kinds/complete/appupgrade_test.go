package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	upgradeCommand  = "app upgrade"
	upgradeAppsPath = "/k8s/clusters/c-x/apis/catalog.cattle.io/v1/namespaces/web/apps"
	upgradeAppPath  = upgradeAppsPath + "/my-up"
	upgradeRepos    = "/k8s/clusters/c-x/apis/catalog.cattle.io/v1/clusterrepos"
)

// upgradeIndex serves the versions of up-chart newest first, with a version
// of each kind the --version source decides on.
const upgradeIndex = `{"entries":{"up-chart":[
  {"version":"3.0.0"},
  {"version":"2.0.0","annotations":{"catalog.cattle.io/auto-install":"crd=1.0.0"}},
  {"version":"1.9.0","annotations":{"catalog.cattle.io/auto-install-gvr":"things.example.io"}},
  {"version":"1.8.0","annotations":{"catalog.cattle.io/namespace":"cattle-x"}},
  {"version":"1.7.0","annotations":{"catalog.cattle.io/release-name":"fleet"}},
  {"version":"1.6.0","annotations":{"catalog.cattle.io/namespace":"web"}},
  {"version":"1.5.0","annotations":{"catalog.cattle.io/release-name":"my-up"}},
  {"version":"1.0.0"}]}}`

const upgradeApp = `{"metadata":{"name":"my-up"},"spec":{"info":{"status":"deployed"},"chart":{"metadata":{"name":"up-chart","version":"1.2.0",` +
	`"annotations":{"catalog.cattle.io/ui-source-repo-type":"cluster","catalog.cattle.io/ui-source-repo":"rancher-charts"}}}}}`

func upgradeServer(t *testing.T, app string) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{
		upgradeAppsPath: `{"items":[` + app + `]}`,
		upgradeAppPath:  app,
		upgradeRepos:    `{"items":[{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}]}`,
		repoObjectPath:  enabledRepo,
		repoIndexPath:   upgradeIndex,
	})
}

func upgradeRequest(flag string, tail ...string) completion.Request {
	return completion.Request{
		Command: upgradeCommand, Flag: flag, ClusterID: "c-x",
		Tail:        append([]string{"--namespace", "web"}, tail...),
		Positionals: []string{"my-up"},
	}
}

// TestUpgradeSourceClaimsItsSitesOnly asserts the source claims the release
// positional, --repo, --chart and --version of app upgrade with the reads each
// needs, and declines every other site and command.
func TestUpgradeSourceClaimsItsSitesOnly(t *testing.T) {
	t.Parallel()
	src := UpgradeSource(upgradeCommand)
	two := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceTwo}
	one := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
	tests := []struct {
		name string
		req  completion.Request
		want completion.Requirements
	}{
		{"release", completion.Request{Command: upgradeCommand}, two},
		{"repo", completion.Request{Command: upgradeCommand, Flag: "repo"}, two},
		{"chart", completion.Request{Command: upgradeCommand, Flag: "chart"}, one},
		{"version", completion.Request{Command: upgradeCommand, Flag: "version"}, completion.ClusterThreeReads()},
	}
	for _, tt := range tests {
		reqs, ok := src.Claim(tt.req)
		require.True(t, ok, tt.name)
		assert.Equal(t, tt.want, reqs, tt.name)
	}
	for _, flag := range []string{"cluster", "namespace", "values", "history-max", "helm-timeout", "timeout", "description", "system-default-registry"} {
		_, ok := src.Claim(completion.Request{Command: upgradeCommand, Flag: flag})
		assert.False(t, ok, "flag %q", flag)
	}
	_, ok := src.Claim(completion.Request{Command: upgradeCommand, Index: 1})
	assert.False(t, ok, "a second positional")
	for _, command := range []string{"", "app install", "app uninstall", "upgrade"} {
		for _, flag := range []string{"", "repo", "chart", "version"} {
			_, ok := src.Claim(completion.Request{Command: command, Flag: flag})
			assert.False(t, ok, "command %q flag %q", command, flag)
		}
	}
	assert.Empty(t, src.Complete(t.Context(), upgradeServer(t, upgradeApp).Conn(3), upgradeRequest("namespace")).Names)
}

// TestUpgradeVersionSourceKeepsTheServedOrder asserts the offered versions
// come in the order Rancher serves them, with the version the app runs last
// when the index does not list it, and that three requests are enough.
func TestUpgradeVersionSourceKeepsTheServedOrder(t *testing.T) {
	t.Parallel()
	ls := upgradeServer(t, upgradeApp)
	conn := ls.Conn(3)
	got := UpgradeSource(upgradeCommand).Complete(t.Context(), conn, upgradeRequest("version"))
	assert.Equal(t, []string{"3.0.0", "1.6.0", "1.5.0", "1.0.0", "1.2.0"}, got.Names)
	assert.Equal(t, []string{upgradeAppPath, repoObjectPath, repoIndexPath + "?link=index"}, ls.Requested())
	assert.Equal(t, 3, conn.Used())
	assert.False(t, conn.Exceeded())
}

// TestUpgradeReleaseSourceStaysWithinTwoRequests asserts the release source
// reads the apps of the namespace and the ClusterRepos, and no more.
func TestUpgradeReleaseSourceStaysWithinTwoRequests(t *testing.T) {
	t.Parallel()
	ls := upgradeServer(t, upgradeApp)
	conn := ls.Conn(2)
	got := UpgradeSource(upgradeCommand).Complete(t.Context(), conn, completion.Request{
		Command: upgradeCommand, ClusterID: "c-x", Tail: []string{"--namespace", "web"},
	})
	assert.Equal(t, []string{"my-up"}, got.Names)
	assert.Equal(t, []string{upgradeAppsPath, upgradeRepos}, ls.Requested())
	assert.False(t, conn.Exceeded())
}

// TestUpgradeSourceReadsNothingWithoutWhatItNeeds asserts no request and no
// names for a line without the namespace or the release it reads, and for a
// namespace or release that is no path segment.
func TestUpgradeSourceReadsNothingWithoutWhatItNeeds(t *testing.T) {
	t.Parallel()
	src := UpgradeSource(upgradeCommand)
	tests := []struct {
		name string
		req  completion.Request
	}{
		{"release without a namespace", completion.Request{Command: upgradeCommand, ClusterID: "c-x"}},
		{"release without a cluster", completion.Request{Command: upgradeCommand, Tail: []string{"--namespace", "web"}}},
		{"release in a namespace that is no path segment", completion.Request{Command: upgradeCommand, ClusterID: "c-x", Tail: []string{"--namespace", "a/b"}}},
		{"chart without a release", completion.Request{Command: upgradeCommand, Flag: "chart", ClusterID: "c-x", Tail: []string{"--namespace", "web"}}},
		{"repo without a namespace", completion.Request{Command: upgradeCommand, Flag: "repo", ClusterID: "c-x", Positionals: []string{"my-up"}}},
		{"version for a release that is no path segment", completion.Request{Command: upgradeCommand, Flag: "version", ClusterID: "c-x", Tail: []string{"--namespace", "web"}, Positionals: []string{"a/b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := upgradeServer(t, upgradeApp)
			assert.Empty(t, src.Complete(t.Context(), ls.Conn(3), tt.req).Names)
			assert.Empty(t, ls.Requested())
		})
	}
}
