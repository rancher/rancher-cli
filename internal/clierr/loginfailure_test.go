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

// TestRecoveryCommandsByKind pins the one source of token recovery text:
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

// TestLoginFailureDetailsJSONShape pins the wire field names agents read.
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

// TestCredentialRecoveryFieldSet pins the struct shape so prose, tests,
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

// TestLoginFailureDetailsTextDetailsOrder pins the rendered order: primary
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

// TestLoginFailureDetailsTextCarriesEveryUnnamedFate pins that a fate the
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

// TestFateLineRendersEveryCleanupResult pins one rendered line per
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

// TestLoginFailureDetailsTextIncludesThePrimaryDetails pins that the
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
