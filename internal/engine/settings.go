package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// SettingValue reads a management.cattle.io/v3 Setting object's "value"
// field, falling back to "default" when value is absent or empty (rancher
// setting.go convention). Shared by
// every caller that reads a Setting's effective value (provisioning's
// kubernetes-versions merge, support-bundle's settings snapshot) so the
// value-or-default fallback cannot drift between them. The bool reports
// whether the object carried either field at all — false means the server
// reports no value for this setting, which the support bundle renders as
// key absence rather than an empty string.
func SettingValue(obj *unstructured.Unstructured) (string, bool) {
	if v, found, _ := unstructured.NestedString(obj.Object, "value"); found && v != "" {
		return v, true
	}
	v, found, _ := unstructured.NestedString(obj.Object, "default")
	return v, found
}
