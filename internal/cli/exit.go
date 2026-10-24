package cli

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	ucli "github.com/urfave/cli/v3"
)

// exitError is the error a command returns to end a run with ce's exit
// code. Its message is empty: the command has rendered ce already, and
// urfave writes an exit error's message. It names ce so that run can report
// which error ended the run; the exit code alone cannot say, because five
// error codes share exit 2 and four share exit 3.
type exitError struct {
	ce *clierr.Error
}

func (e *exitError) Error() string { return "" }

func (e *exitError) ExitCode() int { return e.ce.ExitCode() }

func (e *exitError) Unwrap() error { return e.ce }

// unknownCommandKey is the key in the root command's Metadata under which
// reportUnknownCommand records the unknown-command error. urfave's
// CommandNotFound seam returns nothing, and the command carries on after it
// and returns nil, so the record is the only way the exit code reaches Run.
const unknownCommandKey = "unknownCommand"

// recordUnknownCommand records ce on the root of c, replacing an earlier
// record.
func recordUnknownCommand(c *ucli.Command, ce *clierr.Error) {
	root := c.Root()
	if root.Metadata == nil {
		root.Metadata = map[string]any{}
	}
	root.Metadata[unknownCommandKey] = unknownCommandRecord{ce: ce}
}

// unknownCommandRecord is the value recorded under unknownCommandKey.
type unknownCommandRecord struct {
	ce *clierr.Error
}

// recordedUnknownCommand returns the unknown-command error recorded on root
// since clearUnknownCommand, or nil.
func recordedUnknownCommand(root *ucli.Command) *clierr.Error {
	rec, _ := root.Metadata[unknownCommandKey].(unknownCommandRecord)
	return rec.ce
}

// clearUnknownCommand removes the record from root. Run calls it before
// every run, so a record from an earlier run on the same root never ends a
// later one.
func clearUnknownCommand(root *ucli.Command) {
	delete(root.Metadata, unknownCommandKey)
}

// runResult is how one run of the command line ended. code is the process
// exit code. err is the error that ended the run, with an exit code equal
// to code, and nil when the run ended without one: a success (code 0) or a
// status the command declares as a success, such as 7 for a skill that is
// absent when its presence is required.
type runResult struct {
	code int
	err  *clierr.Error
}

// resolveExit decides how a run ended from the error urfave returned (nil
// when none) and the unknown-command error recorded during the run (nil
// when none). A returned error always wins: its code and rendering
// decide. The recorded error applies only when urfave returned nil, which
// it does after an unknown command.
func resolveExit(root *ucli.Command, returned error, recorded *clierr.Error) runResult {
	if returned != nil {
		return resultOf(root, returned)
	}
	if recorded != nil {
		return runResult{code: recorded.ExitCode(), err: recorded}
	}
	return runResult{}
}

// resultOf turns an error that ended a run into its result, rendering it
// only when nothing has:
//   - An exitError, found anywhere in the chain, names an error the
//     command rendered already.
//   - A *clierr.Error that is the error itself, not one another error
//     wraps, is rendered with its own exit code, in the output format the
//     command line selects. A wrapped one falls to the rule below, which
//     keeps the text its wrapper added.
//   - urfave writes an error's text itself in two ways. "Incorrect Usage"
//     is written only by a command with no OnUsageError handler, and every
//     command has one (the command tree test asserts it). An error passed
//     to its exit handler is written only when the handler is urfave's
//     own; the root's handler does nothing.
//
// Any other error with a message was therefore written by nobody. It is
// rendered as a GENERIC error, and its exit code is GENERIC's: a code
// urfave chose (its help error uses 3, the AUTH code) must not reach the
// caller. An error with no message that is an exit code keeps its code, as
// a declared success status does; one that is not exits 1.
func resultOf(root *ucli.Command, err error) runResult {
	if ee, ok := errors.AsType[*exitError](err); ok {
		return runResult{code: ee.ExitCode(), err: ee.ce}
	}
	if ce, ok := errors.AsType[*clierr.Error](err); ok && err == error(ce) {
		renderRawArgvError(root, ce)
		return runResult{code: ce.ExitCode(), err: ce}
	}
	if msg := err.Error(); msg != "" {
		ce := clierr.New(clierr.CodeGeneric, msg)
		renderRawArgvError(root, ce)
		return runResult{code: ce.ExitCode(), err: ce}
	}
	if ec, ok := errors.AsType[ucli.ExitCoder](err); ok {
		return runResult{code: ec.ExitCode()}
	}
	ce := clierr.New(clierr.CodeGeneric, "")
	return runResult{code: ce.ExitCode(), err: ce}
}
