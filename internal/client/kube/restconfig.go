package kube

import (
	"fmt"
	"net/url"
	"strings"

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
// cluster is a required parameter, never ambient state: callers pass
// engine.LocalCluster or the invocation's own --cluster value.
func RESTConfig(eff config.Effective, cluster string) (*rest.Config, error) {
	if cluster == "" {
		return nil, clierr.New(clierr.CodeValidation, "cluster id must not be empty").
			WithHint("pass an explicit cluster id (\"local\" for the management plane)")
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
		// post-add state under two-phase registration — added but never
		// logged in, or logged out — so it is NOT_LOGGED_IN with a concrete
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

// Dynamic builds a dynamic client for arbitrary GVRs.
func Dynamic(rc *rest.Config) (dynamic.Interface, error) { return dynamic.NewForConfig(rc) }

// Discovery builds a discovery client (used by auth status / server-version).
func Discovery(rc *rest.Config) (discovery.DiscoveryInterface, error) {
	return discovery.NewDiscoveryClientForConfig(rc)
}

// Clientset builds a typed client-go clientset, used for pod log streaming.
func Clientset(rc *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(rc)
}
