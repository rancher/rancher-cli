package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// GlobalFlags returns the confirmation/output flags shared by all commands.
// These are placed on the root command in v3; they are inherited by subcommands
// via urfave/cli/v3's flag resolution (e.g. `rancher ping -o json`,
// `rancher clusters scale ... --apply`). (--config and --debug are on root in root.go.)
//
// These TTY usage strings are the operator-facing counterpart of the schema
// descriptions in runtime.GlobalFlags() (registry.go) — two deliberately
// separate strings (terse here, fuller there), kept in sync by hand.
func GlobalFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.BoolFlag{Name: "dry-run", Usage: "preview the changes without applying them"},
		&ucli.BoolFlag{Name: "apply", Usage: "apply the change"},
		&ucli.BoolFlag{Name: "yes", Usage: "confirm a destructive change"},
		&ucli.BoolFlag{Name: "allow-deprecated", Usage: "allow writing to a deprecated resource"},
		&ucli.BoolFlag{Name: "no-input", Usage: "never prompt; ambiguity becomes an error"},
		&ucli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "table|json|yaml|name|template=<tpl>"},
		&ucli.StringFlag{Name: "color", Usage: "auto|always|never"},
		&ucli.StringFlag{Name: "project", Usage: "scope operations to project namespaces (downstream namespaced resources only)"},
		&ucli.BoolFlag{Name: "read-only", Usage: "refuse commands that make changes this session (one-way)"},
	}
}

// cmdInput adapts a *cli.Command to runtime.Input.
type cmdInput struct{ c *ucli.Command }

func (i cmdInput) String(name string) string    { return i.c.String(name) }
func (i cmdInput) Bool(name string) bool        { return i.c.Bool(name) }
func (i cmdInput) Strings(name string) []string { return i.c.StringSlice(name) }
func (i cmdInput) IsSet(name string) bool       { return i.c.IsSet(name) }
func (i cmdInput) Args() []string               { return i.c.Args().Slice() }

// leafName returns the last space-separated token of a command name
// ("auth status" -> "status") for use as the urfave leaf/subcommand name.
func leafName(fullName string) string {
	if idx := strings.LastIndex(fullName, " "); idx >= 0 {
		return fullName[idx+1:]
	}
	return fullName
}

// groupName returns the verb-group prefix of a command name ("auth status" ->
// "auth", "get pod" -> "get", "schema" -> ""). The registration loop checks it
// against the entry's declared group.
func groupName(fullName string) string {
	if idx := strings.LastIndex(fullName, " "); idx >= 0 {
		return fullName[:idx]
	}
	return ""
}

// flagsFor converts a Descriptor's named Flags into urfave flags (string/bool).
// ArgSpec.Desc feeds the flag's TTY usage text; a backtick-quoted word in it
// sets the value placeholder (urfave's unquoteUsage), e.g. a Desc of
// "patch `file`" renders "--filename file". Required-ness and Enum membership
// are intentionally NOT set on the urfave flag: they are enforced by
// runtime.validateInput so violations surface as structured clierr envelopes
// (urfave's own required/validator errors are unstructured).
func flagsFor(d runtime.Descriptor) []ucli.Flag {
	var out []ucli.Flag
	for _, f := range d.Flags {
		switch f.Type {
		case "bool":
			out = append(out, &ucli.BoolFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc})
		case "stringSlice":
			out = append(out, &ucli.StringSliceFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc})
		default:
			out = append(out, &ucli.StringFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc})
		}
	}
	return out
}

// argsUsage renders a Descriptor's positional Args as urfave's ArgsUsage
// help string: "<name>" for a Required arg, "[name]" otherwise, space-joined
// in declared order. When an arg carries a non-empty Enum the name is replaced
// by the pipe-joined allowed values (e.g. "<bash|zsh|fish|pwsh>"). This is the
// SAME Args (incl. Required and Enum) the schema manifest publishes, so help
// text cannot drift from the manifest.
func argsUsage(args []runtime.ArgSpec) string {
	parts := make([]string, len(args))
	for i, a := range args {
		label := a.Name
		if len(a.Enum) > 0 {
			label = strings.Join(a.Enum, "|")
		}
		if a.Required {
			parts[i] = "<" + label + ">"
		} else {
			parts[i] = "[" + label + "]"
		}
	}
	return strings.Join(parts, " ")
}

// buildIO reads the resolved global flags/streams for a run.
func buildIO(c *ucli.Command) runtime.IO {
	r := c.Root()
	in := r.Reader
	if in == nil {
		in = os.Stdin
	}
	isTTY := false
	if fp, ok := in.(*os.File); ok {
		isTTY = term.IsTerminal(int(fp.Fd()))
	}
	readOnly, readOnlySources := config.ReadOnlyOverride()
	return runtime.IO{
		Out:             r.Writer,
		Err:             r.ErrWriter,
		In:              in,
		Format:          c.String("output"),
		Color:           c.String("color"),
		DryRun:          c.Bool("dry-run"),
		Apply:           c.Bool("apply"),
		Yes:             c.Bool("yes"),
		AllowDeprecated: c.Bool("allow-deprecated"),
		NoInput:         c.Bool("no-input"),
		IsTTY:           isTTY,
		OutTTY:          fileIsTTY(r.Writer),
		ErrTTY:          fileIsTTY(r.ErrWriter),
		ReadOnly:        readOnly,
		ReadOnlySources: readOnlySources,
	}
}

// fileIsTTY reports whether w is a terminal file descriptor — false for any
// writer that is not an *os.File (a buffer, a pipe destination that is not
// itself the terminal).
func fileIsTTY(w io.Writer) bool {
	fp, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(fp.Fd()))
}

// resolveReadOnly OR's the three read-only sources into one bool plus active
// source names. It resets then conditionally re-sets the stored config so
// completion always reflects this invocation's config state (AC-19: one load
// per TAB). Config load failure degrades gracefully: read-only state still
// derives from env/flag sources; stored config is cleared rather than left
// stale.
func resolveReadOnly(c *ucli.Command) (bool, []string) {
	setStoredConfig(nil, "") // clear any prior stored config for this invocation
	var sources []string
	if dir, err := config.ResolveDir(c.String("config")); err == nil {
		if cfg, err := config.Load(config.FilePath(dir)); err == nil {
			setStoredConfig(cfg, dir) // single load shared with completion (AC-19)
			completionDebugf("config loaded: dir=%s currentServer=%q servers=%d", dir, cfg.CurrentServer, len(cfg.Servers))
			if cfg.ReadOnly {
				sources = append(sources, "config readOnly")
			}
		} else {
			completionDebugf("config load failed: %v", err)
		}
	} else {
		completionDebugf("config dir resolve failed: %v", err)
	}
	if os.Getenv(config.EnvReadOnly) != "" {
		sources = append(sources, config.EnvReadOnly)
	}
	if c.Bool("read-only") {
		sources = append(sources, "--read-only")
	}
	return len(sources) > 0, sources
}

// Adapt wraps a fixed runtime.Command that needs no per-run config dir/store
// (e.g. `schema`, `completion`). The binding is derived from the registry
// uniformly, so a mutating command wired this way still runs under its gate.
func Adapt(reg *runtime.Registry, rc runtime.Command) *ucli.Command {
	d := rc.Describe()
	b := reg.Binding(d.Name)
	cmd := &ucli.Command{
		Name:        leafName(d.Name),
		Aliases:     d.Aliases,
		Usage:       d.Usage,
		Description: primeDescription(d),
		ArgsUsage:   argsUsage(d.Args),
		Flags:       flagsFor(d),
		// Slice flags receive one entry per occurrence: urfave's default
		// comma-splitting would tear a value like `--set tags=a,b` into two
		// entries (MC-6a). Only slice/map flags read this; single-value
		// flags are untouched.
		DisableSliceFlagSeparator: true,
		OnUsageError:              onUsageError,
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			// Flags have already parsed successfully at this point, so the
			// invocation's own --output value is trusted directly — no raw-argv
			// scan needed here, unlike commandNotFound/onUsageError below.
			if ce := checkFlagValues(d.Flags, rawArgv, completionActive()); ce != nil {
				_ = runtime.RenderErrorStyled(c.Root().ErrWriter, c.String("output"), ce, errStyle(c, c.String("color")))
				return ctx, exit(ce)
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, c *ucli.Command) error {
			streams := buildIO(c)
			// A command wired through Adapt has no store dependency
			// (schema, completion), so streams.Notices stays nil: nothing
			// here can record a notice, and resolving a dir/store would
			// cost a config.yaml read+parse on every invocation purely to
			// fill a seam nothing drains. A future local command that
			// grows a store dependency registers through adaptEntry,
			// which wires the store and its notices.
			status, ce := runtime.Run(ctx, rc, cmdInput{c}, streams, b)
			if ce != nil {
				return exit(ce)
			}
			if status != 0 {
				return ucli.Exit("", status)
			}
			return nil
		},
	}
	installShellComplete(rc, cmd)
	return cmd
}

// primeDescription is the normal `--help` marker seam: urfave's own default
// help template renders a command's Description in a DESCRIPTION: block
// (Usage feeds every other consumer — the TOC, the manifest, the command
// card's usage line — and must never carry the marker). "" leaves urfave's
// DESCRIPTION: block absent, exactly as for every non-Prime command today.
func primeDescription(d runtime.Descriptor) string {
	if d.Capability.Tier == runtime.CapabilityPrime {
		return runtime.PrimeSentence
	}
	return ""
}

// installShellComplete installs the universal completion callback on cmd for
// every adapted leaf (AC-17a). When the command also implements
// runtime.ShellCompleter, the config-local delegation layer is wired in.
func installShellComplete(rc runtime.Command, cmd *ucli.Command) {
	d := rc.Describe()
	var sc runtime.ShellCompleter
	if s, ok := rc.(runtime.ShellCompleter); ok {
		sc = s
	}
	cmd.ShellComplete = universalShellCompleteFunc(d, sc)
}

// resolveDeps resolves the config dir from --config (flag > env > XDG) and
// opens the credential store per the config's policy. This is THE single
// place dir/store are resolved, so --config always takes effect.
// config.LoadAndStore encapsulates the Load + NewLazyStore pairing. The store
// is LAZY: policy-name validation happens here (fail fast on every
// resolveDeps-backed command), but the auto probe / credstore-pin write /
// config-dir creation defer to the first credential call — an invocation
// that never touches credentials leaves no store-init side effects
// (spec LS-1; Windows acceptance FAIL 2).
func resolveDeps(c *ucli.Command) (string, config.CredStore, error) {
	dir, err := config.ResolveDir(c.String("config"))
	if err != nil {
		return "", nil, err
	}
	_, store, err := config.LoadAndStore(dir)
	if err != nil {
		return "", nil, err
	}
	return dir, store, nil
}

// exit converts a structured error into a urfave exit with the right code.
func exit(ce *clierr.Error) error {
	return ucli.Exit("", ce.ExitCode())
}

// hintEntry pairs a matcher with the extra MESSAGE-body detail it selects
// (never a hint — none of these entries has a single concrete, runnable
// command to offer beyond the generic schema pointer every caller falls
// back to) and an optional hint override for the rare entry that does have
// one. Both receive the same input as match so one entry can vary its
// wording by context instead of relying on table order between overlapping
// entries.
type hintEntry[T any] struct {
	match  func(T) bool
	detail func(T) string // extra message-body text; "" adds nothing
	hint   func(T) string // hint override; nil uses the caller's generic fallback
}

// firstMatch walks an ordered registration table and returns the first
// matching entry's detail and hint-override (nil hint if the entry has
// none); ok is false when nothing matches.
func firstMatch[T any](entries []hintEntry[T], in T) (detail string, hint func(T) string, ok bool) {
	for _, e := range entries {
		if e.match(in) {
			d := ""
			if e.detail != nil {
				d = e.detail(in)
			}
			return d, e.hint, true
		}
	}
	return "", nil, false
}

// constText wraps a fixed string for a detail or hint field whose wording
// does not vary by input.
func constText[T any](s string) func(T) string {
	return func(T) string { return s }
}

// genericSchemaHint is the hint every commandNotFound/onUsageError caller
// falls back to when no targeted entry matches, or a matched entry carries
// no hint override.
const genericSchemaHint = "rancher schema"

// resolveGuidance wraps firstMatch with the generic schema fallback shared
// by every guidance table: no match, or a matched entry with a nil hint,
// both resolve to genericSchemaHint.
func resolveGuidance[T any](entries []hintEntry[T], in T) (detail, hint string) {
	if d, h, ok := firstMatch(entries, in); ok {
		hint := genericSchemaHint
		if h != nil {
			hint = h(in)
		}
		return d, hint
	}
	return "", genericSchemaHint
}

// notFoundInput bundles the arguments passed to commandNotFound.
type notFoundInput struct {
	c    *ucli.Command
	name string
}

// notFoundHints is the registration table for targeted command-not-found
// guidance: the first matching entry's detail is appended to the message,
// and its hint (if any) overrides the generic schema pointer. Register new
// targeted entries here, not in the seam.
var notFoundHints = []hintEntry[notFoundInput]{
	{
		// --version is a root-level flag only; a subgroup miss like `get
		// version` has no such flag to point at, so it keeps the generic entry.
		match: func(in notFoundInput) bool {
			return in.name == "version" && in.c == in.c.Root()
		},
		detail: constText[notFoundInput]("there is no version command"),
		hint:   constText[notFoundInput]("rancher --version"),
	},
	{
		match: func(in notFoundInput) bool {
			return in.c.Name == "delete" && (in.name == "cluster" || in.name == "clusters")
		},
		detail: constText[notFoundInput]("cluster deletion goes through rancher delete provisioning-cluster <name> (management clusters are not directly deletable)"),
	},
}

// notFoundGuidance returns commandNotFound's message detail (possibly "")
// and hint for name.
func notFoundGuidance(c *ucli.Command, name string) (detail, hint string) {
	return resolveGuidance(notFoundHints, notFoundInput{c: c, name: name})
}

// usageErrorInput bundles the arguments passed to onUsageError.
type usageErrorInput struct {
	c   *ucli.Command
	err error
}

const noAllNamespacesFlag = "there is no all-namespaces flag"

// isAllNamespacesFlag reports whether err is an unknown-flag error for the
// kubectl -A/--all-namespaces reflex. urfave normalises --all-namespaces
// to -all-namespaces in its error text.
func isAllNamespacesFlag(err error) bool {
	// test constructions and future entries may hand the predicate a nil error.
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasSuffix(msg, ": -A") || strings.HasSuffix(msg, ": -all-namespaces")
}

// allNamespacesEntry is the kubectl-reflex -A/--all-namespaces entry —
// addressable by name so tests pin THIS entry, not a table position.
//
// c.Path() returns the root-to-leaf segment slice (urfave v3 Path()),
// so a `get pod` leaf reports ["rancher","get","pod"]; the `get` group
// itself reports ["rancher","get"]. The positional p[1]=="get" check
// matches only commands directly under the `get` group — a "get" segment
// anywhere deeper in the path (e.g. a hypothetical `auth get-token`) does
// not trigger the detail.
var allNamespacesEntry = hintEntry[usageErrorInput]{
	// kubectl reflex -A/--all-namespaces: the flag exists nowhere.
	// Under the get group the wording explains that lists already
	// span all namespaces; elsewhere the base message alone covers it.
	// One entry, wording chosen here — no order-dependent
	// specialize-then-generalize pair.
	match: func(in usageErrorInput) bool { return isAllNamespacesFlag(in.err) },
	detail: func(in usageErrorInput) string {
		if p := in.c.Path(); len(p) >= 2 && p[1] == "get" {
			return noAllNamespacesFlag + " — list operations already span all namespaces; use --namespace where the kind accepts one"
		}
		return noAllNamespacesFlag
	},
}

// usageErrorHints is the registration table for targeted flag-parse
// guidance: the first matching entry's detail is appended to the message;
// callers fall back to just the base message plus the generic schema hint
// when nothing matches. Register new targeted entries here, not in the seam.
var usageErrorHints = []hintEntry[usageErrorInput]{allNamespacesEntry}

// usageErrorGuidance returns onUsageError's message detail (possibly "")
// and hint for err.
func usageErrorGuidance(c *ucli.Command, err error) (detail, hint string) {
	return resolveGuidance(usageErrorHints, usageErrorInput{c: c, err: err})
}

// commandNotFound is urfave/cli's seam for an unmatched command/subcommand
// name (registered as root's and every group's CommandNotFound). Without it,
// urfave's own DefaultShowCommandHelp hard-codes a plain-text "No help topic
// for '<name>'" on stderr and exits 3 — indistinguishable from the AUTH exit
// code documented for exit 3, and with no structured error for a script to
// parse. CommandNotFoundFunc has no error return, so rendering and the
// process exit both happen here directly, mirroring how urfave's own
// ShowRootCommandHelpAndExit calls OsExiter. No flag has parsed successfully
// yet at this point, so the rendering format comes from a raw-argv scan
// (scanOutputFormat), not a parsed --output value.
func commandNotFound(_ context.Context, c *ucli.Command, name string) {
	msg := fmt.Sprintf("unknown command %q", name)
	detail, hint := notFoundGuidance(c, name)
	if detail != "" {
		msg += "; " + detail
	}
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	_ = runtime.RenderErrorStyled(c.Root().ErrWriter, scanOutputFormat(rawArgv), ce, errStyle(c, scanColorFlag(rawArgv)))
	ucli.OsExiter(ce.ExitCode())
}

// onUsageError is urfave/cli's seam for a flag-parse failure (unknown flag,
// bad value, missing value). Without it, urfave prints a plain-text
// "Incorrect Usage: ..." line and falls through to help text — no structured
// error for a script to parse, mirroring the gap commandNotFound closes for
// unmatched command names. Wired on the root command and on every leaf and
// group command via the adapters, since urfave checks OnUsageError on the
// command node whose own flags failed to parse, not the root's. This
// command's own flags failed to parse, so — like commandNotFound — the
// rendering format comes from a raw-argv scan, not a parsed --output value.
func onUsageError(_ context.Context, c *ucli.Command, err error, _ bool) error {
	msg := err.Error() + "; pass --help for usage"
	detail, hint := usageErrorGuidance(c, err)
	if detail != "" {
		msg += "; " + detail
	}
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	_ = runtime.RenderErrorStyled(c.Root().ErrWriter, scanOutputFormat(rawArgv), ce, errStyle(c, scanColorFlag(rawArgv)))
	return exit(ce)
}
