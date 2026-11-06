package fidelity

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminOnlyTarget resolves the admin bearer and fails every other identity. A
// teardown step that needs another identity therefore fails in the live run as
// well.
type adminOnlyTarget struct {
	*fakeTarget
}

func (a adminOnlyTarget) Bearer(ctx context.Context, id Identity) (string, error) {
	if id != IdentityAdmin {
		return "", fmt.Errorf("teardown needs the %s bearer", id)
	}
	return a.fakeTarget.Bearer(ctx, id)
}

// probesWithoutTeardownRequests lists every probe whose teardown sends no
// request in the all-probes test, each with its reason.
var probesWithoutTeardownRequests = map[string]string{
	"access/standard-cloud-credential-by-name": "the credential id is captured by the setup step and not seeded",
	"access-downstream/member-cluster-token":   "the token name is captured by the setup step and not seeded",
	"access-downstream/owner-steve":            "the owner's login token and user id are captured by the setup step and not seeded",
	"access-downstream/owner-management-gate":  "the owner's login token and user id are captured by the setup step and not seeded",
	"serverfacts/install-uuid-no-global-role":  "the login token and user id are captured by the setup step and not seeded",
	"authproviders/external-providers-enabled": "a restore sends nothing for an auth config its enable step never wrote; TestAuthProvidersProbeRestoresWhenTheCallerContextIsCancelled covers the sends",
}

// teardownRun is what one run of a probe's teardown did.
type teardownRun struct {
	requests []string
	errText  string
	err      error
}

// runProbeTeardown runs the teardown of p once against its own server, which
// responds 200 to every DELETE and 404 to every other request, with the
// captures teardown paths need seeded.
func runProbeTeardown(ctx context.Context, p Probe) teardownRun {
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		status := http.StatusNotFound
		if r.Method == http.MethodDelete {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tgt := adminOnlyTarget{adminTarget(srv)}

	captured := map[string]string{}
	if p.Downstream {
		captured["downstream"] = testDownstream
	}
	for _, o := range p.Observations {
		for name := range o.Captures {
			captured[name] = "c-m-x:p-x"
		}
	}
	err := runTeardown(ctx, tgt, p, captured, 5*time.Second)
	run := teardownRun{err: err}
	if err != nil {
		run.errText = strings.ReplaceAll(err.Error(), srv.URL, "<server>")
	}
	mu.Lock()
	defer mu.Unlock()
	run.requests = append([]string(nil), requests...)
	return run
}

// TestEveryProbeTeardownSendsTheSameRequestsWhenTheCallerContextIsCancelled
// asserts that, for every probe with a teardown, the teardown sends the same
// requests and returns the same error text whether the caller's context is live
// or cancelled, and that the cancelled run carries no context error. A teardown
// step that uses a value captured by a setup step sends nothing in either run,
// because the test does not seed that value. The probes whose live run sends
// nothing must be exactly the probes in probesWithoutTeardownRequests. A
// change that makes more probes send nothing fails the test.
func TestEveryProbeTeardownSendsTheSameRequestsWhenTheCallerContextIsCancelled(t *testing.T) {
	t.Parallel()
	var withTeardown []Probe
	for _, p := range All() {
		if len(p.Teardown) > 0 {
			withTeardown = append(withTeardown, p)
		}
	}
	require.NotEmpty(t, withTeardown)

	var mu sync.Mutex
	compared := 0
	var sentNothing []string
	cancelledRequests := 0

	t.Run("probes", func(t *testing.T) {
		for _, p := range withTeardown {
			t.Run(p.Name, func(t *testing.T) {
				t.Parallel()
				live := runProbeTeardown(t.Context(), p)
				cancelled, cancel := context.WithCancel(t.Context())
				cancel()
				ended := runProbeTeardown(cancelled, p)

				assert.Equal(t, live.requests, ended.requests, "the same requests with the caller's context cancelled")
				assert.Equal(t, live.errText, ended.errText, "the same error text with the caller's context cancelled")
				require.NotErrorIs(t, ended.err, context.Canceled)
				require.NotErrorIs(t, ended.err, context.DeadlineExceeded)
				assert.NotContains(t, live.errText, "teardown needs", "no teardown step needs a bearer other than admin")

				mu.Lock()
				defer mu.Unlock()
				compared++
				cancelledRequests += len(ended.requests)
				if len(live.requests) == 0 {
					sentNothing = append(sentNothing, p.Name)
				}
			})
		}
	})

	assert.Equal(t, len(withTeardown), compared, "every probe with a teardown was compared")
	assert.Positive(t, cancelledRequests, "the cancelled runs sent requests")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(probesWithoutTeardownRequests)), sentNothing,
		"the probes whose live run sent nothing are the listed ones")
	t.Logf("probes compared: %d; probes whose live run sent a request: %d; probes whose live run sent nothing: %d",
		compared, compared-len(sentNothing), len(sentNothing))
}
