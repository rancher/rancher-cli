package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runtimeRowsFile = "internal/runtime/remedysites_test.go"

// runtimeRows is what the table of rows in internal/runtime names: the number
// of rows of each site (the map literal siteRowCounts), the payload each row
// of an error holds (the type of the literal that is the first argument of
// withDetails inside the literal errorRows), as "<package directory> <type>",
// and the channels of the rows of each payload, as the field paths those rows
// name after the type's name.
type runtimeRows struct {
	counts   map[string]int
	payloads []string
	channels map[string][]string
}

// readRuntimeRows reads the row data from the source of a test file, with no
// type check. A payload is read only where a literal is the first argument of
// withDetails, so a literal nested in it names no payload.
func readRuntimeRows(t *testing.T, name string, src any) runtimeRows {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	require.NoError(t, err)
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		alias := lastElement(p)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		imports[alias] = p
	}
	rows := runtimeRows{counts: map[string]int{}, channels: map[string][]string{}}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				switch id.Name {
				case "siteRowCounts":
					lit, ok := vs.Values[i].(*ast.CompositeLit)
					require.True(t, ok, "siteRowCounts is a map literal")
					for _, e := range lit.Elts {
						kv := e.(*ast.KeyValueExpr)
						key, err := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
						require.NoError(t, err)
						n, err := strconv.Atoi(kv.Value.(*ast.BasicLit).Value)
						require.NoError(t, err)
						rows.counts[key] = n
					}
				case "errorRows":
					list, ok := vs.Values[i].(*ast.CompositeLit)
					require.True(t, ok, "errorRows is a slice literal")
					for _, e := range list.Elts {
						row, ok := e.(*ast.CompositeLit)
						if !ok || len(row.Elts) == 0 {
							continue
						}
						payload := rowPayload(row, imports)
						if payload == "" {
							continue
						}
						channel, err := strconv.Unquote(row.Elts[0].(*ast.BasicLit).Value)
						require.NoError(t, err)
						rows.payloads = append(rows.payloads, payload)
						_, typeName, _ := strings.Cut(payload, " ")
						rows.channels[payload] = append(rows.channels[payload], strings.TrimPrefix(channel, typeName+"."))
					}
				}
			}
		}
	}
	slices.Sort(rows.payloads)
	rows.payloads = slices.Compact(rows.payloads)
	return rows
}

// rowPayload returns the payload of one row of errorRows, as "<package
// directory> <type>": the type of the literal that is the first argument of
// the first call of withDetails in the row, or "" for a row with none.
func rowPayload(row *ast.CompositeLit, imports map[string]string) string {
	var payload string
	ast.Inspect(row, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || payload != "" {
			return payload == ""
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "withDetails" || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				payload = strings.TrimPrefix(imports[pkg.Name], modulePrefix) + " " + sel.Sel.Name
			}
		}
		return true
	})
	return payload
}

// lastElement is the last element of an import path.
func lastElement(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// remedyFieldSource says where the test that keeps a payload's field
// classification holds its list of remedy fields: the file, the package
// variable that is the composite literal of the classification, and the key
// of that literal that lists the remedy fields.
type remedyFieldSource struct{ file, variable, key string }

// remedyFieldSources has one entry for each production payload with a
// MapText method, so a payload whose rows cannot be compared with its remedy
// fields fails. The lists are read from the tests that classify every field
// of the type, which fail when a field is in no list.
var remedyFieldSources = map[string]remedyFieldSource{
	"internal/clierr CommittedContextDetails":               {"internal/clierr/remedymap_test.go", "committedContextFields", "remedy"},
	"internal/clierr LoginFailureDetails":                   {"internal/clierr/remedymap_test.go", "loginFailureFields", "remedy"},
	"internal/engine/rbac RevokePartialFailure":             {"internal/engine/rbac/revoke_maptext_test.go", "revokeFields", "Remedy"},
	"internal/engine/resources DeleteProjectPartialFailure": {"internal/engine/resources/deleteproject_maptext_test.go", "deleteProjectFields", "Remedy"},
}

// readRemedyFields reads the strings of the key of the composite literal
// that is the package variable named variable in the source of a test file,
// with no type check. The brackets of a slice element ("Objects[].Message")
// are dropped, as the channel of a row names the path without them.
func readRemedyFields(t *testing.T, name string, src any, variable, key string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	require.NoError(t, err)
	var out []string
	found := false
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if id.Name != variable || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				require.True(t, ok, "%s in %s is a composite literal", variable, name)
				for _, e := range lit.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok || kv.Key.(*ast.Ident).Name != key {
						continue
					}
					found = true
					list, ok := kv.Value.(*ast.CompositeLit)
					require.True(t, ok, "%s.%s in %s is a slice literal", variable, key, name)
					for _, item := range list.Elts {
						field, err := strconv.Unquote(item.(*ast.BasicLit).Value)
						require.NoError(t, err)
						out = append(out, strings.ReplaceAll(field, "[]", ""))
					}
				}
			}
		}
	}
	require.True(t, found, "%s in %s has no key %s", variable, name, key)
	slices.Sort(out)
	return out
}

// compareRuntimeRows returns each way the rows and the two lists of the scans
// disagree: a site in one list and not the other, a site with fewer rows than
// occurrences, a payload with a MapText method that no row holds or a row
// for a type that has none, and, for each payload, a remedy field with no
// row or a row for a field that is not a remedy field. remedyFields holds the
// remedy fields of each payload with MapText.
func compareRuntimeRows(rows runtimeRows, siteList map[string]int, mapTextPayloads []string, remedyFields map[string][]string) []string {
	var out []string
	for _, site := range slices.Sorted(maps.Keys(siteList)) {
		n, ok := rows.counts[site]
		switch {
		case !ok:
			out = append(out, "site "+site+" is in the scan's list and has no rows")
		case n < siteList[site]:
			out = append(out, "site "+site+" has "+strconv.Itoa(n)+" row(s) for "+strconv.Itoa(siteList[site])+" occurrence(s)")
		}
	}
	for _, site := range slices.Sorted(maps.Keys(rows.counts)) {
		if _, ok := siteList[site]; !ok {
			out = append(out, "site "+site+" has rows and is not in the scan's list")
		}
	}
	for _, p := range mapTextPayloads {
		if !slices.Contains(rows.payloads, p) {
			out = append(out, "payload "+p+" has a MapText method and no row")
		}
	}
	for _, p := range rows.payloads {
		if !slices.Contains(mapTextPayloads, p) {
			out = append(out, "payload "+p+" has a row and no MapText method")
		}
	}
	for _, p := range mapTextPayloads {
		fields, listed := remedyFields[p]
		if !listed {
			out = append(out, "payload "+p+" has a MapText method and no list of remedy fields")
			continue
		}
		_, typeName, _ := strings.Cut(p, " ")
		for _, field := range fields {
			if !slices.Contains(rows.channels[p], field) {
				out = append(out, "payload "+p+" has the remedy field "+field+" and no row for "+typeName+"."+field)
			}
		}
		for _, channel := range rows.channels[p] {
			if !slices.Contains(fields, channel) {
				out = append(out, "payload "+p+" has a row for "+typeName+"."+channel+", which is not one of its remedy fields")
			}
		}
	}
	return out
}

// productionMapTextPayloads are the types of the production tree that have a
// MapText method, less the error itself and the test helper.
func productionMapTextPayloads(t *testing.T) []string {
	t.Helper()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var out []string
	for key, ty := range textDetailsTypes(tree.files, tree.paths) {
		if ty.mapText && key != "internal/clierr Error" && key != "internal/remedytest MarkedDetails" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// productionRemedyFields reads the remedy fields of every production payload
// with MapText from the tests that classify its fields.
func productionRemedyFields(t *testing.T, payloads []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, p := range payloads {
		if src, ok := remedyFieldSources[p]; ok {
			out[p] = readRemedyFields(t, src.file, nil, src.variable, src.key)
		}
	}
	return out
}

// TestRuntimeRowsMatchTheScans asserts the rows of internal/runtime and the
// scans of this package agree by key: the sites of siteRowCounts are the
// sites of expansionSiteList, each with at least as many rows as
// occurrences, and the payloads the rows of an error name are the production
// types with MapText. For each of those payloads, the field paths its rows
// name are its remedy fields, read from the test that classifies the fields
// of the type. A site, a payload or a remedy field added to one side fails
// until the other has it. The comparison cannot tell whether a row's drive
// runs the site its key names: the row of a site that stands in for another
// (the one of renderDepsError) says so, and review checks the rest.
func TestRuntimeRowsMatchTheScans(t *testing.T) {
	t.Parallel()
	rows := readRuntimeRows(t, runtimeRowsFile, nil)
	require.NotEmpty(t, rows.counts)
	require.NotEmpty(t, rows.payloads)
	payloads := productionMapTextPayloads(t)
	assert.Empty(t, compareRuntimeRows(rows, expansionSiteList, payloads, productionRemedyFields(t, payloads)))
	for p := range remedyFieldSources {
		assert.Contains(t, payloads, p, "the list of remedy fields of %s matches no production payload with MapText", p)
	}
}

// TestRuntimeRowReaderReadsOnlyTheRowPayloads asserts the reader on a source
// with nested literals: the payload of a row is the literal handed to
// withDetails, not the literals inside it, the channels of a payload are the
// first values of its rows less the type's name, and a comparison reports a
// site, a payload or a remedy field that one side lacks.
func TestRuntimeRowReaderReadsOnlyTheRowPayloads(t *testing.T) {
	t.Parallel()
	const src = `package runtime_test

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

var siteRowCounts = map[string]int{
	"internal/runtime.a ExpandText":  1,
	"internal/runtime.b ExpandError": 3,
}

var errorRows = []row{
	{"message", func(s string) *clierr.Error { return clierr.New("x", s) }},
	{"LoginFailureDetails.Primary.Details", false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Primary: clierr.PrimaryFailure{Details: clierr.CommittedContextDetails{Recovery: s}}})
	}},
	{"LoginFailureDetails.Recovery", false, func(s string) *clierr.Error {
		return withDetails(clierr.LoginFailureDetails{Recovery: s})
	}},
	{"RevokePartialFailure.Failed.Message", false, func(s string) *clierr.Error {
		return withDetails(rbac.RevokePartialFailure{Failed: rbac.RevokeFailure{Message: s}})
	}},
}
`
	rows := readRuntimeRows(t, "x_test.go", src)
	assert.Equal(t, map[string]int{"internal/runtime.a ExpandText": 1, "internal/runtime.b ExpandError": 3}, rows.counts)
	const login, revoke = "internal/clierr LoginFailureDetails", "internal/engine/rbac RevokePartialFailure"
	assert.Equal(t, []string{login, revoke}, rows.payloads)
	assert.Equal(t, map[string][]string{
		login:  {"Primary.Details", "Recovery"},
		revoke: {"Failed.Message"},
	}, rows.channels)

	list := map[string]int{"internal/runtime.a ExpandText": 1, "internal/runtime.b ExpandError": 1}
	both := []string{login, revoke}
	fields := map[string][]string{login: {"Primary.Details", "Recovery"}, revoke: {"Failed.Message"}}
	assert.Empty(t, compareRuntimeRows(rows, list, both, fields))
	assert.Equal(t, []string{"site internal/runtime.c ExpandText is in the scan's list and has no rows"},
		compareRuntimeRows(rows, map[string]int{"internal/runtime.a ExpandText": 1, "internal/runtime.b ExpandError": 1, "internal/runtime.c ExpandText": 1}, both, fields))
	assert.Equal(t, []string{"site internal/runtime.b ExpandError has rows and is not in the scan's list"},
		compareRuntimeRows(rows, map[string]int{"internal/runtime.a ExpandText": 1}, both, fields))
	assert.Equal(t, []string{"site internal/runtime.a ExpandText has 1 row(s) for 2 occurrence(s)"},
		compareRuntimeRows(rows, map[string]int{"internal/runtime.a ExpandText": 2, "internal/runtime.b ExpandError": 1}, both, fields))
	const deleted = "internal/engine/resources DeleteProjectPartialFailure"
	assert.Equal(t, []string{
		"payload " + deleted + " has a MapText method and no row",
		"payload " + deleted + " has a MapText method and no list of remedy fields",
	}, compareRuntimeRows(rows, list, append(slices.Clone(both), deleted), fields))
	assert.Equal(t, []string{"payload " + revoke + " has a row and no MapText method"},
		compareRuntimeRows(rows, list, both[:1], fields))
	assert.Equal(t, []string{"payload " + login + " has the remedy field Note and no row for LoginFailureDetails.Note"},
		compareRuntimeRows(rows, list, both, map[string][]string{login: {"Primary.Details", "Recovery", "Note"}, revoke: {"Failed.Message"}}))
	assert.Equal(t, []string{"payload " + login + " has a row for LoginFailureDetails.Recovery, which is not one of its remedy fields"},
		compareRuntimeRows(rows, list, both, map[string][]string{login: {"Primary.Details"}, revoke: {"Failed.Message"}}))
}

// TestRemedyFieldReaderReadsTheRemedyKey asserts the reader of a field
// classification on a source with three lists: it returns the strings of the
// named key only, sorted, with the brackets of a slice element dropped.
func TestRemedyFieldReaderReadsTheRemedyKey(t *testing.T) {
	t.Parallel()
	const src = `package p

var other = lists{Remedy: []string{"Wrong"}}

var fields = lists{
	Remedy: []string{"Objects[].Message", "Hint"},
	Server: []string{"Objects[].Name"},
}
`
	assert.Equal(t, []string{"Hint", "Objects.Message"}, readRemedyFields(t, "x_test.go", src, "fields", "Remedy"))
	assert.Equal(t, []string{"Objects.Name"}, readRemedyFields(t, "x_test.go", src, "fields", "Server"))
}

// TestEveryRemedyFieldListIsReadFromARealClassification asserts the file,
// variable and key of each entry of remedyFieldSources exist in the tree
// and name a non-empty list.
func TestEveryRemedyFieldListIsReadFromARealClassification(t *testing.T) {
	t.Parallel()
	for payload, src := range remedyFieldSources {
		assert.NotEmpty(t, readRemedyFields(t, src.file, nil, src.variable, src.key), payload)
	}
}
