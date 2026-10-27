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
