package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userServer serves a management API with a mix of regular and system users,
// counting hits.
func userServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/users", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"u-alice"},"username":"alice","principalIds":["local://u-alice"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]},
			{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
			{"metadata":{"name":"u-nopids"},"username":"nopids"},
			{"metadata":{"name":"sameid"},"username":"sameid","principalIds":["local://sameid"]}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchUsersUnionAndSystemExclusion(t *testing.T) {
	t.Parallel()
	srv, _ := userServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	got := fetchUsers(t.Context(), srv.Client(), cap)
	// Expected: union of name+username for non-system users; sanitize drops
	// "bad name!"; dedup removes the u-bob/bob duplicate pair; sorted.
	// u-alice + alice, u-bob + bob (deduped from two rows), u-badname (name ok,
	// username "bad name!" dropped), u-nopids + nopids, sameid (name==username: appears once).
	// system user u-sys/system-svc excluded.
	assert.Equal(t, []string{"alice", "bob", "nopids", "sameid", "u-alice", "u-badname", "u-bob", "u-nopids"}, got)

	// Verify same-item dedupe: metadata.name == username must not appear twice.
	count := 0
	for _, c := range got {
		if c == "sameid" {
			count++
		}
	}
	assert.Equal(t, 1, count, "metadata.name == username must appear exactly once after dedupe")
}

// TestFetchUsersUsernameUniqueness covers the three cases from the reviewer's
// uniqueness rule: shared username suppressed, username colliding with another
// user's displayName suppressed, own displayName==username emitted.
func TestFetchUsersUsernameUniqueness(t *testing.T) {
	t.Parallel()

	t.Run("shared username not emitted; both ids present", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"u-dev-1"},"username":"dev","principalIds":["local://u-dev-1"]},
				{"metadata":{"name":"u-dev-2"},"username":"dev","principalIds":["local://u-dev-2"]}
			]}`))
		}))
		t.Cleanup(srv.Close)
		cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
		got := fetchUsers(t.Context(), srv.Client(), cap)
		assert.NotContains(t, got, "dev", "shared username must not be a candidate")
		assert.Contains(t, got, "u-dev-1")
		assert.Contains(t, got, "u-dev-2")
	})

	t.Run("username colliding with other user displayName not emitted; both ids present", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"u-ops-1"},"username":"ops","principalIds":["local://u-ops-1"]},
				{"metadata":{"name":"u-ops-2"},"username":"other","displayName":"ops","principalIds":["local://u-ops-2"]}
			]}`))
		}))
		t.Cleanup(srv.Close)
		cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
		got := fetchUsers(t.Context(), srv.Client(), cap)
		assert.NotContains(t, got, "ops", "username colliding with another user's displayName must not be a candidate")
		assert.Contains(t, got, "u-ops-1")
		assert.Contains(t, got, "u-ops-2")
		// "other" is unique (freq=1): should appear.
		assert.Contains(t, got, "other")
	})

	t.Run("own displayName equals own username (self-collision) is emitted", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"u-solo"},"username":"solo","displayName":"solo","principalIds":["local://u-solo"]}
			]}`))
		}))
		t.Cleanup(srv.Close)
		cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
		got := fetchUsers(t.Context(), srv.Client(), cap)
		assert.Contains(t, got, "solo", "self-collision (username==displayName) must not suppress the candidate")
		assert.Contains(t, got, "u-solo")
		// "solo" appears exactly once (self-collision does not double-count).
		count := 0
		for _, c := range got {
			if c == "solo" {
				count++
			}
		}
		assert.Equal(t, 1, count, "solo must appear exactly once")
	})
}

func TestFetchUsersTokenBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := userServer(t)
	tests := []struct {
		name string
		cap  TokenCapability
	}{
		{name: "no token", cap: TokenCapability{ServerURL: srv.URL}},
		{name: "IDOnly exec-cred", cap: TokenCapability{Token: "downstream", IDOnly: true, ServerURL: srv.URL}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, fetchUsers(t.Context(), srv.Client(), tt.cap))
		})
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
}

func TestFetchUsersFailEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	assert.Nil(t, fetchUsers(t.Context(), srv.Client(), cap))

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(srv2.Close)
	cap2 := TokenCapability{Token: "token-abc:secret", ServerURL: srv2.URL}
	require.Nil(t, fetchUsers(t.Context(), srv2.Client(), cap2))
}

func TestFetchUsersTimeout(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	assert.Nil(t, fetchUsers(ctx, srv.Client(), cap))
}
