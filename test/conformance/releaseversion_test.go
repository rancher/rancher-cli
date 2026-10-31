package conformance

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// buildCallRE matches a call of scripts/build, alone or after an
	// environment assignment, in a command of shell code.
	buildCallRE = regexp.MustCompile(`(^|[\s;&|(])(\./)?scripts/build($|[\s;&|)])`)
	// versionFromTagRE matches the assignment that gives the build the tag as
	// its version.
	versionFromTagRE = regexp.MustCompile(`(^|\s)VERSION="\$TAG"(\s|$)`)
	// otherBuildRE matches the build commands that do not go through
	// scripts/build, so would not set the version.
	otherBuildRE = regexp.MustCompile(`(^|[\s;&|(])(go\s+(build|install)|make\s+build)($|[\s;&|)])`)
	// uploadsReleaseRE matches a command that publishes files to a release.
	uploadsReleaseRE = regexp.MustCompile(`(^|[\s;&|(])(\./)?scripts/release-upsert($|[\s;&|)])|(^|[\s;&|(])gh\s+release\b`)
)

// workflowCommands returns every command of the run steps of a workflow, with
// comments removed and continued lines joined.
func workflowCommands(wf gateWorkflow) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		for _, step := range wf.Jobs[name].Steps {
			out = append(out, shellCodeOf(step.Run)...)
		}
	}
	return out
}

// uploadsBinary reports whether a workflow publishes built files. A workflow
// uploads a binary when a step runs scripts/release-upsert or gh release, or
// uses actions/attest-build-provenance, or uses actions/upload-artifact while
// the workflow builds something (fuzz.yaml uploads failure artifacts and
// calls no build, so it does not count).
func uploadsBinary(wf gateWorkflow) bool {
	builds, artifacts := false, false
	for _, c := range workflowCommands(wf) {
		if uploadsReleaseRE.MatchString(c) {
			return true
		}
		if buildCallRE.MatchString(c) || otherBuildRE.MatchString(c) {
			builds = true
		}
	}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			switch {
			case strings.HasPrefix(step.Uses, "actions/attest-build-provenance@"):
				return true
			case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
				artifacts = true
			}
		}
	}
	return builds && artifacts
}

// versionFromTagProblems returns what stops a workflow that uploads a binary
// from giving its build the release tag as the version: a scripts/build call
// without VERSION="$TAG" before it on the same command, and a build command
// that does not go through scripts/build. A workflow that uploads nothing has
// no problem.
func versionFromTagProblems(wf gateWorkflow) []string {
	if !uploadsBinary(wf) {
		return nil
	}
	var out []string
	for _, c := range workflowCommands(wf) {
		if loc := buildCallRE.FindStringIndex(c); loc != nil {
			if v := versionFromTagRE.FindStringIndex(c); v == nil || v[0] > loc[0] {
				out = append(out, "scripts/build without VERSION=\"$TAG\": "+strings.TrimSpace(c))
			}
		}
		if otherBuildRE.MatchString(c) {
			out = append(out, "a build that does not set the version: "+strings.TrimSpace(c))
		}
	}
	return out
}

// TestReleaseBuildsSetVersionFromTag asserts every scripts/build call of the
// release workflow, and of any workflow that uploads a binary, carries
// VERSION="$TAG", and that those workflows hold no go build, go install or
// make build, which would give the binary the version dev, the one version
// that honors the experiment variable. A workflow uploads a binary when a
// step runs scripts/release-upsert or gh release, uses
// actions/attest-build-provenance, or uses actions/upload-artifact in a
// workflow that also builds.
func TestReleaseBuildsSetVersionFromTag(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(".github/workflows/*.y*ml")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	sawRelease := false
	for _, p := range paths {
		wf := readGateWorkflow(t, p)
		if p == releaseWorkflowPath {
			sawRelease = true
			assert.True(t, uploadsBinary(wf), "the scan no longer sees the release workflow upload a binary")
			assert.NotEmpty(t, workflowCommands(wf), p)
			assert.True(t, slices.ContainsFunc(workflowCommands(wf), buildCallRE.MatchString), "%s holds no scripts/build call", p)
		}
		assert.Empty(t, versionFromTagProblems(wf), p)
	}
	assert.True(t, sawRelease)
}

// TestReleaseVersionCheckRules asserts what the check accepts and refuses.
func TestReleaseVersionCheckRules(t *testing.T) {
	t.Parallel()
	const ok = `
jobs:
  publish:
    steps:
      - name: Build
        run: |
          for platform in linux/amd64 darwin/arm64; do
            # a comment mentioning ./scripts/build is ignored
            GOOS="$goos" GOARCH="$goarch" VERSION="$TAG" ./scripts/build
          done
      - uses: actions/attest-build-provenance@0000000000000000000000000000000000000000
      - run: scripts/release-upsert dist/*
`
	assert.Empty(t, versionFromTagProblems(parseGateWorkflow(t, ok)))
	assert.True(t, uploadsBinary(parseGateWorkflow(t, ok)))

	for _, c := range []struct{ name, old, repl string }{
		{"a bare build line", "      - uses: actions/attest", "      - run: ./scripts/build\n      - uses: actions/attest"},
		{"a build with another version", `VERSION="$TAG" ./scripts/build`, `VERSION="$GITHUB_SHA" ./scripts/build`},
		{"a build with no version", `GOOS="$goos" GOARCH="$goarch" VERSION="$TAG" ./scripts/build`, `GOOS="$goos" GOARCH="$goarch" ./scripts/build`},
		{"the version after the build", `VERSION="$TAG" ./scripts/build`, `./scripts/build VERSION="$TAG"`},
		{"a build through make", "      - uses: actions/attest", "      - run: make build\n      - uses: actions/attest"},
		{"a go build", "      - uses: actions/attest", "      - run: go build -o dist/rancher ./cmd/rancher\n      - uses: actions/attest"},
		{"a go install", "      - uses: actions/attest", "      - run: go install ./cmd/rancher\n      - uses: actions/attest"},
	} {
		require.Contains(t, ok, c.old, c.name)
		text := strings.Replace(ok, c.old, c.repl, 1)
		assert.NotEmpty(t, versionFromTagProblems(parseGateWorkflow(t, text)), c.name)
	}

	// Which workflows count as uploading a binary.
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"release-upsert", "jobs:\n  a:\n    steps:\n      - run: scripts/release-upsert dist/*\n", true},
		{"gh release", "jobs:\n  a:\n    steps:\n      - run: gh release create \"$TAG\" dist/*\n", true},
		{"attestation", "jobs:\n  a:\n    steps:\n      - uses: actions/attest-build-provenance@0\n", true},
		{"artifact with a build", "jobs:\n  a:\n    steps:\n      - run: ./scripts/build\n      - uses: actions/upload-artifact@0\n", true},
		{"artifact with no build", "jobs:\n  a:\n    steps:\n      - run: go test ./...\n      - uses: actions/upload-artifact@0\n", false},
		{"a build and no upload", "jobs:\n  a:\n    steps:\n      - run: go build ./...\n", false},
	} {
		assert.Equal(t, c.want, uploadsBinary(parseGateWorkflow(t, c.text)), c.name)
	}

	// A workflow that uploads nothing may build as it likes.
	assert.Empty(t, versionFromTagProblems(parseGateWorkflow(t, "jobs:\n  a:\n    steps:\n      - run: ./scripts/build\n      - run: go build ./...\n")))
}
