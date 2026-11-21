package engine

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// ReadPasswordStdin reads and validates a --password-stdin value, the one
// shared input path for every command in the user-password workflow: there
// is no --password flag, ever, on any of them - a flag value lands in shell
// history and process listings. The flag itself must be set (a VALIDATION
// error otherwise, with no hint: the password is a value the CLI does not
// have, so no command is safe to paste). The value is the invocation's whole
// stdin with every byte order mark at the start of stdin dropped and at most
// one final newline (`\n` or `\r\n`) trimmed - never other whitespace, which a
// password may legitimately contain; a value that still contains a newline is
// refused with a VALIDATION error that has no hint and does not repeat the typed
// text. newlineRemedy is a sentence added to that error; empty adds nothing.
func ReadPasswordStdin(src FileSource, in runtime.Input, newlineRemedy string) (string, error) {
	if !in.Bool("password-stdin") {
		return "", clierr.New(clierr.CodeValidation,
			"--password-stdin is required: pipe the password on stdin and pass --password-stdin (there is no --password flag)")
	}
	raw, err := src.Read("-")
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, fmt.Sprintf("reading --password-stdin: %v", err))
	}
	password := trimFinalNewline(string(DropLeadingByteOrderMarks(raw)))
	if password == "" {
		return "", clierr.New(clierr.CodeValidation, "the password read from stdin is empty; pipe a non-empty password on stdin")
	}
	if strings.Contains(password, "\n") {
		message := "the password read from stdin has more than one line; --password-stdin takes the password on one line"
		if newlineRemedy != "" {
			message += ". " + newlineRemedy
		}
		return "", clierr.New(clierr.CodeValidation, message)
	}
	return password, nil
}

// ReadCurrentAndNewPasswordStdin reads the two values of --current-password-stdin
// with --password-stdin: the invocation's whole stdin holds two lines, the
// current password and then the new password. Every byte order mark at the
// start of stdin is dropped; a mark at the start of line 2 is part of the new
// password. Every refusal is a VALIDATION error with no hint that does not
// repeat the typed text. A newline is `\n` or `\r\n`: one final newline is
// trimmed, the rest of stdin is split on `\n`, and one `\r` is trimmed from the
// end of line 1. A `\r` that is not part of a newline stays in its value, and so
// does a bare final `\r`, as ReadPasswordStdin keeps it. An empty line is
// refused.
func ReadCurrentAndNewPasswordStdin(src FileSource, in runtime.Input) (current, newPassword string, err error) {
	if !in.Bool("password-stdin") {
		return "", "", clierr.New(clierr.CodeValidation,
			"--current-password-stdin needs --password-stdin: pipe the current password and the new password, one per line")
	}
	raw, err := src.Read("-")
	if err != nil {
		return "", "", clierr.New(clierr.CodeGeneric, fmt.Sprintf("reading --password-stdin: %v", err))
	}
	text := string(DropLeadingByteOrderMarks(raw))
	if text == "" {
		return "", "", clierr.New(clierr.CodeValidation,
			"stdin is empty; with --current-password-stdin, pipe the current password and the new password, one per line")
	}
	lines := strings.Split(trimFinalNewline(text), "\n")
	if len(lines) != 2 {
		return "", "", clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"with --current-password-stdin, stdin must have two lines, the current password and then the new password; lines found: %d", len(lines)))
	}
	current = strings.TrimSuffix(lines[0], "\r")
	newPassword = lines[1]
	if current == "" {
		return "", "", clierr.New(clierr.CodeValidation, "line 1 (the current password) is empty")
	}
	if newPassword == "" {
		return "", "", clierr.New(clierr.CodeValidation, "line 2 (the new password) is empty")
	}
	return current, newPassword, nil
}

// trimFinalNewline trims one final `\r\n` or `\n`, when there is one: never a
// bare final `\r` or any other byte, which a password may legitimately end
// with.
func trimFinalNewline(s string) string {
	if trimmed, ok := strings.CutSuffix(s, "\r\n"); ok {
		return trimmed
	}
	return strings.TrimSuffix(s, "\n")
}
