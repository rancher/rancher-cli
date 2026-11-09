package fidelity

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// portRangeLow and portRangeHigh bound the ports the tests give to a
	// script case: below Linux's 32768 and macOS's 49152 ephemeral ranges, so
	// a listener that asks the OS for a free port never receives one of them.
	portRangeLow  = 20000
	portRangeHigh = 29999

	// portPickAttempts bounds the candidates one request tries.
	portPickAttempts = 200

	// openPortTries bounds the ports reserveOpenPort tries before it fails.
	openPortTries = 5
)

// portAllocator gives out ports. A port it gave out is never given again by
// the same allocator. pick returns one candidate; bind reports an error when
// the port cannot be bound on 127.0.0.1.
type portAllocator struct {
	mu    sync.Mutex
	given map[int]bool
	pick  func() int
	bind  func(port int) error
}

func newPortAllocator(pick func() int, bind func(port int) error) *portAllocator {
	return &portAllocator{given: map[int]bool{}, pick: pick, bind: bind}
}

// next returns a port that was not given before and that binds, or an error
// after portPickAttempts candidates.
func (a *portAllocator) next() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for range portPickAttempts {
		port := a.pick()
		if a.given[port] {
			continue
		}
		if err := a.bind(port); err != nil {
			continue
		}
		a.given[port] = true
		return port, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d after %d attempts", portRangeLow, portRangeHigh, portPickAttempts)
}

func pickPortInRange() int {
	return portRangeLow + rand.IntN(portRangeHigh-portRangeLow+1)
}

// release makes a port available to the allocator again.
func (a *portAllocator) release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.given, port)
}

// bindLoopbackPort listens on the port and closes the listener at once.
func bindLoopbackPort(port int) error {
	listener, err := listenLoopbackPort(port)
	if err != nil {
		return err
	}
	return listener.Close()
}

func listenLoopbackPort(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// testPorts is the allocator every script case draws from, so that no two
// cases of the test process receive the same port. A port returns to it when
// the test that reserved it ends, so the range of 10000 ports does not run out
// over a long run.
var testPorts = newPortAllocator(pickPortInRange, bindLoopbackPort)

// reserveFreePort returns a port in the range that no other case received and
// that no listener asking the OS for a free port can hold.
func reserveFreePort(t *testing.T) int {
	t.Helper()
	port, err := testPorts.next()
	require.NoError(t, err)
	t.Cleanup(func() { testPorts.release(port) })
	return port
}

// reserveOpenPort returns a port with its listener open; the listener closes
// when the test ends. A case that binds its own port holds it from the moment
// it is chosen.
func reserveOpenPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	port, listener, err := openPort(testPorts, listenLoopbackPort)
	require.NoError(t, err)
	t.Cleanup(func() { testPorts.release(port) })
	t.Cleanup(func() { _ = listener.Close() })
	return port, listener
}

// openPort takes a port from the allocator and listens on it. The allocator's
// check closed the port, so another process can take it before the listen;
// on a listen error it takes another port, at most openPortTries times.
func openPort(a *portAllocator, listen func(port int) (net.Listener, error)) (int, net.Listener, error) {
	var errs []error
	for range openPortTries {
		port, err := a.next()
		if err != nil {
			return 0, nil, errors.Join(append(errs, err)...)
		}
		listener, err := listen(port)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return port, listener, nil
	}
	return 0, nil, fmt.Errorf("no port could be held after %d tries: %w", openPortTries, errors.Join(errs...))
}

// portSequence returns a pick function that returns the given ports in order and
// fails the test when it runs out.
func portSequence(t *testing.T, ports ...int) func() int {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		require.Less(t, i, len(ports), "the allocator asked for more candidates than the test gave")
		port := ports[i]
		i++
		return port
	}
}

func acceptEveryPort(int) error { return nil }

// TestPortAllocatorSkipsAPortAlreadyGiven asserts a port that was given is
// skipped when it is picked again.
func TestPortAllocatorSkipsAPortAlreadyGiven(t *testing.T) {
	t.Parallel()
	const first, second = 20001, 20002
	a := newPortAllocator(portSequence(t, first, first, second), acceptEveryPort)
	got, err := a.next()
	require.NoError(t, err)
	require.Equal(t, first, got)
	got, err = a.next()
	require.NoError(t, err)
	assert.Equal(t, second, got)
}

// TestPortAllocatorSkipsAPortThatDoesNotBind asserts a refused port is
// skipped and not given.
func TestPortAllocatorSkipsAPortThatDoesNotBind(t *testing.T) {
	t.Parallel()
	const refused, free = 20001, 20002
	a := newPortAllocator(portSequence(t, refused, free), func(port int) error {
		if port == refused {
			return errors.New("in use")
		}
		return nil
	})
	got, err := a.next()
	require.NoError(t, err)
	assert.Equal(t, free, got)
}

// TestPortAllocatorPicksInsideTheRange asserts the real pick function stays
// inside the range outside the OS ephemeral ranges.
func TestPortAllocatorPicksInsideTheRange(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for range 5000 {
		port := pickPortInRange()
		require.GreaterOrEqual(t, port, 20000)
		require.LessOrEqual(t, port, 29999)
		seen[port] = true
	}
	assert.Greater(t, len(seen), 1000, "picks must spread over the range")
}

// TestPortAllocatorGivesUpWhenNoPortBinds asserts the allocator stops after
// its bound with an error that names the range.
func TestPortAllocatorGivesUpWhenNoPortBinds(t *testing.T) {
	t.Parallel()
	calls := 0
	a := newPortAllocator(func() int { calls++; return 20000 + calls }, func(int) error { return errors.New("in use") })
	_, err := a.next()
	require.ErrorContains(t, err, "20000-29999")
	assert.Equal(t, portPickAttempts, calls)
}

// TestPortAllocatorGivesEveryConcurrentCallerAnOwnPort asserts that callers
// at the same time receive distinct ports, with a pick that offers every port
// twice in a row.
func TestPortAllocatorGivesEveryConcurrentCallerAnOwnPort(t *testing.T) {
	t.Parallel()
	var picks atomic.Int64
	a := newPortAllocator(func() int { return 20000 + int(picks.Add(1)/2) }, acceptEveryPort)
	const callers = 64
	ports := make([]int, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			port, err := a.next()
			assert.NoError(t, err)
			ports[i] = port
		})
	}
	wg.Wait()
	distinct := map[int]bool{}
	for _, port := range ports {
		distinct[port] = true
	}
	assert.Len(t, distinct, callers)
}

// TestOpenPortTakesAnotherPortWhenTheListenFails asserts a failed listen on a
// port the allocator checked moves on to another port.
func TestOpenPortTakesAnotherPortWhenTheListenFails(t *testing.T) {
	t.Parallel()
	a := newPortAllocator(portSequence(t, 20001, 20002), acceptEveryPort)
	listens := 0
	port, listener, err := openPort(a, func(port int) (net.Listener, error) {
		listens++
		if listens == 1 {
			return nil, errors.New("taken since the check")
		}
		return listenLoopbackPort(0)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	assert.Equal(t, 20002, port)
	assert.Equal(t, 2, listens)
}

// TestOpenPortFailsWhenEveryListenFails asserts the open port helper stops
// after its bound and reports the listen errors.
func TestOpenPortFailsWhenEveryListenFails(t *testing.T) {
	t.Parallel()
	next := 20000
	a := newPortAllocator(func() int { next++; return next }, acceptEveryPort)
	listens := 0
	_, _, err := openPort(a, func(int) (net.Listener, error) {
		listens++
		return nil, errors.New("taken since the check")
	})
	require.ErrorContains(t, err, "taken since the check")
	assert.Equal(t, openPortTries, listens)
}

// TestReserveOpenPortHoldsThePort asserts the listener of the helper is open
// on the port it returns, and that the port cannot be bound again.
func TestReserveOpenPortHoldsThePort(t *testing.T) {
	t.Parallel()
	port, listener := reserveOpenPort(t)
	assert.GreaterOrEqual(t, port, portRangeLow)
	assert.LessOrEqual(t, port, portRangeHigh)
	assert.Equal(t, port, listener.Addr().(*net.TCPAddr).Port)
	assert.Error(t, bindLoopbackPort(port))
}

// TestPortAllocatorGivesAReleasedPortAgain asserts a port that was released is
// given again.
func TestPortAllocatorGivesAReleasedPortAgain(t *testing.T) {
	t.Parallel()
	const port = 20001
	a := newPortAllocator(portSequence(t, port, port), acceptEveryPort)
	got, err := a.next()
	require.NoError(t, err)
	require.Equal(t, port, got)
	a.release(port)
	got, err = a.next()
	require.NoError(t, err)
	assert.Equal(t, port, got)
}

// TestReservedPortsReturnWhenTheTestEnds asserts that reserveFreePort and
// reserveOpenPort give their ports back to the allocator at the end of the
// test that reserved them.
func TestReservedPortsReturnWhenTheTestEnds(t *testing.T) {
	t.Parallel()
	var free, open int
	t.Run("reserve", func(t *testing.T) {
		free = reserveFreePort(t)
		open, _ = reserveOpenPort(t)
	})
	testPorts.mu.Lock()
	defer testPorts.mu.Unlock()
	assert.NotContains(t, testPorts.given, free)
	assert.NotContains(t, testPorts.given, open)
}

// TestOpenPortReportsEveryListenError asserts that when the allocator gives up
// after a failed listen, the error holds the listen error too.
func TestOpenPortReportsEveryListenError(t *testing.T) {
	t.Parallel()
	binds := 0
	a := newPortAllocator(func() int { return 20001 + binds }, func(int) error {
		binds++
		if binds > 1 {
			return errors.New("in use")
		}
		return nil
	})
	_, _, err := openPort(a, func(int) (net.Listener, error) { return nil, errors.New("taken since the check") })
	require.ErrorContains(t, err, "taken since the check")
	require.ErrorContains(t, err, "20000-29999")
}
