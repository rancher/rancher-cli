package fidelity

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultNormalizeRules scrub the volatile or secret fields present on most
// Rancher and Kubernetes response bodies. Presence is preserved - a
// placeholder is written in - except managedFields, which is dropped whole.
// A created ext Token carries its plaintext credential in BOTH status.bearerToken
// (the "ext/<name>:<value>" form) and status.value (the bare value); both are
// live secrets and must never reach a golden. status.hash is the token's stored
// verification digest - secret-adjacent and per-token unique - and is scrubbed
// too. status.lastUpdateTime is a per-create timestamp, the same volatile class as
// status.expiresAt.
func DefaultNormalizeRules() []NormalizeRule {
	return []NormalizeRule{
		{Path: "metadata.uid", Replace: "<uid>"},
		{Path: "metadata.resourceVersion", Replace: "<rv>"},
		{Path: "metadata.creationTimestamp", Replace: "<ts>"},
		{Path: "metadata.managedFields"},
		{Path: "status.expiresAt", Replace: "<ts>"},
		{Path: "status.lastUpdateTime", Replace: "<ts>"},
		{Path: "status.bearerToken", Replace: "<secret>"},
		{Path: "status.value", Replace: "<secret>"},
		{Path: "status.hash", Replace: "<secret>"},
	}
}

// Normalize applies rules to a decoded JSON body in place. Bodies are
// map[string]any/[]any trees straight out of encoding/json.
func Normalize(body map[string]any, rules []NormalizeRule) {
	for _, r := range rules {
		applyRule(body, strings.Split(r.Path, "."), r)
	}
}

func applyRule(node any, segs []string, r NormalizeRule) {
	if arr, ok := node.([]any); ok {
		for _, el := range arr {
			applyRule(el, segs, r)
		}
		return
	}
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	head, rest := segs[0], segs[1:]
	if len(rest) == 0 {
		if head == "*" {
			for k := range m {
				setOrDelete(m, k, r)
			}
			return
		}
		setOrDelete(m, head, r)
		return
	}
	if head == "*" {
		for _, v := range m {
			applyRule(v, rest, r)
		}
		return
	}
	if v, present := m[head]; present {
		applyRule(v, rest, r)
	}
}

func setOrDelete(m map[string]any, key string, r NormalizeRule) {
	v, present := m[key]
	if !present {
		return
	}
	if r.SortBy != "" {
		// SortBy orders the addressed array in place; a value that is not
		// an array is left as it is (the golden's shape shows the misuse).
		if items, ok := v.([]any); ok {
			sort.SliceStable(items, func(i, j int) bool {
				return nestedString(items[i], r.SortBy) < nestedString(items[j], r.SortBy)
			})
		}
		return
	}
	if r.KeepPrefix != "" {
		kept, err := keepPrefix(v, r.KeepPrefix)
		if err != nil {
			// The addressed value is not the array of strings the rule
			// expects. It was named because it carries instance data, so
			// it is never left in place: the whole value becomes the
			// placeholder, and the golden's shape shows the misuse.
			m[key] = "<group>"
			return
		}
		m[key] = kept
		return
	}
	if r.Replace == "" {
		delete(m, key)
		return
	}
	m[key] = r.Replace
}

// keepPrefix replaces every element of arr not starting with prefix with
// "<group>", keeping order and count. arr must be a []any of strings; any
// other shape is an error.
func keepPrefix(arr any, prefix string) ([]any, error) {
	elems, ok := arr.([]any)
	if !ok {
		return nil, fmt.Errorf("KeepPrefix: value is not an array")
	}
	out := make([]any, len(elems))
	for i, el := range elems {
		s, ok := el.(string)
		if !ok {
			return nil, fmt.Errorf("KeepPrefix: element %d is not a string", i)
		}
		if strings.HasPrefix(s, prefix) {
			out[i] = s
		} else {
			out[i] = "<group>"
		}
	}
	return out, nil
}

// nestedString reads a dot-separated path of string keys from a decoded
// JSON value; "" when any step is missing or not a string.
func nestedString(v any, path string) string {
	for k := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}
