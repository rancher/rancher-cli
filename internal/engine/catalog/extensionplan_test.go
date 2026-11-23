package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

const (
	extChart      = "epinio"
	extRepo       = "partner-extensions"
	extRepoPath   = reposPath + "/" + extRepo
	extIndexPath  = extRepoPath + "?link=index"
	extUIPlugin   = "cattle-ui-plugin-system"
	extCompatible = `"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0"`
)

// extFake is the local cluster, the Rancher server and the action port as the
// extension commands see them: a community Rancher v2.15.3 on Kubernetes
// v1.36.5 with the feature, the namespace, one active repository that offers
// the chart epinio in versions 1.14.3 and 1.14.2, and no App or UIPlugin.
type extFake struct {
	*appFake
	version string
	edition engine.Edition
}

// List serves the UIPlugins of every namespace for an empty namespace, as the
// server does; any other list is the app fake's.
func (f *extFake) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	if gvr.Resource != "uiplugins" || ns != "" {
		return f.appFake.List(ctx, gvr, ns, selector)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, "list:uiplugins/")
	if err := f.errs["list:uiplugins/"]; err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	for _, k := range slices.Sorted(maps.Keys(f.objs)) {
		if strings.HasPrefix(k, "uiplugins/") {
			list.Items = append(list.Items, *f.objs[k].DeepCopy())
		}
	}
	return list, nil
}

func (f *extFake) Cluster(context.Context, string) (engine.ClusterClient, error) { return f, nil }

func (f *extFake) ServerVersion(context.Context) (string, error)         { return f.version, nil }
func (f *extFake) ServerEdition(context.Context) (engine.Edition, error) { return f.edition, nil }

// indexOfExtension is an index with one extension chart in the given versions,
// each with the annotations of extCompatible.
func indexOfExtension(chart string, versions ...string) string {
	entries := make([]string, len(versions))
	for i, v := range versions {
		entries[i] = `{"name":"` + chart + `","version":"` + v + `","digest":"sha256:` + v + `","annotations":{` + extCompatible + `}}`
	}
	return `{"entries":{"` + chart + `":[` + strings.Join(entries, ",") + `]}}`
}

func newExtFake(t *testing.T) *extFake {
	t.Helper()
	f := &extFake{appFake: newAppFake(t), version: "v2.15.3", edition: engine.EditionCommunity}
	f.put("features", "", "uiextension", map[string]any{"spec": map[string]any{"value": true}})
	f.put("namespaces", "", extUIPlugin, nil)
	f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemAt(extRepo, "active", "https://github.com/rancher/partner-extensions"))}
	f.root.answers[localCluster] = rootAnswer{status: 200, body: clusterBody("v1.36.5+k3s1", "v1.36.5+k3s1")}
	f.root.answers[extIndexPath] = rootAnswer{status: 200, body: indexOfExtension(extChart, "1.14.3", "1.14.2")}
	f.root.answers[settingPath(settingSystemDefaultRegistry)] = rootAnswer{status: 200, body: `{"id":"system-default-registry","value":"","default":""}`}
	f.postAnswer = func(string) ([]byte, error) {
		return []byte(`{"type":"chartActionOutput","links":{},"operationName":"helm-operation-abcde","operationNamespace":"` + extUIPlugin + `"}`), nil
	}
	return f
}

func newTestExtensionInstall(f *extFake) *extensionInstallCmd {
	return &extensionInstallCmd{clients: f, roots: f, actions: f,
		now: func() time.Time { return testNow }, newID: func() string { return testRequestID }, interval: time.Second}
}

// extInput is the input of an extension command: the chart and the flags.
func extInput(chart string, flags ...string) appInput {
	in := appInput{s: map[string]string{}, b: map[string]bool{}, strs: map[string][]string{}, args: []string{chart}}
	for i := 0; i+1 < len(flags); i += 2 {
		in.s[flags[i]] = flags[i+1]
	}
	return in
}

func errCode(err error) string { return clierr.From(err).Code }

// TestExtensionInstallPreview asserts the Effect of an install: a create of the
// App from server reads with no precondition, the chart's first compatible
// version, the fixed namespace and release name, and no option the Rancher UI
// does not send.
func TestExtensionInstallPreview(t *testing.T) {
	t.Parallel()
	f := newExtFake(t)
	eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart))
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.False(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	assert.False(t, eff.ConflictDetection)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.Equal(t, engine.LocalCluster, eff.Cluster)
	d := diffPaths(eff)
	assert.Equal(t, map[string]any{"repo": extRepo, "chart": extChart, "version": "1.14.3", "namespace": extUIPlugin, "releaseName": extChart}, d)
	assert.Contains(t, eff.Warnings, warnServerReads)
	assert.Zero(t, f.postCount())
}

// TestExtensionInstallBody asserts the request: the exact key set of the body
// and of its chart entry, values sent as {} and the local path.
func TestExtensionInstallBody(t *testing.T) {
	t.Parallel()
	f := newExtFake(t)
	_, exec := plan(t, newTestExtensionInstall(f), extInput(extChart))
	_, err := exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	p := f.posts[0]
	assert.Equal(t, "/v1/catalog.cattle.io.clusterrepos/"+extRepo+"?action=install", p.path)
	assert.NotContains(t, p.path, "/k8s/clusters/")
	assert.ElementsMatch(t, []string{"namespace", "charts"}, keysOf(p.body))
	assert.Equal(t, extUIPlugin, p.body["namespace"])
	chart := p.body["charts"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"chartName", "version", "releaseName", "values", "annotations"}, keysOf(chart))
	assert.Equal(t, extChart, chart["releaseName"])
	assert.Equal(t, map[string]any{}, chart["values"])
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExtensionInstallRefusals asserts each refusal of the install plan: its
// code, its text and that no request is sent.
func TestExtensionInstallRefusals(t *testing.T) {
	t.Parallel()
	app := func(status string) func(*extFake) {
		return func(f *extFake) {
			f.put("apps", extUIPlugin, extChart, map[string]any{"spec": map[string]any{"info": map[string]any{"status": status}}})
		}
	}
	cases := []struct {
		name  string
		setup func(*extFake)
		in    appInput
		code  string
		text  string
		hint  string
	}{
		{name: "app deployed", setup: app("deployed"), in: extInput(extChart), code: clierr.CodeConflict,
			text: "extension 'epinio' is already installed: App 'epinio' in namespace cattle-ui-plugin-system (status deployed)",
			hint: clierr.Rancher + " extension upgrade 'epinio' --dry-run"},
		{name: "app failed", setup: app("failed"), in: extInput(extChart), code: clierr.CodeConflict,
			text: "extension 'epinio' is already installed: App 'epinio' in namespace cattle-ui-plugin-system (status failed)"},
		{name: "app uninstalled with kept history", setup: app("uninstalled"), in: extInput(extChart), code: clierr.CodeConflict,
			text: "App 'epinio' in namespace cattle-ui-plugin-system has the status uninstalled, and Helm does not install a release of that name over an App with that status",
			hint: clierr.Rancher + " get app 'epinio' --cluster 'local' --namespace 'cattle-ui-plugin-system'"},
		{name: "uiplugin without an app", in: extInput(extChart), code: clierr.CodeConflict,
			setup: func(f *extFake) {
				f.put("uiplugins", "other-ns", "dev-load", map[string]any{"spec": map[string]any{"plugin": map[string]any{"name": extChart, "version": "0.1.0"}}})
			},
			text: "an extension named 'epinio' is already loaded (UIPlugin 'other-ns/dev-load'); an extension loaded from a URL is uninstalled in the Rancher UI"},
		{name: "no repository offers the chart", in: extInput("nothing"), code: clierr.CodeValidation,
			text: "no ClusterRepo the Rancher UI reads offers an extension named 'nothing'", hint: clierr.Rancher + " extension list"},
		{name: "no repository offers the chart and one was skipped", in: extInput("nothing"), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					repoItemAt(extRepo, "active", "https://github.com/rancher/partner-extensions"), repoItemAt("slow", "downloading", "https://example.test/slow"))}
			},
			text: "no ClusterRepo the Rancher UI reads offers an extension named 'nothing'; skipped ClusterRepo 'slow': its state is downloading"},
		{name: "two repositories offer the chart", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					repoItemAt(extRepo, "active", "https://github.com/rancher/partner-extensions"), repoItemAt("mirror", "active", "https://example.test/mirror"))}
				f.root.answers[reposPath+"/mirror?link=index"] = rootAnswer{status: 200, body: indexOfExtension(extChart, "1.14.3")}
			},
			text: "extension 'epinio' is offered by more than one ClusterRepo: 'mirror', 'partner-extensions'; name one with --repo",
			hint: clierr.Rancher + " extension install 'epinio' --repo 'mirror' --dry-run"},
		{name: "repository absent", in: extInput(extChart, flagRepo, "nope"), code: clierr.CodeNotFound,
			text: "ClusterRepo 'nope' not found", hint: clierr.Rancher + " get clusterrepos --cluster 'local'"},
		{name: "repository hidden", in: extInput(extChart, flagRepo, "quiet"), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					repoItemBody("quiet", "active", `,"annotations":{"clusterrepo.cattle.io/hidden":"true"}`))}
			},
			text: "ClusterRepo 'quiet' is hidden, and the Rancher UI does not read extensions from it"},
		{name: "repository disabled", in: extInput(extChart, flagRepo, "off"), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					`{"metadata":{"name":"off","state":{"name":"active"}},"spec":{"enabled":false}}`)}
			},
			text: "ClusterRepo 'off' is disabled"},
		{name: "repository not active", in: extInput(extChart, flagRepo, "slow"), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemAt("slow", "downloading", "https://example.test/slow"))}
			},
			text: "ClusterRepo 'slow' is not ready: its state is downloading", hint: clierr.Rancher + " get clusterrepo 'slow' --cluster 'local'"},
		{name: "index still downloading", in: extInput(extChart, flagRepo, extRepo), code: clierr.CodeValidation,
			setup: func(f *extFake) { f.root.answers[extIndexPath] = rootAnswer{status: 503} },
			text:  "ClusterRepo 'partner-extensions' is not ready: Rancher is still downloading it"},
		{name: "index forbidden", in: extInput(extChart, flagRepo, extRepo), code: clierr.CodeRBAC,
			setup: func(f *extFake) { f.root.answers[extIndexPath] = rootAnswer{status: 403} }},
		{name: "chart is not an extension", in: extInput("plain", flagRepo, extRepo), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"plain":[{"name":"plain","version":"1.0.0"}]}}`}
			},
			text: "chart 'plain' in ClusterRepo 'partner-extensions' is not a Rancher UI extension",
			hint: clierr.Rancher + " charts --cluster 'local' --repo 'partner-extensions'"},
		{name: "version not in the index", in: extInput(extChart, flagVersion, "9.9.9"), code: clierr.CodeValidation,
			text: "version '9.9.9' of extension 'epinio' is not in the index ClusterRepo 'partner-extensions' serves; --version names one exact version; the index has 1.14.3, 1.14.2"},
		{name: "operation running", in: extInput(extChart), code: clierr.CodeConflict,
			setup: func(f *extFake) {
				f.put("operations", extUIPlugin, "helm-operation-a", operation(extChart, testNow.Add(-time.Minute), "True", 1, 1))
			},
			text: "operation 'helm-operation-a' for release 'epinio' in namespace 'cattle-ui-plugin-system' may still be running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, _, err := newTestExtensionInstall(f).Plan(t.Context(), tc.in)
			require.Error(t, err)
			ce := clierr.From(err)
			assert.Equal(t, tc.code, ce.Code, ce.Message)
			if tc.text != "" {
				require.ErrorContains(t, err, tc.text)
			}
			if tc.hint != "" {
				assert.Equal(t, tc.hint, ce.Hint)
			}
			assert.Zero(t, f.postCount())
		})
	}
}

// TestExtensionInstallBundleRefusal asserts the bundle rule at every position
// of the annotation: first version, a later one, an empty value and none.
func TestExtensionInstallBundleRefusal(t *testing.T) {
	t.Parallel()
	member := `"catalog.cattle.io/ui-extensions-bundle-members":"a,b"`
	ver := func(v, extra string) string {
		return `{"name":"epinio","version":"` + v + `","annotations":{` + extCompatible + extra + `}}`
	}
	cases := []struct {
		name    string
		first   string
		second  string
		refused bool
	}{
		{name: "first version", first: "," + member, refused: true},
		{name: "a later version", second: "," + member, refused: true},
		{name: "empty value", first: `,"catalog.cattle.io/ui-extensions-bundle-members":""`},
		{name: "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + ver("2.0.0", tc.first) + `,` + ver("1.0.0", tc.second) + `]}}`}
			_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "this chart installs several extensions together; install it from the Rancher UI")
			assert.Equal(t, clierr.CodeValidation, errCode(err))
		})
	}
}

// TestExtensionInstallVersionChoice asserts the default version rule and its
// warnings: the first compatible version in index order, skipping a newer
// incompatible one, with a warning for a prerelease or deprecated default.
func TestExtensionInstallVersionChoice(t *testing.T) {
	t.Parallel()
	entry := func(v, annotations string, deprecated bool) string {
		d := ""
		if deprecated {
			d = `,"deprecated":true`
		}
		return `{"name":"epinio","version":"` + v + `"` + d + `,"annotations":{` + extCompatible + annotations + `}}`
	}
	cases := []struct {
		name    string
		index   string
		version string
		warning string
	}{
		{name: "default version rule", index: entry("3.0.0", `,"catalog.cattle.io/rancher-version":">= 9.0.0"`, false) + "," + entry("2.0.0", "", false), version: "2.0.0"},
		{name: "prerelease default", index: entry("2.0.0-rc.1", "", false) + "," + entry("1.0.0", "", false), version: "2.0.0-rc.1",
			warning: "2.0.0-rc.1 is a prerelease version of extension epinio; name another with --version"},
		{name: "deprecated default", index: entry("2.0.0", "", true) + "," + entry("1.0.0", "", false), version: "2.0.0",
			warning: "2.0.0 of extension epinio is deprecated; name another with --version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + tc.index + `]}}`}
			eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart))
			assert.Equal(t, tc.version, diffPaths(eff)["version"])
			if tc.warning != "" {
				assert.Contains(t, eff.Warnings, tc.warning)
			}
		})
	}
}

// TestExtensionInstallNeverInstallsAVersionThatFailsACheck asserts, for every
// check of the Rancher UI and the missing annotation, that a typed version that
// fails only that check is refused with the check's reason and no request, and
// that without --version the default is the older version that passes; an index
// whose only version fails gives the refusal of no compatible version.
func TestExtensionInstallNeverInstallsAVersionThatFailsACheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		annotations string
		reason      string
	}{
		{name: "kube-version", annotations: extCompatible + `,"catalog.cattle.io/kube-version":">= 1.40.0-0"`, reason: "requires Kubernetes >= 1.40.0-0; the local cluster runs v1.36.5"},
		{name: "rancher-version", annotations: extCompatible + `,"catalog.cattle.io/rancher-version":">= 2.16.0"`, reason: "requires Rancher >= 2.16.0; this server is v2.15.3"},
		{name: "ui-version", annotations: extCompatible + `,"catalog.cattle.io/ui-version":">= 2.16.0"`, reason: "requires Rancher >= 2.16.0; this server is v2.15.3"},
		{name: "ui-extensions-version", annotations: `"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 9.0.0"`, reason: "requires Extensions API version >= 9.0.0"},
		{name: "ui-extensions-version missing", annotations: `"catalog.cattle.io/ui-component":"plugins"`, reason: "has no annotation catalog.cattle.io/ui-extensions-version, which the Rancher UI requires"},
		{name: "ui-extensions-host", annotations: extCompatible + `,"catalog.cattle.io/ui-extensions-host":"rancher-manager"`, reason: "has ui-extensions-host rancher-manager, which is not compatible with the Rancher UI"},
		{name: "prime-only", annotations: extCompatible + `,"catalog.cattle.io/prime-only":"true"`, reason: "requires Rancher Prime"},
	}
	bad := func(v, ann string) string {
		return `{"name":"epinio","version":"` + v + `","annotations":{` + ann + `}}`
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + bad("2.0.0", tc.annotations) + `,` + bad("1.0.0", extCompatible) + `]}}`}
			_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart, flagVersion, "2.0.0"))
			require.ErrorContains(t, err, "version '2.0.0' of extension 'epinio' is not compatible with this server: ")
			require.ErrorContains(t, err, tc.reason)
			assert.Equal(t, clierr.CodeValidation, errCode(err))
			assert.Zero(t, f.postCount())

			eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart))
			assert.Equal(t, "1.0.0", diffPaths(eff)["version"])

			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + bad("2.0.0", tc.annotations) + `]}}`}
			_, _, err = newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
			require.ErrorContains(t, err, "no version of extension 'epinio' in ClusterRepo 'partner-extensions' is compatible with this server; the newest, 2.0.0, ")
			assert.Equal(t, clierr.Rancher+" extension list", clierr.From(err).Hint)
		})
	}
}

// TestExtensionInstallRegistryRule asserts the registry the values carry: not
// read when the setting is empty, sent and shown when the setting is set and
// the chart's image repository starts with rancher/, not sent for another
// repository, and a refused setting read is RBAC.
func TestExtensionInstallRegistryRule(t *testing.T) {
	t.Parallel()
	info := func(repository string) rootAnswer {
		return rootAnswer{status: 200, body: `{"values":{"image":{"repository":"` + repository + `"}},"chart":{"version":"1.14.3"}}`}
	}
	infoURL := extRepoPath + "?link=info&chartName=epinio&version=1.14.3"
	set := rootAnswer{status: 200, body: `{"id":"system-default-registry","value":"registry.example.test","default":""}`}
	cases := []struct {
		name     string
		setting  rootAnswer
		info     rootAnswer
		registry string
		infoRead bool
		rbac     bool
		refusal  string
		// code and hint are the error's code (VALIDATION when empty) and hint
		// (the extension list command when empty).
		code, hint string
	}{
		{name: "setting empty", setting: rootAnswer{status: 200, body: `{"value":"","default":""}`}},
		{name: "rancher image", setting: set, info: info("rancher/epinio-ui"), registry: "registry.example.test", infoRead: true},
		{name: "other image", setting: set, info: info("other/epinio-ui"), infoRead: true},
		{name: "setting denied", setting: rootAnswer{status: 403}, rbac: true},
		{name: "info of another version", setting: set, infoRead: true,
			info:    rootAnswer{status: 200, body: `{"values":{"image":{"repository":"rancher/epinio-ui"}},"chart":{"version":"1.14.2"}}`},
			refusal: "version '1.14.3' of extension 'epinio' is no longer in ClusterRepo 'partner-extensions' (Rancher returned version '1.14.2' instead)"},
		{name: "version gone from the repository", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart version found for epinio-1.14.3"}`},
			refusal: "version '1.14.3' of extension 'epinio' is no longer in ClusterRepo 'partner-extensions'; name one with --version"},
		{name: "chart gone from the repository", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart name found for epinio"}`},
			refusal: "extension 'epinio' is no longer in ClusterRepo 'partner-extensions'"},
		{name: "chart with no versions", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart version found"}`},
			refusal: "extension 'epinio' has no versions in ClusterRepo 'partner-extensions'"},
		{name: "repository gone", setting: set, infoRead: true, info: rootAnswer{status: 404},
			refusal: "ClusterRepo 'partner-extensions'", code: clierr.CodeNotFound, hint: clierr.Rancher + " get clusterrepos --cluster 'local'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.root.answers[settingPath(settingSystemDefaultRegistry)] = tc.setting
			f.root.answers[infoURL] = tc.info
			_, exec, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
			if tc.rbac {
				assert.Equal(t, clierr.CodeRBAC, errCode(err))
				require.ErrorContains(t, err, "you cannot get the setting 'system-default-registry', which the command reads to find the system default registry it sends in the values; ask for permission to get it")
				return
			}
			if tc.refusal != "" {
				require.ErrorContains(t, err, tc.refusal)
				assert.Equal(t, cmp.Or(tc.code, clierr.CodeValidation), errCode(err))
				assert.Equal(t, cmp.Or(tc.hint, clierr.Rancher+" extension list"), clierr.From(err).Hint)
				assert.Equal(t, tc.infoRead, slices.Contains(f.root.requests(), infoURL))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.infoRead, slices.Contains(f.root.requests(), infoURL))
			_, err = exec(t.Context())
			require.NoError(t, err)
			chart := f.posts[0].body["charts"].([]any)[0].(map[string]any)
			if tc.registry == "" {
				assert.Equal(t, map[string]any{}, chart["values"])
				return
			}
			assert.Equal(t, map[string]any{"global": map[string]any{"cattle": map[string]any{"systemDefaultRegistry": tc.registry}}}, chart["values"])
		})
	}
}

// TestExtensionInstallRegistrySettingNotAnObject asserts that a response to the
// read of the registry setting that is not a JSON object is a GENERIC error
// that says so.
func TestExtensionInstallRegistrySettingNotAnObject(t *testing.T) {
	t.Parallel()
	f := newExtFake(t)
	f.root.answers[settingPath(settingSystemDefaultRegistry)] = rootAnswer{status: 200, body: `[]`}
	_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
	require.ErrorContains(t, err, "the response to GET "+settingPath(settingSystemDefaultRegistry)+" on the Rancher server is not a JSON object")
	assert.Equal(t, clierr.CodeGeneric, errCode(err))
}

// TestExtensionInstallExecuteRechecks asserts that Execute refuses, with no
// request sent, when the App was created, an operation started or the index
// entry changed after the preview.
func TestExtensionInstallExecuteRechecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		after func(*extFake)
		text  string
	}{
		{name: "app created", after: func(f *extFake) { f.put("apps", extUIPlugin, extChart, nil) }, text: "app 'epinio' was created since the preview"},
		{name: "operation started", after: func(f *extFake) {
			f.put("operations", extUIPlugin, "helm-operation-b", operation(extChart, testNow.Add(-time.Minute), "True", 1, 1))
		}, text: "operation 'helm-operation-b' for release 'epinio' started since the preview"},
		{name: "index digest changed", after: func(f *extFake) {
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: strings.ReplaceAll(indexOfExtension(extChart, "1.14.3", "1.14.2"), "sha256:1.14.3", "sha256:other")}
		}, text: "the content of chart 'epinio' '1.14.3' changed in the index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			_, exec := plan(t, newTestExtensionInstall(f), extInput(extChart))
			tc.after(f)
			_, err := exec(t.Context())
			require.ErrorContains(t, err, tc.text)
			assert.Equal(t, clierr.CodeConflict, errCode(err))
			assert.Zero(t, f.postCount())
		})
	}
}

// TestExtensionInstallWait asserts that --wait follows the operation to the end
// of Helm's run through the existing wait.
func TestExtensionInstallWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newExtFake(t)
		in := extInput(extChart)
		in.b[flagWait] = true
		_, exec := plan(t, newTestExtensionInstall(f), in)
		f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
			"version": int64(1), "info": map[string]any{"status": "deployed"},
			"chart": map[string]any{"metadata": map[string]any{"annotations": map[string]any{annotationRequestID: testRequestID}}},
		}}))
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Contains(t, res.Text, "app epinio: installed (")
		assert.Contains(t, res.Text, "status deployed)")
	})
}

// TestExtensionInstallWaitFailedRun asserts that --wait reports an error, and
// not a finished install, when Helm's run ended with the App in the status
// failed.
func TestExtensionInstallWaitFailedRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newExtFake(t)
		in := extInput(extChart)
		in.b[flagWait] = true
		_, exec := plan(t, newTestExtensionInstall(f), in)
		f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
			"version": int64(1), "info": map[string]any{"status": "failed"},
			"chart": map[string]any{"metadata": map[string]any{"annotations": map[string]any{annotationRequestID: testRequestID}}},
		}}))
		res, err := exec(t.Context())
		require.Error(t, err)
		require.ErrorContains(t, err, "release 'epinio' failed (status failed)")
		assert.NotContains(t, res.Text, "installed (")
	})
}

// TestExtensionInstallWaitWithoutOperationRead asserts that --wait for a
// caller who may not get the Operation, when the ClusterRepo has a service
// account, warns in the preview and goes by the App alone: it finishes with
// the App's status and reads no Operation.
func TestExtensionInstallWaitWithoutOperationRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newExtFake(t)
		f.allowed = false
		f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
			`{"metadata":{"name":"` + extRepo + `","state":{"name":"active"}},"spec":{"gitRepo":"` + extPartnerAddress + `","serviceAccount":"helm","serviceAccountNamespace":"` + extUIPlugin + `"}}`)}
		in := extInput(extChart)
		in.b[flagWait] = true
		eff, exec := plan(t, newTestExtensionInstall(f), in)
		assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool {
			return strings.HasPrefix(w, "you cannot get operations in namespace '"+extUIPlugin+"'; the wait reads the app only")
		}), "%v", eff.Warnings)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
			"version": int64(1), "info": map[string]any{"status": "deployed"},
			"chart": map[string]any{"metadata": map[string]any{"annotations": map[string]any{annotationRequestID: testRequestID}}},
		}}))
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Contains(t, res.Text, "status deployed)")
		assert.NotContains(t, f.reads, "get:operations/"+extUIPlugin+"/helm-operation-abcde")
	})
}

// TestExtensionInstallPlanDisplayIdentity asserts the Effect names the app and
// the chart's name, with no "*" marker in the identity that is committed.
func TestExtensionInstallPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := newExtFake(t)
	eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart))
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.NotContains(t, eff.DisplayName, "*")
	assert.Equal(t, "apps/"+extChart, eff.Target)
}

// TestExtensionInstallChecksRunFirst asserts the shared checks end the command
// before any other read: the feature turned off, then the namespace missing.
func TestExtensionInstallChecksRunFirst(t *testing.T) {
	t.Parallel()
	t.Run("feature off", func(t *testing.T) {
		t.Parallel()
		f := newExtFake(t)
		f.put("features", "", "uiextension", map[string]any{"spec": map[string]any{"value": false}})
		_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
		require.ErrorContains(t, err, "UI extensions are turned off on this server (feature uiextension)")
		assert.Equal(t, clierr.CodeValidation, errCode(err))
		assert.Equal(t, []string{"get:features//uiextension"}, f.reads)
		assert.Empty(t, f.root.requests())
	})
	t.Run("namespace missing", func(t *testing.T) {
		t.Parallel()
		f := newExtFake(t)
		f.remove("namespaces", "", extUIPlugin)
		_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
		require.ErrorContains(t, err, "namespace cattle-ui-plugin-system not found")
		assert.Equal(t, clierr.CodeNotFound, errCode(err))
		assert.Equal(t, []string{"get:features//uiextension", "get:namespaces//cattle-ui-plugin-system"}, f.reads)
		assert.Empty(t, f.root.requests())
	})
}

// TestExtensionInstallVersionWarnings asserts the preview warns when the Kubernetes
// version of the local cluster could not be read while a version asks for one,
// and when the Extensions API version of the server is not known, for the
// default version and for a typed --version.
func TestExtensionInstallVersionWarnings(t *testing.T) {
	t.Parallel()
	entry := func(version, extra string) string {
		return `{"name":"epinio","version":"` + version + `","digest":"sha256:` + version + `","annotations":{` + extCompatible + extra + `}}`
	}
	kubeIndex := `{"entries":{"epinio":[` + entry("1.14.4", `,"catalog.cattle.io/kube-version":">= 1.20.0"`) + `,` + entry("1.14.3", "") + `,` + entry("1.14.2", "") + `]}}`
	unread := func(f *extFake) {
		f.root.answers[localCluster] = rootAnswer{status: 403}
		f.root.answers[extIndexPath] = rootAnswer{status: 200, body: kubeIndex}
	}
	unknown := func(f *extFake) { f.version = "v2.99.0" }
	cases := []struct {
		name    string
		setup   func(*extFake)
		flags   []string
		warning string
	}{
		{name: "Kubernetes version unread", setup: unread, warning: "the Kubernetes version of the local cluster could not be read"},
		{name: "Extensions API version unknown", setup: unknown, warning: "the Extensions API version of Rancher v2.99.0 is not known"},
		{name: "Extensions API version unknown for a typed version", setup: unknown, flags: []string{flagVersion, "1.14.3"}, warning: "the Extensions API version of Rancher v2.99.0 is not known"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			tc.setup(f)
			eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart, tc.flags...))
			assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool { return strings.Contains(w, tc.warning) }), "%q in %q", tc.warning, eff.Warnings)
		})
	}
}

// TestExtensionInstallRBAC asserts a refused read or action is the server's
// RBAC refusal, and a refused read of the local Cluster is only the warning.
func TestExtensionInstallRBAC(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		path string
		in   appInput
	}{
		"repositories": {reposPath, extInput(extChart)},
		"index":        {extIndexPath, extInput(extChart, flagRepo, extRepo)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.root.answers[tc.path] = rootAnswer{status: 403}
			_, _, err := newTestExtensionInstall(f).Plan(t.Context(), tc.in)
			assert.Equal(t, clierr.CodeRBAC, errCode(err))
		})
	}
	for name, key := range map[string]string{"the UIPlugins": "list:uiplugins/", "the App": "get:apps/" + extUIPlugin + "/" + extChart} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			f.fail(key, clierr.FromResponse("GET", "GET", 403, []byte(`{}`), nil))
			_, _, err := newTestExtensionInstall(f).Plan(t.Context(), extInput(extChart))
			assert.Equal(t, clierr.CodeRBAC, errCode(err))
		})
	}
	t.Run("the action", func(t *testing.T) {
		t.Parallel()
		f := newExtFake(t)
		f.postAnswer = func(string) ([]byte, error) {
			return nil, &engine.ActionRefused{Err: clierr.FromResponse("POST", "POST", 403, []byte(`{}`), nil), Body: []byte(`{}`)}
		}
		_, exec := plan(t, newTestExtensionInstall(f), extInput(extChart))
		_, err := exec(t.Context())
		assert.Equal(t, clierr.CodeRBAC, errCode(err))
	})
	t.Run("the local Cluster", func(t *testing.T) {
		t.Parallel()
		f := newExtFake(t)
		f.root.answers[localCluster] = rootAnswer{status: 403}
		eff, _ := plan(t, newTestExtensionInstall(f), extInput(extChart))
		assert.Equal(t, "1.14.3", diffPaths(eff)["version"])
	})
}

var _ runtime.Command = (*extensionInstallCmd)(nil)

// extensionInfoPlan plans the extension command ("install" or "upgrade") on a
// fake whose registry setting is set and whose repository returns the info of
// both versions of the chart, and returns the fake, the versions its index
// lists and the error of the Plan.
func extensionInfoPlan(t *testing.T, command string, edit func(*appInput)) (*extFake, []string, error) {
	t.Helper()
	f := newExtFake(t)
	f.root.answers[settingPath(settingSystemDefaultRegistry)] = rootAnswer{status: 200, body: `{"value":"registry.example.test","default":""}`}
	for _, v := range []string{"1.14.3", "1.14.2"} {
		f.root.answers[extRepoPath+"?link=info&chartName=epinio&version="+v] = rootAnswer{status: 200, body: `{"values":{},"chart":{"version":"` + v + `"}}`}
	}
	var idx Index
	require.NoError(t, json.Unmarshal([]byte(f.root.answers[extIndexPath].body), &idx))
	var listed []string
	for _, v := range idx.Entries[extChart] {
		listed = append(listed, v.Version)
	}
	in := extInput(extChart)
	if edit != nil {
		edit(&in)
	}
	var err error
	if command == "upgrade" {
		putExtApp(f, "deployed", extInstalledVersion, extRepo)
		_, _, err = newTestExtensionUpgrade(f).Plan(t.Context(), in)
	} else {
		_, _, err = newTestExtensionInstall(f).Plan(t.Context(), in)
	}
	return f, listed, err
}
