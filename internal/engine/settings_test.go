package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// settingObj builds a fixture management.cattle.io/v3 Setting object with an
// optional "value" and/or "default" top-level field - SettingValue's only
// two inputs.
func settingObj(value, def string, hasValue, hasDefault bool) *unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": "test-setting"}}
	if hasValue {
		obj["value"] = value
	}
	if hasDefault {
		obj["default"] = def
	}
	return &unstructured.Unstructured{Object: obj}
}

// TestSettingValue asserts SettingValue's value-or-default fallback contract:
// both provisioning's kubernetes-versions merge and
// support-bundle's settings snapshot depend on this behavior being
// consistent between them.
func TestSettingValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		value      string
		def        string
		hasValue   bool
		hasDefault bool
		want       string
		wantFound  bool
	}{
		{
			name:      "value present and non-empty is returned",
			value:     "v1.30.5+rke2r1",
			hasValue:  true,
			want:      "v1.30.5+rke2r1",
			wantFound: true,
		},
		{
			name:       "value present but empty falls back to default",
			value:      "",
			hasValue:   true,
			def:        "v1.30.5+rke2r1",
			hasDefault: true,
			want:       "v1.30.5+rke2r1",
			wantFound:  true,
		},
		{
			name:       "value absent falls back to default",
			hasValue:   false,
			def:        "v1.30.5+rke2r1",
			hasDefault: true,
			want:       "v1.30.5+rke2r1",
			wantFound:  true,
		},
		{
			name:       "empty default still reports found",
			hasValue:   false,
			def:        "",
			hasDefault: true,
			want:       "",
			wantFound:  true,
		},
		{
			name:       "both absent is empty and not found",
			hasValue:   false,
			hasDefault: false,
			want:       "",
			wantFound:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := settingObj(tc.value, tc.def, tc.hasValue, tc.hasDefault)
			got, found := SettingValue(obj)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantFound, found)
		})
	}
}
