// Package ui provides the single injected prompter/notifier for interactive
// commands. Prompts and notices always go to Err; Out is reserved for machine
// output (e.g. the exec-credential JSON). Prompting is refused unless attached
// to a TTY and not running under --no-input.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrNoInput is returned by Prompt/PromptSecret when interaction is disallowed
// (no TTY, or --no-input). Callers map it to the appropriate clierr.Error.
var ErrNoInput = errors.New("input required but interaction is disabled")

// Prompter carries the resolved streams and interactivity flags.
type Prompter struct {
	out, err io.Writer
	in       io.Reader
	noInput  bool
	isTTY    bool
}

// New builds a Prompter from resolved streams and flags.
func New(out, errw io.Writer, in io.Reader, noInput, isTTY bool) *Prompter {
	return &Prompter{out: out, err: errw, in: in, noInput: noInput, isTTY: isTTY}
}

// Discard is a non-interactive Prompter for tests and non-interactive callers:
// output is dropped and any prompt returns ErrNoInput.
func Discard() *Prompter {
	return &Prompter{out: io.Discard, err: io.Discard, in: strings.NewReader(""), noInput: true}
}

// CanPrompt is the single source of truth for whether prompting is allowed.
func (p *Prompter) CanPrompt() bool { return p.isTTY && !p.noInput }

// Notice writes an informational/warning line to Err (never Out).
func (p *Prompter) Notice(msg string) { _, _ = fmt.Fprintln(p.err, msg) }

// Prompt writes label to Err and reads one line from In, honoring ctx.
func (p *Prompter) Prompt(ctx context.Context, label string) (string, error) {
	if !p.CanPrompt() {
		return "", ErrNoInput
	}
	_, _ = fmt.Fprint(p.err, label)
	return p.readLine(ctx)
}

// PromptSecret reads without echo when In is a terminal fd; otherwise reads a
// line (test buffers). Refuses when prompting is disallowed.
func (p *Prompter) PromptSecret(ctx context.Context, label string) (string, error) {
	if !p.CanPrompt() {
		return "", ErrNoInput
	}
	_, _ = fmt.Fprint(p.err, label)
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(p.err)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return p.readLine(ctx)
}

// readLine reads a single line in a goroutine and aborts on ctx cancellation.
// A cancelled read abandons the goroutine (the process is exiting).
func (p *Prompter) readLine(ctx context.Context) (string, error) {
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		sc := bufio.NewScanner(p.in)
		if sc.Scan() {
			ch <- res{line: sc.Text()}
			return
		}
		if err := sc.Err(); err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{err: io.EOF}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.line, r.err
	}
}
