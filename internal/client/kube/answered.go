package kube

import (
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/httpx"
)

// answeredTransport records the host of every request that came back with a
// response, with the transport below it, and forwards the request. It sits
// above the certificate classifier and below client-go's bearer, User-Agent
// and impersonation round-trippers, so a request sent through the recorded
// transport carries none of them.
type answeredTransport struct {
	next http.RoundTripper
}

// WrappedRoundTripper returns the transport this one wraps; client-go walks
// wrapped transports through this method.
func (t *answeredTransport) WrappedRoundTripper() http.RoundTripper { return t.next }

func (t *answeredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil {
		httpx.RecordAnswered(req.URL, t.next)
	}
	return resp, err
}

func recordAnswered(next http.RoundTripper) http.RoundTripper {
	return &answeredTransport{next: next}
}
