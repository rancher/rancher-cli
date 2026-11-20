package resources

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// day2Rows returns the rows of the operation.cattle.io group, and asserts the
// four rows for the objects of an imported cluster are among them.
func day2Rows(t *testing.T) []kinds.Kind {
	t.Helper()
	var rows []kinds.Kind
	var keys []string
	for _, k := range kinds.All() {
		if k.GVR.Group == "operation.cattle.io" {
			rows = append(rows, k)
			keys = append(keys, k.Key)
		}
	}
	for _, want := range []string{"etcdsnapshotsave", "etcdsnapshotrestore", "encryptionkeyrotation", "certificaterotation"} {
		require.Contains(t, keys, want, "the day-2 operation rows are missing a row")
	}
	return rows
}

// day2Status is an object whose conditions are the given ones, each a type,
// status, reason and message.
func day2Status(conds ...[4]string) *unstructured.Unstructured {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		cond := map[string]any{"type": c[0], "status": c[1]}
		if c[2] != "" {
			cond["reason"] = c[2]
		}
		if c[3] != "" {
			cond["message"] = c[3]
		}
		list = append(list, cond)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "op"},
		"status":   map[string]any{"conditions": list},
	}}
}

// runDay2Get runs `get <kind> op --cluster c-m-x --wait` (with --for when
// forExpr is not empty) against a client that answers with objs in turn.
func runDay2Get(t *testing.T, k kinds.Kind, forExpr string, objs ...*unstructured.Unstructured) (runtime.Result, error) {
	t.Helper()
	client := &seqClient{objs: objs}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	in := input{args: []string{"op"}, s: map[string]string{"cluster": "c-m-x", "timeout": "1m"}, b: map[string]bool{"wait": true}, isSet: map[string]bool{"wait": true}}
	if forExpr != "" {
		in.s["for"] = forExpr
	}
	return cmd.Execute(t.Context(), in, nil)
}

// TestImportedDay2WaitEndsOnFailedOrCanceled asserts, for every row of the
// group and both condition types, that a wait ends with the condition's reason
// and message when the condition is True, that Pending does not end it, that a
// caller waiting for the condition itself gets it as the answer, and that a
// watch event with the condition True closes the stream and the next List ends
// the wait.
func TestImportedDay2WaitEndsOnFailedOrCanceled(t *testing.T) {
	withFastPoll(t)
	for _, k := range day2Rows(t) {
		for _, failType := range []string{"Failed", "Canceled"} {
			t.Run(k.Key+"/"+failType, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ended := day2Status([4]string{"Pending", "False", "", ""}, [4]string{"Succeeded", "False", "NotSuccessful", ""}, [4]string{failType, "True", "PreflightCheckFailed", "no etcd node"})
					_, err := runDay2Get(t, k, "", ended)
					require.Error(t, err)
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok, "want a clierr.Error, got %v", err)
					assert.Equal(t, clierr.CodeGeneric, ce.Code)
					assert.False(t, ce.Retryable)
					assert.Equal(t, "op failed: PreflightCheckFailed: no etcd node", ce.Message)
					assert.Empty(t, ce.Hint, "the Stalled remedy rule does not cover these rows")

					res, err := runDay2Get(t, k, "condition="+failType, ended)
					require.NoError(t, err, "a caller waiting for %s gets it as the answer", failType)
					assert.NotNil(t, res)

					notTrue := day2Status([4]string{failType, "False", "", ""}, [4]string{"Succeeded", "True", "Finished", ""})
					_, err = runDay2Get(t, k, "", notTrue)
					require.NoError(t, err, "%s=False is no failure", failType)

					c := &parityWatch{steps: []parityStep{obj(day2Status([4]string{"Pending", "True", "WaitingForBeacon", ""})), obj(ended)}}
					_, err = waitFor(t.Context(), c, schema.GroupVersionResource{}, "", "op", waitCond{kind: "condition", name: "Succeeded", status: "True"}, time.Minute, stalledRule{failsOn: k.WaitFailsOn})
					require.ErrorContains(t, err, "op failed: PreflightCheckFailed: no etcd node")
					assert.Equal(t, 1, c.opens, "no second stream opens: the List after the event ends the wait")
					assert.Equal(t, 2, c.lists)
				})
			})
		}

		t.Run(k.Key+"/Pending continues", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pending := day2Status([4]string{"Pending", "True", "WaitingForBeacon", "waiting for beacon creation"})
				client := &seqClient{objs: []*unstructured.Unstructured{pending, pending, day2Status([4]string{"Succeeded", "True", "Finished", ""})}}
				cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
				_, err := cmd.Execute(t.Context(), input{args: []string{"op"}, s: map[string]string{"cluster": "c-m-x", "timeout": "1m"}, b: map[string]bool{"wait": true}, isSet: map[string]bool{"wait": true}}, nil)
				require.NoError(t, err)
				assert.Equal(t, 3, client.calls, "Pending is polled through")
			})
		})
	}
}

// TestWaitFailsOnNamesTheReasonAndMessage asserts the failure text leaves out an
// empty reason or message with its separator, and the same wait with no listed
// types is not affected.
func TestWaitFailsOnNamesTheReasonAndMessage(t *testing.T) {
	withFastPoll(t)
	cond := waitCond{kind: "condition", name: "Succeeded", status: "True"}
	for name, tc := range map[string]struct {
		cond [4]string
		want string
	}{
		"reason and message": {[4]string{"Failed", "True", "Boom", "it broke"}, "x failed: Boom: it broke"},
		"reason only":        {[4]string{"Failed", "True", "Boom", ""}, "x failed: Boom"},
		"message only":       {[4]string{"Failed", "True", "", "it broke"}, "x failed: it broke"},
		"neither":            {[4]string{"Failed", "True", "", ""}, "x failed"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &seqClient{objs: []*unstructured.Unstructured{day2Status(tc.cond)}}
				_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, stalledRule{failsOn: []string{"Failed", "Canceled"}})
				require.Error(t, err)
				assert.Equal(t, tc.want, clierr.From(err).Message)
			})
		})
	}
	t.Run("a row that lists none is not affected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := &seqClient{objs: []*unstructured.Unstructured{day2Status([4]string{"Failed", "True", "Boom", "it broke"})}}
			_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, 20*time.Millisecond, stalledRule{})
			require.ErrorContains(t, err, "timed out")
		})
	})
}

// day2Manifest is the manifest of the row for the generated checks of a create:
// the one createWarningManifest builds, which passes the object checks.
func day2Manifest(t *testing.T, key string) (kinds.Kind, []byte, input) {
	t.Helper()
	k, ok := kinds.ByKey(key)
	require.True(t, ok)
	manifest, in := createWarningManifest(k)
	return k, manifest, in
}

// TestImportedDay2PatchPlanRunsTheObjectCheck asserts the generated patch of
// every row refuses a change the check refuses before it sends the dry-run,
// accepts a ttl change with a dry-run, and puts the resume warning after the
// warnings of the server preview.
func TestImportedDay2PatchPlanRunsTheObjectCheck(t *testing.T) {
	t.Parallel()
	for _, k := range day2Rows(t) {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			orig := objFromJSON(t, `{"apiVersion":"operation.cattle.io/v1alpha1","kind":"`+k.K8sKind+`","metadata":{"name":"op","namespace":"c-m-x","resourceVersion":"7"},`+
				`"spec":{"paused":true,"ttl":60,"clusterRef":{"apiVersion":"management.cattle.io/v3","kind":"Cluster","name":"c-m-x"}},"status":{"phase":"InProgress"}}`)
			plan := func(patch string) (*patchClient, []string, error) {
				client := &patchClient{obj: orig.DeepCopy()}
				fac := &recFactory{client: client}
				eff, _, err := planPatch(t, NewPatch(k, fac, nil), input{args: []string{"op"}, s: map[string]string{"cluster": "c-m-x", "type": "merge", "patch": patch}})
				return client, eff.Warnings, err
			}

			client, _, err := plan(`{"spec":{"clusterRef":{"name":"c-m-y"}}}`)
			requireValidation(t, err)
			require.ErrorContains(t, err, "only spec.paused and spec.ttl of a "+k.Name+" object can change; create a new "+k.Name+" to change spec.clusterRef.name")
			assert.Empty(t, client.updates, "a refused patch sends no dry-run")

			client, warnings, err := plan(`{"spec":{"ttl":120}}`)
			require.NoError(t, err)
			require.Len(t, client.updates, 1)
			assert.True(t, client.updates[0].dryRun)
			assert.Empty(t, warnings)

			client, warnings, err = plan(`{"spec":{"paused":false}}`)
			require.NoError(t, err)
			require.Len(t, client.updates, 1, "an un-pause of a started operation is previewed")
			assert.Equal(t, []string{"resuming: " + k.CreateWarning}, warnings)
		})
	}
}

// TestImportedDay2DeletePlanWarns asserts the generated delete of every row
// adds the check's warning to the Effect when the object has not ended, sends
// the dry-run delete either way, and has no cascade warning of its own.
func TestImportedDay2DeletePlanWarns(t *testing.T) {
	t.Parallel()
	for _, k := range day2Rows(t) {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			for phase, wantWarning := range map[string]bool{"InProgress": true, "Pending": true, "Succeeded": false, "Failed": false, "Canceled": false} {
				obj := objWithRV("op", "9")
				_ = unstructured.SetNestedField(obj.Object, phase, "status", "phase")
				client := &delClient{obj: obj}
				fac := &recFactory{client: client}
				eff, _, err := planDelete(t, NewDelete(k, fac), input{args: []string{"op"}, s: map[string]string{"cluster": "c-m-x"}, b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}})
				require.NoError(t, err, phase)
				require.Len(t, client.deletes, 1, phase)
				assert.True(t, client.deletes[0].dryRun, phase)
				if wantWarning {
					assert.Equal(t, []string{"deleting this " + k.Name + " before it ends stops Rancher's work on it part way, which can leave the cluster in an unstable state"}, eff.Warnings, phase)
				} else {
					assert.Empty(t, eff.Warnings, phase)
				}
			}
		})
	}
}

// createRecorder is a client that records every Create and answers a dry-run
// the way a server does, with the object it was given.
type createRecorder struct {
	createClient
	answer func(*unstructured.Unstructured) *unstructured.Unstructured
}

func (c *createRecorder) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	out, err := c.createClient.Create(ctx, gvr, ns, obj, dryRun)
	if err == nil && c.answer != nil {
		return c.answer(out), nil
	}
	return out, err
}

// TestCreateChecksRunInOrder asserts the create check of a row runs Before the
// dry-run and After it with the server's answer, that the warnings are the
// check's and then the row's, and that an error of either ends the command
// with no write.
func TestCreateChecksRunInOrder(t *testing.T) {
	t.Parallel()
	k, manifest, in := day2Manifest(t, "etcdsnapshotsave")

	var order []string
	client := &createRecorder{answer: func(o *unstructured.Unstructured) *unstructured.Unstructured {
		order = append(order, "dry-run")
		answer := o.DeepCopy()
		answer.SetName("c-m-x-abcde")
		return answer
	}}
	var observed *unstructured.Unstructured
	var seen CreateCheckInput
	check := CreateCheck{
		Key: k.Key, Server: func() string { return "https://rancher.example" },
		Before: func(_ context.Context, in CreateCheckInput) ([]string, error) {
			order = append(order, "before")
			seen = in
			return []string{"the check's warning"}, nil
		},
		After: func(_ context.Context, _ CreateCheckInput, answer *unstructured.Unstructured) error {
			order = append(order, "after")
			observed = answer
			return nil
		},
	}
	fac := &recFactory{client: client}
	eff, exec, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": manifest}, check), in)
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []string{"before", "dry-run", "after"}, order)
	assert.Equal(t, "c-m-x-abcde", observed.GetName(), "After sees the server's dry-run answer")
	assert.Same(t, client, seen.Local, "the check reads through the client of the create")
	assert.Equal(t, "c-abc", seen.Obj.GetNamespace(), "the check sees the manifest as routed")
	assert.Equal(t, "https://rancher.example", seen.Server())
	assert.Equal(t, []string{"the check's warning", k.CreateWarning}, eff.Warnings)
	assert.False(t, eff.Reversible, "a day-2 create is not reversible")

	t.Run("an error of Before sends no dry-run", func(t *testing.T) {
		t.Parallel()
		client := &createRecorder{}
		want := clierr.New(clierr.CodeNotFound, "cluster c-m-x not found")
		bad := check
		bad.Before = func(context.Context, CreateCheckInput) ([]string, error) { return []string{"unused"}, want }
		_, _, err := planCreate(t, NewCreate(&recFactory{client: client}, mapFileSource{"m.yaml": manifest}, bad), in)
		assert.Same(t, want, err, "the error is returned as it is")
		assert.Empty(t, client.creates)
	})
	t.Run("an error of After sends no write", func(t *testing.T) {
		t.Parallel()
		client := &createRecorder{}
		want := clierr.New(clierr.CodeCapabilityUnsupported, "the connected server does not support a field")
		bad := check
		bad.After = func(context.Context, CreateCheckInput, *unstructured.Unstructured) error { return want }
		_, exec, err := planCreate(t, NewCreate(&recFactory{client: client}, mapFileSource{"m.yaml": manifest}, bad), in)
		assert.Same(t, want, err)
		assert.Nil(t, exec)
		require.Len(t, client.creates, 1)
		assert.True(t, client.creates[0].dryRun, "only the dry-run was sent")
	})
	t.Run("a check of another row does not run", func(t *testing.T) {
		t.Parallel()
		client := &createRecorder{}
		other := check
		other.Key = "etcdsnapshotrestore"
		other.Before = func(context.Context, CreateCheckInput) ([]string, error) {
			t.Error("the check of another row ran")
			return nil, nil
		}
		eff, _, err := planCreate(t, NewCreate(&recFactory{client: client}, mapFileSource{"m.yaml": manifest}, other), in)
		require.NoError(t, err)
		assert.Equal(t, []string{k.CreateWarning}, eff.Warnings)
	})
}

// TestNewCreateRefusesTwoChecksOfOneRow asserts a key given twice is an
// internal error at build, not a silent choice of one of the two.
func TestNewCreateRefusesTwoChecksOfOneRow(t *testing.T) {
	t.Parallel()
	check := CreateCheck{Key: "etcdsnapshotsave"}
	assert.PanicsWithValue(t, "internal error: NewCreate was given two checks for the kind row etcdsnapshotsave", func() {
		NewCreate(&recFactory{client: &createClient{}}, mapFileSource{}, check, CreateCheck{Key: "etcdsnapshotrestore"}, check)
	})
	assert.NotPanics(t, func() {
		NewCreate(&recFactory{client: &createClient{}}, mapFileSource{}, check, CreateCheck{Key: "etcdsnapshotrestore"})
	})
}

// TestDay2CreateRefusalsSendNoRequest asserts each refusal of the checks of the
// manifest alone is VALIDATION and sends nothing, not even the dry-run, for every
// row.
func TestDay2CreateRefusalsSendNoRequest(t *testing.T) {
	t.Parallel()
	for _, k := range day2Rows(t) {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			_, valid, in := day2Manifest(t, k.Key)
			text := string(valid)
			for name, mutated := range map[string]string{
				"another clusterRef kind":       replaceOnce(t, text, "kind: Cluster", "kind: Secret"),
				"a clusterRef namespace":        replaceOnce(t, text, "    name: c-abc\n", "    name: c-abc\n    namespace: c-abc\n"),
				"a namespace other than the id": replaceOnce(t, text, "  namespace: c-abc\nspec", "  namespace: c-other\nspec"),
				"a paused object":               replaceOnce(t, text, "  ttl: 60\n", "  ttl: 60\n  paused: true\n"),
			} {
				client := &createRecorder{}
				check := CreateCheck{Key: k.Key, Before: func(context.Context, CreateCheckInput) ([]string, error) {
					t.Errorf("%s: the check that reads ran after a refusal of the manifest", name)
					return nil, nil
				}}
				_, _, err := planCreate(t, NewCreate(&recFactory{client: client}, mapFileSource{"m.yaml": []byte(mutated)}, check), in)
				requireValidation(t, err)
				assert.Empty(t, client.creates, "%s: %s sends no request", k.Key, name)
			}
		})
	}
}

func replaceOnce(t *testing.T, s, old, replacement string) string {
	t.Helper()
	require.Contains(t, s, old)
	return strings.Replace(s, old, replacement, 1)
}
