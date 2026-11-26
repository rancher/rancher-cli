package user

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCreateSecretWhoseResultIsUnknownKeepsOutcomeUnknown asserts that a
// password Secret create whose response was lost, after the user was
// created, keeps outcomeUnknown and stays not retryable, with the committed
// details of the user.
func TestCreateSecretWhoseResultIsUnknownKeepsOutcomeUnknown(t *testing.T) {
	t.Parallel()
	lost := clierr.UnknownOutcome("the request to create secrets 'u-real2'", clierr.OutcomeCause(io.EOF), io.EOF, clierr.Command{})
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2", secretCreateErr: lost}
	_, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.NoError(t, err)

	_, err = exec(t.Context())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.True(t, ce.OutcomeUnknown)
	assert.False(t, ce.Retryable)
	assert.Equal(t, lost.Message, ce.Message)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.Equal(t, "u-real2", details.ResourceName)
	assert.Equal(t, "the user u-real2 was created and its password Secret may exist; check whether it exists first; if it is there, the user already has that password; only if it is not there, to set a password without putting it on the command line or in shell history, set PASSWORD in your shell (a password containing a double quote or backslash must be JSON-escaped in the manifest) and add --apply after reading the preview of: "+
		`echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"u-real2","namespace":"cattle-local-user-passwords"},"stringData":{"password":"'"$PASSWORD"'"}}' | `+
		clierr.Rancher+" create resource -f - --cluster local --dry-run", details.Recovery)
}

// TestCreateSecretWhoseResultIsUnknownSaysToReadTheSecretFirst asserts the
// recovery of a password Secret create whose result is unknown. The Secret may
// exist: the recovery says to read it first with the command the error names,
// that a Secret that is there means the user already has that password, and
// to set a password only if it is not there; it never says the user was
// created without a password. An id that is not an object name gets prose
// only, with the read command when the error names one.
func TestCreateSecretWhoseResultIsUnknownSaysToReadTheSecretFirst(t *testing.T) {
	t.Parallel()
	read := clierr.NewCommand("get").Word("secret").Arg("u-real2").FlagValue("--namespace", "cattle-local-user-passwords")
	const (
		steps = "to set a password without putting it on the command line or in shell history, set PASSWORD in your shell"
		there = "; if it is there, the user already has that password"
	)
	for _, tc := range []struct {
		name     string
		userName string
		read     clierr.Command
		want     string
	}{
		{"with a read command", "u-real2", read,
			"the user u-real2 was created and its password Secret may exist; read it first with: " + clierr.Rancher + " get secret 'u-real2' --namespace 'cattle-local-user-passwords'" + there + "; only if it is not there, " + steps},
		{"without a read command", "u-real2", clierr.Command{},
			"the user u-real2 was created and its password Secret may exist; check whether it exists first" + there + "; only if it is not there, " + steps},
		{"an id that is not an object name", "a'b", clierr.Command{},
			"the user 'a'\\''b' was created and its password Secret may exist; check whether it exists first" + there + "; the CLI prints no command for an id it cannot quote"},
		{"an id that is not an object name, with a read command", "a'b", read,
			"the user 'a'\\''b' was created and its password Secret may exist; read it first with: " + clierr.Rancher + " get secret 'u-real2' --namespace 'cattle-local-user-passwords'" + there + "; the CLI prints no command for an id it cannot quote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lost := clierr.UnknownOutcome("the request to create secrets 'x'", clierr.OutcomeCause(io.EOF), io.EOF, tc.read)
			client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: tc.userName, secretCreateErr: lost}
			_, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.NotContains(t, details.Recovery, "without a password")
			if tc.userName == "u-real2" {
				assert.True(t, strings.HasPrefix(details.Recovery, tc.want), details.Recovery)
				assert.Contains(t, details.Recovery, clierr.Rancher+" create resource -f - --cluster local --dry-run")
				return
			}
			assert.Equal(t, tc.want, details.Recovery)
		})
	}
}

// TestCreateSecretWhoseResultIsUnknownStillNamesTheRolesNotBound asserts that
// when the password Secret create has an unknown result and global roles were
// given, no binding is attempted and the recovery adds the grant clause of
// each role after the Secret clause.
func TestCreateSecretWhoseResultIsUnknownStillNamesTheRolesNotBound(t *testing.T) {
	t.Parallel()
	lost := clierr.UnknownOutcome("the request to create secrets 'x'", clierr.OutcomeCause(io.EOF), io.EOF, clierr.Command{})
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
		roles:           []*unstructured.Unstructured{defaultRole("alpha"), defaultRole("beta")},
		secretCreateErr: lost}
	_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.NoError(t, err)
	_, err = exec(t.Context())
	ce, details := committedDetails(t, err)
	assert.True(t, ce.OutcomeUnknown)
	recovery, _ := details["recovery"].(string)
	assert.Contains(t, recovery, "the user u-real2 was created and its password Secret may exist; check whether it exists first")
	assert.Contains(t, recovery, "--dry-run; global role alpha was not bound; preview the grant with: "+clierr.Rancher+" grant 'u-real2' --global --role 'alpha' --dry-run; global role beta was not bound; preview the grant with: "+clierr.Rancher+" grant 'u-real2' --global --role 'beta' --dry-run")
	assert.Empty(t, bindingRoles(client, false), "no binding is attempted")
}

// TestCreateBindingWhoseResultIsUnknownKeepsOutcomeUnknown asserts that when
// a global role's binding create has an unknown result, the error returned
// has OutcomeUnknown, is not retryable and is the first error whose result is
// unknown, whatever other binding create was refused before or after it. The
// recovery words each role by what is known: a refused role was not bound and
// has a grant to preview; a role whose result is unknown may be bound, and the
// clause says which fields to look for and, when the error names a read
// command, the command with -o json, because the default table of a
// GlobalRoleBinding shows neither field.
func TestCreateBindingWhoseResultIsUnknownKeepsOutcomeUnknown(t *testing.T) {
	t.Parallel()
	list := clierr.NewCommand("get").Word("globalrolebinding")
	lost := func(which string, read clierr.Command) *clierr.Error {
		return clierr.UnknownOutcome("the request to create globalrolebindings.management.cattle.io ("+which+")", clierr.OutcomeCause(io.EOF), io.EOF, read)
	}
	refused := clierr.New(clierr.CodeRBAC, "alpha is refused")
	bound := func(id, role string, hinted bool) string {
		text := "global role " + role + " may be bound: look for a binding with userName " + id + " and globalRoleName " + role
		if hinted {
			text += " in the output of: " + clierr.Rancher + " get globalrolebinding -o json"
		}
		return text
	}
	notBound := func(role string) string {
		return "global role " + role + " was not bound; preview the grant with: " + clierr.Rancher + " grant 'u-real2' --global --role '" + role + "' --dry-run"
	}
	first, second := lost("first", list), lost("second", list)
	for _, tc := range []struct {
		name     string
		userName string
		errs     map[string]error
		want     *clierr.Error
		recovery []string
		absent   []string
	}{
		{"the refusal comes first", "u-real2", map[string]error{"alpha": refused, "beta": first}, first,
			[]string{notBound("alpha"), bound("'u-real2'", "beta", true)}, []string{"global role beta was not bound"}},
		{"the lost response comes first", "u-real2", map[string]error{"alpha": first, "beta": refused}, first,
			[]string{bound("'u-real2'", "alpha", true), "global role beta was not bound"}, []string{"global role alpha was not bound"}},
		{"the error names no read command", "u-real2", map[string]error{"alpha": lost("none", clierr.Command{}), "beta": refused}, lost("none", clierr.Command{}),
			[]string{bound("'u-real2'", "alpha", false) + "; global role beta was not bound"}, []string{"in the output of"}},
		{"two lost responses", "u-real2", map[string]error{"alpha": first, "beta": second}, first,
			[]string{bound("'u-real2'", "alpha", true) + "; " + bound("'u-real2'", "beta", true)}, []string{"was not bound"}},
		{"an id that is not an object name", "a'b", map[string]error{"alpha": first, "beta": refused}, first,
			[]string{"the user 'a'\\''b' was created; " + bound("'a'\\''b'", "alpha", true) + "; global role beta was not bound"}, []string{"grant"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeUserClient{
				userPreviewName: "u-preview1", userRealName: tc.userName,
				roles:          []*unstructured.Unstructured{defaultRole("alpha"), defaultRole("beta")},
				bindingRealErr: tc.errs,
			}
			_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			ce, details := committedDetails(t, err)
			assert.True(t, ce.OutcomeUnknown)
			assert.False(t, ce.Retryable)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, tc.want.Message, ce.Message)
			recovery, _ := details["recovery"].(string)
			for _, want := range tc.recovery {
				assert.Contains(t, recovery, want)
			}
			for _, absent := range tc.absent {
				assert.NotContains(t, recovery, absent)
			}
			assert.Equal(t, []string{"alpha", "beta"}, bindingRoles(client, false), "every role is attempted")
		})
	}
}
