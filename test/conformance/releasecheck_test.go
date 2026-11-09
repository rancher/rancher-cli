package conformance

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	checkRepo = "rancher/rancher-cli"
	checkTag  = "v0.1.0-beta.2"
	checkSHA  = "0123456789abcdef0123456789abcdef01234567"
	otherSHA  = "fedcba9876543210fedcba9876543210fedcba98"

	// The stub answers the gh calls the script may make and nothing else: it
	// logs every call, answers the run list and the artifact list from
	// canned JSON by running the script's own --jq expression with the real
	// jq (so the script must filter every field itself: the stub applies no
	// query), prints the canned archive for a download, and exits 99 for any
	// other call shape, so a changed call can never reach a real gh.
	checkStubGH = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
if [ "${1:-}" = api ] && [ "$#" -eq 5 ] && [ "$3" = --paginate ] && [ "$4" = --jq ]; then
  case "$2" in
    repos/*/actions/workflows/release.yaml/runs\?*)
      if [ -n "${STUB_RUNS_FAIL:-}" ]; then echo "stub gh: HTTP 500" >&2; exit 1; fi
      exec jq -rc "$5" "$STUB_DIR/runs.json" ;;
    repos/*/actions/runs/*/artifacts\?*)
      if [ -n "${STUB_ARTIFACTS_FAIL:-}" ]; then echo "stub gh: HTTP 500" >&2; exit 1; fi
      run="${2#repos/*/actions/runs/}"
      run="${run%%/*}"
      exec jq -rc "$5" "$STUB_DIR/artifacts-$run.json" ;;
  esac
fi
if [ "${1:-}" = api ] && [ "$#" -eq 2 ]; then
  case "$2" in
    repos/*/actions/artifacts/*/zip)
      if [ -n "${STUB_DOWNLOAD_FAIL:-}" ]; then echo "stub gh: HTTP 410" >&2; exit 1; fi
      id="${2#repos/*/actions/artifacts/}"
      id="${id%/zip}"
      exec cat "$STUB_DIR/zip-$id.zip" ;;
  esac
fi
echo "stub gh: unknown call: $*" >&2
exit 99
`
)

type checkRun struct {
	ID         int    `json:"id"`
	CreatedAt  string `json:"created_at"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	Conclusion string `json:"conclusion"`
}

type checkArtifact struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
	// Sums is the SHA256SUMS the archive holds; the field is not part of the
	// answer.
	Sums string `json:"-"`
	// NoSums leaves SHA256SUMS out of the archive.
	NoSums bool `json:"-"`
	// NotZip makes the download something other than an archive.
	NotZip bool `json:"-"`
}

type checkInput struct {
	tag, sha      string
	runs          []checkRun
	artifacts     map[int][]checkArtifact
	dist          string
	noDist        bool
	repoUnset     bool
	runsFail      bool
	artifactsFail bool
	downloadFails bool
}

type checkResult struct {
	exit   int
	stdout string
	stderr string
	calls  []string
}

// goodRun is a successful manual run of release.yaml on main for checkSHA.
func goodRun(id int, created string) checkRun {
	return checkRun{ID: id, CreatedAt: created, Path: ".github/workflows/release.yaml", Event: "workflow_dispatch", HeadSHA: checkSHA, HeadBranch: "main", Conclusion: "success"}
}

func sumsArtifact(id int, tag, sums string) checkArtifact {
	return checkArtifact{ID: id, Name: "release-rehearsal-sums-" + tag, Sums: sums}
}

const (
	sumsA = "aaaa  rancher-linux-amd64\nbbbb  rancher-windows-amd64.exe\n"
	sumsB = "aaaa  rancher-linux-amd64\ncccc  rancher-windows-amd64.exe\n"
)

// checkTools returns the one directory the script runs with as PATH: the
// stub gh and links to the real tools the script and check-release-tag use.
func checkTools(t *testing.T) (dir, bash string) {
	t.Helper()
	testfs.SkipOnWindows(t, posixOnlyReason)
	dir = t.TempDir()
	for _, name := range []string{"bash", "sh", "jq", "unzip", "cmp", "diff", "mktemp", "rm", "grep", "cat"} {
		target, err := exec.LookPath(name)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("CI is set: a missing %s fails instead of skipping: %v", name, err)
			}
			t.Skipf("%s not installed: %v", name, err)
		}
		if name == "bash" {
			bash = target
		}
		testfs.Symlink(t, target, filepath.Join(dir, name))
	}
	testfs.WriteExecutable(t, filepath.Join(dir, "gh"), []byte(checkStubGH))
	return dir, bash
}

func zipOf(t *testing.T, a checkArtifact) []byte {
	t.Helper()
	if a.NotZip {
		return []byte("this is not an archive")
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	name := "SHA256SUMS"
	if a.NoSums {
		name = "other"
	}
	f, err := w.Create(name)
	require.NoError(t, err)
	_, err = f.Write([]byte(a.Sums))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func runCheck(t *testing.T, in checkInput) checkResult {
	t.Helper()
	tools, bash := checkTools(t)
	script, err := filepath.Abs(rehearsalCheckScript)
	require.NoError(t, err)

	stubDir := t.TempDir()
	writeJSON := func(name string, v any) {
		data, err := json.Marshal(v)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(stubDir, name), data, 0o644))
	}
	runs := in.runs
	if runs == nil {
		runs = []checkRun{}
	}
	writeJSON("runs.json", map[string]any{"workflow_runs": runs})
	for run, artifacts := range in.artifacts {
		if artifacts == nil {
			artifacts = []checkArtifact{}
		}
		writeJSON(fmt.Sprintf("artifacts-%d.json", run), map[string]any{"artifacts": artifacts})
		for _, a := range artifacts {
			require.NoError(t, os.WriteFile(filepath.Join(stubDir, fmt.Sprintf("zip-%d.zip", a.ID)), zipOf(t, a), 0o644))
		}
	}
	work := t.TempDir()
	if !in.noDist {
		require.NoError(t, os.Mkdir(filepath.Join(work, "dist"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, "dist", "SHA256SUMS"), []byte(in.dist), 0o644))
	}
	logFile := filepath.Join(t.TempDir(), "calls.log")

	env := []string{"PATH=" + tools, "STUB_LOG=" + logFile, "STUB_DIR=" + stubDir}
	if !in.repoUnset {
		env = append(env, "GITHUB_REPOSITORY="+checkRepo)
	}
	for flag, on := range map[string]bool{"STUB_RUNS_FAIL": in.runsFail, "STUB_ARTIFACTS_FAIL": in.artifactsFail, "STUB_DOWNLOAD_FAIL": in.downloadFails} {
		if on {
			env = append(env, flag+"=1")
		}
	}
	tag, sha := in.tag, in.sha
	if tag == "" {
		tag = checkTag
	}
	if sha == "" {
		sha = checkSHA
	}
	cmd := exec.CommandContext(t.Context(), bash, script, tag, sha)
	cmd.Dir = work
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := checkResult{}
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "%v", err)
		res.exit = exitErr.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if data, err := os.ReadFile(logFile); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				res.calls = append(res.calls, line)
			}
		}
	}
	return res
}

// downloads returns the artifact ids the script downloaded.
func (r checkResult) downloads() []string {
	var out []string
	for _, c := range r.calls {
		if rest, ok := strings.CutPrefix(c, "api repos/"+checkRepo+"/actions/artifacts/"); ok {
			id, _, _ := strings.Cut(rest, "/")
			out = append(out, id)
		}
	}
	return out
}

func startCommand(tag string) string {
	return "gh workflow run release.yaml --ref main -f tag=" + tag
}

// TestReleaseRehearsalCheckCases asserts what scripts/release-rehearsal-check
// does for each state of the run list and the artifacts: it passes only when
// the newest successful manual run of release.yaml on main for the commit
// holds the sums artifact of the tag and its SHA256SUMS equals the file
// built, byte for byte. It fails with a plain line that names the case, the
// start command where a rehearsal is missing, and the differing lines. The
// script runs with bash and a stub gh on a PATH of its own; the stub applies
// no query, so every row of another branch, workflow, event, commit or
// conclusion tests the script's own filter.
func TestReleaseRehearsalCheckCases(t *testing.T) {
	t.Parallel()
	good := func(id int, created string) checkRun { return goodRun(id, created) }
	with := func(r checkRun, change func(*checkRun)) checkRun { change(&r); return r }
	const older, newer = "2026-11-01T10:00:00Z", "2026-11-02T10:00:00Z"

	passes := []struct {
		name string
		in   checkInput
		ids  []string
	}{
		{"equal sums", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}, []string{"101"}},
		{"the newest of two runs wins", checkInput{
			runs: []checkRun{good(11, older), good(12, newer)},
			artifacts: map[int][]checkArtifact{
				11: {sumsArtifact(101, checkTag, sumsB)},
				12: {sumsArtifact(102, checkTag, sumsA)},
			},
			dist: sumsA,
		}, []string{"102"}},
		{"the newest of two runs wins when it is listed first", checkInput{
			runs: []checkRun{good(12, newer), good(11, older)},
			artifacts: map[int][]checkArtifact{
				11: {sumsArtifact(101, checkTag, sumsB)},
				12: {sumsArtifact(102, checkTag, sumsA)},
			},
			dist: sumsA,
		}, []string{"102"}},
		{"a failed newer run with other sums is ignored", checkInput{
			runs: []checkRun{
				with(good(12, newer), func(r *checkRun) { r.Conclusion = "failure" }),
				good(11, older),
			},
			artifacts: map[int][]checkArtifact{
				11: {sumsArtifact(101, checkTag, sumsA)},
				12: {sumsArtifact(102, checkTag, sumsB)},
			},
			dist: sumsA,
		}, []string{"101"}},
		{"an artifact of another tag beside the one of the tag", checkInput{
			runs: []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {
				sumsArtifact(100, "v0.1.0-beta.9", sumsB),
				sumsArtifact(101, checkTag, sumsA),
			}},
			dist: sumsA,
		}, []string{"101"}},
		{"the artifact of the newest run expired and an older one is kept", checkInput{
			runs: []checkRun{good(11, older), good(12, newer)},
			artifacts: map[int][]checkArtifact{
				11: {sumsArtifact(101, checkTag, sumsA)},
				12: {with2(sumsArtifact(102, checkTag, sumsA), func(a *checkArtifact) { a.Expired = true })},
			},
			dist: sumsA,
		}, []string{"101"}},
	}
	for _, c := range passes {
		t.Run("passes/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			require.Zero(t, res.exit, res.stderr)
			assert.Contains(t, res.stdout, "equals the rehearsal run")
			assert.Equal(t, c.ids, res.downloads())
			require.NotEmpty(t, res.calls)
			// The run list asks for the filters; the script also re-checks
			// every field of each run.
			first := res.calls[0]
			pathAndQuery := strings.Fields(first)[1]
			path, query, ok := strings.Cut(pathAndQuery, "?")
			require.True(t, ok, first)
			assert.Equal(t, "repos/"+checkRepo+"/actions/workflows/release.yaml/runs", path)
			values, err := url.ParseQuery(query)
			require.NoError(t, err)
			assert.Equal(t, "workflow_dispatch", values.Get("event"))
			assert.Equal(t, checkSHA, values.Get("head_sha"))
			assert.Equal(t, "success", values.Get("status"))
			assert.Equal(t, "main", values.Get("branch"))
		})
	}

	noRun := []struct {
		name string
		in   checkInput
	}{
		{"no run at all", checkInput{dist: sumsA}},
		{"a run of another commit", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.HeadSHA = otherSHA })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a run of another branch", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.HeadBranch = "feature" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a run of another workflow", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Path = ".github/workflows/ci.yaml" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a workflow whose name ends in release.yaml", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Path = ".github/workflows/prerelease.yaml" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a run started by a push", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Event = "push" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a failed run with an artifact", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Conclusion = "failure" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a cancelled run with an artifact", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Conclusion = "cancelled" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
		{"a run still going", checkInput{
			runs:      []checkRun{with(good(11, newer), func(r *checkRun) { r.Conclusion = "" })},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA,
		}},
	}
	for _, c := range noRun {
		t.Run("fails with no run/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, "no successful rehearsal run of release.yaml on main for commit "+checkSHA)
			assert.Contains(t, res.stderr, startCommand(checkTag))
			assert.Empty(t, res.downloads())
		})
	}

	noArtifact := []struct {
		name string
		in   checkInput
		want string
	}{
		{"a successful run without the artifact", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {}},
			dist:      sumsA,
		}, "no rehearsal of " + checkTag},
		{"an artifact of another tag only", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, "v0.1.0-beta.9", sumsA)}},
			dist:      sumsA,
		}, "no rehearsal of " + checkTag},
		{"the full artifact only", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {{ID: 101, Name: "release-rehearsal-" + checkTag, Sums: sumsA}}},
			dist:      sumsA,
		}, "no rehearsal of " + checkTag},
		{"an expired artifact", checkInput{
			runs: []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {
				with2(sumsArtifact(101, checkTag, sumsA), func(a *checkArtifact) { a.Expired = true }),
			}},
			dist: sumsA,
		}, "has expired"},
	}
	for _, c := range noArtifact {
		t.Run("fails with no artifact/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, c.want)
			assert.Contains(t, res.stderr, startCommand(checkTag))
			assert.Empty(t, res.downloads())
		})
	}

	differs := []struct {
		name      string
		in        checkInput
		wantLines []string
	}{
		{"the same names with one different hash", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsB)}},
			dist:      sumsA,
		}, []string{"< cccc  rancher-windows-amd64.exe", "> bbbb  rancher-windows-amd64.exe"}},
		{"a file only in the build", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      sumsA + "dddd  install.sh\n",
		}, []string{"> dddd  install.sh"}},
		{"a file only in the rehearsal", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA+"dddd  install.sh\n")}},
			dist:      sumsA,
		}, []string{"< dddd  install.sh"}},
		{"the same lines in another order", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, "bbbb  rancher-windows-amd64.exe\naaaa  rancher-linux-amd64\n")}},
			dist:      sumsA,
		}, nil},
		{"a missing final newline", checkInput{
			runs:      []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			dist:      strings.TrimSuffix(sumsA, "\n"),
		}, nil},
		{"the newest of two runs differs and an older one is equal", checkInput{
			runs: []checkRun{good(11, older), good(12, newer)},
			artifacts: map[int][]checkArtifact{
				11: {sumsArtifact(101, checkTag, sumsA)},
				12: {sumsArtifact(102, checkTag, sumsB)},
			},
			dist: sumsA,
		}, []string{"> bbbb  rancher-windows-amd64.exe"}},
	}
	for _, c := range differs {
		t.Run("fails with other sums/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, "SHA256SUMS differs from the rehearsal run")
			for _, line := range c.wantLines {
				assert.Contains(t, res.stderr, line)
			}
			assert.NotContains(t, res.stdout, "equals")
		})
	}

	failures := []struct {
		name string
		in   checkInput
		want string
	}{
		{"the run list fails", checkInput{runsFail: true, dist: sumsA}, "could not list the runs of release.yaml"},
		{"the artifact list fails", checkInput{
			runs:          []checkRun{good(11, newer)},
			artifactsFail: true,
			dist:          sumsA,
		}, "could not list the artifacts of run 11"},
		{"the download fails", checkInput{
			runs:          []checkRun{good(11, newer)},
			artifacts:     map[int][]checkArtifact{11: {sumsArtifact(101, checkTag, sumsA)}},
			downloadFails: true,
			dist:          sumsA,
		}, "could not download the artifact"},
		{"the archive holds no SHA256SUMS", checkInput{
			runs: []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {
				with2(sumsArtifact(101, checkTag, sumsA), func(a *checkArtifact) { a.NoSums = true }),
			}},
			dist: sumsA,
		}, "could not read SHA256SUMS from the rehearsal artifact release-rehearsal-sums-v0.1.0-beta.2 of run 11"},
		{"the download is not an archive", checkInput{
			runs: []checkRun{good(11, newer)},
			artifacts: map[int][]checkArtifact{11: {
				with2(sumsArtifact(101, checkTag, sumsA), func(a *checkArtifact) { a.NotZip = true }),
			}},
			dist: sumsA,
		}, "could not read SHA256SUMS from the rehearsal artifact release-rehearsal-sums-v0.1.0-beta.2 of run 11"},
	}
	for _, c := range failures {
		t.Run("fails when gh fails/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, c.want)
			assert.NotContains(t, res.stderr, "no successful rehearsal run", "a failed call is not read as no rehearsal")
			assert.NotContains(t, res.stderr, "no rehearsal of")
		})
	}

	refusals := []struct {
		name string
		in   checkInput
		exit int
		want string
	}{
		{"a short sha", checkInput{sha: "0123456", dist: sumsA}, 2, "not a commit sha"},
		{"an uppercase sha", checkInput{sha: strings.ToUpper(checkSHA), dist: sumsA}, 2, "not a commit sha"},
		{"a sha of 41 digits", checkInput{sha: checkSHA + "0", dist: sumsA}, 2, "not a commit sha"},
		{"a tag that is not a release tag", checkInput{tag: "v1", dist: sumsA}, 1, "not a valid release tag"},
		{"GITHUB_REPOSITORY unset", checkInput{repoUnset: true, dist: sumsA}, 1, "GITHUB_REPOSITORY is not set"},
		{"no dist/SHA256SUMS", checkInput{noDist: true}, 1, "dist/SHA256SUMS does not exist"},
	}
	for _, c := range refusals {
		t.Run("refuses/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runCheck(t, c.in)
			assert.Equal(t, c.exit, res.exit)
			assert.Contains(t, res.stderr, c.want)
			assert.Empty(t, res.calls, "no gh call at all")
		})
	}
}

func with2(a checkArtifact, change func(*checkArtifact)) checkArtifact {
	change(&a)
	return a
}
