package fidelity

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knownFileName is the file name a known-differences file must have; its
// minor is the file's own.
var knownFileName = regexp.MustCompile(`^v([1-9][0-9]*\.(0|[1-9][0-9]*))\.json$`)

// checkKnownFile applies every rule to one known-differences file's bytes:
// the format, the minor against the file name, and each entry against the
// goldens under oracleDir and downstreamDir.
func checkKnownFile(name string, raw []byte, probes []Probe, oracleDir, downstreamDir string) error {
	m := knownFileName.FindStringSubmatch(name)
	if m == nil {
		return errors.New("the file name is not v<major>.<minor>.json")
	}
	k, err := ParseKnownFile(raw)
	if err != nil {
		return err
	}
	var errs []error
	if k.Minor != m[1] {
		errs = append(errs, errors.New("minor "+k.Minor+" does not match the file name "+name))
	}
	if err := k.CheckGoldens(probes, oracleDir, downstreamDir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// TestKnownDifferencesFiles asserts that every tracked known-differences
// file, of an upcoming minor and of the helm-check, passes every rule: strict
// decode, the minor against its name, every probe in All(), want equal to the
// current golden's value, and the format. The helm-check's file for v2.15
// exists.
func TestKnownDifferencesFiles(t *testing.T) {
	testdata := filepath.Join("testdata")
	files, err := filepath.Glob(filepath.Join(testdata, "upcoming", "*.json"))
	require.NoError(t, err)
	install, err := filepath.Glob(filepath.Join(testdata, "install", "*.json"))
	require.NoError(t, err)
	require.Contains(t, install, filepath.Join(testdata, "install", "v2.15.json"))
	files = append(files, install...)
	for _, path := range files {
		t.Run(filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, checkKnownFile(filepath.Base(path), raw, All(),
				filepath.Join(testdata, "oracle"), filepath.Join(testdata, "downstream")))
		})
	}
}

// fixtureGoldens writes the goldens the fixtures refer to: one probe that is
// not downstream with an edition observation and one downstream probe.
func fixtureGoldens(t *testing.T) (probes []Probe, oracleDir, downstreamDir string) {
	t.Helper()
	root := t.TempDir()
	oracleDir = filepath.Join(root, "oracle")
	downstreamDir = filepath.Join(root, "downstream")
	probes = []Probe{{Name: "fixture/oracle"}, {Name: "fixture/downstream", Downstream: true}}
	require.NoError(t, SaveGolden(GoldenPath(oracleDir, "fixture/oracle"), Golden{
		OracleImage: "image",
		Observations: []ObservationResult{{
			Name: "edition", Status: 200, ContentType: "text/plain",
			Body: map[string]any{
				"GitCommit": "aaaa",
				"Version":   "v1.0.0",
				"a/b":       "slash",
				"~1":        "tilde",
				"labels":    map[string]any{"a/b": "x"},
				"list":      []any{1.0, 2.0},
				"nothing":   nil,
			},
		}, {Name: "gone", Status: 404, ContentType: "application/json"}},
	}))
	require.NoError(t, SaveGolden(GoldenPath(downstreamDir, "fixture/downstream"), Golden{
		OracleImage: "image",
		Observations: []ObservationResult{{
			Name: "steve", Status: 200, ContentType: "application/json",
			Body: map[string]any{"state": "active"},
		}},
	}))
	return probes, oracleDir, downstreamDir
}

// TestKnownDifferencesFixtures asserts that every passing fixture passes
// every rule and that each failing fixture fails with the message of the one
// rule it breaks, so a validator that drops a rule fails its fixture. The
// fixtures describe a small set of goldens written for the test.
func TestKnownDifferencesFixtures(t *testing.T) {
	failing := map[string]string{
		"bad-unknown-field":          `unknown field "note"`,
		"bad-minor-name":             "minor 2.17 does not match the file name v2.16.json",
		"bad-minor-format":           `minor "2.x" is not <major>.<minor>`,
		"bad-entries-missing":        "entries is missing",
		"bad-probe":                  "no probe with that name",
		"bad-pointer-slash":          `pointer "edition/status" does not name a place in an observation; a pointer starts with / and every ~ is ~0 or ~1`,
		"bad-pointer-tilde":          `pointer "/edition/a~2b" does not name a place in an observation; a pointer starts with / and every ~ is ~0 or ~1`,
		"bad-pointer-empty":          `pointer "" does not name a place in an observation; a pointer starts with / and every ~ is ~0 or ~1`,
		"bad-pointer-trailing-tilde": `pointer "/edition/a~" does not name a place in an observation; a pointer starts with / and every ~ is ~0 or ~1`,
		"bad-pointer-leading-zero":   "want 1 differs from the golden's nothing; update the entry's want or remove the entry",
		"bad-trailing-data":          "the file is not in canonical form",
		"bad-url-empty-ruling":       "url is set with an empty ruling; a url goes with rancher-defect or cli-change",
		"bad-want-differs":           "want 201 differs from the golden's 200; update the entry's want or remove the entry",
		"bad-absent-want":            "want nothing differs from the golden's 200",
		"bad-got-and-varies":         "got and gotVaries are both set",
		"bad-cause":                  `cause "kubernetes-ish" is not rancher, kubernetes, install or undetermined`,
		"bad-ruling":                 `ruling "later" is not fake-follows, cli-change, expected-at-ga, newer-patch, rancher-defect or empty`,
		"bad-defect-no-url":          "ruling rancher-defect needs a url",
		"bad-url-unlisted":           `url "https://github.com/rancher/not-listed/issues/1" is not an issue or pull request of an allowed repository`,
		"bad-url-shape":              `url "https://github.com/rancher/rancher/issues/abc" is not an issue or pull request of an allowed repository`,
		"bad-url-ruling":             `ruling "expected-at-ga" takes no url; a url goes with rancher-defect or cli-change`,
		"bad-first-seen":             `firstSeen "ABC1234" is not 7 to 40 lowercase hex digits`,
		"bad-unsorted":               "entries are not sorted by probe, then pointer",
		"bad-duplicate":              "duplicate entry; one entry per probe and pointer",
		"bad-indent":                 "the file is not in canonical form",
		"bad-no-final-newline":       "the file is not in canonical form",
		"bad-non-ascii":              "the file is not in canonical form",
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "upcoming-fixtures", "*.txt"))
	require.NoError(t, err)
	probes, oracleDir, downstreamDir := fixtureGoldens(t)
	seen := map[string]bool{}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			err = checkKnownFile("v2.16.json", raw, probes, oracleDir, downstreamDir)
			if strings.HasPrefix(name, "ok-") {
				require.NoError(t, err)
				return
			}
			want, ok := failing[name]
			require.Truef(t, ok, "failing fixture %s has no expected message", name)
			require.ErrorContains(t, err, want)
		})
	}
	for name := range failing {
		assert.Truef(t, seen[name], "no fixture file for %s", name)
	}
}

// TestKnownPointerSyntax asserts the pointer rule: at least one segment,
// each starting with a slash, every tilde followed by 0 or 1, empty segments
// allowed.
func TestKnownPointerSyntax(t *testing.T) {
	for _, ok := range []string{"/", "/a", "/a/b", "/a//b", "/a~0", "/a~1b", "/~01", "/a/0", "/\u00b7"} {
		assert.Truef(t, pointerPattern.MatchString(ok), "%q", ok)
	}
	for _, bad := range []string{"", "a", "a/b", "/a~", "/a~2", "/a~b", "~0", "/a~/b"} {
		assert.Falsef(t, pointerPattern.MatchString(bad), "%q", bad)
	}
}

// TestKnownDifferencesFileName asserts that a file name other than
// v<major>.<minor>.json is refused.
func TestKnownDifferencesFileName(t *testing.T) {
	probes, oracleDir, downstreamDir := fixtureGoldens(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "upcoming-fixtures", "ok-empty.txt"))
	require.NoError(t, err)
	for _, name := range []string{"2.16.json", "v2.16.txt", "v2.json", "v02.16.json"} {
		require.ErrorContains(t, checkKnownFile(name, raw, probes, oracleDir, downstreamDir), "the file name is not")
	}
	require.NoError(t, checkKnownFile("v2.16.json", raw, probes, oracleDir, downstreamDir))
}

// TestKnownURLRepositories asserts the exact list of repositories a url may
// name, issues and pull requests alike, and that a near miss is refused.
func TestKnownURLRepositories(t *testing.T) {
	for _, repo := range []string{
		"rancher/rancher", "rancher/rancher-cli", "rancher/webhook", "rancher/steve", "rancher/dashboard",
		"kubernetes/kubernetes", "kubernetes-sigs/controller-runtime", "k3s-io/k3s",
	} {
		assert.Truef(t, knownURLPattern.MatchString("https://github.com/"+repo+"/issues/12"), "%s issue", repo)
		assert.Truef(t, knownURLPattern.MatchString("https://github.com/"+repo+"/pull/12"), "%s pull", repo)
	}
	for _, url := range []string{
		"https://github.com/rancher/not-listed/issues/1",
		"https://github.com/rancher/rancher-cli-extra/issues/1",
		"https://github.com/xrancher/rancher/issues/1",
		"https://github.com/rancher/ranchex/issues/1",
		"http://github.com/rancher/rancher/issues/1",
		"https://github.com/rancher/rancher/issues/0",
		"https://github.com/rancher/rancher/issues/01",
		"https://github.com/rancher/rancher/issues/1/",
		"https://github.com/rancher/rancher/issues/1#x",
		"https://github.com/rancher/rancher/discussions/1",
		"https://gist.github.com/rancher/rancher/issues/1",
		"https://github.com/rancher/rancher",
	} {
		assert.Falsef(t, knownURLPattern.MatchString(url), "%s", url)
	}
}

// TestEncodeDriftJSONEscapesNonASCII asserts that the encoder writes every
// non-ASCII character as \uXXXX, with a surrogate pair above U+FFFF, indents
// with two spaces and ends with one newline.
func TestEncodeDriftJSONEscapesNonASCII(t *testing.T) {
	raw, err := encodeDriftJSON(map[string]any{"k": "a\u00b7\u00e9\U0001F600"})
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"k\": \"a\\u00b7\\u00e9\\ud83d\\ude00\"\n}\n", string(raw))
}

// TestLoadKnownFile asserts that a missing file is no file, a valid file
// loads, and an unreadable or invalid one is an error with Exists set.
func TestLoadKnownFile(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, KnownFileLoad{}, LoadKnownFile(filepath.Join(dir, "v2.16.json")))

	raw, err := os.ReadFile(filepath.Join("testdata", "upcoming-fixtures", "ok-oracle-entry.txt"))
	require.NoError(t, err)
	path := filepath.Join(dir, "v2.16.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	load := LoadKnownFile(path)
	require.NoError(t, load.Err)
	assert.True(t, load.Exists)
	require.Len(t, load.File.Entries, 1)
	assert.Equal(t, "/edition/status", load.File.Entries[0].Pointer)

	require.NoError(t, os.WriteFile(path, []byte(`{"minor": "2.16", "entries": [], "extra": 1}`), 0o600))
	load = LoadKnownFile(path)
	require.ErrorContains(t, load.Err, `unknown field "extra"`)
	assert.True(t, load.Exists)

	load = LoadKnownFile(dir)
	require.Error(t, load.Err, "a directory is not a readable file")
	assert.True(t, load.Exists)
}
