package configcmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
)

// TestRecordsOfAnEntryThatHoldsAnAtSignAfterTheHostShowNoHost asserts that for
// an entry whose URL holds an @ after the host, the records an earlier build
// stored with the host read from the URL (an ext token and a v3 token) show
// that server as unusable and name no part of it: in the status lines, the
// lines set-token-reuse writes for a plugin login entry, the status fields and
// the report of the entry, and in the administrator's line for the v3 token.
func TestRecordsOfAnEntryThatHoldsAnAtSignAfterTheHostShowNoHost(t *testing.T) {
	t.Parallel()
	hostRead := regexp.MustCompile(`\badmin\b|1234|recover-|pwzq7`)
	records := []config.SupersededToken{
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old88"}, UserID: "u-abc12", SupersededAt: "2026-10-02T00:00:00Z", Server: "https://admin:1234"},
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-old99"}, UserID: "u-abc12", SupersededAt: "2026-10-03T00:00:00Z", Server: "https://admin:1234"},
	}
	for _, prov := range []clierr.Provenance{"", clierr.ProvenanceExecLogin} {
		name := "x"
		if prov != "" {
			name = "exec-login-x"
		}
		s := config.Server{URL: "https://admin:1234/pwzq7@x", Provenance: prov, SupersededTokens: records}
		t.Run("provenance "+string(prov), func(t *testing.T) {
			t.Parallel()
			lines := strings.Join(recordLines(name, s, "  "), "\n")
			assert.NotRegexp(t, hostRead, lines)
			assert.Contains(t, lines, "an administrator logged in to <unusable URL> can delete it; run: ")
			assert.NotRegexp(t, hostRead, fmt.Sprintf("%+v", entrySection(name, s)))
			assert.NotRegexp(t, hostRead, fmt.Sprintf("%+v", entryReport(name, s)))
		})
	}
}

// TestAdministratorLineShowsAServerReadFromAnEntryURLAsUnusable asserts the
// administrator's line names the server a token records, except one read from
// an entry URL that holds an @ after the host.
func TestAdministratorLineShowsAServerReadFromAnEntryURLAsUnusable(t *testing.T) {
	t.Parallel()
	v3 := clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-x"}
	assert.Contains(t, administratorLine("https://hostzq7", "https://other.example", v3), "logged in to https://other.example can")
	assert.Contains(t, administratorLine("https://hostzq7", "https://hostzq7", v3), "logged in to https://hostzq7 can")
	assert.Contains(t, administratorLine("https://admin:1234/pwzq7@x", "https://admin:1234", v3), "logged in to <unusable URL> can")
}
