package runtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type durationInput map[string]string

func (d durationInput) String(name string) string { return d[name] }
func (durationInput) Bool(string) bool            { return false }
func (durationInput) Strings(string) []string     { return nil }
func (d durationInput) IsSet(name string) bool    { _, ok := d[name]; return ok }
func (durationInput) Args() []string              { return nil }

// TestDurationFlag asserts the shared duration-flag contract: omitted is
// the default, a positive duration is taken as written, anything else is a
// VALIDATION error naming the flag and the example.
func TestDurationFlag(t *testing.T) {
	t.Parallel()
	d, err := runtime.DurationFlag(durationInput{}, "wait", 90*time.Second, "5m")
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)
	d, err = runtime.DurationFlag(durationInput{"wait": "2m"}, "wait", 90*time.Second, "5m")
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, d)
	for raw, want := range map[string]string{
		"soon": `--wait "soon" is not a valid duration, e.g. 5m`,
		"0":    `--wait "0" must be a positive duration, e.g. 5m`,
		"-1s":  `--wait "-1s" must be a positive duration, e.g. 5m`,
	} {
		_, err := runtime.DurationFlag(durationInput{"wait": raw}, "wait", 90*time.Second, "5m")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, raw)
		assert.Equal(t, clierr.CodeValidation, ce.Code, raw)
		require.ErrorContains(t, err, want)
	}
}
