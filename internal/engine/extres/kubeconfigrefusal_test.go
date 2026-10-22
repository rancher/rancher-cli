package extres

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// versionFactory is a stubFactory that also reports the server version and
// the stored login's name, and counts the version requests.
type versionFactory struct {
	c           *fakeClient
	version     string
	versionErr  error
	login       string
	loginErr    error
	versionAsks *int
}

func (f versionFactory) Cluster(ctx context.Context, id string) (engine.ClusterClient, error) {
	return stubFactory{c: f.c}.Cluster(ctx, id)
}

func (f versionFactory) ImpersonatedCluster(ctx context.Context, id string, as engine.Impersonation) (engine.ClusterClient, error) {
	return stubFactory{c: f.c}.ImpersonatedCluster(ctx, id, as)
}

func (f versionFactory) ResolveCluster(ctx context.Context, s string) (string, error) {
	return stubFactory{c: f.c}.ResolveCluster(ctx, s)
}

func (f versionFactory) ServerVersion(context.Context) (string, error) {
	*f.versionAsks++
	return f.version, f.versionErr
}

func (f versionFactory) StoredLoginName() (string, error) { return f.login, f.loginErr }

var _ engine.ServerVersionProvider = versionFactory{}
var _ engine.StoredLoginNamer = versionFactory{}

func storeForbidden(t *testing.T, cause error) error {
	t.Helper()
	gr := schema.GroupResource{Group: "ext.cattle.io", Resource: "kubeconfigs"}
	ce := clierr.FromStatus(apierrors.NewForbidden(gr, "", cause), &clierr.Request{Cluster: "local"})
	require.NotNil(t, ce)
	require.Equal(t, clierr.CodeRBAC, ce.Code)
	return ce
}

func lookupForbidden(t *testing.T) error {
	t.Helper()
	return storeForbidden(t, fmt.Errorf("error getting request token %s: %w", "token-abc",
		errors.New(`tokens.management.cattle.io "token-abc" not found`)))
}

// TestRefuseUnsupportedTokenLookupRows asserts the reclassification of the
// request-token 403 on a server below 2.15, and that every other error, and
// every server version that is not known to be below 2.15, comes back as the
// same error value.
func TestRefuseUnsupportedTokenLookupRows(t *testing.T) {
	t.Parallel()
	impersonation := storeForbidden(t, errors.New("error getting request token: boom"))
	other := storeForbidden(t, errors.New("permission denied"))
	transport := errors.New("dial tcp: connection refused")
	notFound := clierr.New(clierr.CodeNotFound, "kubeconfigs.ext.cattle.io is forbidden: error getting request token x: gone")
	bareText := clierr.New(clierr.CodeRBAC, "error getting request token token-abc: not found")

	tests := []struct {
		name    string
		err     func(t *testing.T) error
		version string
		refused bool
	}{
		{"2.14.5 reclassified", lookupForbidden, "v2.14.5", true},
		{"2.14.0 reclassified", lookupForbidden, "v2.14.0", true},
		{"2.14.6 reclassified", lookupForbidden, "v2.14.6", true},
		{"no v prefix", lookupForbidden, "2.14.5", true},
		{"release candidate suffix", lookupForbidden, "v2.14.5-rc1", true},
		{"release branch head below the fix", lookupForbidden, "v2.14-head", true},
		{"2.15.0 unchanged", lookupForbidden, "v2.15.0", false},
		{"2.15.2 unchanged", lookupForbidden, "v2.15.2", false},
		{"2.16 alpha unchanged", lookupForbidden, "v2.16.0-alpha1", false},
		{"2.15 alpha before the fix keeps the RBAC error", lookupForbidden, "v2.15.0-alpha1", false},
		{"release branch head with the fix", lookupForbidden, "v2.16-head", false},
		{"dev unchanged", lookupForbidden, "dev", false},
		{"bare head unchanged", lookupForbidden, "head", false},
		{"empty unchanged", lookupForbidden, "", false},
		{"other forbidden text unchanged", func(*testing.T) error { return other }, "v2.14.5", false},
		{"impersonation text unchanged", func(*testing.T) error { return impersonation }, "v2.14.5", false},
		{"text without the resource prefix unchanged", func(*testing.T) error { return bareText }, "v2.14.5", false},
		{"not found with the text unchanged", func(*testing.T) error { return notFound }, "v2.14.5", false},
		{"transport error unchanged", func(*testing.T) error { return transport }, "v2.14.5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asks int
			clients := versionFactory{c: &fakeClient{}, version: tt.version, login: "prod", versionAsks: &asks}
			orig := tt.err(t)
			got := refuseUnsupportedTokenLookup(t.Context(), clients, orig)
			if !tt.refused {
				assert.Same(t, orig, got, "an error that is not refused comes back as it was")
				return
			}
			assert.Equal(t, 1, asks)
			ce, ok := got.(*clierr.Error)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
			assert.Equal(t, 11, ce.ExitCode())
			assert.Contains(t, ce.Message, "Rancher "+tt.version+" ")
			assert.Contains(t, ce.Message, "use Rancher 2.15 or later")
			assert.Equal(t, "rancher auth login 'prod' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
			assert.True(t, clierr.HasRemedy(ce.Hint), "the login command is a remedy command")
			assert.Equal(t, clierr.CapabilityUnsupportedDetails{Feature: "create kubeconfig", Server: tt.version}, ce.Details)
			status, ok := clierr.HTTPStatus(got)
			assert.True(t, ok, "the 403 stays readable as the cause")
			assert.Equal(t, http.StatusForbidden, status)
		})
	}
}

// TestRefuseUnsupportedTokenLookupMessageAndHint asserts the exact texts for
// a stored login: the hint is the one login command, and the message holds
// the same command so it reads whole when the hint is dropped.
func TestRefuseUnsupportedTokenLookupMessageAndHint(t *testing.T) {
	t.Parallel()
	var asks int
	clients := versionFactory{c: &fakeClient{}, version: "v2.14.5", login: "prod's", versionAsks: &asks}
	ce := refuseUnsupportedTokenLookup(t.Context(), clients, lookupForbidden(t)).(*clierr.Error)
	const command = `rancher auth login 'prod'\''s' --token-stdin`
	assert.Equal(t, "Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or an API token (pipe id:secret to "+command+")",
		clierr.ExpandRemedies(ce.Message, ""))
	assert.Equal(t, command, clierr.ExpandRemedies(ce.Hint, ""))
	assert.Contains(t, ce.Message, ce.Hint, "the message names the same command, marker included")
	assert.Equal(t, strings.Replace(command, "rancher", "rancher --config '/d'", 1), clierr.ExpandRemedies(ce.Hint, " --config '/d'"))
}

// TestRefuseUnsupportedTokenLookupTrimsTheVersion asserts that spaces around
// the reported version are left out of the message and the details.
func TestRefuseUnsupportedTokenLookupTrimsTheVersion(t *testing.T) {
	t.Parallel()
	var asks int
	clients := versionFactory{c: &fakeClient{}, version: " v2.14.5 ", login: "prod", versionAsks: &asks}
	ce := refuseUnsupportedTokenLookup(t.Context(), clients, lookupForbidden(t)).(*clierr.Error)
	assert.Contains(t, ce.Message, "Rancher v2.14.5 cannot create a kubeconfig")
	assert.Equal(t, clierr.CapabilityUnsupportedDetails{Feature: "create kubeconfig", Server: "v2.14.5"}, ce.Details)
}

// TestRefuseUnsupportedTokenLookupWithoutAStoredLogin asserts no hint, and a
// message that names RANCHER_CLI_TOKEN, when no stored login in use can take
// the login command.
func TestRefuseUnsupportedTokenLookupWithoutAStoredLogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		f    func(asks *int) engine.ClusterClientFactory
	}{
		{"no entry in use", func(asks *int) engine.ClusterClientFactory {
			return versionFactory{c: &fakeClient{}, version: "v2.14.5", versionAsks: asks}
		}},
		{"entry name unreadable", func(asks *int) engine.ClusterClientFactory {
			return versionFactory{c: &fakeClient{}, version: "v2.14.5", login: "prod", loginErr: errors.New("boom"), versionAsks: asks}
		}},
		{"plugin login entry", func(asks *int) engine.ClusterClientFactory {
			return versionFactory{c: &fakeClient{}, version: "v2.14.5", login: "exec-login-rancher.example.com", versionAsks: asks}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asks int
			ce := refuseUnsupportedTokenLookup(t.Context(), tc.f(&asks), lookupForbidden(t)).(*clierr.Error)
			assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, "Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or set RANCHER_CLI_TOKEN to an API token", ce.Message)
		})
	}
}

// TestRefuseUnsupportedTokenLookupWithAnUnprintableConfigDirectory asserts
// that when the invocation's config directory cannot be printed, the login
// command is dropped from the hint and the message, and the message keeps
// the version fact and the first part of the remedy.
func TestRefuseUnsupportedTokenLookupWithAnUnprintableConfigDirectory(t *testing.T) {
	t.Parallel()
	var asks int
	clients := versionFactory{c: &fakeClient{}, version: "v2.14.5", login: "prod", versionAsks: &asks}
	ce := refuseUnsupportedTokenLookup(t.Context(), clients, lookupForbidden(t)).(*clierr.Error)
	out := runtime.IO{RemedyDir: "/tmp/dir\x01"}.ExpandError(ce)
	assert.Empty(t, out.Hint)
	assert.Equal(t, "Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or an API token; "+runtime.UnprintableConfigDirNote, out.Message)
}

func TestRefuseUnsupportedTokenLookupVersionUnavailable(t *testing.T) {
	t.Parallel()
	orig := lookupForbidden(t)

	var asks int
	failing := versionFactory{c: &fakeClient{}, versionErr: errors.New("unreachable"), versionAsks: &asks}
	assert.Same(t, orig, refuseUnsupportedTokenLookup(t.Context(), failing, orig), "a failed version fetch leaves the RBAC error")
	assert.Equal(t, 1, asks)

	assert.Same(t, orig, refuseUnsupportedTokenLookup(t.Context(), stubFactory{c: &fakeClient{}}, orig),
		"a factory that cannot report a version leaves the RBAC error")
}

func TestCreateKubeconfigRefusesOnDryRunFailure(t *testing.T) {
	t.Parallel()
	var asks int
	client := &fakeClient{createErr: lookupForbidden(t)}
	cmd := NewCreateKubeconfig(versionFactory{c: client, version: "v2.14.5", login: "prod", versionAsks: &asks})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, 1, asks)
}

// TestCreateKubeconfigRefusesWhenOnlyTheRealCreateFails asserts the call
// after the real create: the dry-run passes, the create answers the
// request-token 403.
func TestCreateKubeconfigRefusesWhenOnlyTheRealCreateFails(t *testing.T) {
	t.Parallel()
	var asks int
	client := &fakeClient{realCreateErr: lookupForbidden(t)}
	cmd := NewCreateKubeconfig(versionFactory{c: client, version: "v2.14.5", login: "prod", versionAsks: &asks})

	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	assert.Zero(t, asks, "a dry-run that passes never asks for the version")

	_, err = exec(t.Context())
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, 1, asks)
}

// TestCreateKubeconfigAsksForTheVersionOnlyAfterAFailure asserts no version
// request on success, and none for a failure that is not the request-token
// 403.
func TestCreateKubeconfigAsksForTheVersionOnlyAfterAFailure(t *testing.T) {
	t.Parallel()
	var asks int
	ok := &fakeClient{name: "kubeconfig-abc", status: map[string]any{"value": "v"}}
	cmd := NewCreateKubeconfig(versionFactory{c: ok, version: "v2.14.5", versionAsks: &asks})
	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
	assert.Zero(t, asks)

	for _, failure := range []error{errors.New("boom"), storeForbidden(t, errors.New("permission denied")), clierr.New(clierr.CodeNotFound, "x")} {
		cmd := NewCreateKubeconfig(versionFactory{c: &fakeClient{createErr: failure}, version: "v2.14.5", versionAsks: &asks})
		_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
		require.Error(t, err)
	}
	assert.Zero(t, asks)
}
