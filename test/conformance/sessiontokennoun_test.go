package conformance

import (
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

// loginTokenNounHome is the one function that may hold the words "session
// token": the noun method of the token fate, which chooses between them and
// "kubeconfig token" by the flow that created the token. Every other text names
// the token through it, so a SAML sign-in's token is never called a session
// token.
const loginTokenNounHome = "internal/clierr/loginfailure.go#TokenFate.LoginTokenNoun"

// sessionTokenAllowlist lists the functions, other than the noun method,
// whose string literals may say "session token", each keyed as
// "path#Function" or "path#Receiver.Method" with the reason the words are
// correct there. An entry that matches no literal fails the scan.
var sessionTokenAllowlist = map[string]string{}

// sessionTokenHit is one string literal, or one + chain of literals, that says
// "session token".
type sessionTokenHit struct {
	key  string
	line int
}

// enclosingFunction is the name of the declaration that holds pos, as declName
// writes it: Function, Receiver.Method, or packageLevelFunc for a literal
// outside any function.
func enclosingFunction(f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		if pos >= d.Pos() && pos < d.End() {
			return declName(d)
		}
	}
	return packageLevelFunc
}

// sessionTokenHits lists every string literal or + chain of literals in f that
// holds the words "session token" in any case, found as a substring, with the
// function it is in. A literal split across a + chain counts as one string; a
// raw (backquoted) string is read as its text. Comments are never visited.
func sessionTokenHits(fset *token.FileSet, path string, f *ast.File) []sessionTokenHit {
	var hits []sessionTokenHit
	for _, c := range foldedChains(f) {
		if !strings.Contains(strings.ToLower(c.text), "session token") {
			continue
		}
		at := c.literalAt(strings.Index(strings.ToLower(c.text), "session token"))
		hits = append(hits, sessionTokenHit{key: path + "#" + enclosingFunction(f, at.Pos()), line: fset.Position(at.Pos()).Line})
	}
	return hits
}

// sessionTokenProblems lists what is wrong with the files' literals: a literal
// outside the home function and the allowlist, a home with no literal, and an
// allowlist entry that matches no literal.
func sessionTokenProblems(fset *token.FileSet, paths []string, files []*ast.File, homeKey string, allowlist map[string]string) []string {
	var problems []string
	used := map[string]bool{}
	home := false
	for i, f := range files {
		for _, h := range sessionTokenHits(fset, paths[i], f) {
			switch {
			case h.key == homeKey:
				home = true
			case allowlist[h.key] != "":
				used[h.key] = true
			default:
				problems = append(problems, h.key+":"+strconv.Itoa(h.line)+": a text says \"session token\"; name the token with TokenFate.LoginTokenNoun, which says kubeconfig token for a SAML sign-in")
			}
		}
	}
	if !home {
		problems = append(problems, homeKey+" holds no literal \"session token\"; the noun has another home, so update loginTokenNounHome")
	}
	var stale []string
	for key := range allowlist {
		if !used[key] {
			stale = append(stale, key)
		}
	}
	slices.Sort(stale)
	for _, key := range stale {
		problems = append(problems, key+": the allowlist entry matches no literal; remove it")
	}
	return problems
}

// TestOnlyTheLoginTokenNounMethodSaysSessionToken asserts that no string in the
// production source says "session token" except the noun method of the token
// fate (and the allowlist, with a reason for each entry), so one function
// decides whether a login's token is a session token or, for a SAML sign-in, a
// kubeconfig token. The scan reads string literals, joined across + chains;
// comments are out of its scope. A stale allowlist entry fails.
func TestOnlyTheLoginTokenNounMethodSaysSessionToken(t *testing.T) {
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	require.NotEmpty(t, tree.files, "the walk saw no source files")
	for key, reason := range sessionTokenAllowlist {
		assert.NotEmpty(t, reason, "%s: an allowlist entry carries its reason", key)
	}
	for _, p := range sessionTokenProblems(tree.fset, tree.paths, tree.files, loginTokenNounHome, sessionTokenAllowlist) {
		assert.Fail(t, p)
	}
}

// TestSessionTokenScanFindsEveryShapeOfTheLiteral asserts what the scan reports
// from a fixture: a literal, one split across a + chain, one in a raw string, a
// different case, a literal in a function literal inside a method, and a
// package-level one; and what it passes: a comment and text with other words.
// It also asserts the noun method's literal is not a problem, an allowlisted
// literal is not a problem, and a stale entry is one.
func TestSessionTokenScanFindsEveryShapeOfTheLiteral(t *testing.T) {
	src := "package p\n" +
		"type T struct{}\n" +
		"func plain() string { return \"the session token\" }\n" +
		"func split() string { return \"the session \" + \"token\" }\n" +
		"func raw() string { return `the session token` }\n" +
		"func upper() string { return \"A Session Token\" }\n" +
		"func (t *T) nested() func() string { return func() string { return \"x\" + \"session tokens\" } }\n" +
		"var level = \"session\" + \" token\"\n" +
		"// a session token in a comment is not text\n" +
		"func other() string { return \"the session and the token\" }\n" +
		"func (t *T) LoginTokenNoun() string { return \"session token\" }\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	require.NoError(t, err)
	var got []string
	for _, h := range sessionTokenHits(fset, "p.go", f) {
		got = append(got, h.key+":"+strconv.Itoa(h.line))
	}
	assert.Equal(t, []string{
		"p.go#plain:3", "p.go#split:4", "p.go#raw:5", "p.go#upper:6", "p.go#T.nested:7", "p.go#package level:8", "p.go#T.LoginTokenNoun:11",
	}, got)

	const home = "p.go#T.LoginTokenNoun"
	problems := sessionTokenProblems(fset, []string{"p.go"}, []*ast.File{f}, home, nil)
	assert.Len(t, problems, 6, "every hit but the home's is a problem: %v", problems)
	assert.NotContains(t, strings.Join(problems, "\n"), "LoginTokenNoun:")
	assert.Contains(t, sessionTokenProblems(fset, []string{"p.go"}, []*ast.File{f}, "p.go#nowhere", nil), "p.go#nowhere holds no literal \"session token\"; the noun has another home, so update loginTokenNounHome")

	allowed := map[string]string{"p.go#plain": "a password login", "p.go#split": "a password login", "p.go#raw": "r", "p.go#upper": "r",
		"p.go#T.nested": "r", "p.go#package level": "r", "p.go#gone": "r"}
	problems = sessionTokenProblems(fset, []string{"p.go"}, []*ast.File{f}, home, allowed)
	require.Len(t, problems, 1, "%v", problems)
	assert.Contains(t, problems[0], "p.go#gone: the allowlist entry matches no literal")
}
