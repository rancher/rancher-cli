package fidelity

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catalogWriteFiles are the only non-test files that may send the install or
// uninstall action: they hold the catalog write probes and the setup and
// teardown steps those probes share.
var catalogWriteFiles = []string{"catalogwrite_probes.go", "catalogcleanup.go"}

// isCatalogWriteProbe reports whether one of p's observations sends an
// action to a ClusterRepo or an App of the catalog API, the requests that
// install or uninstall a chart.
func isCatalogWriteProbe(p Probe) bool {
	for _, o := range p.Observations {
		path := o.Request.Path
		if !strings.Contains(path, "catalog.cattle.io.clusterrepos") && !strings.Contains(path, "catalog.cattle.io.apps") {
			continue
		}
		if o.Request.Query.Has("action") || strings.Contains(path, "action=") {
			return true
		}
	}
	return false
}

// stepNames returns the names of steps.
func stepNames(steps []Step) []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names
}

// catalogSetupProblem returns what is wrong with the leading setup steps of
// a catalog write probe, or "" when they are the cleanup's setup steps in
// order: the check for leftovers, one or more waits for the chart in the
// index, then the request id, the cluster values, the snapshot and the wait
// budget.
func catalogSetupProblem(names []string) string {
	if len(names) == 0 || names[0] != stepPrecheck {
		return "the first setup step is not " + strconv.Quote(stepPrecheck)
	}
	i := 1
	for i < len(names) && strings.HasPrefix(names[i], stepIndex+" (") {
		i++
	}
	if i == 1 {
		return "no setup step waits for the chart in the repository index"
	}
	rest := []string{stepRequestID, stepValues, stepSnapshot, stepDeadline}
	if len(names) < i+len(rest) || !slices.Equal(names[i:i+len(rest)], rest) {
		return "the setup steps after the index waits are not " + strings.Join(rest, ", ")
	}
	return ""
}

// TestEveryCatalogWriteProbeRunsTheCleanupOnlyOnItsOwnPair asserts each
// probe that installs or uninstalls a chart starts with the cleanup's setup
// steps, ends with exactly the cleanup's teardown steps and runs only on a
// pair the harness booted or kept, and that at least two such probes exist.
func TestEveryCatalogWriteProbeRunsTheCleanupOnlyOnItsOwnPair(t *testing.T) {
	t.Parallel()
	wantTeardown := []string{stepUninstall, stepRecord, stepRemove, stepGone, stepCompare}
	count := 0
	for _, p := range All() {
		if !isCatalogWriteProbe(p) {
			continue
		}
		count++
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, catalogSetupProblem(stepNames(p.Setup)), "setup")
			assert.Equal(t, wantTeardown, stepNames(p.Teardown), "teardown")
			assert.True(t, p.OwnPairOnly, "a probe that installs a chart must set OwnPairOnly")
		})
	}
	assert.GreaterOrEqual(t, count, 2, "the probes that install a chart were not found")
}

// TestCatalogWriteProbeSetupProblem asserts catalogSetupProblem accepts the
// cleanup's own setup steps and reports a missing or reordered step.
func TestCatalogWriteProbeSetupProblem(t *testing.T) {
	t.Parallel()
	setup, _ := newCatalogCleanup("local", "installop", "uninstallop").steps()
	full := stepNames(setup)
	index := stepIndex + " (1 of 1)"
	for _, tc := range []struct {
		name  string
		names []string
		ok    bool
	}{
		{"the cleanup's own steps", full, true},
		{"one index wait", []string{stepPrecheck, index, stepRequestID, stepValues, stepSnapshot, stepDeadline}, true},
		{"a step after the cleanup's", append(slices.Clone(full), "another step"), true},
		{"no steps", nil, false},
		{"no check for leftovers", []string{index, stepRequestID, stepValues, stepSnapshot, stepDeadline}, false},
		{"no index wait", []string{stepPrecheck, stepRequestID, stepValues, stepSnapshot, stepDeadline}, false},
		{"no wait budget", []string{stepPrecheck, index, stepRequestID, stepValues, stepSnapshot}, false},
		{"values before the request id", []string{stepPrecheck, index, stepValues, stepRequestID, stepSnapshot, stepDeadline}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problem := catalogSetupProblem(tc.names)
			if tc.ok {
				assert.Empty(t, problem)
			} else {
				assert.NotEmpty(t, problem)
			}
		})
	}
}

// isInstallOrUninstall reports whether e is the string literal "install" or
// "uninstall".
func isInstallOrUninstall(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && (v == "install" || v == "uninstall")
}

// isActionLiteral reports whether e is the string literal "action".
func isActionLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == "action"
}

// containsInstallLiteral reports whether a string literal "install" or
// "uninstall" occurs anywhere in e.
func containsInstallLiteral(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if x, ok := n.(ast.Expr); ok && isInstallOrUninstall(x) {
			found = true
		}
		return !found
	})
	return found
}

// installActionFindings returns one message for each place the Go source of
// each file in sources sends the install or uninstall action of the catalog
// API: a string literal with action=install or action=uninstall, a map or
// url.Values entry with the key "action" and the value install or uninstall,
// and a call such as Set("action", "install"). Only the files named in
// catalogWriteFiles may do so. A file that does not parse is a finding.
func installActionFindings(sources map[string]string) []string {
	var out []string
	for name, src := range sources {
		if slices.Contains(catalogWriteFiles, name) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			out = append(out, name+": does not parse: "+err.Error())
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING && (strings.Contains(x.Value, "action=install") || strings.Contains(x.Value, "action=uninstall")) {
					out = append(out, name+": a string literal sends "+strconv.Quote(x.Value))
				}
			case *ast.KeyValueExpr:
				if isActionLiteral(x.Key) && containsInstallLiteral(x.Value) {
					out = append(out, name+": an entry with the key \"action\" sends the install or uninstall action")
				}
			case *ast.CallExpr:
				if len(x.Args) >= 2 && isActionLiteral(x.Args[0]) && isInstallOrUninstall(x.Args[1]) {
					out = append(out, name+": a call sends the install or uninstall action")
				}
			}
			return true
		})
	}
	slices.Sort(out)
	return out
}

// TestInstallActionOnlyInCatalogWriteFiles asserts no non-test file of the
// package but the two that hold the catalog write probes and their cleanup
// sends the install or uninstall action, so no probe or setup step installs a
// chart without the cleanup that removes what the install creates.
func TestInstallActionOnlyInCatalogWriteFiles(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	sources := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		require.NoError(t, err)
		sources[name] = string(b)
	}
	for _, name := range catalogWriteFiles {
		require.Contains(t, sources, name)
	}
	assert.Empty(t, installActionFindings(sources))
}

// TestInstallActionFindings asserts installActionFindings accepts a file that
// sends no install action and reports a file that posts to the install
// action through the path, a url.Values literal or a call, and a file that
// does not parse; the files of catalogWriteFiles are never reported.
func TestInstallActionFindings(t *testing.T) {
	t.Parallel()
	const header = "package fidelity\n\nimport (\n\t\"net/http\"\n\t\"net/url\"\n)\n\n"
	for _, tc := range []struct {
		name string
		file string
		src  string
		want int
	}{
		{"a clean file", "other_probes.go", header + "var _ = RequestSpec{Method: http.MethodGet, Path: \"/v1/catalog.cattle.io.clusterrepos\", Query: url.Values{\"limit\": {\"1\"}}}\n", 0},
		{"another action", "other_probes.go", header + "var _ = url.Values{\"action\": {\"refresh\"}}\n", 0},
		{"an install in the path", "other_probes.go", header + "var _ = RequestSpec{Method: http.MethodPost, Path: \"/v1/catalog.cattle.io.clusterrepos/rancher-charts?action=install\"}\n", 1},
		{"an uninstall in the path", "other_probes.go", header + "var _ = RequestSpec{Method: http.MethodPost, Path: \"/v1/catalog.cattle.io.apps/default/x?action=uninstall\"}\n", 1},
		{"an install in the query", "other_probes.go", header + "var _ = RequestSpec{Method: http.MethodPost, Path: \"/v1/catalog.cattle.io.clusterrepos/rancher-charts\", Query: url.Values{\"action\": {\"install\"}}}\n", 1},
		{"an install through Set", "other_probes.go", header + "func f(q url.Values) { q.Set(\"action\", \"install\") }\n", 1},
		{"an install in a catalog write file", "catalogwrite_probes.go", header + "var _ = url.Values{\"action\": {\"install\"}}\n", 0},
		{"a file that does not parse", "other_probes.go", "package fidelity\nfunc (", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Len(t, installActionFindings(map[string]string{tc.file: tc.src}), tc.want)
		})
	}
}

// importsOf returns the import paths of the Go source src.
func importsOf(src string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// startsProcess reports whether the Go source src imports os/exec.
func startsProcess(src string) (bool, error) {
	paths, err := importsOf(src)
	if err != nil {
		return false, err
	}
	return slices.Contains(paths, "os/exec"), nil
}

// TestCatalogWriteFilesStartNoProcess asserts the catalog write probes and
// their cleanup import no os/exec: they reach the cluster only through the
// requests of the Target they are given, so a run on the downstream and a
// run on the local cluster use the same path.
func TestCatalogWriteFilesStartNoProcess(t *testing.T) {
	t.Parallel()
	for _, name := range catalogWriteFiles {
		b, err := os.ReadFile(name)
		require.NoError(t, err)
		starts, err := startsProcess(string(b))
		require.NoError(t, err)
		assert.False(t, starts, "%s imports os/exec", name)
	}
}

// TestStartsProcess asserts startsProcess reports an os/exec import, plain,
// aliased or blank, and none for other imports.
func TestStartsProcess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"no imports", "package p\n", false},
		{"os alone", "package p\n\nimport \"os\"\n", false},
		{"os/exec", "package p\n\nimport (\n\t\"fmt\"\n\t\"os/exec\"\n)\n", true},
		{"an alias", "package p\n\nimport run \"os/exec\"\n", true},
		{"a blank import", "package p\n\nimport _ \"os/exec\"\n", true},
		{"a path that only contains it", "package p\n\nimport \"example.com/os/exec\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := startsProcess(tc.src)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	_, err := startsProcess("package")
	require.Error(t, err)
}

// parseGoFile parses the Go file at path.
func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err)
	return file
}

// stringConstants returns the value of each of the named string constants
// declared in file; a name that is not a string constant is a failure.
func stringConstants(t *testing.T, file *ast.File, names ...string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !slices.Contains(names, id.Name) || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "constant %s is not a string literal", id.Name)
				v, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				got[id.Name] = v
			}
		}
	}
	for _, name := range names {
		require.Contains(t, got, name, "constant %s", name)
	}
	return got
}

// placeholderPattern matches a {name} placeholder in a probe body, with the
// quotes around it when it stands inside a string.
var placeholderPattern = regexp.MustCompile(`"?\{[^{}"]+\}"?`)

// decodedInstallBody returns the install body of the catalog write probes
// decoded as JSON, each placeholder replaced by a JSON string.
func decodedInstallBody(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(placeholderPattern.ReplaceAllString(catalogInstallBody(), `"x"`)), &body))
	return body
}

// installChart returns the one chart entry of a decoded install body.
func installChart(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	charts, ok := body["charts"].([]any)
	require.True(t, ok, "the install body has no charts")
	require.Len(t, charts, 1)
	chart, ok := charts[0].(map[string]any)
	require.True(t, ok)
	return chart
}

// TestCatalogWriteProbesSendTheCLIsAnnotations asserts the annotation keys
// and the source repository type the catalog write probes send equal the
// ones app install sends, read from its source, and that the probes' install
// body carries all three annotations.
func TestCatalogWriteProbesSendTheCLIsAnnotations(t *testing.T) {
	t.Parallel()
	cli := stringConstants(t, parseGoFile(t, filepath.Join("..", "..", "engine", "catalog", "action.go")),
		"annotationRequestID", "annotationSourceRepoType", "annotationSourceRepo", "sourceRepoTypeCluster")
	assert.Equal(t, cli["annotationRequestID"], catalogRequestIDAnnotation)
	assert.Equal(t, cli["annotationSourceRepoType"], catalogSourceRepoTypeAnnotation)
	assert.Equal(t, cli["annotationSourceRepo"], catalogSourceRepoAnnotation)
	assert.Equal(t, cli["sourceRepoTypeCluster"], catalogSourceRepoTypeCluster)

	annotations, ok := installChart(t, decodedInstallBody(t))["annotations"].(map[string]any)
	require.True(t, ok, "the chart entry has no annotations")
	assert.Contains(t, annotations, cli["annotationRequestID"])
	assert.Equal(t, cli["sourceRepoTypeCluster"], annotations[cli["annotationSourceRepoType"]])
	assert.Contains(t, annotations, cli["annotationSourceRepo"])
}

// cliClusterValueKeys returns the dotted path of each value app install adds
// to every chart, read from the clusterValues list in file.
func cliClusterValueKeys(t *testing.T, file *ast.File) []string {
	t.Helper()
	var keys []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || vs.Names[0].Name != "clusterValues" || len(vs.Values) != 1 {
				continue
			}
			list, ok := vs.Values[0].(*ast.CompositeLit)
			require.True(t, ok, "clusterValues is not a composite literal")
			for _, elt := range list.Elts {
				row, ok := elt.(*ast.CompositeLit)
				require.True(t, ok && len(row.Elts) > 0, "a clusterValues row is not a composite literal")
				path, ok := row.Elts[0].(*ast.CompositeLit)
				require.True(t, ok, "the first element of a clusterValues row is not a path")
				var parts []string
				for _, p := range path.Elts {
					lit, ok := p.(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING, "a path element is not a string literal")
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					parts = append(parts, v)
				}
				keys = append(keys, strings.Join(parts, "."))
			}
		}
	}
	require.NotEmpty(t, keys, "no clusterValues rows found")
	slices.Sort(keys)
	return keys
}

// TestCatalogWriteProbesSendTheCLIsClusterValueKeys asserts the cluster
// values the catalog write probes send are the ones app install adds to
// every chart, read from its source, except the Windows value, and that the
// probes' install body carries exactly those keys under global.cattle. App
// install sends global.cattle.windows.enabled only for a cluster with Windows
// workers, which neither oracle cluster has.
func TestCatalogWriteProbesSendTheCLIsClusterValueKeys(t *testing.T) {
	t.Parallel()
	const windowsOnly = "global.cattle.windows.enabled"
	cli := cliClusterValueKeys(t, parseGoFile(t, filepath.Join("..", "..", "engine", "catalog", "clustervalues.go")))
	require.Contains(t, cli, windowsOnly, "the CLI no longer has a Windows-only cluster value; remove the exception")
	var want []string
	for _, k := range cli {
		if k != windowsOnly {
			want = append(want, k)
		}
	}
	var got []string
	for _, k := range catalogValueKeys {
		got = append(got, "global.cattle."+k)
	}
	slices.Sort(got)
	assert.Equal(t, want, got)

	values, ok := installChart(t, decodedInstallBody(t))["values"].(map[string]any)
	require.True(t, ok, "the chart entry has no values")
	global, ok := values["global"].(map[string]any)
	require.True(t, ok)
	cattle, ok := global["cattle"].(map[string]any)
	require.True(t, ok)
	var sent []string
	for k := range cattle {
		sent = append(sent, k)
	}
	assert.ElementsMatch(t, catalogValueKeys, sent)
}
