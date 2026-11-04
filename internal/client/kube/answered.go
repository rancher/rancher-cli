package kube

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
)

// retryAfterCap is the longest wait a 429 answer may ask client-go for
// before it repeats the request, and retryBudget is the total of those
// waits within one call. Variables so a test can shorten them
// (SetRetryLimitsForTest).
var (
	retryAfterCap = 5 * time.Second
	retryBudget   = 10 * time.Second
)

// SetRetryLimitsForTest changes the cap on one 429 wait and the total of
// those waits within one call, and returns a restore func. In non-test code
// so other packages' tests can reach it, as provider.SetRequestTimeoutForTest
// is. client-go reads whole seconds, so neither value goes below one second.
// Callers must not run in parallel with tests issuing real requests.
func SetRetryLimitsForTest(waitCap, budget time.Duration) (restore func()) {
	oldCap, oldBudget := retryAfterCap, retryBudget
	retryAfterCap, retryBudget = waitCap, budget
	return func() { retryAfterCap, retryBudget = oldCap, oldBudget }
}

// answers is what one call learned from its responses: the status of the
// response to the request in flight, or to the last request when none is, and
// the time it has agreed to wait for 429 answers. The state belongs to the
// call, never to a transport or a client, which serve many calls at once.
// Requests of one call that run at the same time must not share the status:
// each gets a record of its own (WithNewAnswers), and the records of one call
// share its budget.
type answers struct {
	mu     sync.Mutex
	last   int
	budget *budget
}

// budget is the time one call has agreed to wait for 429 answers, shared by
// every record of the call under a mutex of its own.
type budget struct {
	mu    sync.Mutex
	spent time.Duration
}

type answersKey struct{}

// WithAnswers returns ctx carrying the record of one call's responses,
// or ctx itself when it carries one already, so a call made inside another
// shares the outer call's record and budget.
func WithAnswers(ctx context.Context) context.Context {
	if answersOf(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, answersKey{}, &answers{budget: &budget{}})
}

// WithNewAnswers returns ctx carrying a record of its own, whatever record ctx
// carries: for a request that runs at the same time as others of its call,
// so that another request's answer never stands for it when it runs out of
// time, and its own answer never stands for theirs. The record shares the
// 429 budget of the record ctx carries, so the requests together wait no
// longer than the call's budget; a ctx with no record gets a budget of its
// own.
func WithNewAnswers(ctx context.Context) context.Context {
	shared := &budget{}
	if parent := answersOf(ctx); parent != nil {
		shared = parent.budget
	}
	return context.WithValue(ctx, answersKey{}, &answers{budget: shared})
}

// answersOf is the record ctx carries, nil when it has none.
func answersOf(ctx context.Context) *answers {
	a, _ := ctx.Value(answersKey{}).(*answers)
	return a
}

// LastAnsweredStatus is the HTTP status of the response to the request the
// call sent last, false when that request received none (it ran out of
// time, or failed) or ctx carries no record. A request that a 429 answer
// told to wait is still the one with the 429 answer until it sends the
// next. A deadline reads it from the context of the call it ended: the
// context's values outlive it.
func LastAnsweredStatus(ctx context.Context) (int, bool) {
	a := answersOf(ctx)
	if a == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last, a.last != 0
}

// begin forgets the answer to the request before it, so that a request that
// gets none does not read as answered with that status; a nil record keeps
// nothing.
func (a *answers) begin() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.last = 0
	a.mu.Unlock()
}

// record keeps status as the answer to the request in flight; a nil record
// keeps nothing.
func (a *answers) record(status int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.last = status
	a.mu.Unlock()
}

// grant returns the part of want that the call's budget still allows, in
// whole seconds, and charges it; zero when less than a second is left. A
// nil record keeps no budget, so only the cap holds.
func (a *answers) grant(want time.Duration) time.Duration {
	if a == nil {
		return want
	}
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	granted := min(want, retryBudget-a.budget.spent).Truncate(time.Second)
	if granted < time.Second {
		return 0
	}
	a.budget.spent += granted
	return granted
}

// answeredTransport records the host of every request that came back with a
// response, with the transport below it, and forwards the request. It also
// keeps client-go from waiting on a server that is down or limiting: a 5xx
// answer loses its Retry-After header, so client-go returns the answer
// instead of repeating the request until the call's deadline, and the header
// of a 429 answer is cut to the cap and to what is left of the call's budget.
// It sits above the certificate classifier and below client-go's bearer,
// User-Agent and impersonation round-trippers, so a request sent through the
// recorded transport carries none of them.
type answeredTransport struct {
	next http.RoundTripper
}

// WrappedRoundTripper returns the transport this one wraps; client-go walks
// wrapped transports through this method.
func (t *answeredTransport) WrappedRoundTripper() http.RoundTripper { return t.next }

func (t *answeredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	calls := answersOf(req.Context())
	calls.begin()
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	httpx.RecordAnswered(req.URL, t.next)
	calls.record(resp.StatusCode)
	switch {
	case resp.StatusCode >= http.StatusInternalServerError:
		resp.Header.Del("Retry-After")
	case resp.StatusCode == http.StatusTooManyRequests:
		limitRetryAfter(resp.Header, calls)
	}
	return resp, err
}

// limitRetryAfter rewrites the Retry-After header of a 429 answer. client-go
// repeats a request only on a header that is a whole number of seconds
// (strconv.Atoi), and it waits that long: a value that is not one is left as
// it is. A number is raised to one second and lowered to the cap, and to the
// call's budget; the header goes when less than a second of the budget is
// left, and client-go returns the 429.
func limitRetryAfter(h http.Header, calls *answers) {
	n, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil {
		return
	}
	secs := min(max(n, 1), int(retryAfterCap/time.Second))
	wait := calls.grant(time.Duration(secs) * time.Second)
	if wait == 0 {
		h.Del("Retry-After")
		return
	}
	h.Set("Retry-After", strconv.Itoa(int(wait/time.Second)))
}

func recordAnswered(next http.RoundTripper) http.RoundTripper {
	return &answeredTransport{next: next}
}
