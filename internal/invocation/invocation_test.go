package invocation

import (
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookup returns a getenv backed by m, never by the test process's own
// environment: an agent running the tests sets AI_AGENT and CLAUDECODE.
func lookup(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

func detect(version string, env map[string]string) Context {
	return Detect(version, lookup(env))
}

// TestDetectAgentRows asserts every agent row, the vendor signals' exact
// values, and the id each AI_AGENT form normalizes to.
func TestDetectAgentRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no signal", nil, ""},
		{"row a child session", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1"}, "claude-code"},
		{"row a child session value must be 1", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "0"}, ""},
		{"row a claudecode with claude AI_AGENT", map[string]string{"CLAUDECODE": "1", "AI_AGENT": "claude-code_2-1-308_agent"}, "claude-code"},
		{"row a claudecode with upper case AI_AGENT", map[string]string{"CLAUDECODE": "1", "AI_AGENT": "Claude-Code"}, "claude-code"},
		{"claudecode alone", map[string]string{"CLAUDECODE": "1"}, ""},
		{"claudecode with another AI_AGENT", map[string]string{"CLAUDECODE": "1", "AI_AGENT": "crush"}, "crush"},
		{"row b codex thread", map[string]string{"CODEX_THREAD_ID": "019a"}, "codex"},
		{"row b codex ci", map[string]string{"CODEX_CI": "1"}, "codex"},
		{"row b codex ci value must be 1", map[string]string{"CODEX_CI": "true"}, ""},
		{"row c gemini", map[string]string{"GEMINI_CLI": "1"}, "gemini-cli"},
		{"row d copilot", map[string]string{"COPILOT_CLI": "1"}, "copilot-cli"},
		{"row e opencode", map[string]string{"OPENCODE": "1"}, "opencode"},
		{"row f cline", map[string]string{"CLINE_ACTIVE": "true"}, "cline"},
		{"row f cline value must be true", map[string]string{"CLINE_ACTIVE": "1"}, ""},
		{"row g roo", map[string]string{"ROO_ACTIVE": "true"}, "roo-code"},
		{"row h claude form", map[string]string{"AI_AGENT": "claude-code_2-1-308_agent"}, "claude-code"},
		{"row h copilot vscode", map[string]string{"AI_AGENT": "github_copilot_vscode_agent"}, "copilot-vscode"},
		{"row h copilot cloud", map[string]string{"AI_AGENT": "github_copilot_cloud_agent"}, "copilot-cloud"},
		{"row h crush", map[string]string{"AI_AGENT": "crush"}, "crush"},
		{"row h pi", map[string]string{"AI_AGENT": "pi"}, "pi"},
		{"row h cut at at sign", map[string]string{"AI_AGENT": "Codex@1.2.3"}, "codex"},
		{"row h cut at underscore", map[string]string{"AI_AGENT": "gemini-cli_x"}, "gemini-cli"},
		{"row h unlisted", map[string]string{"AI_AGENT": "my-harness"}, "agent"},
		{"row h unlisted copilot form", map[string]string{"AI_AGENT": "github_copilot_other_agent"}, "agent"},
		{"row h one character", map[string]string{"AI_AGENT": "x"}, "agent"},
		{"row h text that looks like a header", map[string]string{"AI_AGENT": "mozilla/5.0 (evil)"}, "agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, detect("v1", tc.env).Agent)
		})
	}
}

// TestDetectAgentOrder asserts the first matching row wins: a vendor row
// beats AI_AGENT, row a beats row b, and a nested harness reports the
// outer one.
func TestDetectAgentOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a before b", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1", "CODEX_THREAD_ID": "x"}, "claude-code"},
		{"b before c", map[string]string{"CODEX_CI": "1", "GEMINI_CLI": "1"}, "codex"},
		{"c before d", map[string]string{"GEMINI_CLI": "1", "COPILOT_CLI": "1"}, "gemini-cli"},
		{"d before e", map[string]string{"COPILOT_CLI": "1", "OPENCODE": "1"}, "copilot-cli"},
		{"e before f", map[string]string{"OPENCODE": "1", "CLINE_ACTIVE": "true"}, "opencode"},
		{"f before g", map[string]string{"CLINE_ACTIVE": "true", "ROO_ACTIVE": "true"}, "cline"},
		{"g before h", map[string]string{"ROO_ACTIVE": "true", "AI_AGENT": "crush"}, "roo-code"},
		{"d before a copilot AI_AGENT", map[string]string{"COPILOT_CLI": "1", "AI_AGENT": "github_copilot_vscode_agent"}, "copilot-cli"},
		{"vendor row before a different AI_AGENT", map[string]string{"GEMINI_CLI": "1", "AI_AGENT": "claude-code_x"}, "gemini-cli"},
		{"nested codex inside claude code", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1", "CODEX_THREAD_ID": "x", "AI_AGENT": "codex"}, "claude-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, detect("v1", tc.env).Agent)
		})
	}
}

// TestDetectCI asserts the CI rule: CI non-empty and not false or 0 in any
// case, or one of four CI systems' variables; CODEX_CI is not CI.
func TestDetectCI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"none", nil, false},
		{"CI true", map[string]string{"CI": "true"}, true},
		{"CI 1", map[string]string{"CI": "1"}, true},
		{"CI any text", map[string]string{"CI": "yes"}, true},
		{"CI false", map[string]string{"CI": "false"}, false},
		{"CI False", map[string]string{"CI": "False"}, false},
		{"CI FALSE", map[string]string{"CI": "FALSE"}, false},
		{"CI 0", map[string]string{"CI": "0"}, false},
		{"CI empty", map[string]string{"CI": ""}, false},
		{"jenkins", map[string]string{"JENKINS_URL": "https://j"}, true},
		{"teamcity", map[string]string{"TEAMCITY_VERSION": "2024"}, true},
		{"azure pipelines", map[string]string{"TF_BUILD": "True"}, true},
		{"codebuild", map[string]string{"CODEBUILD_BUILD_ARN": "arn:x"}, true},
		{"CI false with jenkins", map[string]string{"CI": "false", "JENKINS_URL": "x"}, true},
		{"codex ci is not CI", map[string]string{"CODEX_CI": "1"}, false},
		{"github actions variable alone", map[string]string{"GITHUB_ACTIONS": "true"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, detect("v1", tc.env).CI)
		})
	}
}

// TestDetectExecPlugin asserts ExecPlugin is true for any non-empty
// KUBERNETES_EXEC_INFO and false otherwise.
func TestDetectExecPlugin(t *testing.T) {
	t.Parallel()
	assert.False(t, detect("v1", nil).ExecPlugin)
	assert.False(t, detect("v1", map[string]string{"KUBERNETES_EXEC_INFO": ""}).ExecPlugin)
	assert.True(t, detect("v1", map[string]string{"KUBERNETES_EXEC_INFO": `{"kind":"ExecCredential"}`}).ExecPlugin)
}

// TestDetectVersion asserts a version is kept when it is a safe product
// token and replaced by "unknown" when it is empty, over 64 bytes, has a
// character outside the RFC 9110 token characters, or contains "mozilla".
func TestDetectVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, version, want string
	}{
		{"release", "v0.1.0-alpha.8", "v0.1.0-alpha.8"},
		{"local build", "dev-83a23a71", "dev-83a23a71"},
		{"dev", "dev", "dev"},
		{"token punctuation", "1!#$%&'*+-.^_`|~", "1!#$%&'*+-.^_`|~"},
		{"64 bytes", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"65 bytes", strings.Repeat("a", 65), "unknown"},
		{"empty", "", "unknown"},
		{"space", "v1 beta", "unknown"},
		{"slash", "v1/2", "unknown"},
		{"parenthesis", "v1(", "unknown"},
		{"semicolon", "v1;x", "unknown"},
		{"newline", "v1\nx", "unknown"},
		{"carriage return", "v1\r", "unknown"},
		{"non ascii", "v1\u00e9", "unknown"},
		{"mozilla", "mozilla", "unknown"},
		{"Mozilla", "Mozilla", "unknown"},
		{"embedded MoZiLlA", "v1-MoZiLlA-x", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, detect(tc.version, nil).Version)
		})
	}
}

// TestDetectPlatform asserts OS and Arch come from the runtime.
func TestDetectPlatform(t *testing.T) {
	t.Parallel()
	c := detect("v1", nil)
	assert.Equal(t, runtime.GOOS, c.OS)
	assert.Equal(t, runtime.GOARCH, c.Arch)
}

// TestDetectIgnoresDoNotTrack asserts DO_NOT_TRACK and RANCHER_CLI_CALLER
// change nothing: the CLI has no opt-out and defines no caller variable.
func TestDetectIgnoresDoNotTrack(t *testing.T) {
	t.Parallel()
	want := detect("v1", nil)
	got := detect("v1", map[string]string{"DO_NOT_TRACK": "1", "RANCHER_CLI_CALLER": "codex", "RANCHER_CLI_TELEMETRY": "off"})
	assert.Equal(t, want, got)
}

// TestUserAgent asserts the value for each documented shape and the part
// order: exec-plugin, then agent, then ci.
func TestUserAgent(t *testing.T) {
	t.Parallel()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	cases := []struct {
		name    string
		version string
		env     map[string]string
		want    string
	}{
		{"terminal or script", "v0.1.0-alpha.8", nil, "cli.rancher.io/v0.1.0-alpha.8 (" + platform + ")"},
		{"inside a Claude Code session", "v0.1.0-alpha.8", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1"}, "cli.rancher.io/v0.1.0-alpha.8 (" + platform + "; agent=claude-code)"},
		{"kubectl exec plugin", "v0.1.0-alpha.8", map[string]string{"KUBERNETES_EXEC_INFO": "{}"}, "cli.rancher.io/v0.1.0-alpha.8 (" + platform + "; exec-plugin)"},
		{"exec plugin inside a Codex session", "v0.1.0-alpha.8", map[string]string{"KUBERNETES_EXEC_INFO": "{}", "CODEX_THREAD_ID": "x"}, "cli.rancher.io/v0.1.0-alpha.8 (" + platform + "; exec-plugin; agent=codex)"},
		{"CI", "v0.1.0-alpha.8", map[string]string{"CI": "true"}, "cli.rancher.io/v0.1.0-alpha.8 (" + platform + "; ci)"},
		{"all parts", "v1", map[string]string{"KUBERNETES_EXEC_INFO": "{}", "AI_AGENT": "unlisted", "CI": "1"}, "cli.rancher.io/v1 (" + platform + "; exec-plugin; agent=agent; ci)"},
		{"local build", "dev-83a23a71", nil, "cli.rancher.io/dev-83a23a71 (" + platform + ")"},
		{"empty version", "", nil, "cli.rancher.io/unknown (" + platform + ")"},
		{"mozilla version", "Mozilla-1", nil, "cli.rancher.io/unknown (" + platform + ")"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, detect(tc.version, tc.env).UserAgent())
		})
	}
}

// TestDetectReadsExactlyTheNamedVariables asserts the set of names Detect
// reads, over every row of the tables above plus an empty environment,
// equals EnvNames, and that DO_NOT_TRACK and RANCHER_CLI_CALLER are not in
// it. The conformance scan and the reference page take their names from
// EnvNames: a variable Detect starts to read without being listed fails
// here.
func TestDetectReadsExactlyTheNamedVariables(t *testing.T) {
	t.Parallel()
	read := map[string]bool{}
	record := func(env map[string]string) {
		Detect("v1", func(name string) string {
			read[name] = true
			return env[name]
		})
	}
	record(nil)
	all := map[string]string{}
	for _, name := range EnvNames() {
		all[name] = "1"
		record(map[string]string{name: "1"})
		record(map[string]string{name: "true"})
	}
	record(all)
	record(map[string]string{"AI_AGENT": "github_copilot_vscode_agent"})

	got := slices.Sorted(maps.Keys(read))
	want := slices.Sorted(slices.Values(EnvNames()))
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "DO_NOT_TRACK")
	assert.NotContains(t, got, "RANCHER_CLI_CALLER")
	assert.Len(t, slices.Compact(slices.Clone(want)), len(want), "EnvNames lists no name twice")
}

// TestAgentIDsAreSafe asserts every listed id is lower case letters,
// digits and hyphens, holds no "mozilla", and is listed once.
func TestAgentIDsAreSafe(t *testing.T) {
	t.Parallel()
	ids := AgentIDs()
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(ids))), len(ids))
	for _, id := range ids {
		require.Regexp(t, `^[a-z][a-z0-9-]*$`, id)
		assert.NotContains(t, id, "mozilla")
	}
}
