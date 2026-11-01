package config

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// refusedNameCharacters are characters a terminal removes when it prints a
// name, each with the code point text the refusal names.
var refusedNameCharacters = []struct {
	label string
	char  string
	text  string
}{
	{"invisible separator", "\u2063", "U+2063"},
	{"soft hyphen", "\u00ad", "U+00AD"},
	{"zero width space", "\u200b", "U+200B"},
	{"zero width joiner", "\u200d", "U+200D"},
	{"left-to-right mark", "\u200e", "U+200E"},
	{"right-to-left override", "\u202e", "U+202E"},
	{"word joiner", "\u2060", "U+2060"},
	{"left-to-right isolate", "\u2066", "U+2066"},
	{"byte order mark", "\ufeff", "U+FEFF"},
	{"language tag", "\U000e0001", "U+E0001"},
	{"NUL", "\x00", "U+0000"},
	{"tab", "\t", "U+0009"},
	{"newline", "\n", "U+000A"},
	{"carriage return", "\r", "U+000D"},
	{"escape", "\x1b", "U+001B"},
	{"delete", "\x7f", "U+007F"},
	{"next line control", "\u0085", "U+0085"},
	{"line separator", "\u2028", "U+2028"},
	{"paragraph separator", "\u2029", "U+2029"},
}

// acceptedNames are names that print as themselves.
var acceptedNames = []string{
	"qa1", "-q", "--x", "a b", "it's", `x"y`, "$(touch PWNED)", "a:b", " lead", "trail ",
	"a\u00a0b", "a\u3000b", "a\ufffdb", "e\u0301", "\u4e2d\u6587",
	strings.Repeat("x", 500),
}

// TestEntryNameProblemFindsEveryCharacterAPrinterRemoves asserts the check
// reports the removed character, wherever it sits in the name, and refuses
// the name with a typed error that names the code point.
func TestEntryNameProblemFindsEveryCharacterAPrinterRemoves(t *testing.T) {
	for _, tc := range refusedNameCharacters {
		for place, name := range map[string]string{
			"start":  tc.char + "qa1",
			"middle": "qa" + tc.char + "1",
			"end":    "qa1" + tc.char,
			"alone":  tc.char,
		} {
			t.Run(tc.label+" at the "+place, func(t *testing.T) {
				p, bad := EntryNameProblem(name)
				require.True(t, bad)
				assert.False(t, p.InvalidUTF8)
				assert.Equal(t, []rune(tc.char)[0], p.Rune)

				ce := CheckNewEntryName(name)
				require.NotNil(t, ce)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				require.ErrorContains(t, ce, tc.text)
			})
		}
	}
}

// TestEntryNameProblemRefusesAnInvisibleSeparatorAheadOfADashOrAMarker
// asserts a name that starts with the invisible separator the remedy marker
// opens with is refused whatever follows it, and that a plain leading dash
// is an ordinary character.
func TestEntryNameProblemRefusesAnInvisibleSeparatorAheadOfADashOrAMarker(t *testing.T) {
	for _, name := range []string{"\u2063-q", "\u2063\u2060\u2061\u2062\u2064rancher"} {
		p, bad := EntryNameProblem(name)
		require.True(t, bad, "%q", name)
		assert.Equal(t, '\u2063', p.Rune)
		require.NotNil(t, CheckNewEntryName(name), "%q", name)
	}
	assert.Nil(t, CheckNewEntryName("-q"))
}

// TestEntryNameProblemRefusesAnInvalidByte asserts a name that is not valid
// UTF-8 is refused with a message of its own, at any place.
func TestEntryNameProblemRefusesAnInvalidByte(t *testing.T) {
	for _, name := range []string{"\xff", "qa\xff1", "qa1\x80", "qa\xe2\x80"} {
		p, bad := EntryNameProblem(name)
		require.True(t, bad, "%q", name)
		assert.True(t, p.InvalidUTF8, "%q", name)
		assert.Zero(t, p.Rune, "%q", name)
		ce := CheckNewEntryName(name)
		require.NotNil(t, ce, "%q", name)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "a new server name must be valid UTF-8; use a different name", ce.Message)
	}
}

// TestCheckNewEntryNameMessageNamesTheCharacter asserts the refusal text of a
// character, in full.
func TestCheckNewEntryNameMessageNamesTheCharacter(t *testing.T) {
	ce := CheckNewEntryName("a\u200bb")
	require.NotNil(t, ce)
	assert.Equal(t, "a new server name cannot hold U+200B, which is removed when a name is printed; use a different name", ce.Message)
}

// TestEntryNameProblemAcceptsNamesThatPrintAsThemselves asserts names with
// spaces, quotes, shell syntax, a leading dash, a colon, combining marks,
// non-ASCII letters and a real replacement character are accepted, and that
// length is not this check's concern.
func TestEntryNameProblemAcceptsNamesThatPrintAsThemselves(t *testing.T) {
	for _, name := range acceptedNames {
		_, bad := EntryNameProblem(name)
		assert.False(t, bad, "%q", name)
		assert.Nil(t, CheckNewEntryName(name), "%q", name)
	}
}

// TestValidateEntryNameStillAcceptsAStoredName asserts the check for a stored
// name leaves a name with an invisible character alone: an entry already
// stored with one keeps working.
func TestValidateEntryNameStillAcceptsAStoredName(t *testing.T) {
	assert.Nil(t, ValidateEntryName("qa\u200b1"))
}

// namesInDir lists the file names in dir.
func namesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestPersistLoginCredentialRefusesANewNameBeforeAnyWrite asserts a login
// that would create an entry named with an invisible or control character is
// refused under the locks with nothing written: the config file is as it was,
// no credential is stored and no manifest or other file is left. The two lock
// files the call takes stay in place.
func TestPersistLoginCredentialRefusesANewNameBeforeAnyWrite(t *testing.T) {
	dir := settledFixture(t)
	before, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)
	filesBefore := namesInDir(t, dir)

	for _, name := range []string{"\u2063-q", "qa\u200b1", "qa\n1", "\xff"} {
		in := settledInput()
		in.Name, in.OwnsEntry = name, true
		out, perr := PersistLoginCredential(t.Context(), dir, in)
		ce, ok := errors.AsType[*clierr.Error](perr)
		require.True(t, ok, "%q: %v", name, perr)
		assert.Equal(t, clierr.CodeValidation, ce.Code, "%q", name)
		assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState, "%q", name)
		assert.Equal(t, clierr.CredentialNotWritten, out.CredentialState, "%q", name)

		after, rerr := os.ReadFile(FilePath(dir))
		require.NoError(t, rerr)
		assert.Equal(t, string(before), string(after), "%q: the config file is as it was", name)
		_, gerr := NewFileStore(dir).GetToken(ServerRef{Name: name, URL: "https://r.ex"})
		require.ErrorIs(t, gerr, ErrNoToken, "%q: no credential is stored", name)
	}

	var added []string
	for _, f := range namesInDir(t, dir) {
		if !slices.Contains(filesBefore, f) {
			added = append(added, f)
		}
	}
	assert.ElementsMatch(t, []string{configLockName, transitionLockName}, added, "only the two lock files are left")
}

// TestPersistLoginCredentialRefusesOnlyANameThatIsNotYetAnEntry asserts the
// refusal applies to a name that is not an entry: an ordinary new name is
// created, and an entry already stored with an invisible character in its
// name, at the same URL, is updated.
func TestPersistLoginCredentialRefusesOnlyANameThatIsNotYetAnEntry(t *testing.T) {
	dir := settledFixture(t)
	in := settledInput()
	in.Name, in.OwnsEntry = "-q", true
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, "-q", out.Name)

	stored := "qa\u200b1"
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	s := Server{URL: "https://r.ex"}
	require.NoError(t, AdvanceCacheBinding(&s))
	cfg.Servers[stored] = s
	require.NoError(t, cfg.Write())

	upd := settledInput()
	upd.Name = stored
	out, err = PersistLoginCredential(t.Context(), dir, upd)
	require.NoError(t, err)
	assert.Equal(t, stored, out.Name)
}
