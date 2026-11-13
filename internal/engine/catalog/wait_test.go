package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The read keys of the Operation the action answer names and of the App.
const (
	testOperation = "helm-operation-abcde"
	opKey         = "get:operations/" + testNS + "/" + testOperation
	appKey        = "get:apps/" + testNS + "/" + testRelease
	otherID       = "req-9999"
)

// cond is one status condition.
func cond(typ, status, message string) map[string]any {
	c := map[string]any{"type": typ, "status": status}
	if message != "" {
		c["message"] = message
	}
	return c
}

// opFields is an Operation at generation 1 with observedGeneration observed
// and the conditions.
func opFields(observed int64, conds ...map[string]any) map[string]any {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, c)
	}
	return map[string]any{
		"metadata": map[string]any{"generation": int64(1)},
		"status":   map[string]any{"observedGeneration": observed, "conditions": list},
	}
}

var (
	opDone    = found(opFields(1, cond("Reconciling", "False", ""), cond("Stalled", "False", "")))
	opRunning = found(opFields(1, cond("Reconciling", "True", "running operation"), cond("Stalled", "False", "")))
	opBehind  = found(opFields(0, cond("Reconciling", "False", "")))
)

func opStalled(message string) scripted {
	return found(opFields(1, cond("Reconciling", "False", ""), cond("Stalled", "True", message)))
}

// appRev is the App at revision version with status and description,
// whose chart carries the request id id ("" for none).
func appRev(status, id string, version int64, description string) scripted {
	ann := map[string]any{annotationSourceRepoType: sourceRepoTypeCluster, annotationSourceRepo: testRepo}
	if id != "" {
		ann[annotationRequestID] = id
	}
	info := map[string]any{"status": status}
	if description != "" {
		info["description"] = description
	}
	return found(map[string]any{"spec": map[string]any{
		"name": testRelease, "version": version, "info": info,
		"chart": map[string]any{"metadata": map[string]any{"name": testChart, "version": "2.0.0", "annotations": ann}},
	}})
}

// The revisions the fixtures use: the first revision, this request's, and
// a later one another request made.
const (
	firstRevision = 1
	laterRevision = 2
)

func mine(status string) scripted { return appRev(status, testRequestID, firstRevision, "") }

// anotherChange is the message of a revision past the recorded one with
// another request's id.
func anotherChange(version int64) string {
	return fmt.Sprintf("the install ran; another change to 'my-nginx' also ran (revision %d)", version)
}

// waitFor is the input edit that asks for the wait with timeout ("" for
// the default).
func waitFor(timeout string) func(*appInput) {
	return func(in *appInput) {
		in.b[flagWait] = true
		if timeout != "" {
			in.s[flagTimeout] = timeout
		}
	}
}

// waitRun is what one Execute with a wait gave.
type waitRun struct {
	res     runtime.Result
	err     error
	elapsed time.Duration
}

func (r waitRun) clierr(t *testing.T) *clierr.Error {
	t.Helper()
	require.Error(t, r.err)
	ce, ok := errors.AsType[*clierr.Error](r.err)
	require.True(t, ok, "%T %v", r.err, r.err)
	return ce
}

func execute(t *testing.T, exec runtime.ExecuteFunc) waitRun {
	t.Helper()
	start := time.Now()
	res, err := exec(t.Context())
	return waitRun{res: res, err: err, elapsed: time.Since(start)}
}

// installWait plans app install over f with the edits and executes it.
func installWait(t *testing.T, f *appFake, edits ...func(*appInput)) waitRun {
	t.Helper()
	_, exec := plan(t, newTestInstall(f, nil), installInput(edits...))
	return execute(t, exec)
}

// uninstallWait plans app uninstall over f with the edits and executes it.
func uninstallWait(t *testing.T, f *appFake, edits ...func(*appInput)) waitRun {
	t.Helper()
	_, exec := plan(t, newTestUninstall(f), uninstallInput(edits...))
	return execute(t, exec)
}

// requireCommitted asserts the error of a wait after the request: its code,
// not retryable, the committed details naming the App, with the hint as
// the recovery.
func requireCommitted(t *testing.T, ce *clierr.Error, code string) {
	t.Helper()
	assert.Equal(t, code, ce.Code)
	assert.False(t, ce.Retryable)
	d, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok, "details %T", ce.Details)
	assert.True(t, d.Committed)
	assert.Equal(t, "app", d.ResourceKind)
	assert.Equal(t, testRelease, d.ResourceName)
	if code == clierr.CodeGeneric {
		assert.Equal(t, ce.Hint, d.Recovery)
	}
}

var (
	getOperationHint = clierr.Rancher + " get operation 'helm-operation-abcde' --cluster 'prod' --namespace 'web'"
	getAppHint       = clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	uninstallCmdOf   = clierr.Rancher + " app uninstall 'my-nginx' --cluster 'prod' --namespace 'web' --dry-run"
)

// requireWaitError asserts a GENERIC committed error with the get app hint
// whose message is want.
func requireWaitError(t *testing.T, r waitRun, want string) {
	t.Helper()
	ce := r.clierr(t)
	requireCommitted(t, ce, clierr.CodeGeneric)
	assert.Equal(t, want, ce.Message)
	assert.Equal(t, getAppHint, ce.Hint)
}

// TestInstallWaitSucceeds asserts the ends of an install wait that
// succeed: the revision with this request's id reads deployed, after the
// App was absent or pending for a while.
func TestInstallWaitSucceeds(t *testing.T) {
	t.Parallel()
	for name, apps := range map[string][]scripted{
		"deployed":                    {mine("deployed")},
		"absent then deployed":        {absent, absent, absent, mine("deployed")},
		"pending then deployed":       {mine("pending-install"), mine("pending-install"), mine("deployed")},
		"deployed with a description": {appRev("deployed", testRequestID, firstRevision, "Install complete")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, opRunning, opBehind, opDone)
				f.script(appKey, apps...)
				r := installWait(t, f, waitFor(""))
				require.NoError(t, r.err)
				assert.Equal(t, result{
					Release: testRelease, Namespace: testNS,
					OperationName: testOperation, OperationNamespace: testNS, RequestID: testRequestID,
					Status: "deployed", Revision: firstRevision,
					FollowUp: []string{getAppHint},
				}, r.res.Data)
				assert.Equal(t, fmt.Sprintf("app my-nginx: installed (revision %d, status deployed)\nfollow it with: %s\n", firstRevision, getAppHint), r.res.Text)
				assert.NotContains(t, r.res.Warnings, warnNoWatch)
				assert.NotContains(t, r.res.Warnings, noAppNote)
				assert.Less(t, r.elapsed, 30*time.Second)
			})
		})
	}
}

// TestInstallWaitAfterADoneOperation asserts the errors after an Operation
// that ended without Stalled: the revision failed, a revision past the
// recorded one with another id, and a status that stays at the end of the
// margin.
func TestInstallWaitAfterADoneOperation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		apps []scripted
		want string
		// margin is true when the error comes at the end of the margin.
		margin bool
	}{
		"failed": {
			apps: []scripted{appRev("failed", testRequestID, 1, "Install  failed:\n\ttimed out\x07 waiting")},
			want: "release 'my-nginx' failed: Install failed: timed out waiting (status failed); the release stays installed; " +
				"to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
		},
		"another change": {
			apps: []scripted{absent, appRev("deployed", otherID, laterRevision, "")},
			want: anotherChange(laterRevision),
		},
		"another change with no id": {
			apps: []scripted{appRev("deployed", "", laterRevision, "")},
			want: anotherChange(laterRevision),
		},
		"pending past the margin": {
			apps:   []scripted{mine("pending-install")},
			want:   "Helm stopped and left release 'my-nginx' pending; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
			margin: true,
		},
		"superseded past the margin": {
			apps:   []scripted{mine("superseded")},
			want:   "release 'my-nginx' is at status superseded",
			margin: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, opDone)
				f.script(appKey, tc.apps...)
				r := installWait(t, f, waitFor(""))
				requireWaitError(t, r, tc.want)
				if tc.margin {
					assert.Equal(t, appMargin, r.elapsed, "the margin starts at the first read of that status")
				} else {
					assert.Less(t, r.elapsed, 5*time.Second)
				}
			})
		})
	}
}

// TestInstallWaitAfterAStalledOperation asserts the App read after a
// Stalled Operation: each end, the exit code or the message in another
// shape, and the margin.
func TestInstallWaitAfterAStalledOperation(t *testing.T) {
	t.Parallel()
	noRevision := "and no revision of 'my-nginx' carries this request's id; Helm records no revision when it fails before it stores the release, " +
		"for example on values that fail the chart's values schema, template errors or resources another release owns; read the operation with " +
		getOperationHint
	cases := map[string]struct {
		message string
		apps    []scripted
		want    string
		atOnce  bool
	}{
		"failed": {
			message: " exit code: 123",
			apps:    []scripted{absent, appRev("failed", testRequestID, 1, "Install failed: image pull")},
			want: "release 'my-nginx' failed: Install failed: image pull (status failed, exit code 123); the release stays installed; " +
				"to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
			atOnce: true,
		},
		"no app": {
			message: " exit code: 1",
			apps:    []scripted{absent},
			want:    "the operation failed (exit code 1) " + noRevision,
		},
		"pending at the end": {
			message: " exit code: 137",
			apps:    []scripted{mine("pending-install")},
			want:    "Helm stopped (exit code 137) and left release 'my-nginx' pending; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
		},
		"pending then failed": {
			message: " exit code: 1",
			apps:    []scripted{mine("pending-install"), mine("pending-install"), appRev("failed", testRequestID, 1, "hook failed")},
			want: "release 'my-nginx' failed: hook failed (status failed, exit code 1); the release stays installed; " +
				"to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
			atOnce: true,
		},
		"deployed": {
			message: " exit code: 1",
			apps:    []scripted{mine("deployed")},
			want:    "operation 'helm-operation-abcde' failed (exit code 1) after release 'my-nginx' was deployed",
			atOnce:  true,
		},
		"another change": {
			message: " exit code: 1",
			apps:    []scripted{appRev("deployed", otherID, laterRevision, "")},
			want:    fmt.Sprintf("this request failed (exit code 1) and another change to 'my-nginx' ran (revision %d)", laterRevision),
			atOnce:  true,
		},
		"uninstalling at the end": {
			message: " exit code: 1",
			apps:    []scripted{mine("uninstalling")},
			want:    "operation 'helm-operation-abcde' failed (exit code 1); release 'my-nginx' is at status uninstalling",
		},
		"a message with no exit code, pending": {
			message: "OOMKilled",
			apps:    []scripted{mine("pending-install")},
			want: "Helm stopped (operation message: OOMKilled) and left release 'my-nginx' pending; " +
				"to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
		},
		"another shape, pending": {
			message: "foo exit code: 1",
			apps:    []scripted{mine("pending-install")},
			want: "Helm stopped (operation message: foo exit code: 1) and left release 'my-nginx' pending; " +
				"to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
		},
		"a sign is another shape": {
			message: "exit code: -1",
			apps:    []scripted{absent},
			want:    "the operation failed (operation message: exit code: -1) " + noRevision,
		},
		"no message": {
			message: "",
			apps:    []scripted{absent},
			want:    "the operation failed " + noRevision,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, opRunning, opStalled(tc.message))
				f.script(appKey, tc.apps...)
				r := installWait(t, f, waitFor(""))
				ce := r.clierr(t)
				requireCommitted(t, ce, clierr.CodeGeneric)
				assert.Equal(t, tc.want, ce.Message)
				if strings.Contains(tc.want, "no revision of") {
					assert.Equal(t, getOperationHint, ce.Hint, "the Operation holds the run's result")
				} else {
					assert.Equal(t, getAppHint, ce.Hint)
				}
				assert.NotContains(t, ce.Message, "another change to 'my-nginx' also ran")
				if tc.message == "foo exit code: 1" {
					assert.NotContains(t, ce.Message, "exit code 1")
				}
				if tc.atOnce {
					assert.Less(t, r.elapsed, 10*time.Second)
				} else {
					assert.Equal(t, time.Second+appMargin, r.elapsed, "the margin starts at the Stalled read")
				}
			})
		})
	}
}

// TestInstallWaitTimesOut asserts the timeout of an install wait: GENERIC,
// not retryable, the committed details, the last state the wait read, and
// the last error.
func TestInstallWaitTimesOut(t *testing.T) {
	t.Parallel()
	forbidden := clierr.New(clierr.CodeRBAC, "operations \"helm-operation-abcde\" is forbidden")
	cases := map[string]struct {
		ops, apps []scripted
		want      string
	}{
		"app absent": {
			ops:  []scripted{opDone},
			apps: []scripted{absent},
			want: "timed out after 2m waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=False, Stalled=False; app not found",
		},
		"operation running": {
			ops:  []scripted{opRunning},
			want: "timed out after 2m waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=True, Stalled=False",
		},
		"operation not yet observed": {
			ops:  []scripted{opBehind},
			want: "timed out after 2m waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' observedGeneration 0 behind generation 1, Reconciling=False",
		},
		"operation forbidden": {
			ops:  []scripted{failing(forbidden)},
			want: "timed out after 2m waiting for release 'my-nginx'; last state: nothing read; last error: RBAC: operations \"helm-operation-abcde\" is forbidden",
		},
		"app with no revision number and no request id": {
			ops:  []scripted{opDone},
			apps: []scripted{appRev("deployed", "", 0, "")},
			want: "timed out after 2m waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=False, Stalled=False; app status deployed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, tc.ops...)
				if tc.apps != nil {
					f.script(appKey, tc.apps...)
				}
				r := installWait(t, f, waitFor("2m"))
				requireWaitError(t, r, tc.want)
				assert.Equal(t, 2*time.Minute, r.elapsed)
			})
		})
	}
}

// TestInstallWaitRetriesAForbiddenOperation asserts a 403 on the Operation
// is read again, and the wait goes on when it passes.
func TestInstallWaitRetriesAForbiddenOperation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		forbidden := failing(clierr.New(clierr.CodeRBAC, "forbidden"))
		f.script(opKey, forbidden, forbidden, opDone)
		f.script(appKey, mine("deployed"))
		r := installWait(t, f, waitFor(""))
		require.NoError(t, r.err)
		assert.Equal(t, "deployed", r.res.Data.(result).Status)
	})
}

// TestInstallWaitWhenTheOperationIsGone asserts a NOT_FOUND of the
// Operation: a warning, the App read alone, and a pending revision is
// "cut off" only when the Operation was read before.
func TestInstallWaitWhenTheOperationIsGone(t *testing.T) {
	t.Parallel()
	gone := operationGoneWarning(testOperation)
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		f.script(opKey, opRunning, absent)
		f.script(appKey, mine("pending-install"))
		r := installWait(t, f, waitFor(""))
		requireWaitError(t, r, "Helm's run was cut off; release 'my-nginx' stays pending; to remove it, add --apply --yes after reading the preview of "+uninstallCmdOf)
	})
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		f.script(opKey, opRunning, absent)
		f.script(appKey, absent, mine("deployed"))
		r := installWait(t, f, waitFor(""))
		require.NoError(t, r.err)
		assert.Equal(t, []string{gone}, r.res.Warnings)
	})
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		f.script(opKey, absent)
		f.script(appKey, mine("pending-install"), mine("pending-install"), mine("pending-install"), mine("deployed"))
		r := installWait(t, f, waitFor(""))
		require.NoError(t, r.err, "a pending revision keeps the wait reading when the Operation was never read")
		assert.Equal(t, []string{gone}, r.res.Warnings)
		assert.Equal(t, 3*time.Second, r.elapsed)
	})
}

// skipOperation makes Helm run as a ClusterRepo's service account that the
// caller cannot get operations for.
func skipOperation(f *appFake) {
	f.allowed = false
	f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{
		"url": "https://charts.example.test", "serviceAccount": "helm", "serviceAccountNamespace": "tools",
	}})
}

// TestInstallWaitWithoutTheOperation asserts the wait when the caller
// cannot get operations: the warning in the preview and the result, no
// Operation read, the App's ends, and a pending revision that keeps the
// wait reading until the timeout, whose message names what the wait cannot
// see.
func TestInstallWaitWithoutTheOperation(t *testing.T) {
	t.Parallel()
	warning := "you cannot get operations in namespace 'web'; the wait reads the app only, and a chart Helm refuses before it stores a release " +
		"is seen only as the wait timeout; a shorter --timeout ends that wait sooner"
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		skipOperation(f)
		f.script(appKey, absent, mine("deployed"))
		eff, exec := plan(t, newTestInstall(f, nil), installInput(waitFor("")))
		assert.Contains(t, eff.Warnings, warning)
		r := execute(t, exec)
		require.NoError(t, r.err)
		assert.NotContains(t, r.res.Warnings, warning, "the Effect's warning reaches the result once, through the runtime")
		assert.NotContains(t, f.reads, opKey)
	})
	t.Run("review failed", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		skipOperation(f)
		f.review = errors.New("the review request failed")
		eff, _ := plan(t, newTestInstall(f, nil), installInput(waitFor("")))
		assert.Contains(t, eff.Warnings, "rancher-cli could not check whether you can get operations in namespace 'web'; the wait reads the app only, "+
			"and a chart Helm refuses before it stores a release is seen only as the wait timeout; a shorter --timeout ends that wait sooner")
		assert.NotContains(t, eff.Warnings, warning)
	})
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		skipOperation(f)
		f.script(appKey, mine("pending-install"))
		r := installWait(t, f, waitFor("1m"))
		requireWaitError(t, r, "timed out after 1m waiting for release 'my-nginx'; last state: app status pending-install; "+
			"a chart Helm refuses before it stores a release is seen only as the wait timeout")
		assert.Equal(t, time.Minute, r.elapsed)
	})
	for name, tc := range map[string]struct {
		apps []scripted
		want string
	}{
		"failed": {[]scripted{appRev("failed", testRequestID, 1, "bad values")},
			"release 'my-nginx' failed: bad values (status failed); the release stays installed; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf},
		"another change": {[]scripted{appRev("deployed", otherID, laterRevision, "")}, anotherChange(laterRevision)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				skipOperation(f)
				f.script(appKey, tc.apps...)
				requireWaitError(t, installWait(t, f, waitFor("")), tc.want)
			})
		})
	}
}

// TestWaitMarginNeverOutlivesTheTimeout asserts a margin read ends with the
// time left when that is less than the margin, with the end-of-margin
// error, and with the timeout error when no read was answered.
func TestWaitMarginNeverOutlivesTheTimeout(t *testing.T) {
	t.Parallel()
	forbidden := failing(clierr.New(clierr.CodeRBAC, "apps \"my-nginx\" is forbidden"))
	cases := map[string]struct {
		uninstall bool
		apps      []scripted
		want      string
	}{
		"no revision": {apps: []scripted{absent},
			want: "the operation failed (exit code 1) and no revision of 'my-nginx' carries this request's id"},
		"left pending": {apps: []scripted{mine("pending-install")},
			want: "Helm stopped (exit code 1) and left release 'my-nginx' pending"},
		"stays installed": {uninstall: true, apps: []scripted{mine("deployed")},
			want: "operation 'helm-operation-abcde' ended (exit code 1); release 'my-nginx' is at status deployed; the release stays installed"},
		"every read fails": {apps: []scripted{forbidden},
			want: "timed out after 10s waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=False, Stalled=True (exit code: 1); " +
				"last error: RBAC: apps \"my-nginx\" is forbidden"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, opStalled(" exit code: 1"))
				var r waitRun
				if tc.uninstall {
					deployedApp(f, "deployed", nil)
					f.script(appKey, tc.apps...)
					r = uninstallWait(t, f, waitFor("10s"))
				} else {
					f.script(appKey, tc.apps...)
					r = installWait(t, f, waitFor("10s"))
				}
				ce := r.clierr(t)
				requireCommitted(t, ce, clierr.CodeGeneric)
				assert.Contains(t, ce.Message, tc.want)
				assert.Equal(t, 10*time.Second, r.elapsed)
			})
		})
	}
}

// watchingFake is appFake with a watch: a List by name answers the Get,
// and a watch sends no event until its context ends, or, with deleteAfter
// set, a Deleted event for the App that long after it opens.
type watchingFake struct {
	*appFake
	deleteAfter time.Duration
}

var _ engine.ObjectWatcher = (*watchingFake)(nil)

func (w *watchingFake) Cluster(context.Context, string) (engine.ClusterClient, error) { return w, nil }

func (w *watchingFake) ListObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, string, error) {
	obj, err := w.Get(ctx, gvr, ns, name)
	switch {
	case err == nil:
		return obj, "1", nil
	case notFound(err):
		return nil, "1", nil
	}
	return nil, "", err
}

func (w *watchingFake) WatchObject(ctx context.Context, gvr schema.GroupVersionResource, _, _, _ string, _ time.Duration) (<-chan engine.ObjectEvent, error) {
	ch := make(chan engine.ObjectEvent)
	go func() {
		defer close(ch)
		if w.deleteAfter > 0 && gvr == kind("app").GVR {
			select {
			case <-time.After(w.deleteAfter):
			case <-ctx.Done():
				return
			}
			select {
			case ch <- engine.ObjectEvent{Type: engine.ObjectDeleted}:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return ch, nil
}

// TestWaitReadsADeletedAppAsAbsent asserts a Deleted event of a watched App
// is an answer that the App is gone. An uninstall ends at the event: done
// after a done Operation and without the Operation, "was uninstalled" after
// a Stalled one. An install reads on: inside a margin shorter than two
// intervals, after Stalled there is no revision, after a done Operation it
// times out with the App not found, and without the Operation it reads to
// the timeout, never done on the event.
func TestWaitReadsADeletedAppAsAbsent(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		uninstall, skip bool
		ops             []scripted
		apps            []scripted
		deleteAfter     time.Duration
		timeout         string
		elapsed         time.Duration
		// want is the error message's start; "" for success.
		want string
	}{
		"uninstall": {uninstall: true, ops: []scripted{opDone}, apps: []scripted{mine("deployed"), absent},
			deleteAfter: time.Second, timeout: "5m", elapsed: time.Second},
		"uninstall without the operation": {uninstall: true, skip: true, apps: []scripted{mine("uninstalling"), absent},
			deleteAfter: time.Second, timeout: "5m", elapsed: time.Second},
		"uninstall after stalled": {uninstall: true, ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("uninstalling"), absent},
			deleteAfter: time.Second, timeout: "5m", elapsed: time.Second,
			want: "release 'my-nginx' was uninstalled; a later step failed (exit code 1)"},
		"install after stalled": {ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("pending-install"), absent},
			deleteAfter: 3 * time.Second, timeout: "8s", elapsed: 8 * time.Second,
			want: "the operation failed (exit code 1) and no revision of 'my-nginx' carries this request's id; Helm records"},
		"install after a done operation": {ops: []scripted{opDone}, apps: []scripted{mine("pending-install"), mine("pending-install"), absent},
			deleteAfter: 3 * time.Second, timeout: "8s", elapsed: 8 * time.Second,
			want: "timed out after 8s waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=False, Stalled=False; app not found"},
		"install without the operation": {skip: true, apps: []scripted{mine("pending-install"), absent},
			deleteAfter: time.Second, timeout: "8s", elapsed: 8 * time.Second,
			want: "timed out after 8s waiting for release 'my-nginx'; last state: app not found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				w := &watchingFake{appFake: f, deleteAfter: tc.deleteAfter}
				f.script(opKey, tc.ops...)
				f.script(appKey, tc.apps...)
				var exec runtime.ExecuteFunc
				switch {
				case tc.uninstall:
					if tc.skip {
						skipUninstallOperation(f, "deployed")
					} else {
						deployedApp(f, "deployed", nil)
					}
					cmd := newTestUninstall(f)
					cmd.clients, cmd.interval = w, defaultPollInterval
					_, exec = plan(t, cmd, uninstallInput(waitFor(tc.timeout)))
				default:
					if tc.skip {
						skipOperation(f)
					}
					cmd := newTestInstall(f, nil)
					cmd.clients, cmd.interval = w, defaultPollInterval
					_, exec = plan(t, cmd, installInput(waitFor(tc.timeout)))
				}
				r := execute(t, exec)
				assert.Equal(t, tc.elapsed, r.elapsed)
				if tc.want == "" {
					require.NoError(t, r.err)
					assert.Equal(t, "uninstalled", r.res.Data.(result).Status)
					assert.Equal(t, []string{}, r.res.Data.(result).FollowUp)
					return
				}
				ce := r.clierr(t)
				requireCommitted(t, ce, clierr.CodeGeneric)
				assert.True(t, strings.HasPrefix(ce.Message, tc.want), ce.Message)
			})
		})
	}
}

// TestWaitMarginEndsOnTimeWhenWatched asserts the margin of a watched App
// that sends no event ends at the margin: the end is never left to an
// event.
func TestWaitMarginEndsOnTimeWhenWatched(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		f.script(opKey, opDone)
		f.script(appKey, mine("pending-install"))
		w := &watchingFake{appFake: f}
		cmd := newTestInstall(f, nil)
		cmd.clients = w
		_, exec := plan(t, cmd, installInput(waitFor("")))
		r := execute(t, exec)
		ce := r.clierr(t)
		assert.Contains(t, ce.Message, "Helm stopped and left release 'my-nginx' pending")
		assert.Equal(t, appMargin, r.elapsed)
	})
}

// TestWaitAbortKeepsTheCode asserts an authentication or certificate
// failure during the wait ends it at once with its code, not retryable,
// with the committed details.
func TestWaitAbortKeepsTheCode(t *testing.T) {
	t.Parallel()
	for _, bad := range []*clierr.Error{
		clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithRetryable(true),
		clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted"),
	} {
		t.Run(bad.Code, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				f.script(opKey, failing(bad))
				r := installWait(t, f, waitFor(""))
				ce := r.clierr(t)
				requireCommitted(t, ce, bad.Code)
				assert.Equal(t, getAppHint, ce.Details.(clierr.CommittedContextDetails).Recovery)
				assert.Zero(t, r.elapsed)
			})
		})
	}
}

// TestWaitCancelNamesTheRequest asserts a wait the caller ends returns the
// committed details and names the operation and, for an install, the
// request id.
func TestWaitCancelNamesTheRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		f.script(opKey, opRunning)
		_, exec := plan(t, newTestInstall(f, nil), installInput(waitFor("")))
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(7 * time.Second)
			cancel()
		}()
		_, err := exec(ctx)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%T", err)
		requireCommitted(t, ce, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, "operation 'helm-operation-abcde' in namespace 'web' may still run Helm")
		assert.Contains(t, ce.Message, "this request's id is "+testRequestID)
		assert.Contains(t, ce.Message, "is the revision this request created")
		assert.NotErrorIs(t, err, context.Canceled, "the runtime keeps the details of an error that is not the context's own")
	})
}

// TestUpgradeWaitCancelNamesTheRequest asserts the error of an upgrade wait
// the caller ended names the request's id and the revision it produced.
func TestUpgradeWaitCancelNamesTheRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := upgradeFake(t, nil)
		f.script(opKey, opRunning)
		_, exec := plan(t, newTestUpgrade(f, nil), upgradeInput(waitFor("")))
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(7 * time.Second)
			cancel()
		}()
		_, err := exec(ctx)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%T", err)
		requireCommitted(t, ce, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, "operation 'helm-operation-abcde' in namespace 'web' may still run Helm")
		assert.Contains(t, ce.Message, "this request's id is "+testRequestID)
		assert.Contains(t, ce.Message, "is the revision this request created")
	})
}

// appGoneAtTheEnd reports whether the last scripted App answer is
// NOT_FOUND.
func appGoneAtTheEnd(apps []scripted) bool {
	return len(apps) > 0 && apps[len(apps)-1].fields == nil && apps[len(apps)-1].err == nil
}

// TestUninstallWait asserts the ends of an uninstall wait: done when the
// App is gone, or with --keep-history when it reads uninstalled, never on
// a status Plan read; and the errors after a done and a Stalled Operation.
func TestUninstallWait(t *testing.T) {
	t.Parallel()
	keep := func(in *appInput) { in.b[flagKeepHistory] = true }
	cases := map[string]struct {
		planStatus string
		edits      []func(*appInput)
		ops, apps  []scripted
		// want is the error message; "" for success.
		want    string
		hint    string
		elapsed time.Duration
	}{
		"gone": {planStatus: "deployed", ops: []scripted{opRunning, opDone}, apps: []scripted{absent}, elapsed: time.Second},
		"keep history, uninstalled": {planStatus: "deployed", edits: []func(*appInput){keep},
			ops: []scripted{opDone}, apps: []scripted{mine("uninstalled")}},
		"keep history, uninstalling then uninstalled": {planStatus: "deployed", edits: []func(*appInput){keep},
			ops: []scripted{opDone}, apps: []scripted{mine("uninstalling"), mine("uninstalling"), mine("uninstalled")}, elapsed: 2 * time.Second},
		"failed before, gone": {planStatus: "failed", ops: []scripted{opDone}, apps: []scripted{mine("failed"), absent}, elapsed: time.Second},
		"uninstalled before, gone later": {planStatus: "uninstalled", ops: []scripted{opDone},
			apps: []scripted{mine("uninstalled"), mine("uninstalled"), absent}, elapsed: 2 * time.Second},
		"unchanged after a done operation": {planStatus: "deployed", ops: []scripted{opDone}, apps: []scripted{appRev("deployed", "", 1, "Install complete")},
			want: "operation 'helm-operation-abcde' ended; release 'my-nginx' is at status deployed (Install complete); the release stays installed", elapsed: appMargin},
		"kept history not removed": {planStatus: "deployed", ops: []scripted{opDone}, apps: []scripted{mine("uninstalled")},
			want: "release 'my-nginx' is at status uninstalled; its kept history was not removed", elapsed: appMargin},
		"uninstalled before, still uninstalled": {planStatus: "uninstalled", ops: []scripted{opDone}, apps: []scripted{mine("uninstalled")},
			want: "operation 'helm-operation-abcde' ended; release 'my-nginx' is at status uninstalled; the release stays installed", elapsed: appMargin},
		"stalled, gone": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("deployed"), absent},
			want: "release 'my-nginx' was uninstalled; a later step failed (exit code 1)",
			hint: clierr.Rancher + " get apps --cluster 'prod' --namespace 'web'", elapsed: time.Second},
		"stalled, uninstalled": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("uninstalled")},
			want: "release 'my-nginx' was uninstalled; a later step failed (exit code 1)"},
		"stalled, uninstalling for the margin": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 2")},
			apps:    []scripted{appRev("uninstalling", "", 1, "Deletion in progress (or silently failed)")},
			want:    "operation 'helm-operation-abcde' ended (exit code 2); release 'my-nginx' is at status uninstalling (Deletion in progress (or silently failed)); the release stays installed",
			elapsed: appMargin},
		"stalled, uninstalling then gone": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 1")},
			apps: []scripted{mine("uninstalling"), mine("uninstalling"), absent},
			want: "release 'my-nginx' was uninstalled; a later step failed (exit code 1)",
			hint: clierr.Rancher + " get apps --cluster 'prod' --namespace 'web'", elapsed: 2 * time.Second},
		"stalled, uninstalling then uninstalled": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 1")},
			apps: []scripted{mine("uninstalling"), mine("uninstalled")},
			want: "release 'my-nginx' was uninstalled; a later step failed (exit code 1)", elapsed: time.Second},
		"stalled, unchanged": {planStatus: "deployed", ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("deployed")},
			want: "operation 'helm-operation-abcde' ended (exit code 1); release 'my-nginx' is at status deployed; the release stays installed", elapsed: appMargin},
		"stalled, uninstalled before and still": {planStatus: "uninstalled", ops: []scripted{opStalled(" exit code: 1")}, apps: []scripted{mine("uninstalled")},
			want: "operation 'helm-operation-abcde' ended (exit code 1); release 'my-nginx' is at status uninstalled; the release stays installed", elapsed: appMargin},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				deployedApp(f, tc.planStatus, nil)
				f.script(opKey, tc.ops...)
				f.script(appKey, tc.apps...)
				r := uninstallWait(t, f, append(tc.edits, waitFor(""))...)
				assert.Equal(t, tc.elapsed, r.elapsed)
				if tc.want == "" {
					require.NoError(t, r.err)
					assert.Equal(t, "uninstalled", r.res.Data.(result).Status)
					assert.Zero(t, r.res.Data.(result).Revision)
					if appGoneAtTheEnd(tc.apps) {
						assert.Equal(t, []string{}, r.res.Data.(result).FollowUp, "an App that is gone has no get app")
						assert.Equal(t, "app my-nginx: uninstalled\n", r.res.Text)
					} else {
						assert.Equal(t, []string{getAppHint}, r.res.Data.(result).FollowUp)
						assert.Equal(t, "app my-nginx: uninstalled\nfollow it with: "+getAppHint+"\n", r.res.Text)
					}
					return
				}
				ce := r.clierr(t)
				requireCommitted(t, ce, clierr.CodeGeneric)
				assert.Equal(t, tc.want, ce.Message)
				hint := tc.hint
				if hint == "" {
					hint = getAppHint
				}
				assert.Equal(t, hint, ce.Hint)
			})
		})
	}
}

// skipUninstallOperation makes the App name a ClusterRepo whose service
// account runs Helm, which the caller cannot get operations for.
func skipUninstallOperation(f *appFake, status string) {
	f.allowed = false
	f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{"serviceAccount": "helm"}})
	deployedApp(f, status, map[string]any{annotationSourceRepoType: sourceRepoTypeCluster, annotationSourceRepo: testRepo})
}

// TestUninstallWaitWithoutTheOperation asserts the uninstall wait when the
// caller cannot get operations: the warning, and the App's ends alone.
func TestUninstallWaitWithoutTheOperation(t *testing.T) {
	t.Parallel()
	warning := "you cannot get operations in namespace 'web'; the wait reads the app only, and an uninstall that fails and leaves the app unchanged " +
		"is seen only as the wait timeout; a shorter --timeout ends that wait sooner"
	keep := func(in *appInput) { in.b[flagKeepHistory] = true }
	cases := map[string]struct {
		planStatus string
		edits      []func(*appInput)
		apps       []scripted
		elapsed    time.Duration
	}{
		"gone":                       {planStatus: "deployed", apps: []scripted{mine("uninstalling"), absent}, elapsed: time.Second},
		"keep history, uninstalled":  {planStatus: "deployed", edits: []func(*appInput){keep}, apps: []scripted{mine("uninstalled")}},
		"uninstalled before, gone":   {planStatus: "uninstalled", apps: []scripted{mine("uninstalled"), mine("uninstalled"), mine("uninstalled"), absent}, elapsed: 3 * time.Second},
		"failed before, then gone":   {planStatus: "failed", apps: []scripted{mine("failed"), absent}, elapsed: time.Second},
		"deployed for a while, gone": {planStatus: "deployed", apps: []scripted{mine("deployed"), mine("deployed"), absent}, elapsed: 2 * time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newAppFake(t)
				skipUninstallOperation(f, tc.planStatus)
				f.script(appKey, tc.apps...)
				eff, exec := plan(t, newTestUninstall(f), uninstallInput(append(tc.edits, waitFor(""))...))
				assert.Contains(t, eff.Warnings, warning)
				r := execute(t, exec)
				require.NoError(t, r.err)
				assert.NotContains(t, r.res.Warnings, warning, "the Effect's warning reaches the result once, through the runtime")
				assert.Equal(t, tc.elapsed, r.elapsed)
				assert.NotContains(t, f.reads, opKey)
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		f := newAppFake(t)
		skipUninstallOperation(f, "deployed")
		f.script(appKey, mine("deployed"))
		r := uninstallWait(t, f, waitFor("30s"))
		requireWaitError(t, r, "timed out after 30s waiting for release 'my-nginx'; last state: app status deployed; "+
			"an uninstall that fails and leaves the app unchanged is seen only as the wait timeout")
	})
	t.Run("review failed", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		skipUninstallOperation(f, "deployed")
		f.review = errors.New("the review request failed")
		eff, _ := plan(t, newTestUninstall(f), uninstallInput(waitFor("")))
		assert.Contains(t, eff.Warnings, "rancher-cli could not check whether you can get operations in namespace 'web'; the wait reads the app only, "+
			"and an uninstall that fails and leaves the app unchanged is seen only as the wait timeout; a shorter --timeout ends that wait sooner")
		assert.NotContains(t, eff.Warnings, warning)
	})
}

// TestWaitFlags asserts the flags: --timeout alone waits, --wait=false with
// --timeout and a --timeout that is not a positive duration of at most 55m
// are refused in Plan, and without a wait the result is the one without.
func TestWaitFlags(t *testing.T) {
	t.Parallel()
	assert.Less(t, maxWaitTimeout, time.Hour, "Rancher removes the operation one hour after it starts")
	refused := map[string]struct {
		edit func(*appInput)
		want string
	}{
		"wait false with timeout": {func(in *appInput) { in.b[flagWait] = false; in.s[flagTimeout] = "1m" },
			"--wait=false conflicts with --timeout; remove one"},
		"longer than 55m": {func(in *appInput) { in.s[flagTimeout] = "56m" },
			"--timeout 56m is longer than 55m; Rancher removes the operation one hour after it starts"},
		"zero":         {func(in *appInput) { in.s[flagTimeout] = "0" }, `--timeout "0" must be a positive duration, e.g. 10m`},
		"not duration": {func(in *appInput) { in.s[flagTimeout] = "soon" }, `--timeout "soon" is not a valid duration, e.g. 10m`},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, cmd := range []runtime.MutatingCommand{newTestInstall(newAppFake(t), nil), newTestUninstall(newAppFake(t))} {
				in := installInput(tc.edit)
				if _, ok := cmd.(*uninstallCmd); ok {
					in = uninstallInput(tc.edit)
				}
				_, _, err := cmd.Plan(t.Context(), in)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%T %v", err, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, tc.want, ce.Message)
			}
		})
	}
	t.Run("timeout alone waits", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := newAppFake(t)
			f.script(opKey, opDone)
			f.script(appKey, mine("deployed"))
			r := installWait(t, f, func(in *appInput) { in.s[flagTimeout] = "55m" })
			require.NoError(t, r.err)
			assert.Equal(t, "deployed", r.res.Data.(result).Status)
		})
	})
	t.Run("without a wait", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		f.script(opKey, opRunning)
		r := installWait(t, f, func(in *appInput) { in.b[flagWait] = false })
		require.NoError(t, r.err)
		assert.Empty(t, r.res.Data.(result).Status)
		assert.Contains(t, r.res.Warnings, warnNoWatch)
		assert.NotContains(t, f.reads, opKey)
	})
}

// eventsOnce is an ObjectWatcher whose one stream holds a Deleted event and
// is closed.
type eventsOnce struct{}

func (eventsOnce) ListObject(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, string, error) {
	return nil, "1", nil
}

func (eventsOnce) WatchObject(context.Context, schema.GroupVersionResource, string, string, string, time.Duration) (<-chan engine.ObjectEvent, error) {
	ch := make(chan engine.ObjectEvent, 1)
	ch <- engine.ObjectEvent{Type: engine.ObjectDeleted}
	close(ch)
	return ch, nil
}

// TestAnsweringWatcherDropsEventsAfterItsContextEnds asserts answeringWatcher
// reports and passes on no event once its context has ended: an event
// the stream hands over late belongs to a read that has returned.
func TestAnsweringWatcherDropsEventsAfterItsContextEnds(t *testing.T) {
	t.Parallel()
	seen := 0
	a := &answeringWatcher{answeringClient: &answeringClient{seen: func(*unstructured.Unstructured) { seen++ }}, watcher: eventsOnce{}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	events, err := a.WatchObject(ctx, kind("app").GVR, testNS, testRelease, "1", time.Second)
	require.NoError(t, err)
	var got []engine.ObjectEvent
	for ev := range events {
		got = append(got, ev)
	}
	assert.Empty(t, got)
	assert.Zero(t, seen)
}

// The revisions of an upgrade wait: the App the fake serves is at the
// recorded revision, and this request makes the next one.
const (
	recordedRevision = 3
	upgradedRevision = 4
)

var upgradeCmdOf = clierr.Rancher + " app upgrade 'my-nginx' --cluster 'prod' --namespace 'web' --dry-run"

// mineAt is the App at revision with this request's id and status.
func mineAt(status string, revision int64) scripted {
	return appRev(status, testRequestID, revision, "")
}

// atRecorded is the App still at the revision Plan read, with another
// request's id: the app controller has not written this request's revision.
func atRecorded() scripted { return appRev("deployed", otherID, recordedRevision, "") }

// upgradeAnotherChange is the message of a revision past the recorded one
// with another request's id.
func upgradeAnotherChange(version int64) string {
	return fmt.Sprintf("the upgrade ran; another change to 'my-nginx' also ran (revision %d)", version)
}

// upgradeFailedText is the error of an upgrade whose revision is at failed:
// description is Helm's text, list the exit code clause of the parenthesis
// ("" or ", exit code <n>"), cleanup the clause --cleanup-on-fail adds.
func upgradeFailedText(description, list string, cleanup bool) string {
	msg := "the upgrade of release 'my-nginx' failed: " + description + fmt.Sprintf(" (revision %d, status failed", upgradedRevision) + list + "); " +
		"Helm does not roll back: the resources this upgrade already changed stay changed, and in the release's history the last deployed revision still has the status deployed"
	if cleanup {
		msg += "; with --cleanup-on-fail, Helm was asked to delete the resources this upgrade created"
	}
	return msg + "; to upgrade again with a change, add the change to this preview command, read the preview, then add --apply --yes: " + upgradeCmdOf +
		fmt.Sprintf("; the next upgrade sends the values of revision %d, the revision that failed", upgradedRevision) + "; to remove the release, add --apply --yes after reading the preview of " + uninstallCmdOf
}

// upgradeWait plans app upgrade over f with the edits and executes it.
func upgradeWait(t *testing.T, f *appFake, edits ...func(*appInput)) waitRun {
	t.Helper()
	_, exec := plan(t, newTestUpgrade(f, nil), upgradeInput(edits...))
	return execute(t, exec)
}

// TestUpgradeWaitSucceeds asserts the ends of an upgrade wait that succeed:
// the revision after the recorded one with this request's id reads deployed,
// after the App showed the recorded revision or pending for a while. The
// result carries the revision, the status and the request id.
func TestUpgradeWaitSucceeds(t *testing.T) {
	t.Parallel()
	for name, apps := range map[string][]scripted{
		"deployed":                              {mineAt("deployed", upgradedRevision)},
		"the app lags at the recorded revision": {atRecorded(), atRecorded(), mineAt("deployed", upgradedRevision)},
		"pending then deployed":                 {mineAt("pending-upgrade", upgradedRevision), mineAt("pending-upgrade", upgradedRevision), mineAt("deployed", upgradedRevision)},
		"deployed with a description":           {appRev("deployed", testRequestID, upgradedRevision, "Upgrade complete")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := upgradeFake(t, nil)
				f.script(opKey, opRunning, opBehind, opDone)
				f.script(appKey, apps...)
				r := upgradeWait(t, f, waitFor(""))
				require.NoError(t, r.err)
				assert.Equal(t, result{
					Release: testRelease, Namespace: testNS,
					OperationName: testOperation, OperationNamespace: testNS, RequestID: testRequestID,
					Status: "deployed", Revision: upgradedRevision,
					FollowUp: []string{getAppHint},
				}, r.res.Data)
				assert.Equal(t, fmt.Sprintf("app my-nginx: upgraded (revision %d, status deployed)\nfollow it with: %s\n", upgradedRevision, getAppHint), r.res.Text)
				assert.NotContains(t, r.res.Warnings, warnNoWatch)
				assert.NotContains(t, r.res.Warnings, unchangedAppNote)
				assert.Less(t, r.elapsed, 30*time.Second)
				b, err := json.Marshal(r.res.Data)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(b, &fields))
				assert.Equal(t, float64(upgradedRevision), fields["revision"])
				assert.Equal(t, "deployed", fields["status"])
				assert.Equal(t, testRequestID, fields["requestID"])
			})
		})
	}
}

// TestUpgradeWaitAfterADoneOperation asserts the errors after an Operation
// that ended without Stalled: this revision failed, with and without
// --cleanup-on-fail, a revision past the recorded one with another id or none,
// and a status that stays at the end of the margin. An App that still shows
// the recorded revision keeps the wait reading until the timeout.
func TestUpgradeWaitAfterADoneOperation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		apps    []scripted
		edit    func(*appInput)
		timeout string
		want    string
		// margin is true when the error comes at the end of the margin.
		margin bool
	}{
		"failed": {
			apps: []scripted{appRev("failed", testRequestID, upgradedRevision, "Upgrade \"my-nginx\" failed:\n\ttimed out\x07 waiting")},
			want: upgradeFailedText("Upgrade \"my-nginx\" failed: timed out waiting", "", false),
		},
		"failed with --cleanup-on-fail": {
			apps: []scripted{appRev("failed", testRequestID, upgradedRevision, "post-upgrade hooks failed")},
			edit: withBool(flagCleanupOnFail, true),
			want: upgradeFailedText("post-upgrade hooks failed", "", true),
		},
		"failed after the app lagged": {
			apps: []scripted{atRecorded(), appRev("failed", testRequestID, upgradedRevision, "no description")},
			want: upgradeFailedText("no description", "", false),
		},
		"another change": {
			apps: []scripted{atRecorded(), appRev("deployed", otherID, upgradedRevision, "")},
			want: upgradeAnotherChange(upgradedRevision),
		},
		"another change with no id": {
			apps: []scripted{appRev("deployed", "", upgradedRevision, "")},
			want: upgradeAnotherChange(upgradedRevision),
		},
		"pending past the margin": {
			apps:   []scripted{mineAt("pending-upgrade", upgradedRevision)},
			want:   "Helm stopped and left release 'my-nginx' pending; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
			margin: true,
		},
		"superseded past the margin": {
			apps:   []scripted{mineAt("superseded", upgradedRevision)},
			want:   "release 'my-nginx' is at status superseded",
			margin: true,
		},
		"the app stays at the recorded revision": {
			apps:    []scripted{atRecorded()},
			timeout: "2m",
			want:    "timed out after 2m waiting for release 'my-nginx'; last state: operation 'helm-operation-abcde' Reconciling=False, Stalled=False; app status deployed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := upgradeFake(t, nil)
				f.script(opKey, opDone)
				f.script(appKey, tc.apps...)
				edits := []func(*appInput){waitFor(tc.timeout)}
				if tc.edit != nil {
					edits = append(edits, tc.edit)
				}
				r := upgradeWait(t, f, edits...)
				requireWaitError(t, r, tc.want)
				switch {
				case tc.timeout != "":
					assert.Equal(t, 2*time.Minute, r.elapsed)
				case tc.margin:
					assert.Equal(t, appMargin, r.elapsed, "the margin starts at the first read of that status")
				default:
					assert.Less(t, r.elapsed, 5*time.Second)
				}
			})
		})
	}
}

// TestUpgradeWaitAfterAStalledOperation asserts the App read after a Stalled
// Operation: this revision failed (with the exit code), deployed, pending, at
// another status or absent; an App still at the recorded revision is the
// no-revision error, which reads the Operation.
func TestUpgradeWaitAfterAStalledOperation(t *testing.T) {
	t.Parallel()
	noRevision := "and no revision of 'my-nginx' carries this request's id; Helm records no revision when it fails before it stores the release, " +
		"for example on values that fail the chart's values schema, template errors or resources another release owns; release 'my-nginx' is at " + fmt.Sprintf("revision %d", recordedRevision) + ", status deployed; read the operation with " +
		getOperationHint
	cases := map[string]struct {
		message string
		apps    []scripted
		edit    func(*appInput)
		want    string
		atOnce  bool
	}{
		"failed": {
			message: " exit code: 123",
			apps:    []scripted{atRecorded(), appRev("failed", testRequestID, upgradedRevision, "Upgrade \"my-nginx\" failed: image pull")},
			want:    upgradeFailedText("Upgrade \"my-nginx\" failed: image pull", ", exit code 123", false),
			atOnce:  true,
		},
		"failed with --cleanup-on-fail": {
			message: " exit code: 1",
			apps:    []scripted{appRev("failed", testRequestID, upgradedRevision, "hook failed")},
			edit:    withBool(flagCleanupOnFail, true),
			want:    upgradeFailedText("hook failed", ", exit code 1", true),
			atOnce:  true,
		},
		"pending then failed": {
			message: " exit code: 1",
			apps:    []scripted{mineAt("pending-upgrade", upgradedRevision), mineAt("pending-upgrade", upgradedRevision), appRev("failed", testRequestID, upgradedRevision, "hook failed")},
			want:    upgradeFailedText("hook failed", ", exit code 1", false),
			atOnce:  true,
		},
		"the app stays at the recorded revision": {
			message: " exit code: 1",
			apps:    []scripted{atRecorded()},
			want:    "the operation failed (exit code 1) " + noRevision,
		},
		"deployed": {
			message: " exit code: 1",
			apps:    []scripted{mineAt("deployed", upgradedRevision)},
			want:    "operation 'helm-operation-abcde' failed (exit code 1) after release 'my-nginx' was deployed",
			atOnce:  true,
		},
		"pending at the end": {
			message: " exit code: 137",
			apps:    []scripted{mineAt("pending-upgrade", upgradedRevision)},
			want:    "Helm stopped (exit code 137) and left release 'my-nginx' pending; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf,
		},
		"another change": {
			message: " exit code: 1",
			apps:    []scripted{appRev("deployed", otherID, upgradedRevision, "")},
			want:    fmt.Sprintf("this request failed (exit code 1) and another change to 'my-nginx' ran (revision %d)", upgradedRevision),
			atOnce:  true,
		},
		"another status at the end": {
			message: " exit code: 1",
			apps:    []scripted{mineAt("superseded", upgradedRevision)},
			want:    "operation 'helm-operation-abcde' failed (exit code 1); release 'my-nginx' is at status superseded",
		},
		"a message with no exit code": {
			message: "OOMKilled",
			apps:    []scripted{appRev("failed", testRequestID, upgradedRevision, "out of memory")},
			want:    upgradeFailedText("out of memory", ", operation message: OOMKilled", false),
			atOnce:  true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := upgradeFake(t, nil)
				f.script(opKey, opRunning, opStalled(tc.message))
				f.script(appKey, tc.apps...)
				edits := []func(*appInput){waitFor("")}
				if tc.edit != nil {
					edits = append(edits, tc.edit)
				}
				r := upgradeWait(t, f, edits...)
				ce := r.clierr(t)
				requireCommitted(t, ce, clierr.CodeGeneric)
				assert.Equal(t, tc.want, ce.Message)
				if strings.Contains(tc.want, "no revision of") {
					assert.Equal(t, getOperationHint, ce.Hint, "the Operation holds the run's result")
				} else {
					assert.Equal(t, getAppHint, ce.Hint)
				}
				if tc.atOnce {
					assert.Less(t, r.elapsed, 10*time.Second)
				} else {
					assert.Equal(t, time.Second+appMargin, r.elapsed, "the margin starts at the Stalled read")
				}
			})
		})
	}
}

// TestUpgradeWaitWithoutTheOperation asserts the wait of an upgrade when the
// caller cannot get operations: the warning names what the wait cannot see for
// an upgrade, the App's ends are the same, and an App that stays at the
// recorded revision ends at the timeout, whose message says the same.
func TestUpgradeWaitWithoutTheOperation(t *testing.T) {
	t.Parallel()
	const fact = "an upgrade Helm refuses before it stores a revision is seen only as the wait timeout"
	warning := "you cannot get operations in namespace 'web'; the wait reads the app only, and " + fact + "; a shorter --timeout ends that wait sooner"
	synctest.Test(t, func(t *testing.T) {
		f := upgradeFake(t, nil)
		skipOperation(f)
		f.script(appKey, atRecorded(), mineAt("deployed", upgradedRevision))
		eff, exec := plan(t, newTestUpgrade(f, nil), upgradeInput(waitFor("")))
		assert.Contains(t, eff.Warnings, warning)
		r := execute(t, exec)
		require.NoError(t, r.err)
		assert.Equal(t, "deployed", r.res.Data.(result).Status)
		assert.Equal(t, int64(upgradedRevision), r.res.Data.(result).Revision)
		assert.NotContains(t, f.reads, opKey)
	})
	t.Run("review failed", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil)
		skipOperation(f)
		f.review = errors.New("the review request failed")
		eff, _, err := newTestUpgrade(f, nil).Plan(t.Context(), upgradeInput(waitFor("")))
		require.NoError(t, err)
		assert.Contains(t, eff.Warnings, "rancher-cli could not check whether you can get operations in namespace 'web'; the wait reads the app only, and "+fact+"; a shorter --timeout ends that wait sooner")
		assert.NotContains(t, eff.Warnings, warning)
	})
	synctest.Test(t, func(t *testing.T) {
		f := upgradeFake(t, nil)
		skipOperation(f)
		f.script(appKey, atRecorded())
		r := upgradeWait(t, f, waitFor("1m"))
		requireWaitError(t, r, "timed out after 1m waiting for release 'my-nginx'; last state: app status deployed; "+fact)
		assert.Equal(t, time.Minute, r.elapsed)
	})
	for name, tc := range map[string]struct {
		apps []scripted
		want string
	}{
		"failed":         {[]scripted{appRev("failed", testRequestID, upgradedRevision, "bad values")}, upgradeFailedText("bad values", "", false)},
		"another change": {[]scripted{appRev("deployed", otherID, upgradedRevision, "")}, upgradeAnotherChange(upgradedRevision)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := upgradeFake(t, nil)
				skipOperation(f)
				f.script(appKey, tc.apps...)
				requireWaitError(t, upgradeWait(t, f, waitFor("")), tc.want)
			})
		})
	}
}

// TestUpgradeWaitWhenTheOperationIsGone asserts a NOT_FOUND of the Operation
// is a warning with the App read alone, and a pending revision is cut off
// only when the Operation was read before.
func TestUpgradeWaitWhenTheOperationIsGone(t *testing.T) {
	t.Parallel()
	gone := operationGoneWarning(testOperation)
	synctest.Test(t, func(t *testing.T) {
		f := upgradeFake(t, nil)
		f.script(opKey, opRunning, absent)
		f.script(appKey, mineAt("pending-upgrade", upgradedRevision))
		requireWaitError(t, upgradeWait(t, f, waitFor("")),
			"Helm's run was cut off; release 'my-nginx' stays pending; to remove it, add --apply --yes after reading the preview of "+uninstallCmdOf)
	})
	synctest.Test(t, func(t *testing.T) {
		f := upgradeFake(t, nil)
		f.script(opKey, opRunning, absent)
		f.script(appKey, atRecorded(), mineAt("deployed", upgradedRevision))
		r := upgradeWait(t, f, waitFor(""))
		require.NoError(t, r.err)
		assert.Equal(t, []string{gone}, r.res.Warnings)
	})
}
