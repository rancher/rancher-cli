//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/tidwall/gjson"
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

// cmdFlipStatus drives the shim's status lever: flipstatus <cluster>
// <group>/<version>/<resource> <ns> <name> <json-status-merge>.
func cmdFlipStatus(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 5 {
		ts.Fatalf("usage: flipstatus <cluster> <group>/<version>/<resource> <ns> <name> <json-status-merge>")
	}
	cluster, gvr, ns, name, body := args[0], args[1], args[2], args[3], args[4]

	url := fmt.Sprintf("%s/_test/status/%s/%s/%s/%s", shimURL, cluster, gvr, ns, name)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		ts.Fatalf("flipstatus: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.Fatalf("flipstatus: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ts.Fatalf("flipstatus: shim returned %s", resp.Status)
	}
}
