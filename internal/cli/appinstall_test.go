package cli

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appCanary is a value no output of the app commands may show.
const appCanary = "appCanaryS3cr3tValue"

var appInstallArgs = []string{"app", "install", appRelease, "--cluster", "prod", "--namespace", appNS, "--repo", appRepo, "--chart", appChart}

var appUninstallArgs = []string{"app", "uninstall", appRelease, "--cluster", "prod", "--namespace", appNS}

func withArgs(base []string, more ...string) []string {
	return append(append([]string{}, base...), more...)
}

// TestAppInstallDryRunEnvelope asserts the decoded --dry-run envelope of an
// install through the real root: a create of the App from server reads,
// with the request's fields, the value paths (a value from a file hidden, a
// value the CLI adds shown), the preview's warnings, and no action request
// sent.
func TestAppInstallDryRunEnvelope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "community")
	stdout, stderr, err := appRunStdin(t, s, "auth:\n  password: "+appCanary+"\n",
		withArgs(appInstallArgs, "--values", "-", "--dry-run", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "Effect", env.Kind)
	assert.Equal(t, "create", env.Data["action"])
	assert.Equal(t, "apps/my-nginx", env.Data["target"])
	assert.Equal(t, "app", env.Data["displayKind"])
	assert.Equal(t, "my-nginx", env.Data["displayName"])
	assert.Equal(t, "serverReads", env.Data["previewSource"])
	assert.Equal(t, "c-aaa11", env.Data["cluster"])
	assert.Equal(t, false, env.Data["conflictDetection"])
	assert.Equal(t, false, env.Data["destructive"])
	assert.Equal(t, false, env.Data["reversible"])
	assert.NotContains(t, env.Data, "resourceVersion")
	paths := map[string]any{}
	for _, d := range env.Data["diff"].([]any) {
		e := d.(map[string]any)
		paths[e["path"].(string)] = e["to"]
	}
	assert.Equal(t, map[string]any{
		"repo": appRepo, "chart": appChart, "version": "2.0.0", "namespace": appNS, "releaseName": appRelease,
		"wait": true, "timeout": "10m0s", "values.auth.password": "<hidden>",
		"values.global.cattle.clusterId":            "c-aaa11",
		"values.global.cattle.clusterName":          appClusterName,
		"values.global.cattle.rkePathPrefix":        "",
		"values.global.cattle.rkeWindowsPathPrefix": "",
		"values.global.cattle.systemProjectId":      appSystemProject,
		"values.global.cattle.url":                  appServerURL,
	}, paths)
	assert.Contains(t, env.Warnings, "rancher-cli adds the values the Rancher UI adds when it installs a chart: "+
		"values.global.cattle.clusterId, values.global.cattle.clusterName, values.global.cattle.rkePathPrefix, "+
		"values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.systemProjectId, values.global.cattle.url")
	assert.Contains(t, env.Warnings, "this preview comes from server reads; the server has no dry-run for this action")
	assert.Contains(t, env.Warnings, "Rancher may create, change or delete image pull secrets for this release; the preview cannot list them")
	assert.Zero(t, s.postCount())
	assert.NotContains(t, stdout+stderr, appCanary)
}

// TestAppUninstallDryRunEnvelope asserts the decoded --dry-run envelope of
// an uninstall: a destructive delete of the App from server reads, and no
// action request sent.
func TestAppUninstallDryRunEnvelope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "prime")
	s.putApp("deployed")
	stdout, stderr, err := appRun(t, s, withArgs(appUninstallArgs, "--dry-run", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "delete", env.Data["action"])
	assert.Equal(t, "serverReads", env.Data["previewSource"])
	assert.Equal(t, true, env.Data["destructive"])
	assert.NotContains(t, env.Data, "resourceVersion")
	assert.NotContains(t, env.Warnings, "Rancher may delete image pull secrets for this release; the preview cannot list them",
		"the warning applies from v2.15")
	assert.Zero(t, s.postCount())
}

// TestAppCommandsSendNothingBeforeTheGates asserts no action request is
// sent on --dry-run, without --apply, without --yes for an uninstall, or
// when Execute's re-read refuses.
func TestAppCommandsSendNothingBeforeTheGates(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	cases := map[string]struct {
		args  []string
		app   string
		code  string
		setup func(*appServer)
	}{
		"install dry run":        {withArgs(appInstallArgs, "--dry-run", "-o", "json"), "", "", nil},
		"install without apply":  {withArgs(appInstallArgs, "-o", "json"), "", "CONFIRMATION_REQUIRED", nil},
		"uninstall dry run":      {withArgs(appUninstallArgs, "--dry-run", "-o", "json"), "deployed", "", nil},
		"uninstall without yes":  {withArgs(appUninstallArgs, "--apply", "-o", "json"), "deployed", "CONFIRMATION_REQUIRED", nil},
		"uninstall without both": {withArgs(appUninstallArgs, "-o", "json"), "deployed", "CONFIRMATION_REQUIRED", nil},
		"install over an app":    {withArgs(appInstallArgs, "--apply", "-o", "json"), "failed", "CONFLICT", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			if tc.app != "" {
				s.putApp(tc.app)
			}
			_, stderr, err := appRun(t, s, tc.args...)
			if tc.code == "" {
				require.NoError(t, err, stderr)
			} else {
				assert.Equal(t, tc.code, decodeErrorEnvelope(t, stderr).Code)
			}
			assert.Zero(t, s.postCount())
		})
	}
}

// TestAppInstallApply asserts an applied install through the real root:
// one action request with the body the preview showed, and the result
// naming the operation, the request id and the follow-up commands.
func TestAppInstallApply(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "community")
	stdout, stderr, err := appRunStdin(t, s, "image:\n  tag: \"2\"\n",
		withArgs(appInstallArgs, "--values", "-", "--version", "1.0.0", "--helm-timeout", "5m", "--description", "first", "--apply", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "AppOperation", env.Kind)
	assert.Equal(t, appOpName, env.Data["operationName"])
	assert.Equal(t, appNS, env.Data["operationNamespace"])
	id, _ := env.Data["requestID"].(string)
	assert.Len(t, id, 32)
	follow, _ := env.Data["followUp"].([]any)
	require.Len(t, follow, 2)
	assert.True(t, strings.HasSuffix(follow[0].(string), " get operation 'helm-operation-abcde' --cluster 'prod' --namespace 'web' --wait"), follow[0])
	assert.True(t, strings.HasSuffix(follow[1].(string), " get app 'my-nginx' --cluster 'prod' --namespace 'web'"), follow[1])
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "action=install&fieldManager=cli.rancher.io", posts[0].query)
	assert.Equal(t, map[string]any{
		"timeout": "5m0s", "wait": true, "namespace": appNS,
		"charts": []any{map[string]any{
			"chartName": appChart, "version": "1.0.0", "releaseName": appRelease, "description": "first",
			"values": map[string]any{
				"image": map[string]any{"tag": "2"},
				"global": map[string]any{"cattle": map[string]any{
					"clusterId": "c-aaa11", "clusterName": appClusterName, "rkePathPrefix": "", "rkeWindowsPathPrefix": "",
					"systemProjectId": appSystemProject, "url": appServerURL,
				}},
			},
			"annotations": map[string]any{
				"cli.rancher.io/request-id":             id,
				"catalog.cattle.io/ui-source-repo-type": "cluster",
				"catalog.cattle.io/ui-source-repo":      appRepo,
			},
		}},
	}, posts[0].body)
}

// TestAppUninstallApply asserts an applied uninstall: one action request
// with the flags given, and the result naming the operation.
func TestAppUninstallApply(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "prime")
	s.putApp("deployed")
	stdout, stderr, err := appRun(t, s, withArgs(appUninstallArgs, "--keep-history", "--no-hooks", "--apply", "--yes", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, appOpName, env.Data["operationName"])
	assert.NotContains(t, env.Data, "requestID")
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/k8s/clusters/c-aaa11/v1/catalog.cattle.io.apps/web/my-nginx", posts[0].path)
	assert.Equal(t, map[string]any{"keepHistory": true, "noHooks": true}, posts[0].body)
}

// TestAppInstallOnePostPerAnswer asserts each answer that is an error, and
// a connection closed after the request was read, gives exactly one action
// request: 429 and 503 keep the status table's retry advice; 500, 502, 504 and
// the closed connection are GENERIC, not retryable, and carry the request
// id.
func TestAppInstallOnePostPerAnswer(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	status := func(code int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, code, map[string]any{"type": "error", "status": code, "message": "no"})
		}
	}
	cases := map[string]struct {
		answer    func(http.ResponseWriter, *http.Request)
		retryable bool
		unknown   bool
	}{
		"429":   {status(http.StatusTooManyRequests), true, false},
		"503":   {status(http.StatusServiceUnavailable), true, false},
		"500":   {status(http.StatusInternalServerError), false, true},
		"502":   {status(http.StatusBadGateway), false, true},
		"504":   {status(http.StatusGatewayTimeout), false, true},
		"reset": {hangUp, false, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			s.action = tc.answer
			_, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
			require.Error(t, err)
			assert.Equal(t, 1, s.postCount())
			ce := decodeErrorEnvelope(t, stderr)
			assert.Equal(t, tc.retryable, ce.Retryable, stderr)
			if tc.unknown {
				assert.Equal(t, "GENERIC", ce.Code)
				id := s.recorded()[0].body["charts"].([]any)[0].(map[string]any)["annotations"].(map[string]any)["cli.rancher.io/request-id"].(string)
				assert.Contains(t, ce.Message, id)
				assert.Contains(t, ce.Message, "the answer did not say whether Rancher ran it")
			}
		})
	}
}

// appError is the decoded error of an envelope.
type appError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
}

func decodeErrorEnvelope(t *testing.T, raw string) appError {
	t.Helper()
	var env struct {
		Data struct {
			Error appError `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &env), raw)
	return env.Data.Error
}

// TestAppCommandsNeverPrintValues asserts no path of the two commands
// prints a value from a values file: the preview, the applied result, each
// refused action (400, 403, 404, 409, 500 and 503) whose answer quotes the
// value plainly, JSON-escaped or one line of a multi-line value, an answer
// whose outcome is unknown, a values file whose top level is a scalar, a
// YAML syntax error on the value's line and a repeated key holding the
// value; no refusal carries the YAML decoders' own text. Each run is
// checked in stdout, stderr and the decoded envelope. The values file also
// sets every path app install adds a cluster value at, so the canary there
// is a file's value too. A second canary at the two registry paths of a
// chart that declares them is shown only as those two paths' to in the
// preview, and nowhere else (registryValuesShownOnlyAsTheirTo).
func TestAppCommandsNeverPrintValues(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	multi := "first-" + appCanary + "\nsecond-line-" + appCanary
	values := "password: " + appCanary + "\nquoted: 'say \"" + appCanary + "\" <now>'\nmulti: |\n  first-" + appCanary + "\n  second-line-" + appCanary + "\n" +
		clusterCanaryValues("")
	answer := func(code int, message string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, code, map[string]any{"type": "error", "status": code, "message": message})
		}
	}
	type run struct {
		name   string
		stdin  string
		args   []string
		answer func(http.ResponseWriter, *http.Request)
	}
	runs := []run{
		{"dry run", values, withArgs(appInstallArgs, "--values", "-", "--dry-run"), nil},
		{"apply", values, withArgs(appInstallArgs, "--values", "-", "--apply"), nil},
		{"unknown outcome", values, withArgs(appInstallArgs, "--values", "-", "--apply"), hangUp},
		{"504", values, withArgs(appInstallArgs, "--values", "-", "--apply"), answer(504, "gateway "+appCanary)},
		{"scalar top level", appCanary + "\n", withArgs(appInstallArgs, "--values", "-", "--dry-run"), nil},
		{"syntax error", "a: 1\nb: [" + appCanary + "\n", withArgs(appInstallArgs, "--values", "-", "--dry-run"), nil},
		{"repeated key", "a: 1\na: " + appCanary + "\n", withArgs(appInstallArgs, "--values", "-", "--dry-run"), nil},
	}
	// The escaped value is a leaf only in its quoted form, so only the
	// filter's JSON-escaped needle can hide it: the server's message quotes
	// the request body.
	escapedValues := "quoted: 'say \"" + appCanary + "\" <now>'\n"
	escapedMessage := `refused body {"quoted":"say \"` + appCanary + `\" \u003cnow\u003e"}`
	runs = append(runs, run{"escaped only", escapedValues, withArgs(appInstallArgs, "--values", "-", "--apply"), answer(403, escapedMessage)})
	for _, code := range []int{400, 403, 404, 409, 500, 503} {
		runs = append(runs,
			run{"plain " + http.StatusText(code), values, withArgs(appInstallArgs, "--values", "-", "--apply"), answer(code, "refused "+appCanary)},
			run{"escaped " + http.StatusText(code), values, withArgs(appInstallArgs, "--values", "-", "--apply"), answer(code, `value say "`+appCanary+`" <now> refused`)},
			run{"multi-line " + http.StatusText(code), values, withArgs(appInstallArgs, "--values", "-", "--apply"), answer(code, "refused "+strings.ReplaceAll(multi, "\n", " then "))},
		)
	}
	for _, r := range runs {
		for _, format := range []string{"json", "yaml", "table"} {
			t.Run(r.name+" "+format, func(t *testing.T) {
				s := newAppServer(t, "community")
				s.action = r.answer
				stdout, stderr, _ := appRunStdin(t, s, r.stdin, withArgs(r.args, "-o", format)...)
				out := stdout + "\n" + stderr
				assert.NotContains(t, out, appCanary)
				assert.NotContains(t, out, `\"`+appCanary)
				assert.NotContains(t, out, "second-line-")
				assert.NotContains(t, out, "yaml: ", "the YAML decoders' own text never reaches the output")
			})
		}
	}
	registryValuesShownOnlyAsTheirTo(t, answer)
}

// registryCanary is the value a values file sets at both registry paths.
const registryCanary = "registryCanaryH0st.example.test"

// appRegistryPaths are the two registry paths, the only file values the
// preview shows, as strings and for a chart that declares one. A third path
// shown needs a change here.
var appRegistryPaths = []string{"values.global.cattle.systemDefaultRegistry", "values.global.systemDefaultRegistry"}

// clusterCanaryValues is a values file's global mapping with appCanary at
// every path app install adds a cluster value at, and registry, when not
// empty, at both registry paths.
func clusterCanaryValues(registry string) string {
	out := "global:\n"
	if registry != "" {
		out += "  systemDefaultRegistry: " + registry + "\n"
	}
	out += "  cattle:\n"
	for _, k := range []string{"clusterId", "clusterName", "systemProjectId", "url", "rkePathPrefix", "rkeWindowsPathPrefix"} {
		out += "    " + k + ": " + appCanary + "\n"
	}
	out += "    windows:\n      enabled: " + appCanary + "\n"
	if registry != "" {
		out += "    systemDefaultRegistry: " + registry + "\n"
	}
	return out
}

// registryValuesShownOnlyAsTheirTo runs app install of the chart that
// declares both registry paths with registryCanary at them. In the decoded
// JSON envelope of the dry run and the apply, the canary appears only as
// the to of the two registry Diff entries in the dry run, and not at all in
// the applied result, which carries no Diff; it is absent from stderr and
// from every refused or unknown answer in every format; appCanary is absent
// from every output. A canary inside a list at a registry path, on that
// chart and on one that declares neither path, and a canary string at a
// registry path of the chart that declares neither, appear in no output.
func registryValuesShownOnlyAsTheirTo(t *testing.T, answer func(int, string) func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	values := "password: " + appCanary + "\n" + clusterCanaryValues(registryCanary)
	args := slices.Clone(appInstallArgs)
	args[slices.Index(args, appChart)] = appRegistryChart
	args = append(args, "--version", appRegistryVersion, "--values", "-")
	for _, mode := range []string{"--dry-run", "--apply"} {
		t.Run("registry "+mode, func(t *testing.T) {
			s := newAppServer(t, "community")
			stdout, stderr, err := appRunStdin(t, s, values, withArgs(args, mode, "-o", "json")...)
			require.NoError(t, err, stderr)
			assert.NotContains(t, stdout+stderr, appCanary)
			assert.NotContains(t, stderr, registryCanary)
			var env any
			require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
			shown := registryCanaryPlaces(t, env, "")
			if mode == "--dry-run" {
				assert.ElementsMatch(t, appRegistryPaths, shown)
			} else {
				assert.Empty(t, shown, "the applied result carries no Diff")
			}
			for _, yamlOrTable := range []string{"yaml", "table"} {
				stdout, stderr, _ := appRunStdin(t, newAppServer(t, "community"), values, withArgs(args, mode, "-o", yamlOrTable)...)
				assert.NotContains(t, stdout+stderr, appCanary, yamlOrTable)
				assert.NotContains(t, stderr, registryCanary, yamlOrTable)
			}
		})
	}
	hidden := map[string]struct {
		chart  []string
		values string
	}{
		"list, chart declares a path": {args, "password: " + appCanary + "\nglobal:\n  systemDefaultRegistry:\n    - " + registryCanary + "\n"},
		// global.cattle: null makes the CLI add neither registry path,
		// so the file's list is the value sent.
		"list kept, chart declares a path": {args, "password: " + appCanary + "\nglobal:\n  cattle: null\n  systemDefaultRegistry:\n    - " + registryCanary + "\n"},
		"list, chart declares neither":     {withArgs(appInstallArgs, "--values", "-"), "password: " + appCanary + "\nglobal:\n  systemDefaultRegistry:\n    - " + registryCanary + "\n"},
		"string, chart declares neither":   {withArgs(appInstallArgs, "--values", "-"), "password: " + appCanary + "\nglobal:\n  systemDefaultRegistry: " + registryCanary + "\n"},
	}
	for name, h := range hidden {
		for _, mode := range []string{"--dry-run", "--apply"} {
			for _, format := range []string{"json", "yaml", "table"} {
				t.Run("registry hidden "+name+" "+mode+" "+format, func(t *testing.T) {
					stdout, stderr, err := appRunStdin(t, newAppServer(t, "community"), h.values, withArgs(h.chart, mode, "-o", format)...)
					require.NoError(t, err, stderr)
					assert.NotContains(t, stdout+stderr, registryCanary)
					assert.NotContains(t, stdout+stderr, appCanary)
				})
			}
		}
	}
	failures := map[string]func(http.ResponseWriter, *http.Request){"unknown outcome": hangUp}
	for _, code := range []int{400, 403, 404, 409, 500, 503} {
		failures[http.StatusText(code)] = answer(code, "refused "+registryCanary+" and "+appCanary)
	}
	for name, a := range failures {
		for _, format := range []string{"json", "yaml", "table"} {
			t.Run("registry "+name+" "+format, func(t *testing.T) {
				s := newAppServer(t, "community")
				s.action = a
				stdout, stderr, _ := appRunStdin(t, s, values, withArgs(args, "--apply", "-o", format)...)
				assert.NotContains(t, stdout+stderr, registryCanary)
				assert.NotContains(t, stdout+stderr, appCanary)
			})
		}
	}
}

// registryCanaryPlaces walks a decoded envelope and returns the Diff paths
// whose to is registryCanary, failing the test on any other string that
// holds it.
func registryCanaryPlaces(t *testing.T, x any, at string) []string {
	t.Helper()
	switch v := x.(type) {
	case map[string]any:
		if path, ok := v["path"].(string); ok && slices.Contains(appRegistryPaths, path) && v["to"] == registryCanary {
			rest := maps.Clone(v)
			delete(rest, "to")
			return append(registryCanaryPlaces(t, rest, at), path)
		}
		var out []string
		for k, e := range v {
			out = append(out, registryCanaryPlaces(t, e, at+"."+k)...)
		}
		return out
	case []any:
		var out []string
		for i, e := range v {
			out = append(out, registryCanaryPlaces(t, e, at+"["+strconv.Itoa(i)+"]")...)
		}
		return out
	case string:
		assert.NotContains(t, v, registryCanary, "at %s", at)
	}
	return nil
}

// TestAppInstallHelmWaitSpellings asserts, through the real parser, the
// body's wait for each spelling of --helm-wait: unset, the flag alone and
// --helm-wait=true send true, --helm-wait=false sends false.
func TestAppInstallHelmWaitSpellings(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	cases := map[string]struct {
		flags []string
		want  bool
	}{
		"unset":             {nil, true},
		"--helm-wait":       {[]string{"--helm-wait"}, true},
		"--helm-wait=true":  {[]string{"--helm-wait=true"}, true},
		"--helm-wait=false": {[]string{"--helm-wait=false"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			_, stderr, err := appRun(t, s, withArgs(withArgs(appInstallArgs, tc.flags...), "--apply", "-o", "json")...)
			require.NoError(t, err, stderr)
			posts := s.recorded()
			require.Len(t, posts, 1)
			assert.Equal(t, tc.want, posts[0].body["wait"])
		})
	}
}

// TestActionClientNeedsTheRootClient asserts the action client is built only
// from the CLI's own root client: any other root client is an internal
// error, never a panic.
func TestActionClientNeedsTheRootClient(t *testing.T) {
	t.Parallel()
	f := newClientFactory(t.TempDir(), nil)
	f.rootOnce.Do(func() { f.root = otherRoot{} })
	_, err := f.Action()
	require.ErrorContains(t, err, "internal error")
}

// otherRoot is a root client that is not the CLI's own.
type otherRoot struct{}

func (otherRoot) GetJSON(context.Context, string) ([]byte, error) { return nil, nil }

// TestAppInstallActionTimeout asserts the action client's own timeout,
// expiring after the request was read, is an unknown outcome worded as a
// timeout, with exactly one request.
func TestAppInstallActionTimeout(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	saved := actionTimeout
	actionTimeout = 3 * time.Second
	t.Cleanup(func() { actionTimeout = saved })
	s := newAppServer(t, "community")
	s.action = func(_ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) }
	_, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
	require.Error(t, err)
	assert.Equal(t, 1, s.postCount())
	ce := decodeErrorEnvelope(t, stderr)
	assert.Equal(t, "GENERIC", ce.Code)
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "the answer did not say whether Rancher ran it (the request timed out)")
}

// TestAppInstall500CarriesRancherMessage asserts a 500 answer to the install
// action is an unknown outcome that still shows Rancher's message, with
// every value of the request hidden in it.
func TestAppInstall500CarriesRancherMessage(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := newAppServer(t, "community")
	s.action = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"type": "error", "status": 500, "code": "ServerError",
			"message": "failed to fetch chart: value " + appCanary + " refused"})
	}
	stdout, stderr, err := appRunStdin(t, s, "password: "+appCanary+"\n", withArgs(appInstallArgs, "--values", "-", "--apply", "-o", "json")...)
	require.Error(t, err)
	assert.Equal(t, 1, s.postCount())
	ce := decodeErrorEnvelope(t, stderr)
	assert.Equal(t, "GENERIC", ce.Code)
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "(the server answered 500: failed to fetch chart: value <hidden> refused)")
	assert.NotContains(t, stdout+stderr, appCanary)
}

// TestAppInstall500MessageShapes asserts the message an unknown outcome
// shows from a 500 answer: a long message is cut after the values filter,
// so a value across the cut is hidden whole; a blank message, a JSON array
// and an object without a message add nothing; a plain-text body shows its
// first line.
func TestAppInstall500MessageShapes(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	long := strings.Repeat("a", 190) + appCanary + " tail"
	cases := map[string]struct {
		body      string
		contains  string
		noMessage bool
	}{
		"long":                   {`{"type":"error","message":"` + long + `"}`, "(the server answered 500: " + strings.Repeat("a", 190) + "<hidden", false},
		"blank":                  {`{"type":"error","message":"   "}`, "", true},
		"array":                  {`["` + appCanary + `"]`, "", true},
		"object without message": {`{"type":"error","code":"ServerError"}`, "", true},
		"plain text":             {"chart fetch failed " + appCanary + "\nsecond line", "(the server answered 500: chart fetch failed <hidden>)", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			s.action = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, tc.body)
			}
			stdout, stderr, err := appRunStdin(t, s, "password: "+appCanary+"\n", withArgs(appInstallArgs, "--values", "-", "--apply", "-o", "json")...)
			require.Error(t, err)
			ce := decodeErrorEnvelope(t, stderr)
			assert.Equal(t, "GENERIC", ce.Code)
			if tc.noMessage {
				assert.Contains(t, ce.Message, "(the server answered 500)")
			} else {
				assert.Contains(t, ce.Message, tc.contains)
			}
			assert.NotContains(t, stdout+stderr, appCanary)
			assert.NotContains(t, stdout+stderr, "second line")
		})
	}
}

// TestAppInstallRefusalHidesValuesBeforeTheCut asserts a refused action
// shows the status table's wording for its answer with the server's text,
// and no part of a value of the request even when it crosses the 200-byte
// cut of server text: a 403, a 409, a 503 (whose message names the status),
// a 401 (AUTH, with the credential hint), and a Kubernetes Status whose
// message over 200 bytes stays whole.
func TestAppInstallRefusalHidesValuesBeforeTheCut(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	message := "refused " + strings.Repeat("a", 185) + appCanary + " tail"
	statusMessage := "admission webhook denied the request: " + strings.Repeat("b", 180) + " value " + appCanary + " " + strings.Repeat("c", 60)
	cases := map[string]struct {
		status     int
		code       string
		retryable  bool
		kubeStatus bool
		contains   string
		hint       string
	}{
		"403": {status: http.StatusForbidden, code: "RBAC", contains: "refused " + strings.Repeat("a", 185) + "<hidden"},
		"409": {status: http.StatusConflict, code: "CONFLICT", contains: "the server reported a conflict"},
		"503": {status: http.StatusServiceUnavailable, code: "GENERIC", retryable: true, contains: "503"},
		"401": {status: http.StatusUnauthorized, code: "AUTH", hint: clierr.AuthStatusRemedy},
		"422 Kubernetes Status": {status: http.StatusUnprocessableEntity, code: "VALIDATION", kubeStatus: true,
			contains: "admission webhook denied the request: " + strings.Repeat("b", 180) + " value <hidden> " + strings.Repeat("c", 60)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			s.action = func(w http.ResponseWriter, _ *http.Request) {
				if tc.kubeStatus {
					writeJSON(w, tc.status, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure",
						"reason": "Invalid", "code": tc.status, "message": statusMessage})
					return
				}
				writeJSON(w, tc.status, map[string]any{"type": "error", "status": tc.status, "message": message})
			}
			stdout, stderr, err := appRunStdin(t, s, "password: "+appCanary+"\n", withArgs(appInstallArgs, "--values", "-", "--apply", "-o", "json")...)
			require.Error(t, err)
			assert.Equal(t, 1, s.postCount())
			ce := decodeErrorEnvelope(t, stderr)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			if tc.contains != "" {
				assert.Contains(t, ce.Message, tc.contains)
			}
			if tc.hint != "" {
				assert.True(t, strings.HasSuffix(ce.Hint, strings.TrimPrefix(tc.hint, clierr.Rancher)), ce.Hint)
			}
			assert.NotContains(t, stdout+stderr, appCanary[:minPrefix])
		})
	}
}

// minPrefix is the length of the shortest part of a value the filter
// replaces.
const minPrefix = 4

// installedValues is the values of the one install request the server
// read.
func installedValues(t *testing.T, s *appServer) map[string]any {
	t.Helper()
	posts := s.recorded()
	require.Len(t, posts, 1)
	charts, _ := posts[0].body["charts"].([]any)
	require.Len(t, charts, 1)
	values, _ := charts[0].(map[string]any)["values"].(map[string]any)
	return values
}

// TestAppInstallRegistryValues asserts the two registry values of a chart
// whose default values declare both: the Setting's default when it has no
// value, the cluster's private registry over it, the flag over both,
// neither with --no-system-default-registry; and a registry flag on a
// chart that declares neither is refused.
func TestAppInstallRegistryValues(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	chart := []string{"app", "install", appRelease, "--cluster", "prod", "--namespace", appNS, "--repo", appRepo, "--chart", appRegistryChart, "--apply", "-o", "json"}
	const clusterRegistry = "cluster-registry.example.test"
	withClusterRegistry := func(s *appServer) {
		s.answer(appClusterPath, http.StatusOK, `{"id":"c-aaa11","metadata":{"name":"c-aaa11"},"spec":{"displayName":"prod","importedConfig":{"privateRegistryURL":"`+clusterRegistry+`"}},"status":{"linuxWorkerCount":2}}`)
	}
	cases := map[string]struct {
		setup func(*appServer)
		flags []string
		want  any
	}{
		"setting default":  {nil, nil, appSettingRegistry},
		"cluster registry": {withClusterRegistry, nil, clusterRegistry},
		"flag":             {withClusterRegistry, []string{"--system-default-registry", "flag-registry.example.test"}, "flag-registry.example.test"},
		"none":             {nil, []string{"--no-system-default-registry"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAppServer(t, "community")
			if tc.setup != nil {
				tc.setup(s)
			}
			_, stderr, err := appRun(t, s, withArgs(chart, tc.flags...)...)
			require.NoError(t, err, stderr)
			global, _ := installedValues(t, s)["global"].(map[string]any)
			cattle, _ := global["cattle"].(map[string]any)
			assert.Equal(t, tc.want, global["systemDefaultRegistry"])
			assert.Equal(t, tc.want, cattle["systemDefaultRegistry"])
		})
	}
	t.Run("flag on a chart that declares neither", func(t *testing.T) {
		s := newAppServer(t, "community")
		_, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--system-default-registry", "r.example.test", "--apply", "-o", "json")...)
		require.Error(t, err)
		assert.Equal(t, "VALIDATION", decodeErrorEnvelope(t, stderr).Code)
		assert.Zero(t, s.postCount())
	})
}

// TestAppInstallDeniedSource asserts a source the caller cannot read (403)
// leaves its values out of the request with a warning, and the install goes
// on; a 404 on the management Cluster is the command's error.
func TestAppInstallDeniedSource(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Run("server-url", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.deny(appServerURLPath)
		stdout, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
		require.NoError(t, err, stderr)
		assert.Contains(t, decodeApp(t, stdout).Warnings, "cannot read the server-url setting; values.global.cattle.url was not sent")
		cattle, _ := installedValues(t, s)["global"].(map[string]any)["cattle"].(map[string]any)
		assert.NotContains(t, cattle, "url")
		assert.Equal(t, "c-aaa11", cattle["clusterId"])
	})
	t.Run("projects", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.deny(appProjectsPath)
		stdout, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
		require.NoError(t, err, stderr)
		assert.Contains(t, decodeApp(t, stdout).Warnings, "cannot list the projects of cluster 'prod'; values.global.cattle.systemProjectId was not sent")
		cattle, _ := installedValues(t, s)["global"].(map[string]any)["cattle"].(map[string]any)
		assert.NotContains(t, cattle, "systemProjectId")
	})
	t.Run("management cluster", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.deny(appClusterPath)
		stdout, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
		require.NoError(t, err, stderr)
		assert.Equal(t, 1, s.getCount(appClusterPath))
		assert.Contains(t, decodeApp(t, stdout).Warnings, "cannot read cluster 'prod'; values.global.cattle.clusterName, values.global.cattle.rkePathPrefix, values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.windows.enabled were not sent")
		cattle, _ := installedValues(t, s)["global"].(map[string]any)["cattle"].(map[string]any)
		assert.NotContains(t, cattle, "clusterName")
	})
	t.Run("management cluster not found", func(t *testing.T) {
		s := newAppServer(t, "community")
		s.answer(appClusterPath, http.StatusNotFound, `{"type":"error","status":404,"code":"NotFound","message":"not found"}`)
		_, stderr, err := appRun(t, s, withArgs(appInstallArgs, "--apply", "-o", "json")...)
		require.Error(t, err)
		assert.Equal(t, "NOT_FOUND", decodeErrorEnvelope(t, stderr).Code)
		assert.Zero(t, s.postCount())
	})
}
