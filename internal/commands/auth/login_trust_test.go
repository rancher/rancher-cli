package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

func TestTrustCardSelfIdentifiesAsDynamiclistenerCA(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=dynamiclistener-ca"}
	assert.Contains(t, trustCard(d, ""), "identifies itself as dynamiclistener-ca")
}

func TestTrustCardDoesNotSelfIdentify(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example"}
	assert.Contains(t, trustCard(d, ""), "the certificate is not Rancher's generated CA")
}

func TestTrustCardFirstContactCarriesNoWarning(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Fingerprint: "newfp"}
	assert.NotContains(t, trustCard(d, ""), "warning:")
}

func TestTrustCardChangedIdentityWarnsWithBothFingerprints(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Fingerprint: "newfp"}
	card := trustCard(d, "oldfp")
	assert.Contains(t, card, "warning:")
	assert.Contains(t, card, "oldfp")
}

func TestTrustCardSameFingerprintCarriesNoWarning(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Fingerprint: "samefp"}
	assert.NotContains(t, trustCard(d, "samefp"), "warning:",
		"identical prior and candidate fingerprints is not a changed identity")
}

// callCountingPrompter wraps a *ui.Prompter and counts Prompt/Notice calls
// so a test can assert guided trust never engaged at all - distinct from
// engaging and being declined, which reads identically from the returned
// error alone.
type callCountingPrompter struct {
	*ui.Prompter
	prompts int
}

func (p *callCountingPrompter) Prompt(ctx context.Context, msg string) (string, error) {
	p.prompts++
	return p.Prompter.Prompt(ctx, msg)
}

// driftingPrompter answers the consent prompt "y" but, on its first call,
// first mutates the config directory - simulating a concurrent writer
// changing the entry during the interactive gap between the preflight and
// the locked commit.
type driftingPrompter struct {
	*ui.Prompter
	mutate  func()
	mutated bool
}

func (p *driftingPrompter) Prompt(ctx context.Context, msg string) (string, error) {
	if !p.mutated {
		p.mutated = true
		p.mutate()
	}
	return "y", nil
}

// noticeCapturingPrompter records every Notice() call so a test can assert
// on the exact card text shown to the operator.
type noticeCapturingPrompter struct {
	*ui.Prompter
	notices []string
}

func (p *noticeCapturingPrompter) Notice(msg string) {
	p.notices = append(p.notices, msg)
	p.Prompter.Notice(msg)
}

// TestLoginPlainUnknownAuthorityCardLabelsLeafOnlyServingCertificate asserts
// that the interactive consent card, not just the error text, gets
// the leaf-only wording: the fixture here is a self-signed leaf with no
// separate CA, so the card the operator actually consents to must label
// the fingerprint a serving certificate, never a certificate authority.
func TestLoginPlainUnknownAuthorityCardLabelsLeafOnlyServingCertificate(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	base := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	p := &noticeCapturingPrompter{Prompter: base}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)

	require.NotEmpty(t, p.notices)
	assert.Contains(t, p.notices[0], "serving certificate:")
	assert.NotContains(t, p.notices[0], "certificate authority:")
	assert.NotContains(t, p.notices[0], "warning:", "a first-contact entry has no prior identity to warn about")
}

// seedServerCAFingerprint sets an already-seeded entry's recorded
// CAFingerprint directly - the state a prior guided-trust consent would
// have left, without running that consent flow again.
func seedServerCAFingerprint(t *testing.T, dir, name, fingerprint string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	entry := cfg.Servers[name]
	entry.CAFingerprint = fingerprint
	cfg.Servers[name] = entry
	require.NoError(t, cfg.Write())
}

// TestLoginPlainChangedIdentityConsentWarnsAndReplacesFingerprint asserts
// the SSH-changed-key moment: a re-prompt against an entry with a recorded
// consent must open with a warning naming the previously recorded
// fingerprint next to the new candidate's, and accepting replaces it.
func TestLoginPlainChangedIdentityConsentWarnsAndReplacesFingerprint(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	const oldFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	seedServerCAFingerprint(t, dir, "prod", oldFingerprint)
	cmd := tlsLoginCmd(dir, store)
	base := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	p := &noticeCapturingPrompter{Prompter: base}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)

	require.NotEmpty(t, p.notices)
	assert.Contains(t, p.notices[0], "warning:")
	assert.Contains(t, p.notices[0], oldFingerprint)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.NotEqual(t, oldFingerprint, cfg.Servers["prod"].CAFingerprint,
		"accepting the new candidate must replace the stale recorded fingerprint")
}

// TestLoginPlainChangedIdentityDeclineKeepsByteIdenticalRefusal asserts
// that a decline on the changed-identity warning path is exactly as
// invisible as any other decline: the refusal is unchanged, and neither
// the CA nor the recorded fingerprint move.
func TestLoginPlainChangedIdentityDeclineKeepsByteIdenticalRefusal(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	const oldFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	seedServerCAFingerprint(t, dir, "prod", oldFingerprint)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("n"), false, true)
	in := cmdtest.Input{Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)

	nonInteractive := cmdtest.StdinPrompter("id:sec\n")
	nonInteractiveIn := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, wantErr := cmd.Execute(t.Context(), nonInteractiveIn, nonInteractive)

	var gotCE, wantCE *clierr.Error
	require.ErrorAs(t, err, &gotCE)
	require.ErrorAs(t, wantErr, &wantCE)
	assert.Equal(t, wantCE.Code, gotCE.Code)
	assert.Equal(t, wantCE.Message, gotCE.Message)
	assert.Equal(t, wantCE.Hint, gotCE.Hint)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, oldFingerprint, cfg.Servers["prod"].CAFingerprint, "a decline must never move the recorded fingerprint")
}

// TestLoginPlainPreGuidedEntryFallsBackToFirstContact asserts that an
// entry with an existing CA but no recorded CAFingerprint - one that
// arrived via an explicit --cacert, never through guided trust - reads as
// first contact: there is no recorded identity to compare against, so no
// changed-identity warning is shown, even though the entry is not new.
func TestLoginPlainPreGuidedEntryFallsBackToFirstContact(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	// A stale explicit CA that no longer verifies (a different, unrelated
	// certificate) - CAFingerprint stays empty, exactly as an explicit
	// --cacert entry always leaves it.
	stale := selfSignedFixture(t, "unrelated.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	seedServer(t, dir, "prod", srv.URL, stale.pem, false)
	cmd := tlsLoginCmd(dir, store)
	base := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	p := &noticeCapturingPrompter{Prompter: base}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)

	require.NotEmpty(t, p.notices)
	assert.NotContains(t, p.notices[0], "warning:")
}

// TestLoginPlainReverifyConnectivityFailureIsTreatedAsFailure asserts that
// the consent re-verify's own non-fatal, warn-only posture for a
// connectivity failure (DNS/refused/timeout) does NOT read as a silent
// pass here: "the candidate becomes the verified CA" must be proven, and a
// re-verify that could not run proves nothing.
func TestLoginPlainReverifyConnectivityFailureIsTreatedAsFailure(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)

	calls := 0
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(ctx context.Context, serverURL, caPEM string) (string, error) {
			calls++
			if calls == 1 {
				return register.TLSPreflight(ctx, serverURL, caPEM)
			}
			// The re-verify call: simulate the real preflight's own
			// swallowed connectivity failure, ("", nil).
			return "", nil
		},
	}
	p := ui.New(&bytes.Buffer{}, newLineReader("y"), false, true)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	require.ErrorContains(t, err, "could not be confirmed")
	require.ErrorContains(t, err, "fingerprint", "must describe the just-consented candidate")
	assert.NotContains(t, err.Error(), "--cacert", "the operator holds no file for a certificate that arrived over the wire")
	assert.NotContains(t, err.Error(), "config update-server", "must never be reframed as a failure of the entry's saved CA")
	assert.Equal(t, 2, calls, "the re-verify must actually run")

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	require.Error(t, tokErr, "an unconfirmed candidate must never persist a token")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.Servers["prod"].CA)
}

func TestLoginPlainUnknownAuthorityConsentAcceptedPersistsCAAndFingerprint(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	res, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	entry := cfg.Servers["prod"]
	assert.Contains(t, entry.CA, "-----BEGIN CERTIFICATE-----")
	assert.NotEmpty(t, entry.CAFingerprint)
	assert.Len(t, entry.CAFingerprint, 64, "SHA-256 lowercase hex")
}

func TestLoginPlainUnknownAuthorityDeclineKeepsByteIdenticalRefusal(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("n"), false, true)
	in := cmdtest.Input{Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)

	// Build the exact non-interactive refusal for comparison.
	nonInteractive := cmdtest.StdinPrompter("id:sec\n")
	nonInteractiveIn := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, wantErr := cmd.Execute(t.Context(), nonInteractiveIn, nonInteractive)

	var gotCE, wantCE *clierr.Error
	require.ErrorAs(t, err, &gotCE)
	require.ErrorAs(t, wantErr, &wantCE)
	assert.Equal(t, wantCE.Code, gotCE.Code)
	assert.Equal(t, wantCE.Message, gotCE.Message)
	assert.Equal(t, wantCE.Hint, gotCE.Hint)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	require.Error(t, tokErr, "a decline must never persist a token")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.Servers["prod"].CA, "a decline must never persist a CA")
}

func TestLoginPlainUnknownAuthorityExplicitNoInputNeverPrompts(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	// isTTY=true but noInput=true (--no-input on an attached terminal):
	// CanPrompt() is false, so guided trust must never engage even though a
	// session is nominally interactive.
	base := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), true, true)
	p := &callCountingPrompter{Prompter: base}
	in := cmdtest.Input{Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 0, p.prompts, "an explicit --no-input session must never be prompted")
}

func TestLoginPlainUnknownAuthorityEntryDriftDuringConsentReturnsConflict(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	base := ui.New(&bytes.Buffer{}, newLineReader("id:sec"), false, true)
	p := &driftingPrompter{Prompter: base, mutate: func() {
		seedServer(t, dir, "prod", srv.URL, "-----BEGIN CERTIFICATE-----concurrent", false)
	}}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----concurrent", cfg.Servers["prod"].CA,
		"the concurrent writer's CA must survive - the consented candidate must never overwrite it")
}

func TestLoginPlainUnknownAuthorityTokenWriteFailureAbortsBeforeConfigWrite(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, realStore := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	boom := errors.New("store boom")
	store := &controllableStore{CredStore: realStore, failOnCall: map[int]error{1: boom}}
	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: register.TLSPreflight,
	}
	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	require.ErrorIs(t, err, boom)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.Servers["prod"].CA, "an aborted token write must never reach the config write")
}

// This test does not run in parallel: it mutates the package-level
// trustCommitConfigWriteHookForTest, which every other test in this file
// would otherwise observe while it is set.
func TestLoginPlainUnknownAuthorityCompensationFailureIsReported(t *testing.T) {
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, realStore := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	compensateErr := errors.New("compensate boom")
	store := &controllableStore{CredStore: realStore, failOnCall: map[int]error{2: compensateErr}}
	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: register.TLSPreflight,
	}

	configErr := errors.New("disk full")
	restore := setTrustCommitConfigWriteHookForTest(func() error { return configErr })
	defer restore()

	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	ce, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
	assert.Equal(t, clierr.CredentialCompensationFailed, d.CredentialState)
	require.NotNil(t, d.CredentialRecovery)
	assert.Equal(t, clierr.BackendFile, d.CredentialRecovery.Backend)
	assert.Equal(t, compensateErr.Error(), d.CredentialRecovery.CompensationError)
	assert.Contains(t, d.Primary.Message, configErr.Error())
	assert.Equal(t, config.LoginHintFor("prod"), d.Primary.Hint)
	assert.Equal(t, clierr.Rancher+" auth logout "+clierr.ShellQuote("prod"), d.CredentialRecovery.Command)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.Servers["prod"].CA, "the failed config write must never be observed as applied")
}

func TestLoginDelegatedNewEntryGuidedTrustAcceptedCommitsCAAndFingerprint(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	in := cmdtest.Input{Str: map[string]string{"url": srv.URL}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"newentry"}}
	res, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	entry := cfg.Servers["newentry"]
	assert.Contains(t, entry.CA, "-----BEGIN CERTIFICATE-----")
	assert.Len(t, entry.CAFingerprint, 64)
}

// TestLoginDelegatedDerivedNameGuidedTrustAcceptedCommitsCAAndFingerprint
// composes the derived-name login (no explicit name argument, derived from
// --url's host) with guided-trust consent: the entry lands under the
// derived name with the accepted candidate's CA and fingerprint.
func TestLoginDelegatedDerivedNameGuidedTrustAcceptedCommitsCAAndFingerprint(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	in := cmdtest.Input{Str: map[string]string{"url": srv.URL}, Flag: map[string]bool{"token-stdin": true}}
	res, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	entry, ok := cfg.Servers["127.0.0.1"]
	require.True(t, ok, "the derived name is the URL's bare host")
	assert.Contains(t, entry.CA, "-----BEGIN CERTIFICATE-----")
	assert.Len(t, entry.CAFingerprint, 64)
}

// TestLoginDelegatedKnownEntryRotatedCANeverAutoTrusts asserts the scope
// boundary: guided trust on the --url path applies only to a genuinely new
// entry. An already-registered entry re-run with --url and no --cacert
// keeps today's non-interactive refusal even in a promptable session - a
// rotated CA on a known entry is a plain-login (`auth login <name>`)
// concern, which runs the combined token+CA commit protocol instead.
func TestLoginDelegatedKnownEntryRotatedCANeverAutoTrusts(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	base := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	p := &callCountingPrompter{Prompter: base}
	in := cmdtest.Input{Str: map[string]string{"url": srv.URL}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 0, p.prompts, "a known entry's rotated CA must never auto-trust on the delegated path")
	assert.Contains(t, ce.Message, "rancher auth login 'prod'", "must point at the plain-login path that can actually re-establish trust")
	assert.Equal(t, clierr.Rancher+" auth login 'prod'", ce.Hint)
}

// controllableStore wraps a real CredStore and fails a chosen call ordinal
// on a counter shared by the token write and the compensation it hands
// back (call 1 is the write, call 2 the undo) - used to exercise the commit
// protocol's abort-before-config-write and compensation-failure paths,
// neither of which a real backend failure is practical to trigger on
// demand.
type controllableStore struct {
	config.CredStore
	failOnCall map[int]error
	calls      int
}

func (s *controllableStore) SetTokenWithOutcome(ref config.ServerRef, token string, prior config.PriorCredential) (config.CommitOutcome, error) {
	s.calls++
	if err, ok := s.failOnCall[s.calls]; ok {
		return config.CommitOutcome{Backend: clierr.BackendFile, State: config.CommitNotWritten, PriorSource: clierr.PriorUnknown}, err
	}
	out, err := s.CredStore.SetTokenWithOutcome(ref, token, prior)
	if inner := out.Compensate; inner != nil {
		out.Compensate = func() error {
			s.calls++
			if cerr, ok := s.failOnCall[s.calls]; ok {
				return cerr
			}
			return inner()
		}
	}
	return out, err
}

func (s *controllableStore) SetToken(ref config.ServerRef, token string) error {
	s.calls++
	if err, ok := s.failOnCall[s.calls]; ok {
		return err
	}
	return s.CredStore.SetToken(ref, token)
}

func (s *controllableStore) DeleteToken(ref config.ServerRef) error {
	s.calls++
	if err, ok := s.failOnCall[s.calls]; ok {
		return err
	}
	return s.CredStore.DeleteToken(ref)
}

// setTrustCommitConfigWriteHookForTest fails the persistence primitive's
// guided-trust config write and returns a restore func; every other write
// step of the same login proceeds normally.
func setTrustCommitConfigWriteHookForTest(fn func() error) (restore func()) {
	return config.SetPersistWriteHookForTest(func(step string) error {
		if step != "trust" {
			return nil
		}
		return fn()
	})
}

// TestTrustCardUsesPlainWords asserts the trust card's three explanations in
// plain words, for a certificate that is not Rancher's own, a changed
// certificate and the closing line, and that none of the retired phrases
// appears in any card. Mutation: restore any retired line.
func TestTrustCardUsesPlainWords(t *testing.T) {
	d := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Fingerprint: "newfp"}
	first := trustCard(d, "")
	changed := trustCard(d, "oldfp")
	rancher := trustCard(tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=dynamiclistener-ca"}, "")

	assert.Contains(t, first, "the certificate is not Rancher's generated CA: it may come from your organization's CA, or from a proxy between you and the server that replaces certificates")
	assert.True(t, strings.HasSuffix(first, "confirm the fingerprint with the server's administrator, whatever the subject above says"), first)
	assert.Contains(t, changed, "it now presents a different one - the certificate may have been replaced, or another server may be answering in its place")
	for _, card := range []string{first, changed, rancher} {
		for _, phrase := range []string{"out of band", "out-of-band", "trusted channel", "intercepting TLS", "TLS-intercepting", "server operator",
			"does not identify itself", "different party"} {
			assert.NotContains(t, card, phrase)
		}
	}
}

// TestPlainLoginWithASavedCAThatDoesNotFitNamesConfigUpdateServer asserts the
// unknown-authority failure of a plain login to an entry that saves a CA the
// server's certificate does not match is rebuilt around the one remedy that
// works for it, saving the CA with config update-server: the message names
// the command and never says to pass --cacert (which a login without --url
// refuses), nor "pinned trust" or a possessive on the quoted name. Mutation:
// append the old sentence about the entry's pinned trust.
func TestPlainLoginWithASavedCAThatDoesNotFitNamesConfigUpdateServer(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, otherCA(t), false)

	_, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Empty(t, ce.Hint)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.UnknownAuthorityMessage(details.IsLeafCertificate(), "; save it for this entry with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>"), ce.Message)
	assert.NotContains(t, ce.Message, "pass it with --cacert")
	assert.NotContains(t, ce.Message, "pinned")
	assert.NotContains(t, ce.Message, "'prod''s")
}

// TestPlainLoginCertificateFailuresNameTheSavedEntry asserts that a plain
// login whose saved CA verifies the server's certificate, but whose
// certificate is expired or names another host, words the failure for the
// entry - the server URL saved for it and a different CA - and never names
// --url or --cacert, which a login without --url does not take. Mutations:
// rebuild only an unknown authority; rebuild with the zero Remedy.
func TestPlainLoginCertificateFailuresNameTheSavedEntry(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		dns       string
		notBefore time.Duration
		notAfter  time.Duration
		want      func(host string) string
	}{
		"expired": {
			dns: "127.0.0.1", notBefore: -2 * time.Hour, notAfter: -time.Hour,
			want: func(string) string {
				return "the server's certificate has expired; ask the server's administrator to renew it; a different CA cannot fix an expired certificate"
			},
		},
		"hostname mismatch": {
			dns: "rancher.example", notBefore: -time.Hour, notAfter: time.Hour,
			want: func(host string) string {
				return "the server's certificate is not valid for " + host +
					"; check that the server URL saved for entry 'prod' matches the certificate's hostname; a different CA cannot fix a hostname mismatch"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fix := selfSignedFixture(t, tc.dns, time.Now().Add(tc.notBefore), time.Now().Add(tc.notAfter))
			srv := newTLSFake(t, fix)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)

			_, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:sec\n"))
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, tc.want(register.ServerLabel(srv.URL)), ce.Message)
		})
	}
}

// TestPinnedTrustFailurePassesOtherErrorsThrough asserts an error that is not
// TLS_TRUST comes back unchanged. Mutation: wrap every error.
func TestPinnedTrustFailurePassesOtherErrorsThrough(t *testing.T) {
	t.Parallel()
	other := errors.New("boom")
	assert.Same(t, other, pinnedTrustFailure(other, "prod", "127.0.0.1:443"))
	assert.NoError(t, pinnedTrustFailure(nil, "prod", "127.0.0.1:443"))
}
