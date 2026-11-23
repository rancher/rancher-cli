package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const kubernetesPinsPath = "test/conformance/testdata/rancher-kubernetes-pins.json"

var (
	kubernetesCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	releaseVersionPattern   = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	pseudoVersionPattern    = regexp.MustCompile(`^v((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))(-0\.|-)([0-9]{14})-([0-9a-f]{12})$`)
)

// kubernetesPins is the content of rancher-kubernetes-pins.json: for each
// module that go.mod must not have at a newer version than Rancher main, the
// version the go.mod of Rancher main has. Modules maps a module path to that
// version: the replacement version of a replace directive when Rancher main
// has one, else the version of its require directive. The modules in it are
// the covered modules: every k8s.io module and sigs.k8s.io/controller-runtime.
type kubernetesPins struct {
	Source  string            `json:"source"`
	Commit  string            `json:"commit"`
	Date    string            `json:"date"`
	Modules map[string]string `json:"modules"`
}

func loadKubernetesPins(path string) (kubernetesPins, error) {
	var pins kubernetesPins
	data, err := os.ReadFile(path)
	if err != nil {
		return pins, err
	}
	err = json.Unmarshal(data, &pins)
	return pins, err
}

// kubernetesCovered reports whether TestKubernetesModulesAreNotAheadOfRancher
// compares the module path: every k8s.io module and
// sigs.k8s.io/controller-runtime. The other sigs.k8s.io modules are not
// covered. A replace directive from an uncovered path to a covered module,
// such as example.com/fork => k8s.io/api v0.99.0, is not seen.
func kubernetesCovered(path string) bool {
	return strings.HasPrefix(path, "k8s.io/") ||
		path == "sigs.k8s.io/controller-runtime" ||
		strings.HasPrefix(path, "sigs.k8s.io/controller-runtime/")
}

// kubernetesVersionProblems reads the output of `go mod edit -json go.mod`
// and returns one message for each problem between go.mod and the pins file:
// a commit that is not 40 hexadecimal digits, a covered module with no entry,
// a covered module replaced by another module, a replace directive that names
// an old version, a version newer than the entry, versions this package
// cannot compare, and an entry go.mod has no directive for. An error means the
// input is not that JSON.
func kubernetesVersionProblems(modEdit []byte, pins kubernetesPins) ([]string, error) {
	var edit struct {
		Require []struct {
			Path    string
			Version string
		}
		Replace []struct {
			Old struct{ Path, Version string }
			New struct{ Path, Version string }
		}
	}
	if err := json.Unmarshal(modEdit, &edit); err != nil {
		return nil, err
	}

	var problems []string
	if !kubernetesCommitPattern.MatchString(pins.Commit) {
		problems = append(problems, fmt.Sprintf("the commit %q in %s is not 40 hexadecimal digits; set it to the commit of Rancher main that the entries were read from", pins.Commit, kubernetesPinsPath))
	}

	named := map[string]bool{}
	versions := map[string]string{}
	for _, r := range edit.Require {
		if kubernetesCovered(r.Path) {
			named[r.Path] = true
			versions[r.Path] = r.Version
		}
	}
	notCompared := map[string]bool{}
	sameReplace := map[string]string{}
	for _, r := range edit.Replace {
		path := r.Old.Path
		if !kubernetesCovered(path) {
			continue
		}
		named[path] = true
		switch {
		case r.New.Path != path:
			notCompared[path] = true
			problems = append(problems, fmt.Sprintf("go.mod replaces %s with %s, which this test cannot compare; replace it only with another version of %s, or remove the replace directive", path, r.New.Path, path))
		case r.Old.Version != "":
			notCompared[path] = true
			problems = append(problems, fmt.Sprintf("the replace directive for %s names a version on its left side (%s); write it without one", path, r.Old.Version))
		default:
			sameReplace[path] = r.New.Version
		}
	}
	maps.Copy(versions, sameReplace)

	for _, path := range slices.Sorted(maps.Keys(named)) {
		if notCompared[path] {
			continue
		}
		theirs, ok := pins.Modules[path]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s has no entry in \"modules\" of %s; add an entry with the version from the go.mod of Rancher main", path, kubernetesPinsPath))
			continue
		}
		ours := versions[path]
		cmp, err := compareModuleVersions(ours, theirs)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: go.mod has %s and the entry in %s has %s; %v; write both in the same form, as the go.mod of Rancher main does", path, ours, kubernetesPinsPath, theirs, err))
			continue
		}
		if cmp > 0 {
			problems = append(problems, fmt.Sprintf("go.mod has %s %s, which is newer than the entry %s in %s (the version in the go.mod of Rancher main at %s); lower the version in go.mod, or update the entry, and the file's \"commit\" and \"date\", when Rancher main has moved to it", path, ours, theirs, kubernetesPinsPath, pins.Commit))
		}
	}

	for _, path := range slices.Sorted(maps.Keys(pins.Modules)) {
		if !named[path] {
			problems = append(problems, fmt.Sprintf("%s is in \"modules\" of %s but go.mod has no require or replace directive for it; remove the entry from %s", path, kubernetesPinsPath, kubernetesPinsPath))
		}
	}
	return problems, nil
}

// compareModuleVersions returns -1, 0 or 1 when a is older than, equal to or
// newer than b. Equal strings are equal. Otherwise it compares two releases
// (three numeric fields each), or two pseudo-versions of the same base version
// and the same form, such as v0.0.0-20260101000000-0123456789ab or
// v0.37.3-0.20260101000000-0123456789ab, and returns an error for any other
// pair; kubernetesVersionProblems reports that error as a problem.
func compareModuleVersions(a, b string) (int, error) {
	if a == b && a != "" {
		return 0, nil
	}
	if ra, rb := releaseVersionPattern.FindStringSubmatch(a), releaseVersionPattern.FindStringSubmatch(b); ra != nil && rb != nil {
		for i := 1; i <= 3; i++ {
			na, err := strconv.ParseUint(ra[i], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("versions %q and %q cannot be compared: %w", a, b, err)
			}
			nb, err := strconv.ParseUint(rb[i], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("versions %q and %q cannot be compared: %w", a, b, err)
			}
			if na != nb {
				if na < nb {
					return -1, nil
				}
				return 1, nil
			}
		}
		return 0, nil
	}
	pa, pb := pseudoVersionPattern.FindStringSubmatch(a), pseudoVersionPattern.FindStringSubmatch(b)
	if pa != nil && pb != nil {
		if pa[1] != pb[1] || pa[2] != pb[2] {
			return 0, fmt.Errorf("pseudo-versions %s and %s cannot be compared: they have a different base version or a different form (one has -0. before the timestamp, the other has only -)", a, b)
		}
		if c := strings.Compare(pa[3], pb[3]); c != 0 {
			return c, nil
		}
		return 0, fmt.Errorf("pseudo-versions %s and %s cannot be compared: they have the same timestamp and different commit hashes", a, b)
	}
	for _, v := range []string{a, b} {
		if !releaseVersionPattern.MatchString(v) && !pseudoVersionPattern.MatchString(v) {
			return 0, fmt.Errorf("versions %q and %q cannot be compared because the form of %q is not vMAJOR.MINOR.PATCH or a pseudo-version (a pre-release version and +incompatible are neither)", a, b, v)
		}
	}
	// Both versions have a valid form and they are not both releases or both
	// pseudo-versions: one is a release and the other a pseudo-version.
	release, pseudo := a, b
	if pa != nil {
		release, pseudo = b, a
	}
	return 0, fmt.Errorf("the release %s and the pseudo-version %s cannot be compared because their forms differ", release, pseudo)
}

type testRequire struct {
	path     string
	version  string
	indirect bool
}

type testReplace struct {
	oldPath    string
	oldVersion string
	newPath    string
	newVersion string
}

type testModule struct {
	requires []testRequire
	replaces []testReplace
}

// json builds the JSON that `go mod edit -json go.mod` prints.
func (m testModule) json(t *testing.T) []byte {
	t.Helper()
	var requires, replaces []map[string]any
	for _, r := range m.requires {
		entry := map[string]any{"Path": r.path, "Version": r.version}
		if r.indirect {
			entry["Indirect"] = true
		}
		requires = append(requires, entry)
	}
	for _, r := range m.replaces {
		oldModule := map[string]any{"Path": r.oldPath}
		if r.oldVersion != "" {
			oldModule["Version"] = r.oldVersion
		}
		newModule := map[string]any{"Path": r.newPath}
		if r.newVersion != "" {
			newModule["Version"] = r.newVersion
		}
		replaces = append(replaces, map[string]any{"Old": oldModule, "New": newModule})
	}
	out, err := json.Marshal(map[string]any{
		"Module":  map[string]any{"Path": "github.com/rancher/rancher-cli"},
		"Go":      "1.27.4",
		"Require": requires,
		"Replace": replaces,
	})
	require.NoError(t, err)
	return out
}

func (m *testModule) setRequire(path, version string) {
	for i := range m.requires {
		if m.requires[i].path == path {
			m.requires[i].version = version
			return
		}
	}
	panic("no require " + path)
}

func (m *testModule) setReplace(path, version string) {
	for i := range m.replaces {
		if m.replaces[i].oldPath == path {
			m.replaces[i].newVersion = version
			return
		}
	}
	panic("no replace " + path)
}

// baseKubernetesModule has the same kinds of directives as the real go.mod:
// client-go has a require directive at a placeholder version and a replace
// directive with its real version, kubernetes has a replace directive and no
// require directive, and three libraries are indirect.
func baseKubernetesModule() testModule {
	return testModule{
		requires: []testRequire{
			{path: "github.com/stretchr/testify", version: "v1.12.1"},
			{path: "golang.org/x/net", version: "v0.62.0"},
			{path: "k8s.io/api", version: "v0.37.2"},
			{path: "k8s.io/apiextensions-apiserver", version: "v0.37.2"},
			{path: "k8s.io/apimachinery", version: "v0.37.2"},
			{path: "k8s.io/client-go", version: "v12.0.0+incompatible"},
			{path: "sigs.k8s.io/controller-runtime", version: "v0.24.1"},
			{path: "sigs.k8s.io/yaml", version: "v1.6.0"},
			{path: "k8s.io/klog/v2", version: "v2.140.0", indirect: true},
			{path: "k8s.io/kube-openapi", version: "v0.0.0-20260721132016-d427ff9ee9ad", indirect: true},
			{path: "k8s.io/utils", version: "v0.0.0-20260626114624-be93311217bd", indirect: true},
		},
		replaces: []testReplace{
			{oldPath: "k8s.io/apiserver", newPath: "k8s.io/apiserver", newVersion: "v0.37.2"},
			{oldPath: "k8s.io/client-go", newPath: "k8s.io/client-go", newVersion: "v0.37.2"},
			{oldPath: "k8s.io/component-base", newPath: "k8s.io/component-base", newVersion: "v0.37.2"},
			{oldPath: "k8s.io/kubernetes", newPath: "k8s.io/kubernetes", newVersion: "v1.37.2"},
		},
	}
}

// baseKubernetesPins is a literal and does not read the pins file on purpose:
// a refresh of the file must not break the rows that use it.
func baseKubernetesPins() kubernetesPins {
	return kubernetesPins{
		Source: "go.mod of github.com/rancher/rancher at main",
		Commit: "29798df3784b062cbd06d7a189fd6ef8c063ae3f",
		Date:   "2026-11-23",
		Modules: map[string]string{
			"k8s.io/api":                     "v0.37.2",
			"k8s.io/apiextensions-apiserver": "v0.37.2",
			"k8s.io/apimachinery":            "v0.37.2",
			"k8s.io/apiserver":               "v0.37.2",
			"k8s.io/client-go":               "v0.37.2",
			"k8s.io/component-base":          "v0.37.2",
			"k8s.io/klog/v2":                 "v2.140.0",
			"k8s.io/kube-openapi":            "v0.0.0-20260821135717-be32def86098",
			"k8s.io/kubernetes":              "v1.37.2",
			"k8s.io/utils":                   "v0.0.0-20261023123148-ca04ba58ecab",
			"sigs.k8s.io/controller-runtime": "v0.25.2",
		},
	}
}

// TestKubernetesVersionCheck asserts that kubernetesVersionProblems reports no
// problem for the unmodified go.mod and reports each problem kind: a covered
// module newer than its entry in the pins file, a module with no entry, an
// entry go.mod does not have, a replace by another module, a replace that names
// a version on its left side, versions that cannot be compared, and a bad
// commit or bad input.
func TestKubernetesVersionCheck(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name   string
		change func(m *testModule, p *kubernetesPins)
		raw    []byte
		want   []string
		err    string
	}{
		{name: "the unmodified module has no problem"},
		{
			name:   "apiserver replaced by a newer version",
			change: func(m *testModule, p *kubernetesPins) { m.setReplace("k8s.io/apiserver", "v0.37.3") },
			want:   []string{"go.mod has k8s.io/apiserver v0.37.3, which is newer than the entry v0.37.2"},
		},
		{
			name:   "api required at a newer version",
			change: func(m *testModule, p *kubernetesPins) { m.setRequire("k8s.io/api", "v0.37.3") },
			want:   []string{"go.mod has k8s.io/api v0.37.3, which is newer than the entry v0.37.2"},
		},
		{
			name:   "controller-runtime required at a newer version",
			change: func(m *testModule, p *kubernetesPins) { m.setRequire("sigs.k8s.io/controller-runtime", "v0.25.3") },
			want:   []string{"go.mod has sigs.k8s.io/controller-runtime v0.25.3, which is newer than the entry v0.25.2"},
		},
		{
			name:   "kubernetes has no entry",
			change: func(m *testModule, p *kubernetesPins) { delete(p.Modules, "k8s.io/kubernetes") },
			want:   []string{"k8s.io/kubernetes has no entry in \"modules\" of test/conformance/testdata/rancher-kubernetes-pins.json; add an entry with the version from the go.mod of Rancher main"},
		},
		{
			name: "a required k8s.io module has no entry",
			change: func(m *testModule, p *kubernetesPins) {
				m.requires = append(m.requires, testRequire{path: "k8s.io/kms", version: "v0.37.2"})
			},
			want: []string{"k8s.io/kms has no entry in \"modules\""},
		},
		{
			name: "an entry for a module go.mod has no directive for",
			change: func(m *testModule, p *kubernetesPins) {
				p.Modules["k8s.io/gengo/v2"] = "v2.0.0-20260408192533-25e2208e0dc3"
			},
			want: []string{"k8s.io/gengo/v2 is in \"modules\" of test/conformance/testdata/rancher-kubernetes-pins.json but go.mod has no require or replace directive for it; remove the entry from test/conformance/testdata/rancher-kubernetes-pins.json"},
		},
		{
			name: "kubernetes replaced by a local path",
			change: func(m *testModule, p *kubernetesPins) {
				m.setReplace("k8s.io/kubernetes", "")
				for i := range m.replaces {
					if m.replaces[i].oldPath == "k8s.io/kubernetes" {
						m.replaces[i].newPath = "./nowhere"
					}
				}
			},
			want: []string{"go.mod replaces k8s.io/kubernetes with ./nowhere, which this test cannot compare; replace it only with another version of k8s.io/kubernetes, or remove the replace directive"},
		},
		{
			name:   "the client-go entry is older than the replacement version",
			change: func(m *testModule, p *kubernetesPins) { p.Modules["k8s.io/client-go"] = "v0.37.1" },
			want:   []string{"go.mod has k8s.io/client-go v0.37.2, which is newer than the entry v0.37.1"},
		},
		{
			name: "pseudo-version entries dated before the module versions",
			change: func(m *testModule, p *kubernetesPins) {
				p.Modules["k8s.io/kube-openapi"] = "v0.0.0-20260701000000-0123456789ab"
				p.Modules["k8s.io/utils"] = "v0.0.0-20260601000000-0123456789ab"
			},
			want: []string{
				"go.mod has k8s.io/kube-openapi v0.0.0-20260721132016-d427ff9ee9ad, which is newer than the entry v0.0.0-20260701000000-0123456789ab",
				"go.mod has k8s.io/utils v0.0.0-20260626114624-be93311217bd, which is newer than the entry v0.0.0-20260601000000-0123456789ab",
			},
		},
		{
			name:   "the klog entry is older than the indirect require",
			change: func(m *testModule, p *kubernetesPins) { p.Modules["k8s.io/klog/v2"] = "v2.139.0" },
			want:   []string{"go.mod has k8s.io/klog/v2 v2.140.0, which is newer than the entry v2.139.0"},
		},
		{
			name:   "a pseudo-version entry where go.mod has a release",
			change: func(m *testModule, p *kubernetesPins) { p.Modules["k8s.io/api"] = "v0.0.0-20260101000000-0123456789ab" },
			want:   []string{"k8s.io/api: go.mod has v0.37.2 and the entry in test/conformance/testdata/rancher-kubernetes-pins.json has v0.0.0-20260101000000-0123456789ab; the release v0.37.2 and the pseudo-version v0.0.0-20260101000000-0123456789ab cannot be compared because their forms differ"},
		},
		{
			name: "a replace directive of a covered module with an old version",
			change: func(m *testModule, p *kubernetesPins) {
				m.replaces = append(m.replaces, testReplace{oldPath: "k8s.io/api", oldVersion: "v0.37.1", newPath: "k8s.io/api", newVersion: "v0.37.2"})
			},
			want: []string{"the replace directive for k8s.io/api names a version on its left side (v0.37.1); write it without one"},
		},
		{
			name: "an uncovered module is neither compared nor checked for a replace directive",
			change: func(m *testModule, p *kubernetesPins) {
				m.replaces = append(m.replaces, testReplace{oldPath: "sigs.k8s.io/yaml", newPath: "example.com/yaml", newVersion: "v1.0.0"})
				m.requires = append(m.requires, testRequire{path: "sigs.k8s.io/json", version: "v0.0.0-20250730193827-2d320260d730", indirect: true})
			},
		},
		{
			name:   "a commit of 39 digits",
			change: func(m *testModule, p *kubernetesPins) { p.Commit = p.Commit[:39] },
			want:   []string{"in test/conformance/testdata/rancher-kubernetes-pins.json is not 40 hexadecimal digits; set it to the commit of Rancher main that the entries were read from"},
		},
		{
			name:   "a commit with a digit that is not hexadecimal",
			change: func(m *testModule, p *kubernetesPins) { p.Commit = "g" + p.Commit[1:] },
			want:   []string{"in test/conformance/testdata/rancher-kubernetes-pins.json is not 40 hexadecimal digits; set it to the commit of Rancher main that the entries were read from"},
		},
		{
			name: "bytes that are not JSON",
			raw:  []byte("module example.com/m\n"),
			err:  "invalid character",
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			m := baseKubernetesModule()
			pins := baseKubernetesPins()
			if row.change != nil {
				row.change(&m, &pins)
			}
			input := row.raw
			if input == nil {
				input = m.json(t)
			}
			problems, err := kubernetesVersionProblems(input, pins)
			if row.err != "" {
				require.ErrorContains(t, err, row.err)
				return
			}
			require.NoError(t, err)
			if len(row.want) == 0 {
				assert.Empty(t, problems)
				return
			}
			for _, want := range row.want {
				assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }),
					"none of the problems contains %q; the problems are:\n%s", want, strings.Join(problems, "\n"))
			}
			assert.Len(t, problems, len(row.want))
		})
	}
}

// TestKubernetesVersionComparison asserts which of two releases or two
// pseudo-versions is newer, and that every other pair is an error.
func TestKubernetesVersionComparison(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name string
		a, b string
		want int
		err  string
	}{
		{name: "equal releases", a: "v0.37.2", b: "v0.37.2", want: 0},
		{name: "older patch", a: "v0.37.1", b: "v0.37.2", want: -1},
		{name: "newer patch", a: "v0.37.3", b: "v0.37.2", want: 1},
		{name: "older minor", a: "v0.36.9", b: "v0.37.0", want: -1},
		{name: "newer minor", a: "v0.38.0", b: "v0.37.9", want: 1},
		{name: "older major", a: "v1.0.0", b: "v2.0.0", want: -1},
		{name: "ten is newer than two", a: "v0.37.10", b: "v0.37.2", want: 1},
		{name: "two is older than ten", a: "v0.37.2", b: "v0.37.10", want: -1},
		{name: "equal incompatible versions", a: "v12.0.0+incompatible", b: "v12.0.0+incompatible", want: 0},
		{name: "older pseudo-version", a: "v0.0.0-20260101000000-0123456789ab", b: "v0.0.0-20260201000000-ba9876543210", want: -1},
		{name: "newer pseudo-version", a: "v0.0.0-20260301000000-0123456789ab", b: "v0.0.0-20260201000000-ba9876543210", want: 1},
		{name: "older pseudo-version after a release base", a: "v0.37.3-0.20260101000000-0123456789ab", b: "v0.37.3-0.20260201000000-ba9876543210", want: -1},
		{name: "newer pseudo-version after a release base", a: "v0.37.3-0.20260301000000-0123456789ab", b: "v0.37.3-0.20260201000000-ba9876543210", want: 1},
		{name: "equal pseudo-versions", a: "v0.0.0-20260101000000-0123456789ab", b: "v0.0.0-20260101000000-0123456789ab", want: 0},
		{name: "same timestamp and different commit hashes", a: "v0.0.0-20260101000000-0123456789ab", b: "v0.0.0-20260101000000-ba9876543210", err: "pseudo-versions v0.0.0-20260101000000-0123456789ab and v0.0.0-20260101000000-ba9876543210 cannot be compared: they have the same timestamp and different commit hashes"},
		{name: "different bases", a: "v0.0.0-20260101000000-0123456789ab", b: "v0.1.0-0.20260201000000-ba9876543210", err: "cannot be compared: they have a different base version or a different form (one has -0. before the timestamp, the other has only -)"},
		{name: "same base version and different forms", a: "v0.37.3-0.20260101000000-0123456789ab", b: "v0.37.3-20260201000000-ba9876543210", err: "cannot be compared: they have a different base version or a different form (one has -0. before the timestamp, the other has only -)"},
		{name: "release against pseudo-version", a: "v0.37.2", b: "v0.0.0-20260101000000-0123456789ab", err: "the release v0.37.2 and the pseudo-version v0.0.0-20260101000000-0123456789ab cannot be compared because their forms differ"},
		{name: "pseudo-version against release", a: "v0.0.0-20260101000000-0123456789ab", b: "v0.37.2", err: "the release v0.37.2 and the pseudo-version v0.0.0-20260101000000-0123456789ab cannot be compared because their forms differ"},
		{name: "pre-release", a: "v0.38.0-rc.0", b: "v0.37.2", err: `versions "v0.38.0-rc.0" and "v0.37.2" cannot be compared because the form of "v0.38.0-rc.0" is not vMAJOR.MINOR.PATCH or a pseudo-version (a pre-release version and +incompatible are neither)`},
		{name: "the pre.0 form", a: "v0.38.0-pre.0.20260101000000-0123456789ab", b: "v0.38.0-pre.0.20260201000000-ba9876543210", err: "cannot be compared because the form of"},
		{name: "incompatible against a release", a: "v12.0.0+incompatible", b: "v0.37.2", err: "cannot be compared because the form of"},
		{name: "leading zero", a: "v0.037.2", b: "v0.37.2", err: "cannot be compared because the form of"},
		{name: "empty string", a: "", b: "v0.37.2", err: "cannot be compared because the form of"},
		{name: "two empty strings", a: "", b: "", err: "cannot be compared because the form of"},
		{name: "malformed", a: "0.37.2", b: "v0.37.2", err: "cannot be compared because the form of"},
		{name: "a release field that overflows uint64 in the first version", a: "v0.37.99999999999999999999", b: "v0.37.2", err: "value out of range"},
		{name: "a release field that overflows uint64 in the second version", a: "v0.37.2", b: "v0.37.99999999999999999999", err: "value out of range"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := compareModuleVersions(row.a, row.b)
			if row.err != "" {
				require.ErrorContains(t, err, row.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, row.want, got)
		})
	}
}

// TestKubernetesModulesAreNotAheadOfRancher asserts that go.mod has no k8s.io
// module and no controller-runtime at a version newer than the one in the
// go.mod of Rancher main at the commit in the pins file. "Not ahead of" in the
// name means "not newer than".
func TestKubernetesModulesAreNotAheadOfRancher(t *testing.T) {
	t.Parallel()

	root, err := moduleRoot()
	require.NoError(t, err)
	pins, err := loadKubernetesPins(filepath.Join(root, filepath.FromSlash(kubernetesPinsPath)))
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), "go", "mod", "edit", "-json", "go.mod")
	cmd.Dir = root
	cmd.Env = testfs.EnvWithoutGit()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())

	problems, err := kubernetesVersionProblems(out, pins)
	require.NoError(t, err)
	if len(problems) > 0 {
		t.Errorf("go.mod and %s do not match:\n%s", kubernetesPinsPath, strings.Join(problems, "\n"))
	}
}
