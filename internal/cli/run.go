package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// Run runs args (os.Args: the program name, then the words typed) on root,
// built by BuildRoot, as the process does, and returns the process exit
// code, which is 0 for a success. It renders any error that nothing else
// rendered and never ends the process: the caller exits with the code.
// Before urfave parses the line, it refuses a line with words after a - on
// its own, a bool flag typed with = and an empty or whitespace-only value,
// or a word that reads as a bool typed after a bool flag. A completion
// request is left to the completion callbacks, which offer nothing on such
// a line.
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
// writes the server's facts file the same way. A line with no chosen command
// (an unknown command, a group or the root alone, help, the version,
// urfave's flag errors, the refusals above, a completion request) and the
// completion and telemetry commands record nothing. Recording never changes
// the output or the exit code.
//
// With RANCHER_CLI_EXPERIMENT set in a development build, a bad item adds a
// warning line.
func Run(ctx context.Context, root *ucli.Command, args []string) int {
	return run(ctx, root, args).code
}

// run is Run with how the run ended, not just the code.
func run(ctx context.Context, root *ucli.Command, args []string) runResult {
	start := now()
	clearUnknownCommand(root)
	resetRecordingState()
	resetExperimentState()
	httpx.ClearRecords()
	clearHeldFacts()
	_ = takeHelpWriteError(root)
	k, err := runArgv(ctx, root, args)
	res := resolveExit(root, err, recordedUnknownCommand(root))
	if helpErr := takeHelpWriteError(root); helpErr != nil && res.code == 0 && res.err == nil {
		ce := clierr.From(helpErr)
		renderRawArgvError(root, ce)
		res = runResult{code: ce.ExitCode(), err: ce}
	}
	refreshServerFacts(ctx, recordRun(root, k, res, start))
	return res
}

// runArgv runs args on root and returns the parse of the line, when it made
// one, and the error urfave returned, or the one that refuses the line before
// urfave parses it.
func runArgv(ctx context.Context, root *ucli.Command, args []string) (k keptLine, err error) {
	if len(args) == 0 {
		return k, root.Run(ctx, args)
	}
	CaptureArgv(args[1:])
	// urfave sets the writers inside root.Run, so a refusal written before
	// it needs the error writer set.
	if root.ErrWriter == nil {
		root.ErrWriter = ucli.ErrWriter
	}
	if !completionRequest(args[1:]) {
		p := parseLine(root, rawArgv)
		k = keptLine{line: p, parsed: true}
		if ce, wholeLine := lineRefusal(p); ce != nil {
			if wholeLine || !p.readWholeLine() {
				renderRawArgvError(root, ce)
			} else {
				runtime.WriteError(root.ErrWriter, p.valueOf("output"), ce, errStyle(root, p.valueOf("color")))
			}
			return k, exit(ce)
		}
		// A kubectl plugin call that signs in sends its facts requests
		// through this function, with the parse of this line.
		kept := k
		httpx.RegisterSignInRequest(func(ctx context.Context, origin string, fetch httpx.InstallIDFunc) {
			requestFactsDuringSignIn(ctx, kept, origin, fetch)
		})
	}
	return k, root.Run(ctx, SanitizedArgs(args))
}
