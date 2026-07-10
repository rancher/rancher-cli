package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMinRancherVersionFloor(t *testing.T) {
	assert.Equal(t, "2.14", MinRancherVersion)
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		a, b    string
		wantCmp int
		wantOK  bool
	}{
		{"equal with absent patch on the right", "2.14", "2.14.0", 0, true},
		{"equal with absent patch on the left", "2.14.0", "2.14", 0, true},
		{"patch above floor", "v2.14.1", "2.14", 1, true},
		{"patch below a hypothetical floor", "2.14.0", "2.14.2", -1, true},
		{"minor above floor", "2.15", "2.14", 1, true},
		{"minor below floor", "2.13", "2.14", -1, true},
		{"pre-release suffix is stripped", "v2.14.3-rc1", "2.14.3", 0, true},
		{"head suffix with no patch", "v2.14-head", "2.14", 0, true},
		{"dev build is unparseable", "dev", "2.14", 0, false},
		{"empty string is unparseable", "", "2.14", 0, false},
		{"floor itself unparseable makes the pair unparseable", "2.14", "dev", 0, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmp, ok := CompareVersions(tt.a, tt.b)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantCmp, cmp)
			}
		})
	}
}

func TestEvalMinVersionSupport(t *testing.T) {
	t.Parallel()

	t.Run("at floor is supported with no warning", func(t *testing.T) {
		t.Parallel()
		supported, warning := EvalMinVersionSupport("2.14.0")
		require.NotNil(t, supported)
		assert.True(t, *supported)
		assert.Empty(t, warning)
	})

	t.Run("above floor is supported with no warning", func(t *testing.T) {
		t.Parallel()
		supported, warning := EvalMinVersionSupport("v2.15.2")
		require.NotNil(t, supported)
		assert.True(t, *supported)
		assert.Empty(t, warning)
	})

	t.Run("below floor is unsupported and warns", func(t *testing.T) {
		t.Parallel()
		supported, warning := EvalMinVersionSupport("v2.13.0")
		require.NotNil(t, supported)
		assert.False(t, *supported)
		assert.Contains(t, warning, "below the supported minimum 2.14")
	})

	t.Run("unparseable dev build is unknown, never unsupported", func(t *testing.T) {
		t.Parallel()
		supported, warning := EvalMinVersionSupport("dev")
		assert.Nil(t, supported)
		assert.Equal(t, UnknownServerVersionWarning, warning)
	})

	t.Run("empty value is unknown", func(t *testing.T) {
		t.Parallel()
		supported, warning := EvalMinVersionSupport("")
		assert.Nil(t, supported)
		assert.Equal(t, UnknownServerVersionWarning, warning)
	})
}
