package telemetry

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// TestNormalizeVersionHostileInputs asserts the version rule keeps the
// accepted forms, removes a hotfix id, and turns everything else into
// _OTHER; no customer text reaches an event through a version.
func TestNormalizeVersionHostileInputs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2.15.2", "2.15.2"},
		{"v2.15.2", "2.15.2"},
		{"v2.15.0-alpha22", "2.15.0-alpha22"},
		{"v2.15.2-rc1", "2.15.2-rc1"},
		{"0.1.0-alpha.8", "0.1.0-alpha.8"},
		{"1.2.3-beta.10", "1.2.3-beta.10"},
		{"dev", "dev"},
		{"dev-9f8e7d6", "dev-9f8e7d6"},
		{"dev-unknown", "dev-unknown"},
		{"unknown", "unknown"},
		{"v2.13.4-hotfix-customer1", "2.13.4-hotfix"},
		{"2.13.4-hotfix-acme-corp.2", "2.13.4-hotfix"},
		{"v2.13.4-hotfix", "2.13.4-hotfix"},
		{"2.13.4-hotfix-acme corp", Other},
		{"2.13.4-hotfix-acme\ncorp", Other},
		{"2.13.4-hotfix-" + strings.Repeat("a", 60), Other},
		{"ACME Corp internal build", Other},
		{"0.1.0+dirty", Other},
		{"0.1.0-alpha.8+build.5", Other},
		{"2.15.2-rc1-customer", Other},
		{"v2.15", Other},
		{"vdev", Other},
		{"dev-ZZZ", Other},
		{"dev-" + strings.Repeat("a", 41), Other},
		{"1.2." + strings.Repeat("9", 70), Other},
		{"", Other},
		{"https://rancher.example.com", Other},
		{"1.2.3\n", Other},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, NormalizeVersion(c.in), "%q", c.in)
	}
	assert.Len(t, NormalizeVersion(strings.Repeat("1", 65)), len(Other), "65 characters")
}

// TestNormalizeUUIDHostileInputs asserts only a lowercase hyphenated UUID
// passes.
func TestNormalizeUUIDHostileInputs(t *testing.T) {
	good := "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	got, ok := NormalizeUUID(good)
	require.True(t, ok)
	assert.Equal(t, good, got)
	for _, bad := range []string{
		"", "cluster-prod-1", strings.ToUpper(good), "d3b07384d9a04e7a9c2b5f6a7b8c9d0e",
		good + "\n", " " + good, good[:35], good + "0", "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0g",
		"{" + good + "}", "urn:uuid:" + good,
	} {
		_, ok := NormalizeUUID(bad)
		assert.False(t, ok, "%q", bad)
	}
}

// TestNormalizeRandomIDHostileInputs asserts only a lowercase hyphenated
// version 4 UUID with the RFC 4122 variant passes; the install id's general
// UUID rule is looser.
func TestNormalizeRandomIDHostileInputs(t *testing.T) {
	good := "3f2a6c1e-8b7d-4e5a-9c10-2d4f6a8b0c1e"
	got, ok := NormalizeRandomID(good)
	require.True(t, ok)
	assert.Equal(t, good, got)
	for _, bad := range []string{
		"", "12345678-1234-1234-1234-123456789abc", "3f2a6c1e-8b7d-1e5a-9c10-2d4f6a8b0c1e",
		"3f2a6c1e-8b7d-4e5a-7c10-2d4f6a8b0c1e", strings.ToUpper(good), good + "\n", "x" + good,
		"3f2a6c1e8b7d4e5a9c102d4f6a8b0c1e",
	} {
		_, ok := NormalizeRandomID(bad)
		assert.False(t, ok, "%q", bad)
	}
	_, ok = NormalizeUUID("12345678-1234-1234-1234-123456789abc")
	assert.True(t, ok, "the install id may be any UUID")
}

// TestNormalizeOSAndArch asserts the OpenTelemetry mapping, the shipped
// platforms included, and _OTHER for the rest.
func TestNormalizeOSAndArch(t *testing.T) {
	for goos, want := range map[string]string{"linux": "linux", "darwin": "darwin", "windows": "windows", "freebsd": "freebsd", "dragonfly": "dragonflybsd", "zos": "z_os", "plan9": Other, "js": Other, "": Other, "Linux": Other, "my-os": Other} {
		assert.Equal(t, want, NormalizeOS(goos), goos)
	}
	for goarch, want := range map[string]string{"amd64": "amd64", "arm64": "arm64", "s390x": "s390x", "386": "x86", "arm": "arm32", "ppc64le": "ppc64", "riscv64": Other, "wasm": Other, "": Other, "AMD64": Other} {
		assert.Equal(t, want, NormalizeArch(goarch), goarch)
	}
	assert.Contains(t, OSValues(), "windows")
	assert.Equal(t, Other, OSValues()[len(OSValues())-1])
	assert.Equal(t, Other, ArchValues()[len(ArchValues())-1])
}

// TestNormalizeCommandHostileInputs asserts the command name rule.
func TestNormalizeCommandHostileInputs(t *testing.T) {
	for _, ok := range []string{"get", "get project", "auth get-token", "a b c", "config set-token-reuse", strings.Repeat("a", 32) + " " + strings.Repeat("b", 32) + " " + strings.Repeat("c", 32)} {
		got, accepted := NormalizeCommand(ok)
		assert.True(t, accepted, ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{
		"", "Get", "get Project", "a b c d", "get  project", " get", "get ", "1get", "get_project", "get\nproject",
		strings.Repeat("a", 33), "get my-cluster/prod", "get --output",
	} {
		_, accepted := NormalizeCommand(bad)
		assert.False(t, accepted, "%q", bad)
	}
}

// TestNormalizeFlagsKeepsTheFirstThirtyTwoSorted asserts the flag rule:
// invalid names dropped, duplicates removed, sorted, at most 32 kept, never
// nil.
func TestNormalizeFlagsKeepsTheFirstThirtyTwoSorted(t *testing.T) {
	assert.Equal(t, []string{}, NormalizeFlags(nil))
	assert.Equal(t, []string{"cluster", "output"}, NormalizeFlags([]string{"output", "cluster", "output"}))
	assert.Equal(t, []string{"cluster"}, NormalizeFlags([]string{"cluster", "Output", "output=json", "-o", "x_y", "", strings.Repeat("a", 33), "--cluster"}))
	var many []string
	for i := 40; i > 0; i-- {
		many = append(many, "flag-"+string(rune('a'+i/10))+string(rune('a'+i%10)))
	}
	got := NormalizeFlags(many)
	require.Len(t, got, 32)
	assert.IsIncreasing(t, got)
	all := NormalizeFlags(append(many, "aaa"))
	assert.Equal(t, "aaa", all[0], "the sorted order decides which 32 stay")
}

// TestNormalizeErrorTypeAgentAndExit asserts the closed-list normalizers.
func TestNormalizeErrorTypeAgentAndExit(t *testing.T) {
	known := []string{"RBAC", "GENERIC"}
	assert.Equal(t, "RBAC", NormalizeErrorType("RBAC", known))
	for _, bad := range []string{"", "rbac", "Some customer error text", "RBAC\n"} {
		assert.Equal(t, Other, NormalizeErrorType(bad, known), bad)
	}
	id, ok := NormalizeAgent("claude-code")
	assert.True(t, ok)
	assert.Equal(t, "claude-code", id)
	id, ok = NormalizeAgent("my-private-agent")
	assert.True(t, ok)
	assert.Equal(t, "agent", id)
	_, ok = NormalizeAgent("")
	assert.False(t, ok)
	for n, want := range map[int]bool{0: true, 255: true, 130: true, -1: false, 256: false} {
		_, ok := NormalizeExitCode(n)
		assert.Equal(t, want, ok, n)
	}
}

// TestBucketBoundaries asserts the duration buckets at each boundary.
func TestBucketBoundaries(t *testing.T) {
	for d, want := range map[time.Duration]Bucket{
		0: BucketUnder1s, 999 * time.Millisecond: BucketUnder1s,
		time.Second: Bucket1to5s, 5 * time.Second: Bucket1to5s,
		5*time.Second + 1: BucketOver5s, time.Hour: BucketOver5s, -time.Second: BucketUnder1s,
	} {
		assert.Equal(t, want, BucketFor(d), d.String())
	}
}

// TestClientTypeRule asserts the ordered client type rule.
func TestClientTypeRule(t *testing.T) {
	cases := []struct {
		name string
		ctx  invocation.Context
		tty  bool
		want ClientType
	}{
		{"agent wins over a terminal", invocation.Context{Agent: "claude-code"}, true, ClientAgent},
		{"agent in CI is an agent", invocation.Context{Agent: "codex", CI: true}, false, ClientAgent},
		{"person at a terminal", invocation.Context{}, true, ClientPerson},
		{"exec plugin is automation", invocation.Context{ExecPlugin: true}, true, ClientAutomation},
		{"piped output is automation", invocation.Context{}, false, ClientAutomation},
		{"CI does not change a person", invocation.Context{CI: true}, true, ClientPerson},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ClientTypeFor(c.ctx, c.tty), c.name)
	}
}

// TestIsDevelopmentVersionEqualsTheVersionRule asserts the prefix shortcut
// gives the same answer as the rule it stands for: NormalizeVersion returns
// dev, dev-<hex> or dev-unknown.
func TestIsDevelopmentVersionEqualsTheVersionRule(t *testing.T) {
	t.Parallel()
	rule := func(raw string) bool {
		v := NormalizeVersion(raw)
		return v == "dev" || v == "dev-unknown" || regexp.MustCompile(`^dev-[0-9a-f]{1,40}$`).MatchString(v)
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"dev", true},
		{"dev-abc123", true},
		{"dev-0", true},
		{"dev-unknown", true},
		{"dev-" + strings.Repeat("a", 40), true},
		{"dev-" + strings.Repeat("a", 41), false},
		{"dev-" + strings.Repeat("a", 61), false},
		{"devel", false},
		{"dev-", false},
		{"dev-ABC", false},
		{"dev-xyz", false},
		{"dev-abc-1", false},
		{"v1.2.3", false},
		{"1.2.3", false},
		{"0.1.0-alpha.8", false},
		{"1.2.3-hotfix-x", false},
		{"unknown", false},
		{"_OTHER", false},
		{"", false},
		{"Dev", false},
		{"vdev", false},
	} {
		assert.Equal(t, tc.want, IsDevelopmentVersion(tc.raw), tc.raw)
		assert.Equal(t, rule(tc.raw), IsDevelopmentVersion(tc.raw), "the rule and the shortcut differ for %q", tc.raw)
	}
}
