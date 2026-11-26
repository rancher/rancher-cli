package conformance

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reasons a list sent with no namespace is allowed. A caller whom Rancher
// grants objects only by name, or in some namespaces only, is refused such a
// list by the Kubernetes API.
const (
	reasonAcrossTheCluster = "lists a kind that has no namespace, or lists it across every namespace of the cluster; a refusal is returned as it is"
	reasonWholeCluster     = "lists the pods, nodes or deployments of the whole cluster for a usage or inspect report; a refusal makes the section unavailable"
)

// listsAcrossAllNamespaces maps each non-test function that sends a list with
// the literal "" as its namespace to why that is allowed, as
// <file>:<function>. A new function that does so fails
// TestEveryListAcrossAllNamespacesIsListed until it is listed with a reason;
// a row whose function no longer does so fails too.
var listsAcrossAllNamespaces = map[string]string{
	"internal/engine/access/bindings.go:readProvenance":                      reasonAcrossTheCluster,
	"internal/engine/access/candidates.go:listCandidates":                    reasonAcrossTheCluster,
	"internal/engine/access/chain.go:aggregatedRoles":                        reasonAcrossTheCluster,
	"internal/engine/access/identity.go:noRancherBindingsFor":                reasonAcrossTheCluster,
	"internal/engine/access/scope.go:resolveScope":                           "lists the namespaces of the cluster for access --all-namespaces; a caller who cannot list them is refused",
	"internal/engine/access/subject.go:findRancherUser":                      reasonAcrossTheCluster,
	"internal/engine/catalog/charts.go:enabledRepos":                         reasonAcrossTheCluster,
	"internal/engine/catalog/extensionadd.go:Plan":                           reasonAcrossTheCluster,
	"internal/engine/catalog/extensionlist.go:listUIPlugins":                 reasonAcrossTheCluster,
	"internal/engine/inspect/images.go:imagesFor":                            reasonWholeCluster,
	"internal/engine/inspect/images_workload.go:workloadImagesFor":           reasonWholeCluster,
	"internal/engine/inspect/project.go:downstreamSections":                  reasonWholeCluster,
	"internal/engine/inspect/usage.go:collectByNamespace":                    reasonWholeCluster,
	"internal/engine/inspect/usage_node.go:collectByNode":                    reasonWholeCluster,
	"internal/engine/inspect/usage_project.go:collectByProject":              "lists the namespaces of the cluster for usage --by project; a caller who cannot list them is refused",
	"internal/engine/observe/bundle.go:collectCluster":                       reasonAcrossTheCluster,
	"internal/engine/observe/bundle.go:collectFeatures":                      reasonAcrossTheCluster,
	"internal/engine/provisioning/cloudcredwrite.go:listClusters":            reasonAcrossTheCluster,
	"internal/engine/provisioning/cloudcredwrite.go:resolveCredentialDriver": reasonAcrossTheCluster,
	"internal/engine/provisioning/upgradeinfo.go:readNodes":                  reasonAcrossTheCluster,
	"internal/engine/rbac/grant.go:listRoleTemplateObjects":                  reasonAcrossTheCluster,
	"internal/engine/rbac/grant.go:rolePoolAtScope":                          reasonAcrossTheCluster,
	"internal/engine/rbac/heldbindings.go:HeldClusterBindings":               reasonAcrossTheCluster,
	"internal/engine/rbac/heldbindings.go:HeldGlobalBindings":                reasonAcrossTheCluster,
	"internal/engine/rbac/heldbindings.go:HeldProjectBindings":               reasonAcrossTheCluster,
	"internal/engine/rbac/inspectresolve.go:resolveInspectUserBare":          reasonAcrossTheCluster,
	"internal/engine/rbac/inspectresolve.go:resolveInspectUserPrincipal":     reasonAcrossTheCluster,
	"internal/engine/rbac/members.go:resolveUserDisplayNames":                reasonAcrossTheCluster,
	"internal/engine/rbac/resolve.go:ResolveSubject":                         reasonAcrossTheCluster,
	"internal/engine/steve/projects.go:ProjectNamespaceObjects":              "lists the namespaces through the Kubernetes API, narrowed by the project label, and reads the Steve API after the Kubernetes API refuses",
	"internal/engine/user/globalroles.go:checkGlobalRole":                    reasonAcrossTheCluster,
	"internal/engine/user/globalroles.go:listRoleTemplateObjects":            reasonAcrossTheCluster,
	"internal/engine/user/globalroles.go:resolveRoles":                       reasonAcrossTheCluster,
}

// namespaceListFiles are the files that may list the namespace kind through
// a variable assigned from kinds.Lookup("namespace") or kinds.ByKey("namespace"),
// each with why: the Steve namespace read, which lists through the Kubernetes
// API and reads the Steve API after a 403, and the callers that still list
// every namespace of the cluster and are refused for a caller who cannot.
var namespaceListFiles = map[string]string{
	"internal/engine/steve/projects.go":        "the Steve namespace read: the project's namespaces through the Kubernetes API, then the Steve API after a 403",
	"internal/engine/access/scope.go":          "access --all-namespaces lists every namespace of the cluster",
	"internal/engine/inspect/usage_project.go": "usage --by project lists every namespace of the cluster",
}

// TestEveryListAcrossAllNamespacesIsListed asserts that every function of a
// non-test file under internal/ that calls a method named List with the
// literal "" as its namespace argument is a key of listsAcrossAllNamespaces
// with a reason, that each key names a function that does, and that a List
// of the namespace kind (a variable assigned from kinds.Lookup("namespace") or
// kinds.ByKey("namespace"), then its GVR field, as the first argument after
// the context) is called only from the files of namespaceListFiles, and that
// each file of that table lists the namespace kind that way. It reads
// names only: a namespace passed in a variable, or a GVR built another way, is
// not seen. The generated get passes the namespace as a variable and has its
// own table test.
func TestEveryListAcrossAllNamespacesIsListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	found := map[string]bool{}
	listsNamespaces := map[string]bool{}
	var offenders []string
	for i, file := range tree.files {
		path := tree.paths[i]
		if !strings.HasPrefix(path, "internal/") {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			namespaceKinds := namespaceKindVariables(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "List" || len(call.Args) != 4 {
					return true
				}
				key := path + ":" + fn.Name.Name
				at := tree.fset.Position(call.Pos()).String()
				if lit, ok := call.Args[2].(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `""` {
					found[key] = true
					if reason := listsAcrossAllNamespaces[key]; reason == "" {
						offenders = append(offenders, at+": a list with no namespace in "+key+" is not in listsAcrossAllNamespaces")
					}
				}
				if gvr, ok := call.Args[1].(*ast.SelectorExpr); ok && gvr.Sel.Name == "GVR" {
					if id, ok := gvr.X.(*ast.Ident); ok && namespaceKinds[id.Name] {
						listsNamespaces[path] = true
						if _, allowed := namespaceListFiles[path]; !allowed {
							offenders = append(offenders, at+": a list of the namespace kind outside namespaceListFiles")
						}
					}
				}
				return true
			})
		}
	}
	assert.Empty(t, offenders)
	for key := range listsAcrossAllNamespaces {
		assert.True(t, found[key], "listsAcrossAllNamespaces lists %s, which sends no list with the literal \"\" as its namespace", key)
	}
	for path, reason := range namespaceListFiles {
		assert.NotEmpty(t, reason, path)
		assert.True(t, listsNamespaces[path], "namespaceListFiles lists %s, which does not list the namespace kind through a variable", path)
	}
}

// namespaceKindVariables returns the names of the variables fn assigns from
// kinds.Lookup("namespace") or kinds.ByKey("namespace").
func namespaceKindVariables(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Lookup" && sel.Sel.Name != "ByKey") {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "kinds" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"namespace"` {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}
