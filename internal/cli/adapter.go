package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	ucli "github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// GlobalFlags returns the confirmation/output flags shared by all commands.
// These are placed on the root command in v3; they are inherited by subcommands
// via urfave/cli/v3's flag resolution (e.g. `rancher ping -o json`,
// `rancher clusters scale ... --apply`). (--config and --debug are on root in root.go.)
//
// These TTY usage strings are the operator-facing counterpart of the schema
// descriptions in runtime.GlobalFlags() (registry.go) - two deliberately
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
		&ucli.StringFlag{Name: "project", Usage: "the project, by id or display name (only commands that take --project)"},
		&ucli.BoolFlag{Name: "read-only", Usage: "refuse commands that make changes this session (one-way); not accepted by local commands"},
	}
}

// cmdInput adapts a *cli.Command to runtime.Input.
type cmdInput struct{ c *ucli.Command }

func (i cmdInput) String(name string) string    { return i.c.String(name) }
func (i cmdInput) Bool(name string) bool        { return i.c.Bool(name) }
func (i cmdInput) Strings(name string) []string { return i.c.StringSlice(name) }
func (i cmdInput) IsSet(name string) bool       { return i.c.IsSet(name) }
func (i cmdInput) Args() []string               { return i.c.Args().Slice() }

// BoolValueArgs implements runtime.BoolValueArgs from the captured command
// line.
func (i cmdInput) BoolValueArgs() ([]runtime.BoolValue, string, bool) {
	return boolValueArgs(rawArgv, i.c)
}

// EmptyBoolFlags implements runtime.EmptyBoolFlags from the captured
// command line.
func (i cmdInput) EmptyBoolFlags() ([]string, bool) {
	return emptyBoolFlags(rawArgv, i.c)
}

// DroppedWords implements runtime.DroppedWords from the captured command
// line. The format and color come from the raw scans the root and group
// refusals render with (scanOutputFormat, scanColorFlag), so a refusal
// renders alike at every level.
func (i cmdInput) DroppedWords() ([]string, string, string, bool) {
	words, ok := droppedWords(rawArgv, i.c)
	return words, scanOutputFormat(rawArgv), scanColorFlag(rawArgv), ok
}

// leafName returns the last space-separated token of a command name
// ("auth status" -> "status") for use as the urfave leaf/subcommand name.
func leafName(fullName string) string {
	if _, leaf, found := strings.CutLast(fullName, " "); found {
		return leaf
	}
	return fullName
}

// groupName returns the verb-group prefix of a command name ("auth status" ->
// "auth", "get pod" -> "get", "schema" -> ""). The registration loop checks it
// against the entry's declared group.
func groupName(fullName string) string {
	if group, _, found := strings.CutLast(fullName, " "); found {
		return group
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
		if f.Name == runtime.FlagProject {
			continue // the global --project flag; listing it declares that the command takes it
		}
		switch f.Type {
		case "bool":
			out = append(out, &ucli.BoolFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc, OnlyOnce: f.Once})
		case "stringSlice":
			out = append(out, &ucli.StringSliceFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc, OnlyOnce: f.Once})
		default:
			out = append(out, &ucli.StringFlag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc, OnlyOnce: f.Once})
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

// fileIsTTY reports whether w is a terminal file descriptor - false for any
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
// completion always reflects this invocation's config state (one load
// per TAB). Config load failure degrades gracefully: read-only state still
// derives from env/flag sources; stored config is cleared rather than left
// stale.
func resolveReadOnly(c *ucli.Command) (bool, []string) {
	setStoredConfig(nil, "") // clear any prior stored config for this invocation
	config.SetProcessCacheAnchor(nil)
	var sources []string
	if dir, err := config.ResolveDir(c.String("config")); err == nil {
		// The binding stamp is read BEFORE the config load, and the pair
		// is what every cache reader without an anchor of its own - the
		// legacy shim, live completion - judges a record against. One
		// read per invocation: a reader takes a second only when it is
		// about to hand a cached credential back.
		anchor := config.NewCacheAnchor(dir)
		if cfg, err := config.Load(config.FilePath(dir)); err == nil {
			setStoredConfig(cfg, dir) // single load shared with completion
			config.SetProcessCacheAnchor(anchor.WithConfig(cfg))
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
		Metadata:    leafMetadata(d),
		ArgsUsage:   argsUsage(d.Args),
		Flags:       flagsFor(d),
		// Slice flags receive one entry per occurrence: urfave's default
		// comma-splitting would tear a value like `--set tags=a,b` into two
		// entries. Only slice/map flags read this; single-value
		// flags are untouched.
		DisableSliceFlagSeparator: true,
		OnUsageError:              onUsageError,
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			// Flags have already parsed successfully at this point, so the
			// invocation's own --output value is trusted directly - no raw-argv
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
// (Usage feeds every other consumer - the TOC, the manifest, the command
// card's usage line - and must never carry the marker). "" leaves urfave's
// DESCRIPTION: block absent, exactly as for every non-Prime command today.
func primeDescription(d runtime.Descriptor) string {
	if d.Capability.Tier == runtime.CapabilityPrime {
		return runtime.PrimeSentence
	}
	return ""
}

// primeMetadataKey is the Metadata map key the help and schema TOC renderers
// read to decide whether a command's row carries the "[Prime]" marker.
const primeMetadataKey = "prime"

// primeMetadata returns a Metadata map flagging a Prime descriptor, or nil
// for an ordinary one - nil leaves urfave's Metadata absent, exactly as
// before this flag existed.
func primeMetadata(d runtime.Descriptor) map[string]any {
	if d.Capability.Tier != runtime.CapabilityPrime {
		return nil
	}
	return map[string]any{primeMetadataKey: true}
}

// projectMetadataKey is the Metadata map key holding the description of a
// command's own --project entry: --project is parsed once, as the global
// flag, so the help and flag-name completion of a command that takes it
// show it from here, with what it does for that command.
const projectMetadataKey = "project"

// localMetadataKey is the Metadata map key marking a Local command: the
// help and flag-name completion of a command that refuses --read-only
// leave it out.
const localMetadataKey = "local"

// leafMetadata is primeMetadata plus the command's --project description
// when it takes --project and the Local marker for a Local command; nil
// for an ordinary command that takes none.
func leafMetadata(d runtime.Descriptor) map[string]any {
	md := primeMetadata(d)
	if d.CommandType == runtime.Local {
		if md == nil {
			md = map[string]any{}
		}
		md[localMetadataKey] = true
	}
	for _, f := range d.Flags {
		if f.Name != runtime.FlagProject {
			continue
		}
		if md == nil {
			md = map[string]any{}
		}
		md[projectMetadataKey] = f.Desc
	}
	return md
}

// commandProject returns the description of cmd's own --project entry and
// whether cmd takes --project.
func commandProject(cmd *ucli.Command) (string, bool) {
	desc, ok := cmd.Metadata[projectMetadataKey].(string)
	return desc, ok
}

// commandIsLocal reports whether cmd is a Local command.
func commandIsLocal(cmd *ucli.Command) bool {
	v, _ := cmd.Metadata[localMetadataKey].(bool)
	return v
}

// commandIsPrime reports whether cmd carries the Prime metadata flag.
func commandIsPrime(cmd *ucli.Command) bool {
	v, _ := cmd.Metadata[primeMetadataKey].(bool)
	return v
}

// installShellComplete installs the universal completion callback on cmd for
// every adapted leaf. When the command also implements
// runtime.ShellCompleter, the config-local delegation layer is wired in;
// when it implements completion.Completer, its live source is consulted
// first for every live site.
func installShellComplete(rc runtime.Command, cmd *ucli.Command) {
	d := rc.Describe()
	var sc runtime.ShellCompleter
	if s, ok := rc.(runtime.ShellCompleter); ok {
		sc = s
	}
	var lc completion.Completer
	if l, ok := rc.(completion.Completer); ok {
		lc = l
	}
	cmd.ShellComplete = universalShellCompleteFunc(d, sc, lc)
}

// resolveDeps opens the credential store per the config's policy for dir.
// adaptEntry resolves dir from --config (flag > env > XDG), once, for the
// command and its remedies alike - the single place the directory is
// resolved, so --config always takes effect - and this is the single place
// the store is opened for it.
// config.LoadAndStore encapsulates the Load + NewLazyStore pairing. The store
// is LAZY: policy-name validation happens here (fail fast on every
// resolveDeps-backed command), but the auto probe / credstore-pin write /
// config-dir creation defer to the first credential call - an invocation
// that never touches credentials leaves no store-init side effects.
func resolveDeps(dir string) (config.CredStore, *config.CacheAnchor, error) {
	// The stamp the root hook already read anchors this load too: it
	// precedes every config load in the process, which is what the check
	// needs, and reading a second one here would cost the plugin's cache
	// hit an extra file read for nothing. A caller that reached here
	// without the root hook - a test, a direct construction - reads its
	// own, before the load.
	anchor := config.ProcessCacheAnchor()
	if !anchor.SameDirectory(dir) {
		anchor = config.NewCacheAnchor(dir)
	}
	cfg, store, err := config.LoadAndStore(dir)
	if err != nil {
		return nil, nil, err
	}
	return store, anchor.WithConfig(cfg), nil
}

// openDirectory is resolveDeps behind a package var, so a test can make
// the config load fail with the error it needs.
var openDirectory = resolveDeps

// remedyDirFor is the directory the invocation's remedies name, dir being
// the one the invocation opens: absolute, so a remedy run from another
// working directory reaches it, and as resolved when it cannot be made
// absolute.
func remedyDirFor(dir string) string {
	if abs := config.CanonicalDir(dir); abs != "" {
		return abs
	}
	return dir
}

// renderDepsError renders a failure to resolve or open the invocation's
// directory, before the command runs: a bare exit would fail non-zero with
// an empty stderr. Flags have already parsed, so the invocation's own
// --output value is trusted, and its remedies name its directory.
func renderDepsError(c *ucli.Command, io runtime.IO, err error) error {
	ce := clierr.New(clierr.CodeGeneric, err.Error())
	_ = runtime.RenderErrorStyled(c.Root().ErrWriter, c.String("output"), io.ExpandError(ce), errStyle(c, c.String("color")))
	return exit(ce)
}

// exit converts a structured error into a urfave exit with the right code.
func exit(ce *clierr.Error) error {
	return ucli.Exit("", ce.ExitCode())
}

// hintEntry pairs a matcher with the extra MESSAGE-body detail it selects
// (never a hint - none of these entries has a single concrete, runnable
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
	// after holds the words typed after name, in order. Callers pass them
	// explicitly: the command c may not have run, so it has no parsed
	// arguments to read.
	after []string
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
	{
		// There is no help command; help is the --help flag. The hint is the
		// command the caller meant, built from the words typed around
		// `help` (or its short spelling `h`).
		match: func(in notFoundInput) bool { return in.name == "help" || in.name == "h" },
		hint:  helpHint,
	},
}

// helpHint returns the runnable `--help` command for a mistyped `help` (or
// `h`) command. The words are the path down to the command that did not know
// `help`, then the words typed after it, in the order typed (`rancher help
// get pod` and `rancher get help pod` both give `rancher get pod --help`).
// Words are kept while each one names a visible command under the previous
// one, so the hint never names a command that does not exist. Every word
// kept is a command name, which needs no shell quoting. The words typed
// after `help` come parsed from commandNotFound and raw from the `--help`
// walk, so a flag the command accepts and its value can sit among them:
// they are skipped. Any other flag ends the words.
func helpHint(in notFoundInput) string {
	var typed []string
	lineage := in.c.Lineage()
	for _, c := range slices.Backward(lineage[:len(lineage)-1]) { // not the root
		typed = append(typed, c.Name)
	}
	words, kept, _ := tailWords(in.after, levelFlags(in.c))
	typed = append(typed, words[:kept]...)

	var hint strings.Builder
	hint.WriteString("rancher")
	node := in.c.Root()
	for _, word := range typed {
		next := visibleSubcommand(node, word)
		if next == nil {
			break
		}
		node = next
		hint.WriteString(" " + word)
	}
	hint.WriteString(" --help")
	return hint.String()
}

// notFoundGuidance returns commandNotFound's message detail (possibly "")
// and hint for name.
func notFoundGuidance(c *ucli.Command, name string, after []string) (detail, hint string) {
	return resolveGuidance(notFoundHints, notFoundInput{c: c, name: name, after: after})
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

// allNamespacesEntry is the kubectl-reflex -A/--all-namespaces entry -
// addressable by name so tests assert on THIS entry, not a table position.
//
// c.Path() returns the root-to-leaf segment slice (urfave v3 Path()),
// so a `get pod` leaf reports ["rancher","get","pod"]; the `get` group
// itself reports ["rancher","get"]. The positional p[1]=="get" check
// matches only commands directly under the `get` group - a "get" segment
// anywhere deeper in the path (e.g. a hypothetical `auth get-token`) does
// not trigger the detail.
var allNamespacesEntry = hintEntry[usageErrorInput]{
	// kubectl reflex -A/--all-namespaces: the flag exists nowhere.
	// Under the get group the wording explains that lists already
	// span all namespaces; elsewhere the base message alone covers it.
	// One entry, wording chosen here - no order-dependent
	// specialize-then-generalize pair.
	match: func(in usageErrorInput) bool { return isAllNamespacesFlag(in.err) },
	detail: func(in usageErrorInput) string {
		if p := in.c.Path(); len(p) >= 2 && p[1] == "get" {
			return noAllNamespacesFlag + " - list operations already span all namespaces; use --namespace where the kind accepts one"
		}
		return noAllNamespacesFlag
	},
}

// onceFlagRE reads the flag name from urfave's error for a second instance
// of a flag declared runtime.ArgSpec.Once ("invalid value ... for flag
// -name: can't duplicate this flag"; urfave prints one leading dash).
var onceFlagRE = regexp.MustCompile(`for flag -+([^:\s]+): can't duplicate this flag$`)

// onceFlagEntry names the remedy for a flag declared Once given twice: one
// instance carrying every entry.
var onceFlagEntry = hintEntry[usageErrorInput]{
	match: func(in usageErrorInput) bool { return in.err != nil && onceFlagRE.MatchString(in.err.Error()) },
	detail: func(in usageErrorInput) string {
		name := onceFlagRE.FindStringSubmatch(in.err.Error())[1]
		return fmt.Sprintf("--%s may be given once; give all its entries in one comma-separated value", name)
	},
}

// usageErrorHints is the registration table for targeted flag-parse
// guidance: the first matching entry's detail is appended to the message;
// callers fall back to just the base message plus the generic schema hint
// when nothing matches. Register new targeted entries here, not in the seam.
var usageErrorHints = []hintEntry[usageErrorInput]{allNamespacesEntry, onceFlagEntry}

// usageErrorGuidance returns onUsageError's message detail (possibly "")
// and hint for err.
func usageErrorGuidance(c *ucli.Command, err error) (detail, hint string) {
	return resolveGuidance(usageErrorHints, usageErrorInput{c: c, err: err})
}

// commandNotFound is urfave/cli's seam for an unmatched command/subcommand
// name (registered as root's and every group's CommandNotFound). Without it,
// urfave's own DefaultShowCommandHelp hard-codes a plain-text "No help topic
// for '<name>'" on stderr and exits 3 - indistinguishable from the AUTH exit
// code documented for exit 3, and with no structured error for a script to
// parse. CommandNotFoundFunc has no error return, so rendering and the
// process exit both happen here directly, mirroring how urfave's own
// ShowRootCommandHelpAndExit calls OsExiter. No flag has parsed successfully
// yet at this point, so the rendering format comes from a raw-argv scan
// (scanOutputFormat), not a parsed --output value. A bare "-" typed where
// a command name goes (name is "-") stops urfave at c, and the words
// after it, which can hold the command name, are never read: that line
// is refused the way the command pipeline refuses one, naming those
// words. Any other name is an unknown command first, whatever follows it.
func commandNotFound(_ context.Context, c *ucli.Command, name string) {
	if words, ok := droppedWords(rawArgv, c); ok && name == "-" {
		ce := runtime.DroppedWordsError(words)
		renderRawArgvError(c, ce)
		ucli.OsExiter(ce.ExitCode())
		return
	}
	reportUnknownCommand(c, name, c.Args().Tail())
}

// reportUnknownCommand renders the unknown-command error for name under c
// and exits. after is the words typed after name (see notFoundInput).
func reportUnknownCommand(c *ucli.Command, name string, after []string) {
	msg := fmt.Sprintf("unknown command %q", name)
	detail, hint := notFoundGuidance(c, name, after)
	if detail != "" {
		msg += "; " + detail
	}
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	renderRawArgvError(c, ce)
	ucli.OsExiter(ce.ExitCode())
}

// renderRawArgvError writes ce to the root's error writer in the format and
// color the raw command line selects. It serves errors raised where no
// flag has parsed successfully yet (an unknown command, a flag parse
// failure) or that no command rendered (an error urfave returns itself).
func renderRawArgvError(c *ucli.Command, ce *clierr.Error) {
	_ = runtime.RenderErrorStyled(c.Root().ErrWriter, scanOutputFormat(rawArgv), ce, errStyle(c, scanColorFlag(rawArgv)))
}

// ReportRunError handles an error Run returned to the process entry point
// and returns the process exit code. It renders an error only when nothing
// else has:
//   - An error the command pipeline rendered comes back from Run with an
//     empty message; it keeps its exit code.
//   - urfave writes an error's text itself in two ways. "Incorrect Usage"
//     is written only by a command with no OnUsageError handler, and every
//     command has one (the command tree test asserts it). An error passed
//     to its exit handler is written and then followed by an immediate
//     process exit, so it never returns here.
//
// A non-empty message that does return was therefore written by nobody. It
// is rendered as a GENERIC error in the requested output format, and its
// exit code is GENERIC's: a code urfave chose (its help error uses 3, the
// AUTH code) must not reach the caller. An error with no message that is
// not an exit code exits 1.
func ReportRunError(root *ucli.Command, err error) int {
	if msg := err.Error(); msg != "" {
		ce := clierr.New(clierr.CodeGeneric, msg)
		renderRawArgvError(root, ce)
		return ce.ExitCode()
	}
	if ec, ok := errors.AsType[ucli.ExitCoder](err); ok {
		return ec.ExitCode()
	}
	return clierr.ExitCode(clierr.CodeGeneric)
}

// onUsageError is urfave/cli's seam for a flag-parse failure (unknown flag,
// bad value, missing value). Without it, urfave prints a plain-text
// "Incorrect Usage: ..." line and falls through to help text - no structured
// error for a script to parse, mirroring the gap commandNotFound closes for
// unmatched command names. Wired on the root command and on every leaf and
// group command via the adapters, since urfave checks OnUsageError on the
// command node whose own flags failed to parse, not the root's. This
// command's own flags failed to parse, so - like commandNotFound - the
// rendering format comes from a raw-argv scan, not a parsed --output value.
func onUsageError(_ context.Context, c *ucli.Command, err error, _ bool) error {
	msg := err.Error() + "; pass --help for usage"
	detail, hint := usageErrorGuidance(c, err)
	if detail != "" {
		msg += "; " + detail
	}
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	renderRawArgvError(c, ce)
	return exit(ce)
}
