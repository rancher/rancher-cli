package cli

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"

	accesscomplete "github.com/rancher/rancher-cli/internal/engine/access/complete"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/engine/rbac/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllowanceLimit asserts the closed class maps to its counts and that
// the zero value and any value past the class are not granted.
func TestAllowanceLimit(t *testing.T) {
	t.Parallel()
	n, ok := allowanceLimit(completion.AllowanceOne)
	require.True(t, ok)
	assert.Equal(t, 1, n)
	n, ok = allowanceLimit(completion.AllowanceDiscovery)
	require.True(t, ok)
	assert.Equal(t, 2+2*(1+completion.DiscoveryGroupLimit), n, "two roots, and per root a plain document plus one per group")
	n, ok = allowanceLimit(completion.AllowanceSteveClusterPages)
	require.True(t, ok)
	assert.Equal(t, steve.MaxPages, n)
	n, ok = allowanceLimit(completion.AllowanceSteveProjectPages)
	require.True(t, ok)
	assert.Equal(t, steve.MaxPages, n)
	for _, a := range []completion.Allowance{0, -1, completion.AllowanceSteveProjectPages + 1, 100} {
		_, ok := allowanceLimit(a)
		assert.False(t, ok, "allowance %d", a)
	}
}

// localClaim and localComplete are declared in this package, outside the
// complete set, so a source built from them must be rejected.
func localClaim(completion.Request) (completion.Requirements, bool) {
	return completion.Requirements{}, true
}
func localComplete(context.Context, *completion.Connection, completion.Request) completion.Result {
	return completion.Result{}
}

type localCompleter struct{}

func (localCompleter) claim(completion.Request) (completion.Requirements, bool) {
	return completion.Requirements{}, true
}

// TestDeclaringPackage asserts the declaring package is read off a
// top-level function, a closure and a method value, and that a nil
// function has none.
func TestDeclaringPackage(t *testing.T) {
	t.Parallel()
	src := complete.RoleSource()
	assert.Equal(t, modulePath+"/internal/engine/rbac/complete", declaringPackage(src.Claim))
	assert.Equal(t, modulePath+"/internal/engine/rbac/complete", declaringPackage(src.Complete))
	assert.Equal(t, modulePath+"/internal/cli", declaringPackage(localClaim))
	closure := func(completion.Request) (completion.Requirements, bool) { return completion.Requirements{}, false }
	assert.Equal(t, modulePath+"/internal/cli", declaringPackage(closure))
	assert.Equal(t, modulePath+"/internal/cli", declaringPackage(localCompleter{}.claim))
	var nilFn func(completion.Request) (completion.Requirements, bool)
	assert.Empty(t, declaringPackage(nilFn))
	assert.Empty(t, declaringPackage(nil))
}

// TestCheckSourceDeclared asserts a source is accepted only when both
// functions are declared in a complete package.
func TestCheckSourceDeclared(t *testing.T) {
	t.Parallel()
	good := complete.RoleSource()
	require.NoError(t, checkSourceDeclared("command grant", good))
	require.NoError(t, checkSourceDeclared("the generic kinds source", kindscomplete.KindSource("")))

	err := checkSourceDeclared("command x", completion.Source{Claim: localClaim, Complete: good.Complete})
	require.ErrorContains(t, err, "Claim is declared in "+modulePath+"/internal/cli, not in a complete package")

	err = checkSourceDeclared("command x", completion.Source{Claim: good.Claim, Complete: localComplete})
	require.ErrorContains(t, err, "Complete is declared in "+modulePath+"/internal/cli")

	err = checkSourceDeclared("command x", completion.Source{Claim: good.Claim})
	require.ErrorContains(t, err, "has no Complete function")

	err = checkSourceDeclared("--cluster", completion.Source{})
	require.ErrorContains(t, err, "live completion source for --cluster has no")
}

// TestValidateLiveWiring asserts each table's rule: shared-flag keys only,
// declared by some leaf; kind keys that name a Completable kind; bespoke
// commands that exist and name a Completable kind; every function in a
// complete package; and the generic kinds source checked on its own,
// whatever the tables carry.
func TestValidateLiveWiring(t *testing.T) {
	t.Parallel()
	good := complete.RoleSource()
	leaves := map[string]runtime.Descriptor{
		"grant":       {Name: "grant", Flags: []runtime.ArgSpec{{Name: "role"}, {Name: "cluster"}, {Name: "project"}}},
		"get pod":     {Name: "get pod", Flags: []runtime.ArgSpec{{Name: "cluster"}, {Name: "namespace"}}},
		"logs":        {Name: "logs", Flags: []runtime.ArgSpec{{Name: "cluster"}, {Name: "pod"}}},
		"inspect pod": {Name: "inspect pod"},
	}
	tests := []struct {
		name       string
		completers map[string]completion.Source
		flags      map[string]completion.Source
		kindTable  map[string]completion.Source
		positional map[string]string
		wantErr    string
	}{
		{name: "empty tables pass"},
		{name: "declared completer passes", completers: map[string]completion.Source{"grant": good}},
		{name: "completer outside the set", completers: map[string]completion.Source{"grant": {Claim: localClaim, Complete: localComplete}},
			wantErr: "command grant: Claim is declared in " + modulePath + "/internal/cli"},
		{name: "shared flag passes", flags: map[string]completion.Source{"cluster": good, "namespace": good, "project": good}},
		{name: "flag outside the shared set", flags: map[string]completion.Source{"role": good}, wantErr: "--role is not a shared completion flag"},
		{name: "shared flag no leaf declares", flags: map[string]completion.Source{"node": good}, wantErr: "no command declares --node"},
		{name: "flag source outside the set", flags: map[string]completion.Source{"cluster": {Claim: localClaim, Complete: localComplete}},
			wantErr: "--cluster: Claim is declared in"},
		{name: "kind key passes", kindTable: map[string]completion.Source{"user": good, "managementcluster": good, "provisioningcluster": good}},
		{name: "kind name is not a key", kindTable: map[string]completion.Source{"cluster": good}, wantErr: `"cluster" is not a kind key`},
		{name: "kind not completable", kindTable: map[string]completion.Source{"secret": good}, wantErr: `kind "secret" is not Completable`},
		{name: "kind source outside the set", kindTable: map[string]completion.Source{"user": {Claim: localClaim, Complete: localComplete}},
			wantErr: "kind user: Claim is declared in"},
		{name: "positional passes", positional: map[string]string{"inspect pod": "provisioningcluster", "logs": "user"}},
		{name: "positional unknown command", positional: map[string]string{"inspect cluster": "managementcluster"}, wantErr: `"inspect cluster" is not an adapted command`},
		{name: "positional unknown kind", positional: map[string]string{"logs": "cluster"}, wantErr: `"logs" names "cluster", which is not a kind key`},
		{name: "positional non-completable kind", positional: map[string]string{"logs": "secret"}, wantErr: `names kind "secret", which is not Completable`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateLiveWiring(leaves, tt.completers, tt.flags, tt.kindTable, tt.positional)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestWiredCompletersAreDeclaredInCompletePackages asserts the real entry
// table: grant and revoke own live completion through the rbac complete
// package, access projects and the three access questions through the
// access one, create cluster and the three cluster-family verbs that
// take a pool through the provisioning one, the two oidc-client secret
// verbs through the oidcclient one, logs through the observe one, inspect
// project's positional through the projects one, inspect cluster's
// through the access one, the three sign-in commands through the token
// one, every other entry's probe implements nothing, and BuildRoot
// accepts the wiring.
func TestWiredCompletersAreDeclaredInCompletePackages(t *testing.T) {
	t.Parallel()
	var owners []string
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		lc, ok := probe.(completion.Completer)
		if !ok {
			continue
		}
		name := probe.Describe().Name
		owners = append(owners, name)
		require.NoError(t, checkSourceDeclared("command "+name, lc.LiveSource()))
	}
	assert.ElementsMatch(t, []string{
		"access projects", "grant", "revoke",
		"create cluster", "cluster scale", "cluster set-autoscaling", "cluster clear-autoscaling",
		"access can", "access who-can", "access explain",
		"oidc-client rotate-secret", "oidc-client remove-secret",
		"logs", "inspect project", "inspect cluster",
		"auth login", "auth get-token", "token",
	}, owners)
	_, err := BuildRoot("v-test")
	require.NoError(t, err)
}

// TestPositionalKindsMatchTheWiringTables is a golden of the wiring on purpose:
// the bespoke first-positional table names exactly the ten commands that get
// a kind (inspect user, the five cluster-family verbs, the three oidc-client
// secret verbs and create password-change-request; inspect project and
// inspect cluster complete through their own sources), and the
// flag-source and kind-source tables carry exactly the keys listed in the
// assertions. An intended change to the wiring edits this test in the same
// commit.
func TestPositionalKindsMatchTheWiringTables(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"inspect user":                   "user",
		"cluster scale":                  "provisioningcluster",
		"cluster set-autoscaling":        "provisioningcluster",
		"cluster clear-autoscaling":      "provisioningcluster",
		"cluster pause-autoscaling":      "provisioningcluster",
		"cluster resume-autoscaling":     "provisioningcluster",
		"oidc-client add-secret":         "oidcclient",
		"oidc-client rotate-secret":      "oidcclient",
		"oidc-client remove-secret":      "oidcclient",
		"create password-change-request": "user",
	}
	assert.Equal(t, want, positionalKinds)
	assert.ElementsMatch(t, []string{"cluster", "project", "namespace", "node", "as"}, slices.Collect(maps.Keys(flagSources)))
	assert.ElementsMatch(t, []string{"managementcluster", "project", "user"}, slices.Collect(maps.Keys(kindSources)))
}

// TestClusterSourcesAreTheCallerScopedOnes asserts which cluster source
// serves each site: a --cluster value and inspect cluster's positional
// complete with the clusters the caller can read (the set the commands
// resolve against), and the generated verbs' managementcluster positional
// with the management API's list, the set get, patch and delete act on.
func TestClusterSourcesAreTheCallerScopedOnes(t *testing.T) {
	t.Parallel()
	same := func(a, b completion.Source) bool {
		return reflect.ValueOf(a.Claim).Pointer() == reflect.ValueOf(b.Claim).Pointer() &&
			reflect.ValueOf(a.Complete).Pointer() == reflect.ValueOf(b.Complete).Pointer()
	}
	assert.True(t, same(flagSources["cluster"], accesscomplete.SteveClusterSource()), "--cluster")
	assert.True(t, same(kindSources["managementcluster"], accesscomplete.ClusterSource()), "managementcluster")
	var inspect completion.Completer
	for _, e := range buildEntries() {
		if probe := e.make(runDeps{}); probe.Describe().Name == "inspect cluster" {
			inspect, _ = probe.(completion.Completer)
		}
	}
	require.NotNil(t, inspect, "inspect cluster has its own source")
	assert.True(t, same(inspect.LiveSource(), accesscomplete.SteveClusterSource()), "inspect cluster")
	assert.False(t, same(accesscomplete.SteveClusterSource(), accesscomplete.ClusterSource()))
}
