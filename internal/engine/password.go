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
// have, so no command is safe to paste); its value is always the
// invocation's whole stdin with at most one trailing newline (or CRLF)
// trimmed - never other whitespace, which a password may legitimately
// contain.
func ReadPasswordStdin(src FileSource, in runtime.Input) (string, error) {
	if !in.Bool("password-stdin") {
		return "", clierr.New(clierr.CodeValidation,
			"--password-stdin is required: pipe the password on stdin and pass --password-stdin (there is no --password flag)")
	}
	raw, err := src.Read("-")
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, fmt.Sprintf("reading --password-stdin: %v", err))
	}
	password := string(raw)
	// Exactly one trailing newline or CRLF pair is trimmed - never a bare
	// trailing carriage return or any other byte, which a password may
	// legitimately end with.
	if trimmed, ok := strings.CutSuffix(password, "\r\n"); ok {
		password = trimmed
	} else {
		password = strings.TrimSuffix(password, "\n")
	}
	if password == "" {
		return "", clierr.New(clierr.CodeValidation, "the password read from stdin is empty; pipe a non-empty password on stdin")
	}
	return password, nil
}
