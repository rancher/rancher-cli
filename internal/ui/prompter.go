// Package ui provides the single injected prompter/notifier for interactive
// commands. Prompts and notices go to Err. Stdout is owned by the pipeline for
// machine output (e.g. the exec-credential JSON). Prompting is refused unless
// attached to a TTY and not running under --no-input.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// ErrNoInput is returned by Prompt/PromptSecret when interaction is disallowed
// (no TTY, or --no-input). Callers map it to the appropriate clierr.Error.
var ErrNoInput = errors.New("input required but interaction is disabled")

// Prompter carries the resolved streams and interactivity flags. mu guards
// every write to err: a background goroutine (e.g. the OAuth loopback
// callback handler) can call Notice concurrently with the main goroutine's
// own prompts/notices to the same writer.
type Prompter struct {
	mu      sync.Mutex
	err     io.Writer
	in      io.Reader
	noInput bool
	isTTY   bool
}

// New builds a Prompter from resolved streams and flags.
func New(errw io.Writer, in io.Reader, noInput, isTTY bool) *Prompter {
	return &Prompter{err: errw, in: in, noInput: noInput, isTTY: isTTY}
}

// Discard is a Prompter currently used only by tests: prompts fail, notices
// vanish. Kept deliberately despite no production callers — it is the test
// suite's standard fixture.
func Discard() *Prompter {
	return &Prompter{err: io.Discard, in: strings.NewReader(""), noInput: true}
}

// CanPrompt is the single source of truth for whether prompting is allowed.
func (p *Prompter) CanPrompt() bool { return p.isTTY && !p.noInput }

// NoInput reports whether interaction is disabled entirely (the --no-input flag).
// Distinct from CanPrompt: a print-and-poll flow (SAML, OAuth device-code) needs a
// user to act out-of-band (!NoInput) but no terminal, whereas reading stdin needs
// a TTY (CanPrompt).
func (p *Prompter) NoInput() bool { return p.noInput }

// Notice writes an informational/warning line to Err (never Out).
func (p *Prompter) Notice(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintln(p.err, msg)
}

// Prompt writes label to Err and reads one line from In, honoring ctx.
func (p *Prompter) Prompt(ctx context.Context, label string) (string, error) {
	if !p.CanPrompt() {
		return "", ErrNoInput
	}
	p.writeErr(label)
	return p.readLine(ctx)
}

// PromptSecret reads without echo when In is a terminal fd; otherwise reads a
// line (test buffers). Refuses when prompting is disallowed.
func (p *Prompter) PromptSecret(ctx context.Context, label string) (string, error) {
	if !p.CanPrompt() {
		return "", ErrNoInput
	}
	p.writeErr(label)
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		p.writeErr("\n")
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return p.readLine(ctx)
}

// writeErr writes s to err under mu — held only for the write itself, never
// across a blocking read, so a concurrent Notice (e.g. from a background
// callback handler) is never blocked behind a pending prompt.
func (p *Prompter) writeErr(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprint(p.err, s)
}

// ReadLine reads a single line from In, honoring ctx, with NO TTY/NoInput gating.
// It is for explicit programmatic input (e.g. --token-stdin) fed by a pipe or file
// redirect, which is not a terminal — distinct from PromptSecret (interactive,
// TTY-gated, no-echo). Returns io.EOF only when the read yields no bytes; data
// present on the final read is returned with no error.
func (p *Prompter) ReadLine(ctx context.Context) (string, error) {
	return p.readLine(ctx)
}

// readLine reads a single line in a goroutine and aborts on ctx cancellation.
// A cancelled read abandons the goroutine (the process is exiting).
// Creating a fresh Scanner per call is safe because callers read exactly once
// (either stdin OR prompt, never both) — a single ungated ReadLine is fine.
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
		// A cancelled read (Ctrl-C at a prompt) leaves the prompt line
		// unterminated; end it so whatever renders next — the error text —
		// starts on its own line.
		p.mu.Lock()
		_, _ = fmt.Fprintln(p.err)
		p.mu.Unlock()
		return "", ctx.Err()
	case r := <-ch:
		return r.line, r.err
	}
}
