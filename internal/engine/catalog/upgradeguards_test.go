package catalog

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceFile is a non-test Go file of the package, parsed.
type sourceFile struct {
	name string
	file *ast.File
}

// packageSources parses the non-test Go files of the package, found with
// os.ReadDir and named without a directory.
func packageSources(t *testing.T) []sourceFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var out []sourceFile
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		out = append(out, sourceFile{name: name, file: f})
	}
	require.NotEmpty(t, out)
	return out
}

// functionsThat lists, as "file:function", the functions and methods of the
// named files whose body satisfies match for some node; a closure counts for
// the function that holds it.
func functionsThat(t *testing.T, sources []sourceFile, files []string, match func(ast.Node) bool) []string {
	t.Helper()
	var out []string
	for _, src := range sources {
		if files != nil && !slices.Contains(files, src.name) {
			continue
		}
		for _, d := range src.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n != nil && match(n) {
					found = true
				}
				return !found
			})
			if found {
				out = append(out, src.name+":"+fn.Name.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// callOf reports whether n calls the function or method called name.
func callOf(n ast.Node, names ...string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return slices.Contains(names, fun.Name)
	case *ast.SelectorExpr:
		return slices.Contains(names, fun.Sel.Name)
	}
	return false
}

// infoPlanPath is one Plan path that reads the info of a chart version, with
// the function of non-test code that makes the read: a new caller of ReadInfo
// fails TestEveryInfoReadIsForAListedOrCurrentVersion until it gets a row.
type infoPlanPath struct {
	name string
	// caller is the function that calls ReadInfo, as "file:function".
	caller string
	// upgrade is true for app upgrade, whose own chart version may be read
	// though the index does not list it.
	upgrade bool
	setup   func(*appFake)
	edit    func(*appInput)
	// refused is true when the Plan is VALIDATION with no info request.
	refused bool
}

// TestEveryInfoReadIsForAListedOrCurrentVersion asserts, for every Plan path
// that reads the info of a chart version, that each info request names a
// version the filtered index lists or, for app upgrade, the version the App
// runs; and that a typed version the index does not list is VALIDATION with
// no info request. A source scan asserts every call of ReadInfo in the
// package's non-test code sits in a function the table names, and that no
// package that imports this one calls it.
func TestEveryInfoReadIsForAListedOrCurrentVersion(t *testing.T) {
	t.Parallel()
	const install, upgrade = "install.go:Plan", "upgrade.go:readInfo"
	only2 := func(f *appFake) { listIndex(f, "2.0.0") }
	paths := []infoPlanPath{
		{name: "install with a listed version", caller: install, edit: nil},
		{name: "install with a typed listed version", caller: install, edit: withFlag(flagVersion, "1.0.0")},
		{name: "install with a typed version the index does not list", caller: install, refused: true, edit: withFlag(flagVersion, "9.9.9")},
		{name: "upgrade with a typed listed version", caller: upgrade, upgrade: true, edit: withFlag(flagVersion, "2.0.0")},
		{name: "upgrade keeping a version the index lists", caller: upgrade, upgrade: true},
		{name: "upgrade keeping a version the index does not list", caller: upgrade, upgrade: true, setup: only2},
		{name: "upgrade keeping a typed version the index does not list", caller: upgrade, upgrade: true, setup: only2, edit: withFlag(flagVersion, "1.0.0")},
		{name: "upgrade with a typed version the index does not list", caller: upgrade, upgrade: true, refused: true, edit: withFlag(flagVersion, "9.9.9")},
		{name: "upgrade with a typed version the index does not list, current not listed", caller: upgrade, upgrade: true, refused: true, setup: only2, edit: withFlag(flagVersion, "9.9.9")},
		{name: "upgrade with another repo that lists the version the App runs", caller: upgrade, upgrade: true,
			setup: func(f *appFake) { addOtherRepo(f, nil, "1.0.0") }, edit: withFlag(flagRepo, otherRepo)},
		{name: "upgrade with another repo whose index does not list the version the App runs", caller: upgrade, upgrade: true, refused: true,
			setup: func(f *appFake) { addOtherRepo(f, nil, "2.0.0") }, edit: withFlag(flagRepo, otherRepo)},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			var f *appFake
			var err error
			in := installInput()
			if p.upgrade {
				f = upgradeFake(t, nil)
				in = upgradeInput()
			} else {
				f = newAppFake(t)
			}
			if p.setup != nil {
				p.setup(f)
			}
			if p.edit != nil {
				p.edit(&in)
			}
			if p.upgrade {
				_, _, err = planUpgrade(t, f, nil, in)
			} else {
				_, _, err = newTestInstall(f, nil).Plan(t.Context(), in)
			}
			listed := listedVersions(t, f)
			infos := requestsOf(f, "link=info")
			if p.refused {
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Empty(t, infos, "an info request for a version the index does not list")
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, infos, "the path reads the info of the version it sends")
			for _, path := range infos {
				u, perr := url.Parse(path)
				require.NoError(t, perr)
				version := u.Query().Get("version")
				ok := slices.Contains(listed, version) || (p.upgrade && version == "1.0.0")
				assert.True(t, ok, "info of version %q, which the index %v does not list and the App does not run", version, listed)
			}
		})
	}

	callers := functionsThat(t, packageSources(t), nil, func(n ast.Node) bool { return callOf(n, "ReadInfo") })
	var named []string
	for _, p := range paths {
		if !slices.Contains(named, p.caller) {
			named = append(named, p.caller)
		}
	}
	slices.Sort(named)
	assert.Equal(t, named, callers, "every function of non-test code that calls ReadInfo has a row in the table, and every row names one")
	assert.Empty(t, readInfoCallersOutsideThePackage(t),
		"no package that imports this one calls ReadInfo: a new caller needs a row of the table first")
}

// readInfoCallersOutsideThePackage lists, as "directory/file:function", the
// functions of the module's non-test Go files outside this package that call
// ReadInfo of this package: every package that imports it can.
func readInfoCallersOutsideThePackage(t *testing.T) []string {
	t.Helper()
	const importPath = "github.com/rancher/rancher-cli/internal/engine/catalog"
	root := filepath.Join("..", "..", "..")
	here, err := filepath.Abs(".")
	require.NoError(t, err)
	var out []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "graphify-out" || name == "docs") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil || dir == here {
			return err
		}
		imports, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(imports.Imports, func(i *ast.ImportSpec) bool { return i.Path.Value == strconv.Quote(importPath) }) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, fn := range functionsThat(t, []sourceFile{{name: "f", file: f}}, nil, func(n ast.Node) bool { return callOf(n, "ReadInfo") }) {
			out = append(out, filepath.ToSlash(rel)+strings.TrimPrefix(fn, "f"))
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// listedVersions are the versions of the chart the fake's filtered index
// lists.
func listedVersions(t *testing.T, f *appFake) []string {
	t.Helper()
	answer := f.root.answers[indexPath()]
	var idx Index
	require.NoError(t, json.Unmarshal([]byte(answer.body), &idx))
	var out []string
	for _, v := range idx.Entries[testChart] {
		out = append(out, v.Version)
	}
	return out
}

// conflictRow is one CONFLICT app upgrade builds: the function of non-test
// code that builds it, whether it is retryable, and how to make it.
type conflictRow struct {
	name      string
	builder   string
	retryable bool
	run       func(t *testing.T) error
}

// planConflict runs the Plan of an upgrade on a fake with the App edited and
// returns its error.
func planConflict(app func(*upgradeApp), setup func(*appFake)) func(t *testing.T) error {
	return func(t *testing.T) error {
		t.Helper()
		var edits []func(*upgradeApp)
		if app != nil {
			edits = append(edits, app)
		}
		f := upgradeFake(t, nil, edits...)
		if setup != nil {
			setup(f)
		}
		_, _, err := planUpgrade(t, f, nil, upgradeInput())
		return err
	}
}

// executeConflict plans an upgrade, changes the fake and returns the error of
// Execute.
func executeConflict(change func(*appFake)) func(t *testing.T) error {
	return func(t *testing.T) error {
		t.Helper()
		f := upgradeFake(t, nil)
		_, exec, err := planUpgrade(t, f, nil, upgradeInput())
		require.NoError(t, err)
		change(f)
		_, err = exec(t.Context())
		assert.Zero(t, f.postCount())
		return err
	}
}

// TestEveryUpgradeConflictHasARetryableRow asserts every CONFLICT app
// upgrade builds: the refusals of the Plan are not retryable, as their remedy
// says what to do first; the mismatches of the release read and the re-reads
// of Execute are retryable. A source scan asserts every use of
// clierr.CodeConflict and every call of changedBeforeRequest,
// operationRunning and recheckOperations in the files that hold upgrade code
// sits in a function a row names.
func TestEveryUpgradeConflictHasARetryableRow(t *testing.T) {
	t.Parallel()
	status := func(s string) func(*upgradeApp) { return func(a *upgradeApp) { a.status = s } }
	other := func(body string) func(*appFake) {
		return func(f *appFake) {
			f.root.answers[releaseSecretPath(releaseSecretName(3))] = rootAnswer{status: 200, body: body}
		}
	}
	rows := []conflictRow{
		{"status uninstalled", "upgrade.go:refuseStatus", false, planConflict(status("uninstalled"), nil)},
		{"status uninstalling", "upgrade.go:refuseStatus", false, planConflict(status("uninstalling"), nil)},
		{"status pending-install", "upgrade.go:refuseStatus", false, planConflict(status("pending-install"), nil)},
		{"status pending-upgrade", "upgrade.go:refuseStatus", false, planConflict(status("pending-upgrade"), nil)},
		{"status pending-rollback", "upgrade.go:refuseStatus", false, planConflict(status("pending-rollback"), nil)},
		{"status empty", "upgrade.go:refuseStatus", false, planConflict(status(""), nil)},
		{"status unknown", "upgrade.go:refuseStatus", false, planConflict(status("unknown"), nil)},
		{"pending with an operation running", "upgrade.go:checkStatus", false, planConflict(status("pending-install"), runningOperationFixture)},
		{"deployed with an operation running", "upgrade.go:Plan", false, planConflict(nil, runningOperationFixture)},
		{"release Secret not found", "release.go:moved", true, planConflict(nil, func(f *appFake) {
			f.root.answers[releaseSecretPath(releaseSecretName(3))] = rootAnswer{status: http.StatusNotFound, body: `{"message":"not found"}`}
		})},
		{"release Secret of another revision", "release.go:moved", true, planConflict(nil,
			other(secretBody("helm", "my-nginx", "2", `{"name":"my-nginx","namespace":"web","version":2}`)))},
		{"release Secret of another release", "release.go:moved", true, planConflict(nil,
			other(secretBody("helm", "my-nginx", "3", `{"name":"other","namespace":"web","version":3}`)))},
		{"app changed before the request", "upgrade.go:execute", true, executeConflict(func(f *appFake) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.objs[objKey("apps", testNS, testRelease)].SetResourceVersion("2")
		})},
		{"app removed before the request", "upgrade.go:execute", true, executeConflict(func(f *appFake) { f.remove("apps", testNS, testRelease) })},
		{"operation started before the request", "upgrade.go:execute", true, executeConflict(func(f *appFake) {
			f.put("operations", testNS, "helm-operation-z", operation(testRelease, testNow, "", 1, 0))
		})},
		{"digest changed before the request", "chartplan.go:recheckIndex", true, executeConflict(func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0", Digest: "sha256:new"}}})}
		})},
		{"version left the index before the request", "chartplan.go:recheckIndex", true, executeConflict(func(f *appFake) { listIndex(f, "2.0.0") })},
		{"index unreadable before the request", "chartplan.go:recheckIndex", true, executeConflict(func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: http.StatusInternalServerError, body: apiserverError("down")}
		})},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := row.run(t)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Equal(t, row.retryable, ce.Retryable)
		})
	}

	builders := map[string]bool{}
	for _, row := range rows {
		builders[row.builder] = true
	}
	upgradeFiles := []string{"upgrade.go", "release.go", "chartplan.go"}
	uses := functionsThat(t, packageSources(t), upgradeFiles, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "clierr" && sel.Sel.Name == "CodeConflict" {
				return true
			}
		}
		return callOf(n, "changedBeforeRequest", "operationRunning", "recheckOperations")
	})
	assert.Equal(t, slices.Sorted(maps.Keys(builders)), uses,
		"every function that builds a CONFLICT has a row, and every row names one")
}
