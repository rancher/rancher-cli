package testrancher_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extTokensPath = token.ExtTokensPath
const v3TokensPath = token.V3TokensPath

// recordingHandler captures every request it receives, for asserting a
// create-discrimination or unknown-name case actually fell through instead of
// being served by the facade itself.
type recordingHandler struct {
	requests []*http.Request
	bodies   [][]byte
}

func (r *recordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.requests = append(r.requests, req)
	r.bodies = append(r.bodies, body)
	w.WriteHeader(http.StatusTeapot) // distinctive: never a code the facade itself would return
}

func newFacade(t *testing.T, opts testrancher.Options, fall *recordingHandler) (*testrancher.Facade, *httptest.Server) {
	t.Helper()
	if opts.Fallthrough == nil {
		opts.Fallthrough = fall
	}
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, bearer string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func postJSON(t *testing.T, client *http.Client, url, bearer string, body any) map[string]any {
	t.Helper()
	resp, respBody := doJSON(t, client, http.MethodPost, url, body, bearer)
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300, "expected 2xx, got %d", resp.StatusCode)
	var result map[string]any
	require.NoError(t, json.Unmarshal(respBody, &result))
	return result
}

func statusOf(t *testing.T, client *http.Client, method, url, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func statusOfBody(t *testing.T, client *http.Client, method, url, bearer string, body any) int {
	t.Helper()
	resp, _ := doJSON(t, client, method, url, body, bearer)
	return resp.StatusCode
}

func TestAuthProvidersListsLocalAndOptionallyAzureAD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		enableIdP bool
		wantTypes []string
	}{
		{"idp disabled: local only", false, []string{"localProvider"}},
		{"idp enabled: local first, then azureAD", true, []string{"localProvider", "azureADProvider"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newFacade(t, testrancher.Options{EnableIdP: tt.enableIdP}, &recordingHandler{})
			resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1-public/authproviders", nil, "")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var decoded struct {
				Data []struct {
					Type          string `json:"type"`
					ClientID      string `json:"clientId"`
					AuthURL       string `json:"authUrl"`
					DeviceAuthURL string `json:"deviceAuthUrl"`
					TokenURL      string `json:"tokenUrl"`
				}
			}
			require.NoError(t, json.Unmarshal(body, &decoded))
			require.Len(t, decoded.Data, len(tt.wantTypes))
			for i, want := range tt.wantTypes {
				assert.Equal(t, want, decoded.Data[i].Type)
			}
			if tt.enableIdP {
				az := decoded.Data[1]
				assert.NotEmpty(t, az.ClientID)
				assert.Contains(t, az.DeviceAuthURL, "/idp/devicecode")
				assert.Contains(t, az.TokenURL, "/idp/token")
			}

			resp2, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v3-public/anything", nil, "")
			assert.Equal(t, http.StatusNotFound, resp2.StatusCode, "/v3-public must 404 like a modern Rancher")
		})
	}
}

func TestLocalLoginHappyAndReject(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{}, &recordingHandler{})

	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass"}, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var tok struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Status     struct {
			BearerToken string `json:"bearerToken"`
			ExpiresAt   string `json:"expiresAt"`
		}
	}
	require.NoError(t, json.Unmarshal(body, &tok))
	assert.Equal(t, "ext.cattle.io/v1", tok.APIVersion)
	assert.Equal(t, "Token", tok.Kind)
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", tok.Status.BearerToken)
	assert.NotEmpty(t, tok.Status.ExpiresAt)
	_, err := time.Parse(time.RFC3339, tok.Status.ExpiresAt)
	require.NoError(t, err)

	resp2, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "localProvider", "username": "e2e-user", "password": "WRONG"}, "")
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestAzureADLoginHappyAndReject(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{EnableIdP: true, IdPPendingPolls: 0}, &recordingHandler{})

	// Drive the device-code stub directly to obtain a stub-issued id_token.
	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/idp/devicecode", nil, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var dc struct {
		DeviceCode string `json:"device_code"`
	}
	require.NoError(t, json.Unmarshal(body, &dc))

	var tokenJSON struct {
		Error   string `json:"error"`
		IDToken string `json:"id_token"`
	}
	for tokenJSON.IDToken == "" {
		tokResp, tokBody := postForm(t, srv.Client(), srv.URL+"/idp/token", map[string]string{"device_code": dc.DeviceCode})
		require.Equal(t, http.StatusOK, tokResp.StatusCode)
		tokenJSON = struct {
			Error   string `json:"error"`
			IDToken string `json:"id_token"`
		}{}
		require.NoError(t, json.Unmarshal(tokBody, &tokenJSON))
	}

	resp2, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "azureADProvider", "responseType": "kubeconfig", "id_token": tokenJSON.IDToken}, "")
	require.Equal(t, http.StatusCreated, resp2.StatusCode)
	var loginTok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body2, &loginTok))
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", loginTok.Status.BearerToken)

	resp3, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "azureADProvider", "responseType": "kubeconfig", "id_token": "not-issued-by-stub"}, "")
	assert.Equal(t, http.StatusUnauthorized, resp3.StatusCode)
}

func postForm(t *testing.T, client *http.Client, url string, form map[string]string) (*http.Response, []byte) {
	t.Helper()
	v := make([]string, 0, len(form))
	for k, val := range form {
		v = append(v, k+"="+val)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(strings.Join(v, "&")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestDerivedCreateDiscriminatesCRCreateFromImperativeCreate(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	// A full CR (metadata.name present) must fall through untouched.
	resp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"apiVersion": "ext.cattle.io/v1", "kind": "Token", "metadata": map[string]string{"name": "cr-token"}},
		"e2e:secret")
	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "CR-shaped body must reach the recording fallthrough")
	require.Len(t, fall.requests, 1)

	// A bare {"spec": ...} body is a derived token create and must not reach fallthrough.
	resp2, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{"description": "d"}}, "e2e:secret")
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)
	assert.Len(t, fall.requests, 1, "the derived token create branch must not fall through")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body2, &tok))
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", tok.Status.BearerToken)
}

func TestDerivedCreateTTLHonoredAndDefaulted(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	before := time.Now()
	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{"ttl": 60_000}}, "e2e:secret")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var tok struct {
		Status struct {
			ExpiresAt string `json:"expiresAt"`
		}
	}
	require.NoError(t, json.Unmarshal(body, &tok))
	expiresAt, err := time.Parse(time.RFC3339, tok.Status.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(60*time.Second), expiresAt, 5*time.Second, "spec.ttl must be honored")

	before2 := time.Now()
	resp2, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{}}, "e2e:secret")
	require.Equal(t, http.StatusCreated, resp2.StatusCode)
	var tok2 struct {
		Status struct {
			ExpiresAt string `json:"expiresAt"`
		}
	}
	require.NoError(t, json.Unmarshal(body2, &tok2))
	expiresAt2, err := time.Parse(time.RFC3339, tok2.Status.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, before2.Add(15*time.Minute), expiresAt2, 5*time.Second, "absent spec.ttl must default to +15m")
}

func TestDerivedCreateClusterNameRecorded(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{"clusterName": "c-test"}}, "e2e:secret")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body, &tok))
	name := "fac-token-1"

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/"+name, nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var cr struct {
		Spec struct {
			ClusterName string `json:"clusterName"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody, &cr))
	assert.Equal(t, "c-test", cr.Spec.ClusterName)
}

func TestDeterministicNameAndSecretSequenceIncludingProviderLoginTwoSlots(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	// Slot 1: two independent creates in sequence are strictly incrementing.
	_, body1 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	_, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var t1, t2 struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body1, &t1))
	require.NoError(t, json.Unmarshal(body2, &t2))
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", t1.Status.BearerToken)
	assert.Equal(t, "ext/fac-token-2:fac-secret-2", t2.Status.BearerToken)
}

func TestProviderLoginConsumesTwoCounterSlots(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	// Reproduces resolveProviderLoginToken (login_ext.go): login creates session
	// token S (slot 1), CreateDerivedToken creates derived token D (slot 2) using S
	// as bearer, then S is revoked. The stored credential is D = fac-token-2.
	_, loginBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass"}, "")
	var s struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(loginBody, &s))
	require.Equal(t, "ext/fac-token-1:fac-secret-1", s.Status.BearerToken)

	createResp, createBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{}}, s.Status.BearerToken)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	var d struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(createBody, &d))
	assert.Equal(t, "ext/fac-token-2:fac-secret-2", d.Status.BearerToken, "the stored credential after a provider login is fac-token-2")

	revokeResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", nil, s.Status.BearerToken)
	assert.Equal(t, http.StatusOK, revokeResp.StatusCode)
}

func TestNamedGetServedUnknownFallsThroughAndListMerges(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	_, createBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(createBody, &tok))
	require.True(t, f.ValidBearer(tok.Status.BearerToken))

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var cr struct {
		Metadata struct {
			Name            string `json:"name"`
			ResourceVersion string `json:"resourceVersion"`
		}
		Status struct {
			BearerToken string `json:"bearerToken"`
			Value       string `json:"value"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody, &cr))
	assert.Equal(t, "fac-token-1", cr.Metadata.Name)
	assert.NotEmpty(t, cr.Metadata.ResourceVersion)
	assert.Empty(t, cr.Status.BearerToken, "bearer must never be served on GET")
	assert.Empty(t, cr.Status.Value, "value must never be served on GET")

	getResp2, getBody2 := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp2.StatusCode)
	var cr2 struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody2, &cr2))
	assert.Equal(t, cr.Metadata.ResourceVersion, cr2.Metadata.ResourceVersion, "resourceVersion must be stable across reads")

	unknownResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/no-such-token", nil, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, unknownResp.StatusCode, "unknown-name GET must fall through")

	before := len(fall.requests)
	listResp, listBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath, nil, "e2e:secret")
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	assert.Len(t, fall.requests, before+1, "LIST asks the fallthrough for its own items first")
	var list struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(listBody, &list))
	assert.Equal(t, "TokenList", list.Kind)
	require.Len(t, list.Items, 1, "the fallthrough served no items, so only the facade record is listed")
	assert.Equal(t, "fac-token-1", list.Items[0].Metadata.Name)
}

func TestDeleteDryRunRealDeleteReDeleteFallsThrough(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	_, createBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(createBody, &tok))

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var cr struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody, &cr))

	dryRunBody := map[string]any{"dryRun": []string{"All"}, "preconditions": map[string]string{"resourceVersion": cr.Metadata.ResourceVersion}}
	dryResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", dryRunBody, "e2e:secret")
	assert.Equal(t, http.StatusOK, dryResp.StatusCode)
	assert.True(t, f.ValidBearer(tok.Status.BearerToken), "dry-run delete must not revoke")

	// Wrong precondition on the real delete must be rejected, still not revoking.
	badPrecondition := map[string]any{"preconditions": map[string]string{"resourceVersion": "stale"}}
	badResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", badPrecondition, "e2e:secret")
	assert.Equal(t, http.StatusConflict, badResp.StatusCode)
	assert.True(t, f.ValidBearer(tok.Status.BearerToken))

	realBody := map[string]any{"preconditions": map[string]string{"resourceVersion": cr.Metadata.ResourceVersion}}
	realResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", realBody, "e2e:secret")
	assert.Equal(t, http.StatusOK, realResp.StatusCode)
	assert.False(t, f.ValidBearer(tok.Status.BearerToken), "the real delete must revoke")

	reDeleteResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, reDeleteResp.StatusCode, "re-delete of an already-gone name must fall through")
	require.Len(t, fall.requests, 1)
}

func TestLegacyModeDerivedCreateNotFoundCRFallthroughIntact(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{Legacy: true, SeedToken: "e2e:secret"}, fall)

	createResp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	assert.Equal(t, http.StatusNotFound, createResp.StatusCode, "legacy mode 404s the derived token create route")

	crResp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"metadata": map[string]string{"name": "cr-token"}}, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, crResp.StatusCode, "CR-create fallthrough stays intact under legacy mode")
	require.Len(t, fall.requests, 1)
}

func TestDerivedCreateRejectsUnauthorizedBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	resp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "garbage:garbage")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestNamedGetAndDeleteRejectUnauthorizedBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	_, createBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(createBody, &tok))

	for _, bearer := range []string{"", "garbage:garbage"} {
		getResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, bearer)
		assert.Equal(t, http.StatusUnauthorized, getResp.StatusCode, "GET with bearer %q must 401", bearer)

		delResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", nil, bearer)
		assert.Equal(t, http.StatusUnauthorized, delResp.StatusCode, "DELETE with bearer %q must 401", bearer)
	}

	// The token is still valid after both rejections - neither failed call revoked it.
	getResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, tok.Status.BearerToken)
	assert.Equal(t, http.StatusOK, getResp.StatusCode)
}

func TestDeviceCodePendingThenTokenIncludesIDToken(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{EnableIdP: true, IdPPendingPolls: 2}, &recordingHandler{})

	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/idp/devicecode", nil, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var dc struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
	}
	require.NoError(t, json.Unmarshal(body, &dc))
	assert.NotEmpty(t, dc.DeviceCode)
	assert.NotEmpty(t, dc.UserCode)
	assert.Equal(t, 1, dc.Interval)

	for i := range 2 {
		pollResp, pollBody := postForm(t, srv.Client(), srv.URL+"/idp/token", map[string]string{"device_code": dc.DeviceCode})
		require.Equal(t, http.StatusOK, pollResp.StatusCode)
		var pending struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(pollBody, &pending))
		assert.Equal(t, "authorization_pending", pending.Error, "poll %d must be pending", i)
	}

	finalResp, finalBody := postForm(t, srv.Client(), srv.URL+"/idp/token", map[string]string{"device_code": dc.DeviceCode})
	require.Equal(t, http.StatusOK, finalResp.StatusCode)
	var final struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	require.NoError(t, json.Unmarshal(finalBody, &final))
	assert.NotEmpty(t, final.AccessToken)
	assert.NotEmpty(t, final.IDToken)
}

// TestFacadeIdPURLPointsProviderAtExternalOrigin asserts the split-origin
// wiring: with IdPURL set, the azureADProvider document names the external
// origin for every endpoint and the facade itself no longer answers /idp/*,
// while IdPHandler serves the stub for that external server.
func TestFacadeIdPURLPointsProviderAtExternalOrigin(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{EnableIdP: true, IdPURL: "https://idp.test:8443"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1-public/authproviders")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	var azure map[string]any
	for _, p := range doc.Data {
		if p["type"] == "azureADProvider" {
			azure = p
		}
	}
	require.NotNil(t, azure)
	assert.Equal(t, "https://idp.test:8443/idp/authorize", azure["authUrl"])
	assert.Equal(t, "https://idp.test:8443/idp/devicecode", azure["deviceAuthUrl"])
	assert.Equal(t, "https://idp.test:8443/idp/token", azure["tokenUrl"])

	local, err := http.Post(srv.URL+"/idp/devicecode", "application/x-www-form-urlencoded", nil)
	require.NoError(t, err)
	_ = local.Body.Close()
	assert.Equal(t, http.StatusNotFound, local.StatusCode, "the facade must not serve the IdP on its own origin when IdPURL is set")

	idp := httptest.NewServer(f.IdPHandler())
	t.Cleanup(idp.Close)
	ext, err := http.Post(idp.URL+"/idp/devicecode", "application/x-www-form-urlencoded", nil)
	require.NoError(t, err)
	defer func() { _ = ext.Body.Close() }()
	assert.Equal(t, http.StatusOK, ext.StatusCode)
}

// TestIdPHandlerWithoutIdPIsNotFound asserts that a facade built without
// EnableIdP still hands out a usable handler: every route is 404 instead of
// a nil-receiver panic.
func TestIdPHandlerWithoutIdPIsNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(testrancher.New(testrancher.Options{}).IdPHandler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/idp/devicecode", "application/x-www-form-urlencoded", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// testPublicKeyB64 is a base64-encoded JSON RSA public key the authtokens
// accepts. Nothing here decrypts: the private half is dropped.
func testPublicKeyB64(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	b, err := json.Marshal(priv.PublicKey)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// nameOfBearer returns the resource name a facade bearer carries: the text
// between an optional "ext/" prefix and the secret separator.
func nameOfBearer(bearer string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(bearer, "ext/"), ":")
	return name
}

// TestFacadeJSONLoginReturnsV3ShapeWithDescription asserts the responseType
// json login: the response is the v3 token shape and the description the
// client sent is recorded and applied.
func TestFacadeJSONLoginReturnsV3ShapeWithDescription(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	body := `{"type":"localProvider","responseType":"json","description":"rancher-cli login @ h [op:0123456789ab]","username":"e2e-user","password":"e2e-pass"}`
	resp, err := http.Post(srv.URL+"/v1-public/login", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var got struct {
		Token     string `json:"token"`
		ID        string `json:"id"`
		ExpiresAt string `json:"expiresAt"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.NotEmpty(t, got.ID)
	assert.True(t, strings.HasPrefix(got.Token, got.ID+":"), "v3 bearer is <name>:<key>")
	assert.Equal(t, []string{"rancher-cli login @ h [op:0123456789ab]"}, f.LoginDescriptions())
}

// TestFacadeDeleteRecordsKindNameAndBearer asserts what cleanup tests read:
// every DELETE against either token store is recorded with its kind, name,
// and bearer.
func TestFacadeDeleteRecordsKindNameAndBearer(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{SeedToken: "seed:secret"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/k8s/clusters/local/apis/management.cattle.io/v3/tokens/token-abc", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer seed:secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	recs := f.DeleteRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, testrancher.DeleteRecord{Kind: "v3", Name: "token-abc", Bearer: "seed:secret"}, recs[0])
}

// TestFacadeLoginFailureFixturesCreateTheRightKind asserts that every login
// failure fixture still created the resource the request asked for: a json
// login is a v3 record whatever the response looked like, and no ext record
// appears.
func TestFacadeLoginFailureFixturesCreateTheRightKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts testrancher.Options
	}{
		{"dropped", testrancher.Options{DropLoginResponse: true}},
		{"malformed", testrancher.Options{MalformedLoginResponse: true}},
		{"name only", testrancher.Options{LoginNameOnly: true}},
		{"unusable bearer", testrancher.Options{LoginBearerOverride: "malformed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := testrancher.New(tc.opts)
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			body := `{"type":"localProvider","responseType":"json","username":"e2e-user","password":"e2e-pass"}`
			resp, err := http.Post(srv.URL+"/v1-public/login", "application/json", strings.NewReader(body))
			if err == nil {
				_ = resp.Body.Close()
			}
			assert.Equal(t, 1, f.TokenCount(), "the token was created whatever the response")
			assert.Len(t, f.BearersOfKind("v3"), 1, "a json login creates a v3 record")
			assert.Empty(t, f.BearersOfKind("ext"))
		})
	}
}

// TestFacadeKubeconfigLoginIsAnExtObjectWithAName asserts the kubeconfig login
// shape: an ext Token object whose metadata names the record and whose
// bearer carries the ext prefix, recorded as an ext token.
func TestFacadeKubeconfigLoginIsAnExtObjectWithAName(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	body := `{"type":"localProvider","responseType":"kubeconfig","username":"e2e-user","password":"e2e-pass"}`
	resp, err := http.Post(srv.URL+"/v1-public/login", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			BearerToken string `json:"bearerToken"`
		} `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.Metadata.Name)
	assert.Equal(t, "ext/"+got.Metadata.Name+":", got.Status.BearerToken[:len("ext/"+got.Metadata.Name+":")])
	assert.Equal(t, []string{got.Status.BearerToken}, f.BearersOfKind("ext"))
	assert.Empty(t, f.BearersOfKind("v3"))
}

// TestFacadeForcedStatusCreatesOnlyAfterDeterministic4xx asserts the
// facade's model of the server: a deterministic 4xx proves nothing was
// created, so no record exists after it; any other forced status models a
// commit the client never learned of, so the record exists.
func TestFacadeForcedStatusCreatesOnlyAfterDeterministic4xx(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		opts   testrancher.Options
		create bool
		status int
		want   int
	}{
		{"login 401", testrancher.Options{LoginStatus: 401}, false, 401, 0},
		{"login 500", testrancher.Options{LoginStatus: 500}, false, 500, 1},
		{"create 401", testrancher.Options{DerivedCreateStatus: 401, SeedToken: "seed:secret"}, true, 401, 0},
		{"create 500", testrancher.Options{DerivedCreateStatus: 500, SeedToken: "seed:secret"}, true, 500, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := testrancher.New(tc.opts)
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			var req *http.Request
			var err error
			if tc.create {
				req, err = http.NewRequest(http.MethodPost, srv.URL+extTokensPath, strings.NewReader(`{"spec":{}}`))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer seed:secret")
			} else {
				req, err = http.NewRequest(http.MethodPost, srv.URL+"/v1-public/login", strings.NewReader(`{"type":"localProvider","responseType":"json","username":"e2e-user","password":"e2e-pass"}`))
				require.NoError(t, err)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, tc.want, f.TokenCount())
		})
	}
}

// TestFacadeDeleteFaultModesFollowTheRecordRole asserts that the fault modes
// select by the record's role, not by the route: a v3 session token, an
// ext session token, and an ext derived token each answer their own mode.
func TestFacadeDeleteFaultModesFollowTheRecordRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		opts              testrancher.Options
		wantSessionStatus int
		wantDerivedStatus int
	}{
		{"v3 session fails, derived fine", testrancher.Options{SessionDeleteMode: "fail"}, 500, 200},
		{"ext session fails, derived fine", testrancher.Options{LoginShape: "ext", SessionDeleteMode: "fail"}, 500, 200},
		{"derived fails, ext session fine", testrancher.Options{LoginShape: "ext", DerivedDeleteMode: "fail"}, 200, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := testrancher.New(tc.opts)
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			login, err := http.Post(srv.URL+"/v1-public/login", "application/json", strings.NewReader(`{"type":"localProvider","responseType":"json","username":"e2e-user","password":"e2e-pass"}`))
			require.NoError(t, err)
			_ = login.Body.Close()
			sessionKind := "v3"
			if tc.opts.LoginShape == "ext" {
				sessionKind = "ext"
			}
			sBearer := f.BearersOfKind(sessionKind)[0]
			create, err := http.NewRequest(http.MethodPost, srv.URL+extTokensPath, strings.NewReader(`{"spec":{}}`))
			require.NoError(t, err)
			create.Header.Set("Authorization", "Bearer "+sBearer)
			resp, err := http.DefaultClient.Do(create)
			require.NoError(t, err)
			_ = resp.Body.Close()
			var dBearer string
			for _, b := range f.BearersOfKind("ext") {
				if b != sBearer {
					dBearer = b
				}
			}
			require.NotEmpty(t, dBearer)
			del := func(kind, name, bearer string) int {
				path := "/k8s/clusters/local/apis/management.cattle.io/v3/tokens/"
				if kind == "ext" {
					path = extTokensPath + "/"
				}
				req, err := http.NewRequest(http.MethodDelete, srv.URL+path+name, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+bearer)
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			sName, dName := nameOfBearer(sBearer), nameOfBearer(dBearer)
			assert.Equal(t, tc.wantSessionStatus, del(sessionKind, sName, dBearer))
			assert.Equal(t, tc.wantDerivedStatus, del("ext", dName, dBearer))
		})
	}
}

// TestFacadeAuthTokenDepositOnCommand asserts the SAML fixture: nothing is served
// at /v1-public/authtokens/<id> until DepositAuthToken, then the encrypted
// bearer appears.
func TestFacadeAuthTokenDepositOnCommand(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{EnableSAMLAuthTokens: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1-public/authtokens/req-1")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	pub := testPublicKeyB64(t)
	f.DepositAuthToken("req-1", pub)
	resp, err = http.Get(srv.URL + "/v1-public/authtokens/req-1")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	recs := f.AuthTokensRequests()
	require.Len(t, recs, 2)
	assert.Equal(t, testrancher.AuthTokensRequest{Method: http.MethodGet, Path: "/v1-public/authtokens/req-1"}, recs[0])
}

// TestFacadeExtTokenListAndDryRunDelete asserts what the CLI's get and delete
// verbs need against the facade: the ext collection lists every ext record
// with a resourceVersion, a dry-run delete with the current precondition
// changes nothing, a stale precondition is refused with 409, and every
// attempt is recorded with its dry-run flag and precondition. The record is
// created through the derived token create POST, so it is an ext token, not a v3 login
// token.
func TestFacadeExtTokenListAndDryRunDelete(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{SeedToken: "seed:secret"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	create, err := http.NewRequest(http.MethodPost, srv.URL+extTokensPath, strings.NewReader(`{"spec":{"description":"x [op:0123456789ab]"}}`))
	require.NoError(t, err)
	create.Header.Set("Authorization", "Bearer seed:secret")
	resp, err := http.DefaultClient.Do(create)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	names := f.TokensWithDescriptionContaining("ext", "[op:0123456789ab]")
	require.Len(t, names, 1)
	assert.Empty(t, f.TokensWithDescriptionContaining("v3", "[op:0123456789ab]"))
	var created struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.Equal(t, names[0], created.Metadata.Name, "the derived token create response names the created resource")

	get := func(path string) map[string]any {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer seed:secret")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var got map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		return got
	}
	list := get(extTokensPath)
	assert.Equal(t, "TokenList", list["kind"])
	items, _ := list["items"].([]any)
	descriptions := map[string]string{}
	for _, it := range items {
		obj, _ := it.(map[string]any)
		meta, _ := obj["metadata"].(map[string]any)
		spec, _ := obj["spec"].(map[string]any)
		desc, _ := spec["description"].(string)
		descriptions[meta["name"].(string)] = desc
	}
	require.Contains(t, descriptions, names[0], "the ext collection lists the ext record")
	assert.Equal(t, "x [op:0123456789ab]", descriptions[names[0]], "the list carries the description the recovery searches")
	one := get(extTokensPath + "/" + names[0])
	meta, _ := one["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	require.NotEmpty(t, rv)

	del := func(body string) int {
		req, err := http.NewRequest(http.MethodDelete, srv.URL+extTokensPath+"/"+names[0], strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer seed:secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, del(`{"dryRun":["All"],"preconditions":{"resourceVersion":"`+rv+`"}}`))
	assert.Len(t, f.TokensWithDescriptionContaining("ext", "[op:0123456789ab]"), 1, "dry run deletes nothing")
	assert.Equal(t, http.StatusConflict, del(`{"preconditions":{"resourceVersion":"stale"}}`))
	assert.Len(t, f.TokensWithDescriptionContaining("ext", "[op:0123456789ab]"), 1, "a stale precondition deletes nothing")
	recs := f.DeleteRequests()
	require.Len(t, recs, 2, "every attempt is recorded")
	assert.Equal(t, testrancher.DeleteRecord{Kind: "ext", Name: names[0], Bearer: "seed:secret", DryRun: true, Precondition: rv}, recs[0])
	assert.Equal(t, testrancher.DeleteRecord{Kind: "ext", Name: names[0], Bearer: "seed:secret", Precondition: "stale"}, recs[1])
}

// TestFacadeCreateEchoesOwnerAndScope asserts a scoped create answers with
// spec.clusterName, spec.description, and spec.userID of the bearer's
// owner, records the role cluster, and that OmitSpecOnCreate withholds spec.
func TestFacadeCreateEchoesOwnerAndScope(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-alice"}}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	login := postJSON(t, srv.Client(), srv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "alice", "password": "pw", "responseType": "json"})
	bearer := login["token"].(string)
	created := postJSON(t, srv.Client(), srv.URL+extTokensPath, bearer,
		map[string]any{"spec": map[string]any{"clusterName": "c-test", "description": "d"}})
	spec := created["spec"].(map[string]any)
	assert.Equal(t, "c-test", spec["clusterName"])
	assert.Equal(t, "user-alice", spec["userID"])
	assert.Equal(t, "d", spec["description"])
	name := created["metadata"].(map[string]any)["name"].(string)
	owner, ok := f.TokenOwner(name)
	require.True(t, ok)
	assert.Equal(t, "user-alice", owner)
	recs := f.CreateRequests()
	require.Len(t, recs, 2, "the login and the scoped create")
	assert.Equal(t, "session", recs[0].Role)
	assert.Equal(t, login["id"], recs[0].Name, "the login record names the token it created")
	assert.Empty(t, recs[0].Bearer, "a login carries no bearer")
	assert.Equal(t, "user-alice", recs[0].OwnerUserID)
	assert.Equal(t, "cluster", recs[1].Role)
	assert.Equal(t, name, recs[1].Name)
	assert.Equal(t, bearer, recs[1].Bearer)
	assert.Equal(t, "c-test", recs[1].ClusterName)
	assert.Equal(t, "user-alice", recs[1].OwnerUserID)

	quiet := testrancher.New(testrancher.Options{OmitSpecOnCreate: true})
	qsrv := httptest.NewServer(quiet)
	t.Cleanup(qsrv.Close)
	l := postJSON(t, qsrv.Client(), qsrv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	c := postJSON(t, qsrv.Client(), qsrv.URL+extTokensPath, l["token"].(string), map[string]any{"spec": map[string]any{"clusterName": "c-test"}})
	_, hasSpec := c["spec"]
	assert.False(t, hasSpec)
}

// TestFacadeDeleteByAnotherOwnerIsNotFound asserts the ext delete rule: a
// bearer whose owner differs answers 404 and the token survives; the same
// owner deletes it. On the v3 path a non-admin owner is refused with 403
// when configured, an admin succeeds.
func TestFacadeDeleteByAnotherOwnerIsNotFound(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{Users: []testrancher.User{{Name: "a", Password: "pw", UserID: "user-a"}, {Name: "b", Password: "pw", UserID: "user-b"}, {Name: "root", Password: "pw", UserID: "user-root"}},
		AdminUserIDs: []string{"user-root"}, V3DeleteForbiddenForNonAdmin: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	loginAs := func(u string) string {
		return postJSON(t, srv.Client(), srv.URL+"/v1-public/login", "",
			map[string]any{"type": "localProvider", "username": u, "password": "pw", "responseType": "json"})["token"].(string)
	}
	a, b, root := loginAs("a"), loginAs("b"), loginAs("root")
	aDerived := postJSON(t, srv.Client(), srv.URL+extTokensPath, a, map[string]any{"spec": map[string]any{}})
	aName := aDerived["metadata"].(map[string]any)["name"].(string)
	assert.Equal(t, http.StatusNotFound, statusOf(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/"+aName, b))
	_, alive := f.TokenOwner(aName)
	assert.True(t, alive, "another owner's delete never removes the token")
	assert.Equal(t, http.StatusOK, statusOf(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/"+aName, a))

	// The v3 login bodies are v3 records; their names are the ids.
	aSess, _, _ := strings.Cut(a, ":")
	assert.Equal(t, http.StatusForbidden, statusOf(t, srv.Client(), http.MethodDelete, srv.URL+v3TokensPath+"/"+aSess, a))
	assert.Equal(t, http.StatusOK, statusOf(t, srv.Client(), http.MethodDelete, srv.URL+v3TokensPath+"/"+aSess, root))
}

// TestFacadeNormanV3TokenCreate asserts the legacy cluster-token create:
// POST /v3/tokens with clusterId answers the Norman shape with clusterId
// and userId echoed, records a v3 record of role cluster, honors a forced
// status and a clusterId override, and is 404 unless enabled.
func TestFacadeNormanV3TokenCreate(t *testing.T) {
	t.Parallel()
	off := testrancher.New(testrancher.Options{Legacy: true})
	osrv := httptest.NewServer(off)
	t.Cleanup(osrv.Close)
	l := postJSON(t, osrv.Client(), osrv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	assert.Equal(t, http.StatusNotFound, statusOfBody(t, osrv.Client(), http.MethodPost, osrv.URL+"/v3/tokens", l["token"].(string),
		map[string]any{"type": "token", "clusterId": "c-test"}))

	f := testrancher.New(testrancher.Options{Legacy: true, V3TokenCreate: true, LegacyLoginBodyUserID: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	login := postJSON(t, srv.Client(), srv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	assert.Equal(t, "user-e2e", login["userId"], "a pre-ext login body carries the owner")
	created := postJSON(t, srv.Client(), srv.URL+"/v3/tokens", login["token"].(string),
		map[string]any{"type": "token", "description": "d", "clusterId": "c-test", "ttl": 60000})
	assert.Equal(t, "c-test", created["clusterId"])
	assert.Equal(t, "user-e2e", created["userId"])
	assert.NotEmpty(t, created["token"])
	assert.NotEmpty(t, created["id"])
	recs := f.CreateRequests()
	assert.Equal(t, "v3", recs[len(recs)-1].Kind)
	assert.Equal(t, "cluster", recs[len(recs)-1].Role)

	empty := ""
	g := testrancher.New(testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateClusterID: &empty})
	gsrv := httptest.NewServer(g)
	t.Cleanup(gsrv.Close)
	gl := postJSON(t, gsrv.Client(), gsrv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	unscoped := postJSON(t, gsrv.Client(), gsrv.URL+"/v3/tokens", gl["token"].(string), map[string]any{"type": "token", "clusterId": "c-test"})
	_, has := unscoped["clusterId"]
	assert.False(t, has, "the override withholds clusterId")
}

// TestFacadeSetDeleteModeHangsUntilTheRequestEnds asserts the hang mode
// used by the retirement-budget tests: the delete of the named token blocks
// until the client gives up, and the record survives.
func TestFacadeSetDeleteModeHangsUntilTheRequestEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := testrancher.New(testrancher.Options{})
		srv := httptest.NewTestServer(t, f)
		client := srv.Client() // sets srv.URL: an in-memory server's URL is set by its first Client call
		login := postJSON(t, client, srv.URL+"/v1-public/login", "",
			map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
		bearer := login["token"].(string)
		created := postJSON(t, client, srv.URL+extTokensPath, bearer, map[string]any{"spec": map[string]any{}})
		name := created["metadata"].(map[string]any)["name"].(string)
		f.SetDeleteMode(name, "hang")
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+extTokensPath+"/"+name, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		start := time.Now()
		_, err := client.Do(req)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 200*time.Millisecond, time.Since(start), "the client's deadline ended the hung delete on the fake clock")
		_, alive := f.TokenOwner(name)
		assert.True(t, alive)
	})
}

// TestDerivedCreateAuthenticatesBeforeTheHook asserts a bearer revoked while
// the create hook holds the request still creates the token: authentication
// happened on arrival, as Rancher's filter does, so a caller that gives up
// and revokes its credential cannot turn the create it abandoned into a 401.
func TestDerivedCreateAuthenticatesBeforeTheHook(t *testing.T) {
	t.Parallel()
	var f *testrancher.Facade
	var srv *httptest.Server
	var creates atomic.Int32
	f, srv = newFacade(t, testrancher.Options{SeedToken: "e2e:secret", DerivedCreateHook: func(r *http.Request) {
		if creates.Add(1) != 2 {
			return
		}
		// Revoke the credential the second create arrived with, with the
		// seed bearer, and wait for the revocation to land.
		resp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}}, &recordingHandler{})

	_, createBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(createBody, &tok))
	require.NotEmpty(t, tok.Status.BearerToken)

	resp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath,
		map[string]any{"spec": map[string]any{"clusterName": "c-test", "description": "[op:abc]"}}, tok.Status.BearerToken)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, []string{"fac-token-2"}, f.TokensWithDescriptionContaining("ext", "[op:abc]"), "the create the hook held still produced its token")
	assert.False(t, f.ValidBearer(tok.Status.BearerToken), "the revocation landed")
}

// TestSelfSubjectReviewNamesTheBearersOwner asserts the facade answers a
// SelfSubjectReview through a cluster's path as Rancher does: 201 with the
// bearer's owner as status.userInfo.username and no uid, 401 for a bearer
// it does not know, the forced status when one is set, and a fall through
// with the option off.
func TestSelfSubjectReviewNamesTheBearersOwner(t *testing.T) {
	const path = "/k8s/clusters/c-1/apis/authentication.k8s.io/v1/selfsubjectreviews"
	post := func(t *testing.T, srv *httptest.Server, bearer string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path,
			strings.NewReader(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	for _, tc := range []struct {
		name   string
		opts   testrancher.Options
		bearer string
		status int
		user   string
	}{
		{"the seed bearer", testrancher.Options{SelfSubjectReviews: true, SeedToken: "seed:secret"}, "seed:secret", http.StatusCreated, "e2e-admin"},
		{"an unknown bearer", testrancher.Options{SelfSubjectReviews: true, SeedToken: "seed:secret"}, "other:secret", http.StatusUnauthorized, ""},
		{"a forced status", testrancher.Options{SelfSubjectReviews: true, SelfSubjectReviewStatus: http.StatusInternalServerError, SeedToken: "seed:secret"}, "seed:secret", http.StatusInternalServerError, ""},
		{"the option off", testrancher.Options{SeedToken: "seed:secret", Fallthrough: &recordingHandler{}}, "seed:secret", http.StatusTeapot, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(testrancher.New(tc.opts))
			t.Cleanup(srv.Close)
			status, body := post(t, srv, tc.bearer)
			assert.Equal(t, tc.status, status)
			if tc.user == "" {
				return
			}
			userInfo := body["status"].(map[string]any)["userInfo"].(map[string]any)
			assert.Equal(t, tc.user, userInfo["username"])
			assert.NotContains(t, userInfo, "uid", "Rancher's answer carries no uid")
		})
	}
}
