package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// stubDiscovery serves only ServerGroups (the sole method resolveVersion needs)
// and counts how many times it is consulted, so the memoization can be
// asserted. The embedded interface panics on any other call.
type stubDiscovery struct {
	discovery.DiscoveryInterface
	groups map[string]string // group name → preferred version
	calls  int
}

func (s *stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	s.calls++
	gl := &metav1.APIGroupList{}
	for name, ver := range s.groups {
		gl.Groups = append(gl.Groups, metav1.APIGroup{
			Name:             name,
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: name + "/" + ver, Version: ver},
		})
	}
	return gl, nil
}

func TestEmptyVersionResolvedViaDiscovery(t *testing.T) {
	t.Parallel()
	machinesV1 := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machines"}
	clustersV1 := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"}
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{machinesV1: "MachineList", clustersV1: "ClusterList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	var lastVersion string
	fc.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		lastVersion = a.GetResource().Version
		return false, nil, nil // fall through to the tracker
	})
	disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": "v1beta1"}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	_, err := c.List(t.Context(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"}, "fleet-default", "")
	require.NoError(t, err)
	assert.Equal(t, "v1beta1", lastVersion, "the empty version is filled from discovery before the API call")

	// Second call, same group: discovery is memoized, not consulted again.
	_, err = c.List(t.Context(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "clusters"}, "fleet-default", "")
	require.NoError(t, err)
	assert.Equal(t, 1, disc.calls)
}

// TestEmptyPreferredVersionIsNotCached guards the cache-poisoning fix: a group
// whose preferred version is the empty string must error with an actionable
// hint, and the empty value must NOT be memoized — a later call re-consults
// discovery so a group that starts serving a version can recover without
// recreating the client.
func TestEmptyPreferredVersionIsNotCached(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{})
	disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": ""}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	machines := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"}
	_, err := c.List(t.Context(), machines, "fleet-default", "")
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "rancher schema", "message must mention rancher schema for requirements")
	assert.Equal(t, 1, disc.calls)

	// Second call: the empty version was not cached, so discovery is consulted again.
	_, err = c.List(t.Context(), machines, "fleet-default", "")
	require.Error(t, err)
	assert.Equal(t, 2, disc.calls, "an empty preferred version must not poison the memo")
}

// TestAbsentExtGroupYieldsFriendlyHint asserts EXT-R-4: an older Rancher without
// the aggregated ext.cattle.io APIService must surface a hint pointing at the
// pre-existing local alternatives, not the generic "not served" message other
// groups get. Non-ext absent groups carry their own actionable hint.
func TestAbsentExtGroupYieldsFriendlyHint(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{})
	disc := &stubDiscovery{groups: map[string]string{}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	tokens := schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "tokens"}
	_, err := c.List(t.Context(), tokens, "", "")
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	require.ErrorContains(t, ce, "ext.cattle.io")
	assert.Contains(t, ce.Message, "auth list-tokens")

	// A non-ext group absence carries the generic "not served" hint (not the ext hint).
	_, err = c.List(t.Context(), schema.GroupVersionResource{Group: "other.example.io", Version: "", Resource: "widgets"}, "", "")
	require.Error(t, err)
	require.ErrorAs(t, err, &ce)
	assert.NotContains(t, ce.Message, "auth list-tokens", "non-ext group must not carry ext hint")
	assert.Contains(t, ce.Message, "rancher schema", "non-ext not-served message must mention rancher schema")
}

// TestResolveVersionErrorReturnsEarlyFromGet asserts the REAL mechanism: a
// resolveVersion failure returns from Get unchanged before curateNotFound is
// ever reached. The message and hint from resolveVersion are preserved because
// the early return bypasses curation entirely, not because curateNotFound
// passes them through.
func TestResolveVersionErrorReturnsEarlyFromGet(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{})

	t.Run("no-preferred-version error returns early from Get with hint intact", func(t *testing.T) {
		t.Parallel()
		disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": ""}}
		c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}
		_, err := c.Get(t.Context(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"}, "", "m-1")
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		// resolveVersion's hint is preserved via early return, not curation.
		assert.Contains(t, ce.Message, "rancher schema", "resolveVersion message must survive early return unchanged")
		// The message must not be the curated format — curateNotFound never ran.
		assert.NotContains(t, ce.Message, "not found: m-1", "resolveVersion diagnostic must not be overwritten by curation")
	})

	t.Run("not-served error returns early from Get with hint intact", func(t *testing.T) {
		t.Parallel()
		disc := &stubDiscovery{groups: map[string]string{}}
		c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}
		_, err := c.Get(t.Context(), schema.GroupVersionResource{Group: "other.example.io", Version: "", Resource: "widgets"}, "", "w-1")
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		// resolveVersion's hint is preserved via early return, not curation.
		assert.Contains(t, ce.Message, "rancher schema", "resolveVersion message must survive early return unchanged")
		assert.NotContains(t, ce.Message, "not found: w-1", "resolveVersion diagnostic must not be overwritten by curation")
	})
}

// TestCurateNotFoundHintBearingPassesThrough directly tests curateNotFound's
// has-hint guard: a hinted NOT_FOUND for a known GVR must pass through
// unchanged. This guard protects any hinted NOT_FOUND that reaches the curation
// seam (resolveVersion diagnostics never reach it, protected by their early
// return; this covers other hinted NOT_FOUNDs that may arise in the future).
func TestCurateNotFoundHintBearingPassesThrough(t *testing.T) {
	t.Parallel()
	hinted := clierr.New(clierr.CodeNotFound, "some diagnostic with hint").
		WithHint("run: rancher schema project")
	// projectGVR is in the kinds table, so a hint-less NOT_FOUND would be
	// curated. The hint guard must prevent that rewrite.
	r := &dynamicClient{}
	got := r.curateNotFound(projectGVR, "p-missing", hinted)
	var ce *clierr.Error
	require.ErrorAs(t, got, &ce)
	assert.Equal(t, hinted.Message, ce.Message, "hinted NOT_FOUND message must not be overwritten by curation")
	assert.Equal(t, hinted.Hint, ce.Hint, "hinted NOT_FOUND hint must not be overwritten by curation")
}

var projectGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

// newFakeClient builds a dynamicClient over the fake dynamic client. Discovery
// is stubbed (empty groups) and the version memo initialized, so exercising an
// empty-version GVR resolves through the stub instead of panicking on a nil
// discovery client.
func newFakeClient(objs ...runtime.Object) (*dynamicClient, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	// Register the GVR->ListKind so the fake tracker can serve List.
	gvrToListKind := map[schema.GroupVersionResource]string{projectGVR: "ProjectList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
	return &dynamicClient{dyn: fc, disc: &stubDiscovery{groups: map[string]string{}}, versions: map[string]string{}}, fc
}

func project(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"clusterName": ns, "displayName": name},
	}}
}

func TestDynamicRepoCreateAndGet(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	out, err := r.Create(t.Context(), projectGVR, "c-x", project("c-x", "p-1"), false)
	require.NoError(t, err)
	assert.Equal(t, "p-1", out.GetName())

	got, err := r.Get(t.Context(), projectGVR, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", got.GetName())
}

func TestDynamicRepoCreatePassesDryRun(t *testing.T) {
	t.Parallel()
	r, fc := newFakeClient()
	var sawDryRun []string
	fc.PrependReactor("create", "projects", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sawDryRun = a.(k8stesting.CreateActionImpl).GetCreateOptions().DryRun
		return false, nil, nil
	})
	_, err := r.Create(t.Context(), projectGVR, "c-x", project("c-x", "p-2"), true)
	require.NoError(t, err)
	assert.Equal(t, []string{"All"}, sawDryRun)
}

func TestDynamicRepoListAndDelete(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient(project("c-x", "p-1"), project("c-x", "p-2"))
	list, err := r.List(t.Context(), projectGVR, "c-x", "")
	require.NoError(t, err)
	assert.Len(t, list.Items, 2)

	require.NoError(t, r.Delete(t.Context(), projectGVR, "c-x", "p-1", "", false))
	after, err := r.List(t.Context(), projectGVR, "c-x", "")
	require.NoError(t, err)
	assert.Len(t, after.Items, 1)
}

// TestListPassesLabelSelector asserts the selector param reaches the fake
// dynamic client's ListOptions.LabelSelector unchanged, and that "" list-all
// sends no selector.
func TestListPassesLabelSelector(t *testing.T) {
	t.Parallel()
	r, fc := newFakeClient(project("c-x", "p-1"), project("c-x", "p-2"))
	var lastSelector string
	sawSelector := false
	fc.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		la := a.(k8stesting.ListActionImpl)
		lastSelector = la.ListOptions.LabelSelector
		sawSelector = true
		return false, nil, nil
	})

	_, err := r.List(t.Context(), projectGVR, "c-x", "app=web")
	require.NoError(t, err)
	assert.True(t, sawSelector)
	assert.Equal(t, "app=web", lastSelector)

	_, err = r.List(t.Context(), projectGVR, "c-x", "")
	require.NoError(t, err)
	assert.Empty(t, lastSelector)
}

// TestLogsFetchesTail asserts Logs returns the fake clientset's canned log
// body and maps LogOptions onto corev1.PodLogOptions 1:1, including the
// nil-vs-set SinceSeconds distinction (nil = no limit; *0 = "since now").
func TestLogsFetchesTail(t *testing.T) {
	t.Parallel()

	lastOpts := func(t *testing.T, cs *k8sfake.Clientset) *corev1.PodLogOptions {
		t.Helper()
		for _, a := range cs.Actions() {
			gen, ok := a.(k8stesting.GenericActionImpl)
			if !ok || a.GetSubresource() != "log" {
				continue
			}
			opts, ok := gen.Value.(*corev1.PodLogOptions)
			require.True(t, ok)
			return opts
		}
		t.Fatal("expected a get pods/log action")
		return nil
	}

	t.Run("maps container, tail, previous, timestamps", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns1"}}
		cs := k8sfake.NewClientset(pod)
		r := &dynamicClient{cs: cs}

		tail := int64(50)
		out, err := r.Logs(t.Context(), "ns1", "web", engine.LogOptions{
			Container: "app", TailLines: &tail, Previous: true, Timestamps: true,
		})
		require.NoError(t, err)
		assert.NotEmpty(t, out)

		opts := lastOpts(t, cs)
		assert.Equal(t, "app", opts.Container)
		require.NotNil(t, opts.TailLines)
		assert.Equal(t, int64(50), *opts.TailLines)
		assert.True(t, opts.Previous)
		assert.True(t, opts.Timestamps)
		assert.Nil(t, opts.SinceSeconds, "SinceSeconds omitted must stay nil (no limit), not become *0")
	})

	t.Run("SinceSeconds nil means no limit", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns1"}}
		cs := k8sfake.NewClientset(pod)
		r := &dynamicClient{cs: cs}

		_, err := r.Logs(t.Context(), "ns1", "web", engine.LogOptions{Container: "app"})
		require.NoError(t, err)
		assert.Nil(t, lastOpts(t, cs).SinceSeconds)
	})

	t.Run("TailLines nil means no limit, not a request for zero lines", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns1"}}
		cs := k8sfake.NewClientset(pod)
		r := &dynamicClient{cs: cs}

		_, err := r.Logs(t.Context(), "ns1", "web", engine.LogOptions{Container: "app"})
		require.NoError(t, err)
		assert.Nil(t, lastOpts(t, cs).TailLines)
	})

	t.Run("SinceSeconds set is passed through verbatim, including zero", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns1"}}
		cs := k8sfake.NewClientset(pod)
		r := &dynamicClient{cs: cs}
		zero := int64(0)

		_, err := r.Logs(t.Context(), "ns1", "web", engine.LogOptions{Container: "app", SinceSeconds: &zero})
		require.NoError(t, err)
		got := lastOpts(t, cs).SinceSeconds
		require.NotNil(t, got)
		assert.Equal(t, int64(0), *got)
	})
}

func TestDynamicClientBoundsRequestWithDeadline(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Hour))
	defer cancel()
	_, err := r.Get(ctx, projectGVR, "c-x", "p-1")
	require.Error(t, err, "an already-expired parent context must surface a deadline error")
}

func TestDynamicRepoGetNotFoundMapsToClierr(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	_, err := r.Get(t.Context(), projectGVR, "c-x", "nope")
	require.Error(t, err)
	// mapErr turns the k8s NotFound into a CodeNotFound clierr; curateNotFound
	// then adds the kind-aware message and list hint.
	assert.ErrorContains(t, err, "not found")
}

// TestClientGetCuratesKnownGVRNotFound asserts that Get curates a hint-less
// NOT_FOUND for a GVR that resolves in the kinds table (Fix 3).
func TestClientGetCuratesKnownGVRNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	_, err := r.Get(t.Context(), projectGVR, "c-x", "p-missing")
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-missing", ce.Message)
	assert.Equal(t, "list with: rancher get projects", ce.Hint)
}

// TestClientGetHintedNotFoundPassesThrough asserts that Get does not overwrite a
// resolveVersion diagnostic with the curated "<kind> not found" shape: it
// returns before curateNotFound is ever reached.
func TestClientGetHintedNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{})
	// empty preferred version → resolveVersion returns its own diagnostic
	disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": ""}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	_, err := c.Get(t.Context(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "clusters"}, "ns", "x")
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "no preferred version", "resolveVersion diagnostic must pass through curateNotFound unchanged")
	assert.NotContains(t, ce.Message, "capi-cluster not found", "resolveVersion diagnostic must not be overwritten by curation")
}

// TestClientGetUnknownGVRNotFoundPassesThrough asserts that Get does not curate a
// hint-less NOT_FOUND for a GVR absent from the kinds table (Fix 3: unknown GVR
// passes through untouched by curateNotFound).
func TestClientGetUnknownGVRNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	unknownGVR := schema.GroupVersionResource{Group: "custom.example.io", Version: "v1", Resource: "widgets"}
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{unknownGVR: "WidgetList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	c := &dynamicClient{
		dyn:      fc,
		disc:     &stubDiscovery{groups: map[string]string{"custom.example.io": "v1"}},
		versions: map[string]string{},
	}

	_, err := c.Get(t.Context(), unknownGVR, "", "w-missing")
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Hint, "unknown GVR must not be curated — no hint expected")
}

// TestClientDeleteCuratesKnownGVRNotFound asserts that Delete curates a hint-less
// NOT_FOUND for a GVR that resolves in the kinds table (Fix 3).
func TestClientDeleteCuratesKnownGVRNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	err := r.Delete(t.Context(), projectGVR, "c-x", "p-missing", "", false)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-missing", ce.Message)
	assert.Equal(t, "list with: rancher get projects", ce.Hint)
}

// TestClientUpdateCuratesKnownGVRNotFound asserts that Update curates a hint-less
// NOT_FOUND for a GVR that resolves in the kinds table (Fix 3).
func TestClientUpdateCuratesKnownGVRNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	obj := project("c-x", "p-missing")
	_, err := r.Update(t.Context(), projectGVR, "c-x", obj, false)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-missing", ce.Message)
	assert.Equal(t, "list with: rancher get projects", ce.Hint)
}

// TestClusterClientDeleteRefusesCrossOriginRedirect asserts that a mutation
// through the dynamic client is refused at the redirect and rendered as
// the structured refusal, with no request reaching the other origin.
func TestClusterClientDeleteRefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	srv, caPEM, hits := redirectingServer(t, http.StatusTemporaryRedirect)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: caPEM, Token: "id:secret"}, "local")
	require.NoError(t, err)
	cc, err := NewClusterClient(rc, "local")
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}
	err = cc.Delete(t.Context(), gvr, "", "p-x", "", false)
	ce2, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce2.Code)
	assert.False(t, ce2.Retryable)
	require.ErrorContains(t, err, "credentialed redirects are not followed across origins")
	assert.Zero(t, hits.Load())
}

// TestDynamicRepoCreateAndUpdateNameTheFieldManager asserts the manager name
// the server records in managedFields for every write this client makes:
// an explicit, stable identity instead of one derived from the User-Agent.
func TestDynamicRepoCreateAndUpdateNameTheFieldManager(t *testing.T) {
	t.Parallel()
	r, fc := newFakeClient(project("c-x", "p-1"))
	var createManager, updateManager string
	fc.PrependReactor("create", "projects", func(a k8stesting.Action) (bool, runtime.Object, error) {
		createManager = a.(k8stesting.CreateActionImpl).GetCreateOptions().FieldManager
		return false, nil, nil
	})
	fc.PrependReactor("update", "projects", func(a k8stesting.Action) (bool, runtime.Object, error) {
		updateManager = a.(k8stesting.UpdateActionImpl).GetUpdateOptions().FieldManager
		return false, nil, nil
	})
	_, err := r.Create(t.Context(), projectGVR, "c-x", project("c-x", "p-2"), false)
	require.NoError(t, err)
	_, err = r.Update(t.Context(), projectGVR, "c-x", project("c-x", "p-1"), true)
	require.NoError(t, err)
	assert.Equal(t, FieldManager, createManager)
	assert.Equal(t, FieldManager, updateManager)
	assert.Equal(t, "cli.rancher.io", FieldManager, "the identity is the CLI's domain, unversioned, so it survives a binary rename and a schema bump")
}
