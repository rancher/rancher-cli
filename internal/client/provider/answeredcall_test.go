package provider

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// tlsClient is a cluster client built from kube.RESTConfig against a TLS
// test server, with the server's transport.
func tlsClient(t *testing.T, srv *httptest.Server, cluster string) (engine.ClusterClient, *rest.Config) {
	t.Helper()
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, cluster)
	require.NoError(t, err)
	rc.Transport = srv.Client().Transport
	c, err := NewClusterClient(rc, cluster)
	require.NoError(t, err)
	return c, rc
}

// hang is a handler body for a server that never answers. It reads the
// request body first: net/http notices a closed connection, and ends the
// request's context, only once the body has been read.
func hang(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	testtransport.HangAndAbort(r)
}

const notFoundBody = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"namespaces \"b\" not found","reason":"NotFound","code":404}`

func writeAnswer(w http.ResponseWriter, status int, retryAfter, body string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestConcurrentCallsOnOneClientKeepTheirOwnLastAnswer asserts the status a
// call was answered belongs to that call alone, over one client and so one
// configuration: call A is answered 429 with a Retry-After header of 120
// seconds, its wait is cut by the deadline, and it names the 429; call B,
// started after the server wrote that 429, hangs with no answer and says the
// server may be slow, is answered 530 and names 530, or is answered 404 and
// is NOT_FOUND, and none of them names a 429 (matched as "HTTP 429", since
// the message also names the test server's random port). Run with -race.
func TestConcurrentCallsOnOneClientKeepTheirOwnLastAnswer(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(2 * time.Second))
	for _, tc := range []struct {
		name string
		// answer writes B's answer, or hangs when it is nil.
		answer func(w http.ResponseWriter)
		check  func(t *testing.T, err error)
	}{
		{"B hangs", nil, func(t *testing.T, err error) {
			t.Helper()
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Contains(t, ce.Message, "the server may be slow")
			assert.NotContains(t, ce.Message, "HTTP 429")
			_, answered := clierr.HTTPStatus(err)
			assert.False(t, answered)
		}},
		{"B is answered 530", func(w http.ResponseWriter) { writeAnswer(w, 530, "", cloudflareBody) }, func(t *testing.T, err error) {
			t.Helper()
			ce := assertAnswered(t, err, 530)
			assert.NotContains(t, ce.Message, "HTTP 429")
		}},
		{"B is answered 404", func(w http.ResponseWriter) { writeAnswer(w, 404, "", notFoundBody) }, func(t *testing.T, err error) {
			t.Helper()
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.NotContains(t, ce.Message, "HTTP 429")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var once sync.Once
			firstAnswer := make(chan struct{})
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/a"):
					writeAnswer(w, 429, "120", cloudflareBody)
					once.Do(func() { close(firstAnswer) })
				case tc.answer != nil:
					tc.answer(w)
				default:
					hang(r)
				}
			}))
			t.Cleanup(srv.Close)
			c, _ := tlsClient(t, srv, "local")

			var wg sync.WaitGroup
			var errA, errB error
			aDone := make(chan struct{})
			wg.Go(func() {
				defer close(aDone)
				_, errA = c.Get(t.Context(), guardNamespaces, "", "a")
			})
			select {
			case <-firstAnswer:
			case <-aDone:
				require.Fail(t, "call A ended before the server wrote its 429", "%v", errA)
			}
			wg.Go(func() { _, errB = c.Get(t.Context(), guardNamespaces, "", "b") })
			wg.Wait()

			ce := assertAnswered429AfterTimeout(t, errA)
			assert.Contains(t, ce.Message, " answered HTTP 429 Too Many Requests and the request ran out of time (2s) - retry later")
			tc.check(t, errB)
		})
	}
}

// assertAnswered429AfterTimeout asserts err is the retryable GENERIC error
// of a call that a deadline ended while it waited on a 429 answer, with the
// status in its cause.
func assertAnswered429AfterTimeout(t *testing.T, err error) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	status, ok := clierr.HTTPStatus(err)
	assert.True(t, ok)
	assert.Equal(t, 429, status)
	return ce
}

// TestConcurrentCallsOnOneClientHaveABudgetEach asserts the time a call
// agrees to wait on 429 answers is the call's own: two calls on one client,
// each answered 429 with a Retry-After header of 120 seconds on every try,
// each send exactly two requests. Run with -race.
func TestConcurrentCallsOnOneClientHaveABudgetEach(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(4 * time.Second))
	t.Cleanup(kube.SetRetryLimitsForTest(time.Second, time.Second))
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]++
		mu.Unlock()
		writeAnswer(w, 429, "120", cloudflareBody)
	}))
	t.Cleanup(srv.Close)
	c, _ := tlsClient(t, srv, "local")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"a", "b"} {
		wg.Go(func() { _, errs[i] = c.Get(t.Context(), guardNamespaces, "", name) })
	}
	wg.Wait()
	for _, err := range errs {
		_ = assertAnswered(t, err, 429)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int{"a": 2, "b": 2}, hits)
}

// answeredThenHangs is a server that answers the request paths /probe<status>
// with that status and hangs on any other.
func answeredThenHangs(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, status, ok := strings.Cut(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "probe")
		if !ok {
			hang(r)
			return
		}
		code := map[string]int{"200": 200, "404": 404, "409": 409, "503": 503}[status]
		writeAnswer(w, code, "", `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// probe sends one GET of the call ctx belongs to, through the client
// built from rc, so the call's record holds the answer.
func probe(t *testing.T, ctx context.Context, rc *rest.Config, name string) {
	t.Helper()
	hc, err := kube.HTTPClient(rc)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rc.Host+"/"+name, nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

// TestADeadlineNamesOnlyTheAnswerOfTheRequestThatRanOutOfTime asserts the
// status a deadline error names is the answer to the request in flight when
// the deadline came, never an earlier request's of the same call: a 404
// followed by a request that hangs, and a 429 whose retry hangs, read as no
// answer; a 429 whose wait the deadline cuts names 429, also for a POST,
// whose message and cause then say nothing of a GET.
func TestADeadlineNamesOnlyTheAnswerOfTheRequestThatRanOutOfTime(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(time.Second))

	t.Run("a 404 then a request that hangs", func(t *testing.T) {
		srv := answeredThenHangs(t)
		c, rc := tlsClient(t, srv, "local")
		ctx := kube.WithAnswers(t.Context())
		probe(t, ctx, rc, "probe404")
		_, err := c.Get(ctx, guardNamespaces, "", "a")
		assertNoAnswerTimeout(t, err)
	})

	t.Run("a 429 whose retry hangs", func(t *testing.T) {
		defer kube.SetRetryLimitsForTest(time.Second, 10*time.Second)()
		defer SetRequestTimeoutForTest(2 * time.Second)()
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				writeAnswer(w, 429, "1", cloudflareBody)
				return
			}
			hang(r)
		}))
		t.Cleanup(srv.Close)
		c, _ := tlsClient(t, srv, "local")
		_, err := c.Get(t.Context(), guardNamespaces, "", "a")
		assert.Equal(t, 2, int(hits.Load()), "the retry was sent")
		assertNoAnswerTimeout(t, err)
	})

	t.Run("a 429 whose wait the deadline cuts", func(t *testing.T) {
		for method, call := range map[string]func(c engine.ClusterClient) error{
			"GET": func(c engine.ClusterClient) error {
				_, err := c.Get(t.Context(), guardNamespaces, "", "a")
				return err
			},
			"POST": func(c engine.ClusterClient) error {
				_, err := c.Create(t.Context(), guardNamespaces, "", unavailableObject(), false)
				return err
			},
		} {
			t.Run(method, func(t *testing.T) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					writeAnswer(w, 429, "120", cloudflareBody)
				}))
				t.Cleanup(srv.Close)
				c, _ := tlsClient(t, srv, "local")
				err := call(c)
				ce := assertAnswered429AfterTimeout(t, err)
				assert.Equal(t, config.DisplayServerURL(srv.URL)+" answered HTTP 429 Too Many Requests and the request ran out of time (1s) - retry later", ce.Message)
				if method == "POST" {
					assert.NotContains(t, strings.ToLower(ce.Message), "get")
					cause := errors.Unwrap(ce)
					require.Error(t, cause, "the error keeps the answered status as its cause")
					assert.NotContains(t, strings.ToLower(cause.Error()), "get")
				}
			})
		}
	})
}

// assertNoAnswerTimeout asserts err says the server may be slow and holds no
// answered status.
func assertNoAnswerTimeout(t *testing.T, err error) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "the server may be slow")
	assert.NotContains(t, ce.Message, "answered HTTP")
	_, answered := clierr.HTTPStatus(err)
	assert.False(t, answered)
}

// TestTheMethodOfACallLabelsTheStatusItsDeadlineNames asserts the cause of a
// deadline error that names a status is the answer as that method gets it: a
// 409 answers a POST AlreadyExists and a GET Conflict. The answer is read
// from the record of the request in flight, put there by a request sent
// before the error is mapped.
func TestTheMethodOfACallLabelsTheStatusItsDeadlineNames(t *testing.T) {
	t.Parallel()
	srv := answeredThenHangs(t)
	_, rc := tlsClient(t, srv, "local")
	timeout := &url.Error{Op: "Post", URL: srv.URL, Err: context.DeadlineExceeded}
	for method, check := range map[string]func(error) bool{
		http.MethodPost: apierrors.IsAlreadyExists,
		http.MethodGet:  apierrors.IsConflict,
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			ctx := kube.WithAnswers(t.Context())
			probe(t, ctx, rc, "probe409")
			err := mapRequestErr(ctx, method, timeout, &clierr.Request{Server: config.DisplayServerURL(srv.URL)})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			require.Error(t, errors.Unwrap(ce), "the error keeps the answered status as its cause")
			assert.True(t, check(errors.Unwrap(ce)), "%v", errors.Unwrap(ce))
		})
	}
}

// TestAPreflightThatIsCutWhileItWaitsOnA429NamesTheStatus asserts the version
// request of the preflight and of the owner check, ended by the request
// timeout while it waits on a 429 answer, says so, and one that was never
// answered still says the server cannot be reached.
func TestAPreflightThatIsCutWhileItWaitsOnA429NamesTheStatus(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(2 * time.Second))
	for name, call := range map[string]func(t *testing.T, srv *httptest.Server) error{
		"preflight": func(t *testing.T, srv *httptest.Server) error {
			t.Helper()
			_, err := Preflight(t.Context(), config.Effective{URL: srv.URL, Token: "id:secret", CA: certPEM(srv)})
			return err
		},
		"owner": func(t *testing.T, srv *httptest.Server) error {
			t.Helper()
			_, err := Owner(t.Context(), config.Effective{URL: srv.URL, Token: "id:secret", CA: certPEM(srv)})
			return err
		},
	} {
		t.Run(name+" while it waits on a 429", func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				writeAnswer(w, 429, "120", cloudflareBody)
			}))
			t.Cleanup(srv.Close)
			ce := assertAnswered429AfterTimeout(t, call(t, srv))
			assert.Equal(t, config.DisplayServerURL(srv.URL)+" answered HTTP 429 Too Many Requests and the request ran out of time (2s) - retry later", ce.Message)
			assert.NotContains(t, ce.Message, "cannot reach server")
		})
		t.Run(name+" with no answer", func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hang(r) }))
			t.Cleanup(srv.Close)
			err := call(t, srv)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.True(t, ce.Retryable)
			assert.Contains(t, ce.Message, "cannot reach server")
		})
	}
}

// certPEM is the certificate of a TLS test server in PEM.
func certPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// TestAnAnswerNamesTheTimeOnlyOfACallThatHasABound asserts the deadline error
// of a call that was answered 429 names the bound of the call, and none for a
// call that has no bound of its own, which a discovery walk has: the time
// differs from the caller's deadline and from the request timeout.
func TestAnAnswerNamesTheTimeOnlyOfACallThatHasABound(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(3 * time.Second))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeAnswer(w, 429, "", `{}`) }))
	t.Cleanup(srv.Close)
	_, rc := tlsClient(t, srv, "local")
	ctx := kube.WithAnswers(t.Context())
	probe(t, ctx, rc, "version")
	timeout := &url.Error{Op: "Get", URL: srv.URL, Err: context.DeadlineExceeded}
	q := &clierr.Request{Server: config.DisplayServerURL(srv.URL)}
	lead := config.DisplayServerURL(srv.URL) + " answered HTTP 429 Too Many Requests and the request ran out of time"

	ce, ok := errors.AsType[*clierr.Error](mapRequestErr(ctx, http.MethodGet, timeout, q))
	require.True(t, ok)
	assert.Equal(t, lead+" (3s) - retry later", ce.Message)
	ce, ok = errors.AsType[*clierr.Error](mapRequestErrWithin(ctx, http.MethodGet, 0, timeout, q))
	require.True(t, ok)
	assert.Equal(t, lead+" - retry later", ce.Message)
}

// TestServerResourcesThatRunsOutOfTimeSaysTheServerMayBeSlow asserts a
// discovery walk whose aggregated request was answered 503, and whose legacy
// request then hung until the caller's deadline, reads as no answer: the 503
// is the answer to another request.
func TestServerResourcesThatRunsOutOfTimeSaysTheServerMayBeSlow(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(3 * time.Second))
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			writeAnswer(w, 503, "", cloudflareBody)
			return
		}
		hang(r)
	}))
	t.Cleanup(srv.Close)
	c, _ := tlsClient(t, srv, "local")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.ServerResources(ctx)
	assertNoAnswerTimeout(t, err)
}

// TestParallelDiscoveryRequestsDoNotAnswerForOneAnother asserts that when
// the legacy walk reads its group version documents at the same time, one
// document answered 503 does not stand for another that hangs until the
// caller's deadline: the walk reads as no answer. The server holds the 503
// until the hanging request has arrived, so the 503 is the last answer any
// request of the walk got.
func TestParallelDiscoveryRequestsDoNotAnswerForOneAnother(t *testing.T) {
	arrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.Header.Get("Accept"), "apidiscovery"):
			w.WriteHeader(http.StatusNotAcceptable)
		case r.URL.Path == "/k8s/clusters/local/api":
			writeAnswer(w, 200, "", `{"kind":"APIVersions","versions":["v1"]}`)
		case r.URL.Path == "/k8s/clusters/local/apis":
			writeAnswer(w, 200, "", `{"kind":"APIGroupList","groups":[`+
				`{"name":"a.io","versions":[{"groupVersion":"a.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"a.io/v1","version":"v1"}},`+
				`{"name":"b.io","versions":[{"groupVersion":"b.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"b.io/v1","version":"v1"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1"):
			writeAnswer(w, 200, "", `{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`)
		case strings.HasSuffix(r.URL.Path, "/apis/a.io/v1"):
			<-arrived
			writeAnswer(w, 503, "", cloudflareBody)
		default:
			once.Do(func() { close(arrived) })
			hang(r)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := tlsClient(t, srv, "local")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := c.ServerResources(ctx)
	assertNoAnswerTimeout(t, err)
}

// TestADiscoveryWalkOfManyGroupVersionsAnsweredWith429NamesTheStatus asserts
// a legacy discovery walk of 20 group version documents, more than the 8 it
// reads at once, that the server answers 429 with a Retry-After header of
// 120 seconds returns at once, long before the request timeout, with the 429
// named for each document. None of its requests waits: the raw client never
// repeats a request. The 429 budget the requests share is asserted in the
// kube package (TestRecordsOfOneCallShareItsBudget).
func TestADiscoveryWalkOfManyGroupVersionsAnsweredWith429NamesTheStatus(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(10 * time.Second))
	var groups []string
	for i := range 20 {
		name := "g" + strconv.Itoa(i) + ".io"
		groups = append(groups, `{"name":"`+name+`","versions":[{"groupVersion":"`+name+`/v1","version":"v1"}],"preferredVersion":{"groupVersion":"`+name+`/v1","version":"v1"}}`)
	}
	var documents atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.Header.Get("Accept"), "apidiscovery"):
			w.WriteHeader(http.StatusNotAcceptable)
		case r.URL.Path == "/k8s/clusters/local/api":
			writeAnswer(w, 200, "", `{"kind":"APIVersions","versions":["v1"]}`)
		case r.URL.Path == "/k8s/clusters/local/apis":
			writeAnswer(w, 200, "", `{"kind":"APIGroupList","groups":[`+strings.Join(groups, ",")+`]}`)
		default:
			documents.Add(1)
			writeAnswer(w, 429, "120", cloudflareBody)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := tlsClient(t, srv, "local")
	start := time.Now()
	_, err := c.ServerResources(t.Context())
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 21, int(documents.Load()), "the core document and the 20 groups")
	require.Error(t, err)
	var incomplete *engine.DiscoveryIncomplete
	require.ErrorAs(t, err, &incomplete)
	assert.Len(t, incomplete.Failed, 21)
	for gv, cause := range incomplete.Failed {
		status, ok := clierr.HTTPStatus(cause)
		assert.True(t, ok, "%v", gv)
		assert.Equal(t, 429, status, "%v", gv)
	}
}
