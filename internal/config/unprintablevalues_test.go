package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// unprintableValues are values a command cannot print so that every supported
// shell reads them as written.
var unprintableValues = map[string]string{
	"zero width space":       "abc\u200b",
	"quote":                  "a'b",
	"right single quotation": "a\u2019b",
	"two backslashes":        `a\\b`,
}

// TestUnusableURLDisplayIsWhatDisplayServerURLShows asserts the constant is
// the text DisplayServerURL gives for a URL with no scheme and host, so that a
// caller can tell it from a URL.
func TestUnusableURLDisplayIsWhatDisplayServerURLShows(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "<unusable URL>", UnusableURLDisplay)
	assert.Equal(t, UnusableURLDisplay, DisplayServerURL("not a url"))
	assert.Empty(t, DisplayServerURL(""))
}

// TestRecordTextsLeaveOutTheDeleteForATokenNameThatCannotBePrinted asserts the
// two texts of a token record whose server cannot be used give the list
// command and the note, and no delete command.
func TestRecordTextsLeaveOutTheDeleteForATokenNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: value}
			noServer := clierr.ExpandRemedies(noServerText(ref, "u-1"), "")
			assert.Contains(t, noServer, "delete it as user 'u-1', signed in to the server that issued it; find it with: rancher get tokens; "+clierr.LeftOutNote)
			recorded := clierr.ExpandRemedies(recordServerText(ref, errors.New("the URL has no host"), "u-1"), "")
			assert.Contains(t, recorded, "the token stays on the server that issued it; delete it there as user 'u-1' while signed in to that server; find it with: rancher get tokens; "+clierr.LeftOutNote)
			for _, text := range []string{noServer, recorded} {
				assert.NotContains(t, text, "delete token")
				assert.NotContains(t, text, "\u200b", "a hidden rune is shown as an escape, never removed")
				assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote))
			}
		})
	}
}

// TestAdviseRecordLeavesOutTheSequenceForAServerThatCannotBePrinted asserts a
// recorded server whose URL holds a quote, a right single quotation mark, a
// zero width space or two backslashes gives a sequence that is left out whole:
// no delete line in its text or json form, the json form the list command, and
// the text the server, the list command and the note.
func TestAdviseRecordLeavesOutTheSequenceForAServerThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := "https://o" + value + ".example"
			ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-1"}
			advice := AdviseRecord(RecordSite{}, server, "u-1", ref)
			if advice.Text != "" {
				t.Skipf("the server is refused before a sequence is built: %s", advice.Text)
			}
			assert.False(t, advice.Sequence.OK())
			assert.Nil(t, advice.Sequence.Lines())
			field := advice.Sequence.Field()
			require.Len(t, field, 1)
			assert.Equal(t, "rancher get tokens", clierr.ExpandRemedies(field[0], ""))
			text := clierr.ExpandRemedies(advice.Sequence.String(), "")
			assert.NotContains(t, text, "delete token")
			assert.Contains(t, text, "the token stays on "+DisplayServerURL(CanonicalServerIdentity(server))+"; log in there and run: rancher get tokens; "+clierr.LeftOutNote)
		})
	}
}

// TestHintsLeaveOutTheCommandForAnEntryNameThatCannotBePrinted asserts the
// login hint, the logout command, the not-logged-in error of a lone server and
// the credential errors named for an entry carry no hint and the note once
// when the entry name cannot be printed.
func TestHintsLeaveOutTheCommandForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			login, ok := LoginHintFor(value)
			require.True(t, ok)
			assert.False(t, login.OK())
			assert.Empty(t, login.Field())
			assert.False(t, LogoutHintFor(value).OK())

			for what, err := range map[string]*clierr.Error{
				"not logged in":       ErrNotLoggedInFor(map[string]string{value: "https://r.example"}, nil),
				"credential replaced": credentialReplacedError(value).(*clierr.Error),
				"login incomplete":    loginIncompleteError(value).(*clierr.Error),
				"store change":        transitionRefusal("a change cannot run", []string{value}).(*clierr.Error),
			} {
				assert.Empty(t, err.Hint, what)
				assert.NotContains(t, err.Message, "auth login", what)
				assert.NotContains(t, err.Message, "auth logout", what)
				assert.Equal(t, 1, strings.Count(err.Message, clierr.LeftOutNote), what)
			}
		})
	}
}

// TestRollbackRecoveryCommandIsEmptyForAnEntryNameThatCannotBePrinted asserts
// the delete-server command of a failed rollback is empty for an entry name
// that cannot be printed, which the details then show as no command.
func TestRollbackRecoveryCommandIsEmptyForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out PersistLoginOutcome
			_ = rollbackFailed(&out, value, &Server{URL: "https://r.example"}, errors.New("write failed"), errors.New("login failed"))
			require.NotNil(t, out.ConfigRecovery)
			assert.Empty(t, out.ConfigRecovery.Command)
			assert.Equal(t, value, out.ConfigRecovery.Entry)
		})
	}
}

// TestUnfinishedChangeNotesNameTheStatusCommand asserts the notes of a store
// change that is unfinished name the status command once, as a command.
func TestUnfinishedChangeNotesNameTheStatusCommand(t *testing.T) {
	t.Parallel()
	status := clierr.NewCommand(StatusWords).String()
	assert.Equal(t, "a credential store change is unfinished; see it with: "+status, unfinishedChangeNote)
	assert.True(t, strings.HasSuffix(unreadableChangeNote, "; see it with: "+status))
}

// TestServerURLOriginTextLeavesOutTheCommandForAnEntryNameThatCannotBePrinted
// asserts the refusal of an entry's URL keeps its facts, gives no command and
// carries the note once when the entry name cannot be printed, for the entry of
// the exec plugin as well as for any other.
func TestServerURLOriginTextLeavesOutTheCommandForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, value := range unprintableValues {
		for _, entry := range []string{value, ExecLoginPrefix + value} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				text := EntryOrigin(entry).Text(ErrServerURLScheme)
				assert.Contains(t, text, "its scheme is not http or https; "+clierr.LeftOutNote)
				assert.NotContains(t, text, "config update-server")
				assert.NotContains(t, text, "config delete-server")
				assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote))
			})
		}
	}
}
