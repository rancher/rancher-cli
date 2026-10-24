package cli

import (
	"context"

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
func Run(ctx context.Context, root *ucli.Command, args []string) int {
	return run(ctx, root, args).code
}

// run is Run with how the run ended, not just the code.
func run(ctx context.Context, root *ucli.Command, args []string) runResult {
	clearUnknownCommand(root)
	err := runArgv(ctx, root, args)
	return resolveExit(root, err, recordedUnknownCommand(root))
}

// runArgv runs args on root and returns the error urfave returned, or the
// one that refuses the line before urfave parses it.
func runArgv(ctx context.Context, root *ucli.Command, args []string) error {
	if len(args) == 0 {
		return root.Run(ctx, args)
	}
	CaptureArgv(args[1:])
	// urfave sets the writers inside root.Run, so a refusal written before
	// it needs the error writer set.
	if root.ErrWriter == nil {
		root.ErrWriter = ucli.ErrWriter
	}
	if !completionRequest(args[1:]) {
		p := parseLine(root, rawArgv)
		if ce, wholeLine := lineRefusal(p); ce != nil {
			if wholeLine || !p.readWholeLine() {
				renderRawArgvError(root, ce)
			} else {
				_ = runtime.RenderErrorStyled(root.ErrWriter, p.valueOf("output"), ce, errStyle(root, p.valueOf("color")))
			}
			return exit(ce)
		}
	}
	return root.Run(ctx, SanitizedArgs(args))
}
