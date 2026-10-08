package token

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// directEndpoint is a fake Authorized Cluster Endpoint: it answers the
// SelfSubjectReview 401 for the first unauthorized probes, then per mode.
type directEndpoint struct {
	srv          *httptest.Server
	inMemory     bool         // served on the in-memory network for synctest bubbles
	ca           []byte       // the stand-in CA of the in-memory variant, generated once
	unauthorized int32        // answers 401 while > 0, decrementing
	mode         atomic.Value // "review", "forbidden-status", "forbidden-plain", "empty-review", "notfound", "throttled", "unavailable", "hang"
	probes       atomic.Int32
	release      chan struct{}
}

// newDirectEndpoint serves the endpoint on a loopback TLS listener, for
// tests that run outside a synctest bubble.
func newDirectEndpoint(t *testing.T, unauthorized int, mode string) *directEndpoint {
	t.Helper()
	d := &directEndpoint{unauthorized: int32(unauthorized), release: make(chan struct{})}
	d.mode.Store(mode)
	d.srv = httptest.NewTLSServer(http.HandlerFunc(d.handle))
	t.Cleanup(func() { close(d.release); d.srv.Close() })
	return d
}

// newInMemoryDirectEndpoint serves the same endpoint on the in-memory
// network for tests inside a synctest bubble; it answers for directOrigin
// through the transport returned by transport().
func newInMemoryDirectEndpoint(t *testing.T, unauthorized int, mode string) *directEndpoint {
	t.Helper()
	d := &directEndpoint{unauthorized: int32(unauthorized), release: make(chan struct{}), inMemory: true}
	d.mode.Store(mode)
	d.ca = pemEncode(distinctLoopbackCert(t).Certificate[0])
	d.srv = httptest.NewTestServer(t, http.HandlerFunc(d.handle))
	t.Cleanup(func() { close(d.release) })
	return d
}

// transport dispatches directOrigin to this in-memory endpoint.
func (d *directEndpoint) transport() hostTransport {
	return hostTransport{hostOf(directOrigin): d.srv.Client().Transport}
}

// url is the origin the endpoint answers for: the loopback URL, or
// directOrigin for the in-memory variant.
func (d *directEndpoint) url() string {
	if d.inMemory {
		return directOrigin
	}
	return d.srv.URL
}

// caPEM is the CA the exec cluster entry carries for this endpoint. The
// in-memory variant has no TLS listener of its own, so a self-signed
// certificate generated once at construction stands in: the injected
// client never consults it, and the fingerprint only needs stable bytes.
func (d *directEndpoint) caPEM(t *testing.T) []byte {
	t.Helper()
	if d.inMemory {
		return d.ca
	}
	return pemEncode(d.srv.Certificate().Raw)
}

func (d *directEndpoint) handle(w http.ResponseWriter, r *http.Request) {
	{
		if r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		d.probes.Add(1)
		if atomic.AddInt32(&d.unauthorized, -1) >= 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch d.mode.Load().(string) {
		case "review":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-1","groups":["system:authenticated"]}}}`))
		case "empty-review":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{}}`))
		case "forbidden-status":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`))
		case "forbidden-plain":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`access denied by the load balancer`))
		case "notfound":
			w.WriteHeader(http.StatusNotFound)
		case "throttled":
			w.WriteHeader(http.StatusTooManyRequests)
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "hang":
			select {
			case <-d.release:
			case <-r.Context().Done():
			}
		}
	}
}

// cluster builds the execCluster the plugin would load for this endpoint.
func (d *directEndpoint) cluster(t *testing.T) *execCluster {
	t.Helper()
	pemCA := d.caPEM(t)
	return &execCluster{server: d.url(), cfg: &rest.Config{Host: d.url(), CAData: pemCA}, fingerprint: endpointFingerprint(d.url(), pemCA, "", false, "")}
}

// execInfo is the KUBERNETES_EXEC_INFO kubectl sets for this endpoint.
func (d *directEndpoint) execInfo(t *testing.T) string {
	t.Helper()
	return execInfoFor(t, d.url(), d.caPEM(t))
}

func execInfoFor(t *testing.T, server string, caPEM []byte) string {
	t.Helper()
	cluster := map[string]any{"server": server}
	if caPEM != nil {
		cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString(caPEM)
	}
	b, err := json.Marshal(map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential",
		"spec": map[string]any{"interactive": false, "cluster": cluster},
	})
	require.NoError(t, err)
	return string(b)
}

// TestIsRancherProxyIsExact asserts the proxy is identified by exact
// canonical comparison: case of scheme and host, one trailing slash, the
// escaped cluster id; never a prefix, a longer path or another cluster.
func TestIsRancherProxyIsExact(t *testing.T) {
	t.Parallel()
	base := "https://rancher.example.com"
	assert.True(t, isRancherProxy("https://rancher.example.com/k8s/clusters/c-1", base, "c-1"))
	assert.True(t, isRancherProxy("HTTPS://Rancher.Example.com/k8s/clusters/c-1/", base+"/", "c-1"))
	assert.False(t, isRancherProxy("https://rancher.example.com/k8s/clusters/c-1/api", base, "c-1"), "a longer path")
	assert.False(t, isRancherProxy("https://rancher.example.com/k8s/clusters/c-10", base, "c-1"), "a near-prefix cluster id")
	assert.False(t, isRancherProxy("https://rancher.example.com/k8s/clusters/c-2", base, "c-1"), "another cluster")
	assert.False(t, isRancherProxy("https://rancher.example.com.evil.test/k8s/clusters/c-1", base, "c-1"), "another host")
	assert.False(t, isRancherProxy("https://ace.example.test/k8s/clusters/local", base, "c-1"), "a path-bearing fqdn")
	assert.False(t, isRancherProxy("https://ace.example.test", base, "c-1"))
	for _, extra := range []string{
		"https://u:p@rancher.example.com/k8s/clusters/c-1",
		"https://rancher.example.com/k8s/clusters/c-1?x=1",
		"https://rancher.example.com/k8s/clusters/c-1?",
		"https://rancher.example.com/k8s/clusters/c-1#f",
	} {
		assert.False(t, isRancherProxy(extra, base, "c-1"), extra)
	}
}

// TestEndpointFingerprintFields asserts every routing and trust field
// changes the fingerprint and that its inputs are length-delimited.
func TestEndpointFingerprintFields(t *testing.T) {
	t.Parallel()
	ca := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	base := endpointFingerprint("https://a:6443", ca, "", false, "")
	assert.NotEqual(t, base, endpointFingerprint("https://b:6443", ca, "", false, ""), "server")
	assert.NotEqual(t, base, endpointFingerprint("https://a:6443", nil, "", false, ""), "ca")
	assert.NotEqual(t, base, endpointFingerprint("https://a:6443", ca, "sni.example", false, ""), "tls server name")
	assert.NotEqual(t, base, endpointFingerprint("https://a:6443", ca, "", true, ""), "insecure")
	assert.NotEqual(t, base, endpointFingerprint("https://a:6443", ca, "", false, "http://proxy-a:3128"), "proxy")
	assert.NotEqual(t, endpointFingerprint("https://a:6443", ca, "", false, "http://proxy-a:3128"), endpointFingerprint("https://a:6443", ca, "", false, "http://proxy-b:3128"), "two proxies")
	assert.Equal(t, base, endpointFingerprint("https://a:6443", ca, "", false, ""), "deterministic")
	assert.NotEqual(t, endpointFingerprint("ab", []byte("c"), "", false, ""), endpointFingerprint("a", []byte("bc"), "", false, ""), "length-delimited")
}

// TestLoadExecClusterShapes asserts the loader: no variable and no cluster
// information are nil; a cluster entry yields its server and fingerprint;
// disable-compression does not change the fingerprint; garbage is an error.
func TestLoadExecClusterShapes(t *testing.T) {
	t.Setenv(execInfoEnv, "")
	ec, err := loadExecCluster()
	require.NoError(t, err)
	assert.Nil(t, ec)

	t.Setenv(execInfoEnv, `{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","spec":{"interactive":false}}`)
	ec, err = loadExecCluster()
	require.NoError(t, err)
	assert.Nil(t, ec, "no cluster information: a kubeconfig without provideClusterInfo")

	ca := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	t.Setenv(execInfoEnv, execInfoFor(t, "https://ace.example.test:6443", ca))
	ec, err = loadExecCluster()
	require.NoError(t, err)
	require.NotNil(t, ec)
	assert.Equal(t, "https://ace.example.test:6443", ec.server)
	assert.Equal(t, "https://ace.example.test:6443", ec.cfg.Host)
	assert.Equal(t, endpointFingerprint("https://ace.example.test:6443", ca, "", false, ""), ec.fingerprint)

	var withCompression map[string]any
	require.NoError(t, json.Unmarshal([]byte(execInfoFor(t, "https://ace.example.test:6443", ca)), &withCompression))
	withCompression["spec"].(map[string]any)["cluster"].(map[string]any)["disable-compression"] = true
	b, err := json.Marshal(withCompression)
	require.NoError(t, err)
	t.Setenv(execInfoEnv, string(b))
	ec2, err := loadExecCluster()
	require.NoError(t, err)
	assert.Equal(t, ec.fingerprint, ec2.fingerprint, "disable-compression is not a routing or trust field")

	t.Setenv(execInfoEnv, "{not json")
	_, err = loadExecCluster()
	require.Error(t, err)
}

// TestProbeDirectClassifies asserts each answer class: 401 is not yet; a
// review with a username and a Kubernetes Forbidden Status are ready;
// an empty review, a plain 403, 404, 429 and 503 prove nothing.
func TestProbeDirectClassifies(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]probeAnswer{
		"review": probeReady, "forbidden-status": probeReady,
		"empty-review": probeRetry, "forbidden-plain": probeRetry, "notfound": probeRetry, "throttled": probeRetry, "unavailable": probeRetry,
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			d := newDirectEndpoint(t, 0, mode)
			assert.Equal(t, want, probeDirect(t.Context(), d.srv.Client(), d.srv.URL, "tok"))
		})
	}
	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		d := newDirectEndpoint(t, 1, "review")
		assert.Equal(t, probeUnauthorized, probeDirect(t.Context(), d.srv.Client(), d.srv.URL, "tok"))
	})
	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		d := newDirectEndpoint(t, 0, "review")
		assert.Equal(t, probeRetry, probeDirect(t.Context(), http.DefaultClient, d.srv.URL, "tok"), "an untrusted certificate is a retry, never readiness")
	})
}

// TestWaitDirectReadySettles asserts the wait on the fake clock: 401s then
// a ready answer followed by the settling interval without a 401 is
// ready; a bound expiry is (false, nil); a caller cancellation is the
// caller's error; an endpoint that accepts and never answers ends at the
// bound; a CA that does not parse is unavailable. Every subtest is its own
// bubble with an in-memory endpoint, so the production intervals run
// unshortened and the elapsed times are exact.
func TestWaitDirectReadySettles(t *testing.T) {
	t.Run("ready after 401s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 3, "review")
			start := time.Now()
			ready, err := waitDirectReady(t.Context(), d.cluster(t), "tok", directWaitBound, d.transport().directClient())
			require.NoError(t, err)
			assert.True(t, ready)
			assert.Equal(t, 3*directProbeInterval+directSettle, time.Since(start), "three 401s one interval apart, then the settling interval from the first ready answer")
			assert.Equal(t, int32(3+1+int(directSettle/directProbeInterval)), d.probes.Load(), "three 401s, one ready, then one probe per interval until the interval elapsed")
		})
	})
	t.Run("forbidden status is ready", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 1, "forbidden-status")
			ready, err := waitDirectReady(t.Context(), d.cluster(t), "tok", directWaitBound, d.transport().directClient())
			require.NoError(t, err)
			assert.True(t, ready)
		})
	})
	t.Run("never ready expires the bound", func(t *testing.T) {
		for _, mode := range []string{"notfound", "throttled", "unavailable", "forbidden-plain", "empty-review"} {
			synctest.Test(t, func(t *testing.T) {
				d := newInMemoryDirectEndpoint(t, 0, mode)
				start := time.Now()
				ready, err := waitDirectReady(t.Context(), d.cluster(t), "tok", directWaitBound, d.transport().directClient())
				require.NoError(t, err, mode)
				assert.False(t, ready, mode)
				assert.Equal(t, directWaitBound, time.Since(start), mode)
			})
		}
	})
	t.Run("hang ends at the bound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 0, "hang")
			start := time.Now()
			ready, err := waitDirectReady(t.Context(), d.cluster(t), "tok", directWaitBound, d.transport().directClient())
			require.NoError(t, err)
			assert.False(t, ready)
			assert.Equal(t, directWaitBound, time.Since(start), "a stalled request cannot outlive the bound")
		})
	})
	t.Run("cancellation is the caller's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 1000, "review")
			ctx, cancel := context.WithCancel(t.Context())
			// Cancel between two ticks, never on one, so the probe count
			// does not depend on which of two timers due at the same instant
			// fires first.
			go func() { time.Sleep(5 * directProbeInterval / 2); cancel() }()
			ready, err := waitDirectReady(ctx, d.cluster(t), "tok", directWaitBound, d.transport().directClient())
			require.ErrorIs(t, err, context.Canceled)
			assert.False(t, ready)
			assert.Equal(t, int32(3), d.probes.Load(), "the probe at the start and one per interval until the cancellation")
		})
	})
	t.Run("unparsable ca is unavailable", func(t *testing.T) {
		ec := &execCluster{server: "https://ace.example.test", cfg: &rest.Config{Host: "https://ace.example.test", CAData: []byte("not a pem")}}
		_, err := waitDirectReady(t.Context(), ec, "tok", directWaitBound, kube.HTTPClient)
		var unavailable *directProbeUnavailable
		require.ErrorAs(t, err, &unavailable)
	})
	t.Run("cancellation wins over an unparsable ca", func(t *testing.T) {
		ec := &execCluster{server: "https://ace.example.test", cfg: &rest.Config{Host: "https://ace.example.test", CAData: []byte("not a pem")}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := waitDirectReady(ctx, ec, "tok", directWaitBound, kube.HTTPClient)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// pemEncode wraps a DER certificate as PEM.
func pemEncode(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestGetTokenWaitsForTheDirectEndpoint asserts the plugin end to end with
// the interactive cluster-scoped login as the producer: with the invoking
// context's cluster entry in KUBERNETES_EXEC_INFO naming a direct endpoint
// that answers 401 twice, the credential is returned only after the
// endpoint authenticates it and settles, and the cache record carries the
// endpoint's fingerprint; a second call is a cache hit with no probe.
func TestGetTokenWaitsForTheDirectEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newInMemoryDirectEndpoint(t, 2, "review")
		t.Setenv(execInfoEnv, d.execInfo(t))
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		x.route(d)
		res, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		cred := res.Data.(*execCredential)
		require.NotEmpty(t, cred.Status.Token)
		assert.Equal(t, int32(2+1+int(directSettle/directProbeInterval)), d.probes.Load(), "two 401s, a ready answer, and one probe per interval through the settling interval")
		assert.Empty(t, res.Warnings)

		key := config.CacheKey("kc", credhelperCanonical(t, x.url), "local")
		detail, err := x.store.GetCachedCredDetail(key, anchorFor(x.dir))
		require.NoError(t, err)
		assert.Equal(t, cred.Status.Token, detail.Token)
		assert.Equal(t, []string{d.cluster(t).fingerprint}, detail.ReadyEndpoints)

		before := d.probes.Load()
		res2, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, cred.Status.Token, res2.Data.(*execCredential).Status.Token, "a cache hit")
		assert.Equal(t, before, d.probes.Load(), "a ready endpoint is not probed again")
	})
}

// TestGetTokenTimeoutWarnsAndReprobesOnHit asserts a bound expiry: the
// credential is returned with the one warning naming the endpoint and the
// proxy context, no fingerprint is cached, and the next call re-probes the
// cached token (a shorter bound) and marks it once the endpoint answers,
// creating no token.
func TestGetTokenTimeoutWarnsAndReprobesOnHit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newInMemoryDirectEndpoint(t, 1000, "review")
		t.Setenv(execInfoEnv, d.execInfo(t))
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		x.route(d)
		start := time.Now()
		res, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, directWaitBound, time.Since(start), "the whole bound elapsed on the fake clock before the warning")
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "may not yet be accepted at "+d.url())
		assert.Contains(t, res.Warnings[0], "the proxy context kc is the immediate fallback")
		key := config.CacheKey("kc", credhelperCanonical(t, x.url), "local")
		detail, err := x.store.GetCachedCredDetail(key, anchorFor(x.dir))
		require.NoError(t, err)
		assert.Empty(t, detail.ReadyEndpoints)
		creates := x.f.TokenCount()

		atomic.StoreInt32(&d.unauthorized, 0)
		res2, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, detail.Token, res2.Data.(*execCredential).Status.Token, "the cached token, re-probed")
		assert.Empty(t, res2.Warnings)
		assert.Equal(t, creates, x.f.TokenCount(), "no replacement token")
		detail, err = x.store.GetCachedCredDetail(key, anchorFor(x.dir))
		require.NoError(t, err)
		assert.Equal(t, []string{d.cluster(t).fingerprint}, detail.ReadyEndpoints)
	})
}

// TestGetTokenProxyContextNeverWaits asserts an exec info naming the
// Rancher proxy exactly makes no probe, while a near-prefix URL does.
func TestGetTokenProxyContextNeverWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		d := newInMemoryDirectEndpoint(t, 0, "review")
		x.route(d)
		t.Setenv(execInfoEnv, execInfoFor(t, x.url+"/k8s/clusters/local", nil))
		_, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, int32(0), d.probes.Load())

		t.Setenv(execInfoEnv, "")
		x2 := newInMemoryPluginFixture(t, false, testrancher.Options{})
		x2.route(d)
		t.Setenv(execInfoEnv, d.execInfo(t))
		_, err = x2.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Greater(t, d.probes.Load(), int32(0), "a direct endpoint is probed")
	})
}

// TestGetTokenCancelledDuringWaitRevokesTheToken asserts the cleanup: a
// caller cancellation during the wait revokes the token this invocation
// created, with the token itself as bearer for the interactive login, and
// returns the cancellation with the token's fate as details, no credential.
func TestGetTokenCancelledDuringWaitRevokesTheToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newInMemoryDirectEndpoint(t, 1000, "review")
		t.Setenv(execInfoEnv, d.execInfo(t))
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		x.route(d)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			for d.probes.Load() < 2 {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		_, err := x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "cancelled while waiting for the direct endpoint")
		require.ErrorContains(t, err, "was revoked")
		deletes := x.f.DeleteRequests()
		require.Len(t, deletes, 1)
		creates := x.f.CreateRequests()
		require.NotEmpty(t, creates)
		assert.Equal(t, deletes[0].Name, creates[len(creates)-1].Name, "the token this invocation created")
		assert.Contains(t, deletes[0].Bearer, deletes[0].Name, "the interactive login's token revokes itself")
		_, cached, cerr := x.store.GetCachedCred(config.CacheKey("kc", credhelperCanonical(t, x.url), "local"), anchorFor(x.dir))
		assert.ErrorIs(t, cerr, config.ErrNoToken, "nothing cached", cached)
	})
}

func credhelperCanonical(t *testing.T, server string) string {
	t.Helper()
	id, err := credhelper.CanonicalServerURL(server)
	require.NoError(t, err)
	return id
}

// TestGetTokenCancelledDuringWaitRevokesEveryProducersChild asserts the
// cleanup capability per fresh-token producer. Stored credentials of every
// provenance (a user entry, userSupplied, derived) go through
// storedCredToken; a cache miss with token reuse on and no stored entry
// goes through pluginLogin (the facade counts its interactive login and
// the exec-login entry appears). In every case the child token this
// invocation created is deleted with the parent bearer, never the parent.
func TestGetTokenCancelledDuringWaitRevokesEveryProducersChild(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance clierr.Provenance
		stored     bool
		reuse      bool
	}{
		{"stored user entry", "", true, false},
		{"stored user-supplied entry", clierr.ProvenanceUserSupplied, true, false},
		{"stored derived entry", clierr.ProvenanceDerived, true, false},
		{"plugin-created login", clierr.ProvenanceExecLogin, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newInMemoryDirectEndpoint(t, 1000, "review")
				t.Setenv(execInfoEnv, "")
				x := newInMemoryPluginFixture(t, tc.reuse, testrancher.Options{})
				x.route(d)
				var parentBearer string
				if tc.stored {
					parent, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0,
						ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
					require.NoError(t, err)
					seedEntry(t, x.dir, x.store, "stored", config.Server{URL: x.url, Provenance: tc.provenance, TokenRef: parent.Ref, TokenUserID: parent.UserID}, parent.Token, tc.reuse)
					parentBearer = parent.Token
				}
				loginsBefore := x.f.LoginCount()

				t.Setenv(execInfoEnv, d.execInfo(t))
				ctx, cancel := context.WithCancel(t.Context())
				go func() {
					for d.probes.Load() < 2 {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
				before := len(x.f.DeleteRequests())
				_, err := x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorContains(t, err, "was revoked")

				if tc.stored {
					assert.Equal(t, loginsBefore, x.f.LoginCount(), "the stored credential produced the child; no login ran")
				} else {
					assert.Equal(t, loginsBefore+1, x.f.LoginCount(), "the plugin-created login ran")
					entry, present := x.entry(t)
					require.True(t, present, "the exec-login entry was persisted before the wait")
					assert.Equal(t, clierr.ProvenanceExecLogin, entry.Provenance)
					parentBearer, err = x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.url})
					require.NoError(t, err)
				}
				creates := x.f.CreateRequests()
				require.NotEmpty(t, creates)
				child := creates[len(creates)-1].Name
				// The acquisition of a plugin-created login revokes its own
				// interim session token; only the child's delete is this wait's.
				var childDeletes []testrancher.DeleteRecord
				for _, del := range x.f.DeleteRequests()[before:] {
					assert.NotEqual(t, extBearerName(parentBearer), del.Name, "the parent is never revoked")
					if del.Name == child {
						childDeletes = append(childDeletes, del)
					}
				}
				require.Len(t, childDeletes, 1, "the child this invocation created is deleted once")
				assert.Equal(t, parentBearer, childDeletes[0].Bearer, "deleted with the parent bearer whatever its provenance")
			})
		})
	}
}

// extBearerName is the token name inside an ext bearer ("ext/<name>:<secret>"
// or "<name>:<secret>").
func extBearerName(bearer string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(bearer, "ext/"), ":")
	return name
}

// TestGetTokenCancelledBeforeAnUnprobeableEndpointStillRevokes asserts the
// caller's cancellation wins over an endpoint that cannot be probed: with
// the context cancelled as the token is created and an exec info whose CA
// does not parse, the fresh token is revoked and no credential returned;
// on a cache hit the cancellation is returned and nothing is revoked.
func TestGetTokenCancelledBeforeAnUnprobeableEndpointStillRevokes(t *testing.T) {
	badCA := execInfoFor(t, "https://ace.example.test:6443", []byte("not a pem"))
	t.Run("fresh token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			// No direct endpoint is routed: the probe keeps the production
			// client constructor, which fails on the CA before any request.
			x := newInMemoryPluginFixture(t, false, testrancher.Options{})
			directWaitStarted = cancel // the token exists; the caller is gone as the wait begins
			t.Cleanup(func() { directWaitStarted = nil })
			t.Setenv(execInfoEnv, badCA)
			before := len(x.f.DeleteRequests())
			_, err := x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "was revoked")
			require.Len(t, x.f.DeleteRequests()[before:], 1)
			_, _, cerr := x.store.GetCachedCred(config.CacheKey("kc", credhelperCanonical(t, x.url), "local"), anchorFor(x.dir))
			assert.ErrorIs(t, cerr, config.ErrNoToken)
		})
	})
	t.Run("cache hit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			x := newInMemoryPluginFixture(t, false, testrancher.Options{})
			res, err := x.getToken(t, "local", "kc", localPrompter())
			require.NoError(t, err)
			cached := res.Data.(*execCredential).Status.Token
			t.Setenv(execInfoEnv, badCA)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			before := len(x.f.DeleteRequests())
			_, err = x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, x.f.DeleteRequests()[before:], "a cached token is not this invocation's to revoke")
			tok, _, cerr := x.store.GetCachedCred(config.CacheKey("kc", credhelperCanonical(t, x.url), "local"), anchorFor(x.dir))
			require.NoError(t, cerr)
			assert.Equal(t, cached, tok, "the cache is untouched")
		})
	})
}

// markRefusingStore is a credential store whose readiness mark always
// finds the record changed: it deletes the record and reports false, the
// shape of another process refreshing or clearing the slot mid-probe.
type markRefusingStore struct {
	config.CredStore
	refused atomic.Int32
}

func (s *markRefusingStore) MarkCachedCredReady(key, token, _ string) (bool, error) {
	s.refused.Add(1)
	_, _, err := s.DeleteCachedCredsIfUnchanged(map[string]string{key: token})
	return false, err
}

// TestGetTokenRefusedReadinessMarkIsAMiss asserts a cache hit whose
// re-probe succeeds but whose readiness mark is refused takes the miss
// path: the token read from the cache is never returned; a new token is
// created and returned instead.
func TestGetTokenRefusedReadinessMarkIsAMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv(execInfoEnv, "")
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		res, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		cached := res.Data.(*execCredential).Status.Token

		d := newInMemoryDirectEndpoint(t, 0, "review")
		x.route(d)
		t.Setenv(execInfoEnv, d.execInfo(t))
		store := &markRefusingStore{CredStore: x.store}
		creates := x.f.TokenCount()
		res2, err := x.command(store).Execute(t.Context(), x.input("local", "kc"), localPrompter())
		require.NoError(t, err)
		assert.Equal(t, int32(2), store.refused.Load(), "the hit's mark and the fresh token's mark were both refused")
		fresh := res2.Data.(*execCredential).Status.Token
		assert.NotEqual(t, cached, fresh, "the stale token was never returned")
		assert.Equal(t, creates+1, x.f.TokenCount(), "the miss path created a token")
	})
}

// TestGetTokenMalformedExecInfoWarnsOnAWarmCache asserts a value in
// KUBERNETES_EXEC_INFO that does not parse never takes the flag-only fast
// path: the cached credential is returned through the resolved path with
// exactly one warning naming the variable.
func TestGetTokenMalformedExecInfoWarnsOnAWarmCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv(execInfoEnv, "")
		x := newInMemoryPluginFixture(t, false, testrancher.Options{})
		res, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		cached := res.Data.(*execCredential).Status.Token
		creates := x.f.TokenCount()

		t.Setenv(execInfoEnv, "{not json")
		res2, err := x.getToken(t, "local", "kc", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, cached, res2.Data.(*execCredential).Status.Token)
		assert.Equal(t, creates, x.f.TokenCount(), "a cache hit")
		require.Len(t, res2.Warnings, 1)
		assert.Contains(t, res2.Warnings[0], execInfoEnv+" could not be read")
	})
}

// TestDirectWaitCancelledReportsTheFate asserts the cancellation error's
// details for a revoked, a failed, and a never-attempted cleanup.
func TestDirectWaitCancelledReportsTheFate(t *testing.T) {
	t.Parallel()
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-x"}
	fresh := &freshToken{create: CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, Bearer: "b"}}
	for _, tc := range []struct {
		cleanup RevokeOutcome
		message string
		detail  string
	}{
		{RevokeOutcome{Ref: ref, Result: ResultRevoked}, "was revoked", "cluster token cleanup: revoked (ext/token-x)"},
		{RevokeOutcome{Ref: ref, Result: ResultFailed, Error: "500 Internal Server Error"}, "could not be revoked", "cluster token cleanup: failed (ext/token-x): 500 Internal Server Error"},
		{RevokeOutcome{Result: ResultNotAttempted}, "nothing was revoked", "cluster token cleanup: notAttempted (ext/token-x)"},
	} {
		err := directWaitCancelled(context.Canceled, fresh, tc.cleanup)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, tc.message)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		details, ok := ce.Details.(clierr.TextDetails)
		require.True(t, ok)
		assert.Equal(t, []string{tc.detail}, details.TextDetails())
	}
}
