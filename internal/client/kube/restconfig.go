package kube

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// BaseURL returns scheme://host for a Rancher server URL, dropping any userinfo,
// path, query and fragment. It returns "" when the URL does not parse into a
// scheme and a host: no raw text, which may hold a password, passes through.
func BaseURL(serverURL string) string {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// RequireLogin is the refusal of a command that needs the server when the
// invocation has no server URL or no token: every client built for the
// server - a cluster client or the root client - answers it the same way,
// whichever reaches the server first. nil when both are present.
func RequireLogin(eff config.Effective) error {
	if eff.URL == "" {
		// Config-aware when the inventory is present: after a logout the
		// server entries survive, and every command routed through this
		// factory should answer with the re-login shortcut, not the
		// blank-machine wording (falls back to it when truly blank).
		return config.ErrNotLoggedInFor(eff.KnownServers, eff.ServersWithCA)
	}
	if eff.Token == "" {
		// eff.URL is resolved (a current server or RANCHER_CLI_URL exists) but
		// no token. A concrete configured ServerName is the normal
		// post-add state under two-phase registration - added but never
		// logged in, or logged out - so it is NOT_LOGGED_IN with a concrete
		// login hint, and a message that names the directory's unfinished
		// store change, read here and only here - the status report as the
		// hint while its record cannot be read. An env-only RANCHER_CLI_URL carries no name to
		// interpolate (Effective clears ServerName on a mismatched env URL,
		// or never had one on a blank machine), so it stays AUTH with body
		// guidance naming RANCHER_CLI_TOKEN and no machine hint.
		if eff.ServerName != "" {
			return eff.UnfinishedChange().Apply(clierr.New(clierr.CodeNotLoggedIn,
				fmt.Sprintf("not logged in to %q", eff.ServerName)).
				WithHint(config.LoginHintFor(eff.ServerName)))
		}
		return clierr.New(clierr.CodeAuth,
			"no token for server; set "+config.EnvToken+", or unset "+config.EnvURL+" and use a configured server")
	}
	return nil
}

// RESTConfig builds a client-go rest.Config aimed at the Rancher k8s proxy
// for the EXPLICITLY named cluster ("local" for the management plane). The
// cluster is a required parameter, never inherited from stored state: callers pass
// engine.LocalCluster or the invocation's own --cluster value.
func RESTConfig(eff config.Effective, cluster string) (*rest.Config, error) {
	if cluster == "" {
		return nil, clierr.New(clierr.CodeValidation,
			`cluster id must not be empty; pass an explicit cluster id ("local" for the management plane)`)
	}
	if err := RequireLogin(eff); err != nil {
		return nil, err
	}
	host := fmt.Sprintf("%s/k8s/clusters/%s", BaseURL(eff.URL), cluster)

	// eff.CA is pem-or-path, same as kubeconfig's and get-token's --cacert:
	// resolve through config.ResolveCA rather than using it as raw CAData, or
	// a path-valued CA (from a stored `--cacert /path/to/ca.pem` login) never
	// gets read and every cluster command fails to parse it as PEM.
	caPEM, err := config.ResolveCA(eff.CA)
	if err != nil {
		return nil, err
	}

	rc := &rest.Config{
		Host:        host,
		BearerToken: eff.Token,
	}
	if caPEM != "" {
		rc.TLSClientConfig = rest.TLSClientConfig{CAData: []byte(caPEM)}
	}
	// A certificate failure of a request to the Rancher origin becomes
	// TLS_TRUST. HTTPClient copies rc, so every client built from it carries
	// the wrapper; a config that does not come from here (the direct
	// endpoint of a kubeconfig's exec entry) is not the Rancher server and
	// stays unwrapped.
	rc.WrapTransport = tlstrust.WrapTransportFor(host, eff.TLSRemedy)
	return rc, nil
}

// RESTConfigImpersonating is RESTConfig with an Impersonate-User header on
// every request. Only the user is set: Rancher attaches the subject's
// groups itself and refuses client-supplied group headers downstream.
func RESTConfigImpersonating(eff config.Effective, cluster, user string) (*rest.Config, error) {
	if user == "" {
		return nil, clierr.New(clierr.CodeValidation, "impersonated user must not be empty")
	}
	rc, err := RESTConfig(eff, cluster)
	if err != nil {
		return nil, err
	}
	rc.Impersonate = rest.ImpersonationConfig{UserName: user}
	return rc, nil
}

// HTTPClient builds the http.Client every client-go client in this repo
// rides on: rest.HTTPClientFor plus the same-origin redirect policy shared
// with httpx. The User-Agent is always set to httpx.UserAgent(), replacing
// the config's: client-go installs its User-Agent round-tripper inside
// HTTPClientFor and only when the config names one, and the
// *ForConfigAndClient constructors never revisit a client they were
// handed, so an unset agent here would send Go's stdlib default on every
// request. The client is copied before the policy is set so it never lands
// on a client this package does not own: HTTPClientFor can hand back a
// shared client (http.DefaultClient for a config whose transport is the
// default), and client-go is free to widen that sharing.
func HTTPClient(rc *rest.Config) (*http.Client, error) {
	cfg := *rc
	cfg.UserAgent = httpx.UserAgent()
	base, err := rest.HTTPClientFor(&cfg)
	if err != nil {
		return nil, err
	}
	client := *base
	client.CheckRedirect = httpx.RejectCrossOriginRedirect
	return &client, nil
}

// Dynamic builds a dynamic client for arbitrary GVRs.
func Dynamic(rc *rest.Config) (dynamic.Interface, error) {
	hc, err := HTTPClient(rc)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfigAndClient(rc, hc)
}

// TableClient builds the REST client for requests whose answer is a Table:
// the metadata client's config shape (no group version, a decoder that
// reads the meta.k8s.io Table in both its versions), over the same
// HTTPClient the dynamic client uses, so the certificate classification, the
// same-origin redirect policy and the User-Agent are the same. The caller
// sets the Accept header and builds the path with AbsPath.
func TableClient(rc *rest.Config) (rest.Interface, error) {
	hc, err := HTTPClient(rc)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(rc)
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.AcceptContentTypes = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	cfg.GroupVersion = &schema.GroupVersion{}
	// The table client builds every request path with AbsPath. APIPath is a
	// value no request may send: a request built with Resource or Name
	// instead would carry it to the server and fail.
	cfg.APIPath = "/this-value-should-never-be-sent"
	return rest.UnversionedRESTClientForConfigAndClient(cfg, hc)
}

// Discovery builds a discovery client (used by auth status / server-version).
func Discovery(rc *rest.Config) (discovery.DiscoveryInterface, error) {
	hc, err := HTTPClient(rc)
	if err != nil {
		return nil, err
	}
	return discovery.NewDiscoveryClientForConfigAndClient(rc, hc)
}

// Clientset builds a typed client-go clientset, used for pod log streaming.
func Clientset(rc *rest.Config) (kubernetes.Interface, error) {
	hc, err := HTTPClient(rc)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfigAndClient(rc, hc)
}
