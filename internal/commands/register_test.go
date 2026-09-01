package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCAPem1 = "-----BEGIN CERTIFICATE-----\nMIIB...one\n-----END CERTIFICATE-----\n"
const testCAPem2 = "-----BEGIN CERTIFICATE-----\nMIIB...two\n-----END CERTIFICATE-----\n"

func writeCAFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

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
	canonical, err := canonicalizeServerURL("HTTPS://Example.com:443/ignored")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", canonical)
}

func TestCanonicalizeServerURLInvalidURL(t *testing.T) {
	_, err := canonicalizeServerURL("not-a-url")
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestPrepareRegistrationRejectsUnsupportedScheme pins that a scheme
// CanonicalServerURL accepts but config.Effective's credential-read path
// would later refuse (anything but http/https) must never reach a
// committed entry or token — it would otherwise commit successfully and
// then silently withhold the stored credential on every later command,
// surfacing as an unexplained NOT_LOGGED_IN.
func TestPrepareRegistrationRejectsUnsupportedScheme(t *testing.T) {
	_, err := prepareRegistration(context.Background(), nil, "ftp://host", "")
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestPrepareRegistrationResolvesPathCAOnlyForHTTPS(t *testing.T) {
	path := writeCAFile(t, testCAPem1)

	p, err := prepareRegistration(context.Background(), nil, "https://example.com", path)
	require.NoError(t, err)
	assert.Equal(t, testCAPem1, p.caBytes)

	p2, err := prepareRegistration(context.Background(), nil, "http://example.com", path)
	require.NoError(t, err)
	assert.Empty(t, p2.caBytes, "a stale/unreadable stored CA path must never break an http registration")
}

func TestPrepareRegistrationTLSTrustIsFatal(t *testing.T) {
	fatal := clierr.New(clierr.CodeTLSTrust, "untrusted")
	preflight := func(context.Context, string, string) (string, error) { return "", fatal }
	_, err := prepareRegistration(context.Background(), preflight, "https://example.com", "")
	assert.Same(t, fatal, err)
}

func TestPrepareRegistrationVersionWarning(t *testing.T) {
	preflight := func(context.Context, string, string) (string, error) { return "2.10.0", nil }
	p, err := prepareRegistration(context.Background(), preflight, "https://example.com", "")
	require.NoError(t, err)
	assert.Contains(t, p.versionWarning, config.MinRancherVersion)
}

func TestPrepareRegistrationUnknownVersionOnConnectivityFailure(t *testing.T) {
	// tlsPreflightDefault's own warn-only posture: a connectivity failure
	// returns ("", nil), never an error.
	preflight := func(context.Context, string, string) (string, error) { return "", nil }
	p, err := prepareRegistration(context.Background(), preflight, "https://example.com", "")
	require.NoError(t, err)
	assert.Equal(t, config.UnknownServerVersionWarning, p.versionWarning)
}

// --- commitCreate ---

func TestCommitCreateAbsentNameCreatesEntry(t *testing.T) {
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: ""}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.True(t, out.created)
	assert.Equal(t, "qa1", out.currentServer)
	assert.True(t, out.pointerRepaired, "bootstrap: pointer was empty")
	assert.Equal(t, config.Server{URL: "https://qa1.example.com"}, cfg.Servers["qa1"])
}

func TestCommitCreateAbsentNamePathRotationBetweenProbeAndCommit(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: path, caBytes: testCAPem1}

	require.NoError(t, os.WriteFile(path, []byte(testCAPem2), 0o600))

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Empty(t, cfg.Servers, "no entry write on CA rotation conflict")
	assert.Empty(t, cfg.CurrentServer, "no pointer write on CA rotation conflict")
	assert.Empty(t, ce.Hint, "this seam is shared by 3 callers and cannot name the right verb — no hint, guidance stays in the message")
}

func TestCommitCreateAbsentNamePathDisappearsBetweenProbeAndCommit(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: path, caBytes: testCAPem1}

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
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: ""}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.False(t, out.created)
	assert.False(t, out.pointerRepaired)
	assert.Equal(t, "prod", out.currentServer)
	assert.Equal(t, before, readBytes(t, cfg.Path()), "no config write on a full no-op")
}

func TestCommitCreateExistingIdenticalPointerEmptyRepairsPointerOnly(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "")

	out, err := commitCreate(cfg, "qa1", registrationProbe{canonicalURL: "https://qa1.example.com"})
	require.NoError(t, err)
	assert.False(t, out.created)
	assert.True(t, out.pointerRepaired)
	assert.Equal(t, "qa1", out.currentServer)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com"}, cfg.Servers["qa1"], "entry not rewritten")
}

func TestCommitCreateExistingIdenticalPointerDanglingRepairsPointer(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "ghost")

	out, err := commitCreate(cfg, "qa1", registrationProbe{canonicalURL: "https://qa1.example.com"})
	require.NoError(t, err)
	assert.True(t, out.pointerRepaired)
	assert.Equal(t, "qa1", out.currentServer)
}

func TestCommitCreateExistingIdenticalPathRotationConflicts(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: path}}
	cfg := writableCfg(t, servers, "qa1")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: path, caBytes: testCAPem1}

	require.NoError(t, os.WriteFile(path, []byte(testCAPem2), 0o600))

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com", CA: path}, cfg.Servers["qa1"])
}

func TestCommitCreateExistingDifferentURLConflictsWithUpdateHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://old.example.com"}}
	cfg := writableCfg(t, servers, "")
	probe := registrationProbe{canonicalURL: "https://new.example.com"}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "https://old.example.com")
	assert.Contains(t, ce.Message, "choose a different name")
	assert.Equal(t, "rancher config update-server 'qa1' --url 'https://new.example.com'", ce.Hint)
	assert.Equal(t, config.Server{URL: "https://old.example.com"}, cfg.Servers["qa1"], "never a silent upsert")
}

// TestCommitCreateExistingSameURLDifferentCAConflictsWithCacertHint pins
// that a same-URL/different-CA conflict must hint --cacert, never --url —
// the URL didn't change, so an --url hint would execute as a no-op and
// silently discard the trust change the caller asked for.
func TestCommitCreateExistingSameURLDifferentCAConflictsWithCacertHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: testCAPem1}}
	cfg := writableCfg(t, servers, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: testCAPem2, caBytes: testCAPem2}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "different CA")
	assert.Equal(t, "rancher config update-server 'qa1' --cacert "+shquote(testCAPem2), ce.Hint)
	assert.Equal(t, config.Server{URL: "https://qa1.example.com", CA: testCAPem1}, cfg.Servers["qa1"], "never a silent upsert")
}

// TestCommitCreateExistingSameURLClearedCAConflictsWithClearCacertHint pins
// the system-roots case of the same fix: update-server has no --cacert ""
// spelling, so the hint must be --clear-cacert, not an empty --cacert value.
func TestCommitCreateExistingSameURLClearedCAConflictsWithClearCacertHint(t *testing.T) {
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com", CA: "/old.pem"}}
	cfg := writableCfg(t, servers, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: ""}

	_, err := commitCreate(cfg, "qa1", probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, "rancher config update-server 'qa1' --clear-cacert", ce.Hint)
}

func TestCommitCreateConcurrentIdenticalCreationIsIdempotent(t *testing.T) {
	// prepareRegistration ran when "qa1" was absent; by commit time another
	// process created it with identical fields.
	servers := map[string]config.Server{"qa1": {URL: "https://qa1.example.com"}}
	cfg := writableCfg(t, servers, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com"}

	out, err := commitCreate(cfg, "qa1", probe)
	require.NoError(t, err)
	assert.False(t, out.created)
}

// --- commitUpdate ---

func srcOf(existing config.Server) updateSource {
	return updateSource{canonicalURL: existing.URL, caSelector: existing.CA}
}

func TestCommitUpdateURLChangeClearsCredentialsAndWrites(t *testing.T) {
	existing := config.Server{URL: "https://old.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "qa1")
	probe := registrationProbe{canonicalURL: "https://new.example.com"}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.False(t, out.unchanged)
	assert.Equal(t, []string{"url"}, out.changedFields)
	assert.True(t, out.credentialsCleared)
	assert.Equal(t, config.Server{URL: "https://new.example.com"}, cfg.Servers["qa1"])
}

func TestCommitUpdateCAOnlyChangeKeepsCredentials(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "qa1")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: testCAPem1, caBytes: testCAPem1}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.Equal(t, []string{"ca"}, out.changedFields)
	assert.False(t, out.credentialsCleared)
}

func TestCommitUpdateSameValueIsNoOpButRepairsPointer(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com"}

	out, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	require.NoError(t, err)
	assert.True(t, out.unchanged)
	assert.Empty(t, out.changedFields)
	assert.False(t, out.credentialsCleared)
	assert.True(t, out.pointerRepaired)
	assert.Equal(t, "qa1", out.currentServer)
}

func TestCommitUpdateEntryDeletedConflicts(t *testing.T) {
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{}, "")
	probe := registrationProbe{canonicalURL: "https://new.example.com"}

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

func TestCommitUpdateSourceURLDriftConflicts(t *testing.T) {
	original := config.Server{URL: "https://qa1.example.com"}
	// Between prepare and commit, a concurrent writer moved the source URL.
	cfg := writableCfg(t, map[string]config.Server{"qa1": {URL: "https://moved.example.com"}}, "")
	probe := registrationProbe{canonicalURL: "https://new.example.com"}

	_, err := commitUpdate(cfg, "qa1", srcOf(original), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, config.Server{URL: "https://moved.example.com"}, cfg.Servers["qa1"], "no mutation on source drift")
}

func TestCommitUpdateSourcePathRotationConflicts(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	existing := config.Server{URL: "https://qa1.example.com", CA: path}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	src := srcOf(existing)
	src.caBytes = testCAPem1
	probe := registrationProbe{canonicalURL: "https://qa1.example.com"} // --clear-cacert

	require.NoError(t, os.WriteFile(path, []byte(testCAPem2), 0o600))

	_, err := commitUpdate(cfg, "qa1", src, probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, existing, cfg.Servers["qa1"], "no mutation on source-byte drift")
}

func TestCommitUpdateSourcePathDisappearedConflicts(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	existing := config.Server{URL: "https://qa1.example.com", CA: path}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	src := srcOf(existing)
	src.caBytes = testCAPem1
	probe := registrationProbe{canonicalURL: "https://qa1.example.com"}

	require.NoError(t, os.Remove(path))

	_, err := commitUpdate(cfg, "qa1", src, probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

func TestCommitUpdateResultPathRotatesBetweenProbeAndCommit(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: path, caBytes: testCAPem1}

	require.NoError(t, os.WriteFile(path, []byte(testCAPem2), 0o600))

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, existing, cfg.Servers["qa1"], "no mutation on result-byte drift")
}

func TestCommitUpdateResultPathDisappearsBetweenProbeAndCommit(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	existing := config.Server{URL: "https://qa1.example.com"}
	cfg := writableCfg(t, map[string]config.Server{"qa1": existing}, "")
	probe := registrationProbe{canonicalURL: "https://qa1.example.com", caSelector: path, caBytes: testCAPem1}

	require.NoError(t, os.Remove(path))

	_, err := commitUpdate(cfg, "qa1", srcOf(existing), probe)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}
