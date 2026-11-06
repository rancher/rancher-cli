package conformance

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	upsertScript = "scripts/release-upsert"
	upsertRepo   = "rancher/rancher-cli"

	// The stub answers the two gh calls the script may make and nothing
	// else: it logs every call, runs the script's own --jq expression over
	// a canned release list with the real jq, and exits 99 for any other
	// call shape: a changed call can never reach a real gh.
	upsertStubGH = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
if [ "${1:-}" = api ] && [ "$#" -eq 5 ] && [ "$3" = --paginate ] && [ "$4" = --jq ]; then
  case "$2" in
    repos/*/releases) ;;
    *) echo "stub gh: unknown api path $2" >&2; exit 99 ;;
  esac
  if [ -n "${STUB_LIST_FAIL:-}" ]; then
    echo "stub gh: HTTP 500" >&2
    exit 1
  fi
  exec jq -r "$5" "$STUB_JSON"
fi
if [ "${1:-}" = release ]; then
  case "${2:-}" in
    create|upload) exit 0 ;;
  esac
fi
echo "stub gh: unknown call: $*" >&2
exit 99
`
)

// upsertTools returns the one directory the script runs with as PATH: the
// stub gh and links to the real sh, awk, grep, wc and jq. No other
// binary, a real gh included, is reachable. It also returns the path of sh.
func upsertTools(t *testing.T) (dir, sh string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(posixOnlyReason)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set: a missing jq fails instead of skipping: %v", err)
		}
		t.Skipf("jq not installed: %v", err)
	}
	dir = t.TempDir()
	for _, name := range []string{"sh", "awk", "grep", "wc", "jq"} {
		target, err := exec.LookPath(name)
		require.NoError(t, err, name)
		if name == "sh" {
			sh = target
		}
		require.NoError(t, os.Symlink(target, filepath.Join(dir, name)))
	}
	testfs.WriteExecutable(t, filepath.Join(dir, "gh"), []byte(upsertStubGH))
	return dir, sh
}

type upsertResult struct {
	exit   int
	stderr string
	calls  [][]string
}

// releaseCalls returns the logged calls of gh release.
func (r upsertResult) releaseCalls() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "release" {
			out = append(out, c)
		}
	}
	return out
}

type upsertInput struct {
	tag       string
	repo      string
	repoUnset bool
	assets    []string
	listing   string
	listFails bool
}

func runUpsert(t *testing.T, in upsertInput) upsertResult {
	t.Helper()
	dir, sh := upsertTools(t)
	jsonFile := filepath.Join(t.TempDir(), "releases.json")
	listing := in.listing
	if listing == "" {
		listing = "[]"
	}
	require.NoError(t, os.WriteFile(jsonFile, []byte(listing), 0o644))
	logFile := filepath.Join(t.TempDir(), "calls.log")

	env := []string{"PATH=" + dir, "STUB_LOG=" + logFile, "STUB_JSON=" + jsonFile}
	if in.tag != "" {
		env = append(env, "TAG="+in.tag)
	}
	if !in.repoUnset {
		env = append(env, "GITHUB_REPOSITORY="+in.repo)
	}
	if in.listFails {
		env = append(env, "STUB_LIST_FAIL=1")
	}
	cmd := exec.CommandContext(t.Context(), sh, append([]string{upsertScript}, in.assets...)...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := upsertResult{stderr: stderr.String()}
	if err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "%v", err)
		res.exit = exitErr.ExitCode()
	}
	if data, err := os.ReadFile(logFile); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				res.calls = append(res.calls, strings.Fields(line))
			}
		}
	}
	return res
}

// flagValue returns the argument that follows flag in a call.
func flagValue(call []string, flag string) (string, bool) {
	i := slices.Index(call, flag)
	if i < 0 || i+1 >= len(call) {
		return "", false
	}
	return call[i+1], true
}

// hasRun reports whether want appears in call as one run of arguments.
func hasRun(call, want []string) bool {
	i := slices.Index(call, want[0])
	return i >= 0 && i+len(want) <= len(call) && slices.Equal(call[i:i+len(want)], want)
}

// TestReleaseUpsertCases asserts what scripts/release-upsert does for each
// state of the release list: it creates a draft when the tag has no
// release, attaches assets to the one draft, and refuses a published
// release, duplicates, a failed listing and missing input. The script runs
// under sh with a stub gh on a PATH of its own; the stub applies the
// script's real --jq expression with the real jq.
func TestReleaseUpsertCases(t *testing.T) {
	t.Parallel()
	assets := []string{"dist/a", "dist/b"}
	const (
		plainTag  = "v0.1.0"
		suffixTag = "v0.1.0-alpha.7"
	)
	draft := func(tag string) string { return `{"tag_name":"` + tag + `","draft":true,"id":1}` }
	published := func(tag string) string { return `{"tag_name":"` + tag + `","draft":false,"id":2}` }
	list := func(rows ...string) string { return "[" + strings.Join(rows, ",") + "]" }

	creates := []struct {
		name    string
		in      upsertInput
		prerel  bool
		wantTag string
	}{
		{"no release", upsertInput{tag: plainTag, listing: list()}, false, plainTag},
		{"no release for a suffix tag", upsertInput{tag: suffixTag, listing: list()}, true, suffixTag},
		{"another tag published", upsertInput{tag: plainTag, listing: list(published("v0.0.9"))}, false, plainTag},
		{"a tag that differs at a dot only", upsertInput{tag: "v0.1.0", listing: list(published("v0x1.0"))}, false, plainTag},
		{"the other way round", upsertInput{tag: "v0x1.0", listing: list(draft("v0.1.0"))}, false, "v0x1.0"},
		{"a published release of a longer tag", upsertInput{tag: plainTag, listing: list(published("v0.1.0-alpha.1"))}, false, plainTag},
	}
	for _, c := range creates {
		t.Run("creates the draft/"+c.name, func(t *testing.T) {
			t.Parallel()
			c.in.repo, c.in.assets = upsertRepo, assets
			res := runUpsert(t, c.in)
			require.Zero(t, res.exit, res.stderr)
			calls := res.releaseCalls()
			require.Len(t, calls, 1, "exactly one gh release call")
			call := calls[0]
			require.Equal(t, []string{"release", "create", c.wantTag}, call[:3])
			assert.Contains(t, call, "--draft")
			assert.Contains(t, call, "--verify-tag")
			assert.Equal(t, c.prerel, slices.Contains(call, "--prerelease"))
			repo, ok := flagValue(call, "--repo")
			assert.True(t, ok)
			assert.Equal(t, upsertRepo, repo)
			title, ok := flagValue(call, "--title")
			assert.True(t, ok)
			assert.Equal(t, c.wantTag, title)
			assert.True(t, hasRun(call, assets), "the assets %v appear in order in %v", assets, call)
			assert.NotContains(t, call, "--clobber")
		})
	}

	uploads := []struct {
		name string
		in   upsertInput
	}{
		{"one draft", upsertInput{tag: plainTag, listing: list(draft(plainTag))}},
		{"one draft of a suffix tag", upsertInput{tag: suffixTag, listing: list(draft(suffixTag))}},
		{"another tag published beside the draft", upsertInput{tag: plainTag, listing: list(published("v0.0.9"), draft(plainTag))}},
	}
	for _, c := range uploads {
		t.Run("attaches assets to the draft/"+c.name, func(t *testing.T) {
			t.Parallel()
			c.in.repo, c.in.assets = upsertRepo, assets
			res := runUpsert(t, c.in)
			require.Zero(t, res.exit, res.stderr)
			calls := res.releaseCalls()
			require.Len(t, calls, 1, "exactly one gh release call")
			call := calls[0]
			require.Equal(t, []string{"release", "upload", c.in.tag}, call[:3])
			assert.Contains(t, call, "--clobber")
			repo, ok := flagValue(call, "--repo")
			assert.True(t, ok)
			assert.Equal(t, upsertRepo, repo)
			assert.True(t, hasRun(call, assets), "the assets %v appear in order in %v", assets, call)
		})
	}

	refusals := []struct {
		name       string
		in         upsertInput
		wantStderr string
	}{
		{"one published release", upsertInput{tag: plainTag, listing: list(published(plainTag))}, "published"},
		{"one published release of a suffix tag", upsertInput{tag: suffixTag, listing: list(published(suffixTag))}, "published"},
		{"a published and a draft row for one tag", upsertInput{tag: plainTag, listing: list(published(plainTag), draft(plainTag))}, "releases already exist"},
		{"two drafts", upsertInput{tag: plainTag, listing: list(draft(plainTag), draft(plainTag))}, "releases already exist"},
		{"the listing fails", upsertInput{tag: plainTag, listing: list(), listFails: true}, "failed to list releases"},
	}
	for _, c := range refusals {
		t.Run("refuses/"+c.name, func(t *testing.T) {
			t.Parallel()
			c.in.repo, c.in.assets = upsertRepo, assets
			res := runUpsert(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, c.wantStderr)
			assert.Empty(t, res.releaseCalls(), "no gh release call")
			if c.wantStderr == "published" {
				assert.Contains(t, res.stderr, "takes no new assets")
				assert.Contains(t, res.stderr, "next version")
			}
		})
	}

	missing := []struct {
		name       string
		in         upsertInput
		wantStderr string
	}{
		{"TAG", upsertInput{repo: upsertRepo, assets: assets}, "TAG is not set"},
		{"GITHUB_REPOSITORY unset", upsertInput{tag: plainTag, repoUnset: true, assets: assets}, "GITHUB_REPOSITORY is not set"},
		{"GITHUB_REPOSITORY empty", upsertInput{tag: plainTag, repo: "", assets: assets}, "GITHUB_REPOSITORY is not set"},
		{"assets", upsertInput{tag: plainTag, repo: upsertRepo}, "no asset files given"},
	}
	for _, c := range missing {
		t.Run("refuses missing input/"+c.name, func(t *testing.T) {
			t.Parallel()
			res := runUpsert(t, c.in)
			assert.Equal(t, 1, res.exit)
			assert.Contains(t, res.stderr, c.wantStderr)
			assert.Empty(t, res.calls, "no gh call at all")
		})
	}
}
