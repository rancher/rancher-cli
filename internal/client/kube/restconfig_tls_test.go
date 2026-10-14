package kube

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
)

func untrustedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var distinctRemedy = tlstrust.Remedy{CA: "; CA-SLOT", URL: "URL-SLOT", Fix: "FIX-SLOT"}

// TestRESTConfigClassifiesCertificateFailuresOfTheServer asserts every
// client built from a RESTConfig or RESTConfigImpersonating (the plain
// http client, discovery and the dynamic client) returns a TLS_TRUST error
// worded by the effective connection's remedy when the server's
// certificate is not trusted. Mutations: leave WrapTransport unset in
// either builder; build the wrapper without the remedy.
func TestRESTConfigClassifiesCertificateFailuresOfTheServer(t *testing.T) {
	srv := untrustedServer(t)
	eff := config.Effective{URL: srv.URL, Token: "id:secret", TLSRemedy: distinctRemedy}
	builders := map[string]func() (*rest.Config, error){
		"RESTConfig":              func() (*rest.Config, error) { return RESTConfig(eff, "c-1") },
		"RESTConfigImpersonating": func() (*rest.Config, error) { return RESTConfigImpersonating(eff, "c-1", "alice") },
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			rc, err := build()
			require.NoError(t, err)
			require.NotNil(t, rc.WrapTransport)
			// The system roots do not trust the test server. The test gives
			// the config its own transport, as the default one is guarded.
			rc.Transport = &http.Transport{}
			t.Cleanup(rc.Transport.(*http.Transport).CloseIdleConnections)

			hc, err := HTTPClient(rc)
			require.NoError(t, err)
			_, err = hc.Get(rc.Host + "/version")
			assertTrustError(t, err)

			dc, err := Discovery(rc)
			require.NoError(t, err)
			_, err = dc.ServerVersion()
			assertTrustError(t, err)

			dyn, err := Dynamic(rc)
			require.NoError(t, err)
			_, err = dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace("ns").Get(t.Context(), "p", metav1.GetOptions{})
			assertTrustError(t, err)
		})
	}
}

func assertTrustError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce := tlstrust.TypedError(err)
	require.NotNil(t, ce, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "(or verify the fingerprint shown here out of band); CA-SLOT; if this server")
}

// TestHTTPClientInstallsNoClassifier asserts a client built by HTTPClient
// from a config that did not come from RESTConfig keeps the transport
// error: the direct endpoint of a kubeconfig's exec entry is not the
// Rancher server. Mutation: install the classifier in HTTPClient.
func TestHTTPClientInstallsNoClassifier(t *testing.T) {
	srv := untrustedServer(t)
	hc, err := HTTPClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: &http.Transport{}})
	require.NoError(t, err)
	_, err = hc.Get(srv.URL + "/version")
	require.Error(t, err)
	assert.Nil(t, tlstrust.TypedError(err))
}

// TestRESTConfigWrapperLetsClientGoReachTheTransportBeneath asserts the
// classifying wrapper RESTConfig installs exposes the transport it wraps the
// way client-go's transport wrappers do, so a client-go client with a timeout
// can cancel a timed-out request through it instead of logging that it
// cannot. Mutation: remove WrappedRoundTripper from the wrapper.
func TestRESTConfigWrapperLetsClientGoReachTheTransportBeneath(t *testing.T) {
	rc, err := RESTConfig(config.Effective{URL: "https://rancher.example", Token: "id:secret"}, "local")
	require.NoError(t, err)
	require.NotNil(t, rc.WrapTransport)
	inner := &http.Transport{}
	wrapped, ok := rc.WrapTransport(inner).(utilnet.RoundTripperWrapper)
	require.True(t, ok, "the wrapper must expose the transport it wraps")
	assert.Same(t, inner, wrapped.WrappedRoundTripper())
}
