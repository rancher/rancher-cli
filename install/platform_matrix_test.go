package install_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var platformLoopRe = regexp.MustCompile(`for platform in ([^;]+); do`)

// parseWorkflowPlatforms extracts the GOOS/GOARCH list from a workflow's
// `for platform in ...; do` build loop.
func parseWorkflowPlatforms(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	m := platformLoopRe.FindStringSubmatch(string(raw))
	require.NotNilf(t, m, "%s: no `for platform in ...; do` build loop found - it moved or was reworded", path)
	return strings.Fields(m[1])
}

// installerPlatforms is every release platform install.sh must serve -
// derived from release.yaml rather than duplicated, so harness_test.go's
// stub release assets cannot drift from what a real release actually
// publishes. Windows is excluded: the shell installer serves POSIX
// platforms only, by design - it has no Windows binary format to install.
var installerPlatforms = deriveInstallerPlatforms()

func deriveInstallerPlatforms() []string {
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yaml"))
	if err != nil {
		panic(err)
	}
	m := platformLoopRe.FindStringSubmatch(string(raw))
	if m == nil {
		panic("release.yaml: no `for platform in ...; do` build loop found - it moved or was reworded")
	}
	var out []string
	for p := range strings.FieldsSeq(m[1]) {
		if strings.HasPrefix(p, "windows/") {
			continue
		}
		out = append(out, strings.Replace(p, "/", "-", 1))
	}
	return out
}

// TestPlatformMatrixConformance keeps the release platform matrix from
// desynchronizing across its independent copies: release.yaml's build loop,
// ci.yaml's compile-check loop (which must exercise the same targets before
// a tag is cut), and install.sh's uname-to-GOOS/GOARCH map (asserted via
// platformMappingCases, since install.sh is downloaded standalone and cannot
// derive its map from any other file in this repo).
func TestPlatformMatrixConformance(t *testing.T) {
	t.Parallel()

	release := parseWorkflowPlatforms(t, filepath.Join("..", ".github", "workflows", "release.yaml"))
	ci := parseWorkflowPlatforms(t, filepath.Join("..", ".github", "workflows", "ci.yaml"))
	assert.ElementsMatch(t, release, ci, "release.yaml and ci.yaml must build-check the same platform set")

	// platformMappingCases has multiple uname aliases per platform (e.g. both
	// "x86_64" and "amd64" map to linux/amd64) - dedupe before comparing sets.
	seen := map[string]bool{}
	var installSHAccepts []string
	for _, tc := range platformMappingCases {
		if tc.want != "" && !seen[tc.want] {
			seen[tc.want] = true
			installSHAccepts = append(installSHAccepts, tc.want)
		}
	}
	var wantAccepts []string
	for _, p := range installerPlatforms {
		wantAccepts = append(wantAccepts, strings.Replace(p, "-", "/", 1))
	}
	assert.ElementsMatch(t, wantAccepts, installSHAccepts,
		"install.sh's accepted platforms (platformMappingCases) must match release.yaml's non-Windows targets")
}
