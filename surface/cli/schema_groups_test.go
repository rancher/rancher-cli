package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaGroupsMatchRootAssembly is the SE-18 test: the manifest's Groups
// reproduce the root assembly EXACTLY — section order and per-section command
// order are both derived from the built root's own command tree (never a
// hand-list), covering the leading fixed top-level section and the
// ungrouped-entry section at its real position. Only commands the manifest
// registers count; urfave-appended machinery (help, __completion) is not
// surface.
func TestSchemaGroupsMatchRootAssembly(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	// Snapshot the assembly BEFORE Run: urfave's Run injects help/completion
	// machinery subcommands into every command, which are not surface.
	type node struct {
		name   string
		leaves []string
	}
	var assembly []node
	for _, c := range root.Commands {
		n := node{name: c.Name}
		for _, leaf := range c.Commands {
			n.leaves = append(n.leaves, leaf.Name)
		}
		assembly = append(assembly, n)
	}

	m, err := buildManifest("v-test")
	require.NoError(t, err)

	registered := map[string]bool{}
	for _, c := range m.Commands {
		registered[c.Name] = true
	}

	var expected []runtime.SchemaGroup
	for _, c := range assembly {
		if len(c.leaves) > 0 {
			var cmds []string
			for _, leaf := range c.leaves {
				if full := c.name + " " + leaf; registered[full] {
					cmds = append(cmds, full)
				}
			}
			if len(cmds) > 0 {
				expected = append(expected, runtime.SchemaGroup{Name: c.name, Commands: cmds})
			}
			continue
		}
		if !registered[c.name] {
			continue
		}
		// A run of consecutive top-level leaves is one "" section.
		if n := len(expected); n > 0 && expected[n-1].Name == "" {
			expected[n-1].Commands = append(expected[n-1].Commands, c.name)
			continue
		}
		expected = append(expected, runtime.SchemaGroup{Name: "", Commands: []string{c.name}})
	}

	require.Equal(t, expected, m.Groups)

	// Position pins: the fixed top-level section leads; the ungrouped-entry
	// section is a SECOND "" section at its own later position.
	require.NotEmpty(t, m.Groups)
	assert.Equal(t, "", m.Groups[0].Name)
	assert.Contains(t, m.Groups[0].Commands, "schema")
	assert.Contains(t, m.Groups[0].Commands, "completion")
	var laterUngrouped *runtime.SchemaGroup
	for i := 1; i < len(m.Groups); i++ {
		if m.Groups[i].Name == "" {
			laterUngrouped = &m.Groups[i]
			break
		}
	}
	require.NotNil(t, laterUngrouped, "the ungrouped-entry section must appear at its own position, not fold into the leading section")
	assert.Contains(t, laterUngrouped.Commands, "grant")
}

// TestSchemaCommandGroupField pins SE-18's informational per-command Group:
// grouped commands carry their section name; the two positional-root
// sections' commands carry "".
func TestSchemaCommandGroupField(t *testing.T) {
	m := schemaManifest(t)
	byName := map[string]runtime.SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}

	assert.Equal(t, "get", byName["get pod"].Group)
	assert.Equal(t, "auth", byName["auth login"].Group)
	assert.Equal(t, "", byName["schema"].Group)
	assert.Equal(t, "", byName["grant"].Group)
}
