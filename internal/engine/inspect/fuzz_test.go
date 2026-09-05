package inspect

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzOwnerTargetRoundTrip fuzzes ownerTarget/splitOwnerTarget/
// escapeTargetComponent against server-derived strings this CLI never
// validated. Never call t.Parallel() here - it panics inside f.Fuzz.
func FuzzOwnerTargetRoundTrip(f *testing.F) {
	f.Add("", "v1", "Pod", "default", "web-1", "u-1")
	f.Add("c-aaa", "apps/v1", "ReplicaSet", "web", "web-7d9", "u-2")
	f.Add("", "apps/v1", "Rep|Set", "ns", "n", "u")
	f.Add("", "a%b", "k", "ns", "n", "u")
	f.Add("", "a%7Cb", "k", "ns", "n", "u")
	f.Add("", "a%25b", "k", "ns", "n", "u")
	f.Add("", "", "", "", "", "")
	f.Add("|", "|", "|", "|", "|", "|")
	f.Add("%25", "%7C", "%", "%%", "a|b|c", "u")

	f.Fuzz(func(t *testing.T, clusterID, apiVersion, kind, ns, name, uid string) {
		// (a) no panic on arbitrary raw targets - malformed arity
		// legitimately yields ok=false. Two probes: a truly arbitrary
		// string (reaches zero-through-five-field arities the six-join
		// below never can - it always carries five separators), and the
		// unescaped six-join (reaches six-and-over arities).
		splitOwnerTarget(uid)
		raw := strings.Join([]string{clusterID, apiVersion, kind, ns, name, uid}, ownerTargetSep)
		splitOwnerTarget(raw)

		// (b) split-then-rejoin fix-point, scoped to targets ownerTarget
		// builds from these fuzzed components. splitOwnerTarget yields the
		// ESCAPED form of each component; rejoining with a plain
		// strings.Join (never ownerTarget, which would double-escape)
		// must reproduce the constructed target byte-for-byte.
		ref := ownerRef{apiVersion: apiVersion, kind: kind, name: name, uid: uid}
		target := ownerTarget(clusterID, ns, ref)

		wantFields := []string{
			escapeTargetComponent(apiVersion), escapeTargetComponent(kind),
			escapeTargetComponent(ns), escapeTargetComponent(name), escapeTargetComponent(uid),
		}
		if clusterID != "" {
			wantFields = append([]string{escapeTargetComponent(clusterID)}, wantFields...)
		}
		assert.Equal(t, strings.Join(wantFields, ownerTargetSep), target)

		parts, ok := splitOwnerTarget(target)
		require.True(t, ok)

		gotFields := []string{parts.APIVersion, parts.Kind, parts.Namespace, parts.Name, parts.UID}
		if clusterID != "" {
			gotFields = append([]string{parts.ClusterID}, gotFields...)
		}
		assert.Equal(t, target, strings.Join(gotFields, ownerTargetSep))

		// (c) injectivity: distinct fuzzed inputs must escape to distinct
		// outputs, or two different owners would collapse into one target.
		if escapeTargetComponent(apiVersion) == escapeTargetComponent(kind) {
			assert.Equal(t, apiVersion, kind)
		}
	})
}
