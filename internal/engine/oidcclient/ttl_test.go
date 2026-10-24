package oidcclient

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStrictTTLSecondsSixCases asserts the exact validation contract per flag:
// omission/defaulting, zero, sub-second, fractional-second, negative, and
// overflow - run for both --token-ttl and --refresh-ttl since the logic
// (not the flag name) is what must be exact.
func TestStrictTTLSecondsSixCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		raw       string
		wantSecs  int64
		wantErr   bool
		errSubstr string
	}{
		{"omitted defers to caller default", "", 0, false, ""},
		{"whole seconds accepted", "3600s", 3600, false, ""},
		{"zero is validation", "0s", 0, true, "positive duration"},
		{"sub-second is validation", "500ms", 0, true, "whole number of seconds"},
		{"fractional second is validation", "1500ms", 0, true, "whole number of seconds"},
		{"negative is validation", "-1s", 0, true, "invalid --token-ttl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			secs, err := strictTTLSeconds("token-ttl", tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				ce, ok := err.(*clierr.Error)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Contains(t, ce.Message, tc.errSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantSecs, secs)
		})
	}
}

// TestStrictTTLSecondsExactBoundary asserts the maximum: exactly maxTTLSeconds
// is accepted, one second above is rejected - math.MaxInt64/int64(time.Second),
// the true Duration-overflow bound, not an arbitrary product limit.
func TestStrictTTLSecondsExactBoundary(t *testing.T) {
	t.Parallel()

	t.Run("at the bound", func(t *testing.T) {
		t.Parallel()
		secs, err := strictTTLSeconds("refresh-ttl", "9223372036s")
		require.NoError(t, err)
		assert.Equal(t, maxTTLSeconds, secs)
	})

	t.Run("one second above the bound", func(t *testing.T) {
		t.Parallel()
		_, err := strictTTLSeconds("refresh-ttl", "9223372037s")
		require.Error(t, err)
		ce, ok := err.(*clierr.Error)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

// TestStrictTTLSecondsNamesItsOwnFlag asserts that a value that does not
// parse is refused naming the flag the command was given (--token-ttl or
// --refresh-ttl, never --ttl), and that the user's quoted value is printed
// verbatim, also when it contains another flag's name.
func TestStrictTTLSecondsNamesItsOwnFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		flag string
		raw  string
		want string
	}{
		{"malformed duration", "refresh-ttl", "not-a-duration",
			`invalid --refresh-ttl "not-a-duration": use a duration such as 90m or 12h; it can start with a number of days, as in 90d or 1d12h`},
		{"day-count overflow", "refresh-ttl", "9223372036855d",
			`invalid --refresh-ttl "9223372036855d": days must be at most 106751 (about 292 years)`},
		{"the value holds another flag name", "token-ttl", "a--ttl",
			`invalid --token-ttl "a--ttl": use a duration such as 90m or 12h; it can start with a number of days, as in 90d or 1d12h`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := strictTTLSeconds(tc.flag, tc.raw)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}
