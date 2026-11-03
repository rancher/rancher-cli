package telemetrycmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/ui"
)

// runUpload runs the command with the function and the flags and returns its
// result and error.
func runUpload(t *testing.T, dir string, fn telemetry.UploadFunc, flags map[string]bool) (rt.Result, error) {
	t.Helper()
	cmd := NewUpload(dir, fn)
	return cmd.(rt.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: flags}, ui.Discard())
}

func replyWith(res telemetry.UploadResult) telemetry.UploadFunc {
	return func(context.Context, bool, telemetry.Decision) telemetry.UploadResult { return res }
}

// TestUploadCommandDescribesItself asserts that Describe works with no
// function and calls none, that the command is local with a text shape, takes no
// argument and no flag, and that Execute with no function returns a generic
// error and does not panic.
func TestUploadCommandDescribesItself(t *testing.T) {
	calls := 0
	d := NewUpload(newDir(t), func(context.Context, bool, telemetry.Decision) telemetry.UploadResult {
		calls++
		return telemetry.UploadResult{}
	}).Describe()
	assert.Zero(t, calls)
	assert.Equal(t, rt.Local, d.CommandType)
	assert.Equal(t, rt.ShapeText, d.Shape)
	assert.Equal(t, "telemetry upload", d.Name)
	assert.Empty(t, d.Args)
	assert.Empty(t, d.Flags)
	assert.NotContains(t, d.Desc, ", so")
	assert.Contains(t, d.Desc, "sent counts every event in a request the endpoint answered with a 2xx, including the ones it refused")

	nilDescribe := NewUpload(newDir(t), nil).Describe()
	assert.Equal(t, d.Name, nilDescribe.Name)

	controlsEnv(t)
	var err error
	require.NotPanics(t, func() { _, err = runUpload(t, newDir(t), nil, nil) })
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

// TestUploadCommandPassesTheDecisionAndTheDebugFlag asserts that the command
// resolves the recording decision from config.yaml and passes it, and the
// value of the debug flag, to the function.
func TestUploadCommandPassesTheDecisionAndTheDebugFlag(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	writeConfig(t, dir, "telemetry: \"false\"\n")
	var got telemetry.Decision
	var debug bool
	fn := func(_ context.Context, d bool, rec telemetry.Decision) telemetry.UploadResult {
		debug, got = d, rec
		return telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}
	}
	_, err := runUpload(t, dir, fn, map[string]bool{"debug": true})
	require.NoError(t, err)
	assert.True(t, debug)
	assert.Equal(t, telemetry.Decision{On: false, By: telemetry.ControlConfig}, got)

	_, err = runUpload(t, dir, fn, nil)
	require.NoError(t, err)
	assert.False(t, debug)
}

// TestUploadCommandFailsOnAnUnreadableConfig asserts the command stops with the
// error of the configuration load and never calls the function.
func TestUploadCommandFailsOnAnUnreadableConfig(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	writeConfig(t, dir, "telemetry: [a]\n")
	called := false
	_, err := runUpload(t, dir, func(context.Context, bool, telemetry.Decision) telemetry.UploadResult {
		called = true
		return telemetry.UploadResult{}
	}, nil)
	require.Error(t, err)
	assert.False(t, called)
}

// TestUploadCommandMapsRefusalsAndFailures asserts the code, the exit code and
// the message of each refusal and failure, that a refusal never holds the
// endpoint value, and that none of them returns a result.
func TestUploadCommandMapsRefusalsAndFailures(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	reason := "it holds a path; give only scheme://host[:port]"
	rows := []struct {
		name     string
		res      telemetry.UploadResult
		code     string
		contains []string
		hint     string
	}{
		{"exec plugin", telemetry.UploadResult{Refusal: telemetry.RefusalExecPlugin}, clierr.CodeValidation, []string{"kubectl exec plugin"}, ""},
		{"release build", telemetry.UploadResult{Refusal: telemetry.RefusalReleaseBuild}, clierr.CodeValidation,
			[]string{"a release build ignores RANCHER_CLI_EXPERIMENT"}, ""},
		{"no endpoint", telemetry.UploadResult{Refusal: telemetry.RefusalNoEndpoint}, clierr.CodeValidation,
			[]string{"no endpoint", "RANCHER_CLI_EXPERIMENT=telemetryupload=<url>"}, ""},
		{"invalid endpoint", telemetry.UploadResult{Refusal: telemetry.RefusalInvalidEndpoint, RefusalReason: reason}, clierr.CodeValidation,
			[]string{reason, "RANCHER_CLI_EXPERIMENT=telemetryupload=<url>"}, ""},
		{"recording off by the variable", telemetry.UploadResult{Refusal: telemetry.RefusalRecordingOff, RefusalReason: string(telemetry.ControlVariable)},
			clierr.CodeValidation, []string{"RANCHER_CLI_TELEMETRY is false", "unset RANCHER_CLI_TELEMETRY"}, ""},
		{"recording off by the config key", telemetry.UploadResult{Refusal: telemetry.RefusalRecordingOff, RefusalReason: string(telemetry.ControlConfig)},
			clierr.CodeValidation, []string{"the telemetry key of config.yaml is false"}, clierr.Rancher + " telemetry on"},
		{"recording off by the test binary", telemetry.UploadResult{Refusal: telemetry.RefusalRecordingOff, RefusalReason: string(telemetry.ControlTestBinary)},
			clierr.CodeValidation, []string{"decided by test binary"}, ""},
		{"recording turned off during the attempt", telemetry.UploadResult{Failure: telemetry.FailureRecordingTurnedOff, Requests: 2},
			clierr.CodeValidation, []string{"recording was turned off", "nothing was deleted", "requests sent: 2"}, ""},
		{"upload directory unsafe", telemetry.UploadResult{Failure: telemetry.FailureUploadDirectoryUnsafe, FailurePath: "/c/telemetry/upload"},
			clierr.CodeGeneric, []string{"/c/telemetry/upload is a symlink, not a directory, or cannot be read", clierr.Rancher + " telemetry reset", "requests sent: 0"}, ""},
		{"lock unreadable", telemetry.UploadResult{Failure: telemetry.FailureLockUnreadable, FailurePath: "/c/telemetry/upload/lock"},
			clierr.CodeGeneric, []string{"the upload lock /c/telemetry/upload/lock cannot be read", "remove it", clierr.Rancher + " telemetry reset"}, ""},
		{"lock unavailable", telemetry.UploadResult{Failure: telemetry.FailureLockUnavailable, FailurePath: "/c/telemetry/upload/lock"},
			clierr.CodeGeneric, []string{"/c/telemetry/upload/lock cannot be created", "hard link", "RANCHER_CLI_CONFIG_DIR"}, ""},
		{"lock permission denied", telemetry.UploadResult{Failure: telemetry.FailureLockPermissionDenied, FailurePath: filepath.Join("c", "telemetry", "upload", "lock")},
			clierr.CodeGeneric, []string{"permission denied", "write access to " + filepath.Join("c", "telemetry", "upload"), clierr.Rancher + " telemetry reset"}, ""},
		{"lock create failed", telemetry.UploadResult{Failure: telemetry.FailureLockCreateFailed, FailurePath: filepath.Join("c", "telemetry", "upload", "lock")},
			clierr.CodeGeneric, []string{"cannot be created", "free space", clierr.Rancher + " telemetry reset"}, ""},
		{"store unavailable", telemetry.UploadResult{Failure: telemetry.FailureStoreUnavailable},
			clierr.CodeGeneric, []string{telemetryPath(dir), "cannot be opened"}, ""},
		{"read failed", telemetry.UploadResult{Failure: telemetry.FailureReadFailed},
			clierr.CodeGeneric, []string{"stored events", "cannot be listed"}, ""},
		{"request failed", telemetry.UploadResult{Failure: telemetry.FailureRequestFailed, Requests: 1},
			clierr.CodeGeneric, []string{"a request could not be built", "requests sent: 1"}, ""},
		{"sender failed", telemetry.UploadResult{Failure: telemetry.FailureSenderFailed, Requests: 3},
			clierr.CodeGeneric, []string{"unexpected error", "requests sent: 3"}, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			res, err := runUpload(t, dir, replyWith(row.res), nil)
			require.Error(t, err)
			assert.Empty(t, res.Text)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, row.code, ce.Code)
			for _, want := range row.contains {
				assert.Contains(t, ce.Message, want)
			}
			assert.Equal(t, row.hint, ce.Hint)
			assert.NotContains(t, ce.Message, "http")
			if strings.HasPrefix(row.name, "lock permission") || strings.HasPrefix(row.name, "lock create") {
				assert.NotContains(t, ce.Message, "hard link", "the hard link remedy is for a refused link only")
			}
		})
	}
}

// TestUploadCommandReturnsAnInterruptedErrorWhenTheContextIsCancelled asserts a
// cancelled context makes Execute return an error that names the requests
// already sent, whatever the result holds.
func TestUploadCommandReturnsAnInterruptedErrorWhenTheContextIsCancelled(t *testing.T) {
	controlsEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := NewUpload(newDir(t), replyWith(telemetry.UploadResult{Requests: 2, Sent: 5, Skipped: telemetry.SkippedNothingWaiting}))
	_, err := cmd.(rt.ReadCommand).Execute(ctx, cmdtest.Input{}, ui.Discard())
	require.ErrorContains(t, err, "requests sent: 2")
}

// TestUploadCommandText asserts the text line and the data for each case.
func TestUploadCommandText(t *testing.T) {
	controlsEnv(t)
	next := time.Date(2026, 11, 2, 13, 4, 5, 0, time.UTC)
	nextText := "; next automatic attempt after 2026-11-02T13:04:05Z"
	refused := func(n int) *int { return &n }
	rows := []struct {
		name string
		res  telemetry.UploadResult
		line string
		data map[string]any
	}{
		{"all sent", telemetry.UploadResult{LastOutcome: telemetry.OutcomeAccepted, Sent: 412, Refused: refused(3), HTTPStatus: 200, AcceptedStatus: 200, NextAttempt: next, Requests: 1},
			"sent 412 events (HTTP 200, 3 refused)" + nextText,
			map[string]any{"sent": 412, "refused": 3, "dropped": 0, "httpStatus": 200, "noAnswer": "", "nextAttempt": "2026-11-02T13:04:05Z", "timeLimitReached": false, "skipped": ""}},
		{"unknown count", telemetry.UploadResult{LastOutcome: telemetry.OutcomeAccepted, Sent: 412, HTTPStatus: 200, AcceptedStatus: 200, NextAttempt: next, Requests: 1},
			"sent 412 events (HTTP 200, refused count unknown)" + nextText, nil},
		{"deleting answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeDeleted, Dropped: 412, HTTPStatus: 400, NextAttempt: next, Requests: 1},
			"sent 0 events: HTTP 400, 412 events deleted" + nextText, nil},
		{"kept answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeKept, HTTPStatus: 503, NextAttempt: next, Requests: 1},
			"sent 0 events: HTTP 503, files kept" + nextText,
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": 503, "noAnswer": "", "nextAttempt": "2026-11-02T13:04:05Z", "timeLimitReached": false, "skipped": ""}},
		{"no answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeNoAnswer, NoAnswer: "timeout", NextAttempt: next, Requests: 1},
			"sent 0 events: no answer (timeout), files kept" + nextText,
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": nil, "noAnswer": "timeout", "nextAttempt": "2026-11-02T13:04:05Z", "timeLimitReached": false, "skipped": ""}},
		{"a 2xx then no answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeNoAnswer, Sent: 400, Refused: refused(0), AcceptedStatus: 200, NoAnswer: "timeout", NextAttempt: next, Requests: 2},
			"sent 400 events (HTTP 200, 0 refused), then no answer (timeout), files kept" + nextText, nil},
		{"a 2xx then a kept answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeKept, Sent: 400, Refused: refused(0), AcceptedStatus: 200, HTTPStatus: 503, NextAttempt: next, Requests: 2},
			"sent 400 events (HTTP 200, 0 refused), then HTTP 503, files kept" + nextText, nil},
		{"a 400 then a kept answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeKept, Dropped: 5, HTTPStatus: 503, NextAttempt: next, Requests: 2},
			"sent 0 events: HTTP 503, files kept, 5 events deleted" + nextText, nil},
		{"a 400 then no answer", telemetry.UploadResult{LastOutcome: telemetry.OutcomeNoAnswer, Dropped: 5, NoAnswer: "timeout", NextAttempt: next, Requests: 2},
			"sent 0 events: no answer (timeout), files kept, 5 events deleted" + nextText, nil},
		{"a 2xx then a 400", telemetry.UploadResult{LastOutcome: telemetry.OutcomeDeleted, Sent: 400, Dropped: 12, Refused: refused(0), AcceptedStatus: 200, HTTPStatus: 400, NextAttempt: next, Requests: 2},
			"sent 400 events (HTTP 200, 0 refused), then HTTP 400, 12 events deleted" + nextText, nil},
		{"a 400 then a 2xx", telemetry.UploadResult{LastOutcome: telemetry.OutcomeAccepted, Sent: 400, Dropped: 12, Refused: refused(0), AcceptedStatus: 200, HTTPStatus: 200, NextAttempt: next, Requests: 2},
			"sent 400 events (HTTP 200, 0 refused), 12 events deleted" + nextText, nil},
		{"the limit after some requests", telemetry.UploadResult{LastOutcome: telemetry.OutcomeAccepted, Sent: 400, Refused: refused(0), HTTPStatus: 200, AcceptedStatus: 200, TimeLimitReached: true, NextAttempt: next, Requests: 1},
			"sent 400 events (HTTP 200, 0 refused), then stopped at the 30-second limit, the rest kept" + nextText,
			map[string]any{"sent": 400, "refused": 0, "dropped": 0, "httpStatus": 200, "noAnswer": "", "nextAttempt": "2026-11-02T13:04:05Z", "timeLimitReached": true, "skipped": ""}},
		{"no next attempt", telemetry.UploadResult{LastOutcome: telemetry.OutcomeAccepted, Sent: 1, Refused: refused(0), HTTPStatus: 200, AcceptedStatus: 200, Requests: 1},
			"sent 1 events (HTTP 200, 0 refused); next automatic attempt at the next recorded run", nil},
		{"lock held", telemetry.UploadResult{Skipped: telemetry.SkippedLockHeld},
			"nothing sent: another upload is running",
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": nil, "noAnswer": "", "nextAttempt": nil, "timeLimitReached": false, "skipped": "lockHeld"}},
		{"nothing waiting", telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}, "nothing waiting", nil},
		{"time up", telemetry.UploadResult{Skipped: telemetry.SkippedTimeUp, TimeLimitReached: true},
			"nothing sent: the 30-second limit passed before the first request",
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": nil, "noAnswer": "", "nextAttempt": nil, "timeLimitReached": true, "skipped": "timeUp"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			res, err := runUpload(t, newDir(t), replyWith(row.res), nil)
			require.NoError(t, err)
			assert.Equal(t, row.line, res.Text[:len(res.Text)-len("\n")])
			assert.Equal(t, "TelemetryUpload", res.Kind)
			if row.data != nil {
				assert.Equal(t, row.data, dataMap(t, res))
			}
		})
	}
}

// resultConstants returns the values of the constants of the telemetry
// package whose names start with prefix, read from the file that declares
// them, so a constant added there is found with no edit here.
func resultConstants(t *testing.T, prefix string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "telemetry", "uploadcommand.go"), nil, 0)
	require.NoError(t, err)
	var values []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, prefix) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.True(t, ok, "%s is not a string literal", name.Name)
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				values = append(values, value)
			}
		}
	}
	return values
}

// TestEveryRefusalAndFailureHasItsOwnText asserts, over every Refusal and
// Failure constant of the telemetry package, that each one gets a message of
// its own: a constant added without a text gets the message of the fall-through
// case, which another constant already has, and fails here.
func TestEveryRefusalAndFailureHasItsOwnText(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	refusals := resultConstants(t, "Refusal")
	failures := resultConstants(t, "Failure")
	require.GreaterOrEqual(t, len(refusals), 5)
	require.GreaterOrEqual(t, len(failures), 8)

	seen := map[string]string{}
	check := func(kind, value string, res telemetry.UploadResult) {
		_, err := runUpload(t, dir, replyWith(res), nil)
		require.Error(t, err, kind+" "+value)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		if other, dup := seen[ce.Message]; dup {
			t.Errorf("%s %q and %s share the message %q", kind, value, other, ce.Message)
		}
		seen[ce.Message] = kind + " " + value
	}
	for _, v := range refusals {
		check("refusal", v, telemetry.UploadResult{Refusal: v, RefusalReason: string(telemetry.ControlVariable), FailurePath: "/c/telemetry/upload/lock"})
	}
	for _, v := range failures {
		check("failure", v, telemetry.UploadResult{Failure: v, FailurePath: "/c/telemetry/upload/lock"})
	}
}
