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
// object and return it as Steve returns it (the readers of a cluster, a
// project, a cluster's namespaces, and a collection or one object of any one
// Steve type), and the conversion of what they return.
var steveObjectReaders = []string{"ClusterObjects", "ProjectObjects", "GetProject", "GetClusterObject", "TypeObjects", "GetTypeObject", "NamespaceObjects", "GetNamespaceObject", "KubernetesObject"}

// steveFirstReader is the entry of a function that reads through Steve with no
// Kubernetes read before it: the files that may call it, each named in full,
// and one reason that names each command that calls it.
type steveFirstReader struct {
	files  []string
	reason string
}

// steveFirstReaders maps each function of package resources that reads an
// object through Steve with no Kubernetes read before it to the files that may
// call it, and the reason.
var steveFirstReaders = map[string]steveFirstReader{
	"ManagementClusterThroughSteve": {
		files: []string{
			"internal/engine/provisioning/upgrade.go",
			"internal/engine/provisioning/day2ops.go",
			"internal/engine/provisioning/importedday2.go",
		},
		reason: "cluster upgrade and cluster set-upgrade-strategy of an imported cluster, and cluster save-etcd-snapshot and cluster restore-etcd-snapshot to refuse an imported cluster and to check that the cluster is ready, and create resource of an etcd snapshot, restore, encryption key rotation or certificate rotation object of an imported cluster to check the cluster before the dry-run: a Cluster Owner can read the management Cluster by name only through Steve, and the update of cluster upgrade and of cluster set-upgrade-strategy sends the Kubernetes shape of it",
	},
}

// steveFirstReaderOutside reports whether name is a function of
// steveFirstReaders and path is not one of its files.
func steveFirstReaderOutside(name, path string) bool {
	reader, listed := steveFirstReaders[name]
	return listed && !slices.Contains(reader.files, path)
}

// TestSteveObjectReadersOnlyFromTheGetPath asserts that, in every non-test Go
// file under internal/, steveList, steveGet, newSteveWaitClient and
// wrapSteveWait are called only from the files of steveGetPathFiles, that the
// readers that return an object as Steve returns it, and KubernetesObject,
// are called only from steveread.go (the steve package itself declares them and
// its tests call them), and that each function of steveFirstReaders is called
// only from the files of its entry, each of which calls it. Only the generated
// get, EtcdSnapshotsAfterForbidden, ManagementClustersAfterForbidden and
// ProvisioningClustersAfterForbidden (steveread.go) may read a kind through
// Steve after the Kubernetes API refused it; the functions of steveFirstReaders
// are the reviewed exceptions that read through Steve first.
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
				case steveFirstReaderOutside(name, path):
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
				case steveFirstReaderOutside(id.Name, path):
					offenders = append(offenders, at)
				}
			}
			return true
		})
	}
	assert.Empty(t, offenders, "%v are called only from %v; %v only from %s; the functions of steveFirstReaders only from the files of their entry", steveGetPathReaders, steveGetPathFiles, steveObjectReaders, steveObjectReaderFile)
	for name, reader := range steveFirstReaders {
		assert.NotEmpty(t, reader.reason, name)
		assert.NotEmpty(t, reader.files, name)
		for _, file := range reader.files {
			assert.True(t, calledFrom(tree, name, file), "steveFirstReaders lists %s for %s, which does not call it", name, file)
		}
	}
}

// TestSteveReadersOfTheResourcesFileReadAfterARefusalOrAreListed asserts that
// each function of steveread.go that is not a method of getCmd and calls a
// reader of steveObjectReaders (the conversion KubernetesObject aside) either
// takes a parameter named k8sErr, the Kubernetes error that sends it to Steve,
// or is a key of steveFirstReaders.
func TestSteveReadersOfTheResourcesFileReadAfterARefusalOrAreListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	checked := 0
	for i, file := range tree.files {
		if tree.paths[i] != steveObjectReaderFile {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isGetCmdMethod(fn) {
				continue
			}
			reads := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name != "KubernetesObject" && slices.Contains(steveObjectReaders, sel.Sel.Name) {
					reads = true
				}
				return !reads
			})
			if !reads {
				continue
			}
			checked++
			_, listed := steveFirstReaders[fn.Name.Name]
			if !listed && !hasParameter(fn, "k8sErr") {
				offenders = append(offenders, tree.fset.Position(fn.Pos()).String()+": "+fn.Name.Name)
			}
		}
	}
	assert.Positive(t, checked, "the scan found no function of %s that reads through Steve", steveObjectReaderFile)
	assert.Empty(t, offenders, "a function of %s outside getCmd that reads through Steve takes the k8sErr that sent it there or is a key of steveFirstReaders", steveObjectReaderFile)
}

// isGetCmdMethod reports whether fn is a method of getCmd.
func isGetCmdMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	id, ok := fn.Recv.List[0].Type.(*ast.Ident)
	return ok && id.Name == "getCmd"
}

// hasParameter reports whether fn declares a parameter named name.
func hasParameter(fn *ast.FuncDecl, name string) bool {
	for _, field := range fn.Type.Params.List {
		for _, id := range field.Names {
			if id.Name == name {
				return true
			}
		}
	}
	return false
}

// calledFrom reports whether the parsed file at path calls the function name,
// through a package or by its bare name.
func calledFrom(tree parsedSourceTree, name, path string) bool {
	for i, file := range tree.files {
		if tree.paths[i] != path {
			continue
		}
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return !found
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				found = found || fn.Sel.Name == name
			case *ast.Ident:
				found = found || fn.Name == name
			}
			return !found
		})
		return found
	}
	return false
}
