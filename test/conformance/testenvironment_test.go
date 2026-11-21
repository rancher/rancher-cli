package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Git sets GIT_DIR, GIT_INDEX_FILE and other GIT_* variables for a hook, and
// the pre-commit hook runs the tests. A test that runs git in a temporary
// directory with those variables acts on the repository that runs the hook.
// Two rules keep the variables out of every process a test starts:
//
//	environment-read: no test file reads the whole environment (os.Environ,
//	  syscall.Environ or the Environ method of exec.Cmd); testfs.EnvWithoutGit
//	  is the one way.
//	process-start: every process start in a test file sets Env on its command
//	  with a statement that comes after the statement that creates the command
//	  and before its first use, in the same block as that statement (not
//	  inside an if, switch, loop or closure), or passes the command to a
//	  function of the package that sets Env on that parameter. The value
//	  assigned is not nil and not built from the command's own Env; the last
//	  assignment before the first use decides; a command created again needs
//	  its own assignment.
//
// Because the first rule forbids every other read of the whole environment,
// an Env that a test assigns is built from testfs.EnvWithoutGit or is a list
// written in the test.
//
// A scan of the source cannot follow every flow of control. These shapes get
// past it, and a test has no reason to write them: an environment passed in
// as a parameter; a variable name built at run time and read with os.Getenv;
// a nil assigned inside an if or a closure after a good assignment; a helper
// function that sets Env and then clears it, or returns before it sets it; a
// command held in a field more than one level deep (s.a.cmd), whose owner's
// methods are not counted as a use.
const (
	envRuleRead  = "environment-read"
	envRuleStart = "process-start"
)

// testEnvSite is one place that breaks a rule, in the top-level declaration
// named function ("(package level)" outside any function).
type testEnvSite struct {
	rule     string
	function string
	line     int
	what     string
}

// testEnvRow lists how many sites of one rule a function of a file holds.
type testEnvRow struct {
	path     string
	function string
	rule     string
	count    int
	reason   string
}

type testEnvKey struct {
	path, function, rule string
}

const packageLevel = "(package level)"

// testEnvImports holds the names a file imports os/exec, os and syscall
// under; an empty name means the file does not import the package.
type testEnvImports struct {
	exec, os, syscall string
}

func testEnvImportNames(file *ast.File) (testEnvImports, error) {
	var names testEnvImports
	for _, imp := range file.Imports {
		var target *string
		switch imp.Path.Value {
		case `"os/exec"`:
			target = &names.exec
		case `"os"`:
			target = &names.os
		case `"syscall"`:
			target = &names.syscall
		default:
			continue
		}
		*target = path.Base(strings.Trim(imp.Path.Value, `"`))
		if imp.Name != nil {
			*target = imp.Name.Name
		}
		if *target == "." {
			return names, fmt.Errorf("imports %s with a dot; the scan cannot find the processes it starts", imp.Path.Value)
		}
	}
	return names, nil
}

func testEnvSelector(e ast.Expr, pkg string, names ...string) (*ast.SelectorExpr, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || pkg == "" {
		return nil, false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != pkg || !slices.Contains(names, sel.Sel.Name) {
		return nil, false
	}
	return sel, true
}

// testEnvSetFuncs returns the functions of one package that set Env on one of
// their own parameters, with the index of that parameter. The assignment is a
// statement of the function's body (not inside an if, a loop or a closure)
// with a value that is neither nil nor built from the parameter's own Env: the
// same rule a start follows. A method is not listed, because a call matches
// only by a plain function name.
func testEnvSetFuncs(files []*ast.File) map[string]int {
	setFuncs := map[string]int{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			var params []string
			for _, field := range fn.Type.Params.List {
				if len(field.Names) == 0 {
					params = append(params, "_")
				}
				for _, name := range field.Names {
					params = append(params, name.Name)
				}
			}
			for _, stmt := range fn.Body.List {
				for index, param := range params {
					if testEnvEnvStatement(stmt, param, nil) == envSet {
						setFuncs[fn.Name.Name] = index
					}
				}
			}
		}
	}
	return setFuncs
}

// testEnvDeclName names the top-level declaration the allowlist rows use.
func testEnvDeclName(decl ast.Decl) string {
	if fn, ok := decl.(*ast.FuncDecl); ok {
		return fn.Name.Name
	}
	return packageLevel
}

// testEnvHasEnvKey reports whether the composite literal sets the field.
func testEnvHasEnvKey(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Env" {
				return !testEnvIsNil(kv.Value)
			}
		}
	}
	return false
}

// testEnvSites returns every site of one file that breaks a rule.
func testEnvSites(fset *token.FileSet, file *ast.File, setFuncs map[string]int) ([]testEnvSite, error) {
	imp, err := testEnvImportNames(file)
	if err != nil {
		return nil, err
	}
	var sites []testEnvSite
	for _, decl := range file.Decls {
		function := testEnvDeclName(decl)
		callees := map[*ast.SelectorExpr]bool{}
		var stack []ast.Node
		add := func(rule string, pos token.Pos, what string) {
			sites = append(sites, testEnvSite{rule: rule, function: function, line: fset.Position(pos).Line, what: what})
		}
		// startNeedsEnv records a process start whose command is the value
		// at stack[i]; satisfied tells whether the start sets Env itself.
		startNeedsEnv := func(i int, pos token.Pos, what string, satisfied bool) {
			if satisfied {
				return
			}
			bound := testEnvBinding(stack, i, setFuncs)
			if !bound.passed && (bound.key == "" || !testEnvEnvFollows(bound, setFuncs)) {
				add(envRuleStart, pos, what)
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			i := len(stack) - 1
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := testEnvSelector(n.Fun, imp.exec, "Command", "CommandContext"); ok {
					callees[sel] = true
					startNeedsEnv(i, n.Pos(), "exec."+sel.Sel.Name, false)
				}
				if sel, ok := testEnvSelector(n.Fun, imp.os, "StartProcess"); ok {
					callees[sel] = true
					startNeedsEnv(i, n.Pos(), "os.StartProcess", testEnvAttrHasEnv(n, 2))
				}
				if sel, ok := testEnvSelector(n.Fun, imp.syscall, "ForkExec", "StartProcess"); ok {
					callees[sel] = true
					startNeedsEnv(i, n.Pos(), "syscall."+sel.Sel.Name, testEnvAttrHasEnv(n, 2))
				}
				if sel, ok := testEnvSelector(n.Fun, imp.syscall, "Exec"); ok {
					callees[sel] = true
					startNeedsEnv(i, n.Pos(), "syscall.Exec", len(n.Args) == 3 && !testEnvIsNil(n.Args[2]))
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "Environ" {
					add(envRuleRead, n.Pos(), "Environ")
				}
				if callees[n] {
					break
				}
				if _, ok := testEnvSelector(n, imp.exec, "Command", "CommandContext"); ok {
					add(envRuleStart, n.Pos(), "exec."+n.Sel.Name+" used as a value")
				}
				if _, ok := testEnvSelector(n, imp.os, "StartProcess"); ok {
					add(envRuleStart, n.Pos(), "os.StartProcess used as a value")
				}
				if _, ok := testEnvSelector(n, imp.syscall, "Exec", "ForkExec", "StartProcess"); ok {
					add(envRuleStart, n.Pos(), "syscall."+n.Sel.Name+" used as a value")
				}
				if _, ok := testEnvSelector(n, imp.exec, "Cmd"); ok {
					testEnvCmdType(stack, i, n, startNeedsEnv)
				}
			}
			return true
		})
	}
	return sites, nil
}

// testEnvCmdType handles a use of the exec.Cmd type that is not a pointer
// type: a composite literal or a variable of the type holds a command.
func testEnvCmdType(stack []ast.Node, i int, sel *ast.SelectorExpr, startNeedsEnv func(int, token.Pos, string, bool)) {
	if i > 0 {
		if _, ok := stack[i-1].(*ast.StarExpr); ok {
			return
		}
	}
	if i > 0 {
		if lit, ok := stack[i-1].(*ast.CompositeLit); ok && lit.Type == ast.Expr(sel) {
			startNeedsEnv(i-1, sel.Pos(), "exec.Cmd literal", testEnvHasEnvKey(lit))
			return
		}
	}
	startNeedsEnv(i, sel.Pos(), "exec.Cmd value", false)
}

func testEnvIsNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// testEnvAttrHasEnv reports whether argument index of the call is a literal
// process attribute list that sets Env, with or without an address-of.
func testEnvAttrHasEnv(call *ast.CallExpr, index int) bool {
	if index >= len(call.Args) {
		return false
	}
	e := call.Args[index]
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	return ok && testEnvHasEnvKey(lit)
}

// testEnvBound says what holds a command: the variable or field the start is
// assigned to (key) and the statement list that holds that statement (list,
// with the statement at position index), or that the start is an argument of
// a function that sets Env (passed). An empty key with passed false means
// nothing holds the command, or the statement that creates it is not in a
// statement list (the initializer of an if or for, a package-level variable).
type testEnvBound struct {
	key    string
	passed bool
	list   []ast.Stmt
	index  int
}

// testEnvBinding finds what holds the command value at stack[i].
func testEnvBinding(stack []ast.Node, i int, setFuncs map[string]int) testEnvBound {
	for i > 0 {
		switch p := stack[i-1].(type) {
		case *ast.ParenExpr:
			i--
			continue
		case *ast.UnaryExpr:
			i--
			continue
		case *ast.CompositeLit:
			if p.Type != nil && p.Type == stack[i] {
				i--
				continue
			}
		}
		break
	}
	if i == 0 {
		return testEnvBound{}
	}
	node := stack[i]
	switch p := stack[i-1].(type) {
	case *ast.AssignStmt:
		if len(p.Lhs) == len(p.Rhs) {
			for j, rhs := range p.Rhs {
				if ast.Node(rhs) == node {
					return testEnvHolder(stack, i-1, types.ExprString(p.Lhs[j]))
				}
			}
		}
	case *ast.ValueSpec:
		for j, v := range p.Values {
			if ast.Node(v) == node && j < len(p.Names) {
				return testEnvHolder(stack, i-3, p.Names[j].Name)
			}
		}
		if p.Type != nil && ast.Node(p.Type) == node && len(p.Names) > 0 {
			return testEnvHolder(stack, i-3, p.Names[0].Name)
		}
	case *ast.CallExpr:
		index, ok := testEnvSetFuncIndex(p, setFuncs)
		if ok && index < len(p.Args) && ast.Node(p.Args[index]) == node {
			return testEnvBound{passed: true}
		}
	}
	return testEnvBound{}
}

// testEnvHolder finds the statement list that holds the statement at
// stack[at] (an assignment, or the declaration statement of a var).
func testEnvHolder(stack []ast.Node, at int, key string) testEnvBound {
	if at < 1 {
		return testEnvBound{}
	}
	var list []ast.Stmt
	switch parent := stack[at-1].(type) {
	case *ast.BlockStmt:
		list = parent.List
	case *ast.CaseClause:
		list = parent.Body
	case *ast.CommClause:
		list = parent.Body
	default:
		return testEnvBound{}
	}
	for j, stmt := range list {
		if ast.Node(stmt) == stack[at] {
			return testEnvBound{key: key, list: list, index: j}
		}
	}
	return testEnvBound{}
}

// What a statement does to the Env of a command.
const (
	envNone  = iota // nothing
	envSet          // assigns a value that is neither nil nor built from the command's own Env
	envKeep         // assigns a value built from the command's own Env, which leaves it as it was
	envClear        // assigns nil
)

// testEnvEnvFollows reports whether Env is set on the command when it is first
// used. It reads the statements after the one that creates the command, in the
// same statement list, up to the first use of the command or the next time it
// is created; the last assignment among them decides.
func testEnvEnvFollows(bound testEnvBound, setFuncs map[string]int) bool {
	set := false
	for j := bound.index + 1; j < len(bound.list); j++ {
		stmt := bound.list[j]
		switch testEnvEnvStatement(stmt, bound.key, setFuncs) {
		case envSet:
			set = true
			continue
		case envKeep:
			continue
		case envClear:
			set = false
			continue
		}
		if testEnvUsesCommand(stmt, bound.key) || testEnvRebinds(stmt, bound.key) {
			break
		}
	}
	return set
}

// testEnvEnvStatement classifies a statement that assigns the Env of key, or
// passes key to the parameter of a function that does.
func testEnvEnvStatement(stmt ast.Stmt, key string, setFuncs map[string]int) int {
	switch stmt := stmt.(type) {
	case *ast.AssignStmt:
		if stmt.Tok != token.ASSIGN {
			return envNone
		}
		for j, lhs := range stmt.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Env" || types.ExprString(sel.X) != key {
				continue
			}
			if len(stmt.Lhs) != len(stmt.Rhs) {
				return envSet
			}
			switch rhs := stmt.Rhs[j]; {
			case testEnvIsNil(rhs):
				return envClear
			case testEnvMentions(rhs, key+".Env"):
				return envKeep
			}
			return envSet
		}
	case *ast.ExprStmt:
		call, ok := stmt.X.(*ast.CallExpr)
		if !ok {
			return envNone
		}
		index, ok := testEnvSetFuncIndex(call, setFuncs)
		if !ok || index >= len(call.Args) {
			return envNone
		}
		arg := call.Args[index]
		if u, ok := arg.(*ast.UnaryExpr); ok {
			arg = u.X
		}
		if types.ExprString(arg) == key {
			return envSet
		}
	}
	return envNone
}

// testEnvMentions reports whether the expression contains a part that is
// written as text (for example cmd.Env).
func testEnvMentions(e ast.Expr, text string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if x, ok := n.(ast.Expr); ok && types.ExprString(x) == text {
			found = true
		}
		return !found
	})
	return found
}

// testEnvSetFuncIndex returns the index of the argument a call passes to the
// parameter that a function of the package sets Env on. Only a call of a
// function by its plain name matches: a selector may name another package's
// function or a method.
func testEnvSetFuncIndex(call *ast.CallExpr, setFuncs map[string]int) (int, bool) {
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return 0, false
	}
	index, ok := setFuncs[id.Name]
	return index, ok
}

// testEnvCommandFields are the members of exec.Cmd a test sets or reads while
// it prepares a command; touching one does not hand the command to anything.
var testEnvCommandFields = []string{
	"Path", "Args", "Env", "Dir", "Stdin", "Stdout", "Stderr", "ExtraFiles",
	"SysProcAttr", "Cancel", "WaitDelay", "Process", "ProcessState", "StdinPipe", "StdoutPipe", "StderrPipe", "String",
}

// testEnvUsesCommand reports whether the statement does anything with the
// command beyond preparing it: running it, passing it on, returning it,
// sending it, storing it, taking a method value, or (for a command held in a
// field) calling anything on the value that owns the field. Setting or
// reading one of its fields, and creating it again, are not uses.
func testEnvUsesCommand(stmt ast.Stmt, key string) bool {
	root, field, dotted := strings.Cut(key, ".")
	dotted = dotted && !strings.Contains(field, ".")
	used := false
	var stack []ast.Node
	ast.Inspect(stmt, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		expr, ok := n.(ast.Expr)
		if !ok || used {
			return !used
		}
		var parent ast.Node
		if len(stack) > 1 {
			parent = stack[len(stack)-2]
		}
		if sel, ok := parent.(*ast.SelectorExpr); ok && sel.Sel == n {
			return true // the name of a member, not a use of a variable of that name
		}
		if kv, ok := parent.(*ast.KeyValueExpr); ok && kv.Key == n && !testEnvInMapLiteral(stack) {
			return true // the name of a field in a struct literal
		}
		switch name := types.ExprString(expr); {
		case name == key:
			used = !testEnvPreparesCommand(parent, expr)
		case dotted && name == root:
			sel, ok := parent.(*ast.SelectorExpr)
			used = !ok || sel.X != expr || sel.Sel.Name != field
		}
		return !used
	})
	return used
}

// testEnvInMapLiteral reports whether the key-value pair at the top of the
// stack belongs to a map literal, whose key is a value and can be the command.
func testEnvInMapLiteral(stack []ast.Node) bool {
	if len(stack) < 3 {
		return false
	}
	lit, ok := stack[len(stack)-3].(*ast.CompositeLit)
	if !ok {
		return false
	}
	_, isMap := lit.Type.(*ast.MapType)
	return isMap
}

// testEnvPreparesCommand reports whether the expression at parent is only the
// receiver of a field of the command or the target of an assignment.
func testEnvPreparesCommand(parent ast.Node, expr ast.Expr) bool {
	switch p := parent.(type) {
	case *ast.SelectorExpr:
		return p.X == expr && slices.Contains(testEnvCommandFields, p.Sel.Name)
	case *ast.AssignStmt:
		return slices.Contains(p.Lhs, expr)
	}
	return false
}

// testEnvRebinds reports whether the statement assigns a new value to key.
func testEnvRebinds(stmt ast.Stmt, key string) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok {
		return false
	}
	return slices.ContainsFunc(assign.Lhs, func(lhs ast.Expr) bool { return types.ExprString(lhs) == key })
}

// testEnvFileSites parses src and returns its sites.
// With no functions that set Env given, it reads them from the file itself.
func testEnvFileSites(file, src string, setFuncs map[string]int) ([]testEnvSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	if setFuncs == nil {
		setFuncs = testEnvSetFuncs([]*ast.File{f})
	}
	return testEnvSites(fset, f, setFuncs)
}

// testEnvProblems compares the sites with the allowlist: a site with no row,
// a count that differs from its row in either direction, and a row that is
// malformed, duplicated or names a missing file.
func testEnvProblems(found map[testEnvKey][]testEnvSite, rows []testEnvRow, exists func(string) bool) []string {
	var problems []string
	seen := map[testEnvKey]bool{}
	for _, r := range rows {
		key := testEnvKey{r.path, r.function, r.rule}
		switch {
		case seen[key]:
			problems = append(problems, fmt.Sprintf("%s: two rows for %s in %s", r.path, r.rule, r.function))
			continue
		case strings.TrimSpace(r.reason) == "":
			problems = append(problems, fmt.Sprintf("%s: the row for %s in %s has no reason", r.path, r.rule, r.function))
		case r.rule != envRuleRead && r.rule != envRuleStart:
			problems = append(problems, fmt.Sprintf("%s: the row for %s in %s names the unknown rule %q", r.path, r.rule, r.function, r.rule))
		case !exists(r.path):
			problems = append(problems, fmt.Sprintf("%s: the file of the row for %s in %s no longer exists", r.path, r.rule, r.function))
		case r.count < 1:
			problems = append(problems, fmt.Sprintf("%s: the row for %s in %s lists no site; delete it", r.path, r.rule, r.function))
		}
		seen[key] = true
		got := len(found[key])
		switch {
		case got > r.count:
			problems = append(problems, fmt.Sprintf("%s: %d sites of %s in %s and its row in testEnvAllowlist (test/conformance/testenvironment_test.go) allows %d; %s", r.path, got, r.rule, r.function, r.count, testEnvAdvice[r.rule]))
		case got < r.count:
			problems = append(problems, fmt.Sprintf("%s: %d sites of %s in %s and its row in testEnvAllowlist (test/conformance/testenvironment_test.go) lists %d; lower or delete the row", r.path, got, r.rule, r.function, r.count))
		}
	}
	for key, sites := range found {
		if seen[key] {
			continue
		}
		for _, s := range sites {
			problems = append(problems, fmt.Sprintf("%s:%d: %s in %s: %s", key.path, s.line, s.what, s.function, testEnvAdvice[s.rule]))
		}
	}
	slices.Sort(problems)
	return problems
}

var testEnvAdvice = map[string]string{
	envRuleRead:  "build the environment of a process a test starts from testfs.EnvWithoutGit; GIT_* variables from a hook then never reach it",
	envRuleStart: "set cmd.Env = testfs.EnvWithoutGit() (or append to it) on the command before it starts, or pass it to a function of the package that sets Env",
}

// testEnvScanned reports whether the scan reads a file: every _test.go file
// the repository lists outside testdata, and the other Go files of the
// test-only packages (internal/testfs and every package of internal whose
// name ends in test), which tests call and which start processes for them.
func testEnvScanned(file string) bool {
	if !strings.HasSuffix(file, ".go") || inTestdata(file) {
		return false
	}
	if strings.HasSuffix(file, "_test.go") {
		return true
	}
	dir := path.Dir(file)
	base := path.Base(dir)
	return strings.HasPrefix(dir, "internal/") && (base == "testfs" || strings.HasSuffix(base, "test"))
}

func testEnvFound(t *testing.T) map[testEnvKey][]testEnvSite {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	parsed := map[string]*ast.File{}
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	for _, f := range files {
		if !testEnvScanned(f) {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		pf, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		require.NoError(t, err, f)
		parsed[f] = pf
		byDir[path.Dir(f)] = append(byDir[path.Dir(f)], pf)
	}
	found := map[testEnvKey][]testEnvSite{}
	for f, pf := range parsed {
		sites, err := testEnvSites(fset, pf, testEnvSetFuncs(byDir[path.Dir(f)]))
		require.NoError(t, err, f)
		for _, s := range sites {
			key := testEnvKey{f, s.function, s.rule}
			found[key] = append(found[key], s)
		}
	}
	return found
}

// TestTestsKeepGitVariablesOutOfProcesses asserts the two rules above over
// every file testEnvScanned accepts, tagged files included. Every site
// that remains is a row of testEnvAllowlist with its exact count: a new site
// fails, and so does a row that is too high or names a site that is gone.
func TestTestsKeepGitVariablesOutOfProcesses(t *testing.T) {
	t.Parallel()
	problems := testEnvProblems(testEnvFound(t), testEnvAllowlist, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	assert.Empty(t, problems)
}

// testEnvAllowlist holds a site that must keep the whole environment. A row
// names the site and says why.
var testEnvAllowlist = []testEnvRow{
	{
		path:     "internal/testfs/envwithoutgit.go",
		function: packageLevel,
		rule:     envRuleRead,
		count:    1,
		reason:   "the one read of the whole environment, which testfs.EnvWithoutGit filters",
	},
}

func testEnvSnippetSites(t *testing.T, src string) []testEnvSite {
	t.Helper()
	sites, err := testEnvFileSites("x/x_test.go", src, nil)
	require.NoError(t, err)
	return sites
}

// TestTestEnvironmentScanFixtures asserts what each rule accepts and refuses
// from source snippets. Mutation: drop a start form, or accept an Env set on
// another variable.
func TestTestEnvironmentScanFixtures(t *testing.T) {
	t.Parallel()
	const setFuncSnippets = `
func setEnv(cmd *exec.Cmd) { cmd.Env = envList() }
func withEnv(cmd *exec.Cmd) *exec.Cmd { cmd.Env = envList(); return cmd }
func condEnv(cmd *exec.Cmd, ok bool) { if ok { cmd.Env = envList() } }
func nilEnv(cmd *exec.Cmd) { cmd.Env = nil }
func ownEnv(cmd *exec.Cmd, x []string) { cmd.Env = append(cmd.Env, x...) }
func second(a, b *exec.Cmd) { b.Env = envList() }
`
	const head = "package p\nimport (\n\"os\"\n\"os/exec\"\n\"syscall\"\n\"context\"\n)\nvar _ = os.Args\nvar _ = syscall.Getpid\nvar _ = context.Background\nvar _ = exec.ErrNotFound\n" + setFuncSnippets
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"command with Env", `func f() { cmd := exec.Command("git", "init"); cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"command without Env", `func f() { cmd := exec.Command("git", "init"); _ = cmd.Run() }`, []string{envRuleStart}},
		{"chained command", `func f() { _ = exec.Command("git", "init").Run() }`, []string{envRuleStart}},
		{"chained command output", `func f() { _, _ = exec.Command("git", "ls-files").Output() }`, []string{envRuleStart}},
		{"context command with Env", `func f() { cmd := exec.CommandContext(context.Background(), "sh"); cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"context command without Env", `func f() { cmd := exec.CommandContext(context.Background(), "sh"); _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env on another variable", `func f() { a := exec.Command("sh"); b := exec.Command("sh"); b.Env = envList(); _ = a.Run(); _ = b.Run() }`, []string{envRuleStart}},
		{"two commands with Env", `func f() { a := exec.Command("sh"); b := exec.Command("sh"); a.Env = envList(); b.Env = envList() }`, nil},
		{"field command with Env", `func f(s *S) { s.cmd = exec.Command("sh"); s.cmd.Env = envList() }`, nil},
		{"var command with Env", `func f() { var cmd = exec.Command("sh"); cmd.Env = envList() }`, nil},
		{"command passed to a function that sets Env", `func f() { cmd := exec.Command("sh"); setEnv(cmd) }`, nil},
		{"command as an argument of a function that sets Env", `func f() { withEnv(exec.Command("sh")) }`, nil},
		{"command passed to another function", `func f() { cmd := exec.Command("sh"); other(cmd) }`, []string{envRuleStart}},
		{"command as an argument of another function", `func f() { other(exec.Command("sh")) }`, []string{envRuleStart}},
		{"command returned", `func f() *exec.Cmd { return exec.Command("sh") }`, []string{envRuleStart}},
		{"command in a table", `var rows = []*exec.Cmd{exec.Command("sh")}`, []string{envRuleStart}},
		{"command function value", `var run = exec.Command`, []string{envRuleStart}},
		{"Env set in a closure", `func f() { cmd := exec.Command("sh"); func() { cmd.Env = envList() }() }`, []string{envRuleStart}},
		{"Env set to nil", `func f() { cmd := exec.Command("git", "init"); cmd.Env = nil; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env set in an if", `func f(ok bool) { cmd := exec.Command("git", "init"); if ok { cmd.Env = envList() }; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env set in a switch case", `func f(a int) { cmd := exec.Command("git", "init"); switch a { case 1: cmd.Env = envList() }; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env set to its own value", `func f() { cmd := exec.Command("git", "init"); cmd.Env = cmd.Env; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env set after the command runs", `func f() { cmd := exec.Command("git", "init"); _ = cmd.Run(); cmd.Env = envList() }`, []string{envRuleStart}},
		{"Env set after the command is passed on", `func f() { cmd := exec.Command("git", "init"); other(cmd); cmd.Env = envList() }`, []string{envRuleStart}},
		{"Env set on a variable of the same name in a closure", `func f() { cmd := exec.Command("git", "init"); func() { cmd := &exec.Cmd{}; cmd.Env = envList() }(); _ = cmd.Run() }`, []string{envRuleStart}},
		{"command created again after its Env", `func f() { cmd := exec.Command("true"); cmd.Env = envList(); cmd = exec.Command("git", "init"); _ = cmd.Run() }`, []string{envRuleStart}},
		{"command created again with its own Env", `func f() { cmd := exec.Command("true"); cmd.Env = envList(); cmd = exec.Command("git", "init"); cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"Env set in an if inside a loop", `func f(args []string) { var cmd *exec.Cmd; for _, a := range args { cmd = exec.Command("git", a); if a == "init" { cmd.Env = envList() }; _ = cmd.Run() } }`, []string{envRuleStart}},
		{"Env set in a loop body", `func f(args []string) { var cmd *exec.Cmd; for _, a := range args { cmd = exec.Command("git", a); cmd.Env = envList(); _ = cmd.Run() } }`, nil},
		{"Env set in a case clause", `func f(a int) { switch a { case 1: cmd := exec.Command("sh"); cmd.Env = envList(); _ = cmd.Run() } }`, nil},
		{"Env after other settings of the command", `func f() { cmd := exec.Command("sh"); cmd.Dir = "d"; cmd.Stdout = nil; cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"Env from a parameter, which the scan does not follow", `func f(env []string) { cmd := exec.Command("git", "init"); cmd.Env = env; _ = cmd.Run() }`, nil},
		{"literal with a nil Env", `func f() { cmd := &exec.Cmd{Path: "x", Env: nil}; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env built from its own Env and more", `func f(x []string) { cmd := exec.Command("git", "init"); cmd.Env = append(cmd.Env, x...); _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env cut from its own Env", `func f() { cmd := exec.Command("git", "init"); cmd.Env = cmd.Env[:0]; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env extended after a good assignment", `func f() { cmd := exec.Command("git", "init"); cmd.Env = envList(); cmd.Env = append(cmd.Env, "A=b"); _ = cmd.Run() }`, nil},
		{"Env cleared after a good assignment", `func f() { cmd := exec.Command("git", "init"); cmd.Env = envList(); cmd.Env = nil; _ = cmd.Run() }`, []string{envRuleStart}},
		{"Env set after being cleared", `func f() { cmd := exec.Command("git", "init"); cmd.Env = nil; cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"function that sets Env in an if", `func f() { cmd := exec.Command("sh"); condEnv(cmd, true); _ = cmd.Run() }`, []string{envRuleStart}},
		{"function that sets Env to nil", `func f() { cmd := exec.Command("sh"); nilEnv(cmd); _ = cmd.Run() }`, []string{envRuleStart}},
		{"function that builds Env from its own Env", `func f(x []string) { cmd := exec.Command("sh"); ownEnv(cmd, x); _ = cmd.Run() }`, []string{envRuleStart}},
		{"call of a method named like a function that sets Env", `func f(h holder) { cmd := exec.Command("sh"); h.setEnv(cmd); _ = cmd.Run() }`, []string{envRuleStart}},
		{"call of another package's function named like a function that sets Env", `func f() { cmd := exec.Command("sh"); other.setEnv(cmd); _ = cmd.Run() }`, []string{envRuleStart}},
		{"command at the argument that does not get Env", `func f(other *exec.Cmd) { cmd := exec.Command("sh"); second(cmd, other); _ = cmd.Run() }`, []string{envRuleStart}},
		{"command at the argument that gets Env", `func f(other *exec.Cmd) { cmd := exec.Command("sh"); second(other, cmd); _ = cmd.Run() }`, nil},
		{"method value taken before Env", `func f() { cmd := exec.Command("sh"); run := cmd.Run; run(); cmd.Env = envList() }`, []string{envRuleStart}},
		{"channel send before Env", `func f(ch chan *exec.Cmd) { cmd := exec.Command("sh"); ch <- cmd; cmd.Env = envList() }`, []string{envRuleStart}},
		{"struct literal before Env", `func f() { cmd := exec.Command("sh"); h := &holder{cmd: cmd}; h.run(); cmd.Env = envList() }`, []string{envRuleStart}},
		{"field assigned from the command before Env", `func f(s *S) { cmd := exec.Command("sh"); s.cmd = cmd; s.cmd.Run(); cmd.Env = envList() }`, []string{envRuleStart}},
		{"field assigned from the command, Env after", `func f(s *S) { cmd := exec.Command("sh"); s.cmd = cmd; cmd.Env = envList() }`, []string{envRuleStart}},
		{"method of the owner of a field command before Env", `func f(s *S) { s.cmd = exec.Command("sh"); s.run(); s.cmd.Env = envList() }`, []string{envRuleStart}},
		{"field command prepared before Env", `func f(s *S) { s.cmd = exec.Command("sh"); s.cmd.Dir = "d"; s.cmd.Env = envList(); s.run() }`, nil},
		{"command used through another name, then created again with Env", `func f() { a := exec.Command("git", "init"); b := a; _ = b.Run(); a = exec.Command("true"); a.Env = envList() }`, []string{envRuleStart}},
		{"command created again before it has Env", `func f() { a := exec.Command("git", "init"); a = exec.Command("true"); a.Env = envList(); _ = a.Run() }`, []string{envRuleStart}},
		{"command returned before Env", `func f(x bool) *exec.Cmd { cmd := exec.Command("sh"); if x { return cmd }; cmd.Env = envList(); return cmd }`, []string{envRuleStart}},
		{"Process read in the Cancel function of the command", `func f() { cmd := exec.Command("sh"); cmd.Cancel = func() error { return cmd.Process.Kill() }; cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"ProcessState read before Env", `func f() { cmd := exec.Command("sh"); _ = cmd.ProcessState; cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"struct literal field named like the command", `func f() { cmd := exec.Command("sh"); h := holder{cmd: nil}; _ = h; cmd.Env = envList(); _ = cmd.Run() }`, nil},
		{"command as the key of a map literal before Env", `func f() { cmd := exec.Command("sh"); m := map[*exec.Cmd]int{cmd: 1}; _ = m; cmd.Env = envList() }`, []string{envRuleStart}},
		{"command in the initializer of an if", `func f() { if err := exec.Command("sh").Run(); err != nil { return } }`, []string{envRuleStart}},
		{"literal with Env", `func f() { cmd := &exec.Cmd{Path: "x", Env: envList()}; _ = cmd.Run() }`, nil},
		{"literal without Env", `func f() { cmd := &exec.Cmd{Path: "x"}; _ = cmd.Run() }`, []string{envRuleStart}},
		{"literal value without Env", `func f() { cmd := exec.Cmd{Path: "x"}; _ = cmd.Run() }`, []string{envRuleStart}},
		{"literal without Env, set later", `func f() { cmd := &exec.Cmd{Path: "x"}; cmd.Env = envList() }`, nil},
		{"literal in a slice", `var rows = []exec.Cmd{{Path: "x"}}`, []string{envRuleStart}},
		{"variable of type exec.Cmd", `func f() { var cmd exec.Cmd; cmd.Env = envList() }`, nil},
		{"variable of type exec.Cmd without Env", `func f() { var cmd exec.Cmd; _ = cmd.Run() }`, []string{envRuleStart}},
		{"parameter of type *exec.Cmd", `func f(cmd *exec.Cmd) { _ = cmd.Run() }`, nil},
		{"os.StartProcess with Env", `func f() { _, _ = os.StartProcess("x", nil, &os.ProcAttr{Env: envList()}) }`, nil},
		{"os.StartProcess without Env", `func f() { _, _ = os.StartProcess("x", nil, &os.ProcAttr{}) }`, []string{envRuleStart}},
		{"os.StartProcess with no attributes", `func f() { _, _ = os.StartProcess("x", nil, nil) }`, []string{envRuleStart}},
		{"syscall.ForkExec with Env", `func f() { _, _ = syscall.ForkExec("x", nil, &syscall.ProcAttr{Env: envList()}) }`, nil},
		{"syscall.ForkExec without Env", `func f() { _, _ = syscall.ForkExec("x", nil, nil) }`, []string{envRuleStart}},
		{"syscall.Exec with an environment", `func f() { _ = syscall.Exec("x", nil, envList()) }`, nil},
		{"syscall.Exec with nil", `func f() { _ = syscall.Exec("x", nil, nil) }`, []string{envRuleStart}},
		{"os.Environ", `func f() []string { return os.Environ() }`, []string{envRuleRead}},
		{"os.Environ as a value", `var get = os.Environ`, []string{envRuleRead}},
		{"syscall.Environ", `func f() []string { return syscall.Environ() }`, []string{envRuleRead}},
		{"the Environ method", `func f(cmd *exec.Cmd) []string { return cmd.Environ() }`, []string{envRuleRead}},
		{"os.Environ appended to", `func f() { cmd := exec.Command("sh"); cmd.Env = append(os.Environ(), "A=b"); _ = cmd.Run() }`, []string{envRuleRead}},
		{"a variable read by name", `func f() string { return os.Getenv("HOME") }`, nil},
		{"a command with a literal environment", `func f() { cmd := exec.Command("sh"); cmd.Env = []string{"A=b"}; _ = cmd.Run() }`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, s := range testEnvSnippetSites(t, head+tc.body+"\n") {
				got = append(got, s.rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTestEnvironmentScanRefusesDotImports asserts a dot import of a package
// whose starts the scan looks for is an error, because the scan could not
// find them.
func TestTestEnvironmentScanRefusesDotImports(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{"os/exec", "os", "syscall"} {
		_, err := testEnvFileSites("x/x_test.go", "package p\nimport . \""+pkg+"\"\n", nil)
		require.ErrorContains(t, err, "imports \""+pkg+"\" with a dot")
	}
}

// TestTestEnvironmentScanFollowsImportNames asserts the scan resolves a
// package through the name the file imports it under.
func TestTestEnvironmentScanFollowsImportNames(t *testing.T) {
	t.Parallel()
	src := "package p\nimport (\n\tx \"os/exec\"\n\to \"os\"\n)\nfunc f() { _ = x.Command(\"sh\").Run(); _ = o.Environ() }\n"
	sites, err := testEnvFileSites("x/x_test.go", src, nil)
	require.NoError(t, err)
	var rules []string
	for _, s := range sites {
		rules = append(rules, s.rule)
	}
	assert.ElementsMatch(t, []string{envRuleStart, envRuleRead}, rules)
}

// TestTestEnvironmentScanAppliesBothRulesInTheHelperPackage asserts the files
// of internal/testfs follow both rules like any other file; the one read of
// the whole environment there has an allowlist row.
func TestTestEnvironmentScanAppliesBothRulesInTheHelperPackage(t *testing.T) {
	t.Parallel()
	src := "package p\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\nfunc f() { _ = os.Environ(); _ = exec.Command(\"sh\").Run() }\n"
	sites, err := testEnvFileSites("internal/testfs/x_test.go", src, nil)
	require.NoError(t, err)
	assert.Len(t, sites, 2)
}

// TestTestEnvironmentScannedFiles asserts which files the scan reads: test
// files and the other Go files of the test-only packages, never testdata or
// the files of another package.
func TestTestEnvironmentScannedFiles(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"internal/token/x_test.go":                       true,
		"test/conformance/x_test.go":                     true,
		"internal/testfs/testfs.go":                      true,
		"internal/testfs/envwithoutgit.go":               true,
		"internal/commands/cmdtest/fakehelper.go":        true,
		"internal/remedytest/x.go":                       true,
		"internal/commands/cmdtest/testdata/x.go":        false,
		"internal/token/token.go":                        false,
		"internal/testrancher/x.go":                      false,
		"cmd/rancher/main.go":                            false,
		"scripts/readme.md":                              false,
		"internal/commands/cmdtest/testdata/y/x_test.go": false,
		"docs/cmdtest/x.go":                              false,
	}
	for file, want := range cases {
		assert.Equal(t, want, testEnvScanned(file), file)
	}
}

// TestTestEnvironmentAllowlistTable asserts the allowlist comparison accepts
// an exact row and reports every problem of a refused case.
func TestTestEnvironmentAllowlistTable(t *testing.T) {
	t.Parallel()
	site := func(rule string, line int) testEnvSite {
		return testEnvSite{rule: rule, function: "f", line: line, what: "exec.Command"}
	}
	key := testEnvKey{"a/a_test.go", "f", envRuleStart}
	found := map[testEnvKey][]testEnvSite{key: {site(envRuleStart, 3), site(envRuleStart, 9)}}
	exists := func(string) bool { return true }
	row := func(count int, reason string) testEnvRow {
		return testEnvRow{path: "a/a_test.go", function: "f", rule: envRuleStart, count: count, reason: reason}
	}
	cases := []struct {
		name   string
		found  map[testEnvKey][]testEnvSite
		rows   []testEnvRow
		exists func(string) bool
		want   []string
	}{
		{name: "an exact row", found: found, rows: []testEnvRow{row(2, "reads the index")}},
		{name: "no row", found: found, want: []string{"a/a_test.go:3: exec.Command in f", "a/a_test.go:9: exec.Command in f"}},
		{name: "a row that is too low", found: found, rows: []testEnvRow{row(1, "r")}, want: []string{"2 sites of process-start in f and its row in testEnvAllowlist"}},
		{name: "a stale row, too high", found: found, rows: []testEnvRow{row(3, "r")}, want: []string{"lists 3; lower or delete the row"}},
		{name: "a stale row, no site", found: map[testEnvKey][]testEnvSite{}, rows: []testEnvRow{row(1, "r")}, want: []string{"0 sites of process-start in f", "lists 1"}},
		{name: "a row with no reason", found: found, rows: []testEnvRow{row(2, " ")}, want: []string{"has no reason"}},
		{name: "a row for a missing file", found: found, rows: []testEnvRow{row(2, "r")}, exists: func(string) bool { return false }, want: []string{"no longer exists"}},
		{name: "a row of an unknown rule", found: found, rows: []testEnvRow{{path: "a/a_test.go", function: "f", rule: "unknown-rule", count: 1, reason: "r"}}, want: []string{`unknown rule "unknown-rule"`}},
		{name: "two rows", found: found, rows: []testEnvRow{row(2, "r"), row(2, "r")}, want: []string{"two rows for process-start in f"}},
		{name: "a row with no count", found: map[testEnvKey][]testEnvSite{}, rows: []testEnvRow{row(0, "r")}, want: []string{"lists no site; delete it"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := c.exists
			if ex == nil {
				ex = exists
			}
			problems := strings.Join(testEnvProblems(c.found, c.rows, ex), "\n")
			if len(c.want) == 0 {
				assert.Empty(t, problems)
			}
			for _, w := range c.want {
				assert.Contains(t, problems, w)
			}
		})
	}
}
