package telemetry

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// Other is the value recorded for anything a value list or pattern does
// not accept.
const Other = "_OTHER"

// maxVersionLen is the longest version text the version rule accepts.
const maxVersionLen = 64

// patternSet holds the compiled regular expressions of the normalizers.
type patternSet struct {
	releaseVersion, hotfixVersion, devVersion, uuid, randomID, command, flag *regexp.Regexp
}

// patterns compiles the set on first use, so a process that never
// normalizes a value pays nothing at start.
var patterns = sync.OnceValue(func() *patternSet {
	return &patternSet{
		releaseVersion: regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha|beta|rc)\.?[0-9]+)?$`),
		hotfixVersion:  regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)-hotfix(?:-[A-Za-z0-9._-]+)?$`),
		devVersion:     regexp.MustCompile(`^dev-[0-9a-f]{1,40}$`),
		uuid:           regexp.MustCompile(uuidPattern),
		randomID:       regexp.MustCompile(randomIDPattern),
		command:        regexp.MustCompile(commandPattern),
		flag:           regexp.MustCompile(flagPattern),
	}
})

// NormalizeVersion applies the version rule to a version text: MAJOR.MINOR.PATCH
// with a leading v removed, optionally followed by -alphaN, -betaN or -rcN
// with or without a dot before the number; the development forms dev,
// dev-<hex sha>, dev-unknown and unknown unchanged; a hotfix tag
// (X.Y.Z-hotfix-<id>) as X.Y.Z-hotfix with the id removed, because a hotfix
// id can belong to one customer. Anything else (a build suffix, other
// text, more than 64 characters) is _OTHER.
func NormalizeVersion(raw string) string {
	if len(raw) > maxVersionLen {
		return Other
	}
	v := raw
	if len(v) > 1 && v[0] == 'v' && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	switch {
	case v == "dev", v == "dev-unknown", v == "unknown":
		return v
	case patterns().devVersion.MatchString(v), patterns().releaseVersion.MatchString(v):
		return v
	}
	if m := patterns().hotfixVersion.FindStringSubmatch(v); m != nil {
		return m[1] + "-hotfix"
	}
	return Other
}

// IsDevelopmentVersion reports whether raw is a development build's version:
// NormalizeVersion gives dev, dev-<hex sha> or dev-unknown. unknown, _OTHER
// and every release form are not. Every development form starts with dev, so
// the prefix test answers a release version without compiling the version
// rule's regular expressions.
func IsDevelopmentVersion(raw string) bool {
	if !strings.HasPrefix(raw, "dev") {
		return false
	}
	v := NormalizeVersion(raw)
	return v == "dev" || v == "dev-unknown" || patterns().devVersion.MatchString(v)
}

// NormalizeUUID reports whether s is a lowercase hyphenated UUID and
// returns it. Anything else, an uppercase or unhyphenated form included,
// is rejected.
func NormalizeUUID(s string) (string, bool) {
	if !patterns().uuid.MatchString(s) {
		return "", false
	}
	return s, true
}

// NormalizeRandomID reports whether s is a lowercase hyphenated UUID of
// version 4 with the RFC 4122 variant, the form of every id this producer
// creates (the process instance id, the configuration directory id and
// each record id). A server's install id is not one of them; it goes
// through NormalizeUUID.
func NormalizeRandomID(s string) (string, bool) {
	if !patterns().randomID.MatchString(s) {
		return "", false
	}
	return s, true
}

var osValues = map[string]string{
	"aix":       "aix",
	"darwin":    "darwin",
	"dragonfly": "dragonflybsd",
	"freebsd":   "freebsd",
	"linux":     "linux",
	"netbsd":    "netbsd",
	"openbsd":   "openbsd",
	"solaris":   "solaris",
	"windows":   "windows",
	"zos":       "z_os",
}

var archValues = map[string]string{
	"386":     "x86",
	"amd64":   "amd64",
	"arm":     "arm32",
	"arm64":   "arm64",
	"ppc64":   "ppc64",
	"ppc64le": "ppc64",
	"s390x":   "s390x",
}

// NormalizeOS maps a Go GOOS value to the OpenTelemetry os.type value, or
// _OTHER when there is none.
func NormalizeOS(goos string) string {
	if v, ok := osValues[goos]; ok {
		return v
	}
	return Other
}

// NormalizeArch maps a Go GOARCH value to the OpenTelemetry host.arch
// value, or _OTHER when there is none.
func NormalizeArch(goarch string) string {
	if v, ok := archValues[goarch]; ok {
		return v
	}
	return Other
}

// OSValues is the closed list of os.type values, _OTHER last.
func OSValues() []string { return sortedValues(osValues) }

// ArchValues is the closed list of host.arch values, _OTHER last.
func ArchValues() []string { return sortedValues(archValues) }

func sortedValues(m map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range m {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return append(out, Other)
}

// NormalizeCommand reports whether name follows the command name rule: one
// to three words of lowercase letters, digits and hyphens, each starting
// with a letter and at most 32 characters.
func NormalizeCommand(name string) (string, bool) {
	if !patterns().command.MatchString(name) {
		return "", false
	}
	return name, true
}

// NormalizeFlags applies the flag name rule to a list: names that do not
// follow it are dropped, duplicates removed, the rest sorted, and the first
// 32 kept. The result is never nil.
func NormalizeFlags(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if patterns().flag.MatchString(n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxFlags {
		out = out[:maxFlags]
	}
	return out
}

// NormalizeErrorType returns code when it is in known, else _OTHER.
func NormalizeErrorType(code string, known []string) string {
	if slices.Contains(known, code) {
		return code
	}
	return Other
}

// NormalizeAgent returns an agent id from the closed list, the generic
// agent for an id not on it, and false for an empty id.
func NormalizeAgent(id string) (string, bool) {
	switch {
	case id == "":
		return "", false
	case slices.Contains(invocation.AgentIDs(), id):
		return id, true
	}
	return invocation.AgentGeneric, true
}

// NormalizeExitCode reports whether n is a valid process exit status.
func NormalizeExitCode(n int) (int, bool) {
	if n < 0 || n > 255 {
		return 0, false
	}
	return n, true
}

// Outcome says whether a command succeeded.
type Outcome string

// The outcomes.
const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
)

// Edition is the edition of a Rancher server.
type Edition string

// The editions. EditionUnknown records nothing.
const (
	EditionUnknown   Edition = ""
	EditionCommunity Edition = "community"
	EditionPrime     Edition = "prime"
)

// Bucket is a command duration bucket.
type Bucket string

// The duration buckets.
const (
	BucketUnder1s Bucket = "lt1s"
	Bucket1to5s   Bucket = "1to5s"
	BucketOver5s  Bucket = "gt5s"
)

// BucketFor returns the bucket of a duration: under one second, from one to
// five seconds, over five seconds.
func BucketFor(d time.Duration) Bucket {
	switch {
	case d < time.Second:
		return BucketUnder1s
	case d <= 5*time.Second:
		return Bucket1to5s
	}
	return BucketOver5s
}

// ClientType is the hint about who ran a command.
type ClientType string

// The client types.
const (
	ClientPerson     ClientType = "person"
	ClientAgent      ClientType = "agent"
	ClientAutomation ClientType = "automation"
)

// ClientTypeFor applies the ordered client type rule: agent when the
// invocation context names an agent harness; else person when standard
// output is a terminal and the process is not the exec plugin; else
// automation. The CI and exec plugin flags are reported beside the type and
// never change it.
func ClientTypeFor(ctx invocation.Context, stdoutTTY bool) ClientType {
	switch {
	case ctx.Agent != "":
		return ClientAgent
	case stdoutTTY && !ctx.ExecPlugin:
		return ClientPerson
	}
	return ClientAutomation
}

// MissingReason says why an install id is absent.
type MissingReason string

// The reasons.
const (
	ReasonNoServer     MissingReason = "no_server"
	ReasonNotPermitted MissingReason = "not_permitted"
	ReasonNotFetched   MissingReason = "not_fetched"
	ReasonInvalid      MissingReason = "invalid"
)

func (r MissingReason) valid() bool {
	switch r {
	case ReasonNoServer, ReasonNotPermitted, ReasonNotFetched, ReasonInvalid:
		return true
	}
	return false
}

func parseEdition(s string) Edition {
	switch Edition(strings.ToLower(s)) {
	case EditionCommunity:
		return EditionCommunity
	case EditionPrime:
		return EditionPrime
	}
	return EditionUnknown
}
