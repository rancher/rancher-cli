package completion

import (
	"slices"
	"strings"
)

// ItemList is the Kubernetes list envelope every listing a source reads
// arrives in: {"items": [...]}, each item an object.
type ItemList struct {
	Items []map[string]any `json:"items"`
}

// Names returns metadata.name of every item that has one, in list order.
func (l ItemList) Names() []string {
	out := make([]string, 0, len(l.Items))
	for _, item := range l.Items {
		if name := ItemName(item); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ItemName returns item's metadata.name, or "" when it has none.
func ItemName(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

// Dedupe returns the distinct strings of s, sorted; s is not modified.
func Dedupe(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

// IDsWithUniqueDisplayNames projects a list of Rancher management objects
// that carry a display name (clusters, projects) into the values a
// command resolving "id or unique display name" accepts: every
// metadata.name, plus each spec.displayName that appears exactly once
// across the list's rows and collides with no id. In list order, each
// value once; an item without metadata is skipped whole.
func IDsWithUniqueDisplayNames(items []map[string]any) []string {
	type entry struct{ id, display string }
	entries := make([]entry, 0, len(items))
	ids := map[string]bool{}
	displays := map[string]int{}
	for _, item := range items {
		if _, ok := item["metadata"].(map[string]any); !ok {
			continue
		}
		id := ItemName(item)
		display := ""
		if spec, _ := item["spec"].(map[string]any); spec != nil {
			display, _ = spec["displayName"].(string)
		}
		entries = append(entries, entry{id: id, display: display})
		if id != "" {
			ids[id] = true
		}
		if display != "" {
			displays[display]++
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.id != "" && !seen[e.id] {
			seen[e.id] = true
			out = append(out, e.id)
		}
		if e.display != "" && displays[e.display] == 1 && !ids[e.display] && !seen[e.display] {
			seen[e.display] = true
			out = append(out, e.display)
		}
	}
	return out
}

// TailFlag returns the value of --name on an arg tail: the token after a
// bare --name, or the text after --name=, or "" when the flag is absent
// or its last spelling is the last token, still without a value. A flag
// typed more than once takes its last spelling, as the command's parser
// reads it. A source uses it for a sibling flag the surface does not
// carry in Request (--namespace, --type); the value is data until a Path
// builder validates it. The completion dispatcher spells every flag the
// command defines in its long form before a source reads the tail.
func TailFlag(tail []string, name string) string {
	long := "--" + name
	value := ""
	for i, a := range tail {
		if v, ok := strings.CutPrefix(a, long+"="); ok {
			value = v
			continue
		}
		if a == long {
			value = ""
			if i+1 < len(tail) {
				value = tail[i+1]
			}
		}
	}
	return value
}
