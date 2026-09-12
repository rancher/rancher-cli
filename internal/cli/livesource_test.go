package cli

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/rbac/complete"
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
	assert.Equal(t, 2, n)
	n, ok = allowanceLimit(completion.AllowanceSteveClusterPages)
	require.True(t, ok)
	assert.Equal(t, access.SteveMaxPages, n)
	for _, a := range []completion.Allowance{0, -1, completion.AllowanceSteveClusterPages + 1, 100} {
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
// commands that exist and name a Completable kind; and every function in
// a complete package.
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
// package, every other entry's probe implements nothing, and BuildRoot
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
	assert.ElementsMatch(t, []string{"grant", "revoke"}, owners)
	_, err := BuildRoot("v-test")
	require.NoError(t, err)
}
