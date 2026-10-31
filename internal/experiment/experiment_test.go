package experiment

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryExperimentHasADescription asserts every row of the table has a
// name that matches the name pattern, a description and a value form, and is
// honored only in a development build: the gate is global, so a row that says
// otherwise would be false.
func TestEveryExperimentHasADescription(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, Table())
	pattern := regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	seen := map[string]bool{}
	for _, e := range Table() {
		assert.Regexp(t, pattern, e.Name)
		assert.NotEmpty(t, e.Description, e.Name)
		assert.NotEmpty(t, e.ValueForm, e.Name)
		assert.True(t, e.DevOnly, "%s must be development builds only", e.Name)
		assert.False(t, seen[e.Name], "%s is listed twice", e.Name)
		seen[e.Name] = true
	}
}

// TestParse asserts the split rules: items at commas, each at its first =,
// empty items skipped but counted in the position, no trimming, an empty
// value the same as a bare name, and the kind of each item.
func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want []Item
	}{
		{"empty", "", nil},
		{"only commas", ",,,", nil},
		{"bare known", "telemetryupload", []Item{{Position: 1, Kind: Known, Name: "telemetryupload"}}},
		{"empty value", "telemetryupload=", []Item{{Position: 1, Kind: Known, Name: "telemetryupload"}}},
		{"value", "telemetryupload=https://h.example", []Item{{Position: 1, Kind: Known, Name: "telemetryupload", Value: "https://h.example"}}},
		{"first equals sign splits", "telemetryupload=a=b", []Item{{Position: 1, Kind: Known, Name: "telemetryupload", Value: "a=b"}}},
		{"unknown name", "other=1", []Item{{Position: 1, Kind: Unknown, Name: "other", Value: "1"}}},
		{"empty items count", ",,other,,telemetryupload", []Item{
			{Position: 3, Kind: Unknown, Name: "other"},
			{Position: 5, Kind: Known, Name: "telemetryupload"},
		}},
		{"upper case", "Telemetryupload=x", []Item{{Position: 1, Kind: Unrecognized}}},
		{"leading space", " telemetryupload", []Item{{Position: 1, Kind: Unrecognized}}},
		{"trailing space", "telemetryupload =x", []Item{{Position: 1, Kind: Unrecognized}}},
		{"digit first", "1a", []Item{{Position: 1, Kind: Unrecognized}}},
		{"digit later", "a1", []Item{{Position: 1, Kind: Unknown, Name: "a1"}}},
		{"hyphen", "a-b", []Item{{Position: 1, Kind: Unrecognized}}},
		{"no name", "=x", []Item{{Position: 1, Kind: Unrecognized}}},
		{"colon form", "telemetryupload:https://user:s3cr3t@h.example", []Item{{Position: 1, Kind: Unrecognized}}},
		{"space form", "telemetryupload https://user:s3cr3t@h.example", []Item{{Position: 1, Kind: Unrecognized}}},
		{"value stops at the comma", "telemetryupload=https://h.example/?a=1,b=2", []Item{
			{Position: 1, Kind: Known, Name: "telemetryupload", Value: "https://h.example/?a=1"},
			{Position: 2, Kind: Unknown, Name: "b", Value: "2"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, Parse(tc.text))
		})
	}
}

// TestParseNeverReturnsAnUnrecognizedItem asserts an unrecognized item has no
// name and no value, so no caller can print part of it.
func TestParseNeverReturnsAnUnrecognizedItem(t *testing.T) {
	t.Parallel()
	for _, it := range Parse("telemetryupload:https://user:s3cr3t@h.example,Bad=s3cr3t, x=s3cr3t") {
		require.Equal(t, Unrecognized, it.Kind)
		assert.Empty(t, it.Name)
		assert.Empty(t, it.Value)
	}
}
