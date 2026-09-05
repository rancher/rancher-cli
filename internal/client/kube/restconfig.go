package kube

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// BaseURL returns scheme://host for a Rancher server URL, dropping a /v3 suffix.
func BaseURL(serverURL string) string {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil || u.Host == "" {
		// Best-effort: trim a trailing /v3 textually.
		return strings.TrimSuffix(strings.TrimSuffix(serverURL, "/"), "/v3")
	}
	return u.Scheme + "://" + u.Host
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
	if eff.URL == "" {
		// Config-aware when the inventory is present: after a logout the
		// server entries survive, and every command routed through this
		// factory should answer with the re-login shortcut, not the
		// blank-machine wording (falls back to it when truly blank).
		return nil, config.ErrNotLoggedInFor(eff.KnownServers)
	}
	if eff.Token == "" {
		// eff.URL is resolved (a current server or RANCHER_CLI_URL exists) but
		// no token. A concrete configured ServerName is the designed
		// post-add state under two-phase registration - added but never
		// logged in, or logged out - so it is NOT_LOGGED_IN with a concrete
		// login hint. An env-only RANCHER_CLI_URL carries no name to
		// interpolate (Effective clears ServerName on a mismatched env URL,
		// or never had one on a blank machine), so it stays AUTH with body
		// guidance naming RANCHER_CLI_TOKEN and no machine hint.
		if eff.ServerName != "" {
			return nil, clierr.New(clierr.CodeNotLoggedIn,
				fmt.Sprintf("not logged in to %q", eff.ServerName)).
				WithHint(config.LoginHintFor(eff.ServerName))
		}
		return nil, clierr.New(clierr.CodeAuth,
			"no token for server; set "+config.EnvToken+", or unset "+config.EnvURL+" and use a configured server")
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
	return rc, nil
}

// HTTPClient builds the http.Client every client-go client in this repo
// rides on: rest.HTTPClientFor plus the same-origin redirect policy shared
// with httpx. The User-Agent is defaulted first: client-go installs its
// User-Agent round-tripper inside HTTPClientFor and only when the config
// names one, and the *ForConfigAndClient constructors never revisit a
// client they were handed, so an unset agent here would send Go's stdlib
// default on every request. The client is copied before the policy is
// set so it never lands on a client this package does not own:
// HTTPClientFor can hand back a shared client (http.DefaultClient for a
// config whose transport is the default), and client-go is free to widen
// that sharing.
func HTTPClient(rc *rest.Config) (*http.Client, error) {
	cfg := *rc
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
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
