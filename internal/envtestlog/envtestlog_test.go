package envtestlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The documented sizes, written out so a change to the limits fails here.
const (
	wantTailBytes = 64 * 1024
	wantEtcdBytes = 8 * 1024
)

var errStart = errors.New("start failed")

// capture returns an environment passed through Capture with attach output
// off, its wrap function, and the writers the processes were given.
func capture(t *testing.T) (wrap func(error) error, etcd, apiserver io.Writer) {
	t.Helper()
	t.Setenv(attachEnv, "")
	env := &envtest.Environment{}
	wrap = Capture(env, io.Discard, io.Discard)
	require.NotNil(t, env.ControlPlane.Etcd)
	return wrap, env.ControlPlane.Etcd.Out, env.ControlPlane.GetAPIServer().Out
}

func lines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %06d\n", i)
	}
	return b.String()
}

// TestTailKeepsLastBytes asserts a Tail keeps only the last 64 KiB written,
// across many small writes and across one write larger than the limit.
func TestTailKeepsLastBytes(t *testing.T) {
	t.Parallel()
	all := lines(8000)
	require.Greater(t, len(all), wantTailBytes)

	var small Tail
	for chunk := all; chunk != ""; {
		n := min(len(chunk), 1000)
		written, err := small.Write([]byte(chunk[:n]))
		require.NoError(t, err)
		require.Equal(t, n, written)
		chunk = chunk[n:]
	}
	assert.Equal(t, all[len(all)-wantTailBytes:], small.String())

	var big Tail
	written, err := big.Write([]byte(all))
	require.NoError(t, err)
	assert.Equal(t, len(all), written)
	assert.Equal(t, all[len(all)-wantTailBytes:], big.String())

	var short Tail
	_, err = short.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, "abc", short.String())
}

// TestTailConcurrentWrites asserts concurrent writers neither race nor lose
// the size bound; run under -race.
func TestTailConcurrentWrites(t *testing.T) {
	t.Parallel()
	var tail Tail
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	chunk := bytes.Repeat([]byte("x"), 1000)
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := tail.Write(chunk); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				_ = tail.String()
			}
		})
	}
	wg.Wait()
	require.Empty(t, errs)
	assert.Len(t, tail.String(), wantTailBytes)
}

// TestCaptureGivesEveryProcessOneTail asserts Capture creates the etcd
// settings when absent and sets the same writer as Out and Err of each
// process.
func TestCaptureGivesEveryProcessOneTail(t *testing.T) {
	t.Setenv(attachEnv, "")
	env := &envtest.Environment{}
	require.Nil(t, env.ControlPlane.Etcd)
	_ = Capture(env, io.Discard, io.Discard)
	require.NotNil(t, env.ControlPlane.Etcd)
	api := env.ControlPlane.GetAPIServer()
	for name, process := range map[string][2]io.Writer{
		"etcd":           {env.ControlPlane.Etcd.Out, env.ControlPlane.Etcd.Err},
		"kube-apiserver": {api.Out, api.Err},
	} {
		require.NotNil(t, process[0], name)
		assert.Same(t, process[0], process[1], name)
	}
	assert.NotSame(t, env.ControlPlane.Etcd.Out, api.Out)
}

// TestWrapNilPassesThrough asserts a nil start error stays nil.
func TestWrapNilPassesThrough(t *testing.T) {
	wrap, _, _ := capture(t)
	assert.NoError(t, wrap(nil))
}

// TestWrapOrderLabelsAndLimits asserts the wrapped error is the original
// message followed by the kube-apiserver section and then the etcd section,
// each labelled with the length of the output it shows, and that the
// original error stays in the chain.
func TestWrapOrderLabelsAndLimits(t *testing.T) {
	wrap, etcd, api := capture(t)
	_, err := io.WriteString(etcd, "etcd says hi\n")
	require.NoError(t, err)
	_, err = io.WriteString(api, "api says hi\n")
	require.NoError(t, err)

	wrapped := wrap(errStart)
	require.ErrorIs(t, wrapped, errStart)
	assert.Equal(t, "start failed"+
		"\nkube-apiserver output (last 12 bytes):\napi says hi\n"+
		"\netcd output (last 13 bytes):\netcd says hi\n", wrapped.Error())
}

// TestWrapNoOutput asserts a process that wrote nothing reads "(no output)".
func TestWrapNoOutput(t *testing.T) {
	wrap, _, _ := capture(t)
	assert.Equal(t, "start failed"+
		"\nkube-apiserver output: (no output)"+
		"\netcd output: (no output)", wrap(errStart).Error())
}

// TestWrapKubeAPIServerShowsWholeBuffer asserts the kube-apiserver section
// carries the whole 64 KiB buffer and no more.
func TestWrapKubeAPIServerShowsWholeBuffer(t *testing.T) {
	wrap, _, api := capture(t)
	all := lines(8000)
	_, err := io.WriteString(api, all)
	require.NoError(t, err)

	want := all[len(all)-wantTailBytes:]
	got := wrap(errStart).Error()
	assert.Contains(t, got, fmt.Sprintf("\nkube-apiserver output (last %d bytes):\n%s\netcd output", wantTailBytes, want))
}

// TestWrapEtcdCutStartsAtLine asserts a cut etcd tail starts right after the
// first newline at or after the cut point, and that its label counts the
// bytes shown.
func TestWrapEtcdCutStartsAtLine(t *testing.T) {
	wrap, etcd, _ := capture(t)
	all := lines(2000)
	_, err := io.WriteString(etcd, all)
	require.NoError(t, err)

	cut := len(all) - wantEtcdBytes
	nl := strings.IndexByte(all[cut:], '\n')
	require.GreaterOrEqual(t, nl, 0)
	shown := all[cut+nl+1:]
	require.True(t, strings.HasPrefix(shown, "line "))

	got := wrap(errStart).Error()
	assert.True(t, strings.HasSuffix(got, fmt.Sprintf("\netcd output (last %d bytes):\n%s", len(shown), shown)))
	assert.Less(t, len(shown), wantEtcdBytes)
}

// TestWrapEtcdCutWithoutNewlineIsRaw asserts that when the last 8 KiB hold
// no newline, the tail is the raw last 8 KiB.
func TestWrapEtcdCutWithoutNewlineIsRaw(t *testing.T) {
	wrap, etcd, _ := capture(t)
	all := "head\n" + strings.Repeat("y", 20000)
	_, err := io.WriteString(etcd, all)
	require.NoError(t, err)

	shown := all[len(all)-wantEtcdBytes:]
	got := wrap(errStart).Error()
	assert.True(t, strings.HasSuffix(got, fmt.Sprintf("\netcd output (last %d bytes):\n%s", wantEtcdBytes, shown)))
}

// TestWrapEtcdCutNewlineOnlyAtEndIsRaw asserts that when the only newline in
// the last 8 KiB is the last byte, the tail is the raw last 8 KiB rather
// than nothing.
func TestWrapEtcdCutNewlineOnlyAtEndIsRaw(t *testing.T) {
	wrap, etcd, _ := capture(t)
	all := "head\n" + strings.Repeat("y", 20000) + "\n"
	_, err := io.WriteString(etcd, all)
	require.NoError(t, err)

	shown := all[len(all)-wantEtcdBytes:]
	got := wrap(errStart).Error()
	assert.True(t, strings.HasSuffix(got, fmt.Sprintf("\netcd output (last %d bytes):\n%s", wantEtcdBytes, shown)))
}

// TestWrapEtcdBelowLimitIsWhole asserts an etcd buffer of at most 8 KiB is
// shown entire, starting mid-line or not.
func TestWrapEtcdBelowLimitIsWhole(t *testing.T) {
	wrap, etcd, _ := capture(t)
	all := strings.Repeat("z", wantEtcdBytes)
	_, err := io.WriteString(etcd, all)
	require.NoError(t, err)
	got := wrap(errStart).Error()
	assert.True(t, strings.HasSuffix(got, fmt.Sprintf("\netcd output (last %d bytes):\n%s", wantEtcdBytes, all)))
}

// TestCaptureAttachVariable asserts that with
// KUBEBUILDER_ATTACH_CONTROL_PLANE_OUTPUT=true, or with the environment's
// AttachControlPlaneOutput set, each process's output also reaches the given
// writers (Out to stdout, Err to stderr) as well as its Tail, and that any
// other value leaves the writers untouched.
func TestCaptureAttachVariable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    string
		field    bool
		attached bool
	}{
		{name: "true", value: "true", attached: true},
		{name: "field", value: "", field: true, attached: true},
		{name: "unset", value: "", attached: false},
		{name: "one", value: "1", attached: false},
		{name: "upper", value: "TRUE", attached: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(attachEnv, tc.value)
			var stdout, stderr bytes.Buffer
			env := &envtest.Environment{AttachControlPlaneOutput: tc.field}
			wrap := Capture(env, &stdout, &stderr)
			api := env.ControlPlane.GetAPIServer()
			for _, w := range []struct {
				writer io.Writer
				text   string
			}{
				{env.ControlPlane.Etcd.Out, "etcd out\n"},
				{env.ControlPlane.Etcd.Err, "etcd err\n"},
				{api.Out, "api out\n"},
				{api.Err, "api err\n"},
			} {
				_, err := io.WriteString(w.writer, w.text)
				require.NoError(t, err)
			}

			got := wrap(errStart).Error()
			for _, text := range []string{"etcd out\n", "etcd err\n", "api out\n", "api err\n"} {
				assert.Contains(t, got, text)
			}
			if tc.attached {
				assert.Equal(t, "etcd out\napi out\n", stdout.String())
				assert.Equal(t, "etcd err\napi err\n", stderr.String())
			} else {
				assert.Empty(t, stdout.String())
				assert.Empty(t, stderr.String())
			}
		})
	}
}

// TestCaptureAttachWithoutWriters asserts a nil writer is skipped rather than
// failing the first write.
func TestCaptureAttachWithoutWriters(t *testing.T) {
	t.Setenv(attachEnv, "true")
	env := &envtest.Environment{}
	wrap := Capture(env, nil, nil)
	_, err := io.WriteString(env.ControlPlane.Etcd.Out, "out\n")
	require.NoError(t, err)
	_, err = io.WriteString(env.ControlPlane.Etcd.Err, "err\n")
	require.NoError(t, err)
	assert.Contains(t, wrap(errStart).Error(), "out\nerr\n")
}

// TestStartFailureCarriesProcessOutput asserts that when etcd exits before it
// is ready, the error from Start, passed through the wrap function, carries
// the output the process wrote and keeps envtest's own error in the chain.
// The API server never starts.
func TestStartFailureCarriesProcessOutput(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(fakeEtcdEnv, "1")
	t.Setenv(attachEnv, "")
	t.Setenv("USE_EXISTING_CLUSTER", "")

	env := &envtest.Environment{}
	wrap := Capture(env, io.Discard, io.Discard)
	env.ControlPlane.Etcd.Path = self
	env.ControlPlane.Etcd.DataDir = filepath.Join(t.TempDir(), "etcd")
	env.ControlPlane.GetAPIServer().Path = self

	_, startErr := env.Start()
	require.Error(t, startErr)
	wrapped := wrap(startErr)
	require.ErrorIs(t, wrapped, startErr)
	t.Logf("wrapped error:\n%s", wrapped)

	require.ErrorContains(t, wrapped, "kube-apiserver output: (no output)")
	require.ErrorContains(t, wrapped, "\netcd output (last ")
	require.ErrorContains(t, wrapped, fakeEtcdMarker)
}
