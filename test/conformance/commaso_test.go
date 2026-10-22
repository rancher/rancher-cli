package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commaSoRE matches ", so" followed by a word: the informal joint the CLI's
// messages do not use. A message states what was typed or found, then the
// rule or remedy after "; ".
var commaSoRE = regexp.MustCompile(`,\s+so\b`)

// commaSoGap stands for an operand of a + chain that is not a string
// literal; a literal on each side of it cannot form a match across it.
const commaSoGap = "\x00"

// foldedPart is one node of a folded + chain: a string literal and where
// its text starts in the folded string, or (lit nil) a + node.
type foldedPart struct {
	node   ast.Node
	lit    *ast.BasicLit
	offset int
}

// foldChain folds a string literal, or a + chain of operands, into one
// string with every operand that is not a string literal replaced by
// commaSoGap. It reports the literals it folded with their offsets, and
// whether e holds at least one string literal.
func foldChain(e ast.Expr, folded *strings.Builder, parts *[]foldedPart) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			break
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			break
		}
		*parts = append(*parts, foldedPart{node: x, lit: x, offset: folded.Len()})
		folded.WriteString(s)
		return true
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			*parts = append(*parts, foldedPart{node: x})
			l := foldChain(x.X, folded, parts)
			r := foldChain(x.Y, folded, parts)
			return l || r
		}
	case *ast.ParenExpr:
		return foldChain(x.X, folded, parts)
	}
	folded.WriteString(commaSoGap)
	return false
}

// commaSoContext is up to 40 bytes of s on each side of the match at
// [start, end), cut on rune boundaries, with gaps shown as "...".
func commaSoContext(s string, start, end int) string {
	from := max(start-40, 0)
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	to := min(end+40, len(s))
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to++
	}
	return strings.ReplaceAll(s[from:to], commaSoGap, "...")
}

// foldedChain is one string literal, or one + chain of operands, as
// foldChain folds it.
type foldedChain struct {
	text  string
	parts []foldedPart
}

// partAt is the literal part that holds byte offset off of the folded text:
// the last literal that starts at or before it.
func (c foldedChain) partAt(off int) foldedPart {
	var at foldedPart
	for _, p := range c.parts {
		if p.lit != nil && (at.lit == nil || p.offset <= off) {
			at = p
		}
	}
	return at
}

// literalAt is the literal that holds byte offset off of the folded text.
func (c foldedChain) literalAt(off int) *ast.BasicLit {
	return c.partAt(off).lit
}

// foldedChains lists every string literal or + chain of literals in f, each
// as one folded string. Comments are never visited: the walk reads
// expressions only. A chain is listed once as a whole, and the walk goes on
// into its other operands (a call inside the chain), skipping the literals
// the chain already covered.
func foldedChains(f *ast.File) []foldedChain {
	var chains []foldedChain
	covered := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.BinaryExpr, *ast.BasicLit:
		default:
			return true
		}
		e := n.(ast.Expr)
		if covered[n] {
			return true
		}
		if bin, ok := e.(*ast.BinaryExpr); ok && bin.Op != token.ADD {
			return true
		}
		var folded strings.Builder
		var parts []foldedPart
		if !foldChain(e, &folded, &parts) {
			return true
		}
		for _, p := range parts {
			covered[p.node] = true
		}
		chains = append(chains, foldedChain{text: folded.String(), parts: parts})
		return true
	})
	return chains
}

// commaSoHits lists "path:line: text" for every string literal or + chain
// of literals in f that matches commaSoRE, with the line of the literal
// that holds the match. A chain is tested once as a whole.
func commaSoHits(fset *token.FileSet, path string, f *ast.File) []string {
	var hits []string
	for _, c := range foldedChains(f) {
		if loc := commaSoRE.FindStringIndex(c.text); loc != nil {
			at := c.literalAt(loc[0])
			hits = append(hits, path+":"+strconv.Itoa(fset.Position(at.Pos()).Line)+": "+commaSoContext(c.text, loc[0], loc[1]))
		}
	}
	return hits
}

// TestNoCommaSoInMessages asserts that no string in the production source
// joins a fact to its consequence with ", so". Write what was typed or
// found, then the rule after "; ". There is no allowlist; a text that must
// quote such a phrase is a decision for the maintainers.
func TestNoCommaSoInMessages(t *testing.T) {
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	require.NotEmpty(t, tree.files, "the walk saw no source files")
	var hits []string
	for i, f := range tree.files {
		hits = append(hits, commaSoHits(tree.fset, tree.paths[i], f)...)
	}
	for _, h := range hits {
		assert.Fail(t, h+`: write what was found, then the rule after "; " (no ", so")`)
	}
}

// TestCommaSoMatcher asserts what the scan finds and what it passes, and
// the line it reports.
func TestCommaSoMatcher(t *testing.T) {
	for _, s := range []string{"a, so b", "a,\n so b", "a,  so"} {
		assert.True(t, commaSoRE.MatchString(s), s)
	}
	for _, s := range []string{"also", "do so", "resolve, sort", "a, some", "a; so b"} {
		assert.False(t, commaSoRE.MatchString(s), s)
	}

	src := `package p
const a = "x, " + "so y"
const b = "x," + v + " so y"
// a comment, so it is not text
const c = "x; y"
var d = "x" + f("p, so q") + "y"
var e = "x" +
	"z, so w"
var g = ("a" + "b, ") + "so c"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	require.NoError(t, err)
	hits := commaSoHits(fset, "p.go", f)
	require.Len(t, hits, 4, "a folded chain, a literal inside a call in a chain and a multi-line chain are text; a non-literal operand and a comment are not: %v", hits)
	assert.Contains(t, hits[0], "p.go:2:")
	assert.Contains(t, hits[1], "p.go:6:")
	assert.Contains(t, hits[2], "p.go:8:", "the line of the literal that holds the match")
	assert.Contains(t, hits[3], "p.go:9:", "a nested chain is reported once")
}
