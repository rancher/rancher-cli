package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byteOrderMark is the UTF-8 byte order mark, the bytes EF BB BF.
const byteOrderMark = "\ufeff"

// stdinMarkRow is one command and flag of the table: a flag whose name ends in
// -stdin, with the function that runs the command on a stdin text and returns
// the values the server received from it, in the order the stdin text holds
// them.
type stdinMarkRow struct {
	command, flag string
	// lines is the number of values the stdin text holds, one per line.
	lines int
	// run runs the command with stdin and returns the values the server
	// received.
	run func(t *testing.T, stdin string) []string
}

// stdinMarkKey is the key of a row and of a flag in the gap check.
func stdinMarkKey(command, flag string) string { return command + " " + flag }

// stdinMarkGaps returns one problem per flag of cmds whose name ends in -stdin
// and has no row in rows, and one per row that names no such flag.
func stdinMarkGaps(cmds []runtime.Descriptor, rows map[string]bool) []string {
	flags := map[string]bool{}
	var problems []string
	for _, d := range cmds {
		for _, f := range d.Flags {
			if !strings.HasSuffix(f.Name, "-stdin") {
				continue
			}
			key := stdinMarkKey(d.Name, f.Name)
			flags[key] = true
			if !rows[key] {
				problems = append(problems, key+" is a flag read from stdin with no row in stdinMarkRows; add one")
			}
		}
	}
	for key := range rows {
		if !flags[key] {
			problems = append(problems, key+" is a row of stdinMarkRows with no flag; remove it")
		}
	}
	slices.Sort(problems)
	return problems
}

// stdinPasswordSeen records the passwords a stub server received: those of
// the requests that change nothing (dry-run) and those that commit.
type stdinPasswordSeen struct {
	mu        sync.Mutex
	dry, real [][]string
}

func (s *stdinPasswordSeen) add(dry bool, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dry {
		s.dry = append(s.dry, values)
		return
	}
	s.real = append(s.real, values)
}

// committed returns the values of the one request that committed, after
// asserting the dry-run requests carried the same values.
func (s *stdinPasswordSeen) committed(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.real, 1, "requests that committed")
	for _, dry := range s.dry {
		assert.Equal(t, s.real[0], dry, "a dry-run request carried other values")
	}
	return s.real[0]
}

// userCreateStub serves the discovery of a server that has the local user
// password workflow, answers every create with 201 and the posted object (a
// name assigned when it has only a generateName), and records the password of
// each create of the password Secret.
func userCreateStub(seen *stdinPasswordSeen) secretStub {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return shortPasswordStub(w, r)
		}
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			writeKubeJSON(w, http.StatusBadRequest, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"bad request","reason":"BadRequest","code":400}`)
			return true
		}
		meta, _ := obj["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			obj["metadata"] = meta
		}
		if name, _ := meta["name"].(string); name == "" {
			meta["name"] = "u-abc12"
		}
		if strings.HasSuffix(r.URL.Path, "/namespaces/cattle-local-user-passwords/secrets") {
			stringData, _ := obj["stringData"].(map[string]any)
			password, _ := stringData["password"].(string)
			seen.add(r.URL.Query().Get("dryRun") == "All", password)
			delete(obj, "stringData")
		}
		out, err := json.Marshal(obj)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		writeKubeJSON(w, http.StatusCreated, string(out))
		return true
	}
}

// passwordChangeStub records the current and new password of each create of a
// PasswordChangeRequest and lets the stub that serves such a server answer it.
func passwordChangeStub(seen *stdinPasswordSeen, withCurrent bool) secretStub {
	inner := currentPasswordStub(0, "")
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/passwordchangerequests") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return true
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var sent struct {
				Spec struct {
					CurrentPassword string `json:"currentPassword"`
					NewPassword     string `json:"newPassword"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return true
			}
			values := []string{sent.Spec.NewPassword}
			if withCurrent {
				values = []string{sent.Spec.CurrentPassword, sent.Spec.NewPassword}
			} else if sent.Spec.CurrentPassword != "" {
				// A current password nobody piped: the values cannot match.
				values = append([]string{sent.Spec.CurrentPassword}, values...)
			}
			seen.add(r.URL.Query().Get("dryRun") == "All", values...)
		}
		return inner(w, r)
	}
}

// bearerSeen records the Authorization header of every SelfSubjectReview
// request the stub server received: the request with which the login checks
// the piped token.
type bearerSeen struct {
	mu      sync.Mutex
	headers []string
}

func (b *bearerSeen) stub() secretStub {
	return func(_ http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/selfsubjectreviews") {
			return false
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.headers = append(b.headers, r.Header.Get("Authorization"))
		return false
	}
}

// piped returns the token of the Bearer headers other than the one the run's
// environment sets: the requests the login sends with the piped token.
func (b *bearerSeen) piped(t *testing.T) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var tokens []string
	for _, h := range b.headers {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if ok && token != secretBearer {
			tokens = append(tokens, token)
		}
	}
	require.NotEmpty(t, tokens, "no request carried the piped token: %v", b.headers)
	for _, token := range tokens {
		assert.Equal(t, tokens[0], token, "requests carried different tokens")
	}
	return tokens[:1]
}

// stdinMarkRows is the table: one row for every flag that reads a secret from
// stdin, each running the real command against a stub server that records the
// value it receives.
func stdinMarkRows() []stdinMarkRow {
	// passwordRow runs argv against the stub that stubFor builds around the
	// recorder and returns the passwords the server received.
	passwordRow := func(stubFor func(*stdinPasswordSeen) secretStub, argv ...string) func(*testing.T, string) []string {
		return func(t *testing.T, stdin string) []string {
			t.Helper()
			seen := &stdinPasswordSeen{}
			secretServer(t, stubFor(seen), secretBearer)
			stdout, stderr, code := runWithStdin(t, append([]string{"-o", "json"}, argv...), stdin)
			require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			return seen.committed(t)
		}
	}
	changeStub := func(withCurrent bool) func(*stdinPasswordSeen) secretStub {
		return func(seen *stdinPasswordSeen) secretStub { return passwordChangeStub(seen, withCurrent) }
	}
	login := func(t *testing.T, stdin string) []string {
		t.Helper()
		seen := &bearerSeen{}
		srv := secretServer(t, seen.stub(), secretBearer)
		fill := secretFiller(t, srv)
		argv := append([]string{"-o", "json"}, fillAll(fill, strings.Fields(loginLine+" --token-stdin"))...)
		// The login may refuse the token the stub server does not know; the
		// request that carried it is what the row observes.
		runWithStdin(t, argv, stdin)
		return seen.piped(t)
	}
	return []stdinMarkRow{
		{command: "auth login", flag: "token-stdin", lines: 1, run: login},
		{command: "create user", flag: "password-stdin", lines: 1,
			run: passwordRow(userCreateStub, "create", "user", "bob", "--password-stdin", "--apply", "--yes")},
		{command: "create password-change-request", flag: "password-stdin", lines: 1,
			run: passwordRow(changeStub(false), "create", "password-change-request", "u-abc", "--password-stdin", "--apply", "--yes")},
		{command: "create password-change-request", flag: "current-password-stdin", lines: 2,
			run: passwordRow(changeStub(true), "create", "password-change-request", "u-abc", "--current-password-stdin", "--password-stdin", "--apply", "--yes")},
	}
}

// stdinMarkValues are the values a row pipes: a token for the login row and
// passwords for the others, in the order of the lines of stdin. plain has no
// byte order mark; inside has one inside each value.
func stdinMarkValues(row stdinMarkRow) (plain, inside []string) {
	if row.command == "auth login" {
		return []string{"id:secretvalue"}, []string{"id:secret" + byteOrderMark + "value"}
	}
	if row.lines == 2 {
		return []string{"current-passw0rd-1", "new-passw0rd-1234"},
			[]string{"current" + byteOrderMark + "-passw0rd-1", "new-passw0rd" + byteOrderMark + "-1234"}
	}
	return []string{"new-passw0rd-1234"}, []string{"new-passw0rd" + byteOrderMark + "-1234"}
}

// stdinMarkText joins values into the text a shell pipes: one line each, with
// the newline (CRLF) Windows PowerShell sends.
func stdinMarkText(values []string) string {
	return strings.Join(values, "\r\n") + "\r\n"
}

// TestEveryStdinFlagHasAByteOrderMarkRow asserts that every flag of the built
// command tree whose name ends in -stdin has a row in the table of
// TestEveryStdinSecretDropsLeadingByteOrderMarks, and that no row is stale.
func TestEveryStdinFlagHasAByteOrderMarkRow(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	var descriptors []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		descriptors = append(descriptors, c.Describe())
	}
	rows := map[string]bool{}
	for _, row := range stdinMarkRows() {
		key := stdinMarkKey(row.command, row.flag)
		assert.False(t, rows[key], "two rows for %s", key)
		rows[key] = true
	}
	assert.Empty(t, stdinMarkGaps(descriptors, rows))
	assert.Equal(t, []string{
		"auth login token-stdin",
		"create password-change-request current-password-stdin",
		"create password-change-request password-stdin",
		"create user password-stdin",
	}, slices.Sorted(maps.Keys(rows)))
}

// TestStdinMarkGapsNamesAFlagWithNoRowAndARowWithNoFlag asserts that the gap
// check reports a flag read from stdin that has no row, and a row whose flag
// is gone.
func TestStdinMarkGapsNamesAFlagWithNoRowAndARowWithNoFlag(t *testing.T) {
	t.Parallel()
	cmds := []runtime.Descriptor{
		{Name: "a b", Flags: []runtime.ArgSpec{{Name: "x-stdin", Type: "bool"}, {Name: "y-stdin", Type: "bool"}, {Name: "other", Type: "string"}}},
		{Name: "c", Flags: []runtime.ArgSpec{{Name: "x-stdin", Type: "bool"}}},
	}
	rows := map[string]bool{"a b x-stdin": true, "c x-stdin": true, "gone-stdin": true, "c other-stdin": true}
	assert.Equal(t, []string{
		"a b y-stdin is a flag read from stdin with no row in stdinMarkRows; add one",
		"c other-stdin is a row of stdinMarkRows with no flag; remove it",
		"gone-stdin is a row of stdinMarkRows with no flag; remove it",
	}, stdinMarkGaps(cmds, rows))
	assert.Empty(t, stdinMarkGaps(cmds, map[string]bool{"a b x-stdin": true, "a b y-stdin": true, "c x-stdin": true}))
}

// TestEveryStdinSecretDropsLeadingByteOrderMarks asserts, for every flag that
// reads a secret from stdin, that the value the server receives is the same
// with no byte order mark, one and two before it on stdin (the bytes Windows
// PowerShell 5.1 sends, one by default and two with its $OutputEncoding set
// to UTF-8), that a mark at the start of line 2 stays, and that a
// mark inside a value stays. Not parallel: it sets the process environment and
// the captured command line.
func TestEveryStdinSecretDropsLeadingByteOrderMarks(t *testing.T) {
	restoreRawArgv(t)
	for _, row := range stdinMarkRows() {
		t.Run(stdinMarkKey(row.command, row.flag), func(t *testing.T) {
			plain, inside := stdinMarkValues(row)
			for _, tc := range []struct {
				name  string
				stdin string
				want  []string
			}{
				{"no mark", stdinMarkText(plain), plain},
				{"one mark", byteOrderMark + stdinMarkText(plain), plain},
				{"two marks", byteOrderMark + byteOrderMark + stdinMarkText(plain), plain},
				{"a mark inside a value", stdinMarkText(inside), inside},
				{"a mark inside a value and one before it", byteOrderMark + stdinMarkText(inside), inside},
				{"a mark inside a value and two before it", byteOrderMark + byteOrderMark + stdinMarkText(inside), inside},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := row.run(t, tc.stdin)
					assert.Equal(t, tc.want, got)
					for _, value := range got {
						assert.False(t, strings.HasPrefix(value, byteOrderMark), "a value starts with a byte order mark")
					}
				})
			}
			if row.lines == 2 {
				t.Run("a mark at the start of line 2", func(t *testing.T) {
					for _, marks := range []string{"", byteOrderMark, byteOrderMark + byteOrderMark} {
						stdin := marks + plain[0] + "\r\n" + byteOrderMark + plain[1] + "\r\n"
						got := row.run(t, stdin)
						assert.Equal(t, []string{plain[0], byteOrderMark + plain[1]}, got, "marks before line 1: %d", len(marks)/len(byteOrderMark))
					}
				})
			}
		})
	}
}
