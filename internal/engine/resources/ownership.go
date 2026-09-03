package resources

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// applyOwners returns the managers that own the field at path (a JSON
// pointer from a diff entry) through server-side apply, read from the
// object's managed fields. Managers whose operation is Update
// (controllers, kubectl edit) do not reconcile fields back and are not
// reported. Each manager is reported once, however many entries it has.
func applyOwners(managedFields []any, path string) []string {
	var owners []string
	for _, raw := range managedFields {
		entry, ok := raw.(map[string]any)
		if !ok || entry["operation"] != "Apply" {
			continue
		}
		manager, _ := entry["manager"].(string)
		fields, _ := entry["fieldsV1"].(map[string]any)
		if manager == "" || slices.Contains(owners, manager) {
			continue
		}
		if fieldsCover(fields, strings.TrimPrefix(path, "/")) {
			owners = append(owners, manager)
		}
	}
	return owners
}

// fieldsCover reports whether a fieldsV1 tree owns the field at rest, a
// JSON pointer without its leading slash. Diff paths join keys with "/"
// and never escape, while a field name may itself contain "/" (a
// domain-prefixed label or annotation key), so the walk never splits the
// pointer up front: at each node it matches the f: key that equals the
// remaining path or is a "/"-terminated prefix of it. A node whose keys
// name list items (k:, v:, i:) is a list, and the list is the unit of
// ownership: managed fields key items by value, the diff by position, so
// the walk stops there. An empty node below the root means the owner's
// entry ends above the path and owns the subtree; an empty root owns
// nothing.
func fieldsCover(node map[string]any, rest string) bool {
	consumed := false
	for {
		if len(node) == 0 {
			return consumed
		}
		if rest == "" {
			return true
		}
		if isListNode(node) {
			return true
		}
		next, remaining, ok := descend(node, rest)
		if !ok {
			return false
		}
		node, rest, consumed = next, remaining, true
	}
}

// descend finds the f: key of node that the remaining path starts with,
// preferring the longest such key, and returns its child with the path
// that remains after it.
func descend(node map[string]any, rest string) (map[string]any, string, bool) {
	var bestKey string
	for key := range node {
		name, ok := strings.CutPrefix(key, "f:")
		if !ok {
			continue
		}
		if (rest == name || strings.HasPrefix(rest, name+"/")) && len(name) > len(bestKey) {
			bestKey = name
		}
	}
	if bestKey == "" {
		return nil, "", false
	}
	child, _ := node["f:"+bestKey].(map[string]any)
	if child == nil {
		child = map[string]any{}
	}
	return child, strings.TrimPrefix(strings.TrimPrefix(rest, bestKey), "/"), true
}

// isListNode reports whether a fieldsV1 node describes list items.
func isListNode(node map[string]any) bool {
	for key := range node {
		if strings.HasPrefix(key, "k:") || strings.HasPrefix(key, "v:") || strings.HasPrefix(key, "i:") {
			return true
		}
	}
	return false
}

// ownershipWarnings renders one warning per changed field that an Apply
// manager owns, so an operator learns before confirming that the change
// may not stick.
func ownershipWarnings(managedFields []any, diff []safety.DiffEntry) []string {
	var out []string
	for _, e := range diff {
		for _, m := range applyOwners(managedFields, e.Path) {
			out = append(out, "field "+e.Path+" is applied by "+m+"; that manager may revert this change on its next reconcile")
		}
	}
	return out
}
