package completion

import "slices"

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
