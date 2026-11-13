package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var appUpgradeArgs = []string{"app", "upgrade", appRelease, "--cluster", "prod", "--namespace", appNS}

// The revision the App fixtures of the upgrade tests are at, and the one an
// upgrade makes.
const (
	upgradeRevision     = 3
	upgradeNextRevision = 4
)

// upgradeStoredCanary is a value only the release Secret's stored values
// hold: no values file of a test sets it.
const upgradeStoredCanary = "storedCanaryS3cr3tValue"

// upgradeCanaries are the values no output of app upgrade may show: the
// canary of the values files, the one only the stored values hold, and the
// release Secret's chart values, manifest and hooks.
var upgradeCanaries = []string{appCanary, upgradeStoredCanary, appStoredChartValue, appStoredManifest, appStoredHook}

// upgradeStoredConfig is the stored values of the release at upgradeRevision.
func upgradeStoredConfig() map[string]any {
	return map[string]any{
		"replicaCount": 3,
		"password":     appCanary,
		"storedSecret": upgradeStoredCanary,
		"image":        map[string]any{"tag": "stable"},
	}
}

// upgradeConfigWithClusterValues is upgradeStoredConfig with the values an
// install adds, so that an upgrade with no other change adds nothing.
func upgradeConfigWithClusterValues() map[string]any {
	config := upgradeStoredConfig()
	config["global"] = map[string]any{"cattle": map[string]any{
		"clusterId": "c-aaa11", "clusterName": appClusterName, "rkePathPrefix": "", "rkeWindowsPathPrefix": "",
		"systemProjectId": appSystemProject, "url": appServerURL,
	}}
	return config
}

// upgradeServer is a server whose release is at upgradeRevision with status
// deployed and holds upgradeStoredConfig.
func upgradeServer(t *testing.T) *appServer {
	t.Helper()
	s := newAppServer(t, "community")
	s.putRelease("deployed", upgradeRevision, upgradeStoredConfig())
	return s
}

// upgradedBy answers an upgrade action with the done Operation and the App at
// the next revision, deployed, that the request id the action carries
// produced.
func upgradedBy(s *appServer, status string, description string) {
	s.completeOnAction(opStatus([3]string{"Reconciling", "False", ""}, [3]string{"Stalled", "False", ""}),
		func(id string) map[string]any { return appObject(status, id, upgradeNextRevision, description) })
}

// upgradeDiff is the Diff of a decoded Effect by path.
func upgradeDiff(t *testing.T, env appEnvelope) map[string]map[string]any {
	t.Helper()
	diff := map[string]map[string]any{}
	entries, _ := env.Data["diff"].([]any)
	for _, d := range entries {
		e, _ := d.(map[string]any)
		path, _ := e["path"].(string)
		diff[path] = e
	}
	return diff
}

// TestAppUpgradeDryRunEnvelope asserts the decoded --dry-run envelope of an
// upgrade through the real root: an update of the App from server reads,
// destructive, with no precondition, the revision the upgrade makes, the
// value paths with their values hidden and the values the CLI adds shown, the
// stored values read once from the release Secret with includeHelmData=true,
// and no action request sent.
func TestAppUpgradeDryRunEnvelope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := upgradeServer(t)
	stdout, stderr, err := appRunStdin(t, s, "replicaCount: 5\nextra: "+appCanary+"\n",
		withArgs(appUpgradeArgs, "--values", "-", "--dry-run", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "Effect", env.Kind)
	assert.Equal(t, "update", env.Data["action"])
	assert.Equal(t, "apps/my-nginx", env.Data["target"])
	assert.Equal(t, "app", env.Data["displayKind"])
	assert.Equal(t, "my-nginx", env.Data["displayName"])
	assert.Equal(t, "serverReads", env.Data["previewSource"])
	assert.Equal(t, "c-aaa11", env.Data["cluster"])
	assert.Equal(t, true, env.Data["destructive"])
	assert.Equal(t, false, env.Data["reversible"])
	assert.Equal(t, false, env.Data["conflictDetection"])
	assert.NotContains(t, env.Data, "resourceVersion")

	diff := upgradeDiff(t, env)
	assert.Equal(t, appRepo, diff["repo"]["to"])
	assert.Equal(t, "2.0.0", diff["version"]["to"])
	assert.Equal(t, appNS, diff["namespace"]["to"])
	assert.Equal(t, appRelease, diff["releaseName"]["to"])
	assert.EqualValues(t, upgradeRevision, diff["revision"]["from"])
	assert.EqualValues(t, upgradeNextRevision, diff["revision"]["to"])
	assert.Equal(t, "<hidden>", diff["values.replicaCount"]["from"])
	assert.Equal(t, "<hidden>", diff["values.replicaCount"]["to"])
	assert.Equal(t, "<hidden>", diff["values.extra"]["to"])
	assert.Equal(t, "c-aaa11", diff["values.global.cattle.clusterId"]["to"])
	assert.NotContains(t, diff, "values.storedSecret", "a stored value resent unchanged is counted, not listed")

	assert.Contains(t, env.Warnings, "this preview comes from server reads; the server has no dry-run for this action")
	assert.Contains(t, env.Warnings, fmt.Sprintf("the release's values come from revision %d (status deployed), Secret '%s'", upgradeRevision, appReleaseSecretName(upgradeRevision)))
	assert.Zero(t, s.postCount())
	assert.Equal(t, 1, s.getCount(s.releaseSecret))
	for _, canary := range upgradeCanaries {
		assert.NotContains(t, stdout+stderr, canary)
	}
}

// TestAppUpgradeApply asserts an applied upgrade through the real root: one
// action request to the ClusterRepo's upgrade action with the stored values
// resent and merged with the file's, the App's release and namespace, the
// version the App runs, the request id and the source annotations, and no
// install field; and the result naming the operation, the request id and the
// follow-up commands.
func TestAppUpgradeApply(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := upgradeServer(t)
	stdout, stderr, err := appRunStdin(t, s, "replicaCount: 5\n",
		withArgs(appUpgradeArgs, "--values", "-", "--helm-timeout", "5m", "--description", "bigger", "--apply", "--yes", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "AppOperation", env.Kind)
	assert.Equal(t, appOpName, env.Data["operationName"])
	id, _ := env.Data["requestID"].(string)
	assert.Len(t, id, 32)
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, appDown+"/v1/catalog.cattle.io.clusterrepos/"+appRepo, posts[0].path)
	assert.Equal(t, "action=upgrade&fieldManager=cli.rancher.io", posts[0].query)
	assert.NotContains(t, posts[0].body, "install")
	assert.Equal(t, map[string]any{
		"timeout": "5m0s", "wait": true, "namespace": appNS,
		"charts": []any{map[string]any{
			"chartName": appChart, "version": "2.0.0", "releaseName": appRelease, "description": "bigger",
			"values": map[string]any{
				"replicaCount": float64(5), "password": appCanary, "storedSecret": upgradeStoredCanary,
				"image": map[string]any{"tag": "stable"},
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

// TestAppUpgradeNeedsBothGates asserts an upgrade without --apply, or without
// --yes, is refused with CONFIRMATION_REQUIRED naming the flags missing, and
// sends no action request.
func TestAppUpgradeNeedsBothGates(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	cases := map[string]struct {
		flags   []string
		missing []string
	}{
		"neither":      {nil, []string{"--apply", "--yes"}},
		"only apply":   {[]string{"--apply"}, []string{"--yes"}},
		"only yes":     {[]string{"--yes"}, []string{"--apply"}},
		"dry run only": {[]string{"--dry-run"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := upgradeServer(t)
			_, stderr, err := appRun(t, s, withArgs(appUpgradeArgs, append(tc.flags, "--description", "x", "-o", "json")...)...)
			if tc.missing == nil {
				require.NoError(t, err, stderr)
			} else {
				require.Error(t, err)
				ce := decodeErrorEnvelope(t, stderr)
				assert.Equal(t, "CONFIRMATION_REQUIRED", ce.Code)
				for _, flag := range tc.missing {
					assert.Contains(t, ce.Message, flag)
				}
			}
			assert.Zero(t, s.postCount())
		})
	}
}

// TestAppUpgradeRefusals asserts each refusal that needs only the App and the
// release Secret: the code of its error and no action request. A refusal of
// the stored values names --reset-values only when the caller did not type it.
func TestAppUpgradeRefusals(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	withStatus := func(status string) func(*appServer) {
		return func(s *appServer) { s.putRelease(status, upgradeRevision, upgradeStoredConfig()) }
	}
	withoutOwner := func(s *appServer) {
		app := upgradeApp("deployed", upgradeRevision)
		delete(app["metadata"].(map[string]any), "ownerReferences")
		s.put(appAppPath(appNS, appRelease), app)
	}
	secretAnswer := func(status int, body string) func(*appServer) {
		return func(s *appServer) { s.answer(s.releaseSecret, status, body) }
	}
	cases := map[string]struct {
		setup func(*appServer)
		flags []string
		code  string
	}{
		"app absent": {func(s *appServer) {
			s.mu.Lock()
			delete(s.objects, appAppPath(appNS, appRelease))
			s.mu.Unlock()
		}, nil, "NOT_FOUND"},
		"uninstalled":             {withStatus("uninstalled"), []string{"--description", "x"}, "CONFLICT"},
		"uninstalling":            {withStatus("uninstalling"), []string{"--description", "x"}, "CONFLICT"},
		"pending-upgrade":         {withStatus("pending-upgrade"), []string{"--description", "x"}, "CONFLICT"},
		"unknown status":          {withStatus("unknown"), []string{"--description", "x"}, "CONFLICT"},
		"another chart":           {nil, []string{"--chart", "other"}, "VALIDATION"},
		"force and ownership":     {nil, []string{"--force", "--take-ownership"}, "VALIDATION"},
		"history max zero":        {nil, []string{"--history-max", "0"}, "VALIDATION"},
		"history max over range":  {nil, []string{"--history-max", "101"}, "VALIDATION"},
		"both registry flags":     {nil, []string{"--system-default-registry", "r.example.test", "--no-system-default-registry"}, "VALIDATION"},
		"registry on a plain app": {nil, []string{"--system-default-registry", "r.example.test"}, "VALIDATION"},
		"unlisted version":        {nil, []string{"--version", "9.9.9"}, "VALIDATION"},
		"no owner reference":      {withoutOwner, []string{"--description", "x"}, "GENERIC"},
		"secret denied":           {func(s *appServer) { s.deny(s.releaseSecret) }, []string{"--description", "x"}, "RBAC"},
		"secret gone": {func(s *appServer) {
			s.answer(s.releaseSecret, http.StatusNotFound, `{"type":"error","status":404,"message":"not found"}`)
		}, []string{"--description", "x"}, "CONFLICT"},
		"release still a string": {secretAnswer(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{"release":"H4sIAAAA"}}`), []string{"--description", "x"}, "GENERIC"},
		"release absent":         {secretAnswer(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{}}`), []string{"--description", "x"}, "GENERIC"},
		"not a Helm secret":      {secretAnswer(http.StatusOK, `{"metadata":{"labels":{}},"data":{"release":{"name":"my-nginx"}}}`), []string{"--description", "x"}, "GENERIC"},
		"another revision": {secretAnswer(http.StatusOK,
			`{"metadata":{"labels":{"owner":"helm"}},"data":{"release":{"name":"my-nginx","namespace":"web","version":2,"config":{}}}}`), []string{"--description", "x"}, "CONFLICT"},
		"nothing to change": {func(s *appServer) { s.putRelease("deployed", upgradeRevision, upgradeConfigWithClusterValues()) }, nil, "VALIDATION"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := upgradeServer(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			_, stderr, err := appRun(t, s, withArgs(appUpgradeArgs, append(tc.flags, "--apply", "--yes", "-o", "json")...)...)
			require.Error(t, err)
			ce := decodeErrorEnvelope(t, stderr)
			assert.Equal(t, tc.code, ce.Code, stderr)
			assert.Zero(t, s.postCount())
			for _, canary := range upgradeCanaries {
				assert.NotContains(t, stderr, canary)
			}
		})
	}
}

// TestAppUpgradeResetValuesGoesOnWithoutTheStoredValues asserts that with
// --reset-values a Secret the caller cannot read is a warning, not an
// error, and the request sends resetValues with the file's values and the
// values the CLI adds, none of the stored values.
func TestAppUpgradeResetValuesGoesOnWithoutTheStoredValues(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := upgradeServer(t)
	s.deny(s.releaseSecret)
	stdout, stderr, err := appRunStdin(t, s, "replicaCount: 7\n",
		withArgs(appUpgradeArgs, "--reset-values", "--values", "-", "--apply", "--yes", "-o", "json")...)
	require.NoError(t, err, stderr)
	assert.Contains(t, strings.Join(decodeApp(t, stdout).Warnings, "\n"), "cannot read the release's values (you cannot get Secret 'sh.helm.release.v1.my-nginx.v3' in namespace 'web'); the preview cannot list the paths --reset-values removes", stdout)
	posts := s.recorded()
	require.Len(t, posts, 1)
	chart := posts[0].body["charts"].([]any)[0].(map[string]any)
	assert.Equal(t, true, chart["resetValues"])
	values := chart["values"].(map[string]any)
	assert.Equal(t, float64(7), values["replicaCount"])
	assert.NotContains(t, values, "storedSecret")
	assert.NotContains(t, values, "password")
	assert.Contains(t, values, "global")
}

// TestAppUpgradeWait asserts --wait through the real root: the Operation done
// and the App at the next revision deployed make the result the new revision
// and status, with the request id; and the App's request id and revision are
// the ones the request produced.
func TestAppUpgradeWait(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	s := upgradeServer(t)
	upgradedBy(s, "deployed", "Upgrade complete")
	stdout, stderr, err := appRun(t, s, withArgs(appUpgradeArgs, "--description", "x", "--wait", "--apply", "--yes", "-o", "json")...)
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, "deployed", env.Data["status"])
	assert.EqualValues(t, upgradeNextRevision, env.Data["revision"])
	id, _ := env.Data["requestID"].(string)
	assert.Len(t, id, 32)
	assert.Equal(t, 1, s.postCount())
}

// upgradeOnePostPerAnswer is the upgrade rows of TestAppInstallOnePostPerAnswer.
func upgradeOnePostPerAnswer(t *testing.T) {
	t.Helper()
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
		t.Run("upgrade "+name, func(t *testing.T) {
			s := upgradeServer(t)
			s.action = tc.answer
			_, stderr, err := appRun(t, s, withArgs(appUpgradeArgs, "--description", "x", "--apply", "--yes", "-o", "json")...)
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

// upgradeNeverPrintsValues runs app upgrade over a release whose stored
// values, chart values, manifest and hooks and whose --values files all hold
// canaries, and asserts none of them reaches stdout, stderr or the decoded
// envelope in any format, on every path the command has at this level: the
// preview, the applied result, each refused action, an unknown outcome, each
// refusal of the stored values and every end of the wait. The values the CLI
// adds are the only values the preview shows.
func upgradeNeverPrintsValues(t *testing.T, answer func(int, string) func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	multi := "first-" + appCanary + "\nsecond-line-" + appCanary
	values := "password: " + appCanary + "\nextra: 'say \"" + appCanary + "\" <now>'\nmulti: |\n  first-" + appCanary + "\n  second-line-" + appCanary + "\n"
	storedOnly := func(code int) func(http.ResponseWriter, *http.Request) {
		return answer(code, "refused "+upgradeStoredCanary)
	}
	type run struct {
		name   string
		stdin  string
		args   []string
		setup  func(*appServer)
		answer func(http.ResponseWriter, *http.Request)
	}
	upArgs := withArgs(appUpgradeArgs, "--values", "-")
	runs := []run{
		{name: "dry run", stdin: values, args: withArgs(upArgs, "--dry-run")},
		{name: "dry run reset", stdin: values, args: withArgs(upArgs, "--reset-values", "--dry-run")},
		{name: "apply", stdin: values, args: withArgs(upArgs, "--apply", "--yes")},
		{name: "apply reset", stdin: values, args: withArgs(upArgs, "--reset-values", "--apply", "--yes")},
		{name: "unknown outcome", stdin: values, args: withArgs(upArgs, "--apply", "--yes"), answer: hangUp},
		{name: "504", stdin: values, args: withArgs(upArgs, "--apply", "--yes"), answer: answer(504, "gateway "+appCanary)},
		{name: "scalar top level", stdin: appCanary + "\n", args: withArgs(upArgs, "--dry-run")},
		{name: "syntax error", stdin: "a: 1\nb: [" + appCanary + "\n", args: withArgs(upArgs, "--dry-run")},
		{name: "repeated key", stdin: "a: 1\na: " + appCanary + "\n", args: withArgs(upArgs, "--dry-run")},
		{name: "escaped only", stdin: "extra: 'say \"" + appCanary + "\" <now>'\n", args: withArgs(upArgs, "--apply", "--yes"),
			answer: answer(403, `refused body {"extra":"say \"`+appCanary+`\" <now>"}`)},
		{name: "stored value only", stdin: "other: 1\n", args: withArgs(upArgs, "--apply", "--yes"),
			answer: answer(403, `refused body {"storedSecret":"`+upgradeStoredCanary+`"}`)},
	}
	for _, code := range []int{400, 403, 404, 409, 500, 503} {
		runs = append(runs,
			run{name: "plain " + http.StatusText(code), stdin: values, args: withArgs(upArgs, "--apply", "--yes"), answer: answer(code, "refused "+appCanary)},
			run{name: "stored " + http.StatusText(code), stdin: values, args: withArgs(upArgs, "--apply", "--yes"), answer: storedOnly(code)},
			run{name: "multi-line " + http.StatusText(code), stdin: values, args: withArgs(upArgs, "--apply", "--yes"),
				answer: answer(code, "refused "+strings.ReplaceAll(multi, "\n", " then "))},
		)
	}
	secret := func(status int, body string) func(*appServer) {
		return func(s *appServer) { s.answer(s.releaseSecret, status, body) }
	}
	for name, setup := range map[string]func(*appServer){
		"secret denied":      func(s *appServer) { s.deny(s.releaseSecret) },
		"secret not found":   secret(http.StatusNotFound, `{"type":"error","status":404,"message":"`+appCanary+`"}`),
		"secret not JSON":    secret(http.StatusOK, "plain "+appCanary+" "+appStoredHook),
		"release is text":    secret(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{"release":"`+appCanary+`"}}`),
		"release is a list":  secret(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{"release":["`+appCanary+`"]}}`),
		"release bad types":  secret(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{"release":{"name":["`+appCanary+`"],"version":"`+appCanary+`"}}}`),
		"another revision":   secret(http.StatusOK, `{"metadata":{"labels":{"owner":"helm"}},"data":{"release":{"name":"my-nginx","namespace":"web","version":2,"config":{"k":"`+appCanary+`"},"manifest":"`+appStoredManifest+`"}}}`),
		"another release":    secret(http.StatusOK, `{"metadata":{"labels":{"owner":"helm","name":"other"}},"data":{"release":{"name":"my-nginx","namespace":"web","version":3,"config":{"k":"`+appCanary+`"}}}}`),
		"secret server fail": secret(http.StatusInternalServerError, `{"type":"error","status":500,"message":"the release is not available"}`),
	} {
		runs = append(runs,
			run{name: name, stdin: values, args: withArgs(upArgs, "--dry-run"), setup: setup},
			run{name: name + " apply", stdin: values, args: withArgs(upArgs, "--apply", "--yes"), setup: setup},
			run{name: name + " reset", stdin: values, args: withArgs(upArgs, "--reset-values", "--dry-run"), setup: setup},
		)
	}
	for _, status := range []string{"uninstalled", "uninstalling", "pending-install", "unknown"} {
		runs = append(runs, run{name: "status " + status, stdin: values, args: withArgs(upArgs, "--dry-run"),
			setup: func(s *appServer) { s.putRelease(status, upgradeRevision, upgradeStoredConfig()) }})
	}
	for _, r := range runs {
		for _, format := range []string{"json", "yaml", "table"} {
			t.Run("upgrade "+r.name+" "+format, func(t *testing.T) {
				s := upgradeServer(t)
				if r.setup != nil {
					r.setup(s)
				}
				s.action = r.answer
				stdout, stderr, _ := appRunStdin(t, s, r.stdin, withArgs(r.args, "-o", format)...)
				out := stdout + "\n" + stderr
				for _, canary := range upgradeCanaries {
					assert.NotContains(t, out, canary)
				}
				assert.NotContains(t, out, `\"`+appCanary)
				assert.NotContains(t, out, "second-line-")
				assert.NotContains(t, out, "yaml: ", "the YAML decoders' own text never reaches the output")
			})
		}
	}

	done := opStatus([3]string{"Reconciling", "False", ""}, [3]string{"Stalled", "False", ""})
	stalled := func(msg string) map[string]any {
		return opStatus([3]string{"Reconciling", "False", ""}, [3]string{"Stalled", "True", msg})
	}
	revision := func(status string, version int64, desc string, own bool) func(string) map[string]any {
		return func(id string) map[string]any {
			if !own {
				id = "another-request"
			}
			return appObject(status, id, version, desc)
		}
	}
	canaryDesc := `Upgrade "my-nginx" failed: password ` + appCanary + " " + upgradeStoredCanary
	waitArgs := withArgs(upArgs, "--apply", "--yes", "--wait")
	type waitRun struct {
		name  string
		args  []string
		setup func(s *appServer)
		shown string
	}
	waits := []waitRun{
		{name: "wait success", args: waitArgs, shown: fmt.Sprintf("upgraded (revision %d, status deployed)", upgradeNextRevision), setup: func(s *appServer) {
			s.completeOnAction(done, revision("deployed", upgradeNextRevision, "Upgrade complete "+appCanary, true))
		}},
		{name: "wait stalled", args: waitArgs, shown: "operation message: refused <hidden>", setup: func(s *appServer) {
			s.completeOnAction(stalled("refused "+appCanary), revision("failed", upgradeNextRevision, canaryDesc, true))
		}},
		{name: "wait failed", args: waitArgs, shown: `Upgrade "my-nginx" failed: password <hidden> <hidden>`, setup: func(s *appServer) {
			s.completeOnAction(done, revision("failed", upgradeNextRevision, canaryDesc, true))
		}},
		{name: "wait failed cleanup", args: withArgs(waitArgs, "--cleanup-on-fail"), shown: `Upgrade "my-nginx" failed: password <hidden> <hidden>`, setup: func(s *appServer) {
			s.completeOnAction(done, revision("failed", upgradeNextRevision, canaryDesc, true))
		}},
		{name: "wait failed multi-line", args: waitArgs, shown: `Upgrade "my-nginx" failed: <hidden> <hidden>`, setup: func(s *appServer) {
			s.completeOnAction(done, revision("failed", upgradeNextRevision, "Upgrade \"my-nginx\" failed:\n"+multi, true))
		}},
		{name: "wait timeout", args: withArgs(waitArgs, "--timeout", "1s"), shown: "Stalled=False; last error: GENERIC: ", setup: func(s *appServer) {
			s.completeOnAction(done, nil)
			complete := s.action
			s.action = func(w http.ResponseWriter, r *http.Request) {
				complete(w, r)
				s.answer(appAppPath(appNS, appRelease), http.StatusServiceUnavailable, `{"type":"error","status":503,"message":"busy `+appCanary+`"}`)
			}
		}},
		{name: "wait no revision", args: withArgs(waitArgs, "--timeout", "2s"), setup: func(s *appServer) {
			s.completeOnAction(stalled("boom "+appCanary), nil)
		}},
		{name: "wait another change", args: waitArgs, shown: fmt.Sprintf("another change to 'my-nginx' also ran (revision %d)", upgradeNextRevision), setup: func(s *appServer) {
			s.completeOnAction(done, revision("deployed", upgradeNextRevision, canaryDesc, false))
		}},
	}
	for _, r := range waits {
		for _, format := range []string{"json", "yaml", "table"} {
			t.Run("upgrade "+r.name+" "+format, func(t *testing.T) {
				s := upgradeServer(t)
				r.setup(s)
				stdout, stderr, _ := appRunStdin(t, s, values, withArgs(r.args, "-o", format)...)
				out := stdout + "\n" + stderr
				for _, canary := range upgradeCanaries {
					assert.NotContains(t, out, canary)
				}
				assert.NotContains(t, out, `\"`+appCanary)
				assert.NotContains(t, out, "second-line-")
				if format == "table" && r.shown != "" {
					assert.Contains(t, out, r.shown)
				}
			})
		}
	}

	t.Run("upgrade envelope holds no canary", func(t *testing.T) {
		s := upgradeServer(t)
		stdout, stderr, err := appRunStdin(t, s, values, withArgs(upArgs, "--dry-run", "-o", "json")...)
		require.NoError(t, err, stderr)
		var env any
		require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
		raw, err := json.Marshal(env)
		require.NoError(t, err)
		for _, canary := range upgradeCanaries {
			assert.NotContains(t, string(raw), canary)
		}
	})
}
