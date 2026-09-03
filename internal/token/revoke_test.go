package token

import (
	"context"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRevokeTokenChoosesPathByKind asserts the two delete paths and the 404
// reading: ext names go to the ext store, v3 names to the v3 path, and a
// missing token is alreadyGone, not a failure.
func TestRevokeTokenChoosesPathByKind(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{SeedToken: "seed:s"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "seed:s")
	assert.Equal(t, ResultAlreadyGone, out.Result)
	recs := f.DeleteRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, testrancher.DeleteRecord{Kind: "v3", Name: "token-1", Bearer: "seed:s"}, recs[0])
	out = revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "ext-1"}, "seed:s")
	assert.Equal(t, ResultAlreadyGone, out.Result)
	assert.Equal(t, "ext", f.DeleteRequests()[1].Kind)
}

// TestRevokeTokenFailedCarriesStatus asserts the failed shape.
func TestRevokeTokenFailedCarriesStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(srv.Close)
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "ext-1"}, "b")
	assert.Equal(t, ResultFailed, out.Result)
	assert.Contains(t, out.Error, "500")
}

// TestCleanupContextIsDetachedAndBounded asserts the two properties every
// revoke relies on: a cancelled operation context does not cancel it, and
// it ends within the bound.
func TestCleanupContextIsDetachedAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		cancel()
		ctx, done := cleanupContext(parent)
		defer done()
		require.NoError(t, ctx.Err(), "detached from the cancelled operation context")
		time.Sleep(cleanupTimeout + time.Second)
		synctest.Wait()
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	})
}

// TestUserSuppliedRevokeIsNotApplicable asserts that a pasted token is never
// revoked and no request is sent.
func TestUserSuppliedRevokeIsNotApplicable(t *testing.T) {
	t.Parallel()
	c := UserSupplied("id:secret")
	assert.Equal(t, ProvenanceUserSupplied, c.Provenance)
	assert.Equal(t, ResultNotApplicable, c.Revoke().Result)
}

// TestRevokeHasNoContextParameter asserts the API shape: the exported revoke
// cannot receive the cancelled operation context, and it is the only
// exported method on the credential.
func TestRevokeHasNoContextParameter(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[LoginCredential]()
	m, ok := typ.MethodByName("Revoke")
	require.True(t, ok)
	assert.Equal(t, 1, m.Type.NumIn(), "receiver only")
	assert.Equal(t, 1, typ.NumMethod(), "Revoke is the only exported method")
}

// TestNoExportedRevokeFunctionBesidesTheMethod asserts that the package
// exports no free function whose name contains Revoke: the safe, bounded,
// detached cleanup call is the method alone.
func TestNoExportedRevokeFunctionBesidesTheMethod(t *testing.T) {
	t.Parallel()
	fset := gotoken.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, perr)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if fn.Name.IsExported() && strings.Contains(fn.Name.Name, "Revoke") {
				t.Errorf("%s: exported revoke function %s", fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
}
