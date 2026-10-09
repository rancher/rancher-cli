package conformance

import (
	"go/ast"
	gotoken "go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logoutPathFiles are the only non-test files that may name the logout
// endpoint's path: the revoke primitive, whose local check keeps the
// request to the bearer's own token, and the test server, which cannot
// import it. Logout deletes the token its bearer names, so no other code
// may send it.
var logoutPathFiles = []string{
	"internal/token/revoke.go",
	"internal/testrancher/testrancher.go",
}

// logoutConstantPrefixes are the paths whose non-test Go files may
// reference the LogoutPath constant: the revoke primitive, and the
// fidelity probes, which record the endpoint's answers with a captured
// bearer and no identity.
var logoutConstantPrefixes = []string{
	"internal/token/revoke.go",
	"internal/testrancher/fidelity/",
}

// allSessionsQuery matches a query key that asks for logout of every
// session; a longer key such as allowWatchBookmarks does not match.
var allSessionsQuery = regexp.MustCompile(`[?&]all(=|&|$)`)

// TestLogoutPathIsNamedOnlyWhereItsBearerIsChecked asserts no non-test Go
// file outside logoutPathFiles holds the logout path in a string literal,
// no file outside logoutConstantPrefixes references the LogoutPath
// constant, and none holds a query that asks for logout of every session.
func TestLogoutPathIsNamedOnlyWhereItsBearerIsChecked(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	for i, file := range tree.files {
		path := tree.paths[i]
		constantAllowed := slices.ContainsFunc(logoutConstantPrefixes, func(prefix string) bool { return strings.HasPrefix(path, prefix) })
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name == "LogoutPath" && !constantAllowed {
					offenders = append(offenders, tree.fset.Position(v.Pos()).String()+": the LogoutPath constant")
				}
				return true
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				value = lit.Value
			}
			if strings.Contains(value, "/v1/logout") && !slices.Contains(logoutPathFiles, path) {
				offenders = append(offenders, tree.fset.Position(lit.Pos()).String()+": the logout path")
			}
			if allSessionsQuery.MatchString(value) {
				offenders = append(offenders, tree.fset.Position(lit.Pos()).String()+": an all-sessions query")
			}
			return true
		})
	}
	assert.Empty(t, offenders, "only %v may name the logout path, only %v may reference its constant, and none may ask for every session", logoutPathFiles, logoutConstantPrefixes)
}

// TestNoSourceFileNamesTheLogoutAllAction asserts no non-test Go file holds
// logoutAll, the action that logs the whole account out at the identity
// provider. The match is case-sensitive: a name such as DeleteForLogoutAll
// is another thing.
func TestNoSourceFileNamesTheLogoutAllAction(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) {
			continue
		}
		data, rerr := os.ReadFile(path)
		require.NoError(t, rerr, path)
		if strings.Contains(string(data), "logoutAll") {
			offenders = append(offenders, path)
		}
	}
	assert.Empty(t, offenders)
}

// TestAllSessionsQueryMatchesOnlyTheWholeKey asserts the all-sessions query
// check matches the key all and no longer key that starts with it.
func TestAllSessionsQueryMatchesOnlyTheWholeKey(t *testing.T) {
	t.Parallel()
	for _, hit := range []string{"/v1/logout?all", "/x?all=true", "/x?a=1&all", "/x?a=1&all=1&b=2"} {
		assert.True(t, allSessionsQuery.MatchString(hit), hit)
	}
	for _, miss := range []string{"/x?allowWatchBookmarks=true", "/x?a=1&allow=1", "/x?small=1", "all"} {
		assert.False(t, allSessionsQuery.MatchString(miss), miss)
	}
}
