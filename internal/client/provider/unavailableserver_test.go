package provider

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// cloudflareBody is the JSON a Cloudflare error answers with: not a
// Kubernetes Status, no message field.
const cloudflareBody = `{"type":"https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-1xxx-errors/error-1033/","title":"Error 1033: Cloudflare Tunnel error","status":530,"detail":"The tunnel is not connected"}`

const guardStatusBody = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"etcdserver: request timed out","reason":"ServiceUnavailable","code":503}`

var (
	guardNamespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	guardDeployment = schema.GroupVersionResource{Group: "apps", Resource: "deployments"}
)

// guardPath is one way a command sends a request to the server, and what
// it takes to make one call down it.
type guardPath struct {
	name string
	// method is the engine.ClusterClient or engine.TableReader method the
	// path calls, empty for a path that is no method of either.
	method string
	// retries is true where client-go repeats a request answered 429 with a
	// Retry-After header; the other paths send each request once.
	retries bool
	// named is true where the error carries the server, so its message
	// starts with it.
	named bool
	// prefix is the text the path puts in front of the server's answer.
	prefix string
	// clusterOK is true for a path that goes through the cluster the test
	// names; the others always go through the local cluster.
	clusterOK bool
	// several is true where one call sends more than one request, so only
	// its count with a Retry-After header is compared with its count with
	// none.
	several bool
	// plain is true where the error is not a *clierr.Error, only an error
	// whose chain holds the answered status.
	plain bool
	// run makes one call, through the cluster, against srv.
	run func(t *testing.T, srv *testtransport.Answers, cluster string) error
}

// guardEff is the configuration of a call against srv, with the server's
// certificate as the CA.
func guardEff(srv *testtransport.Answers) config.Effective {
	return config.Effective{URL: srv.URL(), Token: "id:secret", CA: srv.CertPEM()}
}

// guardClient is a cluster client built from kube.RESTConfig, as a command
// builds one, with the test server's transport, which needs no CA in eff.
func guardClient(t *testing.T, srv *testtransport.Answers, cluster string) engine.ClusterClient {
	t.Helper()
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL(), Token: "id:secret"}, cluster)
	require.NoError(t, err)
	rc.Transport = srv.Client().Transport
	c, err := NewClusterClient(rc, cluster)
	require.NoError(t, err)
	return c
}

func unavailableObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "a"},
	}}
}

func guardPaths() []guardPath {
	return []guardPath{
		{name: "get", method: "Get", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).Get(t.Context(), guardNamespaces, "", "a")
				return err
			}},
		{name: "list", method: "List", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).List(t.Context(), guardNamespaces, "", "")
				return err
			}},
		{name: "create", method: "Create", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).Create(t.Context(), guardNamespaces, "", unavailableObject(), true)
				return err
			}},
		{name: "update", method: "Update", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).Update(t.Context(), guardNamespaces, "", unavailableObject(), false)
				return err
			}},
		{name: "delete", method: "Delete", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				return guardClient(t, srv, cluster).Delete(t.Context(), guardNamespaces, "", "a", "", false)
			}},
		{name: "logs", method: "Logs", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).Logs(t.Context(), "ns", "pod", engine.LogOptions{})
				return err
			}},
		{name: "list table", method: "ListTable", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).(engine.TableReader).ListTable(t.Context(), guardNamespaces, "", "")
				return err
			}},
		{name: "get table", method: "GetTable", retries: true, named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).(engine.TableReader).GetTable(t.Context(), guardNamespaces, "", "a")
				return err
			}},
		{name: "version resolution", retries: true, named: true, clusterOK: true, prefix: `discovering version for group "apps": `,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).Get(t.Context(), guardDeployment, "ns", "d")
				return err
			}},
		{name: "version resolution of a list table", retries: true, named: true, clusterOK: true, prefix: `discovering version for group "apps": `,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).(engine.TableReader).ListTable(t.Context(), guardDeployment, "ns", "")
				return err
			}},
		{name: "version resolution of a get table", retries: true, named: true, clusterOK: true, prefix: `discovering version for group "apps": `,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).(engine.TableReader).GetTable(t.Context(), guardDeployment, "ns", "d")
				return err
			}},
		{name: "server resources", method: "ServerResources", several: true, named: true, clusterOK: true, prefix: "GET /api: ",
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				_, err := guardClient(t, srv, cluster).ServerResources(t.Context())
				return err
			}},
		{name: "post review", method: "PostReview", named: true, clusterOK: true,
			run: func(t *testing.T, srv *testtransport.Answers, cluster string) error {
				t.Helper()
				gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(gvk)
				resp, err := guardClient(t, srv, cluster).PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: obj})
				if err != nil {
					return err
				}
				if resp.Mapped == nil {
					return nil
				}
				return resp.Mapped
			}},
		{name: "preflight", retries: true, named: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := Preflight(t.Context(), guardEff(srv))
				return err
			}},
		{name: "owner", retries: true, named: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := Owner(t.Context(), guardEff(srv))
				return err
			}},
		{name: "identity", retries: true, named: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := Identity(t.Context(), guardEff(srv))
				return err
			}},
		{name: "effective token metadata", retries: true, named: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := EffectiveTokenMetadata(t.Context(), guardEff(srv), "t")
				return err
			}},
		{name: "v3 token metadata", retries: true, named: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := V3TokenMetadata(t.Context(), guardEff(srv), "t")
				return err
			}},
		{name: "server info", plain: true,
			run: func(t *testing.T, srv *testtransport.Answers, _ string) error {
				t.Helper()
				_, err := GetServerInfo(t.Context(), guardEff(srv))
				return err
			}},
	}
}

// unavailableRow is one answer a server gives to every request.
type unavailableRow struct {
	status      int
	contentType string
	body        string
}

// guardResult is what one call down a path came to.
type guardResult struct {
	err     error
	count   int
	elapsed time.Duration
	// server is the server as output may show it.
	server string
}

// answerOf runs path against a server that gives the row's answer with
// retryAfter.
func answerOf(t *testing.T, p guardPath, row unavailableRow, retryAfter, cluster string) guardResult {
	t.Helper()
	srv := testtransport.NewAnswers(t, true, row.status, retryAfter, row.contentType, row.body)
	start := time.Now()
	err := p.run(t, srv, cluster)
	return guardResult{err: err, count: srv.Count(), elapsed: time.Since(start), server: config.DisplayServerURL(srv.URL())}
}

// guardSeams shortens the call deadline to 4 seconds and a 429 wait to one
// second, for the test and its subtests.
func guardSeams(t *testing.T) {
	t.Helper()
	t.Cleanup(SetRequestTimeoutForTest(4 * time.Second))
	t.Cleanup(kube.SetRetryLimitsForTest(time.Second, time.Second))
}

// assertAnswered asserts err is the retryable GENERIC error for an answered
// status, that clierr.HTTPStatus finds the status in it, and that its
// message says nothing about a slow server, an unreachable one, a deadline
// or a cluster to inspect.
func assertAnswered(t *testing.T, err error, status int) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable, ce.Message)
	got, ok := clierr.HTTPStatus(err)
	assert.True(t, ok, "no status in %q", ce.Message)
	assert.Equal(t, status, got)
	for _, banned := range []string{"may be slow", "cannot reach server", "inspect cluster", "ran out of time"} {
		assert.NotContains(t, ce.Message, banned)
	}
	return ce
}

// assertAnswered is the package's assertAnswered for a path whose error is a
// *clierr.Error; for a path whose error is a plain one it asserts the
// answered status in the chain and returns nil.
func (p guardPath) assertAnswered(t *testing.T, err error, status int) *clierr.Error {
	t.Helper()
	if !p.plain {
		return assertAnswered(t, err, status)
	}
	require.Error(t, err)
	got, ok := clierr.HTTPStatus(err)
	assert.True(t, ok, "no status in %v", err)
	assert.Equal(t, status, got)
	return nil
}

// TestEveryClientPathFailsAtOnceOnAnUnavailableServer asserts, over every
// way a command sends a request to the server, that a server answering a 5xx
// with a Retry-After header of 1 or 120 seconds gets the same one request as
// one answering with none, and that the call returns a retryable GENERIC
// error that holds the answered status, in well under the call deadline,
// with a message that names the server and the status and never says the
// server is slow, cannot be reached, or that a cluster is to be inspected.
// Where client-go repeats a request, a 429 with a Retry-After header of 1 or
// 120 seconds, answered on every try, makes exactly two requests and
// returns before the deadline: the call's budget allows one wait. A path
// that repeats nothing sends one.
func TestEveryClientPathFailsAtOnceOnAnUnavailableServer(t *testing.T) {
	guardSeams(t)
	for _, p := range guardPaths() {
		t.Run(p.name, func(t *testing.T) {
			for _, status := range testtransport.UnavailableStatuses {
				t.Run(strconv.Itoa(status), func(t *testing.T) {
					t.Parallel()
					row := unavailableRow{status: status, contentType: "application/json", body: cloudflareBody}
					base := answerOf(t, p, row, "", "local")
					if !p.several {
						assert.Equal(t, 1, base.count, "a request answered %d with no Retry-After", status)
					}
					ce := p.assertAnswered(t, base.err, status)
					if p.named {
						assert.Contains(t, ce.Message, p.prefix+base.server+" answered HTTP "+strconv.Itoa(status))
					}
					t.Logf("requests %s %d with no Retry-After: %d", p.name, status, base.count)
					for _, retryAfter := range []string{"1", "120"} {
						got := answerOf(t, p, row, retryAfter, "local")
						assert.Equal(t, base.count, got.count, "Retry-After %s", retryAfter)
						assert.Less(t, got.elapsed, 2*time.Second, "Retry-After %s", retryAfter)
						ce := p.assertAnswered(t, got.err, status)
						if p.named {
							assert.Contains(t, ce.Message, p.prefix+got.server+" answered HTTP "+strconv.Itoa(status))
						}
					}
				})
			}
			t.Run("429", func(t *testing.T) {
				t.Parallel()
				row := unavailableRow{status: 429, contentType: "application/json", body: cloudflareBody}
				base := answerOf(t, p, row, "", "local")
				if !p.several {
					assert.Equal(t, 1, base.count, "a request answered 429 with no Retry-After")
				}
				_ = p.assertAnswered(t, base.err, 429)
				for _, retryAfter := range []string{"1", "120"} {
					got := answerOf(t, p, row, retryAfter, "local")
					want := base.count
					if p.retries {
						want = 2
					}
					assert.Equal(t, want, got.count, "Retry-After %s", retryAfter)
					assert.Less(t, got.elapsed, 3500*time.Millisecond, "Retry-After %s", retryAfter)
					ce := p.assertAnswered(t, got.err, 429)
					if p.named {
						assert.Contains(t, ce.Message, p.prefix+got.server+" answered HTTP 429 Too Many Requests")
					}
				}
			})
		})
	}
}

// TestEveryClientPathWordsAnUnavailableServerTheSame asserts the exact
// message of each answer, written out here, on every path that names its
// server: the Rancher sentence for a proxy's code through Rancher itself
// and the cluster sentence through a cluster; the plain status text for
// 502, 503, 429 and 500, with the agent note for a 502 through a cluster;
// the text a server sent with its answer, a Kubernetes Status or plain
// text, kept as received.
func TestEveryClientPathWordsAnUnavailableServerTheSame(t *testing.T) {
	guardSeams(t)
	const (
		rancher = "a proxy or load balancer in front of Rancher could not reach Rancher"
		agent   = "its agent may not be registered yet; retry once the cluster is active"
	)
	for _, tc := range []struct {
		name    string
		cluster string
		row     unavailableRow
		want    string
	}{
		{"530 to Rancher", "local", unavailableRow{530, "application/json", cloudflareBody}, " answered HTTP 530: " + rancher + " - retry later"},
		{"502 to Rancher", "local", unavailableRow{502, "application/json", cloudflareBody}, " answered HTTP 502 Bad Gateway - retry later"},
		{"503 to Rancher", "local", unavailableRow{503, "application/json", cloudflareBody}, " answered HTTP 503 Service Unavailable - retry later"},
		{"429 to Rancher", "local", unavailableRow{429, "application/json", cloudflareBody}, " answered HTTP 429 Too Many Requests - retry later"},
		{"500 to Rancher", "local", unavailableRow{500, "application/json", cloudflareBody}, " answered HTTP 500 Internal Server Error - retry later"},
		{"530 through a cluster", "c-m-x", unavailableRow{530, "application/json", cloudflareBody}, " answered HTTP 530: cluster 'c-m-x' could not be reached - retry later"},
		{"502 through a cluster", "c-m-x", unavailableRow{502, "application/json", cloudflareBody}, " answered HTTP 502 Bad Gateway: cluster 'c-m-x' could not be reached - " + agent},
		{"503 through a cluster", "c-m-x", unavailableRow{503, "application/json", cloudflareBody}, " answered HTTP 503 Service Unavailable - retry later"},
		{"a Status 503", "local", unavailableRow{503, "application/json", guardStatusBody}, " answered HTTP 503 Service Unavailable: etcdserver: request timed out - retry later"},
		{"a plain text 502", "local", unavailableRow{502, "text/plain", "upstream connect error"}, " answered HTTP 502 Bad Gateway: upstream connect error - retry later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range guardPaths() {
				if !p.named || (tc.cluster != "local" && !p.clusterOK) {
					continue
				}
				t.Run(p.name, func(t *testing.T) {
					t.Parallel()
					got := answerOf(t, p, tc.row, "", tc.cluster)
					ce := assertAnswered(t, got.err, tc.row.status)
					assert.Equal(t, p.prefix+got.server+tc.want, ce.Message)
				})
			}
		})
	}
}

// TestTheGuardCoversEveryClusterClientMethod asserts every method of
// engine.ClusterClient and engine.TableReader has a path in the guard, so a
// method added without one fails here. A method that sends no request goes
// in the allowlist with its reason.
func TestTheGuardCoversEveryClusterClientMethod(t *testing.T) {
	t.Parallel()
	noRequest := map[string]string{}
	covered := map[string]bool{}
	for _, p := range guardPaths() {
		covered[p.method] = true
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[engine.ClusterClient](), reflect.TypeFor[engine.TableReader]()} {
		for m := range typ.Methods() {
			if _, ok := noRequest[m.Name]; ok {
				continue
			}
			assert.True(t, covered[m.Name], "%s.%s has no path in the guard", typ.Name(), m.Name)
		}
	}
	for name := range noRequest {
		assert.False(t, covered[name], "%s is allowlisted and covered", name)
	}
}

// TestAnAnswerNamesTheServerWithoutItsUserinfoOrPath asserts the message
// holds the scheme and host of the configured URL, and none of the user,
// the password or the path it carries.
func TestAnAnswerNamesTheServerWithoutItsUserinfoOrPath(t *testing.T) {
	t.Parallel()
	srv := testtransport.NewAnswers(t, true, 530, "", "application/json", cloudflareBody)
	host := strings.TrimPrefix(srv.URL(), "https://")
	eff := guardEff(srv)
	eff.URL = "https://u-secret:pw-secret@" + host + "/p-marker"
	_, err := Preflight(t.Context(), eff)
	ce := assertAnswered(t, err, 530)
	assert.Contains(t, ce.Message, "https://"+host+" answered HTTP 530")
	for _, secret := range []string{"u-secret", "pw-secret", "p-marker"} {
		assert.NotContains(t, ce.Message, secret)
	}
}

// sentinelTransport ends a transport chain.
type sentinelTransport struct{}

func (sentinelTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// chainHoldsTheAnsweredTransport reports whether the transport that wrap
// builds around a sentinel has the recorded transport in its chain.
func chainHoldsTheAnsweredTransport(wrap func(http.RoundTripper) http.RoundTripper) bool {
	rt := wrap(sentinelTransport{})
	for rt != nil {
		if fmt.Sprintf("%T", rt) == "*kube.answeredTransport" {
			return true
		}
		next, ok := rt.(interface{ WrappedRoundTripper() http.RoundTripper })
		if !ok {
			return false
		}
		rt = next.WrappedRoundTripper()
	}
	return false
}

// TestEveryConfigFromRESTConfigCarriesTheAnsweredTransport asserts the
// config a command builds with kube.RESTConfig, and its impersonating form,
// has the transport that drops a 5xx's Retry-After and limits a 429's in its
// chain. A path that builds its config another way fails the guard's rows
// above, which go through this builder.
func TestEveryConfigFromRESTConfigCarriesTheAnsweredTransport(t *testing.T) {
	t.Parallel()
	eff := config.Effective{URL: "https://rancher.example", Token: "id:secret"}
	rc, err := kube.RESTConfig(eff, "c-m-x")
	require.NoError(t, err)
	assert.True(t, chainHoldsTheAnsweredTransport(rc.WrapTransport))
	imp, err := kube.RESTConfigImpersonating(eff, "c-m-x", "u-1")
	require.NoError(t, err)
	assert.True(t, chainHoldsTheAnsweredTransport(imp.WrapTransport))
	assert.False(t, chainHoldsTheAnsweredTransport(func(rt http.RoundTripper) http.RoundTripper { return rt }), "the check reports a chain with none")
}

// TestA429WaitIsLimitedToTheCapOnEveryRetryingPath asserts a Retry-After
// header of 120 seconds on a 429 answered on every try is cut to the cap, one
// second here, with a budget of two: the call sends three requests, waits
// two seconds in all and returns the 429, on every path where client-go
// repeats a request. The count is the main sign; the deadline is far off the
// two seconds, so a stalled runner does not end the call first.
func TestA429WaitIsLimitedToTheCapOnEveryRetryingPath(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(8 * time.Second))
	t.Cleanup(kube.SetRetryLimitsForTest(time.Second, 2*time.Second))
	row := unavailableRow{status: 429, contentType: "application/json", body: cloudflareBody}
	for _, p := range guardPaths() {
		if !p.retries {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			got := answerOf(t, p, row, "120", "local")
			assert.Equal(t, 3, got.count)
			assert.Less(t, got.elapsed, 6*time.Second)
			_ = p.assertAnswered(t, got.err, 429)
		})
	}
}
