package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roleTemplateFieldReaders are the functions of internal/engine/kinds that
// read one field of a RoleTemplate for a role flag. The list of role
// templates a flag offers for a new binding is built only by
// kinds.RoleTemplateOffered. A field added to the rule reaches every such
// flag. Each function maps the files outside internal/engine/kinds that may
// call it to the reason; a function no file may call has an empty map.
var roleTemplateFieldReaders = map[string]map[string]string{
	"RoleTemplateLocked": {},
	"RoleTemplateHidden": {
		"internal/engine/rbac/grant.go": "grant warns when the role template it was asked to bind is hidden; the candidates it lists come from RoleTemplateOffered",
	},
	"RoleTemplateInContext": {
		"internal/engine/rbac/grant.go":          "grant refuses a role template whose context is not the scope's; the candidates it lists come from RoleTemplateOffered",
		"internal/engine/rbac/complete/roles.go": "the completion of revoke --role offers every role template of the scope's context, locked and hidden ones included",
	},
}

// TestRoleTemplateFiltersCallTheOfferedPredicate asserts no non-test Go file
// outside internal/engine/kinds calls a function that reads one field of a
// RoleTemplate (RoleTemplateLocked, RoleTemplateHidden, RoleTemplateInContext)
// except from a file the table above names for it, with its reason, and that
// every file the table names calls the function it is named for: a row for a
// call that no longer exists is removed. The NOT_FOUND candidates of grant
// and the completion of grant --role call RoleTemplateOffered; a new flag
// that offers role templates calls it too.
func TestRoleTemplateFiltersCallTheOfferedPredicate(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	var offenders []string
	called := map[string]map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "internal/engine/kinds/") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if allowed, listed := roleTemplateFieldReaders[sel.Sel.Name]; listed {
				if _, ok := allowed[path]; !ok {
					offenders = append(offenders, fset.Position(sel.Pos()).String()+": "+sel.Sel.Name)
				}
				if called[sel.Sel.Name] == nil {
					called[sel.Sel.Name] = map[string]bool{}
				}
				called[sel.Sel.Name][path] = true
			}
			return true
		})
	}
	assert.Empty(t, offenders, "a call of RoleTemplateLocked, RoleTemplateHidden or RoleTemplateInContext outside the allowlist: a role flag filters role templates with kinds.RoleTemplateOffered, or the call is listed in roleTemplateFieldReaders with its reason")
	var unused []string
	for name, allowed := range roleTemplateFieldReaders {
		for path, reason := range allowed {
			assert.NotEmpty(t, reason, name+" in "+path)
			if !called[name][path] {
				unused = append(unused, path+": "+name)
			}
		}
	}
	assert.Empty(t, unused, "a row of roleTemplateFieldReaders names a file that does not call the function: remove the row")
}
