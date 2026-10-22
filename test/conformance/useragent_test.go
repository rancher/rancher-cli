package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	invocationDir = "internal/invocation/"
	httpxPath     = "github.com/rancher/rancher-cli/internal/client/httpx"
)

// detectionAllow lists a use of a detection name outside internal/invocation
// that is allowed, the number of uses, and why. Every entry must match
// exactly that many uses.
type detectionAllow struct {
	path, name string
	uses       int
	reason     string
}

var detectionAllowlist = []detectionAllow{
	{"internal/token/directwait.go", "KUBERNETES_EXEC_INFO", 1, "reads the variable's value to find the cluster entry; Detect only tests that it is non-empty"},
}

// detectionFindings returns one message per use in f, a file at path, of a
// string literal that equals one of names in any letter case or of an
// environment-wide read: os.Environ, os.ExpandEnv, os.Expand, syscall.Getenv,
// syscall.Environ (called or taken as a value) and a call of a method named
// Environ with no arguments (exec.Cmd.Environ). A literal is compared
// whole: a message that holds a name as a word is not a use; a name built
// by concatenation is out of scope, which the environment-wide reads cover. allowed counts the uses an allowlist
// entry covers, by "path name".
func detectionFindings(fset *token.FileSet, path string, f *ast.File, names []string, allowed map[string]int) []string {
	if strings.HasPrefix(path, invocationDir) {
		return nil
	}
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = p
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			unquoted, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			// Windows compares variable names without regard to case:
			// "ai_agent" reads AI_AGENT there.
			i := slices.IndexFunc(names, func(name string) bool { return strings.EqualFold(name, unquoted) })
			if i < 0 {
				return true
			}
			value := names[i]
			if _, ok := allowed[path+" "+value]; ok {
				allowed[path+" "+value]++
				return true
			}
			out = append(out, fset.Position(x.Pos()).String()+": the literal "+x.Value+" names a variable that identifies the caller; internal/invocation is the one place that reads it")
		case *ast.CallExpr:
			// A call of a method named Environ with no arguments is the
			// whole environment of an exec.Cmd.
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Environ" || len(x.Args) != 0 {
				return true
			}
			if id, isIdent := sel.X.(*ast.Ident); isIdent && (imports[id.Name] == "os" || imports[id.Name] == "syscall") {
				return true // the selector case below reports it
			}
			out = append(out, fset.Position(x.Pos()).String()+": a call of Environ reads the whole environment; read a named variable, and only internal/invocation reads the ones that identify the caller")
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch imports[id.Name] + "." + x.Sel.Name {
			case "os.Environ", "os.ExpandEnv", "os.Expand", "syscall.Getenv", "syscall.Environ":
				out = append(out, fset.Position(x.Pos()).String()+": "+id.Name+"."+x.Sel.Name+" reads the whole environment; read a named variable, and only internal/invocation reads the ones that identify the caller")
			}
		}
		return true
	})
	return out
}

func detectionSnippet(t *testing.T, path, src string, names []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	require.NoError(t, err)
	return detectionFindings(fset, path, f, names, map[string]int{})
}

// TestDetectionScanFixtures asserts the scan's rules from source snippets.
// Mutation: drop the whole-environment case, or compare a literal by
// contains instead of equality.
func TestDetectionScanFixtures(t *testing.T) {
	t.Parallel()
	names := invocation.EnvNames()
	cases := []struct {
		name, path, src string
		reject          bool
	}{
		{"const CI", "internal/x/x.go", `package p; const ci = "CI"`, true},
		{"getenv AI_AGENT", "internal/x/x.go", `package p; import "os"; var _ = os.Getenv("AI_AGENT")`, true},
		{"raw string", "internal/x/x.go", "package p; const c = `CLAUDECODE`", true},
		{"exec info outside the allowlist", "internal/x/x.go", `package p; const c = "KUBERNETES_EXEC_INFO"`, true},
		{"os.Environ", "internal/x/x.go", `package p; import "os"; var _ = os.Environ()`, true},
		{"os.Environ as a value", "internal/x/x.go", `package p; import "os"; var f = os.Environ`, true},
		{"os.ExpandEnv", "internal/x/x.go", `package p; import "os"; var _ = os.ExpandEnv("$CI")`, true},
		{"os.Expand", "internal/x/x.go", `package p; import "os"; var _ = os.Expand("x", func(string) string { return "" })`, true},
		{"syscall.Getenv", "internal/x/x.go", `package p; import "syscall"; var _, _ = syscall.Getenv("X")`, true},
		{"syscall.Environ", "internal/x/x.go", `package p; import "syscall"; var _ = syscall.Environ()`, true},
		{"exec.Cmd.Environ", "internal/x/x.go", `package p; import "os/exec"; var _ = exec.Command("x").Environ()`, true},
		{"lower case name", "internal/x/x.go", `package p; import "os"; var _ = os.Getenv("ai_agent")`, true},
		{"mixed case name", "internal/x/x.go", `package p; const c = "Claudecode"`, true},
		{"aliased os", "internal/x/x.go", `package p; import o "os"; var _ = o.Environ()`, true},
		{"the package itself", "internal/invocation/invocation.go", `package p; import "os"; const c = "CI"; var _ = os.Environ()`, false},
		{"a message with the name as a word", "internal/x/x.go", `package p; const c = "set CI to a value"`, false},
		{"a longer name", "internal/x/x.go", `package p; const c = "CI_COMMIT"`, false},
		{"another variable", "internal/x/x.go", `package p; import "os"; var _ = os.Getenv("HOME")`, false},
		{"a local function named Environ", "internal/x/x.go", `package p; type e struct{}; func (e) Environ() {}; var x e; var _ = x.Environ`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := detectionSnippet(t, tc.path, tc.src, names)
			if tc.reject {
				assert.NotEmpty(t, got, "expected a violation")
			} else {
				assert.Empty(t, got, "expected no violation, got %v", got)
			}
		})
	}
}

// TestOnlyInvocationDetectsTheCaller asserts that no production code
// outside internal/invocation holds a string literal equal to a variable
// name Detect reads (the names come from invocation.EnvNames, which a test
// there keeps equal to the names Detect reads) or reads the whole
// environment, and that every allowlist entry matches its exact number of
// uses.
func TestOnlyInvocationDetectsTheCaller(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	names := invocation.EnvNames()
	allowed := map[string]int{}
	for _, entry := range detectionAllowlist {
		require.Contains(t, names, entry.name, "the allowlist entry for %s names no detection variable", entry.path)
		require.NotEmpty(t, entry.reason)
		allowed[entry.path+" "+entry.name] = 0
	}
	for i, f := range tree.files {
		for _, msg := range detectionFindings(tree.fset, tree.paths[i], f, names, allowed) {
			t.Error(msg)
		}
	}
	for _, entry := range detectionAllowlist {
		assert.Equal(t, entry.uses, allowed[entry.path+" "+entry.name], "allowlist entry %s %s: the number of uses changed; update the entry", entry.path, entry.name)
	}
}

// TestEveryDetectionNameIsInTheReference asserts the REFERENCE.md section on
// what the CLI sends to the server lists every variable Detect reads and
// every agent id, each as a code span.
func TestEveryDetectionNameIsInTheReference(t *testing.T) {
	t.Parallel()
	section := referenceSection(t, "What the CLI sends to the server")
	for _, name := range invocation.EnvNames() {
		assert.Contains(t, section, "`"+name+"`", "the reference section does not list the variable %s", name)
	}
	for _, id := range invocation.AgentIDs() {
		assert.Contains(t, section, "`"+id+"`", "the reference section does not list the agent id %s", id)
	}
	assert.Contains(t, section, "`"+invocation.AgentGeneric+"`")
}

// referenceSection returns the text of REFERENCE.md under the heading
// whose title is title, up to the next heading of the same or a higher
// level.
func referenceSection(t *testing.T, title string) string {
	t.Helper()
	src, err := os.ReadFile("REFERENCE.md")
	require.NoError(t, err)
	var out []string
	level := 0
	fenced := false
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "#") {
			hashes := len(line) - len(strings.TrimLeft(line, "#"))
			if level == 0 && strings.TrimSpace(line[hashes:]) == title {
				level = hashes
				continue
			}
			if level != 0 && hashes <= level {
				break
			}
		}
		if level != 0 {
			out = append(out, line)
		}
	}
	require.NotEmpty(t, out, "REFERENCE.md has no section %q", title)
	return strings.Join(out, "\n")
}

// userAgentSetters are the directories that may set the User-Agent header.
var userAgentSetters = []string{"internal/client/httpx/", "internal/client/kube/", "cmd/rancher/"}

// isUserAgentKey reports whether e is a string literal naming the
// User-Agent header in any letter case.
func isUserAgentKey(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && strings.EqualFold(value, "User-Agent")
}

// userAgentSets returns one message per place in f, a file at path, that
// sets the User-Agent: a call of Header.Set or Header.Add (also as the
// method expression http.Header.Set(h, key, value)), an index assignment, or
// a map or http.Header literal entry with the key "User-Agent" in any case;
// an assignment to, or a rest.Config literal field named, UserAgent; or a
// call of httpx.SetUserAgent or rest.AddUserAgent. A read does not count.
// The key must be a string literal; a key held in a variable is out of
// scope.
func userAgentSets(fset *token.FileSet, path string, f *ast.File) []string {
	for _, dir := range userAgentSetters {
		if strings.HasPrefix(path, dir) {
			return nil
		}
	}
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = p
	}
	var out []string
	report := func(pos token.Pos, what string) {
		out = append(out, fset.Position(pos).String()+": "+what+"; only the httpx, kube and main packages set the User-Agent")
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] == httpxPath && sel.Sel.Name == "SetUserAgent" {
				report(x.Pos(), "a call of httpx.SetUserAgent")
			}
			if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] == restPath && sel.Sel.Name == "AddUserAgent" {
				report(x.Pos(), "a call of rest.AddUserAgent")
			}
			if (sel.Sel.Name == "Set" || sel.Sel.Name == "Add") && len(x.Args) >= 1 && isUserAgentKey(x.Args[0]) {
				report(x.Pos(), "a call of "+sel.Sel.Name+" with the key User-Agent")
			}
			// The method expression http.Header.Set(h, key, value) takes the
			// key second.
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Header" && (sel.Sel.Name == "Set" || sel.Sel.Name == "Add") && len(x.Args) >= 2 && isUserAgentKey(x.Args[1]) {
				if id, ok := inner.X.(*ast.Ident); ok && imports[id.Name] == "net/http" {
					report(x.Pos(), "a call of http.Header."+sel.Sel.Name+" with the key User-Agent")
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				switch l := lhs.(type) {
				case *ast.IndexExpr:
					if isUserAgentKey(l.Index) {
						report(l.Pos(), "an index assignment with the key User-Agent")
					}
				case *ast.SelectorExpr:
					if l.Sel.Name == "UserAgent" {
						report(l.Pos(), "an assignment to a UserAgent field")
					}
				}
			}
		case *ast.CompositeLit:
			isRestConfig := false
			if sel, ok := x.Type.(*ast.SelectorExpr); ok {
				id, isIdent := sel.X.(*ast.Ident)
				isRestConfig = isIdent && imports[id.Name] == restPath && sel.Sel.Name == "Config"
			}
			for _, elt := range x.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if isUserAgentKey(kv.Key) {
					report(kv.Pos(), "a literal entry with the key User-Agent")
				}
				if id, ok := kv.Key.(*ast.Ident); ok && isRestConfig && id.Name == "UserAgent" {
					report(kv.Pos(), "a rest.Config literal with UserAgent")
				}
			}
		}
		return true
	})
	return out
}

func userAgentSnippet(t *testing.T, path, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	require.NoError(t, err)
	return userAgentSets(fset, path, f)
}

// TestUserAgentScanFixtures asserts the scan's rules from source snippets.
// Mutation: a fixture that sets the header in another package.
func TestUserAgentScanFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, src string
		reject          bool
	}{
		{"Header.Set", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header.Set("User-Agent", "x") }`, true},
		{"Header.Set lower case", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header.Set("user-agent", "x") }`, true},
		{"Header.Add", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header.Add("USER-AGENT", "x") }`, true},
		{"index assignment", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header["User-Agent"] = []string{"x"} }`, true},
		{"http.Header literal", "internal/x/x.go", `package p; import "net/http"; var _ = http.Header{"User-Agent": {"x"}}`, true},
		{"map literal", "internal/x/x.go", `package p; var _ = map[string]string{"user-agent": "x"}`, true},
		{"http.Header.Set method expression", "internal/x/x.go", `package p; import "net/http"; func f(h http.Header) { http.Header.Set(h, "User-Agent", "x") }`, true},
		{"http.Header.Add method expression", "internal/x/x.go", `package p; import "net/http"; func f(h http.Header) { http.Header.Add(h, "user-agent", "x") }`, true},
		{"rest.AddUserAgent", "internal/x/x.go", `package p; import "k8s.io/client-go/rest"; func f(c *rest.Config) { rest.AddUserAgent(c, "x") }`, true},
		{"http.Header.Set method expression of another key", "internal/x/x.go", `package p; import "net/http"; func f(h http.Header) { http.Header.Set(h, "Accept", "x") }`, false},
		{"rest.AddUserAgent in kube", "internal/client/kube/x.go", `package p; import "k8s.io/client-go/rest"; func f(c *rest.Config) { rest.AddUserAgent(c, "x") }`, false},
		{"UserAgent assignment", "internal/x/x.go", `package p; import "k8s.io/client-go/rest"; func f(c *rest.Config) { c.UserAgent = "x" }`, true},
		{"rest.Config literal", "internal/x/x.go", `package p; import "k8s.io/client-go/rest"; var _ = rest.Config{UserAgent: "x"}`, true},
		{"rest.Config literal through a pointer", "internal/x/x.go", `package p; import "k8s.io/client-go/rest"; var _ = &rest.Config{UserAgent: "x"}`, true},
		{"httpx.SetUserAgent", "internal/x/x.go", `package p; import "github.com/rancher/rancher-cli/internal/client/httpx"; func f() { httpx.SetUserAgent("x") }`, true},
		{"httpx.SetUserAgent aliased", "internal/x/x.go", `package p; import h "github.com/rancher/rancher-cli/internal/client/httpx"; func f() { h.SetUserAgent("x") }`, true},
		{"read of a request", "internal/testrancher/x.go", `package p; import "net/http"; func f(r *http.Request) string { return r.UserAgent() }`, false},
		{"read of the header", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) string { return r.Header.Get("User-Agent") }`, false},
		{"httpx.UserAgent", "internal/x/x.go", `package p; import "github.com/rancher/rancher-cli/internal/client/httpx"; var _ = httpx.UserAgent()`, false},
		{"another header", "internal/x/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header.Set("Accept", "x") }`, false},
		{"another struct with a UserAgent field in a literal", "internal/x/x.go", `package p; type rec struct{ UserAgent string }; var _ = rec{UserAgent: "x"}`, false},
		{"httpx directory", "internal/client/httpx/x.go", `package p; import "net/http"; func f(r *http.Request) { r.Header.Set("User-Agent", "x") }`, false},
		{"kube directory", "internal/client/kube/x.go", `package p; import "k8s.io/client-go/rest"; func f(c *rest.Config) { c.UserAgent = "x" }`, false},
		{"main directory", "cmd/rancher/x.go", `package p; import "github.com/rancher/rancher-cli/internal/client/httpx"; func f() { httpx.SetUserAgent("x") }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := userAgentSnippet(t, tc.path, tc.src)
			if tc.reject {
				assert.NotEmpty(t, got, "expected a violation")
			} else {
				assert.Empty(t, got, "expected no violation, got %v", got)
			}
		})
	}
}

// TestOnlyHTTPXKubeAndMainSetTheUserAgent runs the scan over every
// production file in the module.
func TestOnlyHTTPXKubeAndMainSetTheUserAgent(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for i, f := range tree.files {
		for _, msg := range userAgentSets(tree.fset, tree.paths[i], f) {
			t.Error(msg)
		}
	}
}

// TestInvocationHoldsNoPackageVariable asserts internal/invocation declares
// no package-level variable: the description it builds is a value, never
// state that changes after the process starts.
func TestInvocationHoldsNoPackageVariable(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	seen := 0
	for i, f := range tree.files {
		if !strings.HasPrefix(tree.paths[i], invocationDir) {
			continue
		}
		seen++
		for _, decl := range f.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
				t.Errorf("%s: package-level var; internal/invocation holds none", tree.fset.Position(gen.Pos()))
			}
		}
	}
	assert.Positive(t, seen, "the walk saw no file of internal/invocation")
}

// TestInvocationImportsOnlyThePureStandardLibrary asserts the production
// files of internal/invocation import only runtime, slices and strings: no
// package that reads the environment, a file or the network, so Detect can
// read the environment only through the lookup it is given. A package that
// must be added to the list is a deliberate change to this test.
func TestInvocationImportsOnlyThePureStandardLibrary(t *testing.T) {
	t.Parallel()
	allowed := []string{"runtime", "slices", "strings"}
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	seen := 0
	for i, f := range tree.files {
		if !strings.HasPrefix(tree.paths[i], invocationDir) {
			continue
		}
		seen++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if !slices.Contains(allowed, path) {
				t.Errorf("%s: imports %s; internal/invocation imports only %v", tree.fset.Position(imp.Pos()), path, allowed)
			}
		}
	}
	assert.Positive(t, seen, "the walk saw no file of internal/invocation")
}
