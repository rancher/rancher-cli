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

// RunProbe executes p's setup, observations, and teardown against tgt and
// returns one normalized result per observation.
func RunProbe(ctx context.Context, tgt Target, p Probe) ([]ObservationResult, error) {
	return runProbe(ctx, tgt, p, defaultStepTimeout)
}

// runProbe is RunProbe with an injectable per-step deadline. Teardown is
// installed BEFORE setup runs and always executes - after a partial setup
// failure, an observation failure, a capture failure, or success - with the
// captures resolved so far. Every teardown error is collected and joined
// with the primary error; none is dropped.
func runProbe(ctx context.Context, tgt Target, p Probe, timeout time.Duration) (results []ObservationResult, err error) {
	if verr := p.Validate(); verr != nil {
		return nil, verr
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
		errs := []error{err}
		for _, s := range p.Teardown {
			if terr := runStep(s); terr != nil {
				errs = append(errs, fmt.Errorf("probe %s: teardown %s: %w", p.Name, s.Name, terr))
			}
		}
		err = errors.Join(errs...)
	}()
	for _, s := range p.Setup {
		if serr := runStep(s); serr != nil {
			return nil, fmt.Errorf("probe %s: setup %s: %w", p.Name, s.Name, serr)
		}
	}

	rules := append(DefaultNormalizeRules(), p.Normalize...)
	for _, o := range p.Observations {
		res, raw, oerr := runObservation(ctx, tgt, o, captured, timeout)
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

func runObservation(ctx context.Context, tgt Target, o Observation, captured map[string]string, timeout time.Duration) (ObservationResult, map[string]any, error) {
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := attempt(octx, tgt, o, captured)
	if err != nil {
		return ObservationResult{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ObservationResult{}, nil, fmt.Errorf("reading response: %w", err)
	}

	res := ObservationResult{
		Name:        o.Name,
		Status:      resp.StatusCode,
		ContentType: strings.Split(resp.Header.Get("Content-Type"), ";")[0],
	}
	var decoded map[string]any
	if len(rawBody) > 0 {
		if json.Unmarshal(rawBody, &decoded) == nil {
			res.Body = decoded
		} else {
			res.Text = strings.TrimSpace(string(rawBody))
		}
	}
	return res, decoded, nil
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
	return doResilient(tgt.Client(), build)
}

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

// substituteCaptured replaces every captured value with its "{name}"
// placeholder in the keys and string values of body, longest values first
// so a value that contains another is replaced whole.
func substituteCaptured(body map[string]any, captured map[string]string) {
	names := slices.SortedFunc(maps.Keys(captured), func(a, b string) int {
		return cmp.Or(len(captured[b])-len(captured[a]), strings.Compare(a, b))
	})
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
		order = slices.SortedFunc(maps.Keys(captured), func(a, b string) int {
			return cmp.Or(len(captured[b])-len(captured[a]), strings.Compare(a, b))
		})
	}
	for _, name := range order {
		if v := captured[name]; v != "" {
			s = strings.ReplaceAll(s, v, "{"+name+"}")
		}
	}
	return s
}
