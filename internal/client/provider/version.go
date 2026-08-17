package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
)

// rancherVersionResponse is the minimal wire shape of GET /rancherversion.
type rancherVersionResponse struct {
	Version string `json:"Version"`
}

// RancherVersion reads the server's ldflags-stamped version from the
// unauthenticated GET /rancherversion endpoint for `auth status`'s PS-8 floor
// check. Unlike the previous server-version Setting, this endpoint requires no
// RBAC permissions and reflects the binary's actual stamped version rather than
// an editable config object — it is equivalent to checking the running binary,
// not a user-editable value. Any error (connectivity, non-200 status,
// unrecognized response body) is returned as-is; the caller folds it into
// PS-8's warn-only "cannot determine" posture rather than failing the check.
func RancherVersion(ctx context.Context, eff config.Effective) (string, error) {
	caPEM, err := config.ResolveCA(eff.CA)
	if err != nil {
		return "", err
	}
	client, err := httpx.New(httpx.Options{CACert: caPEM})
	if err != nil {
		return "", err
	}
	boundedCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	base := strings.TrimRight(eff.URL, "/")
	req, err := http.NewRequestWithContext(boundedCtx, http.MethodGet, base+"/rancherversion", nil)
	if err != nil {
		return "", err
	}
	resp, body, err := httpx.Do(client, req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/rancherversion: unexpected status %d", resp.StatusCode)
	}
	var r rancherVersionResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("/rancherversion: malformed response: %w", err)
	}
	if r.Version == "" {
		return "", fmt.Errorf("/rancherversion: response carried no Version field")
	}
	return r.Version, nil
}
