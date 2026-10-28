package fidelity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/token"
)

const cannedInstallID = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"

var serverFactsNames = []string{
	"serverfacts/rancherversion", "serverfacts/install-uuid-standard", "serverfacts/install-uuid-admin",
	"serverfacts/install-uuid-no-global-role",
}

func serverFactsProbe(t *testing.T, name string) Probe {
	t.Helper()
	for _, p := range All() {
		if p.Name == name {
			return p
		}
	}
	require.FailNow(t, "no probe "+name)
	return Probe{}
}

// TestEveryServerFactsProbeIsOracleOnly asserts the family is the four
// probes and each is oracle-only: the facade serves neither path, so a probe
// that conformance replays against it would fail once its golden exists.
func TestEveryServerFactsProbeIsOracleOnly(t *testing.T) {
	t.Parallel()
	var got []string
	for _, p := range All() {
		if strings.HasPrefix(p.Name, "serverfacts/") {
			got = append(got, p.Name)
			assert.True(t, p.OracleOnly, p.Name)
			assert.False(t, p.Downstream, p.Name)
		}
	}
	assert.ElementsMatch(t, serverFactsNames, got)
}

// TestServerFactsProbesSendTheCLIsRequests asserts the paths, the identities
// and the headers: the version read carries no credential, the install id
// reads carry the Accept header the CLI sends, and the read of the user with
// no global role sends that user's login bearer with no identity.
func TestServerFactsProbesSendTheCLIsRequests(t *testing.T) {
	t.Parallel()
	one := func(name string) RequestSpec {
		p := serverFactsProbe(t, name)
		require.Len(t, p.Observations, 1, name)
		return p.Observations[0].Request
	}
	v := one("serverfacts/rancherversion")
	assert.Equal(t, http.MethodGet, v.Method)
	assert.Equal(t, "/rancherversion", v.Path)
	assert.Equal(t, IdentityNone, v.Identity)
	assert.Empty(t, v.Headers)

	for name, id := range map[string]Identity{
		"serverfacts/install-uuid-standard": IdentityStandard,
		"serverfacts/install-uuid-admin":    IdentityAdmin,
	} {
		r := one(name)
		assert.Equal(t, http.MethodGet, r.Method, name)
		assert.Equal(t, "/v1/management.cattle.io.settings/install-uuid", r.Path, name)
		assert.Equal(t, id, r.Identity, name)
		assert.Equal(t, map[string]string{"Accept": "application/json"}, r.Headers, name)
	}
	r := one("serverfacts/install-uuid-no-global-role")
	assert.Equal(t, "/v1/management.cattle.io.settings/install-uuid", r.Path)
	assert.Equal(t, IdentityNone, r.Identity)
	assert.Equal(t, "application/json", r.Headers["Accept"])
	assert.Equal(t, "Bearer {unrecorded.rolelessbearer}", r.Headers["Authorization"])
}

// TestTheInstallIDSetupStepWantsAUUID asserts the step passes for a lowercase
// hyphenated UUID and fails for any other value, an empty one, a status
// other than 200 and a body that is not JSON.
func TestTheInstallIDSetupStepWantsAUUID(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"a UUID", 200, `{"id":"install-uuid","value":"` + cannedInstallID + `"}`, ""},
		{"a value that is not a UUID", 200, `{"value":"not-a-uuid"}`, "not a lowercase hyphenated UUID"},
		{"an upper case UUID", 200, `{"value":"` + strings.ToUpper(cannedInstallID) + `"}`, "not a lowercase hyphenated UUID"},
		{"an empty value", 200, `{"value":""}`, "not a lowercase hyphenated UUID"},
		{"no value", 200, `{"id":"install-uuid"}`, "not a lowercase hyphenated UUID"},
		{"a refusal", 404, `{"type":"error"}`, "status 404"},
		{"a body that is not JSON", 200, `<html>`, "not JSON"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, installIDPath, r.URL.Path)
				assert.Equal(t, "Bearer admin-token", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Accept"))
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.body))
			}))
			defer srv.Close()
			tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "admin-token"}}
			err := installIDIsAUUID().Do(t.Context(), tgt, map[string]string{})
			if row.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, row.wantErr)
			assert.NotContains(t, err.Error(), "not-a-uuid", "the error never repeats the value")
		})
	}
}

// TestTheNormalizeRulesLeaveNoInstallIDInARecordedBody asserts a canned steve
// setting that holds the value in value and in metadata.fields records with
// the value nowhere in it, with the id and type kept and the envelope gone,
// and that the version rules drop only the commit.
func TestTheNormalizeRulesLeaveNoInstallIDInARecordedBody(t *testing.T) {
	t.Parallel()
	canned := func() map[string]any {
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(`{
			"id":"install-uuid","type":"management.cattle.io.setting",
			"links":{"self":"https://127.0.0.1:8443/v1/management.cattle.io.settings/install-uuid"},
			"actions":{"x":"https://127.0.0.1:8443/v1/management.cattle.io.settings/install-uuid?action=x"},
			"metadata":{"name":"install-uuid","uid":"`+cannedInstallID+`","fields":["install-uuid","`+cannedInstallID+`"],
				"state":{"name":"active"},"relationships":null,"resourceVersion":"123"},
			"value":"`+cannedInstallID+`"}`), &body))
		return body
	}
	for _, name := range []string{"serverfacts/install-uuid-standard", "serverfacts/install-uuid-admin", "serverfacts/install-uuid-no-global-role"} {
		p := serverFactsProbe(t, name)
		body := canned()
		Normalize(body, append(DefaultNormalizeRules(), p.Normalize...))
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), cannedInstallID, name)
		assert.Equal(t, map[string]any{"id": "install-uuid", "type": "management.cattle.io.setting", "value": "<install-id>"}, body, name)
	}

	p := serverFactsProbe(t, "serverfacts/rancherversion")
	var version map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"GitCommit":"272e65","RancherPrime":"false","Version":"v2.15.2"}`), &version))
	Normalize(version, append(DefaultNormalizeRules(), p.Normalize...))
	assert.Equal(t, map[string]any{"RancherPrime": "false", "Version": "v2.15.2"}, version)
}

// TestTheRoleLessProbeCreatesAndDeletesOnlyWhatItRecorded asserts the probe
// run against a fake server: it creates one user and signs it in, sends the
// read with that login's bearer and no other credential, and deletes the
// login token and the user by the ids it recorded, nothing else.
func TestTheRoleLessProbeCreatesAndDeletesOnlyWhatItRecorded(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var calls []string
	var readAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v3/users":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"u-fake1"}`))
		case "POST /v1-public/login":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"token-fake1","token":"token-fake1:secret"}`))
		case "GET " + installIDPath:
			readAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","code":"NotFound"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "admin-token"}}

	results, err := RunProbe(t.Context(), tgt, serverFactsProbe(t, "serverfacts/install-uuid-no-global-role"))
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, http.StatusNotFound, results[0].Status)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer token-fake1:secret", readAuth)
	assert.Equal(t, []string{
		"POST /v3/users",
		"POST /v1-public/login",
		"GET " + installIDPath,
		"DELETE " + token.V3TokensPath + "/token-fake1",
		"DELETE /v3/users/u-fake1",
	}, calls)
}
