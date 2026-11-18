package nodes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestFormatDuration asserts a duration is written in whole units with the
// zero units left out, as the flags' help shows a default.
func TestFormatDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Minute, "2m"},
		{3 * time.Minute, "3m"},
		{90 * time.Minute, "1h30m"},
		{90 * time.Second, "1m30s"},
		{3 * time.Hour, "3h"},
		{time.Second, "1s"},
		{time.Hour + 5*time.Second, "1h5s"},
		{1500 * time.Millisecond, "1.5s"},
		{0, "0s"},
	} {
		assert.Equal(t, tc.want, formatDuration(tc.d), tc.d.String())
	}
}
