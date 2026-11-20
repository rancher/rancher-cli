package extension

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/nodesemver"
)

// Edition is what the server says about itself on /rancherversion: Rancher
// Prime, a community build, or nothing the reader can tell. Unknown is not
// community.
type Edition int

const (
	EditionUnknown Edition = iota
	EditionCommunity
	EditionPrime
)

// ParseEdition decodes the RancherPrime field of /rancherversion, which is the
// text "true" or "false", ignoring case and surrounding space. Anything else is
// EditionUnknown.
func ParseEdition(raw string) Edition {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return EditionPrime
	case "false":
		return EditionCommunity
	}
	return EditionUnknown
}

func (e Edition) String() string {
	switch e {
	case EditionPrime:
		return "prime"
	case EditionCommunity:
		return "community"
	}
	return "unknown"
}

// The results of one check.
const (
	ResultOK         = "ok"
	ResultFailed     = "failed"
	ResultNotChecked = "notChecked"
)

// The names of the checks, in the order the Rancher UI runs them.
const (
	CheckKubeVersion       = "kube-version"
	CheckRancherVersion    = "rancher-version"
	CheckUIVersion         = "ui-version"
	CheckExtensionsVersion = "ui-extensions-version"
	CheckExtensionsHost    = "ui-extensions-host"
	CheckPrimeOnly         = "prime-only"
)

// extensionsHostApp is the host name the Rancher UI compares an
// ui-extensions-host annotation with, as a version.
const extensionsHostApp = "rancher-manager"

// Inputs are what a version of an extension is checked against.
type Inputs struct {
	// RancherVersion is the version the server reports, such as v2.15.3 or
	// v2.16-head. Empty when it could not be read.
	RancherVersion string
	// KubernetesVersion is the Kubernetes version of the local cluster, as
	// KubernetesVersion returns it. Empty when it could not be read.
	KubernetesVersion string
	// ExtensionsAPIVersion is the extensions API version of the server's
	// Rancher UI. Empty when the table has no row for the server's version.
	ExtensionsAPIVersion string
	Edition              Edition
}

// Check is one check that ran for a version of an extension.
type Check struct {
	// Check is the name of the check, one of the Check constants.
	Check string `json:"check"`
	// Annotation is the chart annotation the check reads.
	Annotation string `json:"annotation"`
	// Range is the annotation's value, empty when the annotation is missing.
	Range string `json:"range"`
	// Value is what the range was checked against.
	Value string `json:"value"`
	// Result is ResultOK, ResultFailed or ResultNotChecked.
	Result string `json:"result"`
}

// Outcome is the answer of Compatibility for one version of an extension.
type Outcome struct {
	// Installable is true when no check failed.
	Installable bool
	// Reason says why the first failed check failed; empty when none did.
	Reason string
	// Checks are the checks that ran, in order, up to and including the first
	// failure.
	Checks []Check
}

// ParseRancherVersion is the Rancher UI's reading of the server's version for
// the version checks: its major.minor.patch, and for a version with a "-" (a
// head or prerelease build) a patch of 999, so that a build of a minor passes
// the ranges the minor's releases pass. It returns "" when v has no number.
func ParseRancherVersion(v string) string {
	parsed, ok := nodesemver.Coerce(v)
	if !ok {
		return ""
	}
	if strings.Contains(v, "-") {
		if parts := strings.Split(parsed, "."); len(parts) == 3 {
			return parts[0] + "." + parts[1] + ".999"
		}
	}
	return parsed
}

// Compatibility runs the checks of the Rancher UI's Extensions page on one
// version of an extension chart, given the annotations of that version, in the
// UI's order: kube-version, rancher-version, ui-version, ui-extensions-version
// (missing, then not satisfied), ui-extensions-host and prime-only. The first
// check that fails ends the run. A check whose annotation is missing passes
// and is not listed, except ui-extensions-version, which the UI requires. A
// server whose extensions API version is not known passes the range of
// ui-extensions-version as not checked.
func Compatibility(annotations map[string]string, in Inputs) Outcome {
	var out Outcome
	fail := func(c Check, reason string) Outcome {
		c.Result = ResultFailed
		out.Checks = append(out.Checks, c)
		out.Reason = reason
		return out
	}
	pass := func(c Check) {
		c.Result = ResultOK
		out.Checks = append(out.Checks, c)
	}

	if rng := annotations[AnnotationKubeVersion]; rng != "" {
		c := Check{Check: CheckKubeVersion, Annotation: AnnotationKubeVersion, Range: rng, Value: in.KubernetesVersion}
		switch {
		case in.KubernetesVersion == "":
			return fail(c, "the Kubernetes version of the local cluster could not be read")
		case !nodesemver.Satisfies(in.KubernetesVersion, rng):
			return fail(c, "needs Kubernetes "+rng+"; the local cluster runs "+in.KubernetesVersion)
		}
		pass(c)
	}

	rancher := ParseRancherVersion(in.RancherVersion)
	for _, spec := range []struct{ name, annotation string }{
		{CheckRancherVersion, AnnotationRancherVersion},
		{CheckUIVersion, AnnotationUIVersion},
	} {
		rng := annotations[spec.annotation]
		if rng == "" {
			continue
		}
		c := Check{Check: spec.name, Annotation: spec.annotation, Range: rng, Value: rancher}
		switch {
		case rancher == "":
			return fail(c, "the Rancher version could not be read")
		case !nodesemver.Satisfies(rancher, rng):
			return fail(c, "needs Rancher "+rng+"; this server is "+in.RancherVersion)
		}
		pass(c)
	}

	rng := annotations[AnnotationExtensionsVersion]
	if rng == "" {
		return fail(Check{Check: CheckExtensionsVersion, Annotation: AnnotationExtensionsVersion, Value: in.ExtensionsAPIVersion},
			"sets no ui-extensions-version, which the Rancher UI requires")
	}
	c := Check{Check: CheckExtensionsVersion, Annotation: AnnotationExtensionsVersion, Range: rng}
	if in.ExtensionsAPIVersion == "" {
		c.Result = ResultNotChecked
		out.Checks = append(out.Checks, c)
	} else {
		api, _ := nodesemver.Coerce(in.ExtensionsAPIVersion)
		c.Value = api
		if !nodesemver.Satisfies(api, rng) {
			return fail(c, "needs extensions API "+rng+"; this Rancher's UI offers "+api)
		}
		pass(c)
	}

	if host := annotations[AnnotationExtensionsHost]; host != "" {
		c := Check{Check: CheckExtensionsHost, Annotation: AnnotationExtensionsHost, Range: host, Value: extensionsHostApp}
		if !nodesemver.Satisfies(extensionsHostApp, host) {
			return fail(c, "sets ui-extensions-host "+host+", which the Rancher UI does not install")
		}
		pass(c)
	}

	if annotations[AnnotationPrimeOnly] == "true" {
		c := Check{Check: CheckPrimeOnly, Annotation: AnnotationPrimeOnly, Range: "true", Value: in.Edition.String()}
		switch in.Edition {
		case EditionUnknown:
			return fail(c, "needs Rancher Prime and the edition of this server could not be read")
		case EditionCommunity:
			return fail(c, "needs Rancher Prime")
		}
		pass(c)
	}

	out.Installable = true
	return out
}
