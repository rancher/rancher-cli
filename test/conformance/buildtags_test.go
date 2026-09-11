package conformance

import (
	"bufio"
	"go/build/constraint"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platformTags are the build constraint identifiers the toolchain
// defines on its own: operating systems, architectures, the compiler,
// sanitizer and crypto tags, and the standard library's link-mode tags.
// Release and experiment tags are prefixes (go1.N, goexperiment.X).
// Every other identifier in a //go:build line is a tag this repository
// invented, and lint compiles a file behind it only when .golangci.yml
// names it.
var platformTags = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"illumos": true, "ios": true, "js": true, "linux": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true,
	"unix": true,
	"386":  true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mipsle": true, "mips64": true, "mips64le": true, "ppc64": true,
	"ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
	"cgo": true, "gc": true, "gccgo": true, "purego": true, "race": true, "ignore": true,
	"msan": true, "asan": true, "boringcrypto": true,
	"netgo": true, "osusergo": true, "timetzdata": true, "math_big_pure_go": true,
}

// TestLintBuildTagsCoverEveryCustomTag asserts that every build tag the
// repository's Go files invent appears in .golangci.yml's run.build-tags,
// and that every tag listed there is still in use. A tag missing from
// the list means golangci-lint never compiles the files behind it and
// reports a clean gate over code it has not seen.
func TestLintBuildTagsCoverEveryCustomTag(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	inTree := map[string][]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		for _, tag := range customTags(t, path) {
			inTree[tag] = append(inTree[tag], path)
		}
	}
	listed := lintBuildTags(t)
	for tag, where := range inTree {
		assert.True(t, listed[tag], "build tag %q (used in %v) is missing from .golangci.yml run.build-tags", tag, where)
	}
	for tag := range listed {
		assert.Contains(t, inTree, tag, "build tag %q is listed in .golangci.yml but no Go file uses it", tag)
	}
}

// customTags returns the non-platform identifiers of path's //go:build
// lines. Only the lines before the package clause are constraints.
func customTags(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var tags []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		require.NoError(t, err, "%s: %s", path, line)
		expr.Eval(func(tag string) bool {
			if !platformTags[tag] && !strings.HasPrefix(tag, "go1.") && !strings.HasPrefix(tag, "goexperiment.") {
				tags = append(tags, tag)
			}
			return true
		})
	}
	require.NoError(t, sc.Err())
	return tags
}

var lintBuildTagLine = regexp.MustCompile(`^\s+-\s+(\S+)\s*$`)

// lintBuildTags reads the run.build-tags list from .golangci.yml: the
// indented "- tag" lines that follow the build-tags key.
func lintBuildTags(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(".golangci.yml")
	require.NoError(t, err)
	tags := map[string]bool{}
	in := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case strings.TrimSpace(line) == "build-tags:":
			in = true
		case in:
			m := lintBuildTagLine.FindStringSubmatch(line)
			if m == nil {
				in = false
				continue
			}
			tags[m[1]] = true
		}
	}
	require.NotEmpty(t, tags, ".golangci.yml has no run.build-tags list")
	return tags
}
