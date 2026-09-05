package access

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// fanOutLimit bounds concurrent reviews in every fan-out; fanOutBudget
// bounds the whole fan-out operation (target discovery, subject and
// group reads, reviews), not one request. Both are variables so tests
// can shrink them.
var (
	fanOutLimit  = 5
	fanOutBudget = 60 * time.Second
)

// errFanOutBudget is the cause carried by the budget context.
var errFanOutBudget = errors.New("fan-out budget exhausted")

// fanOutContext opens the one budget context for a fan-out command. It is
// created before target discovery and subject resolution, so a slow LIST
// or UserAttribute read spends the same budget the reviews do.
func fanOutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, fanOutBudget, errFanOutBudget)
}

// budgetError names the budget as the cause when it cancelled the stage.
func budgetError(ctx context.Context, err error, stage string) error {
	if err == nil || !errors.Is(context.Cause(ctx), errFanOutBudget) {
		return err
	}
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("fan-out budget of %s exhausted during %s; no review was posted", fanOutBudget, stage))
}

// member is one fan-out row: a namespace for can --project and list, a
// candidate for who-can.
type member[T any] struct {
	Key    string
	Source MemberSource
	Issue  *Issue
	Value  T
}

// fanOutFailure is the outcome that aborted a fan-out; Kind and Status
// let a caller remap exactly one shape (see can.go) and nothing else.
type fanOutFailure struct {
	Kind   ReviewOutcomeKind
	Status int
	Err    *clierr.Error
}

func (f *fanOutFailure) Error() string { return f.Err.Error() }

// runFanOut evaluates fn for every key under the fan-out limit and budget
// and applies the review outcome matrix: a degradable outcome becomes the
// member's issue; the first terminal outcome (or budget exhaustion) aborts
// the whole fan-out and is returned with the evaluated count. Members come
// back in key order regardless of completion order.
func runFanOut[T any](parent context.Context, keys []string, fn func(ctx context.Context, key string) (T, reviewResult)) ([]member[T], *fanOutFailure) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	out := make([]member[T], len(keys))
	var mu sync.Mutex
	var terminal *fanOutFailure
	evaluated := 0
	// A fixed worker pool instead of errgroup.SetLimit: SetLimit's Go
	// blocks on its slot without watching the context, so a queued member
	// would still start after a terminal result freed the slot. Here a
	// worker re-checks the context after taking a key and before calling
	// fn, so nothing starts once a terminal member cancelled the run.
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(fanOutLimit, len(keys)) {
		wg.Go(func() {
			for {
				var i int
				select {
				case <-ctx.Done():
					return
				case idx, ok := <-next:
					if !ok {
						return
					}
					i = idx
				}
				if ctx.Err() != nil {
					return
				}
				val, r := fn(ctx, keys[i])
				mu.Lock()
				switch r.kind {
				case outcomeAnswer:
					out[i] = member[T]{Key: keys[i], Source: MemberAuthorizer, Value: val}
					evaluated++
				case outcomeDegradable:
					iss := issueFrom(r.err)
					out[i] = member[T]{Key: keys[i], Source: MemberRequest, Issue: &iss, Value: val}
				case outcomeNoClusterAccess:
					// The subject passed the preflight and lost cluster access
					// mid-run: terminal, never a row.
					if terminal == nil {
						terminal = &fanOutFailure{Kind: r.kind, Status: r.resp.Status,
							Err: clierr.New(clierr.CodeRBAC, fmt.Sprintf("cluster access changed during the run; evaluated %d of %d", evaluated, len(keys)))}
					}
					mu.Unlock()
					cancel()
					return
				default:
					if terminal == nil {
						terminal = &fanOutFailure{Kind: r.kind, Status: r.resp.Status, Err: r.err}
					}
					mu.Unlock()
					cancel()
					return
				}
				mu.Unlock()
			}
		})
	}
feed:
	for i := range keys {
		select {
		case <-ctx.Done():
			break feed
		case next <- i:
		}
	}
	close(next)
	wg.Wait()
	// Budget or parent cancellation wins over whatever a member reported
	// while the context was already dead.
	if parent.Err() != nil {
		msg := fmt.Sprintf("cancelled; evaluated %d of %d", evaluated, len(keys))
		if errors.Is(context.Cause(parent), errFanOutBudget) {
			msg = fmt.Sprintf("fan-out budget of %s exhausted; evaluated %d of %d", fanOutBudget, evaluated, len(keys))
		}
		return nil, &fanOutFailure{Kind: outcomeTerminal, Err: clierr.New(clierr.CodeGeneric, msg)}
	}
	if terminal != nil {
		return nil, terminal
	}
	return out, nil
}

// deriveSource applies the rollup rule: no members -> scopeDiscovery; one
// source shared by all -> that source; otherwise mixed.
func deriveSource[T any](members []member[T]) ResultSource {
	if len(members) == 0 {
		return SourceScopeDiscovery
	}
	first := members[0].Source
	for _, m := range members[1:] {
		if m.Source != first {
			return SourceMixed
		}
	}
	return ResultSource(first)
}
