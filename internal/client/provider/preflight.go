package provider

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// Preflight verifies connectivity and credentials by asking the server for
// its version. It owns rest.Config construction and error mapping so
// commands never reach into client/kube directly (L-2): auth failures map
// to AUTH, everything else stays GENERIC/retryable instead of being
// mislabeled as an auth problem.
func Preflight(eff config.Effective) (string, error) {
	rc, err := kube.RESTConfig(eff)
	if err != nil {
		return "", clierr.New(clierr.CodeAuth, err.Error()).WithHint(config.LoginHint)
	}
	disc, err := kube.Discovery(rc)
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, err.Error())
	}
	ver, err := disc.ServerVersion()
	if err != nil {
		return "", mapPreflightErr(err)
	}
	if ver == nil {
		return "", clierr.New(clierr.CodeGeneric, "server returned no version info")
	}
	return ver.GitVersion, nil
}

// mapPreflightErr distinguishes credential rejection from unreachability.
func mapPreflightErr(err error) error {
	if mapped := mapErr(err); mapped != nil {
		var ce *clierr.Error
		if errors.As(mapped, &ce) && (ce.Code == clierr.CodeAuth || ce.Code == clierr.CodeRBAC) {
			return ce
		}
	}
	return clierr.New(clierr.CodeGeneric, "cannot reach server: "+err.Error()).
		WithHint("check the server URL and CA").WithRetryable(true)
}
