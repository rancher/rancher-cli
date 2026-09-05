package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// FeatureEffectiveValue reads a v3 Feature object's effective-enabled value:
// spec.value falling back to status.default - the one fallback rule the
// feature kind's VALUE table column also declares. A nil return means both
// fields were absent or not bools; the value is never guessed as false.
// This is the canonical accessor for the pair of field paths; new readers
// use it rather than re-deriving the paths.
func FeatureEffectiveValue(obj *unstructured.Unstructured) *bool {
	if v, ok, _ := unstructured.NestedBool(obj.Object, "spec", "value"); ok {
		return &v
	}
	if v, ok, _ := unstructured.NestedBool(obj.Object, "status", "default"); ok {
		return &v
	}
	return nil
}

// FeaturePrimeOnly reads a v3 Feature object's status.prime classification -
// whether the feature is only available in Rancher Prime builds. Nil means
// the field was absent (an older server) or not a bool: unknown, never false.
func FeaturePrimeOnly(obj *unstructured.Unstructured) *bool {
	if v, ok, _ := unstructured.NestedBool(obj.Object, "status", "prime"); ok {
		return &v
	}
	return nil
}
