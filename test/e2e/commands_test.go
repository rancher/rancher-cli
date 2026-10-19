//go:build e2e

// The testscript command functions (every cmd* func taking a
// *testscript.TestScript) are deliberately plain-stdlib: their fatal
// checks run on *testscript.TestScript, which does not satisfy testify's
// TestingT (no Errorf), so conversion would be a type error, not a style
// choice. The *testing.T unit tests below them use testify like the rest
// of the suite.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// cmdJSONQuery asserts a gjson path inside a JSON (or, with -yaml, YAML)
// document equals the expected value: jsonquery [-yaml] <file> <path> <want>.
func cmdJSONQuery(ts *testscript.TestScript, neg bool, args []string) {
	asYAML := false
	if len(args) > 0 && args[0] == "-yaml" {
		asYAML = true
		args = args[1:]
	}
	if len(args) != 3 {
		ts.Fatalf("usage: jsonquery [-yaml] <file> <gjson-path> <expected>")
	}
	file, path, want := args[0], args[1], args[2]

	doc := ts.ReadFile(file)
	if asYAML {
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			ts.Fatalf("jsonquery: converting %s from YAML: %v", file, err)
		}
		doc = string(j)
	}

	got := gjson.Get(doc, path).String()
	if match := got == want; match == neg {
		if neg {
			ts.Fatalf("jsonquery %s %q: got %q, want anything but %q", file, path, got, want)
		}
		ts.Fatalf("jsonquery %s %q: got %q, want %q", file, path, got, want)
	}
}

// cmdJSONGet writes the value a gjson path selects to a file, so a later
// command can consume it: jsonget <file> <gjson-path> <output-file>. A path
// that selects nothing is a failure - a script that carries a value forward
// must never carry an empty one.
func cmdJSONGet(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("jsonget does not support the ! prefix")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: jsonget <file> <gjson-path> <output-file>")
	}
	file, path, out := args[0], args[1], args[2]
	res := gjson.Get(ts.ReadFile(file), path)
	if !res.Exists() {
		ts.Fatalf("jsonget %s %q: path selects nothing", file, path)
	}
	if res.String() == "" {
		ts.Fatalf("jsonget %s %q: selected an empty value", file, path)
	}
	if err := os.WriteFile(ts.MkAbs(out), []byte(res.String()), 0o600); err != nil {
		ts.Fatalf("jsonget: writing %s: %v", out, err)
	}
}

// cmdSetenvFrom sets an environment variable for the rest of the script
// from a file's content, trimmed: setenvfrom <NAME> <file>.
func cmdSetenvFrom(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("setenvfrom does not support the ! prefix")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: setenvfrom <NAME> <file>")
	}
	value := strings.TrimSpace(ts.ReadFile(args[1]))
	if value == "" {
		ts.Fatalf("setenvfrom %s: %s is empty", args[0], args[1])
	}
	ts.Setenv(args[0], value)
}

// recordingT records what a testscript run logs and whether it failed,
// standing in for the *testing.T a script would otherwise fail. A command's
// own failure path can only be exercised through a running script -
// *testscript.TestScript has no exported constructor.
type recordingT struct {
	mu     sync.Mutex
	log    strings.Builder
	failed bool
}

func (r *recordingT) Skip(args ...any) {
	r.Log(args...)
	runtime.Goexit()
}

func (r *recordingT) Fatal(args ...any) {
	r.Log(args...)
	r.FailNow()
}

func (r *recordingT) Parallel() {}

func (r *recordingT) Log(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(&r.log, args...)
}

func (r *recordingT) FailNow() {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recordingT) Verbose() bool { return false }

// Run executes f on its own goroutine so the runtime.Goexit a failure ends
// with unwinds that goroutine instead of the test's.
func (r *recordingT) Run(name string, f func(testscript.T)) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(r)
	}()
	<-done
}

func (r *recordingT) result() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.String(), r.failed
}

// runInlineScript runs script through testscript with only the two commands
// under test registered, and reports its log and whether it failed.
func runInlineScript(t *testing.T, script string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "case.txtar"), []byte(script), 0o600))
	rec := &recordingT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testscript.RunT(rec, testscript.Params{
			Dir: dir,
			Cmds: map[string]func(*testscript.TestScript, bool, []string){
				"jsonget":    cmdJSONGet,
				"setenvfrom": cmdSetenvFrom,
				"waitobj":    cmdWaitObj,
			},
		})
	}()
	<-done
	return rec.result()
}

// TestJSONGetWritesSelectedValue asserts that the selected value reaches the
// output file with no trailing newline, so a later setenvfrom carries it
// verbatim.
func TestJSONGetWritesSelectedValue(t *testing.T) {
	t.Parallel()
	log, failed := runInlineScript(t, `jsonget doc.json 'data.#(spec.description%"*[op:abc]*").metadata.name' out.txt
grep '\Afac-token-2\z' out.txt

-- doc.json --
{"data":[{"metadata":{"name":"fac-token-1"},"spec":{"description":"login [op:zzz]"}},{"metadata":{"name":"fac-token-2"},"spec":{"description":"login [op:abc]"}}]}
`)
	assert.False(t, failed, log)
}

// TestJSONGetFailures asserts the three ways the command refuses to carry a
// value forward.
func TestJSONGetFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "wrong arity",
			script: `jsonget doc.json data.name

-- doc.json --
{"data":{"name":"x"}}
`,
			want: "usage: jsonget <file> <gjson-path> <output-file>",
		},
		{
			name: "path selects nothing",
			script: `jsonget doc.json data.missing out.txt

-- doc.json --
{"data":{"name":"x"}}
`,
			want: "path selects nothing",
		},
		{
			name: "empty value",
			script: `jsonget doc.json data.name out.txt

-- doc.json --
{"data":{"name":""}}
`,
			want: "selected an empty value",
		},
		{
			name: "negated",
			script: `! jsonget doc.json data.name out.txt

-- doc.json --
{"data":{"name":"x"}}
`,
			want: "jsonget does not support the ! prefix",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, failed := runInlineScript(t, tc.script)
			assert.True(t, failed, log)
			assert.Contains(t, log, tc.want)
		})
	}
}

// TestSetenvFromSetsTrimmedValue asserts the round trip the recovery scenario
// depends on: what jsonget wrote is what later lines expand.
func TestSetenvFromSetsTrimmedValue(t *testing.T) {
	t.Parallel()
	log, failed := runInlineScript(t, `jsonget doc.json data.name value.txt
setenvfrom LEFTOVER value.txt
cp value.txt $LEFTOVER.out
exists fac-token-1.out

-- doc.json --
{"data":{"name":"fac-token-1"}}
`)
	assert.False(t, failed, log)
}

// TestSetenvFromFailures asserts that a missing argument or an empty file stops
// the script rather than setting an empty variable.
func TestSetenvFromFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "wrong arity",
			script: `setenvfrom LEFTOVER

-- value.txt --
fac-token-1
`,
			want: "usage: setenvfrom <NAME> <file>",
		},
		{
			name: "empty file",
			script: `setenvfrom LEFTOVER value.txt

-- value.txt --
   
`,
			want: "value.txt is empty",
		},
		{
			name: "negated",
			script: `! setenvfrom LEFTOVER value.txt

-- value.txt --
fac-token-1
`,
			want: "setenvfrom does not support the ! prefix",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, failed := runInlineScript(t, tc.script)
			assert.True(t, failed, log)
			assert.Contains(t, log, tc.want)
		})
	}
}

// TestWaitObj asserts waitobj's outcomes: it returns once the object has
// the path, fails naming the path and the timeout when the path never
// appears or the object never exists, and refuses the ! prefix and a
// malformed call.
func TestWaitObj(t *testing.T) {
	t.Parallel()
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "waitobj-cm", "namespace": "default",
			"annotations": map[string]any{"example.io/request": "true"},
		},
	}}
	_, err := dynCli.Resource(gvr).Namespace("default").Create(t.Context(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = dynCli.Resource(gvr).Namespace("default").Delete(context.Background(), "waitobj-cm", metav1.DeleteOptions{}) //nolint:usetesting // t.Context() is canceled before cleanups run
	})

	for _, tc := range []struct {
		name, script, want string
		fails              bool
	}{
		{name: "the path is present", script: `waitobj core/v1/configmaps default waitobj-cm 'metadata.annotations.example\.io/request'`},
		{name: "the path never appears", fails: true,
			script: `waitobj -timeout 200ms core/v1/configmaps default waitobj-cm 'metadata.annotations.example\.io/other'`,
			want:   `metadata.annotations.example\.io/other is not present after 200ms`},
		{name: "the object never exists", fails: true,
			script: `waitobj -timeout 200ms core/v1/configmaps default waitobj-missing metadata.name`,
			want:   "metadata.name is not present after 200ms"},
		{name: "the ! prefix", fails: true, script: `! waitobj core/v1/configmaps default waitobj-cm metadata.name`,
			want: "waitobj does not support the ! prefix"},
		{name: "a missing argument", fails: true, script: `waitobj core/v1/configmaps default waitobj-cm`,
			want: "usage: waitobj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, failed := runInlineScript(t, tc.script+"\n")
			assert.Equal(t, tc.fails, failed, log)
			assert.Contains(t, log, tc.want)
		})
	}
}

// TestPollObjPath asserts pollObjPath's handling of GET errors: a transient
// error of any kind is retried until the path appears, the timeout error
// names the last error, and each GET runs under a deadline.
func TestPollObjPath(t *testing.T) {
	t.Parallel()
	withPath := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "x"}}}

	t.Run("a transient error is retried", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := pollObjPath(func(context.Context) (*unstructured.Unstructured, error) {
			calls++
			if calls < 3 {
				return nil, errors.New("connection reset")
			}
			return withPath, nil
		}, "metadata.name", 10*time.Second)
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	})

	t.Run("the timeout error names the last error", func(t *testing.T) {
		t.Parallel()
		err := pollObjPath(func(context.Context) (*unstructured.Unstructured, error) {
			return nil, errors.New("connection reset")
		}, "metadata.name", 100*time.Millisecond)
		require.ErrorContains(t, err, "metadata.name is not present after 100ms")
		assert.ErrorContains(t, err, "last error: connection reset")
	})

	t.Run("each GET runs under a deadline", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		var deadlines []time.Time
		err := pollObjPath(func(ctx context.Context) (*unstructured.Unstructured, error) {
			d, ok := ctx.Deadline()
			require.True(t, ok, "a GET without a deadline can outlive the timeout")
			deadlines = append(deadlines, d)
			return nil, errors.New("slow")
		}, "metadata.name", 100*time.Millisecond)
		require.Error(t, err)
		require.NotEmpty(t, deadlines)
		for _, d := range deadlines {
			assert.Less(t, d.Sub(start), 100*time.Millisecond+10*time.Second, "a GET may run at most a few seconds past the timeout")
		}
	})
}

// cmdPathmatch asserts the named output stream contains a file path:
// pathmatch <stdout|stderr> <path>. The path argument is compared with its
// separators converted to the native form - the stdout/stderr assertions
// are regexps, and a Windows path's backslashes are invalid escapes there,
// so a script asserting on a printed native path needs this command to stay
// portable.
func cmdPathmatch(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 || (args[0] != "stdout" && args[0] != "stderr") {
		ts.Fatalf("usage: pathmatch <stdout|stderr> <path>")
	}
	stream, want := args[0], filepath.FromSlash(args[1])
	got := ts.ReadFile(stream)
	if match := strings.Contains(got, want); match == neg {
		if neg {
			ts.Fatalf("pathmatch %s: %q unexpectedly contains %q", stream, got, want)
		}
		ts.Fatalf("pathmatch %s: %q does not contain %q", stream, got, want)
	}
}

// clusterScopedNSSegment is the URL-safe placeholder for a cluster-scoped
// resource's <ns> path segment: a literal "." (seedobj's convention for
// "no namespace") or an empty segment are both dot-segment/redundant-slash
// forms that path normalization strips before the handler ever sees them,
// silently shifting the split part count. An ordinary token round-trips
// untouched.
const clusterScopedNSSegment = "_"

// cmdFlipStatus drives the shim's status lever: flipstatus <cluster>
// <group>/<version>/<resource> <ns> <name> <json-status-merge>. Use "." for
// <ns> on a cluster-scoped resource (matches seedobj's convention).
func cmdFlipStatus(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 5 {
		ts.Fatalf("usage: flipstatus <cluster> <group>/<version>/<resource> <ns> <name> <json-status-merge>")
	}
	cluster, gvr, ns, name, body := args[0], args[1], args[2], args[3], args[4]
	if ns == "." {
		ns = clusterScopedNSSegment
	}

	reqURL := fmt.Sprintf("%s/_test/status/%s/%s/%s/%s", shimURL, cluster, gvr, ns, name)
	req, err := http.NewRequest(http.MethodPut, reqURL, strings.NewReader(body))
	if err != nil {
		ts.Fatalf("flipstatus: %v", err)
	}
	resp, err := shimClient.Do(req)
	if err != nil {
		ts.Fatalf("flipstatus: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		ts.Fatalf("flipstatus: shim returned %s", resp.Status)
	}
}

// cmdRawGet writes an object's raw API JSON to a file so a scenario can
// assert a precondition on the served object (e.g. managedFields IS
// present) before checking what the CLI renders:
// rawget <group>/<version>/<resource> <ns> <name> <file>. "core" is the
// empty-group sentinel, as in flipstatus.
func cmdRawGet(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 4 {
		ts.Fatalf("usage: rawget <group>/<version>/<resource> <ns> <name> <file>")
	}
	parts := strings.SplitN(args[0], "/", 3)
	if len(parts) != 3 {
		ts.Fatalf("rawget: malformed gvr %q, want group/version/resource", args[0])
	}
	group, version, resource := parts[0], parts[1], parts[2]
	if group == "core" {
		group = ""
	}
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	obj, err := dynCli.Resource(gvr).Namespace(args[1]).Get(context.Background(), args[2], metav1.GetOptions{}) // testscript command callback: no *testing.T in scope
	if err != nil {
		ts.Fatalf("rawget %s %s/%s: %v", args[0], args[1], args[2], err)
	}
	b, err := json.Marshal(obj.Object)
	if err != nil {
		ts.Fatalf("rawget: marshal: %v", err)
	}
	if err := os.WriteFile(ts.MkAbs(args[3]), b, 0o644); err != nil {
		ts.Fatalf("rawget: write %s: %v", args[3], err)
	}
}

// cmdWaitObj waits until an object exists and a gjson path in its JSON is
// present: waitobj [-timeout <duration>] <group>/<version>/<resource> <ns>
// <name> <path>. "core" is the empty-group sentinel and "." the
// cluster-scoped <ns>, as in seedobj. A scenario uses it instead of a fixed
// sleep when a background command must reach a state before the scenario
// acts. It keeps polling through any GET error and fails when the path is
// still absent after the timeout (default one minute, a bound on a correct
// wait, never the wait itself), naming the last error.
func cmdWaitObj(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("waitobj does not support the ! prefix")
	}
	timeout := time.Minute
	if len(args) >= 2 && args[0] == "-timeout" {
		d, err := time.ParseDuration(args[1])
		if err != nil {
			ts.Fatalf("waitobj: -timeout: %v", err)
		}
		timeout, args = d, args[2:]
	}
	if len(args) != 4 {
		ts.Fatalf("usage: waitobj [-timeout <duration>] <group>/<version>/<resource> <ns> <name> <path>")
	}
	parts := strings.SplitN(args[0], "/", 3)
	if len(parts) != 3 {
		ts.Fatalf("waitobj: malformed gvr %q, want group/version/resource", args[0])
	}
	group, version, resource := parts[0], parts[1], parts[2]
	if group == "core" {
		group = ""
	}
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	ns, name, path := args[1], args[2], args[3]
	if ns == "." {
		ns = ""
	}
	get := func(ctx context.Context) (*unstructured.Unstructured, error) {
		return dynCli.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	if err := pollObjPath(get, path, timeout); err != nil {
		ts.Fatalf("waitobj %s %s/%s: %v", args[0], args[1], name, err)
	}
}

// getSlack is how long a single GET of pollObjPath may run past the
// timeout. A GET bound exactly to the deadline fails with the client's own
// rate-limiter error near the deadline instead of the timeout message; the
// slack leaves that to the loop and still bounds a stalled request.
const getSlack = 5 * time.Second

// pollObjPath calls get every 25 ms until the object it returns has path,
// or the timeout passes. Every error from get, including a transient one,
// is retried; the last one is named in the timeout error.
func pollObjPath(get func(context.Context) (*unstructured.Unstructured, error), path string, timeout time.Duration) error {
	end := time.Now().Add(timeout)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(end)+getSlack) // testscript command callback: no *testing.T in scope
		obj, err := get(ctx)
		cancel()
		if err == nil {
			b, merr := json.Marshal(obj.Object)
			if merr != nil {
				return fmt.Errorf("marshal: %w", merr)
			}
			if gjson.GetBytes(b, path).Exists() {
				return nil
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		select {
		case <-deadline.C:
			msg := fmt.Sprintf("%s is not present after %s", path, timeout)
			if lastErr != nil {
				msg += fmt.Sprintf(" (last error: %v)", lastErr)
			}
			return errors.New(msg)
		case <-tick.C:
		}
	}
}

// cmdSeedExecCache pre-warms the exec-credential cache `rancher auth
// get-token` reads: seedexeccache [-token <token>] <name> <cluster>
// [<user-id>], the slot of a call with --user-id when one is given, holding
// the harness token unless -token names another. Scenarios that want
// warm-cache state without exercising login use this instead of the
// facade's cold create/login paths. Seeding through the real
// config.CacheKey/SetCachedCred keeps the key composition and file format
// guaranteed-consistent with what get-token reads; a hand-written fixture
// can't work here since the cache key embeds the facade's host:port, which
// is dynamic per test run AND per-script now - the host comes from
// this script's own TEST_RANCHER_URL, not the global shim.
func cmdSeedExecCache(ts *testscript.TestScript, neg bool, args []string) {
	tok := testToken
	if len(args) >= 2 && args[0] == "-token" {
		tok, args = args[1], args[2:]
	}
	if len(args) != 2 && len(args) != 3 {
		ts.Fatalf("usage: seedexeccache [-token <token>] <name> <cluster> [<user-id>]")
	}
	name, cluster := args[0], args[1]
	userID := ""
	if len(args) == 3 {
		userID = args[2]
	}

	rawURL := ts.Getenv("TEST_RANCHER_URL")
	if _, err := url.Parse(rawURL); err != nil {
		ts.Fatalf("seedexeccache: parsing TEST_RANCHER_URL: %v", err)
	}
	srvID := config.CanonicalServerIdentity(rawURL)
	dir := ts.Getenv("RANCHER_CLI_CONFIG_DIR")
	store := config.NewFileStore(dir)
	key := config.CacheKeyFor(name, srvID, cluster, userID)
	// The record carries the binding a real invocation would write for
	// this --name: the directory, and, when the name identifies an entry,
	// that entry and its cache binding id, as a call answered by the
	// entry's own login writes it; else what the lookup of a call that
	// identifies none reads, and the user id the key carries. Without it
	// the record names no owner and every reader treats it as another
	// directory's.
	binding := config.CacheBindingForTest(dir, "", "")
	if cfg, err := config.Load(config.FilePath(dir)); err == nil {
		binding = config.CacheBindingForSeedOf(dir, cfg, name, srvID, cluster, userID)
	}
	if err := store.SetCachedCred(key, tok, time.Now().Add(24*time.Hour), name, srvID, cluster, binding); err != nil {
		ts.Fatalf("seedexeccache: %v", err)
	}
}

// cmdSeedObj creates an object directly via the envtest dynamic client:
// seedobj <group>/<version>/<resource> <ns> <file>. "core" is the
// empty-group sentinel (matches flipstatus/rawget convention). Use "." for
// <ns> when the resource is cluster-scoped (namespace is omitted from the
// API call). Reads a YAML or JSON manifest from the embedded txtar file.
func cmdSeedObj(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 3 {
		ts.Fatalf("usage: seedobj <group>/<version>/<resource> <ns> <file>")
	}
	parts := strings.SplitN(args[0], "/", 3)
	if len(parts) != 3 {
		ts.Fatalf("seedobj: malformed gvr %q, want group/version/resource", args[0])
	}
	group, version, resource := parts[0], parts[1], parts[2]
	if group == "core" {
		group = ""
	}
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	ns := args[1]
	if ns == "." {
		ns = ""
	}
	raw := ts.ReadFile(args[2])
	j, err := yaml.YAMLToJSON([]byte(raw))
	if err != nil {
		ts.Fatalf("seedobj: yaml-to-json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(j, &m); err != nil {
		ts.Fatalf("seedobj: unmarshal: %v", err)
	}
	obj := &unstructured.Unstructured{Object: m}
	if ns == "" {
		if _, err := dynCli.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // testscript command callback: no *testing.T in scope
			ts.Fatalf("seedobj: %v", err)
		}
	} else {
		if _, err := dynCli.Resource(gvr).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil { // testscript command callback: no *testing.T in scope
			ts.Fatalf("seedobj: %v", err)
		}
	}
}
