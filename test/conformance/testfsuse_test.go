package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The direct calls that differ by OS, named by what a row of the allowlist
// records.
const (
	callGOOSWindows = `runtime.GOOS compared with "windows"`
	callSeparator   = "path separator compared"
	callSymlink     = "os.Symlink"
	callChmod       = "os.Chmod"
	callGeteuid     = "os.Geteuid or os.Getuid"
	callModTime     = "ModTime"
	callENOTDIR     = "ENOTDIR"
	callACLRead     = "access list read"
	callGOOSValue   = "runtime.GOOS used as a value"
	callDotImport   = "dot import of a package with OS calls"
)

// testfsAdvice names, for each call, what a test uses instead.
var testfsAdvice = map[string]string{
	callGOOSWindows: "use testfs.OnWindows or testfs.SkipOnWindows",
	callSeparator:   "use testfs.OnWindows, or path.Join and filepath.ToSlash for a path",
	callSymlink:     "use testfs.Symlink",
	callChmod:       "use testfs.RestrictAccess",
	callGeteuid:     "use testfs.RestrictAccess, which skips as root",
	callModTime:     "use testfs.Snapshot, which leaves a directory's time out on Windows",
	callENOTDIR:     "add a helper to internal/testfs for a path below a plain file",
	callACLRead:     "use testfs.ReadACL or testfs.RequireOwnerOnly",
	callGOOSValue:   "use testfs.OnWindows for an OS check, or add a permanent row with the reason to testfsAllowlist in test/conformance/testfsuse_test.go when the value is only text",
	callDotImport:   "import the package by its name so that a call can be checked",
}

// testfsSite is one use of a call that differs by OS. call is the group an
// allowlist row counts it in; spelled is the call as written when that
// differs from the group (syscall.Chmod is counted with os.Chmod).
type testfsSite struct {
	call    string
	spelled string
	line    int
}

func (s testfsSite) message(file string) string {
	name := s.spelled
	if name == "" {
		name = s.call
	}
	return fmt.Sprintf("%s:%d: %s in test code: %s", file, s.line, name, testfsAdvice[s.call])
}

// testfsRow lists how many sites of one call a file holds. A pending row is
// removed when the tests of its package move to the helpers; a permanent row
// stays because the site is not a difference the helpers handle.
type testfsRow struct {
	path   string
	call   string
	count  int
	kind   string
	reason string
}

const (
	rowPending   = "pending"
	rowPermanent = "permanent"

	pendingReason = "migrates with the rest of its package's tests"
)

func pending(path, call string, count int) testfsRow {
	return testfsRow{path: path, call: call, count: count, kind: rowPending, reason: pendingReason}
}

func permanent(path, call string, count int, reason string) testfsRow {
	return testfsRow{path: path, call: call, count: count, kind: rowPermanent, reason: reason}
}

// testfsPackageSelectors maps an import path to the names of that package
// that count as a call, per call kind.
var testfsPackageSelectors = map[string]map[string]string{
	"os": {
		"Symlink": callSymlink,
		"Chmod":   callChmod,
		"Geteuid": callGeteuid,
		"Getuid":  callGeteuid,
	},
	"syscall": {
		"ENOTDIR": callENOTDIR,
		"Symlink": callSymlink,
		"Chmod":   callChmod,
		"Geteuid": callGeteuid,
		"Getuid":  callGeteuid,
	},
	"golang.org/x/sys/unix": {
		"ENOTDIR": callENOTDIR,
		"Chmod":   callChmod,
		"Geteuid": callGeteuid,
		"Getuid":  callGeteuid,
	},
	"golang.org/x/sys/windows": {"GetNamedSecurityInfo": callACLRead, "GetAce": callACLRead, "ACCESS_ALLOWED_ACE": callACLRead},
}

// testfsNoDotImports are the packages whose OS calls a dot import would hide
// from the scan.
var testfsNoDotImports = []string{"os", "syscall", "runtime", "path/filepath", "go/build", "golang.org/x/sys/unix", "golang.org/x/sys/windows"}

// testfsUseSites parses one Go file, whatever its build tags, and returns
// every use of a call that differs by OS, resolving each package selector
// through the file's own imports.
func testfsUseSites(file string, src string) ([]testfsSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var sites []testfsSite
	add := func(call, spelled string, pos token.Pos) {
		sites = append(sites, testfsSite{call: call, spelled: spelled, line: fset.Position(pos).Line})
	}
	imports := map[string]string{}
	for _, spec := range f.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." && slices.Contains(testfsNoDotImports, importPath) {
			add(callDotImport, `a dot import of "`+importPath+`"`, spec.Pos())
		}
		imports[name] = importPath
	}
	unparen := func(e ast.Expr) ast.Expr {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				return e
			}
			e = p.X
		}
	}
	// pkgSelector returns the import path and the name when e is a selector
	// on an imported package.
	pkgSelector := func(e ast.Expr) (string, string, bool) {
		sel, ok := unparen(e).(*ast.SelectorExpr)
		if !ok {
			return "", "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		importPath, ok := imports[id.Name]
		return importPath, sel.Sel.Name, ok
	}
	isSelector := func(e ast.Expr, importPath, name string) bool {
		p, n, ok := pkgSelector(e)
		return ok && p == importPath && n == name
	}
	// osName returns the OS name selector (runtime.GOOS or build.Default.GOOS
	// from go/build) under any parentheses, and how it is written.
	osName := func(e ast.Expr) (*ast.SelectorExpr, string, bool) {
		e = unparen(e)
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return nil, "", false
		}
		if isSelector(sel, "runtime", "GOOS") {
			return sel, "runtime.GOOS", true
		}
		if sel.Sel.Name == "GOOS" && isSelector(sel.X, "go/build", "Default") {
			return sel, "build.Default.GOOS", true
		}
		return nil, "", false
	}
	// separator reports whether e is os.PathSeparator or filepath.Separator,
	// also converted with string().
	separator := func(e ast.Expr) bool {
		e = unparen(e)
		if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "string" {
				e = unparen(call.Args[0])
			}
		}
		return isSelector(e, "os", "PathSeparator") || isSelector(e, "path/filepath", "Separator")
	}
	literal := func(e ast.Expr, kinds ...token.Token) (string, bool) {
		lit, ok := unparen(e).(*ast.BasicLit)
		if !ok || !slices.Contains(kinds, lit.Kind) {
			return "", false
		}
		return lit.Value, true
	}
	isWindowsLiteral := func(e ast.Expr) bool {
		v, ok := literal(e, token.STRING)
		return ok && (v == `"windows"` || v == "`windows`")
	}
	windowsCall := func(name string) (string, string) {
		if name == "runtime.GOOS" {
			return callGOOSWindows, ""
		}
		return callGOOSWindows, name + ` compared with "windows"`
	}

	counted := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
				if sel, name, ok := osName(pair[0]); ok {
					if _, ok := literal(pair[1], token.STRING); ok {
						counted[sel] = true
						if isWindowsLiteral(pair[1]) {
							call, spelled := windowsCall(name)
							add(call, spelled, n.Pos())
						}
					}
				}
				if separator(pair[0]) {
					if _, ok := literal(pair[1], token.CHAR, token.STRING); ok {
						add(callSeparator, "", n.Pos())
					}
				}
			}
		case *ast.SwitchStmt:
			if n.Tag == nil {
				return true
			}
			sel, name, ok := osName(n.Tag)
			if !ok {
				return true
			}
			allLiterals := true
			for _, stmt := range n.Body.List {
				windowsCase := false
				for _, e := range stmt.(*ast.CaseClause).List {
					if _, ok := literal(e, token.STRING); !ok {
						allLiterals = false
					}
					windowsCase = windowsCase || isWindowsLiteral(e)
				}
				if windowsCase {
					call, spelled := windowsCall(name)
					add(call, spelled, stmt.Pos())
				}
			}
			if allLiterals {
				counted[sel] = true
			}
		case *ast.CallExpr:
			hasWindows := false
			var names []string
			var sels []*ast.SelectorExpr
			for _, arg := range n.Args {
				hasWindows = hasWindows || isWindowsLiteral(arg)
				if sel, name, ok := osName(arg); ok {
					sels = append(sels, sel)
					names = append(names, name)
				}
			}
			if hasWindows && len(sels) > 0 {
				for _, sel := range sels {
					counted[sel] = true
				}
				call, spelled := windowsCall(names[0])
				add(call, spelled, n.Pos())
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "ModTime" {
			add(callModTime, "", sel.Pos())
		}
		if importPath, name, ok := pkgSelector(sel); ok {
			if call, ok := testfsPackageSelectors[importPath][name]; ok {
				spelled := ""
				if importPath != "os" {
					spelled = path.Base(importPath) + "." + name
				}
				add(call, spelled, sel.Pos())
			}
		}
		if _, name, ok := osName(sel); ok && !counted[sel] {
			spelled := ""
			if name != "runtime.GOOS" {
				spelled = name + " used as a value"
			}
			add(callGOOSValue, spelled, sel.Pos())
		}
		return true
	})
	slices.SortFunc(sites, func(a, b testfsSite) int { return a.line - b.line })
	return sites, nil
}

// testfsUseProblems returns one message for every direct call that differs
// by OS in the file, each naming the call and the helper to use.
func testfsUseProblems(file string, src string) []string {
	sites, err := testfsUseSites(file, src)
	if err != nil {
		return []string{file + ": " + err.Error()}
	}
	var problems []string
	for _, s := range sites {
		problems = append(problems, s.message(file))
	}
	return problems
}

// testfsKey identifies one call in one file.
type testfsKey struct {
	path string
	call string
}

// testfsAllowlistProblems compares what the scan found with the allowlist:
// a call with no row, a count that differs from its row in either direction,
// and a row that is malformed, duplicated or names a missing file or a file
// of internal/testfs.
func testfsAllowlistProblems(found map[testfsKey][]testfsSite, rows []testfsRow, exists func(string) bool) []string {
	var problems []string
	seen := map[testfsKey]bool{}
	for _, r := range rows {
		key := testfsKey{r.path, r.call}
		switch {
		case seen[key]:
			problems = append(problems, fmt.Sprintf("%s: two rows for %s", r.path, r.call))
			continue
		case strings.HasPrefix(r.path, "internal/testfs/"):
			problems = append(problems, fmt.Sprintf("%s: internal/testfs is not scanned and has no row", r.path))
		case strings.TrimSpace(r.reason) == "":
			problems = append(problems, fmt.Sprintf("%s: the row for %s has no reason", r.path, r.call))
		case r.kind != rowPending && r.kind != rowPermanent:
			problems = append(problems, fmt.Sprintf("%s: the row for %s has the kind %q; a row is pending (removed when its package's tests move to the helpers) or permanent (it stays, with a reason)", r.path, r.call, r.kind))
		case !exists(r.path):
			problems = append(problems, fmt.Sprintf("%s: the file of the row for %s no longer exists", r.path, r.call))
		case testfsAdvice[r.call] == "":
			problems = append(problems, fmt.Sprintf("%s: the row names the unknown call %q", r.path, r.call))
		case r.count < 1:
			problems = append(problems, fmt.Sprintf("%s: the row for %s lists no use; delete it", r.path, r.call))
		}
		seen[key] = true
		got := len(found[key])
		switch {
		case got > r.count:
			problems = append(problems, fmt.Sprintf("%s: %d uses of %s and its row in testfsAllowlist (test/conformance/testfsuse_test.go) allows %d; %s", r.path, got, r.call, r.count, testfsAdvice[r.call]))
		case got < r.count:
			problems = append(problems, fmt.Sprintf("%s: %d uses of %s and its row in testfsAllowlist (test/conformance/testfsuse_test.go) lists %d; lower or delete the row", r.path, got, r.call, r.count))
		}
	}
	for key, sites := range found {
		if seen[key] {
			continue
		}
		for _, s := range sites {
			problems = append(problems, s.message(key.path))
		}
	}
	slices.Sort(problems)
	return problems
}

// testfsScanFiles returns the Go files the scan reads: every test file the
// repository lists, and the non-test files of the test-support packages that
// tests call, leaving out internal/testfs itself.
func testfsScanFiles(t *testing.T) []string {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") || inTestdata(f) || strings.HasPrefix(f, "internal/testfs/") {
			continue
		}
		if strings.HasSuffix(f, "_test.go") || wordingSkipsPath(f) {
			out = append(out, f)
		}
	}
	return out
}

func testfsFound(t *testing.T) map[testfsKey][]testfsSite {
	t.Helper()
	found := map[testfsKey][]testfsSite{}
	for _, f := range testfsScanFiles(t) {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		sites, err := testfsUseSites(f, string(src))
		require.NoError(t, err, f)
		for _, s := range sites {
			key := testfsKey{f, s.call}
			found[key] = append(found[key], s)
		}
	}
	return found
}

// TestTestsUseTheTestfsHelpers asserts a test calls the helpers of
// internal/testfs and not the direct calls that differ by OS (an OS check,
// os.Symlink, os.Chmod, os.Geteuid, a modification time, a path separator
// comparison, ENOTDIR, the raw access list reads). Every use that remains is
// a row of the allowlist with its exact count: a new use fails, and so does
// a row that is too high.
func TestTestsUseTheTestfsHelpers(t *testing.T) {
	t.Parallel()
	problems := testfsAllowlistProblems(testfsFound(t), testfsAllowlist, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	assert.Empty(t, problems)
}

var testfsAllowlist = []testfsRow{
	pending("cmd/rancher/exittable_test.go", callGOOSWindows, 1),
	pending("cmd/rancher/remedyleak_test.go", callGOOSWindows, 2),
	permanent("cmd/rancher/useragent_test.go", callGOOSValue, 1, "the platform string of the user agent"),
	pending("install/harness_test.go", callSymlink, 1),
	pending("install/install_test.go", callChmod, 2),
	pending("install/install_test.go", callSymlink, 5),
	permanent("install/install_test.go", callGOOSValue, 2, "the platform string of an asset name"),
	pending("install/layout_test.go", callSymlink, 1),
	pending("install/windows_harness_test.go", callGOOSWindows, 1),
	permanent("install/windows_harness_test.go", callGOOSValue, 1, "the OS name in a skip message"),
	pending("install/windows_install_test.go", callSymlink, 1),
	pending("internal/cli/callerscope_test.go", callChmod, 2),
	permanent("internal/cli/completion_invariants_test.go", callModTime, 1, "hashDir hashes regular files only (it returns on a directory) and the lazy directory time of NTFS does not reach it"),
	pending("internal/cli/completion_invariants_test.go", callChmod, 2),
	pending("internal/cli/completions/pwsh_continuation_test.go", callGOOSWindows, 1),
	pending("internal/cli/completions/pwsh_description_test.go", callGOOSWindows, 1),
	pending("internal/cli/completions/pwsh_exec_test.go", callGOOSWindows, 3),
	pending("internal/cli/completions/pwsh_execcommand_test.go", callGOOSWindows, 1),
	pending("internal/cli/execcommand_completion_test.go", callSeparator, 1),
	pending("internal/cli/refresh_test.go", callChmod, 2),
	pending("internal/cli/refresh_test.go", callGeteuid, 1),
	pending("internal/cli/refresh_test.go", callSeparator, 1),
	pending("internal/cli/remedydirrule_test.go", callSymlink, 1),
	pending("internal/cli/remedydirrule_test.go", callGOOSWindows, 3),
	pending("internal/cli/telemetry_commands_test.go", callChmod, 2),
	pending("internal/cli/telemetry_commands_test.go", callGeteuid, 1),
	pending("internal/cli/telemetry_commands_test.go", callGOOSWindows, 1),
	pending("internal/clierr/recovery_test.go", callGOOSWindows, 1),
	pending("internal/commands/auth/logout_test.go", callChmod, 1),
	pending("internal/commands/auth/logout_test.go", callSymlink, 3),
	pending("internal/commands/auth/logout_test.go", callGOOSWindows, 4),
	pending("internal/commands/cmdtest/fakehelper.go", callGOOSWindows, 1),
	pending("internal/commands/cmdtest/keychain.go", callGOOSWindows, 1),
	pending("internal/commands/cmdtest/prelude.go", callChmod, 1),
	pending("internal/commands/cmdtest/workdir_test.go", callChmod, 2),
	pending("internal/commands/cmdtest/workdir_test.go", callGeteuid, 1),
	pending("internal/commands/cmdtest/workdir_test.go", callGOOSWindows, 1),
	pending("internal/commands/configcmd/config_delete_server_test.go", callChmod, 2),
	pending("internal/commands/configcmd/config_delete_server_test.go", callGeteuid, 1),
	pending("internal/commands/configcmd/config_delete_server_test.go", callGOOSWindows, 1),
	pending("internal/commands/configcmd/config_edit_test.go", callSymlink, 1),
	pending("internal/commands/configcmd/server_revoke_test.go", callChmod, 3),
	pending("internal/commands/configcmd/server_revoke_test.go", callGOOSWindows, 2),
	pending("internal/commands/telemetrycmd/helpers_test.go", callModTime, 1),
	pending("internal/commands/telemetrycmd/helpers_test.go", callGOOSWindows, 2),
	permanent("internal/commands/telemetrycmd/set_test.go", callModTime, 1, "snapshotOne fingerprints one file, not a directory"),
	pending("internal/commands/telemetrycmd/set_test.go", callSymlink, 1),
	pending("internal/commands/telemetrycmd/status_test.go", callChmod, 2),
	pending("internal/commands/telemetrycmd/status_test.go", callGeteuid, 1),
	pending("internal/commands/telemetrycmd/status_test.go", callSymlink, 1),
	pending("internal/config/cachepause_test.go", callSymlink, 1),
	pending("internal/config/cachepause_test.go", callGOOSWindows, 1),
	pending("internal/config/completion_cache_test.go", callChmod, 1),
	pending("internal/config/completion_cache_test.go", callGOOSWindows, 1),
	pending("internal/config/config_test.go", callGOOSWindows, 1),
	pending("internal/config/credstore_test.go", callChmod, 3),
	pending("internal/config/credstore_test.go", callGOOSWindows, 1),
	pending("internal/config/credstoreid_test.go", callChmod, 1),
	pending("internal/config/credstoreid_test.go", callGeteuid, 1),
	pending("internal/config/credstoreid_test.go", callSymlink, 4),
	pending("internal/config/credtransition_test.go", callChmod, 1),
	pending("internal/config/credtransition_test.go", callGeteuid, 1),
	pending("internal/config/credtransition_test.go", callSymlink, 2),
	pending("internal/config/credtransition_test.go", callGOOSWindows, 2),
	pending("internal/config/credtransition_test.go", callGOOSValue, 2),
	pending("internal/config/credtransitionreplay_test.go", callSymlink, 1),
	pending("internal/config/credtransitionreplay_test.go", callGOOSWindows, 1),
	pending("internal/config/exechelper_test.go", callGOOSWindows, 2),
	pending("internal/config/loginpause_test.go", callSymlink, 1),
	pending("internal/config/loginpause_test.go", callGOOSWindows, 1),
	pending("internal/config/persistlogin_test.go", callGOOSWindows, 1),
	pending("internal/config/pin_test.go", callGOOSWindows, 1),
	pending("internal/config/resolve_test.go", callSymlink, 2),
	pending("internal/config/resolve_test.go", callGOOSWindows, 1),
	pending("internal/config/storeunavailable_test.go", callGOOSWindows, 1),
	pending("internal/config/write_test.go", callSymlink, 1),
	pending("internal/config/write_test.go", callGOOSWindows, 3),
	pending("internal/engine/kubeconfig/execcommand_test.go", callSeparator, 1),
	pending("internal/engine/kubeconfig/noowner_remedies_test.go", callGOOSWindows, 1),
	pending("internal/engine/kubeconfig/records_test.go", callSeparator, 1),
	pending("internal/engine/observe/archive_test.go", callSymlink, 1),
	pending("internal/engine/observe/archive_test.go", callGOOSWindows, 1),
	pending("internal/engine/observe/bundle_test.go", callSymlink, 2),
	permanent("internal/invocation/invocation_test.go", callGOOSValue, 2, "the recorded OS and the platform string"),
	pending("internal/skillinstall/content_test.go", callSymlink, 2),
	pending("internal/skillinstall/helpers_test.go", callSymlink, 1),
	pending("internal/skillinstall/helpers_test.go", callGOOSWindows, 1),
	pending("internal/skillinstall/ops_test.go", callSymlink, 4),
	pending("internal/skillinstall/ops_test.go", callGOOSWindows, 2),
	pending("internal/skillinstall/table_test.go", callSymlink, 1),
	permanent("internal/telemetry/store_test.go", callModTime, 1, "the assertion is the time of one file, the marker, equal to the clock"),
	pending("internal/testrancher/fidelity/driftscript_test.go", callChmod, 2),
	pending("internal/testrancher/fidelity/driftscript_test.go", callGeteuid, 1),
	pending("internal/testrancher/fidelity/driftscript_test.go", callGOOSWindows, 2),
	pending("internal/testrancher/fidelity/oracleminor_test.go", callGOOSWindows, 3),
	pending("internal/token/execstore_fallback_test.go", callGOOSWindows, 1),
	pending("internal/token/noterminalplugin_test.go", callGOOSWindows, 1),
	pending("internal/token/remedyca_test.go", callGOOSWindows, 1),
	pending("internal/token/remedyparse_test.go", callGOOSWindows, 1),
	pending("internal/token/revocation_place_test.go", callChmod, 2),
	pending("internal/token/revocation_rules_test.go", callChmod, 1),
	pending("internal/token/revocation_rules_test.go", callGOOSWindows, 1),
	pending("internal/token/revocation_test.go", callChmod, 2),
	pending("internal/token/shim_test.go", callChmod, 3),
	pending("internal/token/shim_test.go", callGOOSWindows, 3),
	pending("internal/token/storeunavailable_test.go", callGOOSWindows, 1),
	pending("test/conformance/actionlintignore_test.go", callGOOSWindows, 1),
	permanent("test/conformance/cataloglinks_test.go", callGOOSValue, 1, "the platform of a package load, skipped when it is the one this machine already loaded"),
	pending("test/conformance/ciworkflow_test.go", callSymlink, 1),
	pending("test/conformance/ciworkflow_test.go", callGOOSWindows, 2),
	pending("test/conformance/releaseupsert_test.go", callSymlink, 1),
	pending("test/conformance/releaseupsert_test.go", callGOOSWindows, 1),
	pending("test/e2e/harness_test.go", callGOOSWindows, 1),
}

// testfsFixture wraps statements in a test file that imports the packages
// the checker resolves. A row then holds only the statement it is about.
func testfsFixture(imports, body string) string {
	return "package x\n\nimport (\n" + imports + "\n)\n\nfunc f() {\n" + body + "\n}\n"
}

const testfsStdImports = `"os"
"runtime"
"strings"
"slices"
"syscall"
"path/filepath"
"io/fs"
`

// TestTestfsUseCheckerTable asserts the checker accepts text that only
// looks like a direct call and refuses each direct call in every spelling it
// resolves: a function value, an aliased import, a function literal and a
// table row's func field included.
func TestTestfsUseCheckerTable(t *testing.T) {
	t.Parallel()
	fx := func(body string) string { return testfsFixture(testfsStdImports, body) }
	accept := []struct{ name, src string }{
		{"a comment and a string that mention the calls", fx("// os.Symlink(a, b) is not called\n_ = \"os.Symlink(\"")},
		{"runtime.GOOS compared with another name", fx(`_ = runtime.GOOS == "darwin"`)},
		{"a switch with only literal cases", fx("switch runtime.GOOS {\ncase \"darwin\", \"linux\":\n}")},
		{"Chmod on an open file", fx("var f *os.File\n_ = f.Chmod(0o600)")},
		{"a local function named Symlink", "package x\n\nfunc Symlink(a, b string) {}\n\nfunc f() { Symlink(\"a\", \"b\") }\n"},
		{"a user type with a ModTime field", "package x\n\ntype entry struct{ ModTime int }\n\nvar e = entry{ModTime: 1}\n"},
		{"a path separator compared with a variable", fx("var c = filepath.Separator\n_ = filepath.Separator == os.PathSeparator")},
		{"build.Default.GOOS compared with another name", testfsFixture("\"go/build\"", `_ = build.Default.GOOS == "linux"`)},
		{"a parenthesized runtime.GOOS compared with another name", fx(`_ = (runtime.GOOS) == "darwin"`)},
		{"a dot import of a package with no OS calls", testfsFixture(". \"strings\"", `_ = ToUpper("a")`)},
		{"an unqualified Symlink from a local package", "package x\n\nfunc f() { Symlink(\"a\", \"b\") }\n"},
	}
	for _, c := range accept {
		t.Run("accept "+c.name, func(t *testing.T) {
			sites, err := testfsUseSites("x_test.go", c.src)
			require.NoError(t, err)
			assert.Empty(t, sites)
		})
	}

	refuse := []struct {
		name string
		src  string
		want []string
	}{
		{"equality with windows", fx(`_ = runtime.GOOS == "windows"`), []string{callGOOSWindows}},
		{"inequality with windows", fx(`_ = runtime.GOOS != "windows"`), []string{callGOOSWindows}},
		{"windows on the left", fx(`_ = "windows" == runtime.GOOS`), []string{callGOOSWindows}},
		{"a switch with a windows case", fx("switch runtime.GOOS {\ncase \"windows\":\ncase \"darwin\":\n}"), []string{callGOOSWindows}},
		{"strings.EqualFold", fx(`_ = strings.EqualFold(runtime.GOOS, "windows")`), []string{callGOOSWindows}},
		{"a separator compared", fx(`_ = os.PathSeparator == '\\'`), []string{callSeparator}},
		{"filepath separator compared", fx(`_ = filepath.Separator != '/'`), []string{callSeparator}},
		{"os.Symlink", fx(`_ = os.Symlink("a", "b")`), []string{callSymlink}},
		{"os.Chmod", fx(`_ = os.Chmod("a", 0o500)`), []string{callChmod}},
		{"os.Geteuid", fx(`_ = os.Geteuid()`), []string{callGeteuid}},
		{"os.Getuid", fx(`_ = os.Getuid()`), []string{callGeteuid}},
		{"ModTime call", fx("var fi fs.FileInfo\n_ = fi.ModTime()"), []string{callModTime}},
		{"ModTime method value", fx("var fi fs.FileInfo\nstamp := fi.ModTime\n_ = stamp"), []string{callModTime}},
		{"ModTime read of a user type", "package x\n\ntype entry struct{ ModTime int }\n\nfunc f(e entry) int { return e.ModTime }\n", []string{callModTime}},
		{"syscall.ENOTDIR", fx(`_ = syscall.ENOTDIR`), []string{callENOTDIR}},
		{"unix ENOTDIR", testfsFixture("\"golang.org/x/sys/unix\"", `_ = unix.ENOTDIR`), []string{callENOTDIR}},
		{"the access list reads", testfsFixture("\"golang.org/x/sys/windows\"", "_, _ = windows.GetNamedSecurityInfo(\"a\", 0, 0)\nvar a *windows.ACCESS_ALLOWED_ACE\n_ = windows.GetAce(nil, 0, &a)"), []string{callACLRead, callACLRead, callACLRead}},
		{"an aliased runtime", testfsFixture("goruntime \"runtime\"", `_ = goruntime.GOOS == "windows"`), []string{callGOOSWindows}},
		{"os imported under another name", testfsFixture("opsys \"os\"", `_ = opsys.Symlink("a", "b")`), []string{callSymlink}},
		{"a call in a function literal", fx(`go func() { _ = os.Symlink("a", "b") }()`), []string{callSymlink}},
		{"a call in a table row's func field", fx("_ = []struct{ mk func(a, b string) error }{{mk: os.Symlink}}"), []string{callSymlink}},
		{"os.Symlink as a value", fx("symlink := os.Symlink\n_ = symlink"), []string{callSymlink}},
		{"os.Chmod as a value", fx("chmod := os.Chmod\n_ = chmod"), []string{callChmod}},
		{"os.Geteuid as a value", fx("geteuid := os.Geteuid\n_ = geteuid"), []string{callGeteuid}},
		{"runtime.GOOS in a variable", fx("goos := runtime.GOOS\n_ = goos"), []string{callGOOSValue}},
		{"runtime.GOOS in slices.Contains", fx(`_ = slices.Contains([]string{"windows"}, runtime.GOOS)`), []string{callGOOSValue}},
		{"runtime.GOOS in strings.HasPrefix", fx(`_ = strings.HasPrefix(runtime.GOOS, "win")`), []string{callGOOSValue}},
		{"runtime.GOOS against a field", fx("var skip string\n_ = skip == runtime.GOOS"), []string{callGOOSValue}},
		{"runtime.GOOS in a concatenation", fx(`_ = "platform-" + runtime.GOOS`), []string{callGOOSValue}},
		{"a parenthesized runtime.GOOS", fx(`_ = (runtime.GOOS) == "windows"`), []string{callGOOSWindows}},
		{"build.Default.GOOS compared", testfsFixture("\"go/build\"", `_ = build.Default.GOOS == "windows"`), []string{callGOOSWindows}},
		{"build.Default.GOOS as a value", testfsFixture("\"go/build\"", "goos := build.Default.GOOS\n_ = goos"), []string{callGOOSValue}},
		{"a separator converted to a string", fx(`_ = string(os.PathSeparator) == "\\"`), []string{callSeparator}},
		{"a filepath separator converted to a string", fx(`_ = string(filepath.Separator) != "/"`), []string{callSeparator}},
		{"a dot import of os", testfsFixture(". \"os\"", `_ = Symlink("a", "b")`), []string{callDotImport}},
		{"a dot import of syscall", testfsFixture(". \"syscall\"", `_ = Chmod("a", 0)`), []string{callDotImport}},
		{"a dot import of runtime", testfsFixture(". \"runtime\"", `_ = GOOS`), []string{callDotImport}},
		{"a dot import of path/filepath", testfsFixture(". \"path/filepath\"", `_ = Separator`), []string{callDotImport}},
		{"syscall.Geteuid", fx(`_ = syscall.Geteuid()`), []string{callGeteuid}},
		{"syscall.Getuid", fx(`_ = syscall.Getuid()`), []string{callGeteuid}},
		{"syscall.Chmod", fx(`_ = syscall.Chmod("a", 0)`), []string{callChmod}},
		{"syscall.Symlink", fx(`_ = syscall.Symlink("a", "b")`), []string{callSymlink}},
		{"unix Geteuid", testfsFixture("\"golang.org/x/sys/unix\"", `_ = unix.Geteuid()`), []string{callGeteuid}},
		{"unix Chmod", testfsFixture("\"golang.org/x/sys/unix\"", `_ = unix.Chmod("a", 0)`), []string{callChmod}},
		{"a switch with a variable case", fx("var a string\nswitch runtime.GOOS {\ncase a:\n}"), []string{callGOOSValue}},
	}
	for _, c := range refuse {
		t.Run("refuse "+c.name, func(t *testing.T) {
			sites, err := testfsUseSites("x_test.go", c.src)
			require.NoError(t, err)
			var got []string
			for _, s := range sites {
				got = append(got, s.call)
			}
			assert.Equal(t, c.want, got)
			problems := testfsUseProblems("x_test.go", c.src)
			require.Len(t, problems, len(c.want))
			assert.Contains(t, problems[0], "x_test.go:")
			assert.Contains(t, problems[0], testfsAdvice[c.want[0]])
		})
	}
}

// TestTestfsAllowlistTable asserts the comparison of the scan with the
// allowlist: a call with no row, a count above or below its row, and each
// malformed row are refused, and an exact count is accepted.
func TestTestfsAllowlistTable(t *testing.T) {
	t.Parallel()
	site := testfsSite{call: callSymlink, line: 3}
	found := func(n int) map[testfsKey][]testfsSite {
		return map[testfsKey][]testfsSite{{"a_test.go", callSymlink}: slices.Repeat([]testfsSite{site}, n)}
	}
	exists := func(p string) bool { return p != "gone_test.go" }
	row := func(edit func(*testfsRow)) []testfsRow {
		r := pending("a_test.go", callSymlink, 2)
		if edit != nil {
			edit(&r)
		}
		return []testfsRow{r}
	}
	rows := []struct {
		name  string
		found map[testfsKey][]testfsSite
		rows  []testfsRow
		want  string
	}{
		{name: "an exact count", found: found(2), rows: row(nil)},
		{name: "a use above the row", found: found(3), rows: row(nil), want: "3 uses of os.Symlink and its row in testfsAllowlist (test/conformance/testfsuse_test.go) allows 2"},
		{name: "a use below the row", found: found(1), rows: row(nil), want: "lists 2; lower or delete the row"},
		{name: "a stale row", found: nil, rows: row(nil), want: "0 uses of os.Symlink"},
		{name: "a call with no row", found: found(1), want: "os.Symlink in test code: use testfs.Symlink"},
		{name: "an empty reason", found: found(2), rows: row(func(r *testfsRow) { r.reason = "" }), want: "has no reason"},
		{name: "a missing file", found: nil, rows: []testfsRow{pending("gone_test.go", callSymlink, 0)}, want: "no longer exists"},
		{name: "another kind", found: found(2), rows: row(func(r *testfsRow) { r.kind = "other" }), want: "a row is pending (removed when its package's tests move to the helpers) or permanent"},
		{name: "a row under internal/testfs", found: nil, rows: []testfsRow{pending("internal/testfs/x_test.go", callSymlink, 0)}, want: "is not scanned"},
		{name: "a row of count zero", found: nil, rows: []testfsRow{pending("a_test.go", callSymlink, 0)}, want: "lists no use; delete it"},
		{name: "a row of count zero over a file with uses", found: found(2), rows: []testfsRow{pending("a_test.go", callSymlink, 0)}, want: "lists no use; delete it"},
		{name: "an unknown call", found: nil, rows: []testfsRow{pending("a_test.go", "os.Remove", 0)}, want: "unknown call"},
		{name: "two rows for one call", found: found(2), rows: append(row(nil), row(nil)...), want: "two rows"},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			problems := testfsAllowlistProblems(c.found, c.rows, exists)
			if c.want == "" {
				assert.Empty(t, problems)
				return
			}
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), c.want)
		})
	}
}

// testfsImporterRow names a test-support package that may import
// internal/testfs, with the reason.
type testfsImporterRow struct {
	pkg    string
	reason string
}

// testfsImporterAllowlist is empty. internal/testfs imports testing and no
// production package may import it.
var testfsImporterAllowlist []testfsImporterRow

// testfsImportProblems returns the packages that import internal/testfs
// without a row, and the rows that have no reason or name a package with no
// such import. It reports every problem it finds.
func testfsImportProblems(edges map[string]bool, allow []testfsImporterRow) []string {
	var problems []string
	importers := map[string]bool{}
	for edge := range edges {
		from, to, ok := strings.Cut(edge, " -> ")
		if ok && (to == "internal/testfs" || strings.HasPrefix(to, "internal/testfs/")) {
			importers[from] = true
		}
	}
	allowed := map[string]bool{}
	for _, r := range allow {
		allowed[r.pkg] = true
		switch {
		case strings.TrimSpace(r.reason) == "":
			problems = append(problems, r.pkg+": the row has no reason")
		case !importers[r.pkg]:
			problems = append(problems, r.pkg+": the row names a package that does not import internal/testfs")
		}
	}
	for from := range importers {
		if !allowed[from] {
			problems = append(problems, from+" imports internal/testfs, which is test-only (its files import testing): only tests and test-support packages may import it; add a test-support package to testfsImporterAllowlist in test/conformance/testfsuse_test.go with a reason")
		}
	}
	slices.Sort(problems)
	return problems
}

// TestOnlyTestCodeImportsTestfs asserts no non-test package imports
// internal/testfs unless it is a listed test-support package.
func TestOnlyTestCodeImportsTestfs(t *testing.T) {
	t.Parallel()
	assert.Empty(t, testfsImportProblems(realImportEdges(t), testfsImporterAllowlist))
}

// TestTestfsImportTable asserts the import check accepts an unused or an
// allowlisted edge and reports every problem of a refused case.
func TestTestfsImportTable(t *testing.T) {
	t.Parallel()
	edges := map[string]bool{"internal/cli -> internal/config": true, "internal/commands/cmdtest -> internal/testfs": true}
	rows := []struct {
		name  string
		edges map[string]bool
		allow []testfsImporterRow
		want  []string
	}{
		{name: "no edge into internal/testfs", edges: map[string]bool{"internal/cli -> internal/config": true}},
		{name: "an allowlisted package with a reason", edges: edges, allow: []testfsImporterRow{{"internal/commands/cmdtest", "a test-support package"}}},
		{name: "an edge from another package", edges: edges, want: []string{"internal/commands/cmdtest imports internal/testfs", "add a test-support package to testfsImporterAllowlist in test/conformance/testfsuse_test.go with a reason"}},
		{name: "a row with no reason", edges: edges, allow: []testfsImporterRow{{"internal/commands/cmdtest", ""}}, want: []string{"the row has no reason"}},
		{name: "a stale row", edges: edges, allow: []testfsImporterRow{{"internal/commands/cmdtest", "support"}, {"internal/cli", "support"}}, want: []string{"internal/cli: the row names a package that does not import"}},
		{name: "a stale row and a row with no reason", edges: edges, allow: []testfsImporterRow{{"internal/commands/cmdtest", ""}, {"internal/cli", "support"}}, want: []string{"the row has no reason", "does not import internal/testfs"}},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			problems := strings.Join(testfsImportProblems(c.edges, c.allow), "\n")
			if len(c.want) == 0 {
				assert.Empty(t, problems)
			}
			for _, w := range c.want {
				assert.Contains(t, problems, w)
			}
		})
	}
}
