package extres

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"90d", 90 * 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"30m", 30 * time.Minute},
		{"1d12h", 24*time.Hour + 12*time.Hour},
	}
	for _, tt := range tests {
		got, err := ParseTTL(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
}

func TestParseTTLRejections(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"garbage",
		"90",    // bare number, no unit
		"-12h",  // negative
		"-1d",   // negative days
		"12h1d", // days segment not leading
		"1d2d",  // second days segment
		"1.5d",  // fractional days
	} {
		_, err := ParseTTL(in)
		require.Error(t, err, in)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, in)
		assert.Equal(t, clierr.CodeValidation, ce.Code, in)
	}
}
