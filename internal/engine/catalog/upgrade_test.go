package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// upgradeApp describes the App an upgrade test starts from. The zero value
// of a field takes the default noted on it.
type upgradeApp struct {
	// revision is spec.version (default 3).
	revision int64
	// status is spec.info.status (default deployed).
	status string
	// version is the chart version the App runs (default 1.0.0).
	version string
	// noSource leaves the source annotations out.
	noSource bool
	// source is the ClusterRepo of the source annotation (default repo1).
	source string
	// noOwner leaves the Secret owner reference out.
	noOwner bool
	// secret is the owner reference's Secret name (default the name Helm
	// gives revision).
	secret string
	// annotations are added to the chart's metadata annotations.
	annotations map[string]string
	// name and namespace are spec.name and spec.namespace (default the
	// release and namespace the command is typed with).
	name, namespace string
}

func (a upgradeApp) releaseName() string {
	if a.name == "" {
		return testRelease
	}
	return a.name
}

func (a upgradeApp) releaseNamespace() string {
	if a.namespace == "" {
		return testNS
	}
	return a.namespace
}

// releaseSecretName is the name Helm gives the Secret of a revision.
func releaseSecretName(revision int64) string {
	return fmt.Sprintf("sh.helm.release.v1.%s.v%d", testRelease, revision)
}

// releaseSecretPath is the steve path of a release Secret read with the
// release decoded.
func releaseSecretPath(secret string) string { return releaseSecretPathIn(testNS, secret) }

// releaseSecretPathIn is releaseSecretPath for a Secret in namespace ns.
func releaseSecretPathIn(ns, secret string) string {
	return "/k8s/clusters/" + testCluster + "/v1/secrets/" + ns + "/" + secret + "?includeHelmData=true"
}

// putUpgradeApp stores the App my-nginx that upgrade tests change: chart
// nginx 1.0.0 from the ClusterRepo repo1, at its default revision, deployed, with the
// owner reference to the release Secret of its revision. edits change it.
func (f *appFake) putUpgradeApp(edits ...func(*upgradeApp)) upgradeApp {
	a := upgradeApp{revision: 3, status: "deployed", version: "1.0.0", source: testRepo}
	for _, e := range edits {
		e(&a)
	}
	if a.secret == "" {
		a.secret = releaseSecretName(a.revision)
	}
	ann := map[string]any{}
	if !a.noSource {
		ann[annotationSourceRepoType] = sourceRepoTypeCluster
		ann[annotationSourceRepo] = a.source
	}
	for k, v := range a.annotations {
		ann[k] = v
	}
	obj := f.put("apps", testNS, testRelease, map[string]any{"spec": map[string]any{
		"name": a.releaseName(), "namespace": a.releaseNamespace(), "version": a.revision,
		"info":  map[string]any{"status": a.status},
		"chart": map[string]any{"metadata": map[string]any{"name": testChart, "version": a.version, "annotations": ann}},
	}})
	if !a.noOwner {
		obj.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: a.secret}})
	}
	return a
}

// releaseFixture describes the release Secret steve returns for a revision.
// The zero value of a field takes the default noted on it.
type releaseFixture struct {
	// config is the release's user values.
	config map[string]any
	// status is the release's info.status (default deployed).
	status string
	// revision is the release's version (default 3).
	revision int64
	// owner is the label owner (default helm).
	owner string
	// raw replaces the whole Secret body.
	raw string
	// release replaces data.release with this JSON.
	release string
}

// canary is in every part of a fake release that no command may print.
// putRelease serves the release Secret of the App at the path an upgrade
// reads, with the chart's own values, manifest and hooks holding the canary.
func (f *appFake) putRelease(a upgradeApp, r releaseFixture) {
	if r.status == "" {
		r.status = "deployed"
	}
	if r.revision == 0 {
		r.revision = a.revision
	}
	if r.owner == "" {
		r.owner = "helm"
	}
	body := r.raw
	if body == "" {
		release := r.release
		if release == "" {
			rel := map[string]any{
				"name": a.releaseName(), "namespace": a.releaseNamespace(), "version": r.revision,
				"info":     map[string]any{"status": r.status},
				"chart":    map[string]any{"values": map[string]any{"chartSecret": canary}},
				"manifest": "kind: Secret\ndata:\n  v: " + canary + "\n",
				"hooks":    []any{map[string]any{"manifest": canary}},
			}
			if r.config != nil {
				rel["config"] = r.config
			}
			b, err := json.Marshal(rel)
			require.NoError(f.t, err)
			release = string(b)
		}
		body = `{"metadata":{"labels":{"owner":"` + r.owner + `","name":"` + a.releaseName() + `","version":"` + fmt.Sprint(r.revision) + `"}},"data":{"release":` + release + `}}`
	}
	path := releaseSecretPathIn(a.releaseNamespace(), a.secret)
	f.root.mu.Lock()
	defer f.root.mu.Unlock()
	f.root.answers[path] = rootAnswer{status: 200, body: body}
	f.root.secretPath = path
}

// TestUpgradePreview asserts the Effect of an upgrade: an update of the App
// previewed from server reads, destructive, with no precondition, the
// version change, the value paths that change, the stored values counted,
// and no request sent.
func TestUpgradePreview(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	a := f.putUpgradeApp()
	f.putRelease(a, releaseFixture{config: map[string]any{"keep": "kept-value", "gone": "gone-value"}})
	src := files{"v.yaml": "added: " + canary + "\ngone: null\n"}
	eff, _ := plan(t, newTestUpgrade(f, src), upgradeInput(func(in *appInput) {
		in.s[flagVersion] = "2.0.0"
		in.strs[flagValues] = []string{"v.yaml"}
	}))

	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, "apps/"+testRelease, eff.Target)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.ConflictDetection)
	assert.Empty(t, eff.ResourceVersion)
	paths := diffPaths(eff)
	assert.Equal(t, "2.0.0", paths["version"])
	assert.Equal(t, "<hidden>", paths["values.added"])
	assert.Zero(t, f.postCount())
}

// storedValues is a release's stored values that already hold every value an
// upgrade of the fixture chart adds, with the values of extra merged over
// them.
func storedValues(extra map[string]any) map[string]any {
	tree := map[string]any{"global": testCattleValues()}
	mergeValues(tree, extra)
	return tree
}

// upgradeFake is a fake with the App my-nginx and the release Secret of its
// revision, whose stored values are config.
func upgradeFake(t *testing.T, config map[string]any, edits ...func(*upgradeApp)) *appFake {
	t.Helper()
	f := newAppFake(t)
	a := f.putUpgradeApp(edits...)
	f.putRelease(a, releaseFixture{config: config})
	return f
}

// planUpgrade runs the Plan of app upgrade over f.
func planUpgrade(t *testing.T, f *appFake, src engine.FileSource, in appInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return newTestUpgrade(f, src).Plan(t.Context(), in)
}

// upgradeRefusal runs the Plan of app upgrade, requires its error and that no
// request was sent, and returns the error.
func upgradeRefusal(t *testing.T, f *appFake, src engine.FileSource, in appInput) *clierr.Error {
	t.Helper()
	_, _, err := planUpgrade(t, f, src, in)
	require.Error(t, err)
	assert.Zero(t, f.postCount())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%T %v", err, err)
	return ce
}

// upgradeBodyOf runs Plan and Execute of app upgrade and returns the one body
// the fake recorded.
func upgradeBodyOf(t *testing.T, f *appFake, src engine.FileSource, in appInput) map[string]any {
	t.Helper()
	_, exec, err := planUpgrade(t, f, src, in)
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	return f.posts[0].body
}

// upgradeChartOf is the one chart entry of an upgrade body.
func upgradeChartOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	charts, _ := body["charts"].([]any)
	require.Len(t, charts, 1)
	chart, ok := charts[0].(map[string]any)
	require.True(t, ok)
	return chart
}

// upgradeValuesOf is the values of the one upgrade request f recorded.
func upgradeValuesOf(t *testing.T, f *appFake) map[string]any {
	t.Helper()
	require.Equal(t, 1, f.postCount())
	values, _ := upgradeChartOf(t, f.posts[0].body)["values"].(map[string]any)
	return values
}

// withFlag is an input edit that types a string flag.
func withFlag(name, value string) func(*appInput) {
	return func(in *appInput) { in.s[name] = value }
}

// withBool is an input edit that types a bool flag.
func withBool(name string, value bool) func(*appInput) {
	return func(in *appInput) { in.b[name] = value }
}

// requestsOf lists the paths the fake root answered that hold substr.
func requestsOf(f *appFake, substr string) []string {
	var out []string
	for _, p := range f.root.requests() {
		if strings.Contains(p, substr) {
			out = append(out, p)
		}
	}
	return out
}

// TestUpgradePlanDisplayIdentity asserts the preview names the App by the
// release, DisplayKind app, and the committed result names the same release,
// never with the dry-run preview's "*" marker.
func TestUpgradePlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	eff, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, testRelease, eff.DisplayName)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testRelease, res.Data.(result).Release)
	assert.NotContains(t, res.Text, "*")
	assert.True(t, strings.HasPrefix(res.Text, "app "+testRelease+":"), res.Text)
}
