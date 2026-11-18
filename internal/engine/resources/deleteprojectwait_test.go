package resources

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var waitProjectRecovery = clierr.Rancher + " get project 'p-1' --cluster 'c-abc'"

const (
	waitProjectKept = "; the project was kept; run the same command again, which reads the project's current namespaces"
	// foreverGets is a number of GETs no test reaches: the object stays.
	foreverGets = 1 << 30
)

// flagInput is the input of a run with --delete-namespaces and the given
// flags; every flag it gives is set.
func flagInput(values map[string]string, bools map[string]bool) input {
	in := input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-abc"},
		b: map[string]bool{FlagDeleteNamespaces: true}, isSet: map[string]bool{FlagDeleteNamespaces: true}}
	for k, v := range values {
		in.s[k], in.isSet[k] = v, true
	}
	for k, v := range bools {
		in.b[k], in.isSet[k] = v, true
	}
	return in
}

func (f *projectFixture) planInput(t *testing.T, in input) (safety.Effect, runtime.ExecuteFunc) {
	t.Helper()
	eff, exec, err := f.cmd.Plan(t.Context(), in)
	require.NoError(t, err)
	require.NotNil(t, exec)
	return eff, exec
}

// stays makes the object of key, a namespace or the project, respond to n
// GETs and then respond with NOT_FOUND, whether or not it was deleted. A key
// with no object stored gets a plain one.
func (c *projClient) stays(key string, n int) {
	if c.objs == nil {
		c.objs = map[string][]*unstructured.Unstructured{}
	}
	if len(c.objs[key]) == 0 {
		_, name, _ := strings.Cut(key, "/")
		c.objs[key] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": name}}}}
	}
	if c.lingers == nil {
		c.lingers = map[string]int{}
	}
	c.lingers[key] = n
}

func indexOf(t *testing.T, log []string, entry string) int {
	t.Helper()
	i := slices.Index(log, entry)
	require.GreaterOrEqual(t, i, 0, "%q is in %v", entry, log)
	return i
}

func lastIndexOf(t *testing.T, log []string, entry string) int {
	t.Helper()
	for i, e := range slices.Backward(log) {
		if e == entry {
			return i
		}
	}
	require.Fail(t, "entry not found", "%q is in %v", entry, log)
	return -1
}

// countOf is how many entries of log are entry; the slices package has no
// count.
func countOf(log []string, entry string) int {
	n := 0
	for _, e := range log {
		if e == entry {
			n++
		}
	}
	return n
}

func forbidden() error { return clierr.FromResponse("", "GET", 403, nil, nil) }

// TestDeleteProjectWithNamespacesWaitsForEveryNamespaceBeforeTheProject
// asserts the project's delete follows the last read of every namespace the
// command deleted, that a namespace the command saw gone is deleted, and that
// the project and its backing namespace are waited for after the project's
// delete.
func TestDeleteProjectWithNamespacesWaitsForEveryNamespaceBeforeTheProject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"), member("tools", "2"))
		f.down.stays("/apps", 2)
		f.down.stays("/tools", 1)
		f.local.objs["/c-abc-p-1"] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "c-abc-p-1"}}}}
		f.local.stays("/c-abc-p-1", 1)
		_, exec := f.mustPlan(t, true)
		f.local.stays("c-abc/p-1", 1)
		f.local.lingerSeen = map[string]int{}
		planned := len(f.log)

		res, err := exec(t.Context())
		require.NoError(t, err)
		log := f.log[planned:]
		projectDelete := indexOf(t, log, "delete c-abc/p-1 rv=5")
		assert.Equal(t, 3, countOf(log[:projectDelete], "get /apps"), "reads until the server responds with NOT_FOUND")
		assert.Equal(t, 2, countOf(log[:projectDelete], "get /tools"))
		assert.Less(t, lastIndexOf(t, log, "get /apps"), projectDelete, "the project is deleted after the last read of a namespace")
		assert.Less(t, lastIndexOf(t, log, "get /tools"), projectDelete)
		assert.Equal(t, 2, countOf(log[projectDelete:], "get c-abc/p-1"), "the project is read until it is gone")
		assert.Equal(t, 2, countOf(log[projectDelete:], "get /c-abc-p-1"), "the backing namespace is read until it is gone")
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    deleted: apps, tools\n", res.Text)
		_, objects := resultData(t, res)
		assert.Equal(t, map[string]string{"namespace/apps": "deleted", "namespace/tools": "deleted", "project/p-1": "deleted"}, objectsByName(objects))
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, data["waited"])
		assert.Empty(t, res.Warnings)
	})
}

// TestDeleteProjectWithNamespacesWaitsForANamespaceThatIsBeingDeleted asserts
// a namespace of the project that is already Terminating when the command
// plans the delete is not deleted again, is still waited for before the
// project is deleted, keeps the outcome `being deleted`, and counts toward
// waited.
func TestDeleteProjectWithNamespacesWaitsForANamespaceThatIsBeingDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"), member("old", "4", terminating))
		_, exec := f.mustPlan(t, true)
		f.down.stays("/old", 2)
		planned := len(f.log)

		res, err := exec(t.Context())
		require.NoError(t, err)
		log := f.log[planned:]
		assert.Equal(t, []string{"delete apps rv=1", "delete c-abc/p-1 rv=5"}, deletesOf(log), "no delete of the namespace that is being deleted")
		assert.Equal(t, 3, countOf(log, "get /old"))
		assert.Less(t, lastIndexOf(t, log, "get /old"), indexOf(t, log, "delete c-abc/p-1 rv=5"))
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    being deleted: old\n    deleted: apps\n", res.Text)
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, data["waited"])
	})
}

// TestDeleteProjectWithNamespacesDoesNotDeleteTheProjectWhenANamespaceIsNotGoneWithinTheTimeLimit
// asserts a namespace that is still there at the time limit, one the command
// deleted and one that was being deleted, ends the run with an error that
// says the project was kept, names the project in its committed details and
// its get command as the hint, lists every object with the project not
// attempted, and sends no delete of the project.
func TestDeleteProjectWithNamespacesDoesNotDeleteTheProjectWhenANamespaceIsNotGoneWithinTheTimeLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		members []unstructured.Unstructured
		stuck   string
		want    map[string]string
	}{
		"a namespace the command deleted": {
			members: []unstructured.Unstructured{member("apps", "1"), member("tools", "2")},
			stuck:   "apps",
			want:    map[string]string{"namespace/apps": "deletion started", "namespace/tools": "deletion started", "project/p-1": "not attempted"},
		},
		"a namespace that was being deleted": {
			members: []unstructured.Unstructured{member("apps", "1"), member("old", "4", terminating)},
			stuck:   "old",
			want:    map[string]string{"namespace/apps": "deleted", "namespace/old": "being deleted", "project/p-1": "not attempted"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newProjectFixture(t, tc.members...)
				_, exec := f.planInput(t, flagInput(map[string]string{"timeout": "7s"}, nil))
				f.down.stays("/"+tc.stuck, foreverGets)
				planned := len(f.log)
				start := time.Now()

				_, err := exec(t.Context())
				assert.Equal(t, 7*time.Second, time.Since(start), "the time limit is --timeout")
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.False(t, ce.Retryable)
				assert.Equal(t, waitProjectRecovery, ce.Hint)
				assert.True(t, strings.HasPrefix(ce.Message, "timed out after 7s waiting for namespace "+tc.stuck+" to be deleted; the deletion was requested; last state: "), ce.Message)
				assert.True(t, strings.HasSuffix(ce.Message, waitProjectKept), ce.Message)
				details := detailsOf(t, err)
				require.NotNil(t, details.CommittedContextDetails)
				assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "project", ResourceName: "p-1", Recovery: waitProjectRecovery}, *details.CommittedContextDetails)
				assert.Equal(t, tc.want, objectsByName(details.Objects))
				for _, d := range deletesOf(f.log[planned:]) {
					assert.NotContains(t, d, "p-1", "the project is not deleted")
				}
				texts := remedytest.DetailTexts(details.TextDetails())
				assert.Contains(t, texts, "projects:")
				assert.Contains(t, texts, "  not attempted: p-1")
			})
		})
	}
}

// TestDeleteProjectWithNamespacesNamespaceWaitErrorsNameTheProject asserts an
// interrupt, an authentication failure and a certificate failure during the
// wait for a namespace keep their code and hint, end the run before the
// project's delete and name the project in the committed details.
func TestDeleteProjectWithNamespacesNamespaceWaitErrorsNameTheProject(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		code      string
		hint      string
		interrupt bool
	}{
		"authentication failure": {err: clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithHint(clierr.Rancher + " auth login"), code: clierr.CodeAuth, hint: clierr.Rancher + " auth login"},
		"certificate failure":    {err: clierr.New(clierr.CodeTLSTrust, "the server's certificate is not trusted"), code: clierr.CodeTLSTrust, hint: waitProjectRecovery},
		"interrupt":              {code: clierr.CodeGeneric, hint: waitProjectRecovery, interrupt: true},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newProjectFixture(t, member("apps", "1"), member("tools", "2"))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				_, exec := f.mustPlan(t, true)
				f.down.stays("/apps", 0)
				if tc.interrupt {
					f.down.stays("/tools", foreverGets)
					f.down.onGet = func(key string) {
						if key == "/tools" {
							cancel()
						}
					}
				} else {
					f.down.getErr = map[string]error{"/tools": tc.err}
				}
				planned := len(f.log)

				_, err := exec(ctx)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, tc.code, ce.Code)
				assert.False(t, ce.Retryable)
				assert.Equal(t, tc.hint, ce.Hint)
				assert.True(t, strings.HasSuffix(ce.Message, waitProjectKept), ce.Message)
				details := detailsOf(t, err)
				require.NotNil(t, details.CommittedContextDetails)
				assert.Equal(t, "project", details.ResourceKind)
				assert.Equal(t, "p-1", details.ResourceName)
				assert.Equal(t, waitProjectRecovery, details.Recovery)
				assert.Equal(t, map[string]string{"namespace/apps": "deleted", "namespace/tools": "deletion started", "project/p-1": "not attempted"}, objectsByName(details.Objects))
				assert.Equal(t, []string{"delete apps rv=1", "delete tools rv=2"}, deletesOf(f.log[planned:]), "the project is not deleted")
			})
		})
	}
}

// TestDeleteProjectWithNamespacesRefusedNamespaceReadIsAWarning asserts a
// refused read of a namespace after its delete ends that wait with a warning,
// keeps the namespace's outcome `deletion started` (not `deleted`, because
// the command never saw it gone), makes waited false, and still deletes and
// waits for the project.
func TestDeleteProjectWithNamespacesRefusedNamespaceReadIsAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"), member("tools", "2"))
		_, exec := f.mustPlan(t, true)
		f.down.getErr = map[string]error{"/apps": forbidden()}
		planned := len(f.log)

		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"your access to namespace apps ended after the delete request; the deletion was requested and not confirmed"}, res.Warnings)
		assert.Equal(t, "project/p-1 deletion started\n  namespaces:\n    deletion started: apps\n    deleted: tools\n", res.Text)
		_, objects := resultData(t, res)
		assert.Equal(t, map[string]string{"namespace/apps": "deletion started", "namespace/tools": "deleted", "project/p-1": "deleted"}, objectsByName(objects))
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, data["waited"])
		assert.Equal(t, 1, countOf(f.log[planned:], "get /apps"), "one read of the namespace")
		assert.Equal(t, []string{"delete apps rv=1", "delete tools rv=2", "delete c-abc/p-1 rv=5"}, deletesOf(f.log[planned:]))
		assert.Greater(t, countOf(f.log[planned:], "get c-abc/p-1"), 0, "the project is still waited for")
	})
}

// TestDeleteProjectWithNamespacesDoesNotReadNamespacesAlreadyGoneOrNotDeleted
// asserts namespaces that are already gone or not deleted are not read after
// the run starts.
func TestDeleteProjectWithNamespacesDoesNotReadNamespacesAlreadyGoneOrNotDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"), member("default", "2"), member("vanished", "3"))
		f.down.deleteErr = func(_, name, _ string, dryRun bool) error {
			if !dryRun && name == "vanished" {
				return clierr.New(clierr.CodeNotFound, "not found")
			}
			return nil
		}
		_, exec := f.mustPlan(t, true)
		planned := len(f.log)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, countOf(f.log[planned:], "get /apps"))
		assert.Zero(t, countOf(f.log[planned:], "get /default"))
		assert.Zero(t, countOf(f.log[planned:], "get /vanished"))
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    not deleted: default (Kubernetes never deletes it)\n    deleted: apps\n    already gone: vanished\n", res.Text)
	})
}

// TestDeleteProjectWithNamespacesProjectWaitErrorsKeepTheCommittedDetails
// asserts a wait for the project, or for its backing namespace, that ends
// with an error carries that object's committed details and lists the
// namespaces with the outcomes they had when the wait failed: the project
// says deletion started when its own wait failed, and deleted when the
// command saw it gone and the wait for its backing namespace failed.
func TestDeleteProjectWithNamespacesProjectWaitErrorsKeepTheCommittedDetails(t *testing.T) {
	for name, tc := range map[string]struct {
		stuck, kind, recovery string
	}{
		"the project":           {stuck: "c-abc/p-1", kind: "project", recovery: waitProjectRecovery},
		"its backing namespace": {stuck: "/c-abc-p-1", kind: "namespace", recovery: clierr.Rancher + " get namespace 'c-abc-p-1' --cluster 'local'"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newProjectFixture(t, member("apps", "1"))
				f.local.objs["/c-abc-p-1"] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "c-abc-p-1"}}}}
				_, exec := f.planInput(t, flagInput(map[string]string{"timeout": "3s"}, nil))
				f.local.stays(tc.stuck, foreverGets)

				_, err := exec(t.Context())
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.False(t, ce.Retryable)
				assert.True(t, strings.HasPrefix(ce.Message, "timed out after 3s waiting for "+tc.kind+" "), ce.Message)
				assert.NotContains(t, ce.Message, "the project was kept")
				assert.Equal(t, tc.recovery, ce.Hint)
				details := detailsOf(t, err)
				require.NotNil(t, details.CommittedContextDetails)
				assert.True(t, details.Committed)
				assert.Equal(t, tc.kind, details.ResourceKind)
				assert.Equal(t, tc.recovery, details.Recovery)
				want := "deletion started"
				if tc.kind == "namespace" {
					want = "deleted"
				}
				assert.Equal(t, map[string]string{"namespace/apps": "deleted", "project/p-1": want}, objectsByName(details.Objects))
			})
		})
	}
}

// TestDeleteProjectWithNamespacesTimeLimitCoversEveryWait asserts the time
// limit starts at the first delete the server accepts and does not restart: a
// time limit spent before the project's wait ends that wait at once, with no
// read.
func TestDeleteProjectWithNamespacesTimeLimitCoversEveryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"))
		f.local.deleteErr = func(_, name, _ string, dryRun bool) error {
			if !dryRun && name == "p-1" {
				time.Sleep(10 * time.Second)
			}
			return nil
		}
		_, exec := f.planInput(t, flagInput(map[string]string{"timeout": "5s"}, nil))
		planned := len(f.log)

		_, err := exec(t.Context())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, "timed out after 5s waiting for project p-1 to be deleted; the deletion was requested; last state: <no state observed>", ce.Message)
		assert.Equal(t, 0, countOf(f.log[planned:], "get c-abc/p-1"), "no read after the time limit passed")
		assert.Equal(t, map[string]string{"namespace/apps": "deleted", "project/p-1": "deletion started"}, objectsByName(detailsOf(t, err).Objects))
	})
}

// TestDeleteProjectWithNamespacesTakesWaitAndTimeout asserts
// --delete-namespaces takes --wait and --timeout like every other delete:
// --wait and --timeout are accepted, --wait=false sends nothing after the
// deletes, reads no backing namespace and says deletion started with no
// warning, and --wait=false with --timeout is refused before any request.
func TestDeleteProjectWithNamespacesTakesWaitAndTimeout(t *testing.T) {
	t.Run("--wait and --timeout", func(t *testing.T) {
		t.Parallel()
		f := newProjectFixture(t, member("apps", "1"))
		_, exec := f.planInput(t, flagInput(map[string]string{"timeout": "1m"}, map[string]bool{"wait": true}))
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    deleted: apps\n", res.Text)
	})
	t.Run("--wait=false", func(t *testing.T) {
		t.Parallel()
		f := newProjectFixture(t, member("apps", "1"))
		f.local.objs["/c-abc-p-1"] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "c-abc-p-1"}}}}
		_, exec := f.planInput(t, flagInput(nil, map[string]bool{"wait": false}))
		assert.NotContains(t, f.log, "get /c-abc-p-1", "the backing namespace is not read")
		planned := len(f.log)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "project/p-1 deletion started\n  namespaces:\n    deletion started: apps\n", res.Text)
		assert.Equal(t, []string{"delete apps rv=1", "delete c-abc/p-1 rv=5"}, f.log[planned:], "nothing is read after the project's delete")
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, data["waited"])
		assert.Empty(t, res.Warnings)
	})
	t.Run("--wait=false --timeout", func(t *testing.T) {
		t.Parallel()
		f := newProjectFixture(t, member("apps", "1"))
		_, _, err := f.cmd.Plan(t.Context(), flagInput(map[string]string{"timeout": "1m"}, map[string]bool{"wait": false}))
		require.ErrorContains(t, err, "--wait=false conflicts with --timeout; remove one")
		assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
		assert.Empty(t, f.log, "no request")
	})
}

// TestDeleteProjectWithNamespacesWaitsForTheBackingNamespace asserts the run
// with --delete-namespaces reads the project's backing namespace in the Plan,
// before any delete, as the run without it does, and waits for it after the
// project's delete.
func TestDeleteProjectWithNamespacesWaitsForTheBackingNamespace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProjectFixture(t, member("apps", "1"))
		f.local.objs["/c-abc-p-1"] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "c-abc-p-1"}}}}
		_, exec := f.mustPlan(t, true)
		assert.Equal(t, []string{"get c-abc/p-1", "get /c-abc-p-1"}, f.log[:2], "the backing namespace is read in the Plan, before any delete")
		f.local.stays("/c-abc-p-1", 1)
		planned := len(f.log)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 2, countOf(f.log[planned:], "get /c-abc-p-1"))
		assert.Greater(t, indexOf(t, f.log[planned:], "get /c-abc-p-1"), indexOf(t, f.log[planned:], "delete c-abc/p-1 rv=5"), "the backing namespace is read after the project's delete")
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    deleted: apps\n", res.Text)
	})
}

// TestDeleteProjectWithNamespacesBackingNamespaceRefusedIsAWarning asserts a
// backing namespace the caller cannot read is not waited for: the Plan's
// warning reaches the result, waited is false and the project is deleted and
// waited for.
func TestDeleteProjectWithNamespacesBackingNamespaceRefusedIsAWarning(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"))
	f.local.getErr = map[string]error{"/c-abc-p-1": forbidden()}
	_, exec := f.mustPlan(t, true)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"you cannot get namespace c-abc-p-1 (the project's backing namespace on the local cluster); the command does not wait for it"}, res.Warnings)
	assert.Equal(t, "project/p-1 deletion started\n  namespaces:\n    deleted: apps\n", res.Text)
	_, objects := resultData(t, res)
	assert.Equal(t, "deleted", objectsByName(objects)["project/p-1"])
}

// TestDeleteProjectWaitReadsTheProjectWhereItWasDeleted asserts the wait
// reads the project in the namespace (the cluster id) that the project's
// delete used: the project's metadata.namespace as the dry-run preview read
// it.
func TestDeleteProjectWaitReadsTheProjectWhereItWasDeleted(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"))
	cmd, ok := f.cmd.(*deleteProjectCmd)
	require.True(t, ok)
	_, ns, name, _, effectCluster, err := cmd.inner.fetch(t.Context(), flagInput(nil, nil))
	require.NoError(t, err)
	assert.Equal(t, "p-1", name)
	assert.Equal(t, "c-abc", ns)
	assert.Equal(t, effectCluster, ns, "the project's metadata.namespace is its cluster id")

	eff, exec := f.mustPlan(t, true)
	assert.Equal(t, ns, eff.Cluster)
	planned := len(f.log)
	_, err = exec(t.Context())
	require.NoError(t, err)
	log := f.log[planned:]
	assert.Equal(t, indexOf(t, log, "delete "+ns+"/"+name+" rv=5")+1, indexOf(t, log, "get "+ns+"/"+name), "the project is read in its metadata.namespace after the project delete request")
}

// TestDeleteProjectWithNamespacesSkipsAWaitThatRepeatsAnother asserts a
// project of the local cluster that has a namespace with the same name as its
// backing namespace waits for that namespace once, before the project's
// delete, and not again after it.
func TestDeleteProjectWithNamespacesSkipsAWaitThatRepeatsAnother(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log []string
		client := &projClient{log: &log,
			objs: map[string][]*unstructured.Unstructured{"local/p-1": {deleteTestProject("5", "1")}},
			list: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{localMember("c-abc-p-1")}},
		}
		client.objs["/c-abc-p-1"] = []*unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "c-abc-p-1"}}}}
		cmd := NewDeleteProject(mustKind(t, "project"), projFactory{local: client, down: client}, &steveRoots{}).(runtime.MutatingCommand)
		_, exec, err := cmd.Plan(t.Context(), input{args: []string{"p-1"}, s: map[string]string{"cluster": "local"}, b: map[string]bool{FlagDeleteNamespaces: true}})
		require.NoError(t, err)
		client.stays("/c-abc-p-1", 1)
		planned := len(log)

		res, err := exec(t.Context())
		require.NoError(t, err)
		projectDelete := indexOf(t, log[planned:], "delete local/p-1 rv=5")
		assert.Equal(t, 2, countOf(log[planned:][:projectDelete], "get /c-abc-p-1"), "the namespace is read until it is gone, before the project's delete")
		assert.Zero(t, countOf(log[planned:][projectDelete:], "get /c-abc-p-1"), "and not again after it")
		assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    deleted: c-abc-p-1\n", res.Text)
	})
}

// TestDeleteProjectPartialFailureListsTheDeletedOutcome asserts the text
// lines of a failure's details list the outcome deleted with the others, in
// the order of the outcomes.
func TestDeleteProjectPartialFailureListsTheDeletedOutcome(t *testing.T) {
	t.Parallel()
	details := DeleteProjectPartialFailure{
		Objects: []safety.Object{
			{Kind: "namespace", Name: "apps", Outcome: safety.OutcomeDeleted},
			{Kind: "namespace", Name: "old", Outcome: safety.OutcomeBeingDeleted},
			{Kind: "namespace", Name: "tools", Outcome: safety.OutcomeDeletionStarted},
			{Kind: "project", Name: "p-1", Outcome: safety.OutcomeNotAttempted},
		},
		CommittedContextDetails: &clierr.CommittedContextDetails{Committed: true, ResourceKind: "project", ResourceName: "p-1", Recovery: waitProjectRecovery},
	}
	assert.Equal(t, []string{
		"namespaces:",
		"  being deleted: old",
		"  deletion started: tools",
		"  deleted: apps",
		"projects:",
		"  not attempted: p-1",
		"committed: project p-1 - the change may already be on the server",
		"recover: " + waitProjectRecovery,
	}, remedytest.DetailTexts(details.TextDetails()))
}
