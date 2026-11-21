package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// podRefusalCase is one app action and the server's responses to it.
type podRefusalCase struct {
	name string
	// server is the Rancher that responds to the action.
	server func(t *testing.T) *appServer
	args   []string
	path   string
	note   string
}

var podRefusalCases = []podRefusalCase{
	{
		name: "install",
		server: func(t *testing.T) *appServer {
			t.Helper()
			return newAppServer(t, "community")
		},
		args: withArgs(appInstallArgs, "--apply", "-o", "json"),
		path: appDown + "/v1/catalog.cattle.io.clusterrepos/" + appRepo + "?action=install",
		note: "Rancher may have created, changed or deleted the image pull secrets it manages for this release",
	},
	{
		name:   "upgrade",
		server: upgradeServer,
		args:   withArgs(appUpgradeArgs, "--description", "x", "--apply", "--yes", "-o", "json"),
		path:   appDown + "/v1/catalog.cattle.io.clusterrepos/" + appRepo + "?action=upgrade",
		note:   "Rancher may have created, changed or deleted the image pull secrets it manages for this release",
	},
	{
		name: "uninstall",
		server: func(t *testing.T) *appServer {
			t.Helper()
			s := newAppServer(t, "community")
			s.putApp("deployed")
			return s
		},
		args: withArgs(appUninstallArgs, "--apply", "--yes", "-o", "json"),
		path: appDown + "/v1/catalog.cattle.io.apps/" + appNS + "/" + appRelease + "?action=uninstall",
		note: "Rancher may have deleted the image pull secrets it manages for this release",
	},
}

// podRefusalForms are the two responses to the refusal of the pod Rancher runs
// Helm in: the 500 Rancher 2.14 to 2.16 send, and the 403 with the API server's
// own status.
var podRefusalForms = []struct {
	name   string
	answer func(w http.ResponseWriter, message string)
}{
	{"500", func(w http.ResponseWriter, message string) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"type": "error", "links": map[string]any{}, "code": "ServerError", "message": message, "status": 500,
		})
	}},
	{"403", func(w http.ResponseWriter, message string) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
			"message": message, "reason": "Forbidden", "details": map[string]any{"kind": "pods"}, "code": 403,
		})
	}},
}

// TestAppActionReportsARefusedOperationPod asserts, through the real action
// client and the exit pipeline, that Kubernetes admission refusing the pod
// Rancher runs Helm in is the admission refusal for an install, an upgrade and
// an uninstall, in a 500 response and a 403 response: the whole text, the words
// that Rancher created no operation, the sentence on the image pull secrets, no
// hint and one request; and that a 500 that is not the pod keeps the error that
// says the CLI does not know whether Rancher ran the action.
func TestAppActionReportsARefusedOperationPod(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for _, c := range podRefusalCases {
		for _, form := range podRefusalForms {
			for _, tc := range []struct {
				name      string
				text      string
				code      string
				retryable bool
			}{
				{"validation", "ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no helm pods", "VALIDATION", false},
				{"retryable", "not yet ready to handle request", "GENERIC", true},
			} {
				t.Run(c.name+" "+form.name+" "+tc.name, func(t *testing.T) {
					s := c.server(t)
					serverText := `pods "helm-operation-q26kh" is forbidden: ` + tc.text
					s.action = func(w http.ResponseWriter, _ *http.Request) { form.answer(w, serverText) }
					_, stderr, err := appRun(t, s, c.args...)
					require.Error(t, err)
					assert.Equal(t, 1, s.postCount())
					ce := decodeErrorEnvelope(t, stderr)
					want := "POST " + c.path + " on the Rancher server was refused: " + serverText +
						"; Rancher created no operation for this request of release 'my-nginx'; " + c.note
					if tc.retryable {
						want += " - retry later"
					}
					assert.Equal(t, tc.code, ce.Code, stderr)
					assert.Equal(t, tc.retryable, ce.Retryable, stderr)
					assert.Equal(t, want, ce.Message)
					assert.Empty(t, ce.Hint)
				})
			}
		}
		t.Run(c.name+" a 500 that is not the pod", func(t *testing.T) {
			s := c.server(t)
			s.action = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"type": "error", "code": "ServerError", "status": 500,
					"message": `secrets "helm-operation-q26kh" is forbidden: ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no helm pods`,
				})
			}
			_, stderr, err := appRun(t, s, c.args...)
			require.Error(t, err)
			assert.Equal(t, 1, s.postCount())
			ce := decodeErrorEnvelope(t, stderr)
			assert.Equal(t, "GENERIC", ce.Code)
			assert.False(t, ce.Retryable)
			assert.Contains(t, ce.Message, "the answer did not say whether Rancher ran it")
			assert.NotContains(t, ce.Message, "Rancher created no operation")
		})
	}
}
