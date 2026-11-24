package cli

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// exitError is the error a command returns to end a run with ce's exit
// code. Its message is empty: the command has rendered ce already. It names
// ce so that run can report
// which error ended the run; the exit code alone cannot say, because five
// error codes share exit 2 and four share exit 3.
type exitError struct {
	ce *clierr.Error
}

func (e *exitError) Error() string { return "" }

func (e *exitError) ExitCode() int { return e.ce.ExitCode() }

func (e *exitError) Unwrap() error { return e.ce }

// runResult is how one run of the command line ended. code is the process
// exit code. err is the error that ended the run, with an exit code equal
// to code, and nil when the run ended without one: a success (code 0) or a
// status the command declares as a success, such as 7 for a skill that is
// absent when its presence is required. outcome is how the line ended.
type runResult struct {
	code    int
	err     *clierr.Error
	outcome outcome
}

// resolveExit decides how a run ended from the error it returned, nil when
// none. The code and rendering of a returned error decide.
func resolveExit(root *Command, returned error) runResult {
	if returned != nil {
		return resultOf(root, returned)
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
//
// Any other error with a message was written by nobody. It is rendered as a
// GENERIC error, and its exit code is GENERIC's: a code the error itself
// carries must not reach the caller. An error with no message that is an
// exit code keeps its code, as a declared success status does; one that is
// not exits 1.
func resultOf(root *Command, err error) runResult {
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
	if ec, ok := errors.AsType[exitCoder](err); ok {
		return runResult{code: ec.ExitCode()}
	}
	ce := clierr.New(clierr.CodeGeneric, "")
	return runResult{code: ce.ExitCode(), err: ce}
}
