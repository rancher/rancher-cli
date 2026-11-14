package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// guardRancher is an httptest Rancher that answers the SelfUser request with
// a fixed user ID and records the Project requests: the dry-run first, then
// the create.
type guardRancher struct {
	mu       sync.Mutex
	projects []guardProjectRequest
	// selfUserForbidden answers the SelfUser request with a 403 Status.
	selfUserForbidden bool
}

type guardProjectRequest struct {
	dryRun      bool
	annotations map[string]any
}

func (g *guardRancher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	switch {
	case r.Method != http.MethodPost:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"not found","reason":"NotFound","code":404}`))
		return
	case strings.HasSuffix(r.URL.Path, "/apis/ext.cattle.io/v1/selfusers") && g.selfUserForbidden:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"selfusers.ext.cattle.io is forbidden","reason":"Forbidden","code":403}`))
		return
	case strings.HasSuffix(r.URL.Path, "/apis/ext.cattle.io/v1/selfusers"):
		_ = unstructured.SetNestedField(body, "u-guard", "status", "userID")
	case strings.HasSuffix(r.URL.Path, "/apis/management.cattle.io/v3/namespaces/c-guard/projects"):
		annotations, _, _ := unstructured.NestedMap(body, "metadata", "annotations")
		g.mu.Lock()
		g.projects = append(g.projects, guardProjectRequest{dryRun: strings.Contains(r.URL.RawQuery, "dryRun=All"), annotations: annotations})
		g.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

// guardFactory hands the real client of the httptest Rancher to a command.
type guardFactory struct{ client engine.ClusterClient }

func (f guardFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return f.client, nil
}

func (f guardFactory) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return f.client, nil
}

func (f guardFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

type guardFiles map[string][]byte

func (f guardFiles) Read(path string) ([]byte, error) {
	b, ok := f[path]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return b, nil
}

type guardInput struct {
	s map[string]string
	a []string
}

func (i guardInput) String(k string) string  { return i.s[k] }
func (i guardInput) Bool(string) bool        { return false }
func (i guardInput) Strings(string) []string { return nil }
func (i guardInput) IsSet(string) bool       { return false }
func (i guardInput) Args() []string          { return i.a }

const guardProjectManifest = `apiVersion: management.cattle.io/v3
kind: Project
metadata:
  name: guarded
  namespace: c-guard
spec:
  displayName: guarded
  clusterName: c-guard
`

// projectCreatePaths is every command that creates a Project, each a
// function that runs one real Plan and the create against the client.
var projectCreatePaths = []struct {
	name string
	run  func(t *testing.T, factory engine.ClusterClientFactory)
}{
	{
		name: "create project",
		run: func(t *testing.T, factory engine.ClusterClientFactory) {
			t.Helper()
			runGuardedCreate(t, projects.NewCreate(factory), guardInput{s: map[string]string{"cluster": "c-guard"}, a: []string{"guarded"}})
		},
	},
	{
		name: "create project with a generated id",
		run: func(t *testing.T, factory engine.ClusterClientFactory) {
			t.Helper()
			runGuardedCreate(t, projects.NewCreate(factory), guardInput{s: map[string]string{"cluster": "c-guard", "display-name": "Guarded"}})
		},
	},
	{
		name: "create resource with a Project manifest",
		run: func(t *testing.T, factory engine.ClusterClientFactory) {
			t.Helper()
			runGuardedCreate(t, resources.NewCreate(factory, guardFiles{"p.yaml": []byte(guardProjectManifest)}), guardInput{s: map[string]string{"filename": "p.yaml"}})
		},
	},
}

func runGuardedCreate(t *testing.T, cmd runtime.Command, in runtime.Input) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "a create command is a MutatingCommand")
	_, exec, err := mc.Plan(t.Context(), in)
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
}

// TestEveryProjectCreatePathSetsTheCreatorID asserts every command that
// creates a Project sends the creator annotation, set to the user ID the
// SelfUser request returned, on the dry-run and on the create.
func TestEveryProjectCreatePathSetsTheCreatorID(t *testing.T) {
	t.Parallel()
	for _, path := range projectCreatePaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			rancher := &guardRancher{}
			srv := httptest.NewServer(rancher)
			t.Cleanup(srv.Close)
			rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, engine.LocalCluster)
			require.NoError(t, err)
			rc.Transport = srv.Client().Transport
			client, err := provider.NewClusterClient(rc, engine.LocalCluster)
			require.NoError(t, err)

			path.run(t, guardFactory{client: client})

			require.Len(t, rancher.projects, 2, "the dry-run and the create")
			assert.True(t, rancher.projects[0].dryRun, "the dry-run goes first")
			assert.False(t, rancher.projects[1].dryRun)
			for _, req := range rancher.projects {
				assert.Equal(t, "u-guard", req.annotations["field.cattle.io/creatorId"], "dry-run: %v", req.dryRun)
			}
		})
	}
}

// TestCreateResourceProjectManifestRefusalsInTheEnvelope asserts the two
// refusals of a manifest creatorId, run through the command line with
// -o json: the error code, its exit code, no hint and the top-level
// warnings, with no Project request sent.
func TestCreateResourceProjectManifestRefusalsInTheEnvelope(t *testing.T) {
	keyring.MockInit()
	tests := []struct {
		name         string
		manifestID   string
		forbidden    bool
		wantCode     string
		wantMessage  string
		wantWarnings []string
	}{
		{
			name:        "another user",
			manifestID:  "u-other",
			wantCode:    clierr.CodeValidation,
			wantMessage: "the CLI sets the creator of a project to you",
		},
		{
			name:        "SelfUser request refused",
			manifestID:  "u-guard",
			forbidden:   true,
			wantCode:    clierr.CodeRBAC,
			wantMessage: "selfusers.ext.cattle.io is forbidden",
			wantWarnings: []string{
				"the manifest sets field.cattle.io/creatorId and the CLI could not get your user ID to compare it; remove the annotation, or log in again and retry",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rancher := &guardRancher{selfUserForbidden: tt.forbidden}
			srv := httptest.NewTLSServer(testrancher.New(testrancher.Options{
				SeedToken: "u-guard:secret",
				ClusterVisibility: func(bearer string) ([]testrancher.ClusterFixture, bool) {
					return []testrancher.ClusterFixture{{ID: "c-guard", DisplayName: "guard"}}, bearer == "u-guard:secret"
				},
				Fallthrough: rancher,
			}))
			t.Cleanup(srv.Close)

			cfgDir, home := t.TempDir(), t.TempDir()
			t.Setenv(config.EnvConfigDir, cfgDir)
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
			t.Setenv(config.EnvCredentialStore, "file")
			require.NoError(t, os.Unsetenv(config.EnvURL))
			require.NoError(t, os.Unsetenv(config.EnvToken))
			cfg := &config.Config{CredentialStore: "file", CurrentServer: "x", Servers: map[string]config.Server{"x": {URL: srv.URL, CA: string(pemFromServer(t, srv))}}}
			cfg.SetPath(config.FilePath(cfgDir))
			require.NoError(t, cfg.Write())
			require.NoError(t, config.NewFileStore(cfgDir).SetToken(config.ServerRef{Name: "x", URL: srv.URL}, "u-guard:secret"))

			manifest := filepath.Join(t.TempDir(), "p.yaml")
			require.NoError(t, os.WriteFile(manifest, []byte(strings.Replace(guardProjectManifest, "spec:", "  annotations:\n    field.cattle.io/creatorId: "+tt.manifestID+"\nspec:", 1)), 0o600))

			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out, errOut bytes.Buffer
			root.Writer, root.ErrWriter = &out, &errOut
			root.Reader = strings.NewReader("")
			root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
			args := []string{"create", "resource", "-f", manifest, "--dry-run", "-o", "json"}
			CaptureArgv(args)
			t.Cleanup(func() { CaptureArgv(nil) })
			runErr := root.Run(t.Context(), append([]string{"rancher"}, args...))
			require.Error(t, runErr)
			coder, ok := errors.AsType[ucli.ExitCoder](runErr)
			require.True(t, ok, "got %v", runErr)
			assert.Equal(t, clierr.ExitCode(tt.wantCode), coder.ExitCode())

			var env struct {
				Warnings []string `json:"warnings"`
				Data     struct {
					Error *struct {
						Code    string `json:"code"`
						Message string `json:"message"`
						Hint    string `json:"hint"`
					} `json:"error"`
				} `json:"data"`
			}
			stream := out.String() + errOut.String()
			i := strings.Index(stream, "{")
			require.GreaterOrEqual(t, i, 0, "no JSON envelope in %q", stream)
			require.NoError(t, json.Unmarshal([]byte(stream[i:]), &env), stream)
			require.NotNil(t, env.Data.Error, stream)
			assert.Equal(t, tt.wantCode, env.Data.Error.Code)
			assert.Contains(t, env.Data.Error.Message, tt.wantMessage)
			assert.Empty(t, env.Data.Error.Hint)
			assert.Equal(t, tt.wantWarnings, env.Warnings)
			rancher.mu.Lock()
			defer rancher.mu.Unlock()
			assert.Empty(t, rancher.projects, "no Project request is sent")
		})
	}
}
