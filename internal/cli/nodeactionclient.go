package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sync/atomic"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// normanNodesPath is the collection path of Rancher's Norman nodes API. Only
// the node action client's two methods use it.
const normanNodesPath = "/v3/nodes"

// NodeActions resolves the memoized engine.NodeActionClient for this
// invocation: the root client's server, credential and transport (its TLS
// trust included), with actionTimeout in place of the root client's timeout.
func (f *clientFactory) NodeActions() (engine.NodeActionClient, error) {
	f.nodeActionOnce.Do(func() {
		root, err := f.Root()
		if err != nil {
			f.nodeActionErr = err
			return
		}
		rc, ok := root.(*rootClient)
		if !ok {
			f.nodeActionErr = clierr.New(clierr.CodeGeneric, "internal error: the node action client could not be built from the root client")
			return
		}
		hc := *rc.hc
		hc.Timeout = actionTimeout
		f.nodeAction = &nodeActionClient{base: rc.base, token: rc.token, login: rc.login, hc: &hc}
	})
	return f.nodeAction, f.nodeActionErr
}

// nodeActionClient implements engine.NodeActionClient over httpx: a GET of a
// node and one POST per action, no retry.
type nodeActionClient struct {
	base  string
	token string
	// login is the command a refused token names as the hint of its AUTH
	// error; the zero value names none.
	login clierr.Command
	hc    *http.Client
}

// normanNodeID is the id Rancher's Norman API gives node nodeID of cluster
// clusterID, escaped for a path segment.
func normanNodeID(clusterID, nodeID string) string {
	return url.PathEscape(clusterID + ":" + nodeID)
}

// newNodeActionRequest builds a request to the Rancher server with the
// bearer token. A request with a body says it is JSON. Norman takes no field
// manager.
func (n *nodeActionClient) newNodeActionRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.base+path, body)
	if err != nil {
		return nil, err
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (n *nodeActionClient) request() *clierr.Request {
	return &clierr.Request{Server: config.DisplayServerURL(n.base), Login: n.login}
}

func (n *nodeActionClient) GetNode(ctx context.Context, clusterID, nodeID string) (engine.NormanNode, error) {
	path := normanNodesPath + "/" + normanNodeID(clusterID, nodeID)
	req, err := n.newNodeActionRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return engine.NormanNode{}, err
	}
	resp, body, err := doBounded(n.hc, req)
	if err != nil {
		if ce := tlstrust.TypedError(err); ce != nil {
			return engine.NormanNode{}, ce
		}
		return engine.NormanNode{}, clierr.New(clierr.CodeGeneric, fmt.Sprintf("GET %s on the Rancher server: %v", path, err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return engine.NormanNode{}, clierr.FromResponse("GET "+path+" on the Rancher server", http.MethodGet, resp.StatusCode, body, n.request())
	}
	node, problem := decodeNormanNode(body)
	if problem != "" {
		return engine.NormanNode{}, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("could not read management node %s of cluster %s from GET %s: %s", nodeID, clusterID, path, problem)).WithRetryable(false)
	}
	return node, nil
}

// decodeNormanNode reads the state and the names of the actions of a Norman
// node. The response must be a JSON object, and its actions, when present, a
// map; otherwise the second result says what is wrong.
func decodeNormanNode(body []byte) (node engine.NormanNode, problem string) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return engine.NormanNode{}, err.Error()
	}
	if obj == nil {
		return engine.NormanNode{}, "the response is not a JSON object"
	}
	node.State, _ = obj["state"].(string)
	if raw, ok := obj["actions"]; ok && raw != nil {
		actions, isMap := raw.(map[string]any)
		if !isMap {
			return engine.NormanNode{}, "actions in the response is not a JSON object"
		}
		for name := range actions {
			node.Actions = append(node.Actions, name)
		}
		slices.Sort(node.Actions)
		node.HasActions = len(node.Actions) > 0
	}
	return node, ""
}

func (n *nodeActionClient) PostNodeAction(ctx context.Context, clusterID, nodeID string, action engine.NodeAction, body []byte) error {
	if !action.Valid() {
		return clierr.New(clierr.CodeGeneric, fmt.Sprintf("internal error: %q is not a node action", string(action)))
	}
	path := normanNodesPath + "/" + normanNodeID(clusterID, nodeID) + "?action=" + string(action)
	// written records that the transport wrote the request, its body
	// included: an error after that may come after the server ran the
	// action, so its outcome is unknown.
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { written.Store(true) },
	})
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := n.newNodeActionRequest(ctx, http.MethodPost, path, reader)
	if err != nil {
		return err
	}
	resp, respBody, err := doBounded(n.hc, req)
	if err != nil {
		if written.Load() {
			return &engine.ActionOutcomeUnknown{Err: err}
		}
		if ce := tlstrust.TypedError(err); ce != nil {
			return ce
		}
		return clierr.New(clierr.CodeGeneric, fmt.Sprintf("POST %s on the Rancher server: %v", path, err))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusInternalServerError || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout:
		return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, resp.StatusCode, respBody), Body: respBody}
	}
	request := n.request()
	return &engine.ActionRefused{
		Err:     clierr.FromResponse("POST "+path+" on the Rancher server", http.MethodPost, resp.StatusCode, respBody, request),
		Body:    respBody,
		Request: request,
	}
}
