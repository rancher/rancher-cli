package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const credhelperPackage = modulePrefix + "credhelper"

// envURLValue is the value of config.EnvURL; TestEnvURLValueMatchesTheScan
// asserts it.
const envURLValue = "RANCHER_CLI_URL"

// isEnvURLConstant reports whether expr names the constant config.EnvURL.
func isEnvURLConstant(info *types.Info, expr ast.Expr) bool {
	var id *ast.Ident
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return false
	}
	c, ok := info.Uses[id].(*types.Const)
	return ok && c.Pkg() != nil && c.Pkg().Path() == configPackage && c.Name() == "EnvURL"
}

// serverURLRouteKind names what a use of the server URL machinery is, or ""
// when node is none: a call of config.(*Config).Effective or
// EffectiveConnection, of a kube.RESTConfig function, of
// credhelper.CanonicalServerURL or CheckServerURL (and config.CheckServerURL),
// any use of clierr.RecoverySequence (a call or a function value; every
// sequence for a record goes through config.AdviseRecord), a read of the
// constant config.EnvURL, or a config.Effective composite literal.
func serverURLRouteKind(info *types.Info, n ast.Node) string {
	switch n := n.(type) {
	case *ast.CallExpr:
		fn := methodOf(info, n)
		if fn == nil || fn.Pkg() == nil {
			return ""
		}
		switch pkg := fn.Pkg().Path(); {
		case pkg == "os" && (fn.Name() == "Getenv" || fn.Name() == "LookupEnv") && len(n.Args) == 1 && !isEnvURLConstant(info, n.Args[0]):
			// A read of the variable by its name, as a literal or a local
			// constant; a read through config.EnvURL is reported once, as
			// that constant.
			if v := info.Types[n.Args[0]].Value; v != nil && v.Kind() == constant.String && constant.StringVal(v) == envURLValue {
				return "os." + fn.Name() + " of " + envURLValue
			}
		case pkg == configPackage && receiverName(fn) == "Config" && (fn.Name() == "Effective" || fn.Name() == "EffectiveConnection"):
			return "config.Config." + fn.Name()
		case pkg == configPackage && receiverName(fn) == "" && fn.Name() == "CheckServerURL":
			return "config.CheckServerURL"
		case pkg == kubePackage && receiverName(fn) == "" && strings.HasPrefix(fn.Name(), "RESTConfig"):
			return "kube." + fn.Name()
		case pkg == credhelperPackage && (fn.Name() == "CanonicalServerURL" || fn.Name() == "CheckServerURL"):
			return "credhelper." + fn.Name()
		}
	case *ast.Ident:
		if f, ok := info.Uses[n].(*types.Func); ok && f.Pkg() != nil && f.Pkg().Path() == clierrPackage && receiverName(f) == "" && f.Name() == "RecoverySequence" {
			// Every use of the function, a call or a function value: a call is
			// one use, through its callee identifier.
			return "clierr.RecoverySequence"
		}
		if c, ok := info.Uses[n].(*types.Const); ok && c.Pkg() != nil && c.Pkg().Path() == configPackage && c.Name() == "EnvURL" {
			return "config.EnvURL"
		}
	case *ast.CompositeLit:
		if t := info.TypeOf(n); t != nil && isNamedType(t, configPackage, "Effective") {
			return "config.Effective literal"
		}
	}
	return ""
}

// serverURLRoutes returns the uses of the server URL machinery in pkg's
// production files, one match per use.
func serverURLRoutes(pkg *packages.Package, wd string) []requestMatch {
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
				if kind := serverURLRouteKind(pkg.TypesInfo, n); kind != "" {
					out = append(out, requestMatch{rel + ":" + fn, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(n.Pos()).Line, kind)})
				}
				return true
			})
		}
	}
	return out
}

// serverURLRouteAllowed maps "<file>:<function>" to the uses of the server
// URL machinery in production code, with the count and why each is covered:
// by a row of the secret input table (internal/cli/secretinput_test.go and
// secreturl_test.go) or by the reason given. A new use fails the scan until a
// row or an entry covers it.
var serverURLRouteAllowed = map[string]requestAllowance{
	"internal/cli/completion_token.go:completionServerURL":         {1, "completion refuses a URL with userinfo or a raw @ after the host and treats it as missing; TestCompletionTraceAndRefusalForAServerURLWithUserinfo"},
	"internal/cli/completion_token.go:resolveCompletionConnection": {1, "completion reads the variable; the same test, and completionServerURL checks it before any use"},
	"internal/cli/engine.go:Cluster":                               {1, "the cluster client, after resolve checked the URL; rows get cluster"},
	"internal/cli/engine.go:ImpersonatedCluster":                   {1, "the impersonating cluster client, after resolve checked the URL; rows get cluster"},
	"internal/cli/engine.go:resolve":                               {1, "the one place ordinary commands resolve a connection; rows get cluster, get node, auth status"},
	"internal/cli/record.go:(package level)":                       {1, "telemetry reads the variable the exec plugin's flag stands for; canonicalized, only a host is recorded"},
	"internal/cli/record.go:effectiveConnectionURL":                {1, "telemetry; empty for a refused URL"},
	"internal/cli/record.go:recordedServer":                        {1, "telemetry; canonicalized, only a host is recorded"},
	"internal/client/kube/restconfig.go:RESTConfigImpersonating":   {1, "the impersonating variant of RESTConfig, built from an Effective that was checked"},
	"internal/client/kube/restconfig.go:RequireLogin":              {1, "the not-logged-in wording reads whether the variable is set"},
	"internal/client/provider/client.go:bootstrapLocalDynamic":     {1, "the provider's own client, from an Effective that was checked; row auth status"},
	"internal/client/provider/preflight.go:localRESTConfig":        {1, "the preflight client, from an Effective that was checked; row auth status"},
	"internal/commands/auth/authcheck.go:Execute":                  {1, "auth status; row auth status"},
	"internal/commands/auth/login.go:authenticateAndStore":         {1, "the login's verify connection, built from the target's URL, which entryLoginTarget or CanonicalizeServerURL checked; rows auth login"},
	"internal/commands/auth/login.go:nameCollisionError":           {1, "the check of the stored URL of the entry a name collides with, so the refusal names the entry and never shows the URL; rows auth login x with another URL"},
	"internal/commands/auth/logintarget.go:entryLoginTarget":       {1, "the check of a stored entry's URL; rows auth login x"},
	"internal/config/cachebinding.go:usableEntryNames":             {1, "the check of the entries a record is bound to, which the lookups leave out when unusable; TestAnEntryWithAnAtSignAfterTheHostDoesNotStopTheCacheOfAUsableEntry"},
	"internal/token/revocationplan.go:retry":                       {1, "the entry's URL checked so a record of an entry that cannot be used is not retried at its next login"},
	"internal/token/revocationplan.go:manualWarning":               {1, "the entry's URL checked so the warning names the delete route when no login can revoke the token"},
	"internal/config/cachebinding.go:recordNamesUnusableEntry":     {1, "the check of the entries a live record names, so a record of an unusable entry is kept; TestARecordBoundToAnEntryThatCannotBeUsedIsServedToNobodyAndKept"},
	"internal/config/cachebinding.go:servesRecord":                 {1, "the check of the identified entry's URL, so a cache record bound to an unusable entry is a miss; TestThePluginSendsNothingForAnEntryThatCannotBeUsed"},
	"internal/token/entry.go:unusableEntryError":                   {1, "the check of the URL of the entry a call names, so its refusal comes before any command built from it; TestEntryOnOtherServerErrorNamesTheRefusalOfAnUnusableURL"},
	"internal/token/entry.go:usableEntry":                          {1, "the check of an entry's URL before its stored login is read for a call; TestThePluginSendsNothingForAnEntryThatCannotBeUsed"},
	"internal/config/serverurl.go:vouchesFor":                      {1, "whether an entry URL is usable at a server, so a record the entry owns keeps that server; TestAMaskedCacheRecordOfAUsableEntryKeepsItsRevokeDecisionAndStoredServer"},
	"internal/commands/cmdtest/env.go:UnsetResolutionEnv":          {1, "a test helper that clears the variable"},
	"internal/commands/register/register.go:CanonicalizeServerURL": {1, "registration's check of a typed or stored URL; rows config add-server, config update-server, auth login --url"},
	"internal/commands/register/register.go:TLSPreflight":          {1, "the registration preflight's connection, from the canonical URL registration checked"},
	"internal/commands/register/register.go:commitCreateWith":      {1, "the check of the stored URL of the entry a name conflicts with, so the refusal names the entry and never shows the URL; row config add-server with another URL"},
	"internal/config/config.go:ErrNotLoggedInFor":                  {2, "leaves an unusable URL out of the login command and the hint; TestErrNotLoggedInForLeavesAnUnusableURLOut"},
	"internal/config/credstore.go:CanonicalServerIdentity":         {1, "the identity of a URL; the fallback holds no user information; TestCanonicalServerIdentity"},
	"internal/config/credstore.go:DeleteLoginToken":                {1, "the exec helper's delete request; its error holds no part of the URL"},
	"internal/config/credstore.go:canonicalServerHost":             {1, "the host of a URL for legacy records; never shown"},
	"internal/config/credstore_exec.go:wireRequest":                {1, "the exec helper's request; its error holds no part of the URL"},
	"internal/config/resolve.go:Effective":                         {2, "the Effective literals of the store-error and success returns"},
	"internal/config/resolve.go:EnvCACertFor":                      {1, "reads whether the variable names the server the CA applies to"},
	"internal/config/resolve.go:IsSupportedSchemeURL":              {1, "a scheme check before a credential read; its error is discarded"},
	"internal/config/resolve.go:SameServerURL":                     {2, "a comparison; its errors are discarded"},
	"internal/config/resolve.go:connection":                        {3, "resolves the connection and checks the URL that wins; rows get cluster"},
	"internal/config/resolve.go:tlsRemedyFor":                      {1, "the certificate remedy's wording"},
	"internal/config/serverurl.go:CheckServerURL":                  {1, "the wrapper of credhelper.CheckServerURL"},
	"internal/config/serverurl.go:AdviseRecord":                    {2, "the one place that decides what to say about the server of a token record: a recovery sequence built from the record's identity, never from its text as stored, after the check of the entry's own URL; a command that names a record's server calls AdviseRecord and cannot call clierr.RecoverySequence itself"},
	"internal/config/serverurl.go:identityProblem":                 {2, "the check of an identity before a recovery sequence is built from it; rows config status, config delete-server, auth logout"},
	"internal/config/serverurl.go:(package level)":                 {1, "the reason of a URL that holds an @ after the host, which identityProblem gives for the identity stored for one; TestRecordServerIdentity"},
	"internal/config/serverurl.go:misreadURLProblem":               {1, "the check of the URL an identity was read from, so a recovery sequence is never built for the host read from a URL with an @ after the host; rows config update-server, config delete-server, auth logout"},
	"internal/engine/kubeconfig/command.go:Execute":                {1, "kubeconfig; Effective refuses an unusable URL; TestKubeconfigRefusesAMalformedServerURL"},
	"internal/engine/kubeconfig/records.go:installHint":            {1, "the install hint's wording"},
	"internal/telemetrysend/origin.go:Origin":                      {1, "the check of the telemetryupload value, which refuses a user name or password and a raw @ after the host; its errors hold no part of the value; TestUploadURLRules"},
	"internal/token/revocation.go:newClient":                       {1, "a revoke never dials an unusable stored URL; rows config delete-server, auth logout"},
	"internal/token/token.go:computeCacheProbeKey":                 {1, "a URL with userinfo or a raw @ after the host gives no probe key; rows auth get-token and token, a cache hit"},
	"internal/token/token.go:checkServerOrigin":                    {1, "the exec plugin's and the shim's check of the URL they connect to; rows auth get-token and token"},
	"internal/token/token.go:connectionTarget":                     {2, "reads the connection's URL and the variable that names its source; rows auth get-token and token"},
	"internal/token/token.go:runWithParams":                        {1, "canonicalizes the checked server for the cache key; its error holds no part of the URL"},
	"internal/token/userid.go:RecordedUserIDs":                     {1, "completion of --user-id; returns nothing for a refused URL"},
}

// TestEveryServerURLRouteHasARowOrAnAllowlistEntry asserts every production
// caller of config.(*Config).Effective and EffectiveConnection, of
// kube.RESTConfig*, of credhelper.CanonicalServerURL and CheckServerURL, every
// use of clierr.RecoverySequence (a call or a function value), every read of
// config.EnvURL and every config.Effective literal is allowlisted with its
// count and a reason, so a route that carries a server URL into a command
// cannot appear without a row of the secret input table or a stated reason.
func TestEveryServerURLRouteHasARowOrAnAllowlistEntry(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var matches []requestMatch
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		matches = append(matches, serverURLRoutes(pkg, wd)...)
	}
	for _, problem := range checkRequestMatches(matches, serverURLRouteAllowed) {
		t.Error(problem)
	}
	for key, a := range serverURLRouteAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestServerURLRouteScanFixtures asserts the scan reports each kind of use
// in a fixture package.
func TestServerURLRouteScanFixtures(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./test/conformance/testdata/serverurl/routes")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	for _, e := range pkgs[0].Errors {
		require.Fail(t, "type-check", e.Error())
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	var kinds []string
	for _, m := range serverURLRoutes(pkgs[0], wd) {
		_, kind, _ := strings.Cut(strings.TrimPrefix(m.text, m.key[:strings.Index(m.key, ":")]), ": ")
		kinds = append(kinds, kind)
	}
	assert.ElementsMatch(t, []string{
		"config.Config.Effective", "config.Config.EffectiveConnection", "kube.RESTConfig", "credhelper.CanonicalServerURL",
		"credhelper.CheckServerURL", "config.CheckServerURL", "config.EnvURL", "config.Effective literal",
		"os.Getenv of RANCHER_CLI_URL", "os.LookupEnv of RANCHER_CLI_URL", "clierr.RecoverySequence", "clierr.RecoverySequence",
	}, kinds)
}

// TestEnvURLValueMatchesTheScan asserts the name the route scan looks for in
// a read of the environment is the value of config.EnvURL.
func TestEnvURLValueMatchesTheScan(t *testing.T) {
	t.Parallel()
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes}, "./internal/config")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	c, ok := pkgs[0].Types.Scope().Lookup("EnvURL").(*types.Const)
	require.True(t, ok)
	assert.Equal(t, envURLValue, constant.StringVal(c.Val()))
}
