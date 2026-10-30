package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// sendFromASignIn calls the function the run registered, as a sign-in does,
// with the server as the origin and a login token.
func sendFromASignIn(t *testing.T, f *signInFixture) {
	t.Helper()
	httpx.SendSignInRequest(t.Context(), f.srv.URL, f.fetch(t))
}

// TestRunArgvRegistersTheSignInRequestOnlyForAParsedLine asserts the function
// a sign-in calls is registered once the line is parsed and not refused, and
// not for an empty command line, a completion request or a line the CLI
// refuses before urfave runs it: with the hook state on and the facts due,
// only the parsed line makes a sign-in send its install id request.
func TestRunArgvRegistersTheSignInRequestOnlyForAParsedLine(t *testing.T) {
	setup := func(t *testing.T) *signInFixture {
		t.Helper()
		f := newSignInFixture(t)
		f.srv.useServer(t)
		seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) { fc.FailInstallID(telemetry.FailureForbidden, time.Now().Add(-time.Hour)) })
		return f
	}
	t.Run("a parsed line", func(t *testing.T) {
		f := setup(t)
		restoreRawArgv(t)
		root := newRoot(t)
		root.Writer, root.ErrWriter = io.Discard, io.Discard
		_, _ = runArgv(t.Context(), root, []string{"rancher", "access", "clusters"})
		sendFromASignIn(t, f)
		assert.Len(t, f.srv.hitsOn(installIDPath), 1)
		assert.Equal(t, "Bearer "+signInLoginToken, f.srv.hitsOn(installIDPath)[0].header.Get("Authorization"))
	})
	for name, args := range map[string][]string{
		"an empty command line": {},
		"a completion request":  {"rancher", protoSentinel, "access", ""},
		"a refused line":        {"rancher", "get", "pods", "-", "x", "-o", "json"},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			restoreRawArgv(t)
			root := newRoot(t)
			root.ErrWriter, root.Writer = io.Discard, io.Discard
			noteRecording(f.dir, telemetry.ConfigUnset)
			f.setConfig(t, f.srv.URL)
			require.True(t, recordingStateNow().on)
			_, _ = runArgv(t.Context(), root, args)
			if len(args) > 0 && args[1] == protoSentinel {
				noteRecording(f.dir, telemetry.ConfigUnset)
			}
			f.srv.clearHits()
			sendFromASignIn(t, f)
			assert.Empty(t, f.srv.hitsOn(installIDPath), "nothing was registered")
		})
	}
}

// TestRunClearsTheFunctionAndTheAnswersOfThePreviousRun asserts a run drops
// what an earlier run in the process registered or held.
func TestRunClearsTheFunctionAndTheAnswersOfThePreviousRun(t *testing.T) {
	f := newSignInFixture(t)
	id, ver := goodAnswers()
	holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &id, version: &ver})
	var calls int
	httpx.RegisterSignInRequest(func(context.Context, string, httpx.InstallIDFunc) { calls++ })
	runRecorded(t, newRoot(t), "config", "path")
	httpx.SendSignInRequest(t.Context(), f.srv.URL, f.fetch(t))
	assert.Zero(t, calls, "the earlier function was dropped")
	_, held := heldFactsFor(f.canonical)
	assert.False(t, held, "the earlier answers were dropped")
}
