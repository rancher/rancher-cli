package cli

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// factsBudget is how long the requests of one refresh share: one deadline,
// counted from the start of the first, or from the start of the sign-in's
// request when the sign-in sent one. A package var so tests shrink it.
var factsBudget = 2 * time.Second

// factsDeadline is the time the requests of a refresh that started at start
// must end by.
func factsDeadline(start time.Time) time.Time { return start.Add(factsBudget) }

// The two requests a refresh sends and the write of the facts file. Tests
// replace them to make one panic.
var (
	saveServerFacts      = telemetry.SaveFacts
	sendVersionRequest   = httpx.RequestVersion
	sendInstallIDRequest = func(ctx context.Context, f httpx.InstallIDFetcher) (string, int, error) {
		return f.Fetch(ctx)
	}
)

// refreshInput is what a run that recorded hands the refresh: the server it
// recorded, the options the event was recorded under, and what the event read
// of the server's facts file. The zero value refreshes nothing.
type refreshInput struct {
	server string
	opts   telemetry.Options
	start  telemetry.FactsRead
}

// refreshAnswers are the answers gathered for the records, one per record; a
// nil answer means the record has none.
type refreshAnswers struct {
	version, installID *telemetry.Answer
}

// refreshTarget is what the requests of one refresh can use: the transport
// the server's host answered through, and the install id fetcher when it
// targets the same server.
type refreshTarget struct {
	origin     string
	transport  http.RoundTripper
	fetcher    httpx.InstallIDFetcher
	hasFetcher bool
}

// refreshRecord names a record of the facts file.
type refreshRecord int

const (
	versionRecord refreshRecord = iota
	installIDRecord
)

// answeredHostFor returns the host that answered and compares equal to
// server, the one recorded last among several.
func answeredHostFor(server string) (httpx.AnsweredHost, bool) {
	for _, h := range slices.Backward(httpx.AnsweredHosts()) {
		if config.SameServerURL(h.Origin, server) {
			return h, true
		}
	}
	return httpx.AnsweredHost{}, false
}

// refreshTargetFor returns what the refresh for server can use, and false
// when no host that answered matches the server.
func refreshTargetFor(server string) (refreshTarget, bool) {
	host, ok := answeredHostFor(server)
	if !ok {
		return refreshTarget{}, false
	}
	t := refreshTarget{origin: host.Origin, transport: host.Transport}
	if f, ok := httpx.RegisteredInstallIDFetcher(); ok && config.SameServerURL(f.Origin, server) {
		t.fetcher, t.hasFetcher = f, true
	}
	return t, true
}

// requestFor returns the request that fills one record, or nil when the
// record is not due at now or its request has nothing to send through: the
// version needs the transport the server answered through, the install id a
// fetcher registered for the same server; a fetcher registered with the skip
// asks again after any stored failure of the install id record. A request
// returns the answer to store; runCtx is the run's own context, to tell a
// request the run ended from one that failed.
func requestFor(runCtx context.Context, rec refreshRecord, facts telemetry.Facts, at time.Time, t refreshTarget) func(context.Context) telemetry.Answer {
	switch rec {
	case versionRecord:
		if !facts.VersionDue(at) || t.transport == nil {
			return nil
		}
		return func(ctx context.Context) telemetry.Answer { return versionAnswer(ctx, runCtx, t) }
	case installIDRecord:
		if !t.hasFetcher {
			return nil
		}
		due := facts.InstallIDDue
		if t.fetcher.SkipFailureWait {
			due = facts.InstallIDDueSkippingFailureWait
		}
		if !due(at) {
			return nil
		}
		return func(ctx context.Context) telemetry.Answer { return installIDAnswer(ctx, runCtx, t.fetcher) }
	}
	return nil
}

// versionAnswer sends the version request and turns what came back into the
// answer to store.
func versionAnswer(ctx, runCtx context.Context, t refreshTarget) telemetry.Answer {
	resp, err := sendVersionRequest(ctx, t.transport, t.origin)
	if err != nil {
		return telemetry.Answer{Cancelled: runCtx.Err() != nil}
	}
	a := telemetry.Answer{Status: resp.Status}
	if resp.Status >= 200 && resp.Status < 300 && !resp.TooLong {
		if info, err := provider.DecodeServerInfo(resp.Body); err == nil {
			a.Value = info.Version
			a.Edition = telemetryEdition(info.Edition)
		}
	}
	return a
}

// installIDAnswer sends the install id request and turns what came back into
// the answer to store.
func installIDAnswer(ctx, runCtx context.Context, f httpx.InstallIDFetcher) telemetry.Answer {
	value, status, err := sendInstallIDRequest(ctx, f)
	if err != nil {
		return telemetry.Answer{Cancelled: runCtx.Err() != nil}
	}
	return telemetry.Answer{Status: status, Value: value}
}

func telemetryEdition(e provider.Edition) telemetry.Edition {
	switch e {
	case provider.EditionCommunity:
		return telemetry.EditionCommunity
	case provider.EditionPrime:
		return telemetry.EditionPrime
	}
	return telemetry.EditionUnknown
}

// sendRequest runs one request and recovers a panic: the record then stores a
// network failure, which its hour wait bounds.
func sendRequest(ctx context.Context, send func(context.Context) telemetry.Answer) (a telemetry.Answer) {
	defer func() {
		if recover() != nil {
			a = telemetry.Answer{}
		}
	}()
	return send(ctx)
}

// refreshServerFacts asks the server the run reached for the version record
// and the install id record that are due, and writes the facts file once
// when a record changed. It runs after the event is recorded and never changes
// the run: it prints nothing, a failure is dropped, and a panic is recovered.
// A request the sign-in already sent is not sent again: its answer is written
// with the others, and the budget counts from when the sign-in's request
// started. A request that would start at or after the deadline is not sent
// and stores nothing.
// It does nothing when the run recorded no server, when the event could not
// read the facts file, when the run's context is done, or when no host that
// answered matches the server.
func refreshServerFacts(ctx context.Context, in refreshInput) {
	defer func() { _ = recover() }()
	if in.server == "" || !in.start.Usable || ctx.Err() != nil {
		return
	}
	target, ok := refreshTargetFor(in.server)
	if !ok {
		return
	}
	held, isHeld := heldFactsFor(in.server)
	facts := in.start.Facts
	at := time.Now()
	var versionReq, installIDReq func(context.Context) telemetry.Answer
	if held.version == nil {
		versionReq = requestFor(ctx, versionRecord, facts, at, target)
	}
	if held.installID == nil {
		installIDReq = requestFor(ctx, installIDRecord, facts, at, target)
	}
	if versionReq == nil && installIDReq == nil && !isHeld {
		return
	}
	deadline := factsDeadline(time.Now())
	if isHeld {
		deadline = factsDeadline(held.start)
	}
	answers := refreshAnswers{version: held.version, installID: held.installID}
	if time.Now().Before(deadline) {
		reqCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var wg sync.WaitGroup
		if versionReq != nil {
			wg.Go(func() {
				a := sendRequest(reqCtx, versionReq)
				answers.version = &a
			})
		}
		if installIDReq != nil {
			wg.Go(func() {
				a := sendRequest(reqCtx, installIDReq)
				answers.installID = &a
			})
		}
		wg.Wait()
	}
	stored := time.Now()
	versionChanged := answers.version != nil && facts.StoreVersion(*answers.version, stored)
	installIDChanged := answers.installID != nil && facts.StoreInstallID(*answers.installID, stored)
	if versionChanged || installIDChanged {
		saveServerFacts(in.server, facts, in.opts)
	}
}
