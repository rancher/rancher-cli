package main_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
)

// parsedSourceTree is the shared parse result every root-level AST-walk
// tripwire needs: every non-test .go file under the module root, parsed once
// against one *token.FileSet. files and paths are parallel slices.
type parsedSourceTree struct {
	fset  *token.FileSet
	files []*ast.File
	paths []string
	err   error
}

// parseSourceTree walks and parses the source tree exactly once per test
// binary run, however many tripwires call it.
var parseSourceTree = sync.OnceValue(func() parsedSourceTree {
	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if path != "." && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if perr != nil {
			return perr
		}
		files = append(files, f)
		paths = append(paths, filepath.ToSlash(path))
		return nil
	})
	return parsedSourceTree{fset: fset, files: files, paths: paths, err: err}
})
