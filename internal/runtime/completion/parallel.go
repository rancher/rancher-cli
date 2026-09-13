package completion

import "sync"

// Parallel runs fn for every index in [0, n) on at most workers
// goroutines at a time and returns when all have finished. A source uses
// it for a fan-out of independent requests (one document per API group)
// so a TAB does not pay for them one after another; the Connection counts
// each request against the allowance whichever goroutine sends it.
func Parallel(n, workers int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	slots := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			fn(i)
		})
	}
	wg.Wait()
}
