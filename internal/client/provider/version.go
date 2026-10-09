package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// rancherVersionResponse is the minimal wire shape of GET /rancherversion.
// RancherPrime is serialized as the STRING "true"/"false" (not a JSON
// boolean) - decoded via parseEdition, never unmarshaled as bool.
type rancherVersionResponse struct {
	Version      string `json:"Version"`
	RancherPrime string `json:"RancherPrime"`
}

// Edition is the tri-state decode of /rancherversion's RancherPrime field:
// the ONLY signal that says what the running server is (never the Feature
// resource, which classifies a feature, not the server).
type Edition int

const (
	// EditionUnknown covers every case that is not an affirmative "true" or
	// "false": the field absent, malformed, or carrying an unrecognized
	// value. Unknown is not community - callers must not treat it as false.
	EditionUnknown Edition = iota
	EditionCommunity
	EditionPrime
)

// parseEdition decodes RancherPrime's wire string into an Edition, case- and
// whitespace-insensitive. Anything other than an exact "true"/"false" decode
// is EditionUnknown.
func parseEdition(raw string) Edition {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return EditionPrime
	case "false":
		return EditionCommunity
	default:
		return EditionUnknown
	}
}

// ServerInfo is the shared shape of a /rancherversion read: the version
// string and the decoded server edition.
type ServerInfo struct {
	Version string
	Edition Edition
}

// fetchVersionResponse performs the shared unauthenticated GET
// /rancherversion HTTP round trip and JSON decode. RancherVersion and
// ServerInfo both build on it, so a single call site owns the request
// construction, the CA resolution, and the malformed-body/non-200
// classification.
func fetchVersionResponse(ctx context.Context, eff config.Effective) (rancherVersionResponse, error) {
	caPEM, err := config.ResolveCA(eff.CA)
	if err != nil {
		return rancherVersionResponse{}, err
	}
	client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: requestTimeout})
	if err != nil {
		return rancherVersionResponse{}, err
	}
	boundedCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	base := strings.TrimRight(eff.URL, "/")
	req, err := http.NewRequestWithContext(boundedCtx, http.MethodGet, base+"/rancherversion", nil)
	if err != nil {
		return rancherVersionResponse{}, err
	}
	resp, body, err := httpx.Do(client, req)
	if err != nil {
		return rancherVersionResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return rancherVersionResponse{}, fmt.Errorf("/rancherversion: %w", clierr.ResponseError(http.MethodGet, resp.StatusCode, body))
	}
	var r rancherVersionResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return rancherVersionResponse{}, fmt.Errorf("/rancherversion: malformed response: %w", err)
	}
	return r, nil
}

// RancherVersion reads the server's ldflags-stamped version from the
// unauthenticated GET /rancherversion endpoint for `auth status`'s
// minimum-version check. Unlike the previous server-version Setting, this
// endpoint requires no RBAC permissions and reflects the binary's actual
// stamped version rather than an editable config object - it is equivalent to
// checking the running binary, not a user-editable value. Any error
// (connectivity, non-200 status, unrecognized response body) is returned as-is;
// the caller folds it into the check's warn-only "cannot determine" posture
// rather than failing the check.
func RancherVersion(ctx context.Context, eff config.Effective) (string, error) {
	r, err := fetchVersionResponse(ctx, eff)
	if err != nil {
		return "", err
	}
	if r.Version == "" {
		return "", fmt.Errorf("/rancherversion: response carried no Version field")
	}
	return r.Version, nil
}

// GetServerInfo reads the server's version and edition from the same
// unauthenticated GET /rancherversion endpoint RancherVersion uses - the
// capability detector's edition signal. Any error is returned as-is; the
// caller folds it into the detector's unknown outcome rather than failing.
func GetServerInfo(ctx context.Context, eff config.Effective) (ServerInfo, error) {
	r, err := fetchVersionResponse(ctx, eff)
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Version: r.Version, Edition: parseEdition(r.RancherPrime)}, nil
}
