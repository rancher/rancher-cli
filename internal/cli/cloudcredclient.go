package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// normanCloudCredentialsPath is Rancher's Norman collection of cloud
// credentials. Rancher serves no other API that runs its credential checks,
// so this file is the one place the CLI sends a request to it; a source scan
// limits every Norman path to a short list.
const normanCloudCredentialsPath = "/v3/cloudcredentials"

// CloudCredentials resolves the memoized engine.CloudCredentialClient for this
// invocation: the root client's server, credential and transport (its TLS
// trust included), with actionTimeout in place of the root client's timeout.
func (f *clientFactory) CloudCredentials() (engine.CloudCredentialClient, error) {
	f.cloudCredentialOnce.Do(func() {
		root, err := f.Root()
		if err != nil {
			f.cloudCredentialErr = err
			return
		}
		rc, ok := root.(*rootClient)
		if !ok {
			f.cloudCredentialErr = clierr.New(clierr.CodeGeneric, "internal error: the root client is not the one the cloud credential client is built from")
			return
		}
		hc := *rc.hc
		hc.Timeout = actionTimeout
		f.cloudCredential = &cloudCredentialClient{base: rc.base, token: rc.token, login: rc.login, hc: &hc}
	})
	return f.cloudCredential, f.cloudCredentialErr
}

// cloudCredentialClient implements engine.CloudCredentialClient over httpx:
// one request per call, no retry.
type cloudCredentialClient struct {
	base  string
	token string
	// login is the command a refused token names as the hint of its AUTH
	// error; the zero value names none.
	login clierr.Command
	hc    *http.Client
}

func (c *cloudCredentialClient) Create(ctx context.Context, body engine.CloudCredentialBody) (string, error) {
	answer, err := c.send(ctx, http.MethodPost, normanCloudCredentialsPath, body)
	if err != nil {
		return "", err
	}
	// The response may have the decoded config values: only the id is read.
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &created); err != nil || created.ID == "" {
		return "", &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric,
			"POST "+normanCloudCredentialsPath+" on the Rancher server responded with no cloud credential id")}
	}
	return created.ID, nil
}

// send sends one request and classifies the response. A 2xx response returns its
// body. A 500, a 502, a 504 and an error once the request was written are an
// *engine.ActionOutcomeUnknown; any other non-2xx response is an
// *engine.ActionRefused. Each error's Err has the HTTP status and a fixed
// text, never the response's text; the response is only in Body.
func (c *cloudCredentialClient) send(ctx context.Context, method, path string, body engine.CloudCredentialBody) ([]byte, error) {
	raw, err := body.RequestJSON()
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the cloud credential request body cannot be encoded")
	}
	payload := bytes.NewReader(raw)
	// written records that the transport wrote the request, its body
	// included: an error after that may come after the server ran it, so its
	// outcome is unknown.
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { written.Store(true) },
	})
	req, err := newCloudCredentialRequest(ctx, method, c.base+path, payload)
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the cloud credential request cannot be built")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	prefix := method + " " + path + " on the Rancher server"
	resp, respBody, err := doBounded(c.hc, req)
	if err != nil {
		if written.Load() {
			return nil, &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric,
				prefix+": the request was sent, but the connection failed before the response arrived")}
		}
		if ce := tlstrust.TypedError(err); ce != nil {
			return nil, ce
		}
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("%s: the request was not sent", prefix))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return respBody, nil
	case resp.StatusCode == http.StatusInternalServerError || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout:
		return nil, &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(method, resp.StatusCode, nil), Body: respBody}
	}
	request := &clierr.Request{Server: config.DisplayServerURL(c.base), Login: c.login}
	return nil, &engine.ActionRefused{
		Err:     clierr.FromResponse(prefix, method, resp.StatusCode, nil, request),
		Body:    respBody,
		Request: request,
	}
}

// newCloudCredentialRequest builds one Norman request. It sends no field
// manager: Norman is not a Kubernetes API.
func newCloudCredentialRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, rawURL, body)
}
