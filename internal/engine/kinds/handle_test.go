package kinds

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleRowsAndTheirDescriptions asserts which rows take an id as the
// positional argument, what a user can type instead, and the description
// each row's schema carries.
func TestHandleRowsAndTheirDescriptions(t *testing.T) {
	t.Parallel()
	display := HandleField{Path: []string{"displayName"}, Label: "display name"}
	specDisplay := HandleField{Path: []string{"spec", "displayName"}, Label: "display name"}
	want := map[string]struct {
		lookup []HandleField
		desc   string
	}{
		"user": {[]HandleField{{Path: []string{"username"}, Label: "username"}, display},
			"user id (the NAME column of rancher get users), not the username or display name"},
		"project": {[]HandleField{specDisplay},
			"project id (p-xxxxx; a project made with rancher create project uses the typed name)"},
		"roletemplate": {[]HandleField{display},
			"role template id (the NAME column of rancher get roletemplates), not the display name"},
		"globalrole": {[]HandleField{display},
			"global role id (the NAME column of rancher get globalroles), not the display name"},
		"nodedriver": {[]HandleField{specDisplay},
			"node driver id (the NAME column of rancher get nodedrivers), not the display name"},
	}
	got := map[string]bool{}
	for _, k := range All() {
		if k.Handle == nil {
			continue
		}
		got[k.Key] = true
		w, ok := want[k.Key]
		require.True(t, ok, k.Key)
		assert.Equal(t, "id", k.Handle.Arg, k.Key)
		assert.Equal(t, w.lookup, k.Handle.Lookup, k.Key)
		assert.Equal(t, w.desc, k.ArgDesc, k.Key)
	}
	assert.Len(t, got, len(want))

	cluster, ok := ByKey("managementcluster")
	require.True(t, ok)
	assert.Nil(t, cluster.Handle)
	assert.Equal(t, "cluster id or display name", cluster.ArgDesc)
	for _, k := range All() {
		if k.Handle == nil && k.Key != "managementcluster" {
			assert.Empty(t, k.ArgDesc, k.Key)
		}
	}
}

// TestValidateKindsRejectsABadHandle asserts a handle the generated verbs
// could not use fails validation, and a sound one passes.
func TestValidateKindsRejectsABadHandle(t *testing.T) {
	t.Parallel()
	sound := func() Kind {
		return Kind{Key: "thing", Name: "thing", Aliases: []string{"things"}, K8sKind: "Thing",
			ArgDesc: "thing id",
			Handle: &Handle{Arg: "id", Lookup: []HandleField{
				{Path: []string{"spec", "title"}, Label: "title"},
				{Path: []string{"label"}, Label: "label"},
			}}}
	}
	require.NoError(t, validateKinds([]Kind{sound()}))

	cases := map[string]struct {
		edit func(k *Kind)
		want string
	}{
		"empty arg":         {func(k *Kind) { k.Handle.Arg = "" }, "DNS-1123"},
		"underscore arg":    {func(k *Kind) { k.Handle.Arg = "kind_id" }, "DNS-1123"},
		"upper case arg":    {func(k *Kind) { k.Handle.Arg = "ID" }, "DNS-1123"},
		"empty description": {func(k *Kind) { k.ArgDesc = "" }, "ArgDesc"},
		"no lookup":         {func(k *Kind) { k.Handle.Lookup = nil }, "empty Lookup"},
		"empty path":        {func(k *Kind) { k.Handle.Lookup[0].Path = nil }, "empty Path"},
		"empty segment":     {func(k *Kind) { k.Handle.Lookup[0].Path = []string{"spec", ""} }, "empty segment"},
		"empty label":       {func(k *Kind) { k.Handle.Lookup[0].Label = "" }, "empty Label"},
		"repeated label":    {func(k *Kind) { k.Handle.Lookup[1].Label = "title" }, "repeats the label"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := sound()
			c.edit(&k)
			err := validateKinds([]Kind{k})
			require.ErrorContains(t, err, c.want)
		})
	}
}

// handleException names one lookup path of one row that no column of the row
// shows.
type handleException struct{ row, path string }

// handleLookupExceptions lists the lookup paths a row may declare that no
// column of the row shows, each with the reason.
var handleLookupExceptions = map[handleException]string{
	{"globalrole", "displayName"}: "the table has no display column for it",
}

// handleViolations returns one message per row that shows a display name or
// username column without a Handle, per Handle lookup path that is neither a
// column of the row nor a listed exception, and per exception that is
// missing a reason or no longer applies (its row is absent, does not look
// the path up, or now shows it). The management cluster resolves a display
// name in the verbs themselves and has no Handle.
func handleViolations(rows []Kind, exceptions map[handleException]string) []string {
	var out []string
	used := map[handleException]bool{}
	for _, k := range rows {
		paths := map[string]bool{}
		shown := false
		for _, c := range k.Columns {
			for _, p := range append([][]string{c.Path}, c.AltPaths...) {
				if len(p) == 0 {
					continue
				}
				paths[strings.Join(p, ".")] = true
				if last := p[len(p)-1]; last == "displayName" || last == "username" {
					shown = true
				}
			}
		}
		if shown && k.Handle == nil && k.Key != "managementcluster" {
			out = append(out, k.Key+" shows a display name or username column and has no Handle")
		}
		if k.Handle == nil {
			continue
		}
		for _, f := range k.Handle.Lookup {
			path := strings.Join(f.Path, ".")
			if paths[path] {
				continue
			}
			ex := handleException{k.Key, path}
			if _, listed := exceptions[ex]; listed {
				used[ex] = true
				continue
			}
			out = append(out, k.Key+" looks up "+path+", which no column shows")
		}
	}
	for ex, reason := range exceptions {
		if reason == "" {
			out = append(out, "exception "+ex.row+" "+ex.path+" has no reason")
		}
		if !used[ex] {
			out = append(out, "exception "+ex.row+" "+ex.path+" no longer applies")
		}
	}
	slices.Sort(out)
	return out
}

// TestEveryRowShowingADisplayNameHasAHandle asserts a row with a display name
// or username column carries a Handle, a Handle looks up only what the row
// shows or what is listed with a reason, and every listed exception still
// applies.
func TestEveryRowShowingADisplayNameHasAHandle(t *testing.T) {
	t.Parallel()
	assert.Empty(t, handleViolations(All(), handleLookupExceptions))
}

// TestHandleGuardRejectsAPlantedRow asserts the guard fails a row with a
// display column and no Handle, fails a Handle that looks up an unshown
// field, and passes a row whose column is the CRD's userName spelling.
func TestHandleGuardRejectsAPlantedRow(t *testing.T) {
	t.Parallel()
	planted := Kind{Key: "widget", Name: "widget", Columns: []Column{{Header: "DISPLAY", Path: []string{"spec", "displayName"}}}}
	assert.Equal(t, []string{"widget shows a display name or username column and has no Handle"}, handleViolations([]Kind{planted}, nil))

	alt := Kind{Key: "widget", Name: "widget", Columns: []Column{{Header: "MEMBER", Path: []string{"x"}, AltPaths: [][]string{{"username"}}}}}
	assert.Len(t, handleViolations([]Kind{alt}, nil), 1)

	unshown := Kind{Key: "widget", Name: "widget", Columns: []Column{{Header: "DISPLAY", Path: []string{"displayName"}}},
		Handle: &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"title"}, Label: "title"}}}}
	assert.Equal(t, []string{"widget looks up title, which no column shows"}, handleViolations([]Kind{unshown}, nil))

	userName := Kind{Key: "widget", Name: "widget", Columns: []Column{{Header: "MEMBER", Path: []string{"userName"}}}}
	assert.Empty(t, handleViolations([]Kind{userName}, nil))

	cluster, ok := ByKey("managementcluster")
	require.True(t, ok)
	assert.True(t, slices.ContainsFunc(cluster.Columns, func(c Column) bool { return c.Header == "DISPLAY" }))
	assert.Empty(t, handleViolations([]Kind{cluster}, nil))
}

// TestHandleGuardFailsAStaleException asserts an exception fails when its row
// is absent, does not look the path up, now shows the path in a column, or has
// no reason, and passes while it covers a lookup path no column shows.
func TestHandleGuardFailsAStaleException(t *testing.T) {
	t.Parallel()
	lookup := &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"displayName"}, Label: "display name"}}}
	hidden := Kind{Key: "widget", Name: "widget", Handle: lookup}
	ex := handleException{"widget", "displayName"}

	assert.Empty(t, handleViolations([]Kind{hidden}, map[handleException]string{ex: "no display column"}))
	assert.Equal(t, []string{"exception widget displayName has no reason"},
		handleViolations([]Kind{hidden}, map[handleException]string{ex: ""}))

	shown := hidden
	shown.Columns = []Column{{Header: "DISPLAY", Path: []string{"displayName"}}}
	assert.Equal(t, []string{"exception widget displayName no longer applies"},
		handleViolations([]Kind{shown}, map[handleException]string{ex: "no display column"}))

	assert.Equal(t, []string{"exception widget displayName no longer applies"},
		handleViolations(nil, map[handleException]string{ex: "no display column"}))
	other := handleException{"widget", "title"}
	assert.Equal(t, []string{"exception widget title no longer applies", "widget looks up displayName, which no column shows"},
		handleViolations([]Kind{hidden}, map[handleException]string{other: "no title column"}))
}
