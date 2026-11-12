package fidelity

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// garbageBearer is the malformed credential IdentityGarbage sends - the same
// literal on every target so recordings are comparable.
const garbageBearer = "not-a-real-token"

// defaultStepTimeout bounds every observation and every fixture step. Tests
// inject a shorter deadline through runProbe; production callers use the
// exported RunProbe and never mutate shared state.
const defaultStepTimeout = 30 * time.Second

// waitForStatusTimeout and waitForStatusInterval pace an observation that
// sets WaitForStatus. The bound stays well inside defaultStepTimeout, so a
// wait that runs out fails with its own error, naming the status it waited
// for, not with the step deadline. Tests shorten both.
var (
	waitForStatusTimeout  = 10 * time.Second
	waitForStatusInterval = 250 * time.Millisecond
)

// untilTimeout, untilInterval and untilReadTimeout pace an observation that
// sets Until: the longest the observation waits, the pause between its
// reads, and the limit of each read. The wait runs on a context of its own,
// not the step deadline, and a setup step can end it earlier through
// waitDeadlineCapture. Tests shorten them.
var (
	untilTimeout     = 2 * time.Minute
	untilInterval    = 2 * time.Second
	untilReadTimeout = 5 * time.Second
)

// waitDeadlineCapture names the capture a setup step stores an RFC 3339 time
// under to end every Until wait of the probe at that time. A probe's waits
// then share one budget, counted from the step that stored it.
const waitDeadlineCapture = unrecordedPrefix + "waitdeadline"

// RunOptions changes how RunProbeWith executes a probe.
type RunOptions struct {
	// KeepLastAnswer records the last answer of an observation whose
	// WaitForStatus never arrived, instead of failing the probe. The result
	// carries WaitRanOut.
	KeepLastAnswer bool
}

// RunProbe executes p's setup, observations, and teardown against tgt and
// returns one normalized result per observation.
func RunProbe(ctx context.Context, tgt Target, p Probe) ([]ObservationResult, error) {
	return runProbe(ctx, tgt, p, defaultStepTimeout)
}

// RunProbeWith is RunProbe with options. A cancelled context still fails the
// probe whatever the options say. Teardown steps run on their own context and
// time limit, so the caller's context ending does not skip them.
func RunProbeWith(ctx context.Context, tgt Target, p Probe, opts RunOptions) ([]ObservationResult, error) {
	return runProbeWith(ctx, tgt, p, defaultStepTimeout, opts)
}

func runProbe(ctx context.Context, tgt Target, p Probe, timeout time.Duration) ([]ObservationResult, error) {
	return runProbeWith(ctx, tgt, p, timeout, RunOptions{})
}

// runProbeWith is RunProbeWith with an injectable per-step deadline. When the
// server's minor is newer than the probe's MaxMinor it returns an error that
// wraps ErrNotForMinor before any other request, setup or teardown. For a
// probe with OwnPairOnly, when the target names no install id of the pair
// scripts/e2e-fidelity booted or kept, or the server's install-uuid setting
// differs from it, it returns a not-checked error after at most that one
// read, before any setup or teardown. Teardown is installed BEFORE setup runs
// and always executes - after a partial setup failure, an observation
// failure, a capture failure, or success - with the captures resolved so far.
// Every teardown error is collected and joined with the primary error; none
// is dropped. Teardown steps run on their own context and time limit, so the
// caller's context ending does not skip them.
func runProbeWith(ctx context.Context, tgt Target, p Probe, timeout time.Duration, opts RunOptions) (results []ObservationResult, err error) {
	if verr := p.Validate(); verr != nil {
		return nil, verr
	}
	if merr := checkMaxMinor(ctx, tgt, p, timeout); merr != nil {
		return nil, merr
	}
	if perr := checkOwnPair(ctx, tgt, p); perr != nil {
		return nil, perr
	}
	captured := map[string]string{}
	if p.Downstream {
		id := tgt.Downstream()
		if id == "" {
			return nil, fmt.Errorf("probe %s: target has no downstream cluster", p.Name)
		}
		captured["downstream"] = id
	}
	runStep := func(s Step) error {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return s.Do(sctx, tgt, captured)
	}
	defer func() {
		err = errors.Join(err, runTeardown(ctx, tgt, p, captured, timeout))
	}()
	for _, s := range p.Setup {
		if serr := runStep(s); serr != nil {
			return nil, fmt.Errorf("probe %s: setup %s: %w", p.Name, s.Name, serr)
		}
	}

	probeRules := append(DefaultNormalizeRules(), p.Normalize...)
	for _, o := range p.Observations {
		rules := append(slices.Clone(probeRules), o.Normalize...)
		res, raw, oerr := runObservation(ctx, tgt, o, captured, timeout, opts)
		if oerr != nil {
			return results, fmt.Errorf("probe %s: observation %s: %w", p.Name, o.Name, oerr)
		}
		// res.Body and raw alias the same decoded map, so captures MUST be
		// resolved before normalization scrubs the very fields they read
		// (metadata.resourceVersion -> "<rv>", status.bearerToken ->
		// "<secret>").
		var capErr error
		capNames := make([]string, 0, len(o.Captures))
		for name := range o.Captures {
			capNames = append(capNames, name)
		}
		sort.Strings(capNames)
		for _, name := range capNames {
			v, cerr := resolveCapture(raw, o.Captures[name])
			if cerr != nil {
				// A declared capture that cannot be resolved is an execution
				// error: dependent requests must never run against a
				// synthetic URL, and no golden may be saved from this run.
				capErr = fmt.Errorf("probe %s: observation %s: capture %q: %w", p.Name, o.Name, name, cerr)
				break
			}
			captured[name] = v
		}
		// Normalize and retain the completed observation either way - on a
		// capture failure it is returned for diagnostics.
		Normalize(res.Body, rules)
		if p.SubstituteCaptures {
			substituteCaptured(res.Body, captured)
			res.Text = substituteCapturedString(res.Text, captured)
		}
		results = append(results, res)
		if capErr != nil {
			return results, capErr
		}
	}
	return results, nil
}

// runTeardown runs every step of p.Teardown, in order, and returns every step
// error joined (nil when none failed). A step that fails does not stop the
// ones after it. Each step runs on a context that keeps the values of ctx but
// neither its cancellation nor its deadline, and ends after timeout. Every
// restore and delete is sent even when the caller's context has ended.
func runTeardown(ctx context.Context, tgt Target, p Probe, captured map[string]string, timeout time.Duration) error {
	var errs []error
	for _, s := range p.Teardown {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		terr := s.Do(sctx, tgt, captured)
		cancel()
		if terr != nil {
			errs = append(errs, fmt.Errorf("probe %s: teardown %s: %w", p.Name, s.Name, terr))
		}
	}
	return errors.Join(errs...)
}

func resolveCapture(raw map[string]any, c Capture) (string, error) {
	v, err := captureValue(raw, c.Path)
	if err != nil {
		return "", err
	}
	if c.ExtBearerName {
		derived, ok := NameFromExtBearer(v)
		if !ok {
			// The captured value is a credential - its shape may be wrong,
			// but it can still be a live secret. Never embed it in the error.
			return "", fmt.Errorf("captured value is not ext-shaped (want ext/<name>:<secret>)")
		}
		return derived, nil
	}
	return v, nil
}

func runObservation(ctx context.Context, tgt Target, o Observation, captured map[string]string, timeout time.Duration, opts RunOptions) (ObservationResult, map[string]any, error) {
	var ans answer
	var err error
	if o.Until != nil {
		ans, err = readUntil(ctx, tgt, o, captured)
	} else {
		ans, err = sendOnce(ctx, tgt, o, captured, timeout, opts.KeepLastAnswer)
	}
	if err != nil {
		return ObservationResult{}, nil, err
	}

	res := ObservationResult{
		Name:        o.Name,
		Status:      ans.status,
		ContentType: ans.contentType,
		WaitRanOut:  ans.waitRanOut,
	}
	var decoded map[string]any
	if len(ans.body) > 0 {
		if json.Unmarshal(ans.body, &decoded) == nil {
			res.Body = decoded
		} else {
			res.Text = strings.TrimSpace(string(ans.body))
		}
	}
	return res, decoded, nil
}

// answer is one read response: its status, its media type without
// parameters and its body.
type answer struct {
	status      int
	contentType string
	body        []byte
	waitRanOut  bool
}

// readAnswer reads resp whole and closes its body.
func readAnswer(resp *http.Response) (answer, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer{}, fmt.Errorf("reading response: %w", err)
	}
	return answer{
		status:      resp.StatusCode,
		contentType: strings.Split(resp.Header.Get("Content-Type"), ";")[0],
		body:        raw,
	}, nil
}

// sendOnce runs an observation without Until within the step deadline.
func sendOnce(ctx context.Context, tgt Target, o Observation, captured map[string]string, timeout time.Duration, keepLast bool) (answer, error) {
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, waitRanOut, err := attemptUntilStatus(octx, tgt, o, captured, keepLast)
	if err != nil {
		return answer{}, err
	}
	ans, err := readAnswer(resp)
	ans.waitRanOut = waitRanOut
	return ans, err
}

// readUntil sends the observation every untilInterval until o.Until reports
// done and returns that answer. The wait ends at the earlier of untilTimeout
// after the first read and the time stored under waitDeadlineCapture; a
// deadline that has already passed fails before any request. Each read is
// limited to untilReadTimeout. A read that runs past its limit or gets a
// transport error is sent again, as any answer that is not done is: the
// request is a GET. A wait that ends fails the observation and names the
// last read error, if any. An error building the request or resolving its
// bearer, and an error from Until, fail it at once. A wait that ran out is
// never returned as an answer.
func readUntil(ctx context.Context, tgt Target, o Observation, captured map[string]string) (answer, error) {
	start := time.Now()
	deadline := start.Add(untilTimeout)
	if raw, ok := captured[waitDeadlineCapture]; ok {
		stored, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return answer{}, fmt.Errorf("the wait deadline in %s is not an RFC 3339 time", waitDeadlineCapture)
		}
		if !stored.After(start) {
			return answer{}, fmt.Errorf("the wait deadline in %s passed before the first read", waitDeadlineCapture)
		}
		if stored.Before(deadline) {
			deadline = stored
		}
	}
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	state, last := "without an answer", "no status"
	var lastErr error
	ranOut := func(reads int) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("still %s after %d reads (last status %s): %w", state, reads, last, err)
		}
		if lastErr != nil {
			return fmt.Errorf("still %s after %d reads in %s (last status %s; last read error: %w)", state, reads, time.Since(start).Round(time.Millisecond), last, lastErr)
		}
		return fmt.Errorf("still %s after %d reads in %s (last status %s)", state, reads, time.Since(start).Round(time.Millisecond), last)
	}
	for reads := 1; ; reads++ {
		rctx, rcancel := context.WithTimeout(wctx, untilReadTimeout)
		resp, err := attempt(rctx, tgt, o, captured)
		if _, ok := errors.AsType[*requestError](err); ok {
			rcancel()
			return answer{}, err
		}
		var ans answer
		if err == nil {
			ans, err = readAnswer(resp)
		}
		rcancel()
		if err == nil {
			lastErr = nil
			last = strconv.Itoa(ans.status)
			var decoded map[string]any
			_ = json.Unmarshal(ans.body, &decoded)
			done, seen, uerr := o.Until(decoded, ans.status, captured)
			if uerr != nil {
				return answer{}, uerr
			}
			if done {
				return ans, nil
			}
			state = seen
		} else if wctx.Err() == nil {
			// A read the wait's own end cut short is not a read error.
			lastErr = err
		}
		if wctx.Err() != nil {
			return answer{}, ranOut(reads)
		}
		select {
		case <-wctx.Done():
			return answer{}, ranOut(reads)
		case <-time.After(untilInterval):
		}
	}
}

// attemptUntilStatus sends the observation once, or, when it sets
// WaitForStatus, again every waitForStatusInterval until the answer has
// that status. A transport error ends it at once; when waitForStatusTimeout
// passes first it fails, naming the status waited for, the last status and
// the attempt count. With keepLast the last answer is returned instead, with
// waitRanOut true, unless the context ended.
func attemptUntilStatus(ctx context.Context, tgt Target, o Observation, captured map[string]string, keepLast bool) (resp *http.Response, waitRanOut bool, err error) {
	start := time.Now()
	for attempts := 1; ; attempts++ {
		resp, err := attempt(ctx, tgt, o, captured)
		if err != nil || o.WaitForStatus == 0 || resp.StatusCode == o.WaitForStatus {
			return resp, false, err
		}
		last := resp.StatusCode
		timedOut := time.Since(start) >= waitForStatusTimeout
		if timedOut && keepLast && ctx.Err() == nil {
			return resp, true, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if timedOut {
			return nil, false, fmt.Errorf("want status %d, still %d after %d attempts in %s", o.WaitForStatus, last, attempts, waitForStatusTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, false, fmt.Errorf("want status %d, last %d after %d attempts: %w", o.WaitForStatus, last, attempts, ctx.Err())
		case <-time.After(waitForStatusInterval):
		}
	}
}

// attempt builds the observation's HTTP request from its RequestSpec and sends
// it through doResilient, which supplies the connection-reset retry.
func attempt(ctx context.Context, tgt Target, o Observation, captured map[string]string) (*http.Response, error) {
	build := func() (*http.Request, error) {
		u := tgt.BaseURL() + substitute(o.Request.Path, captured)
		if len(o.Request.Query) > 0 {
			q := url.Values{}
			for k, vs := range o.Request.Query {
				for _, v := range vs {
					q.Add(k, substitute(v, captured))
				}
			}
			u += "?" + q.Encode()
		}
		var body io.Reader
		if o.Request.Body != "" {
			body = strings.NewReader(substitute(o.Request.Body, captured))
		}
		req, err := http.NewRequestWithContext(ctx, o.Request.Method, u, body)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		for k, v := range o.Request.Headers {
			req.Header.Set(k, substitute(v, captured))
		}
		if o.Request.Body != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		switch o.Request.Identity {
		case IdentityNone:
		case IdentityGarbage:
			req.Header.Set("Authorization", "Bearer "+garbageBearer)
		case IdentityClusterOwner:
			tok := captured[clusterOwnerBearer]
			if tok == "" {
				return nil, fmt.Errorf("no %s bearer: the probe's setup must create the cluster owner", IdentityClusterOwner)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		default:
			tok, berr := tgt.Bearer(ctx, o.Request.Identity)
			if berr != nil {
				return nil, fmt.Errorf("resolving %s bearer: %w", o.Request.Identity, berr)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return req, nil
	}
	return doResilient(tgt.Client(), func() (*http.Request, error) {
		req, err := build()
		if err != nil {
			return nil, &requestError{err: err}
		}
		return req, nil
	})
}

// requestError is an error building an observation's request or resolving
// its bearer, before anything is sent. Sending again cannot change it. Its
// text is the error it wraps.
type requestError struct{ err error }

func (e *requestError) Error() string { return e.err.Error() }

func (e *requestError) Unwrap() error { return e.err }

// doResilient sends the request build produces and, on a transport-level
// connection reset (errors.Is(err, syscall.ECONNRESET)), retries exactly once -
// but only for idempotent methods (GET/PUT/DELETE/HEAD). A POST is never
// retried: a reset that lands after the server has committed the mutation but
// before its response reaches the client would, on retry, create a second
// resource and orphan the first. build is invoked afresh per attempt so each
// send gets an unconsumed body reader. It is the single send path shared by
// the executor, the fixture steps, and the real-target client.
func doResilient(client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	req, err := build()
	if err != nil {
		return nil, err
	}
	method := req.Method
	resp, err := client.Do(req)
	if err == nil || !isConnReset(err) || !isIdempotent(method) {
		return resp, err
	}
	retry, err := build()
	if err != nil {
		return nil, err
	}
	return client.Do(retry)
}

// isIdempotent reports whether an HTTP method is safe to replay after a
// connection reset. POST is deliberately excluded.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead:
		return true
	default:
		return false
	}
}

// isConnReset classifies a transport error as a TCP connection reset via
// wrapped-error inspection, never message text.
func isConnReset(err error) bool {
	return err != nil && errors.Is(err, syscall.ECONNRESET)
}

func substitute(s string, captured map[string]string) string {
	for k, v := range captured {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// captureValue reads a capture path from a decoded body. Segments are
// split at dots; a segment written as [key] is one literal key that may
// itself contain dots (an annotation such as
// [authz.cluster.cattle.io/globalrolebinding]); a non-negative integer
// segment indexes an array (data.0.id).
func captureValue(body map[string]any, path string) (string, error) {
	var node any = body
	for _, seg := range captureSegments(path) {
		if arr, ok := node.([]any); ok {
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(arr) {
				return "", fmt.Errorf("capture %s: no element %q", path, seg)
			}
			node = arr[i]
			continue
		}
		m, ok := node.(map[string]any)
		if !ok {
			return "", fmt.Errorf("capture %s: not an object at %q", path, seg)
		}
		node, ok = m[seg]
		if !ok {
			return "", fmt.Errorf("capture %s: missing %q", path, seg)
		}
	}
	s, ok := node.(string)
	if !ok {
		return "", fmt.Errorf("capture %s: not a string", path)
	}
	return s, nil
}

// captureSegments splits a capture path at dots, keeping a [bracketed]
// segment whole without its brackets.
func captureSegments(path string) []string {
	var segs []string
	for len(path) > 0 {
		if path[0] == '[' {
			end := strings.IndexByte(path, ']')
			if end < 0 {
				segs = append(segs, path[1:])
				break
			}
			segs = append(segs, path[1:end])
			path = strings.TrimPrefix(path[end+1:], ".")
			continue
		}
		seg, rest, _ := strings.Cut(path, ".")
		segs = append(segs, seg)
		path = rest
	}
	return segs
}

// NameFromExtBearer extracts the token name from an ext-style bearer
// ("ext/<name>:<secret>"). Both testrancher and real Rancher create this shape,
// which lets targets revoke a token without depending on the create response
// carrying metadata.name.
func NameFromExtBearer(bearer string) (string, bool) {
	rest, ok := strings.CutPrefix(bearer, "ext/")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, ":")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// unrecordedPrefix opens the name of a capture that is only an input to
// later requests and waits: a setup step stores a value under it, a
// request or wait reads it, and substituteCaptured and
// substituteCapturedString never rewrite it into a recorded body or text.
const unrecordedPrefix = "unrecorded."

// recordedNames lists the captures whose values are rewritten into their
// placeholders in recorded output, longest value first so a value that
// contains another is replaced whole; captures named under
// unrecordedPrefix are left out.
func recordedNames(captured map[string]string) []string {
	names := slices.DeleteFunc(slices.Collect(maps.Keys(captured)), func(name string) bool {
		return strings.HasPrefix(name, unrecordedPrefix)
	})
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(len(captured[b])-len(captured[a]), strings.Compare(a, b))
	})
	return names
}

// substituteCaptured replaces every recorded capture's value with its
// "{name}" placeholder in the keys and string values of body, longest
// values first so a value that contains another is replaced whole.
func substituteCaptured(body map[string]any, captured map[string]string) {
	names := recordedNames(captured)
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, child := range t {
				out[substituteCapturedString(k, captured, names...)] = walk(child)
			}
			return out
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case string:
			return substituteCapturedString(t, captured, names...)
		default:
			return v
		}
	}
	out := walk(body).(map[string]any)
	clear(body)
	maps.Copy(body, out)
}

func substituteCapturedString(s string, captured map[string]string, order ...string) string {
	if len(order) == 0 {
		order = recordedNames(captured)
	}
	for _, name := range order {
		if v := captured[name]; v != "" {
			s = strings.ReplaceAll(s, v, "{"+name+"}")
		}
	}
	return s
}
