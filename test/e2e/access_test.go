//go:build e2e

package e2e

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/testrancher"
)

// selfSubjectReviewPath is where the identity protocol's downstream whoami
// lands after the shim rewrites the cluster prefix away.
const selfSubjectReviewPath = "/apis/authentication.k8s.io/v1/selfsubjectreviews"

// sarAllowed posts a SubjectAccessReview to envtest as the harness admin and
// reports the authorizer's answer. It asks the apiserver directly, with no
// CLI and no shim in the path.
func sarAllowed(t *testing.T, user string, groups []string, ns, group, resource, verb string) bool {
	t.Helper()
	gvr := schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"}
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SubjectAccessReview",
		"spec": map[string]any{
			"user":   user,
			"groups": toAny(groups),
			"resourceAttributes": map[string]any{
				"namespace": ns, "group": group, "resource": resource, "verb": verb,
			},
		},
	}}
	out, err := dynCli.Resource(gvr).Create(t.Context(), review, metav1.CreateOptions{})
	require.NoError(t, err)
	allowed, found, err := unstructured.NestedBool(out.Object, "status", "allowed")
	require.NoError(t, err)
	require.True(t, found, "SubjectAccessReview answered without status.allowed")
	return allowed
}

// TestAccessFixtureThroughTheRealAuthorizer asserts the seeded roles and
// bindings mean what the access scenarios read them to mean. Asking the
// authorizer here makes fixture drift fail at its source instead of
// surfacing as an unexplained decision or rollup mismatch in a txtar.
func TestAccessFixtureThroughTheRealAuthorizer(t *testing.T) {
	t.Parallel()
	authenticated := []string{"system:authenticated"}

	assert.True(t, sarAllowed(t, "e2e-std", authenticated, "team-a", "", "pods", "get"),
		"e2e-std holds the pod reader rule in team-a")
	assert.False(t, sarAllowed(t, "e2e-std", authenticated, "team-b", "", "pods", "get"),
		"the pod reader rule is bound in team-a only")

	assert.True(t, sarAllowed(t, "someone-else", authenticated, "team-a", "apps", "deployments", "get"),
		"system:authenticated may get deployments in team-a")
	assert.False(t, sarAllowed(t, "someone-else", authenticated, "team-a", "", "pods", "get"),
		"the group grant covers deployments only, never pods")

	assert.True(t, sarAllowed(t, "system:serviceaccount:team-a:ci",
		[]string{"system:serviceaccounts", "system:serviceaccounts:team-a", "system:authenticated"},
		"team-a", "", "configmaps", "get"),
		"the team-a service account group may get configmaps there")
	assert.False(t, sarAllowed(t, "system:anonymous", []string{"system:unauthenticated"},
		"team-a", "apps", "deployments", "get"),
		"the anonymous identity is never credited with the authenticated group's grant")

	assert.True(t, sarAllowed(t, "u-bob", []string{"okta_group://eng", "system:authenticated"}, "team-b", "", "configmaps", "get"),
		"a cached member of okta_group://eng may get configmaps in team-b")
	assert.True(t, sarAllowed(t, "oidc-external@corp", authenticated, "team-b", "", "secrets", "get"),
		"the external username is bound directly in team-b")
	assert.False(t, sarAllowed(t, "oidc-external@corp", authenticated, "team-b", "", "pods", "get"),
		"and holds nothing else there")
	assert.True(t, sarAllowed(t, "e2e-std", authenticated, "team-c", "", "pods", "delete"),
		"e2e-std may delete pods in team-c through the classic-grammar RoleBinding")
	assert.False(t, sarAllowed(t, "e2e-std", authenticated, "team-a", "", "pods", "delete"),
		"and team-a's rules are untouched by the explain fixtures")
	assert.True(t, sarAllowed(t, "e2e-std", authenticated, "", "", "nodes", "get"),
		"e2e-std may get nodes through the aggregation-grammar ClusterRoleBinding")
	assert.True(t, sarAllowed(t, "e2e-std", authenticated, "", "", "namespaces", "get"),
		"e2e-std may get namespaces through the shared-key ClusterRoleBinding")
	assert.False(t, sarAllowed(t, "e2e-std", authenticated, "team-c", "", "secrets", "delete"),
		"and may not delete secrets there")
}

// accessRunner runs the built binary against a facade whose router records
// every identity the shim injected.
type accessRunner struct {
	rec  *shimRecorder
	env  []string
	tdir string
}

func newAccessRunner(t *testing.T) *accessRunner {
	t.Helper()
	rec := &shimRecorder{}
	var facade *testrancher.Facade
	resolve := facadeIdentityResolver(shimIdentities, func(tok string) bool {
		return facade != nil && facade.ValidBearer(tok)
	})
	router := newRecordingRouter(resolve, rec)
	facade = testrancher.New(testrancher.Options{Fallthrough: router, SeedToken: testToken,
		ClusterVisibility: clusterVisibility(resolve), LocalManagementGate: localManagementGate(resolve), ClusterObject: storedCluster,
		UserAttributeAccess: userAttributeAccess(resolve), UserAttributeObjects: storedUserAttributes})
	srv := httptest.NewTLSServer(facade)
	t.Cleanup(srv.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	dir := t.TempDir()
	return &accessRunner{
		rec:  rec,
		tdir: dir,
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + dir,
			"RANCHER_CLI_URL=" + srv.URL,
			"RANCHER_CLI_TOKEN=" + testToken,
			"RANCHER_CLI_CACERT=" + caPEM,
			"RANCHER_CLI_CONFIG_DIR=" + filepath.Join(dir, "rancher-config"),
			"RANCHER_CLI_CREDENTIAL_STORE=file",
			"RANCHER_CLI_TELEMETRY=false",
		},
	}
}

// run executes the binary and returns its combined output; the recording
// starts empty so each run's injections stand alone.
func (a *accessRunner) run(t *testing.T, args ...string) string {
	t.Helper()
	a.rec.Reset()
	cmd := exec.CommandContext(t.Context(), binPath, args...)
	cmd.Env = a.env
	cmd.Dir = a.tdir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "rancher %s: %s", strings.Join(args, " "), out)
	return string(out)
}

// whoamiIdentities returns the identities the shim injected on the downstream
// whoami, which is the only request whose subject the CLI's own output cannot
// show.
func (a *accessRunner) whoamiIdentities() []shimIdentity {
	var out []shimIdentity
	for _, inj := range a.rec.Injected() {
		if inj.path == selfSubjectReviewPath {
			out = append(out, inj.identity)
		}
	}
	return out
}

// TestAccessCanImpersonatesWithServerOwnedGroups asserts what reaches the
// downstream authorizer when --as takes the impersonation path: the subject
// as its user and exactly the groups the server owns for it, never a group
// the client asked for.
func TestAccessCanImpersonatesWithServerOwnedGroups(t *testing.T) {
	t.Parallel()
	runner := newAccessRunner(t)

	for _, tc := range []struct {
		name    string
		subject string
		// The resource each subject is allowed, so a run that ends in a
		// denial is a fixture failure and not an expected exit 12.
		resource string
		groups   []string
	}{
		{
			name:     "a Rancher user",
			subject:  "e2e-std",
			resource: "pods",
			groups:   []string{"system:authenticated", "system:cattle:authenticated"},
		},
		{
			name:     "a service account",
			subject:  "system:serviceaccount:team-a:ci",
			resource: "configmaps",
			groups:   []string{"system:serviceaccounts", "system:serviceaccounts:team-a", "system:authenticated"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runner.run(t, "access", "can", "get", tc.resource,
				"--cluster", "testcluster", "-n", "team-a", "--as", tc.subject)
			assert.Contains(t, out, "identity: impersonated")

			whoami := runner.whoamiIdentities()
			// Two: the caller's own preflight, then the subject's.
			require.Len(t, whoami, 2, "one preflight and one impersonated whoami per run")
			assert.Equal(t, "e2e-admin", whoami[0].user)
			assert.Equal(t, tc.subject, whoami[1].user)
			assert.Equal(t, tc.groups, whoami[1].groups,
				"the downstream authorizer sees the server-owned group set, verbatim")
		})
	}
}

// TestAccessListImpersonatesTheSubject asserts list takes the same
// impersonated identity to the downstream rules review.
func TestAccessListImpersonatesTheSubject(t *testing.T) {
	t.Parallel()
	runner := newAccessRunner(t)
	out := runner.run(t, "access", "list", "--cluster", "testcluster", "-n", "team-a", "--as", "e2e-std")
	assert.Contains(t, out, "identity: impersonated")

	whoami := runner.whoamiIdentities()
	require.Len(t, whoami, 2)
	assert.Equal(t, "e2e-std", whoami[1].user)
	assert.Equal(t, []string{"system:authenticated", "system:cattle:authenticated"}, whoami[1].groups)

	var rulesReviews int
	for _, inj := range runner.rec.Injected() {
		if strings.HasSuffix(inj.path, "/selfsubjectrulesreviews") {
			rulesReviews++
			assert.Equal(t, "e2e-std", inj.identity.user, "the rules review runs as the subject, not the caller")
		}
	}
	assert.Positive(t, rulesReviews, "list asks the authorizer with a SelfSubjectRulesReview")
}

// TestShimStripsClientImpersonationHeaders asserts the shim never forwards
// what the client asked to be: an identity the CLI could not have obtained
// otherwise must not reach the apiserver.
func TestShimStripsClientImpersonationHeaders(t *testing.T) {
	t.Parallel()
	rec := &shimRecorder{}
	router := newRecordingRouter(staticIdentityResolver(shimIdentities), rec)
	srv := httptest.NewServer(router)
	defer srv.Close()

	post := func(bearer, impersonate string, impersonateGroups []string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			srv.URL+"/k8s/clusters/c-test/api/v1/namespaces/team-a/pods", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		if impersonate != "" {
			req.Header.Set("Impersonate-User", impersonate)
		}
		for _, g := range impersonateGroups {
			req.Header.Add("Impersonate-Group", g)
		}
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// A caller who cannot impersonate is refused the way Rancher refuses,
	// and nothing reaches the apiserver.
	assert.Equal(t, 403, post("e2e-std:secret", "e2e-admin", nil))
	assert.Empty(t, rec.Injected())

	// A caller who can impersonate gets the subject's server-owned groups,
	// never the groups it asked for.
	rec.Reset()
	assert.Equal(t, 200, post(testToken, "e2e-std", []string{"system:masters"}))
	injected := rec.Injected()
	require.Len(t, injected, 1)
	assert.Equal(t, "e2e-std", injected[0].identity.user)
	assert.Equal(t, []string{"system:authenticated", "system:cattle:authenticated"}, injected[0].identity.groups)
}
