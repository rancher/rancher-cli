package provisioning

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// credentialResolvers are the functions that select a cloud credential from a
// value the user typed. Each takes the lookup as an explicit parameter, the
// last argument of every call.
var credentialResolvers = []string{"resolveCredentialObj", "ResolveCloudCredential", "readStoredCredential"}

const (
	readLookup   = "lookupForRead"
	changeLookup = "lookupForChange"
	// passedOn is a call inside one of credentialResolvers that hands on its
	// own `lookup` parameter; the scan follows it to the callers of that
	// function.
	passedOn = "lookup"
)

// credentialCallers are the calls of credentialResolvers in the non-test Go
// files of the module, by file, enclosing function, callee and lookup, each
// with the command whose rows run below. A call that is not listed fails the
// scan, so a new caller needs a review and a row. lookupForRead is allowed in
// one call, the read command's: a mutation that takes a cloud credential
// selects a credential by id only.
var credentialCallers = []struct {
	file, function, callee, lookup, command string
}{
	{"internal/engine/provisioning/cloudcred.go", "getCloudCredCmd.Execute", "resolveCredentialObj", readLookup, "get cloud-credential"},
	{"internal/engine/provisioning/createcluster.go", "createClusterCmd.Plan", "ResolveCloudCredential", changeLookup, "create cluster"},
	{"internal/engine/provisioning/cloudcredwrite.go", "patchCloudCredentialCmd.Plan", "readStoredCredential", changeLookup, "patch cloud-credential"},
	{"internal/engine/provisioning/cloudcredwrite.go", "deleteCloudCredentialCmd.Plan", "readStoredCredential", changeLookup, "delete cloud-credential"},
}

// readLookupCaller is the one call that may pass lookupForRead.
const readLookupCaller = "internal/engine/provisioning/cloudcred.go getCloudCredCmd.Execute resolveCredentialObj"

// credentialCall is one call of a function in credentialResolvers, or a use of
// one that is not a call.
type credentialCall struct {
	function, callee, lookup, at string
}

// findCredentialCalls parses the Go source of path (read from the file when src
// is nil) and returns every call of a function in credentialResolvers, with the
// function it is in ("" at the package level) and its lookup argument: the
// name of the last argument when it is an identifier or a selector, else "?".
// A use of one of the functions that is not a call (a function value) has the
// lookup "no call". The declarations are not uses.
//
// The scan does not see reflection, or the value a lookup name holds (it reads
// the name of the last argument; an argument that is neither a name nor a
// selector is reported as `?`). The behavior tests named at findCommandRuns
// cover the three mutations that take a cloud credential.
func findCredentialCalls(path string, src any) ([]credentialCall, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var calls []credentialCall
	for _, decl := range file.Decls {
		function := enclosingFunction(decl)
		declared := map[*ast.Ident]bool{}
		if fd, ok := decl.(*ast.FuncDecl); ok {
			declared[fd.Name] = true
		}
		callees := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name *ast.Ident
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn
			case *ast.SelectorExpr:
				name = fn.Sel
			}
			if name == nil || !slices.Contains(credentialResolvers, name.Name) {
				return true
			}
			callees[name] = true
			lookup := "?"
			if len(call.Args) > 0 {
				switch last := call.Args[len(call.Args)-1].(type) {
				case *ast.Ident:
					lookup = last.Name
				case *ast.SelectorExpr:
					lookup = last.Sel.Name
				}
			}
			calls = append(calls, credentialCall{function, name.Name, lookup, fset.Position(name.Pos()).String()})
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains(credentialResolvers, id.Name) && !callees[id] && !declared[id] {
				calls = append(calls, credentialCall{function, id.Name, "no call", fset.Position(id.Pos()).String()})
			}
			return true
		})
	}
	return calls, nil
}

// enclosingFunction names the function a declaration is: "name", or
// "Receiver.name" for a method; "" for anything that is not a function.
func enclosingFunction(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	function := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		recv := fd.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			function = id.Name + "." + function
		}
	}
	return function
}

// readCommandNames are the names only the read command's own places may use:
// the command is the one entry to the lookup that selects by display name, and
// its completion source is the one entry to the completion that offers display
// names. A use of NewGetCloudCredential is allowed in buildEntries only as the
// value of an entry (readCommandEntryLabel), once in the module.
var readCommandNames = []string{"getCloudCredCmd", "NewGetCloudCredential", "GetCloudCredentialSource"}

// readCommandTypeLabel and readCommandAssertionLabel label the two declarations
// at the package level of the read command's file that may use readCommandNames:
// the type declaration of the command, and the blank-identifier assertion that
// it is a read command.
const (
	readCommandTypeLabel      = "type getCloudCredCmd"
	readCommandAssertionLabel = "var _"
)

// readCommandEntryLabel labels the one use of NewGetCloudCredential that may
// build the read command in the command tree. The use is in buildEntries, in a
// function literal that is the value of the key `make` of a composite literal;
// the literal has one statement, a return of one call of NewGetCloudCredential.
// Any other use in buildEntries keeps the label of the function.
const readCommandEntryLabel = "buildEntries entry"

// readCommandUses are the places that may use readCommandNames, by file and
// the functions in it: the constructor, every method, the type declaration and
// the interface assertion of the command's own file, the method that names its
// completion source, and the entry that builds the command.
var readCommandUses = []struct {
	file string
	// functions are exact names; a name ending in "." is every method of that
	// receiver; readCommandTypeLabel and readCommandAssertionLabel are the two
	// declarations at the package level; readCommandEntryLabel is the entry in
	// buildEntries.
	functions []string
}{
	{"internal/engine/provisioning/cloudcred.go", []string{"NewGetCloudCredential", "getCloudCredCmd.", readCommandTypeLabel, readCommandAssertionLabel}},
	{"internal/engine/provisioning/livesource.go", []string{"getCloudCredCmd.LiveSource"}},
	{"internal/cli/root.go", []string{readCommandEntryLabel}},
}

// readCommandUseAllowed reports whether function of file may use readCommandNames.
func readCommandUseAllowed(file, function string) bool {
	for _, u := range readCommandUses {
		if u.file != file {
			continue
		}
		for _, f := range u.functions {
			if f == function || (strings.HasSuffix(f, ".") && strings.HasPrefix(function, f)) {
				return true
			}
		}
	}
	return false
}

// packageLevelLabel names a declaration at the package level by what it
// declares: "type <name>", "var <names>" or "const <names>". The type
// declaration of the read command (not an alias) is readCommandTypeLabel; a
// blank-identifier declaration that is exactly the assertion
// `var _ runtime.ReadCommand = (*getCloudCredCmd)(nil)` is
// readCommandAssertionLabel, and any other `var _` is "var _ other".
func packageLevelLabel(tok token.Token, spec ast.Spec) string {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		switch {
		case spec.Name.Name != "getCloudCredCmd":
			return "type " + spec.Name.Name
		case spec.Assign.IsValid():
			return readCommandTypeLabel + " alias"
		}
		return readCommandTypeLabel
	case *ast.ValueSpec:
		names := make([]string, 0, len(spec.Names))
		for _, id := range spec.Names {
			names = append(names, id.Name)
		}
		label := tok.String() + " " + strings.Join(names, ", ")
		if label == readCommandAssertionLabel && !isReadCommandAssertion(spec) {
			return label + " other"
		}
		return label
	}
	return tok.String()
}

// isReadCommandAssertion reports whether spec is exactly
// `_ runtime.ReadCommand = (*getCloudCredCmd)(nil)`.
func isReadCommandAssertion(spec *ast.ValueSpec) bool {
	if len(spec.Names) != 1 || spec.Names[0].Name != "_" || len(spec.Values) != 1 {
		return false
	}
	typ, ok := spec.Type.(*ast.SelectorExpr)
	if !ok || typ.Sel.Name != "ReadCommand" {
		return false
	}
	if pkg, ok := typ.X.(*ast.Ident); !ok || pkg.Name != "runtime" {
		return false
	}
	call, ok := spec.Values[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != "nil" {
		return false
	}
	paren, ok := call.Fun.(*ast.ParenExpr)
	if !ok {
		return false
	}
	star, ok := paren.X.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "getCloudCredCmd"
}

// findReadCommandUses parses the Go source of path (read from the file when src
// is nil) and returns every use of readCommandNames, as the function it is in
// and the position. A use at the package level is labelled by the declaration
// it is in (packageLevelLabel); the use that is the entry in buildEntries has
// the label readCommandEntryLabel instead. The declaration of a function or a
// type of that name is not a use.
//
// The scan matches names: it does not see reflection, or the read command
// reached through a value of runtime.Command that buildEntries builds, which
// TestCommandsAreRunOnlyAtTheListedPlaces covers, because it lists the places
// that run a command. The behavior tests named at findCommandRuns cover the
// three mutations that take a cloud credential.
func findReadCommandUses(path string, src any) ([]credentialCall, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var uses []credentialCall
	entries := map[*ast.Ident]bool{}
	record := func(function string, nodes ...ast.Node) {
		for _, node := range nodes {
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && slices.Contains(readCommandNames, id.Name) {
					label := function
					if entries[id] {
						label = readCommandEntryLabel
					}
					uses = append(uses, credentialCall{label, id.Name, "use", fset.Position(id.Pos()).String()})
				}
				return true
			})
		}
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			function := enclosingFunction(decl)
			if slices.Contains(readCommandNames, decl.Name.Name) {
				// The declaration is not a use; its signature, receiver and body are.
				record(function, decl.Type)
				if decl.Recv != nil {
					record(function, decl.Recv)
				}
				if decl.Body != nil {
					record(function, decl.Body)
				}
				continue
			}
			if function == "buildEntries" {
				maps.Copy(entries, readCommandEntries(decl))
			}
			record(function, decl)
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				label := packageLevelLabel(decl.Tok, spec)
				if ts, ok := spec.(*ast.TypeSpec); ok && slices.Contains(readCommandNames, ts.Name.Name) {
					record(label, ts.Type)
					continue
				}
				record(label, spec)
			}
		}
	}
	return uses, nil
}

// readCommandEntries returns the identifier NewGetCloudCredential of each use
// in decl that has the shape of readCommandEntryLabel.
func readCommandEntries(decl *ast.FuncDecl) map[*ast.Ident]bool {
	entries := map[*ast.Ident]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "make" {
			return true
		}
		lit, ok := kv.Value.(*ast.FuncLit)
		if !ok || len(lit.Body.List) != 1 {
			return true
		}
		ret, ok := lit.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "NewGetCloudCredential" {
				entries[fn] = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "NewGetCloudCredential" {
				entries[fn.Sel] = true
			}
		}
		return true
	})
	return entries
}

// The type of the read command, the function that builds it and the directory
// of package provisioning.
const (
	readCommandTypeName        = "getCloudCredCmd"
	readCommandConstructorName = "NewGetCloudCredential"
	provisioningDirectory      = "internal/engine/provisioning"
)

// findReadCommandHandOuts parses the Go source of path, a slash-separated path
// relative to the module root (read from the file when src is nil), and returns
// a message for each hand-out of the read command, a place where it could reach
// code outside its constructor and its own methods: NewGetCloudCredential with
// a body that is not exactly one return of &getCloudCredCmd{...}; a method of
// getCloudCredCmd (with a named, unnamed or blank receiver) that names
// getCloudCredCmd or NewGetCloudCredential in its parameters, results or body,
// so no method builds another command; and a method of getCloudCredCmd that
// uses its receiver other than as the operand of a selector named clients, so
// no method assigns, returns, passes on, converts or takes as a method value
// its receiver. The rule is by name and needs no types: a local name that
// repeats the receiver's is reported too. It does not see a method of another
// type that returns the command; findReadCommandUses reports each use of the
// type's name outside its own places.
//
// The scan does not see reflection or a hand-out through the entry table of
// buildEntries; TestCommandsAreRunOnlyAtTheListedPlaces covers the run of that
// value. The behavior tests named at findCommandRuns cover the three mutations
// that take a cloud credential.
func findReadCommandHandOuts(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var handOuts []string
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		function := enclosingFunction(fd)
		switch {
		case fd.Recv == nil && fd.Name.Name == readCommandConstructorName:
			if !isReadCommandConstructorBody(fd.Body) {
				handOuts = append(handOuts, fmt.Sprintf("%s: %s is not exactly one return of &%s{...}; the command is built and returned, never kept", fset.Position(fd.Pos()), function, readCommandTypeName))
			}
		case fd.Recv != nil && strings.HasPrefix(function, readCommandTypeName+".") && fd.Body != nil:
			for _, part := range []ast.Node{fd.Type, fd.Body} {
				ast.Inspect(part, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && (id.Name == readCommandTypeName || id.Name == readCommandConstructorName) {
						handOuts = append(handOuts, fmt.Sprintf("%s: %s used in %q; a method of the read command builds no command", fset.Position(id.Pos()), id.Name, function))
					}
					return true
				})
			}
			names := fd.Recv.List[0].Names
			if len(names) != 1 || names[0].Name == "_" {
				continue
			}
			receiver := names[0].Name
			allowed := map[*ast.Ident]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					allowed[sel.Sel] = true
					if x, ok := sel.X.(*ast.Ident); ok && sel.Sel.Name == "clients" {
						allowed[x] = true
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == receiver && !allowed[id] {
					handOuts = append(handOuts, fmt.Sprintf("%s: %s used in %q; a method of the read command uses its receiver only as %s.clients", fset.Position(id.Pos()), receiver, function, receiver))
				}
				return true
			})
		}
	}
	return handOuts, nil
}

// isReadCommandConstructorBody reports whether body is exactly
// `return &getCloudCredCmd{...}`.
func isReadCommandConstructorBody(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) != 1 {
		return false
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	addr, ok := ret.Results[0].(*ast.UnaryExpr)
	if !ok || addr.Op != token.AND {
		return false
	}
	lit, ok := addr.X.(*ast.CompositeLit)
	if !ok {
		return false
	}
	typ, ok := lit.Type.(*ast.Ident)
	return ok && typ.Name == readCommandTypeName
}

// lookupDeclarationFile is the file whose package-level const block declares
// lookupForRead and lookupForChange.
const lookupDeclarationFile = "internal/engine/provisioning/cloudcred.go"

// findLookupNameMisuse parses the Go source of path, a slash-separated path
// relative to the module root (read from the file when src is nil), and returns
// a message for every misuse of the names lookupForRead and lookupForChange:
// any declaration of either name other than the package-level const block of
// lookupDeclarationFile that declares both, and any use of lookupForRead other
// than in the case list of the switch of resolveCredentialObj and in
// getCloudCredCmd.Execute, both of that file. A declaration is the name of a
// var or const spec (at the package level or in a function), the left side of
// a := (a type switch binding included), a parameter, result, receiver, struct
// field or interface method (function literals included), the key and value of
// a range with :=, and the name of a function, a type, a label or an import.
// Each message reads "<position>: <text>; <reason>".
//
// The scan matches names: it does not read the number of a lookup written as a
// conversion (credentialLookup(1)), which findCredentialCalls reports as `?`,
// and it does not see reflection. The behavior tests named at findCommandRuns
// cover the three mutations that take a cloud credential.
func findLookupNameMisuse(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	names := []string{readLookup, changeLookup}
	inFile := path == lookupDeclarationFile

	// allowedDeclarations are the names of the one package-level const block
	// that declares both.
	allowedDeclarations := map[*ast.Ident]bool{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST || !inFile {
			continue
		}
		var declared []*ast.Ident
		for _, spec := range gd.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				declared = append(declared, vs.Names...)
			}
		}
		if slices.ContainsFunc(declared, func(id *ast.Ident) bool { return id.Name == readLookup }) &&
			slices.ContainsFunc(declared, func(id *ast.Ident) bool { return id.Name == changeLookup }) {
			for _, id := range declared {
				allowedDeclarations[id] = true
			}
		}
	}

	declaring := map[*ast.Ident]bool{}
	declare := func(ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil {
				declaring[id] = true
			}
		}
	}
	declareDefined := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			if id, ok := e.(*ast.Ident); ok {
				declare(id)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			declare(n.Names...)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				declareDefined(n.Lhs...)
			}
		case *ast.Field:
			declare(n.Names...)
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				declareDefined(n.Key, n.Value)
			}
		case *ast.FuncDecl:
			declare(n.Name)
		case *ast.TypeSpec:
			declare(n.Name)
		case *ast.LabeledStmt:
			declare(n.Label)
		case *ast.ImportSpec:
			declare(n.Name)
		}
		return true
	})

	var misuse []string
	for _, decl := range file.Decls {
		function := enclosingFunction(decl)
		inCaseList := map[*ast.Ident]bool{}
		if inFile && function == "resolveCredentialObj" {
			ast.Inspect(decl, func(n ast.Node) bool {
				if cc, ok := n.(*ast.CaseClause); ok {
					for _, e := range cc.List {
						ast.Inspect(e, func(m ast.Node) bool {
							if id, ok := m.(*ast.Ident); ok {
								inCaseList[id] = true
							}
							return true
						})
					}
				}
				return true
			})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !slices.Contains(names, id.Name) {
				return true
			}
			at := fset.Position(id.Pos()).String()
			switch {
			case declaring[id] && allowedDeclarations[id]:
			case declaring[id]:
				misuse = append(misuse, fmt.Sprintf("%s: %s declared in %q; the package-level const block of %s declares both names, once", at, id.Name, function, lookupDeclarationFile))
			case id.Name != readLookup:
			case inFile && function == "getCloudCredCmd.Execute", inCaseList[id]:
			default:
				misuse = append(misuse, fmt.Sprintf("%s: %s used in %q; only the read command selects by display name", at, id.Name, function))
			}
			return true
		})
	}
	return misuse, nil
}

// scanDirectories returns the directories the module scan reads, sorted: the
// directory of each slash-separated path, and each parent of it up to and
// including the module root ".", each once.
func scanDirectories(paths []string) []string {
	seen := map[string]bool{}
	for _, p := range paths {
		for dir := path.Dir(p); !seen[dir]; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// moduleGoFiles lists the non-test Go files of the module as git sees them
// (every tracked file plus every untracked file no ignore rule covers), as
// slash-separated paths relative to root, leaving out testdata trees. A
// checkout carries ignored local files that are not part of the module.
//
// The git listing is not part of Go's test cache key, so a test that only ran
// it would pass from the cache after a file was added. The directories a test
// reads are part of the key. So the function also reads, with os.ReadDir, the
// directory of every path git lists (any extension, test files and testdata
// included) and each parent of it up to root (scanDirectories): an added,
// renamed or removed file in one of them, or a new directory under one,
// changes the key and the test runs again. One case is not covered: a new file
// in an existing directory that holds no listed file. Go puts into the key the
// stat of each directory the test reads (size, mode, modification time) and,
// for each entry in it, the name and the stat of the entry, the ignored ones
// too. A change to any of them makes the next run uncached; it never makes a
// wrong pass, only a rerun.
func moduleGoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	cmd.Env = testfs.EnvWithoutGit()
	out, err := cmd.Output()
	require.NoError(t, err, "the scan needs a git checkout")
	var listed []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			listed = append(listed, string(p))
		}
	}
	for _, dir := range scanDirectories(listed) {
		// A directory whose files were all deleted, with the deletion not
		// staged, is listed and gone: skipped, as a missing file is.
		if _, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
			require.ErrorIs(t, err, fs.ErrNotExist)
		}
	}
	var files []string
	for _, rel := range listed {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") ||
			slices.Contains(strings.Split(rel, "/"), "testdata") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			require.ErrorIs(t, err, fs.ErrNotExist)
			continue
		}
		files = append(files, rel)
	}
	return files
}

// repositoryRoot is the directory of go.mod above the test's directory.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test's directory")
		dir = parent
	}
}

// TestEveryCredentialSelectionNamesItsLookup asserts that every call of
// resolveCredentialObj, ResolveCloudCredential and readStoredCredential in a
// non-test Go file of the module is a row of credentialCallers, with its
// lookup written as a named constant (or passed on from a lookup parameter of
// one of the three), that no function value or other use hides a call, that
// lookupForRead is used only by the read command, that the names
// lookupForRead and lookupForChange are declared once, that the read command
// is used only in its own places and built by one entry of the command tree,
// and that every row still has its call.
func TestEveryCredentialSelectionNamesItsLookup(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	seen := map[string]bool{}
	var offenders, entryUses []string
	for _, rel := range moduleGoFiles(t, root) {
		calls, err := findCredentialCalls(filepath.Join(root, filepath.FromSlash(rel)), nil)
		require.NoError(t, err)
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		require.NoError(t, err)
		misuse, err := findLookupNameMisuse(rel, source)
		require.NoError(t, err)
		offenders = append(offenders, misuse...)
		for _, c := range calls {
			key := rel + " " + c.function + " " + c.callee
			switch {
			case c.lookup == "no call":
				offenders = append(offenders, fmt.Sprintf("%s: %s used as a value in %q", c.at, c.callee, c.function))
			case c.lookup == passedOn:
				if !slices.Contains(credentialResolvers, c.function) {
					offenders = append(offenders, fmt.Sprintf("%s: %s passes a lookup it did not receive in %q", c.at, c.callee, c.function))
				}
			case c.lookup == readLookup && key != readLookupCaller:
				offenders = append(offenders, fmt.Sprintf("%s: %s with %s in %q; only the read command selects by display name", c.at, c.callee, readLookup, c.function))
			case c.lookup != readLookup && c.lookup != changeLookup:
				offenders = append(offenders, fmt.Sprintf("%s: %s with %q in %q; pass %s or %s", c.at, c.callee, c.lookup, c.function, readLookup, changeLookup))
			default:
				seen[key+" "+c.lookup] = true
				if !slices.ContainsFunc(credentialCallers, func(r struct{ file, function, callee, lookup, command string }) bool {
					return r.file+" "+r.function+" "+r.callee+" "+r.lookup == key+" "+c.lookup
				}) {
					offenders = append(offenders, fmt.Sprintf("%s: %s with %s in %q is not in credentialCallers", c.at, c.callee, c.lookup, c.function))
				}
			}
		}
		uses, err := findReadCommandUses(filepath.Join(root, filepath.FromSlash(rel)), nil)
		require.NoError(t, err)
		for _, u := range uses {
			if u.function == readCommandEntryLabel {
				entryUses = append(entryUses, u.at)
			}
			if !readCommandUseAllowed(rel, u.function) {
				offenders = append(offenders, fmt.Sprintf("%s: %s used in %q; only the read command's own places use it", u.at, u.callee, u.function))
			}
		}
	}
	assert.Len(t, entryUses, 1, "the read command is built by one entry of the command tree: %v", entryUses)
	assert.Empty(t, offenders, "every call that selects a cloud credential names its lookup and has rows")
	for _, r := range credentialCallers {
		assert.True(t, seen[r.file+" "+r.function+" "+r.callee+" "+r.lookup], "%s %s is listed for %s and no longer calls it with %s; remove the row", r.file, r.function, r.callee, r.lookup)
	}
}

// TestTheCredentialSelectionScanFindsEveryCall asserts the rule of
// findCredentialCalls on small sources: a call of each function is found with
// its lookup, in a function, a method or at the package level, through a
// package name, and a function value is found; a declaration and another
// function are not.
func TestTheCredentialSelectionScanFindsEveryCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"a call in a function", "package p\n\nfunc f() { resolveCredentialObj(ctx, c, v, \"\", lookupForRead) }\n", []string{"f resolveCredentialObj lookupForRead"}},
		{"a call in a method", "package p\n\nfunc (c *T) g() { _, _ = readStoredCredential(ctx, c, v, lookupForChange) }\n", []string{"T.g readStoredCredential lookupForChange"}},
		{"a call through a package name", "package p\n\nfunc f() { provisioning.ResolveCloudCredential(ctx, c, v, lookupForChange) }\n", []string{"f ResolveCloudCredential lookupForChange"}},
		{"a lookup passed on", "package p\n\nfunc resolveCredentialObj(lookup int) { other(lookup) }\n\nfunc readStoredCredential(lookup int) { resolveCredentialObj(lookup) }\n", []string{"readStoredCredential resolveCredentialObj lookup"}},
		{"a lookup that is a literal", "package p\n\nfunc f() { resolveCredentialObj(ctx, c, v, \"\", 2) }\n", []string{"f resolveCredentialObj ?"}},
		{"a call at the package level", "package p\n\nvar _ = ResolveCloudCredential(ctx, c, v, lookupForChange)\n", []string{" ResolveCloudCredential lookupForChange"}},
		{"a function value", "package p\n\nfunc f() { g := ResolveCloudCredential; _ = g }\n", []string{"f ResolveCloudCredential no call"}},
		{"a function passed as an argument", "package p\n\nfunc f() { run(readStoredCredential) }\n", []string{"f readStoredCredential no call"}},
		{"a declaration only", "package p\n\nfunc readStoredCredential(lookup int) {}\n", nil},
		{"another function", "package p\n\nfunc f() { getCredential(ctx, c, n, nf) }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls, err := findCredentialCalls("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, c := range calls {
				got = append(got, c.function+" "+c.callee+" "+c.lookup)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheModuleScanListsEveryProductionGoFile asserts moduleGoFiles lists the
// non-test Go files of the whole module, the commands under cmd/ included, and
// leaves out test files, testdata trees and ignored local trees.
func TestTheModuleScanListsEveryProductionGoFile(t *testing.T) {
	t.Parallel()
	files := moduleGoFiles(t, repositoryRoot(t))
	assert.Contains(t, files, "internal/engine/provisioning/cloudcred.go")
	assert.Contains(t, files, "internal/cli/root.go")
	assert.True(t, slices.ContainsFunc(files, func(p string) bool { return strings.HasPrefix(p, "cmd/") }), "a file under cmd/ is listed")
	for _, p := range files {
		assert.True(t, strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go"), p)
		assert.NotContains(t, strings.Split(p, "/"), "testdata", p)
		assert.False(t, strings.HasPrefix(p, "docs/") || strings.HasPrefix(p, "graphify-out/"), p)
	}
}

// TestTheModuleScanReadsEveryDirectoryOfTheListing asserts that
// scanDirectories returns the directory of each listed path of any kind and
// each parent of it up to the module root, each once and sorted, so the
// module scan puts them in the test cache key.
func TestTheModuleScanReadsEveryDirectoryOfTheListing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"a nested path", []string{"a/b/c.go"}, []string{".", "a", "a/b"}},
		{"two paths in one directory", []string{"a/b/c.go", "a/b/d.go"}, []string{".", "a", "a/b"}},
		{"a path that is not a Go file", []string{"a/b/c.yaml"}, []string{".", "a", "a/b"}},
		{"a test file", []string{"x/y/z_test.go"}, []string{".", "x", "x/y"}},
		{"a file under testdata", []string{"p/testdata/g.json"}, []string{".", "p", "p/testdata"}},
		{"a path in the root", []string{"go.mod"}, []string{"."}},
		{"paths in different trees", []string{"a/b/c.go", "d/e.go", "go.mod"}, []string{".", "a", "a/b", "d"}},
		{"no path", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, scanDirectories(tc.paths))
		})
	}
}

// TestTheLookupNameScanFindsEveryMisuse asserts the rule of
// findLookupNameMisuse on small sources: every form that declares
// lookupForRead or lookupForChange outside the one package-level const block
// of cloudcred.go is found, so is a use of lookupForRead anywhere but the
// switch of resolveCredentialObj and getCloudCredCmd.Execute of that file, and
// the const block, those two uses and a use of lookupForChange pass.
func TestTheLookupNameScanFindsEveryMisuse(t *testing.T) {
	t.Parallel()
	const other = "internal/engine/provisioning/other.go"
	const declaring = lookupDeclarationFile
	const block = "const (\n\tlookupForRead credentialLookup = iota + 1\n\tlookupForChange\n)\n"
	for _, tc := range []struct {
		name, file, src string
		want            []string
	}{
		{"a var in a function", other, "package p\n\nfunc f() { var lookupForChange = 1; _ = lookupForChange }\n", []string{`lookupForChange declared in "f"`}},
		{"a const in a function", other, "package p\n\nfunc f() { const lookupForRead = 1 }\n", []string{`lookupForRead declared in "f"`}},
		{"a short variable declaration", other, "package p\n\nfunc f() { lookupForChange := 2; _ = lookupForChange }\n", []string{`lookupForChange declared in "f"`}},
		{"a type switch binding", other, "package p\n\nfunc f(v any) { switch lookupForChange := v.(type) { default: _ = lookupForChange } }\n", []string{`lookupForChange declared in "f"`}},
		{"a parameter", other, "package p\n\nfunc f(lookupForRead int) {}\n", []string{`lookupForRead declared in "f"`}},
		{"a result", other, "package p\n\nfunc f() (lookupForChange int) { return }\n", []string{`lookupForChange declared in "f"`}},
		{"a receiver", other, "package p\n\nfunc (lookupForChange T) f() {}\n", []string{`lookupForChange declared in "T.f"`}},
		{"a struct field", other, "package p\n\ntype T struct{ lookupForRead int }\n", []string{`lookupForRead declared in ""`}},
		{"an interface method", other, "package p\n\ntype I interface{ lookupForChange() }\n", []string{`lookupForChange declared in ""`}},
		{"a parameter of a function literal", other, "package p\n\nfunc f() { _ = func(lookupForRead int) {} }\n", []string{`lookupForRead declared in "f"`}},
		{"a range key", other, "package p\n\nfunc f(s []int) { for lookupForChange := range s { _ = lookupForChange } }\n", []string{`lookupForChange declared in "f"`}},
		{"a range value", other, "package p\n\nfunc f(s []int) { for _, lookupForRead := range s { _ = lookupForRead } }\n", []string{`lookupForRead declared in "f"`, `lookupForRead used in "f"`}},
		{"a function name", other, "package p\n\nfunc lookupForRead() {}\n", []string{`lookupForRead declared in "lookupForRead"`}},
		{"a type name", other, "package p\n\ntype lookupForChange int\n", []string{`lookupForChange declared in ""`}},
		{"a label", other, "package p\n\nfunc f() { lookupForRead: for { break lookupForRead } }\n", []string{`lookupForRead declared in "f"`, `lookupForRead used in "f"`}},
		{"an import name", other, "package p\n\nimport lookupForChange \"x\"\n", []string{`lookupForChange declared in ""`}},
		{"a package-level variable in another file", other, "package p\n\nvar lookupForChange = 3\n", []string{`lookupForChange declared in ""`}},
		{"the const block in another file", other, "package p\n\n" + block, []string{`lookupForRead declared in ""`, `lookupForChange declared in ""`}},
		{"a second const block in the declaring file", declaring, "package p\n\n" + block + "\nconst lookupForRead = 9\n", []string{`lookupForRead declared in ""`}},
		{"a use of lookupForRead in another function", other, "package p\n\nfunc f() { g(lookupForRead) }\n", []string{`lookupForRead used in "f"`}},
		{"a use of lookupForRead in the declaring file", declaring, "package p\n\nfunc f() { g(lookupForRead) }\n", []string{`lookupForRead used in "f"`}},
		{"a use in resolveCredentialObj outside the switch", declaring, "package p\n\nfunc resolveCredentialObj(lookup int) { if lookup == lookupForRead {} }\n", []string{`lookupForRead used in "resolveCredentialObj"`}},
		{"the read command's Execute in another file", other, "package p\n\nfunc (c *getCloudCredCmd) Execute() { resolveCredentialObj(ctx, c, v, \"\", lookupForRead) }\n", []string{`lookupForRead used in "getCloudCredCmd.Execute"`}},
		{"the const block in the declaring file", declaring, "package p\n\ntype credentialLookup int\n\n" + block, nil},
		{"the switch of resolveCredentialObj", declaring, "package p\n\nfunc resolveCredentialObj(lookup int) { switch lookup { case lookupForRead, lookupForChange: default: return } }\n", nil},
		{"the read command's Execute", declaring, "package p\n\nfunc (c *getCloudCredCmd) Execute() { resolveCredentialObj(ctx, c, v, \"\", lookupForRead) }\n", nil},
		{"a use of lookupForChange as an argument", other, "package p\n\nfunc f() { readStoredCredential(ctx, c, v, lookupForChange) }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			misuse, err := findLookupNameMisuse(tc.file, tc.src)
			require.NoError(t, err)
			var got []string
			for _, m := range misuse {
				_, text, _ := strings.Cut(m, ": ")
				text, _, _ = strings.Cut(text, ";")
				got = append(got, text)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheReadCommandScanFindsEveryUse asserts the rule of
// findReadCommandUses and readCommandUseAllowed on small sources: a use of
// the command's type, constructor or completion source is found as a parameter
// type, in a composite literal and in a call, a use at the package level is
// labelled by its declaration, the declarations are not uses, and only the
// listed places may use them. At the package level of the command's file only
// its type declaration and the interface assertion are listed; any other
// variable, constant, alias or type that names the command is an offender. In
// buildEntries only the use that is the whole result of the make function of an
// entry is the entry (readCommandEntryLabel); any other use there is an
// offender, and TestEveryCredentialSelectionNamesItsLookup asserts the entry
// occurs once in the module.
func TestTheReadCommandScanFindsEveryUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file, src string
		want            []string
		allowed         bool
	}{
		{"a parameter type", "internal/engine/provisioning/rotate.go", "package p\n\nfunc rotateCredential(c *getCloudCredCmd) error { _, err := c.Execute(ctx, in, nil); return err }\n", []string{"rotateCredential getCloudCredCmd"}, false},
		{"a composite literal", "internal/engine/provisioning/rotate.go", "package p\n\nfunc rotate() { _ = &getCloudCredCmd{} }\n", []string{"rotate getCloudCredCmd"}, false},
		{"a call of the constructor", "internal/cli/other.go", "package p\n\nfunc f() { provisioning.NewGetCloudCredential(c) }\n", []string{"f NewGetCloudCredential"}, false},
		{"a use at the package level", "internal/engine/provisioning/other.go", "package p\n\nvar _ = NewGetCloudCredential\n", []string{"var _ other NewGetCloudCredential"}, false},
		{"the constructor's own declaration", "internal/engine/provisioning/cloudcred.go", "package p\n\nfunc NewGetCloudCredential(c int) {}\n", nil, true},
		{"the constructor's body", "internal/engine/provisioning/cloudcred.go", "package p\n\nfunc NewGetCloudCredential(c int) runtime.Command { return &getCloudCredCmd{c} }\n", []string{"NewGetCloudCredential getCloudCredCmd"}, true},
		{"a method of the command", "internal/engine/provisioning/cloudcred.go", "package p\n\nfunc (c *getCloudCredCmd) Execute() {}\n", []string{"getCloudCredCmd.Execute getCloudCredCmd"}, true},
		{"the type's declaration and the interface assertion", "internal/engine/provisioning/cloudcred.go", "package p\n\ntype getCloudCredCmd struct{}\n\nvar _ runtime.ReadCommand = (*getCloudCredCmd)(nil)\n", []string{"var _ getCloudCredCmd"}, true},
		{"a package-level variable of the command", "internal/engine/provisioning/cloudcred.go", "package p\n\nvar readCmd = &getCloudCredCmd{}\n", []string{"var readCmd getCloudCredCmd"}, false},
		{"an alias of the command's type", "internal/engine/provisioning/cloudcred.go", "package p\n\ntype credReader = getCloudCredCmd\n", []string{"type credReader getCloudCredCmd"}, false},
		{"a defined type of the command's type", "internal/engine/provisioning/cloudcred.go", "package p\n\ntype credReader getCloudCredCmd\n", []string{"type credReader getCloudCredCmd"}, false},
		{"a package-level constant of the constructor", "internal/engine/provisioning/cloudcred.go", "package p\n\nconst c = NewGetCloudCredential\n", []string{"const c NewGetCloudCredential"}, false},
		{"a blank variable that hands the command on", "internal/engine/provisioning/cloudcred.go", "package p\n\nvar _ = register(&getCloudCredCmd{})\n", []string{"var _ other getCloudCredCmd"}, false},
		{"a blank variable of the interface that calls the constructor", "internal/engine/provisioning/cloudcred.go", "package p\n\nvar _ runtime.ReadCommand = NewGetCloudCredential(nil)\n", []string{"var _ other NewGetCloudCredential"}, false},
		{"a use of the variable in another file", "internal/engine/provisioning/rotate.go", "package p\n\nfunc rotate() { readCmd.Execute(ctx, in, nil) }\n", nil, true},
		{"the completion method", "internal/engine/provisioning/livesource.go", "package p\n\nfunc (*getCloudCredCmd) LiveSource() {}\n", []string{"getCloudCredCmd.LiveSource getCloudCredCmd"}, true},
		{"the same method in another file", "internal/engine/provisioning/other.go", "package p\n\nfunc (*getCloudCredCmd) LiveSource() {}\n", []string{"getCloudCredCmd.LiveSource getCloudCredCmd"}, false},
		{"the source of the read command in its method", "internal/engine/provisioning/livesource.go", "package p\n\nfunc (*getCloudCredCmd) LiveSource() { return complete.GetCloudCredentialSource() }\n", []string{"getCloudCredCmd.LiveSource getCloudCredCmd", "getCloudCredCmd.LiveSource GetCloudCredentialSource"}, true},
		{"the source of the read command in a method of a mutation that takes a cloud credential", "internal/engine/provisioning/livesource.go", "package p\n\nfunc (*deleteCloudCredentialCmd) LiveSource() { return complete.GetCloudCredentialSource() }\n", []string{"deleteCloudCredentialCmd.LiveSource GetCloudCredentialSource"}, false},
		{"a use in the registry function outside an entry", "internal/cli/root.go", "package p\n\nfunc buildEntries() { provisioning.NewGetCloudCredential(d.clients) }\n", []string{"buildEntries NewGetCloudCredential"}, false},
		{"the entry that builds the command", "internal/cli/root.go", "package p\n\nfunc buildEntries() []entry { return []entry{{group: \"get\", make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }}} }\n", []string{"buildEntries entry NewGetCloudCredential"}, true},
		{"the entry in a file that does not list it", "internal/cli/other.go", "package p\n\nfunc buildEntries() []entry { return []entry{{group: \"get\", make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }}} }\n", []string{"buildEntries entry NewGetCloudCredential"}, false},
		{"the command passed into another command's constructor", "internal/cli/root.go", "package p\n\nfunc buildEntries() []entry { return []entry{{group: \"x\", make: func(d runDeps) runtime.Command { return provisioning.NewOther(d.clients, provisioning.NewGetCloudCredential(d.clients)) }}} }\n", []string{"buildEntries NewGetCloudCredential"}, false},
		{"the command kept in a local variable and returned", "internal/cli/root.go", "package p\n\nfunc buildEntries() []entry { return []entry{{group: \"x\", make: func(d runDeps) runtime.Command { cmd := provisioning.NewGetCloudCredential(d.clients); return cmd }}} }\n", []string{"buildEntries NewGetCloudCredential"}, false},
		{"the call outside a make function", "internal/cli/root.go", "package p\n\nfunc buildEntries() []entry { return []entry{{group: \"x\", build: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }}} }\n", []string{"buildEntries NewGetCloudCredential"}, false},
		{"a second entry that builds the command", "internal/cli/root.go", "package p\n\nfunc buildEntries() []entry { return []entry{{make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }}, {make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }}} }\n", []string{"buildEntries entry NewGetCloudCredential", "buildEntries entry NewGetCloudCredential"}, true},
		{"another function", "internal/engine/provisioning/other.go", "package p\n\nfunc f() { NewGetCloudCredentialX() }\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			uses, err := findReadCommandUses("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			allowed := true
			for _, u := range uses {
				got = append(got, u.function+" "+u.callee)
				allowed = allowed && readCommandUseAllowed(tc.file, u.function)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.allowed, allowed)
		})
	}
}

// TestTheReadCommandIsNeverHandedOut asserts that in every non-test file of
// package provisioning the constructor of the read command is exactly one
// return of &getCloudCredCmd{...}, the methods of the command use their
// receiver only as the operand of .clients, and no method names the type or its
// constructor.
func TestTheReadCommandIsNeverHandedOut(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	var offenders []string
	checked := 0
	for _, rel := range moduleGoFiles(t, root) {
		if path.Dir(rel) != provisioningDirectory {
			continue
		}
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		require.NoError(t, err)
		handOuts, err := findReadCommandHandOuts(rel, source)
		require.NoError(t, err)
		offenders = append(offenders, handOuts...)
		if bytes.Contains(source, []byte("func "+readCommandConstructorName+"(")) {
			checked++
		}
	}
	assert.Equal(t, 1, checked, "the file of the constructor is scanned")
	assert.Empty(t, offenders, "the read command is built and returned only")
}

// TestTheReadCommandHandOutScanFindsEveryForm asserts the rule of
// findReadCommandHandOuts on small sources: a constructor that keeps the value
// or has more than one statement, and a method that assigns, returns, passes
// on, converts or takes as a method value its receiver, and a method that
// builds another command (a composite literal, new, or a call of the
// constructor, with any receiver) or names the command in its parameters or
// results, are found; the real constructor, a use of the receiver as the
// operand of .clients, a method with no receiver name and a method that does
// not use its receiver pass.
func TestTheReadCommandHandOutScanFindsEveryForm(t *testing.T) {
	t.Parallel()
	const notOneReturn = "NewGetCloudCredential is not exactly one return of &getCloudCredCmd{...}"
	const constructor = "func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {\n\treturn &getCloudCredCmd{clients: clients}\n}\n"
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"the real constructor", constructor, nil},
		{"a constructor that appends the value to a package-level slice and returns it", "func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {\n\tcmd := &getCloudCredCmd{clients: clients}\n\treaders = append(readers, cmd)\n\treturn cmd\n}\n", []string{notOneReturn}},
		{"a constructor of two statements", "func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {\n\tregister()\n\treturn &getCloudCredCmd{clients: clients}\n}\n", []string{notOneReturn}},
		{"a constructor that returns a value of another form", "func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {\n\treturn getCloudCredCmd{clients: clients}\n}\n", []string{notOneReturn}},
		{"a constructor with no body", "func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command\n", []string{notOneReturn}},
		{"a use of the receiver as the operand of clients", "func (c *getCloudCredCmd) Execute() { use(c.clients) }\n", nil},
		{"a method with an unnamed receiver", "func (*getCloudCredCmd) LiveSource() { use(complete.GetCloudCredentialSource()) }\n", nil},
		{"a method with a blank receiver", "func (_ *getCloudCredCmd) Describe() { use(1) }\n", nil},
		{"a method that does not use its receiver", "func (c *getCloudCredCmd) Describe() { use(1) }\n", nil},
		{"a method of another type", "func (c *other) Describe() { shared = c }\n", nil},
		{"a method that returns its receiver", "func (c *getCloudCredCmd) Describe() runtime.Command { return c }\n", []string{`c used in "getCloudCredCmd.Describe"`}},
		{"a method that assigns its receiver", "func (c *getCloudCredCmd) Describe() { shared = c }\n", []string{`c used in "getCloudCredCmd.Describe"`}},
		{"a method that takes its method as a value", "func (c *getCloudCredCmd) Describe() { shared = c.Execute }\n", []string{`c used in "getCloudCredCmd.Describe"`}},
		{"a method that starts a goroutine with its receiver", "func (c *getCloudCredCmd) Execute() { go use(c) }\n", []string{`c used in "getCloudCredCmd.Execute"`}},
		{"a method that passes its receiver on", "func (c *getCloudCredCmd) Execute() { f(c) }\n", []string{`c used in "getCloudCredCmd.Execute"`}},
		{"a method that converts its receiver", "func (c *getCloudCredCmd) Execute() { _ = runtime.Command(c) }\n", []string{`c used in "getCloudCredCmd.Execute"`}},
		{"a method of a value receiver that returns it", "func (c getCloudCredCmd) Describe() any { return c }\n", []string{`c used in "getCloudCredCmd.Describe"`}},
		{"a use in a function literal", "func (c *getCloudCredCmd) Execute() { go func() { f(c) }() }\n", []string{`c used in "getCloudCredCmd.Execute"`}},
		{"a method that stores a new composite literal of the command", "func (c *getCloudCredCmd) Describe() { shared = &getCloudCredCmd{clients: c.clients} }\n", []string{`getCloudCredCmd used in "getCloudCredCmd.Describe"`}},
		{"a method with an unnamed receiver that calls the constructor", "func (*getCloudCredCmd) LiveSource() { shared = NewGetCloudCredential(nil) }\n", []string{`NewGetCloudCredential used in "getCloudCredCmd.LiveSource"`}},
		{"a method with a blank receiver that calls the constructor", "func (_ *getCloudCredCmd) Describe() { shared = NewGetCloudCredential(nil) }\n", []string{`NewGetCloudCredential used in "getCloudCredCmd.Describe"`}},
		{"a method that builds the command with new", "func (c *getCloudCredCmd) Describe() { shared = new(getCloudCredCmd) }\n", []string{`getCloudCredCmd used in "getCloudCredCmd.Describe"`}},
		{"a method that returns a value of the command", "func (c *getCloudCredCmd) Describe() any { return getCloudCredCmd{} }\n", []string{`getCloudCredCmd used in "getCloudCredCmd.Describe"`}},
		{"a method that names the command in its results", "func (c *getCloudCredCmd) Describe() *getCloudCredCmd { return nil }\n", []string{`getCloudCredCmd used in "getCloudCredCmd.Describe"`}},
		{"a method that names the command in its parameters", "func (c *getCloudCredCmd) Describe(o *getCloudCredCmd) {}\n", []string{`getCloudCredCmd used in "getCloudCredCmd.Describe"`}},
		{"a method of another type that calls the constructor", "func (c *other) Describe() { shared = NewGetCloudCredential(nil) }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handOuts, err := findReadCommandHandOuts("internal/engine/provisioning/p.go", "package p\n\n"+tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range handOuts {
				_, text, _ := strings.Cut(h, ": ")
				text, _, _ = strings.Cut(text, ";")
				got = append(got, text)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// changingRow is one value typed for a credential and what each mutation that
// takes a cloud credential returns.
type changingRow struct {
	name, value string
	// code is the error code the command returns, "" when it selects
	// selected.
	code       string
	selected   string
	candidates []clierr.Candidate
}

// changingRows are the values every mutation that takes a cloud credential
// gets. The credentials are cc-abcde, whose display name is "prod", and cc-other, whose
// display name is cc-abcde.
var changingRows = []changingRow{
	{name: "a value that is only a display name", value: "prod", code: clierr.CodeNotFound,
		candidates: []clierr.Candidate{{ID: "cattle-global-data:cc-abcde", Name: "prod"}}},
	{name: "a prefixed value that is only a display name", value: "cattle-global-data:prod", code: clierr.CodeNotFound,
		candidates: []clierr.Candidate{{ID: "cattle-global-data:cc-abcde", Name: "prod"}}},
	{name: "the id of one credential and the display name of another", value: "cc-abcde", code: clierr.CodeAmbiguousSelection,
		candidates: []clierr.Candidate{{ID: "cattle-global-data:cc-abcde", Name: "prod"}, {ID: "cattle-global-data:cc-other", Name: "cc-abcde"}}},
	{name: "the prefixed id of one credential and the display name of another", value: "cattle-global-data:cc-abcde", selected: "cattle-global-data:cc-abcde"},
	{name: "the id of a credential that is no display name", value: "cc-other", selected: "cattle-global-data:cc-other"},
	{name: "a value that matches nothing", value: "nothing", code: clierr.CodeNotFound,
		candidates: []clierr.Candidate{{ID: "cattle-global-data:cc-abcde", Name: "prod"}, {ID: "cattle-global-data:cc-other", Name: "cc-abcde"}}},
}

// changingCredentials are the two credentials of changingRows, as ccpStored
// builds them.
func changingCredentials() []*unstructured.Unstructured {
	other := secretObj("cc-other", map[string]string{"field.cattle.io/name": ccpID}, map[string]any{"amazonec2credentialConfig-x": "x"})
	return []*unstructured.Unstructured{ccpStored("amazonec2", "7"), other}
}

// changingCommands run a mutation that takes a cloud credential, through its
// Plan, with a credential value and return the credential it selected, as the
// id written "cattle-global-data:<id>", or the error. The keys are the
// commands of credentialCallers.
var changingCommands = map[string]func(t *testing.T, value string) (string, error){
	"create cluster": func(t *testing.T, value string) (string, error) {
		t.Helper()
		client := &ccRecClient{list: listOf(changingCredentials()...)}
		cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})
		_, _, err := planCluster(t, cmd, ccInput{
			a: []string{"node-1"},
			s: map[string]string{"machine-config": "amazonec2config/mycfg", "cloud-credential": value, "k8s-version": "v1.30.5+rke2r1"},
			b: map[string]bool{"worker": true},
		})
		if err != nil {
			return "", err
		}
		id, _, _ := unstructured.NestedString(client.created.Object, "spec", "cloudCredentialSecretName")
		return id, nil
	},
	"patch cloud-credential": func(t *testing.T, value string) (string, error) {
		t.Helper()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		r.client.secrets = listOf(changingCredentials()...)
		eff, _, err := r.plan(t, ccwInput{args: []string{value}, description: new("changed")})
		if err != nil {
			return "", err
		}
		return "cattle-global-data:" + eff.DisplayName, nil
	},
	"delete cloud-credential": func(t *testing.T, value string) (string, error) {
		t.Helper()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		r.client.secrets = listOf(changingCredentials()...)
		eff, _, err := r.plan(t, ccwInput{args: []string{value}})
		if err != nil {
			return "", err
		}
		return "cattle-global-data:" + eff.DisplayName, nil
	},
}

// TestEveryMutationSelectsACredentialByID asserts, for every command
// credentialCallers lists as a mutation that takes a cloud credential, that a
// value that is only a display name is NOT_FOUND naming the credentials of that
// display name, that a value that is the id of one credential and the display
// name of another is AMBIGUOUS_SELECTION naming both, written with
// "cattle-global-data:", that the prefixed form selects the credential with
// that id, and that no other credential is selected. The read command still
// selects by display name.
func TestEveryMutationSelectsACredentialByID(t *testing.T) {
	t.Parallel()
	var commands []string
	for _, r := range credentialCallers {
		if r.lookup == changeLookup {
			commands = append(commands, r.command)
		}
	}
	require.NotEmpty(t, commands)
	for _, command := range commands {
		run, ok := changingCommands[command]
		require.True(t, ok, "credentialCallers lists %q as a mutation that takes a cloud credential and changingCommands has no rows for it", command)
		for _, row := range changingRows {
			t.Run(command+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				selected, err := run(t, row.value)
				if row.code == "" {
					require.NoError(t, err)
					assert.Equal(t, row.selected, selected)
					return
				}
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, row.code, ce.Code)
				assert.Equal(t, row.candidates, ce.Candidates)
				assert.Empty(t, selected)
			})
		}
	}
	for command := range changingCommands {
		assert.True(t, slices.ContainsFunc(credentialCallers, func(r struct{ file, function, callee, lookup, command string }) bool {
			return r.command == command && r.lookup == changeLookup
		}), "%q has rows and credentialCallers does not list it", command)
	}

	t.Run("get cloud-credential keeps the display name", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{list: listOf(changingCredentials()...)}
		res, err := NewGetCloudCredential(fakeFactory{c: client}).(runtime.ReadCommand).Execute(t.Context(), in{args: []string{"prod"}}, nil)
		require.NoError(t, err)
		rows, ok := res.Data.(cloudCredRows)
		require.True(t, ok)
		require.Len(t, rows, 1)
		assert.Equal(t, "cc-abcde", rows[0].SecretName)
	})
}
