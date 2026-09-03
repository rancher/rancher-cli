package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// v3TokensPath is reached ONLY through the Rancher proxy path on the LOCAL
// cluster's apiserver — management.cattle.io/v3 is an aggregated/CRD API,
// never served at the server root, so a bare /apis/... 404s on every
// Rancher.
const v3TokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"

// resolveProviderLoginToken runs the identity-provider dance to mint a
// transient Rancher session token S (memory only), asks the ext Token store
// to mint a purpose-made derived token D (bearer S), and returns D for
// storage exactly where the pasted-token path stores it. S is revoked
// best-effort once D exists. On an older Rancher with no ext.cattle.io
// group, the mint 404s and this falls back to returning S itself (legacy
// semantics), with a notice explaining why.
//
// caPEM is the CA snapshot Execute already resolved (explicit --cacert, else
// the entry the naming algorithm would select's stored CA) — resolved once,
// before any request in the login operation, so this never rediscovers
// trust independently by re-reading a path or ignoring a stored CA the way
// a bare config.ResolveCA(in.String("cacert")) would on a same-server
// re-login with no --cacert flag.
//
// idpClient dials the identity provider; production passes a client that
// trusts only the operating system store, so caPEM never extends past the
// Rancher origin.
func resolveProviderLoginToken(ctx context.Context, server, caPEM string, idpClient *http.Client, in runtime.Input, p runtime.Prompter) (string, error) {
	serverURL, err := url.Parse(server)
	if err != nil {
		return "", clierr.New(clierr.CodeValidation, "invalid Rancher server URL: "+err.Error())
	}
	serverOrigin, err := httpx.OriginOf(serverURL)
	if err != nil {
		return "", clierr.New(clierr.CodeValidation, "invalid Rancher server URL: "+err.Error())
	}

	client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: httpx.DefaultTimeout})
	if err != nil {
		return "", err
	}

	ttl, err := extres.ParseTTL(in.String("ttl"))
	if err != nil {
		return "", err
	}
	ttlMs, err := extres.TTLMilliseconds(ttl, in.String("ttl"))
	if err != nil {
		return "", err
	}

	sessionToken, err := token.Login(ctx, client, token.LoginRequest{
		Server:       server,
		AuthProvider: in.String("auth-provider"),
		AuthFlow:     in.String("auth-flow"),
		IdPClient:    idpClient,
	}, p)
	if err != nil {
		if ce := tlstrust.ClassifyForOrigin(err, serverOrigin, time.Now()); ce != nil {
			return "", ce
		}
		return "", err
	}
	s := strings.TrimSpace(sessionToken.Token)
	if s == "" {
		return "", clierr.New(clierr.CodeAuth, "authentication returned an empty token; check the auth provider and credentials")
	}

	base := kube.BaseURL(server)
	// auth login has no cluster-scoping concept of its own; the stored
	// login token this mints is later scoped per-cluster by get-token's own
	// derived-token call.
	host, herr := os.Hostname()
	if herr != nil {
		host = "unknown"
	}
	dr, err := token.MintDerivedToken(ctx, client, base, s, "", ttlMs, "rancher-cli login @ "+host)
	d := dr.Bearer
	if err != nil {
		if errors.Is(err, token.ErrExtGroupAbsent) {
			note := fmt.Sprintf("this Rancher does not serve ext.cattle.io (requires Rancher >= %s, the supported minimum); storing the session token directly", config.MinRancherVersion)
			if sessionToken.ExpiresAt != "" {
				note += " (expires " + sessionToken.ExpiresAt + ")"
			}
			p.Notice(note)
			return s, nil
		}
		return "", err
	}

	revokeSessionToken(ctx, client, base, s, d, p)
	return d, nil
}

// revokeSessionToken best-effort deletes the transient session token S (bearer
// D, the just-minted derived token): ext-shaped ids ("ext/<name>:...") go
// through the ext Token store; anything else is assumed v3-shaped
// ("token-xxx:...") and goes through the v3 Token CRD path — still no
// Norman. A revoke failure is never an error, only a notice.
func revokeSessionToken(ctx context.Context, client *http.Client, base, sessionToken, bearer string, p runtime.Prompter) {
	id, _, _ := strings.Cut(sessionToken, ":")
	url := base + v3TokensPath + "/" + id
	if name, ok := strings.CutPrefix(id, "ext/"); ok {
		url = base + token.MintTokensPath + "/" + name
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		p.Notice(fmt.Sprintf("could not revoke the transient session token %s: %v", id, err))
		return
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, _, err := httpx.Do(client, req)
	if err != nil {
		p.Notice(fmt.Sprintf("could not revoke the transient session token %s: %v", id, err))
		return
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
	default:
		p.Notice(fmt.Sprintf("could not revoke the transient session token %s: %d %s", id, resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
}
