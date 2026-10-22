package invocation

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// userAgentShape is a product token and one comment whose parts are the
// platform and the fixed words and ids this package writes.
var userAgentShape = regexp.MustCompile("^cli\\.rancher\\.io/[0-9A-Za-z!#$%&'*+.^_`|~-]+ \\([a-z0-9]+/[a-z0-9]+(; exec-plugin)?(; agent=[a-z0-9-]+)?(; ci)?\\)$")

// FuzzDetect asserts, for arbitrary environment values and versions, that
// Agent is on the list, "agent" or empty, and that UserAgent parses as a
// product plus a comment holding no "mozilla", CR or LF: no environment
// text reaches the value. Bit i of mask gives the i-th name of EnvNames the
// value text; every other name is empty. AI_AGENT takes aiAgent.
func FuzzDetect(f *testing.F) {
	names := EnvNames()
	all := uint32(1)<<len(names) - 1
	for _, version := range []string{"", "dev", "v0.1.0-alpha.8", "mozilla", "Mozilla", "v1-MOZILLA", "v1/2", "v1 (x)", strings.Repeat("a", 65)} {
		f.Add(version, "1", "claude-code_2-1-308_agent", all)
	}
	for _, aiAgent := range []string{"", "Mozilla/5.0", "mozilla", "github_copilot_vscode_agent", "github_copilot_mozilla_agent", "x\r\ny", "claude-code@mozilla", "crush", "a;b)"} {
		f.Add("v1", "true", aiAgent, all)
		f.Add("v1", "1", aiAgent, uint32(0))
	}
	f.Fuzz(func(t *testing.T, version, value, aiAgent string, mask uint32) {
		getenv := func(name string) string {
			if name == "AI_AGENT" {
				return aiAgent
			}
			if i := slices.Index(names, name); i >= 0 && mask&(1<<i) != 0 {
				return value
			}
			return ""
		}
		c := Detect(version, getenv)
		if c.Agent != "" && c.Agent != AgentGeneric && !slices.Contains(AgentIDs(), c.Agent) {
			t.Fatalf("Agent %q is not on the list", c.Agent)
		}
		ua := c.UserAgent()
		if !userAgentShape.MatchString(ua) {
			t.Fatalf("User-Agent %q does not parse as a product and a comment", ua)
		}
		if strings.Contains(strings.ToLower(ua), "mozilla") || strings.ContainsAny(ua, "\r\n") {
			t.Fatalf("User-Agent %q holds mozilla, CR or LF", ua)
		}
	})
}
