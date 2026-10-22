// Package invocation describes how a rancher process was started: its
// version and platform, whether it ran inside an agent harness session (and
// which), as the kubectl exec plugin, or in CI. The description is computed
// once per process, from the environment only, and is the one place that
// reads the variables that identify a caller. The package uses the standard
// library only and holds no package-level variable.
package invocation

import (
	"runtime"
	"slices"
	"strings"
)

// Environment variable names Detect reads. EnvNames lists them all.
const (
	envClaudeChildSession = "CLAUDE_CODE_CHILD_SESSION"
	envClaudeCode         = "CLAUDECODE"
	envAIAgent            = "AI_AGENT"
	envCodexThreadID      = "CODEX_THREAD_ID"
	envCodexCI            = "CODEX_CI"
	envGeminiCLI          = "GEMINI_CLI"
	envCopilotCLI         = "COPILOT_CLI"
	envOpenCode           = "OPENCODE"
	envClineActive        = "CLINE_ACTIVE"
	envRooActive          = "ROO_ACTIVE"
	envExecInfo           = "KUBERNETES_EXEC_INFO"
	envCI                 = "CI"
	envJenkinsURL         = "JENKINS_URL"
	envTeamCityVersion    = "TEAMCITY_VERSION"
	envTFBuild            = "TF_BUILD"
	envCodeBuildARN       = "CODEBUILD_BUILD_ARN"
)

// Agent ids. Every Context.Agent is one of AgentIDs, AgentGeneric or empty.
const (
	agentClaudeCode    = "claude-code"
	agentCodex         = "codex"
	agentGeminiCLI     = "gemini-cli"
	agentCopilotCLI    = "copilot-cli"
	agentOpenCode      = "opencode"
	agentCline         = "cline"
	agentRooCode       = "roo-code"
	agentCopilotVSCode = "copilot-vscode"
	agentCopilotCloud  = "copilot-cloud"
	agentCrush         = "crush"
	agentPi            = "pi"

	// AgentGeneric is the Agent of a session whose harness is not on the
	// list: AI_AGENT is set to a name the CLI does not know.
	AgentGeneric = "agent"

	// productToken is the User-Agent's product. It is the name the CLI also
	// uses in its output envelope's apiVersion and as its Kubernetes field
	// manager, so an administrator matches audit entries and managed fields
	// by one name.
	productToken = "cli.rancher.io"

	// unknownVersion replaces a version that is not safe to send.
	unknownVersion = "unknown"

	// maxVersionLen is the longest version Detect keeps.
	maxVersionLen = 64
)

// Context describes how this process was started. Detect builds it, once
// per process, from the environment only; it never changes afterwards. It
// never holds an environment value: Agent is an id from AgentIDs, Version is
// a checked token, and the rest are booleans.
//
// The value is self-reported by the caller's environment. It never selects
// a credential, a server entry or a code path, and a server must not
// authorize on it.
type Context struct {
	// Version is the CLI version passed to Detect, or "unknown" when it is
	// empty, longer than 64 bytes, has a character outside the RFC 9110
	// token characters, or contains "mozilla" in any case (Rancher treats a
	// User-Agent containing it as a browser).
	Version string
	// OS and Arch are runtime.GOOS and runtime.GOARCH.
	OS, Arch string
	// Agent is the id of the agent harness whose session the process ran
	// inside, "agent" for an unlisted harness, and "" for none. It states
	// where the process ran; it does not prove the model chose the command.
	Agent string
	// ExecPlugin is true when KUBERNETES_EXEC_INFO is non-empty: client-go
	// sets it on every call of an exec credential plugin. A hand-run
	// "rancher auth get-token" is not the plugin.
	ExecPlugin bool
	// CI is true when a CI system's variable is set; see Detect.
	CI bool
}

// AgentIDs returns the agent ids Detect can report, in a stable order. A new
// harness is one entry here plus, when it sets no AI_AGENT, one row in
// Detect.
func AgentIDs() []string {
	return []string{
		agentClaudeCode, agentCodex, agentGeminiCLI, agentCopilotCLI,
		agentOpenCode, agentCline, agentRooCode, agentCopilotVSCode,
		agentCopilotCloud, agentCrush, agentPi,
	}
}

// EnvNames returns every environment variable name Detect reads, in a stable
// order. A test drives Detect with a recording lookup and asserts the two
// agree, so no other code needs its own copy of the list.
func EnvNames() []string {
	return []string{
		envClaudeChildSession, envClaudeCode, envAIAgent, envCodexThreadID,
		envCodexCI, envGeminiCLI, envCopilotCLI, envOpenCode, envClineActive,
		envRooActive, envExecInfo, envCI, envJenkinsURL, envTeamCityVersion,
		envTFBuild, envCodeBuildARN,
	}
}

// Detect builds the Context for a process started with version, reading the
// environment through getenv (os.Getenv in main, a map lookup in tests). It
// is pure: it reads no file, terminal or network.
//
// Agent is the first matching row:
//
//	a. CLAUDE_CODE_CHILD_SESSION=1, or CLAUDECODE=1 with AI_AGENT starting
//	   "claude-code": claude-code
//	b. CODEX_THREAD_ID non-empty, or CODEX_CI=1: codex
//	c. GEMINI_CLI=1: gemini-cli
//	d. COPILOT_CLI=1 (also when a Copilot AI_AGENT is set): copilot-cli
//	e. OPENCODE=1: opencode
//	f. CLINE_ACTIVE=true: cline
//	g. ROO_ACTIVE=true: roo-code
//	h. AI_AGENT non-empty: its normalized value when that is on the list,
//	   else "agent"
//
// The vendor rows come first because each is an exact signal of one
// harness; AI_AGENT is a shared convention that a harness started from
// another one's shell can carry over. CLAUDECODE=1 alone (set in IDE
// extension terminals with no session) is not detected. When harnesses are
// nested, the first match can name the outer one.
//
// CI is true when CI is non-empty and not "false" or "0" (case-insensitive),
// or JENKINS_URL, TEAMCITY_VERSION, TF_BUILD or CODEBUILD_BUILD_ARN is
// non-empty. CODEX_CI belongs to Codex and does not mean CI.
func Detect(version string, getenv func(string) string) Context {
	c := Context{
		Version:    safeVersion(version),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Agent:      detectAgent(getenv),
		ExecPlugin: getenv(envExecInfo) != "",
	}
	c.CI = detectCI(getenv)
	return c
}

func detectAgent(getenv func(string) string) string {
	aiAgent := getenv(envAIAgent)
	claudeSession := getenv(envClaudeChildSession) == "1"
	claudeCode := getenv(envClaudeCode) == "1"
	codexThread := getenv(envCodexThreadID) != ""
	codexCI := getenv(envCodexCI) == "1"
	gemini := getenv(envGeminiCLI) == "1"
	copilot := getenv(envCopilotCLI) == "1"
	opencode := getenv(envOpenCode) == "1"
	cline := getenv(envClineActive) == "true"
	roo := getenv(envRooActive) == "true"

	switch {
	case claudeSession || (claudeCode && strings.HasPrefix(strings.ToLower(aiAgent), agentClaudeCode)):
		return agentClaudeCode
	case codexThread || codexCI:
		return agentCodex
	case gemini:
		return agentGeminiCLI
	case copilot:
		return agentCopilotCLI
	case opencode:
		return agentOpenCode
	case cline:
		return agentCline
	case roo:
		return agentRooCode
	case aiAgent != "":
		if id := normalizeAIAgent(aiAgent); slices.Contains(AgentIDs(), id) {
			return id
		}
		return AgentGeneric
	}
	return ""
}

// normalizeAIAgent lowercases v, cuts it at the first "@", turns
// "github_copilot_<x>_agent" into "copilot-<x>", and otherwise cuts it at
// the first "_". For example "claude-code_2-1-308_agent" is "claude-code"
// and "github_copilot_vscode_agent" is "copilot-vscode".
func normalizeAIAgent(v string) string {
	v = strings.ToLower(v)
	v, _, _ = strings.Cut(v, "@")
	if x, ok := strings.CutPrefix(v, "github_copilot_"); ok {
		if x, ok = strings.CutSuffix(x, "_agent"); ok {
			return "copilot-" + x
		}
	}
	v, _, _ = strings.Cut(v, "_")
	return v
}

func detectCI(getenv func(string) string) bool {
	ci := getenv(envCI)
	ciSet := ci != "" && !strings.EqualFold(ci, "false") && ci != "0"
	jenkins := getenv(envJenkinsURL) != ""
	teamCity := getenv(envTeamCityVersion) != ""
	azure := getenv(envTFBuild) != ""
	codeBuild := getenv(envCodeBuildARN) != ""
	return ciSet || jenkins || teamCity || azure || codeBuild
}

// safeVersion returns v when it can go into a User-Agent product token and
// "unknown" otherwise.
func safeVersion(v string) string {
	if v == "" || len(v) > maxVersionLen || strings.Contains(strings.ToLower(v), "mozilla") {
		return unknownVersion
	}
	for i := range len(v) {
		if !isTokenChar(v[i]) {
			return unknownVersion
		}
	}
	return v
}

// isTokenChar reports whether b is an RFC 9110 token character.
func isTokenChar(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
}

// UserAgent returns the User-Agent value for c:
//
//	cli.rancher.io/<version> (<os>/<arch>[; exec-plugin][; agent=<id>][; ci])
//
// The product token is fixed. The parts after the platform appear in the
// order shown. Build c with Detect: a zero Context builds an invalid value.
func (c Context) UserAgent() string {
	var b strings.Builder
	b.WriteString(productToken)
	b.WriteByte('/')
	b.WriteString(c.Version)
	b.WriteString(" (")
	b.WriteString(c.OS)
	b.WriteByte('/')
	b.WriteString(c.Arch)
	if c.ExecPlugin {
		b.WriteString("; exec-plugin")
	}
	if c.Agent != "" {
		b.WriteString("; agent=")
		b.WriteString(c.Agent)
	}
	if c.CI {
		b.WriteString("; ci")
	}
	b.WriteByte(')')
	return b.String()
}
