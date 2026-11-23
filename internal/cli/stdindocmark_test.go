package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stdinDocRow is one flag that reads a document from stdin: the function
// that runs one real command with a text on stdin and returns what the
// server received, and the texts of the document the row pipes.
type stdinDocRow struct {
	flag string
	// document returns the document the row pipes, with CRLF line ends and
	// at least three top-level keys. inside puts a byte order mark inside one
	// value.
	document func(inside bool) string
	// received is what the server received for the document with no mark
	// before it: the names and values that must not change with the marks.
	received func(inside bool) map[string]any
	// empty is the text of the refusal for stdin that has only byte order
	// marks.
	empty string
	// run runs the command with stdin and returns what the server received,
	// or the code and message of the error the command ended with.
	run func(t *testing.T, stdin string) stdinDocRun
}

// stdinDocRun is the outcome of one run of a row.
type stdinDocRun struct {
	received      map[string]any
	code, message string
}

// stdinDocKey is the key of a row and of a flag in the gap check: the flag
// name alone, because the four flags are one reader each whatever command
// carries them.
func stdinDocKey(flag string) string { return flag }

// stdinDocGaps returns one problem per flag of cmds whose description names
// stdin and whose name does not end in -stdin and has no row in rows, and one
// per row that names no such flag.
func stdinDocGaps(cmds []runtime.Descriptor, rows map[string]bool) []string {
	flags := map[string]bool{}
	var problems []string
	for _, d := range cmds {
		for _, f := range d.Flags {
			if strings.HasSuffix(f.Name, "-stdin") || !strings.Contains(f.Desc, "stdin") {
				continue
			}
			key := stdinDocKey(f.Name)
			flags[key] = true
			if !rows[key] {
				problems = append(problems, key+" is a flag read from stdin with no row in stdinDocRows; add one")
			}
		}
	}
	for key := range rows {
		if !flags[key] {
			problems = append(problems, key+" is a row of stdinDocRows with no flag; remove it")
		}
	}
	slices.Sort(problems)
	return problems
}

// docWrite is one write the stub cluster received: a create or an update of a
// ConfigMap.
type docWrite struct {
	dry bool
	obj map[string]any
}

// docSeen records the writes of a stub cluster that serves the ConfigMap "app"
// and accepts a ConfigMap create.
type docSeen struct {
	mu     sync.Mutex
	writes []docWrite
}

// committed returns the object of the one write that committed, after
// asserting every dry-run write carried the same object.
func (s *docSeen) committed(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var committed []map[string]any
	for _, w := range s.writes {
		if !w.dry {
			committed = append(committed, w.obj)
		}
	}
	require.Len(t, committed, 1, "writes that committed")
	for _, w := range s.writes {
		if w.dry {
			assert.Equal(t, committed[0], w.obj, "a dry-run write carried another object")
		}
	}
	return committed[0]
}

// docStub serves the ConfigMap "app" with two keys and a label, answers a
// ConfigMap create or update with the object it received (with a resource
// version), and records each of those writes.
func docStub(seen *docSeen) secretStub {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/configmaps") {
			return false
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/configmaps/app"):
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default","resourceVersion":"7","labels":{"tier":"web"}},"data":{"a":"1","b":"2"}}`)
		case r.Method == http.MethodPut || r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return true
			}
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return true
			}
			seen.mu.Lock()
			seen.writes = append(seen.writes, docWrite{dry: r.URL.Query().Get("dryRun") != "", obj: obj})
			seen.mu.Unlock()
			meta, _ := obj["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				obj["metadata"] = meta
			}
			meta["resourceVersion"] = "8"
			out, err := json.Marshal(obj)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return true
			}
			status := http.StatusOK
			if r.Method == http.MethodPost {
				status = http.StatusCreated
			}
			writeKubeJSON(w, status, string(out))
		default:
			return false
		}
		return true
	}
}

// stdinDocFailure returns the outcome of a run that ended with an error: the
// code and message of the error envelope.
func stdinDocFailure(t *testing.T, stderr string, code int) stdinDocRun {
	t.Helper()
	require.NotZero(t, code, "stderr: %s", stderr)
	ce := renderedError(t, "json", stderr)
	return stdinDocRun{code: string(ce.Code), message: ce.Message}
}

// stdinDocRows is the table: one row for every flag that reads a document from
// stdin, each running the real command against a stub server that records the
// document it receives.
func stdinDocRows() []stdinDocRow {
	dig := func(obj map[string]any, path ...string) any {
		var cur any = obj
		for _, p := range path {
			m, _ := cur.(map[string]any)
			cur = m[p]
		}
		return cur
	}
	return []stdinDocRow{
		{
			flag: "values",
			document: func(inside bool) string {
				tag := "2"
				if inside {
					tag = "2" + byteOrderMark + "b"
				}
				return "replicaCount: 3\r\nimage:\r\n  repository: nginx\r\n  tag: \"" + tag + "\"\r\nservice:\r\n  port: 80\r\n"
			},
			received: func(inside bool) map[string]any {
				tag := "2"
				if inside {
					tag = "2" + byteOrderMark + "b"
				}
				return map[string]any{
					"replicaCount": float64(3),
					"image":        map[string]any{"repository": "nginx", "tag": tag},
					"service":      map[string]any{"port": float64(80)},
				}
			},
			empty: "holds no YAML document",
			run: func(t *testing.T, stdin string) stdinDocRun {
				t.Helper()
				s := newAppServer(t, "community")
				_, stderr, err := appRunStdin(t, s, stdin, withArgs(appInstallArgs, "--values", "-", "--apply", "-o", "json")...)
				if err != nil {
					ce := decodeErrorEnvelope(t, stderr)
					return stdinDocRun{code: ce.Code, message: ce.Message}
				}
				posts := s.recorded()
				require.Len(t, posts, 1)
				charts, _ := posts[0].body["charts"].([]any)
				require.Len(t, charts, 1)
				values, _ := charts[0].(map[string]any)["values"].(map[string]any)
				delete(values, "global")
				return stdinDocRun{received: values}
			},
		},
		{
			flag: "patch-file",
			document: func(inside bool) string {
				a := "9"
				if inside {
					a = "9" + byteOrderMark + "z"
				}
				return "{\r\n\"data\":{\"a\":\"" + a + "\",\"c\":\"3\"},\r\n\"metadata\":{\"labels\":{\"tier\":\"db\"}},\r\n\"binaryData\":{\"d\":\"AAE=\"}\r\n}\r\n"
			},
			received: func(inside bool) map[string]any {
				a := "9"
				if inside {
					a = "9" + byteOrderMark + "z"
				}
				return map[string]any{"data.a": a, "data.b": "2", "data.c": "3", "labels.tier": "db", "binaryData.d": "AAE="}
			},
			empty: "invalid patch: the patch is empty",
			run: func(t *testing.T, stdin string) stdinDocRun {
				t.Helper()
				seen := &docSeen{}
				secretServer(t, docStub(seen), secretBearer)
				_, stderr, code := runWithStdin(t, []string{"-o", "json", "patch", "configmap", "app", "--cluster", "c-x", "--namespace", "default",
					"--type", "merge", "--patch-file", "-", "--apply", "--yes"}, stdin)
				if code != 0 {
					return stdinDocFailure(t, stderr, code)
				}
				obj := seen.committed(t)
				return stdinDocRun{received: map[string]any{
					"data.a":       dig(obj, "data", "a"),
					"data.b":       dig(obj, "data", "b"),
					"data.c":       dig(obj, "data", "c"),
					"labels.tier":  dig(obj, "metadata", "labels", "tier"),
					"binaryData.d": dig(obj, "binaryData", "d"),
				}}
			},
		},
		{
			flag: "filename",
			document: func(inside bool) string {
				a := "x"
				if inside {
					a = "x" + byteOrderMark + "y"
				}
				return "apiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n  name: made\r\n  namespace: default\r\ndata:\r\n  a: \"" + a + "\"\r\n"
			},
			received: func(inside bool) map[string]any {
				a := "x"
				if inside {
					a = "x" + byteOrderMark + "y"
				}
				return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "made", "data.a": a}
			},
			empty: "manifest must contain exactly one document, found 0",
			run: func(t *testing.T, stdin string) stdinDocRun {
				t.Helper()
				seen := &docSeen{}
				secretServer(t, docStub(seen), secretBearer)
				_, stderr, code := runWithStdin(t, []string{"-o", "json", "create", "resource", "--cluster", "c-x", "-f", "-", "--apply"}, stdin)
				if code != 0 {
					return stdinDocFailure(t, stderr, code)
				}
				obj := seen.committed(t)
				return stdinDocRun{received: map[string]any{
					"apiVersion": dig(obj, "apiVersion"),
					"kind":       dig(obj, "kind"),
					"name":       dig(obj, "metadata", "name"),
					"data.a":     dig(obj, "data", "a"),
				}}
			},
		},
		{
			flag: "from-file",
			document: func(inside bool) string {
				token := "kubeconfig-user-x:abc"
				if inside {
					token = "kubeconfig-user-x:a" + byteOrderMark + "bc"
				}
				return "apiVersion: v1\r\nkind: Config\r\nclusters:\r\n- name: c\r\nusers:\r\n- name: u\r\n  user:\r\n    token: " + token + "\r\n"
			},
			received: func(inside bool) map[string]any {
				token := "kubeconfig-user-x:abc"
				if inside {
					token = "kubeconfig-user-x:a" + byteOrderMark + "bc"
				}
				return map[string]any{"kubeconfigContent": "apiVersion: v1\r\nkind: Config\r\nclusters:\r\n- name: c\r\nusers:\r\n- name: u\r\n  user:\r\n    token: " + token}
			},
			empty: "for kubeconfigContent is not a kubeconfig",
			run: func(t *testing.T, stdin string) stdinDocRun {
				t.Helper()
				var posts []string
				_, stderr, code, _ := ccRun(t, ccStub(ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde"}`}, &posts, false),
					[]string{"-o", "json", "create", "cloud-credential", "harvester", "cc-harvester", "--from-file", "kubeconfigContent=-", "--apply", "--wait=false"}, stdin)
				if code != 0 {
					return stdinDocFailure(t, stderr, code)
				}
				require.Len(t, posts, 1)
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(posts[0]), &body))
				config, _ := body["harvestercredentialConfig"].(map[string]any)
				return stdinDocRun{received: map[string]any{"kubeconfigContent": config["kubeconfigContent"]}}
			},
		},
	}
}

// TestEveryStdinDocumentFlagHasAByteOrderMarkRow asserts that every flag of
// the built command tree whose description names stdin and whose name does not
// end in -stdin has a row in the table of
// TestEveryStdinDocumentDropsLeadingByteOrderMarks, that no row is stale, and
// that the set of the flags is the four that read a document. The check is
// by flag name: a new command that reuses one of the names has a row already,
// and the source scan of the readers covers it.
func TestEveryStdinDocumentFlagHasAByteOrderMarkRow(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	var descriptors []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		descriptors = append(descriptors, c.Describe())
	}
	rows := map[string]bool{}
	for _, row := range stdinDocRows() {
		key := stdinDocKey(row.flag)
		assert.False(t, rows[key], "two rows for %s", key)
		rows[key] = true
	}
	assert.Empty(t, stdinDocGaps(descriptors, rows))
	assert.Equal(t, []string{"filename", "from-file", "patch-file", "values"}, slices.Sorted(maps.Keys(rows)))
}

// TestStdinDocGapsNamesAFlagWithNoRowAndARowWithNoFlag asserts that the gap
// check reports a flag read from stdin that has no row, and a row whose flag
// is gone, and skips a flag that does not name stdin and a flag named
// -stdin.
func TestStdinDocGapsNamesAFlagWithNoRowAndARowWithNoFlag(t *testing.T) {
	t.Parallel()
	cmds := []runtime.Descriptor{
		{Name: "a b", Flags: []runtime.ArgSpec{
			{Name: "x", Type: "string", Desc: "a file, or - for stdin"},
			{Name: "y", Type: "string", Desc: "the file, or - for stdin"},
			{Name: "other", Type: "string", Desc: "no input"},
			{Name: "z-stdin", Type: "bool", Desc: "read the value from stdin"},
		}},
		{Name: "c", Flags: []runtime.ArgSpec{{Name: "x", Type: "string", Desc: "a file, or - for stdin"}}},
	}
	rows := map[string]bool{"x": true, "gone": true, "other": true}
	assert.Equal(t, []string{
		"gone is a row of stdinDocRows with no flag; remove it",
		"other is a row of stdinDocRows with no flag; remove it",
		"y is a flag read from stdin with no row in stdinDocRows; add one",
	}, stdinDocGaps(cmds, rows))
	assert.Empty(t, stdinDocGaps(cmds, map[string]bool{"x": true, "y": true}))
}

// TestEveryStdinDocumentDropsLeadingByteOrderMarks asserts, for every flag
// that reads a document from stdin, that the server receives the same
// document with no byte order mark, one, two and three before it on stdin (the
// bytes Windows PowerShell 5.1 sends, one by default and two with its
// $OutputEncoding set to [System.Text.Encoding]::UTF8), that a mark inside
// a value stays with none and with two before it, and that stdin of three
// marks and nothing else gets the refusal an empty document gets. The
// comparison is of the names and values received, because a decoder that
// keeps a second mark changes the names and not their number. Not parallel:
// it sets the process environment and the captured command line.
func TestEveryStdinDocumentDropsLeadingByteOrderMarks(t *testing.T) {
	restoreRawArgv(t)
	for _, row := range stdinDocRows() {
		t.Run(stdinDocKey(row.flag), func(t *testing.T) {
			for marks := range 4 {
				t.Run(fmt.Sprintf("%d marks", marks), func(t *testing.T) {
					got := row.run(t, strings.Repeat(byteOrderMark, marks)+row.document(false))
					require.Empty(t, got.code, "the command was refused: %s", got.message)
					assert.Equal(t, row.received(false), got.received)
				})
			}
			for _, marks := range []int{0, 2} {
				t.Run(fmt.Sprintf("a mark inside a value after %d marks", marks), func(t *testing.T) {
					got := row.run(t, strings.Repeat(byteOrderMark, marks)+row.document(true))
					require.Empty(t, got.code, "the command was refused: %s", got.message)
					assert.Equal(t, row.received(true), got.received)
				})
			}
			t.Run("three marks and nothing else", func(t *testing.T) {
				got := row.run(t, strings.Repeat(byteOrderMark, 3))
				assert.Equal(t, string(clierr.CodeValidation), got.code)
				assert.Contains(t, got.message, row.empty)
			})
		})
	}
}
