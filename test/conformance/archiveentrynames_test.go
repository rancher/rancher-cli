package conformance

import (
	"go/ast"
	"go/token"
	"path"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archiveAddAllowlist names the files of internal/engine/observe whose
// ".Add(" calls on a receiver other than the archive writer are fine, each
// with the reason. It is empty: the package has no other Add call.
var archiveAddAllowlist = map[string]string{}

// TestArchiveEntryNamesComeOnlyFromEntryPath asserts that every entry name
// of the support-bundle archive is built by entryPath or staticEntryPath and
// written by ArchiveWriter.Add, and that no other code builds a name or
// writes a tar header. Over every production Go file of the repository:
//
//  1. only internal/engine/observe/archive.go imports archive/tar or
//     archive/zip;
//  2. in internal/engine/observe, the identifiers entryName (the name type)
//     and entryText (its field) appear only in entryname.go and archive.go,
//     and a composite literal of the name type appears only in
//     entryname.go;
//  3. every call of staticEntryPath has one argument, a string literal;
//  4. no file of internal/engine/observe imports unsafe;
//  5. in internal/engine/observe, every call bw.Add(arg) takes one
//     ArchiveEntry literal whose Path is an identifier or a call of
//     entryPath or staticEntryPath, and every other ".Add(" call is in
//     archiveAddAllowlist.
//
// Because the name type is a struct whose type and field only two files can
// name, an identifier in Path can hold only a value a builder returned (or
// the zero value, which Add refuses); a string, a constant or a "+"
// expression does not compile. The scan reads identifiers, imports and call
// shapes only: it cannot see a name built in a file outside the
// repository's git view or in a file that is not Go source, and it does not
// follow values through reflect.
func TestArchiveEntryNamesComeOnlyFromEntryPath(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	const (
		observeDir    = "internal/engine/observe"
		archiveFile   = observeDir + "/archive.go"
		entryNameFile = observeDir + "/entryname.go"
		bundleFile    = observeDir + "/bundle.go"
	)

	var offenders []string
	report := func(pos token.Pos, msg string) {
		offenders = append(offenders, tree.fset.Position(pos).String()+": "+msg)
	}
	seenFiles := map[string]bool{}
	var entryPathCalls, staticEntryPathCalls, writerAddCalls int

	for i, file := range tree.files {
		filePath := tree.paths[i]
		seenFiles[filePath] = true
		inObserve := path.Dir(filePath) == observeDir

		for _, imp := range file.Imports {
			imported, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err, filePath)
			if (imported == "archive/tar" || imported == "archive/zip") && filePath != archiveFile {
				report(imp.Pos(), "imports "+imported+"; only archive.go writes an archive")
			}
			if imported == "unsafe" && inObserve {
				report(imp.Pos(), "imports unsafe; the entry name type's field must not be written around the name functions")
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if id, ok := v.Type.(*ast.Ident); ok && id.Name == "entryName" && filePath != entryNameFile {
					report(v.Pos(), "builds an entryName; only entryPath and staticEntryPath may")
				}
			case *ast.Ident:
				if inObserve && filePath != entryNameFile && filePath != archiveFile {
					switch v.Name {
					case "entryName":
						report(v.Pos(), "names the entryName type; only entryname.go and archive.go may")
					case "entryText":
						report(v.Pos(), "names the entryName field; only entryname.go and archive.go may")
					}
				}
			case *ast.CallExpr:
				if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "staticEntryPath" {
					if filePath == bundleFile {
						staticEntryPathCalls++
					}
					if len(v.Args) != 1 {
						report(v.Pos(), "staticEntryPath takes one string literal")
						break
					}
					if lit, ok := v.Args[0].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
						report(v.Pos(), "staticEntryPath is called with something other than a string literal")
					}
				}
				if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "entryPath" && filePath == bundleFile {
					entryPathCalls++
				}
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Add" || !inObserve {
					break
				}
				recv, isIdent := sel.X.(*ast.Ident)
				if !isIdent || recv.Name != "bw" {
					if _, allowed := archiveAddAllowlist[filePath]; !allowed {
						report(v.Pos(), ".Add call on a receiver other than the archive writer bw; name the file in archiveAddAllowlist with a reason")
					}
					break
				}
				writerAddCalls++
				if !addTakesBuilderName(v) {
					report(v.Pos(), "bw.Add must take one ArchiveEntry literal whose Path is an identifier or a call of entryPath or staticEntryPath")
				}
			}
			return true
		})
	}

	assert.Empty(t, offenders, "archive entry names are built only by entryPath and staticEntryPath")

	for _, f := range []string{archiveFile, entryNameFile, bundleFile} {
		assert.True(t, seenFiles[f], "the scan did not see %s", f)
	}
	assert.Positive(t, entryPathCalls, "the scan saw no entryPath call in bundle.go")
	assert.Positive(t, staticEntryPathCalls, "the scan saw no staticEntryPath call in bundle.go")
	assert.Positive(t, writerAddCalls, "the scan saw no bw.Add call in the observe package")
}

// addTakesBuilderName reports whether call, a bw.Add call, has exactly one
// argument that is an ArchiveEntry literal with a Path of an identifier or a
// call of entryPath or staticEntryPath.
func addTakesBuilderName(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	lit, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return false
	}
	if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "ArchiveEntry" {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Path" {
			continue
		}
		switch val := kv.Value.(type) {
		case *ast.Ident:
			return true
		case *ast.CallExpr:
			fn, ok := val.Fun.(*ast.Ident)
			return ok && (fn.Name == "entryPath" || fn.Name == "staticEntryPath")
		}
		return false
	}
	return false
}
