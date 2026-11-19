package clusterops

// nested returns the value at path below obj, and whether every step of the
// path was a map with the next key. A JSON null is found with a nil value.
func nested(obj map[string]any, path ...string) (any, bool) {
	var cur any = obj
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// nestedString returns the string at path below obj, "" when it is absent or
// not a string.
func nestedString(obj map[string]any, path ...string) string {
	v, _ := nested(obj, path...)
	s, _ := v.(string)
	return s
}

// nestedMap returns the map at path below obj, nil when it is absent, null or
// not a map.
func nestedMap(obj map[string]any, path ...string) map[string]any {
	v, _ := nested(obj, path...)
	m, _ := v.(map[string]any)
	return m
}

// conditionMessage returns the message of the status condition of the given
// type, "" when the object has no such condition.
func conditionMessage(obj map[string]any, condType string) string {
	v, _ := nested(obj, "status", "conditions")
	conditions, _ := v.([]any)
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			msg, _ := cond["message"].(string)
			return msg
		}
	}
	return ""
}
