package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// EachCondition walks obj's status.conditions in order, invoking fn for
// every well-formed entry until fn returns false; malformed (non-map)
// entries are skipped. The single status.conditions decoder — inspect's
// card readers and the wait engine's first-match lookup build on it.
func EachCondition(obj map[string]any, fn func(typ, status, reason, message string) bool) {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cm, "type")
		status, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		message, _, _ := unstructured.NestedString(cm, "message")
		if !fn(typ, status, reason, message) {
			return
		}
	}
}
