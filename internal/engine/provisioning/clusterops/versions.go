package clusterops

import (
	"cmp"
	"strconv"
	"strings"
)

// ParseVersionKey extracts the comparable parts of a raw channel-server
// version string (e.g. "v1.34.1+rke2r1" or "v1.30.5+k3s1") into
// (major, minor, patch, releaseNum). ok is false when the semver core cannot
// be parsed as MAJOR.MINOR.PATCH. The release number is the trailing decimal
// integer in the build-metadata suffix ("rke2r1" is 1, "k3s1" is 1,
// "rke2r2" is 2); it is 0 when no such suffix exists.
func ParseVersionKey(raw string) (major, minor, patch, releaseNum int, ok bool) {
	v := strings.TrimPrefix(raw, "v")
	semCore := v
	rel := 0
	if before, after, ok := strings.Cut(v, "+"); ok {
		semCore = before
		suffix := after
		j := len(suffix)
		for j > 0 && suffix[j-1] >= '0' && suffix[j-1] <= '9' {
			j--
		}
		if j < len(suffix) {
			rel, _ = strconv.Atoi(suffix[j:])
		}
	}
	parts := strings.SplitN(semCore, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, 0, false
	}
	maj, errMaj := strconv.Atoi(parts[0])
	mnr, errMin := strconv.Atoi(parts[1])
	pat, errPat := strconv.Atoi(parts[2])
	if errMaj != nil || errMin != nil || errPat != nil {
		return 0, 0, 0, 0, false
	}
	return maj, mnr, pat, rel, true
}

// CompareVersions returns -1, 0, or 1 comparing two raw version strings by
// semver core (MAJOR.MINOR.PATCH) then by release number (the trailing rN
// integer in the build-metadata suffix). An unparseable string sorts below any
// parseable one.
func CompareVersions(a, b string) int {
	aMaj, aMin, aPat, aRel, aOk := ParseVersionKey(a)
	bMaj, bMin, bPat, bRel, bOk := ParseVersionKey(b)
	switch {
	case !aOk && !bOk:
		return 0
	case !aOk:
		return -1
	case !bOk:
		return 1
	}
	if c := CompareTriple(aMaj, aMin, aPat, bMaj, bMin, bPat, false); c != 0 {
		return c
	}
	return cmp.Compare(aRel, bRel)
}

// CompareTriple compares (aMajor,aMinor,aPatch) to (bMajor,bMinor,bPatch),
// returning -1/0/1. When wildcard is set the patch position is dropped from
// the comparison entirely - that is what a ".x" operand means.
func CompareTriple(aMajor, aMinor, aPatch, bMajor, bMinor, bPatch int, wildcard bool) int {
	if c := cmp.Compare(aMajor, bMajor); c != 0 {
		return c
	}
	if c := cmp.Compare(aMinor, bMinor); c != 0 {
		return c
	}
	if wildcard {
		return 0
	}
	return cmp.Compare(aPatch, bPatch)
}

// Release is one row of the channel server's release list.
type Release struct {
	Version string `json:"version"`
	// ServerArgs is the row's serverArgs as decoded: nil when the row has
	// none or it is JSON null.
	ServerArgs any `json:"serverArgs"`
}

// HasServerArgs reports whether the row carries serverArgs: present and not
// JSON null, an empty object included. The Rancher UI offers only such rows,
// and a release without them stalls the planner of a provisioned cluster.
func (r Release) HasServerArgs() bool {
	return r.ServerArgs != nil
}

// NewestPatches returns the newest release of each major.minor (the key, for
// example "1.31") by CompareVersions, over the rows with serverArgs: the rows
// the Rancher UI offers before it filters deprecated patch versions. Rows
// that do not parse are left out.
func NewestPatches(releases []Release) map[string]string {
	newest := map[string]string{}
	for _, r := range releases {
		if !r.HasServerArgs() {
			continue
		}
		major, minor, _, _, ok := ParseVersionKey(r.Version)
		if !ok {
			continue
		}
		key := strconv.Itoa(major) + "." + strconv.Itoa(minor)
		if cur, seen := newest[key]; !seen || CompareVersions(r.Version, cur) > 0 {
			newest[key] = r.Version
		}
	}
	return newest
}
