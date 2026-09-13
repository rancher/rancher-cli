package completion

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParallelRunsEveryIndexWithinTheWorkerBound asserts every index runs
// exactly once, at most workers at a time, and that a zero or negative
// count runs nothing.
func TestParallelRunsEveryIndexWithinTheWorkerBound(t *testing.T) {
	t.Parallel()
	const n, workers = 37, 4
	var mu sync.Mutex
	seen := map[int]int{}
	var active, peak atomic.Int32
	Parallel(n, workers, func(i int) {
		cur := active.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		mu.Lock()
		seen[i]++
		mu.Unlock()
		active.Add(-1)
	})
	assert.Len(t, seen, n)
	for i := range n {
		assert.Equal(t, 1, seen[i])
	}
	assert.LessOrEqual(t, peak.Load(), int32(workers))

	ran := false
	Parallel(0, workers, func(int) { ran = true })
	Parallel(-1, 0, func(int) { ran = true })
	assert.False(t, ran)
}
