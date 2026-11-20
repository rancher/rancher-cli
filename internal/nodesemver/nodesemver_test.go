package nodesemver

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "record the golden from the Node.js semver module")

const goldenPath = "testdata/golden.json"

type satisfiesRow struct {
	Version string `json:"version"`
	Range   string `json:"range"`
	Result  bool   `json:"result"`
}

type coerceRow struct {
	Input  string  `json:"input"`
	Result *string `json:"result"`
}

type golden struct {
	SemverVersion string         `json:"semverVersion"`
	Satisfies     []satisfiesRow `json:"satisfies"`
	Coerce        []coerceRow    `json:"coerce"`
}

// allRanges is every range the golden holds an answer for, in the order the
// golden lists them.
func allRanges() []string {
	var all []string
	for _, group := range [][]string{kubeVersionRanges, rancherVersionRanges, uiVersionRanges, extensionsVersionRanges, handWrittenRanges} {
		all = append(all, group...)
	}
	return slices.Compact(slices.Sorted(slices.Values(all)))
}

func readGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	require.NoError(t, err)
	var g golden
	require.NoError(t, json.Unmarshal(raw, &g))
	return g
}

// writeGolden writes the golden with one row to a line, so a rewrite shows in
// review as the rows that changed, and with LF line ends on every platform.
func writeGolden(t *testing.T, g golden) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("{\n  \"semverVersion\": ")
	version, err := json.Marshal(g.SemverVersion)
	require.NoError(t, err)
	b.Write(version)
	b.WriteString(",\n  \"satisfies\": [\n")
	for i, row := range g.Satisfies {
		line, err := json.Marshal(row)
		require.NoError(t, err)
		b.WriteString("    ")
		b.Write(line)
		if i < len(g.Satisfies)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  ],\n  \"coerce\": [\n")
	for i, row := range g.Coerce {
		line, err := json.Marshal(row)
		require.NoError(t, err)
		b.WriteString("    ")
		b.Write(line)
		if i < len(g.Coerce)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  ]\n}\n")
	require.NoError(t, os.WriteFile(goldenPath, b.Bytes(), 0o644))
}

// recordGolden asks the semver module, found in the directory named by
// NODE_SEMVER_PATH, for the answer to every case and rewrites the golden.
func recordGolden(t *testing.T) {
	t.Helper()
	dir := os.Getenv("NODE_SEMVER_PATH")
	require.NotEmpty(t, dir, "set NODE_SEMVER_PATH to the directory that holds the semver module")
	var request golden
	for _, rng := range allRanges() {
		for _, ver := range versions {
			request.Satisfies = append(request.Satisfies, satisfiesRow{Version: ver, Range: rng})
		}
	}
	for _, input := range coerceInputs {
		request.Coerce = append(request.Coerce, coerceRow{Input: input})
	}
	in, err := json.Marshal(request)
	require.NoError(t, err)
	script, err := filepath.Abs("testdata/record.js")
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "node", script)
	cmd.Env = append(os.Environ(), "NODE_PATH="+dir)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	var out golden
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.NotEmpty(t, out.SemverVersion)
	writeGolden(t, out)
}

// TestNodeSemverGolden asserts that Satisfies and Coerce give, for every row of
// the golden, the answer the Node.js semver module gave when the rows were
// recorded. The rows were recorded with semver 7.8.5 and each row was
// compared with the answer of 7.7.4, the version the Rancher UI resolves; a row
// the two versions answered differently is not in the golden. Rewrite the
// golden with `go test ./internal/nodesemver/ -run TestNodeSemverGolden
// -update`, with NODE_SEMVER_PATH set to the node_modules directory that holds
// the semver module to record from; that needs node, and a normal run reads the
// golden only.
func TestNodeSemverGolden(t *testing.T) {
	if *update {
		recordGolden(t)
	}
	g := readGolden(t)
	require.NotEmpty(t, g.SemverVersion)
	require.NotEmpty(t, g.Satisfies)
	require.NotEmpty(t, g.Coerce)

	t.Run("satisfies", func(t *testing.T) {
		t.Parallel()
		for _, row := range g.Satisfies {
			assert.Equal(t, row.Result, Satisfies(row.Version, row.Range), "satisfies(%q, %q)", row.Version, row.Range)
		}
	})
	t.Run("coerce", func(t *testing.T) {
		t.Parallel()
		for _, row := range g.Coerce {
			got, ok := Coerce(row.Input)
			if row.Result == nil {
				assert.False(t, ok, "coerce(%q) found %q, want no version", row.Input, got)
				continue
			}
			assert.True(t, ok, "coerce(%q) found no version, want %q", row.Input, *row.Result)
			assert.Equal(t, *row.Result, got, "coerce(%q)", row.Input)
		}
	})
}

// TestGoldenHoldsEveryCase asserts that the golden holds a row for every range
// of the annotation table and every hand-written range crossed with every
// version, and for every coerce input, so that a case added to the tables
// without a recording fails here.
func TestGoldenHoldsEveryCase(t *testing.T) {
	t.Parallel()
	g := readGolden(t)
	have := map[[2]string]bool{}
	for _, row := range g.Satisfies {
		have[[2]string{row.Version, row.Range}] = true
	}
	for _, rng := range allRanges() {
		for _, ver := range versions {
			assert.True(t, have[[2]string{ver, rng}], "golden has no row for satisfies(%q, %q)", ver, rng)
		}
	}
	inputs := map[string]bool{}
	for _, row := range g.Coerce {
		inputs[row.Input] = true
	}
	for _, input := range coerceInputs {
		assert.True(t, inputs[input], "golden has no row for coerce(%q)", input)
	}
}

// TestAnnotationRangesAreWellFormed asserts that every range value of the
// annotation table is one the Node.js package accepts, so that a typo in the
// table does not record a refusal as an answer.
func TestAnnotationRangesAreWellFormed(t *testing.T) {
	t.Parallel()
	for _, group := range [][]string{kubeVersionRanges, rancherVersionRanges, uiVersionRanges, extensionsVersionRanges} {
		for _, rng := range group {
			_, ok := parseRange(rng)
			assert.True(t, ok, "range %q is refused", rng)
			assert.Equal(t, strings.TrimSpace(rng), rng)
		}
	}
}

// TestRangeSemantics asserts a few answers that define the behavior the
// extension checks rely on, independent of the golden.
func TestRangeSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version, rng string
		want         bool
	}{
		{"2.15.3", ">= 2.15.0-0", true},
		{"2.14.9", ">= 2.15.0-0", false},
		{"2.16.999", ">= 2.13.0-0 < 2.14.0-0", false},
		{"1.2.3-alpha.2", ">=1.0.0", false},
		{"1.2.3-alpha.2", ">=1.2.3-alpha.1", true},
		{"1.2.4-alpha.2", ">=1.2.3-alpha.1", false},
		{"3.0.12-rc.7", ">= 3.0.5-rc.6", false},
		{"3.0.5-rc.7", ">= 3.0.5-rc.6", true},
		{"3.0.12-rc.7", ">= 3.0.0", false},
		{"2.5.0", "2.4.0 || >=2.5.0", true},
		{"2.4.5", "2.4.x", true},
		{"1.2.3", "invalid", false},
		{"", "*", false},
		{"1.2.3", "", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, Satisfies(tt.version, tt.rng), "satisfies(%q, %q)", tt.version, tt.rng)
	}
	for in, want := range map[string]string{"v2.15.3": "2.15.3", "2.16-head": "2.16.0", "v1.36.5+k3s1": "1.36.5", "1.2.3-beta": "1.2.3"} {
		got, ok := Coerce(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	_, ok := Coerce("abc")
	assert.False(t, ok)
}
