package testrancher_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mintTokensPath = token.MintTokensPath

// recordingHandler captures every request it receives, for asserting a
// mint-discrimination or unknown-name case actually fell through instead of
// being served by the façade itself.
type recordingHandler struct {
	requests []*http.Request
	bodies   [][]byte
}

func (r *recordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.requests = append(r.requests, req)
	r.bodies = append(r.bodies, body)
	w.WriteHeader(http.StatusTeapot) // distinctive: never a code the façade itself would return
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
	assert.NoError(t, err)

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

func TestMintDiscriminatesCRCreateFromMint(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	// A full CR (metadata.name present) must fall through untouched.
	resp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
		map[string]any{"apiVersion": "ext.cattle.io/v1", "kind": "Token", "metadata": map[string]string{"name": "cr-token"}},
		"e2e:secret")
	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "CR-shaped body must reach the recording fallthrough")
	require.Len(t, fall.requests, 1)

	// A bare {"spec": ...} body is a mint and must not reach fallthrough.
	resp2, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
		map[string]any{"spec": map[string]any{"description": "d"}}, "e2e:secret")
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)
	assert.Len(t, fall.requests, 1, "the mint branch must not fall through")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body2, &tok))
	assert.Equal(t, "ext/fac-token-1:fac-secret-1", tok.Status.BearerToken)
}

func TestMintTTLHonoredAndDefaulted(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	before := time.Now()
	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
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
	resp2, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
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

func TestMintClusterNameRecorded(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
		map[string]any{"spec": map[string]any{"clusterName": "c-test"}}, "e2e:secret")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(body, &tok))
	name := "fac-token-1"

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/"+name, nil, "e2e:secret")
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

	// Slot 1: two independent mints in sequence are strictly incrementing.
	_, body1 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	_, body2 := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
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

	// Reproduces resolveProviderLoginToken (login_ext.go): login mints session
	// token S (slot 1), MintDerivedToken mints derived token D (slot 2) using S
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

	mintResp, mintBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
		map[string]any{"spec": map[string]any{}}, s.Status.BearerToken)
	require.Equal(t, http.StatusCreated, mintResp.StatusCode)
	var d struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(mintBody, &d))
	assert.Equal(t, "ext/fac-token-2:fac-secret-2", d.Status.BearerToken, "the stored credential after a provider login is fac-token-2")

	revokeResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", nil, s.Status.BearerToken)
	assert.Equal(t, http.StatusOK, revokeResp.StatusCode)
}

func TestNamedGetServedUnknownAndListFallThrough(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	_, mintBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(mintBody, &tok))
	require.True(t, f.ValidBearer(tok.Status.BearerToken))

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/fac-token-1", nil, "e2e:secret")
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

	getResp2, getBody2 := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/fac-token-1", nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp2.StatusCode)
	var cr2 struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody2, &cr2))
	assert.Equal(t, cr.Metadata.ResourceVersion, cr2.Metadata.ResourceVersion, "resourceVersion must be stable across reads")

	unknownResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/no-such-token", nil, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, unknownResp.StatusCode, "unknown-name GET must fall through")

	listResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath, nil, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, listResp.StatusCode, "LIST must always fall through")
}

func TestDeleteDryRunRealDeleteReDeleteFallsThrough(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, fall)

	_, mintBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(mintBody, &tok))

	getResp, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/fac-token-1", nil, "e2e:secret")
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var cr struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		}
	}
	require.NoError(t, json.Unmarshal(getBody, &cr))

	dryRunBody := map[string]any{"dryRun": []string{"All"}, "preconditions": map[string]string{"resourceVersion": cr.Metadata.ResourceVersion}}
	dryResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", dryRunBody, "e2e:secret")
	assert.Equal(t, http.StatusOK, dryResp.StatusCode)
	assert.True(t, f.ValidBearer(tok.Status.BearerToken), "dry-run delete must not revoke")

	// Wrong precondition on the real delete must be rejected, still not revoking.
	badPrecondition := map[string]any{"preconditions": map[string]string{"resourceVersion": "stale"}}
	badResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", badPrecondition, "e2e:secret")
	assert.Equal(t, http.StatusConflict, badResp.StatusCode)
	assert.True(t, f.ValidBearer(tok.Status.BearerToken))

	realBody := map[string]any{"preconditions": map[string]string{"resourceVersion": cr.Metadata.ResourceVersion}}
	realResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", realBody, "e2e:secret")
	assert.Equal(t, http.StatusOK, realResp.StatusCode)
	assert.False(t, f.ValidBearer(tok.Status.BearerToken), "the real delete must revoke")

	reDeleteResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", nil, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, reDeleteResp.StatusCode, "re-delete of an already-gone name must fall through")
	require.Len(t, fall.requests, 1)
}

func TestLegacyModeMintNotFoundCRFallthroughIntact(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{Legacy: true, SeedToken: "e2e:secret"}, fall)

	mintResp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	assert.Equal(t, http.StatusNotFound, mintResp.StatusCode, "legacy mode 404s the mint route")

	crResp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath,
		map[string]any{"metadata": map[string]string{"name": "cr-token"}}, "e2e:secret")
	assert.Equal(t, http.StatusTeapot, crResp.StatusCode, "CR-create fallthrough stays intact under legacy mode")
	require.Len(t, fall.requests, 1)
}

func TestMintRejectsUnauthorizedBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	resp, _ := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "garbage:garbage")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestNamedGetAndDeleteRejectUnauthorizedBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	_, mintBody := doJSON(t, srv.Client(), http.MethodPost, srv.URL+mintTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	var tok struct {
		Status struct {
			BearerToken string `json:"bearerToken"`
		}
	}
	require.NoError(t, json.Unmarshal(mintBody, &tok))

	for _, bearer := range []string{"", "garbage:garbage"} {
		getResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/fac-token-1", nil, bearer)
		assert.Equal(t, http.StatusUnauthorized, getResp.StatusCode, "GET with bearer %q must 401", bearer)

		delResp, _ := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+mintTokensPath+"/fac-token-1", nil, bearer)
		assert.Equal(t, http.StatusUnauthorized, delResp.StatusCode, "DELETE with bearer %q must 401", bearer)
	}

	// The token is still valid after both rejections — neither failed call revoked it.
	getResp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+mintTokensPath+"/fac-token-1", nil, tok.Status.BearerToken)
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

	for i := 0; i < 2; i++ {
		pollResp, pollBody := postForm(t, srv.Client(), srv.URL+"/idp/token", map[string]string{"device_code": dc.DeviceCode})
		require.Equal(t, http.StatusOK, pollResp.StatusCode)
		var pending struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(pollBody, &pending))
		assert.Equal(t, "authorization_pending", pending.Error, fmt.Sprintf("poll %d must be pending", i))
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
