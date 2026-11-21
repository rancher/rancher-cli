package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	podHead      = `pods "helm-operation-x" is forbidden: `
	vapText      = `ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no helm pods`
	vapPrefix    = `ValidatingAdmissionPolicy 'p' with binding 'b' denied request: `
	notReadyText = `not yet ready to handle request`
	installNote  = "Rancher may have created, changed or deleted the image pull secrets it manages for this release"
	removeNote   = "Rancher may have deleted the image pull secrets it manages for this release"
	podRefused   = "Rancher created no operation for this request of release 'my-nginx'"
)

// versionedClients is the appFake as a factory that reports the server's
// version, as the real factory does.
type versionedClients struct {
	*appFake
	version string
	err     error
}

func (v versionedClients) ServerVersion(context.Context) (string, error) { return v.version, v.err }

var _ engine.ServerVersionProvider = versionedClients{}

// clientsFor is the factory a command reads the server's version from: the
// fake alone for "" (no version provider), a version that cannot be read for
// "unreadable", otherwise a server of that version.
func clientsFor(f *appFake, version string) engine.ClusterClientFactory {
	switch version {
	case "":
		return f
	case "unreadable":
		return versionedClients{appFake: f, err: errors.New("the version cannot be read")}
	}
	return versionedClients{appFake: f, version: version}
}

// admissionAction is one catalog action that sends a POST.
type admissionAction struct {
	name string
	// path is the action's POST path.
	path string
	// values is true when the command takes a values file.
	values bool
	// note is the sentence on the image pull secrets for a server of 2.15 or
	// later.
	note string
	// fake is the fake the command runs over.
	fake func(t *testing.T) *appFake
	// run plans the command over a server of version with the values file's
	// content ("" for none), executes it and returns Execute's error.
	run func(t *testing.T, f *appFake, version, valuesFile string) error
}

func withValuesFile(content string) func(*appInput) {
	return func(in *appInput) {
		if content != "" {
			in.strs[flagValues] = []string{"v.yaml"}
		}
	}
}

// admissionActions has one row for each catalog action that sends a POST. The
// scan of the callers of postAction names a row of it for each caller.
var admissionActions = []admissionAction{
	{
		name: "install", path: "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/repo1?action=install",
		values: true, note: installNote,
		fake: newAppFake,
		run: func(t *testing.T, f *appFake, version, valuesFile string) error {
			t.Helper()
			cmd := newTestInstall(f, files{"v.yaml": valuesFile})
			cmd.clients = clientsFor(f, version)
			_, exec := plan(t, cmd, installInput(withValuesFile(valuesFile)))
			_, err := exec(t.Context())
			return err
		},
	},
	{
		name: "upgrade", path: "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/repo1?action=upgrade",
		values: true, note: installNote,
		fake: func(t *testing.T) *appFake {
			t.Helper()
			return upgradeFake(t, nil)
		},
		run: func(t *testing.T, f *appFake, version, valuesFile string) error {
			t.Helper()
			cmd := newTestUpgrade(f, files{"v.yaml": valuesFile})
			cmd.clients = clientsFor(f, version)
			_, exec := plan(t, cmd, upgradeInput(withValuesFile(valuesFile)))
			_, err := exec(t.Context())
			return err
		},
	},
	{
		name: "uninstall", path: "/k8s/clusters/c-1/v1/catalog.cattle.io.apps/web/my-nginx?action=uninstall",
		note: removeNote,
		fake: func(t *testing.T) *appFake {
			t.Helper()
			f := newAppFake(t)
			deployedApp(f, "deployed", nil)
			return f
		},
		run: func(t *testing.T, f *appFake, version, _ string) error {
			t.Helper()
			cmd := newTestUninstall(f)
			cmd.clients = clientsFor(f, version)
			_, exec := plan(t, cmd, uninstallInput())
			_, err := exec(t.Context())
			return err
		},
	},
}

// podAnswer is one response to a refused pod: the status and the error the
// action client hands over.
type podAnswer struct {
	name string
	// status is the HTTP status of the response.
	status int
	// answer is the error the action client hands over for message.
	answer func(t *testing.T, message string) error
}

// rancherServerErrorBody is Rancher's error object for message.
func rancherServerErrorBody(t *testing.T, message string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "error", "links": map[string]any{}, "code": "ServerError", "message": message, "status": 500})
	require.NoError(t, err)
	return b
}

// unknownAnswer is the error the action client hands over for a 500, 502 or
// 504 with body.
func unknownAnswer(status int, body []byte) error {
	return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, status, body), Body: body}
}

// kubernetesStatusBody is the Kubernetes Status the API server answers with.
func kubernetesStatusBody(t *testing.T, code int, message string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": message, "reason": "Forbidden", "details": map[string]any{"kind": "pods"}, "code": code,
	})
	require.NoError(t, err)
	return b
}

var podAnswers = []podAnswer{
	{"a 500 response", http.StatusInternalServerError, func(t *testing.T, message string) error {
		t.Helper()
		return unknownAnswer(http.StatusInternalServerError, rancherServerErrorBody(t, message))
	}},
	{"a 403 response", http.StatusForbidden, func(t *testing.T, message string) error {
		t.Helper()
		return refusal(http.StatusForbidden, string(kubernetesStatusBody(t, http.StatusForbidden, message)))
	}},
}

// podRefusalText is the error text for the refusal of the pod Rancher runs Helm
// in with serverText.
func podRefusalText(path, serverText, note string, retryable bool) string {
	msg := "POST " + path + " on the Rancher server was refused: " + serverText + "; " + podRefused
	if note != "" {
		msg += "; " + note
	}
	if retryable {
		msg += " - retry later"
	}
	return msg
}

// straddling is head, padding and token in that order. The padding puts the
// start of token 16 bytes before bound, which places the cut inside token.
// 3*bound more characters follow, which keeps the text longer than bound once
// token is hidden.
func straddling(head, token string, bound int) string {
	return head + strings.Repeat(".", bound-16-len(head)) + token + strings.Repeat("-", 3*bound)
}

// runPodAnswer runs the action over the response and returns the error, the
// number of POSTs and the path of the first.
func runPodAnswer(t *testing.T, a admissionAction, err error, version, valuesFile string) (*clierr.Error, *appFake) {
	t.Helper()
	f := a.fake(t)
	f.postAnswer = func(string) ([]byte, error) { return nil, err }
	got := a.run(t, f, version, valuesFile)
	require.Error(t, got)
	require.Equal(t, 1, f.postCount())
	require.Equal(t, a.path, f.posts[0].path)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok, "%T %v", got, got)
	return ce, f
}

// TestPodRefusalIsReportedAsTheAdmissionRefusal asserts, for each action, in
// a 500 response and a 403 response and with both names of the pod, a refusal
// of the pod Rancher runs Helm in gets the code the same refusal gets in a 403
// (VALIDATION, or a retryable GENERIC ending " - retry later"), the whole
// server text, the words that Rancher created no operation, the sentence on the
// image pull secrets, no hint, and one POST.
func TestPodRefusalIsReportedAsTheAdmissionRefusal(t *testing.T) {
	t.Parallel()
	texts := []struct {
		name      string
		text      string
		retryable bool
	}{
		{"validation", vapText, false},
		{"retryable", notReadyText, true},
	}
	for _, a := range admissionActions {
		for _, form := range podAnswers {
			for _, podName := range []string{"helm-operation-", "helm-operation-5nswt"} {
				for _, tc := range texts {
					t.Run(a.name+" "+form.name+" "+podName+" "+tc.name, func(t *testing.T) {
						t.Parallel()
						serverText := `pods "` + podName + `" is forbidden: ` + tc.text
						ce, _ := runPodAnswer(t, a, form.answer(t, serverText), "v2.15.3", "")
						if tc.retryable {
							assert.Equal(t, clierr.CodeGeneric, ce.Code)
							assert.Equal(t, 1, ce.ExitCode())
						} else {
							assert.Equal(t, clierr.CodeValidation, ce.Code)
							assert.Equal(t, 2, ce.ExitCode())
						}
						assert.Equal(t, tc.retryable, ce.Retryable)
						assert.Equal(t, podRefusalText(a.path, serverText, a.note, tc.retryable), ce.Message)
						assert.Empty(t, ce.Hint)
					})
				}
			}
		}
	}
}

// TestPodRefusalPullSecretSentenceFollowsThePreview asserts the sentence on
// the image pull secrets is in the error exactly when the preview carries its
// warning on them, from the version Plan read: a server of 2.15 or later, and
// a version that cannot be read or is not offered; not a server below 2.15.
func TestPodRefusalPullSecretSentenceFollowsThePreview(t *testing.T) {
	t.Parallel()
	for _, a := range admissionActions {
		for _, form := range podAnswers {
			for version, applies := range map[string]bool{"v2.15.0": true, "v2.16.1": true, "v2.14.8": false, "unreadable": true, "": true} {
				t.Run(a.name+" "+form.name+" "+version, func(t *testing.T) {
					t.Parallel()
					ce, _ := runPodAnswer(t, a, form.answer(t, podHead+vapText), version, "")
					note := ""
					if applies {
						note = a.note
					}
					assert.Equal(t, podRefusalText(a.path, podHead+vapText, note, false), ce.Message)
				})
			}
		}
	}
}

// TestPodRefusalShowsTheWholeText asserts the server's text is shown past 200
// bytes and cut at 2048 with the ellipsis on a character boundary, and that a
// text whose marker lies past byte 2048 still gets VALIDATION: the match reads
// the whole message.
func TestPodRefusalShowsTheWholeText(t *testing.T) {
	t.Parallel()
	const bound = 2048
	quota := podHead + "exceeded quota: q1, requested: " + strings.Repeat("a=1,", 1250) + "pods=1, used: pods=10, limited: pods=10"
	require.Greater(t, strings.Index(quota, ", limited: "), bound)
	rows := []struct {
		name string
		text string
		want string
	}{
		{"past 200 bytes", podHead + vapPrefix + strings.Repeat("x", 600), podHead + vapPrefix + strings.Repeat("x", 600)},
		{"exactly 2048 bytes", podHead + vapPrefix + strings.Repeat("x", bound-len(podHead+vapPrefix)), podHead + vapPrefix + strings.Repeat("x", bound-len(podHead+vapPrefix))},
		{"past 2048 bytes", podHead + vapPrefix + strings.Repeat("x", 3000), (podHead + vapPrefix + strings.Repeat("x", 3000))[:bound] + "…"},
		{"a marker past 2048 bytes", quota, quota[:bound] + "…"},
	}
	for _, a := range admissionActions {
		for _, form := range podAnswers {
			for _, row := range rows {
				t.Run(a.name+" "+form.name+" "+row.name, func(t *testing.T) {
					t.Parallel()
					ce, _ := runPodAnswer(t, a, form.answer(t, row.text), "v2.15.3", "")
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					assert.Equal(t, podRefusalText(a.path, row.want, a.note, false), ce.Message)
				})
			}
		}
	}
}

// TestPodRefusalHidesTheValues asserts a value of the values files is hidden in
// the shown text, uncut and where the cut falls inside it, in both responses,
// and that the filter hides a value that is part of the text the match reads
// (the pod's name, or `is forbidden`) in the shown text, and the code stays the
// same.
func TestPodRefusalHidesTheValues(t *testing.T) {
	t.Parallel()
	for _, a := range admissionActions {
		if !a.values {
			continue
		}
		for _, form := range podAnswers {
			t.Run(a.name+" "+form.name+" planted value, not cut", func(t *testing.T) {
				t.Parallel()
				token := randomToken()
				text := podHead + vapPrefix + "value " + token + " was refused"
				ce, _ := runPodAnswer(t, a, form.answer(t, text), "v2.15.3", "password: "+token+"\n")
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Contains(t, ce.Message, podRefused)
				assertNoValuePart(t, token, ce, "not cut")
				assert.Contains(t, ce.Message, "value <hidden> was refused")
			})
			t.Run(a.name+" "+form.name+" planted value, cut inside it", func(t *testing.T) {
				t.Parallel()
				token := randomToken()
				text := straddling(podHead+vapPrefix, token, 2048)
				ce, _ := runPodAnswer(t, a, form.answer(t, text), "v2.15.3", "password: "+token+"\n")
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Contains(t, ce.Message, podRefused)
				assertNoValuePart(t, token, ce, "cut")
				assert.Contains(t, ce.Message, "<hidden>")
				assert.Contains(t, ce.Message, "…; "+podRefused)
			})
			t.Run(a.name+" "+form.name+" value that is the pod's name", func(t *testing.T) {
				t.Parallel()
				ce, _ := runPodAnswer(t, a, form.answer(t, podHead+vapText), "v2.15.3", "name: helm-operation\n")
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Contains(t, ce.Message, podRefused)
				assert.Contains(t, ce.Message, `pods "<hidden>-x" is forbidden: `)
			})
			t.Run(a.name+" "+form.name+" value that is part of `is forbidden`", func(t *testing.T) {
				t.Parallel()
				ce, _ := runPodAnswer(t, a, form.answer(t, podHead+vapText), "v2.15.3", "reason: is forbidden\n")
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Contains(t, ce.Message, podRefused)
				assert.Contains(t, ce.Message, `pods "helm-operation-x" <hidden>: `)
			})
			t.Run(a.name+" "+form.name+" retryable text with a value that is part of `is forbidden`", func(t *testing.T) {
				t.Parallel()
				ce, _ := runPodAnswer(t, a, form.answer(t, podHead+notReadyText), "v2.15.3", "reason: is forbidden\n")
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.Contains(t, ce.Message, podRefused)
				assert.True(t, ce.Retryable)
			})
		}
	}
}

// negativeTexts are texts of the list of admission texts, one that is
// VALIDATION and one that is a retryable GENERIC. A wrong accept then fails
// with either code.
var negativeTexts = []string{vapText, notReadyText}

// TestOtherAnswersStayAnUnknownOutcomeIn500Form asserts every 500 response that
// is not the refusal of the pod Rancher runs Helm in, and every 502 and 504,
// stays GENERIC, not retryable, "did not say whether Rancher ran it" with the
// operations hint, with one POST, for each action.
func TestOtherAnswersStayAnUnknownOutcomeIn500Form(t *testing.T) {
	t.Parallel()
	type row struct {
		name string
		err  func(t *testing.T) error
	}
	var rows []row
	add := func(name string, err func(t *testing.T) error) { rows = append(rows, row{name, err}) }
	messageRow := func(name, message string) {
		add(name+" in Rancher's error object", func(t *testing.T) error {
			t.Helper()
			return unknownAnswer(http.StatusInternalServerError, rancherServerErrorBody(t, message))
		})
	}
	for _, text := range negativeTexts {
		for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
			add("status "+http.StatusText(status)+" with "+text[:12], func(t *testing.T) error {
				t.Helper()
				return unknownAnswer(status, rancherServerErrorBody(t, podHead+text))
			})
		}
		messageRow("operations "+text[:12], `operations.catalog.cattle.io "helm-operation-x" is forbidden: `+text)
		messageRow("roles "+text[:12], `roles.rbac.authorization.k8s.io "helm-operation-x-u-role" is forbidden: `+text)
		messageRow("secrets "+text[:12], `secrets "helm-operation-x" is forbidden: `+text)
		messageRow("clusterrolebindings "+text[:12], `clusterrolebindings.rbac.authorization.k8s.io "helm-op-x" is forbidden: `+text)
		messageRow("a pod of another name "+text[:12], `pods "other-x" is forbidden: `+text)
		messageRow("a pod name that is another prefix "+text[:12], `pods "other-" is forbidden: `+text)
		messageRow("no name "+text[:12], `pods is forbidden: `+text)
		messageRow("a wrapped message "+text[:12], `failed to create pod: `+podHead+text)
	}
	messageRow("the authorizer's text", podHead+`User "u" cannot create resource "pods" in API group "" in the namespace "n"`)
	messageRow("an admission webhook's denial", `admission webhook "w" denied the request: no helm pods`)
	messageRow("a webhook's denial of the pod", podHead+`admission webhook "w" denied the request: no helm pods`)
	message := podHead + vapText
	badObject := func(name string, obj map[string]any) {
		add(name, func(t *testing.T) error {
			t.Helper()
			b, err := json.Marshal(obj)
			require.NoError(t, err)
			return unknownAnswer(http.StatusInternalServerError, b)
		})
	}
	badObject("a code other than ServerError", map[string]any{"type": "error", "code": "InternalError", "message": message, "status": 500})
	badObject("no type", map[string]any{"code": "ServerError", "message": message, "status": 500})
	badObject("a type other than error", map[string]any{"type": "collection", "code": "ServerError", "message": message, "status": 500})
	badObject("a status of 400", map[string]any{"type": "error", "code": "ServerError", "message": message, "status": 400})
	badObject("no status", map[string]any{"type": "error", "code": "ServerError", "message": message})
	add("a Kubernetes Status", func(t *testing.T) error {
		t.Helper()
		return unknownAnswer(http.StatusInternalServerError, kubernetesStatusBody(t, http.StatusInternalServerError, message))
	})
	add("a plain-text body", func(*testing.T) error { return unknownAnswer(http.StatusInternalServerError, []byte(message)) })
	add("a nil body", func(*testing.T) error { return unknownAnswer(http.StatusInternalServerError, nil) })
	add("a transport error after the write", func(*testing.T) error {
		return &engine.ActionOutcomeUnknown{Err: errors.New("connection reset by peer")}
	})

	for _, a := range admissionActions {
		for _, r := range rows {
			t.Run(a.name+" "+r.name, func(t *testing.T) {
				t.Parallel()
				ce, _ := runPodAnswer(t, a, r.err(t), "v2.15.3", "")
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.False(t, ce.Retryable)
				assert.Contains(t, ce.Message, "the answer did not say whether Rancher ran it")
				assert.NotContains(t, ce.Message, podRefused)
				assert.Equal(t, clierr.Rancher+" get operations --cluster 'prod' --namespace 'web'", ce.Hint)
			})
		}
	}
}

// TestOtherAnswersKeepTheirRefusedErrorIn403Form asserts a 403 response that is
// not the refusal of the pod Rancher runs Helm in gets the error refused gives
// and no more: the status table's code, no words that Rancher created no
// operation, no sentence on the image pull secrets, one POST, for each action.
func TestOtherAnswersKeepTheirRefusedErrorIn403Form(t *testing.T) {
	t.Parallel()
	type row struct {
		name    string
		message string
		code    string
	}
	var rows []row
	for _, text := range negativeTexts {
		listed := clierr.CodeValidation
		if text == notReadyText {
			listed = clierr.CodeGeneric
		}
		rows = append(rows,
			row{"operations " + text[:12], `operations.catalog.cattle.io "helm-operation-x" is forbidden: ` + text, listed},
			row{"roles " + text[:12], `roles.rbac.authorization.k8s.io "helm-operation-x-u-role" is forbidden: ` + text, listed},
			row{"secrets " + text[:12], `secrets "helm-operation-x" is forbidden: ` + text, listed},
			row{"clusterrolebindings " + text[:12], `clusterrolebindings.rbac.authorization.k8s.io "helm-op-x" is forbidden: ` + text, listed},
			row{"a pod of another name " + text[:12], `pods "other-x" is forbidden: ` + text, listed},
			row{"a pod name that is another prefix " + text[:12], `pods "other-" is forbidden: ` + text, listed},
			row{"no name " + text[:12], `pods is forbidden: ` + text, listed},
			row{"a wrapped message " + text[:12], `failed to create pod: ` + podHead + text, clierr.CodeRBAC},
		)
	}
	rows = append(rows, row{"the authorizer's text", podHead + `User "u" cannot create resource "pods" in API group "" in the namespace "n"`, clierr.CodeRBAC})
	for _, a := range admissionActions {
		for _, r := range rows {
			t.Run(a.name+" "+r.name, func(t *testing.T) {
				t.Parallel()
				body := string(kubernetesStatusBody(t, http.StatusForbidden, r.message))
				ce, _ := runPodAnswer(t, a, refusal(http.StatusForbidden, body), "v2.15.3", "")
				assert.Equal(t, r.code, ce.Code)
				assert.NotContains(t, ce.Message, "Rancher created no operation")
				assert.NotContains(t, ce.Message, "image pull secrets")
				want := clierr.From(refused(a.path, refusal(http.StatusForbidden, body), Values{}))
				assert.Equal(t, want.Message, ce.Message)
				assert.Equal(t, want.Code, ce.Code)
				assert.Equal(t, want.Retryable, ce.Retryable)
			})
		}
	}
}
