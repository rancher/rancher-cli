package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinRancherVersion is the CLI's declared minimum supported Rancher server
// version (PS-8, warn-only floor). It moves forward deliberately, in a
// commit, when the CLI starts depending on newer server API — no
// per-feature version matrix.
const MinRancherVersion = "2.14"

// UnknownServerVersionWarning is PS-8's warn-only text for when a server's
// Rancher version cannot be determined at all: GET /rancherversion failing
// (unreachable, non-200, unrecognized body), or (via EvalMinVersionSupport)
// a successfully read value with no parseable major.minor prefix (dev/head
// builds). This posture is never a failure — the caller assumes supported
// and moves on.
const UnknownServerVersionWarning = "cannot determine server version, assuming supported"

var versionPrefix = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion extracts major.minor[.patch] from a Rancher version string.
// A leading "v" and any pre-release/build suffix after the numeric part are
// stripped (v2.14.3-rc1 -> 2, 14, 3; v2.14-head -> 2, 14, 0). ok is false
// when no leading major.minor is parseable at all — canonical example: a
// dev server's value is literally "dev".
func parseVersion(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	m := versionPrefix.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	return major, minor, patch, true
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// compareVersions compares two Rancher version strings numerically on
// major.minor.patch (PS-8): an absent patch component compares as 0 on
// either side ("2.14" == "2.14.0"; "2.14.1" >= "2.14"; "2.14.0" < "2.14.2").
// ok is false when either string has no parseable leading major.minor — the
// caller must treat that as unknown, never as below-floor.
func compareVersions(a, b string) (cmp int, ok bool) {
	aMaj, aMin, aPatch, aOK := parseVersion(a)
	bMaj, bMin, bPatch, bOK := parseVersion(b)
	if !aOK || !bOK {
		return 0, false
	}
	switch {
	case aMaj != bMaj:
		return sign(aMaj - bMaj), true
	case aMin != bMin:
		return sign(aMin - bMin), true
	default:
		return sign(aPatch - bPatch), true
	}
}

// EvalMinVersionSupport evaluates raw (a server-version Setting value)
// against MinRancherVersion for PS-8's warn-only floor. supported is nil
// when raw has no parseable major.minor (dev/head builds) — the same
// "assume supported" posture as UnknownServerVersionWarning, never a
// failure. warning is "" only when supported is true.
func EvalMinVersionSupport(raw string) (supported *bool, warning string) {
	cmp, ok := compareVersions(raw, MinRancherVersion)
	if !ok {
		return nil, UnknownServerVersionWarning
	}
	if cmp < 0 {
		s := false
		return &s, fmt.Sprintf("server %s is below the supported minimum %s; some commands may fail", raw, MinRancherVersion)
	}
	s := true
	return &s, ""
}
