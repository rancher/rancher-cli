package main_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const modulePrefix = "github.com/rancher/rancher-cli/"

// TestArchitectureDocMatchesImports parses ARCHITECTURE.md's mermaid
// dependency graph and fails when it diverges from the real package-level
// import graph — missing edges, phantom edges, or undocumented packages.
// Dependency-documentation drift (finding L-5/UD-3) becomes a build failure.
func TestArchitectureDocMatchesImports(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	nodes, docEdges := parseMermaid(t, string(doc))
	realEdges := realImportEdges(t)

	for e := range docEdges {
		if !realEdges[e] {
			t.Errorf("doc claims edge %s — not present in code", e)
		}
	}
	for e := range realEdges {
		if !docEdges[e] {
			t.Errorf("code has edge %s — missing from ARCHITECTURE.md", e)
		}
	}
	for pkg := range packagesOnDisk(t) {
		if !nodes[pkg] {
			t.Errorf("package %s exists but is not in the diagram", pkg)
		}
	}
}

var (
	nodeRe = regexp.MustCompile(`(?m)^\s*(\w+)\["([^"]+)"\]`)
	edgeRe = regexp.MustCompile(`(?m)^\s*(\w+)\s*-->\s*(\w+)`)
)

// parseMermaid extracts node id→package mappings and edges from the FIRST
// mermaid block containing "graph TD".
func parseMermaid(t *testing.T, doc string) (map[string]bool, map[string]bool) {
	t.Helper()
	start := strings.Index(doc, "```mermaid")
	if start < 0 {
		t.Fatal("no mermaid block in ARCHITECTURE.md")
	}
	rest := doc[start:]
	end := strings.Index(rest[3:], "```")
	block := rest[:end+3]

	ids := map[string]string{}
	nodes := map[string]bool{}
	for _, m := range nodeRe.FindAllStringSubmatch(block, -1) {
		ids[m[1]] = m[2]
		nodes[m[2]] = true
	}
	edges := map[string]bool{}
	for _, m := range edgeRe.FindAllStringSubmatch(block, -1) {
		from, okF := ids[m[1]]
		to, okT := ids[m[2]]
		if !okF || !okT {
			t.Fatalf("edge %s --> %s references an undeclared node id", m[1], m[2])
		}
		edges[from+" -> "+to] = true
	}
	return nodes, edges
}

// packagesOnDisk returns every package dir under internal/ + surface/ + main.
func packagesOnDisk(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{"main": true}
	for _, root := range []string{"internal", "surface"} {
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if hasGoFiles(path) {
				out[path] = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func hasGoFiles(dir string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			return true
		}
	}
	return false
}

// realImportEdges parses non-test sources and returns package-level edges
// between module-internal packages, using doc node naming ("main" for the
// root package).
func realImportEdges(t *testing.T) map[string]bool {
	t.Helper()
	edges := map[string]bool{}
	fset := token.NewFileSet()
	for pkg := range packagesOnDisk(t) {
		dir := pkg
		if pkg == "main" {
			dir = "."
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if strings.HasPrefix(p, modulePrefix) {
					edges[pkg+" -> "+strings.TrimPrefix(p, modulePrefix)] = true
				}
			}
		}
	}
	return edges
}
