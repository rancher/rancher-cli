package cli

import (
	"context"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// heldFacts is what the facts requests of a sign-in left for the refresh
// after the event: the canonical server they were for, the time the sign-in
// request started (the 2 second budget counts from it), the install id answer
// and the version answer. A nil answer means that request did not go out; a
// start with no install id answer means the budget ran out before any request
// was sent. They are held in memory for one run and written only by the
// refresh.
type heldFacts struct {
	server    string
	start     time.Time
	installID *telemetry.Answer
	version   *telemetry.Answer
}

var (
	heldFactsMu    sync.Mutex
	heldFactsOfRun heldFacts
)

// clearHeldFacts drops what the sign-in held. run calls it before each run.
func clearHeldFacts() {
	heldFactsMu.Lock()
	defer heldFactsMu.Unlock()
	heldFactsOfRun = heldFacts{}
}

func holdFacts(h heldFacts) {
	heldFactsMu.Lock()
	defer heldFactsMu.Unlock()
	heldFactsOfRun = h
}

// heldFactsFor returns what the sign-in held when it held anything for
// server.
func heldFactsFor(server string) (heldFacts, bool) {
	heldFactsMu.Lock()
	defer heldFactsMu.Unlock()
	h := heldFactsOfRun
	if h.start.IsZero() || !config.SameServerURL(h.server, server) {
		return heldFacts{}, false
	}
	return h, true
}

// requestFactsDuringSignIn is the function runArgv registers for a sign-in
// to call after its cluster token exists and before its login token is
// deleted. It sends the install id request with fetch, and the version
// request when the version is due and a host answered, together and under one
// deadline counted from the call, and holds the answers for the refresh. It
// returns at the first check that fails and sends and holds nothing then; the
// checks are the ones the refresh makes for the same line: the line was
// parsed, recording is on, the command and server are recorded, origin is
// that server, the facts file can be read and the install id is due whatever
// failure it stores. A run that created the config directory is the one case
// where the directory is missing at the start: only the commands that may
// create it and write the server's facts file ask, and nothing is read. It
// never writes the facts file and never prints.
func requestFactsDuringSignIn(ctx context.Context, k keptLine, origin string, fetch httpx.InstallIDFunc) {
	start := time.Now()
	if ctx.Err() != nil || !k.parsed {
		return
	}
	st := recordingStateNow()
	dirExisted := st.dirExisted
	opts := telemetry.Options{ConfigDir: st.dir, DirExisted: dirExisted, Getenv: os.Getenv, Config: st.setting}
	st.dirExisted = true
	d, ok := recordDecision(k, st)
	if !ok || (!dirExisted && !slices.Contains(firstLoginCommands, d.Name)) {
		return
	}
	server := recordedServer(d, k.line, storedConfigSnapshot())
	if server == "" || !config.SameServerURL(origin, server) {
		return
	}
	var facts telemetry.Facts
	if dirExisted {
		read := telemetry.ReadServerFacts(server, opts)
		if !read.Usable {
			return
		}
		facts = read.Facts
	}
	at := time.Now()
	if !facts.InstallIDDueSkippingFailureWait(at) {
		return
	}
	if !at.Before(factsDeadline(start)) {
		holdFacts(heldFacts{server: server, start: start})
		return
	}
	host, found := answeredHostFor(server)
	askVersion := found && facts.VersionDue(at)
	reqCtx, cancel := context.WithDeadline(ctx, factsDeadline(start))
	defer cancel()
	held := heldFacts{server: server, start: start}
	var wg sync.WaitGroup
	wg.Go(func() {
		f := httpx.InstallIDFetcher{Origin: origin, Fetch: fetch}
		a := sendRequest(reqCtx, func(c context.Context) telemetry.Answer { return installIDAnswer(c, ctx, f) })
		held.installID = &a
	})
	if askVersion {
		wg.Go(func() {
			t := refreshTarget{origin: host.Origin, transport: host.Transport}
			a := sendRequest(reqCtx, func(c context.Context) telemetry.Answer { return versionAnswer(c, ctx, t) })
			held.version = &a
		})
	}
	wg.Wait()
	holdFacts(held)
}
