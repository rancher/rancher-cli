package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// outcomeKind says how a command line ended.
type outcomeKind int

const (
	// outcomeRan: a command that runs ran.
	outcomeRan outcomeKind = iota + 1
	// outcomeBeforeFailed: a Before of the command or of a command above it
	// returned an error, and the command did not run.
	outcomeBeforeFailed
	// outcomeHelp: a help page was shown, for the command in outcome.cmd.
	outcomeHelp
	// outcomeVersion: the version was shown.
	outcomeVersion
	// outcomeUsageError: the command line has a flag error, of the class in
	// outcome.class.
	outcomeUsageError
	// outcomeUnknownCommand: the first argument after the commands reached
	// names no command.
	outcomeUnknownCommand
	// outcomeCompletion: a completion request was handled.
	outcomeCompletion
	// outcomeRefused: the command line was refused before the dispatcher
	// acted on it.
	outcomeRefused
)

// outcome is how one command line ended.
type outcome struct {
	kind outcomeKind
	// cmd is the command whose help page was shown, the command a completion
	// request reached, or the command that reported the error.
	cmd *Command
	// class is the class of the flag error of an outcomeUsageError.
	class lineErrorClass
	// err is the error that ended the run, nil for a success.
	err error
	// writeErr is the first error of a failed write of a help page or the
	// version.
	writeErr error
}

// Run runs args (the program name, then the arguments typed) on c, the root
// of the command tree after link ran, and returns the error that ended the
// run. It parses the command line, refuses a command line that is unsafe to
// run and acts on the others: it runs a command, shows a help page or the
// version, reports an error, or handles a completion request. It records
// nothing and does not capture the command line (CaptureArgv). A completion
// request, the check of the flags of a command and the text and format of an
// error read the captured command line, which the caller captures first, as
// a process does. A run sets the root's Reader, Writer and ErrWriter when
// they are nil and reads the captured command line, which is process-wide.
func (c *Command) Run(ctx context.Context, args []string) error {
	_, oc := runArgv(ctx, c, args, false)
	if oc.err != nil {
		return oc.err
	}
	return oc.writeErr
}

// runBefore runs the Before function of each command from the root down to
// the command at index last of p.levels, and stops at the first error.
func runBefore(ctx context.Context, p *parsedLine, last int) error {
	for i := 0; i <= last; i++ {
		c := p.levels[i].cmd
		if c.Before == nil {
			continue
		}
		if err := c.Before(ctx, c, &Input{p: p, level: i}); err != nil {
			return err
		}
	}
	return nil
}

// dispatch acts on the command line p, which the command-line parser read, by
// the way it ends. A command line that ends at the root or a group (a bare
// group shows its help page after the checks), at an argument that names no
// command, or at a command that runs, first runs the Before function of each
// command from the root down to it. A help flag (also `--help` followed by an
// argument that names no command), the version flag and a flag error run no
// Before function.
func dispatch(ctx context.Context, root *Command, p *parsedLine) outcome {
	end := p.end
	lv := p.levels[end.level]
	c := lv.cmd
	in := &Input{p: p, level: end.level}
	switch end.kind {
	case lineError:
		return usageErrorOutcome(c, lv.usageErr)
	case lineHelp:
		if end.afterError {
			return helpPageOutcome(c, in)
		}
		return helpFlagOutcome(c, lv.args, in)
	case lineVersion:
		return versionOutcome(root)
	}
	if err := runBefore(ctx, p, end.level); err != nil {
		return outcome{kind: outcomeBeforeFailed, cmd: c, err: err}
	}
	if len(c.Commands) == 0 {
		if c.Action == nil {
			return outcome{kind: outcomeRan, cmd: c}
		}
		return outcome{kind: outcomeRan, cmd: c, err: c.Action(ctx, c, in)}
	}
	if len(lv.args) > 0 {
		return unknownCommandOutcome(c, lv.args[0], lv.args[1:])
	}
	return helpPageOutcome(c, in)
}

// helpFlagOutcome shows the help page a help flag asks for at the command c,
// whose arguments are args: the page of the command c, or, when c has
// subcommands and an argument names one, of the command the arguments name.
func helpFlagOutcome(c *Command, args []string, in *Input) outcome {
	if len(args) > 0 && args[0] != "" {
		return showCommandHelp(c, args[0], args[1:], in)
	}
	return helpPageOutcome(c, in)
}

// visibleSubcommand returns the subcommand of node named word (by name or
// alias), or nil. It is the one lookup behind every command of the `--help
// <arguments>` path and behind the hint for a mistyped `help`.
func visibleSubcommand(node *Command, word string) *Command {
	for _, sub := range node.VisibleCommands() {
		if sub.HasName(word) {
			return sub
		}
	}
	return nil
}

// showCommandHelp shows the help page `<c> --help <name> <arguments>` asks
// for. Two cases:
//   - c has no subcommand: name is an argument of c, not a topic, and c
//     shows its own page.
//   - c has subcommands: the arguments that follow name make a path, and the
//     page is that of the deepest command they name: `rancher --help get
//     pod` shows `get pod`. The path ends at a flag or at a command with no
//     subcommands, which takes the next arguments as its own. An argument that
//     names no command under a command with subcommands is an unknown
//     command, reported with the arguments typed after it.
func showCommandHelp(c *Command, name string, words []string, in *Input) outcome {
	if len(c.Commands) == 0 {
		return helpPageOutcome(c, in)
	}
	node := visibleSubcommand(c, name)
	if node == nil {
		return unknownCommandOutcome(c, name, words)
	}
	for i, word := range words {
		if strings.HasPrefix(word, "-") || len(node.VisibleCommands()) == 0 {
			break
		}
		next := visibleSubcommand(node, word)
		if next == nil {
			return unknownCommandOutcome(node, word, words[i+1:])
		}
		node = next
	}
	return helpPageOutcome(node, in)
}

// helpPageOutcome writes the help page of c: the root page for the root, the
// group page for a command with subcommands, the page of a command that runs
// for the others. The color value comes from in.
func helpPageOutcome(c *Command, in *Input) outcome {
	page := helpPageLeaf
	switch {
	case c.parent == nil:
		page = helpPageRoot
	case len(c.VisibleCommands()) > 0:
		page = helpPageGroup
	}
	err := renderHelpPage(c.Root().Writer, page, c, helpStyle(c, in))
	return outcome{kind: outcomeHelp, cmd: c, writeErr: err}
}

// versionOutcome writes the version line of the root.
func versionOutcome(root *Command) outcome {
	_, err := fmt.Fprintf(root.Writer, "%v version %v\n", root.Name, root.Version)
	return outcome{kind: outcomeVersion, cmd: root, writeErr: err}
}

// unknownCommandOutcome renders the unknown-command error for name under c.
// after is the arguments typed after name.
func unknownCommandOutcome(c *Command, name string, after []string) outcome {
	return outcome{kind: outcomeUnknownCommand, cmd: c, err: exit(reportUnknownCommand(c, name, after))}
}

// usageErrorOutcome renders the flag error ue of the command c.
func usageErrorOutcome(c *Command, ue usageError) outcome {
	msg, hint := usageErrorMessage(c, ue)
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	renderRawArgvError(c, ce)
	return outcome{kind: outcomeUsageError, cmd: c, class: ue.class, err: exit(ce)}
}

// complete handles the completion request of the command line captured in
// rawArgv. It parses the arguments before the first "--" (every argument
// after one stays an argument, which the completion of the command reads
// itself from rawArgv), runs the Before function of each command from the
// root down to the command it reaches and then that command's Complete. A
// completion request with a help or version flag before a command name gets
// no candidates: the shell would read a help page or the version as
// candidates.
func (c *Command) complete(ctx context.Context) outcome {
	p := parseCompletion(c, completionWords(rawArgv))
	last := p.end.level
	reached := p.levels[last].cmd
	if err := runBefore(ctx, &p, last); err != nil {
		return outcome{kind: outcomeBeforeFailed, cmd: reached, err: err}
	}
	if !p.helpOrVersionBeforeCommand && reached.Complete != nil {
		reached.Complete(ctx, reached, &Input{p: &p, level: last})
	}
	return outcome{kind: outcomeCompletion, cmd: reached}
}

// completionWords returns the arguments of a completion request before its
// first "--".
func completionWords(argv []string) []string {
	if i := slices.Index(argv, "--"); i >= 0 {
		return argv[:i]
	}
	return argv
}
