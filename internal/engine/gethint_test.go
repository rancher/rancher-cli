package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// getRefusal is the 403 the Kubernetes authorizer writes for a request of
// verb on name of gvr in ns, as the cluster client returns it.
func getRefusal(verb string, gvr schema.GroupVersionResource, ns, name string) error {
	where := " at the cluster scope"
	if ns != "" {
		where = fmt.Sprintf(" in the namespace %q", ns)
	}
	se := apierrors.NewForbidden(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, name,
		fmt.Errorf("User %q cannot %s resource %q in API group %q%s", "u-update", verb, gvr.Resource, gvr.Group, where))
	return clierr.FromStatus(se, &clierr.Request{Verb: verb, Group: gvr.Group, Resource: gvr.Resource, Name: name})
}

// TestWithMissingGetHintAddsTheHintOnlyToTheAuthorizersRefusalOfThatGet
// asserts the hint names the command, the kind, get on the resource with its
// group and the verb of the write, and leaves out the verb when there is
// none; and that every other error is returned as it is: a refusal of
// another verb or object, a refusal that already has a hint, an error that
// is not a clierr error, and nil.
func TestWithMissingGetHintAddsTheHintOnlyToTheAuthorizersRefusalOfThatGet(t *testing.T) {
	t.Parallel()
	configMaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	clusters := schema.GroupVersionResource{Group: "provisioning.cattle.io", Version: "v1", Resource: "clusters"}
	hinted := getRefusal("get", configMaps, "ns1", "cm1")
	ce, ok := errors.AsType[*clierr.Error](hinted)
	require.True(t, ok)
	_ = ce.WithHint("an earlier hint")
	plain := errors.New("connection refused")
	rows := []struct {
		name     string
		err      error
		command  string
		kind     string
		gvr      schema.GroupVersionResource
		ns, obj  string
		verb     string
		wantHint string
		same     bool
	}{
		{name: "get of a core resource in a namespace", err: getRefusal("get", configMaps, "ns1", "cm1"), command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update",
			wantHint: "this patch reads the configmap first; your role needs permission to get configmaps as well as update"},
		{name: "get of a resource of a named group, with no verb of the write", err: getRefusal("get", clusters, "fleet-default", "c1"), command: "command", kind: "provisioning-cluster", gvr: clusters, ns: "fleet-default", obj: "c1",
			wantHint: "this command reads the provisioning-cluster first; your role needs permission to get clusters.provisioning.cattle.io"},
		{name: "refusal of another verb", err: getRefusal("list", configMaps, "ns1", "cm1"), command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update", same: true},
		{name: "refusal of another object", err: getRefusal("get", configMaps, "ns1", "cm2"), command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update", same: true},
		{name: "refusal that already has a hint", err: hinted, command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update", same: true},
		{name: "error that is not a clierr error", err: plain, command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update", same: true},
		{name: "nil", command: "patch", kind: "configmap", gvr: configMaps, ns: "ns1", obj: "cm1", verb: "update", same: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			var before string
			if ce, ok := errors.AsType[*clierr.Error](r.err); ok {
				before = ce.Hint
			}
			got := WithMissingGetHint(r.err, r.command, r.kind, r.gvr, r.ns, r.obj, r.verb)
			if r.same {
				assert.Equal(t, r.err, got)
				if ce, ok := errors.AsType[*clierr.Error](got); ok {
					assert.Equal(t, before, ce.Hint)
				}
				return
			}
			ce, ok := errors.AsType[*clierr.Error](got)
			require.True(t, ok)
			assert.Equal(t, r.wantHint, ce.Hint)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			require.ErrorContains(t, got, `cannot get resource "`+r.gvr.Resource+`"`)
			in, ok := errors.AsType[*clierr.Error](r.err)
			require.True(t, ok)
			assert.Empty(t, in.Hint, "the refusal passed in is not changed")
		})
	}
}
