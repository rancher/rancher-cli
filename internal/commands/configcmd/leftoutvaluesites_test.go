package configcmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// editBaseline is the bytes of a config file that holds cfg.
func editBaseline(t *testing.T, cfg *config.Config) []byte {
	t.Helper()
	cfg.SetPath(config.FilePath(t.TempDir()))
	require.NoError(t, cfg.Write())
	b, err := os.ReadFile(cfg.Path())
	require.NoError(t, err)
	return b
}

// refusedEditMessage is the message of the refusal of an edit of baseline to
// edited, which keeps the binding stamp the write of baseline gave.
func refusedEditMessage(t *testing.T, baseline []byte, edited *config.Config) string {
	t.Helper()
	parsed, perr := config.ParseConfig(baseline)
	require.NoError(t, perr)
	edited.BindingStamp = parsed.BindingStamp
	err := checkEditRestrictions(baseline, true, edited)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	return ce.Message
}

// seedLoginEntry writes an entry named name to dir that holds a login the CLI
// created, with its token stored.
func seedLoginEntry(t *testing.T, dir string, store config.CredStore, name, url string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[name] = config.Server{
		URL: url, Provenance: clierr.ProvenanceDerived, TokenUserID: "u-abc12",
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"},
	}
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: name, URL: url}, "tok"))
}

// TestConfigEditRefusalOfAnAddedEntryKeepsItsFactsWhenTheNameCannotBePrinted
// asserts the refusal of an edit that adds an entry whose name cannot be
// printed keeps the sentence about the entry and the URL placeholder, leaves
// out the add-server command and its lead-in, and holds the note once.
func TestConfigEditRefusalOfAnAddedEntryKeepsItsFactsWhenTheNameCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			edited := &config.Config{Servers: map[string]config.Server{value: {URL: "https://a.example.com"}}}
			err := checkEditRestrictions(nil, false, edited)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, "config edit cannot change: servers."+value+" added; <url> is the server's address; "+clierr.LeftOutNote, ce.Message)
		})
	}
}

// TestConfigEditRefusalOfAChangedEntryKeepsItsFactsWhenTheNameCannotBePrinted
// asserts the refusal of an edit that changes an entry whose name cannot be
// printed keeps the sentence about the entry, the URL placeholder and the CA
// flags, leaves out the update-server command and its lead-in, and holds the
// note once.
func TestConfigEditRefusalOfAChangedEntryKeepsItsFactsWhenTheNameCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			baseline := editBaseline(t, &config.Config{Servers: map[string]config.Server{value: {URL: "https://a.example.com"}}})
			edited := &config.Config{Servers: map[string]config.Server{value: {URL: "https://b.example.com"}}}
			want := "config edit cannot change: servers." + value + " changed; <url> is the server's address; " +
				config.CAFileArg("--cacert") + " or --clear-cacert changes the CA instead; " + config.CAFileMeaning + "; " + clierr.LeftOutNote
			assert.Equal(t, want, refusedEditMessage(t, baseline, edited))
		})
	}
}

// TestConfigEditRefusalOfACredentialStoreKeepsItsFactsWhenTheNameCannotBePrinted
// asserts the refusal of an edit that sets or changes the credential store to
// a name that cannot be printed keeps the sentence about the entry to log in
// and leaves out the auth login command and its lead-in, with the note once.
func TestConfigEditRefusalOfACredentialStoreKeepsItsFactsWhenTheNameCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			const facts = "; <name> is the entry to log in; log out the other entries first when there are any"
			set := refusedEditMessage(t, editBaseline(t, &config.Config{}), &config.Config{CredentialStore: value})
			assert.Equal(t, "config edit cannot change: credentialStore set"+facts+"; "+clierr.LeftOutNote, set)

			changed := refusedEditMessage(t, editBaseline(t, &config.Config{CredentialStore: "file"}), &config.Config{CredentialStore: value})
			assert.Equal(t, "config edit cannot change: credentialStore changed"+facts+"; "+clierr.LeftOutNote, changed)
		})
	}
}

// TestUpdateServerLeavesOutTheRerunOfAnEntryNameThatCannotBePrinted asserts
// the warning of a URL change whose local delete failed keeps the sentence that
// says the token was not revoked and stays in use, leaves out the update-server
// command that deletes it and its lead-in, holds the note once, and gives the
// recovery of the token no command.
func TestUpdateServerLeavesOutTheRerunOfAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedLoginEntry(t, dir, store, value, "https://old.example.com")
			require.NoError(t, os.Remove(config.CredentialsFilePath(dir)))
			require.NoError(t, os.Mkdir(config.CredentialsFilePath(dir), 0o700))

			_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{value}}, nil)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			require.Len(t, ce.Warnings(), 1)
			assert.Equal(t, "the login token ext/token-abc12 was not revoked: its local copy was not deleted and it stays in use; "+clierr.LeftOutNote, ce.Warnings()[0])

			rep := decodeRevocationDetails(t, ce.Details)
			require.Len(t, rep.NotRevoked, 1)
			assert.Equal(t, "notDeleted", rep.NotRevoked[0].Reason)
			assert.Empty(t, rep.NotRevoked[0].Recovery)
		})
	}
}

// TestUpdateServerWriteFailureAfterThePurgeLeavesOutTheLoginForANameThatCannotBePrinted
// asserts the error of an update whose entry write failed after the purge
// keeps the sentence that names the cleared credentials and the cause, gives no
// hint, and holds the note once, when the entry name cannot be printed.
func TestUpdateServerWriteFailureAfterThePurgeLeavesOutTheLoginForANameThatCannotBePrinted(t *testing.T) {
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			dir, store := cmdtest.FilePolicyDir(t)
			seedLoginEntry(t, dir, store, value, "https://old.example.com")
			t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
				if step == "update" {
					return errors.New("disk full")
				}
				return nil
			}))

			_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{value}}, nil)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, fmt.Sprintf("credentials for %q were cleared, but writing the updated server entry failed: disk full; %s", value, clierr.LeftOutNote), ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}

// TestUpdateServerLeavesOutTheLoginAgainOfAClearedEntryNameThatCannotBePrinted
// asserts the text of a URL change that clears credentials keeps the line that
// says the credentials are cleared, leaves out the auth login command and its
// lead-in, and holds the note once, when the entry name cannot be printed.
func TestUpdateServerLeavesOutTheLoginAgainOfAClearedEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedLoginEntry(t, dir, store, value, "https://old.example.com")

			res, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{value}}, nil)
			require.NoError(t, err)
			assert.Contains(t, res.Text, "\n  credentials cleared; "+clierr.LeftOutNote+"\n")
			assert.Equal(t, 1, strings.Count(res.Text, clierr.LeftOutNote), res.Text)
			assert.NotContains(t, res.Text, "log in again")
			assert.NotContains(t, res.Text, "auth login")
		})
	}
}

// TestTheRemovalOfAnExecLoginEntryWhoseNameCannotBePrintedIsLeftOutOfTheURLRefusal
// asserts the refusal of a URL change on an entry the exec plugin made keeps
// the facts and the instruction to log it out first, leaves out both commands
// of the removal and their lead-in, and holds the note once, when the entry
// name cannot be printed.
func TestTheRemovalOfAnExecLoginEntryWhoseNameCannotBePrintedIsLeftOutOfTheURLRefusal(t *testing.T) {
	t.Parallel()
	for caseName, value := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			ce, ok := errors.AsType[*clierr.Error](execLoginURLChangeRefused(value))
			require.True(t, ok)
			want := clierr.ShellQuote(value) + " is one of the exec plugin's own login entries, and its URL cannot change; the exec plugin creates a new entry for the new URL; " +
				"to remove this entry, log it out first if it holds a login; " + clierr.LeftOutNote
			assert.Equal(t, want, ce.Message)
		})
	}
}

// TestUpdateServerLeavesOutTheRerunOfAURLThatCannotBePrinted asserts the
// warning of a URL change whose local delete failed keeps the sentence that
// says the token was not revoked and stays in use, and leaves out the
// update-server command that deletes it and its lead-in, with the note once,
// when the new URL holds a single quote in its host, which the canonical form
// keeps.
func TestUpdateServerLeavesOutTheRerunOfAURLThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedLoginEntry(t, dir, store, "prod", "https://old.example.com")
	require.NoError(t, os.Remove(config.CredentialsFilePath(dir)))
	require.NoError(t, os.Mkdir(config.CredentialsFilePath(dir), 0o700))

	_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://n'w.example.com"}, Pos: []string{"prod"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	require.Len(t, ce.Warnings(), 1)
	assert.Equal(t, "the login token ext/token-abc12 was not revoked: its local copy was not deleted and it stays in use; "+clierr.LeftOutNote, ce.Warnings()[0])
	rep := decodeRevocationDetails(t, ce.Details)
	require.Len(t, rep.NotRevoked, 1)
	assert.Empty(t, rep.NotRevoked[0].Recovery)
}
