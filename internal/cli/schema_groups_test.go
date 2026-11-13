package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaGroupsMatchRootAssembly checks that the manifest's Groups
// reproduce the root assembly EXACTLY - section order and per-section command
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

	// Groups alignment is a structural invariant (section order, per-section
	// command order). For named groups, strip Desc before comparing - descs are
	// set separately in buildWiredRegistry and are not part of the assembly
	// alignment contract. Unnamed ("") sections must never carry a Desc (they
	// are positional bookmarks), so their full struct is compared directly: a
	// stray Desc assignment fails here rather than being masked.
	got := make([]runtime.SchemaGroup, len(m.Groups))
	for i, g := range m.Groups {
		if g.Name != "" {
			got[i] = runtime.SchemaGroup{Name: g.Name, Commands: g.Commands}
		} else {
			got[i] = g
		}
	}
	require.Equal(t, expected, got)

	// Position assertions: the fixed top-level section leads; the ungrouped-entry
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

// TestSchemaProjectShadowing asserts the deliberate collision between the
// "project" kind and the "project" group. Resolution order is command →
// group → kind (a kind is reachable via --kind, a
// shadowed group had no disambiguator), so rancher schema project returns
// the GROUP card carrying the kind-view hint, and --kind returns the kind
// card carrying the group-view hint.
func TestSchemaProjectShadowing(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()

	// Precondition: both a "project" kind and a "project" group exist in the
	// real manifest - if either disappears this test needs revisiting.
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var hasKind bool
	for _, k := range m.Kinds {
		if k.Name == "project" {
			hasKind = true
			break
		}
	}
	require.True(t, hasKind, "project kind must exist in the manifest")
	var hasGroup bool
	for _, g := range m.Groups {
		if g.Name == "project" {
			hasGroup = true
			break
		}
	}
	require.True(t, hasGroup, "project group must exist in the manifest")

	// rancher schema project → group card (groups win the collision tier).
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(),
		[]string{"rancher", "--config", dir, "schema", "project", "-o", "json"}))

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	var card map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &card))
	assert.Equal(t, "project", card["name"])
	assert.Contains(t, card, "commands", "the group card wins the collision")
	assert.NotContains(t, card, "verbs", "kind card carries verbs; group card does not")
	assert.Equal(t, "project", card["kindViewHint"], "the group card names the shadowed kind view")

	// rancher schema project --kind → kind card with the reverse hint.
	out.Reset()
	require.NoError(t, root.Run(t.Context(),
		[]string{"rancher", "--config", dir, "schema", "project", "--kind", "-o", "json"}))
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	card = map[string]any{}
	require.NoError(t, json.Unmarshal(env.Data, &card))
	assert.Equal(t, "project", card["name"])
	assert.Contains(t, card, "verbs", "--kind resolves the kind card")
	assert.Equal(t, "project", card["groupViewHint"], "the kind card names the colliding group view")
}

// TestSchemaGroupDescsComplete is the conformance guard for groupDescs in
// root.go: every named group in the built manifest must carry a non-empty Desc.
// A group added to buildEntries without a matching groupDescs entry silently
// ships a desc-less card - Desc has omitempty, renderGroupCard skips an empty
// desc, the structural alignment test strips Desc before comparing, and golden
// -update would bless the gap. This test turns that silent gap into a build
// failure at the moment the group is added.
func TestSchemaGroupDescsComplete(t *testing.T) {
	m := schemaManifest(t)
	for _, g := range m.Groups {
		if g.Name == "" {
			continue // unnamed sections are positional bookmarks, not selectable
		}
		assert.NotEmpty(t, g.Desc,
			"group %q has no desc - add it to groupDescs in buildWiredRegistry", g.Name)
	}
}

// TestSchemaCommandGroupField asserts the informational per-command Group:
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

// TestSchemaKindGroupCollisionSweep iterates all kind names, keys, and aliases
// against all named group names, then iterates all single-token command names
// against the same set. Any overlap must be in the knownShadowings allowlist -
// a new collision that is not listed here fails the build, forcing the author
// to make the shadowing deliberate and documented.
//
// Resolution order is command → kind → group, so:
//   - A single-token command name shadowing a group name means the group card
//     for that name is unreachable (the command wins).
//   - A kind identifier shadowing a group name means the group card is
//     reachable only via --kind (groups win the collision tier).
//
// "project", "cluster" and "app" are the known collisions (the GROUP wins each
// time; TestSchemaProjectShadowing asserts "project"'s behavior). Single-token
// command × group: zero overlaps.
func TestSchemaKindGroupCollisionSweep(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)

	// knownShadowings maps each known collision name to a rationale comment.
	// Add an entry here whenever a new deliberate collision is introduced -
	// the test enforces that every collision is reviewed.
	knownShadowings := map[string]string{
		"project": "the project group wins the selector; the kind card is reachable via --kind and each card hints the other; renaming would churn the TOC layout",
		"cluster": "the cluster group wins the selector, same as project; the kind card is reachable via --kind and each card hints the other; renaming would churn every agent prompt referencing the family",
		"app":     "the app group (app install, app upgrade, app uninstall) wins the selector, same as project; the kind card is reachable via --kind and each card hints the other; the group is named for the object Rancher records a release as",
	}

	groupNames := map[string]bool{}
	for _, g := range m.Groups {
		if g.Name != "" {
			groupNames[g.Name] = true
		}
	}

	// Kind identifiers × group names.
	for _, k := range m.Kinds {
		identifiers := append([]string{k.Name, k.Key}, k.Aliases...)
		for _, id := range identifiers {
			if groupNames[id] {
				assert.Contains(t, knownShadowings, id,
					"kind %q (identifier %q) collides with group %q - add to knownShadowings with a rationale comment",
					k.Name, id, id)
			}
		}
	}

	// Single-token command names × group names (command wins first).
	for _, c := range m.Commands {
		if !strings.Contains(c.Name, " ") && groupNames[c.Name] {
			assert.Contains(t, knownShadowings, c.Name,
				"command %q collides with group %q - command wins, group card unreachable; add to knownShadowings with a rationale comment",
				c.Name, c.Name)
		}
	}
}

// TestSchemaGroupKindCollisionsAreDeliberate guards the schema selector's
// name space: a command-group name that is also a kind name (or alias) makes
// the kind reachable only via --kind, so every such collision must be a
// reviewed decision. "project", "cluster" and "app" are the deliberate collisions
// today (the group card wins each; its kind-view hint points at --kind). A
// new name here means either rename the group or extend this allowlist on
// purpose.
func TestSchemaGroupKindCollisionsAreDeliberate(t *testing.T) {
	deliberate := map[string]bool{"project": true, "cluster": true, "app": true}

	m, err := buildManifest("v-test")
	require.NoError(t, err)

	kindNames := map[string]bool{}
	for _, k := range m.Kinds {
		kindNames[k.Name] = true
		for _, a := range k.Aliases {
			kindNames[a] = true
		}
	}
	for _, g := range m.Groups {
		if g.Name == "" {
			continue
		}
		if kindNames[g.Name] && !deliberate[g.Name] {
			t.Errorf("command group %q collides with a kind name or alias; rename it or extend the deliberate-collision allowlist", g.Name)
		}
	}
	for name := range deliberate {
		assert.True(t, kindNames[name], "allowlisted collision %q no longer collides - remove it from the allowlist", name)
	}
}
