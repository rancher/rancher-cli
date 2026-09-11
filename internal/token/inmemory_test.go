package token

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// Logical origins the in-memory fixtures answer for. Every in-memory
// server reports http://example.com as its URL, so fixtures never store
// srv.URL; a request's host selects its server through hostTransport.
const (
	rancherOrigin = "http://rancher.example"
	directOrigin  = "https://direct.example"
	idpOrigin     = "https://idp.example"
)

// hostTransport dispatches each request by its host to the transport of
// the in-memory server that answers for it. A host with no entry is an
// error: nothing inside a bubble may reach the real network.
type hostTransport map[string]http.RoundTripper

func (m hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt, ok := m[r.URL.Host]
	if !ok {
		return nil, &unmappedHostError{host: r.URL.Host}
	}
	return rt.RoundTrip(r)
}

type unmappedHostError struct{ host string }

func (e *unmappedHostError) Error() string { return "no in-memory server answers for host " + e.host }

// client is an http.Client over the transport. It carries no client
// timeout: every wait under test is bounded by a context deadline, and a
// client timeout spawns a cancel goroutine per request that lives until
// the response body is closed - the OAuth library's device-authorization
// request never closes its body, so that goroutine would outlive the
// bubble and be reported as a deadlock.
func (m hostTransport) client() *http.Client {
	return &http.Client{Transport: m}
}

// directClient is a directClientFactory that ignores the rest.Config and
// returns the transport's client: the in-memory direct endpoint is reached
// by host, not by the CA the config carries.
func (m hostTransport) directClient() directClientFactory {
	return func(*rest.Config) (*http.Client, error) { return m.client(), nil }
}

// hostOf strips the scheme from an origin for use as a hostTransport key.
func hostOf(origin string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(origin) > len(p) && origin[:len(p)] == p {
			return origin[len(p):]
		}
	}
	return origin
}

// TestHostTransportDispatchesByHostAndFailsClosed asserts a request reaches
// only the in-memory server mapped for its host, https included, and a
// host with no mapping is an error rather than a real-network dial.
func TestHostTransportDispatchesByHostAndFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var rancherHits, directHits int
		rancher := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rancherHits++ }))
		direct := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { directHits++ }))
		m := hostTransport{hostOf(rancherOrigin): rancher.Client().Transport, hostOf(directOrigin): direct.Client().Transport}
		c := m.client()
		for _, u := range []string{rancherOrigin + "/v3", directOrigin + "/apis"} {
			resp, err := c.Get(u)
			require.NoError(t, err, u)
			_ = resp.Body.Close()
		}
		assert.Equal(t, 1, rancherHits)
		assert.Equal(t, 1, directHits)
		_, err := c.Get("https://nowhere.example/")
		var unmapped *unmappedHostError
		require.ErrorAs(t, err, &unmapped)
		assert.Equal(t, "nowhere.example", unmapped.host)
		assert.Equal(t, 1, rancherHits, "an unmapped host reaches no server")
		assert.Equal(t, 1, directHits)
	})
}

// TestWaitDirectReadyProbesThroughTheInjectedClient asserts the probe uses
// the client the factory returns and nothing else: on the fake clock a
// ready endpoint settles after directSettle with the real interval, and an
// unmapped host never reaches a server and ends at the bound.
func TestWaitDirectReadyProbesThroughTheInjectedClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var probes int
		direct := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probes++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-1"}}}`)
		}))
		m := hostTransport{hostOf(directOrigin): direct.Client().Transport}
		ec := &execCluster{server: directOrigin, cfg: &rest.Config{Host: directOrigin}}
		start := time.Now()
		ready, err := waitDirectReady(t.Context(), ec, "tok", directWaitBound, m.directClient())
		require.NoError(t, err)
		assert.True(t, ready)
		assert.Equal(t, directSettle, time.Since(start), "the first ready answer starts the settling interval; readiness is declared at its end on the fake clock")
		assert.Equal(t, 1+int(directSettle/directProbeInterval), probes)

		before := probes
		unmapped := &execCluster{server: "https://elsewhere.example", cfg: &rest.Config{Host: "https://elsewhere.example"}}
		start = time.Now()
		ready, err = waitDirectReady(t.Context(), unmapped, "tok", directWaitBound, m.directClient())
		require.NoError(t, err)
		assert.False(t, ready, "a host no server answers for never becomes ready")
		assert.Equal(t, directWaitBound, time.Since(start), "the bound expires on the fake clock")
		assert.Equal(t, before, probes, "the mapped server saw nothing")
	})
}
