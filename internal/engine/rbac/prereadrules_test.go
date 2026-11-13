package rbac

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The policies a read of grant or revoke can have when the server refuses it.
const (
	policyContinue       = "continue with warning"
	policyFailSentence   = "fail with sentence"
	policyDegradeWarning = "degrade with warning"
	policyFailBefore     = "fail as before"
	policyDegradeBefore  = "degrade as before"
	policyCaller         = "named by the caller, not a pre-read"
)

var reviewedPolicies = []string{
	policyContinue, policyFailSentence, policyDegradeWarning,
	policyFailBefore, policyDegradeBefore, policyCaller,
}

// readRule is the policy of a read site and the cases that exercise it: a row
// name of refusedReadRows, or the name of an existing test function.
type readRule struct {
	policy string
	cases  []string
}

// readSiteRules names the policy of every read of the files grant and revoke
// resolve their subject and scope in. A site is the file, the enclosing
// top-level function (a method as type.Method), the callee and the ordinal of
// the call among the calls of that callee in the function, in source order.
// Adding a read means adding a site here with the cases that exercise it.
var readSiteRules = map[string][]readRule{
	"resolve.go ResolveSubject Get 1": {
		{policyContinue, []string{"grant local principal, user read refused", "revoke local principal, user read refused"}},
		{policyFailSentence, []string{"grant external principal, user read refused"}},
	},
	"resolve.go ResolveSubject List 1": {
		{policyFailSentence, []string{"grant external principal, user not found and users list refused"}},
	},
	"resolve.go ResolveSubject Get 2": {
		{policyContinue, []string{"grant bare id, user read refused", "revoke bare id, user read refused"}},
		{policyFailSentence, []string{"grant bare name, user read refused", "grant username user-b, user read refused", "revoke bare name, user read refused"}},
		{policyFailBefore, []string{"grant bare id, user read fails"}},
	},
	"resolve.go ResolveSubject List 2": {
		{policyFailSentence, []string{"grant bare id, user not found and users list refused"}},
	},
	"grant.go grantCmd.Plan List 1": {
		{policyDegradeWarning, []string{"grant readable user, bindings list refused", "grant bare id, user read and bindings list refused"}},
		{policyFailBefore, []string{"grant readable user, bindings list fails", "grant readable user, everything readable"}},
	},
	"grant.go checkRole Get 1": {
		{policyDegradeBefore, []string{"grant readable user, role read refused"}},
	},
	"grant.go rolePoolAtScope List 1": {
		{policyDegradeBefore, []string{"grant missing role, role templates list refused"}},
	},
	"grant.go listRoleTemplateObjects List 1": {
		{policyDegradeBefore, []string{"TestGrantRoleErrorCandidatesListTheRoleTemplatesOnlyWhenNeeded"}},
	},
	"revoke.go revokeCmd.Plan List 1": {
		{policyFailBefore, []string{"revoke bare id, user read and bindings list refused", "revoke readable user"}},
	},
	"revoke.go revokeCmd.Plan Delete 1": {
		{policyFailBefore, []string{"revoke bare id, user read refused", "revoke local principal, user read refused"}},
	},
	"scope.go ResolveScope ResolveDownstreamCluster 1": {
		{policyFailBefore, []string{"TestResolveScopeProjectFromContext"}},
	},
	"scope.go ResolveScope TypedProjectOn 1": {
		{policyFailBefore, []string{"TestResolveScopeProjectFromContext"}},
	},
	"scope.go ResolveScope BackingNamespace 1": {
		{policyFailBefore, []string{"TestResolveScopeProjectFromContext"}},
	},
	"scope.go ResolveScope ResolveDownstreamCluster 2": {
		{policyFailBefore, []string{"TestResolveScopeCluster"}},
	},
}

// readSiteAllowlist names the sites that are not pre-reads, each with the
// reason.
var readSiteAllowlist = map[string]string{
	"revoke.go revokeCmd.Plan Delete 2": "the delete the user confirmed, run by the execute function; the dry-run delete before it is the check",
}

// The scan covers the calls whose selector is one of readSelectors (the
// methods of the cluster client that talk to the server) or whose name is one
// of scopeReads (the reads of ResolveScope are those three calls).
var (
	readSelectors = []string{"Get", "List", "Delete", "ServerResources", "PostReview", "Logs"}
	scopeReads    = []string{"ResolveDownstreamCluster", "TypedProjectOn", "BackingNamespace"}
	scannedFiles  = []string{"resolve.go", "grant.go", "revoke.go", "scope.go"}
)

// scanReadSites lists the sites of the reads in the scanned files, in source
// order.
func scanReadSites(t *testing.T) []string {
	t.Helper()
	var sites []string
	for _, file := range scannedFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			counts := map[string]int{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if slices.Contains(readSelectors, fun.Sel.Name) || slices.Contains(scopeReads, fun.Sel.Name) {
						callee = fun.Sel.Name
					}
				case *ast.Ident:
					if slices.Contains(scopeReads, fun.Name) {
						callee = fun.Name
					}
				}
				if callee == "" {
					return true
				}
				counts[callee]++
				sites = append(sites, fmt.Sprintf("%s %s %s %d", filepath.ToSlash(file), name, callee, counts[callee]))
				return true
			})
		}
	}
	return sites
}

// testFunctionNames lists the top-level test functions of this package.
func testFunctionNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	var names []string
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}

// TestEveryPreReadOfGrantAndRevokeHasARule runs every row of the table of
// refused reads, and asserts every read site has a reviewed policy and at
// least one case, a row or an existing test, that exercises it.
// Mutation: restore the fatal return of a refused bare read in
// ResolveSubject (the rows that continue fail); leave out the sentence of a
// refused read (the rows that fail with it fail).
func TestEveryPreReadOfGrantAndRevokeHasARule(t *testing.T) {
	rows := refusedReadRows()
	rowNames := map[string]bool{}
	for _, row := range rows {
		require.False(t, rowNames[row.name], "duplicate row name %q", row.name)
		rowNames[row.name] = true
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			assertRefusedReadRow(t, row)
		})
	}

	t.Run("every site has a policy and a case", func(t *testing.T) {
		tests := testFunctionNames(t)
		for site, rules := range readSiteRules {
			require.NotEmpty(t, rules, "site %q has no rule", site)
			for _, rule := range rules {
				assert.Contains(t, reviewedPolicies, rule.policy, "site %q", site)
				require.NotEmpty(t, rule.cases, "site %q policy %q has no case", site, rule.policy)
				for _, c := range rule.cases {
					assert.True(t, rowNames[c] || slices.Contains(tests, c), "site %q names %q, which is neither a row nor a test function", site, c)
				}
			}
		}
	})
}

// TestNoPreReadWithoutAReviewedRule asserts every read of the server in the
// files that resolve the subject and the scope of grant and revoke, and every
// delete of revoke, is a site with a reviewed rule or an allowlist reason,
// and that no rule or allowlist entry names a call that is gone.
// Mutation: add a client.Get call in Plan of grant.go.
func TestNoPreReadWithoutAReviewedRule(t *testing.T) {
	sites := scanReadSites(t)
	for _, site := range sites {
		_, ruled := readSiteRules[site]
		reason, allowed := readSiteAllowlist[site]
		assert.True(t, ruled || allowed, "read site %q has no reviewed rule: add it to readSiteRules with the cases that exercise it", site)
		assert.False(t, ruled && allowed, "read site %q is both ruled and allowlisted", site)
		if allowed {
			assert.NotEmpty(t, reason)
		}
	}
	for site := range readSiteRules {
		assert.Contains(t, sites, site, "a rule names a call that is gone")
	}
	for site := range readSiteAllowlist {
		assert.Contains(t, sites, site, "an allowlist entry names a call that is gone")
	}
}
