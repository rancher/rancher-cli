package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	gotoken "go/token"
	"go/types"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/skillinstall"
	"github.com/rancher/rancher-cli/internal/token"
)

// renderDetailsDefault is the default-format error text of an error holding
// details d.
func renderDetailsDefault(t *testing.T, d clierr.TextDetails) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, runtime.RenderError(&b, "", clierr.New(clierr.CodeGeneric, "m").WithDetails(d)))
	return b.String()
}

// detailsTextCase is one payload and the default-format text it renders to.
type detailsTextCase struct {
	name    string
	details clierr.TextDetails
	want    string
}

func assertDetailsText(t *testing.T, cases []detailsTextCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, renderDetailsDefault(t, c.details))
		})
	}
}

// TestDetailsTextOfTLSTrust asserts the default-format text of certificate
// details with safe values, with an invalid byte, and with a single quote and
// a hidden character.
func TestDetailsTextOfTLSTrust(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"unknown authority", tlstrust.Details{
			Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Issuer: "CN=ca", Fingerprint: "AA:BB",
			NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
		}, "error (GENERIC): m\n" +
			"  certificate authority: CN=rancher.example\n" +
			"  issuer: CN=ca\n" +
			"  fingerprint: AA:BB\n" +
			"  valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n"},
		{"expired", tlstrust.Details{
			Kind: tlstrust.KindExpired, Subject: "CN=rancher.example", Issuer: "CN=ca", Fingerprint: "AA:BB",
			NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
		}, "error (GENERIC): m\n" +
			"  certificate: CN=rancher.example\n" +
			"  issuer: CN=ca\n" +
			"  fingerprint: AA:BB\n" +
			"  valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n"},
		{"an invalid byte in the subject", tlstrust.Details{
			Kind: tlstrust.KindUnknownAuthority, Subject: "CN=a\x9bb", Issuer: "CN=ca", Fingerprint: "AA:BB",
		}, "error (GENERIC): m\n" +
			"  certificate authority: \"CN=a\\x9bb\"\n" +
			"  issuer: CN=ca\n" +
			"  fingerprint: AA:BB\n"},
		{"a single quote and a hidden character in the subject", tlstrust.Details{
			Kind: tlstrust.KindUnknownAuthority, Subject: "CN=o'b\u200b", Issuer: "CN=ca", Fingerprint: "AA:BB",
		}, "error (GENERIC): m\n" +
			"  certificate authority: CN=o'\"b\\u200b\"\n" +
			"  issuer: CN=ca\n" +
			"  fingerprint: AA:BB\n"},
		{"fingerprint only", tlstrust.Details{Kind: tlstrust.KindInvalidChain, Fingerprint: "AA:BB", NotBefore: "2026-01-01T00:00:00Z"},
			"error (GENERIC): m\n  fingerprint: AA:BB\n"},
		{"nothing", tlstrust.Details{Kind: tlstrust.KindInvalidChain}, "error (GENERIC): m\n"},
	})
}

// TestDetailsTextOfFeatureGates asserts the default-format text of the
// Prime and capability payloads with safe values.
func TestDetailsTextOfFeatureGates(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"prime", clierr.PrimeRequiredDetails{Feature: "feat-a", Server: "https://r.example"},
			"error (GENERIC): m\n  feature: feat-a (checked against server https://r.example)\n"},
		{"capability", clierr.CapabilityUnsupportedDetails{Feature: "cap-a", Server: "https://r.example"},
			"error (GENERIC): m\n  capability: cap-a (checked against server https://r.example)\n"},
	})
}

// TestDetailsTextOfCommittedContext asserts the default-format text of the
// committed-context payload with safe values, with and without partial
// output, with an invalid byte, and with a single quote and a hidden
// character.
func TestDetailsTextOfCommittedContext(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"no partial output", clierr.CommittedContextDetails{Committed: true, ResourceKind: "token", ResourceName: "tok-1", Recovery: "run the list command"},
			"error (GENERIC): m\n" +
				"  committed: token tok-1 - the change may already be on the server\n" +
				"  recover: run the list command\n"},
		{"partial output", clierr.CommittedContextDetails{Committed: true, ResourceKind: "token", ResourceName: "tok-1", PartialOutput: true, Recovery: "run the list command"},
			"error (GENERIC): m\n" +
				"  committed: token tok-1 - the change may already be on the server\n" +
				"  partial output may already be on stdout\n" +
				"  recover: run the list command\n"},
		{"an invalid byte in the resource name", clierr.CommittedContextDetails{Committed: true, ResourceKind: "token", ResourceName: "tok\x9b1", Recovery: "run the list command"},
			"error (GENERIC): m\n" +
				"  committed: token \"tok\\x9b1\" - the change may already be on the server\n" +
				"  recover: run the list command\n"},
		{"a single quote and a hidden character in the resource name", clierr.CommittedContextDetails{Committed: true, ResourceKind: "token", ResourceName: "o'b\u200b", Recovery: "run the list command"},
			"error (GENERIC): m\n" +
				"  committed: token o'\"b\\u200b\" - the change may already be on the server\n" +
				"  recover: run the list command\n"},
	})
}

// TestDetailsTextOfLoginFailure asserts the default-format text of a login
// failure payload with safe values, with every optional line present and
// with the other branches of each optional line.
func TestDetailsTextOfLoginFailure(t *testing.T) {
	t.Parallel()
	expired := tlstrust.Details{
		Kind: tlstrust.KindExpired, Subject: "CN=a", Issuer: "CN=i", Fingerprint: "FP",
		NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
	}
	assertDetailsText(t, []detailsTextCase{
		{"everything", clierr.LoginFailureDetails{
			Primary:         clierr.PrimaryFailure{Message: "boom", Code: "GENERIC", Details: expired},
			Stage:           clierr.StageCommit,
			ConfigState:     clierr.ConfigRollbackFailed,
			CredentialState: clierr.CredentialCompensationFailed,
			CredentialRecovery: &clierr.CredentialRecovery{
				Backend: clierr.BackendKeyring, CompensationError: "undo boom",
				Command: "rancher auth logout", DataLossRisk: clierr.DataLossRiskText("https://r.example"),
			},
			OperationMarker: "abc",
			SessionToken: &clierr.TokenFate{
				Outcome: clierr.CreateKnown, Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "tok-s"}, Marked: true,
				Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "cleanup boom"},
			},
			DerivedToken: &clierr.TokenFate{
				Outcome: clierr.CreateUnknown,
				Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultNotAttempted},
			},
			ClusterToken: &clierr.TokenFate{
				Outcome: clierr.CreateKnown, Kind: clierr.TokenKindV3,
				Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultRevoked},
			},
			ConfigRecovery: &clierr.ConfigRecovery{
				Entry: "e1", Command: "rancher config remove e1",
				Prior: &clierr.PriorEntry{
					URL: "https://old.example", CA: "-----BEGIN x", CAFingerprint: "FP2", CAFromEnvironment: true,
					Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "tok-p"},
					TokenUserID: "u-1", CredentialExpiresAt: "2027-01-01T00:00:00Z",
					SupersededTokens: []clierr.SupersededToken{{}, {}},
				},
			},
			Recovery: "run it again",
		}, "error (GENERIC): m\n" +
			"  failed: boom (GENERIC)\n" +
			"  certificate: CN=a\n" +
			"  issuer: CN=i\n" +
			"  fingerprint: FP\n" +
			"  valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n" +
			"  stage: commit\n" +
			"  config: rollbackFailed\n" +
			"  credential store (keyring): compensationFailed; undo failed: undo boom\n" +
			"  clear the local credential with: rancher auth logout; that command removes the credential for https://r.example from every backend (keychain, file, credential helper), including a credential from an earlier login\n" +
			"  operation marker: [op:abc]\n" +
			"  session token ext/tok-s: known, marked; cleanup failed: cleanup boom\n" +
			"  derived token: unknown (kind unknown); cleanup: notAttempted\n" +
			"  cluster token: known (v3); cleanup: revoked\n" +
			"  config entry e1 could not be restored; remove it with: rancher config remove e1\n" +
			"    prior entry: https://old.example, CA inline PEM (12 bytes) fingerprint FP2, last login connected with RANCHER_CLI_CACERT, provenance derived, token ext/tok-p (user u-1, expires 2027-01-01T00:00:00Z), 2 superseded tokens\n" +
			"  recover: run it again\n"},
		{"minimal", clierr.LoginFailureDetails{
			Primary:         clierr.PrimaryFailure{Message: "m1", Code: "VALIDATION"},
			Stage:           clierr.StageAuthentication,
			CredentialState: clierr.CredentialCommitted,
			SessionToken:    &clierr.TokenFate{Outcome: clierr.CreateNotCreated, Search: &clierr.UnmarkedTokenSearch{}},
			ConfigRecovery: &clierr.ConfigRecovery{
				Entry: "e2",
				Prior: &clierr.PriorEntry{
					URL: "https://o2", CA: "/p/ca.pem", TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"},
					CredentialExpiresAt: "x", SupersededTokens: []clierr.SupersededToken{{}},
				},
			},
			Recovery: "r2",
		}, "error (GENERIC): m\n" +
			"  failed: m1 (VALIDATION)\n" +
			"  stage: authentication\n" +
			"  credential store: committed\n" +
			"  kubeconfig token: notCreated\n" +
			"  config entry e2 could not be restored\n" +
			"    prior entry: https://o2, CA path /p/ca.pem, token ext/t (expires x), 1 superseded token\n" +
			"  recover: r2\n"},
		{"prior with a user only", clierr.LoginFailureDetails{
			Primary: clierr.PrimaryFailure{Message: "m3", Code: "GENERIC"},
			Stage:   clierr.StageClusterToken,
			ClusterToken: &clierr.TokenFate{
				Outcome: clierr.CreateKnown, Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "c"}, Kind: clierr.TokenKindExt,
			},
			ConfigRecovery: &clierr.ConfigRecovery{
				Entry: "e3", Prior: &clierr.PriorEntry{URL: "https://o3", TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "t3"}, TokenUserID: "u3"},
			},
			Recovery: "r3",
		}, "error (GENERIC): m\n" +
			"  failed: m3 (GENERIC)\n" +
			"  stage: clusterToken\n" +
			"  cluster token ext/c: known\n" +
			"  config entry e3 could not be restored\n" +
			"    prior entry: https://o3, CA system roots, token v3/t3 (user u3)\n" +
			"  recover: r3\n"},
		{"unidentified token and a prior with no token", clierr.LoginFailureDetails{
			Primary:      clierr.PrimaryFailure{Message: "m4", Code: "GENERIC"},
			Stage:        clierr.StageCommit,
			DerivedToken: &clierr.TokenFate{Outcome: clierr.CreateUnidentified, Kind: clierr.TokenKindExt, Marked: true},
			ConfigRecovery: &clierr.ConfigRecovery{
				Entry: "e4", Prior: &clierr.PriorEntry{URL: "https://o4", Provenance: clierr.ProvenanceUserSupplied},
			},
			Recovery: "r4",
		}, "error (GENERIC): m\n" +
			"  failed: m4 (GENERIC)\n" +
			"  stage: commit\n" +
			"  derived token: unidentified (ext), marked\n" +
			"  config entry e4 could not be restored\n" +
			"    prior entry: https://o4, CA system roots, provenance userSupplied\n" +
			"  recover: r4\n"},
	})
}

// TestDetailsTextOfLogoutAllPartialFailure asserts the default-format text of
// the logout --all payload with safe values.
func TestDetailsTextOfLogoutAllPartialFailure(t *testing.T) {
	t.Parallel()
	full := auth.LogoutAllPartialFailure{
		LoggedOut:    []string{"a", "b"},
		DeletedFrom:  map[string][]string{"s2": {"file", "keyring"}, "s1": {"file"}},
		DeletedCache: []string{"c1", "c2"},
		Revoked:      []token.RevokedToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t1"}, Source: "cache", Server: "https://s1"}},
		NotRevoked: []token.NotRevokedToken{
			{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t2"}, Source: "cache", Server: "https://s2", Reason: "failed", Retry: "manual"},
			{Source: "login", Server: "https://s3", Reason: "forbidden", Retry: "none"},
			{
				Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "t4"}, Source: "cache", Server: "https://s4", Reason: "failed",
				Code: clierr.CodeTLSTrust, Error: "the certificate has expired", Retry: "nextLogin",
				Certificate: &tlstrust.Details{Kind: tlstrust.KindExpired, Subject: "CN=x", Fingerprint: "FP"},
			},
		},
		Rerun: "rancher auth logout --all",
	}
	omitted, ok := full.MapCommands(func(string) string { return "" }).(auth.LogoutAllPartialFailure)
	require.True(t, ok)
	assertDetailsText(t, []detailsTextCase{
		{"everything", full, "error (GENERIC): m\n" +
			"  already logged out: a, b\n" +
			"  credentials removed: s1: file\n" +
			"  credentials removed: s2: file, keyring\n" +
			"  cached kubectl credentials removed: c1, c2\n" +
			"  revoked: ext/t1 on https://s1\n" +
			"  not revoked: ext/t2 on https://s2 (failed, retry manual)\n" +
			"  not revoked: login token on https://s3 (forbidden, retry none)\n" +
			"  not revoked: v3/t4 on https://s4 (failed, TLS_TRUST, retry nextLogin)\n" +
			"    the certificate has expired\n" +
			"    certificate: CN=x\n" +
			"    fingerprint: FP\n" +
			"  those credentials are gone regardless of this failure; to finish, run: rancher auth logout --all\n"},
		{"command omitted", omitted, "error (GENERIC): m\n" +
			"  already logged out: a, b\n" +
			"  credentials removed: s1: file\n" +
			"  credentials removed: s2: file, keyring\n" +
			"  cached kubectl credentials removed: c1, c2\n" +
			"  revoked: ext/t1 on https://s1\n" +
			"  not revoked: ext/t2 on https://s2 (failed, retry manual)\n" +
			"  not revoked: login token on https://s3 (forbidden, retry none)\n" +
			"  not revoked: v3/t4 on https://s4 (failed, TLS_TRUST, retry nextLogin)\n" +
			"    the certificate has expired\n" +
			"    certificate: CN=x\n" +
			"    fingerprint: FP\n" +
			"  those credentials are gone regardless of this failure\n"},
		{"nothing logged out", auth.LogoutAllPartialFailure{}, "error (GENERIC): m\n  already logged out: \n"},
	})
}

// TestDetailsTextOfBundleFailure asserts the default-format text of the
// bundle failure payload with safe values.
func TestDetailsTextOfBundleFailure(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"with a certificate", observe.BundleFailureDetails{
			OutputFile: "out dir/bundle.tar.gz", Attempted: 3, Failed: 1,
			First: &tlstrust.Details{Kind: tlstrust.KindExpired, Subject: "CN=x", Fingerprint: "FP"},
		}, "error (GENERIC): m\n" +
			"  archive: out dir/bundle.tar.gz\n" +
			"  items attempted: 3, failed: 1\n" +
			"  certificate: CN=x\n" +
			"  fingerprint: FP\n"},
		{"path only", observe.BundleFailureDetails{OutputFile: "bundle.tar.gz"}, "error (GENERIC): m\n  archive: bundle.tar.gz\n"},
	})
}

// TestDetailsTextOfRevokePartialFailure asserts the default-format text of
// the revoke payload with safe values.
func TestDetailsTextOfRevokePartialFailure(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"everything", rbac.RevokePartialFailure{
			Deleted: []string{"a", "b"}, Failed: rbac.RevokeFailure{Name: "c", Code: "FORBIDDEN", Message: "denied"}, Unattempted: []string{"d", "e"},
		}, "error (GENERIC): m\n" +
			"  deleted: a, b\n" +
			"  failed: c (FORBIDDEN: denied)\n" +
			"  not attempted: d, e\n" +
			"  bindings already deleted stay deleted; run the same revoke command again to delete the rest\n"},
		{"the failed binding only", rbac.RevokePartialFailure{Failed: rbac.RevokeFailure{Name: "c", Code: "FORBIDDEN", Message: "denied"}},
			"error (GENERIC): m\n" +
				"  failed: c (FORBIDDEN: denied)\n" +
				"  bindings already deleted stay deleted; run the same revoke command again to delete the rest\n"},
	})
}

// TestDetailsTextOfDeleteProjectPartialFailure asserts the default-format
// text of the project delete payload with safe values.
func TestDetailsTextOfDeleteProjectPartialFailure(t *testing.T) {
	t.Parallel()
	objects := []safety.Object{
		{Kind: "project", Name: "p1", Outcome: safety.OutcomeFailed, Message: "denied"},
		{Kind: "namespace", Name: "n1", Outcome: safety.OutcomeWillBeDeleted, Message: "ignored"},
		{Kind: "namespace", Name: "n2", Outcome: safety.OutcomeNotDeleted, Message: "kept by Kubernetes"},
		{Kind: "namespace", Name: "n3", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "n4", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "n5", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "n6", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "n7", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "n8", Outcome: safety.OutcomeFailed, Message: "timed out"},
	}
	assertDetailsText(t, []detailsTextCase{
		{"six names and reasons", resources.DeleteProjectPartialFailure{Objects: objects}, "error (GENERIC): m\n" +
			"  projects:\n" +
			"    failed: p1 (denied)\n" +
			"  namespaces:\n" +
			"    will be deleted: n1, n3, n4, n5, n6 and 1 more\n" +
			"    not deleted: n2 (kept by Kubernetes)\n" +
			"    failed: n8 (timed out)\n"},
		{"nothing", resources.DeleteProjectPartialFailure{}, "error (GENERIC): m\n"},
	})
}

// TestDetailsTextOfSkillReport asserts the default-format text of the skill
// install report with safe values.
func TestDetailsTextOfSkillReport(t *testing.T) {
	t.Parallel()
	target := func(harness string, scope skillinstall.Scope, dir string) skillinstall.Target {
		return skillinstall.Target{Harness: harness, Scope: scope, Dir: dir}
	}
	assertDetailsText(t, []detailsTextCase{
		{"four targets of one harness and scope", skillinstall.Report{Outcomes: []skillinstall.Outcome{
			{Target: target("claude", skillinstall.ScopeUser, "/h/.claude/skills/rancher"), Action: "installed"},
			{Target: target("claude", skillinstall.ScopeUser, "/h/.claude/skills/other"), Verdict: skillinstall.VerdictSkew, Reason: "older"},
			{Target: target("claude", skillinstall.ScopeUser, "/h/.claude/skills/third"), Action: "failed", Reason: "permission denied"},
			{Target: target("claude", skillinstall.ScopeUser, "/h/.claude/skills/fourth"), Action: "failed"},
		}}, "error (GENERIC): m\n" +
			"  /h/.claude/skills/rancher: installed\n" +
			"  /h/.claude/skills/other: skew, older\n" +
			"  /h/.claude/skills/third: failed, permission denied\n" +
			"  … and 1 more\n" +
			"  to see each target's state, run: rancher skill status --agent claude --scope user; to see every reason, run the command again with -o json\n"},
		{"two harnesses and two scopes", skillinstall.Report{Outcomes: []skillinstall.Outcome{
			{Target: target("claude", skillinstall.ScopeUser, "/a"), Action: "failed"},
			{Target: target("codex", skillinstall.ScopeProject, "/b"), Verdict: skillinstall.VerdictAbsent},
		}}, "error (GENERIC): m\n" +
			"  /a: failed\n" +
			"  /b: absent\n" +
			"  to see each target's state, run: rancher skill status; to see every reason, run the command again with -o json\n"},
		{"two harnesses and one scope", skillinstall.Report{Outcomes: []skillinstall.Outcome{
			{Target: target("claude", skillinstall.ScopeUser, "/a"), Action: "failed"},
			{Target: target("codex", skillinstall.ScopeUser, "/b"), Action: "failed"},
		}}, "error (GENERIC): m\n" +
			"  /a: failed\n" +
			"  /b: failed\n" +
			"  to see each target's state, run: rancher skill status --scope user; to see every reason, run the command again with -o json\n"},
	})
}

// TestDetailsTextOfDeleteTokenFailure asserts the default-format text of the
// token delete payload with safe values.
func TestDetailsTextOfDeleteTokenFailure(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"deleted and not revoked", token.DeleteTokenFailure{
			Deleted: []string{"k1", "k2"},
			NotRevoked: []token.NotRevokedToken{
				{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t2"}, Source: "cache", Server: "https://s2", Reason: "failed", Retry: "manual"},
			},
		}, "error (GENERIC): m\n" +
			"  deleted: k1, k2\n" +
			"  not revoked: ext/t2 on https://s2 (failed, retry manual)\n"},
		{"nothing deleted", token.DeleteTokenFailure{
			NotRevoked: []token.NotRevokedToken{{Source: "login", Server: "https://s3", Reason: "forbidden", Retry: "none"}},
		}, "error (GENERIC): m\n  not revoked: login token on https://s3 (forbidden, retry none)\n"},
	})
}

// TestDetailsTextOfRevocationReport asserts the default-format text of the
// revocation report payload with safe values.
func TestDetailsTextOfRevocationReport(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"with and without a certificate failure", token.RevocationReport{
			NotRevoked: []token.NotRevokedToken{
				{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t2"}, Source: "cache", Server: "https://s2", Reason: "failed", Retry: "manual"},
				{
					Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "t4"}, Source: "cache", Server: "https://s4", Reason: "failed",
					Code: clierr.CodeTLSTrust, Error: "the certificate has expired", Retry: "nextLogin",
					Certificate: &tlstrust.Details{
						Kind: tlstrust.KindUnknownAuthority, Subject: "CN=x", Issuer: "CN=y", Fingerprint: "FP",
						NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
					},
				},
				{Source: "login", Server: "https://s5", Reason: "failed", Code: clierr.CodeTLSTrust, Error: "bad chain", Retry: "manual"},
			},
		}, "error (GENERIC): m\n" +
			"  not revoked: ext/t2 on https://s2 (failed, retry manual)\n" +
			"  not revoked: v3/t4 on https://s4 (failed, TLS_TRUST, retry nextLogin)\n" +
			"    the certificate has expired\n" +
			"    certificate authority: CN=x\n" +
			"    issuer: CN=y\n" +
			"    fingerprint: FP\n" +
			"    valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n" +
			"  not revoked: login token on https://s5 (failed, TLS_TRUST, retry manual)\n" +
			"    bad chain\n"},
		{"nothing", token.RevocationReport{}, "error (GENERIC): m\n"},
	})
}

// TestDetailsTextOfMarkedDetails asserts the default-format text of the test
// helper payload.
func TestDetailsTextOfMarkedDetails(t *testing.T) {
	t.Parallel()
	assertDetailsText(t, []detailsTextCase{
		{"one line", remedytest.MarkedDetails{Text: "plain text"}, "error (GENERIC): m\n  plain text\n"},
	})
}

// A details payload hands the renderer each value it reads from the payload
// as it is, and the renderer shows the value with hidden characters as escapes
// (clierr.IDText), so two values that differ by a hidden character print
// differently. The table below asserts that for every payload type, by
// planting values in every text field and reading the default, json and yaml
// output. The scan after it asserts the TextDetails bodies of the production
// tree, and every function they reach, call no function that escapes, quotes
// or removes characters themselves, and write only constants and numbers as
// labels.

const (
	detailsNeverPrinted   = "the text never prints it"
	detailsSelectsBranch  = "selects a branch or a label of the text; never printed itself"
	detailsNestedPayload  = "a nested payload has its own row"
	detailsCountsOnly     = "only the number of elements is printed"
	detailsUnexportedType = "an unexported type: its own test in the package asserts the same checks"
)

// detailsVariant is a way to fill a payload so another branch of its text
// shows: set changes fields after the planting, and sets lists the text paths
// it writes itself, which are not planted in the variant.
type detailsVariant struct {
	name string
	sets []string
	set  func(t *testing.T, v reflect.Value)
}

// detailsRow is the expectation for one payload type: the type, the variants
// that between them show every text path that the text shows at all, and the
// paths that never show, each with its reason. A type that cannot be built by
// reflection names the prefix of the test functions of its own package that
// make the same checks.
type detailsRow struct {
	typ        reflect.Type
	variants   []detailsVariant
	notShown   map[string]string
	testPrefix string
}

func detailsAt(t *testing.T, v reflect.Value, field string) reflect.Value {
	t.Helper()
	for step := range strings.SplitSeq(field, ".") {
		element := strings.HasSuffix(step, "[]")
		for v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		v = v.FieldByName(strings.TrimSuffix(step, "[]"))
		require.True(t, v.IsValid(), "the field %s", field)
		if element {
			v = v.Index(0)
		}
	}
	return v
}

// setDetails sets the field at path to value.
func setDetails(field string, value any) func(t *testing.T, v reflect.Value) {
	return func(t *testing.T, v reflect.Value) {
		t.Helper()
		detailsAt(t, v, field).Set(reflect.ValueOf(value))
	}
}

// clearDetails sets the field at path to its zero value.
func clearDetails(fields ...string) func(t *testing.T, v reflect.Value) {
	return func(t *testing.T, v reflect.Value) {
		t.Helper()
		for _, field := range fields {
			f := detailsAt(t, v, field)
			f.Set(reflect.Zero(f.Type()))
		}
	}
}

// allDetails runs each of fns.
func allDetails(fns ...func(t *testing.T, v reflect.Value)) func(t *testing.T, v reflect.Value) {
	return func(t *testing.T, v reflect.Value) {
		t.Helper()
		for _, fn := range fns {
			fn(t, v)
		}
	}
}

// notShownFor is, for each prefix, the entries of notShown with the prefix
// and a dot in front, so the fields of the three token fates and of each
// not-revoked token are listed once.
func notShownFor(entries map[string]string, prefixes ...string) map[string]string {
	out := map[string]string{}
	for _, prefix := range prefixes {
		for field, reason := range entries {
			out[prefix+"."+field] = reason
		}
	}
	return out
}

func joinNotShown(lists ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range lists {
		maps.Copy(out, m)
	}
	return out
}

var detailsFateNotShown = map[string]string{
	"CreatedAt":               detailsNeverPrinted,
	"Cleanup.Ref.Kind":        detailsNeverPrinted,
	"Cleanup.Ref.Name":        detailsNeverPrinted,
	"Cleanup.TLSError":        detailsNeverPrinted,
	"Search.AuthProvider":     detailsNeverPrinted,
	"Search.HandoffAt":        detailsNeverPrinted,
	"Search.PollEndedAt":      detailsNeverPrinted,
	"Search.RequestExpiresAt": detailsNeverPrinted,
	"ExpiresAt":               detailsNeverPrinted,
}

var detailsNotRevokedNotShown = map[string]string{
	"Entry":            detailsNeverPrinted,
	"Recovery[]":       detailsNeverPrinted,
	"LoginAs":          detailsNeverPrinted,
	"Certificate.Kind": detailsSelectsBranch,
}

// notRevokedVariants are the two ways a not-revoked token reads: with a name
// and a certificate failure, and with a source and no code.
func notRevokedVariants(list string) []detailsVariant {
	return []detailsVariant{
		{name: "named token with a certificate failure"},
		{
			name: "token with no name and no code",
			sets: []string{list + "[].Ref.Kind", list + "[].Ref.Name", list + "[].Code"},
			set:  clearDetails(list+"[].Ref", list+"[].Code"),
		},
	}
}

var detailsRows = map[string]detailsRow{
	"internal/client/tlstrust Details": {
		typ: reflect.TypeFor[tlstrust.Details](),
		variants: []detailsVariant{
			{name: "certificate"},
			{name: "unknown authority", sets: []string{"Kind"}, set: setDetails("Kind", tlstrust.KindUnknownAuthority)},
		},
		notShown: map[string]string{"Kind": detailsSelectsBranch},
	},
	"internal/clierr PrimeRequiredDetails":         {typ: reflect.TypeFor[clierr.PrimeRequiredDetails](), variants: []detailsVariant{{name: "default"}}},
	"internal/clierr CapabilityUnsupportedDetails": {typ: reflect.TypeFor[clierr.CapabilityUnsupportedDetails](), variants: []detailsVariant{{name: "default"}}},
	"internal/clierr CommittedContextDetails": {
		typ: reflect.TypeFor[clierr.CommittedContextDetails](),
		variants: []detailsVariant{
			{name: "default"},
			{name: "partial output", set: setDetails("PartialOutput", true)},
		},
		notShown: map[string]string{"SecretName": detailsNeverPrinted},
	},
	"internal/clierr LoginFailureDetails": {
		typ: reflect.TypeFor[clierr.LoginFailureDetails](),
		variants: []detailsVariant{
			{name: "named tokens with a prior entry"},
			{
				name: "tokens with no name and a failed cleanup",
				sets: []string{
					"SessionToken.Ref.Kind", "SessionToken.Ref.Name", "SessionToken.Cleanup.Result",
					"DerivedToken.Ref.Kind", "DerivedToken.Ref.Name", "DerivedToken.Cleanup.Result",
					"ClusterToken.Ref.Kind", "ClusterToken.Ref.Name", "ClusterToken.Cleanup.Result",
				},
				set: allDetails(
					clearDetails("SessionToken.Ref", "DerivedToken.Ref", "ClusterToken.Ref"),
					setDetails("SessionToken.Cleanup.Result", clierr.ResultFailed),
					setDetails("DerivedToken.Cleanup.Result", clierr.ResultFailed),
					setDetails("ClusterToken.Cleanup.Result", clierr.ResultFailed),
				),
			},
		},
		notShown: joinNotShown(
			map[string]string{
				"Primary.Hint":                                                 detailsNeverPrinted,
				"Primary.Candidates[].ID":                                      detailsNeverPrinted,
				"Primary.Candidates[].Name":                                    detailsNeverPrinted,
				"Primary.Details":                                              detailsNestedPayload,
				"CredentialRecovery.State":                                     detailsNeverPrinted,
				"CredentialRecovery.PriorSource":                               detailsNeverPrinted,
				"ConfigRecovery.Prior.SupersededTokens[].Ref.Kind":             detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].Ref.Name":             detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].UserID":               detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].ExpiresAt":            detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].SupersededAt":         detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].Server":               detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Ref.Kind": detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Ref.Name": detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Result":   detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Error":    detailsCountsOnly,
				"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.TLSError": detailsCountsOnly,
			},
			notShownFor(detailsFateNotShown, "SessionToken", "DerivedToken", "ClusterToken"),
		),
	},
	"internal/commands/auth LogoutAllPartialFailure": {
		typ:      reflect.TypeFor[auth.LogoutAllPartialFailure](),
		variants: notRevokedVariants("NotRevoked"),
		notShown: joinNotShown(
			map[string]string{"Revoked[].Source": detailsNeverPrinted},
			notShownFor(detailsNotRevokedNotShown, "NotRevoked[]"),
		),
	},
	"internal/engine/observe BundleFailureDetails": {
		typ:      reflect.TypeFor[observe.BundleFailureDetails](),
		variants: []detailsVariant{{name: "default"}},
		notShown: map[string]string{"First.Kind": detailsSelectsBranch},
	},
	"internal/engine/rbac RevokePartialFailure": {typ: reflect.TypeFor[rbac.RevokePartialFailure](), variants: []detailsVariant{{name: "default"}}},
	"internal/engine/resources DeleteProjectPartialFailure": {
		typ: reflect.TypeFor[resources.DeleteProjectPartialFailure](),
		variants: []detailsVariant{
			{name: "default"},
			{name: "a failed object shows its message", sets: []string{"Objects[].Outcome"}, set: setDetails("Objects[].Outcome", safety.OutcomeFailed)},
		},
		notShown: map[string]string{"Objects[].Code": detailsNeverPrinted},
	},
	"internal/skillinstall Report": {
		typ: reflect.TypeFor[skillinstall.Report](),
		variants: []detailsVariant{
			{name: "an action"},
			{name: "a verdict", sets: []string{"Outcomes[].Action"}, set: setDetails("Outcomes[].Action", "")},
		},
		notShown: map[string]string{"Outcomes[].CLIVersion": detailsNeverPrinted},
	},
	"internal/token DeleteTokenFailure": {
		typ:      reflect.TypeFor[token.DeleteTokenFailure](),
		variants: notRevokedVariants("NotRevoked"),
		notShown: joinNotShown(
			map[string]string{
				"Kept[]":             detailsNeverPrinted,
				"Revoked[].Ref.Kind": detailsNeverPrinted,
				"Revoked[].Ref.Name": detailsNeverPrinted,
				"Revoked[].Source":   detailsNeverPrinted,
				"Revoked[].Server":   detailsNeverPrinted,
			},
			notShownFor(detailsNotRevokedNotShown, "NotRevoked[]"),
		),
	},
	"internal/token RevocationReport": {
		typ:      reflect.TypeFor[token.RevocationReport](),
		variants: notRevokedVariants("NotRevoked"),
		notShown: joinNotShown(
			map[string]string{
				"Revoked[].Ref.Kind": detailsNeverPrinted,
				"Revoked[].Ref.Name": detailsNeverPrinted,
				"Revoked[].Source":   detailsNeverPrinted,
				"Revoked[].Server":   detailsNeverPrinted,
			},
			notShownFor(detailsNotRevokedNotShown, "NotRevoked[]"),
		),
	},
	"internal/token directWaitDetails": {testPrefix: "TestDirectWaitDetailsTextIsEscaped"},
}

// detailsPlant is a value of typ with value(path) in every text field, then
// the variant's changes.
func detailsPlant(t *testing.T, row detailsRow, variant detailsVariant, value func(path string) string) clierr.TextDetails {
	t.Helper()
	planted := remedytest.PlantText(t, row.typ, value)
	if variant.set != nil {
		variant.set(t, planted)
	}
	d, ok := reflect.TypeAssert[clierr.TextDetails](planted)
	require.True(t, ok, "%s has TextDetails", row.typ)
	return d
}

func renderDetails(t *testing.T, d clierr.TextDetails, format string) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, runtime.RenderError(&b, format, clierr.New(clierr.CodeGeneric, "m").WithDetails(d)))
	return b.String()
}

// detailsHiddenTail follows the path in an unsafe plant: an escape, a
// right-to-left override, a zero-width space and a line separator, then text.
const detailsHiddenTail = "a\x1b\u202e\u200b\u2028b"

func detailsSafe(path string) string   { return "<" + path + ">" }
func detailsUnsafe(path string) string { return "<" + path + ">" + detailsHiddenTail }

// planted reports whether the variant plants path.
func (v detailsVariant) plants(path string) bool { return !slices.Contains(v.sets, path) }

// detailsShown is the text paths of the row's type that the safe plant of
// variant shows in the default text.
func detailsShown(t *testing.T, row detailsRow, variant detailsVariant) []string {
	t.Helper()
	text := renderDetails(t, detailsPlant(t, row, variant, detailsSafe), "")
	var out []string
	for _, p := range remedytest.TextPaths(row.typ) {
		if variant.plants(p) && strings.Contains(text, detailsSafe(p)) {
			out = append(out, p)
		}
	}
	return out
}

func checkDetailsRow(t *testing.T, row detailsRow) {
	t.Helper()
	paths := remedytest.TextPaths(row.typ)
	shownAnywhere := map[string]bool{}
	for _, variant := range row.variants {
		for _, p := range variant.sets {
			require.Contains(t, paths, p, "variant %q sets a path the type has", variant.name)
		}
		shown := detailsShown(t, row, variant)
		for _, p := range shown {
			shownAnywhere[p] = true
		}
		require.NotEmpty(t, shown, "variant %q shows a value", variant.name)

		safeText := renderDetails(t, detailsPlant(t, row, variant, detailsSafe), "")
		var want strings.Builder
		want.WriteString("error (GENERIC): m\n")
		for _, l := range detailsPlant(t, row, variant, detailsSafe).TextDetails() {
			want.WriteString("  " + l.String() + "\n")
		}
		assert.Equal(t, want.String(), safeText, "variant %q: safe values print as the plain join of each line", variant.name)
		for _, p := range shown {
			assert.NotContains(t, safeText, strconv.Quote(detailsSafe(p)), "variant %q: %s is not quoted when it holds nothing hidden", variant.name, p)
		}

		unsafeText := renderDetails(t, detailsPlant(t, row, variant, detailsUnsafe), "")
		assert.True(t, strings.HasPrefix(unsafeText, "error (GENERIC): m\n"), "variant %q: the header is unchanged: %q", variant.name, unsafeText)
		assert.Negative(t, strings.IndexFunc(strings.ReplaceAll(unsafeText, "\n", ""), clierr.IsUnsafeToDisplay), "variant %q: no hidden rune is printed: %q", variant.name, unsafeText)
		for _, p := range shown {
			value := detailsUnsafe(p)
			assert.Contains(t, unsafeText, strconv.Quote(value), "variant %q: %s shows as an escaped string", variant.name, p)
			assert.NotContains(t, unsafeText, clierr.WithoutUnsafeToDisplay(value), "variant %q: %s has nothing removed", variant.name, p)

			plain := func(tail string) func(path string) string {
				return func(path string) string {
					if path == p {
						return "<" + path + ">" + tail
					}
					return detailsSafe(path)
				}
			}
			a, b := plain("ab")(p), plain("a\u200bb")(p)
			assert.Equal(t, output.Printable(a), output.Printable(b), "the two planted values are the same under output.Printable")
			textA := renderDetails(t, detailsPlant(t, row, variant, plain("ab")), "")
			textB := renderDetails(t, detailsPlant(t, row, variant, plain("a\u200bb")), "")
			assert.NotEqual(t, textA, textB, "variant %q: two values of %s that differ only by a hidden character print differently", variant.name, p)

			invalid := "<" + p + ">a\x9bb"
			invalidText := renderDetails(t, detailsPlant(t, row, variant, plain("a\x9bb")), "")
			assert.Contains(t, invalidText, strconv.Quote(invalid), "variant %q: %s with an invalid byte shows as an escaped string", variant.name, p)
			assert.NotContains(t, invalidText, "\x9b", "variant %q: %s prints no invalid byte", variant.name, p)
			assert.NotContains(t, invalidText, "\ufffd", "variant %q: %s prints no replacement character", variant.name, p)
			assert.True(t, utf8.ValidString(invalidText), "variant %q: the text of %s is valid UTF-8: %q", variant.name, p, invalidText)

			c, d := plain("a\xffb")(p), plain("a\x9bb")(p)
			assert.Equal(t, output.Printable(c), output.Printable(d), "the two planted invalid-byte values are the same under output.Printable")
			textC := renderDetails(t, detailsPlant(t, row, variant, plain("a\xffb")), "")
			textD := renderDetails(t, detailsPlant(t, row, variant, plain("a\x9bb")), "")
			assert.NotEqual(t, textC, textD, "variant %q: two values of %s that differ only by an invalid byte print differently", variant.name, p)

			quoted := renderDetails(t, detailsPlant(t, row, variant, func(path string) string {
				if path == p {
					return "<" + path + ">o'a\u200bb"
				}
				return detailsSafe(path)
			}), "")
			assert.Contains(t, quoted, "<"+p+">o'"+strconv.Quote("a\u200bb"), "variant %q: %s with a single quote and a hidden character is quoted only where it needs it", variant.name, p)
		}

		planted := detailsPlant(t, row, variant, detailsUnsafe)
		raw, err := json.Marshal(planted)
		require.NoError(t, err)
		var wantDetails any
		require.NoError(t, json.Unmarshal(raw, &wantDetails))
		var fromJSON, fromYAML struct {
			Data struct {
				Error struct {
					Details any `json:"details"`
				} `json:"error"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(renderDetails(t, planted, "json")), &fromJSON))
		assert.Equal(t, wantDetails, fromJSON.Data.Error.Details, "variant %q: json keeps every value exactly", variant.name)
		require.NoError(t, yaml.Unmarshal([]byte(renderDetails(t, planted, "yaml")), &fromYAML))
		assert.Equal(t, wantDetails, fromYAML.Data.Error.Details, "variant %q: yaml keeps every value exactly", variant.name)
	}
	for _, p := range paths {
		_, listed := row.notShown[p]
		assert.True(t, shownAnywhere[p] || listed, "%s: the text path %s shows in no variant and is not listed", row.typ, p)
		assert.False(t, shownAnywhere[p] && listed, "%s: the text path %s is listed as not shown and shows", row.typ, p)
	}
	for _, p := range slices.Sorted(maps.Keys(row.notShown)) {
		assert.Contains(t, paths, p, "%s: the listed path %s is a text path of the type", row.typ, p)
	}
}

// TestEveryDetailsPayloadShowsItsValuesAsEscapes asserts, for each payload
// type of the production tree with a TextDetails method, that the default
// text shows every value that holds a hidden character as a double-quoted
// string with escapes, in place; that two values that differ only by a hidden
// character print differently; that safe values print as the plain join of
// the lines; and that json and yaml hold each value exactly. A type with no
// row, and a row with no type, fail.
func TestEveryDetailsPayloadShowsItsValuesAsEscapes(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var keys []string
	for key, ty := range textDetailsTypes(tree.files, tree.paths) {
		if ty.textDetails && key != "internal/remedytest MarkedDetails" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	require.Len(t, keys, 13, "the scan found the 13 payload types: %v", keys)
	for _, key := range keys {
		assert.Contains(t, detailsRows, key, "%s has no row in detailsRows", key)
	}
	for _, key := range slices.Sorted(maps.Keys(detailsRows)) {
		row := detailsRows[key]
		if !slices.Contains(keys, key) {
			t.Errorf("the row %s matches no type with TextDetails: remove it", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			if row.testPrefix != "" {
				dir, _, _ := strings.Cut(key, " ")
				assert.True(t, slices.ContainsFunc(testFunctionNames(t, dir), func(name string) bool {
					return strings.HasPrefix(name, row.testPrefix)
				}), "%s: no test function of %s starts with %s (%s)", key, dir, row.testPrefix, detailsUnexportedType)
				return
			}
			checkDetailsRow(t, row)
		})
	}
}

// detailsForbidden are the functions a TextDetails body, and every function it
// reaches, never uses, called or passed as a value, keyed as objectKey names
// them: the renderer escapes the values, so a body that escapes, quotes or
// removes characters itself would show a value twice over or in another form
// than the table asserts.
var detailsForbidden = map[string]bool{
	"internal/clierr.IDText":                 true,
	"internal/clierr.WithoutUnsafeToDisplay": true,
	"internal/clierr.ShellQuote":             true,
	"internal/clierr.ShellQuoteArgs":         true,
	"internal/output.Printable":              true,
	"internal/output.PrintableLines":         true,
	"internal/runtime.PrintableRemedies":     true,
	"strconv.Quote":                          true,
	"strconv.QuoteToASCII":                   true,
}

// detailsLabelHelpers are the functions of internal/clierr that build a part
// from a label, with the index of the label parameter. Their own bodies take
// the label from a parameter, so the scan checks the argument at each call
// and does not read the bodies.
var detailsLabelHelpers = map[string]int{
	"internal/clierr.LabelPart":     0,
	"internal/clierr.ValuePart":     0,
	"internal/clierr.ListParts":     0,
	"internal/clierr.TokenRefParts": 0,
}

// detailsBuilders are the functions the scan does not follow into: the helpers
// above and the plain constructors and join of internal/clierr/detailline.go.
var detailsBuilders = map[string]bool{
	"internal/clierr.Line":              true,
	"internal/clierr.DetailLine.String": true,
}

const detailsPartType = "DetailPart"

// detailsScan holds one load of the module for the payload scan.
type detailsScan struct {
	funcs    map[string]moduleFunction
	assigns  map[*ast.BlockStmt]map[types.Object]*detailsAssigned
	seen     map[string]bool
	fset     func(pos gotoken.Pos) string
	findings []string
	labels   int
}

// detailsAssigned is what a local variable is given: the expressions it is
// assigned, and whether it is also given something no expression says (a call
// that returns several values).
type detailsAssigned struct {
	exprs   []ast.Expr
	tainted bool
}

func (sc *detailsScan) report(pos gotoken.Pos, format string, args ...any) {
	sc.findings = append(sc.findings, sc.fset(pos)+": "+fmt.Sprintf(format, args...))
}

// assignedIn is, for each local variable of body, the expressions it is
// assigned, by :=, = or += or a var declaration. A variable declared with no
// value is present with no expression: it holds its zero value.
func (sc *detailsScan) assignedIn(info *types.Info, body *ast.BlockStmt) map[types.Object]*detailsAssigned {
	if got, ok := sc.assigns[body]; ok {
		return got
	}
	out := map[types.Object]*detailsAssigned{}
	entry := func(id *ast.Ident) *detailsAssigned {
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if v, ok := obj.(*types.Var); !ok || v.IsField() {
			return nil
		}
		if out[obj] == nil {
			out[obj] = &detailsAssigned{}
		}
		return out[obj]
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				id, ok := ast.Unparen(lhs).(*ast.Ident)
				if !ok {
					continue
				}
				e := entry(id)
				if e == nil {
					continue
				}
				switch {
				case n.Tok != gotoken.DEFINE && n.Tok != gotoken.ASSIGN && n.Tok != gotoken.ADD_ASSIGN:
					e.tainted = true
				case len(n.Lhs) == len(n.Rhs):
					e.exprs = append(e.exprs, n.Rhs[i])
				default:
					e.tainted = true
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				e := entry(id)
				switch {
				case e == nil:
				case len(n.Values) == 0:
				case len(n.Values) == len(n.Names):
					e.exprs = append(e.exprs, n.Values[i])
				default:
					e.tainted = true
				}
			}
		}
		return true
	})
	sc.assigns[body] = out
	return out
}

// onlyNumbers reports whether e is made of constants and numbers: it is a
// constant expression, a local variable assigned only such expressions, a call
// of a function of the module whose every return is such an expression, a
// number written with strconv.Itoa or strconv.FormatInt, a call of fmt.Sprintf
// with a constant format of text and %d verbs and integer arguments, or a
// concatenation of these.
func (sc *detailsScan) onlyNumbers(info *types.Info, body *ast.BlockStmt, e ast.Expr, active map[any]bool) bool {
	e = ast.Unparen(e)
	if tv, ok := info.Types[e]; ok && tv.Value != nil {
		return true
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		return e.Op == gotoken.ADD && sc.onlyNumbers(info, body, e.X, active) && sc.onlyNumbers(info, body, e.Y, active)
	case *ast.Ident:
		obj := info.Uses[e]
		a := sc.assignedIn(info, body)[obj]
		if a == nil || a.tainted || active[obj] {
			return false
		}
		active[obj] = true
		defer delete(active, obj)
		for _, x := range a.exprs {
			if !sc.onlyNumbers(info, body, x, active) {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		fn := calledFunction(info, e)
		if fn == nil {
			return false
		}
		switch objectKey(fn) {
		case "strconv.Itoa", "strconv.FormatInt":
			return len(e.Args) > 0 && isIntegerType(info.TypeOf(e.Args[0]))
		case "fmt.Sprintf":
			return sc.numberFormat(info, e)
		}
		key := funcKey(fn)
		mf, ok := sc.funcs[key]
		if !ok || active[key] {
			return false
		}
		active[key] = true
		defer delete(active, key)
		return sc.returnsOnlyNumbers(mf, active)
	}
	return false
}

func isIntegerType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

// numberFormat reports whether call is fmt.Sprintf with a constant format
// whose only verbs are %d and arguments that are all integers.
func (sc *detailsScan) numberFormat(info *types.Info, call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	format, ok := constString(info, call.Args[0])
	if !ok {
		return false
	}
	verbs := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		switch {
		case i < len(format) && format[i] == '%':
		case i < len(format) && format[i] == 'd':
			verbs++
		default:
			return false
		}
	}
	if verbs != len(call.Args)-1 {
		return false
	}
	for _, a := range call.Args[1:] {
		if !isIntegerType(info.TypeOf(a)) {
			return false
		}
	}
	return true
}

// returnsOnlyNumbers reports whether every return of the function, the
// returns of function literals in it left out, gives one expression that
// onlyNumbers accepts.
func (sc *detailsScan) returnsOnlyNumbers(mf moduleFunction, active map[any]bool) bool {
	ok, returned := true, false
	ast.Inspect(mf.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returned = true
			if len(n.Results) != 1 || !sc.onlyNumbers(mf.info, mf.body, n.Results[0], active) {
				ok = false
			}
		}
		return ok
	})
	return ok && returned
}

// checkLabel records a finding when the expression that sets a label is not
// made of constants and numbers.
func (sc *detailsScan) checkLabel(info *types.Info, body *ast.BlockStmt, e ast.Expr, chain []string) {
	sc.labels++
	if !sc.onlyNumbers(info, body, e, map[any]bool{}) {
		sc.report(e.Pos(), "the label %s holds more than constants and numbers (%s)", types.ExprString(e), strings.Join(chain, " -> "))
	}
}

// walk reads the body of the function key names, and of every function of the
// module it refers to.
func (sc *detailsScan) walk(key string, chain []string) {
	mf, ok := sc.funcs[key]
	if !ok || sc.seen[key] || detailsBuilders[key] {
		return
	}
	sc.seen[key] = true
	chain = append(slices.Clone(chain), key)
	info := mf.info
	ast.Inspect(mf.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			fn, ok := info.Uses[n].(*types.Func)
			if !ok || fn.Pkg() == nil {
				return true
			}
			name := funcKey(fn)
			if detailsForbidden[name] {
				sc.report(n.Pos(), "%s is used (%s): a payload hands over raw values and the renderer escapes them", name, strings.Join(chain, " -> "))
				return true
			}
			if _, helper := detailsLabelHelpers[name]; !helper {
				sc.walk(name, chain)
			}
		case *ast.CallExpr:
			if fn := calledFunction(info, n); fn != nil && fn.Pkg() != nil {
				if i, ok := detailsLabelHelpers[funcKey(fn)]; ok && i < len(n.Args) {
					sc.checkLabel(info, mf.body, n.Args[i], chain)
				}
			}
		case *ast.CompositeLit:
			if !isNamedType(derefType(info.TypeOf(n)), clierrPath, detailsPartType) {
				return true
			}
			for i, elt := range n.Elts {
				switch elt := elt.(type) {
				case *ast.KeyValueExpr:
					if id, ok := elt.Key.(*ast.Ident); ok && id.Name == "Label" {
						sc.checkLabel(info, mf.body, elt.Value, chain)
					}
				default:
					if i == 0 {
						sc.checkLabel(info, mf.body, elt, chain)
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Label" && isNamedType(derefType(info.TypeOf(sel.X)), clierrPath, detailsPartType) && len(n.Lhs) == len(n.Rhs) {
					sc.checkLabel(info, mf.body, n.Rhs[i], chain)
				}
			}
		}
		return true
	})
}

// scanDetailsText walks from every TextDetails method of pkgs and returns the
// findings, the number of methods it started from and the number of labels it
// checked.
func scanDetailsText(pkgs []*packages.Package) (findings []string, roots, labels int) {
	sc := &detailsScan{
		funcs:   moduleFunctions(pkgs),
		assigns: map[*ast.BlockStmt]map[types.Object]*detailsAssigned{},
		seen:    map[string]bool{},
	}
	if len(pkgs) > 0 {
		fset := pkgs[0].Fset
		sc.fset = func(pos gotoken.Pos) string { return fset.Position(pos).String() }
	}
	var rootKeys []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Recv == nil || fd.Name.Name != "TextDetails" {
					continue
				}
				if fn, ok := pkg.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					rootKeys = append(rootKeys, funcKey(fn))
				}
			}
		}
	}
	slices.Sort(rootKeys)
	for _, key := range rootKeys {
		sc.walk(key, nil)
	}
	slices.Sort(sc.findings)
	return slices.Compact(sc.findings), len(rootKeys), sc.labels
}

// TestNoDetailsBodyEscapesOrFiltersAValue asserts the scan finds nothing in
// the production tree, built for this machine and for GOOS=windows: no
// TextDetails body, nor a function it refers to, uses one of the functions in
// detailsForbidden, and every label it writes is made of constants and
// numbers. The scan reads the code, so a function reached through an
// interface (the nested TextDetails call: each concrete type is a root of its
// own), a package-level func variable, a type that gets TextDetails by
// embedding, a verb that calls String or Error (%q and the others), and a
// producer that builds a field's value before the payload exists are outside
// it; the table and the escaped-once tests assert those by their output.
func TestNoDetailsBodyEscapesOrFiltersAValue(t *testing.T) {
	t.Parallel()
	host, err := fieldManagerPackages()
	require.NoError(t, err)
	loads := map[string][]*packages.Package{"this machine": host}
	windows, err := otherGOOSPackages("windows")
	require.NoError(t, err)
	if windows != nil {
		loads["windows"] = windows
	}
	for _, name := range slices.Sorted(maps.Keys(loads)) {
		findings, roots, labels := scanDetailsText(loads[name])
		assert.Equal(t, 14, roots, "%s: the scan started from every TextDetails method of the production tree", name)
		assert.Positive(t, labels, "%s: the scan read labels", name)
		for _, f := range findings {
			t.Errorf("%s: %s", name, f)
		}
	}
}

const detailsFixtureDir = "test/conformance/testdata/detailstext"

// detailsFixtures type-checks the fixture packages once and groups them by
// case: the package of a case and the packages below it.
var detailsFixtures = sync.OnceValues(func() (map[string][]*packages.Package, error) {
	pkgs, err := loadPackages("./"+detailsFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string][]*packages.Package{}
	for _, pkg := range pkgs {
		rest := strings.TrimPrefix(pkg.PkgPath, modulePrefix+detailsFixtureDir+"/")
		name, _, _ := strings.Cut(rest, "/")
		out[name] = append(out[name], pkg)
	}
	return out, nil
})

var detailsFixtureCases = []struct {
	fixture  string
	findings int
}{
	{"direct", 1},
	{"samepackage", 1},
	{"otherpackage", 1},
	{"funcvalue", 1},
	{"labelfield", 1},
	{"labelparameter", 1},
	{"labelexpression", 1},
	{"clean", 0},
}

// TestDetailsScanFixtures asserts the scan on type-checked fixture packages: a
// body that calls an escaping function itself, through a helper of its own
// package or through a helper of another package, or passes one as a value,
// is reported; so is a label that holds a field, a function parameter or a
// call that is not a constant; a payload of labels made of constants, a local
// variable of constants, a function that returns constants and numbers, and
// values passed as values is accepted.
func TestDetailsScanFixtures(t *testing.T) {
	t.Parallel()
	fixtures, err := detailsFixtures()
	require.NoError(t, err)
	for _, tc := range detailsFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkgs, ok := fixtures[tc.fixture]
			require.True(t, ok, "fixture %s", tc.fixture)
			findings, roots, labels := scanDetailsText(pkgs)
			assert.Equal(t, 1, roots)
			assert.Positive(t, labels)
			assert.Len(t, findings, tc.findings, "%v", findings)
		})
	}
}

// TestDetailsFixturesAreAllListed asserts every fixture on disk is in the
// table above, so a fixture added without its row fails.
func TestDetailsFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(detailsFixtureDir)
	require.NoError(t, err)
	fixtures, err := detailsFixtures()
	require.NoError(t, err)
	var onDisk, listed []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range detailsFixtureCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, onDisk, listed)
	assert.ElementsMatch(t, listed, slices.Collect(maps.Keys(fixtures)))
}
