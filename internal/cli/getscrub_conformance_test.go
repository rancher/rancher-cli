package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// getOutputFormats are the formats every get row runs in: the two structured
// formats, a template that prints the whole object, and the names.
var getOutputFormats = []string{"json", "yaml", output.TemplatePrefix + "{{.}}", "name"}

// getUnrowedCommands are the get commands no kind row generates a row for,
// each with the reason it cannot show a value a read hides.
var getUnrowedCommands = map[string]string{
	"get clusterregistrationtoken": "shows the token on purpose, and builds its own rows from the Steve answer instead of the object table",
	"get cloud-credential":         "builds its rows from the Secret's metadata and the names of its data keys",
	"get kubernetes-versions":      "builds its rows from named fields; no server object is read",
}

// getShownAllowlist names the kinds whose get shows planted values the
// redaction of a preview hides, each with the reason.
var getShownAllowlist = map[string]string{
	"secret": "a secret's data is what get secret is for",
}

// getFormatClass says how a get output format is covered: by the rows (a
// run of the command in that format) or by the check of the columns a text
// table prints.
type getFormatClass struct {
	by, reason string
}

// getFormatClasses classifies every output format a command accepts: the
// -o values and the template prefix.
var getFormatClasses = map[string]getFormatClass{
	"json":     {by: "rows"},
	"yaml":     {by: "rows"},
	"name":     {by: "rows"},
	"template": {by: "rows"},
	"table":    {by: "columns", reason: "prints only the cells of the kind's Columns, or the columns the server's own table names"},
	"wide":     {by: "columns", reason: "prints only the cells of the kind's Columns, or the columns the server's own table names"},
}

// getColumnCountAllowlist names the Count columns over a hidden path, by
// kind key and header, each with the reason it shows only a length. A count
// shows no value, but nothing else is allowed over a hidden path.
var getColumnCountAllowlist = map[string]string{}

// getListItems returns the two objects a list of the row's kind answers.
func getListItems(f *scrubFake, k *kinds.Kind) func(gvr schema.GroupVersionResource) []unstructured.Unstructured {
	return func(gvr schema.GroupVersionResource) []unstructured.Unstructured {
		if !f.isKind(gvr) {
			return nil
		}
		ns := ""
		if k.Policy == kinds.NSPlain || k.Policy == kinds.NSWorkspace {
			ns = "ns1"
		}
		var items []unstructured.Unstructured
		for _, name := range []string{scrubObjectName, "obj2"} {
			items = append(items, *f.base(gvr, ns, name))
		}
		return items
	}
}

// withReadyCondition makes a served object satisfy a wait for the Ready
// condition.
func withReadyCondition(obj *unstructured.Unstructured) {
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
}

// Names of the get flags the rows set. The rest are named by the exported
// constants of the packages that declare them.
const (
	getFlagCluster   = "cluster"
	getFlagNamespace = "namespace"
	getFlagWait      = "wait"
	getFlagFor       = "for"
)

// getFlagsNotRowed are the flags of a generated get descriptor no row sets,
// each with the reason a row for it would show nothing new.
var getFlagsNotRowed = map[string]string{
	"timeout":        "sets how long the wait lasts; the wait rows run the wait itself",
	"project":        "keeps the listed objects that lie in one project's namespaces; it never changes an object's content",
	"include-system": "keeps or drops listed objects; it never changes an object's content",
}

// getFlagInput is the flags one get row sets. A flag is a field, so a
// misspelled one does not compile; names reports the flags the row sets.
type getFlagInput struct {
	cluster, namespace string
	selector           string
	forExpr            string
	wait               bool
}

func (f getFlagInput) values() map[string]string {
	out := map[string]string{}
	for name, v := range map[string]string{
		getFlagCluster: f.cluster, getFlagNamespace: f.namespace,
		resources.FlagSelector: f.selector, getFlagFor: f.forExpr,
	} {
		if v != "" {
			out[name] = v
		}
	}
	return out
}

func (f getFlagInput) bools() map[string]bool {
	if f.wait {
		return map[string]bool{getFlagWait: true}
	}
	return nil
}

func (f getFlagInput) names() []string {
	return append(slices.Collect(maps.Keys(f.values())), slices.Collect(maps.Keys(f.bools()))...)
}

// getCase is a row of the get table with the flags it sets.
type getCase struct {
	scrubRow
	flags getFlagInput
}

// getRow builds the row of one get command: one object by name, or a list
// when named is false. The routing flags are always set.
func getRow(k kinds.Kind, variant string, named bool, flags getFlagInput) getCase {
	flags.cluster, flags.namespace = scrubCluster, "ns1"
	var args []string
	if named {
		args = []string{scrubObjectName}
	}
	row := scrubRow{
		command: "get " + k.Name, variant: variant, kind: k.Key, class: classEcho,
		formats: getOutputFormats,
		input: func(*plantSet) fieldCoverageInput {
			return fieldCoverageInput{args: args, values: flags.values(), bools: flags.bools()}
		},
	}
	if !named {
		row.setup = func(f *scrubFake, _ *plantSet) {
			key := k
			f.list = getListItems(f, &key)
		}
	}
	return getCase{scrubRow: row, flags: flags}
}

// getRowShowingPayload marks a row as one that shows the Secret payload on
// purpose, in every format but the names.
func getRowShowingPayload(c getCase) getCase {
	c.shown = func(ps *plantSet, format string) []payloadValue {
		if format == "name" {
			return nil
		}
		return ps.payload
	}
	return c
}

// generatedGetRows are the rows of the get commands generated from the kind
// table: one object by name and a list, per kind, except the commands the
// table lists as unrowed.
func generatedGetRows() []getCase {
	var rows []getCase
	for _, k := range kinds.All() {
		if _, ok := getUnrowedCommands["get "+k.Name]; ok {
			continue
		}
		named, list := getRow(k, "one object", true, getFlagInput{}), getRow(k, "list", false, getFlagInput{})
		if _, ok := getShownAllowlist[k.Key]; ok {
			named, list = getRowShowingPayload(named), getRowShowingPayload(list)
		}
		rows = append(rows, named, list)
	}
	return rows
}

// flagGetRows are the rows of the get flags that change how the answer is
// read, on one kind per class of what a read hides: a Sensitive row, a
// machine config row and a row with its own hidden paths. They run in json.
func flagGetRows() []getCase {
	var rows []getCase
	for _, key := range []string{"fleetcluster", "amazonec2config", "managementtoken"} {
		k, ok := kinds.ByKey(key)
		if !ok {
			panic("no kind row " + key)
		}
		selector := getRow(k, "selector", false, getFlagInput{selector: "app=web"})
		wait := getRow(k, "wait", true, getFlagInput{forExpr: "condition=Ready"})
		if k.WaitDefault != "" {
			wait = getRow(k, "wait", true, getFlagInput{wait: true})
		}
		wait.setup = func(f *scrubFake, _ *plantSet) { f.decorate = withReadyCondition }
		for _, c := range []getCase{selector, wait} {
			c.formats = []string{"json"}
			rows = append(rows, c)
		}
	}
	return rows
}

func getScrubRows() []getCase {
	return append(generatedGetRows(), flagGetRows()...)
}

// TestGetRowsCoverEveryGetCommand asserts the table has a row for exactly
// the get commands the manifest lists as read-only, apart from the commands
// classified with a reason, and that a classification names a command the
// manifest has.
func TestGetRowsCoverEveryGetCommand(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var want []string
	for _, c := range m.Commands {
		if c.CommandType != runtime.ReadOnly || !strings.HasPrefix(c.Name, "get ") {
			continue
		}
		if reason, ok := getUnrowedCommands[c.Name]; ok {
			assert.NotEmpty(t, reason, c.Name)
			continue
		}
		want = append(want, c.Name)
	}
	for name := range getUnrowedCommands {
		assert.True(t, slices.ContainsFunc(m.Commands, func(c runtime.SchemaCommand) bool { return c.Name == name }),
			"%s is classified but the manifest has no such command", name)
	}
	var got []string
	for _, r := range getScrubRows() {
		got = append(got, r.command)
	}
	slices.Sort(want)
	got = slices.Compact(slices.Sorted(slices.Values(got)))
	assert.Equal(t, want, got)
}

// TestGetRowsRunBothVariantsOfEveryKind asserts every kind row that has a
// get command in the table has a row for one object and one for a list.
func TestGetRowsRunBothVariantsOfEveryKind(t *testing.T) {
	counts := map[string]map[string]int{}
	for _, r := range generatedGetRows() {
		if counts[r.command] == nil {
			counts[r.command] = map[string]int{}
		}
		counts[r.command][r.variant]++
	}
	for _, k := range kinds.All() {
		if _, ok := getUnrowedCommands["get "+k.Name]; ok {
			continue
		}
		assert.Equal(t, map[string]int{"one object": 1, "list": 1}, counts["get "+k.Name], k.Name)
	}
}

// TestGetRowsShowPlantedValuesOnlyFromTheAllowlist asserts the only rows
// that show a planted Secret payload are those of an allowlisted kind, that
// every allowlisted kind has such rows, and that every entry states why.
func TestGetRowsShowPlantedValuesOnlyFromTheAllowlist(t *testing.T) {
	used := map[string]bool{}
	for _, r := range getScrubRows() {
		if r.shown == nil {
			continue
		}
		_, ok := getShownAllowlist[r.kind]
		assert.True(t, ok, "%s shows planted values and is not allowlisted", r.name())
		used[r.kind] = true
	}
	for key, reason := range getShownAllowlist {
		assert.NotEmpty(t, reason, key)
		assert.True(t, used[key], "the allowlist names %s, which has no row that shows its payload", key)
	}
}

// outputPrefixConstants returns the value of every exported constant of
// internal/output whose name ends in Prefix, by name: the prefixes that make
// an -o value a format with an argument.
func outputPrefixConstants(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "output")
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range files {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !name.IsExported() || !strings.HasSuffix(name.Name, "Prefix") {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING, "%s is not a string literal", name.Name)
					value, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					out[name.Name] = value
				}
			}
		}
	}
	return out
}

// TestGetOutputFormatsAreAllCovered asserts every format a command accepts
// is classified: the -o values and every prefix constant of internal/output.
// A classification must name a format a command accepts, the formats
// classified as covered by the rows must be the ones the rows run, and the
// prefix tests of validateFormat must use those constants, so a prefix
// written as a plain literal fails too.
func TestGetOutputFormatsAreAllCovered(t *testing.T) {
	prefixes := outputPrefixConstants(t)
	require.Contains(t, prefixes, "TemplatePrefix")
	accepted := slices.Clone(runtime.OutputFormats)
	for _, v := range prefixes {
		accepted = append(accepted, strings.TrimSuffix(v, "="))
	}
	for _, f := range accepted {
		class, ok := getFormatClasses[f]
		require.True(t, ok, "the output format %q is not classified", f)
		if class.by == "columns" {
			assert.NotEmpty(t, class.reason, f)
		}
	}
	for f := range getFormatClasses {
		assert.Contains(t, accepted, f, "the classification names a format no command accepts")
	}
	var rowFormats []string
	for _, f := range getOutputFormats {
		name, _, _ := strings.Cut(f, "=")
		rowFormats = append(rowFormats, name)
	}
	var byRows []string
	for f, class := range getFormatClasses {
		if class.by == "rows" {
			byRows = append(byRows, f)
		}
	}
	slices.Sort(rowFormats)
	slices.Sort(byRows)
	assert.Equal(t, byRows, rowFormats)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "runtime", "pipeline.go"), nil, 0)
	require.NoError(t, err)
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "validateFormat" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "strings" || (sel.Sel.Name != "CutPrefix" && sel.Sel.Name != "HasPrefix") {
				return true
			}
			checked++
			if arg, ok := call.Args[1].(*ast.SelectorExpr); ok {
				if pkg, ok := arg.X.(*ast.Ident); ok && pkg.Name == "output" {
					_, known := prefixes[arg.Sel.Name]
					assert.True(t, known, "validateFormat tests the prefix output.%s, which is no exported Prefix constant", arg.Sel.Name)
					return true
				}
			}
			t.Errorf("validateFormat tests a prefix at %s that is not an output Prefix constant", fset.Position(call.Pos()))
			return true
		})
	}
	assert.Positive(t, checked, "validateFormat tests no prefix")
}

// TestEveryGetFlagIsClassified asserts every flag of every generated get
// descriptor is in exactly one place: set by a guard row, or named with a
// reason in getFlagsNotRowed. A flag a row sets must be a flag of some
// descriptor, and an entry of the map must name one too.
func TestEveryGetFlagIsClassified(t *testing.T) {
	descriptorFlags := map[string]bool{}
	for _, e := range buildEntries() {
		d := e.make(runDeps{}).Describe()
		if _, unrowed := getUnrowedCommands[d.Name]; unrowed || d.CommandType != runtime.ReadOnly || !strings.HasPrefix(d.Name, "get ") {
			continue
		}
		for _, f := range d.Flags {
			descriptorFlags[f.Name] = true
		}
	}
	require.NotEmpty(t, descriptorFlags)
	rowed := map[string]bool{}
	for _, c := range getScrubRows() {
		for _, name := range c.flags.names() {
			rowed[name] = true
		}
	}
	for name := range rowed {
		assert.True(t, descriptorFlags[name], "a row sets --%s, which no get descriptor has", name)
	}
	for name := range getFlagsNotRowed {
		assert.True(t, descriptorFlags[name], "the map names --%s, which no get descriptor has", name)
		assert.False(t, rowed[name], "--%s is both set by a row and named in the map", name)
	}
	for name := range descriptorFlags {
		if !rowed[name] {
			reason, ok := getFlagsNotRowed[name]
			assert.True(t, ok, "--%s of a get descriptor is neither set by a row nor classified", name)
			assert.NotEmpty(t, reason, name)
		}
	}
}

// hiddenColumnSources returns the sources of the columns of k that its read
// redaction hides, as "header: dotted path". A Count column over a hidden
// path is left out when allowed names it by key and header: it shows only a
// length.
func hiddenColumnSources(k kinds.Kind, allowed map[string]string) []string {
	r := k.ReadRedaction()
	var out []string
	for _, c := range k.Columns {
		var sources [][]string
		sources = append(sources, c.Path)
		sources = append(sources, c.AltPaths...)
		if c.Condition != "" {
			sources = append(sources, []string{"status", "conditions"})
		}
		for _, path := range sources {
			if r.Covers(path) {
				out = append(out, c.Header+": "+strings.Join(path, "."))
			}
		}
		if len(c.Count) > 0 && r.Covers(c.Count) {
			if _, ok := allowed[k.Key+"/"+c.Header]; !ok {
				out = append(out, c.Header+": "+strings.Join(c.Count, "."))
			}
		}
	}
	return out
}

// TestNoColumnShowsAValueAReadHides asserts no column of any kind row reads
// a path the kind's read redaction hides, because a text table prints the
// cells of those columns; a server-table kind prints the server's own
// columns and carries none.
func TestNoColumnShowsAValueAReadHides(t *testing.T) {
	for _, k := range kinds.All() {
		assert.Empty(t, hiddenColumnSources(k, getColumnCountAllowlist), "%s: a column reads a hidden path", k.Name)
	}
}

// TestHiddenColumnSourcesReportsAColumnOverAHiddenPath asserts the check
// reports a column over a hidden path, over an alternate path, over a
// condition source and over a Count, and accepts a Count that the allowlist
// names.
func TestHiddenColumnSourcesReportsAColumnOverAHiddenPath(t *testing.T) {
	user, ok := kinds.ByKey("user")
	require.True(t, ok)
	secret, ok := kinds.ByKey("secret")
	require.True(t, ok)

	for name, tc := range map[string]struct {
		k    kinds.Kind
		col  kinds.Column
		want []string
	}{
		"path":           {user, kinds.Column{Header: "PASSWORD", Path: []string{"password"}}, []string{"PASSWORD: password"}},
		"alternate path": {user, kinds.Column{Header: "WHO", Path: []string{"username"}, AltPaths: [][]string{{"Password"}}}, []string{"WHO: Password"}},
		"payload":        {secret, kinds.Column{Header: "DATA", Path: []string{"StringData", "k"}}, nil},
		"annotation": {user, kinds.Column{Header: "APPLIED", Path: []string{"metadata", "annotations", kinds.LastAppliedAnnotation}},
			[]string{"APPLIED: metadata.annotations." + kinds.LastAppliedAnnotation}},
		"count": {user, kinds.Column{Header: "PW", Count: []string{"password"}}, []string{"PW: password"}},
	} {
		k := tc.k
		k.Columns = append(slices.Clone(k.Columns), tc.col)
		assert.Equal(t, tc.want, hiddenColumnSources(k, nil), name)
	}

	k := user
	k.Columns = []kinds.Column{{Header: "PW", Count: []string{"password"}}}
	assert.Empty(t, hiddenColumnSources(k, map[string]string{"user/PW": "shows only a length"}), "an allowlisted count")

	k.Columns = []kinds.Column{{Header: "READY", Condition: "Ready"}}
	assert.Empty(t, hiddenColumnSources(k, nil), "a condition source no redaction covers")
}

// TestGetColumnCountAllowlistHasNoStaleEntry asserts every allowlisted Count
// column exists, reads a hidden path and states why it is allowed.
func TestGetColumnCountAllowlistHasNoStaleEntry(t *testing.T) {
	for entry, reason := range getColumnCountAllowlist {
		assert.NotEmpty(t, reason, entry)
		found := false
		for _, k := range kinds.All() {
			for _, c := range k.Columns {
				if k.Key+"/"+c.Header == entry && len(c.Count) > 0 && k.ReadRedaction().Covers(c.Count) {
					found = true
				}
			}
		}
		assert.True(t, found, "the allowlist names %s, which is no Count column over a hidden path", entry)
	}
}

// TestGetOutputHidesWhatThePreviewHides runs every get command of the kind
// table, for one object and for a list, in json, yaml, a template that
// prints the whole object and the names, against a fake that plants in
// every answer for the row's kind each value the kind's redaction hides
// (each name also in a second letter case) beside values the redaction
// leaves alone. The output must hold none of the hidden values and every
// outside value. The secret kind shows its data and stringData. A kind that
// hides a path with a wildcard segment runs with lists of one item and of
// two.
func TestGetOutputHidesWhatThePreviewHides(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	for _, row := range getScrubRows() {
		counts := plantItemCounts(row.kind)
		for _, items := range counts {
			for _, format := range row.formats {
				t.Run(scrubSubtestName(row.name(), format, items, counts), func(t *testing.T) {
					run := runScrubRow(t, wr.reg, entries, row.scrubRow, format, items)
					requireScrubbed(t, row.scrubRow, format, run)
					if strings.HasPrefix(format, output.TemplatePrefix) {
						for _, v := range run.fake.plant.outside {
							assert.Contains(t, run.stdout, v, "%s: a value outside the redaction is missing from the output", row.name())
						}
					}
					requireSelectorReached(t, row, run)
				})
			}
		}
	}
}

// requireSelectorReached asserts every List the row's own kind received
// carried the row's label selector, and that a list row made one: other
// lists, such as a name lookup, ask for none.
func requireSelectorReached(t *testing.T, row getCase, run scrubRun) {
	t.Helper()
	k := scrubKindRow(t, row.kind)
	run.fake.mu.Lock()
	defer run.fake.mu.Unlock()
	var own []scrubListCall
	for _, call := range run.fake.lists {
		if call.gvr.Group == k.GVR.Group && call.gvr.Resource == k.GVR.Resource {
			own = append(own, call)
		}
	}
	if row.variant == "one object" || row.flags.wait || row.flags.forExpr != "" {
		return
	}
	require.NotEmpty(t, own, "%s: the command listed nothing", row.name())
	for _, call := range own {
		assert.Equal(t, row.flags.selector, call.selector, "%s: the selector the list received", row.name())
	}
}
