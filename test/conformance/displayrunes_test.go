package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
)

// displayRuneNames are the functions and tables of the unicode package the
// scan reports, and no others: the control, format and separator categories,
// the tables for the two format properties Bidi_Control and Join_Control,
// and the functions IsControl, IsPrint and IsGraphic. unicode.Other is the
// alias of unicode.C, the only alias of these categories the pinned Go
// exports; unicode.Categories reaches every category by its name.
var displayRuneNames = []string{
	"IsControl", "Cc", "Cf", "C", "Other", "Categories", "IsPrint", "IsGraphic", "Zl", "Zp", "Z",
	"Bidi_Control", "Join_Control",
}

// displayRuneAllowRow is one reviewed function that tests runes through the
// unicode package for a purpose other than the rule of
// clierr.IsUnsafeToDisplay, with the unicode names it uses.
type displayRuneAllowRow struct {
	file     string
	function string
	names    []string
	reason   string
}

var displayRuneAllowRows = []displayRuneAllowRow{
	{
		file:     "internal/runtime/completion/description.go",
		function: "SanitizeDescription",
		names:    []string{"Cf"},
		reason: "a completion description is one line beside a candidate: it deletes format runes and turns the other " +
			"hidden runes into a space so words stay apart, a different rule on purpose",
	},
	{
		file:     "internal/runtime/completion/description.go",
		function: "descriptionSpace",
		names:    []string{"C"},
		reason: "same rule as SanitizeDescription: category C holds control and format runes and also private-use, " +
			"surrogate and unassigned runes, all shown as a space in a description",
	},
	{
		file:     "internal/runtime/completion/description.go",
		function: "hasVisibleRune",
		names:    []string{"IsGraphic"},
		reason: "it decides whether a description shows anything at all (a graphic rune that is not a space or a blank " +
			"filler); it removes nothing",
	},
}

// displayRuneUse is one use of a scanned unicode name.
type displayRuneUse struct {
	file     string
	function string
	name     string
	place    string
}

// scanDisplayRuneUses returns every selector on the imported unicode package
// whose name is in displayRuneNames, keyed by file and enclosing declaration;
// the dot imports of unicode; and the matches with no named declaration. A
// file's import of unicode under any number of names counts. In a grouped var,
// const or type declaration each spec is its own declaration. The body of
// clierr.IsUnsafeToDisplay in internal/clierr/displayrunes.go is skipped. A
// selector on a local name that shadows an import is not a match.
func scanDisplayRuneUses(fset *token.FileSet, files []*ast.File, paths []string) (uses []displayRuneUse, dotImports, unnamed []string) {
	for i, f := range files {
		path := paths[i]
		importNames := map[string]bool{}
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != "unicode" {
				continue
			}
			switch {
			case imp.Name == nil:
				importNames["unicode"] = true
			case imp.Name.Name == ".":
				dotImports = append(dotImports, path)
			case imp.Name.Name != "_":
				importNames[imp.Name.Name] = true
			}
		}
		if len(importNames) == 0 {
			continue
		}
		for _, decl := range f.Decls {
			for _, unit := range declarationUnits(decl) {
				if path == "internal/clierr/displayrunes.go" && unit.name == "IsUnsafeToDisplay" {
					continue
				}
				ast.Inspect(unit.node, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					x, ok := sel.X.(*ast.Ident)
					if !ok || !importNames[x.Name] || x.Obj != nil || !slices.Contains(displayRuneNames, sel.Sel.Name) {
						return true
					}
					place := fset.Position(sel.Pos()).String()
					if unit.name == "" {
						unnamed = append(unnamed, place+" unicode."+sel.Sel.Name)
						return true
					}
					uses = append(uses, displayRuneUse{file: path, function: unit.name, name: sel.Sel.Name, place: place})
					return true
				})
			}
		}
	}
	return uses, dotImports, unnamed
}

// declarationUnit is a part of a file with its own name: a function or
// method (with its receiver type), or one spec of a var, const or type
// declaration, named by its first name. The name is empty when the part has
// none.
type declarationUnit struct {
	name string
	node ast.Node
}

// declarationUnits splits decl into its units: one for a function, one for
// each spec of a general declaration.
func declarationUnits(decl ast.Decl) []declarationUnit {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return []declarationUnit{{functionKey(d), d}}
	case *ast.GenDecl:
		var units []declarationUnit
		for _, spec := range d.Specs {
			name := ""
			switch s := spec.(type) {
			case *ast.ValueSpec:
				if len(s.Names) > 0 && s.Names[0].Name != "_" {
					name = s.Names[0].Name
				}
			case *ast.TypeSpec:
				name = s.Name.Name
			}
			units = append(units, declarationUnit{name, spec})
		}
		return units
	}
	return nil
}

// TestNoRuneClassTestOutsideTheSharedPredicate asserts no non-test Go file in
// the repository tests the control, format or line and paragraph separator
// classes through the unicode package (a selector named IsControl, Cc, Cf, C,
// Other, Categories, IsPrint, IsGraphic, Zl, Zp, Z, Bidi_Control or
// Join_Control) outside clierr.IsUnsafeToDisplay, except the functions listed
// in displayRuneAllowRows, and that no file dot-imports unicode. A name not
// listed for its function fails; a new call of a name already listed in the
// same function passes. A listed function or name with no use fails. The
// guard's claim is the scan's: it matches names on the imported package, not
// what a use is for. Hand-written comparisons outside it, such as the range
// tests in internal/yamlwrite and the literal U+2028 and U+2029 in
// internal/runtime/completion/description.go, are not covered.
func TestNoRuneClassTestOutsideTheSharedPredicate(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	uses, dotImports, unnamed := scanDisplayRuneUses(tree.fset, tree.files, tree.paths)
	assert.Empty(t, dotImports, "a dot import of unicode hides which names a file uses")
	assert.Empty(t, unnamed, "a use of a scanned name outside a named declaration")

	type fileFunction struct{ file, function string }
	allowed := map[fileFunction]displayRuneAllowRow{}
	for _, row := range displayRuneAllowRows {
		key := fileFunction{row.file, row.function}
		_, duplicate := allowed[key]
		assert.False(t, duplicate, "%s %s has two rows", row.file, row.function)
		assert.NotEmpty(t, row.reason, "%s %s needs a reason", row.file, row.function)
		assert.NotEmpty(t, row.names, "%s %s lists no name", row.file, row.function)
		allowed[key] = row
	}

	used := map[fileFunction][]string{}
	var unlisted []string
	for _, use := range uses {
		key := fileFunction{use.file, use.function}
		used[key] = append(used[key], use.name)
		if row, ok := allowed[key]; !ok || !slices.Contains(row.names, use.name) {
			unlisted = append(unlisted, fmt.Sprintf("%s: unicode.%s in %s", use.place, use.name, use.function))
		}
	}
	slices.Sort(unlisted)
	assert.Empty(t, unlisted, "call clierr.IsUnsafeToDisplay, or add the function to displayRuneAllowRows with the reason its rule differs")

	for key, row := range allowed {
		if _, ok := used[key]; !ok {
			t.Errorf("the row for %s in %s matches no use of a scanned name", row.function, row.file)
			continue
		}
		for _, name := range row.names {
			assert.Contains(t, used[key], name, "the row for %s in %s lists unicode.%s, which it does not use", row.function, row.file, name)
		}
	}
}

// TestDisplayRuneScanFindsEveryForm asserts the scan reports a use in a
// function, a method, a package-level var and a function literal in one; each
// spec of a grouped var; an import under another name; two import names in one
// file; the property tables Bidi_Control and Join_Control; and a dot import;
// and does not report a local name that shadows the import or a name outside
// the list.
func TestDisplayRuneScanFindsEveryForm(t *testing.T) {
	t.Parallel()
	parse := func(src string) (*token.FileSet, []*ast.File, []string) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, parser.AllErrors)
		require.NoError(t, err)
		return fset, []*ast.File{f}, []string{"x.go"}
	}

	fset, files, paths := parse(`package x

import "unicode"

func a(r rune) bool { return unicode.Is(unicode.Cf, r) }

type T struct{}

func (T) b(r rune) bool { return unicode.IsControl(r) }

var c = func(r rune) bool { return unicode.IsGraphic(r) }

func d(r rune) bool { return unicode.IsLetter(r) }
`)
	uses, dots, unnamed := scanDisplayRuneUses(fset, files, paths)
	assert.Empty(t, dots)
	assert.Empty(t, unnamed)
	var got []string
	for _, u := range uses {
		got = append(got, u.function+" "+u.name)
	}
	assert.ElementsMatch(t, []string{"a Cf", "T.b IsControl", "c IsGraphic"}, got)

	fset, files, paths = parse(`package x

import u "unicode"

func a(r rune) bool { return u.IsControl(r) }
`)
	uses, _, _ = scanDisplayRuneUses(fset, files, paths)
	require.Len(t, uses, 1)
	assert.Equal(t, "a", uses[0].function)

	fset, files, paths = parse(`package x

import (
	"unicode"
	u "unicode"
)

func a(r rune) bool { return unicode.IsControl(r) }

func b(r rune) bool { return u.IsControl(r) }
`)
	uses, _, _ = scanDisplayRuneUses(fset, files, paths)
	got = nil
	for _, u := range uses {
		got = append(got, u.function+" "+u.name)
	}
	assert.ElementsMatch(t, []string{"a IsControl", "b IsControl"}, got)

	fset, files, paths = parse(`package x

import "unicode"

var (
	first  = 1
	second = unicode.Is(unicode.Bidi_Control, 'a')
	third  = unicode.Is(unicode.Join_Control, 'a')
)
`)
	uses, _, _ = scanDisplayRuneUses(fset, files, paths)
	got = nil
	for _, u := range uses {
		got = append(got, u.function+" "+u.name)
	}
	assert.ElementsMatch(t, []string{"second Bidi_Control", "third Join_Control"}, got)

	fset, files, paths = parse(`package x

import . "unicode"

func a(r rune) bool { return IsControl(r) }
`)
	_, dots, _ = scanDisplayRuneUses(fset, files, paths)
	assert.Equal(t, []string{"x.go"}, dots)

	fset, files, paths = parse(`package x

import "unicode"

func a(r rune) bool { unicode := struct{ Cf int }{}; return unicode.Cf == 0 }
`)
	uses, _, _ = scanDisplayRuneUses(fset, files, paths)
	assert.Empty(t, uses)

	fset, files, paths = parse(`package x

import "unicode"

var _ = unicode.Cf
`)
	_, _, unnamed = scanDisplayRuneUses(fset, files, paths)
	assert.Len(t, unnamed, 1)
}

// displayRuneRow is one rune class of the shared rule with a sample rune and
// whether every caller removes it.
type displayRuneRow struct {
	class  string
	r      rune
	unsafe bool
}

// TestEveryCallerAppliesTheSharedDisplayRule asserts, for a sample of every
// class clierr.IsUnsafeToDisplay covers and of runes it keeps, that
// IsUnsafeToDisplay, output.Printable, config.EntryNameProblem, clierr.IDText
// and the text of config.AdviseRecord for an owner id holding the rune agree:
// an unsafe rune is removed by Printable, reported by EntryNameProblem and
// written as an escape in the owner's text (never removed from it), and every
// other rune is kept by all of them. LoginAs, the data field, keeps the id
// exactly.
func TestEveryCallerAppliesTheSharedDisplayRule(t *testing.T) {
	t.Parallel()
	rows := []displayRuneRow{
		{"escape", 0x1b, true},
		{"newline", '\n', true},
		{"tab", '\t', true},
		{"carriage return", '\r', true},
		{"delete", 0x7f, true},
		{"next line", 0x85, true},
		{"control sequence introducer", 0x9b, true},
		{"bidi right-to-left override", 0x202e, true},
		{"bidi first strong isolate", 0x2066, true},
		{"left-to-right mark", 0x200e, true},
		{"arabic letter mark", 0x061c, true},
		{"zero width space", 0x200b, true},
		{"invisible separator", 0x2063, true},
		{"byte order mark", 0xfeff, true},
		{"soft hyphen", 0x00ad, true},
		{"language tag", 0xe0001, true},
		{"line separator", 0x2028, true},
		{"paragraph separator", 0x2029, true},
		{"ASCII letter", 'x', false},
		{"space", ' ', false},
		{"latin letter with accent", 0xe9, false},
		{"han character", 0x4e16, false},
		{"hangul filler", 0x3164, false},
		{"replacement character", 0xfffd, false},
	}
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	site := config.RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}
	for _, row := range rows {
		t.Run(row.class, func(t *testing.T) {
			name := "a" + string(row.r) + "b"
			assert.Equal(t, row.unsafe, clierr.IsUnsafeToDisplay(row.r))

			_, problem := config.EntryNameProblem(name)
			assert.Equal(t, row.unsafe, problem, "EntryNameProblem")

			loginAs := config.AdviseRecord(site, "https://hostzq7", "u-"+name, ref).LoginAs
			assert.Equal(t, "u-"+name, loginAs, "LoginAs keeps the id exactly")
			text := config.AdviseRecord(site, "", "u-"+name, ref).Text
			if row.unsafe {
				assert.Equal(t, "ab", output.Printable(name), "Printable")
				assert.Equal(t, strconv.Quote("u-"+name), clierr.IDText("u-"+name), "IDText")
				assert.Contains(t, text, "delete it as user "+clierr.ShellQuote(strconv.Quote("u-"+name)), "the text shows the escaped id")
				return
			}
			assert.Equal(t, name, output.Printable(name), "Printable")
			assert.Equal(t, "u-"+name, clierr.IDText("u-"+name), "IDText")
			assert.Contains(t, text, "delete it as user "+clierr.ShellQuote("u-"+name), "the text shows the id as it is")
		})
	}
}
