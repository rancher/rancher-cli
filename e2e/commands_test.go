//go:build e2e

// Deliberately plain-stdlib (no testify): the fatal checks here run on
// *testscript.TestScript, which does not satisfy testify's TestingT (no
// Errorf), so conversion would be a type error, not a style choice.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rogpeppe/go-internal/testscript"
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

// cmdPathmatch asserts the named output stream contains a file path:
// pathmatch <stdout|stderr> <path>. The path argument is compared with its
// separators converted to the native form — the stdout/stderr assertions
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
	defer resp.Body.Close()
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

// cmdSeedExecCache pre-warms the exec-credential cache `rancher auth
// get-token` reads: seedexeccache <name> <cluster>. Scenarios that want
// warm-cache state without exercising login use this instead of the
// façade's cold mint/login paths. Seeding through the real
// config.CacheKey/SetCachedCred keeps the key composition and file format
// guaranteed-consistent with what get-token reads; a hand-written fixture
// can't work here since the cache key embeds the façade's host:port, which
// is dynamic per test run AND per-script now (FA-3) — the host comes from
// this script's own TEST_RANCHER_URL, not the global shim.
func cmdSeedExecCache(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: seedexeccache <name> <cluster>")
	}
	name, cluster := args[0], args[1]

	rawURL := ts.Getenv("TEST_RANCHER_URL")
	if _, err := url.Parse(rawURL); err != nil {
		ts.Fatalf("seedexeccache: parsing TEST_RANCHER_URL: %v", err)
	}
	srvID := config.CanonicalServerIdentity(rawURL)
	store := config.NewFileStore(ts.Getenv("RANCHER_CLI_CONFIG_DIR"))
	key := config.CacheKey(name, srvID, cluster)
	if err := store.SetCachedCred(key, testToken, time.Now().Add(24*time.Hour), name, srvID, cluster); err != nil {
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
