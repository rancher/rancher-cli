package conformance

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const httpxPackage = modulePrefix + "internal/client/httpx"

// clientConsumers are the functions of internal/client/kube that turn a
// rest.Config into a client, and the httpx constructor for third-party
// origins. A client built from a rest.Config reaches Rancher through the
// wrapper that records the host that answered only when the config comes
// from kube.RESTConfig, so each use is listed with where its config comes
// from.
var clientConsumers = map[string]map[string]bool{
	kubePackage:  {"HTTPClient": true, "Dynamic": true, "Discovery": true, "Clientset": true, "TableClient": true},
	httpxPackage: {"NewExternal": true},
}

// clientConsumerAllowed maps "<file>:<function>" to the uses of a client
// consumer in production code outside internal/client/kube, with the count and
// where the client's config comes from: kube.RESTConfig, which records the
// host that answered, or a reason it must not. A function with a receiver is
// named by its bare name.
var clientConsumerAllowed = map[string]requestAllowance{
	"internal/client/provider/client.go:bootstrapLocalDynamic": {1, "the config comes from kube.RESTConfig (engine.go factory and the provider's local config)"},
	"internal/client/provider/client.go:NewClusterClient":      {5, "the config comes from kube.RESTConfig, passed in by the engine factory"},
	"internal/client/provider/owner.go:Owner":                  {1, "the config comes from kube.RESTConfig through localRESTConfig"},
	"internal/client/provider/preflight.go:Preflight":          {1, "the config comes from kube.RESTConfig through localRESTConfig"},
	"internal/token/token.go:directClientFor":                  {1, "the direct endpoint check of a cached credential: its config comes from the exec entry, not from kube.RESTConfig, and a cache hit must record no host"},
	"internal/commands/auth/login.go:idpClient":                {1, "httpx.NewExternal: the identity provider's origin, never Rancher"},
	"internal/token/pluginlogin.go:pluginInput":                {1, "httpx.NewExternal: the identity provider's origin, never Rancher"},
	"internal/token/token.go:runWithParams":                    {1, "httpx.NewExternal: the identity provider's origin, never Rancher"},
}

// fetcherRegistrationAllowed lists where production code may register the
// install id fetcher: the root client's builder, the two auth commands after
// their check, and the exec plugin's cache miss; no other command ever
// fetches the install id.
var fetcherRegistrationAllowed = map[string]requestAllowance{
	"internal/cli/engine.go:Root":                          {1, "the root client registers the fetcher with its own client and token when a command builds it"},
	"internal/commands/auth/login.go:authenticateAndStore": {1, "auth login registers the token it stored, after the verify call accepted a pasted token or the provider issued the token"},
	"internal/commands/auth/authcheck.go:Execute":          {1, "auth status registers the token it checked, after the connectivity check passed"},
	"internal/token/token.go:runWithParams":                {1, "the exec plugin's cache miss registers the login token the miss holds, after the miss succeeded"},
}

// signInRequestSendAllowed lists where production code may call the function
// a sign-in sends its facts requests through: the plugin's sign-in, between
// the creation of the cluster token and the deletion of the login token.
var signInRequestSendAllowed = map[string]requestAllowance{
	"internal/token/acquire.go:signInForClusterToken": {1, "the plugin's sign-in sends the server's facts requests with the login token before it deletes it"},
}

// signInRequestRegisterAllowed lists where production code may register that
// function: the command line runner, once the line is parsed and not refused.
var signInRequestRegisterAllowed = map[string]requestAllowance{
	"internal/cli/run.go:runArgv": {1, "the runner registers the function with the parse of the line, after the line is accepted"},
}

// usesOf returns one match per use of a function for which kind returns a
// non-empty name, in pkg's production files: a call, a function value or a
// method value, found through the identifier type information resolves.
func usesOf(pkg *packages.Package, wd string, kind func(*types.Func) string) []requestMatch {
	var out []requestMatch
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn := "(package level)"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				used, ok := pkg.TypesInfo.Uses[id].(*types.Func)
				if !ok || used.Pkg() == nil {
					return true
				}
				if what := kind(used); what != "" {
					out = append(out, requestMatch{rel + ":" + fn, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(id.Pos()).Line, what)})
				}
				return true
			})
		}
	}
	return out
}

// clientConsumerKind names a use of a client consumer, or "".
func clientConsumerKind(fn *types.Func) string {
	if receiverName(fn) == "" && clientConsumers[fn.Pkg().Path()][fn.Name()] {
		return fn.Pkg().Name() + "." + fn.Name()
	}
	return ""
}

// fetcherRegistrationKind names a use of the install id fetcher
// registration, or "".
func fetcherRegistrationKind(fn *types.Func) string {
	if fn.Pkg().Path() == httpxPackage && receiverName(fn) == "" && fn.Name() == "RegisterInstallIDFetcher" {
		return "httpx.RegisterInstallIDFetcher"
	}
	return ""
}

// signInRequestSendKind names a call of the function a sign-in sends its
// facts requests through, or "".
func signInRequestSendKind(fn *types.Func) string {
	if fn.Pkg().Path() == httpxPackage && receiverName(fn) == "" && fn.Name() == "SendSignInRequest" {
		return "httpx.SendSignInRequest"
	}
	return ""
}

// signInRequestRegisterKind names a registration of that function, or "".
func signInRequestRegisterKind(fn *types.Func) string {
	if fn.Pkg().Path() == httpxPackage && receiverName(fn) == "" && fn.Name() == "RegisterSignInRequest" {
		return "httpx.RegisterSignInRequest"
	}
	return ""
}

func loadClientConsumerFixture(t *testing.T) *packages.Package {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./test/conformance/testdata/clientconsumers/uses")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	for _, e := range pkgs[0].Errors {
		require.Fail(t, "type-check", e.Error())
	}
	return pkgs[0]
}

// TestEveryClientConsumerIsListedWithWhereItsConfigComes asserts every use
// (a call or a function value) of kube.HTTPClient, Dynamic, Discovery,
// Clientset and TableClient and of httpx.NewExternal in production code
// outside internal/client/kube is allowlisted with its count and the
// reason. A client built from a kube.RESTConfig records the host that
// answered; one built from any other config must say why it does not. A new
// use, a changed count and a stale row fail.
func TestEveryClientConsumerIsListedWithWhereItsConfigComes(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var matches []requestMatch
	for _, pkg := range pkgs {
		if pkg.PkgPath == kubePackage || strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		matches = append(matches, usesOf(pkg, wd, clientConsumerKind)...)
	}
	for _, problem := range checkRequestMatches(matches, clientConsumerAllowed) {
		t.Error(problem)
	}
	for key, a := range clientConsumerAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestTheInstallIDFetcherIsRegisteredOnlyByItsListedCallers asserts
// production code registers the install id fetcher only in the root
// client's builder, in the two auth commands after their check, and in the
// exec plugin's cache miss after the miss succeeded.
func TestTheInstallIDFetcherIsRegisteredOnlyByItsListedCallers(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var matches []requestMatch
	for _, pkg := range pkgs {
		if pkg.PkgPath == httpxPackage || strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		matches = append(matches, usesOf(pkg, wd, fetcherRegistrationKind)...)
	}
	for _, problem := range checkRequestMatches(matches, fetcherRegistrationAllowed) {
		t.Error(problem)
	}
	for key, a := range fetcherRegistrationAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestTheSignInRequestFunctionIsSentAndRegisteredOnlyWhereListed asserts
// production code calls the function a sign-in sends its facts requests
// through only in the plugin's sign-in, and registers it only in the command
// line runner.
func TestTheSignInRequestFunctionIsSentAndRegisteredOnlyWhereListed(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var sends, registers []requestMatch
	for _, pkg := range pkgs {
		if pkg.PkgPath == httpxPackage || strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		sends = append(sends, usesOf(pkg, wd, signInRequestSendKind)...)
		registers = append(registers, usesOf(pkg, wd, signInRequestRegisterKind)...)
	}
	for _, problem := range checkRequestMatches(sends, signInRequestSendAllowed) {
		t.Error(problem)
	}
	for _, problem := range checkRequestMatches(registers, signInRequestRegisterAllowed) {
		t.Error(problem)
	}
	for key, a := range signInRequestSendAllowed {
		assert.NotEmpty(t, a.why, key)
	}
	for key, a := range signInRequestRegisterAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestClientConsumerScanFixtures asserts the scans report each kind of use
// in a fixture package, a call and a function value alike, and leave a
// builder that is no consumer alone.
func TestClientConsumerScanFixtures(t *testing.T) {
	t.Parallel()
	pkg := loadClientConsumerFixture(t)
	wd, err := os.Getwd()
	require.NoError(t, err)
	kinds := func(ms []requestMatch) []string {
		var out []string
		for _, m := range ms {
			_, rest, _ := strings.Cut(m.text, ": ")
			out = append(out, rest)
		}
		return out
	}
	assert.ElementsMatch(t, []string{
		"kube.HTTPClient", "kube.Dynamic", "kube.Discovery", "kube.Clientset", "kube.TableClient", "httpx.NewExternal", "kube.Dynamic",
	}, kinds(usesOf(pkg, wd, clientConsumerKind)))
	assert.Equal(t, []string{"httpx.RegisterInstallIDFetcher"}, kinds(usesOf(pkg, wd, fetcherRegistrationKind)))
	assert.Equal(t, []string{"httpx.SendSignInRequest"}, kinds(usesOf(pkg, wd, signInRequestSendKind)))
	assert.Equal(t, []string{"httpx.RegisterSignInRequest"}, kinds(usesOf(pkg, wd, signInRequestRegisterKind)))
}

// TestClientConsumerAllowlistRules asserts a use outside the list is reported
// (a registration in another function, a new consumer), a different count
// fails and a stale row fails.
func TestClientConsumerAllowlistRules(t *testing.T) {
	t.Parallel()
	rootRow := "internal/cli/engine.go:Root"
	allowed := map[string]requestAllowance{rootRow: fetcherRegistrationAllowed[rootRow]}
	one := requestMatch{rootRow, "internal/cli/engine.go:1: httpx.RegisterInstallIDFetcher"}
	assert.Empty(t, checkRequestMatches([]requestMatch{one}, allowed))
	elsewhere := requestMatch{"internal/cli/engine.go:Cluster", "internal/cli/engine.go:2: httpx.RegisterInstallIDFetcher"}
	assert.Len(t, checkRequestMatches([]requestMatch{one, elsewhere}, allowed), 1)
	otherAuthCommand := requestMatch{"internal/commands/auth/logout.go:Execute", "internal/commands/auth/logout.go:3: httpx.RegisterInstallIDFetcher"}
	problems := checkRequestMatches([]requestMatch{otherAuthCommand}, fetcherRegistrationAllowed)
	assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "internal/commands/auth/logout.go:3") }),
		"a registration in a function with no row is reported: %v", problems)
	for key := range fetcherRegistrationAllowed {
		assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, key) }), "the row %s that nothing used is reported", key)
	}
	assert.Len(t, checkRequestMatches([]requestMatch{one, {one.key, "internal/cli/engine.go:3: httpx.RegisterInstallIDFetcher"}}, allowed), 1)
	assert.Len(t, checkRequestMatches(nil, allowed), 1, "a stale row")
	sendRow := "internal/token/acquire.go:signInForClusterToken"
	send := requestMatch{sendRow, "internal/token/acquire.go:1: httpx.SendSignInRequest"}
	assert.Empty(t, checkRequestMatches([]requestMatch{send}, signInRequestSendAllowed))
	sendElsewhere := requestMatch{"internal/token/token.go:runWithParams", "internal/token/token.go:2: httpx.SendSignInRequest"}
	assert.Len(t, checkRequestMatches([]requestMatch{send, sendElsewhere}, signInRequestSendAllowed), 1, "a send in a function with no row")
	assert.Len(t, checkRequestMatches(nil, signInRequestSendAllowed), 1, "a send row that nothing used")
	registerRow := "internal/cli/run.go:runArgv"
	register := requestMatch{registerRow, "internal/cli/run.go:1: httpx.RegisterSignInRequest"}
	assert.Empty(t, checkRequestMatches([]requestMatch{register}, signInRequestRegisterAllowed))
	registerElsewhere := requestMatch{"internal/cli/adapter.go:resolveReadOnly", "internal/cli/adapter.go:2: httpx.RegisterSignInRequest"}
	assert.Len(t, checkRequestMatches([]requestMatch{register, registerElsewhere}, signInRequestRegisterAllowed), 1, "a registration in a function with no row")
	assert.Len(t, checkRequestMatches(nil, signInRequestRegisterAllowed), 1, "a registration row that nothing used")
	newUse := requestMatch{"internal/client/provider/new.go:Build", "internal/client/provider/new.go:9: kube.Dynamic"}
	assert.Len(t, checkRequestMatches([]requestMatch{newUse}, map[string]requestAllowance{}), 1)
}
