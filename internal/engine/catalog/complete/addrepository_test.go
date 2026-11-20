package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	versionPath = "/rancherversion"
	reposPath   = "/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos"

	partnerURL = "https://github.com/rancher/partner-extensions"
	primeURL   = "https://github.com/rancher/ui-plugin-charts"
)

const command = "extension add-repository"

func repoItems(items ...string) string {
	body := `{"items":[`
	for i, it := range items {
		if i > 0 {
			body += ","
		}
		body += it
	}
	return body + `]}`
}

func repoJSON(name, specField, value, statusURL string) string {
	item := `{"metadata":{"name":"` + name + `"},"spec":{"` + specField + `":"` + value + `"}`
	if statusURL != "" {
		item += `,"status":{"url":"` + statusURL + `"}`
	}
	return item + `}`
}

func versionJSON(prime string) string { return `{"Version":"v2.15.3","RancherPrime":"` + prime + `"}` }

func positional(index int) completion.Request {
	return completion.Request{Command: command, Index: index}
}

// TestAddRepositorySourceClaim asserts the source claims only the first
// positional of the command, as two reads of the management cluster with a
// management token, and declines flags, other positionals and other commands.
func TestAddRepositorySourceClaim(t *testing.T) {
	t.Parallel()
	src := AddRepositorySource(command)
	reqs, ok := src.Claim(positional(0))
	require.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceTwo}, reqs)
	for _, req := range []completion.Request{
		positional(1),
		{Command: command, Flag: "apply"},
		{Command: "extension list"},
		{Command: "app install"},
		{},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestAddRepositorySourceOffersWhatTheCommandAccepts asserts the names offered
// for each edition and each ClusterRepo already on the server: the Prime
// repository only on Prime, and no repository whose URL a ClusterRepo reads by
// status.url, spec.gitRepo or spec.url, or whose name a ClusterRepo has.
func TestAddRepositorySourceOffersWhatTheCommandAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
		repos   string
		want    []string
	}{
		{"prime, nothing added", versionJSON("true"), repoItems(), []string{"partner-extensions", "rancher-ui-plugins"}},
		{"community, nothing added", versionJSON("false"), repoItems(), []string{"partner-extensions"}},
		{"unknown edition", `{"Version":"v2.15.3"}`, repoItems(), []string{"partner-extensions"}},
		{"unreadable edition", `not json`, repoItems(), []string{"partner-extensions"}},
		{"prime, partner added by git repo", versionJSON("true"), repoItems(repoJSON("p", "gitRepo", partnerURL, "")), []string{"rancher-ui-plugins"}},
		{"prime, partner added by url", versionJSON("true"), repoItems(repoJSON("p", "url", partnerURL, "")), []string{"rancher-ui-plugins"}},
		{"prime, partner added by status url", versionJSON("true"), repoItems(repoJSON("p", "gitRepo", "https://example.com/x", partnerURL)), []string{"rancher-ui-plugins"}},
		{"prime, prime added", versionJSON("true"), repoItems(repoJSON("q", "gitRepo", primeURL, "")), []string{"partner-extensions"}},
		{"prime, both added", versionJSON("true"), repoItems(repoJSON("p", "gitRepo", partnerURL, ""), repoJSON("q", "gitRepo", primeURL, "")), nil},
		{"the name is taken by another URL", versionJSON("true"), repoItems(repoJSON("partner-extensions", "gitRepo", "https://example.com/x", "")), []string{"rancher-ui-plugins"}},
		{"status url differs from the git repo", versionJSON("false"), repoItems(repoJSON("p", "gitRepo", partnerURL, "https://example.com/x")), []string{"partner-extensions"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := completiontest.New(t, map[string]string{versionPath: tt.version, reposPath: tt.repos})
			conn := srv.Conn(2)
			got := AddRepositorySource(command).Complete(t.Context(), conn, positional(0))
			assert.ElementsMatch(t, tt.want, got.Names)
			assert.False(t, conn.Exceeded())
			assert.Equal(t, []string{versionPath, reposPath}, srv.Requested(), "the edition, then the ClusterRepos: two requests")
		})
	}
}

// TestAddRepositorySourceOffersNothingWhenTheListFails asserts a ClusterRepo
// list that cannot be read offers nothing, whatever the edition says, and that
// a failed version read still lets the list decide.
func TestAddRepositorySourceOffersNothingWhenTheListFails(t *testing.T) {
	t.Parallel()
	srv := completiontest.New(t, map[string]string{versionPath: versionJSON("true"), reposPath: "not json"})
	got := AddRepositorySource(command).Complete(t.Context(), srv.Conn(2), positional(0))
	assert.Empty(t, got.Names)

	srv = completiontest.NewStatus(t, 500)
	got = AddRepositorySource(command).Complete(t.Context(), srv.Conn(2), positional(0))
	assert.Empty(t, got.Names)
}
