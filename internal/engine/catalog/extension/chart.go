package extension

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The chart annotations the extension rules read.
const (
	AnnotationUIComponent       = "catalog.cattle.io/ui-component"
	AnnotationHidden            = "catalog.cattle.io/hidden"
	AnnotationDisplayName       = "catalog.cattle.io/display-name"
	AnnotationKubeVersion       = "catalog.cattle.io/kube-version"
	AnnotationRancherVersion    = "catalog.cattle.io/rancher-version"
	AnnotationUIVersion         = "catalog.cattle.io/ui-version"
	AnnotationExtensionsVersion = "catalog.cattle.io/ui-extensions-version"
	AnnotationExtensionsHost    = "catalog.cattle.io/ui-extensions-host"
	AnnotationPrimeOnly         = "catalog.cattle.io/prime-only"
	AnnotationSourceRepo        = "catalog.cattle.io/ui-source-repo"
	AnnotationClusterRepoName   = "catalog.cattle.io/cluster-repo-name"
)

// Namespace is the namespace Rancher installs extensions into.
const Namespace = "cattle-ui-plugin-system"

// IsExtensionVersion reports whether the annotations of one chart version
// mark it as a Rancher UI extension: catalog.cattle.io/ui-component is
// "plugins".
func IsExtensionVersion(annotations map[string]string) bool {
	return annotations[AnnotationUIComponent] == "plugins"
}

// IsExtensionChart reports whether a chart is a Rancher UI extension: any of
// its versions is, given the annotations of each version.
func IsExtensionChart(versions []map[string]string) bool {
	return slices.ContainsFunc(versions, IsExtensionVersion)
}

// IsHiddenExtension reports whether the Rancher UI leaves an extension chart
// out of its Extensions page: any version has catalog.cattle.io/hidden equal
// to the text "true". The test of an ordinary chart is a different one: it
// looks at one version and accepts any value that is not empty.
func IsHiddenExtension(versions []map[string]string) bool {
	for _, a := range versions {
		if a[AnnotationHidden] == "true" {
			return true
		}
	}
	return false
}

// kubernetesSuffix is the part of a Kubernetes version from the first "+" or
// "-": the distribution's build, which the Rancher UI removes before it
// compares the version with a range.
var kubernetesSuffix = regexp.MustCompile(`[+-].*$`)

// KubernetesVersion returns the Kubernetes version of the local cluster that
// the kube-version check compares: infoVersion (status.info.kubernetesVersion
// of the management Cluster), else gitVersion (status.version.gitVersion),
// with everything from the first "+" or "-" removed. It reports false when
// both are empty.
func KubernetesVersion(infoVersion, gitVersion string) (string, bool) {
	v := infoVersion
	if v == "" {
		v = gitVersion
	}
	if v == "" {
		return "", false
	}
	return kubernetesSuffix.ReplaceAllString(v, ""), true
}

// VersionLabel is how the Rancher UI names a chart version: the app version
// alone when it equals the chart version or is empty, else the app version
// with the chart version in parentheses.
func VersionLabel(version, appVersion string) string {
	if appVersion == "" || appVersion == version {
		return version
	}
	return fmt.Sprintf("%s (%s)", appVersion, version)
}

// PluginVersion is the version an installed UIPlugin reports for a chart
// version: the app version, else the chart version.
func PluginVersion(version, appVersion string) string {
	if strings.TrimSpace(appVersion) != "" {
		return appVersion
	}
	return version
}
