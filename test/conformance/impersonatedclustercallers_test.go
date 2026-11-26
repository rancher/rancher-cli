package conformance

import (
	"go/ast"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// impersonatedClusterCallers are the only non-test files that call
// ImpersonatedCluster, which returns a cluster client that sends every request
// as another user, each with why. The scan asserts which files call it; it
// does not follow the client those files pass on.
var impersonatedClusterCallers = map[string]string{
	"internal/engine/access/clusters.go": "access clusters --as reads each cluster as the named user",
	"internal/engine/access/projects.go": "access projects --as reads each project as the named user",
	"internal/engine/access/identity.go": "the access commands that take --as ask the server who the named user is and what that user may do, as that user",
}

// TestImpersonatedClusterIsCalledOnlyFromTheFilesThatActAsAnotherUser
// asserts that, in every non-test Go file under internal/, ImpersonatedCluster
// is called only from the files of impersonatedClusterCallers, and that each
// of those files calls it.
func TestImpersonatedClusterIsCalledOnlyFromTheFilesThatActAsAnotherUser(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	called := map[string]bool{}
	for i, file := range tree.files {
		path := tree.paths[i]
		if !strings.HasPrefix(path, "internal/") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ImpersonatedCluster" {
				called[path] = true
				if _, allowed := impersonatedClusterCallers[path]; !allowed {
					offenders = append(offenders, tree.fset.Position(call.Pos()).String())
				}
			}
			return true
		})
	}
	assert.Empty(t, offenders, "ImpersonatedCluster is called only from the files of impersonatedClusterCallers")
	for path, reason := range impersonatedClusterCallers {
		assert.NotEmpty(t, reason, path)
		assert.True(t, called[path], "impersonatedClusterCallers lists %s, which does not call ImpersonatedCluster", path)
	}
}
