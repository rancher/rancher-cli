package extres

import (
	"fmt"
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
		"90",      // bare number, no unit
		"-12h",    // negative
		"-1d",     // negative days
		"12h1d",   // days segment not leading
		"1d2d",    // second days segment
		"1.5d",    // fractional days
		"213504d", // the reviewer's overflow repro: wraps to ~25 minutes unbounded
	} {
		_, err := ParseTTL(in)
		require.Error(t, err, in)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, in)
		assert.Equal(t, clierr.CodeValidation, ce.Code, in)
	}
}

// TestParseTTLDaysOverflow asserts the fix for the day-count overflow: a day
// count whose *24h product would overflow int64 nanoseconds must be
// VALIDATION, never a silently wrapped small positive duration.
func TestParseTTLDaysOverflow(t *testing.T) {
	t.Parallel()

	_, err := ParseTTL("213504d")
	require.Error(t, err, "the reviewer's repro must fail, not wrap to ~25 minutes")

	got, err := ParseTTL(fmt.Sprintf("%dd", maxDays))
	require.NoError(t, err, "the exact bound must still be accepted")
	assert.Equal(t, time.Duration(maxDays)*24*time.Hour, got)

	_, err = ParseTTL(fmt.Sprintf("%dd", maxDays+1))
	require.Error(t, err, "one day past the bound must be VALIDATION")
}

func TestTTLMillisecondsSubMillisecondIsValidation(t *testing.T) {
	t.Parallel()
	ttl, err := ParseTTL("500us")
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))

	_, err = TTLMilliseconds(ttl, "500us")
	require.Error(t, err, "500us truncates to 0ms — must not silently become spec.ttl: 0")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestTTLMillisecondsZeroWhenTTLAbsent(t *testing.T) {
	t.Parallel()
	ms, err := TTLMilliseconds(0, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), ms)
}

func TestTTLSecondsSubSecondIsValidation(t *testing.T) {
	t.Parallel()
	ttl, err := ParseTTL("500ms")
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))

	_, err = TTLSeconds(ttl, "500ms")
	require.Error(t, err, "500ms truncates to 0s — must not silently become spec.ttl: 0")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestTTLSecondsZeroWhenTTLAbsent(t *testing.T) {
	t.Parallel()
	secs, err := TTLSeconds(0, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), secs)
}
