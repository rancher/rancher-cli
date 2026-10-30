package remedytest

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestMarkedDetailsMapTextMapsTheText asserts the double maps its one text
// through both mapping methods.
func TestMarkedDetailsMapTextMapsTheText(t *testing.T) {
	t.Parallel()
	d := MarkedDetails{Text: clierr.Rancher + " get x"}
	upper := func(s string) string { return "<" + s + ">" }
	assert.Equal(t, MarkedDetails{Text: "<" + d.Text + ">"}, d.MapText(upper))
	assert.Equal(t, MarkedDetails{Text: "<" + d.Text + ">"}, d.MapCommands(upper))
	assert.Equal(t, []string{d.Text}, d.TextDetails())
}

// TestTextFieldHelpersWalkEveryTextField asserts the walk finds the text
// fields of a nested type, plants each, and reads each back by path.
func TestTextFieldHelpersWalkEveryTextField(t *testing.T) {
	t.Parallel()
	type inner struct{ Name string }
	type outer struct {
		Message string
		Items   []inner
		Extra   map[string]inner
		Opt     *inner
		Any     any
		Count   int
	}
	typ := reflect.TypeFor[outer]()
	paths := TextPaths(typ)
	assert.ElementsMatch(t, []string{"Message", "Items[].Name", "Extra{key}", "Extra{}.Name", "Opt.Name", "Any"}, paths)

	planted := PlantMarked(t, typ).Interface().(outer)
	values := TextValues(reflect.ValueOf(planted))
	require.Len(t, values, len(paths))
	for _, p := range paths {
		assert.Contains(t, values[p], clierr.Rancher+" plant "+p)
	}

	lists := FieldLists{Remedy: []string{"Message", "Any"}, Server: []string{"Items[].Name", "Extra{key}", "Extra{}.Name"}, Other: []string{"Opt.Name"}}
	AssertEveryFieldClassified(t, typ, lists)
	got := planted
	got.Message = "rancher --config 'd' plant Message"
	got.Any = MarkedDetails{Text: "rancher --config 'd' plant Any"}
	AssertMappedAsListed(t, planted, got, lists, lists.Remedy, " --config 'd'")
}

// TestPlantTextSetsEveryTextFieldToTheValueForItsPath asserts PlantText puts
// value(path) in each string, in the string an interface field holds and in
// the message of a nested *clierr.Error, read back by path with a reader of
// the test's own, and that PlantMarked still plants its commands.
func TestPlantTextSetsEveryTextFieldToTheValueForItsPath(t *testing.T) {
	t.Parallel()
	type inner struct{ Name string }
	type outer struct {
		Message string
		Items   []inner
		Extra   map[string]inner
		Opt     *inner
		Any     any
		Err     *clierr.Error
		Count   int
	}
	typ := reflect.TypeFor[outer]()
	paths := TextPaths(typ)
	require.ElementsMatch(t, []string{"Message", "Items[].Name", "Extra{key}", "Extra{}.Name", "Opt.Name", "Any", "Err"}, paths)

	value := func(path string) string { return "<" + path + ">" }
	planted := PlantText(t, typ, value).Interface().(outer)
	assert.Equal(t, "<Message>", planted.Message)
	require.Len(t, planted.Items, 1)
	assert.Equal(t, "<Items[].Name>", planted.Items[0].Name)
	require.Len(t, planted.Extra, 1)
	for key, in := range planted.Extra {
		assert.Equal(t, "<Extra{key}>", key)
		assert.Equal(t, "<Extra{}.Name>", in.Name)
	}
	require.NotNil(t, planted.Opt)
	assert.Equal(t, "<Opt.Name>", planted.Opt.Name)
	s, ok := planted.Any.(string)
	require.True(t, ok, "an interface field holds the string, not a details payload: %T", planted.Any)
	assert.Equal(t, "<Any>", s)
	require.NotNil(t, planted.Err)
	assert.Equal(t, "<Err>", planted.Err.Message)

	marked := PlantMarked(t, typ).Interface().(outer)
	assert.Equal(t, clierr.Rancher+" plant Message", marked.Message)
	assert.Equal(t, MarkedDetails{Text: clierr.Rancher + " plant Any"}, marked.Any)
	assert.Equal(t, clierr.Rancher+" plant Err", marked.Err.Message)
}
