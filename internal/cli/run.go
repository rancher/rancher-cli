package cli

import (
	"context"
	"os"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Run runs args (os.Args: the program name, then the arguments typed) on
// root, built by BuildRoot, as the process does, and returns the process exit
// code, which is 0 for a success. It renders any error that nothing else
// rendered and never ends the process: the caller exits with the code.
// Before it acts on the command line, it refuses one with arguments after a
// - on its own, a bool flag typed with = and an empty or whitespace-only
// value, or an argument that reads as a bool typed after a bool flag. A
// completion request is left to the completion callbacks, which offer no
// candidates on such a command line.
//
// After the command ends and its output is written, Run records one usage
// event when recording is on and the config directory existed when the root
// Before hook ran. It then may ask the server the command reached for its
// version and edition and, when the command built the root client, auth
// login or auth status registered one after their check, or a kubectl plugin
// call created a credential, its install id, within 2 seconds, when they are
// due; the answers reach the next event, never this one. A plugin call that
// signs in asks during the sign-in instead, before it deletes its login
// token, and the answers are written after the event. The first auth login,
// auth get-token or token run on a fresh install, which creates the config
// directory, records no event; when it succeeds with recording on, it
// writes the server's facts file the same way. After the refresh, a recorded
// run of a development build whose telemetryupload experiment names an
// endpoint may send the stored events of finished hours, at most once an hour,
// inside the same deadline; the kubectl exec plugin never does. A line with no
// chosen command (an unknown command, a group or the root alone, help, the version,
// flag errors, the refusals above, a completion request) and the
// completion and telemetry commands record nothing. Recording never changes
// the output or the exit code.
//
// With RANCHER_CLI_EXPERIMENT set in a development build, a bad item adds a
// warning line.
func Run(ctx context.Context, root *Command, args []string) int {
	return run(ctx, root, args).code
}

// run is Run with how the run ended, not just the code.
func run(ctx context.Context, root *Command, args []string) runResult {
	start := now()
	resetRecordingState()
	resetExperimentState()
	httpx.ClearRecords()
	clearHeldFacts()
	k, oc := runArgv(ctx, root, args, true)
	res := resolveExit(root, oc.err)
	if oc.writeErr != nil && res.code == 0 && res.err == nil {
		ce := clierr.From(oc.writeErr)
		renderRawArgvError(root, ce)
		res = runResult{code: ce.ExitCode(), err: ce}
	}
	res.outcome = oc
	in := recordRun(root, k, res, start)
	deadline := refreshServerFacts(ctx, in)
	uploadStoredEvents(ctx, root, k, in, deadline)
	return res
}

// runArgv runs args on root and returns the result of parsing the command
// line, when it parsed one, and how the command line ended. A root with no
// Reader, Writer or ErrWriter gets os.Stdin, os.Stdout and os.Stderr. A
// command line with
// arguments after a - on its own, a bool flag typed with = and an empty or
// whitespace-only value, or an argument that reads as a bool typed after a
// bool flag is refused before the dispatcher acts on it. process is true for
// the run of a process, whose sign-in sends the facts requests of the command
// line. Only the run of a process captures the command line (see CaptureArgv).
// Command.Run does not capture it: a completion request, the check of the
// flags of a command and the text and format of an error read the captured
// command line, which a caller of Command.Run captures first, as a process
// does.
func runArgv(ctx context.Context, root *Command, args []string, process bool) (k keptLine, oc outcome) {
	if root.Reader == nil {
		root.Reader = os.Stdin
	}
	if root.Writer == nil {
		root.Writer = os.Stdout
	}
	if root.ErrWriter == nil {
		root.ErrWriter = os.Stderr
	}
	if len(args) == 0 {
		if process {
			CaptureArgv(nil)
		}
		p := parseLine(root, nil)
		return k, dispatch(ctx, root, &p)
	}
	if process {
		CaptureArgv(args[1:])
	}
	if completionRequest(args[1:]) {
		return k, root.complete(ctx)
	}
	words := args[1:]
	if process {
		words = rawArgv
	}
	p := parseLine(root, words)
	k = keptLine{line: p, parsed: true}
	if ce, wholeLine := lineRefusal(p); ce != nil {
		if wholeLine || !p.readWholeLine() {
			renderRawArgvError(root, ce)
		} else {
			runtime.WriteError(root.ErrWriter, p.valueOf("output"), ce, errStyle(root, p.valueOf("color")), "")
		}
		return k, outcome{kind: outcomeRefused, err: exit(ce)}
	}
	if process {
		// A kubectl plugin call that signs in sends its facts requests
		// through this function, with the result of parsing this command line.
		kept := k
		httpx.RegisterSignInRequest(func(ctx context.Context, origin string, fetch httpx.InstallIDFunc) {
			requestFactsDuringSignIn(ctx, kept, origin, fetch)
		})
	}
	return k, dispatch(ctx, root, &p)
}
