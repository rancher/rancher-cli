package runtime

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func namespaceObjects(outcome string, names ...string) []safety.Object {
	var out []safety.Object
	for _, n := range names {
		out = append(out, safety.Object{Kind: "namespace", Name: n, Outcome: outcome})
	}
	return out
}

// TestObjectLinesShowsAtMostFiveNamesAndACount asserts one outcome with
// more than five names lists five and counts the rest, and an outcome with
// five or fewer lists them all.
func TestObjectLinesShowsAtMostFiveNamesAndACount(t *testing.T) {
	t.Parallel()
	objs := append(namespaceObjects(safety.OutcomeRancherKeeps, "ns1", "ns2", "ns3", "ns4", "ns5", "ns6", "ns7", "ns8"),
		namespaceObjects(safety.OutcomeRancherDeletes, "legacy")...)
	assert.Equal(t, []string{
		"namespaces:",
		"  rancher keeps: ns1, ns2, ns3, ns4, ns5 and 3 more",
		"  rancher deletes: legacy",
	}, ObjectLines(objs, "project"))
	five := namespaceObjects(safety.OutcomeRancherKeeps, "a", "b", "c", "d", "e")
	assert.Equal(t, []string{"namespaces:", "  rancher keeps: a, b, c, d, e"}, ObjectLines(five, "project"))
}

// TestObjectLinesOrdersOutcomesAndShowsReasons asserts the outcome lines
// follow one fixed order whatever the order of the objects, and a not
// deleted, failed, skipped or blocking name carries its reason in
// parentheses.
func TestObjectLinesOrdersOutcomesAndShowsReasons(t *testing.T) {
	t.Parallel()
	objs := []safety.Object{
		{Kind: "namespace", Name: "old-ns", Outcome: safety.OutcomeBeingDeleted},
		{Kind: "namespace", Name: "default", Outcome: safety.OutcomeNotDeleted, Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "apps", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "tools", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "project", Name: "p-1", Outcome: safety.OutcomeWillBeDeleted},
	}
	assert.Equal(t, []string{
		"namespaces:",
		"  will be deleted: apps, tools",
		"  not deleted: default (Kubernetes never deletes it)",
		"  being deleted: old-ns",
	}, ObjectLines(objs, "project"), "the headline kind is not listed")
	assert.Empty(t, ObjectLines(nil, "project"))

	pods := []safety.Object{
		{Kind: "pod", Name: "kube-system/proxy", Outcome: safety.OutcomeSkipped, Message: "DaemonSet pod"},
		{Kind: "pod", Name: "apps/web", Outcome: safety.OutcomeWillBeEvicted, Message: "ignored"},
		{Kind: "pod", Name: "apps/db", Outcome: safety.OutcomeBlocksTheDrain, Message: "Pods with local storage (use --delete-emptydir-data to override)"},
		{Kind: "pod", Name: "apps/job", Outcome: safety.OutcomeNotDeleted, Message: "kept"},
	}
	assert.Equal(t, []string{
		"pods:",
		"  not deleted: apps/job (kept)",
		"  blocks the drain: apps/db (Pods with local storage (use --delete-emptydir-data to override))",
		"  will be evicted: apps/web",
		"  skipped: kube-system/proxy (DaemonSet pod)",
	}, ObjectLines(pods, "node"), "skipped and blocking names show their reason, an evicted name does not")
}

// TestDryRunTextListsTheEffectObjects asserts the preview body carries the
// object lines between the changes and the gate line, and an Effect with no
// objects renders as before.
func TestDryRunTextListsTheEffectObjects(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{
		PreviewSource: safety.PreviewServerDryRun,
		Action:        safety.ActionDelete, DisplayKind: "project", DisplayName: "p-1", Cluster: "c-abc", Destructive: true,
		Gates:   []string{safety.GateApply, safety.GateYes},
		Objects: append(namespaceObjects(safety.OutcomeRancherKeeps, "apps"), namespaceObjects(safety.OutcomeRancherDeletes, "legacy")...),
	}
	assert.Equal(t, "delete project/p-1\n"+
		"  cluster: c-abc\n"+
		"  namespaces:\n"+
		"    rancher keeps: apps\n"+
		"    rancher deletes: legacy\n"+
		"  dry-run preview - re-run with --apply --yes to apply the change\n", dryRunText(eff))
	eff.Objects = nil
	assert.Equal(t, "delete project/p-1\n  cluster: c-abc\n  dry-run preview - re-run with --apply --yes to apply the change\n", dryRunText(eff))
}

// TestEffectObjectsMarshalAsAnArrayOnlyWhenPresent asserts the JSON key
// "objects" holds the items with their fields and is absent for an Effect
// that lists none.
func TestEffectObjectsMarshalAsAnArrayOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionDelete, DisplayKind: "project", DisplayName: "p-1",
		Objects: []safety.Object{{Kind: "namespace", Name: "default", Outcome: safety.OutcomeNotDeleted, Message: "Kubernetes never deletes it"}}}
	raw, err := json.Marshal(eff)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, []any{map[string]any{"kind": "namespace", "name": "default", "outcome": "not deleted", "message": "Kubernetes never deletes it"}}, decoded["objects"])

	eff.Objects = nil
	raw, err = json.Marshal(eff)
	require.NoError(t, err)
	decoded = nil
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.NotContains(t, decoded, "objects")
}

// TestObjectLinesGroupAndCompareByTheValuesAsGiven asserts the escape of the
// kind and the outcome is for printing only: a kind that differs from the
// headline kind only by a hidden character gets its own line, and two
// outcomes that differ only by one stay two lines, each with its own names
// and message; each line shows the character as an escape.
func TestObjectLinesGroupAndCompareByTheValuesAsGiven(t *testing.T) {
	t.Parallel()
	t.Run("kind", func(t *testing.T) {
		t.Parallel()
		objs := []safety.Object{
			{Kind: "Cluster", Name: "same", Outcome: safety.OutcomeRancherKeeps},
			{Kind: "Clu\x1bster", Name: "other", Outcome: safety.OutcomeRancherKeeps},
		}
		lines := ObjectLines(objs, "Cluster")
		assert.Equal(t, []string{`"Clu\x1bster"s:`, "  rancher keeps: other"}, lines)
	})
	t.Run("outcome", func(t *testing.T) {
		t.Parallel()
		objs := []safety.Object{
			{Kind: "namespace", Name: "a", Outcome: safety.OutcomeFailed, Message: "first"},
			{Kind: "namespace", Name: "b", Outcome: "fail\x1bed", Message: "second"},
		}
		lines := ObjectLines(objs, "project")
		assert.Equal(t, []string{"namespaces:", "  failed: a (first)", `  "fail\x1bed": b`}, lines)
	})
}

// TestObjectLinesWriteHiddenCharactersAsEscapes asserts a name or a message
// that has a hidden character or an invalid byte is written as a
// double-quoted string with escapes, a single quote stays outside the quotes,
// the labels stay as written, and a remedy command in a message prints as the
// plain command.
func TestObjectLinesWriteHiddenCharactersAsEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		obj  safety.Object
		want string
	}{
		{
			name: "a name with a zero-width space",
			obj:  safety.Object{Kind: "namespace", Name: "a\u200bb", Outcome: safety.OutcomeRancherKeeps},
			want: `  rancher keeps: "a\u200bb"`,
		},
		{
			name: "a name with a newline cannot add a line",
			obj:  safety.Object{Kind: "namespace", Name: "a\nforged line", Outcome: safety.OutcomeRancherKeeps},
			want: `  rancher keeps: "a\nforged line"`,
		},
		{
			name: "a message with an invalid byte",
			obj:  safety.Object{Kind: "namespace", Name: "a", Outcome: safety.OutcomeFailed, Message: "denied \xff"},
			want: `  failed: a ("denied \xff")`,
		},
		{
			name: "a single quote stays outside the quotes, the hidden character is quoted",
			obj:  safety.Object{Kind: "namespace", Name: "o'b\u200b", Outcome: safety.OutcomeRancherKeeps},
			want: `  rancher keeps: o'"b\u200b"`,
		},
		{
			name: "a remedy command in a message",
			obj:  safety.Object{Kind: "namespace", Name: "a", Outcome: safety.OutcomeFailed, Message: "x\u200b; " + clierr.Rancher + " get pods"},
			want: `  failed: a ("x\u200b"; rancher get pods)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, []string{"namespaces:", tc.want}, ObjectLines([]safety.Object{tc.obj}, "project"))
		})
	}
}

// TestObjectLinesKeepDifferentNamesApart asserts names that differ only by a
// hidden character or an invalid byte print differently.
func TestObjectLinesKeepDifferentNamesApart(t *testing.T) {
	t.Parallel()
	lines := map[string]bool{}
	for _, name := range []string{"a\u200bb", "a\u200cb", "ab", "a\xffb"} {
		got := ObjectLines([]safety.Object{{Kind: "namespace", Name: name, Outcome: safety.OutcomeRancherKeeps}}, "project")
		require.Len(t, got, 2)
		lines[got[1]] = true
	}
	assert.Len(t, lines, 4)
}

// outcomeConstants are the string values of the constants of file whose
// names start with Outcome.
func outcomeConstants(t *testing.T, file *ast.File) []string {
	t.Helper()
	var values []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Outcome") {
					continue
				}
				require.Less(t, i, len(vs.Values), "%s has a value", name.Name)
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s is a string literal", name.Name)
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				values = append(values, value)
			}
		}
	}
	return values
}

// TestEveryOutcomeConstantIsInObjectOutcomeOrder asserts objectOutcomeOrder
// lists the value of every Outcome constant of the safety package, in any of
// its non-test files, and no other value, and lists no value twice. Every
// constant whose name starts with Outcome is taken as an outcome value and
// must be a string literal. A constant left out would still print, but after
// the listed outcomes in the order it first appears.
func TestEveryOutcomeConstantIsInObjectOutcomeOrder(t *testing.T) {
	t.Parallel()
	dir := "safety"
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var constants []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		constants = append(constants, outcomeConstants(t, file)...)
	}
	require.NotEmpty(t, constants, "the scan found the Outcome constants")
	for _, value := range constants {
		assert.Contains(t, objectOutcomeOrder, value, "the outcome %q is in objectOutcomeOrder", value)
	}
	for _, value := range objectOutcomeOrder {
		assert.Contains(t, constants, value, "objectOutcomeOrder holds %q, which is no Outcome constant", value)
	}
	sorted := slices.Sorted(slices.Values(objectOutcomeOrder))
	assert.Equal(t, len(sorted), len(slices.Compact(sorted)), "objectOutcomeOrder has no duplicate")
}
