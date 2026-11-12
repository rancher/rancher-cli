package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Project the CLI creates carries the creator annotation, so Rancher adds
// the creator's owner binding. Two scans keep that true for every create
// path, now and later:
//
//   - Every function that plans a create (the lines of
//     internal/cli/testdata/create-planner-callers.txt) is reviewed for
//     Projects: it creates one, and then names a row of the table in
//     internal/cli/projectcreatorid_conformance_test.go that runs the path
//     and asserts the annotation, or it creates no Project.
//   - Every call of a method named Create in the non-test Go files under
//     internal/ is reviewed too, by file and function, with what it creates.

const (
	projectCreateGuardFile = "internal/cli/projectcreatorid_conformance_test.go"
	createsAProject        = "creates a Project: "
	createsNoProject       = "creates no Project: "
)

// plannerProjectReview has one entry per line of the create planner caller
// file: what the command creates.
var plannerProjectReview = map[string]string{
	"internal/engine/extres.createKubeconfigCmd.Plan":            createsNoProject + "a kubeconfig",
	"internal/engine/extres.createPasswordChangeRequestCmd.Plan": createsNoProject + "a password change request",
	"internal/engine/extres.createTokenCmd.Plan":                 createsNoProject + "an ext.cattle.io token",
	"internal/engine/oidcclient.createCmd.Plan":                  createsNoProject + "an OIDCClient",
	"internal/engine/projects.createCmd.Plan":                    createsAProject + "create project",
	"internal/engine/projects.createNamespaceCmd.Plan":           createsNoProject + "a namespace",
	"internal/engine/projects.createSecretCmd.Plan":              createsNoProject + "a secret",
	"internal/engine/provisioning.createClusterCmd.Plan":         createsNoProject + "a provisioning cluster",
	"internal/engine/provisioning.createMachineConfigCmd.Plan":   createsNoProject + "a machine config",
	"internal/engine/rbac.grantCmd.Plan":                         createsNoProject + "a role binding",
	"internal/engine/resources.createCmd.Plan":                   createsAProject + "create resource with a Project manifest",
	"internal/engine/user.createCmd.Plan":                        createsNoProject + "a user and its bindings",
}

// TestEveryCreateSiteIsReviewedForProjects asserts every function that plans
// a create is reviewed for Projects, and that a function reviewed as creating
// one names a row of the table that asserts the creator annotation.
func TestEveryCreateSiteIsReviewedForProjects(t *testing.T) {
	t.Parallel()
	keys, err := readPlannerCallerKeys(filepath.FromSlash(createPlannerCallers))
	require.NoError(t, err)
	for _, key := range keys {
		if _, ok := plannerProjectReview[key]; !ok {
			t.Errorf("%s plans a create and is not reviewed for Projects: add it to plannerProjectReview", key)
		}
	}
	guard, err := os.ReadFile(filepath.FromSlash(projectCreateGuardFile))
	require.NoError(t, err)
	for key, review := range plannerProjectReview {
		if !slices.Contains(keys, key) {
			t.Errorf("plannerProjectReview has an entry for %s, which the create planner caller file does not list", key)
		}
		switch {
		case strings.HasPrefix(review, createsNoProject):
		case strings.HasPrefix(review, createsAProject):
			row := strings.TrimPrefix(review, createsAProject)
			assert.Contains(t, string(guard), `name: "`+row+`"`, "%s creates a Project and names no row of the table in %s", key, projectCreateGuardFile)
		default:
			t.Errorf("%s: the review %q starts with neither %q nor %q", key, review, createsAProject, createsNoProject)
		}
	}
}

// createCallRow is one reviewed function that calls a method named Create
// or uses kube.NewWriteRequest, with the number of such uses in it.
type createCallRow struct {
	file     string
	function string
	calls    int
	creates  string
}

// createCallRows lists every function outside tests, under internal/, that
// calls a method named Create. A second call in a listed function changes
// its count and fails until reviewed.
var createCallRows = []createCallRow{
	{"internal/client/provider/client.go", "dynamicClient.Create", 1, "any kind: the dynamic client under every ClusterClient create; a Project reaches it through engine.PlanCreate from the paths the Project guard runs"},
	{"internal/client/provider/identity.go", "Identity", 1, "a SelfUser"},
	{"internal/client/provider/owner.go", "Owner", 1, "a SelfSubjectAccessReview"},
	{"internal/engine/effect.go", "PlanCreateObserved", 2, "the shared create and dry-run"},
	{"internal/engine/extres/create.go", "createTokenCmd.Plan", 1, "an ext.cattle.io token"},
	{"internal/engine/extres/createpasswordchangerequest.go", "createPasswordChangeRequestCmd.Plan", 2, "a password change request"},
	{"internal/engine/extres/kubeconfig.go", "createKubeconfigCmd.Plan", 1, "a kubeconfig"},
	{"internal/engine/oidcclient/create.go", "createCmd.execute", 1, "an OIDCClient"},
	{"internal/engine/projects/creatorid.go", "callerUserID", 1, "a SelfUser"},
	{"internal/engine/provisioning/createcluster.go", "stampCreatorID", 1, "a SelfUser"},
	{"internal/engine/rbac/deleteuser.go", "guardSelfDelete", 1, "a SelfUser"},
	{"internal/engine/user/create.go", "createCmd.execute", 3, "a user, its password Secret and its global role bindings"},
}

// writeRequestRows lists every function outside tests, under internal/, that
// uses kube.NewWriteRequest, the builder of the raw Kubernetes writes that
// are not a method named Create. None creates a Project.
var writeRequestRows = []createCallRow{
	{"internal/cli/engine.go", "actionClient.PostAction", 1, "a steve action"},
	{"internal/client/provider/review.go", "dynamicClient.PostReview", 1, "an authorization or authentication review (the kinds engine.ReviewGVR accepts)"},
	{"internal/token/directwait.go", "probeDirect", 1, "a SelfSubjectReview"},
	{"internal/token/owner.go", "requestOwner", 1, "a SelfSubjectReview"},
	{"internal/token/token.go", "CreateDerivedToken", 1, "a derived token"},
}

// useCounts returns "file function" -> number of uses for every function in
// the non-test Go files under internal/ that uses a selector named name: a
// call of a method named Create when callsOnly is set, any use otherwise. A
// use outside a function has the function "package-level".
func useCounts(t *testing.T, name string, callsOnly bool) map[string]int {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := map[string]int{}
	for _, path := range files {
		if !strings.HasPrefix(path, "internal/") || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		for _, decl := range file.Decls {
			function := "package-level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				function = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					function = createCallReceiver(fd.Recv.List[0].Type) + "." + function
				}
			}
			key := filepath.ToSlash(path) + " " + function
			ast.Inspect(decl, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CallExpr:
					if sel, ok := v.Fun.(*ast.SelectorExpr); ok && callsOnly && sel.Sel.Name == name {
						found[key]++
					}
				case *ast.SelectorExpr:
					if !callsOnly && v.Sel.Name == name {
						found[key]++
					}
				}
				return true
			})
		}
	}
	return found
}

func createCallReceiver(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return createCallReceiver(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return createCallReceiver(v.X)
	}
	return ""
}

// compareUses reports every use the scan found that no row lists, every row
// with no use, and every row whose count differs.
func compareUses(t *testing.T, what string, found map[string]int, rows []createCallRow) {
	t.Helper()
	reviewed := map[string]int{}
	for _, row := range rows {
		assert.NotEmpty(t, row.creates, "%s %s: say what it creates", row.file, row.function)
		reviewed[row.file+" "+row.function] = row.calls
	}
	for key, n := range found {
		want, ok := reviewed[key]
		switch {
		case !ok:
			t.Errorf("%q uses %s %d time(s) and is not a reviewed row: add it to the rows", key, what, n)
		case want != n:
			t.Errorf("%q uses %s %d time(s), its row says %d: review the new use", key, what, n, want)
		}
	}
	for key := range reviewed {
		if _, ok := found[key]; !ok {
			t.Errorf("the rows have %q, which has no use of %s", key, what)
		}
	}
}

// TestEveryCreateCallIsReviewedForProjects asserts every function that calls
// a method named Create is a reviewed row with the right number of calls, and
// every row still has a call.
func TestEveryCreateCallIsReviewedForProjects(t *testing.T) {
	t.Parallel()
	compareUses(t, "a method named Create", useCounts(t, "Create", true), createCallRows)
}

// TestEveryWriteRequestIsReviewedForProjects asserts every function that uses
// kube.NewWriteRequest is a reviewed row with the right number of uses, and
// every row still has one. A raw write could create a Project without a
// method named Create.
func TestEveryWriteRequestIsReviewedForProjects(t *testing.T) {
	t.Parallel()
	compareUses(t, "NewWriteRequest", useCounts(t, "NewWriteRequest", false), writeRequestRows)
}
