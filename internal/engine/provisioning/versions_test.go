package provisioning

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// releaseHandler builds an httptest handler serving a fixed release
// envelope ({"data":[{"version": "..."}, ...]}) at exactly the given path,
// 404ing everything else - the channel-server route contract FetchVersions
// consumes (RootClientFactory.Root().GetJSON).
func releaseHandler(t *testing.T, routes map[string][]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		versions, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]any, 0, len(versions))
		for _, v := range versions {
			data = append(data, map[string]any{"version": v})
		}
		writeJSONFixture(t, w, map[string]any{"data": data})
	}
}

// settingObj builds a fixture management.cattle.io/v3 Setting object with
// only the top-level "value" field set (setting.go convention).
func settingObj(name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"value":    value,
	}}
}

// settingsClient builds a fakeClient (defined in cloudcred_test.go, same
// package) whose objects map is keyed by Setting name - versions.go reads
// Settings by name via ClusterClient.Get, exactly what fakeClient.Get
// serves.
func settingsClient(objs map[string]*unstructured.Unstructured) *fakeClient {
	return &fakeClient{objects: objs}
}

func TestNormalizeVersion(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"v1.34.1+rke2r1": "1.34.1",
		"v1.33.4+k3s1":   "1.33.4",
		"1.30.0":         "1.30.0",
		"v1.30.0":        "1.30.0",
	}
	for in, want := range cases {
		assert.Equal(t, want, normalizeVersion(in), "normalizeVersion(%q)", in)
	}
}

func TestFetchVersionsMergesBothDistrosWithDefaults(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1", "v1.30.6+rke2r1"},
		k3sReleasePath:  {"v1.30.5+k3s1", "v1.30.6+k3s1"},
	}))
	defer srv.Close()

	client := settingsClient(map[string]*unstructured.Unstructured{
		rke2DefaultVersionSetting: settingObj(rke2DefaultVersionSetting, "v1.30.6+rke2r1"),
		k3sDefaultVersionSetting:  settingObj(k3sDefaultVersionSetting, "v1.30.5+k3s1"),
	})
	factory := fakeFactory{c: client}

	rows, err := FetchVersions(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 4)

	byDistroVersion := map[string]Version{}
	for _, r := range rows {
		byDistroVersion[r.Distro+"/"+r.Version] = r
	}

	assert.False(t, byDistroVersion["rke2/v1.30.5+rke2r1"].Default)
	assert.True(t, byDistroVersion["rke2/v1.30.6+rke2r1"].Default, "rke2 default setting names v1.30.6+rke2r1")
	assert.True(t, byDistroVersion["k3s/v1.30.5+k3s1"].Default, "k3s default setting names v1.30.5+k3s1")
	assert.False(t, byDistroVersion["k3s/v1.30.6+k3s1"].Default)
}

func TestFetchVersionsSupportedEvaluation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		rangeValue  string
		rawVersion  string
		wantSupport *bool
	}{
		{"in-range", ">= 1.11.0 <=1.14.x", "v1.12.3+rke2r1", new(true)},
		{"below", ">= 1.11.0 <=1.14.x", "v1.10.9+rke2r1", new(false)},
		{"above", ">= 1.11.0 <=1.14.x", "v1.15.0+rke2r1", new(false)},
		{"wildcard-both-bounds-inside", ">=1.11.x <=1.14.x", "v1.14.99+rke2r1", new(true)},
		{"wildcard-both-bounds-outside", ">=1.11.x <=1.14.x", "v1.15.0+rke2r1", new(false)},
		{"empty-setting", "", "v1.12.3+rke2r1", nil},
		{"garbage-setting", "not a range at all", "v1.12.3+rke2r1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(releaseHandler(t, map[string][]string{
				rke2ReleasePath: {tc.rawVersion},
				k3sReleasePath:  {},
			}))
			defer srv.Close()

			client := settingsClient(map[string]*unstructured.Unstructured{
				supportedRangeSetting: settingObj(supportedRangeSetting, tc.rangeValue),
			})
			factory := fakeFactory{c: client}

			rows, err := FetchVersions(t.Context(), rootsFor(srv), factory)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			if tc.wantSupport == nil {
				assert.Nil(t, rows[0].Supported)
			} else {
				require.NotNil(t, rows[0].Supported)
				assert.Equal(t, *tc.wantSupport, *rows[0].Supported)
			}
		})
	}
}

// TestFetchVersionsDegradesOnOlderServer proves a 404 on one distro's
// channel-server route (an older Rancher server) degrades to the OTHER
// distro's rows plus a warning - never an error.
func TestFetchVersionsDegradesOnOlderServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		k3sReleasePath: {"v1.30.5+k3s1"},
		// rke2ReleasePath deliberately absent -> 404
	}))
	defer srv.Close()

	client := settingsClient(nil)
	factory := fakeFactory{c: client}

	rows, err := FetchVersions(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err, "a 404 on one distro's route must never surface as an error")
	require.Len(t, rows, 1)
	assert.Equal(t, "k3s", rows[0].Distro)

	// The command-level Execute must surface the degraded fetch as a warning.
	cmd := NewGetKubernetesVersions(rootsFor(srv), factory)
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok)
	res, err := rc.Execute(t.Context(), noArgsInput{}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings, "the omitted distro must be surfaced as a warning, not silently dropped")
}

// TestFetchVersionsDegradesEachDistroIndependently proves the table-driven
// fetch (each distro's release call now runs concurrently with the other)
// still degrades each distro on its own: a 404 on k3s alone,
// on rke2 alone, and on both at once all yield the OTHER distro's rows (or
// none, for "both") plus one warning per omitted distro - never an error,
// and never a false warning for the distro that DID resolve.
func TestFetchVersionsDegradesEachDistroIndependently(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		routes      map[string][]string
		wantDistros []string
		wantWarns   int
	}{
		{
			name:        "k3s missing",
			routes:      map[string][]string{rke2ReleasePath: {"v1.30.5+rke2r1"}},
			wantDistros: []string{"rke2"},
			// k3s 404-degrade warn + rke2 missing-default warn (no settings in this fixture).
			wantWarns: 2,
		},
		{
			name:        "rke2 missing",
			routes:      map[string][]string{k3sReleasePath: {"v1.30.5+k3s1"}},
			wantDistros: []string{"k3s"},
			// rke2 404-degrade warn + k3s missing-default warn (no settings in this fixture).
			wantWarns: 2,
		},
		{
			name:        "both missing",
			routes:      map[string][]string{},
			wantDistros: nil,
			wantWarns:   2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(releaseHandler(t, tc.routes))
			defer srv.Close()

			factory := fakeFactory{c: settingsClient(nil)}
			rows, _, warnings, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
			require.NoError(t, err, "a 404 on either (or both) distro's route must never surface as an error")

			var gotDistros []string
			for _, r := range rows {
				gotDistros = append(gotDistros, r.Distro)
			}
			assert.Equal(t, tc.wantDistros, gotDistros)
			assert.Len(t, warnings, tc.wantWarns)
		})
	}
}

// TestFetchVersionsMalformedReleaseBody asserts the transport case:
// a body that is not valid JSON must
// decode to a typed CodeGeneric error naming "decoding" - never a panic,
// never an empty rows slice standing in for the failure silently.
func TestFetchVersionsMalformedReleaseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == rke2ReleasePath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not json{"))
			return
		}
		writeJSONFixture(t, w, map[string]any{"data": []map[string]any{}})
	}))
	defer srv.Close()

	client := settingsClient(nil)
	factory := fakeFactory{c: client}

	assert.NotPanics(t, func() {
		rows, err := FetchVersions(t.Context(), rootsFor(srv), factory)
		require.Error(t, err)
		ce, ok := err.(*clierr.Error)
		require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Contains(t, ce.Message, "decoding")
		assert.Nil(t, rows)
	})
}

// TestFetchVersionsDetailedBothDistrosHardFailSurfacesRKE2ErrorDeterministically
// asserts that when BOTH distros' release fetch hard-fails at
// close to the same time (not a 404-degrade - a genuine 500), the returned
// error must be rke2's (distroSpecs' fixed first entry), deterministically,
// never whichever goroutine happened to race to completion first, and never
// wrapped in "context canceled" by a sibling's cancellation (the bug an
// errgroup.WithContext-based fan-out introduced). The k3s failure must still
// be visible somewhere rather than silently dropped.
func TestFetchVersionsDetailedBothDistrosHardFailSurfacesRKE2ErrorDeterministically(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := settingsClient(nil)
	factory := fakeFactory{c: client}

	_, _, _, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "context canceled", "a sibling's hard failure must never cancel this distro's in-flight request")

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "the rke2 error must still be reachable through any wrapping")
	assert.Contains(t, ce.Message, rke2ReleasePath, "the surfaced error must be rke2's - the deterministic winner, never whichever raced first")
	assert.NotContains(t, ce.Message, k3sReleasePath, "the unwrapped error must be rke2's alone, not a merged/generic one")

	assert.ErrorContains(t, err, k3sReleasePath, "the k3s failure must be visible somewhere, not silently dropped")
}

// TestFetchVersionsDetailedClientResolutionErrorPrecedesReleaseFetchFailure
// asserts that clients.Cluster() resolves BEFORE the
// release-fetch fan-out starts, so when it fails at the same time a release
// fetch would also have failed, the caller must see the client-resolution
// error - the more fundamental root cause - never a release-fetch error.
// This is deliberate, accepted-as-is behavior (not something to "fix"); this
// test exists only so a future change cannot silently flip the precedence
// without a test failing.
func TestFetchVersionsDetailedClientResolutionErrorPrecedesReleaseFetchFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both release routes would also hard-fail, if the fan-out ever
		// reached them.
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	clusterErr := clierr.New(clierr.CodeGeneric, "cluster resolution failed")
	factory := fakeFactory{clusterErr: clusterErr}

	_, _, _, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.ErrorIs(t, err, clusterErr, "a clients.Cluster() failure must precede a simultaneous release-fetch failure")
}

// TestNewGetKubernetesVersionsDescribe asserts the Descriptor shape: a
// bespoke read-only entry under the "get" verb group, network+auth true.
func TestNewGetKubernetesVersionsDescribe(t *testing.T) {
	t.Parallel()
	d := NewGetKubernetesVersions(fakeRootFactory{}, fakeFactory{}).Describe()
	assert.Equal(t, "get kubernetes-versions", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
	assert.True(t, d.Network)
	assert.True(t, d.Auth)
}

// TestNewGetKubernetesVersionsExecuteTabularRendersSupportedColumn proves
// the table renderer emits "yes"/"no"/"" for Supported's true/false/nil.
func TestNewGetKubernetesVersionsExecuteTabularRendersSupportedColumn(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.12.0+rke2r1", "v1.20.0+rke2r1"},
		k3sReleasePath:  {},
	}))
	defer srv.Close()

	client := settingsClient(map[string]*unstructured.Unstructured{
		rke2DefaultVersionSetting: settingObj(rke2DefaultVersionSetting, "v1.12.0+rke2r1"),
		supportedRangeSetting:     settingObj(supportedRangeSetting, ">=1.11.0 <=1.14.x"),
	})
	factory := fakeFactory{c: client}

	cmd := NewGetKubernetesVersions(rootsFor(srv), factory)
	rc := cmd.(runtime.ReadCommand)
	res, err := rc.Execute(t.Context(), noArgsInput{}, nil)
	require.NoError(t, err)

	tab, ok := res.Data.(interface {
		Columns() []string
		Rows() [][]string
	})
	require.True(t, ok, "Result.Data must implement output.Tabular")
	assert.Equal(t, []string{"DISTRO", "VERSION", "DEFAULT", "NEWEST", "SUPPORTED"}, tab.Columns())

	rowsByVersion := map[string][]string{}
	for _, row := range tab.Rows() {
		rowsByVersion[row[1]] = row
	}
	// v1.12.0 is the default and v1.20.0 is the newest (highest semver).
	assert.Equal(t, []string{"rke2", "v1.12.0+rke2r1", "yes", "no", "yes"}, rowsByVersion["v1.12.0+rke2r1"])
	assert.Equal(t, []string{"rke2", "v1.20.0+rke2r1", "no", "yes", "no"}, rowsByVersion["v1.20.0+rke2r1"])

	// The JSON envelope must also carry the raw range for transparency.
	// Encoded with SetEscapeHTML(false), matching render.go's own convention
	// (internal/output/render.go) - the production path never HTML-escapes
	// "<"/">" in a version-range string.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(res.Data))
	assert.Contains(t, buf.String(), "supportedRange")
	assert.Contains(t, buf.String(), ">=1.11.0 <=1.14.x")
}

// TestFetchVersionsNewestMarkedWhenSettingsPresent verifies that the highest
// semver-ordered version per distro has Newest=true, that Default is marked
// honestly from the setting, and that no "no default" warning is emitted when
// both distro settings resolve to a non-empty value.
func TestFetchVersionsNewestMarkedWhenSettingsPresent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1", "v1.30.6+rke2r1"},
		k3sReleasePath:  {"v1.30.5+k3s1", "v1.30.6+k3s1"},
	}))
	defer srv.Close()

	client := settingsClient(map[string]*unstructured.Unstructured{
		rke2DefaultVersionSetting: settingObj(rke2DefaultVersionSetting, "v1.30.6+rke2r1"),
		k3sDefaultVersionSetting:  settingObj(k3sDefaultVersionSetting, "v1.30.5+k3s1"),
	})
	factory := fakeFactory{c: client}

	rows, _, warnings, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 4)

	byKey := map[string]Version{}
	for _, r := range rows {
		byKey[r.Distro+"/"+r.Version] = r
	}

	assert.True(t, byKey["rke2/v1.30.6+rke2r1"].Default, "rke2 default setting names v1.30.6+rke2r1")
	assert.True(t, byKey["rke2/v1.30.6+rke2r1"].Newest, "rke2 newest must be v1.30.6")
	assert.False(t, byKey["rke2/v1.30.5+rke2r1"].Default)
	assert.False(t, byKey["rke2/v1.30.5+rke2r1"].Newest)
	assert.True(t, byKey["k3s/v1.30.5+k3s1"].Default, "k3s default setting names v1.30.5+k3s1")
	assert.False(t, byKey["k3s/v1.30.5+k3s1"].Newest)
	assert.True(t, byKey["k3s/v1.30.6+k3s1"].Newest, "k3s newest must be v1.30.6")
	assert.False(t, byKey["k3s/v1.30.6+k3s1"].Default)

	for _, w := range warnings {
		assert.NotContains(t, w, "no default", "no missing-default warning expected when settings are present")
	}
}

// TestFetchVersionsNewestMarkedWhenSettingsAbsent verifies that when the
// default-version settings are absent (getSettingValue degrades to ""),
// Default is false everywhere, a per-distro warning is emitted, and Newest
// is still correctly marked.
func TestFetchVersionsNewestMarkedWhenSettingsAbsent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1", "v1.30.6+rke2r1"},
		k3sReleasePath:  {"v1.30.5+k3s1", "v1.30.6+k3s1"},
	}))
	defer srv.Close()

	factory := fakeFactory{c: settingsClient(nil)}

	rows, _, warnings, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 4)

	for _, r := range rows {
		assert.False(t, r.Default, "Default must be false for all rows when no setting is present")
	}

	byKey := map[string]Version{}
	for _, r := range rows {
		byKey[r.Distro+"/"+r.Version] = r
	}
	assert.True(t, byKey["rke2/v1.30.6+rke2r1"].Newest, "rke2 newest still marked when settings absent")
	assert.True(t, byKey["k3s/v1.30.6+k3s1"].Newest, "k3s newest still marked when settings absent")

	var foundRKE2Warn, foundK3SWarn bool
	for _, w := range warnings {
		if strings.Contains(w, rke2DefaultVersionSetting) {
			foundRKE2Warn = true
		}
		if strings.Contains(w, k3sDefaultVersionSetting) {
			foundK3SWarn = true
		}
	}
	assert.True(t, foundRKE2Warn, "expected warning about missing %s setting", rke2DefaultVersionSetting)
	assert.True(t, foundK3SWarn, "expected warning about missing %s setting", k3sDefaultVersionSetting)
}

// TestFetchVersionsNewestPicksSemverMaxNotLexicographic verifies that Newest
// is assigned by true semver ordering, not lexicographic string comparison.
// v1.9.x < v1.10.x semantically, but "v1.9" > "v1.10" lexicographically
// because the digit "9" is greater than "1" at the minor-version position.
func TestFetchVersionsNewestPicksSemverMaxNotLexicographic(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.9.2+rke2r1", "v1.10.1+rke2r1"},
		// Empty k3s route serves a clean empty list; with no default-version
		// settings in this fixture, missing-default warnings are generated
		// and deliberately ignored - this test asserts Newest only.
		k3sReleasePath: {},
	}))
	defer srv.Close()

	factory := fakeFactory{c: settingsClient(nil)}

	rows, _, _, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byVersion := map[string]Version{}
	for _, r := range rows {
		byVersion[r.Version] = r
	}
	assert.False(t, byVersion["v1.9.2+rke2r1"].Newest, "v1.9.2 is not the semver max")
	assert.True(t, byVersion["v1.10.1+rke2r1"].Newest, "v1.10.1 is the semver max even though '9' > '1' lexicographically")
}

// TestFetchVersionsNewestReleaseNumberTieBreaker verifies that when the semver
// core is equal across two versions, the higher rN release number wins.
func TestFetchVersionsNewestReleaseNumberTieBreaker(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1", "v1.30.5+rke2r2"},
		// See the fixture note in the semver-max test above: warnings are
		// deliberately ignored, only Newest is under test.
		k3sReleasePath: {},
	}))
	defer srv.Close()

	factory := fakeFactory{c: settingsClient(nil)}

	rows, _, _, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byVersion := map[string]Version{}
	for _, r := range rows {
		byVersion[r.Version] = r
	}
	assert.False(t, byVersion["v1.30.5+rke2r1"].Newest, "r1 is not the newest when r2 is present")
	assert.True(t, byVersion["v1.30.5+rke2r2"].Newest, "r2 is the newest release for the same semver core")
}

// TestFetchVersionsNewestNotMarkedWhenAllVersionsUnparseable verifies that
// when every version string in a distro fails to parse (e.g. garbage data
// from the channel server), no row is marked Newest - the guard must never
// fabricate a Newest entry for an unparseable version string.
func TestFetchVersionsNewestNotMarkedWhenAllVersionsUnparseable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"not-a-version", "also-not"},
		k3sReleasePath:  {},
	}))
	defer srv.Close()

	factory := fakeFactory{c: settingsClient(nil)}

	rows, _, _, err := fetchVersionsDetailed(t.Context(), rootsFor(srv), factory)
	require.NoError(t, err)
	require.Len(t, rows, 2, "rows still returned even when versions are unparseable")

	for _, r := range rows {
		assert.False(t, r.Newest, "no row must be marked Newest when all version strings are unparseable")
	}
}

// noArgsInput is a minimal runtime.Input fake for commands that read no
// args/flags at all.
type noArgsInput struct{}

func (noArgsInput) String(string) string    { return "" }
func (noArgsInput) Bool(string) bool        { return false }
func (noArgsInput) Strings(string) []string { return nil }
func (noArgsInput) IsSet(string) bool       { return false }
func (noArgsInput) Args() []string          { return nil }

var _ runtime.Input = noArgsInput{}
