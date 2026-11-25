package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// resourceFieldName is a string literal that names the requests, the
	// limits or the overhead of a pod in an unstructured object.
	resourceFieldName = "resource field name"
	// resourceFieldSelector is a selector that reads the requests or limits
	// of a container, the overhead of a pod spec, or the containers or init
	// containers of a pod spec.
	resourceFieldSelector = "resource field selector"
	// podResourceHelperImport is an import of the Kubernetes package that
	// computes the resources of a pod.
	podResourceHelperImport = "pod resource helper import"
	// podResourceHelperCall is a call of PodRequests or PodLimits of that
	// package.
	podResourceHelperCall = "pod resource helper call"
)

// podResourceHelperPackage is the package that has Kubernetes' rule for the
// requests and limits of a pod.
const podResourceHelperPackage = "k8s.io/component-helpers/resource"

// podResourceReaders are the places in non-test Go files that may read the
// requests, limits, overhead or containers of a pod spec, import the pod
// resource helper, or call its PodRequests or PodLimits, by file, enclosing
// function and pattern, each with the reason. The requests and limits of a pod
// come from one function, sumPodResources, which hands each pod to the
// Kubernetes helper; no other code adds up the resources of a pod's containers
// by hand. internal/testrancher (the test servers and the fidelity harness) and
// testdata trees are not scanned.
var podResourceReaders = []struct {
	file, function, pattern, reason string
}{
	{"internal/engine/inspect/usage.go", "", podResourceHelperImport, "the file of sumPodResources, the one function that computes the resources of a pod"},
	{"internal/engine/inspect/usage.go", "sumPodResources", podResourceHelperCall, "the one function that computes the resources of a pod, with the options of the pod rows of kubectl describe node"},
	{"internal/engine/kinds/kinds.go", "", resourceFieldName, `the alias "limits" of the limitrange kind, which names a kind and not a field`},
}

// podResourceHit is one place a file reads a field of the resources of a pod
// or imports the helper.
type podResourceHit struct {
	function, pattern, at string
}

// findPodResourceReads parses the Go source of path (read from the file when
// src is nil) and returns every string literal "requests", "limits" or
// "overhead"; every selector Resources.Requests, Resources.Limits,
// Spec.Overhead, Spec.Containers or Spec.InitContainers; an import of the pod
// resource helper; and a call of its PodRequests or PodLimits through the name
// the file imports it by, each with the function declaration it is in ("" at
// the package level).
func findPodResourceReads(path string, src any) ([]podResourceHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []podResourceHit
	helperName := ""
	for _, spec := range file.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == podResourceHelperPackage {
			hits = append(hits, podResourceHit{"", podResourceHelperImport, fset.Position(spec.Pos()).String()})
			helperName = podResourceHelperPackage[strings.LastIndex(podResourceHelperPackage, "/")+1:]
			if spec.Name != nil {
				helperName = spec.Name.Name
			}
		}
	}
	visit := func(function string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING && slices.Contains([]string{`"requests"`, `"limits"`, `"overhead"`}, n.Value) {
					hits = append(hits, podResourceHit{function, resourceFieldName, fset.Position(n.Pos()).String()})
				}
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && id.Name == helperName && slices.Contains([]string{"PodRequests", "PodLimits"}, n.Sel.Name) {
					hits = append(hits, podResourceHit{function, podResourceHelperCall, fset.Position(n.Pos()).String()})
					return true
				}
				inner, ok := n.X.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				field := inner.Sel.Name + "." + n.Sel.Name
				if slices.Contains([]string{"Resources.Requests", "Resources.Limits", "Spec.Overhead", "Spec.Containers", "Spec.InitContainers"}, field) {
					hits = append(hits, podResourceHit{function, resourceFieldSelector, fset.Position(n.Pos()).String()})
				}
			}
			return true
		}
	}
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		ast.Inspect(decl, visit(function))
	}
	return hits, nil
}

// TestEveryPodResourceSumIsInSumPodResources asserts a non-test Go file outside
// internal/testrancher and testdata reads the requests, limits, overhead or
// containers of a pod spec, imports the Kubernetes pod resource helper, or
// calls its PodRequests or PodLimits, only in a place podResourceReaders lists
// with its reason, and that every listed place still does. The test refuses a
// second sum of container resources written by hand elsewhere.
func TestEveryPodResourceSumIsInSumPodResources(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) || strings.HasPrefix(path, "internal/testrancher/") {
			continue
		}
		hits, perr := findPodResourceReads(path, nil)
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function + " " + hit.pattern
			seen[key] = true
			listed := slices.ContainsFunc(podResourceReaders, func(l struct{ file, function, pattern, reason string }) bool {
				return l.file+" "+l.function+" "+l.pattern == key
			})
			if !listed {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q", hit.at, hit.pattern, hit.function))
			}
		}
	}
	assert.Empty(t, offenders, "the requests and limits of a pod are computed only by sumPodResources, or the place is listed with its reason")
	for _, l := range podResourceReaders {
		assert.True(t, seen[l.file+" "+l.function+" "+l.pattern], "%s %s is listed for %s and no longer has it; remove the entry", l.file, l.function, l.pattern)
	}
}

// TestThePodResourceScanFindsEveryPattern asserts the rule of
// findPodResourceReads on small sources: each literal, each selector and the
// import is found, in a function or at the package level, and an unrelated
// literal, selector or call of another package is not.
func TestThePodResourceScanFindsEveryPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"requests literal", "package p\n\nfunc f() string { return \"requests\" }\n", []string{"f " + resourceFieldName}},
		{"limits literal at the package level", "package p\n\nvar l = \"limits\"\n", []string{" " + resourceFieldName}},
		{"overhead literal", "package p\n\nfunc f() string { return \"overhead\" }\n", []string{"f " + resourceFieldName}},
		{"container requests", "package p\n\nfunc f(c C) { _ = c.Resources.Requests }\n", []string{"f " + resourceFieldSelector}},
		{"container limits", "package p\n\nfunc f(c C) { _ = c.Resources.Limits }\n", []string{"f " + resourceFieldSelector}},
		{"spec overhead", "package p\n\nfunc f(p P) { _ = p.Spec.Overhead }\n", []string{"f " + resourceFieldSelector}},
		{"spec containers", "package p\n\nfunc f(p P) { _ = p.Spec.Containers }\n", []string{"f " + resourceFieldSelector}},
		{"spec init containers", "package p\n\nfunc f(p P) { _ = p.Spec.InitContainers }\n", []string{"f " + resourceFieldSelector}},
		{"helper import", "package p\n\nimport \"k8s.io/component-helpers/resource\"\n", []string{" " + podResourceHelperImport}},
		{"helper PodRequests call", "package p\n\nimport \"k8s.io/component-helpers/resource\"\n\nfunc f(p P) { _ = resource.PodRequests(p, resource.PodResourcesOptions{}) }\n", []string{" " + podResourceHelperImport, "f " + podResourceHelperCall}},
		{"helper PodLimits call through an alias", "package p\n\nimport h \"k8s.io/component-helpers/resource\"\n\nfunc f(p P) { _ = h.PodLimits(p, h.PodResourcesOptions{}) }\n", []string{" " + podResourceHelperImport, "f " + podResourceHelperCall}},
		{"helper call at the package level", "package p\n\nimport \"k8s.io/component-helpers/resource\"\n\nvar _ = resource.PodRequests\n", []string{" " + podResourceHelperImport, " " + podResourceHelperCall}},
		{"PodRequests of another package", "package p\n\nfunc f(p P) { _ = other.PodRequests(p) }\n", nil},
		{"unrelated", "package p\n\nfunc f(p P) { _ = p.Status.Containers; _ = \"request\"; _ = p.Resources.Claims }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findPodResourceReads("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, h.function+" "+h.pattern)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
