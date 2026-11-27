package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
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
	mcpKindsScript = "scripts/mcp-kinds"
	mcpKindsCommit = "0123456789abcdef0123456789abcdef01234567"
	mcpKindsNewer  = "fedcba9876543210fedcba9876543210fedcba98"
)

// mcpKindsSource is a stand-in for pkg/converter/grv.go of rancher-ai-mcp:
// constants, a concatenation of constants, keys in no order and a key that is
// a constant.
const mcpKindsSource = `package converter

import "k8s.io/apimachinery/pkg/runtime/schema"

const (
	Prefix      = "capi"
	Group       = "cluster.x-k8s.io"
	MachineKind = Prefix + "machine"
)

var K8sKindsToGVRs = map[string]schema.GroupVersionResource{
	"zebra":     {Group: "", Version: "v1", Resource: "zebras"},
	MachineKind: {Group: Group, Version: "v1beta2", Resource: "machines"},
	"alpha":     {Group: "a.example", Version: "v1", Resource: "alphas"},
}
`

type mcpKindsResult struct {
	stdout, stderr string
	exit           int
}

// mcpKindsRun runs scripts/mcp-kinds with the hooks that stand in for the
// download, in a PATH whose git and curl fail and record the call. A call to
// either fails the test.
func mcpKindsRun(t *testing.T, env ...string) mcpKindsResult {
	t.Helper()
	return mcpKindsRunIn(t, "", env...)
}

// mcpKindsRunIn is mcpKindsRun started in dir, the repository root when dir is
// empty.
func mcpKindsRunIn(t *testing.T, dir string, env ...string) mcpKindsResult {
	t.Helper()
	return mcpKindsRunGit(t, dir, "", env...)
}

// mcpKindsRunGit is mcpKindsRunIn with gitBody, a shell script, as the git the
// script finds. With an empty gitBody git fails and records the call, and a
// call to git or curl fails the test. With a gitBody a call to git is expected
// and only a call to curl fails the test.
func mcpKindsRunGit(t *testing.T, dir, gitBody string, env ...string) mcpKindsResult {
	t.Helper()
	stubs := t.TempDir()
	calls := filepath.Join(stubs, "calls")
	for _, name := range []string{"git", "curl"} {
		stub := "#!/bin/sh\necho " + name + " >> \"$MCP_KINDS_TEST_CALLS\"\nexit 99\n"
		if name == "git" && gitBody != "" {
			stub = gitBody
		}
		testfs.WriteExecutable(t, filepath.Join(stubs, name), []byte(stub))
	}
	script := mcpKindsScript
	if dir != "" {
		abs, err := filepath.Abs(mcpKindsScript)
		require.NoError(t, err)
		script = abs
	}
	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Dir = dir
	cmd.Env = append(testfs.EnvWithoutGit(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MCP_KINDS_TEST_CALLS="+calls,
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"MCP_KINDS_UPDATE=",
		"MCP_KINDS_SOURCE_FILE=",
		"MCP_KINDS_COMMIT=",
		"MCP_KINDS_FIXTURE=",
	)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := mcpKindsResult{}
	if err := cmd.Run(); err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "%v", err)
		res.exit = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if gitBody == "" {
		assert.NoFileExists(t, calls, "the script called git or curl")
	} else {
		if data, err := os.ReadFile(calls); err == nil {
			assert.NotContains(t, string(data), "curl", "the script called curl")
		}
	}
	return res
}

// mcpKindsSetup writes a source file and a fixture path in a temporary
// directory and returns the environment that points the script at them.
func mcpKindsSetup(t *testing.T, source, commit string) (fixture string, env []string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "grv.go")
	require.NoError(t, os.WriteFile(src, []byte(source), 0o644))
	fixture = filepath.Join(dir, "mcp-kinds.json")
	return fixture, []string{"MCP_KINDS_SOURCE_FILE=" + src, "MCP_KINDS_COMMIT=" + commit, "MCP_KINDS_FIXTURE=" + fixture}
}

type mcpKindsFixtureFile struct {
	Commit string `json:"commit"`
	Kinds  map[string]struct {
		Group    string `json:"group"`
		Version  string `json:"version"`
		Resource string `json:"resource"`
	} `json:"kinds"`
}

// mcpKindsRecord runs the script in update mode on source and returns the
// fixture it wrote.
func mcpKindsRecord(t *testing.T, source string) (fixture string) {
	t.Helper()
	fixture, env := mcpKindsSetup(t, source, mcpKindsCommit)
	res := mcpKindsRun(t, append(env, "MCP_KINDS_UPDATE=1")...)
	require.Equal(t, 0, res.exit, res.stderr)
	return fixture
}

// mcpKindsCheck runs the script in compare mode against the fixture, with
// source as the upstream file and commit as its commit.
func mcpKindsCheck(t *testing.T, fixture, source, commit string) mcpKindsResult {
	t.Helper()
	src := filepath.Join(t.TempDir(), "grv.go")
	require.NoError(t, os.WriteFile(src, []byte(source), 0o644))
	return mcpKindsRun(t, "MCP_KINDS_SOURCE_FILE="+src, "MCP_KINDS_COMMIT="+commit, "MCP_KINDS_FIXTURE="+fixture)
}

// TestMCPKindsScriptWritesTheFixture asserts update mode writes the commit and
// every key with its group, version and resource, the keys sorted, the indent
// two spaces and a final newline.
func TestMCPKindsScriptWritesTheFixture(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	fixture := mcpKindsRecord(t, mcpKindsSource)
	data, err := os.ReadFile(fixture)
	require.NoError(t, err)
	var got mcpKindsFixtureFile
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, mcpKindsCommit, got.Commit)
	require.Len(t, got.Kinds, 3)
	assert.Equal(t, "cluster.x-k8s.io", got.Kinds["capimachine"].Group)
	assert.Equal(t, "v1beta2", got.Kinds["capimachine"].Version)
	assert.Equal(t, "machines", got.Kinds["capimachine"].Resource)
	assert.Equal(t, "", got.Kinds["zebra"].Group)
	assert.Equal(t, "zebras", got.Kinds["zebra"].Resource)

	dec := json.NewDecoder(bytes.NewReader(data))
	var keys []string
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' {
				depth++
			} else {
				depth--
			}
			continue
		}
		if s, ok := tok.(string); ok && depth == 2 {
			keys = append(keys, s)
		}
	}
	assert.Equal(t, []string{"alpha", "capimachine", "zebra"}, keys)

	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, bytes.TrimSpace(data), "", "  "))
	assert.Equal(t, indented.String()+"\n", string(data), "the file is indented with two spaces and ends with one newline")
}

// TestMCPKindsScriptComparesTheFixture asserts compare mode passes on the map
// it recorded, passes on a newer commit with the same map, and fails and names
// each added, removed and changed key.
func TestMCPKindsScriptComparesTheFixture(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	fixture := mcpKindsRecord(t, mcpKindsSource)

	res := mcpKindsCheck(t, fixture, mcpKindsSource, mcpKindsCommit)
	assert.Equal(t, 0, res.exit, res.stderr)

	res = mcpKindsCheck(t, fixture, mcpKindsSource, mcpKindsNewer)
	assert.Equal(t, 0, res.exit, "a newer commit with the same map passes: %s", res.stderr)
	assert.Contains(t, res.stdout, mcpKindsNewer)

	added := strings.Replace(mcpKindsSource, "}\n", "\t\"beta\": {Group: \"b.example\", Version: \"v1\", Resource: \"betas\"},\n}\n", 1)
	require.NotEqual(t, mcpKindsSource, added)
	res = mcpKindsCheck(t, fixture, added, mcpKindsNewer)
	assert.Equal(t, 1, res.exit)
	assert.Contains(t, res.stdout, "added in rancher-ai-mcp: beta: ")

	removed := strings.Replace(mcpKindsSource, "\t\"alpha\":     {Group: \"a.example\", Version: \"v1\", Resource: \"alphas\"},\n", "", 1)
	require.NotEqual(t, mcpKindsSource, removed)
	res = mcpKindsCheck(t, fixture, removed, mcpKindsNewer)
	assert.Equal(t, 1, res.exit)
	assert.Contains(t, res.stdout, "removed from rancher-ai-mcp: alpha: ")

	for field, change := range map[string]struct{ key, old, new string }{
		"group":    {"alpha", `Group: "a.example"`, `Group: "other.example"`},
		"version":  {"capimachine", `Version: "v1beta2"`, `Version: "v1beta3"`},
		"resource": {"zebra", `Resource: "zebras"`, `Resource: "zebra"`},
	} {
		changed := strings.Replace(mcpKindsSource, change.old, change.new, 1)
		require.NotEqual(t, mcpKindsSource, changed, field)
		res = mcpKindsCheck(t, fixture, changed, mcpKindsNewer)
		assert.Equal(t, 1, res.exit, field)
		assert.Contains(t, res.stdout, "changed in rancher-ai-mcp: "+change.key+": ", field)
	}
}

// TestMCPKindsScriptReadsRelativePathsFromTheCallersDirectory asserts a
// relative MCP_KINDS_SOURCE_FILE and MCP_KINDS_FIXTURE name files in the
// directory the script is started in, not in the repository root.
func TestMCPKindsScriptReadsRelativePathsFromTheCallersDirectory(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "grv.go"), []byte(mcpKindsSource), 0o644))
	env := []string{"MCP_KINDS_SOURCE_FILE=grv.go", "MCP_KINDS_COMMIT=" + mcpKindsCommit, "MCP_KINDS_FIXTURE=out.json"}

	res := mcpKindsRunIn(t, dir, append(env, "MCP_KINDS_UPDATE=1")...)
	require.Equal(t, 0, res.exit, res.stderr)
	assert.FileExists(t, filepath.Join(dir, "out.json"))
	assert.NoFileExists(t, "out.json", "the fixture is written in the repository root")

	res = mcpKindsRunIn(t, dir, env...)
	assert.Equal(t, 0, res.exit, res.stderr)
}

// TestMCPKindsScriptRefusesWhatItCannotRead asserts a source with no map, a
// constant the file does not declare, an unsupported expression, a missing
// fixture and a hook set alone each stop the script with one plain error.
func TestMCPKindsScriptRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)

	for _, c := range []struct{ name, source, want string }{
		{"no map", "package converter\n\nvar Other = 1\n", "has no variable K8sKindsToGVRs"},
		{"unknown constant", strings.Replace(mcpKindsSource, `"zebra":     {`, `Missing: {`, 1), "unknown constant Missing"},
		{"unsupported expression", strings.Replace(mcpKindsSource, `Resource: "zebras"`, `Resource: f()`, 1), "unsupported expression"},
		{"not go", "this is not Go", "cannot read"},
	} {
		_, env := mcpKindsSetup(t, c.source, mcpKindsCommit)
		res := mcpKindsRun(t, append(env, "MCP_KINDS_UPDATE=1")...)
		assert.Equal(t, 1, res.exit, c.name)
		assert.Contains(t, res.stderr, "error: ", c.name)
		assert.Contains(t, res.stderr, c.want, c.name)
		assert.Equal(t, 1, strings.Count(strings.TrimSpace(res.stderr), "\n")+1, "%s: want one line on stderr, got %q", c.name, res.stderr)
	}

	fixture, env := mcpKindsSetup(t, mcpKindsSource, mcpKindsCommit)
	res := mcpKindsRun(t, env...)
	assert.Equal(t, 1, res.exit, "a missing fixture")
	assert.Contains(t, res.stderr, "cannot read the fixture")
	assert.NoFileExists(t, fixture)

	_, env = mcpKindsSetup(t, mcpKindsSource, "ABC")
	res = mcpKindsRun(t, env...)
	assert.Equal(t, 1, res.exit, "a commit that is not 40 lowercase hex digits")
	assert.Contains(t, res.stderr, "not 40 lowercase hex digits")

	for _, c := range []struct{ name, want string }{
		{"MCP_KINDS_SOURCE_FILE=" + filepath.Join(t.TempDir(), "grv.go"), "MCP_KINDS_SOURCE_FILE is set without MCP_KINDS_COMMIT"},
		{"MCP_KINDS_COMMIT=" + mcpKindsCommit, "MCP_KINDS_COMMIT is set without MCP_KINDS_SOURCE_FILE"},
	} {
		for _, update := range []string{"", "1"} {
			res := mcpKindsRun(t, c.name, "MCP_KINDS_UPDATE="+update)
			assert.Equal(t, 1, res.exit, c.name)
			assert.Contains(t, res.stderr, c.want, "%s with update %q", c.name, update)
		}
	}
}

// TestMCPKindsScriptReportsAnEmptyBranchAnswer asserts a git ls-remote that
// succeeds and prints nothing, as it does when the main branch does not exist,
// stops the script at once with the message that the answer held no commit,
// not with the message for a failed call.
func TestMCPKindsScriptReportsAnEmptyBranchAnswer(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	fixture := filepath.Join(t.TempDir(), "mcp-kinds.json")
	res := mcpKindsRunGit(t, "", "#!/bin/sh\nexit 0\n", "MCP_KINDS_FIXTURE="+fixture)
	assert.Equal(t, 1, res.exit)
	assert.Contains(t, res.stderr, "printed no commit")
	assert.NotContains(t, res.stderr, "cannot read the main branch")
}

// TestMCPKindsScriptMode asserts the script and its update twin are
// executable, because the Makefile runs ./scripts/<target>.
func TestMCPKindsScriptMode(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	for _, script := range []string{mcpKindsScript, mcpKindsScript + "-update"} {
		info, err := os.Stat(filepath.FromSlash(script))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&0o111, "%s must have an execute bit", script)
	}
}
