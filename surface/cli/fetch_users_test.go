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
	got := fetchUsers(context.Background(), srv.Client(), cap)
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
			assert.Nil(t, fetchUsers(context.Background(), srv.Client(), tt.cap))
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
	assert.Nil(t, fetchUsers(context.Background(), srv.Client(), cap))

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(srv2.Close)
	cap2 := TokenCapability{Token: "token-abc:secret", ServerURL: srv2.URL}
	require.Nil(t, fetchUsers(context.Background(), srv2.Client(), cap2))
}

func TestFetchUsersTimeout(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	assert.Nil(t, fetchUsers(ctx, srv.Client(), cap))
}
