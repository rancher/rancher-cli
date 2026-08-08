package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionVerbNeedsNoVault: version must work with no Vault env at all —
// it dispatches before the Vault handler is constructed.
func TestVersionVerbNeedsNoVault(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", "")
	var out bytes.Buffer
	code := run([]string{"version"}, &out, io.Discard)
	require.Equal(t, 0, code)
	assert.Equal(t, VERSION+"\n", out.String())
}

func TestRun(t *testing.T) {
	t.Parallel()
	type runCase struct {
		name               string
		args               []string
		wantCode           int
		wantStdoutContains string // "" = no content check; stdout emptiness is also asserted for codes -1 and 2
		wantStderrContains string // "" = no content check
	}
	cases := []runCase{
		// --help/-h anywhere in argv → usage, exit 0
		{name: "--help", args: []string{"--help"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		{name: "-h", args: []string{"-h"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		// bare "help" first-token-only → usage, exit 0
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		{name: "--help extra", args: []string{"--help", "extra"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		{name: "-h -v", args: []string{"-h", "-v"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		// --help/-h wins even in non-first position (flag-anywhere contract)
		{name: "get --help anywhere-wins", args: []string{"get", "--help"}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		// version first-token-only → version string, exit 0
		{name: "--version", args: []string{"--version"}, wantCode: 0, wantStdoutContains: VERSION},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdoutContains: VERSION},
		{name: "--version extra", args: []string{"--version", "extra"}, wantCode: 0, wantStdoutContains: VERSION},
		{name: "version extra", args: []string{"version", "extra"}, wantCode: 0, wantStdoutContains: VERSION},
		// no args → usage, exit 0
		{name: "no args", args: []string{}, wantCode: 0, wantStdoutContains: "VAULT_ADDR"},
		// unrecognised flags → usage on stderr, exit 2
		{name: "get --debug exit 2", args: []string{"get", "--debug"}, wantCode: 2, wantStderrContains: "get"},
		{name: "unknown -x exit 2", args: []string{"-x"}, wantCode: 2, wantStderrContains: "get"},
		// protocol verbs fall through to Serve (-1); no stdout or stderr output
		{name: "get falls through", args: []string{"get"}, wantCode: -1},
		// "get help": bare "help" in non-first position is not intercepted here —
		// it reaches Serve's arg-count check. Diverges from "get --help" by design.
		{name: "get help falls through", args: []string{"get", "help"}, wantCode: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			assert.Equal(t, tc.wantCode, code)
			if tc.wantStdoutContains != "" {
				assert.Contains(t, stdout.String(), tc.wantStdoutContains)
			}
			if tc.wantStderrContains != "" {
				assert.Contains(t, stderr.String(), tc.wantStderrContains)
			}
			// Fall-through and unknown-flag exits must not write to stdout.
			if tc.wantCode == -1 || tc.wantCode == 2 {
				assert.Empty(t, stdout.String(), "stdout must be empty for code=%d", tc.wantCode)
			}
		})
	}
}
