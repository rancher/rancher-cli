package cli

import (
	"encoding/json"
	"fmt"
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

// misreadPattern finds the texts no output may hold when an entry's stored URL
// is canaryURLMisread: the host read from it (admin and 1234), the login name a
// recovery sequence builds from it (recover-), and the text after it (@x). The
// word "admin" stands alone: the advice for a v3 token says "an administrator".
var misreadPattern = regexp.MustCompile(`\badmin\b|1234|recover-|@x`)

// shownKey is the key output shows for the cache record of the seed.
const shownKey = "x@<unusable URL>/c-1"

// seedCachedTokenAtMisreadHost writes the cache record a build before the @
// check wrote for the entry x of canaryURLMisread: the check accepted the URL
// then, so the record names the host read from it.
func seedCachedTokenAtMisreadHost(tb testing.TB, dir string) {
	tb.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(tb, err)
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch01"}
	binding := config.CacheBindingFor(config.CanonicalDir(dir), cfg, "x").WithCreatedToken(ref, "u-abc12")
	require.NoError(tb, config.NewFileStore(dir).SetCachedCred(config.CacheKey("x", "https://admin:1234", "c-1"), "ext/token-cch01:secret",
		time.Now().Add(time.Hour), "x", "https://admin:1234", "c-1", binding))
}

// misreadSequenceSeed is the entry every sequence starts from: the URL read
// as a host with a port, a login, a superseded record that names no server,
// two (an ext and a v3 token) an earlier build wrote with the host read from
// the URL, and a cache record from an earlier build.
func misreadSequenceSeed() secretStored {
	return secretStored{url: canaryURLMisread, stubCA: true, token: true, records: []config.SupersededToken{
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old77"}, UserID: "u-abc12", SupersededAt: "2026-10-01T00:00:00Z"},
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old88"}, UserID: "u-abc12", SupersededAt: "2026-10-02T00:00:00Z", Server: "https://admin:1234"},
		{Ref: clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-old99"}, UserID: "u-abc12", SupersededAt: "2026-10-03T00:00:00Z", Server: "https://admin:1234"},
	}}
}

// sequenceStep is one command of a sequence and its standard input.
type sequenceStep struct {
	argv  []string
	stdin string
	// listed is the number of records an auth list-tokens step lists, when
	// listing is set.
	listed  int
	listing bool
}

// removes reports whether the step is a command that changes or removes the
// entry, which deletes the cache record of the seed and prints its key.
func (s sequenceStep) removes() bool {
	return slices.ContainsFunc(s.argv, func(a string) bool { return a == "update-server" || a == "delete-server" || a == "logout" })
}

func (s sequenceStep) json() bool { return len(s.argv) > 1 && s.argv[0] == "-o" && s.argv[1] == "json" }

// decodedStrings returns every key and string value of a decoded JSON document.
func decodedStrings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			out = append(out, decodedStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range v {
			out = append(out, k)
			out = append(out, decodedStrings(e)...)
		}
		return out
	}
	return nil
}

// recordServers returns the value of every "server" field under a
// notRevoked or supersededTokens list of a decoded JSON document.
func recordServers(v any, under bool) []string {
	switch v := v.(type) {
	case []any:
		var out []string
		for _, e := range v {
			out = append(out, recordServers(e, under)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range v {
			if s, ok := e.(string); ok && k == "server" && under {
				out = append(out, s)
				continue
			}
			out = append(out, recordServers(e, under || k == "notRevoked" || k == "supersededTokens")...)
		}
		return out
	}
	return nil
}

// TestEntryWithAnAtSignAfterTheHostNamesNoHostInAnySequence asserts that for
// an entry whose stored URL reads as a host with a port (canaryURLMisread),
// with a login, a superseded record and a cache record from an earlier
// build, a command that changes or removes the entry, and every command that
// follows it (status, a later login, logout and delete-server), print no part
// of that host or of the URL after it, build no login sequence for it, and
// show the server of every record, key and group as unusable, in the
// decoded JSON and in the text. Not parallel: it sets the process environment.
func TestEntryWithAnAtSignAfterTheHostNamesNoHostInAnySequence(t *testing.T) {
	restoreRawArgv(t)
	update := []string{"config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}
	status := sequenceStep{argv: []string{"config", "status"}}
	statusJSON := sequenceStep{argv: []string{"-o", "json", "config", "status"}}
	login := sequenceStep{argv: []string{"auth", "login", "x", "--token-stdin", "--no-verify"}, stdin: "id:y"}
	logout := sequenceStep{argv: []string{"-o", "json", "auth", "logout", "x"}}
	logoutText := sequenceStep{argv: []string{"auth", "logout", "x"}}
	remove := sequenceStep{argv: []string{"-o", "json", "config", "delete-server", "x"}}
	list := func(n int) sequenceStep {
		return sequenceStep{argv: []string{"auth", "list-tokens"}, listed: n, listing: true}
	}
	listJSON := func(n int) sequenceStep {
		return sequenceStep{argv: []string{"-o", "json", "auth", "list-tokens"}, listed: n, listing: true}
	}
	deleteByKey := func(key string) sequenceStep {
		return sequenceStep{argv: []string{"-o", "json", "auth", "delete-token", key}}
	}
	removeText := sequenceStep{argv: []string{"config", "delete-server", "x"}}
	updateStep := func(json bool) sequenceStep {
		if json {
			return sequenceStep{argv: append([]string{"-o", "json"}, update...)}
		}
		return sequenceStep{argv: update}
	}
	for _, tc := range []struct {
		name  string
		steps []sequenceStep
		// reports is set when a step of JSON reports a record of the seed.
		reports bool
	}{
		{"update-server, json, then status, a login, logout and delete-server", []sequenceStep{updateStep(true), status, statusJSON, login, logout, removeText}, true},
		{"update-server, text, then status, a login, logout and delete-server", []sequenceStep{updateStep(false), statusJSON, status, login, logoutText, remove}, true},
		{"delete-server, json, then status and a logout of every entry", []sequenceStep{remove, status, statusJSON, {argv: []string{"-o", "json", "auth", "logout", "--all"}}}, true},
		{"delete-server, text, then status", []sequenceStep{removeText, statusJSON, status}, false},
		{"logout of every entry, json, then status, update-server, a login, logout and delete-server", []sequenceStep{
			{argv: []string{"-o", "json", "auth", "logout", "--all"}}, status, statusJSON, updateStep(true), status, login, logout, remove}, true},
		{"logout, text, then update-server, status, logout and delete-server", []sequenceStep{logoutText, updateStep(false), statusJSON, logoutText, removeText}, true},
		{"logout, json, then status, logout again and delete-server", []sequenceStep{logout, status, statusJSON, logout, remove}, true},
		{"list-tokens, then delete-token by the key it shows", []sequenceStep{list(1), listJSON(1), deleteByKey(shownKey), listJSON(0), status}, false},
		{"delete-token by the key the cache stored", []sequenceStep{deleteByKey("x@https://admin:1234/c-1"), listJSON(0)}, false},
		{"list-tokens before an update, a logout and a delete-server", []sequenceStep{listJSON(1), list(1), updateStep(true), list(0), logout, remove}, true},
		{"delete-server of an entry after a logout of every entry, text", []sequenceStep{
			{argv: []string{"auth", "logout", "--all"}}, status, removeText}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := secretServer(t, nil, secretBearer)
			fill := secretFiller(t, srv)
			t.Setenv(config.EnvURL, "")
			dir := os.Getenv(config.EnvConfigDir)
			writeSecretStored(t, dir, misreadSequenceSeed(), fill)
			seedCachedTokenAtMisreadHost(t, dir)
			unusable := 0
			for i, step := range tc.steps {
				stdout, stderr, _ := runWithStdin(t, fillAll(fill, step.argv), step.stdin)
				text := withoutRunDirectory(stdout + "\n" + stderr)
				text = strings.ReplaceAll(text, dir, "")
				where := fmt.Sprintf("step %d: %s", i+1, strings.Join(step.argv, " "))
				assert.NotRegexp(t, misreadPattern, text, where)
				// The record of an entry that held an @ after the host gives that
				// reason, not the one of an identity that is no URL.
				for _, other := range []string{"it must be absolute (scheme://host)", "it is not a valid URL"} {
					assert.NotContains(t, text, other, where)
				}
				assertNoCanary(t, urlCanary, map[string]string{where + " stdout": stdout, where + " stderr": stderr})
				if step.listing && !step.json() && step.listed > 0 {
					assert.Contains(t, stdout, "<unusable URL>", "%s: the server of a cache record", where)
				}
				if i == 0 && step.removes() && !step.json() {
					assert.Contains(t, stdout, "tokens on <unusable URL>:", "%s: the group of the tokens names the server as unusable", where)
				}
				if step.json() {
					var doc any
					require.NoError(t, json.Unmarshal([]byte(stdout), &doc), "%s: %s", where, stdout)
					for _, s := range decodedStrings(doc) {
						s = strings.ReplaceAll(s, dir, "")
						assert.NotRegexp(t, misreadPattern, s, where)
					}
					if i == 0 && step.removes() {
						data, _ := doc.(map[string]any)["data"].(map[string]any)
						assert.Equal(t, []any{shownKey}, data["deletedCache"], "%s: the key of the deleted cache record shows the server as unusable", where)
					}
					if step.listing {
						rows, _ := doc.(map[string]any)["data"].([]any)
						require.Len(t, rows, step.listed, where)
						for _, row := range rows {
							assert.Equal(t, "<unusable URL>", row.(map[string]any)["server"], "%s: the server of a cache record", where)
							assert.Equal(t, shownKey, row.(map[string]any)["key"], "%s: the key of a cache record", where)
						}
					}
					if len(step.argv) > 4 && step.argv[2] == "auth" && step.argv[3] == "delete-token" {
						data, _ := doc.(map[string]any)["data"].(map[string]any)
						assert.Equal(t, []any{shownKey}, data["deleted"], "%s: the key given selects the record of the seed", where)
					}
					for _, server := range recordServers(doc, false) {
						// A login made after the entry was changed names the entry's new URL.
						assert.Contains(t, []string{"<unusable URL>", srv.URL}, server, "%s: the server of a record", where)
						if server == "<unusable URL>" {
							unusable++
						}
					}
				}
			}
			if tc.reports {
				assert.Positive(t, unusable, "a record of the seed reads as unusable in the JSON")
			}
		})
	}
}
