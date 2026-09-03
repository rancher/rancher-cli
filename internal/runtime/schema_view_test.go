package runtime

import (
	"encoding/json"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readOnlyTOCManifest() Manifest {
	return Manifest{
		CLIVersion:        "v-test",
		MinRancherVersion: "2.14",
		OutputAPIVersion:  "cli.rancher.io/v1",
		ReadOnly:          true,
		ReadOnlySources:   []string{"RANCHER_CLI_READONLY"},
		ExitCodes:         map[string]int{"GENERIC": 1, "VALIDATION": 2},
		EffectSchema:      effectSchema(),
		Commands: []SchemaCommand{
			{Name: "auth login", Usage: "Login to a server", CommandType: ReadOnly},
			{Name: "get pod", Usage: "List pods", CommandType: ReadOnly},
			{Name: "delete pod", Usage: "Delete the pod", CommandType: Mutating, Destructive: "true"},
		},
		Groups: []SchemaGroup{
			{Name: "auth", Commands: []string{"auth login"}},
			{Name: "get", Commands: []string{"get pod"}},
			{Name: "delete", Commands: []string{"delete pod"}},
		},
		// pod's Verbs make "get pod" and "delete pod" GENERATED commands
		// (generatedCommandNames derives from Verbs); cluster carries none.
		Kinds: []SchemaKind{
			{Name: "pod", Verbs: map[string]VerbState{"get": {State: VerbAvailable}, "delete": {State: VerbAvailable}}},
			{Name: "cluster"},
		},
	}
}

// TestTOCViewBareMirrorsTheTOC asserts the spec'd bare TOC JSON shape:
// header facts, groups with name+usage entries, kind NAMES only, the
// global exit map — and none of the full-manifest sections.
func TestTOCViewBareMirrorsTheTOC(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(newTOCView(readOnlyTOCManifest(), true))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))

	assert.Equal(t, "v-test", got["cliVersion"])
	assert.Equal(t, float64(3), got["commandCount"], "commandCount counts ALL commands, generated included (mirrors the header)")
	assert.Equal(t, float64(2), got["kindCount"])
	assert.Equal(t, float64(2), got["generatedCommandCount"], "the collapsed line's N (spec'd collapse mirror)")
	assert.Equal(t, true, got["readOnly"])
	assert.Contains(t, got, "globalExitCodes")
	assert.Equal(t, []any{"pod", "cluster"}, got["kinds"])

	// The Effect contract is the second global contract on the bare TOC
	// (beside the exit map): present with the closed action enum and the
	// field set, so a caller parses --dry-run output without --all.
	es, ok := got["effectSchema"].(map[string]any)
	require.True(t, ok, "bare TOC carries the effectSchema contract")
	assert.Equal(t, []any{"create", "update", "delete"}, es["actions"])
	assert.NotEmpty(t, es["fields"])

	// Generated commands are COLLAPSED on the bare path: absent from the
	// per-group commands list, represented instead by a generated summary
	// object — exactly what renderTOC does (spec'd collapse mirror).
	// All-generated groups carry no commands key (omitempty), so the
	// iteration uses a nil-safe type assertion.
	var flat []string
	for _, g := range got["groups"].([]any) {
		cmds, _ := g.(map[string]any)["commands"].([]any)
		for _, e := range cmds {
			flat = append(flat, e.(map[string]any)["name"].(string))
		}
	}
	assert.Equal(t, []string{"auth login"}, flat, "only non-generated commands appear in the per-group commands list on the bare TOC")

	// Full-manifest sections must be absent — the mirror rule. effectSchema
	// left this list deliberately: it is a global contract, disclosed on the
	// bare TOC like the exit map.
	for _, absent := range []string{"commands", "kindsDetail", "envVars", "globalFlags", "errorCodes", "exitCodes"} {
		assert.NotContains(t, got, absent)
	}
}

// TestTOCViewFilteredOmitsBareOnlyFields asserts the spec'd conditionality: a filtered
// selection has no kinds, no kindCount, no globalExitCodes — its text
// header renders a match count and no exit strip.
func TestTOCViewFilteredOmitsBareOnlyFields(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	// Retain only the GENERATED "get pod": the filtered TOC does not
	// collapse — the matches ARE the content — so it must list per line.
	m.Commands = m.Commands[1:2]
	m.Groups = []SchemaGroup{{Name: "get", Commands: []string{"get pod"}}}
	b, err := json.Marshal(newTOCView(m, false))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, float64(1), got["commandCount"])
	entry := got["groups"].([]any)[0].(map[string]any)["commands"].([]any)[0].(map[string]any)
	assert.Equal(t, "get pod", entry["name"], "generated commands list per line on the FILTERED path")
	for _, absent := range []string{"kinds", "kindCount", "globalExitCodes", "effectSchema", "generatedCommandCount"} {
		assert.NotContains(t, got, absent)
	}
	// readOnly stays unconditional (round-3: both text views show it).
	assert.Equal(t, true, got["readOnly"])
}

// TestTOCViewNameRows asserts -o name: unchanged command-name rows.
func TestTOCViewNameRows(t *testing.T) {
	t.Parallel()
	v := newTOCView(readOnlyTOCManifest(), true)
	assert.Equal(t, []string{"NAME"}, v.Columns())
	assert.Equal(t, [][]string{{"auth login"}, {"get pod"}, {"delete pod"}}, v.Rows(),
		"-o name is unchanged: EVERY selection command, generated included")
}

// TestCardViewFlattensTheCommand asserts the spec'd card shape: data IS the command —
// fields at top level, no commands array — plus globalExitCodes and the
// kindViewHint mirror of the text card's collision pointer.
func TestCardViewFlattensTheCommand(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	c := m.Commands[2] // delete pod, destructive
	b, err := json.Marshal(newCardView(m, c, "pod"))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))

	assert.Equal(t, "delete pod", got["name"])
	assert.Equal(t, "true", got["destructive"])
	assert.Contains(t, got, "globalExitCodes")
	assert.Equal(t, "pod", got["kindViewHint"])
	assert.NotContains(t, got, "commands", "flattened: no wrapper array")
	assert.NotContains(t, got, "kinds")
	assert.NotContains(t, got, "envVars")
	// readOnly propagates from manifest — unconditional bool like tocJSON.
	assert.Equal(t, true, got["readOnly"])
	assert.Equal(t, []any{"RANCHER_CLI_READONLY"}, got["readOnlySources"])
}

// TestCardViewReadOnlyFalseIsPresent asserts that readOnly: false is present in
// the card JSON even when inactive (unconditional bool, mirrors tocJSON).
func TestCardViewReadOnlyFalseIsPresent(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	m.ReadOnly = false
	m.ReadOnlySources = nil
	b, err := json.Marshal(newCardView(m, m.Commands[0], ""))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, false, got["readOnly"], "readOnly: false must be present in JSON")
	assert.NotContains(t, got, "readOnlySources")
}

// TestCardViewOmitsEmptyKindHint asserts that a non-colliding selector adds
// no kindViewHint key at all.
func TestCardViewOmitsEmptyKindHint(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	b, err := json.Marshal(newCardView(m, m.Commands[0], ""))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.NotContains(t, got, "kindViewHint")
}

// TestCardViewNameRows asserts -o name for a command selection: one row, the
// command's name.
func TestCardViewNameRows(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	v := newCardView(m, m.Commands[1], "") // Commands[1] = "get pod"
	assert.Equal(t, [][]string{{"get pod"}}, v.Rows())
}

// TestKindViewMarshalsTheRowOnly asserts the spec'd kind view: a kind selection's data
// is the kind row — verb STATE included, per-verb flags deliberately not
// (the two-step: schema <kind> then schema <verb> <kind>).
func TestKindViewMarshalsTheRowOnly(t *testing.T) {
	t.Parallel()
	k := SchemaKind{Name: "pod", Key: "pod", Aliases: []string{"pods"}}
	b, err := json.Marshal(newKindView(Manifest{}, k))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "pod", got["name"])
	assert.NotContains(t, got, "commands")
	assert.NotContains(t, got, "envVars")
	assert.Equal(t, [][]string{{"pod"}}, newKindView(Manifest{}, k).Rows())
}

// TestKindViewReadOnlyPropagates asserts readOnly propagation from the manifest
// into the kind JSON: true propagates with sources; false is unconditional.
func TestKindViewReadOnlyPropagates(t *testing.T) {
	t.Parallel()
	k := SchemaKind{Name: "pod", Key: "pod", Aliases: []string{"pods"}}

	active := Manifest{ReadOnly: true, ReadOnlySources: []string{"--read-only"}}
	b, err := json.Marshal(newKindView(active, k))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "pod", got["name"])
	assert.Equal(t, true, got["readOnly"])
	assert.Equal(t, []any{"--read-only"}, got["readOnlySources"])

	// readOnly: false must still appear (unconditional bool).
	b2, err := json.Marshal(newKindView(Manifest{}, k))
	require.NoError(t, err)
	var got2 map[string]any
	require.NoError(t, json.Unmarshal(b2, &got2))
	assert.Equal(t, false, got2["readOnly"])
	assert.NotContains(t, got2, "readOnlySources")
}

// TestTOCGeneratedCountMatchesTheText asserts the mirror as an assertion,
// not a golden eyeball (plan-review round 4): the JSON's
// generatedCommandCount equals the N the bare text TOC actually prints
// in its collapsed line, on the same manifest.
func TestTOCGeneratedCountMatchesTheText(t *testing.T) {
	t.Parallel()
	m := readOnlyTOCManifest()
	text := renderTOC(m)
	re := regexp.MustCompile(`— (\d+) generated commands across`)
	match := re.FindStringSubmatch(text)
	require.NotNil(t, match, "bare TOC must render the collapsed generated-commands line for this fixture")
	want, err := strconv.Atoi(match[1])
	require.NoError(t, err)
	assert.Equal(t, want, newTOCView(m, true).toc().GeneratedCommandCount)
}
