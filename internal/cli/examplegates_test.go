package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exampleGateProblems returns what is wrong with one built-in example of a
// mutating command copied verbatim: it holds neither --apply nor --dry-run, or
// it holds --apply and lacks a flag in the command's gates. A flag counts only
// as a whole word of the example.
//
// The gates come from the manifest. For a command whose destructive value is
// dependsOnInput (create resource) the manifest lists only --apply: whether
// --yes is needed depends on the manifest the caller passes, and the refusal
// names it, so the example is checked for --apply or --dry-run only.
func exampleGateProblems(name string, gates []string, example string) []string {
	words := strings.Fields(example)
	hasApply := slices.Contains(words, "--apply")
	if !hasApply && !slices.Contains(words, "--dry-run") {
		return []string{name + ": the example holds neither --apply nor --dry-run: " + example}
	}
	if !hasApply {
		return nil
	}
	var problems []string
	for _, gate := range gates {
		if !slices.Contains(words, gate) {
			problems = append(problems, name+": the example holds --apply and lacks "+gate+": "+example)
		}
	}
	return problems
}

func TestExampleGateProblemsRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		row     string
		gates   []string
		example string
		want    []string
	}{
		{"allGatesPresent", []string{"--apply", "--yes", "--allow-deprecated"}, "rancher x --apply --yes --allow-deprecated", nil},
		{"allowDeprecatedMissing", []string{"--apply", "--yes", "--allow-deprecated"}, "rancher x --apply --yes",
			[]string{"x: the example holds --apply and lacks --allow-deprecated: rancher x --apply --yes"}},
		{"yesMissing", []string{"--apply", "--yes"}, "rancher x --apply",
			[]string{"x: the example holds --apply and lacks --yes: rancher x --apply"}},
		{"applyOnlyGate", []string{"--apply"}, "rancher x --apply", nil},
		{"neitherApplyNorDryRun", []string{"--apply"}, "rancher x --name a",
			[]string{"x: the example holds neither --apply nor --dry-run: rancher x --name a"}},
		{"applyPrefixDoesNotCount", []string{"--apply"}, "rancher x --apply-foo",
			[]string{"x: the example holds neither --apply nor --dry-run: rancher x --apply-foo"}},
		{"yesPrefixDoesNotCount", []string{"--apply", "--yes"}, "rancher x --apply --yes-foo",
			[]string{"x: the example holds --apply and lacks --yes: rancher x --apply --yes-foo"}},
		{"dryRunAlone", []string{"--apply", "--yes"}, "rancher x --dry-run", nil},
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, exampleGateProblems("x", tc.gates, tc.example))
		})
	}
}

// TestEveryMutationExampleCarriesItsGateFlags asserts that every built-in
// example of a mutating command passes the gates when copied verbatim. It
// reads the manifest built from the live command registry, never the golden
// file, so a new command with a refused example fails before its golden is
// regenerated.
func TestEveryMutationExampleCarriesItsGateFlags(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	root, err := BuildRoot("golden")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", t.TempDir(), "schema", "--all", "-o", "json"}))

	var env struct {
		Data struct {
			Commands []struct {
				Name        string   `json:"name"`
				CommandType string   `json:"commandType"`
				Gates       []string `json:"gates"`
				Examples    []string `json:"examples"`
			} `json:"commands"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))

	mutating, examples := 0, 0
	var problems []string
	for _, c := range env.Data.Commands {
		if c.CommandType != "mutating" {
			continue
		}
		mutating++
		for _, example := range c.Examples {
			examples++
			problems = append(problems, exampleGateProblems(c.Name, c.Gates, example)...)
		}
	}
	require.Positive(t, mutating, "the manifest lists no mutating command")
	require.Positive(t, examples, "no mutating command has an example")
	assert.Empty(t, problems, "built-in examples the CLI refuses when copied verbatim")
}
