package rbac

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubject(t *testing.T) {
	tests := []struct {
		name, arg, typeFlag string
		want                Subject
		wantErrCode         string
	}{
		{name: "bare defaults to user", arg: "jane", want: Subject{Kind: SubjectUser, Bare: "jane"}},
		{name: "bare explicit user", arg: "u-abc", typeFlag: "user", want: Subject{Kind: SubjectUser, Bare: "u-abc"}},
		{name: "bare group rejected", arg: "devs", typeFlag: "group", wantErrCode: clierr.CodeValidation},
		{name: "local infers user", arg: "local://u-abc", want: Subject{Kind: SubjectUser, Principal: "local://u-abc"}},
		{name: "provider user infers user", arg: "okta_user://x", want: Subject{Kind: SubjectUser, Principal: "okta_user://x"}},
		{name: "provider group infers group", arg: "okta_group://y", want: Subject{Kind: SubjectGroup, Principal: "okta_group://y"}},
		{name: "org infers group", arg: "github_org://42", want: Subject{Kind: SubjectGroup, Principal: "github_org://42"}},
		{name: "team infers group", arg: "github_team://7", want: Subject{Kind: SubjectGroup, Principal: "github_team://7"}},
		{name: "unknown prefix without type rejected", arg: "custom://alice", wantErrCode: clierr.CodeValidation},
		{name: "unknown prefix with explicit user", arg: "custom://alice", typeFlag: "user", want: Subject{Kind: SubjectUser, Principal: "custom://alice"}},
		{name: "unknown prefix with explicit group", arg: "vendor_service://id", typeFlag: "group", want: Subject{Kind: SubjectGroup, Principal: "vendor_service://id"}},
		{name: "type conflict user-vs-group", arg: "okta_group://y", typeFlag: "user", wantErrCode: clierr.CodeValidation},
		{name: "type conflict group-vs-user", arg: "okta_user://x", typeFlag: "group", wantErrCode: clierr.CodeValidation},
		{name: "type agrees ok", arg: "okta_group://y", typeFlag: "group", want: Subject{Kind: SubjectGroup, Principal: "okta_group://y"}},
		{name: "system principal rejected", arg: "system://c-abc", wantErrCode: clierr.CodeValidation},
		{name: "invalid type flag", arg: "jane", typeFlag: "robot", wantErrCode: clierr.CodeValidation},
		{name: "empty arg rejected", arg: "", wantErrCode: clierr.CodeValidation},
		{name: "empty remainder rejected", arg: "local://", wantErrCode: clierr.CodeValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSubject(tt.arg, tt.typeFlag)
			if tt.wantErrCode != "" {
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, tt.wantErrCode, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Vector cross-checked against rancher/rancher EnsureUser
// (pkg/auth/providers/common/usermanager.go:281-283):
// sha256 of the principal, base32 no padding, first 10 chars, lowercased,
// "u-" prefix.
func TestProvisionedUserID(t *testing.T) {
	// Fixed vector: sha256("okta_user://alice") → base32 (no padding) →
	// first 10 chars lowercased, "u-" prefix. Independently computed;
	// matches rancher's EnsureUser derivation.
	assert.Equal(t, "u-wlhgvvw57w", ProvisionedUserID("okta_user://alice"))
	assert.Regexp(t, `^u-[a-z2-7]{10}$`, ProvisionedUserID("okta_user://bob"))
	assert.NotEqual(t, ProvisionedUserID("okta_user://alice"), ProvisionedUserID("okta_user://bob"))
}
