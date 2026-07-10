package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/tidwall/gjson"
)

// mintTokensPath and v3TokensPath are reached ONLY through the Rancher proxy
// path on the LOCAL cluster's apiserver — both ext.cattle.io and
// management.cattle.io/v3 are aggregated/CRD APIs, never served at the
// server root, so a bare /apis/... 404s on every Rancher.
const (
	mintTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"
	v3TokensPath   = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"
)

// errExtGroupAbsent signals a 404 on the mint call itself — the aggregated
// ext.cattle.io API isn't served (an older Rancher), not that some other
// resource is missing.
var errExtGroupAbsent = errors.New("ext.cattle.io not served")

// resolveProviderLoginToken runs the identity-provider dance to mint a
// transient Rancher session token S (memory only), asks the ext Token store
// to mint a purpose-made derived token D (bearer S), and returns D for
// storage exactly where the pasted-token path stores it. S is revoked
// best-effort once D exists. On an older Rancher with no ext.cattle.io
// group, the mint 404s and this falls back to returning S itself (legacy
// semantics), with a notice explaining why.
func resolveProviderLoginToken(ctx context.Context, server string, in runtime.Input, p runtime.Prompter) (string, error) {
	// Login has no --skip-verify flag; CA comes from --cacert only.
	caPEM, err := config.ResolveCA(in.String("cacert"))
	if err != nil {
		return "", clierr.New(clierr.CodeValidation, "reading --cacert: "+err.Error())
	}
	client, err := httpx.New(httpx.Options{CACert: caPEM})
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
	}, p)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(sessionToken.Token)
	if s == "" {
		return "", clierr.New(clierr.CodeAuth, "authentication returned an empty token").
			WithHint("check the auth provider and credentials")
	}

	base := kube.BaseURL(server)
	d, err := mintDerivedToken(ctx, client, base, s, ttlMs)
	if err != nil {
		if errors.Is(err, errExtGroupAbsent) {
			note := "this Rancher does not serve ext.cattle.io (requires Rancher >= 2.12); storing the session token directly"
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

// mintDerivedToken asks the ext Token store to mint a derived token (bearer
// sessionToken) and returns its status.bearerToken. errExtGroupAbsent
// signals the group-level 404 that means an older Rancher. ttlMs <= 0 omits
// spec.ttl entirely (server default), matching create.go/kubeconfig.go's
// `if ttl > 0` idiom — one omit-when-unset convention across all three
// ext.cattle.io create call sites, rather than this one alone sending an
// explicit ttl: 0.
func mintDerivedToken(ctx context.Context, client *http.Client, base, sessionToken string, ttlMs int64) (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	spec := map[string]any{
		"description": fmt.Sprintf("rancher-cli login @ %s", host),
	}
	if ttlMs > 0 {
		spec["ttl"] = ttlMs
	}
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return "", fmt.Errorf("building mint request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+mintTokensPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building mint request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, respBody, err := httpx.Do(client, req)
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, "minting derived token: "+err.Error())
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return "", errExtGroupAbsent
	case http.StatusOK, http.StatusCreated:
		bearer := gjson.GetBytes(respBody, "status.bearerToken").String()
		if bearer == "" {
			return "", clierr.New(clierr.CodeGeneric, "mint response carried no bearer token")
		}
		return bearer, nil
	default:
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("minting derived token: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
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
		url = base + mintTokensPath + "/" + name
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
