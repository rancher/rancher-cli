package catalog

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// valueWithFixedRun is a value of 32 characters that starts with a run of the
// fixed text of every action path (`alog` of `catalog.cattle.io`).
func valueWithFixedRun() string { return "alog" + randomString(28) }

// plantedCaseErrors runs the action over the answer form twice, once with value
// planted in the server text that text builds and once with the other value,
// and returns the two errors. The filter knows the value planted in each run
// when filterOn is true, and no value when it is false.
func plantedCaseErrors(t *testing.T, a admissionAction, form podAnswer, text func(string) string, value string, filterOn bool) (*clierr.Error, *clierr.Error) {
	t.Helper()
	other := otherValue(t, value)
	valuesFile := func(v string) string {
		if filterOn {
			return "password: " + v + "\n"
		}
		return ""
	}
	ce, _ := runPodAnswer(t, a, form.answer(t, text(value)), "v2.15.3", valuesFile(value))
	second, _ := runPodAnswer(t, a, form.answer(t, text(other)), "v2.15.3", valuesFile(other))
	return ce, second
}

func notCut(v string) string { return podHead + vapPrefix + "value " + v + " was refused" }

func cutInside(v string) string { return straddling(podHead+vapPrefix, v, 2048) }

// TestValuePartLeftSkipsTheFixedTextOfTheCase asserts that a value the filter
// hides whole, not cut and cut inside it, leaves no part in the error when the
// value starts with a part of the error's fixed text (`alog` of the path
// `catalog.cattle.io`). The message holds `alog`: a check of one error finds
// it, and valuePartLeft does not, because the error of the case planted with
// another value holds it too.
func TestValuePartLeftSkipsTheFixedTextOfTheCase(t *testing.T) {
	t.Parallel()
	for _, a := range admissionActions {
		if !a.values {
			continue
		}
		for _, form := range podAnswers {
			for name, text := range map[string]func(string) string{"not cut": notCut, "cut": cutInside} {
				t.Run(a.name+" "+form.name+" "+name, func(t *testing.T) {
					t.Parallel()
					value := valueWithFixedRun()
					ce, second := plantedCaseErrors(t, a, form, text, value, true)
					run, found := valuePart(value, ce.Message)
					require.True(t, found, "the message holds a run of the value in its fixed text")
					assert.Contains(t, second.Message, run)
					what, part, found := valuePartLeft(value, ce, second)
					assert.False(t, found, "the %s holds %q of the value: %s", what, part, ce.Message)
				})
			}
		}
	}
}

// TestValuePartLeftFindsAValueTheFilterLeaves asserts valuePartLeft finds a
// part of a value that starts with a part of the fixed text, when the filter
// does not know the value: the whole value when it is not cut, and the start of
// the value when it is cut inside it. The part found is not in the error of the
// case planted with the other value.
func TestValuePartLeftFindsAValueTheFilterLeaves(t *testing.T) {
	t.Parallel()
	for _, a := range admissionActions {
		if !a.values {
			continue
		}
		for _, form := range podAnswers {
			for _, tc := range []struct {
				name string
				text func(string) string
				// left is the part of the value the error holds.
				left func(string) string
			}{
				{"not cut", notCut, func(v string) string { return v }},
				{"cut", cutInside, func(v string) string { return v[:16] }},
			} {
				t.Run(a.name+" "+form.name+" "+tc.name, func(t *testing.T) {
					t.Parallel()
					value := valueWithFixedRun()
					ce, second := plantedCaseErrors(t, a, form, tc.text, value, false)
					what, part, found := valuePartLeft(value, ce, second)
					require.True(t, found, "the error shows the value: %s", ce.Message)
					assert.Equal(t, "message", what)
					assert.Contains(t, tc.left(value), part)
					assert.NotContains(t, second.Message, part)
				})
			}
		}
	}
}
