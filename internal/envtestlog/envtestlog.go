// Package envtestlog keeps the output of the etcd and kube-apiserver
// processes envtest starts, so a failed start can report why a process
// exited. It is test-only: test files import it, production code never
// does, and its writers are parameters so no non-test file names os.Stderr.
package envtestlog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	// attachEnv is the variable envtest itself reads to send control plane
	// output to the test process's stdout and stderr.
	attachEnv = "KUBEBUILDER_ATTACH_CONTROL_PLANE_OUTPUT"

	// tailLimit is how much of a process's output a Tail keeps, and how much
	// of the kube-apiserver's a failed start shows.
	tailLimit = 64 << 10

	// etcdShown is how much of etcd's output a failed start shows.
	etcdShown = 8 << 10
)

// Tail is an io.Writer that keeps the last 64 KiB written to it. It is safe
// for concurrent use; the zero value is ready.
type Tail struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p, dropping the oldest bytes beyond the limit.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= tailLimit {
		t.buf = append(t.buf[:0], p[len(p)-tailLimit:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - tailLimit; over > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[over:])]
	}
	return len(p), nil
}

// Bytes returns a copy of the kept output.
func (t *Tail) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

// String returns the kept output.
func (t *Tail) String() string { return string(t.Bytes()) }

// Capture gives etcd and the kube-apiserver of env one Tail each, used as
// both Out and Err of that process, and returns a function that adds the
// tails to the error from env.Start. Call it before Start. The same process
// objects run on each of envtest's start tries, so a Tail spans all of them.
//
// When env.AttachControlPlaneOutput is set or
// KUBEBUILDER_ATTACH_CONTROL_PLANE_OUTPUT is exactly "true", as envtest
// itself decides, each process's Out also goes to stdout and its Err to
// stderr (a nil writer is skipped). Both processes write to them at once, so
// they must be safe for concurrent use; the caller passes os.Stdout and
// os.Stderr.
//
// The returned function gives a nil error back as nil. Otherwise it returns
// the error followed by the kube-apiserver and then the etcd output, with the
// original error still reachable through errors.Is and errors.As.
func Capture(env *envtest.Environment, stdout, stderr io.Writer) func(error) error {
	if env.ControlPlane.Etcd == nil {
		env.ControlPlane.Etcd = &envtest.Etcd{}
	}
	etcd := env.ControlPlane.Etcd
	apiServer := env.ControlPlane.GetAPIServer()

	attach := env.AttachControlPlaneOutput || os.Getenv(attachEnv) == "true"
	apiTail, etcdTail := &Tail{}, &Tail{}
	for _, p := range []struct {
		tail     *Tail
		out, err *io.Writer
	}{
		{etcdTail, &etcd.Out, &etcd.Err},
		{apiTail, &apiServer.Out, &apiServer.Err},
	} {
		*p.out, *p.err = p.tail, p.tail
		if attach {
			*p.out, *p.err = fanOut(p.tail, stdout), fanOut(p.tail, stderr)
		}
	}

	return func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w%s%s", err,
			section("kube-apiserver", apiTail.Bytes(), tailLimit),
			section("etcd", etcdTail.Bytes(), etcdShown))
	}
}

// fanOut writes to tail and, unless w is nil, to w.
func fanOut(tail *Tail, w io.Writer) io.Writer {
	if w == nil {
		return tail
	}
	return io.MultiWriter(tail, w)
}

// section renders one process's output under a label that gives the length
// shown, or "(no output)" when the process wrote nothing. Output longer than
// limit is cut to start right after the first newline at or after the cut
// point, or at the cut point itself when no newline follows or the newline is
// the last byte.
func section(name string, out []byte, limit int) string {
	if len(out) == 0 {
		return fmt.Sprintf("\n%s output: (no output)", name)
	}
	shown := out
	if len(out) > limit {
		shown = out[len(out)-limit:]
		if i := bytes.IndexByte(shown, '\n'); i >= 0 && i+1 < len(shown) {
			shown = shown[i+1:]
		}
	}
	return fmt.Sprintf("\n%s output (last %d bytes):\n%s", name, len(shown), shown)
}
