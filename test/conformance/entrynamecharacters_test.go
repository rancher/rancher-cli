package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
)

// TestEntryNameRuleAgreesWithThePrinterForEveryCodePoint asserts the rule
// that refuses a new server name and the rule output.Printable applies to
// every text it prints agree: for every code point the name "a", the code
// point, "b" has a problem exactly when the printer changes it, and a name
// with a byte that is not valid UTF-8 has one and the printer changes it.
// internal/config cannot import internal/output, so it holds its own copy of
// the rule, and the test fails when either rule changes alone.
func TestEntryNameRuleAgreesWithThePrinterForEveryCodePoint(t *testing.T) {
	t.Parallel()
	var disagree []string
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		name := "a" + string(r) + "b"
		_, problem := config.EntryNameProblem(name)
		changed := output.Printable(name) != name
		if problem != changed && len(disagree) < 20 {
			disagree = append(disagree, fmt.Sprintf("U+%04X: problem %v, printer changes it %v", r, problem, changed))
		}
	}
	assert.Empty(t, disagree, "the name rule and output.Printable disagree")

	for _, name := range []string{"a\x80b", "a\xffb", "a\xe2\x80", "\xc0\xaf", "a\xf4\x90\x80\x80b"} {
		p, problem := config.EntryNameProblem(name)
		assert.True(t, problem, "%q is a problem", name)
		assert.True(t, p.InvalidUTF8, "%q is invalid UTF-8", name)
		assert.NotEqual(t, name, output.Printable(name), "the printer changes %q", name)
	}
}

// serverWriteRow is one reviewed function that writes an entry into a
// config's Servers map.
type serverWriteRow struct {
	file     string
	function string
	// kind is "creates" (it can add a key that is not in the config yet),
	// "updates" (it writes an entry it read from the same config) or "test
	// support" (it seeds a directory for a test).
	kind string
	// reason says why the kind holds.
	reason string
	// checkedBy names a function in the same file that holds the call of
	// CheckNewEntryName for a creating function that does not call it itself.
	checkedBy string
}

// serverWriteRows lists every function of the repository that writes a Servers
// entry, outside tests and testdata. A function that creates an entry calls
// config.CheckNewEntryName itself, or calls the function checkedBy names,
// which calls it. A method is written Receiver.Method, without a pointer star.
var serverWriteRows = []serverWriteRow{
	{"internal/commands/register/register.go", "commitCreateWith", "creates", "adds the entry of a registration under the config lock; refuses the name before it removes credentials left under it", ""},
	{"internal/config/persistlogin.go", "persistUnderLocks", "creates", "writes the entry of every login; the same call that resolves the name also refuses a name that is not yet an entry; the writes after a failed commit restore or rebuild the entry this call is writing", "resolveNewEntryName"},
	{"internal/config/persistlogin.go", "loginRollback.undo", "updates", "restores the entry a login wrote", ""},
	{"internal/config/persistlogin.go", "UpdateServerEntry", "updates", "rewrites an entry read from the same config", ""},
	{"internal/commands/register/register.go", "ApplyUpdate", "updates", "rewrites an entry that exists; update-server fails when the entry is missing", ""},
	{"internal/commands/configcmd/config_delete_server.go", "configDeleteServerCmd.removeEntry", "updates", "puts back an entry a delete removed", ""},
	{"internal/token/revocation.go", "Deletion.restore", "updates", "puts back an entry read from the same config", ""},
	{"internal/token/revocation.go", "Deletion.Settle", "updates", "rewrites an entry read from the same config", ""},
	{"internal/token/revocationplan.go", "Deletion.firstWrite", "updates", "rewrites entries read from the same config", ""},
	{"internal/config/cachepause.go", "resumeWith", "updates", "rewrites an entry read from the same config", ""},
	{"internal/config/cachebinding.go", "CacheBinding.RecordAuthConfig", "updates", "rewrites an entry read from the same config", ""},
	{"internal/commands/cmdtest/fakehelper.go", "SeedExecLogout", "test support", "seeds a test directory", ""},
	{"internal/commands/cmdtest/lock.go", "WriteSentinelServer", "test support", "seeds a test directory", ""},
	{"internal/commands/cmdtest/lock.go", "SeedTwoServerConfig", "test support", "seeds a test directory with a populated literal", ""},
	{"internal/commands/cmdtest/prelude.go", "RunPreludeRows", "test support", "seeds a test directory", ""},
	{"internal/commands/cmdtest/prelude.go", "PreludeDir", "test support", "seeds a test directory with a populated literal", ""},
	{"internal/commands/cmdtest/store.go", "SeedUnpinned", "test support", "seeds a test directory", ""},
}

// creatorCallRow is one reviewed function that calls a step that creates a
// server entry.
type creatorCallRow struct {
	file     string
	function string
	calls    string
	// earlyCheckFile and earlyCheckFunction name the function that refuses a
	// new name before the call, and before any request: it calls
	// CheckNewEntryName or EntryNameProblem. It is the calling function itself
	// when that one makes the check.
	earlyCheckFile     string
	earlyCheckFunction string
}

// creatorCallRows lists every function outside tests that calls a function
// that creates a server entry for a caller, with the function that checks the
// name before the call. auth login reaches urlLoginTarget through
// resolveLoginTarget before it registers or signs in. A login with no --url
// reaches authenticateAndStore through authenticatePlain without that check;
// it sets no OwnsEntry, and PersistLoginCredential then creates no entry.
var creatorCallRows = []creatorCallRow{
	{"internal/commands/configcmd/config_add_server.go", "configAddServerCmd.Execute", "CommitCreateUnderLock", "internal/commands/configcmd/config_add_server.go", "configAddServerCmd.Execute"},
	{"internal/commands/auth/login.go", "loginCmd.executeRegisterAndLogin", "CommitCreateUnderLock", "internal/commands/auth/logintarget.go", "urlLoginTarget"},
	{"internal/commands/auth/login.go", "loginCmd.authenticateAndStore", "PersistLoginCredential", "internal/commands/auth/logintarget.go", "urlLoginTarget"},
	{"internal/token/pluginlogin.go", "tokenCmd.pluginLogin", "PersistLoginCredential", "internal/token/pluginlogin.go", "tokenCmd.pluginLogin"},
}

// calleeName is the function name a call expression calls: an identifier, or
// the selected name of a package or method call.
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// callsFunction reports whether node holds a call of the named function.
func callsFunction(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == name {
			found = true
		}
		return !found
	})
	return found
}

// isServersSelector reports whether e is X.Servers.
func isServersSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Servers"
}

// serverWritesIn lists the places in node that write a Servers entry: an
// assignment X.Servers[k] = v, an assignment X.Servers = v where v is not an
// empty composite literal, and a composite literal with a Servers field set
// to a map literal that has an element.
func serverWritesIn(fset *token.FileSet, node ast.Node) []string {
	var places []string
	ast.Inspect(node, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				if idx, ok := lhs.(*ast.IndexExpr); ok && isServersSelector(idx.X) {
					places = append(places, fset.Position(v.Pos()).String())
					continue
				}
				if !isServersSelector(lhs) || len(v.Rhs) != len(v.Lhs) {
					continue
				}
				if lit, ok := v.Rhs[i].(*ast.CompositeLit); ok && len(lit.Elts) == 0 {
					continue
				}
				places = append(places, fset.Position(v.Pos()).String())
			}
		case *ast.CompositeLit:
			for _, e := range v.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Servers" {
					if lit, ok := kv.Value.(*ast.CompositeLit); ok && len(lit.Elts) > 0 {
						places = append(places, fset.Position(kv.Pos()).String())
					}
				}
			}
		}
		return true
	})
	return places
}

// TestEveryServerEntryWriteHasAReviewedRow asserts every function of the
// repository that writes a Servers entry has a row in serverWriteRows, that a
// row matches a write, and that a function the table calls creating refuses a
// new name: it calls config.CheckNewEntryName, or calls the function its row
// names, which does. The scan reads every non-test Go file outside a testdata
// directory. It does not see an alias (s := cfg.Servers; s[n] = ...),
// maps.Copy into cfg.Servers, a write to a map passed as a parameter, a YAML
// decode that creates keys, a hand-edited config.yaml, or a call through a
// function value; the tests of the creating functions cover those.
func TestEveryServerEntryWriteHasAReviewedRow(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	type fileFunc struct{ file, function string }
	writes := map[fileFunc][]string{}
	decls := map[fileFunc]*ast.FuncDecl{}
	for i, f := range tree.files {
		path := tree.paths[i]
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				if places := serverWritesIn(tree.fset, decl); len(places) > 0 {
					writes[fileFunc{path, packageLevelFunction}] = append(writes[fileFunc{path, packageLevelFunction}], places...)
				}
				continue
			}
			decls[fileFunc{path, functionKey(fd)}] = fd
			if fd.Body == nil {
				continue
			}
			if places := serverWritesIn(tree.fset, fd.Body); len(places) > 0 {
				writes[fileFunc{path, functionKey(fd)}] = places
			}
		}
	}

	rows := map[fileFunc]serverWriteRow{}
	for _, row := range serverWriteRows {
		key := fileFunc{row.file, row.function}
		_, duplicate := rows[key]
		assert.False(t, duplicate, "%s %s has two rows", row.file, row.function)
		rows[key] = row
		assert.Contains(t, []string{"creates", "updates", "test support"}, row.kind, "%s %s", row.file, row.function)
		assert.NotEmpty(t, row.reason, "%s %s needs a reason", row.file, row.function)
	}

	var unknown []string
	for key, places := range writes {
		if _, ok := rows[key]; !ok {
			unknown = append(unknown, fmt.Sprintf("%s in %s writes a server entry at %s", key.function, key.file, strings.Join(places, ", ")))
		}
	}
	slices.Sort(unknown)
	assert.Empty(t, unknown, "a function that writes a server entry needs a row in serverWriteRows, added after deciding whether it creates an entry: a creating function must call config.CheckNewEntryName")

	for key, row := range rows {
		if _, ok := writes[key]; !ok {
			t.Errorf("the row for %s in %s matches no write of a server entry", row.function, row.file)
			continue
		}
		if row.kind != "creates" {
			continue
		}
		fd := decls[key]
		require.NotNil(t, fd, "%s in %s", row.function, row.file)
		if callsFunction(fd.Body, "CheckNewEntryName") {
			continue
		}
		if row.checkedBy == "" {
			t.Errorf("%s in %s creates a server entry and does not call CheckNewEntryName", row.function, row.file)
			continue
		}
		checker := decls[fileFunc{row.file, row.checkedBy}]
		require.NotNil(t, checker, "%s in %s names %s as its check", row.function, row.file, row.checkedBy)
		assert.True(t, callsFunction(fd.Body, row.checkedBy), "%s in %s does not call %s, which refuses the name", row.function, row.file, row.checkedBy)
		assert.True(t, callsFunction(checker.Body, "CheckNewEntryName"), "%s in %s does not call CheckNewEntryName", row.checkedBy, row.file)
	}
}

// TestEveryCallOfAStepThatCreatesAnEntryHasAnEarlyCheck asserts every
// function outside tests that calls register.CommitCreateUnderLock or
// config.PersistLoginCredential has a row in creatorCallRows, that a row
// matches a call, and that the function the row names as making the early
// check, which refuses a new name before any request, calls CheckNewEntryName
// or EntryNameProblem. The scan does not see a call through a function value,
// and it does not follow calls: that a calling function is reached only after
// its early check is the row's reviewed claim, which the behavior tests of
// config add-server, auth login and the exec plugin cover.
func TestEveryCallOfAStepThatCreatesAnEntryHasAnEarlyCheck(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	creators := []string{"CommitCreateUnderLock", "PersistLoginCredential"}
	type fileFunction struct{ file, function string }
	type functionCall struct{ file, function, callee string }
	calls := map[functionCall]bool{}
	checks := map[fileFunction]bool{}
	for i, f := range tree.files {
		path := tree.paths[i]
		for _, decl := range f.Decls {
			function := packageLevelFunction
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if slices.Contains(creators, fd.Name.Name) {
					continue
				}
				function = functionKey(fd)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch name := calleeName(call); {
				case slices.Contains(creators, name):
					calls[functionCall{path, function, name}] = true
				case name == "CheckNewEntryName" || name == "EntryNameProblem":
					checks[fileFunction{path, function}] = true
				}
				return true
			})
		}
	}

	rows := map[functionCall]creatorCallRow{}
	for _, row := range creatorCallRows {
		rows[functionCall{row.file, row.function, row.calls}] = row
	}
	var unknown []string
	for key := range calls {
		if _, ok := rows[key]; !ok {
			unknown = append(unknown, key.function+" in "+key.file+" calls "+key.callee)
		}
	}
	slices.Sort(unknown)
	assert.Empty(t, unknown, "a function that calls a step that creates a server entry needs a row in creatorCallRows, naming the function that refuses a new name before the call")
	for key, row := range rows {
		assert.True(t, calls[key], "the row for %s in %s calling %s matches no call", row.function, row.file, row.calls)
		assert.True(t, checks[fileFunction{row.earlyCheckFile, row.earlyCheckFunction}], "%s in %s calls %s, and %s in %s does not call CheckNewEntryName or EntryNameProblem",
			row.function, row.file, row.calls, row.earlyCheckFunction, row.earlyCheckFile)
	}
}
