package cli

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/secret"
)

// flagKind is the kind of value a flag takes.
type flagKind int

const (
	// boolFlag takes no value: true when typed, or the value joined to it
	// with "=".
	boolFlag flagKind = iota + 1
	// stringFlag takes one value, the last one typed; a flag with Once
	// refuses a second typing.
	stringFlag
	// sliceFlag takes one value for each time it is typed, never split on
	// commas.
	sliceFlag
)

// Flag is one flag of a command.
type Flag struct {
	Name    string
	Aliases []string
	// Usage is the flag's description. A text between backticks in it is
	// the placeholder of the help row of a flag that takes a value; the row
	// of a bool flag prints the usage as written.
	Usage string
	Kind  flagKind
	// Once marks a flag that may be typed once on a command line.
	Once bool
	// Local marks the help and version flags, which a subcommand does not
	// inherit.
	Local bool
	// Secret marks a slice flag whose values the command reads only through
	// Input.Secrets, as secret.Value: Strings and String of the flag return
	// nothing.
	Secret bool
	// EnvVar names the environment variable a flag not typed reads. Only
	// --config has one.
	EnvVar string
}

// Names returns the name of the flag, then its aliases.
func (f *Flag) Names() []string {
	return append([]string{f.Name}, f.Aliases...)
}

// takesValue reports whether the flag takes a value.
func (f *Flag) takesValue() bool { return f.Kind != boolFlag }

// Command is one command of the tree: the root, a group of commands or a
// command that runs.
type Command struct {
	Name, Usage, ArgsUsage string
	Aliases                []string
	// Description is the Prime sentence of a Prime command, or "".
	Description string
	// Flags are the command's own flags. link adds the help flag to every
	// command and the version flag to the root.
	Flags    []*Flag
	Commands []*Command
	parent   *Command
	// StopAtFirstWord is true for the root and every group: they read flags
	// only before their first argument, which names a subcommand or, when it
	// names none, is reported as an unknown command.
	StopAtFirstWord bool

	descriptor *runtime.Descriptor
	prime      bool
	project    *string

	// Before runs before the command the command line reaches, for each
	// command from the root down to it. Action runs the command. Complete
	// writes the completion candidates of the command.
	Before   func(ctx context.Context, c *Command, in *Input) error
	Action   func(ctx context.Context, c *Command, in *Input) error
	Complete func(ctx context.Context, c *Command, in *Input)

	// Version, Writer, ErrWriter and Reader are read on the root only.
	// Run sets the three streams to the process's when they are nil.
	Version           string
	Writer, ErrWriter io.Writer
	Reader            io.Reader
}

// Root returns the root of the tree c belongs to.
func (c *Command) Root() *Command {
	if c.parent == nil {
		return c
	}
	return c.parent.Root()
}

// Lineage returns c, then its parent and so on up to the root.
func (c *Command) Lineage() []*Command {
	lineage := []*Command{c}
	if c.parent != nil {
		lineage = append(lineage, c.parent.Lineage()...)
	}
	return lineage
}

// Path returns the names of the commands from the root down to c.
func (c *Command) Path() []string {
	if c.parent != nil {
		return append(c.parent.Path(), c.Name)
	}
	return []string{c.Name}
}

// FullName returns the names from the root down to c, joined by spaces.
func (c *Command) FullName() string {
	return strings.Join(c.Path(), " ")
}

// Names returns the name of the command, then its aliases.
func (c *Command) Names() []string {
	return append([]string{c.Name}, c.Aliases...)
}

// HasName reports whether name is the name of the command or one of its
// aliases.
func (c *Command) HasName(name string) bool {
	return slices.Contains(c.Names(), name)
}

// Command returns the subcommand called name (by name or alias), or nil.
func (c *Command) Command(name string) *Command {
	for _, sub := range c.Commands {
		if sub.HasName(name) {
			return sub
		}
	}
	return nil
}

// VisibleCommands returns the subcommands of c. No command is hidden.
func (c *Command) VisibleCommands() []*Command { return c.Commands }

// VisibleFlags returns the flags of c, the help flag included.
func (c *Command) VisibleFlags() []*Flag { return slices.Clone(c.Flags) }

// VisiblePersistentFlags returns the flags of the commands above c that c
// inherits: the flags that are not local to their command and whose names
// no command between them and c, c included, takes.
func (c *Command) VisiblePersistentFlags() []*Flag {
	var out []*Flag
	lineage := c.Lineage()
	for i := len(lineage) - 1; i > 0; i-- {
		for _, f := range lineage[i].Flags {
			if f.Local {
				continue
			}
			if slices.ContainsFunc(f.Names(), func(name string) bool { return c.lookupFlag(name) != f }) {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// lookupFlag returns the first flag called name in c, then in each
// command above it, or nil.
func (c *Command) lookupFlag(name string) *Flag {
	for _, cmd := range c.Lineage() {
		for _, f := range cmd.Flags {
			if slices.Contains(f.Names(), name) {
				return f
			}
		}
	}
	return nil
}

// link sets the parent of every command below root and adds the help flag
// to every command, and the version flag to the root when it has a version.
// A flag a command has under a name of a built-in one keeps the name and the
// built-in flag drops it; a command that takes both names gets no built-in
// flag. It is idempotent.
func link(root *Command) *Command {
	var walk func(c *Command)
	walk = func(c *Command) {
		c.Flags = withBuiltinFlags(c)
		for _, sub := range c.Commands {
			sub.parent = c
			walk(sub)
		}
	}
	walk(root)
	return root
}

// withBuiltinFlags returns the flags of c with the built-in flags added: the
// help flag, then the version flag of the root.
func withBuiltinFlags(c *Command) []*Flag {
	flags := slices.Clip(c.Flags)
	taken := map[string]bool{}
	for _, f := range c.Flags {
		for _, name := range f.Names() {
			taken[name] = true
		}
	}
	has := func(name string) bool {
		return slices.ContainsFunc(c.Flags, func(f *Flag) bool { return f.Local && f.Name == name })
	}
	if !has("help") {
		if f := builtinFlag(&Flag{Name: "help", Aliases: []string{"h"}, Usage: "show help", Kind: boolFlag, Local: true}, taken); f != nil {
			flags = append(flags, f)
		}
	}
	if c.parent == nil && c.Version != "" && !has("version") {
		if f := builtinFlag(&Flag{Name: "version", Aliases: []string{"v"}, Usage: "print the version", Kind: boolFlag, Local: true}, taken); f != nil {
			flags = append(flags, f)
		}
	}
	return flags
}

// builtinFlag returns f without the aliases a flag of the command already
// has, or nil when a flag of the command has the name of f.
func builtinFlag(f *Flag, taken map[string]bool) *Flag {
	if taken[f.Name] {
		return nil
	}
	kept := *f
	kept.Aliases = nil
	for _, alias := range f.Aliases {
		if !taken[alias] {
			kept.Aliases = append(kept.Aliases, alias)
		}
	}
	return &kept
}

// Input is what a command reads of the command line it runs for: the values
// of its flags and its arguments. It implements runtime.Input.
type Input struct {
	p     *parsedLine
	level int
}

// flag returns the state of the first flag called name among the flags of
// the command of in, then of the commands above it.
func (in *Input) flag(name string) *lineFlag {
	for i := in.level; i >= 0; i-- {
		lv := in.p.levels[i]
		for _, f := range lv.flags[:lv.own] {
			if slices.Contains(f.names, name) {
				return f
			}
		}
	}
	return nil
}

// fromEnv reports whether the value of the flag called name comes from its
// environment variable: the flag was not typed and the variable is present,
// an empty value included.
func (in *Input) fromEnv(name string) bool {
	_, ok := in.envValue(name)
	return ok
}

func (in *Input) envValue(name string) (string, bool) {
	f := in.flag(name)
	if f == nil || f.set || f.def == nil || f.def.EnvVar == "" {
		return "", false
	}
	return os.LookupEnv(f.def.EnvVar)
}

// String returns the value of the string flag called name.
func (in *Input) String(name string) string {
	if v, ok := in.envValue(name); ok {
		return v
	}
	if f := in.flag(name); f != nil && !f.secret() {
		return f.text
	}
	return ""
}

// Bool returns the value of the bool flag called name.
func (in *Input) Bool(name string) bool {
	if f := in.flag(name); f != nil {
		return f.value
	}
	return false
}

// Strings returns one value for each time the slice flag called name was
// typed.
func (in *Input) Strings(name string) []string {
	if f := in.flag(name); f != nil && !f.secret() {
		return slices.Clone(f.values)
	}
	return nil
}

// Secrets returns one secret value for each time the slice flag called name
// was typed, when the flag is marked Secret; any other flag has none.
func (in *Input) Secrets(name string) []secret.Value {
	f := in.flag(name)
	if f == nil || !f.secret() {
		return nil
	}
	out := make([]secret.Value, len(f.values))
	for i, v := range f.values {
		out[i] = secret.New(v)
	}
	return out
}

// IsSet reports whether the flag called name was typed. A value from the
// environment does not count.
func (in *Input) IsSet(name string) bool {
	f := in.flag(name)
	return f != nil && f.set
}

// Args returns the arguments of the command, as typed.
func (in *Input) Args() []string {
	return slices.Clone(in.p.levels[in.level].args)
}

// setReadOnly stores the resolved read-only state on the run that in belongs
// to. Every Input of the run reads it, at every command level.
func (in *Input) setReadOnly(active bool, sources []string) {
	in.p.readOnly, in.p.readOnlySources = active, sources
}

// readOnlyState returns the read-only state stored on the run that in
// belongs to; when nothing stored one, read-only mode is off and there are no
// sources.
func (in *Input) readOnlyState() (bool, []string) {
	return in.p.readOnly, in.p.readOnlySources
}

// exitCoder is an error that carries a process exit code.
type exitCoder interface {
	error
	ExitCode() int
}

// statusExit ends a run with an exit code and no message: the command has
// written what it had to say.
type statusExit int

func (s statusExit) Error() string { return "" }

func (s statusExit) ExitCode() int { return int(s) }
