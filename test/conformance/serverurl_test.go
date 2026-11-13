package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	configPackage = modulePrefix + "internal/config"
	clierrPackage = modulePrefix + "internal/clierr"
	kubePackage   = modulePrefix + "internal/client/kube"
)

// urlFields are the fields that hold a stored, effective or prior server URL,
// each as the package and type that declare it.
var urlFields = []struct{ pkg, typ, field string }{
	{configPackage, "Server", "URL"},
	{configPackage, "Effective", "URL"},
	{clierrPackage, "PriorEntry", "URL"},
	{clierrPackage, "SupersededToken", "Server"},
}

// serverURLShown are the functions whose argument may be a stored URL: the
// display form of a URL, and the identity of one.
var serverURLShown = map[string]bool{"DisplayServerURL": true, "CanonicalServerIdentity": true}

// storedURLReadAllowed maps "<file>:<function>" to the reads of a stored,
// effective or prior server URL that are neither the argument of a display
// function nor an assignment target, with the count and why each is safe: it
// builds a request, compares or canonicalizes the URL, or hands it to a
// function that checks it. Anything else shows the URL in output and goes
// through config.DisplayServerURL.
var storedURLReadAllowed = map[string]requestAllowance{
	"internal/cli/capability.go:Check":                              {1, "the capability details; Effective refused an unusable URL before this read"},
	"internal/cli/completion_token.go:fileStoreTokenSilent":         {2, "comparison with the effective URL"},
	"internal/cli/completion_token.go:resolveCompletionConnection":  {1, "completion's connection; completionServerURL refuses an unusable URL and the trace shows the display form"},
	"internal/cli/engine.go:Root":                                   {2, "request building, the TLS wrap and the base of the install id request, after Effective refused an unusable URL"},
	"internal/cli/engine.go:ServerBaseURL":                          {1, "scheme and host for a request, after Effective refused an unusable URL"},
	"internal/cli/record.go:currentURL":                             {1, "telemetry: canonicalized, only a host is recorded"},
	"internal/cli/record.go:effectiveConnectionURL":                 {1, "telemetry: empty for a refused URL, canonicalized otherwise"},
	"internal/cli/record.go:entryURL":                               {1, "telemetry: canonicalized, only a host is recorded"},
	"internal/client/kube/restconfig.go:RESTConfig":                 {1, "request building; BaseURL keeps scheme and host only"},
	"internal/client/kube/restconfig.go:RequireLogin":               {1, "emptiness check"},
	"internal/client/provider/version.go:fetchVersionResponse":      {1, "request building, after Effective refused an unusable URL"},
	"internal/clierr/loginfailure.go:text":                          {1, "the prior entry's URL, set from config.DisplayServerURL by its only producer"},
	"internal/commands/auth/authcheck.go:Execute":                   {2, "emptiness check, and the URL the install id request is built from, after Preflight reached it"},
	"internal/commands/auth/logintarget.go:entryLoginTarget":        {3, "the stored URL is checked (userinfo, parse), and is the login's target"},
	"internal/commands/auth/logintarget.go:urlLoginTarget":          {2, "comparison with the typed URL, and the stored URL handed to the collision check; the message shows the display form or no URL"},
	"internal/commands/configcmd/config_info.go:entrySection":       {7, "recovery sequence source, omitted for a value that cannot be used, the entry's own URL for the repair text, an emptiness check, and the entry's URL and the record's server handed to config.DisplayStoredRecordServer"},
	"internal/commands/configcmd/config_update_server.go:Execute":   {2, "the entry's own URL as the update's starting value, canonicalized by register.CanonicalizeServerURL, and a comparison"},
	"internal/commands/configcmd/token_records.go:entryReport":      {2, "recovery sequence source; the sequence is omitted for a URL CheckServerURL refuses"},
	"internal/commands/configcmd/token_records.go:recordLines":      {6, "the entry's own URL and the record's server, handed to config.AdviseRecord, and the administrator line, which shows the display form of the record's server for that entry"},
	"internal/commands/configcmd/token_records.go:recordReport":     {6, "the entry's own URL and the record's server, handed to config.AdviseRecord and config.DisplayStoredRecordServer, and an emptiness check"},
	"internal/commands/configcmd/token_records.go:recordState":      {1, "comparison"},
	"internal/token/retire.go:RetireSuperseded":                     {4, "comparison, and the record's server handed to config.AdviseRecord; the text shows the display form"},
	"internal/commands/register/register.go:PrepareUpdateSource":    {2, "the entry's own URL as the update's source snapshot"},
	"internal/commands/register/register.go:SnapshotMatchesCurrent": {1, "comparison"},
	"internal/commands/register/register.go:ValidateUpdate":         {5, "comparison, and the stored URL the userinfo-only rewrite compares with the new one"},
	"internal/commands/register/register.go:commitCreateWith":       {3, "comparison, and the stored URL handed to the conflict check; the message shows the display form or no URL"},
	"internal/config/cachebinding.go:MatchingServerNames":           {1, "comparison"},
	"internal/config/cachebinding.go:recordInputs":                  {1, "input of the cache binding computation, never shown"},
	"internal/config/cacherevoke.go:InDeleteScope":                  {1, "comparison"},
	"internal/config/config.go:ErrNotLoggedIn":                      {1, "the inventory ErrNotLoggedInFor reads; it leaves an unusable URL out of every command"},
	"internal/config/credtransitionrun.go:entryURLFor":              {1, "credential store reference, never shown"},
	"internal/config/identify.go:IdentifyEntry":                     {1, "comparison"},
	"internal/config/persistlogin.go:resolveEntryName":              {1, "comparison; the conflict message shows the display form"},
	"internal/config/resolve.go:Effective":                          {4, "comparison, and the stored token read, after the URL check"},
	"internal/config/resolve.go:connection":                         {6, "resolving the connection and checking the URL that wins"},
	"internal/config/resolve.go:tlsRemedyFor":                       {1, "emptiness check"},
	"internal/engine/kubeconfig/command.go:Execute":                 {2, "emptiness check, and scheme and host for the kubeconfig, after Effective refused an unusable URL"},
	"internal/config/cachebinding.go:usableEntryNames":              {1, "the entries' URLs, checked so a record is bound to the entries the lookup reads; never shown"},
	"internal/config/cachebinding.go:recordNamesUnusableEntry":      {1, "the entries' URLs, checked so a record that names an unusable entry is kept; never shown"},
	"internal/config/cachebinding.go:servesRecord":                  {1, "the identified entry's URL, checked so a record bound to an unusable entry is a miss; never shown"},
	"internal/token/cachekeyshown.go:misreadEntryURL":               {1, "the entry's URL, handed to config.DisplayStoredRecordServer with the cache record's server; never shown"},
	"internal/token/entry.go:readEntryCredential":                   {1, "credential store reference, never shown"},
	"internal/token/noterminal.go:NoTerminalLogin":                  {1, "comparison"},
	"internal/token/noterminal.go:currentLoginServes":               {2, "comparison and credential store reference"},
	"internal/token/revocation.go:deleteLogins":                     {1, "credential store reference, never shown"},
	"internal/token/revocation.go:newClient":                        {4, "the URL is checked first; the client dials scheme and host of a usable URL"},
	"internal/token/revocation.go:readLogins":                       {1, "credential store reference, never shown"},
	"internal/token/revocation.go:revokeCA":                         {2, "CA lookup by URL, never shown"},
	"internal/token/revocationplan.go:firstWrite":                   {5, "the entry's URL as loaded and each record's server, handed to config.RecordServerIdentity so the identity filled in or stored is never one read from an @ after the host; and an emptiness check of the record's server"},
	"internal/token/entry.go:unusableEntryError":                    {1, "the entry's URL, checked so an unusable one gets its refusal and never a command built from it; never shown"},
	"internal/token/entry.go:usableEntry":                           {1, "the entry's URL, checked before its stored login is read; never shown"},
	"internal/token/revocationplan.go:planEntry":                    {2, "the record's server as stored and the entry's URL as loaded, kept so config.AdviseRecord and config.DisplayRecordServer see the user information and the @ after the host the identity drops; never shown"},
	"internal/token/revocationplan.go:planLogin":                    {1, "the entry's URL as loaded, kept for config.AdviseRecord and config.DisplayRecordServer; never shown"},
	"internal/token/revocationplan.go:planCacheRecord":              {2, "comparisons"},
	"internal/token/revocationplan.go:serverRecordParent":           {1, "comparison"},
	"internal/token/revocationplan.go:retry":                        {1, "a check of the entry's URL that sends a record to the manual list; never shown"},
	"internal/token/revocationplan.go:trustFor":                     {1, "comparison"},
	"internal/token/revocationplan.go:site":                         {1, "the entry's own URL, handed with the token's server to config.AdviseRecord and config.DisplayRecordServer"},
	"internal/token/token.go:connectionTarget":                      {1, "the server a call connects to, after Effective refused an unusable URL"},
	"internal/token/userid.go:RecordedUserIDs":                      {1, "the server a call connects to, after the refusal check"},
}

// serverURLFieldRead returns "Type.Field" when sel reads one of the URL
// fields, through any chain of embedded structs, and "" otherwise.
func serverURLFieldRead(info *types.Info, sel *ast.SelectorExpr) string {
	s := info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal {
		return ""
	}
	t := derefType(s.Recv())
	idx := s.Index()
	for _, i := range idx[:len(idx)-1] {
		strukt, ok := t.Underlying().(*types.Struct)
		if !ok {
			return ""
		}
		t = derefType(strukt.Field(i).Type())
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	for _, f := range urlFields {
		if named.Obj().Pkg().Path() == f.pkg && named.Obj().Name() == f.typ && s.Obj().Name() == f.field {
			return f.typ + "." + f.field
		}
	}
	return ""
}

// calleeIsShown reports whether call calls a function of the config package
// named in serverURLShown.
func calleeIsShown(info *types.Info, call *ast.CallExpr) bool {
	fn := methodOf(info, call)
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == configPackage && serverURLShown[fn.Name()]
}

// scanStoredURLReads returns the reads of the URL fields in pkg's production
// files that are not a direct argument of a display function and not an
// assignment target.
func scanStoredURLReads(pkg *packages.Package, wd string) []requestMatch {
	info := pkg.TypesInfo
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
			exempt := map[ast.Expr]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if calleeIsShown(info, n) {
						for _, a := range n.Args {
							exempt[ast.Unparen(a)] = true
						}
					}
				case *ast.AssignStmt:
					if n.Tok == token.ASSIGN || n.Tok == token.DEFINE {
						for _, l := range n.Lhs {
							exempt[ast.Unparen(l)] = true
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || exempt[sel] {
					return true
				}
				if field := serverURLFieldRead(info, sel); field != "" {
					out = append(out, requestMatch{rel + ":" + fn, fmt.Sprintf("%s:%d: read of %s", rel, pkg.Fset.Position(sel.Pos()).Line, field)})
				}
				return true
			})
		}
	}
	return out
}

func loadStoredURLFixture(t *testing.T, name string) []requestMatch {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./test/conformance/testdata/serverurl/"+name)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	for _, e := range pkgs[0].Errors {
		require.Fail(t, "type-check "+name, e.Error())
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	return scanStoredURLReads(pkgs[0], wd)
}

// TestNoStoredServerURLIsPrintedRaw asserts every read of a field that holds a
// stored, effective or prior server URL (config.Server.URL,
// config.Effective.URL, clierr.PriorEntry.URL, config.SupersededToken.Server) in
// production code is the direct argument of config.DisplayServerURL or
// config.CanonicalServerIdentity, an assignment target, or in the allowlist
// with its count and the reason it cannot show the URL. A stored URL can hold
// a password only through a hand-edited config.yaml; showing it raw puts the
// password in output. Whole-struct formatting and map values are not seen by
// the scan; the display function is applied at those sites by hand.
func TestNoStoredServerURLIsPrintedRaw(t *testing.T) {
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
		matches = append(matches, scanStoredURLReads(pkg, wd)...)
	}
	for _, problem := range checkRequestMatches(matches, storedURLReadAllowed) {
		t.Error(problem)
	}
	for key, a := range storedURLReadAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestStoredURLScanFixtures asserts the scan on fixture packages: a raw read
// into a message, and into a map of result data, is reported; a read that is
// the argument of the display function or the identity function, an
// assignment target, and the URL field of an unrelated struct are not.
func TestStoredURLScanFixtures(t *testing.T) {
	t.Parallel()
	texts := func(ms []requestMatch) []string {
		var out []string
		for _, m := range ms {
			_, rest, _ := strings.Cut(m.text, ": ")
			out = append(out, rest)
		}
		return out
	}
	assert.Equal(t, []string{"read of Server.URL"}, texts(loadStoredURLFixture(t, "rawmessage")))
	assert.ElementsMatch(t, []string{"read of Effective.URL", "read of SupersededToken.Server"}, texts(loadStoredURLFixture(t, "rawmap")))
	assert.Empty(t, loadStoredURLFixture(t, "wrapped"))
	assert.Empty(t, loadStoredURLFixture(t, "readsother"))
}

// TestStoredURLAllowlistRules asserts the allowlist rules for reads: a read
// outside the list is reported, a different count fails and a stale entry
// fails.
func TestStoredURLAllowlistRules(t *testing.T) {
	t.Parallel()
	one := requestMatch{"a.go:f", "a.go:1: read of Server.URL"}
	allow := map[string]requestAllowance{"a.go:f": {1, "why"}}
	assert.Empty(t, checkRequestMatches([]requestMatch{one}, allow))
	assert.Len(t, checkRequestMatches([]requestMatch{one, {"b.go:g", "b.go:2: read of Server.URL"}}, allow), 1)
	assert.Len(t, checkRequestMatches([]requestMatch{one, {"a.go:f", "a.go:3: read of Server.URL"}}, allow), 1)
	assert.Len(t, checkRequestMatches([]requestMatch{one}, map[string]requestAllowance{"a.go:f": {1, "why"}, "c.go:gone": {1, "why"}}), 1)
}
