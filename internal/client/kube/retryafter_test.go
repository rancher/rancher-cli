package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerWith is a transport that answers every request with status and the
// given Retry-After header, or fails it with err.
type answerWith struct {
	status     int
	retryAfter string
	err        error
}

func (a answerWith) RoundTrip(*http.Request) (*http.Response, error) {
	if a.err != nil {
		return nil, a.err
	}
	h := http.Header{}
	if a.retryAfter != "" {
		h.Set("Retry-After", a.retryAfter)
	}
	return &http.Response{StatusCode: a.status, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// send sends one request with ctx through a recorded transport that answers
// as a does, and returns the answer's Retry-After header.
func send(t *testing.T, ctx context.Context, a answerWith) (header string, present bool) {
	t.Helper()
	resetRecords(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://rancher.example/x", nil)
	require.NoError(t, err)
	resp, err := recordAnswered(a).RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	values, present := resp.Header["Retry-After"]
	if !present {
		return "", false
	}
	return values[0], true
}

// TestTheAnsweredTransportRewritesRetryAfter asserts, over a fake transport
// with no network, which Retry-After header client-go sees for each answer:
// none for any 5xx; for a 429 a whole number of seconds from one to the
// cap, or none once the call's budget is spent; a value client-go does not
// read is left as it is, and no other status is touched.
func TestTheAnsweredTransportRewritesRetryAfter(t *testing.T) {
	httpDate := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		spent      time.Duration
		want       string
		wantHeader bool
	}{
		{"429 above the cap", 429, "120", 0, "5", true},
		{"429 below the cap", 429, "2", 0, "2", true},
		{"429 at the cap", 429, "5", 0, "5", true},
		{"429 with the budget spent", 429, "1", 10 * time.Second, "", false},
		{"429 with under a second of budget", 429, "3", 9500 * time.Millisecond, "", false},
		{"429 with one whole second of budget", 429, "3", 9 * time.Second, "1", true},
		{"429 with budget for part of the wait", 429, "5", 7 * time.Second, "3", true},
		{"429 zero", 429, "0", 0, "1", true},
		{"429 negative", 429, "-3", 0, "1", true},
		{"429 with a plus sign", 429, "+5", 0, "5", true},
		{"429 with a leading zero", 429, "05", 0, "5", true},
		{"429 with a trailing space", 429, "5 ", 0, "5 ", true},
		{"429 beyond int", 429, "99999999999999999999", 0, "99999999999999999999", true},
		{"429 with a date", 429, httpDate, 0, httpDate, true},
		{"429 with text", 429, "soon", 0, "soon", true},
		{"429 with none", 429, "", 0, "", false},
		{"500 with 120", 500, "120", 0, "", false},
		{"500 with 1", 500, "1", 0, "", false},
		{"500 with 0", 500, "0", 0, "", false},
		{"500 with none", 500, "", 0, "", false},
		{"503 with 120", 503, "120", 0, "", false},
		{"503 with 1", 503, "1", 0, "", false},
		{"503 with 0", 503, "0", 0, "", false},
		{"503 with none", 503, "", 0, "", false},
		{"530 with 120", 530, "120", 0, "", false},
		{"530 with 1", 530, "1", 0, "", false},
		{"530 with 0", 530, "0", 0, "", false},
		{"530 with none", 530, "", 0, "", false},
		{"404 with 120", 404, "120", 0, "120", true},
		{"200 with 120", 200, "120", 0, "120", true},
		{"301 with 120", 301, "120", 0, "120", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), answersKey{}, &answers{budget: &budget{spent: tc.spent}})
			got, present := send(t, ctx, answerWith{status: tc.status, retryAfter: tc.retryAfter})
			assert.Equal(t, tc.wantHeader, present)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheAnsweredTransportSpendsOneBudgetPerCall asserts the waits one call
// agrees to add up to the budget and no more: with the production limits
// two waits of 5 seconds, then none; another call has its own budget; a
// request with no record of a call gets the cap only.
func TestTheAnsweredTransportSpendsOneBudgetPerCall(t *testing.T) {
	first := WithAnswers(t.Context())
	var got []string
	for range 3 {
		h, present := send(t, first, answerWith{status: 429, retryAfter: "120"})
		got = append(got, map[bool]string{true: h, false: "none"}[present])
	}
	assert.Equal(t, []string{"5", "5", "none"}, got)

	h, present := send(t, WithAnswers(t.Context()), answerWith{status: 429, retryAfter: "120"})
	assert.True(t, present)
	assert.Equal(t, "5", h)

	for range 3 {
		h, present = send(t, t.Context(), answerWith{status: 429, retryAfter: "120"})
		assert.True(t, present)
		assert.Equal(t, "5", h)
	}
}

// TestSetRetryLimitsForTestChangesTheCapAndTheBudget asserts the seam
// shortens both limits and its restore puts them back.
func TestSetRetryLimitsForTestChangesTheCapAndTheBudget(t *testing.T) {
	restore := SetRetryLimitsForTest(time.Second, time.Second)
	ctx := WithAnswers(t.Context())
	h, present := send(t, ctx, answerWith{status: 429, retryAfter: "120"})
	assert.True(t, present)
	assert.Equal(t, "1", h)
	_, present = send(t, ctx, answerWith{status: 429, retryAfter: "120"})
	assert.False(t, present, "the budget of one second is spent")
	restore()
	h, present = send(t, WithAnswers(t.Context()), answerWith{status: 429, retryAfter: "120"})
	assert.True(t, present)
	assert.Equal(t, "5", h)
}

// TestTheAnsweredTransportRecordsTheStatusOfEveryAnswer asserts a call's
// record holds the status of the last response of any code, none for a
// call with no response, and none for a failed request; a nested call
// shares the record of the call that holds it, and another call does not.
func TestTheAnsweredTransportRecordsTheStatusOfEveryAnswer(t *testing.T) {
	ctx := WithAnswers(t.Context())
	_, ok := LastAnsweredStatus(ctx)
	assert.False(t, ok)

	for _, status := range []int{404, 200, 503, 429} {
		send(t, ctx, answerWith{status: status})
		got, ok := LastAnsweredStatus(ctx)
		require.True(t, ok)
		assert.Equal(t, status, got)
	}

	assert.Same(t, answersOf(ctx), answersOf(WithAnswers(ctx)), "a nested call shares the record")
	other := WithAnswers(t.Context())
	_, ok = LastAnsweredStatus(other)
	assert.False(t, ok, "another call has its own record")

	req, err := http.NewRequestWithContext(other, http.MethodGet, "https://rancher.example/x", nil)
	require.NoError(t, err)
	_, err = recordAnswered(answerWith{err: errors.New("refused")}).RoundTrip(req)
	require.Error(t, err)
	_, ok = LastAnsweredStatus(other)
	assert.False(t, ok, "a request that failed records nothing")

	_, ok = LastAnsweredStatus(t.Context())
	assert.False(t, ok, "a context with no record has no status")
	send(t, t.Context(), answerWith{status: 503})
	_, ok = LastAnsweredStatus(t.Context())
	assert.False(t, ok)
}

// TestARequestThatGetsNoAnswerForgetsTheAnswerOfTheRequestBefore asserts a
// call's record holds the answer to the request in flight: a 404 followed by a
// request that fails, and a 429 followed by one that fails, leave none, and
// a later answer replaces an earlier one.
func TestARequestThatGetsNoAnswerForgetsTheAnswerOfTheRequestBefore(t *testing.T) {
	for _, status := range []int{404, 429, 503} {
		ctx := WithAnswers(t.Context())
		send(t, ctx, answerWith{status: status})
		got, ok := LastAnsweredStatus(ctx)
		require.True(t, ok)
		assert.Equal(t, status, got)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://rancher.example/x", nil)
		require.NoError(t, err)
		_, err = recordAnswered(answerWith{err: errors.New("hung")}).RoundTrip(req)
		require.Error(t, err)
		_, ok = LastAnsweredStatus(ctx)
		assert.False(t, ok, "the request after a %d got no answer", status)
	}
}

// TestRequestsThatRunAtTheSameTimeKeepAnswersOfTheirOwn asserts a request sent
// with a record of its own (WithNewAnswers) neither reads nor changes the
// record of the call it belongs to: an answer to it does not stand for the
// call's request that ran out of time.
func TestRequestsThatRunAtTheSameTimeKeepAnswersOfTheirOwn(t *testing.T) {
	call := WithAnswers(t.Context())
	send(t, call, answerWith{status: 200})
	own := WithNewAnswers(call)
	assert.NotSame(t, answersOf(call), answersOf(own))
	send(t, own, answerWith{status: 503})

	got, ok := LastAnsweredStatus(call)
	require.True(t, ok)
	assert.Equal(t, 200, got, "the call's record keeps the answer of its own request")
	got, ok = LastAnsweredStatus(own)
	require.True(t, ok)
	assert.Equal(t, 503, got)
}

// TestRecordsOfOneCallShareItsBudget asserts the requests of one call that
// each have a record of their own (WithNewAnswers) wait no longer together
// than the call's budget: with the production limits, 20 answers of 429 with
// a Retry-After header of 120 seconds are granted two waits of 5 seconds,
// the rest none. The records of another call have a budget of their own,
// and so does a record made where no call has one.
func TestRecordsOfOneCallShareItsBudget(t *testing.T) {
	granted := func(ctx context.Context) (n int) {
		for range 20 {
			if _, present := send(t, WithNewAnswers(ctx), answerWith{status: 429, retryAfter: "120"}); present {
				n++
			}
		}
		return n
	}
	call := WithAnswers(t.Context())
	assert.Equal(t, 2, granted(call))
	assert.Equal(t, 0, granted(call), "the budget of the call is spent")
	assert.Equal(t, 2, granted(WithAnswers(t.Context())), "another call has its own")
	assert.Equal(t, 20, granted(t.Context()), "no call: a budget for each record")
}

// TestRecordsOfOneCallShareItsBudgetWhenRequestsRunAtTheSameTime asserts the
// budget of a call is the same under requests that run at the same time: 20
// requests, each with a record of its own made from one call's, all answered
// 429 with a Retry-After header of 120 seconds and sent together, are granted
// exactly two waits. Run with -race: the budget's mutex is what keeps the
// requests' updates of the time spent from racing.
func TestRecordsOfOneCallShareItsBudgetWhenRequestsRunAtTheSameTime(t *testing.T) {
	call := WithAnswers(t.Context())
	start := make(chan struct{})
	var granted, failed atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		req, err := http.NewRequestWithContext(WithNewAnswers(call), http.MethodGet, "https://rancher.example/x", nil)
		require.NoError(t, err)
		wg.Go(func() {
			<-start
			resp, err := recordAnswered(answerWith{status: 429, retryAfter: "120"}).RoundTrip(req)
			if err != nil {
				failed.Add(1)
				return
			}
			_ = resp.Body.Close()
			if resp.Header.Get("Retry-After") != "" {
				granted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	assert.Zero(t, failed.Load())
	assert.Equal(t, int32(2), granted.Load())
}
