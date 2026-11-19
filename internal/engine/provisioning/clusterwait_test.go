package provisioning

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadClusterWait asserts how --wait and --timeout are read: the wait is
// on unless --wait=false is given, --timeout is a positive duration with a
// 30m default, and --wait=false with --timeout is refused.
func TestReadClusterWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   mcInput
		want clusterWait
		err  string
	}{
		{name: "default", in: mcInput{}, want: clusterWait{On: true, Timeout: 30 * time.Minute, TimeoutText: "30m"}},
		{name: "--wait", in: mcInput{b: map[string]bool{"wait": true}, is: map[string]bool{"wait": true}}, want: clusterWait{On: true, Timeout: 30 * time.Minute, TimeoutText: "30m"}},
		{name: "--wait=false", in: mcInput{b: map[string]bool{"wait": false}, is: map[string]bool{"wait": true}}, want: clusterWait{}},
		{name: "--timeout 5m", in: mcInput{s: map[string]string{"timeout": "5m"}, is: map[string]bool{"timeout": true}}, want: clusterWait{On: true, Timeout: 5 * time.Minute, TimeoutText: "5m"}},
		{name: "--timeout 2h", in: mcInput{s: map[string]string{"timeout": "2h"}, is: map[string]bool{"timeout": true}}, want: clusterWait{On: true, Timeout: 2 * time.Hour, TimeoutText: "2h"}},
		{name: "--timeout 0s", in: mcInput{s: map[string]string{"timeout": "0s"}, is: map[string]bool{"timeout": true}}, err: `--timeout "0s" must be a positive duration`},
		{name: "--timeout x", in: mcInput{s: map[string]string{"timeout": "x"}, is: map[string]bool{"timeout": true}}, err: `--timeout "x" is not a valid duration`},
		{name: "--wait=false --timeout 5m", in: mcInput{b: map[string]bool{"wait": false}, s: map[string]string{"timeout": "5m"}, is: map[string]bool{"wait": true, "timeout": true}},
			err: "--wait=false conflicts with --timeout; remove one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := readClusterWait(tc.in)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCommittedWaitError asserts the error that ends a wait after the request
// was accepted is GENERIC, not retryable, and carries the recovery command as
// its hint and in its committed details.
func TestCommittedWaitError(t *testing.T) {
	t.Parallel()
	recovery := clierr.NewCommand("get").Word("provisioning-cluster").Arg("prod").FlagValue("--namespace", "fleet-default")
	err := committedWaitError("provisioning-cluster", "prod", recovery, "timed out")
	assert.Equal(t, clierr.CodeGeneric, err.Code)
	assert.False(t, err.Retryable)
	assert.Equal(t, "timed out", err.Message)
	assert.Equal(t, recovery.Field(), err.Hint)
	details, ok := err.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "provisioning-cluster", ResourceName: "prod", Recovery: recovery.Field()}, details)
}

// TestClusterWaitFlags asserts the flags are --wait, a bool, and --timeout, a
// string.
func TestClusterWaitFlags(t *testing.T) {
	t.Parallel()
	flags := clusterWaitFlags()
	require.Len(t, flags, 2)
	assert.Equal(t, "wait", flags[0].Name)
	assert.Equal(t, "bool", flags[0].Type)
	assert.Equal(t, "timeout", flags[1].Name)
	assert.Equal(t, "string", flags[1].Type)
}

// TestClusterWaitIntervalIsTheTestMainOverride asserts the package's tests
// poll at the interval their TestMain sets, and that
// SetClusterWaitIntervalForTest sets the value and its restore function puts
// the old one back. It is not parallel: it changes the interval the parallel
// tests read.
func TestClusterWaitIntervalIsTheTestMainOverride(t *testing.T) {
	assert.Equal(t, time.Millisecond, clusterWaitInterval)
	restore := SetClusterWaitIntervalForTest(time.Hour)
	assert.Equal(t, time.Hour, clusterWaitInterval)
	restore()
	assert.Equal(t, time.Millisecond, clusterWaitInterval)
}
