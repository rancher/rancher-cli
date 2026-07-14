package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// SettingValue reads a management.cattle.io/v3 Setting object's "value"
// field, falling back to "default" when value is absent or empty (rancher
// setting.go convention; mirrors internal/client/provider's
// ServerVersionSetting reading the same top-level "value" field). Shared by
// every caller that reads a Setting's effective value (provisioning's
// kubernetes-versions merge, support-bundle's settings snapshot) so the
// value-or-default fallback cannot drift between them.
func SettingValue(obj *unstructured.Unstructured) string {
	if v, found, _ := unstructured.NestedString(obj.Object, "value"); found && v != "" {
		return v
	}
	v, _, _ := unstructured.NestedString(obj.Object, "default")
	return v
}
