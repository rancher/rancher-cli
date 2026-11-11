package testrancher_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ninetyDays   = 90 * 24 * time.Hour
	thirtyDays   = 30 * 24 * time.Hour
	ninetyDaysMs = int64(ninetyDays / time.Millisecond)
)

// decodeObject decodes a JSON object body.
func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// keysOf is the sorted keys of an object.
func keysOf(obj any) []string {
	return slices.Sorted(maps.Keys(obj.(map[string]any)))
}

func expiryOf(t *testing.T, text any) time.Time {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339, text.(string))
	require.NoError(t, err)
	return expiresAt
}

// TestFacadeExtCreateLifetimeIsRanchersTokenLifetime asserts the lifetime of
// an ext create: 90 days when it asks for none (or for 0), a requested ttl
// up to 90 days kept, a larger one cut to 90 days; the answer's spec.ttl is
// that lifetime in milliseconds and status.expiresAt the creation time plus
// it.
func TestFacadeExtCreateLifetimeIsRanchersTokenLifetime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spec    map[string]any
		wantTTL int64
	}{
		{"no ttl", map[string]any{}, ninetyDaysMs},
		{"ttl 0", map[string]any{"ttl": 0}, ninetyDaysMs},
		{"a ttl below 90 days", map[string]any{"ttl": 60_000}, 60_000},
		{"exactly 90 days", map[string]any{"ttl": ninetyDaysMs}, ninetyDaysMs},
		{"a ttl above 90 days", map[string]any{"ttl": ninetyDaysMs + 1}, ninetyDaysMs},
		{"a huge ttl", map[string]any{"ttl": int64(1) << 62}, ninetyDaysMs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
			before := time.Now()
			resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": tc.spec}, "e2e:secret")
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			tok := decodeObject(t, body)
			assert.EqualValues(t, tc.wantTTL, tok["spec"].(map[string]any)["ttl"])
			want := before.Add(time.Duration(tc.wantTTL) * time.Millisecond)
			assert.WithinDuration(t, want, expiryOf(t, tok["status"].(map[string]any)["expiresAt"]), 5*time.Second)

			get, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/fac-token-1", nil, "e2e:secret")
			require.Equal(t, http.StatusOK, get.StatusCode)
			assert.EqualValues(t, tc.wantTTL, decodeObject(t, getBody)["spec"].(map[string]any)["ttl"], "the stored ttl is the lifetime the token has")
			assert.True(t, f.ValidBearer(tok["status"].(map[string]any)["bearerToken"].(string)))
		})
	}
}

// TestFacadeExtTokenAnswersCarryTheKeysRancherSends asserts the keys of each
// ext Token answer: every answer has the metadata, spec and status keys of
// the recorded Rancher answers, a create adds status.bearerToken and
// status.value, a delete and a dry-run delete add status.hash, and
// spec.clusterName and spec.description appear only when set.
func TestFacadeExtTokenAnswersCarryTheKeysRancherSends(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	common := map[string][]string{
		"metadata": {"creationTimestamp", "generateName", "labels", "name", "resourceVersion", "uid"},
		"spec":     {"enabled", "kind", "ttl", "userID", "userPrincipal"},
		"status":   {"current", "expired", "expiresAt", "lastUpdateTime"},
	}
	assertKeys := func(t *testing.T, tok map[string]any, specExtra, statusExtra []string) {
		t.Helper()
		assert.Equal(t, []string{"apiVersion", "kind", "metadata", "spec", "status"}, keysOf(tok))
		assert.Equal(t, common["metadata"], keysOf(tok["metadata"]))
		specKeys := slices.Concat(common["spec"], specExtra)
		slices.Sort(specKeys)
		statusKeys := slices.Concat(common["status"], statusExtra)
		slices.Sort(statusKeys)
		assert.Equal(t, specKeys, keysOf(tok["spec"]))
		assert.Equal(t, statusKeys, keysOf(tok["status"]))
		assert.Equal(t, []string{"displayName", "loginName", "me", "name", "principalType", "provider"}, keysOf(tok["spec"].(map[string]any)["userPrincipal"]))
		assert.Equal(t, []string{"authn.management.cattle.io/kind", "cattle.io/user-id"}, keysOf(tok["metadata"].(map[string]any)["labels"]))
	}

	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+extTokensPath, map[string]any{"spec": map[string]any{}}, "e2e:secret")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeObject(t, body)
	assertKeys(t, created, nil, []string{"bearerToken", "value"})
	assert.Equal(t, "ext.cattle.io/v1", created["apiVersion"])
	assert.Equal(t, "Token", created["kind"])
	meta, spec, status := created["metadata"].(map[string]any), created["spec"].(map[string]any), created["status"].(map[string]any)
	assert.Equal(t, "token-", meta["generateName"])
	assert.Equal(t, "", spec["kind"], "a plain create")
	assert.Equal(t, true, spec["enabled"])
	assert.Equal(t, "", meta["labels"].(map[string]any)["authn.management.cattle.io/kind"])
	assert.Equal(t, testrancher.SeedOwner, meta["labels"].(map[string]any)["cattle.io/user-id"])
	assert.Equal(t, testrancher.SeedOwner, spec["userID"])
	assert.Equal(t, "local://"+testrancher.SeedOwner, spec["userPrincipal"].(map[string]any)["name"])
	assert.Equal(t, false, status["expired"])
	assert.Equal(t, false, status["current"])
	_, err := time.Parse(time.RFC3339, meta["creationTimestamp"].(string))
	require.NoError(t, err)
	assert.Equal(t, meta["creationTimestamp"], status["lastUpdateTime"])
	assert.NotEmpty(t, meta["uid"])
	assert.Equal(t, "ext/"+meta["name"].(string)+":"+status["value"].(string), status["bearerToken"], "status.value is the secret alone")

	name := meta["name"].(string)
	get, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/"+name, nil, "e2e:secret")
	require.Equal(t, http.StatusOK, get.StatusCode)
	assertKeys(t, decodeObject(t, getBody), nil, nil)

	list, listBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath, nil, "e2e:secret")
	require.Equal(t, http.StatusOK, list.StatusCode)
	items := decodeObject(t, listBody)["items"].([]any)
	require.Len(t, items, 1)
	assertKeys(t, items[0].(map[string]any), nil, nil)

	dry, dryBody := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/"+name, map[string]any{"dryRun": []string{"All"}}, "e2e:secret")
	require.Equal(t, http.StatusOK, dry.StatusCode)
	dryTok := decodeObject(t, dryBody)
	assertKeys(t, dryTok, nil, []string{"hash"})
	assert.NotEqual(t, status["value"], dryTok["status"].(map[string]any)["hash"], "the hash is not the secret")

	del, delBody := doJSON(t, srv.Client(), http.MethodDelete, srv.URL+extTokensPath+"/"+name, nil, "e2e:secret")
	require.Equal(t, http.StatusOK, del.StatusCode)
	assertKeys(t, decodeObject(t, delBody), nil, []string{"hash"})

	scoped := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret",
		map[string]any{"spec": map[string]any{"clusterName": "c-test", "description": "d"}})
	assertKeys(t, scoped, []string{"clusterName", "description"}, []string{"bearerToken", "value"})
	assert.Equal(t, "c-test", scoped["spec"].(map[string]any)["clusterName"])
	assert.Equal(t, "d", scoped["spec"].(map[string]any)["description"])
}

// TestFacadeExtTokenComputedFields asserts the fields the facade computes:
// status.current is true only for the token whose own bearer made the
// request, metadata.uid differs per token, and status.expired follows the
// expiry.
func TestFacadeExtTokenComputedFields(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	first := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": map[string]any{}})
	second := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": map[string]any{}})
	assert.NotEqual(t, first["metadata"].(map[string]any)["uid"], second["metadata"].(map[string]any)["uid"])
	firstBearer := first["status"].(map[string]any)["bearerToken"].(string)
	firstPath := srv.URL + extTokensPath + "/" + nameOfBearer(firstBearer)

	currentOf := func(bearer string) any {
		resp, body := doJSON(t, srv.Client(), http.MethodGet, firstPath, nil, bearer)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return decodeObject(t, body)["status"].(map[string]any)["current"]
	}
	assert.Equal(t, true, currentOf(firstBearer), "the token's own bearer")
	assert.Equal(t, false, currentOf("e2e:secret"), "another bearer")
	assert.Equal(t, false, currentOf(second["status"].(map[string]any)["bearerToken"].(string)), "another token's bearer")

	resp, body := doJSON(t, srv.Client(), http.MethodDelete, firstPath, map[string]any{"dryRun": []string{"All"}}, firstBearer)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, true, decodeObject(t, body)["status"].(map[string]any)["current"], "a delete with the token's own bearer")
}

// TestFacadeExtTokenIsExpiredAfterItsLifetime asserts status.expired turns
// true once the token's expiry has passed.
func TestFacadeExtTokenIsExpiredAfterItsLifetime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := testrancher.New(testrancher.Options{SeedToken: "e2e:secret"})
		srv := httptest.NewTestServer(t, f)
		_ = srv.Client() // sets srv.URL: an in-memory server's URL is set by its first Client call
		created := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": map[string]any{"ttl": 60_000}})
		path := srv.URL + extTokensPath + "/" + created["metadata"].(map[string]any)["name"].(string)
		expiredOf := func() any {
			_, body := doJSON(t, srv.Client(), http.MethodGet, path, nil, "e2e:secret")
			return decodeObject(t, body)["status"].(map[string]any)["expired"]
		}
		assert.Equal(t, false, expiredOf())
		time.Sleep(2 * time.Minute)
		assert.Equal(t, true, expiredOf())
	})
}

// TestFacadeEveryTokenKindHasItsRanchersLifetime asserts, with a clock the
// test advances, how long each token the facade creates authenticates: a
// session token 960 minutes, a kubeconfig login token 30 days, an ext and a
// Norman token 90 days, the token a SAML sign-in deposits 30 days.
func TestFacadeEveryTokenKindHasItsRanchersLifetime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := testrancher.New(testrancher.Options{SeedToken: "e2e:secret", V3TokenCreate: true, EnableSAMLAuthTokens: true})
		srv := httptest.NewTestServer(t, f)
		_ = srv.Client()
		login := func(responseType string) string {
			_, body := loginAnswer(t, srv, responseType, nil)
			return decodeObject(t, body)["token"].(string)
		}
		session, kubeconfig := login("json"), login("kubeconfig")
		ext := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": map[string]any{}})["status"].(map[string]any)["bearerToken"].(string)
		norman := postJSON(t, srv.Client(), srv.URL+"/v3/tokens", "e2e:secret", map[string]any{"type": "token", "clusterId": "c-test"})["token"].(string)
		f.DepositAuthToken("req-1", testPublicKeyB64(t))
		saml := f.BearersOfKind("v3")[0]
		for _, b := range f.BearersOfKind("v3") {
			if !slices.Contains([]string{session, kubeconfig, norman}, b) {
				saml = b
			}
		}

		bearersByLifetime := map[time.Duration]map[string]string{
			960 * time.Minute: {"session": session},
			thirtyDays:        {"kubeconfig login": kubeconfig, "saml": saml},
			ninetyDays:        {"ext": ext, "norman": norman},
		}
		var elapsed time.Duration
		for _, valid := range slices.Sorted(maps.Keys(bearersByLifetime)) {
			time.Sleep(valid - time.Minute - elapsed)
			for name, bearer := range bearersByLifetime[valid] {
				assert.True(t, f.ValidBearer(bearer), "%s is valid just before %s", name, valid)
			}
			time.Sleep(2 * time.Minute)
			elapsed = valid + time.Minute
			for name, bearer := range bearersByLifetime[valid] {
				assert.False(t, f.ValidBearer(bearer), "%s is expired just after %s", name, valid)
			}
		}
	})
}

// TestFacadeAuthTokenDepositsCarryAThirtyDayExpiry asserts every SAML deposit
// stores its expiry beside its payload: the authtokens answer is that expiry,
// thirty days from the deposit, whenever it is read, whatever the kind of the
// deposit; the token a deposit creates is a v3 kubeconfig token.
func TestFacadeAuthTokenDepositsCarryAThirtyDayExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := testrancher.New(testrancher.Options{EnableSAMLAuthTokens: true})
		srv := httptest.NewTestServer(t, f)
		_ = srv.Client()
		pub := testPublicKeyB64(t)
		deposited := time.Now()
		f.DepositAuthToken("req-token", pub)
		f.DepositAuthTokenPlaintext("req-plain", pub, "not a token")
		f.DepositAuthTokenRaw("req-raw", "!!not-base64")
		time.Sleep(10 * 24 * time.Hour)

		for _, id := range []string{"req-token", "req-plain", "req-raw"} {
			resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1-public/authtokens/"+id, nil, "")
			require.Equal(t, http.StatusOK, resp.StatusCode, id)
			assert.Equal(t, deposited.Add(thirtyDays).UTC().Truncate(time.Second), expiryOf(t, decodeObject(t, body)["expiresAt"]), id)
		}
		assert.Len(t, f.BearersOfKind("v3"), 1)
		assert.Equal(t, []string{"fac-token-1"}, f.TokensWithDescriptionContaining("v3", "Kubeconfig token"))
		owner, ok := f.TokenOwner("fac-token-1")
		assert.True(t, ok)
		assert.Equal(t, "user-e2e", owner, "the deposited token belongs to the first configured user")
	})
}

// TestFacadeMadeFromExpiresEachCreateNinetyDaysAfterIt asserts MadeFrom's
// expiry is the time of each create plus 90 days, not one value fixed when
// the server started.
func TestFacadeMadeFromExpiresEachCreateNinetyDaysAfterIt(t *testing.T) {
	t.Parallel()
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	create := func() (time.Time, time.Time) {
		created := time.Now()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, m.URL+extTokensPath, strings.NewReader(`{"spec":{}}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer login")
		resp, err := m.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var tok struct {
			Status struct {
				ExpiresAt string `json:"expiresAt"`
			} `json:"status"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tok))
		return created, expiryOf(t, tok.Status.ExpiresAt)
	}
	firstAt, firstExpiry := create()
	time.Sleep(1100 * time.Millisecond)
	secondAt, secondExpiry := create()
	assert.WithinDuration(t, firstAt.Add(ninetyDays), firstExpiry, 2*time.Second)
	assert.WithinDuration(t, secondAt.Add(ninetyDays), secondExpiry, 2*time.Second)
	assert.GreaterOrEqual(t, secondExpiry.Sub(firstExpiry), time.Second, "the second create's expiry is later")
}

// kubeconfigCreate posts a create of the ext Kubeconfig with bearer and
// returns the response and the decoded object.
func kubeconfigCreate(t *testing.T, srv *httptest.Server, bearer, query string, spec map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+testrancher.KubeconfigsCollectionPath+query, map[string]any{"spec": spec}, bearer)
	return resp, decodeObject(t, body)
}

// TestFacadeCreateKubeconfigCreatesOneKubeconfigToken asserts that a create
// kubeconfig makes one unscoped ext token of kind kubeconfig, valid 30 days,
// labeled with the Kubeconfig it was made for, and that the returned
// kubeconfig names the server the request came to and carries that token's
// bearer, which authenticates.
func TestFacadeCreateKubeconfigCreatesOneKubeconfigToken(t *testing.T) {
	t.Parallel()
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	before := time.Now()
	resp, obj := kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{"clusters": []string{"c-test"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, 1, f.TokenCount(), "one token for the create")
	bearers := f.BearersOfKind("ext")
	require.Len(t, bearers, 1)
	assert.Empty(t, f.BearersOfKind("v3"))

	spec := obj["spec"].(map[string]any)
	assert.EqualValues(t, thirtyDays/time.Second, spec["ttl"], "the default in seconds when none was sent")

	get, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/"+nameOfBearer(bearers[0]), nil, "e2e:secret")
	require.Equal(t, http.StatusOK, get.StatusCode)
	tok := decodeObject(t, getBody)
	tokSpec, meta := tok["spec"].(map[string]any), tok["metadata"].(map[string]any)
	assert.Equal(t, "kubeconfig", tokSpec["kind"])
	assert.Equal(t, "Kubeconfig token", tokSpec["description"])
	assert.Equal(t, testrancher.SeedOwner, tokSpec["userID"], "the token belongs to the user who made the request")
	assert.Equal(t, testrancher.SeedOwner, meta["labels"].(map[string]any)["cattle.io/user-id"])
	assert.Equal(t, []any{meta["name"]}, obj["status"].(map[string]any)["tokens"], "the answer lists the token the create made")
	createdAt, err := time.Parse(time.RFC3339, obj["metadata"].(map[string]any)["creationTimestamp"].(string))
	require.NoError(t, err, "the answer carries the creation time in RFC 3339")
	assert.WithinDuration(t, before, createdAt, 5*time.Second)
	assert.Equal(t, time.UTC, createdAt.Location(), "the creation time is in UTC")
	assert.NotContains(t, tokSpec, "clusterName", "an unscoped token")
	assert.EqualValues(t, thirtyDays/time.Millisecond, tokSpec["ttl"])
	assert.Equal(t, "kubeconfig", meta["labels"].(map[string]any)["authn.management.cattle.io/kind"])
	assert.Equal(t, obj["metadata"].(map[string]any)["name"], meta["labels"].(map[string]any)["authn.management.cattle.io/kubeconfig-id"])
	assert.WithinDuration(t, before.Add(thirtyDays), expiryOf(t, tok["status"].(map[string]any)["expiresAt"]), 5*time.Second)

	value := obj["status"].(map[string]any)["value"].(string)
	cfg, err := clientcmd.Load([]byte(value))
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/k8s/clusters/c-test", cfg.Clusters["c-test"].Server)
	require.Len(t, cfg.AuthInfos, 1)
	for _, a := range cfg.AuthInfos {
		assert.Equal(t, bearers[0], a.Token, "the kubeconfig carries the created token's bearer")
	}
	assert.True(t, f.ValidBearer(bearers[0]))

	kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{})
	assert.Equal(t, 2, f.TokenCount(), "each create makes its own token")
}

// TestFacadeCreateKubeconfigDryRunCreatesNothing asserts a dry run makes no
// token and answers no status.
func TestFacadeCreateKubeconfigDryRunCreatesNothing(t *testing.T) {
	t.Parallel()
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})
	resp, obj := kubeconfigCreate(t, srv, "e2e:secret", "?dryRun=All", map[string]any{"clusters": []string{"c-test"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Zero(t, f.TokenCount())
	assert.NotContains(t, obj, "status")
}

// TestFacadeCreateKubeconfigLifetimeFollowsTheRequestedTTL asserts a
// requested spec.ttl in seconds is the token's lifetime and stays in the
// answer, while one above 30 days or negative is refused with 400 and makes
// no token.
func TestFacadeCreateKubeconfigLifetimeFollowsTheRequestedTTL(t *testing.T) {
	t.Parallel()
	const maxSeconds = int64(thirtyDays / time.Second)
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret"}, &recordingHandler{})

	resp, obj := kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{"ttl": 3600})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.EqualValues(t, 3600, obj["spec"].(map[string]any)["ttl"])
	_, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/"+nameOfBearer(f.BearersOfKind("ext")[0]), nil, "e2e:secret")
	assert.EqualValues(t, 3_600_000, decodeObject(t, body)["spec"].(map[string]any)["ttl"])

	resp, _ = kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{"ttl": maxSeconds})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "exactly 30 days")

	resp, obj = kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{"ttl": maxSeconds + 1})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "spec.ttl 2592001 exceeds max ttl 2592000", obj["message"])
	resp, obj = kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{"ttl": -1})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "spec.ttl can't be negative", obj["message"])
	resp, _ = kubeconfigCreate(t, srv, "e2e:secret", "?dryRun=All", map[string]any{"ttl": maxSeconds + 1})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a dry run is refused too")
	assert.Equal(t, 2, f.TokenCount(), "a refused create makes no token")
}

// TestFacadeDeletingAKubeconfigDeletesItsTokens asserts that the delete of a
// Kubeconfig goes to the real store and, only when that answers success and
// is not a dry run, deletes the tokens the Kubeconfig's create made: a dry
// run (in the query or in the body) and a failed delete keep them, and
// another Kubeconfig's token stays.
func TestFacadeDeletingAKubeconfigDeletesItsTokens(t *testing.T) {
	t.Parallel()
	var storeStatus atomic.Int32
	storeStatus.Store(http.StatusOK)
	store := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(storeStatus.Load())) })
	f := testrancher.New(testrancher.Options{SeedToken: "e2e:secret", Fallthrough: store})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	create := func() (name, bearer string) {
		resp, obj := kubeconfigCreate(t, srv, "e2e:secret", "", map[string]any{})
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		cfg, err := clientcmd.Load([]byte(obj["status"].(map[string]any)["value"].(string)))
		require.NoError(t, err)
		require.Len(t, cfg.AuthInfos, 1)
		for _, a := range cfg.AuthInfos {
			bearer = a.Token
		}
		return obj["metadata"].(map[string]any)["name"].(string), bearer
	}
	deleteKubeconfig := func(name, query string, body any) int {
		return statusOfBody(t, srv.Client(), http.MethodDelete, srv.URL+testrancher.KubeconfigsCollectionPath+"/"+name+query, "e2e:secret", body)
	}
	name, bearer := create()
	_, otherBearer := create()
	require.True(t, f.ValidBearer(bearer))

	assert.Equal(t, http.StatusOK, deleteKubeconfig(name, "?dryRun=All", nil))
	assert.Equal(t, http.StatusOK, deleteKubeconfig(name, "", map[string]any{"dryRun": []string{"All"}}))
	assert.True(t, f.ValidBearer(bearer), "a dry-run delete keeps the token")

	storeStatus.Store(http.StatusNotFound)
	assert.Equal(t, http.StatusNotFound, deleteKubeconfig(name, "", nil), "the answer is the store's")
	assert.True(t, f.ValidBearer(bearer), "a failed delete keeps the token")

	storeStatus.Store(http.StatusOK)
	assert.Equal(t, http.StatusOK, deleteKubeconfig(name, "", nil))
	assert.False(t, f.ValidBearer(bearer), "the token dies with its Kubeconfig")
	assert.True(t, f.ValidBearer(otherBearer), "another Kubeconfig's token stays")
	assert.Equal(t, 1, f.TokenCount())
}

// TestFacadeTimesAreWholeSeconds asserts the creation time and the expiry of
// a token are whole seconds, as Rancher's are: the Norman createdTS is a
// multiple of 1000 milliseconds even when the clock is between seconds.
func TestFacadeTimesAreWholeSeconds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := testrancher.New(testrancher.Options{SeedToken: "e2e:secret", V3TokenCreate: true})
		srv := httptest.NewTestServer(t, f)
		_ = srv.Client()
		time.Sleep(1500 * time.Millisecond)
		norman := postJSON(t, srv.Client(), srv.URL+"/v3/tokens", "e2e:secret", map[string]any{"type": "token", "clusterId": "c-test"})
		assert.EqualValues(t, 0, int64(norman["createdTS"].(float64))%1000)
		ext := postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": map[string]any{}})
		created := expiryOf(t, ext["metadata"].(map[string]any)["creationTimestamp"])
		assert.Equal(t, created.Add(ninetyDays), expiryOf(t, ext["status"].(map[string]any)["expiresAt"]))
		assert.Equal(t, created.UnixMilli(), int64(norman["createdTS"].(float64)))
	})
}

// TestFacadeMaxTTLZeroOnTheExtRoute asserts AuthTokenMaxTTLZero on the ext
// create, as Rancher v2.15 answers it with auth-token-max-ttl-minutes set to
// 0: no ttl gives spec.ttl -1, an empty status.expiresAt and expired false,
// on every answer; a requested ttl is kept with no 90-day cap, a negative one
// too; the token never expires unless it asked for a finite ttl.
func TestFacadeMaxTTLZeroOnTheExtRoute(t *testing.T) {
	t.Parallel()
	f, srv := newFacade(t, testrancher.Options{SeedToken: "e2e:secret", AuthTokenMaxTTLZero: true}, &recordingHandler{})
	create := func(spec map[string]any) map[string]any {
		return postJSON(t, srv.Client(), srv.URL+extTokensPath, "e2e:secret", map[string]any{"spec": spec})
	}

	none := create(map[string]any{})
	noneSpec, noneStatus := none["spec"].(map[string]any), none["status"].(map[string]any)
	assert.EqualValues(t, -1, noneSpec["ttl"])
	assert.Equal(t, "", noneStatus["expiresAt"])
	assert.Contains(t, noneStatus, "expiresAt")
	assert.Equal(t, false, noneStatus["expired"])
	name := none["metadata"].(map[string]any)["name"].(string)
	path := srv.URL + extTokensPath + "/" + name

	_, getBody := doJSON(t, srv.Client(), http.MethodGet, path, nil, "e2e:secret")
	assert.Equal(t, "", decodeObject(t, getBody)["status"].(map[string]any)["expiresAt"])
	_, listBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath, nil, "e2e:secret")
	assert.Equal(t, "", decodeObject(t, listBody)["items"].([]any)[0].(map[string]any)["status"].(map[string]any)["expiresAt"])
	_, delBody := doJSON(t, srv.Client(), http.MethodDelete, path, map[string]any{"dryRun": []string{"All"}}, "e2e:secret")
	assert.Equal(t, "", decodeObject(t, delBody)["status"].(map[string]any)["expiresAt"])
	assert.True(t, f.ValidBearer(noneStatus["bearerToken"].(string)), "a token with no expiry authenticates")

	before := time.Now()
	long := create(map[string]any{"ttl": 2 * ninetyDaysMs})
	assert.EqualValues(t, 2*ninetyDaysMs, long["spec"].(map[string]any)["ttl"], "no 90-day cap")
	assert.WithinDuration(t, before.Add(2*ninetyDays), expiryOf(t, long["status"].(map[string]any)["expiresAt"]), 5*time.Second)

	negative := create(map[string]any{"ttl": -5000})
	assert.EqualValues(t, -5000, negative["spec"].(map[string]any)["ttl"])
	assert.Equal(t, "", negative["status"].(map[string]any)["expiresAt"])
}

// TestFacadeATokenCarriesTheProviderItsOwnerSignedInWith asserts that an
// azureAD sign-in's tokens name the azuread principal: an ext create, a
// Norman create and a Kubeconfig token made with that bearer answer
// azuread_user://<id> and provider azuread, while a local sign-in's stay local.
func TestFacadeATokenCarriesTheProviderItsOwnerSignedInWith(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{EnableIdP: true, IdPPendingPolls: 0, V3TokenCreate: true}, &recordingHandler{})
	resp, body := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login",
		map[string]string{"type": "azureADProvider", "configName": "azuread", "responseType": "json", "id_token": azureIDToken(t, srv)}, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	azure := decodeObject(t, body)["token"].(string)
	local := jsonLogin(t, srv)

	principalOf := func(tok map[string]any) (name, provider any) {
		p := tok["spec"].(map[string]any)["userPrincipal"].(map[string]any)
		return p["name"], p["provider"]
	}
	name, provider := principalOf(postJSON(t, srv.Client(), srv.URL+extTokensPath, azure, map[string]any{"spec": map[string]any{}}))
	assert.Equal(t, "azuread_user://user-e2e", name)
	assert.Equal(t, "azuread", provider)
	name, provider = principalOf(postJSON(t, srv.Client(), srv.URL+extTokensPath, local, map[string]any{"spec": map[string]any{}}))
	assert.Equal(t, "local://user-e2e", name)
	assert.Equal(t, "local", provider)

	norman := postJSON(t, srv.Client(), srv.URL+"/v3/tokens", azure, map[string]any{"type": "token", "clusterId": "c-test"})
	assert.Equal(t, "azuread", norman["authProvider"])
	assert.Contains(t, norman["userPrincipal"], "name:azuread_user://user-e2e")
	assert.Contains(t, norman["userPrincipal"], "provider:azuread")

	_, obj := kubeconfigCreate(t, srv, azure, "", map[string]any{})
	tokenName := obj["status"].(map[string]any)["tokens"].([]any)[0].(string)
	_, getBody := doJSON(t, srv.Client(), http.MethodGet, srv.URL+extTokensPath+"/"+tokenName, nil, azure)
	name, provider = principalOf(decodeObject(t, getBody))
	assert.Equal(t, "azuread_user://user-e2e", name)
	assert.Equal(t, "azuread", provider)
}
