package main_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repositoryFiles lists the repository's files as git sees them: every
// tracked file plus every untracked file no ignore rule covers, as
// slash-separated paths relative to the module root. A checkout carries
// local files git ignores (working notes under docs/, a generated
// knowledge graph, editor and tool state, copied source trees); the
// source scans below must never see them, and a hand-kept list of
// directory names to skip cannot keep up with what a machine accumulates.
// A tracked file deleted from the working tree is dropped from the list.
var repositoryFiles = sync.OnceValues(func() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, errors.New("git ls-files: " + strings.TrimSpace(string(exit.Stderr)) + " (the source scans need a git checkout)")
		}
		return nil, err
	}
	var files []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		path := string(p)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		files = append(files, path)
	}
	return files, nil
})

// inTestdata reports whether path has a testdata directory on it. testdata
// trees ship deliberately contrived helper binaries, not pipeline-owned
// production code, so the production-source scans leave them out.
func inTestdata(path string) bool {
	return slices.Contains(strings.Split(path, "/"), "testdata")
}

// TestRepositoryFilesFollowGit asserts the list is git's view of the tree:
// tracked files are present, ignored and excluded ones are not, and every
// path is relative and slash-separated.
func TestRepositoryFilesFollowGit(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	assert.Contains(t, files, "go.mod")
	assert.Contains(t, files, "sourcewalk_test.go")
	for _, path := range files {
		assert.False(t, strings.HasPrefix(path, "docs/"), "%s is under the ignored docs/", path)
		assert.False(t, strings.HasPrefix(path, "graphify-out/"), "%s is under the ignored graphify-out/", path)
		assert.False(t, strings.HasPrefix(path, "bin/"), "%s is under the ignored bin/", path)
		assert.False(t, strings.HasPrefix(path, ".git/"), "%s is VCS metadata", path)
		assert.False(t, strings.HasPrefix(path, "/") || strings.Contains(path, `\`), "%s is not a relative slash path", path)
	}
}

// parsedSourceTree is the shared parse result every root-level AST-walk
// tripwire needs: every non-test .go file in the repository outside
// testdata, parsed once against one *token.FileSet. files and paths are
// parallel slices.
type parsedSourceTree struct {
	fset  *token.FileSet
	files []*ast.File
	paths []string
	err   error
}

// parseSourceTree parses the production source exactly once per test
// binary run, however many tripwires call it.
var parseSourceTree = sync.OnceValue(func() parsedSourceTree {
	all, err := repositoryFiles()
	if err != nil {
		return parsedSourceTree{err: err}
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
	for _, path := range all {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if perr != nil {
			return parsedSourceTree{err: perr}
		}
		files = append(files, f)
		paths = append(paths, path)
	}
	return parsedSourceTree{fset: fset, files: files, paths: paths}
})
