package token

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
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
// package's test files, holds the string literal text: a subtest named in a
// table field or in a t.Run call. The name of a subtest is a run-time string,
// so the source is the only place to look. That the subtest reaches the
// producer is not checked here.
func testHoldsString(t *testing.T, name, text string) (found, testExists bool) {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
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
		found, exists := testHoldsString(t, test, subtest)
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
// text. covers lists the message builders the row's text goes through, as
// package.Function or package.Receiver.Method.
type leftoverRow struct {
	name    string
	text    string
	noun    string
	person  string
	command string
	own     string
	whole   string
	covers  []string
}

// leftoverRows is one row for each kind of token that reaches each message.
func leftoverRows(t *testing.T) []leftoverRow {
	t.Helper()
	extRef := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	v3Ref := &TokenRef{Kind: TokenKindV3, Name: "token-v3"}
	browser := &SAMLHandoff{Provider: "saml", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:05:00Z"}
	failed := RevokeOutcome{Result: ResultFailed, Error: "500"}
	const (
		owner     = "user 'u-abc12' can delete it from a directory signed in as user 'u-abc12'"
		signedIn  = "the user who signed in can delete it from a directory signed in as that user"
		admin     = "an administrator can delete it sooner with: "
		deleteExt = "rancher delete token 'token-abc' --apply --yes"
		deleteV3  = "rancher delete management-token 'token-v3' --apply --yes --allow-deprecated"
		listV3    = "an administrator can inspect them with: rancher get management-tokens -o json"
		ownReason = "the operator's own token; the command runs as that operator"
		samlUI    = "the user who signed in can find them in the UI under Account & API Keys"
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
	readOnly := func(cred LoginCredential) string {
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
	search := FateOf(clierr.CreateKnown, TokenKindV3, nil, false, "", "", RevokeOutcome{}, browser).Search
	storedSAML := LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: v3Ref, Result: ResultFailed, Error: "500"}, SessionTokenUnmarked: true, SessionTokenSearch: browser}

	waitCovers := []string{"token.directWaitCancelled", "token.freshToken.leftover", "token.signInLeftover"}
	v3Covers := []string{"token.v3AdminRemedy", "token.v3AdminDelete"}
	return []leftoverRow{
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
		{name: "a read-only credential store, a SAML kubeconfig token", text: readOnly(storedSAML),
			noun: "token v3/token-v3", person: admin, command: deleteV3,
			covers: slices.Concat([]string{"token.FinishUnsupportedStore", "token.ResidualRecovery"}, v3Covers)},
		{name: "a read-only credential store, a v3 session token", text: readOnly(LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: v3Ref, Result: ResultFailed, Error: "500"}}),
			noun: "token v3/token-v3", person: admin, command: deleteV3,
			covers: slices.Concat([]string{"token.FinishUnsupportedStore", "token.ResidualRecovery"}, v3Covers)},
		{name: "a read-only credential store, an ext session token", text: readOnly(LoginCredential{Provenance: ProvenanceDerived, SessionTokenCleanup: RevokeOutcome{Ref: extRef, Result: ResultFailed, Error: "500"}}),
			noun: "token ext/token-abc", command: deleteExt, own: ownReason,
			whole:  "the token ext/token-abc created by this login could not be revoked (500); no credential was stored; log in again with: rancher auth login 'prod'; then revoke ext/token-abc with: rancher delete token 'token-abc' --apply --yes",
			covers: []string{"token.FinishUnsupportedStore", "token.ResidualRecovery"}},
	}
}

// TestEveryLeftoverTokenMessageOfThreePackagesNamesTheTokenTheCommandAndWhoRunsIt
// asserts, for each message about a token the CLI could not delete and each
// kind of token that reaches it, the token (its noun or its name), the
// command, and the person who can run the command when that person is not the
// operator who signed in. A message that names no person (the operator's own
// ext token) is asserted whole. The rows are the cancelled wait of a fresh
// token, the warning of auth login for the token it created, the recovery of a
// failed login, the recovery of a login whose store is read-only, and the text
// for a SAML token the CLI cannot name.
func TestEveryLeftoverTokenMessageOfThreePackagesNamesTheTokenTheCommandAndWhoRunsIt(t *testing.T) {
	for _, row := range leftoverRows(t) {
		t.Run(row.name, func(t *testing.T) {
			text := clierr.ExpandRemedies(row.text, "")
			assert.Contains(t, text, row.noun)
			assert.Contains(t, text, row.command)
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
	"auth.loginCmd.authenticateAndStore": "builds the warning of auth login through LoginTokenWarning and ResidualRecovery, covered by the rows of those two for an ext, a v3 and a SAML kubeconfig token; login_test.go asserts the warning's noun and command for a v3 session token and a SAML kubeconfig token, and that the warning of an ext session token says it could not be revoked",
	"clierr.RecoverySequence":            "a list of commands to run in order, not a message about a token",
	"token.Deletion.manualWarning":       "the delete plan's warning for a token it could not revoke; a token whose local copy was not deleted gets the command to run again; otherwise its v3 branches name the administrator, and its ext sequence names the user to log in as only when the plan records the owner",
	"token.Deletion.recovery":            "returns the commands of a deletion for the result data, not a message",
	"token.RetireSuperseded":             "the literal is the reason keptWarning wraps; the message itself is built by keptWarning, v3Remedy and unusableServerKept",
	"token.clusterSignIn.warning":        "the sign-in's warning for its login token, built only through LoginTokenWarning and signInLeftover, whose rows assert the noun, the command and the person",
	"token.deleteTokenCmd.Execute":       "the literal is the warning for an unreadable cache; no token is known to name",
	"token.keptWarning":                  "the retirement's warning for a superseded token that was kept; its v3 branch names the administrator, and its ext sequence names the user to log in as only when the owner is recorded",
	"token.recoveryHint":                 "returns the hint command only, not a message",
	"token.recoveryText":                 "joins the recovery of ResidualRecovery for each token of a failed login; the failed-login golden files assert the whole text",
	"token.revokedClause":                "the ext branch is the operator's own token and the command runs as that operator; the v3 branch names the administrator",
	"token.signInRecovery":               "the recovery of a failed sign-in; its command and person come from signInLeftover, whose rows cover a named token and a SAML token the CLI cannot name (the text says how to search for it), not a token that carries the CLI's marker but has no name",
	"token.signInRevokedClause":          "the clause of a cancelled or refused sign-in; its command and person come from signInLeftover through freshToken.leftover, whose rows (the cancelled-wait rows) assert them for a named ext token and a named v3 token; a token that was revoked, or that has no name, gets no command",
	"token.tokenCmd.pluginLogin":         "builds the warning for the plugin-created login only through LoginTokenWarning and signInLeftover, whose rows assert the noun, the command and the person",
	"token.v3Remedy":                     "the text for a v3 token the CLI cannot delete, used for a superseded token and for a token of a delete plan, built through v3AdminDelete, which names the administrator",
}

// leftoverMessageCalls are the builders every message about a token the CLI
// could not delete goes through.
var leftoverMessageCalls = []string{"signInLeftover", "v3AdminRemedy", "v3Remedy", "v3AdminDelete", "RecoveryCommands", "LoginTokenWarning", "ResidualRecovery"}

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

// leftoverMessageBuilders lists, from the source of internal/token,
// internal/commands/auth and internal/clierr, every function that calls one of
// leftoverMessageCalls (by name, bare or through a package, also from a
// function literal inside it) or holds a string literal that says a token
// could not be revoked or was not revoked, with + chains joined. Texts built
// in internal/commands/configcmd and internal/config are not listed. Details
// lines (fateLine, directWaitDetails.TextDetails) say what became of a token
// and carry no remedy; they are not messages in this sense.
func leftoverMessageBuilders(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{".", "../commands/auth", "../clierr"} {
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
						if strings.Contains(text, "could not be revoked") || strings.Contains(text, "was not revoked") {
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

// TestEveryLeftoverTokenMessageBuilderOfThreePackagesHasARowOrAReason asserts
// each function of internal/token, internal/commands/auth and internal/clierr
// that builds or calls the builders of a message about a token the CLI could
// not delete is in the covers of a row of the message table or in
// leftoverNotMessages with its reason; that no name in either is stale; and
// that no name is in both. A new function that says a token could not be
// revoked, and names a command, fails until it has a row or a reason.
func TestEveryLeftoverTokenMessageBuilderOfThreePackagesHasARowOrAReason(t *testing.T) {
	selected := leftoverMessageBuilders(t)
	covered := map[string]bool{}
	for _, row := range leftoverRows(t) {
		for _, key := range row.covers {
			covered[key] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(covered)) {
		assert.Contains(t, selected, key, "a row covers %s, which no function of the three packages is: the row's covers is stale", key)
		_, reason := leftoverNotMessages[key]
		assert.False(t, reason, "%s is covered by a row and has a reason: keep one", key)
	}
	for key, reason := range leftoverNotMessages {
		assert.Contains(t, selected, key, "%s has a reason and is no builder of the three packages: the entry is stale", key)
		assert.NotEmpty(t, reason, "%s: a function with no row carries its reason", key)
	}
	for _, key := range selected {
		_, reason := leftoverNotMessages[key]
		assert.True(t, covered[key] || reason, "%s builds a message about a token the CLI could not delete: add a row of the message table that covers it, or a reason in leftoverNotMessages", key)
	}
}
