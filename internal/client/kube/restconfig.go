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
// endpoint for the effective cluster (default "local").
func RESTConfig(eff config.Effective) (*rest.Config, error) {
	if eff.URL == "" {
		// Config-aware when the inventory is present: after a logout the
		// server entries survive, and every command routed through this
		// factory should answer with the re-login shortcut, not the
		// blank-machine wording (falls back to it when truly blank).
		return nil, config.ErrNotLoggedInFor(eff.KnownServers)
	}
	if eff.Token == "" {
		// eff.URL is resolved (a context or RANCHER_CLI_URL exists) but no token —
		// a stored-but-tokenless context (never logged in, or a lost/expired
		// token) rather than a missing context, so this is an auth gap, not a
		// CONTEXT_NOT_SET one.
		return nil, clierr.New(clierr.CodeAuth, "no token for server").WithHint(config.LoginOrEnvHint)
	}
	cluster := eff.Cluster
	if cluster == "" {
		cluster = "local"
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

// Discovery builds a discovery client (used by auth check / server-version).
func Discovery(rc *rest.Config) (discovery.DiscoveryInterface, error) {
	return discovery.NewDiscoveryClientForConfig(rc)
}

// Clientset builds a typed client-go clientset, used for pod log streaming.
func Clientset(rc *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(rc)
}
