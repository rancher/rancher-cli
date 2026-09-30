package provider

import (
	"context"
	"errors"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"k8s.io/client-go/rest"
)

// Preflight verifies connectivity and credentials by asking the server for
// its version. It owns rest.Config construction and error mapping so
// commands never reach into client/kube directly: auth failures map
// to AUTH, everything else stays GENERIC/retryable instead of being
// mislabeled as an auth problem.
//
// The version request carries ctx, so an interrupt ends it at once; a slow
// server is otherwise bounded by discoveryConfig's request timeout. The
// caller's own cancellation passes through unmapped, never as a retryable
// connectivity failure.
func Preflight(ctx context.Context, eff config.Effective) (string, error) {
	rc, err := kube.RESTConfig(eff, "local")
	if err != nil {
		// RESTConfig already returns a typed *clierr.Error (NOT_LOGGED_IN,
		// AUTH) with the right code and hint for its own choke points - pass
		// it through unchanged so `auth status` reports the same code/exit as
		// every other command for the identical state, instead of relabeling
		// everything AUTH here.
		if ce, ok := errors.AsType[*clierr.Error](err); ok {
			return "", ce
		}
		hint := ""
		if eff.ServerName != "" {
			hint = config.LoginHintFor(eff.ServerName)
		}
		return "", clierr.New(clierr.CodeAuth, err.Error()).WithHint(hint)
	}
	disc, err := kube.Discovery(discoveryConfig(rc))
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, err.Error())
	}
	ver, err := serverVersion(ctx, disc)
	if err != nil {
		return "", clierr.WrapUnlessCanceled(err, mapPreflightErr)
	}
	if ver == nil {
		return "", clierr.New(clierr.CodeGeneric, "server returned no version info")
	}
	return ver.GitVersion, nil
}

// discoveryConfig returns a copy of rc bounded by requestTimeout, the same
// bound every other request has: without it a discovery request against an
// unreachable server whose caller never cancels hangs indefinitely. The
// input config is not mutated.
func discoveryConfig(rc *rest.Config) *rest.Config {
	c := *rc
	c.Timeout = requestTimeout
	return &c
}

// mapPreflightErr distinguishes credential rejection from unreachability.
func mapPreflightErr(err error) error {
	// A structured error produced below the status layer (the redirect
	// refusal) is deliberate and not retryable; it must not be relabeled
	// as a connectivity failure whose remediation (check the CA) cannot
	// apply.
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return ce
	}
	if mapped := mapErr(err); mapped != nil {
		if ce, ok := errors.AsType[*clierr.Error](mapped); ok && (ce.Code == clierr.CodeAuth || ce.Code == clierr.CodeRBAC) {
			return ce
		}
	}
	return clierr.Retryable(clierr.CodeGeneric, "cannot reach server: "+err.Error()+"; check the server URL and CA")
}
