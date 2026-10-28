package register

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// writableCfg builds a Config backed by a real temp file, so cfg.Write()
// calls in commitCreate/commitUpdate succeed and tests can assert on-disk
// byte-identity for the "no config write" cases.
func writableCfg(t *testing.T, servers map[string]config.Server, current string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Servers: servers, CurrentServer: current}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return cfg
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func TestCanonicalizeServerURLNormalizes(t *testing.T) {
	canonical, err := CanonicalizeServerURL("HTTPS://Example.com:443/ignored", "")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", canonical)
}

func TestCanonicalizeServerURLInvalidURL(t *testing.T) {
	_, err := CanonicalizeServerURL("not-a-url", "")
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestCanonicalizeServerURLRefusesAndLeavesTheURLOut asserts that a URL with
// userinfo, one that does not parse and one with an unsupported scheme are
// refused as VALIDATION, that the message names where the URL came from (--url,
// or the stored entry) and that no part of the URL appears in it.
func TestCanonicalizeServerURLRefusesAndLeavesTheURLOut(t *testing.T) {
	const canary = "CaNaRy-9x"
	const (
		userinfo = "it holds a user name or password"
		badPort  = "it is not a valid URL"
		noScheme = "it must be absolute (scheme://host)"
		scheme   = "its scheme is not http or https"
	)
	for _, tc := range []struct{ name, raw, reason string }{
		{"userinfo", "https://u:" + canary + "@rancher.invalid", userinfo},
		{"a user name alone", "https://" + canary + "@rancher.invalid", userinfo},
		{"userinfo and a bad port", "https://u:" + canary + "@rancher.invalid:abc", badPort},
		{"a slash in the password", "https://u:CaNaRy/9x@rancher.invalid:abc", badPort},
		{"no scheme", "u:" + canary + "@rancher.invalid:1", noScheme},
		{"a scheme-relative URL", "//u:" + canary + "@rancher.invalid", userinfo},
		{"an unsupported scheme", "ftp://rancher.invalid/" + canary, scheme},
	} {
		repair := "give a URL like https://host, without a user name or password"
		if tc.reason == scheme {
			repair = "use http:// or https://"
		}
		for _, entry := range []string{"", "prod"} {
			t.Run(tc.name+" "+entry, func(t *testing.T) {
				_, err := CanonicalizeServerURL(tc.raw, entry)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				if entry == "" {
					assert.Equal(t, "the server URL in --url cannot be used: "+tc.reason+"; "+repair, ce.Message)
				} else {
					assert.Equal(t, "the server URL in server entry 'prod' cannot be used: "+tc.reason+"; change the entry's URL with "+
						clierr.Rancher+" config update-server 'prod' --url <url>", ce.Message)
				}
				lower := strings.ToLower(ce.Message)
				for _, part := range []string{"canary", "rancher.invalid"} {
					assert.NotContains(t, lower, part)
				}
			})
		}
	}
}

// TestPrepareRegistrationRejectsUnsupportedScheme asserts that a scheme
// CanonicalServerURL accepts but config.Effective's credential-read path
// would later refuse (anything but http/https) must never reach a
// committed entry or token - it would otherwise commit successfully and
// then silently withhold the stored credential on every later command,
// surfacing as an unexplained NOT_LOGGED_IN.
func TestPrepareRegistrationRejectsUnsupportedScheme(t *testing.T) {
	_, err := Prepare(context.Background(), nil, "ftp://host", "")
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "the server URL in --url cannot be used: its scheme is not http or https; use http:// or https://", ce.Message)
}

func TestPrepareRegistrationResolvesPathCAOnlyForHTTPS(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)

	p, err := Prepare(context.Background(), nil, "https://example.com", path)
	require.NoError(t, err)
	assert.Equal(t, cmdtest.CAPem1, p.CABytes)

	p2, err := Prepare(context.Background(), nil, "http://example.com", path)
	require.NoError(t, err)
	assert.Empty(t, p2.CABytes, "a stale/unreadable stored CA path must never break an http registration")
}

func TestPrepareRegistrationTLSTrustIsFatal(t *testing.T) {
	fatal := clierr.New(clierr.CodeTLSTrust, "untrusted")
	preflight := func(context.Context, string, string) (string, error) { return "", fatal }
	_, err := Prepare(context.Background(), preflight, "https://example.com", "")
	assert.Same(t, fatal, err)
}

func TestPrepareRegistrationVersionWarning(t *testing.T) {
	preflight := func(context.Context, string, string) (string, error) { return "2.10.0", nil }
	p, err := Prepare(context.Background(), preflight, "https://example.com", "")
	require.NoError(t, err)
	assert.Contains(t, p.VersionWarning, config.MinRancherVersion)
}

func TestPrepareRegistrationUnknownVersionOnConnectivityFailure(t *testing.T) {
	// TLSPreflight's own warn-only posture: a connectivity failure
	// returns ("", nil), never an error.
	preflight := func(context.Context, string, string) (string, error) { return "", nil }
	p, err := Prepare(context.Background(), preflight, "https://example.com", "")
	require.NoError(t, err)
	assert.Equal(t, config.UnknownServerVersionWarning, p.VersionWarning)
}

// --- commitCreate ---

func TestCommitCreateAbsentNameCreatesEntry(t *testing.T) {
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: ""}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.True(t, out.Created)
	assert.Equal(t, "qa1", out.CurrentServer)
	assert.True(t, out.PointerRepaired, "bootstrap: pointer was empty")
	got := cfg.Servers["qa1"]
	assert.Len(t, got.CacheBindingID, 32, "a new entry starts with a cache binding id of its own")
	got.CacheBindingID = ""
	assert.Equal(t, config.Server{URL: "https://qa1.example.com"}, got)
}

func TestCommitCreateAbsentNamePathRotationBetweenProbeAndCommit(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: path, CABytes: cmdtest.CAPem1}

	require.NoError(t, os.WriteFile(path, []byte(cmdtest.CAPem2), 0o600))

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Empty(t, cfg.Servers, "no entry write on CA rotation conflict")
	assert.Empty(t, cfg.CurrentServer, "no pointer write on CA rotation conflict")
	assert.Empty(t, ce.Hint, "this seam is shared by 3 callers and cannot name the right verb - no hint, guidance stays in the message")
}

func TestCommitCreateAbsentNamePathDisappearsBetweenProbeAndCommit(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: path, CABytes: cmdtest.CAPem1}

	require.NoError(t, os.Remove(path))

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Empty(t, cfg.Servers)
	// An I/O resolution failure (as opposed to a genuine content mismatch)
	// must name the underlying error, so an operator can tell a transient
	// read failure from real concurrent drift instead of guessing from
	// unrelated timing.
	assert.Contains(t, ce.Message, fmt.Sprintf("reading CA file %q", path),
		"the underlying resolution error is named, not just \"CA content changed\"")
}

func TestCommitCreateExistingIdenticalIsEntryNoOpPointerValid(t *testing.T) {
	servers := map[string]config.Server{
		"qa1":  {URL: "https://qa1.example.com"},
		"prod": {URL: "https://prod.example.com"},
	}
	cfg := writableCfg(t, servers, "prod")
	before := readBytes(t, cfg.Path())
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: ""}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.False(t, out.Created)
	assert.False(t, out.PointerRepaired)
	assert.Equal(t, "prod", out.CurrentServer)
	assert.Equal(t, before, readBytes(t, cfg.Path()), "no config write on a full no-op")
}

func TestCommitCreateExistingIdenticalPointerEmptyRepairsPointerOnly(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "")

	out, err := commitCreate(cfg, "qa1", Probe{CanonicalURL: "https://qa1.example.com"})
	require.NoError(t, err)
	assert.False(t, out.Created)
	assert.True(t, out.PointerRepaired)
	assert.Equal(t, "qa1", out.CurrentServer)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com"}, cfg.Servers["qa1"], "entry not rewritten")
}

func TestCommitCreateExistingIdenticalPointerDanglingRepairsPointer(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "ghost")

	out, err := commitCreate(cfg, "qa1", Probe{CanonicalURL: "https://qa1.example.com"})
	require.NoError(t, err)
	assert.True(t, out.PointerRepaired)
	assert.Equal(t, "qa1", out.CurrentServer)
}

func TestCommitCreateExistingIdenticalPathRotationConflicts(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: path}}
	cfg := writableCfg(t, servers, "qa1")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: path, CABytes: cmdtest.CAPem1}

	require.NoError(t, os.WriteFile(path, []byte(cmdtest.CAPem2), 0o600))

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com", CA: path}, cfg.Servers["qa1"])
}

func TestCommitCreateExistingDifferentURLConflictsWithUpdateHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://old.example.com"}}
	cfg := writableCfg(t, servers, "")
	probe := Probe{CanonicalURL: "https://new.example.com"}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "https://old.example.com")
	assert.Contains(t, ce.Message, "choose a different name")
	assert.Equal(t, "rancher config update-server 'qa1' --url 'https://new.example.com'", ce.Hint)
	assert.Equal(t, config.Server{URL: "https://old.example.com"}, cfg.Servers["qa1"], "never a silent upsert")
}

// TestCommitCreateExistingSameURLDifferentCAConflictsWithCacertHint asserts
// that a same-URL/different-CA conflict must hint --cacert, never --url -
// the URL didn't change, so an --url hint would execute as a no-op and
// silently discard the trust change the caller asked for.
func TestCommitCreateExistingSameURLDifferentCAConflictsWithCacertHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: cmdtest.CAPem1}}
	cfg := writableCfg(t, servers, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: cmdtest.CAPem2, CABytes: cmdtest.CAPem2}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "different CA")
	assert.Equal(t, "rancher config update-server 'qa1' --cacert "+clierr.ShellQuote(cmdtest.CAPem2), ce.Hint)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com", CA: cmdtest.CAPem1}, cfg.Servers["qa1"], "never a silent upsert")
}

// TestCommitCreateExistingSameURLClearedCAConflictsWithClearCacertHint asserts
// the system-roots case of the same fix: update-server has no --cacert ""
// spelling, so the hint must be --clear-cacert, not an empty --cacert value.
func TestCommitCreateExistingSameURLClearedCAConflictsWithClearCacertHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: "/old.pem"}}
	cfg := writableCfg(t, servers, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: ""}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, "rancher config update-server 'qa1' --clear-cacert", ce.Hint)
}

func TestCommitCreateConcurrentIdenticalCreationIsIdempotent(t *testing.T) {
	// Prepare ran when "qa1" was absent; by commit time another
	// process created it with identical fields.
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com"}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.False(t, out.Created)
}

// --- commitUpdate ---

func srcOf(existing config.Server) UpdateSource {
	return UpdateSource{CanonicalURL: existing.URL, CASelector: existing.CA}
}

func TestCommitUpdateURLChangeClearsCredentialsAndWrites(t *testing.T) {
	existing := config.Server{URL: "https://old.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "qa1")
	probe := Probe{CanonicalURL: "https://new.example.com"}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.False(t, out.Unchanged)
	assert.Equal(t, []string{"url"}, out.ChangedFields)
	assert.True(t, out.CredentialsCleared)
	assert.Equal(t, config.Server{URL: "https://new.example.com"}, cfg.Servers["qa1"])
}

// TestCommitUpdateURLChangeKeepsTheOldServersRecords asserts that a URL
// change keeps the entry's superseded records as they are: naming the old
// server on each is the first write's, before the update.
func TestCommitUpdateURLChangeKeepsTheOldServersRecords(t *testing.T) {
	existing := config.Server{URL: "https://old.example.com", Provenance: clierr.ProvenanceDerived, SupersededTokens: []config.SupersededToken{
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-a"}, SupersededAt: "2026-10-06T00:00:00Z"},
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-b"}, SupersededAt: "2026-10-06T00:00:00Z", Server: "https://older.example.com"},
	}}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "qa1")
	_, err := commitUpdate(cfg, "qa1", srcOf(existing), Probe{CanonicalURL: "https://new.example.com"})
	require.NoError(t, err)
	recs := cfg.Servers["qa1"].SupersededTokens
	require.Len(t, recs, 2)
	assert.Equal(t, existing.SupersededTokens, recs)
	assert.Empty(t, cfg.Servers["qa1"].Provenance, "the credential's fields go")
}

func TestCommitUpdateCAOnlyChangeKeepsCredentials(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "qa1")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: cmdtest.CAPem1, CABytes: cmdtest.CAPem1}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.Equal(t, []string{"ca"}, out.ChangedFields)
	assert.False(t, out.CredentialsCleared)
}

func TestCommitUpdateSameValueIsNoOpButRepairsPointer(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com"}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.True(t, out.Unchanged)
	assert.Empty(t, out.ChangedFields)
	assert.False(t, out.CredentialsCleared)
	assert.True(t, out.PointerRepaired)
	assert.Equal(t, "qa1", out.CurrentServer)
}

func TestCommitUpdateEntryDeletedConflicts(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := Probe{CanonicalURL: "https://new.example.com"}

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

func TestCommitUpdateSourceURLDriftConflicts(t *testing.T) {
	original := config.Server{URL: "https://qa1.example.com"}
	// Between prepare and commit, a concurrent writer moved the source URL.
	cfg := writableCfg(t, map[string]config.Server{"qa1": {URL: "https://moved.example.com"}}, "")
	probe := Probe{CanonicalURL: "https://new.example.com"}

	_, err := commitUpdate(cfg, "qa1", srcOf(original), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, config.Server{URL: "https://moved.example.com"}, cfg.Servers["qa1"], "no mutation on source drift")
}

func TestCommitUpdateSourcePathRotationConflicts(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	existing := config.Server{URL: "https://qa1.example.com", CA: path}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	src := srcOf(existing)
	src.CABytes = cmdtest.CAPem1
	probe := Probe{CanonicalURL: "https://qa1.example.com"} // --clear-cacert

	require.NoError(t, os.WriteFile(path, []byte(cmdtest.CAPem2), 0o600))

	_, err := commitUpdate(cfg, "qa1", src, probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, existing, cfg.Servers["qa1"], "no mutation on source-byte drift")
}

func TestCommitUpdateSourcePathDisappearedConflicts(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	existing := config.Server{URL: "https://qa1.example.com", CA: path}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	src := srcOf(existing)
	src.CABytes = cmdtest.CAPem1
	probe := Probe{CanonicalURL: "https://qa1.example.com"}

	require.NoError(t, os.Remove(path))

	_, err := commitUpdate(cfg, "qa1", src, probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

func TestCommitUpdateResultPathRotatesBetweenProbeAndCommit(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: path, CABytes: cmdtest.CAPem1}

	require.NoError(t, os.WriteFile(path, []byte(cmdtest.CAPem2), 0o600))

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, existing, cfg.Servers["qa1"], "no mutation on result-byte drift")
}

func TestCommitUpdateResultPathDisappearsBetweenProbeAndCommit(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := Probe{CanonicalURL: "https://qa1.example.com", CASelector: path, CABytes: cmdtest.CAPem1}

	require.NoError(t, os.Remove(path))

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

// TestServerCandidatesAreSortedByName asserts every configured server is a
// candidate, in name order, whatever order the map yields.
func TestServerCandidatesAreSortedByName(t *testing.T) {
	servers := map[string]config.Server{}
	for _, n := range []string{"hotel", "alpha", "golf", "charlie", "echo", "bravo", "foxtrot", "delta"} {
		servers[n] = config.Server{URL: "https://" + n + ".example.com"}
	}
	cands := ServerCandidates(&config.Config{Servers: servers})
	require.Len(t, cands, 8)
	var ids []string
	for _, c := range cands {
		assert.Equal(t, c.ID, c.Name)
		ids = append(ids, c.ID)
	}
	assert.True(t, slices.IsSorted(ids), "candidates are in name order: %v", ids)
	assert.Equal(t, []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}, ids)
}

// TestServerNotFoundCarriesSortedServers asserts the NOT_FOUND error names
// the unknown server and lists every configured one, sorted.
func TestServerNotFoundCarriesSortedServers(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.Server{"b": {URL: "https://b"}, "a": {URL: "https://a"}}}
	e := ServerNotFound(cfg, "ghost")
	assert.Equal(t, clierr.CodeNotFound, e.Code)
	require.ErrorContains(t, e, "server not found: ghost")
	assert.Equal(t, []clierr.Candidate{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}, e.Candidates)
}
