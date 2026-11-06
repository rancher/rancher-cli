package token

import (
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deletedNames lists the token names of the deletes sent since before.
func deletedNames(f *testrancher.Facade, before int) []string {
	var names []string
	for _, d := range f.DeleteRequests()[before:] {
		names = append(names, d.Name)
	}
	return names
}

// TestRetireSkipsARecordOfAnotherServer asserts that a record naming a
// server other than the entry's current one is kept without a request,
// with a warning whose recovery logs in to the record's server; a record
// naming the entry's own server, or none, is retired as before.
func TestRetireSkipsARecordOfAnotherServer(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	other, same, bare, fresh := x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a")
	otherRec := record(other, "2026-09-01T00:00:00Z")
	otherRec.Server = "https://old.ex:9443"
	sameRec := record(same, "2026-09-02T00:00:00Z")
	sameRec.Server = config.CanonicalServerIdentity(x.url)
	x.seedRecords(t, []config.SupersededToken{otherRec, sameRec, record(bare, "2026-09-03T00:00:00Z")})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.ElementsMatch(t, []TokenRef{*same.Ref, *bare.Ref}, rep.Retired)
	assert.ElementsMatch(t, []string{same.Ref.Name, bare.Ref.Name}, deletedNames(x.f, before))
	recs := x.records(t)
	require.Len(t, recs, 1)
	assert.Equal(t, other.Ref.Name, recs[0].Ref.Name)
	assert.Nil(t, recs[0].LastAttempt)
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "https://old.ex:9443, which this entry no longer names")
	assert.Contains(t, rep.Warnings[0], "rancher auth login 'recover-old.ex-9443' --url 'https://old.ex:9443'")
}

// TestRetireDropsARecordExpiredPastTheMargin asserts that a record whose
// expiry passed more than an hour ago is dropped without a request, one of
// another server included; one inside the margin, and one with no expiry,
// are attempted.
func TestRetireDropsARecordExpiredPastTheMargin(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	long, recent, noExpiry, otherLong, fresh := x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a")
	now := time.Now().UTC()
	longRec := record(long, "2026-09-01T00:00:00Z")
	longRec.ExpiresAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
	recentRec := record(recent, "2026-09-02T00:00:00Z")
	recentRec.ExpiresAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	noExpiryRec := record(noExpiry, "2026-09-03T00:00:00Z")
	noExpiryRec.ExpiresAt = ""
	otherLongRec := record(otherLong, "2026-09-04T00:00:00Z")
	otherLongRec.ExpiresAt, otherLongRec.Server = now.Add(-3*time.Hour).Format(time.RFC3339), "https://old.ex"
	x.seedRecords(t, []config.SupersededToken{longRec, recentRec, noExpiryRec, otherLongRec})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.ElementsMatch(t, []string{recent.Ref.Name, noExpiry.Ref.Name}, deletedNames(x.f, before))
	assert.ElementsMatch(t, []TokenRef{*recent.Ref, *noExpiry.Ref}, rep.Retired)
	assert.Empty(t, rep.Warnings)
	assert.Empty(t, x.records(t), "the expired records are dropped")
}

// TestRetireSkipsAPastedCredential asserts that a login whose token was
// pasted attempts nothing and stamps nothing: the records wait for a login
// the CLI creates, and each warning says so, a record of another server's
// with that server in its recovery.
func TestRetireSkipsAPastedCredential(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	old, moved := x.credAs(t, "a"), x.credAs(t, "a")
	movedRec := record(moved, "2026-09-03T00:00:00Z")
	movedRec.Server = "https://old.ex"
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z"), movedRec})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, UserSupplied("pasted:secret"), x.records(t))
	assert.Empty(t, deletedNames(x.f, before))
	assert.Empty(t, rep.Retired)
	assert.Empty(t, rep.Failed)
	assert.Len(t, rep.Kept, 2)
	recs := x.records(t)
	require.Len(t, recs, 2)
	assert.Nil(t, recs[0].LastAttempt, "no stamp")
	require.Len(t, rep.Warnings, 2)
	assert.Contains(t, rep.Warnings[0], "it waits for a login the CLI creates")
	assert.Contains(t, rep.Warnings[0], clierr.RecoverySequenceAssumption)
	assert.Contains(t, rep.Warnings[1], "--url 'https://old.ex'", "a record of another server is recovered on its server")
}

// TestRetireKeepsARecordOfAServerThatCannotBeUsed asserts that a record whose
// server holds user information or does not parse is kept without a request,
// that its warning says the token stays on the server that issued it and how
// to delete it there, and that no command, no URL and no password is built
// from the record's server.
func TestRetireKeepsARecordOfAServerThatCannotBeUsed(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	other, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	rec := record(other, "2026-09-01T00:00:00Z")
	rec.Server = "https://u:pwzq7@old.ex:abc"
	x.seedRecords(t, []config.SupersededToken{rec})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Empty(t, deletedNames(x.f, before))
	assert.Len(t, rep.Kept, 1)
	require.Len(t, rep.Warnings, 1)
	name := clierr.ShellQuote(other.Ref.Name)
	require.NotEmpty(t, other.UserID)
	owner := clierr.ShellQuote(other.UserID)
	assert.Equal(t, "the superseded token "+other.Ref.String()+" was not retired: it is a token of <unusable URL>, which this entry no longer names; "+
		"the server recorded for token "+name+" cannot be used: it is not a valid URL; the token stays on the server that issued it; "+
		"delete it there as user "+owner+" while signed in to that server: "+clierr.Rancher+" delete token "+name+" --apply --yes", rep.Warnings[0])
	assert.NotContains(t, rep.Warnings[0], "auth login")
	assert.NotContains(t, rep.Warnings[0], "pwzq7")
}

// TestRetirePastedCredentialKeepsARecordOfAServerThatCannotBeUsed asserts that
// a record of a server that cannot be used gets only the text for that
// server: no login of the CLI retires it, so the wait for one is not said.
func TestRetirePastedCredentialKeepsARecordOfAServerThatCannotBeUsed(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	other := x.credAs(t, "a")
	rec := record(other, "2026-09-01T00:00:00Z")
	rec.Server = "unusable:https://old.ex:abc"
	x.seedRecords(t, []config.SupersededToken{rec})

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, UserSupplied("pasted:secret"), x.records(t))
	require.Len(t, rep.Warnings, 1)
	name := clierr.ShellQuote(other.Ref.Name)
	require.NotEmpty(t, other.UserID)
	owner := clierr.ShellQuote(other.UserID)
	assert.Equal(t, "the superseded token "+other.Ref.String()+" was not retired: the server recorded for token "+name+
		" cannot be used: it is not a valid URL; the token stays on the server that issued it; delete it there as user "+owner+" while signed in to that server: "+
		clierr.Rancher+" delete token "+name+" --apply --yes", rep.Warnings[0])
}

// TestRetireJudgesARecordByTheIdentityOfItsServer asserts that a record whose
// server is the entry's own plus user information is the entry's token and is
// retired, and that one at another server plus user information is kept with
// a recovery sequence built from the canonical URL: no text as stored, no
// password, in the warning.
func TestRetireJudgesARecordByTheIdentityOfItsServer(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	same, other, fresh := x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a")
	sameRec := record(same, "2026-09-01T00:00:00Z")
	sameRec.Server = strings.Replace(x.url, "://", "://u:pwzq7@", 1)
	otherRec := record(other, "2026-09-02T00:00:00Z")
	otherRec.Server = "https://u:pwzq7@old.ex:9443"
	x.seedRecords(t, []config.SupersededToken{sameRec, otherRec})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Equal(t, []TokenRef{*same.Ref}, rep.Retired)
	assert.Equal(t, []string{same.Ref.Name}, deletedNames(x.f, before))
	require.Len(t, rep.Warnings, 1)
	for _, command := range clierr.RecoverySequence("https://old.ex:9443", *other.Ref) {
		assert.Contains(t, rep.Warnings[0], command)
	}
	assert.NotContains(t, rep.Warnings[0], "pwzq7")
	assert.NotContains(t, rep.Warnings[0], "cannot be used")
}

// TestRetirePastedCredentialGivesTheSequenceOfARecordWithUserinfo asserts the
// pasted branch builds the recovery sequence of a record at another server
// plus user information from the canonical URL as well.
func TestRetirePastedCredentialGivesTheSequenceOfARecordWithUserinfo(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	other := x.credAs(t, "a")
	rec := record(other, "2026-09-01T00:00:00Z")
	rec.Server = "https://u:pwzq7@old.ex:9443"
	x.seedRecords(t, []config.SupersededToken{rec})

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, UserSupplied("pasted:secret"), x.records(t))
	require.Len(t, rep.Warnings, 1)
	for _, command := range clierr.RecoverySequence("https://old.ex:9443", *other.Ref) {
		assert.Contains(t, rep.Warnings[0], command)
	}
	assert.NotContains(t, rep.Warnings[0], "pwzq7")
}
