package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// hostileUserID holds a direction override, a zero-width space, a bell, a
// newline and the first rune of the remedy marker followed by the word the
// marker ends with.
const hostileUserID = "u-\u202e\u200b\a\n\u2063rancher-x"

// sourceText is one text a source produces: an error, or a warning. names says
// the text names the id, so the escaped id must appear in it.
type sourceText struct {
	err     *clierr.Error
	warning string
	names   bool
}

// userIDSource is one place an id the CLI did not validate enters text: build
// plants id there, runs the code path and returns each text the path
// produces.
type userIDSource struct {
	name  string
	build func(t *testing.T, id string) []sourceText
}

// exactIDPaths are the JSON paths of the data fields that keep a user id as it
// is recorded. They are data, not text, so the walk leaves them out and hands
// their values back, for the test to assert each is the planted id.
var exactIDPaths = map[string]bool{
	"error.details.configRecovery.prior.tokenUserId": true,
	"error.details.notRevoked[].loginAs":             true,
}

// exactValue is the value the walk found at one of exactIDPaths.
type exactValue struct{ path, value string }

// walkStrings reports the JSON path of every string under v that holds a rune
// the terminal hides, once the remedy marker of this process is read as the
// word it stands for. A string at one of exactIDPaths is not walked; it is
// appended to exact.
func walkStrings(v any, path string, out, all *[]string, exact *[]exactValue) {
	switch x := v.(type) {
	case string:
		if exactIDPaths[path] {
			*exact = append(*exact, exactValue{path, x})
			return
		}
		text := strings.ReplaceAll(x, clierr.Rancher, "rancher")
		*all = append(*all, text)
		if strings.IndexFunc(text, clierr.IsUnsafeToDisplay) >= 0 {
			*out = append(*out, path)
		}
	case map[string]any:
		for k, e := range x {
			walkStrings(e, path+"."+k, out, all, exact)
		}
	case []any:
		for _, e := range x {
			walkStrings(e, path+"[]", out, all, exact)
		}
	}
}

// textOfEnvelope renders one error, or one warning, in the JSON envelope,
// decodes it, and returns every string under error.message, error.hint,
// error.details and warnings, with the paths of those that hold a hidden rune.
// When the error's details project onto text lines of their own, which the
// JSON envelope never holds, each line is checked as a text too. The values of
// the data fields in exactIDPaths come back as exact.
func textOfEnvelope(t *testing.T, st sourceText) (hidden, all []string, exact []exactValue) {
	t.Helper()
	var env *output.Envelope
	if st.err != nil {
		env = output.ErrorEnvelope(st.err)
	} else {
		env = output.NewEnvelope("Result", map[string]any{})
		env.Warnings = []string{st.warning}
	}
	got := renderAndRead(t, "json", env)
	if e, ok := got.Data["error"].(map[string]any); ok {
		for _, k := range []string{"message", "hint", "details"} {
			walkStrings(e[k], "error."+k, &hidden, &all, &exact)
		}
	}
	if st.err != nil {
		if td, ok := st.err.Details.(clierr.TextDetails); ok {
			for _, l := range td.TextDetails() {
				walkStrings(l, "error.details text line", &hidden, &all, &exact)
			}
		}
	}
	for _, w := range got.Warnings {
		walkStrings(w, "warnings[]", &hidden, &all, &exact)
	}
	return hidden, all, exact
}

// refused runs the exec plugin against a stored login of alice, on an entry
// that records id as the login's owner, the fixture Rancher answering as opts
// says, and returns the error the call fails with.
func refused(t *testing.T, opts testrancher.Options, id string, c authCall) *clierr.Error {
	t.Helper()
	x := newOwnerFixture(t, false, opts)
	x.entry("prod", "alice", id, false)
	_, _, err := x.callWith(c)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	return ce
}

// shimRefused runs the legacy shim, which names no --user-id, against a stored
// seed login of an entry that records id as the owner, with the token create
// answered 403, and returns the error. cliOwned gives the entry the token ref
// of a login the CLI made.
func shimRefused(t *testing.T, id string, cliOwned bool) *clierr.Error {
	t.Helper()
	x := newOwnerFixture(t, false, testrancher.Options{DerivedCreateStatus: http.StatusForbidden, SeedToken: "seed:secret"})
	x.entry("prod", "", id, false)
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	s := cfg.Servers["prod"]
	s.Provenance = clierr.ProvenanceUserSupplied
	if cliOwned {
		s.Provenance, s.TokenRef = clierr.ProvenanceDerived, &TokenRef{Kind: TokenKindExt, Name: "token-seed"}
	}
	cfg.Servers["prod"] = s
	require.NoError(t, cfg.Write())
	require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.srv.URL}, "seed:secret"))
	cmd := tokenCmd{dir: x.dir, store: x.store, anchor: anchorFor(x.dir)}
	_, err = cmd.runWithParams(t.Context(), runParams{Server: x.srv.URL, legacyShim: true}, localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	return ce
}

// userIDSources has one row for each source of an id the CLI did not validate
// in this package. A text that a path produces only through a direct call of
// its builder is checked for that builder alone; the rows that run get-token
// check the paths from the command.
func userIDSources() []userIDSource {
	const url = "https://r.example.com"
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	freshOf := func(created CreateResult) *freshToken {
		return &freshToken{create: created, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultRevoked} }}
	}
	asError := func(t *testing.T, err error) *clierr.Error {
		t.Helper()
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		return ce
	}
	return []userIDSource{
		{"config.yaml owner, exec plugin refusal of the call's user", func(t *testing.T, id string) []sourceText {
			t.Helper()
			ce := refused(t, testrancher.Options{}, id, authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice"})
			return []sourceText{{err: ce, names: true}}
		}},
		{"config.yaml owner, exec plugin 403 for a cluster", func(t *testing.T, id string) []sourceText {
			t.Helper()
			ce := refused(t, testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, id, authCall{cluster: "c-test"})
			return []sourceText{{err: ce, names: false}}
		}},
		{"config.yaml owner, legacy shim 403 for Rancher itself, pasted token", func(t *testing.T, id string) []sourceText {
			t.Helper()
			return []sourceText{{err: shimRefused(t, id, false), names: true}}
		}},
		{"config.yaml owner, legacy shim 403 for Rancher itself, CLI-made login", func(t *testing.T, id string) []sourceText {
			t.Helper()
			return []sourceText{{err: shimRefused(t, id, true), names: true}}
		}},
		{"config.yaml owner, refusal builder", func(_ *testing.T, id string) []sourceText {
			u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url}
			plugin := config.Server{URL: url, Provenance: clierr.ProvenanceExecLogin}
			cfg := &config.Config{Servers: map[string]config.Server{"exec-login-r.example.com": plugin, "prod": {URL: url}}}
			return []sourceText{
				{err: u.entryOwnerRefused(cfg, "prod", id, false, true), names: true},
				{err: u.entryOwnerRefused(cfg, "exec-login-r.example.com", id, true, true), names: true},
				{err: u.entryOwnerRefused(cfg, "exec-login-r.example.com", id, true, false), names: true},
			}
		}},
		{"config.yaml owner, grant text builder", func(_ *testing.T, id string) []sourceText {
			return []sourceText{
				{err: clierr.New(clierr.CodeRBAC, noTokenPermissionTextOf(id, "prod", true)), names: true},
				{err: clierr.New(clierr.CodeRBAC, noTokenPermissionTextOf(id, "", false)), names: true},
			}
		}},
		{"config.yaml owner, next login warning", func(_ *testing.T, id string) []sourceText {
			d := textSafetyDeletion(func(p string) string {
				if p == "TokenUserID" {
					return id
				}
				return "<" + p + ">"
			})
			var out []sourceText
			for _, w := range d.Warnings() {
				out = append(out, sourceText{warning: w, names: strings.Contains(w, "the next ")})
			}
			for _, l := range d.TextLines() {
				out = append(out, sourceText{warning: l, names: strings.Contains(l, "the next ")})
			}
			return append(out, sourceText{err: clierr.New(clierr.CodeGeneric, "not every token was revoked").WithDetails(d.Report()), names: false})
		}},
		{"superseded record owner, kept warning", func(t *testing.T, id string) []sourceText {
			t.Helper()
			out := []sourceText{
				{warning: keptWarning(config.SupersededToken{Ref: *ref, UserID: id}, url, "the server refused"), names: true},
				{warning: keptWarning(config.SupersededToken{Ref: *ref, UserID: id, Server: "not a url"}, "not a url", "the server refused"), names: true},
			}
			x := newRetireFixture(t, testrancher.Options{})
			other, fresh := x.credAs(t, "b"), x.credAs(t, "a")
			rec := record(other, "2026-09-02T00:00:00Z")
			rec.UserID = id
			x.seedRecords(t, []config.SupersededToken{rec})
			for _, w := range RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t)).Warnings {
				out = append(out, sourceText{warning: w, names: true})
			}
			return out
		}},
		{"record owner, manual revoke warning", func(_ *testing.T, id string) []sourceText {
			cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
			d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
			var out []sourceText
			for _, it := range []*revocationItem{
				{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), owner: id, entry: "prod", reason: reasonFailed},
				{source: sourceCache, ref: ref, owner: id, reason: reasonNoEntry},
				{source: sourceCache, ref: ref, server: "not a url", owner: id, entry: "prod", reason: reasonFailed},
			} {
				out = append(out, sourceText{warning: d.manualWarning(it), names: true})
			}
			return out
		}},
		{"record owner, recovery advice", func(t *testing.T, id string) []sourceText {
			t.Helper()
			var out []sourceText
			unusableEntry := "https://user:pw@r.example.com"
			for _, c := range []struct {
				site   config.RecordSite
				server string
			}{
				{config.RecordSite{Entry: "prod", EntryURL: url}, ""},
				{config.RecordSite{Entry: "prod", EntryURL: url}, url},
				{config.RecordSite{Entry: "prod", EntryURL: url}, "not a url"},
				{config.RecordSite{EntryURL: unusableEntry}, unusableEntry},
			} {
				a := config.AdviseRecord(c.site, c.server, id, *ref)
				if a.Text != "" {
					out = append(out, sourceText{warning: a.Text, names: true})
				}
				if a.LoginAs != "" {
					assert.Equal(t, id, a.LoginAs, "the data field keeps the id exact")
				}
			}
			return out
		}},
		{"token create owner", func(t *testing.T, id string) []sourceText {
			t.Helper()
			const name = "exec-login-r.example.com"
			created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, UserID: id}
			cfg := &config.Config{Servers: map[string]config.Server{name: {URL: url, Provenance: clierr.ProvenanceExecLogin}, "prod": {URL: url}}}
			u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url}
			var out []sourceText
			for _, sc := range []storedCredential{{name: "prod", entry: cfg.Servers["prod"]}, {name: name, entry: cfg.Servers[name]}} {
				out = append(out, sourceText{err: asError(t, u.checkCreatedOwner(t.Context(), cfg, created, freshOf(created), sc)), names: true})
			}
			return out
		}},
		{"cluster token create owner", func(_ *testing.T, id string) []sourceText {
			var out []sourceText
			for name, f := range signInLeftoverFates() {
				// A v3 token is an administrator's to delete and the SAML
				// text names no user.
				named := name != "v3 with a name" && name != "marked v3" && !strings.HasPrefix(name, "the SAML")
				out = append(out, sourceText{warning: signInLeftover(f, id, "m"), names: named})
			}
			return out
		}},
		{"session owner, 403 login", func(t *testing.T, id string) []sourceText {
			t.Helper()
			opts := testrancher.Options{
				Users:                 []testrancher.User{{Name: "e2e-user", Password: "e2e-pass", UserID: id}},
				LegacyLoginBodyUserID: true,
			}
			ce, _ := forbiddenAcquire(t, opts, "prod")
			return []sourceText{{err: ce, names: true}}
		}},
		{"sign-in owner", func(t *testing.T, id string) []sourceText {
			t.Helper()
			u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url}
			created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, UserID: id}
			return []sourceText{
				{err: asError(t, u.checkSignInOwner(t.Context(), nil, url, created, freshOf(created))), names: true},
				{err: u.pluginSignInRefused(id), names: true},
			}
		}},
		{"prior entry of an auth login failure", func(_ *testing.T, id string) []sourceText {
			d := clierr.LoginFailureDetails{
				Primary: clierr.PrimaryFailure{Code: clierr.CodeGeneric, Message: "the config could not be written"},
				Stage:   clierr.StageClusterToken,
				ConfigRecovery: &clierr.ConfigRecovery{Entry: "prod", Command: "c", Prior: &clierr.PriorEntry{
					URL: url, TokenRef: ref, TokenUserID: id}},
				Recovery: "r",
			}
			return []sourceText{{err: clierr.New(clierr.CodeGeneric, "login failed").WithDetails(d), names: true}}
		}},
		{"owner of a token that may still be live, revocation report", func(_ *testing.T, id string) []sourceText {
			report := RevocationReport{NotRevoked: []NotRevokedToken{
				{Ref: ref, Source: sourceCache, Server: url, Reason: reasonFailed, Retry: "r", LoginAs: id},
				{Source: sourceCache, Server: url, Reason: reasonNoEntry, Retry: "r", LoginAs: id},
			}}
			return []sourceText{{err: clierr.New(clierr.CodeGeneric, "not every token was revoked").WithDetails(report), names: false}}
		}},
		{"SelfSubjectReview user name", func(t *testing.T, id string) []sourceText {
			t.Helper()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"userInfo": map[string]any{"username": id}}})
			}))
			t.Cleanup(srv.Close)
			u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: srv.URL}
			created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, Bearer: "b"}
			return []sourceText{{err: asError(t, u.checkSignInOwner(t.Context(), srv.Client(), srv.URL, created, freshOf(created))), names: true}}
		}},
	}
}

// TestAnIdTheCLIDidNotValidateReachesNoTextWithAHiddenRune asserts that, for
// every source of an unvalidated id in this package, an id holding a direction
// override, a zero-width space, a bell, a newline and the start of a remedy
// marker reaches no string of error.message, error.hint, error.details or
// warnings in the JSON envelope, and no text line the error's details project
// for the terminal, with a rune the terminal hides; that each text that names
// the id shows it with escapes; and that no text holds the id with its hidden
// runes removed, which would read as another id. Each text is checked on its
// own, so a strip put back at one of several builders of a row, or added at a
// new site, fails that row. The data fields at exactIDPaths are not walked as
// text; each must hold the id exactly.
func TestAnIdTheCLIDidNotValidateReachesNoTextWithAHiddenRune(t *testing.T) {
	for _, src := range userIDSources() {
		t.Run(src.name, func(t *testing.T) {
			texts := src.build(t, hostileUserID)
			require.NotEmpty(t, texts, "the source produced no text")
			want, stripped := strconv.Quote(hostileUserID), clierr.WithoutUnsafeToDisplay(hostileUserID)
			for i, st := range texts {
				hidden, all, exact := textOfEnvelope(t, st)
				for _, e := range exact {
					assert.Equal(t, hostileUserID, e.value, "text %d: the data field %s keeps the id exact", i, e.path)
				}
				joined := strings.Join(all, "\n")
				assert.Empty(t, hidden, "text %d: paths whose text holds a hidden rune", i)
				assert.NotContains(t, joined, stripped, "text %d holds the id with its hidden runes removed", i)
				if st.names {
					assert.Contains(t, joined, want, "text %d shows the exact id with escapes", i)
				}
			}
		})
	}
}

// TestTheConfigStatusPointerIsGivenOnlyForAnEntryStatusLists asserts the grant
// text for a recorded owner that is not a Rancher user id points at config
// status only for a login the CLI made, whose entry that command lists, and
// gives the escaped id alone for a pasted token, both through the legacy shim
// command and through the builder. A call for Rancher itself with --user-id
// holds the recorded owner equal to that id, so only the legacy shim, which
// names no --user-id, reaches this text with a recorded owner that is not a
// Rancher user id.
func TestTheConfigStatusPointerIsGivenOnlyForAnEntryStatusLists(t *testing.T) {
	const shimPointer = "the tokenUserId of server entry 'prod' in: "
	cli := shimRefused(t, hostileUserID, true)
	pastedShim := shimRefused(t, hostileUserID, false)
	assert.Contains(t, cli.Message, shimPointer)
	assert.NotContains(t, pastedShim.Message, shimPointer)
	assert.NotContains(t, pastedShim.Message, "config status")
	url := "https://r.example.com"
	owned := storedCredential{name: "prod", entry: config.Server{URL: url, Provenance: clierr.ProvenanceDerived, TokenUserID: hostileUserID,
		TokenRef: &TokenRef{Kind: TokenKindExt, Name: "token-seed"}}}
	pasted := storedCredential{name: "prod", entry: config.Server{URL: url, Provenance: clierr.ProvenanceUserSupplied, TokenUserID: hostileUserID}}
	const pointer = "the tokenUserId of server entry 'prod' in: "
	text := func(sc storedCredential) string {
		return strings.ReplaceAll(noTokenPermissionTextOf(sc.entry.TokenUserID, configStatusEntry(sc), true), clierr.Rancher, "rancher")
	}
	assert.Contains(t, text(owned), pointer+"rancher config status -o json")
	assert.NotContains(t, text(pasted), pointer)
	assert.NotContains(t, text(pasted), "config status")
	for _, sc := range []storedCredential{owned, pasted} {
		assert.Contains(t, text(sc), strconv.Quote(hostileUserID))
	}
}

// TestAnEmailStyleUserNameKeepsItsTextAndGetsNoUserIDCommand asserts an id the
// terminal shows unchanged but that is not a Rancher user id (an email-style
// username) stays as typed in the text and builds no command that takes a user
// id.
func TestAnEmailStyleUserNameKeepsItsTextAndGetsNoUserIDCommand(t *testing.T) {
	t.Parallel()
	const id = "alice@example.com"
	assert.Equal(t, id, clierr.IDText(id))
	text := noTokenPermissionTextOf(id, "prod", true)
	assert.Contains(t, text, "the user id 'alice@example.com' is not a Rancher user id")
	assert.NotContains(t, strings.ReplaceAll(text, clierr.Rancher, "rancher"), "grant 'alice@example.com'")
	assert.Contains(t, text, "grant <user-id>")
	ce := userCheck{userID: "u-abc12"}.entryOwnerRefused(&config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}}, "prod", id, false, true)
	assert.Contains(t, ce.Message, "Rancher user 'alice@example.com'")
}

// TestEveryUserIDFieldHasASourceRowOrAReason asserts that every struct field
// named like a user id, an owner, a user name or a login-as user in internal/token,
// internal/config and internal/clierr is listed here with the source row that
// plants an id in it or the reason it needs none; a new field fails until it is
// listed, and so does an entry that names no field. Limits: the scan reads
// struct fields of the three packages only; anonymous response structs and
// function results are not seen, and the engine packages are covered by their
// own source rows.
func TestEveryUserIDFieldHasASourceRowOrAReason(t *testing.T) {
	t.Parallel()
	type listing struct {
		rows   []string
		reason string
	}
	configRows := []string{
		"config.yaml owner, exec plugin refusal of the call's user",
		"config.yaml owner, exec plugin 403 for a cluster",
		"config.yaml owner, legacy shim 403 for Rancher itself, pasted token",
		"config.yaml owner, legacy shim 403 for Rancher itself, CLI-made login",
		"config.yaml owner, refusal builder",
		"config.yaml owner, grant text builder",
		"config.yaml owner, next login warning",
	}
	listed := map[string]listing{
		"token.AcquireError.SessionUserID":          {rows: []string{"session owner, 403 login"}},
		"token.CreateResult.UserID":                 {rows: []string{"token create owner", "cluster token create owner", "sign-in owner", "SelfSubjectReview user name"}},
		"token.noTerminalCall.userID":               {reason: "the call's own --user-id, checked by ValidUserID before any text is built"},
		"token.userCheck.userID":                    {reason: "the call's own --user-id, checked by ValidUserID before any text is built"},
		"token.revocationItem.owner":                {rows: []string{"record owner, manual revoke warning"}},
		"token.LoginCredential.UserID":              {reason: "reaches text only through the CreateResult and AcquireError rows"},
		"token.runParams.UserID":                    {reason: "the call's own --user-id, checked by ValidUserID before any text is built"},
		"config.CacheBinding.userID":                {reason: "the call's own --user-id, checked by ValidUserID before it is bound"},
		"config.CacheBinding.tokenUserID":           {reason: "the owner recorded for the binding; not placed in any text"},
		"config.entryInputRow.owner":                {reason: "the owner recorded for an entry; written to config.yaml only"},
		"config.CachedRecord.TokenUserID":           {rows: []string{"record owner, recovery advice"}},
		"config.Server.TokenUserID":                 {rows: configRows},
		"config.cachedCred.UserID":                  {reason: "the owner of a cached credential; not placed in any text"},
		"config.cachedCred.TokenUserID":             {reason: "the owner recorded for a cached credential; not placed in any text"},
		"config.CachedCredInfo.TokenUserID":         {reason: "the owner recorded for a cached credential; reaches text through the revocation rows"},
		"config.PersistLoginInput.TokenUserID":      {reason: "the owner a login records; written to config.yaml only"},
		"config.PersistLoginOutcome.ReplacedUserID": {reason: "text built in internal/commands/auth through clierr.IDText (replacedUserWarning), which has its own rows"},
		"clierr.SupersededToken.UserID":             {rows: []string{"superseded record owner, kept warning"}},
		"clierr.PriorEntry.TokenUserID":             {rows: []string{"prior entry of an auth login failure"}},
		"token.NotRevokedToken.LoginAs":             {rows: []string{"owner of a token that may still be live, revocation report"}},
		"config.RecordAdvice.LoginAs":               {rows: []string{"record owner, recovery advice"}},
	}
	rowNames := map[string]bool{}
	for _, src := range userIDSources() {
		rowNames[src.name] = true
	}
	cited := map[string]bool{}
	for field, l := range listed {
		assert.True(t, len(l.rows) > 0 || l.reason != "", "the field %s needs a source row or a reason", field)
		for _, r := range l.rows {
			assert.True(t, rowNames[r], "the field %s names the row %q, which is not a source row", field, r)
			cited[r] = true
		}
	}
	for name := range rowNames {
		assert.True(t, cited[name], "the source row %q is named by no field", name)
	}
	var found []string
	fset := gotoken.NewFileSet()
	for _, dir := range []string{".", "../config", "../clierr"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			require.NoError(t, err)
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						lower := strings.ToLower(name.Name)
						if strings.HasSuffix(lower, "userid") || strings.HasSuffix(lower, "loginas") || lower == "owner" || lower == "username" {
							found = append(found, fmt.Sprintf("%s.%s.%s", file.Name.Name, ts.Name.Name, name.Name))
						}
					}
				}
				return true
			})
		}
	}
	slices.Sort(found)
	for _, f := range found {
		assert.Contains(t, listed, f, "the field %s needs a source row or a reason", f)
	}
	for k := range listed {
		assert.Contains(t, found, k, "the entry %s names no field", k)
	}
}
