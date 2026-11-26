package conformance

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every reader of a secret from stdin drops every byte order mark at the
// start of stdin: Windows PowerShell 5.1 sends one before piped text. The
// table in internal/cli (TestEveryStdinSecretDropsLeadingByteOrderMarks) runs
// each command that has a flag whose name ends in -stdin. The scan below finds
// the other way a new reader appears: a call of a method Read with the literal
// "-" as its path (in a quoted or a raw string), a call of a method ReadLine, or
// any use of os.Stdin, in a function with no allowlist entry.
//
// A reader that reaches stdin through a variable (the document readers of
// create resource -f -, patch --patch-file - and --values -, which pass a path
// the user typed) is not matched here. TestEveryDocumentReaderDropsLeadingByteOrderMarks
// finds those readers by the shape of the method Read they call.

const (
	stdinOSPath       = "os"
	stdinReaderReason = "a secret reader; TestEveryStdinSecretDropsLeadingByteOrderMarks covers it"
)

// stdinReaderAllowance is one function of stdinReaderAllowlist: how many stdin
// reads it has, why it needs no row in the table, and, for a secret reader, why
// the text it reads stays a string.
type stdinReaderAllowance struct {
	count int
	why   string
	// stays is the reason a secret reader keeps its text as a string and not as
	// a secret.Value; empty for a function that reads no secret.
	stays string
}

// stdinSecretReaders are the functions of stdinReaderAllowlist that read a
// secret: each says in stays why the text stays a string.
var stdinSecretReaders = []string{
	"internal/engine/password.go:ReadPasswordStdin",
	"internal/engine/password.go:ReadCurrentAndNewPasswordStdin",
	"internal/commands/auth/login.go:resolveLoginToken",
}

// stdinReaderAllowlist maps "<file>:<func>" to the number of stdin reads in
// that function and why the function is not a secret reader that needs a row
// in the table. A different number of matches fails the test.
var stdinReaderAllowlist = map[string]stdinReaderAllowance{
	"internal/engine/password.go:ReadPasswordStdin":              {1, stdinReaderReason, "stays a string: the password goes into the object the command builds"},
	"internal/engine/password.go:ReadCurrentAndNewPasswordStdin": {1, stdinReaderReason, "stays a string: the passwords go into the object the command builds"},
	"internal/commands/auth/login.go:resolveLoginToken":          {1, stdinReaderReason, "stays a string: the token goes into the login credential the command stores"},
	"internal/cli/adapter.go:buildIO":                            {1, "the root Reader every command reads through; the secret readers are ReadPasswordStdin, ReadCurrentAndNewPasswordStdin and resolveLoginToken", ""},
	"internal/cli/run.go:runArgv":                                {1, "gives a command tree with no Reader the process's standard input, where a run starts; buildIO reads the root's Reader and nothing reads it here", ""},
	"internal/commands/configcmd/config_edit.go:execEditor":      {1, "wires the editor's terminal", ""},
	"credhelper/serve.go:Serve":                                  {1, "the credential helper protocol: requests are JSON and no flag takes a secret", ""},
}

// scanStdinReaders returns one match for every call of a method Read whose
// first argument is the literal "-", every call of a method ReadLine, and
// every reference to os.Stdin in f, keyed by file and enclosing function.
func scanStdinReaders(fset *token.FileSet, filePath string, f *ast.File) []requestMatch {
	osName := importName(f, stdinOSPath)
	var out []requestMatch
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		report := func(pos token.Pos, text string) {
			out = append(out, requestMatch{
				key:  filePath + ":" + fn,
				text: fset.Position(pos).String() + ": " + text + " is a new reader of stdin. If it reads a secret, it must drop every byte order mark at the start of stdin and its flag needs a row in the table of TestEveryStdinSecretDropsLeadingByteOrderMarks; otherwise add the function to stdinReaderAllowlist with a reason",
			})
		}
		selected := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				selected[sel.Sel] = true
			}
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Read":
					if len(n.Args) > 0 {
						if lit, ok := ast.Unparen(n.Args[0]).(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if path, err := strconv.Unquote(lit.Value); err == nil && path == "-" {
								report(n.Pos(), `a call of Read with the path "-"`)
							}
						}
					}
				case "ReadLine":
					report(n.Pos(), "a call of ReadLine")
				}
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && osName != "" && osName != "." && id.Name == osName && n.Sel.Name == "Stdin" {
					report(n.Pos(), "a use of os.Stdin")
				}
			case *ast.Ident:
				if osName == "." && n.Name == "Stdin" && !selected[n] {
					report(n.Pos(), "a use of os.Stdin")
				}
			}
			return true
		})
	}
	return out
}

// checkStdinReaders parses every source in sources (path to content) and
// returns the problems checkRequestMatches finds against allow.
func checkStdinReaders(sources map[string]string, allow map[string]stdinReaderAllowance) ([]string, error) {
	fset := token.NewFileSet()
	var matches []requestMatch
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
		if err != nil {
			return nil, err
		}
		matches = append(matches, scanStdinReaders(fset, path, f)...)
	}
	counts := make(map[string]requestAllowance, len(allow))
	for key, a := range allow {
		counts[key] = requestAllowance{a.count, a.why}
	}
	return checkRequestMatches(matches, counts), nil
}

// TestEveryStdinReaderHasAnAllowlistEntry asserts that every production
// function that calls Read with the path "-", calls ReadLine or uses os.Stdin
// has an entry in the allowlist with the exact number of such uses, and that
// no entry is stale. The secret readers are covered by the table in
// internal/cli, TestEveryStdinSecretDropsLeadingByteOrderMarks.
func TestEveryStdinReaderHasAnAllowlistEntry(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		if bytes.Contains(src, []byte("Read(")) || bytes.Contains(src, []byte("ReadLine")) || bytes.Contains(src, []byte("Stdin")) {
			sources[path] = string(src)
		}
	}
	problems, err := checkStdinReaders(sources, stdinReaderAllowlist)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
	for key, a := range stdinReaderAllowlist {
		assert.NotEmpty(t, a.why, key)
		assert.Positive(t, a.count, key)
		assert.Equal(t, slices.Contains(stdinSecretReaders, key), a.stays != "", "%s: stays is the reason a secret reader keeps its text as a string, and only a secret reader has one", key)
	}
	for _, key := range stdinSecretReaders {
		assert.Contains(t, stdinReaderAllowlist, key)
	}
}

// TestStdinReaderScanFlagsEveryNewReader asserts the scan reports a new call
// of Read with the path "-" (also written as a raw string or with an escape), a
// call of ReadLine, a use of os.Stdin (also under an aliased and a dot import),
// accepts an allowlisted function with its exact count, ignores the definition
// of a method ReadLine, a Read of a path variable and a comment, and reports a
// wrong count and an entry that matches nothing.
func TestStdinReaderScanFlagsEveryNewReader(t *testing.T) {
	t.Parallel()
	const read = `package p

func NewReader(src Source) ([]byte, error) {
	return src.Read("-")
}
`
	const rawRead = `package p

func NewReader(src Source) ([]byte, error) {
	return src.Read(` + "`-`" + `)
}
`
	const escapedRead = `package p

func NewReader(src Source) ([]byte, error) {
	return src.Read("\x2d")
}
`
	const line = `package p

func Token(p Prompter) (string, error) {
	return p.ReadLine(ctx)
}
`
	const stdin = `package p

import "os"

func Wire() { in := os.Stdin; _ = in }
`
	const aliased = `package p

import o "os"

func Wire() { _ = o.Stdin }
`
	const dotted = `package p

import . "os"

func Wire() { _ = Stdin }
`
	const ignored = `package p

import "os"

// Read("-") and os.Stdin are only named here, and ReadLine too.
func (p *Prompter) ReadLine(ctx context.Context) (string, error) { return "", nil }

func Document(src Source, path string) ([]byte, error) {
	_ = "-"
	_ = os.Args
	return src.Read(path)
}
`
	const allowed = `package p

func Two(src Source) {
	src.Read("-")
	src.Read("-")
}
`
	problems, err := checkStdinReaders(map[string]string{"new.go": read}, nil)
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "new.go:4:")
	assert.Contains(t, problems[0], `Read with the path "-"`)

	problems, err = checkStdinReaders(map[string]string{"raw.go": rawRead, "escaped.go": escapedRead}, nil)
	require.NoError(t, err)
	assert.Len(t, problems, 2, "a path of a raw string or with an escape is the dash too")

	problems, err = checkStdinReaders(map[string]string{"line.go": line}, nil)
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "a call of ReadLine")

	problems, err = checkStdinReaders(map[string]string{"stdin.go": stdin, "aliased.go": aliased, "dotted.go": dotted}, nil)
	require.NoError(t, err)
	assert.Len(t, problems, 3, "os.Stdin is reported under a plain, an aliased and a dot import")

	problems, err = checkStdinReaders(map[string]string{"ignored.go": ignored}, nil)
	require.NoError(t, err)
	assert.Empty(t, problems, "a definition of ReadLine, a Read of a path variable, a string and a comment are not readers")

	allow := map[string]stdinReaderAllowance{"two.go:Two": {2, "reason", ""}}
	problems, err = checkStdinReaders(map[string]string{"two.go": allowed}, allow)
	require.NoError(t, err)
	assert.Empty(t, problems, "an allowlisted function with its exact count passes")

	allow["two.go:Two"] = stdinReaderAllowance{1, "reason", ""}
	problems, err = checkStdinReaders(map[string]string{"two.go": allowed}, allow)
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "the allowlist covers 1 match(es)")

	problems, err = checkStdinReaders(map[string]string{"two.go": allowed}, map[string]stdinReaderAllowance{
		"two.go:Two":  {2, "reason", ""},
		"gone.go:Old": {1, "reason", ""},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"allowlist entry gone.go:Old matched nothing: remove it"}, problems)
}
