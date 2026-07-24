package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaGateLineMatrix is the SE-3 conformance test: the gate line is a
// total function of (commandType, destructive, stability) with exact wordings,
// enumerated over every combination. The expected strings restate the spec
// independently of the implementation.
func TestSchemaGateLineMatrix(t *testing.T) {
	t.Parallel()
	commandTypes := []CommandType{Local, ReadOnly, Mutating}
	destructives := []string{"", "false", "true", "dependsOnInput"}
	stabilities := []string{"", "stable", "beta", "deprecated"}
	for _, ct := range commandTypes {
		for _, d := range destructives {
			for _, st := range stabilities {
				var want string
				switch ct {
				case Local:
					want = "gates: none (local command)"
				case ReadOnly:
					want = "gates: none (read-only)"
				case Mutating:
					switch d {
					case "true":
						want = "gates: --apply --yes"
					case "dependsOnInput":
						want = "gates: --apply; --yes when the input is destructive (see description)"
					default:
						want = "gates: --apply"
					}
					if st == "deprecated" {
						want += " --allow-deprecated"
					}
				}
				assert.Equal(t, want, gateLine(ct, d, st),
					"commandType=%s destructive=%q stability=%q", ct, d, st)
			}
		}
	}
}

func cardManifest() Manifest {
	return Manifest{ExitCodes: map[string]int{"GENERIC": 1, "VALIDATION": 2, "AUTH": 3}}
}

func TestSchemaCommandCardReadOnly(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:    "get pod",
		Aliases: []string{"pods", "po"},
		Usage:   "List pods or show one",
		Desc:    "Reads pods on the downstream cluster via the management proxy.",
		Args:    []ArgSpec{{Name: "name", Type: "string"}},
		Flags: []ArgSpec{
			{Name: "cluster", Type: "string", Desc: "target cluster `id|name`"},
			{Name: "wait", Type: "bool", Desc: "wait for the resource"},
		},
		Examples:    []string{"rancher get pod -o json"},
		CommandType: ReadOnly,
		Network:     true,
		Auth:        true,
		Tier:        "crd",
		Stability:   "beta",
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	require.NotEmpty(t, lines)
	assert.Equal(t, "rancher get pod [name]", lines[0], "usage line with args leads the card")
	assert.Equal(t, "  List pods or show one", lines[1])
	assert.Contains(t, lines, "flags:")
	assert.Contains(t, lines, "  --cluster string  target cluster `id|name`")
	assert.Contains(t, lines, "  --wait bool  wait for the resource")
	assert.Contains(t, lines, "contract: readOnly · crd · beta · network · auth")
	assert.Contains(t, lines, "gates: none (read-only)")
	assert.Contains(t, lines, "Reads pods on the downstream cluster via the management proxy.")
	assert.Contains(t, lines, "examples:")
	assert.Contains(t, lines, "  rancher get pod -o json")
	assert.Contains(t, lines, "exit codes: GENERIC=1 · VALIDATION=2 · AUTH=3")
	assert.Contains(t, lines, "global flags: see `rancher schema -o json` (globalFlags)")
}

func TestSchemaCommandCardDestructiveMutating(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:  "delete project",
		Usage: "Delete a project",
		Desc:  "Removes the project and its namespaces after a server-side dry-run preview.",
		Args:  []ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []ArgSpec{
			{Name: "filename", Aliases: []string{"f"}, Type: "string", Required: true,
				Desc: "manifest file", Env: []string{"RANCHER_FILE"}},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type"},
		},
		CommandType: Mutating,
		Destructive: "true",
		Network:     true,
		Auth:        true,
		Tier:        "norman",
		Stability:   "deprecated",
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	require.NotEmpty(t, lines)
	assert.Equal(t, "rancher delete project <name>", lines[0], "required arg renders in angle brackets")
	assert.Contains(t, lines, "  --filename/-f string (required)  manifest file  [env: RANCHER_FILE]")
	assert.Contains(t, lines, "  --type user|group  subject type")
	assert.Contains(t, lines, "contract: mutating · norman · deprecated · destructive=true · network · auth")
	assert.Contains(t, lines, "gates: --apply --yes --allow-deprecated")
}

// TestSchemaCommandCardOmitsEmptySections pins the card's behavior for a
// minimal local command: no flags block, no examples block, no empty
// contract segments, and the footer still carries the exit strip + pointer.
func TestSchemaCommandCardOmitsEmptySections(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:        "config current-context",
		Usage:       "Print the current context",
		CommandType: Local,
	}
	card := renderCommandCard(cardManifest(), c)
	lines := strings.Split(card, "\n")

	assert.Equal(t, "rancher config current-context", lines[0])
	assert.NotContains(t, lines, "flags:")
	assert.NotContains(t, lines, "examples:")
	assert.Contains(t, lines, "contract: local")
	assert.Contains(t, lines, "gates: none (local command)")
	assert.Contains(t, lines, "exit codes: GENERIC=1 · VALIDATION=2 · AUTH=3")
	assert.Contains(t, lines, "global flags: see `rancher schema -o json` (globalFlags)")
}

// TestSchemaCommandCardDependsOnInput pins the SE-3 dependsOnInput wording on
// a rendered card (not just the gate function).
func TestSchemaCommandCardDependsOnInput(t *testing.T) {
	t.Parallel()
	c := SchemaCommand{
		Name:        "apply",
		Usage:       "Apply a manifest",
		CommandType: Mutating,
		Destructive: "dependsOnInput",
		Network:     true,
		Auth:        true,
		Tier:        "public",
		Stability:   "beta",
	}
	card := renderCommandCard(cardManifest(), c)
	assert.Contains(t, strings.Split(card, "\n"),
		"gates: --apply; --yes when the input is destructive (see description)")
}

func TestSchemaExitCodeStripOrdersByExitThenName(t *testing.T) {
	t.Parallel()
	got := exitCodeStrip(map[string]int{
		"RBAC": 6, "VALIDATION": 2, "GENERIC": 1, "AUTH": 3, "AMBIGUOUS_SELECTION": 2,
	})
	assert.Equal(t,
		"exit codes: GENERIC=1 · AMBIGUOUS_SELECTION=2 · VALIDATION=2 · AUTH=3 · RBAC=6",
		got)
}
