package conformance

import (
	"go/ast"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steveGetPathFiles are the only files that call steveList, steveGet,
// newSteveWaitClient and wrapSteveWait: the generated get and the file that
// holds the Steve read.
var steveGetPathFiles = []string{
	"internal/engine/resources/get.go",
	"internal/engine/resources/steveread.go",
}

// steveGetPathReaders are the methods and functions of package resources that
// read Steve for a get.
var steveGetPathReaders = []string{"steveList", "steveGet", "newSteveWaitClient", "wrapSteveWait"}

// steveObjectReaderFile is the only file outside the steve package that calls
// the Steve readers that return an object as Steve returns it, and the
// conversion to the Kubernetes shape.
const steveObjectReaderFile = "internal/engine/resources/steveread.go"

// steveObjectReaders are the functions of internal/engine/steve that read an
// object and return it as Steve returns it, and the conversion of what they
// return.
var steveObjectReaders = []string{"ClusterObjects", "ProjectObjects", "GetProject", "GetClusterObject", "KubernetesObject"}

// TestSteveObjectReadersOnlyFromTheGetPath asserts that, in every non-test Go
// file under internal/, steveList, steveGet, newSteveWaitClient and
// wrapSteveWait are called only from the files of steveGetPathFiles, and that
// the readers that return an object as Steve returns it, and KubernetesObject,
// are called only from steveread.go (the steve package itself declares them and
// its tests call them). Only the generated get may read a kind through Steve
// after the Kubernetes API refused it.
func TestSteveObjectReadersOnlyFromTheGetPath(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	for i, file := range tree.files {
		path := tree.paths[i]
		if !strings.HasPrefix(path, "internal/") {
			continue
		}
		inSteve := strings.HasPrefix(path, "internal/engine/steve/")
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				name := v.Sel.Name
				at := tree.fset.Position(v.Pos()).String() + ": " + name
				switch {
				case slices.Contains(steveGetPathReaders, name) && !slices.Contains(steveGetPathFiles, path):
					offenders = append(offenders, at)
				case slices.Contains(steveObjectReaders, name) && path != steveObjectReaderFile:
					offenders = append(offenders, at)
				}
			case *ast.CallExpr:
				// A call in the same package has no package qualifier.
				id, ok := v.Fun.(*ast.Ident)
				if !ok {
					break
				}
				at := tree.fset.Position(v.Pos()).String() + ": " + id.Name
				switch {
				case inSteve && slices.Contains(steveObjectReaders, id.Name):
					offenders = append(offenders, at)
				case slices.Contains(steveGetPathReaders, id.Name) && !slices.Contains(steveGetPathFiles, path):
					offenders = append(offenders, at)
				}
			}
			return true
		})
	}
	assert.Empty(t, offenders, "%v are called only from %v; %v only from %s", steveGetPathReaders, steveGetPathFiles, steveObjectReaders, steveObjectReaderFile)
}
