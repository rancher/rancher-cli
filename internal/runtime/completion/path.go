package completion

import (
	"errors"
	"net/url"
	"strings"
)

// ErrInvalidSegment is returned by the Path builders for a segment that
// fails ValidSegment.
var ErrInvalidSegment = errors.New("completion: invalid path segment")

// Path is a server-relative request path. Only this package's builders
// produce one, so a string assembled from Request.Tail cannot reach
// GetJSON; the zero value is not requestable.
type Path struct{ path string }

// String returns the server-relative path, query included, for tracing
// and assertions.
func (p Path) String() string { return p.path }

// ValidSegment reports whether s may become one component of a request
// path: letters, digits, dot and underscore, a hyphen anywhere but first,
// no slash, and not a segment of dots alone, which a proxy would
// normalize away and silently change the scope.
func ValidSegment(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		case c == '.':
			dots++
		case c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return dots < len(s)
}

// ManagementPath is a collection of the management cluster through its
// proxy: the group's resource at version, or the core group's (group "")
// under /api. Every segment goes through ValidSegment.
func ManagementPath(group, version, resource string) (Path, error) {
	return ClusterPath("local", group, version, resource)
}

// ManagementNamespacedPath is a namespaced collection of the management
// cluster through its proxy: the group's resource at version under
// namespace, for the management kinds that live in a namespace (a
// cluster's projects, a workspace's provisioning clusters). Every segment
// goes through ValidSegment.
func ManagementNamespacedPath(group, version, namespace, resource string) (Path, error) {
	return NamespacedPath("local", group, version, namespace, resource)
}

// ClusterPath is a cluster-scoped collection under a cluster's proxy:
// /k8s/clusters/<clusterID>/apis/<group>/<version>/<resource>, or the
// core-group form /k8s/clusters/<clusterID>/api/<version>/<resource> when
// group is "". Every segment goes through ValidSegment.
func ClusterPath(clusterID, group, version, resource string) (Path, error) {
	return build(append(apiRoot(clusterID, group, version), resource))
}

// NamespacedPath is a namespaced collection under a cluster's proxy:
// .../namespaces/<namespace>/<resource>. Every segment goes through
// ValidSegment.
func NamespacedPath(clusterID, group, version, namespace, resource string) (Path, error) {
	return build(append(apiRoot(clusterID, group, version), "namespaces", namespace, resource))
}

// NamedResourcePath is one object under a cluster's proxy: the namespaced
// form when namespace is set, the cluster-scoped form when it is "".
// Every segment goes through ValidSegment.
func NamedResourcePath(clusterID, group, version, namespace, resource, name string) (Path, error) {
	segs := apiRoot(clusterID, group, version)
	if namespace != "" {
		segs = append(segs, "namespaces", namespace)
	}
	return build(append(segs, resource, name))
}

// ManagementNamedResourcePath is one object of the management cluster
// through its proxy: the namespaced form when namespace is set, the
// cluster-scoped form when it is "". Every segment goes through
// ValidSegment.
func ManagementNamedResourcePath(group, version, namespace, resource, name string) (Path, error) {
	return NamedResourcePath("local", group, version, namespace, resource, name)
}

// DiscoveryVersionPath is one group version's legacy discovery document
// under a cluster's proxy: /k8s/clusters/<clusterID>/apis/<group>/<version>,
// or /k8s/clusters/<clusterID>/api/<version> for the core group (group
// ""). Every segment goes through ValidSegment.
func DiscoveryVersionPath(clusterID, group, version string) (Path, error) {
	return build(apiRoot(clusterID, group, version))
}

// DiscoveryPaths are the two aggregated discovery roots of a cluster
// through its proxy: /k8s/clusters/<clusterID>/api for the core group and
// /k8s/clusters/<clusterID>/apis for every other group. The cluster id
// goes through ValidSegment.
func DiscoveryPaths(clusterID string) (core, groups Path, err error) {
	if core, err = build([]string{"k8s", "clusters", clusterID, "api"}); err != nil {
		return Path{}, Path{}, err
	}
	groups, err = build([]string{"k8s", "clusters", clusterID, "apis"})
	return core, groups, err
}

// SteveCollectionPath is a collection of Rancher's steve API at the server
// root: /v1/<resource>, the resource named as steve spells it
// ("management.cattle.io.clusters"). A continuation token goes through
// WithQuery. The segment goes through ValidSegment.
func SteveCollectionPath(resource string) (Path, error) {
	return build([]string{"v1", resource})
}

// WithQuery returns p with key=value appended to its query, both
// URL-encoded, so a value can never reach the path. A zero Path stays
// zero.
func (p Path) WithQuery(key, value string) Path {
	if p.path == "" {
		return p
	}
	sep := "?"
	if strings.Contains(p.path, "?") {
		sep = "&"
	}
	return Path{path: p.path + sep + url.QueryEscape(key) + "=" + url.QueryEscape(value)}
}

// apiRoot is the proxy prefix and API root for a cluster and group, as
// segments; the core group has no group segment and uses /api.
func apiRoot(clusterID, group, version string) []string {
	segs := []string{"k8s", "clusters", clusterID}
	if group == "" {
		return append(segs, "api", version)
	}
	return append(segs, "apis", group, version)
}

// build joins validated segments into a Path.
func build(segs []string) (Path, error) {
	for _, s := range segs {
		if !ValidSegment(s) {
			return Path{}, ErrInvalidSegment
		}
	}
	return Path{path: "/" + strings.Join(segs, "/")}, nil
}
