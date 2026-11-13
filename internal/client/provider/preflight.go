package provider

import (
	"context"
	"errors"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
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
	ctx = kube.WithAnswers(ctx)
	rc, err := localRESTConfig(eff)
	if err != nil {
		return "", err
	}
	disc, err := kube.Discovery(discoveryConfig(rc))
	if err != nil {
		return "", clierr.From(err)
	}
	ver, err := serverVersion(ctx, disc)
	if err != nil {
		return "", clierr.WrapUnlessCanceled(err, func(err error) error {
			return mapPreflightErr(ctx, http.MethodGet, eff, err)
		})
	}
	if ver == nil {
		return "", clierr.New(clierr.CodeGeneric, "server returned no version info")
	}
	return ver.GitVersion, nil
}

// localRESTConfig is the rest.Config of the local cluster's path for eff,
// with the error mapping auth status and auth login share: RESTConfig's
// typed errors (NOT_LOGGED_IN, AUTH) pass through unchanged, so every
// command reports the same code for the same state; any other is AUTH,
// with the entry's login as the hint when the entry is named.
func localRESTConfig(eff config.Effective) (*rest.Config, error) {
	rc, err := kube.RESTConfig(eff, engine.LocalCluster)
	if err == nil {
		return rc, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return nil, ce
	}
	out := clierr.New(clierr.CodeAuth, err.Error())
	if eff.ServerName != "" {
		if login, ok := config.LoginHintFor(eff.ServerName); ok {
			out = out.WithHintCommand(login)
		}
	}
	return nil, out
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

// mapPreflightErr distinguishes an answer from no answer. An answer goes
// through the status table as a call that names an API, so a 404 says to
// check the server URL; a call that ran out of time after an answer names
// that answer; a structured error produced below the status layer (the
// redirect refusal) is returned as is; no answer at all is a retryable
// connectivity failure. method is the HTTP method of the call.
func mapPreflightErr(ctx context.Context, method string, eff config.Effective, err error) error {
	q := &clierr.Request{Endpoint: true, Server: config.DisplayServerURL(eff.URL)}
	if ce := clierr.FromStatus(err, q); ce != nil {
		return ce
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if ce := answeredThenTimedOut(ctx, method, requestTimeout, q); ce != nil {
			return ce
		}
	}
	return clierr.Retryable(clierr.CodeGeneric, "cannot reach server: "+err.Error()+"; check the server URL and CA")
}
