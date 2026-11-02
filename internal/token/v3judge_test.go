package token

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// v3Stub answers the two requests that can delete a v3 token with fixed
// statuses: a status of 0 closes the connection with no answer. It records
// every request.
type v3Stub struct {
	srv *httptest.Server
	mu  sync.Mutex
	// logouts and deletes hold the bearer of each request.
	logouts, deletes []string
}

func newV3Stub(t *testing.T, logout, del int) *v3Stub {
	t.Helper()
	s := &v3Stub{}
	answer := func(w http.ResponseWriter, status int) {
		if status == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(status)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == LogoutPath:
			s.logouts = append(s.logouts, bearer)
			answer(w, logout)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, V3TokensPath+"/"):
			s.deletes = append(s.deletes, bearer)
			answer(w, del)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// seen returns the bearers of the logout and delete requests so far.
func (s *v3Stub) seen() (logouts, deletes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.logouts), slices.Clone(s.deletes)
}

const (
	stubV3Name   = "kubeconfig-u-1"
	stubV3Bearer = stubV3Name + ":secret"
)

// stubV3Fixture is a config directory whose entry prod holds no login and
// one unscoped v3 cache record, on the server the stub answers for.
func stubV3Fixture(t *testing.T, stub *v3Stub) *revFixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	x := &revFixture{t: t, srv: stub.srv, url: stub.srv.URL, dir: t.TempDir()}
	x.store = config.NewFileStore(x.dir)
	cfg := &config.Config{Servers: map[string]config.Server{}, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(x.dir))
	require.NoError(t, cfg.Write())
	x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenUserID: "user-alice"})
	ref := &TokenRef{Kind: TokenKindV3, Name: stubV3Name}
	b := x.own("prod").WithCreatedToken(ref, "user-alice")
	require.NoError(t, x.store.SetCachedCred("k", stubV3Bearer, time.Now().Add(time.Hour), "k", config.CanonicalServerIdentity(x.url), "", b))
	return x
}

// v3Row is one pair of answers to the requests that delete a v3 token with
// its own bearer, and how the item ends. A logout or delete of 0 is no
// answer (a transport error); logoutNotSent says the logout was never
// made, which the planner never does and judge must still read. errText
// is the item's error and stampErr the error the record is stamped with,
// each a prefix when prefix is set (a transport error's text varies).
type v3Row struct {
	name          string
	logout, del   int
	logoutNotSent bool
	delNotSent    bool
	revoked       bool
	reason        string
	errText       string
	stampResult   clierr.RevokeResult
	stampErr      string
	prefix        bool
}

func v3Rows() []v3Row {
	const (
		forbidden = "delete 403 Forbidden"
		server    = "delete 500 Internal Server Error"
	)
	return []v3Row{
		{name: "logout 200, delete not sent", logout: 200, delNotSent: true, revoked: true},
		{name: "logout 204, delete not sent", logout: 204, delNotSent: true, revoked: true},
		{name: "logout 401, delete 200", logout: 401, del: 200, revoked: true},
		{name: "logout 401, delete 403", logout: 401, del: 403, reason: "unconfirmed", stampResult: clierr.ResultFailed, stampErr: "logout 401; " + forbidden},
		{name: "logout 401, delete 404", logout: 401, del: 404, reason: "unconfirmed", stampResult: clierr.ResultAlreadyGone},
		{name: "logout 401, delete 500", logout: 401, del: 500, reason: "unconfirmed", stampResult: clierr.ResultFailed, stampErr: "logout 401; " + server},
		{name: "logout 401, delete transport error", logout: 401, del: 0, reason: "unconfirmed", stampResult: clierr.ResultFailed, stampErr: "logout 401; delete ", prefix: true},
		{name: "logout 500, delete 401", logout: 500, del: 401, reason: "failed", errText: "logout 500; delete 401 Unauthorized", stampResult: clierr.ResultFailed, stampErr: "logout 500; delete 401 Unauthorized"},
		{name: "logout 500, delete 403", logout: 500, del: 403, reason: "failed", errText: "logout 500; " + forbidden, stampResult: clierr.ResultFailed, stampErr: "logout 500; " + forbidden},
		{name: "logout 500, delete 500", logout: 500, del: 500, reason: "failed", errText: "logout 500; " + server, stampResult: clierr.ResultFailed, stampErr: "logout 500; " + server},
		{name: "logout 500, delete 404", logout: 500, del: 404, reason: "unconfirmed", stampResult: clierr.ResultAlreadyGone},
		{name: "logout 500, delete 200", logout: 500, del: 200, revoked: true},
		{name: "logout 500, delete transport error", logout: 500, del: 0, reason: "failed", errText: "logout 500; delete ", stampResult: clierr.ResultFailed, stampErr: "logout 500; delete ", prefix: true},
		{name: "logout 401, delete 204", logout: 401, del: 204, revoked: true},
		{name: "logout 404, delete 403", logout: 404, del: 403, reason: "failed", errText: "logout 404; " + forbidden, stampResult: clierr.ResultFailed, stampErr: "logout 404; " + forbidden},
		{name: "logout transport error, delete 403", logout: 0, del: 403, reason: "failed", errText: "logout ", stampResult: clierr.ResultFailed, stampErr: "logout ", prefix: true},
		{name: "logout transport error, delete 404", logout: 0, del: 404, reason: "unconfirmed", stampResult: clierr.ResultAlreadyGone},
		{name: "logout transport error, delete transport error", logout: 0, del: 0, reason: "failed", errText: "logout ", stampResult: clierr.ResultFailed, stampErr: "logout ", prefix: true},
		{name: "logout not sent, delete 401", logoutNotSent: true, del: 401, reason: "failed", errText: "401 Unauthorized"},
		{name: "logout not sent, delete 403", logoutNotSent: true, del: 403, reason: "failed", errText: "403 Forbidden"},
		{name: "logout not sent, delete 404", logoutNotSent: true, del: 404, reason: "unconfirmed"},
	}
}

func assertText(t *testing.T, want, got string, prefix bool) {
	t.Helper()
	if prefix {
		assert.True(t, strings.HasPrefix(got, want), "want a prefix %q, got %q", want, got)
		return
	}
	assert.Equal(t, want, got)
}

// v3AdminDeleteCommand is the administrator's delete of the v3 token name.
func v3AdminDeleteCommand(name string) string {
	return clierr.Rancher + " delete management-token '" + name + "' --apply --yes --allow-deprecated"
}

// TestJudgeAV3TokenWithItsOwnBearer asserts the verdict for every pair of
// logout and delete answers: a 200 or 204 from either request revokes, a
// 404 to the delete or a 401 to the logout leaves the token unconfirmed, any
// other pair fails, and none is forbidden. Each row that sends requests
// runs through a revoke of an unscoped cache record and asserts the report,
// the stamp on the record kept, the retry and the recovery; the rows whose
// logout is never sent assert the judge alone.
func TestJudgeAV3TokenWithItsOwnBearer(t *testing.T) {
	for _, tc := range v3Rows() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.logoutNotSent {
				out := RevokeOutcome{Status: tc.del}
				switch tc.del {
				case http.StatusNotFound:
					out.Result = ResultAlreadyGone
				default:
					out.Result, out.Error = ResultFailed, strings.TrimSpace(http.StatusText(tc.del))
					out.Error = strconv.Itoa(tc.del) + " " + out.Error
				}
				it := &revocationItem{ref: &TokenRef{Kind: TokenKindV3, Name: stubV3Name}, self: true}
				it.judge(out)
				assert.Equal(t, tc.revoked, it.revoked)
				assert.Equal(t, tc.reason, it.reason)
				assert.Equal(t, tc.errText, it.errText)
				return
			}
			stub := newV3Stub(t, tc.logout, tc.del)
			x := stubV3Fixture(t, stub)
			_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)

			logouts, deletes := stub.seen()
			assert.Equal(t, []string{stubV3Bearer}, logouts, "one logout, with the token's own bearer")
			if tc.delNotSent {
				assert.Empty(t, deletes)
			} else {
				require.NotEmpty(t, deletes)
				assert.Equal(t, stubV3Bearer, deletes[0], "the delete carries the token's own bearer")
			}
			recs := x.load().Servers["prod"].SupersededTokens
			if tc.revoked {
				assert.Contains(t, revokedNames(rep), stubV3Name)
				assert.Empty(t, recs, "a revoked token's record is removed")
				return
			}
			n := notRevoked(t, rep, stubV3Name)
			assert.Equal(t, tc.reason, n.Reason)
			assertText(t, tc.errText, n.Error, tc.prefix)
			assert.Equal(t, "manual", n.Retry)
			assert.Equal(t, []string{v3AdminDeleteCommand(stubV3Name)}, n.Recovery)
			assert.Equal(t, "prod", n.Entry)
			require.Len(t, recs, 1, "the record of a token not revoked is kept")
			require.NotNil(t, recs[0].LastAttempt, "and stamped")
			assert.Equal(t, tc.stampResult, recs[0].LastAttempt.Result)
			assertText(t, tc.stampErr, recs[0].LastAttempt.Error, tc.prefix)
		})
	}
}
