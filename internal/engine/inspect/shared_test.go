package inspect

import (
	"context"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestCompositesRejectProjectScope asserts PC-3's composite-rejection rule: the
// non-project inspect composites bypass Route, so without an explicit check
// the global --project flag would reach them and be silently ignored - the
// exact no-op class DR-004 removed --project for.
func TestCompositesRejectProjectScope(t *testing.T) {
	tests := []struct {
		name string
		cmd  runtime.Command
		in   runtime.Input
	}{
		{"inspect pod", NewPod(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"inspect deployment", NewDeployment(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"images", NewImages(&fakeFactory{}),
			input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}}},
		{"inspect user", NewInspectUser(&fakeFactory{}),
			input{args: []string{"u-abc"}, s: map[string]string{"project": "p-x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execRead(t, tt.cmd, tt.in)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, tt.name)
		})
	}
}

// ownerChainWith walks the chain with its own accumulator and returns what a
// caller would publish: the owners plus the finalized warnings.
func ownerChainWith(t *testing.T, client engine.ClusterClient, ns string, pod *unstructured.Unstructured) ([]map[string]any, []string) {
	t.Helper()
	sec := newSections(t.Context(), sectionOwners)
	owners, _, err := ownerChain(t.Context(), client, sec, sectionOwners, "", ns, pod)
	require.NoError(t, err)
	_, warnings := sec.Finalize()
	return owners, warnings
}

func TestOwnerChainUIDMismatchStopsWalk(t *testing.T) {
	t.Parallel()
	rsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	rs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name":      "web-7d9f8b",
			"namespace": "web",
			"uid":       "rs-uid-NEW",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"name": "web", "uid": "dep-uid-1", "controller": true,
			}},
		},
	}}
	client := newFakeClient().withObject(rsGVR, "web", "web-7d9f8b", rs)
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "web-7d9f8b-x1", "namespace": "web",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"name": "web-7d9f8b", "uid": "rs-uid-OLD", "controller": true,
			}},
		},
	}}
	owners, warnings := ownerChainWith(t, client, "web", pod)
	require.Len(t, owners, 1, "walk must stop at the stale ReplicaSet ref")
	assert.Equal(t, "ReplicaSet", owners[0]["kind"])
	assert.Equal(t, "rs-uid-OLD", owners[0]["uid"])
	assert.Equal(t, "apps/v1", owners[0]["apiVersion"])
	require.Len(t, warnings, 1)
	assert.Equal(t,
		"owners unavailable for apps/v1|ReplicaSet|web|web-7d9f8b|rs-uid-OLD: uid mismatch (stale owner reference)",
		warnings[0])
}

func TestOwnerChainTargetIncludesNamespaceAndGroup(t *testing.T) {
	t.Parallel()
	rsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	client := newFakeClient().withGetErr(rsGVR, "ns-a", "same-name", fmt.Errorf("boom"))
	pod := func(ns string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{
				"name": "p", "namespace": ns,
				"ownerReferences": []any{map[string]any{
					"apiVersion": "apps/v1", "kind": "ReplicaSet",
					"name": "same-name", "uid": "u-a", "controller": true,
				}},
			},
		}}
	}
	_, warnA := ownerChainWith(t, client, "ns-a", pod("ns-a"))
	require.Len(t, warnA, 1)
	assert.Equal(t, "owners unavailable for apps/v1|ReplicaSet|ns-a|same-name|u-a: boom", warnA[0])
}

func TestOwnerChainNoUIDStopsWalk(t *testing.T) {
	t.Parallel()
	client := newFakeClient() // no objects registered; Get must never be called
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "p", "namespace": "ns",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"name": "web-rs", "uid": "", "controller": true,
			}},
		},
	}}
	owners, warnings := ownerChainWith(t, client, "ns", pod)
	require.Len(t, owners, 1, "walk must append the ref then stop on empty uid")
	assert.Equal(t, "ReplicaSet", owners[0]["kind"])
	assert.Empty(t, client.getCalls, "Get must not be called when the owner reference has no uid")
	require.Len(t, warnings, 1)
	assert.Equal(t,
		"owners unavailable for apps/v1|ReplicaSet|ns|web-rs|: owner reference has no uid",
		warnings[0])
}

func TestSectionsRecordsEntryAndPairedWarning(t *testing.T) {
	t.Parallel()
	sec := newSections(t.Context(), sectionNamespaces)
	require.NoError(t, sec.Unavailable(sectionNamespaces, clierr.New(clierr.CodeRBAC, "denied")))
	entries, warnings := sec.Finalize()
	assert.Equal(t, []sectionOutcome{{
		Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeRBAC,
	}}, entries)
	assert.Equal(t, []string{"namespaces unavailable: denied"}, warnings)
}

func TestSectionsBlockedNamesTheDependency(t *testing.T) {
	t.Parallel()
	sec := newSections(t.Context(), sectionUsage)
	require.NoError(t, sec.Blocked(sectionUsage, sectionNamespaces, clierr.New(clierr.CodeRBAC, "denied")))
	entries, warnings := sec.Finalize()
	// The code comes from the ORIGINAL error, not from the dependency context.
	assert.Equal(t, clierr.CodeRBAC, entries[0].Code)
	assert.Equal(t, stateUnavailable, entries[0].State)
	assert.Equal(t, []string{"usage unavailable: namespaces unknown (denied)"}, warnings)
}

func TestSectionsPartialCarriesTarget(t *testing.T) {
	t.Parallel()
	sec := newSections(t.Context(), sectionLogs)
	require.NoError(t, sec.Partial(sectionLogs, "app", fmt.Errorf("container not ready")))
	entries, warnings := sec.Finalize()
	assert.Equal(t, []sectionOutcome{{
		Section: sectionLogs, State: statePartial, Code: clierr.CodeGeneric, Target: "app",
	}}, entries)
	assert.Equal(t, []string{"logs unavailable for app: container not ready"}, warnings)
}

// TestSectionsFinalizeSortsAndKeepsParity verifies the two determinism rules
// the agent contract rests on: entries come out sorted by (section, target),
// and every entry keeps its own warning at the same index.
func TestSectionsFinalizeSortsAndKeepsParity(t *testing.T) {
	t.Parallel()
	sec := newSections(t.Context(), sectionLogs, sectionMetrics)
	require.NoError(t, sec.Partial(sectionLogs, "web", fmt.Errorf("w")))
	require.NoError(t, sec.Unavailable(sectionMetrics, fmt.Errorf("m")))
	require.NoError(t, sec.Partial(sectionLogs, "app", fmt.Errorf("a")))
	entries, warnings := sec.Finalize()
	require.Len(t, entries, 3)
	require.Len(t, warnings, len(entries))
	assert.Equal(t, []string{"app", "web", ""},
		[]string{entries[0].Target, entries[1].Target, entries[2].Target})
	assert.Equal(t, []string{sectionLogs, sectionLogs, sectionMetrics},
		[]string{entries[0].Section, entries[1].Section, entries[2].Section})
	assert.Equal(t, []string{
		"logs unavailable for app: a",
		"logs unavailable for web: w",
		"metrics unavailable: m",
	}, warnings)
}

func TestSectionsFinalizeEmptyIsNil(t *testing.T) {
	t.Parallel()
	entries, warnings := newSections(t.Context(), sectionMetrics).Finalize()
	assert.Nil(t, entries, "an undegraded payload must omit the field, never carry []")
	assert.Nil(t, warnings)
}

// TestSectionsCodeFallsBackToGeneric verifies the code contract: a known
// clierr code survives, an unknown one and a plain error become GENERIC.
func TestSectionsCodeFallsBackToGeneric(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"known code", clierr.New(clierr.CodeNotFound, "gone"), clierr.CodeNotFound},
		{"unknown code", clierr.New("MADE_UP", "huh"), clierr.CodeGeneric},
		{"plain error", fmt.Errorf("boom"), clierr.CodeGeneric},
		{"wrapped code", fmt.Errorf("ctx: %w", clierr.New(clierr.CodeRBAC, "denied")), clierr.CodeRBAC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sec := newSections(t.Context(), sectionMetrics)
			require.NoError(t, sec.Unavailable(sectionMetrics, tt.err))
			entries, _ := sec.Finalize()
			assert.Equal(t, tt.want, entries[0].Code)
		})
	}
}

// TestSectionsCancellationReturnsOriginalError is the gate every degradation
// family relies on: a caller abort is a hard error, never a degraded section.
func TestSectionsCancellationReturnsOriginalError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	boom := fmt.Errorf("aborted")
	tests := []struct {
		name string
		call func(*sections) error
	}{
		{"unavailable", func(s *sections) error { return s.Unavailable(sectionMetrics, boom) }},
		{"partial", func(s *sections) error { return s.Partial(sectionLogs, "app", boom) }},
		{"blocked", func(s *sections) error { return s.Blocked(sectionUsage, sectionNamespaces, boom) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sec := newSections(ctx, sectionMetrics, sectionLogs, sectionUsage)
			assert.Same(t, boom, tt.call(sec))
			entries, warnings := sec.Finalize()
			assert.Empty(t, entries, "a canceled lookup records nothing")
			assert.Empty(t, warnings)
		})
	}
}

// TestSectionsRejectsContractViolations covers the central validation. Every
// case is a programmer error - the vocabulary, states, and target grammars
// are fixed in code and no server value can reach them.
func TestSectionsRejectsContractViolations(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("boom")
	tests := []struct {
		name string
		call func()
	}{
		{"unknown section name", func() { newSections(t.Context(), "invented") }},
		{"section outside the command's vocabulary", func() {
			_ = newSections(t.Context(), sectionMetrics).Unavailable(sectionLogs, err)
		}},
		{"whole section given a target", func() {
			_ = newSections(t.Context(), sectionMetrics).Partial(sectionMetrics, "x", err)
		}},
		{"per-item section reported whole", func() {
			_ = newSections(t.Context(), sectionLogs).Unavailable(sectionLogs, err)
		}},
		{"owner target is not canonical", func() {
			_ = newSections(t.Context(), sectionOwners).Partial(sectionOwners, "web", err)
		}},
		{"duplicate section and target", func() {
			sec := newSections(t.Context(), sectionLogs)
			_ = sec.Partial(sectionLogs, "app", err)
			_ = sec.Partial(sectionLogs, "app", err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Panics(t, tt.call)
		})
	}
}

// TestOwnerTargetRoundTrip verifies the canonical encoding survives both the
// core group and a grouped apiVersion, with and without a cluster prefix -
// the ambiguity a slash-joined target would have introduced.
func TestOwnerTargetRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		clusterID string
		ns        string
		ref       ownerRef
		want      string
		parts     ownerTargetParts
	}{
		{
			name: "core group", ns: "default",
			ref:  ownerRef{apiVersion: "v1", kind: "Pod", name: "web-1", uid: "u-1"},
			want: "v1|Pod|default|web-1|u-1",
			parts: ownerTargetParts{
				APIVersion: "v1", Kind: "Pod", Namespace: "default", Name: "web-1", UID: "u-1",
			},
		},
		{
			name: "grouped api version", ns: "web",
			ref:  ownerRef{apiVersion: "apps/v1", kind: "ReplicaSet", name: "web-7d9", uid: "u-2"},
			want: "apps/v1|ReplicaSet|web|web-7d9|u-2",
			parts: ownerTargetParts{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: "web", Name: "web-7d9", UID: "u-2",
			},
		},
		{
			name: "cluster prefixed", clusterID: "c-aaa", ns: "web",
			ref:  ownerRef{apiVersion: "apps/v1", kind: "ReplicaSet", name: "web-7d9", uid: "u-2"},
			want: "c-aaa|apps/v1|ReplicaSet|web|web-7d9|u-2",
			parts: ownerTargetParts{
				ClusterID: "c-aaa", APIVersion: "apps/v1", Kind: "ReplicaSet",
				Namespace: "web", Name: "web-7d9", UID: "u-2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := ownerTarget(tt.clusterID, tt.ns, tt.ref)
			assert.Equal(t, tt.want, target)
			parts, ok := splitOwnerTarget(target)
			require.True(t, ok)
			assert.Equal(t, tt.parts, parts)
			assert.Equal(t, target, ownerTarget(parts.ClusterID, parts.Namespace, ownerRef{
				apiVersion: parts.APIVersion, kind: parts.Kind, name: parts.Name, uid: parts.UID,
			}))
		})
	}
}

// TestOwnerTargetEscapesDelimiterInComponent verifies the grammar holds on
// server data this CLI never validated: a component carrying the delimiter is
// escaped, so the target still splits into the right number of components and
// nothing crashes.
func TestOwnerTargetEscapesDelimiterInComponent(t *testing.T) {
	t.Parallel()
	target := ownerTarget("", "ns", ownerRef{apiVersion: "apps/v1", kind: "Rep|Set", name: "n", uid: "u"})
	assert.Equal(t, "apps/v1|Rep%7CSet|ns|n|u", target)
	parts, ok := splitOwnerTarget(target)
	require.True(t, ok)
	assert.Equal(t, "Rep%7CSet", parts.Kind)
}

// TestTargetEscapingStaysOneToOne verifies the escape is injective: a
// component that already looks escaped must not collide with one that
// carries the delimiter, or two distinct items would dedupe into one entry.
func TestTargetEscapingStaysOneToOne(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a%257Cb", escapeTargetComponent("a%7Cb"))
	assert.Equal(t, "a%7Cb", escapeTargetComponent("a|b"))
	assert.Equal(t, "plain", escapeTargetComponent("plain"), "an ordinary value is untouched")
}

// TestSectionsEscapesTokenTarget verifies a server-derived token target - a
// container name, a cluster id - cannot break the owner-target grammar, and
// that the entry and its warning carry the same escaped value.
func TestSectionsEscapesTokenTarget(t *testing.T) {
	t.Parallel()
	sec := newSections(t.Context(), sectionClusters)
	require.NoError(t, sec.Partial(sectionClusters, "c-a|b", fmt.Errorf("boom")))
	entries, warnings := sec.Finalize()
	require.Len(t, entries, 1)
	assert.Equal(t, "c-a%7Cb", entries[0].Target)
	assert.Equal(t, []string{"clusters unavailable for c-a%7Cb: boom"}, warnings)
}

func TestKindGroupName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "deployment.apps/frontend", kindGroupName("apps/v1", "Deployment", "frontend"))
	assert.Equal(t, "pod/web-1", kindGroupName("v1", "Pod", "web-1"))
	assert.Equal(t, "pod/web-1", kindGroupName("", "pod", "web-1"))
}
