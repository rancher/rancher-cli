package access

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// The goroutineleak profile is process-global: it counts every goroutine
// in the binary that is blocked on a channel or a lock nothing live can
// still release, whichever test left it behind. The access tests run in
// parallel, so a per-test assertion has no ownership boundary. Instead
// each scenario below runs alone in a fresh copy of this test binary and
// the profile is read there, after the scenario released every blocker it
// planted on purpose.

// leakScenarioEnv names the one scenario a child copy of the binary runs.
// Unset in the parent, where the child test skips.
const leakScenarioEnv = "ACCESS_LEAK_SCENARIO"

// plantedScenario is the scenario that leaks one goroutine on purpose; it
// proves the detector runs and attributes a leak before the other
// scenarios gate anything.
const plantedScenario = "planted"

// leakScenarios are the fan-out shapes the package runs, one function per
// way a run can end. Each restores what it changed so the child measures
// the shape, not the fixture.
var leakScenarios = map[string]func(t *testing.T){
	plantedScenario: func(t *testing.T) {
		t.Helper()
		started := make(chan struct{})
		go plantedLeak(make(chan struct{}), started)
		<-started
	},
	"fan-out-answers": func(t *testing.T) {
		t.Helper()
		setFanOutLimit(t, 2)
		members, terminal := runFanOut(t.Context(), []string{"a", "b", "c", "d", "e"}, func(_ context.Context, k string) (string, reviewResult) {
			return k, reviewResult{kind: outcomeAnswer}
		})
		require.Nil(t, terminal)
		require.Len(t, members, 5)
	},
	"fan-out-terminal": func(t *testing.T) {
		t.Helper()
		// Two workers: a turns terminal once b is in flight, b then sees the
		// cancellation and c never starts, so a worker exits from each of
		// the pool's three paths.
		setFanOutLimit(t, 2)
		bStarted := make(chan struct{})
		_, terminal := runFanOut(t.Context(), []string{"a", "b", "c"}, func(ctx context.Context, k string) (string, reviewResult) {
			switch k {
			case "a":
				<-bStarted
				return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, "endpoint gone")}
			case "b":
				close(bStarted)
			}
			<-ctx.Done()
			return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, ctx.Err().Error())}
		})
		require.NotNil(t, terminal)
		require.ErrorContains(t, terminal, "endpoint gone")
	},
	"fan-out-budget": func(t *testing.T) {
		t.Helper()
		setFanOutLimit(t, 2)
		ctx, cancel := fanOutContext(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, terminal := runFanOut(ctx, []string{"a", "b", "c"}, func(ctx context.Context, k string) (string, reviewResult) {
			<-ctx.Done()
			return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, ctx.Err().Error())}
		})
		require.NotNil(t, terminal)
		require.ErrorContains(t, terminal, "fan-out budget")
	},
	"probe-caller": func(t *testing.T) {
		t.Helper()
		// The caller is admitted to prod and refused on edge: one probe of
		// each outcome, plus the local row from the identity review.
		cmd, srv := leakClustersCmd(t, whoamiResp("u-alice"), map[string]engine.ClusterClient{
			"c-prod": scriptedClient(whoamiResp("u-alice")),
			"c-edge": scriptedClient(http403("")),
		}, nil)
		defer srv.Close()
		res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
		require.NoError(t, err)
		require.Len(t, decodeClusters(t, res).Clusters, 3)
	},
	"probe-terminal-sibling": func(t *testing.T) {
		t.Helper()
		// A refused impersonation on prod is the answer while the edge
		// probe is parked; the runner must cancel and collect it. The
		// refusal waits for the edge probe to park, so the parked sibling
		// is in flight when the terminal answer lands, every time.
		edgeParked := make(chan struct{})
		edge := scriptedClient(whoamiResp("u-alice"))
		edge.Before = func(ctx context.Context) error {
			close(edgeParked)
			<-ctx.Done()
			return ctx.Err()
		}
		prod := scriptedClient(http403(rancherImpersonateUserRefusal))
		prod.Before = func(context.Context) error {
			<-edgeParked
			return nil
		}
		cmd, srv := leakClustersCmd(t, whoamiResp("u-admin"), nil, map[string]engine.ClusterClient{
			"c-edge\x00u-alice": edge,
			"c-prod\x00u-alice": prod,
		})
		defer srv.Close()
		_, err := cmd.Execute(t.Context(), clustersInput("u-alice", ""), nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		require.Equal(t, clierr.CodeRBAC, ce.Code)
		require.ErrorContains(t, err, "not allowed to impersonate user")
	},
	"probe-budget": func(t *testing.T) {
		t.Helper()
		// Every probe parks until the budget expires; the runner must
		// collect all of them on the way to the budget error.
		setFanOutLimit(t, 2)
		release := make(chan struct{})
		var inFlight, peak atomic.Int32
		clients := map[string]engine.ClusterClient{}
		for i := range 5 {
			id := fmt.Sprintf("c-%d", i)
			clients[id] = &blockingClient{fakeClient: scriptedClient(whoamiResp("u-alice")), release: release, inFlight: &inFlight, peak: &peak}
		}
		cmd, srv := leakClustersCmd(t, whoamiResp("u-alice"), clients, nil)
		defer srv.Close()
		_, err := cmd.Execute(t.Context(), fakeInput{strs: map[string]string{flagTimeout: "50ms"}}, nil)
		require.ErrorContains(t, err, "fan-out budget")
		require.Zero(t, inFlight.Load(), "every parked probe was cancelled")
	},
}

// plantedLeak parks on ch, which the planted scenario hands over without
// keeping a reference; nothing live can release it, so the detector must
// report this function's name.
func plantedLeak(ch <-chan struct{}, started chan<- struct{}) {
	started <- struct{}{}
	<-ch
}

func setFanOutLimit(t *testing.T, n int) {
	t.Helper()
	old := fanOutLimit
	fanOutLimit = n
	t.Cleanup(func() { fanOutLimit = old })
}

// leakClustersCmd is a clusters command over a loopback steve server that
// lists exactly the clusters given, with the caller's management identity
// answered by whoami. The server is returned so a scenario can close it,
// and so the pooled connections behind it, before the profile is read.
func leakClustersCmd(t *testing.T, whoami engine.ReviewResponse, clusters, impersonated map[string]engine.ClusterClient) (*clustersCmd, *httptest.Server) {
	t.Helper()
	local := provenanceFixture()
	local.script = []scripted{{resp: whoami}}
	f := &fakeFactory{caller: local, clusterClients: clusters, impersonatedClients: impersonated}
	ids := map[string]bool{}
	for id := range clusters {
		ids[id] = true
	}
	for key := range impersonated {
		id, _, _ := strings.Cut(key, "\x00")
		ids[id] = true
	}
	var items []map[string]any
	for id := range ids {
		items = append(items, steveItem(id, id))
	}
	srv := clustersServer(t, items...)
	return &clustersCmd{clients: f, roots: fakeRootFactory{fakeRootClient{srv}}, local: local}, srv
}

// TestGoroutineLeakScenario is the child half: it runs the one scenario
// the environment names, then reads the goroutineleak profile. Writing
// the profile is what runs the leak-detection garbage collection, and
// Count reports that collection's result, so the write comes first. In
// the parent binary the variable is unset and the test skips.
func TestGoroutineLeakScenario(t *testing.T) {
	name := os.Getenv(leakScenarioEnv)
	if name == "" {
		t.Skip("child half of the goroutine-leak tests; run through runLeakScenario")
	}
	scenario, ok := leakScenarios[name]
	require.True(t, ok, "unknown scenario %q", name)
	scenario(t)
	profile := pprof.Lookup("goroutineleak")
	require.NotNil(t, profile)
	var report bytes.Buffer
	require.NoError(t, profile.WriteTo(&report, 1))
	if name == plantedScenario {
		require.NotZero(t, profile.Count(), "the planted leak was not detected")
		require.Contains(t, report.String(), "plantedLeak", "the planted leak was not attributed")
		return
	}
	require.Zero(t, profile.Count(), report.String())
}

// runLeakScenario runs one scenario in a fresh copy of this test binary
// and returns the child's combined output; the child's exit status is the
// verdict.
func runLeakScenario(t *testing.T, name string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestGoroutineLeakScenario$", "-test.count=1", "-test.timeout=60s", "-test.v")
	cmd.Env = append(os.Environ(), leakScenarioEnv+"="+name)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireLeakDetector fails the calling test unless a child copy of the
// binary detects and attributes the planted leak, so a detector that runs
// but sees nothing cannot let the positive scenarios pass.
func requireLeakDetector(t *testing.T) {
	t.Helper()
	out, err := runLeakScenario(t, plantedScenario)
	require.NoError(t, err, "the leak detector did not report the planted leak:\n%s", out)
	assert.Contains(t, out, "PASS: TestGoroutineLeakScenario")
}

// TestFanOutNoGoroutineLeak asserts the fan-out worker pool leaves no
// goroutine behind however a run ends: every member answered, a terminal
// member cancelled the rest, or the budget expired.
func TestFanOutNoGoroutineLeak(t *testing.T) {
	requireLeakDetector(t)
	for _, name := range []string{"fan-out-answers", "fan-out-terminal", "fan-out-budget"} {
		t.Run(name, func(t *testing.T) {
			out, err := runLeakScenario(t, name)
			require.NoError(t, err, out)
		})
	}
}

// TestClusterProbeRunnerNoGoroutineLeak asserts the cluster probe runner
// leaves no goroutine behind however a run ends: every probe answered, a
// terminal answer cancelled a parked sibling, or the budget expired with
// every probe parked.
func TestClusterProbeRunnerNoGoroutineLeak(t *testing.T) {
	requireLeakDetector(t)
	for _, name := range []string{"probe-caller", "probe-terminal-sibling", "probe-budget"} {
		t.Run(name, func(t *testing.T) {
			out, err := runLeakScenario(t, name)
			require.NoError(t, err, out)
		})
	}
}
