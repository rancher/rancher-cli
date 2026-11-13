package token

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// receiverTypeName is the base type name of a method receiver, without the
// pointer or the type parameters.
func receiverTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(x.X)
	case *ast.IndexExpr:
		return receiverTypeName(x.X)
	case *ast.IndexListExpr:
		return receiverTypeName(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// funcKey names a function declaration as Function or Receiver.Method.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		if name := receiverTypeName(fn.Recv.List[0].Type); name != "" {
			return name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// parseNonTestFiles parses the non-test .go files of dir.
func parseNonTestFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, dir)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		files = append(files, file)
	}
	return files
}

// freshTokenProducers lists, from the package's source, every place that can
// make a token whose delete a cancelled wait reports as failed: each call of
// newFreshToken, each freshToken composite literal and new(freshToken) call,
// and each variable declared as a freshToken, outside newFreshToken. A site is
// named Receiver.Function(first argument as written) for a call,
// Receiver.Function{} for a literal, Receiver.Function.new() and
// Receiver.Function.var() for the other two. A freshToken made another way (a
// type conversion, a field of another struct) is not counted.
func freshTokenProducers(t *testing.T) []string {
	t.Helper()
	var sites []string
	for _, file := range parseNonTestFiles(t, ".") {
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "newFreshToken" {
				continue
			}
			name := funcKey(fn)
			isFresh := func(e ast.Expr) bool {
				id, ok := e.(*ast.Ident)
				return ok && id.Name == "freshToken"
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "newFreshToken" && len(x.Args) > 0 {
						sites = append(sites, name+"("+types.ExprString(x.Args[0])+")")
					}
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isFresh(x.Args[0]) {
						sites = append(sites, name+".new()")
					}
				case *ast.CompositeLit:
					if isFresh(x.Type) {
						sites = append(sites, name+"{}")
					}
				case *ast.ValueSpec:
					if isFresh(x.Type) {
						sites = append(sites, name+".var()")
					}
				}
				return true
			})
		}
	}
	slices.Sort(sites)
	return sites
}

// leftoverProducers has one row for each producer of a fresh token: the test
// function and the subtest in it that reaches the producer through a real
// call, cancels the wait for the direct endpoint, makes the delete fail, and
// asserts the message names who can delete the token and the command. Two
// producers never share a subtest.
var leftoverProducers = map[string]string{
	"tokenCmd.pluginLogin(ct)":           "TestOtherProducersKeepTheirCancelledWaitDetails/the plugin-created login",
	"tokenCmd.runWithParams(res.Create)": "TestOtherProducersKeepTheirCancelledWaitDetails/the legacy shim with no cluster",
	"tokenCmd.runWithParams(si.Cluster)": "TestSignInCancelledDuringTheDirectWait/the cluster token cannot be revoked",
	"tokenCmd.storedCredToken(created)":  "TestOtherProducersKeepTheirCancelledWaitDetails/a stored login",
}

// testHoldsString reports whether the body of the test function name, in the
// test files of dir, holds the string literal text, as a whole literal: a
// subtest named in a table field or in a t.Run call, or a word a row asserts.
// The name of a subtest is a run-time string, so the source is the only place
// to look. A literal declared outside the function, and a part of a longer
// literal, are not found. That the subtest reaches the producer is not
// checked here.
func testHoldsString(t *testing.T, dir, name, text string) (found, testExists bool) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, path := range paths {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			testExists = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && s == text {
						found = true
					}
				}
				return !found
			})
		}
	}
	return found, testExists
}

// TestEveryProducerOfAFreshTokenHasARealCallRow asserts the producers the
// source holds are the producers of leftoverProducers: a new call of
// newFreshToken, a freshToken literal, new(freshToken) or freshToken variable
// with no row fails, and so does a row whose site is gone, a row whose test
// or subtest name is not in the source, and two rows that share one subtest.
// Four sites at the time of writing: the sign-in with token reuse off, the
// legacy shim's login with no cluster, the plugin-created login and a stored
// login's child token. Gap: the guard finds the subtest's name in the named
// test, and cannot show that the subtest reaches the producer.
func TestEveryProducerOfAFreshTokenHasARealCallRow(t *testing.T) {
	sites := freshTokenProducers(t)
	var rows []string
	for site := range leftoverProducers {
		rows = append(rows, site)
	}
	slices.Sort(rows)
	assert.Equal(t, rows, sites, "each producer of a fresh token has one row naming the subtest that cancels its wait with the delete failing")
	assert.Len(t, sites, 4)

	seen := map[string]string{}
	for site, row := range leftoverProducers {
		test, subtest, ok := strings.Cut(row, "/")
		require.True(t, ok, "%s: a row names TestName/subtest, got %q", site, row)
		found, exists := testHoldsString(t, ".", test, subtest)
		assert.True(t, exists, "%s: the test %s does not exist", site, test)
		assert.True(t, !exists || found, "%s: the test %s holds no subtest named %q", site, test, subtest)
		if other, dup := seen[row]; dup {
			assert.Fail(t, site+" and "+other+" share the subtest "+row)
		}
		seen[row] = site
	}
}

// leftoverRow is one message about a token the CLI could not delete. A person
// is named when the command is not the operator's own to run: an
// administrator for a v3 token, the owner for an ext token, or the user who
// signed in. A row with no person says why not in own and asserts the whole
// text. marker, when set, is the operation marker the text names for a token
// it cannot name. covers lists the message builders the row's text goes
// through, as package.Function or package.Receiver.Method.
type leftoverRow struct {
	name    string
	text    string
	noun    string
	marker  string
	person  string
	command string
	own     string
	whole   string
	covers  []string
}

// leftoverRows is one row for each kind of token that reaches each message
// built in package token.
func leftoverRows(t *testing.T) []leftoverRow {
	t.Helper()
	extRef := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	v3Ref := &TokenRef{Kind: TokenKindV3, Name: "token-v3"}
	browser := &SAMLHandoff{Provider: "saml", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:05:00Z"}
	failed := RevokeOutcome{Result: ResultFailed, Error: "500"}
	const (
		owner      = "user 'u-abc12' can delete it from a directory signed in as user 'u-abc12'"
		signedIn   = "the user who signed in can delete it from a directory signed in as that user"
		admin      = "an administrator can delete it sooner with: "
		deleteExt  = "rancher delete token 'token-abc' --apply --yes"
		deleteV3   = "rancher delete management-token 'token-v3' --apply --yes --allow-deprecated"
		listV3     = "an administrator can inspect them with: rancher get management-tokens -o json"
		ownReason  = "the operator's own token; the command runs as that operator"
		samlUI     = "the user who signed in can find them in the UI under Account & API Keys"
		listExt    = "rancher get tokens"
		listMgmt   = "rancher get management-tokens"
		administer = "an administrator can list tokens with: rancher get management-tokens"
		login      = "log in again with: rancher auth login 'prod'"
		serverURL  = "https://hostzq7"
		noServer   = "delete it as user 'u-abc12', signed in to the server that issued it"
		noServerAn = "delete it as its owner or an administrator, signed in to the server that issued it"
		badServer  = "delete it there as user 'u-abc12' while signed in to that server"
		badServerA = "delete it there as its owner or an administrator while signed in to that server"
	)
	wait := func(create CreateResult) string {
		fresh := &freshToken{create: create}
		ce, ok := errors.AsType[*clierr.Error](directWaitCancelled(context.Canceled, fresh, failed))
		require.True(t, ok)
		return ce.Message
	}
	warning := func(f clierr.TokenFate) string {
		text, ok := LoginTokenWarning(&f, func(f clierr.TokenFate) string {
			recovery, _ := ResidualRecovery(f, "prod", "m", true)
			return recovery
		})
		require.True(t, ok)
		return text
	}
	residual := func(f clierr.TokenFate, persisted bool) string {
		text, _ := ResidualRecovery(f, "prod", "m", persisted)
		return text
	}
	marked := func(kind clierr.TokenKind) clierr.TokenFate {
		return clierr.TokenFate{Outcome: clierr.CreateUnknown, Kind: kind, Marked: true}
	}
	signIn := func(f clierr.TokenFate, userID string) string {
		return signInRecovery(&f, nil, "m", userID, "", "")
	}
	// readOnly runs FinishUnsupportedStore, the finalizer a read-only
	// credential helper reaches, against a server whose delete answers 500.
	readOnly := func(cred LoginCredential) string {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		cred.client, cred.base, cred.parent = srv.Client(), srv.URL, t.Context()
		var st LoginStates
		FinishUnsupportedStore(cred, "prod", &st)
		require.Len(t, st.Warnings, 2)
		return st.Warnings[1]
	}
	known := func(ref *TokenRef, search *clierr.UnmarkedTokenSearch) clierr.TokenFate {
		f := clierr.TokenFate{Outcome: clierr.CreateKnown, Kind: ref.Kind, Ref: ref, Marked: search == nil, Cleanup: &failed}
		f.Search = search
		return f
	}
	kept := func(ref *TokenRef, userID, server string) string {
		return keptWarning(config.SupersededToken{Ref: *ref, UserID: userID}, server, "the server refused")
	}
	manual := func(it *revocationItem) string {
		d := &Deletion{cfg: &config.Config{}, view: &config.Config{}, req: LocalDelete{Rerun: clierr.NewCommand("auth logout").Arg("prod")}}
		return d.manualWarning(it)
	}
	search := FateOf(clierr.CreateKnown, TokenKindV3, nil, false, "", "", RevokeOutcome{}, browser).Search
	storedSAML := LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: v3Ref, Result: ResultFailed, Error: "500"}, SessionTokenUnmarked: true, SessionTokenSearch: browser}

	waitCovers := []string{"token.directWaitCancelled", "token.freshToken.leftover", "token.signInLeftover"}
	v3Covers := []string{"token.v3AdminRemedy", "token.v3AdminDelete"}
	finishCovers := []string{"token.FinishUnsupportedStore", "token.ResidualRecovery"}
	rows := []leftoverRow{
		{name: "a cancelled wait, an ext token whose owner the create named", text: wait(CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: extRef, UserID: "u-abc12"}),
			noun: "the token it created could not be revoked", person: owner, command: deleteExt, covers: waitCovers},
		{name: "a cancelled wait, an ext token with no owner named", text: wait(CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: extRef}),
			noun: "the token it created could not be revoked", person: signedIn, command: deleteExt, covers: waitCovers},
		{name: "a cancelled wait, a v3 token", text: wait(CreateResult{Outcome: CreateKnown, Kind: TokenKindV3, Ref: v3Ref, ExpiresAt: "2026-10-12T10:00:00Z"}),
			noun: "v3/token-v3 expires at 2026-10-12T10:00:00Z", person: admin, command: deleteV3, covers: slices.Concat(waitCovers, v3Covers)},
		{name: "the warning of auth login, an ext session token", text: warning(known(extRef, nil)),
			noun: "the transient session token ext/token-abc", command: deleteExt, own: ownReason,
			whole:  "the transient session token ext/token-abc created by this login could not be revoked (500); revoke ext/token-abc with: rancher delete token 'token-abc' --apply --yes",
			covers: []string{"token.LoginTokenWarning", "token.ResidualRecovery"}},
		{name: "the warning of auth login, a v3 session token", text: warning(known(v3Ref, nil)),
			noun: "the transient session token v3/token-v3", person: admin, command: deleteV3,
			covers: slices.Concat([]string{"token.LoginTokenWarning", "token.ResidualRecovery"}, v3Covers)},
		{name: "the warning of auth login, a SAML kubeconfig token", text: warning(known(v3Ref, search)),
			noun: "the transient kubeconfig token v3/token-v3", person: admin, command: deleteV3,
			covers: slices.Concat([]string{"token.LoginTokenWarning", "token.ResidualRecovery"}, v3Covers)},
		{name: "the recovery of a failed login, a SAML token the CLI cannot name", text: residual(clierr.TokenFate{Search: search}, true),
			noun: "a kubeconfig token may have been created through saml", person: samlUI, command: listV3,
			covers: []string{"token.ResidualRecovery", "token.signInLeftover"}},
		{name: "the recovery of a failed login, a SAML token the CLI can name", text: residual(known(v3Ref, search), true),
			noun: "v3/token-v3", person: admin, command: deleteV3, covers: slices.Concat([]string{"token.ResidualRecovery"}, v3Covers)},
		{name: "the recovery of a failed login, an ext token", text: residual(known(extRef, nil), true),
			noun: "ext/token-abc", command: deleteExt, own: ownReason,
			whole:  "revoke ext/token-abc with: rancher delete token 'token-abc' --apply --yes",
			covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a SAML token the CLI cannot name", text: residual(clierr.TokenFate{Search: search}, false),
			noun: "a kubeconfig token may have been created through saml", person: samlUI, command: listV3,
			covers: []string{"token.ResidualRecovery", "token.signInLeftover"}},

		{name: "the recovery of a failed login, a marked ext token with no name", text: residual(marked(clierr.TokenKindExt), true),
			noun: "a leftover token created by this login", marker: "[op:m]", command: listExt, own: ownReason,
			whole:  `list tokens with: rancher get tokens; a leftover token created by this login carries "[op:m]" in its description; revoke it with the found name`,
			covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a marked ext token with no name", text: residual(marked(clierr.TokenKindExt), false),
			noun: "a leftover token created by this login", marker: "[op:m]", command: listExt, own: ownReason,
			whole:  `no credential was stored; ` + login + `; then list tokens with: rancher get tokens; a leftover token created by this login carries "[op:m]" in its description; revoke it with the found name`,
			covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login, a marked v3 token with no name, who lists", text: residual(marked(clierr.TokenKindV3), true),
			noun: "a leftover token created by this login", marker: "[op:m]", person: administer, command: listMgmt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login, a marked v3 token with no name, who revokes", text: residual(marked(clierr.TokenKindV3), true),
			noun: "a leftover token created by this login", marker: "[op:m]", person: "an administrator can revoke it with the found name", command: listMgmt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a marked v3 token with no name, who lists", text: residual(marked(clierr.TokenKindV3), false),
			noun: "no credential was stored; " + login + "; an administrator", marker: "[op:m]", person: administer, command: listMgmt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a marked v3 token with no name, who revokes", text: residual(marked(clierr.TokenKindV3), false),
			noun: "no credential was stored; " + login + "; an administrator", marker: "[op:m]", person: "an administrator can revoke it with the found name", command: listMgmt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login, a marked token of unknown kind, who lists management tokens", text: residual(marked(""), true),
			noun: "a leftover token created by this login", marker: "[op:m]", person: "an administrator can also list management tokens with: rancher get management-tokens", command: listExt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login, a marked token of unknown kind, who revokes", text: residual(marked(""), true),
			noun: "a leftover token created by this login", marker: "[op:m]", person: "an administrator can revoke one from the management-token list", command: listMgmt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a marked token of unknown kind, who lists management tokens", text: residual(marked(""), false),
			noun: "no credential was stored; " + login, marker: "[op:m]", person: "an administrator can also list management tokens with: rancher get management-tokens", command: listExt, covers: []string{"token.ResidualRecovery"}},
		{name: "the recovery of a failed login that stored nothing, a marked token of unknown kind, who revokes", text: residual(marked(""), false),
			noun: "no credential was stored; " + login, marker: "[op:m]", person: "an administrator can revoke one from the management-token list", command: listMgmt, covers: []string{"token.ResidualRecovery"}},

		{name: "a failed sign-in, a marked ext token with no name and a known owner", text: signIn(marked(clierr.TokenKindExt), "u-1"),
			noun: "the session token: ", marker: "[op:m]", person: "user 'u-1' can list tokens from a directory signed in as user 'u-1'", command: listExt,
			covers: []string{"token.signInRecovery", "token.signInLeftover"}},
		{name: "a failed sign-in, a marked v3 token with no name", text: signIn(marked(clierr.TokenKindV3), "u-1"),
			noun: "the session token: ", marker: "[op:m]", person: administer, command: listMgmt,
			covers: []string{"token.signInRecovery", "token.signInLeftover"}},
		{name: "a failed sign-in, a marked token of unknown kind and owner, the user who signed in", text: signIn(marked(""), ""),
			noun: "the session token: ", marker: "[op:m]", person: "the user who signed in can list tokens from a directory signed in as that user", command: listExt,
			covers: []string{"token.signInRecovery", "token.signInLeftover"}},
		{name: "a failed sign-in, a marked token of unknown kind and owner, an administrator", text: signIn(marked(""), ""),
			noun: "the session token: ", marker: "[op:m]", person: administer, command: listMgmt,
			covers: []string{"token.signInRecovery", "token.signInLeftover"}},

		{name: "a read-only credential store, a SAML kubeconfig token", text: readOnly(storedSAML),
			noun: "the transient kubeconfig token v3/token-v3", person: admin, command: deleteV3, covers: slices.Concat(finishCovers, v3Covers)},
		{name: "a read-only credential store, a v3 session token", text: readOnly(LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: v3Ref, Result: ResultFailed, Error: "500"}}),
			noun: "the transient session token v3/token-v3", person: admin, command: deleteV3, covers: slices.Concat(finishCovers, v3Covers)},
		{name: "a read-only credential store, an ext session token", text: readOnly(LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: extRef, Result: ResultFailed, Error: "500"}}),
			noun: "the transient session token ext/token-abc", command: deleteExt, own: ownReason,
			whole:  "the transient session token ext/token-abc created by this login could not be revoked (500); no credential was stored; " + login + "; then revoke ext/token-abc with: rancher delete token 'token-abc' --apply --yes",
			covers: finishCovers},
		{name: "a read-only credential store, a derived token", text: readOnly(LoginCredential{Provenance: ProvenanceDerived, Ref: extRef, Token: "ext/token-abc:secret"}),
			noun: "the derived token ext/token-abc", command: deleteExt, own: ownReason,
			whole:  "the derived token ext/token-abc created by this login could not be revoked (500 Internal Server Error); no credential was stored; " + login + "; then revoke ext/token-abc with: rancher delete token 'token-abc' --apply --yes",
			covers: finishCovers},

		{name: "a kept superseded token, an ext token with a known owner", text: kept(extRef, "u-abc12", serverURL),
			noun: "the superseded token ext/token-abc was not retired", person: "revoke it as user 'u-abc12' with: ", command: deleteExt, covers: []string{"token.keptWarning"}},
		{name: "a kept superseded token, an ext token with no known owner", text: kept(extRef, "", serverURL),
			noun: "the superseded token ext/token-abc was not retired", person: "revoke it as its owner or an administrator with: ", command: deleteExt, covers: []string{"token.keptWarning"}},
		{name: "a kept superseded token, a v3 token", text: kept(v3Ref, "u-abc12", serverURL),
			noun: "the superseded token v3/token-v3 was not retired", person: admin, command: deleteV3, covers: slices.Concat([]string{"token.keptWarning"}, v3Covers)},

		{name: "a delete plan, an ext token with a known owner", text: manual(&revocationItem{source: sourceCache, ref: extRef, server: serverURL, owner: "u-abc12", reason: reasonFailed}),
			noun: "the cached token ext/token-abc was not revoked", person: "revoke it as user 'u-abc12' with: ", command: deleteExt, covers: []string{"token.Deletion.manualWarning"}},
		{name: "a delete plan, an ext token with no known owner", text: manual(&revocationItem{source: sourceCache, ref: extRef, server: serverURL, reason: reasonFailed}),
			noun: "the cached token ext/token-abc was not revoked", person: "revoke it as its owner or an administrator with: ", command: deleteExt, covers: []string{"token.Deletion.manualWarning"}},
		{name: "a delete plan, a v3 token", text: manual(&revocationItem{source: sourceSuperseded, ref: v3Ref, server: serverURL, reason: reasonFailed}),
			noun: "the superseded token v3/token-v3 was not revoked", person: admin, command: deleteV3, covers: slices.Concat([]string{"token.Deletion.manualWarning"}, v3Covers)},
		{name: "a delete plan, a v3 token the plan cannot delete", text: manual(&revocationItem{source: sourceSuperseded, ref: v3Ref, server: serverURL, reason: reasonV3}),
			noun: "the superseded token v3/token-v3 was not revoked", person: admin, command: deleteV3, covers: slices.Concat([]string{"token.Deletion.manualWarning"}, v3Covers)},
		{name: "a delete plan, a token whose local copy was not deleted", text: manual(&revocationItem{source: sourceLogin, ref: extRef, server: serverURL, reason: reasonNotDeleted}),
			noun: "the login token ext/token-abc was not revoked", command: "rancher auth logout 'prod'", own: "the command is the CLI's own auth logout, which the operator runs",
			whole:  "the login token ext/token-abc was not revoked: its local copy was not deleted and it stays in use; to delete it, run: rancher auth logout 'prod'",
			covers: []string{"token.Deletion.manualWarning"}},
		{name: "a delete plan, a cached token the CLI cannot prove it created", text: manual(&revocationItem{source: sourceCache, server: serverURL, reason: reasonUnproven}),
			noun: "the cached token with no name recorded was not revoked", command: listExt, own: "the command lists the caller's own tokens",
			whole:  "the cached token with no name recorded was not revoked: nothing proves the CLI created it; find it with: rancher get tokens",
			covers: []string{"token.Deletion.manualWarning"}},
	}
	// The same warning when the server of the record cannot be used: no server
	// is recorded, or the one recorded fails the check. The owner, when known,
	// runs the delete; with none known, its owner or an administrator does.
	for _, tc := range []struct {
		name, server, userID, noun, person string
	}{
		{"no server recorded, a known owner", "", "u-abc12", "no server is recorded for token 'token-abc'", noServer},
		{"no server recorded, no known owner", "", "", "no server is recorded for token 'token-abc'", noServerAn},
		{"a recorded server that cannot be used, a known owner", "unusable:https://old.ex:abc", "u-abc12", "the server recorded for token 'token-abc' cannot be used", badServer},
		{"a recorded server that cannot be used, no known owner", "unusable:https://old.ex:abc", "", "the server recorded for token 'token-abc' cannot be used", badServerA},
	} {
		rows = append(rows,
			leftoverRow{name: "a kept superseded token, " + tc.name, text: kept(extRef, tc.userID, tc.server),
				noun: tc.noun, person: tc.person, command: deleteExt, covers: []string{"token.keptWarning"}},
			leftoverRow{name: "a delete plan, " + tc.name, text: manual(&revocationItem{source: sourceCache, ref: extRef, server: tc.server, recorded: tc.server, owner: tc.userID, reason: reasonFailed}),
				noun: tc.noun, person: tc.person, command: deleteExt, covers: []string{"token.Deletion.manualWarning"}})
	}
	return rows
}

// TestEveryLeftoverTokenMessageOfEveryPackageThatBuildsOneNamesTheTokenTheCommandAndWhoRunsIt
// asserts, for each message about a token the CLI could not delete that
// package token builds and each kind of token that reaches it, the token (its
// noun or its name, and the marker for a token the CLI cannot name), the
// command, and the person who can run the command when that person is not
// the operator who signed in. A message that names no person (the operator's
// own token) is asserted whole. No message says that only an administrator
// can act: the owner of a v3 token can delete it through Rancher's API. The
// rows are the cancelled wait of a fresh token, the warning of auth login for
// the token it created, the recovery of a failed login and of a failed
// sign-in, the recovery of a login whose store is read-only, the text for a
// SAML token the CLI cannot name, the warning for a superseded token that was
// kept and the warning of a delete plan. The messages of internal/config and
// internal/commands/configcmd are asserted in tests of those packages, which
// leftoverRowsInTests names.
func TestEveryLeftoverTokenMessageOfEveryPackageThatBuildsOneNamesTheTokenTheCommandAndWhoRunsIt(t *testing.T) {
	for _, row := range leftoverRows(t) {
		t.Run(row.name, func(t *testing.T) {
			text := clierr.ExpandRemedies(row.text, "")
			assert.Contains(t, text, row.noun)
			assert.Contains(t, text, row.marker)
			assert.Contains(t, text, row.command)
			assert.NotContains(t, text, "only an administrator")
			if row.person == "" {
				assert.NotEmpty(t, row.own, "a message that names no person says why: the command is the operator's own")
				assert.Equal(t, row.whole, text)
				return
			}
			assert.Contains(t, text, row.person)
			assert.Empty(t, row.own)
			assert.Empty(t, row.whole)
		})
	}
}

// leftoverNotMessages lists the builders the source scan selects that need no
// row, each with the reason: the text it builds is asserted where the reason
// says, it builds no message about a token, or it names a person
// another way. Keys are package.Function or package.Receiver.Method.
var leftoverNotMessages = map[string]string{
	"token.FailLogin":                          "calls the Revoke method of the login credential, which deletes the token the login made; the recovery text comes from ResidualRecovery, covered by its rows",
	"auth.loginCmd.authenticateAndStore":       "builds the warning of auth login through LoginTokenWarning and ResidualRecovery, covered by the rows of those two for an ext, a v3 and a SAML kubeconfig token; login_test.go asserts the warning's noun and command for a v3 session token and a SAML kubeconfig token, and that the warning of an ext session token says it could not be revoked",
	"clierr.Revoke":                            "returns the delete command of a token, with its list command, for a message to name; it builds no message",
	"clierr.RecoverySequence":                  "a list of commands to run in order, not a message about a token",
	"config.AdviseRecord":                      "chooses between the sequence of RecoverySequence and the text of noServerText, recordServerText or EntryOrigin, and writes no text of its own",
	"configcmd.configDeleteServerCmd.Describe": "the help text of config delete-server; it names no token",
	"configcmd.keptTokenNotice":                "the warning for a token the rewrite of a URL keeps in the cache: it names auth list-tokens and, for a record not of the owner kind, auth delete-token with the shown key, which need no person: the token is revoked through a usable entry; TestCacheRecordsAcrossConfigChanges asserts that the warning names each token it keeps once and the commands it names",
	"configcmd.tokenRecovery":                  "returns the recovery commands for the recovery list of config status -o json: commands alone, no sentence",
	"token.Deletion.advise":                    "returns the advice of config.AdviseRecord for a delete plan item and writes no text of its own",
	"token.Deletion.recovery":                  "returns the commands of a deletion for the result data, not a message",
	"token.RetireSuperseded":                   "the literal is the reason keptWarning wraps; the message itself is built by keptWarning, v3Remedy and unusableServerKept",
	"token.clusterSignIn.warning":              "the sign-in's warning for its login token, built only through LoginTokenWarning and signInLeftover, whose rows assert the noun, the command and the person",
	"token.deleteTokenCmd.Execute":             "the literal is the warning for an unreadable cache; no token is known to name",
	"token.recoveryHint":                       "returns the hint command only, not a message",
	"token.recoveryText":                       "joins the recovery of ResidualRecovery for each token of a failed login; the failed-login golden files assert the whole text",
	"token.revokedClause":                      "the ext branch is the operator's own token and the command runs as that operator; the v3 branch names the administrator",
	"token.samlAuth":                           "the literal is the error for a failed build of the delete request in the SAML sign-in; it names no token",
	"token.signInRevokedClause":                "the clause of a cancelled or refused sign-in; its command and person come from signInLeftover through freshToken.leftover, whose rows (the cancelled-wait rows) assert them for a named ext token and a named v3 token; a token that was revoked, or that has no name, gets no command",
	"token.tokenCmd.pluginLogin":               "builds the warning for the plugin-created login only through LoginTokenWarning and signInLeftover, whose rows assert the noun, the command and the person",
	"token.v3Remedy":                           "the text for a v3 token the CLI cannot delete, used for a superseded token and for a token of a delete plan, built through v3AdminDelete, which names the administrator",
}

// leftoverTestRow names the rows of a message that package token cannot
// build, because its builder is not exported: a test of dir, a function named
// test, asserts it. The test has one row for each entry of persons, a
// composite literal that holds the builder's key and that person as whole
// string literals; an entry that is listed twice needs two rows. A person is
// the words that name who runs the command, written in the test and never read
// from a constant of the code.
type leftoverTestRow struct {
	dir     string
	test    string
	persons []string
}

// leftoverRowsInTests has the rows of the messages built in internal/config
// and internal/commands/configcmd. Keys are package.Function or
// package.Receiver.Method.
var leftoverRowsInTests = map[string]leftoverTestRow{
	"config.noServerText":   {"../config", "TestRecordTextsNameTheTokenTheCommandAndWhoRunsIt", []string{"user 'u-abc12'", "its owner or an administrator"}},
	"config.TokenStaysText": {"../config", "TestRecordTextsNameTheTokenTheCommandAndWhoRunsIt", []string{"user 'u-abc12'", "its owner or an administrator"}},
	"configcmd.administratorLine": {"../commands/configcmd", "TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt",
		[]string{"an administrator logged in to"}},
	"configcmd.recordLines": {"../commands/configcmd", "TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt",
		[]string{"an administrator logged in to", "log in as user 'u-abc12' (the token's owner), then revoke it with:",
			"log in as the token's owner or an administrator, then revoke it with:",
			"user 'u-abc12'", "user 'u-abc12'", "its owner or an administrator", "its owner or an administrator"}},
	"configcmd.recoveryField": {"../commands/configcmd", "TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt",
		[]string{"an administrator logged in to", "log in as user 'u-abc12' (the token's owner), then run:",
			"log in as the token's owner or an administrator, then run:",
			"user 'u-abc12'", "user 'u-abc12'", "its owner or an administrator", "its owner or an administrator"}},
	"configcmd.sequenceLines": {"../commands/configcmd", "TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt",
		[]string{"log in as user 'u-abc12' (the token's owner), then revoke it with:", "log in as the token's owner or an administrator, then revoke it with:"}},
}

// testRowsHolding counts the composite literals in the body of the test
// function name, in the test files of dir, that hold every one of texts as a
// whole string literal among their elements: the rows of a table. A literal
// inside a row's own elements (a call's argument) is not an element.
func testRowsHolding(t *testing.T, dir, name string, texts ...string) (rows int, testExists bool) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, path := range paths {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			testExists = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				held := map[string]bool{}
				for _, e := range lit.Elts {
					if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
						if s, err := strconv.Unquote(b.Value); err == nil {
							held[s] = true
						}
					}
				}
				if slices.ContainsFunc(texts, func(text string) bool { return !held[text] }) {
					return true
				}
				rows++
				return true
			})
		}
	}
	return rows, testExists
}

// leftoverMessageCalls are the builders every message about a token the CLI
// could not delete goes through, and the two that build the recovery commands
// and the advice for a token record.
var leftoverMessageCalls = []string{"signInLeftover", "v3AdminRemedy", "v3Remedy", "v3AdminDelete", "Revoke", "RecoverySequence", "AdviseRecord", "LoginTokenWarning", "ResidualRecovery"}

// leftoverMessageWords are the words of a message about a token the CLI could
// not delete, or about the command that deletes it: a function that holds a
// string literal with one of them is a message builder.
var leftoverMessageWords = []string{"could not be revoked", "was not revoked", "revoke it", "delete token", "delete management-token"}

// foldedLiteral is the text of a string literal or a + chain of them, a
// placeholder for every operand that is not a literal.
func foldedLiteral(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return foldedLiteral(x.X) + foldedLiteral(x.Y)
		}
	case *ast.ParenExpr:
		return foldedLiteral(x.X)
	}
	return "\x00"
}

// leftoverMessageDirs are the directories, relative to internal/token, whose
// non-test source builds a message about a token the CLI could not delete:
// every production caller of the builders is in one of them.
var leftoverMessageDirs = []string{".", "../commands/auth", "../commands/configcmd", "../clierr", "../config"}

// leftoverMessageBuilders lists, from the source of leftoverMessageDirs, every
// function that calls one of leftoverMessageCalls (by name, bare or through a
// package, also from a function literal inside it) or holds a string literal
// with one of leftoverMessageWords, with + chains joined. The selection does
// not see a command written by hand, in a function that calls none of the
// builders and holds none of the words, and it reads function bodies only: a
// text held in a package-level constant or variable is not listed. Details
// lines (fateLine, directWaitDetails.TextDetails) say what became of a token
// and carry no remedy; they are not messages in this sense.
func leftoverMessageBuilders(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range leftoverMessageDirs {
		for _, file := range parseNonTestFiles(t, dir) {
			for _, d := range file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				selected := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						name := ""
						switch f := x.Fun.(type) {
						case *ast.Ident:
							name = f.Name
						case *ast.SelectorExpr:
							name = f.Sel.Name
						}
						if slices.Contains(leftoverMessageCalls, name) {
							selected = true
						}
					case *ast.BasicLit, *ast.BinaryExpr:
						text := foldedLiteral(n.(ast.Expr))
						if slices.ContainsFunc(leftoverMessageWords, func(word string) bool { return strings.Contains(text, word) }) {
							selected = true
						}
					}
					return !selected
				})
				if selected {
					out = append(out, file.Name.Name+"."+funcKey(fn))
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestEveryLeftoverTokenMessageBuilderOfEveryPackageThatBuildsOneHasARowOrAReason
// asserts each function of leftoverMessageDirs that builds or calls the
// builders of a message about a token the CLI could not delete is in the
// covers of a row of the message table, in leftoverRowsInTests or in
// leftoverNotMessages with its reason; that no name in any of them is stale;
// and that no name is in two. A new function that says a token could not be
// revoked, and names a command, fails until it has a row or a reason. The
// selection reads function bodies only and finds a command by the builders
// it calls and the words of leftoverMessageWords: a command written by hand
// in a function with neither is not found. A leftoverRowsInTests entry needs
// one row of its test for each person it lists.
func TestEveryLeftoverTokenMessageBuilderOfEveryPackageThatBuildsOneHasARowOrAReason(t *testing.T) {
	selected := leftoverMessageBuilders(t)
	covered := map[string]bool{}
	for _, row := range leftoverRows(t) {
		for _, key := range row.covers {
			covered[key] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(covered)) {
		assert.Contains(t, selected, key, "a row covers %s, which no function of the packages is: the row's covers is stale", key)
		_, reason := leftoverNotMessages[key]
		assert.False(t, reason, "%s is covered by a row and has a reason: keep one", key)
		_, inTests := leftoverRowsInTests[key]
		assert.False(t, inTests, "%s is covered by a row and by a test of another package: keep one", key)
	}
	for key, reason := range leftoverNotMessages {
		assert.Contains(t, selected, key, "%s has a reason and is no builder of the packages: the entry is stale", key)
		assert.NotEmpty(t, reason, "%s: a function with no row carries its reason", key)
		_, inTests := leftoverRowsInTests[key]
		assert.False(t, inTests, "%s has a reason and a test of another package: keep one", key)
	}
	for key, row := range leftoverRowsInTests {
		assert.Contains(t, selected, key, "%s has a row in a test and is no builder of the packages: the entry is stale", key)
		assert.NotEmpty(t, row.persons, "%s: a row names the person who runs the command", key)
		wanted := map[string]int{}
		for _, person := range row.persons {
			wanted[person]++
		}
		for person, want := range wanted {
			rows, exists := testRowsHolding(t, row.dir, row.test, key, person)
			require.True(t, exists, "%s: the test %s does not exist in %s", key, row.test, row.dir)
			assert.GreaterOrEqual(t, rows, want, "%s: the test %s has %d rows that hold the key and the person %q as string literals, %d are listed", key, row.test, rows, person, want)
		}
	}
	for _, key := range selected {
		_, reason := leftoverNotMessages[key]
		_, inTests := leftoverRowsInTests[key]
		assert.True(t, covered[key] || reason || inTests, "%s builds a message about a token the CLI could not delete: add a row of the message table that covers it, a row of leftoverRowsInTests, or a reason in leftoverNotMessages", key)
	}
}

// leftoverCallersElsewhere lists the directories outside leftoverMessageDirs
// that use an exported builder of leftoverMessageCalls, each with the
// reason its texts need no row. Keys are directories relative to the
// repository root, with forward slashes.
var leftoverCallersElsewhere = map[string]string{
	"internal/engine/extres": "tokenRecovery names the delete of the token a create command made and recorded, as the way to create it again; the token is the command's own result, not one the CLI could not delete, and the tests of extres assert the text",
}

// TestNoPackageOutsideTheScannedOnesCallsAMessageBuilder asserts that every
// use of an exported builder of leftoverMessageCalls (a call or a function
// value, through a package name or a dot import) in the non-test source of
// internal/ and cmd/ sits in a directory the selection reads, or in
// leftoverCallersElsewhere with a reason, and that no entry of the list is
// stale. A builder called from a new package builds a text the selection
// would never see. Matching is by name only: a field or method with a
// builder's name, or any identifier of that name in a file with a dot
// import, counts as a use too.
func TestNoPackageOutsideTheScannedOnesCallsAMessageBuilder(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	scanned := map[string]bool{}
	for _, dir := range leftoverMessageDirs {
		abs, err := filepath.Abs(dir)
		require.NoError(t, err)
		scanned[abs] = true
	}
	var exported []string
	for _, name := range leftoverMessageCalls {
		if ast.IsExported(name) {
			exported = append(exported, name)
		}
	}
	callers := map[string][]string{}
	for _, top := range []string{"internal", "cmd"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || scanned[filepath.Dir(path)] {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			dotImport := slices.ContainsFunc(file.Imports, func(s *ast.ImportSpec) bool { return s.Name != nil && s.Name.Name == "." })
			ast.Inspect(file, func(n ast.Node) bool {
				var name string
				switch x := n.(type) {
				case *ast.SelectorExpr:
					name = x.Sel.Name
				case *ast.Ident:
					if dotImport {
						name = x.Name
					}
				}
				if slices.Contains(exported, name) {
					rel, err := filepath.Rel(root, filepath.Dir(path))
					require.NoError(t, err)
					dir := filepath.ToSlash(rel)
					callers[dir] = append(callers[dir], name)
				}
				return true
			})
			return nil
		}))
	}
	for dir, names := range callers {
		_, reason := leftoverCallersElsewhere[dir]
		assert.True(t, reason, "%s uses %v, builders of a message about a token the CLI could not delete, and the selection does not read it: add it to leftoverMessageDirs, or give it a reason in leftoverCallersElsewhere", dir, slices.Compact(slices.Sorted(slices.Values(names))))
	}
	for dir, reason := range leftoverCallersElsewhere {
		assert.Contains(t, callers, dir, "%s has a reason and uses no builder: the entry is stale", dir)
		assert.NotEmpty(t, reason, "%s: a directory the selection does not read carries its reason", dir)
	}
}
