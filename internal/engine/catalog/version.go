package catalog

import (
	"regexp"
	"slices"
)

// deprecatedAnnotation marks a deprecated chart version; so does the
// index entry's own deprecated field.
const deprecatedAnnotation = "catalog.cattle.io/deprecated"

// prereleaseText is a version with three numeric parts, an optional leading
// "v", a "-" prerelease and optional build metadata: the shape the Rancher UI
// treats as a prerelease. A "-" inside the build metadata or a version with
// fewer than three numeric parts is not one.
var prereleaseText = regexp.MustCompile(`^v?\d+\.\d+\.\d+-[^+]+(\+.*)?$`)

// IsPrerelease reports whether the chart version text is a prerelease.
func IsPrerelease(version string) bool { return prereleaseText.MatchString(version) }

// IsDeprecated reports whether v is deprecated: the index entry's
// deprecated field is true or its annotation catalog.cattle.io/deprecated is
// "true".
func IsDeprecated(v ChartVersion) bool {
	return v.Deprecated || v.Annotations[deprecatedAnnotation] == "true"
}

// WithoutDeprecated returns the versions that are not deprecated, in their
// order.
func WithoutDeprecated(versions []ChartVersion) []ChartVersion {
	return slices.DeleteFunc(slices.Clone(versions), IsDeprecated)
}

// DefaultVersion picks the version a chart shows when no version is asked
// for: the first one that is not a prerelease, in the order Rancher serves
// them, or the first one when every version is a prerelease. It reports false
// when there is no version.
func DefaultVersion(versions []ChartVersion) (ChartVersion, bool) {
	if len(versions) == 0 {
		return ChartVersion{}, false
	}
	for _, v := range versions {
		if !IsPrerelease(v.Version) {
			return v, true
		}
	}
	return versions[0], true
}
