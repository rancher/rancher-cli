package telemetrysend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// answerLimit is the most bytes read from an answer.
const answerLimit = 64 << 10

// The fixed words that say why a request got no answer.
const (
	noAnswerTimeout  = "timeout"
	noAnswerNetwork  = "network error"
	noAnswerRedirect = "refused redirect"
)

// errRedirect is the error the client returns for every redirect: a redirect
// is never followed, because a followed 301, 302 or 303 turns the POST into a
// GET with no body.
var errRedirect = errors.New("redirect refused")

// newClient builds the client of one attempt: the CLI's transport and
// User-Agent, no CA and no insecure mode, no timeout of its own (the request's
// context carries the deadline) and no redirect followed.
func newClient() (*http.Client, error) {
	client, err := httpx.New(httpx.Options{})
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return client, nil
}

// newRequest builds the POST of one request: the body is the compressed form
// of the encoder's body. The CLI sets no other header; the User-Agent comes
// from the client.
func newRequest(ctx context.Context, url, contentType string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Encoding", "gzip")
	return req, nil
}

// classify names why a request got no answer.
func classify(err error) string {
	timeout, isTimeout := errors.AsType[interface {
		error
		Timeout() bool
	}](err)
	switch {
	case errors.Is(err, errRedirect):
		return noAnswerRedirect
	case errors.Is(err, context.DeadlineExceeded), isTimeout && timeout.Timeout():
		return noAnswerTimeout
	}
	return noAnswerNetwork
}

// sendAll sends the events in requests, in order, until they are all sent,
// the time is up or an answer says keep and wait.
func (a *attempt) sendAll(ctx context.Context, events []telemetry.StoredEvent) result {
	res := result{refusedKnown: true}
	if len(events) == 0 {
		return res
	}
	a.live = &res
	client, err := newClient()
	if err != nil {
		res.failed = true
		return res
	}
	defer client.CloseIdleConnections()
	queue := cut(events)
	reqCtx, cancel := context.WithDeadline(ctx, a.in.Deadline)
	defer cancel()
	for len(queue) > 0 {
		if (res.requests == 0 && a.left() < a.in.Floor) || a.left() <= 0 {
			res.timeUp = true
			return res
		}
		if ctx.Err() != nil {
			return res
		}
		b, rest, err := fit(a.in.Encoder, queue[0])
		if err != nil {
			res.failed = true
			return res
		}
		queue = queue[1:]
		if len(rest) > 0 {
			queue = append([][]telemetry.StoredEvent{rest}, queue...)
		}
		res.requests++
		a.debug(fmt.Sprintf("debug: telemetry upload: sending %d events (contract %s) to %s", len(b.events), contractLabel(schemaVersion(b.events[0])), a.in.Origin))
		keep := a.sendOne(reqCtx, client, b, &res)
		if keep {
			res.keep = true
			return res
		}
	}
	return res
}

// sendOne sends one request, notes its answer in res and returns true when
// the answer says keep and wait. Any error from the request or the read of the
// answer is no answer, whatever the response holds.
func (a *attempt) sendOne(ctx context.Context, client *http.Client, b built, res *result) (keep bool) {
	req, err := newRequest(ctx, a.in.Origin+b.req.Path, b.req.ContentType, b.body)
	if err != nil {
		res.status, res.noAnswer, res.outcome = 0, noAnswerNetwork, telemetry.OutcomeNoAnswer
		a.debug("debug: telemetry upload: no answer: " + res.noAnswer)
		return true
	}
	resp, body, _, err := httpx.DoLimited(client, req, answerLimit)
	if err != nil {
		res.status, res.noAnswer, res.outcome = 0, classify(err), telemetry.OutcomeNoAnswer
		a.debug("debug: telemetry upload: no answer: " + res.noAnswer)
		return true
	}
	res.status, res.noAnswer = resp.StatusCode, ""
	n := len(b.events)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.sent += n
		res.accepted = true
		res.acceptedStatus = resp.StatusCode
		res.outcome = telemetry.OutcomeAccepted
		res.covered = appendIDs(res.covered, b.events)
		count, known := a.in.Encoder.RefusedCount(body)
		if known {
			res.refused += count
			a.debug(fmt.Sprintf("debug: telemetry upload: answer HTTP %d, %d refused", resp.StatusCode, count))
		} else {
			res.refusedKnown = false
			a.debug(fmt.Sprintf("debug: telemetry upload: answer HTTP %d, refused count unknown", resp.StatusCode))
		}
		return false
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusRequestEntityTooLarge,
		resp.StatusCode == http.StatusUnsupportedMediaType:
		res.dropped += n
		res.outcome = telemetry.OutcomeDeleted
		res.covered = appendIDs(res.covered, b.events)
		a.debug(fmt.Sprintf("debug: telemetry upload: answer HTTP %d, refused count unknown", resp.StatusCode))
		return false
	}
	res.outcome = telemetry.OutcomeKept
	if wait, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		res.retryAfter, res.hasRetryAfter = wait, true
	}
	a.debug(fmt.Sprintf("debug: telemetry upload: answer HTTP %d, refused count unknown", resp.StatusCode))
	return true
}

func appendIDs(ids []string, events []telemetry.StoredEvent) []string {
	for _, ev := range events {
		ids = append(ids, ev.UID)
	}
	return ids
}

// contractLabel returns the version when it is three dot-separated numbers,
// and a fixed word otherwise: a stored file may hold any text.
func contractLabel(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "unrecognized"
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return "unrecognized"
		}
	}
	return version
}

// parseRetryAfter reads a Retry-After value: a non-negative decimal number of
// seconds, or an HTTP date. A value over a day, one too large for the number
// types and a date more than a day ahead count as a day; a date in the past
// is no wait at all but is still a valid value. Anything else (a negative
// number, a fraction, text) is ignored.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.Trim(value, "0123456789") == "" {
		secs, err := strconv.ParseInt(value, 10, 64)
		if err != nil || secs > int64(maxWait/time.Second) {
			return maxWait, true
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(t.Sub(now), 0), maxWait), true
}
