package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

const authConfigFixturesPath = "../engine/kinds/testdata/authconfig-fixtures.json"

// authConfigFixture is one object of the shared auth config fixtures. A leaf
// whose path or value holds "SECRET-", or whose value is a reference, is
// hidden; every other leaf is shown.
type authConfigFixture struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Variant string         `json:"variant"`
	Object  map[string]any `json:"object"`
}

func loadAuthConfigFixtures(t *testing.T) []authConfigFixture {
	t.Helper()
	b, err := os.ReadFile(authConfigFixturesPath)
	require.NoError(t, err)
	var out []authConfigFixture
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotEmpty(t, out)
	return out
}

// authConfigLeaves adds one string per leaf of v: the map keys and list markers
// on the way, then "=" and the value.
func authConfigLeaves(v any, p string, out map[string]bool) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			authConfigLeaves(child, p+"."+k, out)
		}
	case []any:
		for _, child := range v {
			authConfigLeaves(child, p+"[]", out)
		}
	default:
		out[p+"="+fmt.Sprint(v)] = true
	}
}

func expectedAuthConfigLeaves(obj map[string]any) map[string]bool {
	all := map[string]bool{}
	authConfigLeaves(obj, "", all)
	out := map[string]bool{}
	for leaf := range all {
		_, value, _ := strings.Cut(leaf, "=")
		if strings.Contains(leaf, "SECRET-") || strings.HasPrefix(value, "cattle-global-data:") {
			continue
		}
		out[leaf] = true
	}
	return out
}

// authConfigServer is a test server for the management path. It responds with
// the fixtures of one variant as JSON: the list and each object by name.
func authConfigServer(t *testing.T, variant string) (*httptest.Server, []authConfigFixture) {
	t.Helper()
	var fixtures []authConfigFixture
	byName := map[string][]byte{}
	var items []any
	for _, f := range loadAuthConfigFixtures(t) {
		if f.Variant != variant {
			continue
		}
		fixtures = append(fixtures, f)
		b, err := json.Marshal(f.Object)
		require.NoError(t, err)
		byName[f.Object["metadata"].(map[string]any)["name"].(string)] = b
		items = append(items, f.Object)
	}
	list, err := json.Marshal(map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "AuthConfigList", "items": items})
	require.NoError(t, err)
	const base = "/k8s/clusters/local/apis/management.cattle.io/v3/authconfigs"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == base:
			_, _ = w.Write(list)
		case path.Dir(r.URL.Path) == base && byName[path.Base(r.URL.Path)] != nil:
			_, _ = w.Write(byName[path.Base(r.URL.Path)])
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fixtures
}

// TestGetAuthConfigHidesEverySecretInEveryFormat asserts, end to end against a
// server that sends real JSON, that get authconfig in -o json, yaml, template
// and name, for the list and for one object, shows every value of every fixture
// except the credential fields and the references, and shows no value marked
// SECRET- and no reference. Output is decoded before it is compared. NOT
// parallel: it sets the process environment.
func TestGetAuthConfigHidesEverySecretInEveryFormat(t *testing.T) {
	exitEnv(t)
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for _, variant := range []string{"reference", "plaintext"} {
		srv, fixtures := authConfigServer(t, variant)
		dir := grConfigDir(t, srv)
		get := func(t *testing.T, args ...string) string {
			t.Helper()
			stdout, stderr, err := grRun(t, grRoot(t), dir, append([]string{"get"}, args...)...)
			require.NoError(t, err, string(stderr))
			return string(stdout)
		}
		var names []string
		for _, f := range fixtures {
			names = append(names, f.Object["metadata"].(map[string]any)["name"].(string))
		}

		t.Run(variant+" list json", func(t *testing.T) {
			var env struct{ Data []map[string]any }
			require.NoError(t, json.Unmarshal([]byte(get(t, "authconfigs", "-o", "json")), &env))
			require.Len(t, env.Data, len(fixtures))
			for i, got := range env.Data {
				leaves := map[string]bool{}
				authConfigLeaves(got, "", leaves)
				assert.Equal(t, expectedAuthConfigLeaves(fixtures[i].Object), leaves, fixtures[i].Name)
			}
		})
		t.Run(variant+" list yaml", func(t *testing.T) {
			var env struct{ Data []map[string]any }
			require.NoError(t, yaml.Unmarshal([]byte(get(t, "authconfigs", "-o", "yaml")), &env))
			require.Len(t, env.Data, len(fixtures))
			for i, got := range env.Data {
				leaves := map[string]bool{}
				authConfigLeaves(got, "", leaves)
				assert.Equal(t, expectedAuthConfigLeaves(fixtures[i].Object), leaves, fixtures[i].Name)
			}
		})
		t.Run(variant+" list name", func(t *testing.T) {
			got := strings.Fields(get(t, "authconfigs", "-o", "name"))
			want := slices.Clone(names)
			slices.Sort(got)
			slices.Sort(want)
			for i := range want {
				want[i] = strings.TrimPrefix(want[i], "authconfig/")
			}
			for i := range got {
				got[i] = strings.TrimPrefix(got[i], "authconfig/")
			}
			assert.Equal(t, want, got)
		})
		t.Run(variant+" list template", func(t *testing.T) {
			out := get(t, "authconfigs", "-o", output.TemplatePrefix+"{{range .}}{{.}}{{end}}")
			assert.NotContains(t, out, "SECRET-")
			assert.NotContains(t, out, "cattle-global-data:SECRET")
			for _, f := range fixtures {
				assert.Contains(t, out, "SHOWN-display-"+f.Type)
			}
		})
		for _, f := range fixtures {
			name := f.Object["metadata"].(map[string]any)["name"].(string)
			t.Run(variant+" "+name+" json", func(t *testing.T) {
				var env struct{ Data map[string]any }
				require.NoError(t, json.Unmarshal([]byte(get(t, "authconfig", name, "-o", "json")), &env))
				leaves := map[string]bool{}
				authConfigLeaves(env.Data, "", leaves)
				assert.Equal(t, expectedAuthConfigLeaves(f.Object), leaves)
			})
			t.Run(variant+" "+name+" yaml", func(t *testing.T) {
				var env struct{ Data map[string]any }
				require.NoError(t, yaml.Unmarshal([]byte(get(t, "authconfig", name, "-o", "yaml")), &env))
				leaves := map[string]bool{}
				authConfigLeaves(env.Data, "", leaves)
				assert.Equal(t, expectedAuthConfigLeaves(f.Object), leaves)
			})
			t.Run(variant+" "+name+" template", func(t *testing.T) {
				out := get(t, "authconfig", name, "-o", output.TemplatePrefix+"{{.}}")
				assert.NotContains(t, out, "SECRET-")
				assert.Contains(t, out, "SHOWN-display-"+f.Type)
			})
			t.Run(variant+" "+name+" name", func(t *testing.T) {
				assert.Equal(t, name, strings.TrimPrefix(strings.TrimSpace(get(t, "authconfig", name, "-o", "name")), "authconfig/"))
			})
		}
	}
}
