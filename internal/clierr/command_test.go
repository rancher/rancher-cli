package clierr

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plain strips the remedy marker so expected texts read as typed.
func plain(s string) string { return strings.ReplaceAll(s, Rancher, "rancher") }

// TestNotQuotableRefusedValues asserts every class of value the rule refuses.
func TestNotQuotableRefusedValues(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"empty":                         "",
		"nul":                           "a\x00b",
		"newline":                       "a\nb",
		"carriage return":               "a\rb",
		"delete":                        "a\x7fb",
		"next line U+0085":              "a\u0085b",
		"zero width space":              "abc\u200b",
		"right to left override":        "a\u202eb",
		"byte order mark":               "a\ufeffb",
		"line separator":                "a\u2028b",
		"paragraph separator":           "a\u2029b",
		"remedy marker start U+2063":    "a\u2063b",
		"apostrophe":                    "o'b",
		"left single quote U+2018":      "o\u2018b",
		"right single quote U+2019":     "o\u2019b",
		"low single quote U+201A":       "o\u201ab",
		"reversed single quote U+201B":  "o\u201bb",
		"double quote":                  `a"b`,
		"two backslashes":               `\\server\share`,
		"two backslashes mid value":     `a\\b`,
		"final backslash":               `a\`,
		"final backslash after space":   "a \\",
		"final backslash after U+00A0":  "a\u00a0\\",
		"drive root":                    `C:\`,
		"overlong encoding prefix byte": "\xc0",
	}
	for b := 0x80; b <= 0xff; b++ {
		refused[fmt.Sprintf("invalid byte %#x alone", b)] = string([]byte{byte(b)})
	}
	for name, v := range refused {
		assert.True(t, NotQuotable(v), name)
	}
}

// TestNotQuotableAcceptedValues asserts the values that stay literal inside
// single quotes in every supported shell pass.
func TestNotQuotableAcceptedValues(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"plain", "a b", "-x", "$(echo hi)", "`id`", `a\b`, `C:\Users\a b\rancher`,
		"a!b", "a;b", "a&b", "a|b", "a<b", "a>b", "a@b", "a#b", "(a)", "{a}", "a,b",
		"\u00fc", "\u201c", "\u201d", "\u201e", "\u2013", "\u2014", "\u2015", "--", "a=b",
		"https://rancher.example/v3", "a\u00a0b", "-a.b",
	} {
		assert.False(t, NotQuotable(v), "%q", v)
	}
}

// TestShellQuoteKeepsItsBytesForProse asserts prose quoting is unchanged.
func TestShellQuoteKeepsItsBytesForProse(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]string{
		"plain":       `'plain'`,
		"o'b":         `'o'\''b'`,
		"a\u2063b":    `'ab'`,
		`a\\b`:        `'a\\b'`,
		"":            `''`,
		"o\u2019b":    "'o\u2019b'",
		"abc\u200b":   "'abc\u200b'",
		"a \"quote\"": `'a "quote"'`,
	} {
		assert.Equal(t, want, ShellQuote(v), "%q", v)
	}
}

func TestPlainWord(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"local", "a.b", "a_b", "a-b", "A1"} {
		assert.True(t, PlainWord(v), v)
	}
	for _, v := range []string{"", "a b", "a'b", "a/b", "\u00fc", "a:b", "$x"} {
		assert.False(t, PlainWord(v), v)
	}
	// PowerShell passes these to a program as two arguments, split at the dot.
	for _, v := range []string{"-a.b", "-a-b.c", "-a_b.c", "-a."} {
		assert.False(t, PlainWord(v), v)
		w, ok := ShellWord(v)
		assert.True(t, ok, v)
		assert.Equal(t, "'"+v+"'", w)
	}
	for _, v := range []string{"-a", "--flag", "--a.b", "-1", "a-b.c"} {
		assert.True(t, PlainWord(v), v)
	}
}

func TestShellWord(t *testing.T) {
	t.Parallel()
	w, ok := ShellWord("local")
	assert.True(t, ok)
	assert.Equal(t, "local", w)
	w, ok = ShellWord("a b")
	assert.True(t, ok)
	assert.Equal(t, "'a b'", w)
	_, ok = ShellWord("o'b")
	assert.False(t, ok)
	_, ok = ShellWord("")
	assert.False(t, ok)
}

func TestFlagFragments(t *testing.T) {
	t.Parallel()
	got, ok := FlagFragment("--cluster", "local")
	assert.True(t, ok)
	assert.Equal(t, "--cluster 'local'", got)
	got, ok = FlagFragment("", "local")
	assert.True(t, ok)
	assert.Equal(t, "'local'", got)
	_, ok = FlagFragment("--cluster", "o'b")
	assert.False(t, ok)

	got, ok = FlagFragmentWord("--auth-provider", "local")
	assert.True(t, ok)
	assert.Equal(t, "--auth-provider local", got)
	got, ok = FlagFragmentWord("--auth-provider", "a b")
	assert.True(t, ok)
	assert.Equal(t, "--auth-provider 'a b'", got)
	_, ok = FlagFragmentWord("--auth-provider", "o'b")
	assert.False(t, ok)
	got, ok = FlagFragmentWord("", "local")
	assert.True(t, ok)
	assert.Equal(t, "local", got)

	for v, want := range map[string]string{"local": "--name <entry>-local", "a b": "--name <entry>-'a b'"} {
		got, ok = FlagFragmentAfterWord("--name", "<entry>-", v)
		assert.True(t, ok, v)
		assert.Equal(t, want, got)
	}
	_, ok = FlagFragmentAfterWord("--name", "<entry>-", `a\`)
	assert.False(t, ok)

	field := CommandWord("region")
	assert.Equal(t, "--set region=<value>", FlagFragmentText("--set", field+"=<value>"))
}

func TestWithLeftOutNoteWritesTheNoteOnce(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "x; "+LeftOutNote, WithLeftOutNote("x"))
	assert.Equal(t, "x; "+LeftOutNote, WithLeftOutNote(WithLeftOutNote("x")))
	assert.Equal(t, LeftOutNote, WithLeftOutNote(""))
}

func TestCommandPlain(t *testing.T) {
	t.Parallel()
	c := NewCommand("get users").Arg("bob").Flag("--dry-run")
	require.True(t, c.OK())
	assert.Equal(t, "rancher get users 'bob' --dry-run", plain(c.String()))
	assert.Equal(t, c.String(), c.Field())
	assert.True(t, strings.HasPrefix(c.String(), Rancher))
	f := FileCommand("get users").Arg("bob")
	assert.Equal(t, "rancher get users 'bob'", f.String())
	assert.Equal(t, "rancher --config 'C:\\Users\\a b' kubeconfig", FileCommandIn(`C:\Users\a b`, "kubeconfig").String())
	assert.Equal(t, "rancher kubeconfig", FileCommandIn("", "kubeconfig").String())
	assert.Equal(t, "rancher get 'users' --cluster 'local'", plain(NewCommand("get").Arg("users").FlagValue("--cluster", "local").String()))
	assert.Equal(t, "rancher kubeconfig --cluster <id>", plain(NewCommand("kubeconfig").FlagText(" --cluster <id>").String()))
	assert.Equal(t, "rancher config use-server <entry>", plain(NewCommand("config use-server").Placeholder("<entry>").String()))
	opt := CommandOption(func(c Command) Command { return c.Flag("--yes") })
	assert.Equal(t, "rancher get users --yes", plain(NewCommand("get users").With(opt).String()))
	assert.Equal(t, "rancher get pods", plain(NewCommand("get").Word("pods").String()))
}

func TestCommandValueStartingWithDashPutsFlagsBeforeTheSeparator(t *testing.T) {
	t.Parallel()
	c := NewCommand("get users").Arg("-x").Flag("--dry-run")
	assert.Equal(t, "rancher get users --dry-run -- '-x'", plain(c.String()))
	c = NewCommand("get users").Arg("a", " -x").FlagValue("--cluster", "local")
	assert.Equal(t, "rancher get users --cluster 'local' -- 'a' ' -x'", plain(c.String()))
	c = NewCommand("get users").Arg("a").FlagValue("--cluster", "-local")
	assert.Equal(t, "rancher get users 'a' --cluster '-local'", plain(c.String()))
}

func TestCommandReceiverStaysUnchanged(t *testing.T) {
	t.Parallel()
	base := NewCommand("get users").Arg("x")
	a := base.Arg("a")
	b := base.Arg("o'b")
	c := base.Flag("--one")
	d := base.Flag("--two")
	assert.True(t, a.OK())
	assert.False(t, b.OK())
	assert.True(t, base.OK())
	assert.Equal(t, "rancher get users 'x' 'a'", plain(a.String()))
	assert.Equal(t, "rancher get users 'x'", plain(base.String()))
	assert.Equal(t, "rancher get users 'x' --one", plain(c.String()))
	assert.Equal(t, "rancher get users 'x' --two", plain(d.String()))
}

// TestCommandRefusedValueInEveryPosition asserts a refused value leaves the
// command out wherever it sits.
func TestCommandRefusedValueInEveryPosition(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]Command{
		"arg":             NewCommand("get users").Arg("o'b"),
		"second arg":      NewCommand("get users").Arg("a", "o'b"),
		"flag value":      NewCommand("get users").FlagValue("--cluster", "o'b"),
		"file command":    FileCommand("get users").Arg("o'b"),
		"config dir":      FileCommandIn(`\\server\share`, "kubeconfig"),
		"hidden rune":     NewCommand("get users").Arg("abc\u200b"),
		"empty":           NewCommand("get users").Arg(""),
		"final backslash": NewCommand("get users").FlagValue("--cacert", `C:\`),
	} {
		assert.False(t, c.OK(), name)
		assert.Equal(t, LeftOutNote, c.String(), name)
		assert.Empty(t, c.Field(), name)
	}
}

func TestRevokeLeftOutKeepsTheListCommand(t *testing.T) {
	t.Parallel()
	ext := Revoke(TokenRef{Kind: TokenKindExt, Name: "abc"})
	assert.Equal(t, "rancher delete token 'abc' --apply --yes", plain(ext.String()))
	v3 := Revoke(TokenRef{Kind: TokenKindV3, Name: "abc"})
	assert.Equal(t, "rancher delete management-token 'abc' --apply --yes --allow-deprecated", plain(v3.String()))

	for _, name := range []string{"a'b", "o\u2019b", "abc\u200b", `a\\b`} {
		r := Revoke(TokenRef{Kind: TokenKindExt, Name: name})
		assert.False(t, r.OK(), name)
		assert.Equal(t, "rancher get tokens", plain(r.Field()), name)
		assert.Equal(t, "find it with: rancher get tokens; "+LeftOutNote, plain(r.String()), name)
		r3 := Revoke(TokenRef{Kind: TokenKindV3, Name: name})
		assert.Equal(t, "rancher get management-tokens", plain(r3.Field()), name)
	}
	on := Revoke(TokenRef{Kind: TokenKindExt, Name: "a'b"}).On("https://rancher.example")
	assert.Equal(t, "the token stays on https://rancher.example; log in there and run: rancher get tokens; "+LeftOutNote, plain(on.String()))
	assert.Equal(t, "rancher get tokens", plain(on.Field()))

	k := RevokeKubeconfig("kc")
	assert.Equal(t, "rancher delete kubeconfig 'kc' --apply --yes", plain(k.String()))
	kl := RevokeKubeconfig("k'c")
	assert.Equal(t, "rancher get kubeconfigs", plain(kl.Field()))
	fk := FileRevokeKubeconfig("kc")
	assert.Equal(t, "rancher delete kubeconfig 'kc' --apply --yes", fk.String())
	assert.NotContains(t, fk.String(), "\u2063")
	assert.Equal(t, "rancher get kubeconfigs", FileRevokeKubeconfig("k'c").Field())
}

func TestSequence(t *testing.T) {
	t.Parallel()
	ref := TokenRef{Kind: TokenKindExt, Name: "abc"}
	build := func(url string) Sequence {
		return NewSequence().
			Then("", NewCommand("auth login").FlagValue("--url", url).Arg("e")).
			Then("then ", NewCommand("config use-server").Arg("e")).
			Then("", Revoke(ref)).
			On(url)
	}
	ok := build("https://rancher.example")
	require.True(t, ok.OK())
	assert.Equal(t, []string{
		"rancher auth login 'e' --url 'https://rancher.example'",
		"rancher config use-server 'e'",
		"rancher delete token 'abc' --apply --yes",
	}, mapPlain(ok.Lines()))
	assert.Equal(t, ok.Lines(), ok.Field())
	assert.Equal(t, "rancher auth login 'e' --url 'https://rancher.example'; then rancher config use-server 'e'; rancher delete token 'abc' --apply --yes", plain(ok.String()))

	out := build("https://o'b.example")
	assert.False(t, out.OK())
	assert.Nil(t, out.Lines())
	assert.Equal(t, []string{"rancher get tokens"}, mapPlain(out.Field()))
	assert.Equal(t, "the token stays on https://o'b.example; log in there and run: rancher get tokens; "+LeftOutNote, plain(out.String()))

	noRevoke := NewSequence().Then("", NewCommand("config use-server").Arg("o'b"))
	assert.Nil(t, noRevoke.Field())
	assert.Equal(t, LeftOutNote, noRevoke.String())

	unnamed := NewSequence().Then("", NewCommand("auth login").Arg("o'b")).Then("", Revoke(ref))
	assert.Equal(t, "find it with: rancher get tokens; "+LeftOutNote, plain(unnamed.String()))

	refusedStepOnly := NewSequence().Then("", NewCommand("a").Arg("ok")).Then("", NewCommand("b").Arg("o'b")).Then("", NewCommand("c").Arg("ok"))
	assert.Nil(t, refusedStepOnly.Lines(), "a refused step leaves out every step, never only itself")
}

func mapPlain(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = plain(s)
	}
	return out
}

func TestTextKeepsFactsAndWritesTheNoteOnce(t *testing.T) {
	t.Parallel()
	ref := TokenRef{Kind: TokenKindExt, Name: "a'b"}
	person := "user " + ShellQuote("u-1")

	noServer := Text("no server is recorded for token "+ShellQuote(ref.Name)+
		"; delete it as "+person+", signed in to the server that issued it", Lead("; find it with: "), Revoke(ref))
	assert.Equal(t, "no server is recorded for token 'a'\\''b'; delete it as user 'u-1', signed in to the server that issued it; find it with: rancher get tokens; "+LeftOutNote, plain(noServer))

	recordServer := Text("the server recorded for token "+ShellQuote(ref.Name)+" cannot be used: the URL has no host"+
		"; the token stays on the server that issued it; delete it there as "+person+" while signed in to that server",
		Lead("; revoke it with: "), Revoke(ref))
	assert.Equal(t, "the server recorded for token 'a'\\''b' cannot be used: the URL has no host; the token stays on the server that issued it; delete it there as user 'u-1' while signed in to that server; find it with: rancher get tokens; "+LeftOutNote, plain(recordServer))

	op := NewCommand("get operation").FlagValue("--cluster", "local").Arg("helm-op-'x")
	waitErr := Text("release 'web' did not become ready; release 'web' is at version 2, status failed", Lead("; read the operation with "), op)
	assert.Equal(t, "release 'web' did not become ready; release 'web' is at version 2, status failed; "+LeftOutNote, waitErr)

	uninstall := NewCommand("app uninstall").FlagValue("--namespace", "apps").Flag("--dry-run").Arg("we'b")
	install := Text("app 'we'\\''b' already exists in namespace 'apps' (status failed)",
		Lead("; to remove the failed release, add --apply --yes after reading the preview of "), uninstall)
	assert.Equal(t, "app 'we'\\''b' already exists in namespace 'apps' (status failed); "+LeftOutNote, install)

	// owner.go: the fact stays, the words that introduce the delete go.
	revoke := Text("the token could not be revoked (timeout)", Lead("; revoke it with: "), Revoke(TokenRef{Kind: TokenKindExt, Name: "abc\u200b"}))
	assert.Equal(t, "the token could not be revoked (timeout); find it with: rancher get tokens; "+LeftOutNote, plain(revoke))
	revokeOK := Text("the token could not be revoked (timeout)", Lead("; revoke it with: "), Revoke(TokenRef{Kind: TokenKindExt, Name: "abc"}))
	assert.Equal(t, "the token could not be revoked (timeout); revoke it with: rancher delete token 'abc' --apply --yes", plain(revokeOK))

	two := Text("run", Lead(": "), NewCommand("x").Arg("o'b"), "; or run", Lead(": "), NewCommand("y").Arg("o'b"))
	assert.Equal(t, 1, strings.Count(two, LeftOutNote), two)

	printed := Text("read it", Lead(" with "), NewCommand("get users").Arg("bob"))
	assert.Equal(t, "read it with rancher get users 'bob'", plain(printed))

	seq := Text("those credentials are gone regardless of this failure", Lead("; "), NewSequence().Then("", NewCommand("x").Arg("o'b")))
	assert.Equal(t, "those credentials are gone regardless of this failure; "+LeftOutNote, seq)
}

type notAPart struct{}

func (notAPart) Error() string { return "x" }

// TestTextPanicsOnAnUnsupportedPart asserts Text refuses every part that is
// not prose, a Lead, a Command or a Sequence, and names the type.
func TestTextPanicsOnAnUnsupportedPart(t *testing.T) {
	t.Parallel()
	for name, part := range map[string]any{
		"CommandWord": CommandWord("x"),
		"[]string":    []string{"x"},
		"*Command":    &Command{},
		"error":       notAPart{},
	} {
		func() {
			defer func() {
				r := recover()
				require.NotNil(t, r, name)
				msg, _ := r.(string)
				assert.Contains(t, msg, "clierr.Text")
				assert.Contains(t, msg, fmt.Sprintf("%T", part), name)
			}()
			Text("x", part)
		}()
	}
}

func TestWithHintCommand(t *testing.T) {
	t.Parallel()
	ok := New(CodeNotFound, "gone").WithHintCommand(NewCommand("get users"))
	assert.Equal(t, "rancher get users", plain(ok.Hint))
	assert.Equal(t, "gone", ok.Message)

	left := New(CodeNotFound, "gone").WithHintCommand(NewCommand("get users").Arg("o'b"))
	assert.Empty(t, left.Hint)
	assert.Equal(t, "gone; "+LeftOutNote, left.Message)

	revoke := New(CodeNotFound, "gone").WithHintCommand(Revoke(TokenRef{Kind: TokenKindExt, Name: "a'b"}))
	assert.Equal(t, "rancher get tokens", plain(revoke.Hint))
	assert.Equal(t, "gone; "+LeftOutNote, revoke.Message)

	again := left.WithHintCommand(NewCommand("get users").Arg("o'b"))
	assert.Equal(t, 1, strings.Count(again.Message, LeftOutNote))

	p := PrimaryFailure{Message: "gone"}.WithHintCommand(Revoke(TokenRef{Kind: TokenKindV3, Name: "a'b"}))
	assert.Equal(t, "rancher get management-tokens", plain(p.Hint))
	assert.Equal(t, "gone; "+LeftOutNote, p.Message)
	p = PrimaryFailure{Message: "gone"}.WithHintCommand(NewCommand("get users"))
	assert.Equal(t, "gone", p.Message)
}

func TestTypedLine(t *testing.T) {
	t.Parallel()
	line := NewTypedLine().Bare("get").Bare("users").Value("bob").Joined("--watch", "false").Bare("--")
	assert.True(t, line.OK())
	assert.Equal(t, "rancher get users 'bob' --watch='false' --", line.Field())
	assert.Equal(t, "rancher 'we ird'", NewTypedLine().Bare("we ird").Field())
	for name, l := range map[string]TypedLine{
		"bare":   NewTypedLine().Bare("get").Bare("o'b"),
		"value":  NewTypedLine().Value(`a\`),
		"joined": NewTypedLine().Joined("--watch", "o'b"),
		"flag":   NewTypedLine().Joined("--w'x", "false"),
	} {
		assert.False(t, l.OK(), name)
		assert.Empty(t, l.Field(), name)
	}
}

// TestTextDropsTheLeadOfAZeroCommand asserts the zero Command, which stands
// for no command, writes neither its lead-in nor the note.
func TestTextDropsTheLeadOfAZeroCommand(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "prose", Text("prose", Lead("; run: "), Command{}))
	assert.Equal(t, "prose; then more", Text("prose", Lead("; run: "), Command{}, "; then more"))
}

// TestNoteOnceMovesEveryNoteToTheEnd asserts a text joined from several
// texts that each carry the note ends with it once, and a text without it is
// returned as it is.
func TestNoteOnceMovesEveryNoteToTheEnd(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "no note", NoteOnce("no note"))
	assert.Equal(t, "a; b; "+LeftOutNote, NoteOnce("a; "+LeftOutNote+"; b; "+LeftOutNote))
	assert.Equal(t, LeftOutNote, NoteOnce(LeftOutNote+"; "+LeftOutNote))
	assert.Equal(t, "x; "+LeftOutNote, NoteOnce("x; "+LeftOutNote))
	assert.Equal(t, "x; "+LeftOutNote, NoteOnce(LeftOutNote+"; x"))
	assert.Equal(t, "; x; "+LeftOutNote, NoteOnce("; x; "+LeftOutNote), "a text that starts with its own separator keeps it")
	// Text applies it to a prose part that already holds the note.
	got := Text("first; "+LeftOutNote+"; second", Lead("; run: "), NewCommand("get users").Arg("o'b"))
	assert.Equal(t, "first; second; "+LeftOutNote, got)
	assert.Equal(t, 1, strings.Count(got, LeftOutNote))
}
