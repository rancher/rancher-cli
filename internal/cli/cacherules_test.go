package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The forms of entry x's URL the rows use. {url} and {host} are the stub's
// address. rulesUsable is the form an earlier build's records were written
// under: x is usable then, and a hand edit gives it its form for the change.
const (
	rulesOrdinary = "https://198.51.100.7"
	rulesRawAt    = "{url}/pq@x"
	rulesUserinfo = "https://u:p@{host}"
	rulesUsable   = "{url}/pq"
	// rulesOther is a second server: the stub has one address, so a record at
	// another server is one no request reaches.
	rulesOther = "https://198.51.100.9"
)

// rulesMisreadHost finds what no output holds for the entry of canaryURLMisread:
// the host read from it (admin:1234), the login name a recovery sequence
// builds from it, and the text after it. The stub's own address may hold any
// digits, so the port alone is no sign.
var rulesMisreadHost = regexp.MustCompile(`\badmin\b|admin:1234|recover-admin|CaNaRy|@x`)

// ruleWorld is the config a row starts from: usable entry y at the stub, with
// a login of owner e2e-admin and the current server, and entry x.
type ruleWorld struct {
	// x is x's URL form when the change runs; empty for no entry x.
	x string
	// owned records owner e2e-admin on x.
	owned bool
	// noY leaves y out.
	noY bool
	// login gives x the stored login the own-credential records are made from;
	// y holds none then.
	login bool
	// pointer is the current server: empty for y (x when there is no y), "none"
	// for an empty pointer.
	pointer string
	// xCA saves the stub's CA on x.
	xCA bool
	// yOwner is the owner y records, other than the user the stub's tokens
	// belong to.
	yOwner string
	// entries are more entries at the stub's server.
	entries []ruleEntry
}

// ruleEntry is an entry of a world: a name, a URL form and the owner it records.
type ruleEntry struct{ name, url, owner string }

// ruleRec is one cached token a row makes.
type ruleRec struct {
	// kind is the call: "server" (a --name that identifies no entry), "owner"
	// (a --user-id), "own" (--name x) or "ownY" (--name y).
	kind string
	// how is "call" (the plugin made it, so it is served), "earlier" (written
	// while x was usable and listing it, as an earlier build did) or "kept" (an
	// earlier record that a later call replaced, which moved it to its kept key).
	how string
	// other puts the record at another server, bound to an entry w there that
	// is usable when it is written and has an @ after the host when the change
	// runs.
	other bool
	// at is the server of a record that is neither the stub's nor w's.
	at string
	// v3 makes the record's token an unscoped v3 token, cached for no cluster.
	v3 bool
	// sameName gives the record the name token-cch01, as tokens of two servers
	// can have, and hand writes it as an earlier build's record, with no call.
	sameName bool
	hand     bool
}

// ruleWant is what a row requires. before and after are "S" (the exec plugin
// serves the token: a repeat of the call sends no request) or "M" (it does
// not). source is where the token can still be revoked after the change:
// "next(E)" (recorded on E for its next login), "rec(E)" (recorded on E as
// manual), "DEL(E)" (one DELETE with its own bearer, in the change itself
// or, with "@LA", in a later logout --all), "live" (the record stays under its lookup
// key), "kept" (the record stays under a kept key) or "once" (named only in
// the change's own result). listed names where the token shows: R (the
// change's result), St (config status), LT (auth list-tokens) and LA (a
// later logout --all).
type ruleWant struct {
	before, after string
	source        string
	listed        string
}

type ruleRow struct {
	id    string
	world ruleWorld
	recs  []ruleRec
	// act is the change; nil for none (the hand edit alone).
	act  []string
	want ruleWant
	// actFn builds the change from the run, when it names something only the run
	// knows (a shown key).
	actFn func(r *ruleRun) []string
	// editAs is the form the hand edit gives x as the change, for a row whose
	// change is the upgrade or the edit itself.
	editAs string
	// asIs writes the earlier records under the world as it is, with no hand
	// edit of x after them.
	asIs bool
	// all applies the want to every token of the row, not only the first.
	all bool
	// custom replaces the checks of the columns by the row's own extra checks:
	// the rows that repeat another row with the misread form of x's URL assert
	// only that nothing names or reaches the host read from it, and a row of
	// two tokens of one name asserts them together.
	custom bool
	// extra runs the checks only this row has, after the shared ones.
	extra func(t *testing.T, r *ruleRun)
}

// ruleRun is one row being run.
type ruleRun struct {
	t       *testing.T
	f       repairFixture
	row     ruleRow
	toks    []string
	keys    map[string]string
	actArgv []string
	// act is the result of the change; la of the logout --all that follows;
	// status of config status after the change; lt of auth list-tokens.
	act, la, status, lt string
	actReqs, laReqs     []repairRequest
}

func jsonOf(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout: %s", stdout)
	data, _ := env["data"].(map[string]any)
	return data
}

// jsonHolds reports whether any string in the decoded JSON holds s.
func jsonHolds(t *testing.T, stdout, s string) bool {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(stdout), &v), "stdout: %s", stdout)
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			return strings.Contains(x, s)
		case []any:
			return slices.ContainsFunc(x, walk)
		case map[string]any:
			return slices.ContainsFunc(slices.Collect(maps.Values(x)), walk)
		}
		return false
	}
	return walk(v)
}

// itemsNamed is the items of data[field] by the name of their token ref.
func itemsNamed(t *testing.T, stdout, field string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := jsonOf(t, stdout)[field].([]any)
	for _, n := range list {
		m, _ := n.(map[string]any)
		ref, _ := m["ref"].(map[string]any)
		if name, ok := ref["name"].(string); ok {
			out[name] = m
		}
	}
	return out
}

func (r *ruleRun) run(argv ...string) string {
	r.t.Helper()
	return r.f.run(r.t, "", argv...)
}

func (r *ruleRun) setX(url string) {
	r.t.Helper()
	cfg, err := config.Load(config.FilePath(r.f.dir))
	require.NoError(r.t, err)
	x := cfg.Servers["x"]
	x.URL = r.f.fill(url)
	cfg.Servers["x"] = x
	require.NoError(r.t, cfg.Write())
}

func callFor(kind string) []string {
	switch kind {
	case "owner":
		return []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--user-id", "e2e-admin"}
	case "own":
		return []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--name", "x"}
	case "ownY":
		return []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--name", "y"}
	}
	return []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--name", "prod-cluster"}
}

// tokenOf is the name of the token an ExecCredential holds.
func tokenOf(t *testing.T, stdout string) string {
	t.Helper()
	var cred struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &cred), "stdout: %s", stdout)
	name := strings.TrimPrefix(cred.Status.Token, "ext/")
	name, _, _ = strings.Cut(name, ":")
	return name
}

func (r *ruleRun) seedWorld(xURL string) {
	t, f, w := r.t, r.f, r.row.world
	owner := func(e config.Server) config.Server {
		if w.owned {
			e.TokenUserID = cacheListOwner
		}
		if w.xCA {
			e.CA = f.fill("{pem}")
		}
		return e
	}
	switch {
	case w.login:
		writeSecretStored(t, f.dir, secretStored{name: "x", url: rulesUsable, stubCA: true, token: true}, f.fill)
		require.NoError(t, config.NewFileStore(f.dir).SetToken(config.ServerRef{Name: "x", URL: f.srvURL + "/pq"}, secretBearer))
		cfg, err := config.Load(config.FilePath(f.dir))
		require.NoError(t, err)
		if !w.noY {
			// y records the owner of the tokens the stub creates and holds no login.
			y := cfg.Servers["x"]
			y.URL, y.TokenRef, y.TokenUserID, y.Provenance, y.CacheBindingID = f.srvURL, nil, cmp.Or(w.yOwner, cacheListOwner), "", "yy"
			cfg.Servers["y"] = y
		}
		r.addEntries(cfg)
		require.NoError(t, cfg.Write())
	case w.noY:
		cfg := &config.Config{Servers: map[string]config.Server{}}
		if xURL != "" {
			cfg.Servers["x"] = owner(config.Server{URL: f.fill(xURL)})
			cfg.CurrentServer = "x"
		}
		cfg.SetPath(config.FilePath(f.dir))
		require.NoError(t, cfg.Write())
	default:
		writeSecretStored(t, f.dir, secretStored{name: "y", url: "{url}", stubCA: true, token: true}, f.fill)
		require.NoError(t, config.NewFileStore(f.dir).SetToken(config.ServerRef{Name: "y", URL: f.srvURL}, secretBearer))
		cfg, err := config.Load(config.FilePath(f.dir))
		require.NoError(t, err)
		y := cfg.Servers["y"]
		y.TokenUserID = cmp.Or(w.yOwner, cacheListOwner)
		cfg.Servers["y"] = y
		if xURL != "" {
			cfg.Servers["x"] = owner(config.Server{URL: f.fill(xURL)})
		}
		r.addEntries(cfg)
		require.NoError(t, cfg.Write())
	}
	if w.pointer != "" {
		cfg, err := config.Load(config.FilePath(f.dir))
		require.NoError(t, err)
		cfg.CurrentServer = map[string]string{"none": ""}[w.pointer]
		require.NoError(t, cfg.Write())
	}
}

// addEntries adds the world's more entries to cfg.
func (r *ruleRun) addEntries(cfg *config.Config) {
	for i, e := range r.row.world.entries {
		cfg.Servers[e.name] = config.Server{URL: r.f.fill(e.url), TokenUserID: e.owner, CacheBindingID: "e" + string(rune('1'+i))}
	}
}

// recKey is the name and user a record's call has, its server and its key.
func (r *ruleRun) recKey(rec ruleRec) (name, user, server, key string) {
	name = "prod-cluster"
	switch rec.kind {
	case "owner":
		name, user = "", cacheListOwner
	case "own":
		name = "x"
	case "ownY":
		name = "y"
	}
	server = r.f.srvURL
	switch {
	case rec.other:
		server = rulesOther
	case rec.at != "":
		server = rec.at
	}
	return name, user, server, config.CacheKeyFor(name, config.CanonicalServerIdentity(server), r.clusterOf(rec), user, "")
}

// clusterOf is the cluster a record's call named: none for a v3 token, which
// the plan sends with its own bearer only when it is unscoped.
func (r *ruleRun) clusterOf(rec ruleRec) string {
	if rec.v3 {
		return ""
	}
	return "c-x"
}

// writeEarlier writes the record an earlier build wrote for rec, under the
// config as it is now, and returns the token's name.
func (r *ruleRun) writeEarlier(rec ruleRec, n int, token string) string {
	t, f := r.t, r.f
	loaded, err := config.Load(config.FilePath(f.dir))
	require.NoError(t, err)
	name, user, server, key := r.recKey(rec)
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch0" + string(rune('0'+n))}
	if rec.sameName {
		ref.Name = "token-cch01"
	}
	secret := "ext/" + ref.Name + ":" + token
	if rec.v3 {
		ref = &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3abc"}
		secret = "token-v3abc:" + token
	}
	cluster := r.clusterOf(rec)
	binding := config.CacheBindingForSeedOf(config.CanonicalDir(f.dir), loaded, name, server, cluster, user, "").WithCreatedToken(ref, cacheListOwner)
	require.NoError(t, config.NewFileStore(f.dir).SetCachedCred(key, secret, time.Now().Add(time.Hour), name, server, cluster, binding))
	return ref.Name
}

// setW gives the entry w at the other server its URL: usable, or with an @
// after the host.
func (r *ruleRun) setW(url string) {
	r.t.Helper()
	cfg, err := config.Load(config.FilePath(r.f.dir))
	require.NoError(r.t, err)
	cfg.Servers["w"] = config.Server{URL: url, CacheBindingID: "ww"}
	require.NoError(r.t, cfg.Write())
}

func (r *ruleRun) listTokens() string { return r.run("-o", "json", "auth", "list-tokens") }

// ltKeys is the shown key of each cached token in an auth list-tokens result.
func (r *ruleRun) ltKeys(stdout string) map[string]string {
	out := map[string]string{}
	var env struct {
		Data []struct {
			Key      string `json:"key"`
			TokenRef struct {
				Name string `json:"name"`
			} `json:"tokenRef"`
		} `json:"data"`
	}
	require.NoError(r.t, json.Unmarshal([]byte(stdout), &env), "stdout: %s", stdout)
	for _, row := range env.Data {
		out[row.TokenRef.Name] = row.Key
	}
	return out
}

// makeRecords makes the row's records and returns the tokens' names.
func (r *ruleRun) makeRecords() {
	row := r.row
	needsEdit := false
	hasOther := false
	for _, rec := range row.recs {
		needsEdit = needsEdit || (rec.how != "call" && !row.asIs)
		hasOther = hasOther || rec.other
	}
	xURL := row.world.x
	seedAs := xURL
	if xURL != "" && (needsEdit || row.world.login || row.editAs != "") {
		seedAs = rulesUsable
	}
	r.seedWorld(seedAs)
	if hasOther {
		r.setW(rulesOther + "/p")
	}
	var pending []ruleRec
	for i, rec := range row.recs {
		switch rec.how {
		case "call":
			r.toks = append(r.toks, tokenOf(r.t, r.run(callFor(rec.kind)...)))
		case "earlier", "kept":
			// An earlier build's record is the one the plugin writes while x is
			// usable and listed; the stub holds the token, so a revoke of it is
			// confirmed. A record the plugin cannot make here (an own credential
			// of x with no login, another server, a v3 token) is written by hand.
			if rec.kind == "own" || rec.other || rec.at != "" || row.asIs || rec.hand {
				r.toks = append(r.toks, r.writeEarlier(rec, i+1, "secret"))
			} else {
				r.toks = append(r.toks, tokenOf(r.t, r.run(callFor(rec.kind)...)))
			}
			if rec.how == "kept" {
				pending = append(pending, rec)
			}
		}
	}
	if seedAs != xURL && row.editAs == "" {
		r.setX(xURL)
	}
	if hasOther && needsEdit {
		r.setW(rulesOther + "/p@w")
	}
	for _, rec := range pending {
		if rec.other || rec.hand {
			rec.sameName = false
			r.writeEarlier(rec, 9, "replacement")
			continue
		}
		r.run(callFor(rec.kind)...)
	}
}

// read is the plugin's read of each record's lookup key, which deletes a
// record that is not served and names no unusable entry.
func (r *ruleRun) read() {
	cfg, err := config.Load(config.FilePath(r.f.dir))
	require.NoError(r.t, err)
	anchor := config.NewCacheAnchor(config.CanonicalDir(r.f.dir)).WithConfig(cfg)
	store := config.NewFileStore(r.f.dir)
	for _, rec := range r.row.recs {
		_, _, _, key := r.recKey(rec)
		_, _, _ = store.GetCachedCred(key, anchor)
	}
}

func (r *ruleRun) beforeCheck() {
	t, want := r.t, r.row.want
	switch want.before {
	case "S":
		mark := r.f.mark()
		out := r.run(callFor(r.row.recs[0].kind)...)
		assert.Empty(t, r.f.requestsSince(mark), "a served record sends no request")
		assert.Equal(t, r.toks[0], tokenOf(t, out))
	case "M":
		lt := r.ltKeys(r.listTokens())
		for _, tok := range r.checked() {
			assert.Contains(t, lt, tok, "the record is in the cache file, not pruned")
		}
	}
	r.keys = r.ltKeys(r.listTokens())
}

func (r *ruleRun) checked() []string {
	if r.row.all {
		return r.toks
	}
	return r.toks[:1]
}

// isOwner reports whether the token's record is of the owner kind.
func (r *ruleRun) isOwner(tok string) bool {
	for i, name := range r.toks {
		if name == tok && r.row.recs[i].kind == "owner" {
			return true
		}
	}
	return false
}

func deletesOf(reqs []repairRequest) []repairRequest {
	var out []repairRequest
	for _, q := range reqs {
		if q.Method == "DELETE" {
			out = append(out, q)
		}
	}
	return out
}

// assertDeleted asserts that the requests hold exactly one DELETE of the token,
// sent with the token's own bearer.
func assertDeleted(t *testing.T, reqs []repairRequest, tok string) {
	t.Helper()
	var n int
	for _, q := range deletesOf(reqs) {
		if strings.HasSuffix(q.Path, "/tokens/"+tok) {
			n++
			assert.True(t, strings.HasPrefix(q.Auth, "Bearer ext/"+tok+":"), "the DELETE of %s carries its own bearer: %q", tok, q.Auth)
		}
	}
	assert.Equal(t, 1, n, "one DELETE of %s: %v", tok, reqs)
}

func (r *ruleRun) doAct() {
	row := r.row
	argv := row.act
	if row.actFn != nil {
		argv = row.actFn(r)
	}
	switch {
	case row.editAs != "":
		r.setX(row.editAs)
		return
	case argv == nil:
		return
	}
	r.actArgv = argv
	mark := r.f.mark()
	r.act = r.run(append([]string{"-o", "json"}, argv...)...)
	r.actReqs = r.f.requestsSince(mark)
	if argv[0] == "config" && argv[1] == "update-server" {
		// Rule 3: update-server sends no DELETE, and no request that carries a
		// bearer; the version refresh it sends has none.
		for _, q := range r.actReqs {
			assert.NotEqual(r.t, "DELETE", q.Method, "update-server sends no DELETE: %v", r.actReqs)
			assert.Empty(r.t, q.Auth, "update-server sends no bearer: %v", q)
		}
	}
}

func (r *ruleRun) isLogoutAll() bool {
	a := r.actArgv
	return len(a) >= 3 && a[0] == "auth" && a[1] == "logout" && a[2] == "--all"
}

func (r *ruleRun) check() {
	t, row := r.t, r.row
	r.makeRecords()
	r.beforeCheck()
	r.doAct()
	r.read()
	r.lt = r.listTokens()
	r.status = r.run("-o", "json", "config", "status")
	if r.isLogoutAll() {
		r.la, r.laReqs = r.act, r.actReqs
	}
	if !row.custom {
		r.checkAfter()
	}
	if !r.isLogoutAll() {
		mark := r.f.mark()
		r.la = r.run("-o", "json", "auth", "logout", "--all")
		r.laReqs = r.f.requestsSince(mark)
	}
	if !row.custom {
		r.checkSource()
		r.checkListed()
	}
	r.checkRules()
	if row.extra != nil {
		row.extra(t, r)
	}
}

// checkAfter repeats the first record's call after the change and the read.
func (r *ruleRun) checkAfter() {
	t, row := r.t, r.row
	if len(row.recs) == 0 || row.recs[0].other || row.recs[0].v3 || row.want.after == "" {
		return
	}
	mark := r.f.mark()
	stdout, _, _ := runWithStdin(t, fillAll(r.f.fill, callFor(row.recs[0].kind)), "")
	reqs := r.f.requestsSince(mark)
	switch row.want.after {
	case "S":
		assert.Empty(t, reqs, "a record still served sends no request")
		assert.Contains(t, stdout, r.toks[0])
	case "M":
		assert.NotContains(t, stdout, r.toks[0], "a record no longer served is not returned")
	}
}

func (r *ruleRun) checkSource() {
	t, row := r.t, r.row
	src := row.want.source
	for _, tok := range r.checked() {
		switch {
		case strings.HasPrefix(src, "next("):
			n, ok := itemsNamed(t, r.act, "notRevoked")[tok]
			require.True(t, ok, "%s is listed in the result: %s", tok, r.act)
			assert.Equal(t, "notAttempted", n["reason"])
			assert.Equal(t, strings.TrimSuffix(strings.TrimPrefix(src, "next("), ")"), n["entry"])
			assert.Equal(t, "nextLogin", n["retry"])
			assert.Empty(t, deletesOf(r.actReqs))
		case strings.HasPrefix(src, "rec("):
			n, ok := itemsNamed(t, r.act, "notRevoked")[tok]
			require.True(t, ok, "%s is listed in the result: %s", tok, r.act)
			assert.Equal(t, strings.TrimSuffix(strings.TrimPrefix(src, "rec("), ")"), n["entry"])
			assert.Equal(t, "manual", n["retry"])
			if len(r.actArgv) > 1 && r.actArgv[1] == "update-server" {
				// update-server never tries the token. On the target, whose old
				// server it leaves, it is otherServer; on another entry that names
				// the server it is left to that entry's login.
				assert.Equal(t, map[bool]string{true: "otherServer", false: "notAttempted"}[n["entry"] == "x"], n["reason"])
			}
			assert.Empty(t, deletesOf(r.actReqs), "nothing is sent")
		case strings.HasPrefix(src, "DEL(") && strings.HasSuffix(src, "@LA"):
			assertDeleted(t, r.laReqs, tok)
			assert.Contains(t, itemsNamed(t, r.la, "revoked"), tok)
		case strings.HasPrefix(src, "DEL("):
			assertDeleted(t, r.actReqs, tok)
			assert.Contains(t, itemsNamed(t, r.act, "revoked"), tok)
		case src == "live":
			assert.Contains(t, r.ltKeys(r.lt), tok, "the record stays in the cache")
			assert.False(t, strings.HasPrefix(r.ltKeys(r.lt)[tok], config.KeptTokenKeyPrefix), "under its own key")
		case src == "kept":
			assert.True(t, strings.HasPrefix(r.ltKeys(r.lt)[tok], config.KeptTokenKeyPrefix), "kept under a kept key: %v", r.ltKeys(r.lt))
		case src == "v3":
			// A v3 token is listed as manual by every plan of update-server; its
			// own bearer is never sent.
			n, ok := itemsNamed(t, r.act, "notRevoked")[tok]
			require.True(t, ok, "%s is listed in the result: %s", tok, r.act)
			assert.Equal(t, "v3", n["reason"])
			assert.Equal(t, "manual", n["retry"])
			assert.Empty(t, deletesOf(r.actReqs))
		case src == "once":
			assert.Contains(t, itemsNamed(t, r.act, "notRevoked"), tok)
		}
	}
}

func (r *ruleRun) checkListed() {
	t, row := r.t, r.row
	listed := strings.Fields(row.want.listed)
	has := func(s string) bool { return slices.Contains(listed, s) }
	for _, tok := range r.checked() {
		inAct := r.act != "" && jsonHolds(t, r.act, tok)
		inLA := jsonHolds(t, r.la, tok)
		if !r.isLogoutAll() {
			assert.Equal(t, has("R"), inAct, "%s in the change's result (want %q): %s", tok, row.want.listed, r.act)
		}
		assert.Equal(t, has("St"), jsonHolds(t, r.status, tok), "%s in config status (want %q): %s", tok, row.want.listed, r.status)
		assert.Equal(t, has("LT"), jsonHolds(t, r.lt, tok), "%s in auth list-tokens (want %q): %s", tok, row.want.listed, r.lt)
		assert.Equal(t, has("LA"), inLA, "%s in logout --all (want %q): %s", tok, row.want.listed, r.la)
	}
}

// checkRules asserts the rules every row meets, whatever it wants.
func (r *ruleRun) checkRules() {
	t, row := r.t, r.row
	for _, tok := range r.checked() {
		if row.custom {
			// The record of the stub's server does not name the host the
			// misread URL reads as, as at every build; the rows of this form
			// assert only that nothing names or reaches that host.
			break
		}
		revoked := (r.act != "" && itemsNamed(t, r.act, "revoked")[tok] != nil) || itemsNamed(t, r.la, "revoked")[tok] != nil
		listed := itemsNamed(t, r.la, "notRevoked")[tok] != nil || (r.act != "" && itemsNamed(t, r.act, "notRevoked")[tok] != nil) ||
			jsonHolds(t, r.status, tok) || jsonHolds(t, r.lt, tok)
		if row.want.source != "once" {
			// Rule 1: a token is revoked, or listed in a source that holds it
			// until it is revoked.
			assert.True(t, revoked || listed, "%s is revoked or listed", tok)
		}
		if row.want.source != "once" && !revoked && !r.isOwner(tok) {
			// Until revoked: the token logout --all did not revoke is in its
			// notRevoked, and config status or auth list-tokens still lists it
			// after that run.
			assert.Contains(t, itemsNamed(t, r.la, "notRevoked"), tok, "a token logout --all did not revoke is in its notRevoked")
			status, tokens := r.run("-o", "json", "config", "status"), r.run("-o", "json", "auth", "list-tokens")
			assert.True(t, jsonHolds(t, status, tok) || jsonHolds(t, tokens, tok), "%s is still listed after logout --all", tok)
		}
		if r.isOwner(tok) && row.want.after == "S" {
			// The exception to rule 1: an owner token is named once, by logout
			// --all.
			assert.Contains(t, itemsNamed(t, r.la, "notRevoked"), tok, "an owner token is named once by logout --all")
		}
		// Rule 2: a record still served is not deleted by a config change.
		if row.want.after == "S" && r.act != "" {
			// Compared with the key auth list-tokens showed and with the key the
			// record is stored under: a delete may show the key another way.
			var stored string
			for i, name := range r.toks {
				if name == tok {
					_, _, _, stored = r.recKey(row.recs[i])
				}
			}
			deleted, _ := jsonOf(t, r.act)["deletedCache"].([]any)
			for _, k := range deleted {
				assert.NotEqual(t, r.keys[tok], k, "%s is served and its record is not deleted", tok)
				assert.NotEqual(t, stored, k, "%s is served and its record is not deleted under its stored key", tok)
			}
		}
	}
	if row.world.x == canaryURLMisread {
		// The host read from the URL is not named, and no request goes to it.
		for _, out := range []string{r.act, r.la, r.status, r.lt} {
			assert.NotRegexp(t, rulesMisreadHost, withoutRunDirectory(strings.ReplaceAll(out, cacheListOwner, "owner")))
		}
	}
}

func runRulesRow(t *testing.T, row ruleRow) {
	t.Helper()
	t.Run(row.id, func(t *testing.T) {
		r := &ruleRun{t: t, f: newRepairFixture(t), row: row}
		r.check()
	})
}

// TestCacheRecordsAcrossConfigChanges asserts, for each case of a config
// change against a kind of cached record, what the exec plugin serves before
// and after, where the token can still be revoked, and where it is listed,
// and that every token is revoked or listed, no record that is still served is
// deleted by a config change, and update-server sends no DELETE and no
// bearer. For a logout --all row the change and the later logout --all are the
// same run: its result is listed as LA. Not parallel: it sets the process
// environment.
//
// A row's id names its case: the change, the kind of cached record and, for
// the "misread" rows, the URL https://admin:1234/...@x, which reads as a host
// the operator never named; those rows assert that nothing names or reaches
// that host. A repair is update-server of an entry whose URL holds an @ after
// the host, to a URL that does not; a rewrite is update-server of an entry whose
// URL holds a user name and password, to the same server without them.
func TestCacheRecordsAcrossConfigChanges(t *testing.T) {
	update := []string{"config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}
	deleteX := []string{"config", "delete-server", "x"}
	logoutAll := []string{"auth", "logout", "--all"}
	server := func(how string) ruleRec { return ruleRec{kind: "server", how: how} }
	owner := func(how string) ruleRec { return ruleRec{kind: "owner", how: how} }
	own := func(how string) ruleRec { return ruleRec{kind: "own", how: how} }
	rows := []ruleRow{
		{id: "update-server to another server, server record", world: ruleWorld{x: rulesOrdinary, owned: true}, recs: []ruleRec{server("call")}, act: update,
			want: ruleWant{before: "S", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "update-server to another server, owner record", world: ruleWorld{x: rulesOrdinary, owned: true}, recs: []ruleRec{owner("call")}, act: update,
			want: ruleWant{before: "S", after: "S", source: "live", listed: "LT LA"}},
		{id: "update-server to another server, record of another server naming an unusable entry", world: ruleWorld{x: rulesOrdinary, owned: true}, recs: []ruleRec{{kind: "server", how: "earlier", other: true}}, act: update,
			want: ruleWant{before: "M", source: "live", listed: "LT LA"}},
		{id: "update-server to another server, empty list at a server with no entry", world: ruleWorld{x: rulesOrdinary, noY: true}, recs: []ruleRec{server("earlier")}, asIs: true, act: update,
			want: ruleWant{before: "S", after: "M", source: "rec(x)", listed: "R St LA"}},
		{id: "repair, server record", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("call")}, act: update,
			want: ruleWant{before: "S", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "repair, server record, misread URL", world: ruleWorld{x: canaryURLMisread, owned: true}, recs: []ruleRec{server("call")}, act: update, custom: true},
		{id: "repair, owner record", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{owner("call")}, act: update,
			want: ruleWant{before: "S", after: "S", source: "live", listed: "LT LA"}},
		{id: "repair, earlier server record naming x", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("earlier")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "repair, kept record naming x", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("kept")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "repair, server record, another owner on the usable entry", world: ruleWorld{x: rulesRawAt, owned: true, yOwner: "u-other"}, recs: []ruleRec{server("call")}, act: update,
			want: ruleWant{before: "S", after: "M", source: "rec(y)", listed: "R St LA"}, extra: assertRecordedOnUsableEntryOfAnotherOwner},
		{id: "repair, own credential of x, another owner on the usable entry", world: ruleWorld{x: rulesRawAt, login: true, yOwner: "u-other"}, recs: []ruleRec{own("call")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "rec(y)", listed: "R St LA"}, extra: assertRecordedOnUsableEntryOfAnotherOwner},
		{id: "repair, own credential of x, a usable entry of another owner sorts first", world: ruleWorld{x: rulesRawAt, login: true, entries: []ruleEntry{{"a", "{url}", "u-other"}}}, recs: []ruleRec{own("call")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "repair, own credential of x, an unusable entry sorts first", world: ruleWorld{x: rulesRawAt, login: true, entries: []ruleEntry{{"a", "{url}/pq@a", cacheListOwner}}}, recs: []ruleRec{own("call")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "repair, own credential of x, no usable entry", world: ruleWorld{x: rulesRawAt, login: true, noY: true}, recs: []ruleRec{own("call")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "rec(x)", listed: "R St LA"},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				assertStoredServerUnusable(t, r, "x")
				assert.Contains(t, withoutRunDirectory(strings.Join(envelopeWarnings(t, r.act), "\n")), "delete token '"+r.toks[0]+"' --apply --yes", "the route that deletes it")
			}},
		{id: "repair, own credential of x, usable entry", world: ruleWorld{x: rulesRawAt, login: true}, recs: []ruleRec{own("call")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "next(y)", listed: "R St LA"}},
		{id: "update-server without a URL change, every kind", world: ruleWorld{x: rulesOrdinary, owned: true}, recs: []ruleRec{server("call"), owner("call"), {kind: "ownY", how: "call"}}, all: true,
			actFn: func(r *ruleRun) []string {
				// The same CA through a file: the command saves it as a path, so
				// the CA changes and nothing else does.
				path := filepath.Join(r.t.TempDir(), "ca.pem")
				require.NoError(r.t, os.WriteFile(path, []byte(r.f.fill("{pem}")), 0o600))
				return []string{"config", "update-server", "y", "--cacert", path}
			},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				assert.Equal(t, []any{"ca"}, jsonOf(t, r.act)["changed"], "the CA changed: %s", r.act)
			},
			want: ruleWant{before: "S", after: "S", source: "live", listed: "LT LA"}},
		{id: "rewrite, server record", world: ruleWorld{x: rulesUserinfo, owned: true}, recs: []ruleRec{server("call")}, act: update,
			want: ruleWant{before: "S", after: "M", source: "kept", listed: "R LT LA"},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				assertDeleted(t, r.laReqs, r.toks[0])
				assertKeptWarning(t, r, true)
			}},
		{id: "rewrite, owner record", world: ruleWorld{x: rulesUserinfo, owned: true}, recs: []ruleRec{owner("call")}, act: update,
			want: ruleWant{before: "S", after: "M", source: "kept", listed: "R LT LA"},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				assertKeptWarning(t, r, false)
			}},
		{id: "rewrite, earlier records naming x", world: ruleWorld{x: rulesUserinfo, owned: true}, recs: []ruleRec{server("earlier"), owner("earlier"), own("earlier")}, all: true, act: update,
			want: ruleWant{before: "M", after: "S", source: "live", listed: "LT LA"}},
		{id: "rewrite, kept record", world: ruleWorld{x: rulesUserinfo, owned: true}, recs: []ruleRec{server("kept")}, act: update,
			want: ruleWant{before: "M", after: "M", source: "kept", listed: "LT LA"}},
		{id: "delete-server of x, server record bound to the usable entry", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("call")}, act: deleteX,
			want: ruleWant{before: "S", after: "S", source: "live", listed: "LT LA"}},
		{id: "delete-server of x, kept record naming x", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("kept")}, act: deleteX,
			want: ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "delete-server of x, own credential, no usable entry", world: ruleWorld{x: rulesRawAt, login: true, noY: true}, recs: []ruleRec{own("call")}, act: deleteX,
			want: ruleWant{before: "M", source: "once", listed: "R"},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				assert.True(t, jsonHolds(t, r.act, "delete token '"+r.toks[0]+"' --apply --yes"), "the route that deletes it is in the result: %s", r.act)
				assert.Empty(t, deletesOf(r.actReqs), "nothing is sent")
			}},
		{id: "delete-server of x, own credential, usable entry", world: ruleWorld{x: rulesRawAt, login: true}, recs: []ruleRec{own("call")}, act: deleteX,
			want: ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "set-token-reuse, record naming an unusable entry", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("earlier")}, act: []string{"config", "set-token-reuse", "on"},
			want: ruleWant{before: "M", after: "M", source: "live", listed: "LT LA"}},
		{id: "logout of the usable entry, kept record naming x", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("kept")}, act: []string{"auth", "logout", "y"},
			want: ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "logout of x, own credential, no usable entry", world: ruleWorld{x: rulesRawAt, login: true, noY: true}, recs: []ruleRec{own("call")}, act: []string{"auth", "logout", "x"},
			want: ruleWant{before: "M", after: "M", source: "rec(x)", listed: "R St LA"}},
		{id: "logout of x, own credential, usable entry", world: ruleWorld{x: rulesRawAt, login: true}, recs: []ruleRec{own("call")}, act: []string{"auth", "logout", "x"},
			want: ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "logout of x, server record bound to the usable entry", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("call")}, act: []string{"auth", "logout", "x"},
			want: ruleWant{before: "S", after: "S", source: "live", listed: "LT LA"}},
		{id: "logout --all, kept record naming x", world: ruleWorld{x: rulesRawAt, owned: true, xCA: true}, recs: []ruleRec{server("kept")}, act: logoutAll,
			want: ruleWant{before: "M", source: "DEL(y)", listed: "LA"}},
		{id: "logout --all, own credential, no usable entry", world: ruleWorld{x: rulesRawAt, login: true, noY: true}, recs: []ruleRec{own("call")}, act: logoutAll,
			want: ruleWant{before: "M", after: "M", source: "rec(x)", listed: "St LA"},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				// Each run lists it again, and sends nothing.
				mark := r.f.mark()
				again := r.run("-o", "json", "auth", "logout", "--all")
				assert.Contains(t, itemsNamed(t, again, "notRevoked"), r.toks[0])
				assert.Empty(t, deletesOf(r.f.requestsSince(mark)))
			}},
		{id: "delete-token by name, earlier record naming x", world: ruleWorld{x: rulesRawAt, owned: true, xCA: true}, recs: []ruleRec{server("earlier")}, act: []string{"auth", "delete-token", "prod-cluster"},
			want: ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "delete-token by shown kept key", world: ruleWorld{x: rulesRawAt, owned: true, xCA: true}, recs: []ruleRec{server("kept")},
			actFn: func(r *ruleRun) []string { return []string{"auth", "delete-token", r.keys[r.toks[0]]} },
			want:  ruleWant{before: "M", source: "DEL(y)", listed: "R"}},
		{id: "hand edit makes x unusable, server record", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("call")}, editAs: rulesRawAt,
			want: ruleWant{before: "S", after: "M", source: "live", listed: "LT LA"}},
		{id: "plugin write over a record naming an unusable entry, owner record", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{owner("kept")},
			want: ruleWant{before: "M", after: "M", source: "kept", listed: "LT LA"}},
		{id: "plugin write over a record naming an unusable entry, server record", world: ruleWorld{x: rulesRawAt, owned: true}, recs: []ruleRec{server("kept")},
			want: ruleWant{before: "M", after: "M", source: "kept", listed: "LT LA"}},
		{id: "update-server to another server, v3 token", world: ruleWorld{x: rulesOrdinary}, recs: []ruleRec{{kind: "own", how: "earlier", at: rulesOrdinary, v3: true}}, asIs: true, act: update,
			want: ruleWant{source: "v3", listed: "R St LA"}},
		{id: "logout --all, kept records of one name at two servers", world: ruleWorld{x: rulesRawAt, owned: true}, act: logoutAll, custom: true,
			recs: []ruleRec{{kind: "server", how: "kept", hand: true, sameName: true}, {kind: "server", how: "kept", other: true, sameName: true}},
			extra: func(t *testing.T, r *ruleRun) {
				t.Helper()
				// Two kept records of one kind and name, at two servers, are two
				// items. The token at the stub's server is sent through y (the
				// stub does not know it, so the answer is unconfirmed); the one at
				// the other server has no usable entry and is listed, and nothing
				// is sent for it.
				var items int
				for _, field := range []string{"revoked", "notRevoked"} {
					list, _ := jsonOf(t, r.la)[field].([]any)
					for _, n := range list {
						ref, _ := n.(map[string]any)["ref"].(map[string]any)
						if ref["name"] == "token-cch01" {
							items++
						}
					}
				}
				assert.Equal(t, 2, items, "one item for each kept record: %s", r.la)
				var sent int
				for _, q := range deletesOf(r.laReqs) {
					if strings.HasSuffix(q.Path, "/tokens/token-cch01") {
						sent++
					}
				}
				assert.Equal(t, 1, sent, "one DELETE: %v", r.laReqs)
			}},
		{id: "delete-server of x, own credential, misread URL", world: ruleWorld{x: canaryURLMisread, login: true}, recs: []ruleRec{own("call")}, act: deleteX, custom: true},
		{id: "logout of x, own credential, misread URL", world: ruleWorld{x: canaryURLMisread, login: true}, recs: []ruleRec{own("call")}, act: []string{"auth", "logout", "x"}, custom: true},
		{id: "logout --all, kept record, misread URL", world: ruleWorld{x: canaryURLMisread, owned: true, xCA: true}, recs: []ruleRec{server("kept")}, act: logoutAll, custom: true},
		{id: "delete-token by name, misread URL", world: ruleWorld{x: canaryURLMisread, owned: true}, recs: []ruleRec{server("earlier")}, act: []string{"auth", "delete-token", "prod-cluster"}, custom: true},
	}
	for _, row := range rows {
		runRulesRow(t, row)
	}
}

// assertKeptWarning asserts that the result of a rewrite names the first
// token in exactly one warning, which names auth list-tokens, names no
// logout --all, and names auth delete-token with the key the token is shown
// under only for a record that is not of the owner kind: logout --all signs
// every entry out, and for an owner record it revokes nothing.
func assertKeptWarning(t *testing.T, r *ruleRun, withDelete bool) {
	t.Helper()
	tok := r.toks[0]
	var about []string
	for _, w := range envelopeWarnings(t, r.act) {
		if strings.Contains(w, tok) {
			about = append(about, w)
		}
	}
	require.Len(t, about, 1, "the token is named in one warning: %s", r.act)
	w := withoutRunDirectory(about[0])
	assert.Contains(t, w, "rancher auth list-tokens")
	assert.NotContains(t, w, "logout")
	if !withDelete {
		assert.NotContains(t, w, "delete-token")
		return
	}
	assert.Contains(t, w, "rancher auth delete-token '"+r.ltKeys(r.lt)[tok]+"'")
}

// assertRecordedOnUsableEntryOfAnotherOwner asserts what a token recorded on a
// usable entry that records another owner shows: the entry's own server, none
// of the text for an URL that cannot be used, the state of a token of another
// owner in config status, and a manual revoke in logout --all.
func assertRecordedOnUsableEntryOfAnotherOwner(t *testing.T, r *ruleRun) {
	t.Helper()
	tok := r.toks[0]
	n := itemsNamed(t, r.act, "notRevoked")[tok]
	require.NotNil(t, n, "%s is listed: %s", tok, r.act)
	assert.Equal(t, "manual", n["retry"])
	assert.Equal(t, r.f.srvURL, n["server"], "the server shown is the entry's own")
	for _, w := range envelopeWarnings(t, r.act) {
		if strings.Contains(w, tok) {
			assert.NotContains(t, w, rawAtReason, "the text of the token is that of the usable entry")
		}
	}
	assert.True(t, jsonHolds(t, r.status, "differentOwner"), "config status gives the state of a token of another owner: %s", r.status)
	la := itemsNamed(t, r.la, "notRevoked")[tok]
	require.NotNil(t, la, "%s is in logout --all: %s", tok, r.la)
	assert.Equal(t, "manual", la["retry"])
}

// assertStoredServerUnusable asserts that the superseded record of the first
// token on entry names the server as unusable, which no login retires.
func assertStoredServerUnusable(t *testing.T, r *ruleRun, entry string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(r.f.dir))
	require.NoError(t, err)
	var found bool
	for _, rec := range cfg.Servers[entry].SupersededTokens {
		if rec.Ref.Name == r.toks[0] {
			found = true
			assert.True(t, strings.HasPrefix(rec.Server, "unusable:"), "stored server: %q", rec.Server)
		}
	}
	assert.True(t, found, "%s is recorded on %s", r.toks[0], entry)
}

// TestTheRewriteOfAURLHandlesAFailedMove asserts what config update-server
// does for the user name and password it removes from an entry's URL when the
// move of the cached tokens that the rewrite stops serving fails. A token
// cache that cannot be read holds nothing to move: the command warns and
// writes the entry. A move that fails for another reason (the cache lock held
// by another process) stops the command before the entry is written, and the
// record stays at its lookup key. A failed write of the entry, after the move,
// keeps the tokens under their kept keys and says so in its warnings. Not
// parallel: it sets the process environment.
func TestTheRewriteOfAURLHandlesAFailedMove(t *testing.T) {
	rewrite := []string{"-o", "json", "config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}
	setup := func(t *testing.T) *ruleRun {
		t.Helper()
		r := &ruleRun{t: t, f: newRepairFixture(t), row: ruleRow{world: ruleWorld{x: rulesUserinfo, owned: true}, recs: []ruleRec{{kind: "server", how: "call"}}}}
		r.makeRecords()
		return r
	}
	xURL := func(r *ruleRun) string {
		cfg, err := config.Load(config.FilePath(r.f.dir))
		require.NoError(t, err)
		return cfg.Servers["x"].URL
	}
	t.Run("an unreadable cache warns and the entry is written", func(t *testing.T) {
		r := setup(t)
		require.NoError(t, os.WriteFile(filepath.Join(r.f.dir, "token-cache.yaml"), []byte("{not yaml"), 0o600))
		out := r.f.run(t, "", rewrite...)
		assert.Contains(t, strings.Join(envelopeWarnings(t, out), "\n"), "no cached token was kept")
		assert.Equal(t, r.f.srvURL, xURL(r), "the entry is written")
	})
	t.Run("another failure stops the command before the entry is written", func(t *testing.T) {
		r := setup(t)
		before := xURL(r)
		t.Cleanup(config.SetCacheLockWaitForTest(200 * time.Millisecond))
		held := flock.New(filepath.Join(r.f.dir, "token-cache.lock"))
		require.NoError(t, held.Lock())
		stdout, stderr, code := runWithStdin(t, fillAll(r.f.fill, rewrite), "")
		require.NoError(t, held.Unlock())
		assert.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
		assert.Equal(t, before, xURL(r), "the entry is not written")
		keys := r.ltKeys(r.listTokens())
		require.Contains(t, keys, r.toks[0])
		assert.False(t, strings.HasPrefix(keys[r.toks[0]], config.KeptTokenKeyPrefix), "the record is where it was")
	})
	t.Run("a failed write of the entry after the move says what was kept", func(t *testing.T) {
		r := setup(t)
		t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
			if step == "update" {
				return errors.New("disk full")
			}
			return nil
		}))
		stdout, stderr, code := runWithStdin(t, fillAll(r.f.fill, rewrite), "")
		assert.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
		assert.Contains(t, strings.Join(envelopeWarnings(t, cmp.Or(stdout, stderr)), "\n"), r.toks[0], "the warnings name the token that was kept")
		assert.True(t, strings.HasPrefix(r.ltKeys(r.listTokens())[r.toks[0]], config.KeptTokenKeyPrefix))
	})
}
