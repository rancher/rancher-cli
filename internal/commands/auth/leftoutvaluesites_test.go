package auth

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestTheSwitchHintOfALoginIsLeftOutForAnEntryNameThatCannotBePrinted asserts
// the lines and the error of a login that point at config use-server keep their
// facts and leave out the command and its lead-in, with the note once, when
// the entry name cannot be printed: the line of a login that leaves no current
// server, the line of a login that leaves another one current, and the
// refusal of a login that names no entry when one entry exists and none is
// current, which gives no hint either.
func TestTheSwitchHintOfALoginIsLeftOutForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			login := func(t *testing.T, dir string, store config.CredStore) string {
				t.Helper()
				res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
					cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{value}}, cmdtest.StdinPrompter("id:sec\n"))
				require.NoError(t, err)
				return res.Text
			}

			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, value, "https://r.ex", "", false)
			line := strings.TrimSpace(lineHolding(t, login(t, dir, store), "the current server is not set"))
			assert.Equal(t, "the current server is not set; "+clierr.LeftOutNote, line)

			dir, store = cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", "https://prod.ex", "", true)
			seedServer(t, dir, value, "https://x.ex", "", false)
			line = strings.TrimSpace(lineHolding(t, login(t, dir, store), "the current server remains"))
			assert.Equal(t, "the current server remains 'prod'; switch to "+clierr.ShellQuote(value)+"; "+clierr.LeftOutNote, line)

			cfg := &config.Config{Servers: map[string]config.Server{value: {URL: "https://r.example.com"}}}
			_, err := resolveLoginTarget(cfg, "", "", "")
			ce := asClierr(t, err)
			assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
			assert.Equal(t, "no current server; "+clierr.LeftOutNote, ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}

// TestTheSignInPointerOfAKnownEntryWithARotatedCAIsLeftOutForANameThatCannotBePrinted
// asserts the certificate failure of a login to a registered entry keeps the
// failure and the words that name the entry as already registered, and leaves
// out the login command, its lead-in and the hint, with the note once, when the
// entry name cannot be printed.
func TestTheSignInPointerOfAKnownEntryWithARotatedCAIsLeftOutForANameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	tlsSrv := newTLSFake(t, fix)
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, value, tlsSrv.URL, "", false)
			_, err := tlsLoginCmd(dir, store).Execute(t.Context(),
				cmdtest.Input{Str: map[string]string{"url": tlsSrv.URL}, Pos: []string{value}},
				ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true))
			ce := asClierr(t, err)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
			assert.True(t, strings.HasSuffix(ce.Message, "; "+clierr.ShellQuote(value)+" is already registered; "+clierr.LeftOutNote), ce.Message)
			assert.NotContains(t, ce.Message, "sign in to it directly")
			assert.NotContains(t, ce.Message, "auth login")
		})
	}
}

// TestTheDeleteOfAnEntryWhoseStoredCAFileIsMissingIsLeftOutForANameThatCannotBePrinted
// asserts the refusal of a login to an entry whose saved CA file cannot be read
// keeps the problem and the instruction to restore the file and leaves out the
// delete command and its lead-in, with the note once, when the entry name
// cannot be printed.
func TestTheDeleteOfAnEntryWhoseStoredCAFileIsMissingIsLeftOutForANameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, value, "https://r.ex", filepath.Join(t.TempDir(), "missing.pem"), false)
			_, err := tlsLoginCmd(dir, store).Execute(t.Context(),
				cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{value}}, cmdtest.StdinPrompter("id:sec\n"))
			ce := asClierr(t, err)
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
			assert.True(t, strings.HasPrefix(ce.Message, fmt.Sprintf("reading stored CA for server entry %q: ", value)), ce.Message)
			assert.True(t, strings.HasSuffix(ce.Message, "; restore the file; "+clierr.LeftOutNote), ce.Message)
			assert.NotContains(t, ce.Message, "delete-server")
			assert.NotContains(t, ce.Message, ", or run")
		})
	}
}

// TestTheUndoOfARegistrationIsLeftOutOfADelegatedFailureForANameThatCannotBePrinted
// asserts the failure of a login after a registration keeps its text, the
// remedy's instruction and the words "undo the registration" and leaves out the
// delete command and its lead-in, with the note once, when the entry name
// cannot be printed: with no remedy the hint is empty, with one the hint is
// the remedy's.
func TestTheUndoOfARegistrationIsLeftOutOfADelegatedFailureForANameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, value, "https://r.ex", "", false)
			snap := register.AuthSnapshot{Name: value, CanonicalURL: "https://r.ex"}

			bare := asClierr(t, decorateDelegatedVerifyFailure(t.Context(), dir, clierr.New(clierr.CodeGeneric, "verify failed"), snap))
			assert.Equal(t, "verify failed; bypass verification with --no-verify; or undo the registration; "+clierr.LeftOutNote, bare.Message)
			assert.Empty(t, bare.Hint)

			remedy := token.LoginRefusalRemedy{Instruction: "run in a terminal", Hint: "keep this hint", Fallback: "run in a terminal"}
			with := asClierr(t, decorateDelegatedFailure(context.Background(), dir, clierr.New(clierr.CodeGeneric, "failed"), snap, "", remedy))
			assert.Equal(t, "failed; run in a terminal; or undo the registration; "+clierr.LeftOutNote, with.Message)
			assert.Equal(t, "keep this hint", with.Hint)
		})
	}
}

// TestTheUpdateServerHintOfACacertWithoutAURLIsLeftOutForAValueThatCannotBePrinted
// asserts the refusal of --cacert without --url for a registered entry keeps
// the sentence about trust and registration and gives no command and no hint,
// with the note once, whether the entry name or the --cacert path cannot be
// printed, and gives the command for printable values.
func TestTheUpdateServerHintOfACacertWithoutAURLIsLeftOutForAValueThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	plain := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}}
	_, err := resolveLoginTarget(plain, "prod", "", "/etc/ca.pem")
	ce := asClierr(t, err)
	assert.Equal(t, clierr.Rancher+" config update-server 'prod' --cacert '/etc/ca.pem'", ce.Hint)
	for name, value := range unprintableValues {
		t.Run(name+", the entry", func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Servers: map[string]config.Server{value: {URL: "https://r.example.com"}}}
			_, err := resolveLoginTarget(cfg, value, "", "/etc/ca.pem")
			ce := asClierr(t, err)
			assert.Equal(t, fmt.Sprintf("--cacert without --url changes trust for %q, which belongs to registration; %s", value, clierr.LeftOutNote), ce.Message)
			assert.Empty(t, ce.Hint)
		})
		t.Run(name+", the CA path", func(t *testing.T) {
			t.Parallel()
			_, err := resolveLoginTarget(plain, "prod", "", "/etc/"+value+".pem")
			ce := asClierr(t, err)
			assert.Equal(t, fmt.Sprintf("--cacert without --url changes trust for %q, which belongs to registration; %s", "prod", clierr.LeftOutNote), ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}
