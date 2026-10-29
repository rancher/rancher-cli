package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/config"
)

// installIDPath is the Rancher setting that holds the server's install id,
// read through the root API of the server, never through a cluster proxy.
const installIDPath = "/v1/management.cattle.io.settings/install-uuid"

// settingBody is the part of a Rancher Setting the install id request reads.
type settingBody struct {
	Value string `json:"value"`
}

// InstallIDFetch returns the function that asks the server at base for its
// install id with token, through client. The function holds the token in
// memory only. A 2xx body that does not decode, has an empty value, or is
// longer than httpx.FactsBodyLimit returns no value with the 2xx status.
// Redirects follow the client's policy.
func InstallIDFetch(client *http.Client, base, token string) httpx.InstallIDFunc {
	return func(ctx context.Context) (string, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+installIDPath, nil)
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, body, tooLong, err := httpx.DoLimited(client, req, httpx.FactsBodyLimit)
		if err != nil {
			return "", 0, err
		}
		if tooLong || resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", resp.StatusCode, nil
		}
		var s settingBody
		if json.Unmarshal(body, &s) != nil {
			return "", resp.StatusCode, nil
		}
		return s.Value, resp.StatusCode, nil
	}
}

// InstallIDFetchFor builds the client and the install id request for a
// command that holds a server URL, a CA (a PEM or a path, as --cacert takes
// it) and a token as text. It returns the scheme and host the request targets
// and the request function. The client verifies the server against the given
// CA, or the operating system roots when there is none; it is never
// insecure. It returns an error when the CA cannot be read or the client
// cannot be built, and the caller then registers nothing.
func InstallIDFetchFor(serverURL, ca, token string) (base string, fetch httpx.InstallIDFunc, err error) {
	caPEM, err := config.ResolveCA(ca)
	if err != nil {
		return "", nil, err
	}
	client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: httpx.DefaultTimeout})
	if err != nil {
		return "", nil, err
	}
	base = kube.BaseURL(serverURL)
	return base, InstallIDFetch(client, base, token), nil
}
