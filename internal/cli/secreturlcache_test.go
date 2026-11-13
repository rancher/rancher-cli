package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheRecordSeed is one exec-credential cache record of an earlier build: the
// entry it is named for, the server it names, and its token.
type cacheRecordSeed struct {
	name, server string
	// ref is the token the record names; nil for the entry's own credential.
	ref    *clierr.TokenRef
	expiry time.Time
}

// seedCacheRecords writes the entries (besides x, which the seed holds) and the
// cache records into dir, which already holds config.yaml.
func seedCacheRecords(tb testing.TB, dir string, entries map[string]string, recs []cacheRecordSeed) {
	tb.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(tb, err)
	for name, url := range entries {
		cfg.Servers[name] = config.Server{URL: url}
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(tb, cfg.Write())
	for _, r := range recs {
		binding := config.CacheBindingFor(config.CanonicalDir(dir), cfg, r.name)
		token := "own-credential"
		if r.ref != nil {
			binding = binding.WithCreatedToken(r.ref, "u-abc12")
			token = "ext/" + r.ref.Name + ":secret"
		}
		expiry := cmpTime(r.expiry)
		require.NoError(tb, config.NewFileStore(dir).SetCachedCred(config.CacheKey(r.name, config.CanonicalServerIdentity(r.server), "c-1"), token,
			expiry, r.name, r.server, "c-1", binding))
	}
}

func cmpTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().Add(time.Hour)
	}
	return t
}

// loginToHostPattern finds a login step that names a URL, as a recovery
// sequence does; the line that names the next login of an entry does not.
var loginToHostPattern = regexp.MustCompile(`auth login [^\n;]*--url`)

// cacheStep is a command and what it is asserted to hold.
type cacheStep struct {
	argv []string
	// code is the error code the step must fail with, empty for success.
	code string
	// deleted, when set, is the exact deletedCache or deleted list of a JSON
	// delete.
	deleted []string
	// own is set for a command that prints the URL of the entry it names, which
	// is a host of its own; the output is then not searched for the host and
	// the servers of its records are asserted instead.
	own bool
	// listed, when listing, is the number of rows a JSON list holds.
	listed  int
	listing bool
}

// runCacheSteps runs the steps in a fresh stub, config directory and seed and
// asserts none of the output holds a part of the hosts read from the URLs: in
// the text with the run directory removed, and in every key and string of the
// decoded JSON.
func runCacheSteps(t *testing.T, pattern *regexp.Regexp, entries map[string]string, recs []cacheRecordSeed, steps []cacheStep) {
	t.Helper()
	srv := secretServer(t, nil, secretBearer)
	fill := secretFiller(t, srv)
	t.Setenv(config.EnvURL, "")
	dir := os.Getenv(config.EnvConfigDir)
	writeSecretStored(t, dir, secretStored{url: canaryURLMisread, stubCA: true, token: true}, fill)
	seedCacheRecords(t, dir, entries, recs)
	for i, step := range steps {
		stdout, stderr, code := runWithStdin(t, fillAll(fill, step.argv), "")
		where := fmt.Sprintf("step %d: %s", i+1, strings.Join(step.argv, " "))
		text := strings.ReplaceAll(withoutRunDirectory(stdout+"\n"+stderr), dir, "")
		if !step.own {
			assert.NotRegexp(t, pattern, text, where)
		}
		assert.NotRegexp(t, loginToHostPattern, text, where)
		if step.code != "" {
			require.NotZero(t, code, where)
			assert.Equal(t, step.code, renderedError(t, "json", stderr).Code, where)
			continue
		}
		if step.argv[0] != "-o" {
			continue
		}
		var doc any
		require.NoError(t, json.Unmarshal([]byte(stdout), &doc), "%s: %s", where, stdout)
		for _, s := range decodedStrings(doc) {
			if !step.own {
				assert.NotRegexp(t, pattern, strings.ReplaceAll(s, dir, ""), where)
			}
		}
		for _, server := range recordServers(doc, false) {
			assert.Equal(t, "<unusable URL>", server, "%s: the server of a record", where)
		}
		data := doc.(map[string]any)["data"]
		if step.listing {
			rows, _ := data.([]any)
			assert.Len(t, rows, step.listed, where)
		}
		if step.deleted != nil {
			m, _ := data.(map[string]any)
			got := m["deleted"]
			if got == nil {
				got = m["deletedCache"]
			}
			assert.Equal(t, toAny(step.deleted), got, where)
		}
	}
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// TestCacheRecordsClassifiedEarlyShowNoHostReadFromAnEntryURL asserts that the
// cache record of an entry that holds an @ after the host shows the host read
// from the URL in no output of auth delete-token (by name and by the key
// list-tokens shows), config delete-server and auth logout, in the text and in
// the decoded JSON, when the record names no token (the entry's own
// credential) or is expired: the delete classifies those before it finds the
// record's entry. Not parallel: it sets the process environment.
func TestCacheRecordsClassifiedEarlyShowNoHostReadFromAnEntryURL(t *testing.T) {
	restoreRawArgv(t)
	own := cacheRecordSeed{name: "x", server: "https://admin:1234"}
	expired := cacheRecordSeed{name: "x", server: "https://admin:1234", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-exp01"},
		expiry: time.Now().Add(-3 * time.Hour)}
	for _, rec := range []struct {
		name string
		seed cacheRecordSeed
		list bool
	}{{"no token", own, true}, {"expired", expired, false}} {
		for _, tc := range []struct {
			name  string
			steps []cacheStep
		}{
			{"delete-token by the shown key", []cacheStep{
				{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 1},
				{argv: []string{"-o", "json", "auth", "delete-token", shownKey}, deleted: []string{shownKey}},
				{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 0}}},
			{"delete-token by name, text", []cacheStep{{argv: []string{"auth", "delete-token", "x"}}}},
			{"delete-token by name, json", []cacheStep{{argv: []string{"-o", "json", "auth", "delete-token", "x"}, deleted: []string{shownKey}}}},
			{"delete-server, text", []cacheStep{{argv: []string{"config", "delete-server", "x"}}}},
			{"delete-server, json", []cacheStep{{argv: []string{"-o", "json", "config", "delete-server", "x"}, deleted: []string{shownKey}}}},
			{"update-server, text", []cacheStep{{argv: []string{"config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}}}},
			{"update-server, json", []cacheStep{{argv: []string{"-o", "json", "config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}, deleted: []string{shownKey}}}},
			{"logout, text", []cacheStep{{argv: []string{"auth", "logout", "x"}}}},
			{"logout, json", []cacheStep{{argv: []string{"-o", "json", "auth", "logout", "x"}, deleted: []string{shownKey}}}},
		} {
			t.Run(rec.name+", "+tc.name, func(t *testing.T) {
				steps := tc.steps
				if !rec.list {
					// An expired record is pruned from the list and from the key lookup.
					steps = slicesWithout(steps)
				}
				runCacheSteps(t, misreadPattern, nil, []cacheRecordSeed{rec.seed}, steps)
			})
		}
	}
}

// slicesWithout drops the steps that need the record to be listed: the list
// steps and the ones that assert the key of a deleted record, which an expired
// record does not give.
func slicesWithout(steps []cacheStep) []cacheStep {
	var out []cacheStep
	for _, s := range steps {
		if s.listing {
			continue
		}
		s.deleted = nil
		out = append(out, s)
	}
	return out
}

// TestAShownKeyThatMatchesSeveralCacheRecordsIsRefused asserts that two cache
// records that differ only in the server they name, each the host read from
// the URL of an entry that holds an @ after the host, list one key; that
// auth delete-token with that key refuses with CONFLICT and deletes nothing,
// while the key as stored selects exactly one record; and that deleting by
// name and config delete-server, which the refusal names, print neither host.
// Not parallel: it sets the process environment.
func TestAShownKeyThatMatchesSeveralCacheRecordsIsRefused(t *testing.T) {
	restoreRawArgv(t)
	pattern := regexp.MustCompile(`\badmin\b|\broot\b|1234|5678|recover-|@x|q@h`)
	entries := map[string]string{"z": "https://root:5678/q@h"}
	recs := []cacheRecordSeed{
		{name: "x", server: "https://admin:1234", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch01"}},
		{name: "x", server: "https://root:5678", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch02"}},
	}
	list := cacheStep{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 2}
	for _, tc := range []struct {
		name  string
		steps []cacheStep
	}{
		{"the shown key is refused and nothing is deleted", []cacheStep{list,
			{argv: []string{"-o", "json", "auth", "delete-token", shownKey}, code: "CONFLICT"}, list}},
		{"the stored key selects one record", []cacheStep{list,
			{argv: []string{"-o", "json", "auth", "delete-token", "x@https://admin:1234/c-1"}, deleted: []string{shownKey}},
			{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 1}}},
		{"the repair the refusal names removes both", []cacheStep{list,
			{argv: []string{"-o", "json", "auth", "delete-token", shownKey}, code: "CONFLICT"},
			{argv: []string{"-o", "json", "auth", "delete-token", "x"}, deleted: []string{shownKey, shownKey}},
			{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 0}}},
		{"logout of every entry, json, then status", []cacheStep{
			{argv: []string{"-o", "json", "auth", "logout", "--all"}}, {argv: []string{"config", "status"}}, {argv: []string{"-o", "json", "config", "status"}}}},
		{"logout of every entry, text, then status", []cacheStep{
			{argv: []string{"auth", "logout", "--all"}}, {argv: []string{"config", "status"}}}},
		{"delete-token by name, text", []cacheStep{{argv: []string{"auth", "delete-token", "x"}}}},
		{"delete-token by name, json", []cacheStep{{argv: []string{"-o", "json", "auth", "delete-token", "x"}, deleted: []string{shownKey, shownKey}}}},
		{"delete-server, text", []cacheStep{{argv: []string{"config", "delete-server", "x"}}}},
		{"delete-server, json", []cacheStep{{argv: []string{"-o", "json", "config", "delete-server", "x"}, deleted: []string{shownKey}}}},
	} {
		t.Run(tc.name, func(t *testing.T) { runCacheSteps(t, pattern, entries, recs, tc.steps) })
	}
	refusal := func(t *testing.T, recs []cacheRecordSeed) string {
		t.Helper()
		srv := secretServer(t, nil, secretBearer)
		fill := secretFiller(t, srv)
		t.Setenv(config.EnvURL, "")
		dir := os.Getenv(config.EnvConfigDir)
		writeSecretStored(t, dir, secretStored{url: canaryURLMisread, stubCA: true, token: true}, fill)
		seedCacheRecords(t, dir, entries, recs)
		_, stderr, code := runWithStdin(t, []string{"-o", "json", "auth", "delete-token", shownKey}, "")
		require.NotZero(t, code)
		msg := clierr.ExpandRemedies(withoutRunDirectory(renderedError(t, "json", stderr).Message), "")
		assert.NotRegexp(t, pattern, msg)
		assert.NotContains(t, msg, "delete-server")
		return msg
	}
	t.Run("the refusal fills in the record name when it selects exactly the matched records", func(t *testing.T) {
		msg := refusal(t, recs)
		assert.Contains(t, msg, "the key matches 2 cached credentials")
		assert.Contains(t, msg, "to remove the 2 cached credentials named 'x', run: rancher auth delete-token 'x'")
		assert.NotContains(t, msg, "<name>")
	})
	t.Run("the refusal keeps a placeholder when the name also selects another record", func(t *testing.T) {
		other := cacheRecordSeed{name: "x", server: "https://other.example", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch03"}}
		msg := refusal(t, append(slices.Clone(recs), other))
		assert.Contains(t, msg, "the key matches 2 cached credentials")
		assert.Contains(t, msg, "run: rancher auth delete-token <name>; <name> is the NAME column of: rancher auth list-tokens; that also removes other credentials of that name")
		assert.NotContains(t, msg, "'x'")
	})
}

// TestACacheRecordOfAUsableEntryAtTheSameIdentityShowsTheSameInEveryOutput
// asserts that when one entry holds an @ after the host and another entry is
// usable at the host read from it, the cache record of the usable entry shows
// as unusable in auth list-tokens and in the outputs of auth delete-token and
// config delete-server alike. A record that names a token differs once a
// delete records it on the usable entry: see
// TestATokenRecordOfAUsableEntryAtTheSameIdentityIsShownUnderItsOwnServerByADelete.
// Not parallel: it sets the process environment.
func TestACacheRecordOfAUsableEntryAtTheSameIdentityShowsTheSameInEveryOutput(t *testing.T) {
	restoreRawArgv(t)
	const key = "y@<unusable URL>/c-1"
	entries := map[string]string{"y": "https://admin:1234"}
	recs := []cacheRecordSeed{{name: "y", server: "https://admin:1234"}}
	for _, tc := range []struct {
		name  string
		steps []cacheStep
	}{
		{"list, then delete-token by name", []cacheStep{
			{argv: []string{"-o", "json", "auth", "list-tokens"}, listing: true, listed: 1},
			{argv: []string{"-o", "json", "auth", "delete-token", "y"}, deleted: []string{key}}}},
		{"delete-token by the shown key", []cacheStep{{argv: []string{"-o", "json", "auth", "delete-token", key}, deleted: []string{key}}}},
		{"delete-server of the usable entry, json", []cacheStep{{argv: []string{"-o", "json", "config", "delete-server", "y"}, deleted: []string{key}, own: true}}},
		{"delete-server of the usable entry, text", []cacheStep{{argv: []string{"config", "delete-server", "y"}, own: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) { runCacheSteps(t, misreadPattern, entries, recs, tc.steps) })
	}
}

// TestATokenRecordOfAUsableEntryAtTheSameIdentityIsShownUnderItsOwnServerByADelete
// asserts that when an entry holds an @ after the host at the host of a usable
// entry y, auth list-tokens shows y's token record as unusable, since it
// chooses no entry, and auth delete-token by that shown key deletes exactly
// that record and shows its key and its token under y's own server, since the
// delete records the token on y. Not parallel: it sets the process
// environment.
func TestATokenRecordOfAUsableEntryAtTheSameIdentityIsShownUnderItsOwnServerByADelete(t *testing.T) {
	restoreRawArgv(t)
	srv := secretServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}, secretBearer)
	fill := secretFiller(t, srv)
	t.Setenv(config.EnvURL, "")
	dir := os.Getenv(config.EnvConfigDir)
	writeSecretStored(t, dir, secretStored{name: "y", url: "{url}", stubCA: true, token: true}, fill)
	seedCacheRecords(t, dir, map[string]string{"w": srv.URL + "/pq@x"}, []cacheRecordSeed{
		{name: "y", server: srv.URL, ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch06"}}})
	const shown = "y@<unusable URL>/c-1"

	stdout, stderr, code := runWithStdin(t, []string{"-o", "json", "auth", "list-tokens"}, "")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	rows, _ := dataOf(t, stdout).([]any)
	require.Len(t, rows, 1, "stdout: %s", stdout)
	row, _ := rows[0].(map[string]any)
	assert.Equal(t, shown, row["key"])
	assert.Equal(t, "<unusable URL>", row["server"])

	stdout, stderr, code = runWithStdin(t, []string{"-o", "json", "auth", "delete-token", shown}, "")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	assert.Equal(t, []any{"y@" + srv.URL + "/c-1"}, dataOf(t, stdout).(map[string]any)["deleted"], "exactly that record")
	var found bool
	for _, n := range notRevokedOf(t, stdout) {
		if ref, _ := n["ref"].(map[string]any); ref["name"] == "token-cch06" {
			found = true
			assert.Equal(t, srv.URL, n["server"])
		}
	}
	assert.True(t, found, "stdout: %s", stdout)

	stdout, _, code = runWithStdin(t, []string{"-o", "json", "auth", "list-tokens"}, "")
	require.Zero(t, code)
	assert.Empty(t, dataOf(t, stdout), "the record is gone")
}

// TestATokenRecordOfAUsableEntryAtAnotherServerKeepsTheMisreadHostMasked
// asserts that when the usable entry a token record belongs to is at another
// server than the record's, which is the host an entry with an @ after the
// host reads as, the delete shows the record as unusable and names no part
// of that host: the usable entry does not name the record's server, so the
// token is not recorded there as at its own server. Not parallel: it sets the
// process environment.
func TestATokenRecordOfAUsableEntryAtAnotherServerKeepsTheMisreadHostMasked(t *testing.T) {
	restoreRawArgv(t)
	entries := map[string]string{"y": "https://other.example"}
	recs := []cacheRecordSeed{{name: "y", server: "https://admin:1234", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch08"}}}
	runCacheSteps(t, misreadPattern, entries, recs, []cacheStep{
		{argv: []string{"-o", "json", "auth", "delete-token", "y"}, deleted: []string{"y@<unusable URL>/c-1"}}})
}

// TestAMaskedCacheRecordOfAUsableEntryKeepsItsRevokeDecisionAndStoredServer
// asserts that when an entry holds an @ after the host at the host of a usable
// entry y, y's cache record shows y's own server, since the token is recorded
// on y, and the decisions are those of a delete with no such entry: a failed
// revoke is left to the next login of y (retry nextLogin), the superseded
// record y keeps is stored with y's own server, and its state is the owner's,
// not another server's. Not parallel: it sets the process environment.
func TestAMaskedCacheRecordOfAUsableEntryKeepsItsRevokeDecisionAndStoredServer(t *testing.T) {
	restoreRawArgv(t)
	srv := secretServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}, secretBearer)
	fill := secretFiller(t, srv)
	t.Setenv(config.EnvURL, "")
	dir := os.Getenv(config.EnvConfigDir)
	writeSecretStored(t, dir, secretStored{name: "y", url: "{url}", stubCA: true, token: true}, fill)
	seedCacheRecords(t, dir, map[string]string{"w": srv.URL + "/pq@x"}, []cacheRecordSeed{
		{name: "y", server: srv.URL, ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch05"}}})

	stdout, stderr, code := runWithStdin(t, []string{"-o", "json", "auth", "delete-token", "y"}, "")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	var cache map[string]any
	for _, n := range notRevokedOf(t, stdout) {
		if ref, _ := n["ref"].(map[string]any); ref["name"] == "token-cch05" {
			cache = n
		}
	}
	require.NotNil(t, cache, "stdout: %s", stdout)
	assert.Equal(t, "nextLogin", cache["retry"], "the decision is the one of a delete with no raw-at entry")
	assert.Equal(t, srv.URL, cache["server"], "the token is recorded on the usable entry, so its own server is shown")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	var stored []config.SupersededToken
	for _, r := range cfg.Servers["y"].SupersededTokens {
		if r.Ref.Name == "token-cch05" {
			stored = append(stored, r)
		}
	}
	require.Len(t, stored, 1, "the record is kept for the next login")
	assert.Equal(t, srv.URL, stored[0].Server, "the stored server is y's own")

	stdout, _, code = runWithStdin(t, []string{"-o", "json", "config", "status"}, "")
	require.Zero(t, code)
	state, _ := recordJSON(t, stdout, "y", "token-cch05")
	assert.NotEqual(t, "otherServer", state)
}
