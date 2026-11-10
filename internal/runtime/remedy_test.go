package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// noticingRead is a read command that prints a notice through its
// prompter and then returns its result or error.
type noticingRead struct {
	d      Descriptor
	notice string
	res    Result
	err    error
}

func (n noticingRead) Describe() Descriptor { return n.d }
func (n noticingRead) Execute(_ context.Context, _ Input, p Prompter) (Result, error) {
	if n.notice != "" {
		p.Notice(n.notice)
	}
	return n.res, n.err
}

func remedyError() *clierr.Error {
	return clierr.New(clierr.CodeNotLoggedIn, "not logged in; run "+clierr.Rancher+" auth login 'prod'").
		WithHint(clierr.Rancher + " auth login 'prod'").
		WithWarnings("see " + clierr.Rancher + " config status").
		WithDetails(clierr.LoginFailureDetails{Recovery: "log in again with: " + clierr.Rancher + " auth login 'prod'"})
}

// TestRemedyCommandsNameTheInvocationsDirectory asserts every remedy
// channel of an invocation with a RemedyDir - the error's message, hint,
// warnings and details in text and in JSON, a result's warnings, and the
// prompter's notices - names the directory shell-quoted, and that the same
// channels print the plain command for an invocation without one.
func TestRemedyCommandsNameTheInvocationsDirectory(t *testing.T) {
	t.Parallel()
	const dir = "/home/u/my dir"
	withDir := "rancher --config '/home/u/my dir' "
	for _, tc := range []struct {
		name, remedyDir, want string
	}{
		{"with a directory", dir, withDir},
		{"without one", "", "rancher "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Descriptor{Name: "kubeconfig", CommandType: Local}

			t.Run("error text", func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				_, ce := Run(t.Context(), noticingRead{d: d, notice: "try " + clierr.Rancher + " config status", err: remedyError()},
					mapInput{}, IO{Out: &out, Err: &errBuf, RemedyDir: tc.remedyDir}, safety.Binding{})
				require.NotNil(t, ce)
				got := errBuf.String()
				assert.Contains(t, got, "try "+tc.want+"config status\n")
				assert.Contains(t, got, "# warning: see "+tc.want+"config status")
				assert.Contains(t, got, "not logged in; run "+tc.want+"auth login 'prod'")
				assert.Contains(t, got, "hint: "+tc.want+"auth login 'prod'")
				assert.Contains(t, got, "recover: log in again with: "+tc.want+"auth login 'prod'")
				assert.NotContains(t, got, clierr.RemedyMarker)
			})

			t.Run("error json", func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				_, ce := Run(t.Context(), noticingRead{d: d, err: remedyError()},
					mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", RemedyDir: tc.remedyDir}, safety.Binding{})
				require.NotNil(t, ce)
				var env struct {
					Warnings []string `json:"warnings"`
					Data     struct {
						Error struct {
							Message string `json:"message"`
							Hint    string `json:"hint"`
							Details struct {
								Recovery string `json:"recovery"`
							} `json:"details"`
						} `json:"error"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
				assert.Equal(t, "not logged in; run "+tc.want+"auth login 'prod'", env.Data.Error.Message)
				assert.Equal(t, tc.want+"auth login 'prod'", env.Data.Error.Hint)
				assert.Equal(t, "log in again with: "+tc.want+"auth login 'prod'", env.Data.Error.Details.Recovery)
				assert.Equal(t, []string{"see " + tc.want + "config status"}, env.Warnings)
			})

			t.Run("result warnings", func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				res := Result{RawBytes: []byte("apiVersion: v1\n"), Warnings: []string{"see " + clierr.Rancher + " config status"}}
				_, ce := Run(t.Context(), noticingRead{d: Descriptor{Name: "kubeconfig", CommandType: Local, Shape: ShapeBytes}, res: res},
					mapInput{}, IO{Out: &out, Err: &errBuf, RemedyDir: tc.remedyDir}, safety.Binding{})
				require.Nil(t, ce)
				assert.Equal(t, "# warning: see "+tc.want+"config status\n", errBuf.String())
				assert.Equal(t, "apiVersion: v1\n", out.String())
			})
		})
	}
}

// TestRemedyCommandsInStructuredResultDataNameTheDirectory asserts a remedy
// command in the data of a result written as json or yaml names the
// invocation's config directory after the word rancher, with no character of
// the remedy marker left.
func TestRemedyCommandsInStructuredResultDataNameTheDirectory(t *testing.T) {
	t.Parallel()
	cmdText := clierr.Rancher + " config status"
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			res := Result{Kind: "Report", Data: map[string]string{"next": cmdText}}
			_, ce := Run(t.Context(), noticingRead{d: Descriptor{Name: "config status", CommandType: Local}, res: res},
				mapInput{}, IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: "/d"}, safety.Binding{})
			require.Nil(t, ce)
			var doc struct {
				Data struct {
					Next string `json:"next"`
				} `json:"data"`
			}
			require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
			assert.Equal(t, "rancher --config '/d' config status", doc.Data.Next)
			assert.False(t, strings.ContainsFunc(out.String(), func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", out.String())
		})
	}
}

// TestUnprintableConfigDirNoteText asserts the words of the note a text says
// in place of its commands when the config directory's path has characters a
// terminal cannot show.
func TestUnprintableConfigDirNoteText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "the config directory's path has characters a terminal cannot show; the commands that would name it are left out", UnprintableConfigDirNote)
}

// TestRemedyCommandsForAnUnprintableDirectory asserts a directory whose
// path a terminal line cannot show is never named in a command, nor
// replaced by a placeholder: every text keeps its words up to the clause
// holding its first command and says why none is offered, the hint (a
// command) is dropped, and no rancher command is printed at all, in text
// or JSON.
func TestRemedyCommandsForAnUnprintableDirectory(t *testing.T) {
	t.Parallel()
	notice := "not caching: a credential change for prod has not finished (see `" + clierr.Rancher + " config status`)"
	for _, dir := range []string{"/home/u/a\nb", "/home/u/\x1b[31mred", "/home/u/\u202eevil"} {
		t.Run(strings.ToValidUTF8(dir, "?"), func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), noticingRead{d: Descriptor{Name: "kubeconfig", CommandType: Local}, notice: notice, err: remedyError()},
				mapInput{}, IO{Out: &out, Err: &errBuf, RemedyDir: dir}, safety.Binding{})
			require.NotNil(t, ce)
			got := errBuf.String()
			assert.Contains(t, got, "not caching: a credential change for prod has not finished; "+UnprintableConfigDirNote+"\n")
			assert.Contains(t, got, "error (NOT_LOGGED_IN): not logged in; "+UnprintableConfigDirNote+"\n")
			assert.Contains(t, got, "# warning: "+UnprintableConfigDirNote+"\n", "a clause that is a remedy leaves only the note")
			assert.Contains(t, got, "recover: "+UnprintableConfigDirNote+"\n")
			assert.NotContains(t, got, "hint:")
			assert.NotContains(t, got, "rancher ")
			assert.NotContains(t, got, "--config")
			assert.NotContains(t, got, "home/u/")

			var jsonErr bytes.Buffer
			_, ce = Run(t.Context(), noticingRead{d: Descriptor{Name: "kubeconfig", CommandType: Local}, err: remedyError()},
				mapInput{}, IO{Out: &out, Err: &jsonErr, Format: "json", RemedyDir: dir}, safety.Binding{})
			require.NotNil(t, ce)
			var env struct {
				Warnings []string `json:"warnings"`
				Data     struct {
					Error struct {
						Message string `json:"message"`
						Hint    string `json:"hint"`
					} `json:"error"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(jsonErr.Bytes(), &env))
			assert.Equal(t, "not logged in; "+UnprintableConfigDirNote, env.Data.Error.Message)
			assert.Empty(t, env.Data.Error.Hint)
			for _, s := range decodedStrings(t, "json", jsonErr.Bytes()) {
				assert.NotContains(t, s, "rancher ")
			}
		})
	}
}

// TestRemedyDirIsPerInvocation asserts two invocations running at once,
// one with a RemedyDir and one without, each print their own form: the
// directory is carried on the invocation, never in a process-wide value.
func TestRemedyDirIsPerInvocation(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "kubeconfig", CommandType: Local}
	var wg sync.WaitGroup
	results := make([]string, 40)
	for i := range results {
		wg.Go(func() {
			var out, errBuf bytes.Buffer
			dir := ""
			if i%2 == 0 {
				dir = "/d"
			}
			_, _ = Run(t.Context(), noticingRead{d: d, err: remedyError()}, mapInput{}, IO{Out: &out, Err: &errBuf, RemedyDir: dir}, safety.Binding{})
			results[i] = errBuf.String()
		})
	}
	wg.Wait()
	for i, got := range results {
		if i%2 == 0 {
			assert.Contains(t, got, "hint: rancher --config '/d' auth login 'prod'", i)
		} else {
			assert.Contains(t, got, "hint: rancher auth login 'prod'", i)
			assert.NotContains(t, got, "--config", i)
		}
	}
}

// TestRenderErrorStyledPrintsPlainRemedies asserts a renderer called
// without an invocation - the CLI layer's own failures - prints the plain
// command rather than the marker.
func TestRenderErrorStyledPrintsPlainRemedies(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "json"} {
		var b bytes.Buffer
		require.NoError(t, RenderError(&b, format, remedyError(), "note: "+clierr.Rancher+" auth status"))
		got := b.String()
		if format == "json" {
			got = strings.Join(decodedStrings(t, format, b.Bytes()), "\n")
		}
		assert.NotContains(t, got, clierr.RemedyMarker, format)
		assert.Contains(t, got, "rancher auth login 'prod'", format)
		assert.NotContains(t, got, "--config", format)
	}
}

// TestRenderErrorStyledWritesCharactersTheYamlLibraryRefuses asserts an error
// whose message, hint and warning hold DEL, U+009B and U+0085 is
// written as yaml with no error, no raw character, and the original text in
// each field.
func TestRenderErrorStyledWritesCharactersTheYamlLibraryRefuses(t *testing.T) {
	t.Parallel()
	text := "text\x7f\u009b\u0085end"
	ce := clierr.New(clierr.CodeNotLoggedIn, "message "+text).WithHint("hint " + text).WithWarnings("warning " + text)
	var b bytes.Buffer
	require.NoError(t, RenderErrorStyled(&b, "yaml", ce, style.Style{}))
	var doc struct {
		Data struct {
			Error struct {
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		} `json:"data"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, yaml.Unmarshal(b.Bytes(), &doc))
	assert.Equal(t, "message "+text, doc.Data.Error.Message)
	assert.Equal(t, "hint "+text, doc.Data.Error.Hint)
	assert.Equal(t, []string{"warning " + text}, doc.Warnings)
	assert.False(t, strings.ContainsAny(b.String(), "\x7f\u009b\u0085"), "a raw character in %q", b.String())
}

// TestDrainedNoticesNameTheInvocationsDirectory asserts a notice the
// credential store recorded outside any result - drained on the error, the
// result and the dry-run paths - names the invocation's directory in its
// commands like every other channel.
func TestDrainedNoticesNameTheInvocationsDirectory(t *testing.T) {
	t.Parallel()
	storeNotice := "the keychain is locked; run " + clierr.Rancher + " auth status"
	want := "the keychain is locked; run rancher --config '/d' auth status"
	for _, tc := range []struct {
		name            string
		cmd             Command
		dryRun, wantErr bool
	}{
		{"the error path", noticingRead{d: Descriptor{Name: "kubeconfig", CommandType: Local}, err: clierr.New(clierr.CodeAuth, "failed")}, false, true},
		{"the result path", noticingRead{d: Descriptor{Name: "kubeconfig", CommandType: Local, Shape: ShapeBytes}, res: Result{RawBytes: []byte("x\n")}}, false, false},
		{"the dry-run path", fakeMutating{d: Descriptor{Name: "clusters scale", CommandType: Mutating},
			eff:  safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: "c"},
			exec: func(context.Context) (Result, error) { return Result{}, nil }}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			streams := IO{Out: &out, Err: &errBuf, RemedyDir: "/d", DryRun: tc.dryRun, Notices: &stubNoticeSource{notices: []string{storeNotice}}}
			_, ce := Run(t.Context(), tc.cmd, mapInput{}, streams, safety.Binding{})
			if tc.wantErr {
				require.NotNil(t, ce)
			} else {
				require.Nil(t, ce, "the %s must not fail", tc.name)
			}
			if tc.dryRun {
				require.Contains(t, out.String(), "dry-run preview", "the preview rendered")
			}
			assert.Contains(t, errBuf.String(), "# warning: "+want+"\n")
			assert.NotContains(t, errBuf.String(), clierr.RemedyMarker)
		})
	}
}

// TestWithoutCommandsKeepsTheWordsBeforeTheFirstCommand asserts what the
// exec plugin's own texts become for a directory no command can name:
// their words up to the clause holding the first command, then why no
// command is offered.
func TestWithoutCommandsKeepsTheWordsBeforeTheFirstCommand(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"interactive login required for local, and this call has no terminal to answer it; to log in without one, pipe an API token to " + clierr.Rancher + " auth login 'prod' --url 'https://r' --token-stdin; or use " + clierr.Rancher + " create kubeconfig": "interactive login required for local, and this call has no terminal to answer it; " + UnprintableConfigDirNote,
		"not caching: a credential change for prod has not finished (see `" + clierr.Rancher + " config status`)":                                                                                                                                                 "not caching: a credential change for prod has not finished; " + UnprintableConfigDirNote,
		clierr.Rancher + " auth login 'prod'":                         UnprintableConfigDirNote,
		"log in again with: " + clierr.Rancher + " auth login 'prod'": UnprintableConfigDirNote,
		"no command here": "no command here",
	} {
		assert.Equal(t, want, withoutCommands(in))
	}
}

// decodedStrings decodes a rendered json or yaml document and returns every
// string it holds, keys and values, as the reader of the document sees
// them.
func decodedStrings(t *testing.T, format string, b []byte) []string {
	t.Helper()
	var doc any
	if format == "yaml" {
		require.NoError(t, yaml.Unmarshal(b, &doc))
	} else {
		require.NoError(t, json.Unmarshal(b, &doc))
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// TestAnUnprintableDirectorySaysWhyOnce asserts, for a directory that
// cannot be printed, that an error whose one command was its hint says why
// in its message; that one whose only command was in a warning says it
// there and not twice; and that a details payload's command field holds no
// sentence where a command was: it is empty.
func TestAnUnprintableDirectorySaysWhyOnce(t *testing.T) {
	t.Parallel()
	io := IO{RemedyDir: "/home/u/a\nb"}

	hintOnly := io.ExpandError(clierr.New(clierr.CodeNotLoggedIn, `not logged in to "prod"`).WithHint(clierr.Rancher + " auth login 'prod'"))
	assert.Equal(t, `not logged in to "prod"; `+UnprintableConfigDirNote, hintOnly.Message)
	assert.Empty(t, hintOnly.Hint)

	warned := io.ExpandError(clierr.New(clierr.CodeGeneric, "failed").WithWarnings("records not updated; they are listed by: " + clierr.Rancher + " config status"))
	assert.Equal(t, "failed", warned.Message)
	assert.Equal(t, []string{"records not updated; " + UnprintableConfigDirNote}, warned.Warnings())

	details := io.ExpandError(clierr.New(clierr.CodeGeneric, "the login failed").WithDetails(clierr.LoginFailureDetails{
		CredentialRecovery: &clierr.CredentialRecovery{Command: clierr.Rancher + " auth logout 'p'"},
		ConfigRecovery:     &clierr.ConfigRecovery{Entry: "p", Command: clierr.Rancher + " config delete-server 'p'"},
	}))
	d, ok := details.Details.(clierr.LoginFailureDetails)
	require.True(t, ok)
	assert.Empty(t, d.CredentialRecovery.Command)
	assert.Empty(t, d.ConfigRecovery.Command)
	assert.Equal(t, "the login failed; "+UnprintableConfigDirNote, details.Message)
	for _, line := range d.TextDetails() {
		assert.NotContains(t, line, "remove it with", "no line offers a command that is not there: %q", line)
		assert.NotContains(t, line, "clear the local credential with", "no line offers a command that is not there: %q", line)
	}

	// A hint with words beside its command is dropped whole, and the note
	// is said once, in the message.
	for _, hint := range []string{
		clierr.Rancher + " config use-server 'p' (this switches the directory's current server; switch back with " + clierr.Rancher + " config use-server 'q')",
		"unset X; then " + clierr.Rancher + " config status",
	} {
		got := io.ExpandError(clierr.New(clierr.CodeGeneric, "failed").WithHint(hint))
		assert.Empty(t, got.Hint, hint)
		assert.Equal(t, "failed; "+UnprintableConfigDirNote, got.Message, hint)
	}

	// A warning or a message that is only a command keeps the note, never
	// an empty line or a leading separator.
	onlyCommands := io.ExpandError(clierr.New(clierr.CodeGeneric, clierr.Rancher+" auth login 'p'").WithWarnings(clierr.Rancher + " config status"))
	assert.Equal(t, UnprintableConfigDirNote, onlyCommands.Message)
	assert.Equal(t, []string{UnprintableConfigDirNote}, onlyCommands.Warnings())
}

// TestOnceOnlyNoticeIsPrintedOnce asserts a once-only result whose notice
// holds a marked command is printed once: the text the result carries is
// not mirrored to the error stream a second time.
func TestOnceOnlyNoticeIsPrintedOnce(t *testing.T) {
	t.Parallel()
	marked := clierr.Rancher + " config status"
	var out, errBuf bytes.Buffer
	notice := "shown once; see " + marked
	res := Result{Text: "secret\n", OnceOnlyNotice: notice, CommittedContext: &CommittedContext{ResourceKind: "token", ResourceName: "t"}}
	cmd := fakeMutating{d: onceOnlyDesc(), eff: safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionCreate, DisplayKind: "token", DisplayName: "t"}, exec: onceOnlyExec(res)}
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Apply: true, RemedyDir: "/d"}, safety.Binding{})
	require.Nil(t, ce)
	assert.NotContains(t, errBuf.String(), "shown once", "the notice the text carries is not mirrored again")
}

// TestPrintableRemediesIsOneLineWithTheMarkersKept asserts a two-line text
// with a marked command and an escape character comes back as one line that
// keeps this process's marker and holds no escape character.
func TestPrintableRemediesIsOneLineWithTheMarkersKept(t *testing.T) {
	t.Parallel()
	got := PrintableRemedies("first\x1b[2J\nrun " + clierr.Rancher + " auth status\r\nlast\u202e")
	assert.Equal(t, "first[2Jrun "+clierr.Rancher+" auth statuslast", got)
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "\x1b")
}
