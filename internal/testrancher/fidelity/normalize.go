package fidelity

import "strings"

// DefaultNormalizeRules scrub the volatile or secret fields present on most
// Rancher and Kubernetes response bodies. Presence is preserved — a
// placeholder is written in — except managedFields, which is dropped whole.
// A created ext Token carries its plaintext credential in BOTH status.bearerToken
// (the "ext/<name>:<value>" form) and status.value (the bare value); both are
// live secrets and must never reach a golden. status.hash is the token's stored
// verification digest — secret-adjacent and per-token unique — and is scrubbed
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
		applyRule(body, strings.Split(r.Path, "."), r.Replace)
	}
}

func applyRule(node any, segs []string, replace string) {
	if arr, ok := node.([]any); ok {
		for _, el := range arr {
			applyRule(el, segs, replace)
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
				setOrDelete(m, k, replace)
			}
			return
		}
		setOrDelete(m, head, replace)
		return
	}
	if head == "*" {
		for _, v := range m {
			applyRule(v, rest, replace)
		}
		return
	}
	if v, present := m[head]; present {
		applyRule(v, rest, replace)
	}
}

func setOrDelete(m map[string]any, key, replace string) {
	if _, present := m[key]; !present {
		return
	}
	if replace == "" {
		delete(m, key)
		return
	}
	m[key] = replace
}
