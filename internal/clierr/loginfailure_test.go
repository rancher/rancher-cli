package clierr

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeTextDetails is a details payload whose only job is to prove the
// primary's own projection survives composition.
type probeTextDetails struct{}

func (probeTextDetails) TextDetails() []string { return []string{"  probe line"} }

// TestRecoveryCommandsByKind asserts the one source of token recovery text:
// an ext token is the `token` kind, a v3 token is the deprecated
// `management-token` kind whose delete needs the deprecated gate.
func TestRecoveryCommandsByKind(t *testing.T) {
	t.Parallel()
	list, revoke := RecoveryCommands(TokenRef{Kind: TokenKindExt, Name: "ext-abc"})
	assert.Equal(t, "rancher get tokens", list)
	assert.Equal(t, "rancher delete token ext-abc --apply --yes", revoke)
	list, revoke = RecoveryCommands(TokenRef{Kind: TokenKindV3, Name: "token-xyz"})
	assert.Equal(t, "rancher get management-tokens", list)
	assert.Equal(t, "rancher delete management-token token-xyz --apply --yes --allow-deprecated", revoke)
}

// TestLoginFailureDetailsJSONShape asserts the wire field names agents read.
func TestLoginFailureDetailsJSONShape(t *testing.T) {
	t.Parallel()
	d := LoginFailureDetails{
		Primary:         PrimaryFailure{Code: CodeConflict, Message: "changed", Retryable: false},
		Stage:           StageCommit,
		ConfigState:     ConfigNotWritten,
		CredentialState: CredentialNotWritten,
		OperationMarker: "0123456789ab",
		DerivedToken: &TokenFate{Outcome: CreateKnown, Ref: &TokenRef{Kind: TokenKindExt, Name: "ext-1"}, Marked: true,
			Cleanup: &RevokeOutcome{Ref: &TokenRef{Kind: TokenKindExt, Name: "ext-1"}, Result: ResultRevoked}},
		Recovery: "re-run rancher auth login prod",
	}
	b, err := json.Marshal(d)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "commit", got["stage"])
	assert.Equal(t, "notWritten", got["configState"])
	assert.Equal(t, "0123456789ab", got["operationMarker"])
	assert.Equal(t, "CONFLICT", got["primary"].(map[string]any)["code"])
	dt := got["derivedToken"].(map[string]any)
	assert.Equal(t, "known", dt["outcome"])
	assert.Equal(t, "revoked", dt["cleanup"].(map[string]any)["result"])
	_, hasSession := got["sessionToken"]
	assert.False(t, hasSession, "absent fates are omitted")
}

// TestCredentialRecoveryFieldSet asserts the struct shape so prose, tests,
// and implementation cannot drift.
func TestCredentialRecoveryFieldSet(t *testing.T) {
	t.Parallel()
	want := []string{"Backend", "State", "PriorSource", "HelperRecordRetained", "CompensationError", "Command", "DataLossRisk"}
	var got []string
	for f := range reflect.TypeFor[CredentialRecovery]().Fields() {
		got = append(got, f.Name)
	}
	assert.Equal(t, want, got)
}

// TestLoginFailureDetailsTextDetailsOrder asserts the rendered order: primary
// first, then stage and states, then token fates, then recovery.
func TestLoginFailureDetailsTextDetailsOrder(t *testing.T) {
	t.Parallel()
	d := LoginFailureDetails{
		Primary:         PrimaryFailure{Code: CodeGeneric, Message: "disk full"},
		Stage:           StageCommit,
		ConfigState:     ConfigWriteFailed,
		CredentialState: CredentialCompensationFailed,
		CredentialRecovery: &CredentialRecovery{Backend: BackendFile, State: CredentialCompensationFailed,
			CompensationError: "read-only", Command: "rancher auth logout prod",
			DataLossRisk: "removes the credential for prod from every backend (keychain, file, credential helper), including a credential from an earlier login"},
		DerivedToken: &TokenFate{Outcome: CreateKnown, Ref: &TokenRef{Kind: TokenKindExt, Name: "ext-1"}, Marked: true,
			Cleanup: &RevokeOutcome{Ref: &TokenRef{Kind: TokenKindExt, Name: "ext-1"}, Result: ResultFailed, Error: "503"}},
		Recovery: "revoke ext/ext-1 with: rancher delete token ext-1 --apply --yes",
	}
	lines := d.TextDetails()
	require.GreaterOrEqual(t, len(lines), 6)
	assert.Equal(t, "failed: disk full (GENERIC)", lines[0])
	assert.Equal(t, "stage: commit", lines[1])
	assert.Contains(t, lines, "config: writeFailed")
	assert.Contains(t, lines, "credential store (file): compensationFailed; undo failed: read-only")
	assert.Contains(t, lines, "derived token ext/ext-1: known, marked; cleanup failed: 503")
	assert.Equal(t, "recover: revoke ext/ext-1 with: rancher delete token ext-1 --apply --yes", lines[len(lines)-1])
}

// TestLoginFailureDetailsTextCarriesEveryUnnamedFate asserts that a fate the
// CLI could not name still renders its cleanup result: an unknown or
// unidentified token with nothing attempted is a fact the operator needs.
func TestLoginFailureDetailsTextCarriesEveryUnnamedFate(t *testing.T) {
	t.Parallel()
	d := LoginFailureDetails{
		Primary:      PrimaryFailure{Code: CodeGeneric, Message: "lost"},
		Stage:        StageAuthentication,
		SessionToken: &TokenFate{Outcome: CreateUnknown, Cleanup: &RevokeOutcome{Result: ResultNotAttempted}},
		DerivedToken: &TokenFate{Outcome: CreateUnidentified, Kind: TokenKindExt, Marked: true,
			Cleanup: &RevokeOutcome{Result: ResultNotAttempted}},
		Recovery: "list both collections",
	}
	lines := d.TextDetails()
	assert.Contains(t, lines, "session token: unknown (kind unknown); cleanup: notAttempted")
	assert.Contains(t, lines, "derived token: unidentified (ext), marked; cleanup: notAttempted")
}

// TestFateLineRendersEveryCleanupResult asserts one rendered line per
// RevokeResult, plus the shapes a fate takes when it has no name.
func TestFateLineRendersEveryCleanupResult(t *testing.T) {
	t.Parallel()
	ref := &TokenRef{Kind: TokenKindV3, Name: "token-9"}
	tests := []struct {
		name string
		fate *TokenFate
		want string
	}{
		{"revoked", &TokenFate{Outcome: CreateKnown, Ref: ref, Cleanup: &RevokeOutcome{Result: ResultRevoked}},
			"session token v3/token-9: known; cleanup: revoked"},
		{"alreadyGone", &TokenFate{Outcome: CreateKnown, Ref: ref, Cleanup: &RevokeOutcome{Result: ResultAlreadyGone}},
			"session token v3/token-9: known; cleanup: alreadyGone"},
		{"failed", &TokenFate{Outcome: CreateKnown, Ref: ref, Cleanup: &RevokeOutcome{Result: ResultFailed, Error: "503"}},
			"session token v3/token-9: known; cleanup failed: 503"},
		{"notAttempted", &TokenFate{Outcome: CreateKnown, Ref: ref, Cleanup: &RevokeOutcome{Result: ResultNotAttempted}},
			"session token v3/token-9: known; cleanup: notAttempted"},
		{"notApplicable", &TokenFate{Outcome: CreateKnown, Ref: ref, Cleanup: &RevokeOutcome{Result: ResultNotApplicable}},
			"session token v3/token-9: known; cleanup: notApplicable"},
		{"no cleanup", &TokenFate{Outcome: CreateKnown, Ref: ref},
			"session token v3/token-9: known"},
		{"nameless with kind", &TokenFate{Outcome: CreateUnidentified, Kind: TokenKindV3},
			"session token: unidentified (v3)"},
		{"nameless without kind", &TokenFate{Outcome: CreateUnknown},
			"session token: unknown (kind unknown)"},
		{"not created", &TokenFate{Outcome: CreateNotCreated},
			"session token: notCreated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, fateLine("session token", tt.fate))
		})
	}
}

// TestLoginFailureDetailsTextIncludesThePrimaryDetails asserts that the
// primary's own projection is rendered directly under it, exactly once,
// and that a primary without details adds no line.
func TestLoginFailureDetailsTextIncludesThePrimaryDetails(t *testing.T) {
	t.Parallel()
	d := LoginFailureDetails{
		Primary: PrimaryFailure{Code: CodeGeneric, Message: "m", Details: probeTextDetails{}},
		Stage:   StageCommit,
	}
	lines := d.TextDetails()
	require.GreaterOrEqual(t, len(lines), 3)
	assert.Equal(t, "failed: m (GENERIC)", lines[0])
	assert.Equal(t, "  probe line", lines[1])
	assert.Equal(t, "stage: commit", lines[2])
	assert.Equal(t, 1, countLines(lines, "  probe line"))

	plain := LoginFailureDetails{Primary: PrimaryFailure{Code: CodeGeneric, Message: "m"}, Stage: StageCommit}.TextDetails()
	assert.Equal(t, []string{"failed: m (GENERIC)", "stage: commit", "recover: "}, plain)
}

func countLines(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}
	return n
}

// TestProvenanceExecLoginIsFourthValue asserts the persisted provenance a
// plugin-created login carries is distinct from the three acquisition
// values and serializes as the documented string.
func TestProvenanceExecLoginIsFourthValue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Provenance("execLogin"), ProvenanceExecLogin)
	for _, p := range []Provenance{ProvenanceDerived, ProvenanceLegacySession, ProvenanceUserSupplied} {
		assert.NotEqual(t, p, ProvenanceExecLogin)
	}
	assert.Equal(t, LoginStage("clusterToken"), StageClusterToken)
	assert.Equal(t, ConfigState("rollbackFailed"), ConfigRollbackFailed)
}

// TestLoginFailureDetailsCarriesClusterTokenAndConfigRecovery asserts the
// two new components serialize under their documented keys and render as
// text lines: the cluster token like the other fates, the config recovery
// with the remove command, the prior entry's URL, and the CA's shape
// (never its bytes).
func TestLoginFailureDetailsCarriesClusterTokenAndConfigRecovery(t *testing.T) {
	t.Parallel()
	ref := TokenRef{Kind: TokenKindExt, Name: "token-c1"}
	d := LoginFailureDetails{
		Primary: PrimaryFailure{Code: CodeGeneric, Message: "boom"},
		Stage:   StageClusterToken,
		ClusterToken: &TokenFate{Outcome: CreateKnown, Ref: &ref, Marked: true,
			Cleanup: &RevokeOutcome{Ref: &ref, Result: ResultRevoked}},
		ConfigState: ConfigRollbackFailed,
		ConfigRecovery: &ConfigRecovery{Entry: "exec-login-r.ex", Command: "rancher config delete-server 'exec-login-r.ex'",
			Prior: &PriorEntry{URL: "https://r.ex", CA: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
				CAFingerprint: "ab12", Provenance: ProvenanceExecLogin, TokenRef: &TokenRef{Kind: TokenKindExt, Name: "token-old"},
				TokenUserID: "user-a", CredentialExpiresAt: "2026-09-20T08:00:00Z",
				SupersededTokens: []SupersededToken{{Ref: TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-x"}, UserID: "user-a",
					SupersededAt: "2026-09-04T18:00:00Z", LastAttempt: &RevokeOutcome{Result: ResultFailed, Error: "503"}}}}},
		Recovery: "r",
	}
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	ct := got["clusterToken"].(map[string]any)
	assert.Equal(t, "known", ct["outcome"])
	cr := got["configRecovery"].(map[string]any)
	assert.Equal(t, "exec-login-r.ex", cr["entry"])
	prior := cr["prior"].(map[string]any)
	assert.Equal(t, "https://r.ex", prior["url"])
	assert.Contains(t, prior["ca"], "BEGIN CERTIFICATE", "the exact CA value travels in the JSON projection")
	recs := prior["supersededTokens"].([]any)
	require.Len(t, recs, 1)
	rec := recs[0].(map[string]any)
	assert.Equal(t, "user-a", rec["userId"])
	last := rec["lastAttempt"].(map[string]any)
	assert.Equal(t, "failed", last["result"])
	_, hasRef := last["ref"]
	assert.False(t, hasRef, "lastAttempt carries no ref: the record's ref is the token")

	lines := d.TextDetails()
	assert.Contains(t, lines, "stage: clusterToken")
	assert.Contains(t, lines, "config: rollbackFailed")
	assert.Contains(t, lines, "cluster token ext/token-c1: known, marked; cleanup: revoked")
	assert.Contains(t, lines, "config entry exec-login-r.ex could not be restored; remove it with: rancher config delete-server 'exec-login-r.ex'")
	assert.Contains(t, lines, "  prior entry: https://r.ex, CA inline PEM (59 bytes) fingerprint ab12, provenance execLogin, token ext/token-old (user user-a, expires 2026-09-20T08:00:00Z), 1 superseded token")
	for _, l := range lines {
		assert.NotContains(t, l, "BEGIN CERTIFICATE", "text never carries CA bytes")
	}
}

// TestPriorEntryTextShapes asserts the three CA shapes the prior entry can
// name in text: system roots, a path, and an inline PEM.
func TestPriorEntryTextShapes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "system roots", (&PriorEntry{}).caShape())
	assert.Equal(t, "path /etc/ca.pem", (&PriorEntry{CA: "/etc/ca.pem"}).caShape())
	assert.Equal(t, "inline PEM (8 bytes)", (&PriorEntry{CA: "-----BEG"}).caShape())
}

// TestSupersededRecordServerTravelsInTheFailureDetails asserts the
// record's server in the failure details' JSON: present when the record
// names one, absent otherwise.
func TestSupersededRecordServerTravelsInTheFailureDetails(t *testing.T) {
	t.Parallel()
	prior := PriorEntry{URL: "https://new.ex", SupersededTokens: []SupersededToken{
		{Ref: TokenRef{Kind: TokenKindExt, Name: "token-a"}, SupersededAt: "2026-10-06T00:00:00Z", Server: "https://old.ex"},
		{Ref: TokenRef{Kind: TokenKindExt, Name: "token-b"}, SupersededAt: "2026-10-06T00:00:00Z"},
	}}
	raw, err := json.Marshal(ConfigRecovery{Entry: "prod", Prior: &prior, Command: "c"})
	require.NoError(t, err)
	var got struct {
		Prior struct {
			SupersededTokens []map[string]any `json:"supersededTokens"`
		} `json:"prior"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got.Prior.SupersededTokens, 2)
	assert.Equal(t, "https://old.ex", got.Prior.SupersededTokens[0]["server"])
	assert.NotContains(t, got.Prior.SupersededTokens[1], "server")
}

// TestTokenFateExpiryIsNotRendered asserts a token's expiry, carried only
// for the recovery text, never reaches the JSON of a failure's details.
func TestTokenFateExpiryIsNotRendered(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(TokenFate{Outcome: CreateKnown, Ref: &TokenRef{Kind: TokenKindV3, Name: "t"}, ExpiresAt: "2026-10-06T00:00:00Z"})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.NotContains(t, decoded, "expiresAt")
	assert.NotContains(t, decoded, "ExpiresAt")
}

// TestPriorEntryCarriesWhetherTheLastLoginUsedTheVariable asserts the prior
// entry's record that a login with no saved CA connected with
// RANCHER_CLI_CACERT: a JSON field only when set, and a phrase in the text
// beside the CA.
func TestPriorEntryCarriesWhetherTheLastLoginUsedTheVariable(t *testing.T) {
	t.Parallel()
	marked := PriorEntry{URL: "https://r.ex", CAFromEnvironment: true}
	raw, err := json.Marshal(marked)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, true, got["caFromEnvironment"])
	assert.Equal(t, "https://r.ex, CA system roots, last login connected with RANCHER_CLI_CACERT", marked.text())

	plain := PriorEntry{URL: "https://r.ex"}
	raw, err = json.Marshal(plain)
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.NotContains(t, got, "caFromEnvironment")
	assert.Equal(t, "https://r.ex, CA system roots", plain.text())
}
