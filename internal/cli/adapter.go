package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"golang.org/x/term"
)

// GlobalFlags returns the confirmation/output flags shared by all commands.
// These are placed on the root command; subcommands inherit them (e.g.
// `rancher ping -o json`, `rancher clusters scale ... --apply`). (--config
// and --debug are on root in root.go.)
//
// These TTY usage strings are the operator-facing counterpart of the schema
// descriptions in runtime.GlobalFlags() (registry.go) - two deliberately
// separate strings (terse here, fuller there), kept in sync by hand.
func GlobalFlags() []*Flag {
	return []*Flag{
		{Name: "dry-run", Kind: boolFlag, Usage: "preview the changes without applying them"},
		{Name: "apply", Kind: boolFlag, Usage: "apply the change"},
		{Name: "yes", Kind: boolFlag, Usage: "confirm a destructive change"},
		{Name: "allow-deprecated", Kind: boolFlag, Usage: "allow writing to a deprecated resource"},
		{Name: "no-input", Kind: boolFlag, Usage: "never prompt; fail with an error when the input is ambiguous"},
		{Name: "output", Kind: stringFlag, Aliases: []string{"o"}, Usage: "table|wide|json|yaml|name|template=<go-template>"},
		{Name: "color", Kind: stringFlag, Usage: "auto|always|never"},
		{Name: "project", Kind: stringFlag, Usage: "the project, by id or display name, on the cluster given by --cluster (only commands that take --project accept it)"},
		{Name: "read-only", Kind: boolFlag, Usage: "refuse commands that make changes in this run; once set it cannot be turned off; local commands do not accept it"},
	}
}

// leafName returns the last space-separated token of a command name
// ("auth status" -> "status") for use as the leaf/subcommand name.
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

// flagsFor converts a Descriptor's named Flags into command flags
// (string/bool/string slice). ArgSpec.Desc feeds the flag's TTY usage text; a
// backtick-quoted word in it sets the value placeholder, e.g. a Desc of
// "patch `file`" renders "--patch-file file". Required-ness and Enum membership
// are intentionally NOT set on the flag: they are enforced by
// runtime.validateInput so violations surface as structured clierr envelopes.
func flagsFor(d runtime.Descriptor) []*Flag {
	var out []*Flag
	for _, f := range d.Flags {
		if f.Name == runtime.FlagProject {
			continue // the global --project flag; listing it declares that the command takes it
		}
		kind := stringFlag
		switch f.Type {
		case "bool":
			kind = boolFlag
		case "stringSlice":
			kind = sliceFlag
		}
		out = append(out, &Flag{Name: f.Name, Aliases: f.Aliases, Usage: f.Desc, Kind: kind, Once: f.Once})
	}
	return out
}

// argsUsage renders a Descriptor's positional Args as the ArgsUsage help
// string: "<name>" for a Required arg, "[name]" otherwise, space-joined
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
func buildIO(c *Command, in *Input) runtime.IO {
	r := c.Root()
	reader := r.Reader
	if reader == nil {
		reader = os.Stdin
	}
	isTTY := false
	if fp, ok := reader.(*os.File); ok {
		isTTY = term.IsTerminal(int(fp.Fd()))
	}
	readOnly, readOnlySources := config.ReadOnlyOverride()
	return runtime.IO{
		Out:             r.Writer,
		Err:             r.ErrWriter,
		In:              reader,
		Format:          in.String("output"),
		Color:           in.String("color"),
		DryRun:          in.Bool("dry-run"),
		Apply:           in.Bool("apply"),
		Yes:             in.Bool("yes"),
		AllowDeprecated: in.Bool("allow-deprecated"),
		NoInput:         in.Bool("no-input"),
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
// stale. It also keeps what recording a usage event needs (see
// noteRecording); a config that fails to load turns recording off.
func resolveReadOnly(in *Input) (bool, []string) {
	setStoredConfig(nil, "") // clear any prior stored config for this invocation
	config.SetProcessCacheAnchor(nil)
	resetRecordingState()
	var sources []string
	if dir, err := config.ResolveDir(in.String("config")); err == nil {
		// The binding stamp is read BEFORE the config load, and the pair
		// is what every cache reader without an anchor of its own - the
		// legacy shim, live completion - judges a record against. One
		// read per invocation: a reader takes a second only when it is
		// about to hand a cached credential back.
		anchor := config.NewCacheAnchor(dir)
		setting := telemetry.ConfigUnreadable
		if cfg, err := config.Load(config.FilePath(dir)); err == nil {
			setting = telemetry.ConfigSettingOf(cfg.Telemetry)
			setStoredConfig(cfg, dir) // single load shared with completion
			config.SetProcessCacheAnchor(anchor.WithConfig(cfg))
			completionDebugf("config loaded: dir=%s currentServer=%q servers=%d", dir, cfg.CurrentServer, len(cfg.Servers))
			if cfg.ReadOnly {
				sources = append(sources, "config setting readOnly")
			}
		} else {
			completionDebugf("config load failed: %v", err)
		}
		// A completion request records nothing: it decides nothing and
		// checks no directory. The check runs here, before any command can
		// create the directory.
		if !completionActive() {
			noteRecording(dir, setting)
		}
	} else {
		completionDebugf("config dir resolve failed: %v", err)
	}
	if os.Getenv(config.EnvReadOnly) != "" {
		sources = append(sources, config.EnvReadOnly)
	}
	if in.Bool("read-only") {
		sources = append(sources, "--read-only")
	}
	return len(sources) > 0, sources
}

// Adapt wraps a fixed runtime.Command that needs no per-run config dir/store
// (e.g. `schema`, `completion`). The binding is derived from the registry
// uniformly, so a mutating command wired this way still runs under its gate.
func Adapt(reg *runtime.Registry, rc runtime.Command) *Command {
	d := rc.Describe()
	b := reg.Binding(d.Name)
	cmd := leafCommand(d)
	cmd.Before = func(ctx context.Context, c *Command, in *Input) error {
		// Flags have already parsed successfully at this point. The
		// invocation's own --output value is trusted directly: no scan of the
		// captured command line is needed here, unlike the unknown-command
		// and flag error reports.
		if ce := checkFlagValues(d.Flags, rawArgv, completionActive(), d.SecretInput); ce != nil {
			runtime.WriteError(c.Root().ErrWriter, in.String("output"), ce, errStyle(c, in.String("color")), "")
			return exit(ce)
		}
		return nil
	}
	cmd.Action = func(ctx context.Context, c *Command, in *Input) error {
		streams := buildIO(c, in)
		// A command wired through Adapt has no store dependency
		// (schema, completion), so the only notices are the warnings
		// of the experiment variable: resolving a dir/store would
		// cost a config.yaml read+parse on every invocation purely to
		// fill a seam nothing else drains. A future local command that
		// grows a store dependency registers through adaptEntry,
		// which wires the store and its notices.
		addExperimentNotices(&streams, d.Name)
		status, ce := runtime.Run(ctx, rc, in, streams, b)
		if ce != nil {
			return exit(ce)
		}
		if status != 0 {
			return statusExit(status)
		}
		return nil
	}
	installShellComplete(rc, cmd)
	return cmd
}

// leafCommand returns the command of d with no Before, Action or Complete:
// its names, help text and flags, the descriptor and the Prime marker.
func leafCommand(d runtime.Descriptor) *Command {
	cmd := &Command{
		Name:        leafName(d.Name),
		Aliases:     d.Aliases,
		Usage:       d.Usage,
		Description: primeDescription(d),
		ArgsUsage:   argsUsage(d.Args),
		Flags:       flagsFor(d),
		descriptor:  &d,
		prime:       d.Capability.Tier == runtime.CapabilityPrime,
	}
	for _, f := range d.Flags {
		if f.Name == runtime.FlagProject {
			desc := f.Desc
			cmd.project = &desc
		}
	}
	return cmd
}

// primeDescription is the normal `--help` marker seam: the help template
// renders a command's Description in a DESCRIPTION: block (Usage feeds every
// other consumer - the TOC, the manifest, the command card's usage line - and
// must never carry the marker). "" leaves the DESCRIPTION: block absent, as
// for every non-Prime command.
func primeDescription(d runtime.Descriptor) string {
	if d.Capability.Tier == runtime.CapabilityPrime {
		return runtime.PrimeSentence
	}
	return ""
}

// commandDescriptor returns the Descriptor of the leaf cmd and whether cmd
// is a leaf built from one.
func commandDescriptor(cmd *Command) (runtime.Descriptor, bool) {
	if cmd.descriptor == nil {
		return runtime.Descriptor{}, false
	}
	return *cmd.descriptor, true
}

// commandProject returns the description of cmd's own --project entry and
// whether cmd takes --project.
func commandProject(cmd *Command) (string, bool) {
	if cmd.project == nil {
		return "", false
	}
	return *cmd.project, true
}

// commandIsPrime reports whether cmd is a Prime command, or a group of Prime
// commands only.
func commandIsPrime(cmd *Command) bool { return cmd.prime }

// installShellComplete installs the universal completion callback on cmd for
// every adapted leaf. When the command also implements
// runtime.ShellCompleter, the config-local delegation layer is wired in;
// when it implements completion.Completer, its live source is consulted
// first for every live site.
func installShellComplete(rc runtime.Command, cmd *Command) {
	d := rc.Describe()
	var sc runtime.ShellCompleter
	if s, ok := rc.(runtime.ShellCompleter); ok {
		sc = s
	}
	var lc completion.Completer
	if l, ok := rc.(completion.Completer); ok {
		lc = l
	}
	cmd.Complete = universalShellCompleteFunc(d, sc, lc)
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
func resolveDeps(dir string, takesSecrets bool) (config.CredStore, *config.CacheAnchor, error) {
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
	cfg, store, err := config.LoadAndStore(dir, takesSecrets)
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
// --output value is trusted, and its remedies name its directory through the
// remedyDir argument of WriteError.
func renderDepsError(c *Command, io runtime.IO, err error) error {
	ce := clierr.From(err)
	runtime.WriteError(c.Root().ErrWriter, io.Format, ce, errStyle(c, io.Color), io.RemedyDir)
	return exit(ce)
}

// exit ends a run with ce's exit code. The returned error has no message,
// because the caller has rendered ce already, and it names ce so that run
// can report which error ended the run.
func exit(ce *clierr.Error) error {
	return &exitError{ce: ce}
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

// genericSchemaHint is the hint every unknown-command and flag-error report
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

// notFoundInput bundles the arguments of reportUnknownCommand.
type notFoundInput struct {
	c    *Command
	name string
	// after holds the arguments typed after name, in order.
	after []string
}

// notFoundHints is the registration table for targeted unknown-command
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
		detail: constText[notFoundInput]("management clusters cannot be deleted directly; to delete a provisioning cluster (<name> is its name), run this and add --apply --yes after reading the preview: " + clierr.Rancher + " delete provisioning-cluster <name> --dry-run"),
	},
	{
		// For `delete` of a row whose delete is refused with a reason of its
		// own, the message gives the row's NoDeleteReason; the typed name may
		// be the kind name or a plural alias.
		match: func(in notFoundInput) bool {
			k, ok := refusedDeleteRow(in)
			return ok && k.NoDeleteReason != ""
		},
		detail: func(in notFoundInput) string {
			k, _ := refusedDeleteRow(in)
			return k.Name + " cannot be deleted - " + k.NoDeleteReason
		},
	},
	{
		// There is no help command; help is the --help flag. The hint is the
		// command the caller meant, built from the words typed around
		// `help` (or its short spelling `h`).
		match: func(in notFoundInput) bool { return in.name == "help" || in.name == "h" },
		hint:  helpHint,
	},
}

// refusedDeleteRow returns the clusterrepo, app, operation, etcdsnapshot,
// authconfig, role, rolebinding, clusterrole, clusterrolebinding,
// managementnode or uiplugin row that the name typed after `delete` resolves to.
func refusedDeleteRow(in notFoundInput) (kinds.Kind, bool) {
	if in.c.Name != "delete" {
		return kinds.Kind{}, false
	}
	k, ok := kinds.Lookup(in.name)
	if !ok || !slices.Contains([]string{"clusterrepo", "app", "operation", "etcdsnapshot", "authconfig", "role", "rolebinding", "clusterrole", "clusterrolebinding", "managementnode", "uiplugin"}, k.Key) {
		return kinds.Kind{}, false
	}
	return k, true
}

// helpHint returns the runnable `--help` command for a mistyped `help` (or
// `h`) command. The arguments are the path down to the command that did not
// know `help`, then the arguments typed after it, in the order typed
// (`rancher help get pod` and `rancher get help pod` both give `rancher get
// pod --help`). Arguments are kept while each one names a visible command
// under the previous one. The hint never names a command that does not exist.
// Every argument kept is a command name, which needs no shell quoting. The
// arguments typed after `help` come as typed, so a flag the command accepts
// and its value can sit among them: they are skipped. Any other flag ends the
// arguments kept.
func helpHint(in notFoundInput) string {
	var typed []string
	lineage := in.c.Lineage()
	for _, c := range slices.Backward(lineage[:len(lineage)-1]) { // not the root
		typed = append(typed, c.Name)
	}
	words, kept, _ := tailWords(in.after, levelFlags(in.c))
	typed = append(typed, words[:kept]...)

	var path []string
	node := in.c.Root()
	for _, word := range typed {
		next := visibleSubcommand(node, word)
		if next == nil {
			break
		}
		node = next
		path = append(path, word)
	}
	return clierr.DirectoryFree(strings.Join(append(path, "--help"), " "))
}

// notFoundGuidance returns the unknown-command message detail (possibly "")
// and hint for name.
func notFoundGuidance(c *Command, name string, after []string) (detail, hint string) {
	return resolveGuidance(notFoundHints, notFoundInput{c: c, name: name, after: after})
}

// usageErrorInput bundles the arguments of the flag error guidance.
type usageErrorInput struct {
	c   *Command
	err usageError
}

const noAllNamespacesFlag = "there is no all-namespaces flag"

// isAllNamespacesFlag reports whether ue is the unknown-flag error for the
// kubectl -A/--all-namespaces reflex.
func isAllNamespacesFlag(ue usageError) bool {
	return ue.class == undefinedFlag && (ue.word == "A" || ue.word == "all-namespaces")
}

// allNamespacesEntry is the kubectl-reflex -A/--all-namespaces entry -
// addressable by name so tests assert on THIS entry, not a table position.
//
// c.Path() returns the root-to-leaf segment slice, so a `get pod` leaf
// reports ["rancher","get","pod"]; the `get` group itself reports
// ["rancher","get"]. The positional p[1]=="get" check matches only commands
// directly under the `get` group - a "get" segment anywhere deeper in the
// path (e.g. a hypothetical `auth get-token`) does not trigger the detail.
var allNamespacesEntry = hintEntry[usageErrorInput]{
	// kubectl reflex -A/--all-namespaces: only a few leaves declare it;
	// the group commands and the other leaves do not.
	// Under the get group the wording explains that a get command lists
	// every namespace when --namespace is omitted, except for a kind with a
	// default namespace; elsewhere the base message alone covers it.
	// One entry, wording chosen here - no order-dependent
	// specialize-then-generalize pair.
	match: func(in usageErrorInput) bool { return isAllNamespacesFlag(in.err) },
	detail: func(in usageErrorInput) string {
		if p := in.c.Path(); len(p) >= 2 && p[1] == "get" {
			return noAllNamespacesFlag + " - a get command lists every namespace when you omit --namespace, except for a kind with a default namespace (fleet-default); use --namespace for a kind that takes one"
		}
		return noAllNamespacesFlag
	},
}

// onceFlagEntry names the remedy for a flag declared Once given twice: one
// instance carrying every entry.
var onceFlagEntry = hintEntry[usageErrorInput]{
	match: func(in usageErrorInput) bool { return in.err.class == repeatedFlag },
	detail: func(in usageErrorInput) string {
		return fmt.Sprintf("--%s may be given once; give all its entries in one comma-separated value", in.err.first)
	},
}

// usageErrorHints is the registration table for targeted flag-parse
// guidance: the first matching entry's detail is appended to the message;
// callers fall back to just the base message plus the generic schema hint
// when nothing matches. Register new targeted entries here, not in the seam.
var usageErrorHints = []hintEntry[usageErrorInput]{allNamespacesEntry, onceFlagEntry}

// usageErrorGuidance returns the flag error message detail (possibly "")
// and hint for err.
func usageErrorGuidance(c *Command, err usageError) (detail, hint string) {
	return resolveGuidance(usageErrorHints, usageErrorInput{c: c, err: err})
}

// reportUnknownCommand renders the unknown-command error for name under c
// and returns it. after is the arguments typed after name (see
// notFoundInput). No flag has parsed successfully yet at this point. The
// rendering format comes from a scan of the captured command line
// (scanOutputFormat), not a parsed --output value.
func reportUnknownCommand(c *Command, name string, after []string) *clierr.Error {
	msg := fmt.Sprintf("unknown command %q", name)
	detail, hint := notFoundGuidance(c, name, after)
	if detail != "" {
		msg += "; " + detail
	}
	ce := clierr.New(clierr.CodeValidation, msg).WithHint(hint)
	renderRawArgvError(c, ce)
	return ce
}

// renderRawArgvError writes ce to the root's error writer in the format and
// color the captured command line selects. It serves errors raised where no
// flag has parsed successfully yet (an unknown command, a flag error) or
// that no command rendered.
func renderRawArgvError(c *Command, ce *clierr.Error) {
	runtime.WriteError(c.Root().ErrWriter, scanOutputFormat(rawArgv), ce, errStyle(c, scanColorFlag(rawArgv)), "")
}

// text is the message of the flag error: the undefined flag, the flag that
// is missing its value, or the flag with a value it cannot take.
func (e usageError) text() string {
	switch e.class {
	case undefinedFlag:
		return "flag provided but not defined: -" + e.word
	case missingFlagValue:
		return "flag needs an argument: " + e.word
	case repeatedFlag:
		return fmt.Sprintf("invalid value %q for flag -%s: can't duplicate this flag", e.value, e.flag)
	default:
		return fmt.Sprintf("invalid value %q for flag -%s: parse error", e.value, e.flag)
	}
}

// usageErrorText is the text of the flag error ue of command c. A command
// that takes a secret input fails closed: any text built from the command
// line may hold a typed argument that is the secret. Only these forms get
// through:
//   - a flag that is missing its value, which is given only for a flag the
//     command defines, and holds no typed argument;
//   - an undefined flag, which becomes the position of the argument (see
//     unknownFlagText);
//   - an invalid value, which becomes the flag and, for the two reasons that
//     quote nothing, the reason.
//
// A command with no descriptor (the root or a group) reports a flag typed
// before the command name, whose command is not known yet, or a flag of a
// command that has no descriptor: an undefined flag gets the position text
// unless the line's command is a leaf that takes no secret input (see
// unknownFlagTextAt), and an invalid value gets the text that names the flag
// and not the value. A missing value passes through. A command with a
// descriptor that takes no secret input names the typed argument.
func usageErrorText(c *Command, ue usageError) string {
	msg := ue.text()
	d, described := commandDescriptor(c)
	if described && !d.SecretInput {
		return msg
	}
	if !described {
		switch ue.class {
		case undefinedFlag:
			return unknownFlagTextAt(c.Root(), ue.word, rawArgv, msg)
		case missingFlagValue:
			return msg
		}
		return invalidValueText(ue)
	}
	switch ue.class {
	case missingFlagValue:
		return msg
	case undefinedFlag:
		return unknownFlagText(ue.word, rawArgv)
	}
	return invalidValueText(ue)
}

// invalidValueText is the text of a flag with a value it cannot take, without
// the value: the flag, and the reason, which is one that quotes nothing.
func invalidValueText(ue usageError) string {
	out := "invalid value for flag -" + ue.flag
	if ue.class == repeatedFlag {
		return out + ": can't duplicate this flag"
	}
	return out + ": the value must be true or false"
}

// unknownFlagTextAt is the text for the undefined flag name that the root
// or a group reports, as the command line argv is typed. The flag's command
// is the leaf the rest of the command line names (see leafOfLine). It is msg,
// the text that names the flag as typed, when that leaf has a descriptor that
// takes no secret input: the flag as typed helps a user find a typo there. In
// every other case, when the leaf takes a secret input, is not found, or has
// no descriptor, it is the position text, which names no flag.
func unknownFlagTextAt(root *Command, name string, argv []string, msg string) string {
	if leaf := leafOfLine(root, argv); leaf != nil {
		if d, described := commandDescriptor(leaf); described && !d.SecretInput {
			return msg
		}
	}
	return unknownFlagText(name, argv)
}

// leafOfLine returns the leaf command the line argv names on root's tree, or
// nil when the line ends at the root or a group. An undefined flag stops the
// walk of parseLine at the level that reports it, so the word is taken out
// and the line read again, until the walk ends for another reason. An
// undefined flag is read as one that takes no value, since the number of
// values it takes cannot be known.
func leafOfLine(root *Command, argv []string) *Command {
	words := slices.Clone(argv)
	for range len(argv) + 1 {
		p := parseLine(root, words)
		end := p.end
		lv := p.levels[end.level]
		if end.kind != lineError || end.class != undefinedFlag || len(lv.words) == 0 {
			if lv.leaf {
				return lv.cmd
			}
			return nil
		}
		words = slices.Delete(words, lv.words[len(lv.words)-1].index, lv.words[len(lv.words)-1].index+1)
	}
	return nil
}

// unknownFlagText is the refusal of the undefined flag name for a command
// that takes a secret input: it gives the position of the word in the
// arguments typed after rancher, the command path and the global flags
// included, counted from 1, and not the word, since a word typed with a
// leading dash may be the secret. The word is the first in argv that starts
// with "-" and whose text up to any "=" is name with its dashes; a word
// with no such match (padded with spaces, three dashes) gives no position,
// and the text names neither the word nor a position.
func unknownFlagText(name string, argv []string) string {
	for i, w := range argv {
		if !strings.HasPrefix(w, "-") {
			continue
		}
		if flag, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "="); flag == name {
			return fmt.Sprintf("unknown flag at argument %d after rancher", i+1)
		}
	}
	return "unknown flag"
}
