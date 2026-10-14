package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// resolveProviderLoginToken runs the identity-provider dance and returns the
// credential to store, exactly where the pasted-token path stores one. The
// token package owns the transient session token from its first creation:
// see AcquireLoginCredential for the ownership rules and for what the
// returned error carries when a token may have been left behind.
//
// caPEM is the CA snapshot Execute already resolved (explicit --cacert, else
// the entry the naming algorithm would select's stored CA) - resolved once,
// before any request in the login operation, so this never rediscovers
// trust independently by re-reading a path or ignoring a stored CA the way
// a bare config.ResolveCA(in.String("cacert")) would on a same-server
// re-login with no --cacert flag.
//
// idpClient dials the identity provider; production passes a client that
// trusts only the operating system store, so caPEM never extends past the
// Rancher origin.
func resolveProviderLoginToken(ctx context.Context, server, caPEM string, idpClient *http.Client, in runtime.Input, p runtime.Prompter) (token.LoginCredential, error) {
	serverURL, err := url.Parse(server)
	if err != nil {
		return token.LoginCredential{}, clierr.New(clierr.CodeValidation, "invalid Rancher server URL: "+err.Error())
	}
	serverOrigin, err := httpx.OriginOf(serverURL)
	if err != nil {
		return token.LoginCredential{}, clierr.New(clierr.CodeValidation, "invalid Rancher server URL: "+err.Error())
	}

	client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: httpx.DefaultTimeout})
	if err != nil {
		return token.LoginCredential{}, err
	}

	ttl, err := extres.ParseTTL("ttl", in.String("ttl"))
	if err != nil {
		return token.LoginCredential{}, err
	}
	ttlMs, err := extres.TTLMilliseconds("ttl", ttl, in.String("ttl"))
	if err != nil {
		return token.LoginCredential{}, err
	}

	cred, err := token.AcquireLoginCredential(ctx, client, token.LoginRequest{
		Server:       server,
		AuthProvider: in.String("auth-provider"),
		AuthFlow:     in.String("auth-flow"),
		IdPClient:    idpClient,
		ResponseType: "json",
	}, ttlMs, p)
	if err != nil {
		// A trust failure is reclassified in place: the cleanup evidence the
		// acquire gathered must survive the reclassification, because it is
		// the only record of a token that may have been left behind.
		if ae, ok := errors.AsType[*token.AcquireError](err); ok {
			if ce := tlstrust.ClassifyForOrigin(ae.Err, serverOrigin, time.Now(), tlstrust.Remedy{}); ce != nil {
				return token.LoginCredential{}, ae.WithPrimary(ce)
			}
			return token.LoginCredential{}, ae
		}
		if ce := tlstrust.ClassifyForOrigin(err, serverOrigin, time.Now(), tlstrust.Remedy{}); ce != nil {
			return token.LoginCredential{}, ce
		}
		return token.LoginCredential{}, err
	}
	return cred, nil
}
