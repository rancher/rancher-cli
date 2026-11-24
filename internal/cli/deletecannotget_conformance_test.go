package cli

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// deleteNeedsGet are the delete commands that read the object before they
// delete it when the caller cannot get it, each with its reason. Every other
// delete command the registry builds deletes an object the caller cannot get
// with a uid precondition; a command added later does so or is listed here.
var deleteNeedsGet = map[string]string{
	"delete project":                 "the preview of the namespaces Rancher deletes or keeps comes from the project object",
	"delete token":                   "the ext.cattle.io store checks the uid precondition in its own code",
	"delete kubeconfig":              "the ext.cattle.io store checks the uid precondition in its own code",
	"delete etcd-snapshot-save":      "the command reads status.phase to warn that deleting an operation that has not ended can leave the cluster in an unstable state",
	"delete etcd-snapshot-restore":   "the command reads status.phase to warn that deleting an operation that has not ended can leave the cluster in an unstable state",
	"delete encryption-key-rotation": "the command reads status.phase to warn that deleting an operation that has not ended can leave the cluster in an unstable state",
	"delete certificate-rotation":    "the command reads status.phase to warn that deleting an operation that has not ended can leave the cluster in an unstable state",
}

// dwUIDWarning is the warning of the Effect of a delete of an object the
// caller cannot get.
func dwUIDWarning(kind string) string {
	return "you cannot get " + kind + " " + dwObject + ": the delete sends a uid precondition (u1), not a resourceVersion precondition. " +
		"It fails if the object is deleted and created again after this preview; it does not fail if the object only changes"
}

// dwNoWaitWarning is the warning of a delete of an object the caller cannot
// get that does not wait for it.
func dwNoWaitWarning(kind string) string {
	return "you cannot get " + kind + " " + dwObject + "; the command does not wait for it"
}

// dwGetHint is the hint of a delete that needs get on the object it deletes.
func dwGetHint(k kinds.Kind) string {
	resource := k.GVR.Resource
	if k.GVR.Group != "" {
		resource += "." + k.GVR.Group
	}
	return "this delete reads the " + k.Name + " first; your role needs permission to get " + resource + " as well as delete"
}

// dwPlan plans the row's command against the clients of f and returns its
// Effect and executor.
func dwPlan(t *testing.T, r dwRow, f *dwFactory, namer engine.SessionTokenNamer, in fieldCoverageInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	cmd, ok := r.e.make(runDeps{clients: f, roots: failingRoots{}, sessionNamer: namer}).(runtime.MutatingCommand)
	require.True(t, ok, "%s is a mutating command", r.name)
	return cmd.Plan(t.Context(), in)
}

// dwRequireNoDelete asserts no delete of dwObject was sent, not even a dry
// run.
func dwRequireNoDelete(t *testing.T, s *dwServer) {
	t.Helper()
	reqs := s.requests()
	assert.Zero(t, s.count("delete", dwObject, reqs)+s.count("dry-run delete", dwObject, reqs), "no delete is sent")
}

// dwRequireGetRefusal asserts err is the refusal of the GET with the hint
// that names the missing verb, and that no delete was sent.
func dwRequireGetRefusal(t *testing.T, r dwRow, s *dwServer, err error) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "a clierr error: %v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, 6, ce.ExitCode())
	require.ErrorContains(t, ce, `cannot get resource "`+r.k.GVR.Resource+`"`)
	assert.Equal(t, dwGetHint(r.k), ce.Hint)
	dwRequireNoDelete(t, s)
}

// dwRequireUIDDeletes asserts the requests for dwObject: the refused GET, a
// dry-run delete with no precondition, then the delete with the uid of the
// response to the dry-run delete and no resourceVersion, and nothing after it.
func dwRequireUIDDeletes(t *testing.T, s *dwServer) {
	t.Helper()
	var got []dwRequest
	for _, req := range s.requests() {
		if req.name == dwObject {
			got = append(got, req)
		}
	}
	require.Len(t, got, 3, "%+v", got)
	assert.Equal(t, "get", got[0].verb)
	assert.Equal(t, "dry-run delete", got[1].verb)
	assert.Empty(t, got[1].uid, "the dry-run delete carries no precondition")
	assert.Empty(t, got[1].rv, "the dry-run delete carries no precondition")
	assert.Equal(t, "delete", got[2].verb)
	assert.Equal(t, "u1", got[2].uid, "the delete carries the uid in the response to the dry-run delete")
	assert.Empty(t, got[2].rv, "the delete carries no resourceVersion")
	require.NotNil(t, s.afterDelete())
	assert.Empty(t, s.afterDelete(), "nothing is sent after the delete")
}

// TestEveryDeleteDeletesAnObjectTheCallerCannotGet asserts, for every delete
// command the registry builds, what it does when the Kubernetes authorizer
// refuses the caller permission to get the object: a command listed in
// deleteNeedsGet returns the refusal with the hint that names the missing verb
// and sends no delete; every other command sends a dry-run delete with no
// precondition, then the delete with the uid in the response to the dry-run
// delete, says so in the Effect (uid set, no resourceVersion,
// conflictDetection false, a warning) and the result (deletion started,
// waited false, a warning unless --wait=false), sends nothing after the delete
// and exits 0. A client that cannot send a uid precondition sends no delete.
func TestEveryDeleteDeletesAnObjectTheCallerCannotGet(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, r := range dwRows(t) {
		seen[r.command] = true
		if _, ok := deleteNeedsGet[r.command]; ok {
			t.Run(r.name+" needs get", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := newDWServer()
					s.refuseGet = true
					_, _, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
					dwRequireGetRefusal(t, r, s, err)
				})
			})
			continue
		}

		t.Run(r.name+" default", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.refuseGet = true
				eff, exec, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
				require.NoError(t, err)
				assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
				assert.Equal(t, safety.ActionDelete, eff.Action)
				assert.Equal(t, "u1", eff.UID)
				assert.Empty(t, eff.ResourceVersion)
				assert.False(t, eff.ConflictDetection)
				assert.True(t, eff.Destructive)
				assert.Contains(t, eff.Warnings, dwUIDWarning(r.k.Name))
				assert.Contains(t, eff.Warnings, dwNoWaitWarning(r.k.Name))
				res, err := exec(t.Context())
				dwRun{res: res, execErr: err}.requireSuccess(t, false, []string{dwNoWaitWarning(r.k.Name)})
				dwRequireUIDDeletes(t, s)
			})
		})

		t.Run(r.name+" --wait=false", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.refuseGet = true
				eff, exec, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, map[string]bool{"wait": false}))
				require.NoError(t, err)
				assert.NotContains(t, eff.Warnings, dwNoWaitWarning(r.k.Name))
				res, err := exec(t.Context())
				dwRun{res: res, execErr: err}.requireSuccess(t, false, nil)
				dwRequireUIDDeletes(t, s)
			})
		})

		t.Run(r.name+" through the pipeline", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.refuseGet = true
				status, ce := dwRunPipeline(t.Context(), t, wr.reg, r, s, nil, r.input(nil, nil), false)
				require.Nil(t, ce)
				assert.Equal(t, 0, status)
				dwRequireUIDDeletes(t, s)
			})
		})

		t.Run(r.name+" dry run", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.refuseGet = true
				var out, errOut bytes.Buffer
				status, ce := runtime.Run(t.Context(), r.e.make(runDeps{clients: &dwFactory{s: s}, roots: failingRoots{}}), r.input(nil, nil), runtime.IO{
					Out: &out, Err: &errOut, Format: "json", DryRun: true,
					Capability: scrubAllowAll{}, Projects: scrubProjects{},
				}, wr.reg.Binding(r.command))
				require.Nil(t, ce)
				assert.Equal(t, 0, status)
				env := decodeEnvelope(t, "json", out.String())
				require.Equal(t, "Effect", env["kind"])
				data, _ := env["data"].(map[string]any)
				assert.Equal(t, "u1", data["uid"])
				assert.Equal(t, false, data["conflictDetection"])
				assert.Equal(t, string(safety.PreviewServerDryRun), data["previewSource"])
				assert.NotContains(t, data, "resourceVersion")
				assert.Zero(t, s.count("delete", dwObject, s.requests()), "a dry run sends no delete")
				assert.Equal(t, 1, s.count("dry-run delete", dwObject, s.requests()))
			})
		})

		t.Run(r.name+" a client that cannot send a uid precondition", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.refuseGet = true
				_, _, err := dwPlan(t, r, &dwFactory{s: s, noUID: true}, nil, r.input(nil, nil))
				dwRequireGetRefusal(t, r, s, err)
			})
		})
	}
	for name := range deleteNeedsGet {
		assert.True(t, seen[name], "%s is listed in deleteNeedsGet and is not a delete command the registry builds; remove it", name)
	}
}

// dwRowNamed is the row of dwRows with that name.
func dwRowNamed(t *testing.T, name string) dwRow {
	t.Helper()
	for _, r := range dwRows(t) {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no row %s", name)
	return dwRow{}
}

// dwConflict is a 409 with message on the delete of a ConfigMap, as the
// cluster client returns it for a delete with a uid precondition.
func dwConflict(message string) error {
	se := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, dwObject, errors.New("x"))
	se.ErrStatus.Message = message
	return clierr.FromStatus(se, &clierr.Request{Verb: "delete", Resource: "configmaps", Name: dwObject})
}

// TestADeleteOfAnObjectTheCallerCannotGetWhenARequestFails asserts
// the delete of an object
// the caller cannot get when a request fails: a 409 that says the uid is not
// the object's is a CONFLICT that says nothing was deleted and names the
// dry-run delete, a webhook's 409 keeps the server's message, a refused
// dry-run delete names the refused read too and sends no delete, a dry-run
// response with no uid and a 403 that is not the authorizer's refusal of get
// send no delete, and the delete of the token this command uses needs get.
func TestADeleteOfAnObjectTheCallerCannotGetWhenARequestFails(t *testing.T) {
	t.Parallel()
	r := dwRowNamed(t, "delete configmap")
	uidText := `Operation cannot be fulfilled on configmaps "obj1": the UID in the precondition (u1) does not match the UID in record (u2). The object might have been deleted and then recreated`
	webhookText := `admission webhook "x.example.io" denied the request: duplicate not allowed`

	t.Run("the object was deleted and created again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.refuseGet = true
			s.deleteErr = dwConflict(uidText)
			_, exec, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
			require.NoError(t, err)
			_, err = exec(t.Context())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "a clierr error: %v", err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Equal(t, 5, ce.ExitCode())
			assert.False(t, ce.Retryable)
			assert.Equal(t, "configmap obj1 was deleted and created again after the preview; this command deleted nothing. "+
				"The configmap named obj1 now is a new object, and you have not previewed it: preview it before you delete it", ce.Message)
			assert.Equal(t, clierr.Rancher+" delete configmap 'obj1' --cluster 'c-test' --namespace 'ns1' --dry-run", ce.Hint)
			assert.Nil(t, ce.Details, "the delete was not accepted: no committed details")
		})
	})

	t.Run("an admission webhook's 409", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.refuseGet = true
			s.deleteErr = dwConflict(webhookText)
			_, exec, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
			require.NoError(t, err)
			_, err = exec(t.Context())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "a clierr error: %v", err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.False(t, ce.Retryable)
			assert.Equal(t, webhookText, ce.Message)
			assert.NotContains(t, ce.Message, "created again")
		})
	})

	t.Run("the dry-run delete is refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.refuseGet = true
			s.dryRunErr = clierr.New(clierr.CodeRBAC, `configmaps "obj1" is forbidden: User "u-delete" cannot delete resource "configmaps" in API group "" in the namespace "ns1"`)
			_, _, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "a clierr error: %v", err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Equal(t, `configmaps "obj1" is forbidden: User "u-delete" cannot delete resource "configmaps" in API group "" in the namespace "ns1"; `+
				`the command also could not get configmap obj1: configmaps "obj1" is forbidden: User "u-delete" cannot get resource "configmaps" in API group "" in the namespace "ns1"`, ce.Message)
			assert.Zero(t, s.count("delete", dwObject, s.requests()), "no delete is sent")
		})
	})

	t.Run("the dry-run response has no uid", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.refuseGet = true
			s.noResponseUID = true
			_, _, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "a clierr error: %v", err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Equal(t, dwGetHint(r.k), ce.Hint)
			assert.Zero(t, s.count("delete", dwObject, s.requests()), "no delete is sent")
		})
	})

	t.Run("a 403 with no reason", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.getErr = clierr.FromStatus(clierr.ResponseError(http.MethodGet, http.StatusForbidden, nil), &clierr.Request{Verb: "get", Resource: "configmaps", Name: dwObject})
			_, _, err := dwPlan(t, r, &dwFactory{s: s}, nil, r.input(nil, nil))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "a clierr error: %v", err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Empty(t, ce.Hint)
			dwRequireNoDelete(t, s)
		})
	})

	t.Run("the token this command uses", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tokenRow := dwRowNamed(t, "delete management-token")
			s := newDWServer()
			s.refuseGet = true
			_, _, err := dwPlan(t, tokenRow, &dwFactory{s: s}, dwNamer{id: dwObject}, tokenRow.input(nil, nil))
			dwRequireGetRefusal(t, tokenRow, s, err)
		})
	})
}

// TestTheClusterClientCanDeleteWithAUIDPrecondition asserts the client
// clientFactory.Cluster returns implements engine.UIDDeleter: a client that
// lacks it sends no delete of an object the caller cannot get.
func TestTheClusterClientCanDeleteWithAUIDPrecondition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: "acme", Servers: map[string]config.Server{"acme": {URL: "https://rancher.invalid"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	c, err := newClientFactory(dir, &countingStore{token: "id:secret"}).Cluster(t.Context(), "c-test")
	require.NoError(t, err)
	_, ok := c.(engine.UIDDeleter)
	assert.True(t, ok, "%T implements engine.UIDDeleter", c)
}

// apiServedBy is who serves a group.
type apiServedBy string

const (
	servedBuiltIn    apiServedBy = "Kubernetes built-in"
	servedCRD        apiServedBy = "CustomResourceDefinition"
	servedAggregated apiServedBy = "aggregated API"
)

// deleteGroups classifies the API group of every kind with a delete command. A
// built-in or CRD group is served by the generic store, which checks a uid
// precondition. An aggregated API checks it, or not, in its own code: every
// row of such a group is listed in deleteNeedsGet or named in
// aggregatedDeleteProbes.
var deleteGroups = map[string]apiServedBy{
	"":                             servedBuiltIn,
	"apps":                         servedBuiltIn,
	"autoscaling":                  servedBuiltIn,
	"batch":                        servedBuiltIn,
	"networking.k8s.io":            servedBuiltIn,
	"policy":                       servedBuiltIn,
	"storage.k8s.io":               servedBuiltIn,
	"fleet.cattle.io":              servedCRD,
	"k3k.io":                       servedCRD,
	"management.cattle.io":         servedCRD,
	"operation.cattle.io":          servedCRD,
	"provisioning.cattle.io":       servedCRD,
	"rke-machine-config.cattle.io": servedCRD,
	"ext.cattle.io":                servedAggregated,
}

// aggregatedDeleteProbes names, for a delete command of an aggregated-API
// group that deletes an object the caller cannot get, the fidelity probe that
// asserts its store checks the uid precondition. None does today.
var aggregatedDeleteProbes = map[string]string{}

// TestEveryDeleteGroupIsClassified asserts the group of every delete command the
// registry builds is in deleteGroups, that every listed group has a delete
// command, and that every delete command of an aggregated-API group is listed
// in deleteNeedsGet or has a probe in aggregatedDeleteProbes.
func TestEveryDeleteGroupIsClassified(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, r := range dwRows(t) {
		group := r.k.GVR.Group
		used[group] = true
		served, ok := deleteGroups[group]
		if !assert.True(t, ok, "%s: group %q is not classified in deleteGroups", r.command, group) {
			continue
		}
		if served != servedAggregated {
			continue
		}
		_, needsGet := deleteNeedsGet[r.command]
		_, probed := aggregatedDeleteProbes[r.command]
		assert.True(t, needsGet || probed, "%s: an aggregated-API delete is listed in deleteNeedsGet or has a fidelity probe", r.command)
	}
	for group := range deleteGroups {
		assert.True(t, used[group], "group %q is classified and has no delete command", group)
	}
}
