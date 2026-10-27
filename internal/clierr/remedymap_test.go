package clierr

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPrefix = " --config '/d'"

func expandTestPrefix(s string) string { return ExpandRemedies(s, testPrefix) }

// committedContextFields classifies every text field of
// CommittedContextDetails: the recovery step is prose that can name a
// command, the resource's name and its secret's are the user's or the
// server's, the kind is the CLI's own word.
var committedContextFields = textFieldLists{
	remedy: []string{"Recovery"},
	server: []string{"ResourceName", "SecretName"},
	other:  []string{"ResourceKind"},
}

// TestCommittedContextDetailsTextFieldsAreAllClassified asserts every text
// field of the type is in exactly one of the three lists.
func TestCommittedContextDetailsTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	assertEveryFieldClassified(t, reflect.TypeFor[CommittedContextDetails](), committedContextFields)
}

// TestCommittedContextDetailsMapTextMapsOnlyTheRecovery asserts MapText
// rewrites the recovery step, which can hold a remedy command, and no other
// field the lists name.
func TestCommittedContextDetailsMapTextMapsOnlyTheRecovery(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[CommittedContextDetails]()
	planted := plantMarked(t, typ, "", nil).Interface().(CommittedContextDetails)
	got, ok := planted.MapText(expandTestPrefix).(CommittedContextDetails)
	require.True(t, ok)
	assertMappedAsListed(t, planted, got, committedContextFields, committedContextFields.remedy, testPrefix)
	assert.Equal(t, "rancher"+testPrefix+" plant Recovery", got.Recovery)
	assert.Equal(t, planted.Committed, got.Committed)
	assert.Contains(t, planted.Recovery, RemedyMarker, "the original is untouched")
}

// TestCommittedContextDetailsHoldsNoCommandOnlyField asserts the type has no
// MapCommands: its recovery step is prose, and a renderer that offers no
// command keeps the prose before the command through MapText.
func TestCommittedContextDetailsHoldsNoCommandOnlyField(t *testing.T) {
	t.Parallel()
	_, ok := any(CommittedContextDetails{}).(RemedyCommands)
	assert.False(t, ok)
}

// loginFailureFields classifies every text field of LoginFailureDetails.
// The primary's own details are mapped through their own mapping when they
// have one; the token facts are token names, kinds, times and the server's
// answers to a revoke, which hold no command.
var loginFailureFields = textFieldLists{
	remedy: []string{
		"Primary.Message", "Primary.Hint", "Primary.Details",
		"CredentialRecovery.Command", "ConfigRecovery.Command", "Recovery",
	},
	server: []string{
		"Primary.Candidates[].ID", "Primary.Candidates[].Name",
		"CredentialRecovery.CompensationError", "SessionToken.Ref.Name",
		"SessionToken.Cleanup.Ref.Name", "SessionToken.Cleanup.Error",
		"SessionToken.Cleanup.TLSError", "DerivedToken.Ref.Name",
		"DerivedToken.Cleanup.Ref.Name", "DerivedToken.Cleanup.Error",
		"DerivedToken.Cleanup.TLSError", "ClusterToken.Ref.Name",
		"ClusterToken.Cleanup.Ref.Name", "ClusterToken.Cleanup.Error",
		"ClusterToken.Cleanup.TLSError", "ConfigRecovery.Entry",
		"ConfigRecovery.Prior.URL", "ConfigRecovery.Prior.CA",
		"ConfigRecovery.Prior.CAFingerprint", "ConfigRecovery.Prior.Provenance",
		"ConfigRecovery.Prior.TokenRef.Kind",
		"ConfigRecovery.Prior.TokenRef.Name", "ConfigRecovery.Prior.TokenUserID",
		"ConfigRecovery.Prior.CredentialExpiresAt",
		"ConfigRecovery.Prior.SupersededTokens[].Ref.Kind",
		"ConfigRecovery.Prior.SupersededTokens[].Ref.Name",
		"ConfigRecovery.Prior.SupersededTokens[].UserID",
		"ConfigRecovery.Prior.SupersededTokens[].ExpiresAt",
		"ConfigRecovery.Prior.SupersededTokens[].SupersededAt",
		"ConfigRecovery.Prior.SupersededTokens[].Server",
		"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Ref.Kind",
		"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Ref.Name",
		"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Result",
		"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.Error",
		"ConfigRecovery.Prior.SupersededTokens[].LastAttempt.TLSError",
	},
	other: []string{
		"Primary.Code", "Stage", "ConfigState", "CredentialState",
		"CredentialRecovery.Backend", "CredentialRecovery.State",
		"CredentialRecovery.PriorSource", "CredentialRecovery.DataLossRisk",
		"OperationMarker", "SessionToken.Outcome", "SessionToken.Kind",
		"SessionToken.Ref.Kind", "SessionToken.CreatedAt",
		"SessionToken.Cleanup.Ref.Kind", "SessionToken.Cleanup.Result",
		"SessionToken.Search.AuthProvider", "SessionToken.Search.HandoffAt",
		"SessionToken.Search.PollEndedAt",
		"SessionToken.Search.RequestExpiresAt", "SessionToken.ExpiresAt",
		"DerivedToken.Outcome", "DerivedToken.Kind", "DerivedToken.Ref.Kind",
		"DerivedToken.CreatedAt", "DerivedToken.Cleanup.Ref.Kind",
		"DerivedToken.Cleanup.Result", "DerivedToken.Search.AuthProvider",
		"DerivedToken.Search.HandoffAt", "DerivedToken.Search.PollEndedAt",
		"DerivedToken.Search.RequestExpiresAt", "DerivedToken.ExpiresAt",
		"ClusterToken.Outcome", "ClusterToken.Kind", "ClusterToken.Ref.Kind",
		"ClusterToken.CreatedAt", "ClusterToken.Cleanup.Ref.Kind",
		"ClusterToken.Cleanup.Result", "ClusterToken.Search.AuthProvider",
		"ClusterToken.Search.HandoffAt", "ClusterToken.Search.PollEndedAt",
		"ClusterToken.Search.RequestExpiresAt", "ClusterToken.ExpiresAt",
	},
}

// loginFailureCommandFields are the fields MapCommands rewrites: the two
// recovery commands and, by recursion, the primary's own details.
var loginFailureCommandFields = []string{"CredentialRecovery.Command", "ConfigRecovery.Command", "Primary.Details"}

// TestLoginFailureDetailsTextFieldsAreAllClassified asserts every text field
// of the type is in exactly one of the three lists.
func TestLoginFailureDetailsTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	assertEveryFieldClassified(t, reflect.TypeFor[LoginFailureDetails](), loginFailureFields)
}

// TestLoginFailureDetailsMapTextMapsOnlyTheRemedyFields asserts MapText
// rewrites the fields that can hold a remedy command, the primary's own
// details among them, and no other field the lists name.
func TestLoginFailureDetailsMapTextMapsOnlyTheRemedyFields(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[LoginFailureDetails]()
	planted := plantMarked(t, typ, "", nil).Interface().(LoginFailureDetails)
	got, ok := planted.MapText(expandTestPrefix).(LoginFailureDetails)
	require.True(t, ok)
	assertMappedAsListed(t, planted, got, loginFailureFields, loginFailureFields.remedy, testPrefix)
	inner, ok := got.Primary.Details.(markedDetails)
	require.True(t, ok, "the primary's details keep their type")
	assert.Equal(t, "rancher"+testPrefix+" plant Primary.Details", inner.Text)
}

// TestLoginFailureDetailsMapCommandsMapsOnlyTheCommandFields asserts
// MapCommands rewrites the two recovery commands and the primary's own
// details, and no other field.
func TestLoginFailureDetailsMapCommandsMapsOnlyTheCommandFields(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[LoginFailureDetails]()
	planted := plantMarked(t, typ, "", nil).Interface().(LoginFailureDetails)
	got, ok := planted.MapCommands(expandTestPrefix).(LoginFailureDetails)
	require.True(t, ok)
	assertMappedAsListed(t, planted, got, loginFailureFields, loginFailureCommandFields, testPrefix)
	for _, field := range loginFailureCommandFields {
		assert.Contains(t, loginFailureFields.remedy, field, "a command field is a remedy field")
	}
}

// TestDirectoryFreeWritesTheCommandsThatReadNoDirectory asserts every form
// of the closed set gives the plain command, and a command outside it
// panics.
func TestDirectoryFreeWritesTheCommandsThatReadNoDirectory(t *testing.T) {
	t.Parallel()
	for _, rest := range []string{
		"--help", "--version",
		"get --help", "get pod --help", "auth get-token --help", "config use-server --help", "oidc-client rotate-secret --help",
		"schema", "schema get pod", "completion", "completion bash", "skill", "skill install --force",
	} {
		assert.Equal(t, "rancher "+rest, DirectoryFree(rest), rest)
	}
	for _, rest := range []string{
		"", "get pod", "auth login", "config status", "schemas", "schema-x", "skillful", "completionbash",
		"Get --help", "get Pod --help", "get  pod --help", " get --help", "get --help ", "get pod --help --x",
		"get 'pod' --help", "get; ls --help", "--help --version", "--version --help", "-- help", "--config x get",
	} {
		assert.Panics(t, func() { DirectoryFree(rest) }, "%q is outside the closed set", rest)
	}
	assert.Equal(t, map[string]bool{"schema": true, "completion": true, "skill": true}, DirectoryFreeCommands)
}
