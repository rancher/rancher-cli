package ui

import (
	"bytes"
	"context"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func TestCanPrompt(t *testing.T) {
	t.Parallel()
	assert.True(t, New(nil, nil, false, true).CanPrompt())
	assert.False(t, New(nil, nil, true, true).CanPrompt(), "NoInput disables")
	assert.False(t, New(nil, nil, false, false).CanPrompt(), "no TTY disables")
}

func TestNoInput(t *testing.T) {
	t.Parallel()
	assert.True(t, New(nil, nil, true, true).NoInput())
	assert.False(t, New(nil, nil, false, true).NoInput())
	assert.True(t, Discard().NoInput())
}

func TestPromptReadsLineToErr(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("alice\n"), false, true)
	got, err := p.Prompt(t.Context(), "user: ")
	require.NoError(t, err)
	assert.Equal(t, "alice", got)
	assert.Equal(t, "user: ", errw.String(), "label goes to Err")
}

func TestPromptRefusesWhenDisabled(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader("x\n"), true, true)
	_, err := p.Prompt(t.Context(), "user: ")
	require.ErrorIs(t, err, ErrNoInput)
	_, err = p.PromptSecret(t.Context(), "pw: ")
	assert.ErrorIs(t, err, ErrNoInput)
}

func TestPromptSecretReadsLineFromBuffer(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("s3cret\n"), false, true)
	got, err := p.PromptSecret(t.Context(), "pw: ")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", got)
}

func TestPromptHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A reader that would block forever if read synchronously.
	p := New(&bytes.Buffer{}, blockingReader{}, false, true)
	_, err := p.Prompt(ctx, "user: ")
	assert.ErrorIs(t, err, context.Canceled, "cancelled ctx aborts the read")
}

func TestNoticeWritesToErr(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	New(&errw, nil, false, false).Notice("heads up")
	assert.Equal(t, "heads up\n", errw.String())
}

// A background goroutine (the OAuth loopback callback handler) calling
// Notice concurrently with the main goroutine's own Notice/Prompt calls
// must not race on the shared writer. Run with -race.
func TestNoticeIsSafeForConcurrentUse(t *testing.T) {
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader(""), true, false)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 200 {
				p.Notice("concurrent notice")
			}
		})
	}
	wg.Wait()
}

func TestDiscardRefuses(t *testing.T) {
	t.Parallel()
	_, err := Discard().Prompt(t.Context(), "x")
	assert.ErrorIs(t, err, ErrNoInput)
}

// TestSelectCancelableReturnsCancellationAndRunsOnCancel asserts the
// select seam behind readPasswordTTY: a cancelled ctx returns ctx.Err()
// immediately and runs onCancel, without waiting for the blocked read.
func TestSelectCancelableReturnsCancellationAndRunsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var onCancelCalled bool
	block := make(chan struct{})
	defer close(block)
	_, err := selectCancelable(ctx, func() (string, error) {
		<-block
		return "too late", nil
	}, func() { onCancelCalled = true })

	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, onCancelCalled, "onCancel must run on the cancellation path")
}

// TestSelectCancelableReturnsReadResult asserts the non-cancelled path
// returns the read's value and error untouched, without invoking onCancel.
func TestSelectCancelableReturnsReadResult(t *testing.T) {
	t.Parallel()
	var onCancelCalled bool
	got, err := selectCancelable(t.Context(), func() (string, error) {
		return "s3cret", nil
	}, func() { onCancelCalled = true })

	require.NoError(t, err)
	assert.Equal(t, "s3cret", got)
	assert.False(t, onCancelCalled)
}

// blockingReader blocks on Read until the test goroutine is torn down.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestReadLineReadsFromInputUngated(t *testing.T) {
	t.Parallel()
	// noInput=true, isTTY=false: ReadLine is NOT gated (unlike Prompt/PromptSecret).
	p := New(&bytes.Buffer{}, strings.NewReader("id:secret\n"), true, false)
	got, err := p.ReadLine(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "id:secret", got)
}

// TestReadLineDropsLeadingByteOrderMarks asserts that every byte order mark at
// the start of the line is dropped, that a mark anywhere else stays, and that
// a line of marks only reads as an empty line, not as the end of input.
func TestReadLineDropsLeadingByteOrderMarks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"no mark", "id:secret\n", "id:secret"},
		{"one mark", "\ufeffid:secret\n", "id:secret"},
		{"two marks", "\ufeff\ufeffid:secret\n", "id:secret"},
		{"one mark and a crlf", "\ufeffid:secret\r\n", "id:secret"},
		{"a mark inside a value stays", "id:\ufeffsecret\n", "id:\ufeffsecret"},
		{"a mark after the first character stays", "i\ufeffd:secret\n", "i\ufeffd:secret"},
		{"a mark after a leading mark and a character stays", "\ufeffi\ufeffd:secret\n", "i\ufeffd:secret"},
		{"a mark at the end stays", "id:secret\ufeff\n", "id:secret\ufeff"},
		{"a line of marks only", "\ufeff\ufeff\nid:secret\n", ""},
		{"a mark at the start of line 2 is not read", "id:secret\n\ufeffnext\n", "id:secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := New(&bytes.Buffer{}, strings.NewReader(tc.in), true, false)
			got, err := p.ReadLine(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestReadLineDropsMarksOfEveryLineItReads asserts the documented limit: the
// drop applies to the start of each line ReadLine reads, not only to the start
// of stdin: a second call on one Prompter drops a mark at the start of line 2.
// The reader gives one byte at a time, so the first call takes only line 1.
func TestReadLineDropsMarksOfEveryLineItReads(t *testing.T) {
	t.Parallel()
	in := iotest.OneByteReader(strings.NewReader("\ufeffid:secret\n\ufeffnext\n"))
	p := New(&bytes.Buffer{}, in, true, false)
	first, err := p.ReadLine(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "id:secret", first)
	second, err := p.ReadLine(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "next", second)
}

func TestReadLineEmptyIsEOF(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader(""), true, false)
	_, err := p.ReadLine(t.Context())
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadLineHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := New(&bytes.Buffer{}, blockingReader{}, true, false)
	_, err := p.ReadLine(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestReadLineCancellationEndsPromptLine asserts a cancelled read terminates
// the pending prompt line on the error writer, so the error text that renders
// next starts on its own line instead of after "Select [1]: ".
func TestReadLineCancellationEndsPromptLine(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	blocked, w := io.Pipe()
	defer func() { _ = w.Close() }()
	p := New(&errBuf, blocked, false, true)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.ReadLine(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "\n", errBuf.String(), "the open prompt line must be terminated")
}

// TestPrompterRemovesControlCharactersFromANotice asserts a Prompter with no
// rewrite writes this process's marker as it is and removes every escape
// character, carriage return and format character from a notice; the newlines
// of a notice stay. The note line that says characters were left out follows
// the notice.
func TestPrompterRemovesControlCharactersFromANotice(t *testing.T) {
	t.Parallel()
	text := "a\x1b[2Jb\r\nrun " + clierr.Rancher + " auth status\u202e\u2063\nlast"
	want := "a[2Jb\nrun " + clierr.Rancher + " auth status\nlast"
	var errw bytes.Buffer
	New(&errw, strings.NewReader(""), false, true).Notice(text)
	assert.Equal(t, want+"\ncharacters a terminal cannot show were left out or replaced\n", errw.String())
}

// TestPrompterWritesHiddenCharactersOfALabelAsEscapes asserts a prompt label
// and a secret prompt label are written clause by clause as clierr.LineText
// writes them: a clause with a hidden character is a double-quoted string
// with escapes, and this process's marker stays where it is.
func TestPrompterWritesHiddenCharactersOfALabelAsEscapes(t *testing.T) {
	t.Parallel()
	text := "a\x1b[2Jb\r\nrun " + clierr.Rancher + " auth status\u202e\u2063\nlast"
	want := `"a\x1b[2Jb\r\nrun "` + clierr.Rancher + `" auth status\u202e\u2063\nlast"`
	t.Run("prompt label", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).Prompt(t.Context(), text)
		require.NoError(t, err)
		assert.Equal(t, want, errw.String())
	})
	t.Run("secret prompt label", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).PromptSecret(t.Context(), text)
		require.NoError(t, err)
		assert.Equal(t, want, errw.String())
	})
}

// TestPrompterWritesALabelWithNothingToQuoteAsItIs asserts a label with no
// hidden character (the token prompt and the trust prompt are two) and the
// line end written after a read without echo are written byte for byte.
func TestPrompterWritesALabelWithNothingToQuoteAsItIs(t *testing.T) {
	t.Parallel()
	for _, label := range []string{
		"Enter token (id:secret): ",
		"trust this certificate for this server entry? [y/N]: ",
		"Enter username [admin]: ",
		"name (o'brien): ",
	} {
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).Prompt(t.Context(), label)
		require.NoError(t, err)
		assert.Equal(t, label, errw.String())
	}

	var errw bytes.Buffer
	New(&errw, strings.NewReader(""), false, true).writeErr("\n")
	assert.Equal(t, "\n", errw.String())
}

// TestPrompterKeepsLabelsThatDifferByAHiddenCharacterApart asserts labels that
// differ only by a hidden character print as three different lines.
func TestPrompterKeepsLabelsThatDifferByAHiddenCharacterApart(t *testing.T) {
	t.Parallel()
	lines := map[string]bool{}
	for _, label := range []string{"a\u200bb", "a\u200cb", "ab"} {
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).Prompt(t.Context(), label)
		require.NoError(t, err)
		lines[errw.String()] = true
	}
	assert.Len(t, lines, 3)
}

// prompterReceiverName returns the receiver name of d when d is a method on
// Prompter or *Prompter with a named receiver, and "p" for any other
// declaration.
func prompterReceiverName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 || len(d.Recv.List[0].Names) != 1 {
		return "p"
	}
	typ := d.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); !ok || id.Name != "Prompter" {
		return "p"
	}
	if name := d.Recv.List[0].Names[0].Name; name != "_" {
		return name
	}
	return "p"
}

// TestOnlyWriteLabelAndTheLineEndCallWriteErr asserts that every use of
// writeErr in the package's non-test source files is one of two calls: the
// call in writeLabel with exactly clierr.LineText(label), or a call with the
// string literal "\n". A new way to write a label that skips clierr.LineText
// fails here, and so does a method value of writeErr. The scan covers whole
// files, package-level variables included, and also fails a write to the
// error writer outside Notice, writeErr and readLine: a use of the err field
// through the receiver of a Prompter method, whatever its name, or through p
// anywhere else.
func TestOnlyWriteLabelAndTheLineEndCallWriteErr(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	require.NotEmpty(t, files)

	render := func(n ast.Node) string {
		var b bytes.Buffer
		require.NoError(t, format.Node(&b, fset, n))
		return b.String()
	}
	writers := map[string]bool{"Notice": true, "writeErr": true, "readLine": true}
	seen := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, errRecv := "", "p"
			if d, ok := decl.(*ast.FuncDecl); ok {
				fn, errRecv = d.Name.Name, prompterReceiverName(d)
			}
			called := map[*ast.SelectorExpr]*ast.CallExpr{}
			ast.Inspect(decl, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "writeErr" {
						called[sel] = call
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				where := fset.Position(sel.Pos()).String()
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == errRecv && sel.Sel.Name == "err" && !writers[fn] {
					t.Errorf("%s: the error writer is used outside Notice, writeErr and readLine; write through writeLabel", where)
				}
				if sel.Sel.Name != "writeErr" {
					return true
				}
				seen++
				call, isCall := called[sel]
				if !isCall || len(call.Args) != 1 {
					t.Errorf("%s: writeErr may only be called as p.writeErr(clierr.LineText(label)) in writeLabel or as p.writeErr(\"\\n\"); write a label through writeLabel", where)
					return true
				}
				arg := render(call.Args[0])
				if fn == "writeLabel" {
					assert.Equal(t, "clierr.LineText(label)", arg, where)
					return true
				}
				assert.Equal(t, `"\n"`, arg, where)
				return true
			})
		}
	}
	assert.Positive(t, seen, "the scan found no use of writeErr")
}

// TestPrompterRunsTheNoticeRewriteBeforeTheFilter asserts that a rewrite
// that appends an escape sequence leaves none in the written notice, that
// the rewrite sees the message as it was given, that the note line follows
// the notice because the filter removed the escape sequence, and that a label
// is not rewritten: only a notice is.
func TestPrompterRunsTheNoticeRewriteBeforeTheFilter(t *testing.T) {
	t.Parallel()
	var seen string
	rewrite := func(s string) string {
		seen = s
		return strings.ReplaceAll(s, clierr.Rancher, "rancher --config 'd'") + "\x1b[2J"
	}
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("x\n"), false, true).WithNoticeRewrite(rewrite)
	given := "run " + clierr.Rancher + " auth status\x1b[31m"
	p.Notice(given)
	assert.Equal(t, given, seen)
	assert.Equal(t, "run rancher --config 'd' auth status[31m[2J\ncharacters a terminal cannot show were left out or replaced\n", errw.String())

	errw.Reset()
	_, err := p.Prompt(t.Context(), "label "+clierr.Rancher+" x")
	require.NoError(t, err)
	assert.Equal(t, "label "+clierr.Rancher+" x", errw.String(), "a label is written as it is, with this process's marker")
}
