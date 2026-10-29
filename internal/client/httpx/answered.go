package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
)

// FactsBodyLimit is the most bytes read from the body of a request for a
// server fact; a longer body is reported as too long.
const FactsBodyLimit = 16 * 1024

// AnsweredHost is the scheme and host of a request that came back with a
// response, as the request carried them, and the transport that carried it.
type AnsweredHost struct {
	// Origin is scheme://host as the request URL had them: the case and an
	// explicit default port are kept.
	Origin string
	// Transport is the transport below the one that recorded the host.
	Transport http.RoundTripper
}

// InstallIDFunc sends the install id request and returns the value parsed
// from a 2xx body ("" when there is none or the body was too long), the
// status, and an error when no complete response came back. It holds the
// token it sends in memory only.
type InstallIDFunc func(ctx context.Context) (value string, status int, err error)

// InstallIDFetcher is the install id request a command registered, the
// scheme and host it targets, and whether the request skips the wait after a
// stored failure: a command that just stored the token the request carries
// asks again whatever failure the server's facts file holds.
type InstallIDFetcher struct {
	Origin          string
	Fetch           InstallIDFunc
	SkipFailureWait bool
}

// The two records of a run: the hosts that answered, in the order each was
// last recorded, and the install id fetcher with its skip. Requests run
// concurrently; each record has a lock.
var (
	answeredMu    sync.Mutex
	answeredHosts []AnsweredHost

	fetcherMu sync.Mutex
	fetcher   InstallIDFetcher
)

// RecordAnswered records the scheme and host of u and the transport that
// carried a request that came back with a response, whatever its status.
// The last transport recorded for a host is the one kept, and the host moves
// to the end of the order. A URL with no scheme or host records nothing.
func RecordAnswered(u *url.URL, transport http.RoundTripper) {
	if u == nil || u.Scheme == "" || u.Host == "" || transport == nil {
		return
	}
	origin := u.Scheme + "://" + u.Host
	answeredMu.Lock()
	defer answeredMu.Unlock()
	answeredHosts = slices.DeleteFunc(answeredHosts, func(h AnsweredHost) bool { return h.Origin == origin })
	answeredHosts = append(answeredHosts, AnsweredHost{Origin: origin, Transport: transport})
}

// AnsweredHosts returns the hosts that answered, in the order each was last
// recorded, the latest last.
func AnsweredHosts() []AnsweredHost {
	answeredMu.Lock()
	defer answeredMu.Unlock()
	return slices.Clone(answeredHosts)
}

// RegisterInstallIDFetcher records the install id request of a command, the
// scheme and host it targets, and whether the request skips the wait after a
// stored failure. The last registration wins. An origin with no scheme or
// host, or a nil function, records nothing.
func RegisterInstallIDFetcher(origin string, fetch InstallIDFunc, skipFailureWait bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || fetch == nil {
		return
	}
	fetcherMu.Lock()
	defer fetcherMu.Unlock()
	fetcher = InstallIDFetcher{Origin: origin, Fetch: fetch, SkipFailureWait: skipFailureWait}
}

// RegisteredInstallIDFetcher returns the fetcher last registered, and false
// when none was.
func RegisteredInstallIDFetcher() (InstallIDFetcher, bool) {
	fetcherMu.Lock()
	defer fetcherMu.Unlock()
	return fetcher, fetcher.Fetch != nil
}

// ClearRecords empties the hosts that answered and the install id fetcher,
// its skip included.
// A run clears both before it starts: nothing carries over to the next run
// of the same process.
func ClearRecords() {
	answeredMu.Lock()
	answeredHosts = nil
	answeredMu.Unlock()
	fetcherMu.Lock()
	fetcher = InstallIDFetcher{}
	fetcherMu.Unlock()
}

// VersionResponse is what GET /rancherversion got: the status, and the body
// read through the limit.
type VersionResponse struct {
	Status int
	Body   []byte
	// TooLong is true when the body was longer than FactsBodyLimit; Body is
	// then empty.
	TooLong bool
}

// RequestVersion sends GET <origin>/rancherversion through transport's
// RoundTrip directly, with the User-Agent and no credential header (no
// Authorization and no Impersonate-User). A RoundTrip follows no redirect.
// It reads the body through FactsBodyLimit and closes it. It returns an
// error when the request failed or the body could not be read.
func RequestVersion(ctx context.Context, transport http.RoundTripper, origin string) (VersionResponse, error) {
	if transport == nil {
		return VersionResponse{}, errors.New("no transport")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/rancherversion", nil)
	if err != nil {
		return VersionResponse{}, err
	}
	req.Header.Set("User-Agent", UserAgent())
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return VersionResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, tooLong, err := ReadLimited(resp.Body, FactsBodyLimit)
	if err != nil {
		return VersionResponse{}, err
	}
	return VersionResponse{Status: resp.StatusCode, Body: body, TooLong: tooLong}, nil
}

// ReadLimited reads r through limit bytes. A longer body is reported as too
// long and returns no bytes.
func ReadLimited(r io.Reader, limit int64) (body []byte, tooLong bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return nil, true, nil
	}
	return body, false, nil
}

// DoLimited is Do with the body read through limit: a longer body is
// reported as too long and returns no bytes. It closes the body.
func DoLimited(client *http.Client, req *http.Request, limit int64) (resp *http.Response, body []byte, tooLong bool, err error) {
	resp, err = client.Do(req)
	if err != nil {
		return resp, nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, tooLong, err = ReadLimited(resp.Body, limit)
	return resp, body, tooLong, err
}
