package catalog

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// waitErrorBuilders builds one error with each error builder of
// waiterrors.go: every function there that returns error or *clierr.Error.
// A builder with no row fails TestEveryWaitErrorIsCommittedAndNotRetryable.
var waitErrorBuilders = map[string]func(w *appWait) error{
	"committedError": func(w *appWait) error { return w.committedError("message", w.t.getApp()) },
	"abortedError": func(w *appWait) error {
		return w.abortedError(clierr.New(clierr.CodeAuth, "401").WithRetryable(true))
	},
	"cancelledError":                 func(w *appWait) error { return w.cancelledError() },
	"timeoutError":                   func(w *appWait) error { return w.timeoutError(clierr.Retryable(clierr.CodeGeneric, "500")) },
	"failureError":                   func(w *appWait) error { return w.failureError(revision{status: "failed"}, &stall{}) },
	"anotherChangeError":             func(w *appWait) error { return w.anotherChangeError(2) },
	"anotherChangeAfterFailureError": func(w *appWait) error { return w.anotherChangeAfterFailureError(stall{}, 2) },
	"leftPendingError":               func(w *appWait) error { return w.leftPendingError(nil) },
	"atStatusError":                  func(w *appWait) error { return w.atStatusError(revision{}) },
	"deployedButFailedError":         func(w *appWait) error { return w.deployedButFailedError(stall{}) },
	"failedAtStatusError":            func(w *appWait) error { return w.failedAtStatusError(stall{}, revision{}) },
	"noRevisionError":                func(w *appWait) error { return w.noRevisionError(stall{}, &revision{}) },
	"cutOffError":                    func(w *appWait) error { return w.cutOffError() },
	"uninstalledButFailedError":      func(w *appWait) error { return w.uninstalledButFailedError(stall{}, true) },
	"staysInstalledError":            func(w *appWait) error { return w.staysInstalledError(nil, revision{}) },
	"keptHistoryError":               func(w *appWait) error { return w.keptHistoryError() },
	"passThroughError": func(w *appWait) error {
		return w.passThroughError(clierr.Retryable(clierr.CodeGeneric, "the server answered 503").WithHint(testGetOperationsHint))
	},
}

// waitPassThrough names the errors run returns that no builder of the
// table above makes on its own, with how each still carries the committed
// details and is not retryable.
var waitPassThrough = map[string]string{
	"AUTH or TLS_TRUST from engine.WaitObject": "OnAbort makes it with abortedError, which keeps the code and the hint",
	"a cancelled context":                      "OnCancel returns cancelledError in place of the context's error",
	"any other error":                          "run passes every error through passThroughError, which keeps its code and hint",
}

// TestEveryWaitErrorIsCommittedAndNotRetryable asserts every error builder
// of the wait, which runs after Rancher accepted the request, gives an
// error that is not retryable and carries the committed details whose
// recovery is the hint's command, and that wait.go builds no error of its
// own: a source scan lists the builders of waiterrors.go, each of which
// needs a row in waitErrorBuilders.
func TestEveryWaitErrorIsCommittedAndNotRetryable(t *testing.T) {
	t.Parallel()
	assert.ElementsMatch(t, waitErrorBuilderNames(t), slices.Collect(maps.Keys(waitErrorBuilders)),
		"every function of waiterrors.go that returns error or *clierr.Error has a row")
	for name, build := range waitErrorBuilders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := &appWait{t: actionTarget{typedCluster: testTypedCluster, namespace: testNS, release: testRelease, requestID: testRequestID},
				op: actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, filter: Values{}.filterFunc()}
			ce, ok := errors.AsType[*clierr.Error](build(w))
			require.True(t, ok)
			assert.False(t, ce.Retryable, "not retryable")
			d, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok, "details %T", ce.Details)
			assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "app", ResourceName: testRelease, Recovery: d.Recovery}, d)
			assert.NotEmpty(t, d.Recovery)
			if ce.Code == clierr.CodeGeneric {
				assert.Equal(t, ce.Hint, d.Recovery, "the recovery is the hint's command")
			}
		})
	}
	assert.Empty(t, errorConstructorsIn(t, "wait.go"), "wait.go builds its errors with the builders of waiterrors.go")
}

// waitErrorBuilderNames lists the functions and methods of waiterrors.go
// whose one result is error or *clierr.Error.
func waitErrorBuilderNames(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "waiterrors.go", nil, 0)
	require.NoError(t, err)
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			continue
		}
		switch res := fn.Type.Results.List[0].Type.(type) {
		case *ast.Ident:
			if res.Name == "error" {
				names = append(names, fn.Name.Name)
			}
		case *ast.StarExpr:
			if sel, ok := res.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "clierr" {
					names = append(names, fn.Name.Name)
				}
			}
		}
	}
	return names
}

// errorConstructorsIn lists the calls in file that create or change an
// error's code or retry advice.
func errorConstructorsIn(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	require.NoError(t, err)
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if pkg, ok := sel.X.(*ast.Ident); ok {
			name = pkg.Name + "." + name
		}
		switch name {
		case "clierr.New", "clierr.Retryable", "clierr.From", "engine.CommittedPartialSuccess", "errors.New", "fmt.Errorf":
			out = append(out, name)
		}
		if sel.Sel.Name == "WithRetryable" || sel.Sel.Name == "WithDetails" {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

// testGetOperationsHint and testLoginHint are commands a hint of an error a
// read returned can name.
var (
	testGetOperationsHint = clierr.Rancher + " get operations --cluster 'prod' --namespace 'web'"
	testLoginHint         = clierr.Rancher + " login"
)

// TestWaitPassThroughErrorsAreCommitted asserts every error of
// waitPassThrough leaves run with the committed details and not retryable:
// an AUTH and a TLS_TRUST failure keep their code and hint with the get app
// recovery, a cancel is GENERIC, and an error the steps return as a read
// gave it keeps its code and hint, which is its recovery, or gains the get
// app command as both when it has no hint.
func TestWaitPassThroughErrorsAreCommitted(t *testing.T) {
	t.Parallel()
	require.Len(t, waitPassThrough, 3, "a new pass-through case needs a row here")
	getApp := clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	raw := func(err error) func(w *appWait) error {
		return func(w *appWait) error {
			w.stepsForTest = func(context.Context) (waitEnd, error) { return waitEnd{}, err }
			_, out := w.run(t.Context())
			return out
		}
	}
	for name, tc := range map[string]struct {
		run            func(w *appWait) error
		code           string
		hint, recovery string
	}{
		"AUTH": {func(w *appWait) error {
			w.client = failingClient{err: clierr.New(clierr.CodeAuth, "401").WithRetryable(true).WithHint(testLoginHint)}
			_, err := w.run(t.Context())
			return err
		}, clierr.CodeAuth, testLoginHint, getApp},
		"TLS_TRUST": {func(w *appWait) error {
			w.client = failingClient{err: clierr.New(clierr.CodeTLSTrust, "untrusted").WithHint(testLoginHint)}
			_, err := w.run(t.Context())
			return err
		}, clierr.CodeTLSTrust, testLoginHint, getApp},
		"cancel": {func(w *appWait) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			w.client = failingClient{err: clierr.New(clierr.CodeGeneric, "unused")}
			_, err := w.run(ctx)
			return err
		}, clierr.CodeGeneric, getApp, getApp},
		"raw GENERIC with a hint": {raw(clierr.Retryable(clierr.CodeGeneric, "the server answered 503").WithHint(testGetOperationsHint)),
			clierr.CodeGeneric, testGetOperationsHint, testGetOperationsHint},
		"raw NOT_FOUND": {raw(clierr.Retryable(clierr.CodeNotFound, "gone")), clierr.CodeNotFound, getApp, getApp},
		"raw error":     {raw(errors.New("connection reset")), clierr.CodeGeneric, getApp, getApp},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := &appWait{t: actionTarget{typedCluster: testTypedCluster, namespace: testNS, release: testRelease, requestID: testRequestID},
				op: actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, filter: Values{}.filterFunc(),
				install: true, timeout: time.Minute, interval: time.Second}
			ce, ok := errors.AsType[*clierr.Error](tc.run(w))
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			assert.False(t, ce.Retryable)
			d, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok, "details %T", ce.Details)
			assert.True(t, d.Committed)
			assert.Equal(t, testRelease, d.ResourceName)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, tc.recovery, d.Recovery)
		})
	}
}

// failingClient answers every Get with err.
type failingClient struct {
	engine.ClusterClient
	err error
}

func (f failingClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, f.err
}
