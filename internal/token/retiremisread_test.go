package token

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetireKeepsARecordStoredForAnEntryThatHeldAnAtSignAfterTheHost asserts
// that a login the CLI created keeps, without a request, a record whose
// stored server is the unusable identity an update of an entry with an @ after
// the host stores, and that its warning shows the server as unusable, holds no
// part of the URL and builds no login sequence.
func TestRetireKeepsARecordStoredForAnEntryThatHeldAnAtSignAfterTheHost(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	rec := record(old, "2026-09-01T00:00:00Z")
	rec.Server = config.RecordServerIdentity(config.RecordSite{PriorEntryURL: "https://admin:1234/pwzq7@x"}, "", "https://admin:1234")
	x.seedRecords(t, []config.SupersededToken{rec})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Empty(t, deletedNames(x.f, before), "no request is sent for the record")
	assert.Empty(t, rep.Retired)
	assert.Len(t, rep.Kept, 1)
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "it is a token of <unusable URL>, which this entry no longer names")
	assert.Contains(t, rep.Warnings[0], "the server recorded for token "+"'"+old.Ref.Name+"' cannot be used: ")
	for _, part := range []string{"admin", "1234", "pwzq7", "recover-", "auth login"} {
		assert.NotContains(t, rep.Warnings[0], part)
	}
}
