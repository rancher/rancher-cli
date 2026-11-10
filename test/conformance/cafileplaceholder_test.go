package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	caFileHelperPath = "internal/config/cafile.go"
	caFileMeaningTxt = "holds the server's CA certificate"
	caFileArgTail    = " <file>"
)

// caFlagPlaceholder matches a placeholder in angle brackets after a CA flag
// in any spelling: --cacert <file>, --cacerts  <path>, --cacert=<file>,
// --cacert '<file>', --cacert `<file>`.
var caFlagPlaceholder = regexp.MustCompile("--cacerts?(\\s+|=)['\"`]?<")

// caFileLiteralIsHandWritten reports whether a string literal writes a CA
// flag with a placeholder, the meaning of the placeholder, or a text that
// starts with the placeholder the argument helper writes (" <file>" or
// "<file>"), which a flag built from two literals ends with: the helpers in
// internal/config/cafile.go are the only place that may.
func caFileLiteralIsHandWritten(text string) bool {
	if caFlagPlaceholder.MatchString(text) || strings.Contains(text, "<file with") || strings.Contains(text, caFileMeaningTxt) {
		return true
	}
	return strings.HasPrefix(text, caFileArgTail) || strings.HasPrefix(text, "<file>")
}

// caFileReferences counts the references of file f to config.CAFileMeaning
// and config.CAFileArg: a selector on the config package, or the bare name
// inside package config.
func caFileReferences(f *ast.File, path string) (meaning, arg int) {
	inConfig := f.Name.Name == "config" && strings.HasPrefix(path, "internal/config/")
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "config" {
				switch x.Sel.Name {
				case "CAFileMeaning":
					meaning++
				case "CAFileArg":
					arg++
				}
			}
		case *ast.Ident:
			if !inConfig {
				return true
			}
			switch x.Name {
			case "CAFileMeaning":
				meaning++
			case "CAFileArg":
				arg++
			}
		}
		return true
	})
	return meaning, arg
}

// TestEveryCACertFilePlaceholderSaysWhatTheFileIs asserts that every message
// that suggests a CA flag with a <file> placeholder says what the file is,
// in the one wording of internal/config/cafile.go:
//
//  1. outside that file no string literal writes a placeholder after a CA
//     flag (any spacing, quotes, any name), the meaning of the placeholder,
//     or a literal that starts with a placeholder (which a flag built from
//     two literals ends with), so each message takes both from the helpers;
//  2. each file uses CAFileMeaning as often as CAFileArg, so a message that
//     names the placeholder also says what it is (a site with several
//     branches takes the meaning into one value the branches share);
//  3. the helper file writes the meaning once and the argument once.
//
// The scan reads string literals, so comments never count.
func TestEveryCACertFilePlaceholderSaysWhatTheFileIs(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	sawHelper := false
	for i, f := range tree.files {
		path := tree.paths[i]
		if path == caFileHelperPath {
			sawHelper = true
			meanings, args, placeholders := 0, 0, 0
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.Contains(text, "<file") {
					return true
				}
				placeholders++
				switch {
				case strings.Contains(text, caFileMeaningTxt):
					meanings++
				case text == caFileArgTail:
					args++
				}
				return true
			})
			assert.Equal(t, 1, meanings, "%s writes the meaning of <file> %d times, want one literal", path, meanings)
			assert.Equal(t, 1, args, "%s writes %q %d times, want one literal", path, caFileArgTail, args)
			assert.Equal(t, 2, placeholders, "%s writes <file> in %d literals, want the meaning and the argument", path, placeholders)
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err == nil && caFileLiteralIsHandWritten(text) {
				t.Errorf("%s: the string %q writes a CA flag placeholder or its meaning by hand: use config.CAFileArg and config.CAFileMeaning",
					tree.fset.Position(lit.Pos()), text)
			}
			return true
		})

		meaning, arg := caFileReferences(f, path)
		assert.Equal(t, arg, meaning, "%s: %d uses of config.CAFileMeaning and %d of config.CAFileArg; a message that names the <file> placeholder says what it is", path, meaning, arg)
	}
	assert.True(t, sawHelper, "the scan did not reach %s", caFileHelperPath)
}

// TestCAFileLiteralMatcherSeesEveryWayToWriteTheFlagByHand asserts the matcher
// of rule 1 on the spellings it must catch and on the texts it must leave
// alone.
func TestCAFileLiteralMatcherSeesEveryWayToWriteTheFlagByHand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"pass --cacert <file> to log in", true},
		{"pass --cacert  <file> to log in", true},
		{"pass --cacert\t<file>", true},
		{"pass --cacert <path> to log in", true},
		{"--cacerts <file>", true},
		{"--cacert=<file>", true},
		{"pass --cacert '<file>' to log in", true},
		{"pass --cacert \"<file>\" to log in", true},
		{"add <file with the CA>", true},
		{"<file> is a file that holds the server's CA certificate", true},
		{" <file>", true},
		{" <file> to log in", true},
		{"<file> reads the manifest", true},
		{"pass --cacert `<file>` to log in", true},
		{"<filename> is required", false},
		{"<file|-> reads the manifest from a file or stdin", false},
		{" <filename>", false},
		{" <file>, or unset", true},
		{"--cacert needs a file", false},
		{"--cacert '/etc/ca.pem'", false},
		{"--cacert", false},
		{"servers.a changed; <url> is the server's address", false},
	} {
		assert.Equal(t, tc.want, caFileLiteralIsHandWritten(tc.text), "%q", tc.text)
	}
}

// loginTextFile defines LoginRemedy and is the one file that may take its
// text. Every other file gets it from Instruction, which puts Facts (the
// meaning of <file> for an inline CA) before the command.
const loginTextFile = "internal/token/noterminal.go"

// loginRemedyUses lists the uses in pkg's files of the members of LoginRemedy
// that hold or build the text of its command: the method Text, the methods
// signIn and pipedLogin, and the field command. A use is a selector whether
// it is called or not (a call, a method value, a method expression, a field
// read). signIn is exempt as part of an argument of an Instruction call, which
// puts Facts before it.
func loginRemedyUses(pkg *packages.Package) []token.Pos {
	var bad []token.Pos
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(filepath.ToSlash(pkg.Fset.Position(f.Pos()).Filename), "/"+loginTextFile) {
			continue
		}
		var inInstruction [][2]token.Pos
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Instruction" {
					for _, arg := range call.Args {
						inInstruction = append(inInstruction, [2]token.Pos{arg.Pos(), arg.End()})
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			use, ok := pkg.TypesInfo.Selections[sel]
			if !ok {
				return true
			}
			recv := use.Recv()
			if p, ok := types.Unalias(recv).(*types.Pointer); ok {
				recv = p.Elem()
			}
			if !isNamedType(recv, tokenPackagePath, "LoginRemedy") {
				return true
			}
			switch use.Obj().Name() {
			case "Text", "pipedLogin", "command":
				bad = append(bad, sel.Pos())
			case "signIn":
				if !slices.ContainsFunc(inInstruction, func(r [2]token.Pos) bool { return r[0] <= sel.Pos() && sel.End() <= r[1] }) {
					bad = append(bad, sel.Pos())
				}
			}
			return true
		})
	}
	return bad
}

// loginRemedyEmbedders lists the struct types of pkg's files that embed
// LoginRemedy: a use of Text on the embedding value would not name LoginRemedy
// as its receiver, so no struct may embed it.
func loginRemedyEmbedders(pkg *packages.Package) []token.Pos {
	var bad []token.Pos
	for _, f := range pkg.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if len(field.Names) != 0 {
					continue
				}
				t := pkg.TypesInfo.TypeOf(field.Type)
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if t != nil && isNamedType(t, tokenPackagePath, "LoginRemedy") {
					bad = append(bad, field.Pos())
				}
			}
			return true
		})
	}
	return bad
}

// loginRemedyPackages is the module type-checked for the host and for each of
// windows and linux that is not the host (the loads are shared with the other
// scans of the package), so a use in a _windows.go or _linux.go file is seen
// on any host.
var loginRemedyPackages = sync.OnceValues(func() (map[string][]*packages.Package, error) {
	out := map[string][]*packages.Package{}
	host, err := fieldManagerPackages()
	if err != nil {
		return nil, err
	}
	out["host"] = host
	for _, goos := range []string{"windows", "linux"} {
		pkgs, err := otherGOOSPackages(goos)
		if err != nil {
			return nil, err
		}
		if pkgs != nil {
			out[goos] = pkgs
		}
	}
	return out, nil
})

// TestEveryLoginRemedyTextCarriesItsFacts asserts no file outside
// internal/token/noterminal.go takes the command of a LoginRemedy without its
// facts, in any form (Text, signIn, pipedLogin and the field command, called,
// held as a value, or named by a method expression), and that no struct
// embeds LoginRemedy: the command of a login with an inline CA holds
// --cacert <file>, and Facts says what <file> is. Every message reaches the
// command through Instruction, or through String, which is Instruction, so a
// %v print of the value carries the facts too. The scan runs over the host's
// files and over the files built for windows and for linux.
//
// Limits: a call through an interface that Text satisfies, and reflection on
// the unexported field, are not seen.
func TestEveryLoginRemedyTextCarriesItsFacts(t *testing.T) {
	t.Parallel()
	byGOOS, err := loginRemedyPackages()
	require.NoError(t, err)
	seen := map[string]bool{}
	for goos, pkgs := range byGOOS {
		reached := false
		for _, pkg := range pkgs {
			if pkg.PkgPath == tokenPackagePath {
				reached = true
			}
			for _, pos := range loginRemedyUses(pkg) {
				at := pkg.Fset.Position(pos).String()
				if !seen[at] {
					seen[at] = true
					t.Errorf("%s: this use of LoginRemedy gives its command without its facts: use LoginRemedy.Instruction", at)
				}
			}
			for _, pos := range loginRemedyEmbedders(pkg) {
				at := pkg.Fset.Position(pos).String()
				if !seen[at] {
					seen[at] = true
					t.Errorf("%s: this struct embeds LoginRemedy, whose methods then give its command without its facts: name the field", at)
				}
			}
		}
		assert.True(t, reached, "the %s scan did not reach %s", goos, tokenPackagePath)
	}
}
