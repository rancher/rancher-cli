package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

// IO carries the resolved global flags and output streams into the pipeline.
type IO struct {
	Out             io.Writer
	Err             io.Writer
	In              io.Reader
	Format          string
	DryRun          bool
	Apply           bool
	Yes             bool
	AllowDeprecated bool
	NoInput         bool
	IsTTY           bool

	// Color is the raw --color flag value ("", "auto", "always", or
	// "never"); "" and "auto" are equivalent. Validated by Run before use.
	Color string
	// OutTTY and ErrTTY report whether stdout and stderr are each a real
	// terminal - decided independently, since one stream can be redirected
	// while the other is not (`rancher get pod | less` still colors
	// stderr). Distinct from IsTTY above, which is stdin-only and feeds the
	// prompter, not styling.
	OutTTY bool
	ErrTTY bool

	// ReadOnly is the OR of three sources - config `readOnly: true`, a
	// non-empty RANCHER_CLI_READONLY, and --read-only - resolved once per
	// invocation by the CLI adapter. Strictly one-way: no flag disables a
	// config/env-sourced read-only. Run refuses every Mutating command
	// while it is set, before Plan (which sends real write-verb requests
	// unconditionally, including for --dry-run).
	ReadOnly bool
	// ReadOnlySources names the active source(s) ("config readOnly",
	// "RANCHER_CLI_READONLY", "--read-only") for the refusal hint.
	ReadOnlySources []string

	// ServerIdentity, when set, resolves the server name for Effect.Server
	// stamping after a successful Plan. AdaptEngine wires the factory's
	// Identity; other adapters leave it nil.
	ServerIdentity func() (string, error)

	// Capability is the invocation-scoped capability checker, wired by
	// adaptEntry's non-bare branch. Run invokes it only when the resolved
	// command's Descriptor carries a CapabilityRequirement - ordinary
	// commands pay nothing, and adapters that never serve requirement-
	// bearing commands may leave it nil. A requirement-bearing command
	// reaching Run with a nil checker is mis-wiring and fails loudly.
	Capability CapabilityChecker

	// Notices drains operator-facing advisories recorded outside any
	// Result: those of this invocation's credential-store dependency (a
	// loose-permission finding, a keychain-degrade warning), wired from
	// the resolved store wherever one exists, and those of the process
	// itself (an ignored item of an environment variable), wired by the CLI
	// layer; nil when a command has neither. renderResult,
	// renderEffect, renderErr, and finishOnceOnlyResult each drain it
	// exactly once, feeding the one render they own.
	Notices NoticeSource

	// RemedyDir is the config directory every remedy command this
	// invocation prints names with --config, absolute where it can be made
	// so; "" for a run whose remedies name none. Set by the CLI layer
	// unless a command pasted without --config in this environment would
	// open the same directory: for a run against a directory other than the
	// platform default, for a run against the default when
	// RANCHER_CLI_CONFIG_DIR or XDG_CONFIG_HOME points elsewhere, for a
	// directory that cannot be compared, and always for the commands whose
	// directory can come from somewhere the operator's shell does not carry
	// (Descriptor.RemedyConfigDir). The renderers of this invocation -
	// errors, warnings, notices, result text, the summary line, the
	// dry-run preview and the json and yaml output (the data of a result and
	// the envelope of a preview included) - write it after the word rancher
	// in every remedy command (clierr.RemedyMarker); the table, wide, name and
	// template output of result data and the text of a command whose
	// descriptor sets PlainText print the plain command; it is never a
	// process-wide value.
	RemedyDir string

	// Projects resolves a typed --project for a command that takes it,
	// wired by adaptEntry's non-bare branch.
	Projects ProjectResolver
}

// NoticeSource drains buffered operator-facing notices in the order they
// were recorded, destructively: a call returns only what was recorded since
// the previous drain. Implemented by a credential store so that resolving it
// lazily - possibly racing a command-body goroutine - never loses a notice
// recorded outside any Result, and by the CLI layer for advisories that
// belong to the process and not to a command. Draining must not itself force
// a lazy store to resolve: a command that never touches credentials must
// still see zero store-init side effects.
type NoticeSource interface {
	DrainNotices() []string
}

// drainNotices drains streams.Notices, or reports no notices when the
// invocation carries none.
func drainNotices(streams IO) []string {
	if streams.Notices == nil {
		return nil
	}
	return streams.Notices.DrainNotices()
}

// Run executes one command through the single middleware path. b is the
// resolver binding for the command's op ({} for config/local commands); the
// internal Binding drives the --allow-deprecated gate.
func Run(ctx context.Context, cmd Command, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	d := cmd.Describe()

	for _, g := range []string{"output", "color"} {
		if in.IsSet(g) && blank(in.String(g)) {
			return 0, renderErr(streams, EmptyValueError(g))
		}
	}

	if ce := validateFormat(streams.Format, d.SecretInput); ce != nil {
		return 0, renderErr(streams, ce)
	}

	if ce := validateColor(streams.Color, d.SecretInput); ce != nil {
		return 0, renderErr(streams, ce)
	}

	// The output-policy gate runs immediately after format-syntax
	// validation and before EVERYTHING else - read-only/dry-run gates,
	// input validation, capability resolution, Plan - so a rejected format
	// makes zero total requests, not merely zero mutation requests: a
	// once-only command's caller can never silently discard the value it
	// delivers by picking an unsupported -o.
	if ce := validateOutputPolicy(d, streams.Format); ce != nil {
		return 0, renderErr(streams, ce)
	}

	if d.CommandType == Mutating && streams.ReadOnly {
		return 0, renderErr(streams, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("read-only mode: %s is a mutating command; enabled by %s",
				d.Name, strings.Join(streams.ReadOnlySources, ", "))))
	}

	// A typed --read-only on a local command is refused: the flag applies to
	// commands that read or change server resources, and a local command
	// that ignored it would break the flag's one-way promise. IsSet, not
	// streams.ReadOnly, so a typed --read-only=false is refused too and the
	// environment and config sources stay usable on local commands. The
	// refusal carries no hint: dropping the flag from the line does not
	// always leave a valid command.
	if d.CommandType == Local && in.IsSet(FlagReadOnly) {
		return 0, renderErr(streams, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s is a local command and --read-only does not apply to it; run it without --read-only "+
				"and without any value typed with it "+
				"(RANCHER_CLI_READONLY and config readOnly apply to every run and still allow local commands)",
				d.Name)))
	}

	if streams.DryRun && d.CommandType != Mutating {
		kind := string(d.CommandType)
		switch d.CommandType {
		case ReadOnly:
			kind = "read-only"
		case Local:
			kind = "local"
		}
		return 0, renderErr(streams, clierr.New(clierr.CodeDryRunNotApplicable,
			fmt.Sprintf("%s is a %s command; re-run without --dry-run", d.Name, kind)))
	}

	if ce := checkProjectFlag(cmd, d, in); ce != nil {
		return 0, renderErr(streams, ce)
	}

	// With --project typed, a missing --cluster is the project stage's
	// refusal, which names the clusters the project is on.
	if ce := validateInput(d, in, in.IsSet(FlagProject)); ce != nil {
		return 0, renderErr(streams, ce)
	}

	// Capability detection is a mandatory stage for any command carrying a
	// requirement - commands never opt in or out - and runs after
	// server/credential resolution but before any resource access (reads)
	// or the server-side dry-run Plan (mutations), so a Prime-backed kind
	// that may not exist on a community server never gets to 404 first.
	//
	// The static descriptor value is validated BEFORE its IsSet use: IsSet
	// (Tier != "") cannot see a half-set value, so validating after it
	// fails open - only a fully valid zero value selects the resolver.
	if problem := ValidCapabilityShape(d.Capability, d.Name); problem != "" {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric, "internal error: "+problem))
	}
	req := d.Capability
	if !req.IsSet() {
		if r, ok := cmd.(CapabilityResolver); ok {
			var err error
			req, err = r.ResolveCapability(ctx, in)
			if err != nil {
				return 0, renderErr(streams, toClierr(ctx, err))
			}
			// Then the resolver's return - an arbitrary runtime value
			// startup validation never saw. A malformed value is an internal
			// error, stopped before the checker and Plan/Execute.
			if problem := ValidCapabilityShape(req, d.Name); problem != "" {
				return 0, renderErr(streams, clierr.New(clierr.CodeGeneric, "internal error: "+problem))
			}
		}
	}
	if req.IsSet() {
		if ce := checkCapability(ctx, d, req, streams); ce != nil {
			return 0, renderErr(streams, ce)
		}
	}

	if in.IsSet(FlagProject) {
		var ce *clierr.Error
		if ctx, streams, ce = withProject(ctx, cmd, in, streams); ce != nil {
			return 0, renderErr(streams, ce)
		}
	}

	switch c := cmd.(type) {
	case MutatingCommand:
		return runMutating(ctx, c, in, streams, b)
	case ReadCommand:
		return runRead(ctx, c, in, streams, b)
	default:
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			"internal error: command implements neither ReadCommand nor MutatingCommand: "+d.Name))
	}
}

// runMutating drives the Plan → invariant checks → server stamping →
// confirmation gate → Execute path for a mutating command.
func runMutating(ctx context.Context, c MutatingCommand, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	eff, exec, err := c.Plan(ctx, stdinInput{Input: in, terminal: streams.IsTTY})
	if err != nil {
		return 0, renderErr(streams, toClierr(ctx, err))
	}
	if ce := checkPlan(eff, exec); ce != nil {
		return 0, renderErr(streams, ce)
	}
	if streams.ServerIdentity != nil {
		server, err := streams.ServerIdentity()
		if err != nil {
			return 0, renderErr(streams, toClierr(ctx, err))
		}
		eff.Server = server
	}
	if streams.DryRun {
		return 0, renderEffect(streams, eff)
	}
	if missing := missingFlags(streams, eff, b); len(missing) > 0 {
		return 0, renderErr(streams, clierr.New(clierr.CodeConfirmationRequired, confirmationMessage(streams, missing)))
	}
	res, err := exec(ctx)
	if err != nil {
		return 0, renderErr(streams, toClierr(ctx, err))
	}
	res = prependOnceOnlyNotice(res)
	res.Warnings = append(append([]string{}, eff.Warnings...), res.Warnings...)
	return finishResult(streams, c.Describe(), res)
}

// prependOnceOnlyNotice prepends res.OnceOnlyNotice, when set, to
// res.Warnings ahead of every other command-declared warning. Called before
// a mutating command's Plan-time Effect.Warnings are merged in (runMutating),
// so the final order stays Effect.Warnings, then the notice, then any other
// Warnings the command itself declared.
func prependOnceOnlyNotice(res Result) Result {
	if res.OnceOnlyNotice != "" {
		res.Warnings = append([]string{res.OnceOnlyNotice}, res.Warnings...)
	}
	return res
}

// checkPlan enforces the invariants of a Plan result, each an internal error
// when broken: a non-nil executor, a valid action, and destructive-delete. It
// never renders; the caller wraps the returned error.
func checkPlan(eff safety.Effect, exec ExecuteFunc) *clierr.Error {
	if exec == nil {
		return clierr.New(clierr.CodeGeneric, "internal error: the command returned nothing to run to apply the change")
	}
	if !eff.Action.Valid() {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: invalid action %q in the dry-run preview", eff.Action))
	}
	if eff.DisplayKind == "" || eff.DisplayName == "" {
		return clierr.New(clierr.CodeGeneric,
			"internal error: the dry-run preview has an empty DisplayKind or DisplayName")
	}
	if !eff.PreviewSource.Valid() {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: invalid preview source %q in the dry-run preview", eff.PreviewSource))
	}
	if eff.Action == safety.ActionDelete && !eff.Destructive {
		return clierr.New(clierr.CodeGeneric, "internal error: a delete in the dry-run preview must be destructive")
	}
	return nil
}

// runRead executes a read command.
func runRead(ctx context.Context, c ReadCommand, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	p := ui.New(streams.Err, streams.In, streams.NoInput, streams.IsTTY).WithNoticeRewrite(streams.ExpandText)
	res, err := c.Execute(ctx, textTableInput{Input: in, textTable: textRenderFormat(streams.Format)}, p)
	if err != nil {
		return 0, renderErr(streams, toClierr(ctx, err))
	}
	res = prependOnceOnlyNotice(res)
	return finishResult(streams, c.Describe(), res)
}

// validateFormat closes the -o/--output value set: the OutputFormats literals
// (plus "" for the default) and template=<go-template>, whose source must
// parse. Run calls it before dispatch so an unknown format is a structured
// VALIDATION error and a mutation can never execute and then fail at render.
// The template parse here makes Render's own parse effectively pre-checked;
// the duplicate parse is cheap and intentional (Render keeps it as defense in
// depth for non-pipeline callers). A flag typed with no value makes urfave
// take the next word as its value, a secret among them: for a command that
// takes a secret input (secret) no text holds the value or the parser's
// message, which quotes parts of the source.
func validateFormat(format string, secret bool) *clierr.Error {
	if format == "" {
		return nil
	}
	if slices.Contains(OutputFormats, format) {
		return nil
	}
	if src, ok := strings.CutPrefix(format, output.TemplatePrefix); ok {
		if src == "" {
			return clierr.New(clierr.CodeValidation,
				"--output template is empty; pass -o template='{{ .field }}'")
		}
		if _, err := template.New("out").Parse(src); err != nil {
			if secret {
				return clierr.New(clierr.CodeValidation,
					"--output template does not parse; must be valid Go text/template syntax")
			}
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--output template does not parse: %v; must be valid Go text/template syntax", err))
		}
		return nil
	}
	if secret {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--output value is not one of %s|template=<go-template>", strings.Join(OutputFormats, "|")))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--output=%q is not one of %s|template=<go-template>", format, strings.Join(OutputFormats, "|")))
}

// validateColor closes the --color value set: "" (the flag omitted, same as
// "auto") and the ColorModes literals. Run calls it before dispatch so an
// unrecognized value is a structured VALIDATION error rather than a silent
// fall-through to auto. For a command that takes a secret input (secret) the
// text leaves the value out.
func validateColor(color string, secret bool) *clierr.Error {
	if color == "" || slices.Contains(ColorModes, color) {
		return nil
	}
	if secret {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--color value is not one of %s", strings.Join(ColorModes, "|")))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--color=%q is not one of %s", color, strings.Join(ColorModes, "|")))
}

// validateOutputPolicy enforces a Descriptor's OutputPolicy: a command that
// restricts formats (typically to exclude "name", which would silently
// discard a once-only value) rejects an unsupported format here, before any
// capability resolution or network work - the zero OutputPolicy always
// passes. A template format of a command that takes a secret input is shown
// as "template", without its source.
func validateOutputPolicy(d Descriptor, format string) *clierr.Error {
	if d.OutputPolicy.Allows(format) {
		return nil
	}
	shown := format
	if shown == "" {
		shown = "table (the default)"
	}
	if d.SecretInput && strings.HasPrefix(format, output.TemplatePrefix) {
		shown = "template"
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s does not support --output=%s; allowed: %s",
			d.Name, shown, strings.Join(d.OutputPolicy.AllowedFormats, ", ")))
}

// validateInput checks the Descriptor's Args and Flags against the parsed
// Input: no more positional args than the Descriptor declares; required
// positional args must be present and non-empty; enum args/flags must hold
// a listed value; required flags must be non-empty.
// Returns a structured VALIDATION error (nil if valid). Commands wanting
// custom missing-input UX (e.g. a candidate list) mark the field
// Required:false and validate it themselves in Execute.
func validateInput(d Descriptor, in Input, deferCluster bool) *clierr.Error {
	args := in.Args()
	if len(args) > len(d.Args) {
		positions := make([]int, len(args)-len(d.Args))
		for i := range positions {
			positions[i] = len(d.Args) + i + 1
		}
		return clierr.New(clierr.CodeValidation, extraArgsMessage(d, args[len(d.Args):], positions)).WithHint(helpHint(d))
	}
	for i, a := range d.Args {
		if a.Required && (i >= len(args) || args[i] == "") {
			return missingArgError(a)
		}
		if len(a.Enum) > 0 && i < len(args) && args[i] != "" {
			ok := slices.Contains(a.Enum, args[i])
			if !ok {
				return enumRefusal(d, "argument <"+a.Name+">", args[i], a.Enum)
			}
		}
	}
	for _, f := range d.Flags {
		if ce := emptyValueError(f, in); ce != nil {
			return ce
		}
		if f.Type == "stringSlice" {
			entries := in.Strings(f.Name)
			if f.Required && len(entries) == 0 {
				return missingFlagError(f)
			}
			if len(f.Enum) > 0 {
				for _, v := range entries {
					if v == "" {
						continue
					}
					ok := slices.Contains(f.Enum, v)
					if !ok {
						return enumRefusal(d, "--"+f.Name, v, f.Enum)
					}
				}
			}
			continue
		}
		v := in.String(f.Name)
		if f.Required && v == "" && (!deferCluster || f.Name != FlagCluster) {
			return missingFlagError(f)
		}
		if len(f.Enum) > 0 && v != "" {
			ok := slices.Contains(f.Enum, v)
			if !ok {
				return enumRefusal(d, "--"+f.Name, v, f.Enum)
			}
		}
	}
	return nil
}

// enumRefusal is the refusal of value for the argument or flag named by
// what ("--type", "argument <kind>"), whose allowed values are enum. The
// value is shown quoted, as what="value", except for a command that takes a
// secret input: a word taken as the value of a flag may be a secret meant
// for another, so that text names what and the allowed values only.
func enumRefusal(d Descriptor, what, value string, enum []string) *clierr.Error {
	if d.SecretInput {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s value is not one of %s", what, OneOf(enum)))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s=%q is not one of %s", what, value, OneOf(enum)))
}

// BoolValueError is the refusal for positional arguments typed as the
// separate value of a bool flag (vals, indexes into args): the flag is on
// and the word is an argument, whatever was meant. A word is named as
// typed. line is the command with each value joined to its flag
// (`--dry-run='false'`), values shell-quoted. The hint is line when the
// arguments left fill every positional argument d declares, required or
// optional, and the command's help otherwise, so the hint never names a
// line refused for a bool value, a missing argument or an extra argument.
// An optional argument counts too: a flag can need it (get refuses --wait
// without a name). A command that takes a secret input names each word by
// its position among the arguments typed instead, and its hint is always
// its help: the typed line holds every flag value. vals holds at least one
// value; an empty list is an internal error.
func BoolValueError(d Descriptor, args []string, vals []BoolValue, line string) *clierr.Error {
	if len(vals) == 0 {
		return clierr.New(clierr.CodeGeneric, "internal error: "+d.Name+" was refused for a bool value but no bool value was found")
	}
	named := make([]string, len(vals))
	isValue := map[int]bool{}
	for i, v := range vals {
		word := clierr.ShellQuote(args[v.Index])
		if d.SecretInput {
			word = fmt.Sprintf("argument at position %d", v.Index+1)
		}
		named[i] = word + " after the bool flag " + v.Flag
		if i == 0 {
			named[i] = word + " was typed after the bool flag " + v.Flag
		}
		isValue[v.Index] = true
	}
	// Not firstWords: past the cap the count here ends with "after bool flags".
	list := strings.Join(named[:min(len(named), maxNamedWords)], ", ")
	if len(named) > maxNamedWords {
		list += fmt.Sprintf(" and %d more after bool flags", len(named)-maxNamedWords)
	}
	msg := fmt.Sprintf("%s: %s; a bool flag takes its value only as %s=<value>. To pass an argument such as true or false, put it before the flags or after --",
		d.Name, list, vals[0].Flag)
	var rest []string
	var positions []int
	for i, a := range args {
		if !isValue[i] {
			rest = append(rest, a)
			positions = append(positions, i+1)
		}
	}
	if len(rest) > len(d.Args) {
		return clierr.New(clierr.CodeValidation, msg+". "+extraArgsMessage(d, rest[len(d.Args):], positions[len(d.Args):])).WithHint(helpHint(d))
	}
	if len(rest) < len(d.Args) || slices.Contains(rest, "") || d.SecretInput {
		return clierr.New(clierr.CodeValidation, msg).WithHint(helpHint(d))
	}
	if line == "" {
		return clierr.New(clierr.CodeValidation, clierr.WithLeftOutNote(msg))
	}
	return clierr.New(clierr.CodeValidation, msg).WithHint(line)
}

// extraArgsMessage states what d takes and the positional arguments
// beyond it, extra, which are the arguments typed at the 1-based
// positions. A command that takes a secret input names each by its
// position; any other command names the word as typed.
func extraArgsMessage(d Descriptor, extra []string, positions []int) string {
	quoted := make([]string, len(extra))
	for i, a := range extra {
		if d.SecretInput {
			quoted[i] = strconv.Itoa(positions[i])
			continue
		}
		quoted[i] = clierr.ShellQuote(a)
	}
	noun := "argument"
	if len(extra) > 1 {
		noun = "arguments"
	}
	if d.SecretInput {
		noun += " at position"
		if len(extra) > 1 {
			noun += "s"
		}
	}
	return fmt.Sprintf("%s %s; unexpected %s %s", d.Name, declaredArgs(d.Args), noun, firstWords(quoted, " "))
}

// maxNamedWords is how many words a refusal names before it counts the
// rest, so a glob that expands to thousands of words stays one line.
const maxNamedWords = 5

// firstWords joins the first maxNamedWords of words with sep and counts
// the rest: "'a' 'b' 'c' 'd' 'e' and 2 more".
func firstWords(words []string, sep string) string {
	if len(words) <= maxNamedWords {
		return strings.Join(words, sep)
	}
	return fmt.Sprintf("%s and %d more", strings.Join(words[:maxNamedWords], sep), len(words)-maxNamedWords)
}

// helpHint is the hint that points at d's help.
func helpHint(d Descriptor) string {
	return clierr.DirectoryFree(d.Name + " --help")
}

// declaredArgs states how many positional arguments args allows, with
// their names as help shows them: "takes no arguments", "takes 1 argument
// (<name>)", "takes at most 2 arguments (<verb> [resource])".
func declaredArgs(args []ArgSpec) string {
	if len(args) == 0 {
		return "takes no arguments"
	}
	names := make([]string, len(args))
	optional := false
	for i, a := range args {
		if a.Required {
			names[i] = "<" + a.Name + ">"
		} else {
			names[i] = "[" + a.Name + "]"
			optional = true
		}
	}
	count := fmt.Sprintf("%d argument", len(args))
	if len(args) > 1 {
		count += "s"
	}
	if optional {
		count = "at most " + count
	}
	return fmt.Sprintf("takes %s (%s)", count, strings.Join(names, " "))
}

// OneOf renders a list of allowed values the way every refusal prints one.
func OneOf(values []string) string {
	return strings.Join(values, "|")
}

// missingArgError is the refusal for a required positional argument that
// was not passed: it lists the argument's allowed values when it declares
// them.
func missingArgError(a ArgSpec) *clierr.Error {
	if len(a.Enum) > 0 {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("argument <%s> is required; one of %s", a.Name, OneOf(a.Enum)))
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf("argument <%s> is required", a.Name))
}

// missingFlagError is the refusal for a required flag that was not passed:
// the flag's own MissingMessage and MissingHint when its descriptor set
// them, otherwise the generic form, which lists the flag's allowed values
// when it declares them.
func missingFlagError(f ArgSpec) *clierr.Error {
	if f.MissingMessage == "" {
		if len(f.Enum) > 0 {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s is required; one of %s", f.Name, OneOf(f.Enum)))
		}
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s is required", f.Name))
	}
	ce := clierr.New(clierr.CodeValidation, f.MissingMessage)
	if f.MissingHint != "" {
		ce = ce.WithHint(f.MissingHint)
	}
	return ce
}

// checkCapability runs the capability stage for a command whose resolved
// requirement (static or deferred) is set. A nil streams.Capability here is
// harness mis-wiring, not signal unavailability: only unknown SIGNALS may
// fall through (the unknown-never-blocks rule); a requirement-bearing
// command registered without a checker would otherwise silently skip a
// mandatory stage forever, so it fails loudly instead. CapabilityProceed and
// CapabilityUnknown both fall through to the target request unchanged;
// CapabilityDisabled reuses the existing VALIDATION taxonomy; only
// CapabilityPrimeRequired returns the structured PRIME_REQUIRED error.
// Identifiers, identical for static and resolver-supplied requirements:
// the PRIME_REQUIRED message names the command (d.Name - for generated
// verbs that carries the kind; for create resource it names the command, a
// reviewed acceptance until the capability Name identifier lands) and its
// details.feature carries req.Feature, the machine-readable identifier.
// The Disabled message names req.Feature directly and carries no details -
// the remedy is the feature flag, not an entitlement. Its wording forks on
// dec.EditionPrime: a known-Prime server gets the actionable "enable
// feature <name>"; every other edition gets a statement that holds
// regardless of tier, since a community or unknown edition might still turn
// out to require Prime once the feature is tiered server-side.
func checkCapability(ctx context.Context, d Descriptor, req CapabilityRequirement, streams IO) *clierr.Error {
	if streams.Capability == nil {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s declares a capability requirement but no capability checker is set up; "+
				"register the command through the standard server-command path", d.Name))
	}
	dec := streams.Capability.Check(ctx, req)
	switch dec.Outcome {
	case CapabilityDisabled:
		if dec.EditionPrime {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s is disabled; enable it on the connected server", req.Feature))
		}
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s is disabled on the connected server, which decides whether it is available - %s",
				req.Feature, clierr.LearnMorePrimeURL))
	case CapabilityPrimeRequired:
		return clierr.New(clierr.CodePrimeRequired,
			fmt.Sprintf("%s requires Rancher Prime; the connected server does not have Rancher Prime; %s",
				d.Name, clierr.LearnMorePrimeURL)).
			WithDetails(clierr.PrimeRequiredDetails{Feature: req.Feature, Server: dec.Server})
	case CapabilityUnsupported:
		return clierr.New(clierr.CodeCapabilityUnsupported,
			fmt.Sprintf("the connected server does not support %s; upgrade the server to a version that does", req.Unsupported)).
			WithDetails(clierr.CapabilityUnsupportedDetails{Feature: req.Feature, Server: dec.Server})
	default: // CapabilityProceed, CapabilityUnknown
		return nil
	}
}

// ValidCapabilityShape reports the malformation in req as a message that
// starts with the name of the owning command or kind, or "" when req is
// valid: the complete zero value, or a known Tier with a non-empty Feature
// (and, for CapabilityExtDiscovery, a non-empty Unsupported). The message
// has no "internal error: " lead: Run adds it for its own messages, and
// the build of the command tree adds it once for the registry and the kind
// table (buildWiredRegistry and validateWiring in internal/cli/root.go). It
// does not hide behind IsSet (Tier != "") - a half-set
// value ({Feature set, Tier ""}) would otherwise read as absent and skip
// every gate that keys on IsSet, including the coverage rule's
// Capability-bearing filter. Shared by Registry.Validate, kinds'
// validateKinds, and both pipeline boundaries in Run, so a malformed value
// is rejected identically wherever it can appear. name identifies the owning
// command or kind in the message.
func ValidCapabilityShape(req CapabilityRequirement, name string) string {
	switch {
	case req.Tier == "" && req.Feature == "" && req.Unsupported == "":
		return ""
	case req.Tier == "":
		return fmt.Sprintf("%s: capability requirement has Feature/Unsupported set with no Tier", name)
	case req.Tier != CapabilityPrime && req.Tier != CapabilityExtDiscovery:
		return fmt.Sprintf("%s: unknown capability tier %q", name, req.Tier)
	case req.Feature == "":
		return fmt.Sprintf("%s: capability tier %q requires a Feature name", name, req.Tier)
	case req.Tier == CapabilityPrime && req.Unsupported != "":
		return fmt.Sprintf("%s: capability tier %q does not use Unsupported", name, req.Tier)
	case req.Tier == CapabilityExtDiscovery && req.Unsupported == "":
		return fmt.Sprintf("%s: capability tier %q requires Unsupported", name, req.Tier)
	default:
		return ""
	}
}

// confirmationMessage is the text of the refusal for the missing flags. A run
// without --apply has not previewed the change yet, so the text names
// --dry-run as the preview and asks for the flags after the caller has read
// it; a run with --apply names only the flags still missing.
func confirmationMessage(streams IO, missing []string) string {
	flags := strings.Join(missing, " ")
	if !streams.Apply {
		return "confirmation required for this mutation; preview it with --dry-run, then re-run with " + flags + " after reading the preview"
	}
	return "confirmation required for this mutation; re-run with " + flags
}

// missingFlags computes the confirmation flags absent for this Effect and binding.
func missingFlags(streams IO, eff safety.Effect, b safety.Binding) []string {
	var missing []string
	if !streams.Apply {
		missing = append(missing, "--apply")
	}
	if eff.Destructive && !streams.Yes {
		missing = append(missing, "--yes")
	}
	if b.Stability == safety.Deprecated && !streams.AllowDeprecated {
		missing = append(missing, "--allow-deprecated")
	}
	return missing
}

// payloadExclusivityRule is the single source for the Result channel
// invariant: only one of Text, Raw and RawBytes can be set, because each uses
// the whole stdout, and Summary cannot be set with any of them.
const payloadExclusivityRule = "only one of Text, Raw and RawBytes can be set, because each uses the whole stdout; Summary cannot be set with any of them"

// tableFormat reports whether format is one of the two formats rendered as text
// tables: the default ("") and the explicit "table" value.
func tableFormat(format string) bool {
	return format == "" || format == "table"
}

// textRenderFormat reports whether format prints a ShapeText Result's Text
// field through output.PrintableLines (as it is for a PlainText command):
// table/default, plus "wide" - for a ShapeText command, -o wide is
// table-family (no separate wide body exists for text).
func textRenderFormat(format string) bool {
	return tableFormat(format) || format == "wide"
}

// checkResultShape verifies res's actual payload against d's declared
// OutputShape - the closed-world conformance contract every registered
// command commits to. A mismatch is an internal error, the same posture as
// payloadExclusivityRule: the command populated the wrong Result field (or
// none) for the shape its own Descriptor declares.
//
// The check is scoped to the render path the requested format actually
// takes. ShapeTabular and the RawBytes/Raw payloads (ShapeBytes,
// ShapeExecJSON) back every format identically - Tabular Data is a payload
// whichever format renders it (a command that serves a table from the
// cluster learns from TextTableInput whether a text table is rendered, and
// returns the table only then), and the RawBytes/Raw branches in
// renderResult never look at format at all - so those three checks apply
// unconditionally. ShapeText is different: Text is read only
// by the table-family render path (textRenderFormat); -o json/-o yaml
// render the envelope over Data exactly as before, and -o name/-o template
// do the same (render.go's existing "-o name over non-Tabular Data emits
// nothing" contract), so an unpopulated Text is not a shape violation
// there - checking it under those formats would reject an unimplemented
// command's Data-only rendering even though nothing about that rendering
// is actually broken.
func checkResultShape(d Descriptor, res Result, format string) *clierr.Error {
	if res.OnceOnlyNotice != "" && d.Delivery != DeliveryOnceOnly {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s is not DeliveryOnceOnly but returned OnceOnlyNotice", d.Name))
	}
	if res.StyledText != nil && res.Text == "" {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s returned StyledText but no Text", d.Name))
	}
	if res.WideText != "" && (d.Shape != ShapeText || res.Text == "") {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s returned WideText without a ShapeText Text", d.Name))
	}
	if res.WideText != "" && res.StyledText != nil {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s returned both WideText and StyledText", d.Name))
	}
	switch d.Shape {
	case ShapeTabular:
		if _, ok := res.Data.(output.Tabular); !ok {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("internal error: %s declares ShapeTabular but its Data does not implement output.Tabular", d.Name))
		}
	case ShapeText:
		if textRenderFormat(format) && res.Text == "" {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("internal error: %s declares ShapeText but returned no Text", d.Name))
		}
	case ShapeBytes:
		if res.RawBytes == nil {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("internal error: %s declares ShapeBytes but returned no RawBytes", d.Name))
		}
	case ShapeExecJSON:
		if !res.Raw {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("internal error: %s declares ShapeExecJSON but Result.Raw is not set", d.Name))
		}
	}
	return nil
}

// envelopeCarriesWarnings reports whether the output format serializes the
// full structured envelope to the user - code, hint, details, and the
// top-level "warnings" key alike: only json and yaml do. Every other format
// (table, name, template, "") writes a plain-text projection instead, so a
// field that lives only in the envelope would otherwise be silently dropped.
func envelopeCarriesWarnings(format string) bool {
	return format == "json" || format == "yaml"
}

// mirrorWarnings writes each entry of warnings to w as a "# warning: <w>"
// line, skipping an entry already counted in silent - a value-matched
// multiset, not positional: Plan-time Effect.Warnings are prepended to a
// Result's Warnings before rendering, so a suppressed entry (the
// once-only advisory) is not reliably first. Decremented per skip so a
// repeated value mirrors past its silenced count. silent may be nil (no
// suppression). s styles the "# warning:" prefix only, never the
// message. The one
// implementation behind every stderr warning mirror in the pipeline:
// renderResult's Text, RawBytes, Raw, and envelope branches, and
// renderEffect's text and envelope branches.
func mirrorWarnings(w io.Writer, warnings []string, silent map[string]int, s style.Style) {
	for _, warn := range warnings {
		if silent[warn] > 0 {
			silent[warn]--
			continue
		}
		_, _ = fmt.Fprintf(w, "%s %s\n", s.Yellow("# warning:"), output.Printable(warn))
	}
}

// printableText is the text print of a result that is not the output of
// another program: the wide text or the text with control and format
// characters removed (newlines and this process's remedy markers kept). A
// styled text is chosen only when the text had nothing to remove and
// removing the six codes a style writes from the styled text gives the text
// back, so a styled text that adds any other sequence is written plain.
func printableText(res Result, wide bool, s style.Style) string {
	if wide {
		return output.PrintableLines(res.WideText)
	}
	clean := output.PrintableLines(res.Text)
	if res.StyledText != nil && clean == res.Text {
		if styled := res.StyledText(s); style.Unstyled(styled) == res.Text {
			return styled
		}
	}
	return clean
}

// renderResult renders res per d's declared shape, having first drained
// streams' NoticeSource and merged it ahead of res.Warnings (recorded
// earliest, delivered first - the same merge order RenderError applies on
// the error path). The returned notices are the drained entries NOT YET
// irrevocably delivered by a write independent of streams.Out succeeding:
// nil when the branch taken mirrors warnings straight to the real stderr
// (Text, RawBytes, Raw, and every non-json/yaml envelope format) or when
// nothing was recorded; the drained slice itself when nothing was written
// at all (an early return for an internal error) or when the only place the notices
// landed is inside the json/yaml bytes just handed to streams.Out - bytes a
// once-only caller may still discard if the real stdout write that follows
// fails. An ordinary (non-buffered) caller's streams.Out IS real stdout, so
// it has nothing left to do with a non-nil return on success; only a
// caller writing into a discardable buffer (finishOnceOnlyResult) needs to
// act on it.
func renderResult(streams IO, d Descriptor, res Result) ([]string, *clierr.Error) {
	outStyle, errStyle := stylesFor(streams)
	notices := streams.expandAll(drainNotices(streams))
	res.Warnings = append(append([]string{}, notices...), streams.expandAll(res.Warnings)...)
	res.OnceOnlyNotice = streams.ExpandText(res.OnceOnlyNotice)

	if ce := checkResultShape(d, res, streams.Format); ce != nil {
		return notices, ce
	}
	// Result's payload channels are mutually exclusive (payloadExclusivityRule);
	// any combination is an internal error.
	claimed := 0
	if res.Text != "" {
		claimed++
	}
	if res.Raw {
		claimed++
	}
	if res.RawBytes != nil {
		claimed++
	}
	if claimed > 1 || (res.Summary != "" && claimed > 0) {
		return notices, clierr.New(clierr.CodeGeneric, "internal error: "+payloadExclusivityRule)
	}
	// Summary rides only over a real table: the JSON-fallback-era use (a
	// Summary line papering over non-Tabular Data) is gone along with the
	// fallback itself.
	if res.Summary != "" {
		if _, ok := res.Data.(output.Tabular); !ok {
			return notices, clierr.New(clierr.CodeGeneric, "internal error: Summary requires Tabular Data")
		}
	}
	if res.Text != "" && textRenderFormat(streams.Format) {
		expand := streams.ExpandLines
		wide := streams.Format == "wide" && res.WideText != ""
		var text string
		if d.PlainText {
			// The output of another program: written as it is.
			expand = plainRemedies
			switch {
			case wide:
				text = res.WideText
			case res.StyledText != nil:
				text = res.StyledText(outStyle)
			default:
				text = res.Text
			}
		} else {
			text = printableText(res, wide, outStyle)
		}
		// Composed, not either-or: a once-only command that also styles
		// its text must not lose the shown-once emphasis. No double
		// wrap either way: a builder that already styled that line left
		// a trailing reset code, so the suffix is no longer terminal and
		// the match declines; an unstyled line keeps the suffix and
		// gains the bold here.
		if d.Delivery == DeliveryOnceOnly {
			text = boldShownOnceLine(text, outStyle)
		}
		if _, err := streams.Out.Write([]byte(expand(text))); err != nil {
			return notices, clierr.From(err)
		}
		// Mirror every Warnings entry to stderr except OnceOnlyNotice
		// (matched by value - see that field's doc for why position
		// cannot be trusted), whose content Text already embeds via
		// ShownOnceSuffix: one channel, no double-print for that one;
		// every other warning, including a mutation's Plan-time advisory,
		// still reaches stderr.
		silent := map[string]int{}
		if res.OnceOnlyNotice != "" {
			silent[res.OnceOnlyNotice]++
		}
		mirrorWarnings(streams.Err, res.Warnings, silent, errStyle)
		return nil, nil
	}
	if res.RawBytes != nil {
		if _, err := streams.Out.Write(res.RawBytes); err != nil {
			return notices, clierr.From(err)
		}
		// RawBytes carries no envelope under any format (kubeconfig,
		// completion): stderr is the only delivery path Warnings has, so it
		// mirrors unconditionally rather than only for the non-json/yaml
		// formats the envelope branch below gates on.
		mirrorWarnings(streams.Err, res.Warnings, nil, errStyle)
		return nil, nil
	}
	// Raw results (exec-credential) bypass the envelope: kubectl parses a single
	// bare JSON object from the plugin's stdout.
	if res.Raw {
		enc := json.NewEncoder(streams.Out)
		if err := enc.Encode(res.Data); err != nil {
			return notices, clierr.From(err)
		}
		// Same rationale as RawBytes above: kubectl surfaces exec-credential
		// stderr, and no envelope exists here under any format.
		mirrorWarnings(streams.Err, res.Warnings, nil, errStyle)
		return nil, nil
	}
	env := output.NewEnvelope(res.Kind, res.Data)
	env.Warnings = res.Warnings
	env.Summary = res.Summary
	// The Summary annotation line prints over the table under both ""/
	// "table" and "wide" - `get --wait`'s wide output is table-family too,
	// not a separate body, so the wait line belongs on it as well.
	if textRenderFormat(streams.Format) && res.Summary != "" {
		if _, err := fmt.Fprintln(streams.Out, streams.ExpandLines(PrintableRemedies(res.Summary))); err != nil {
			return notices, clierr.From(err)
		}
	}
	if err := output.RenderWith(plainRemedyWriter{streams.Out}, streams.Format, env, outStyle, streams.expandData); err != nil {
		return notices, clierr.From(err)
	}
	// renderTable (output/render.go) only ever reads env.Data, so an
	// envelope-level warning would otherwise reach the user only when the
	// format embeds the full envelope (json/yaml). Mirror the
	// "# warning: <w>" stderr convention for every other format: table/default
	// (where a user looks at the rendered rows), name (whose stdout is the raw
	// name field - no envelope), and template (whose stdout is the template
	// expansion - no envelope). json/yaml carry warnings inside the envelope;
	// mirroring there would double-print.
	if !envelopeCarriesWarnings(streams.Format) {
		mirrorWarnings(streams.Err, res.Warnings, nil, errStyle)
		return nil, nil
	}
	// json/yaml: the notices are embedded only in the envelope bytes just
	// written to streams.Out, not mirrored anywhere else - report them back
	// so a caller writing into a discardable buffer can recover them if the
	// real write that follows fails.
	return notices, nil
}

// renderEffect renders a --dry-run preview. table/default/wide (wide is
// table-family here too - there is no separate wide body for a preview)
// render the pipeline-owned dryRunText body; json/yaml keep today's Effect
// envelope unchanged, as do the raw formats (name/template), which read the
// envelope's target field.
func renderEffect(streams IO, eff safety.Effect) *clierr.Error {
	_, errStyle := stylesFor(streams)
	// A dry-run Plan can still touch a lazily-resolving credential store
	// (reading the current server's CA, say); drain ahead of eff.Warnings
	// so that notice is not silently stranded in the source until whatever
	// later invocation happens to drain it.
	eff.Warnings = streams.expandAll(append(drainNotices(streams), eff.Warnings...))
	if textRenderFormat(streams.Format) {
		if _, err := streams.Out.Write([]byte(streams.ExpandLines(output.PrintableLines(dryRunText(eff))))); err != nil {
			return clierr.From(err)
		}
		// Effect.Warnings are deliberately excluded from dryRunText's body -
		// mirror them to stderr here so they are not silently dropped,
		// the same contract renderResult's Text branch applies to
		// Result.Warnings.
		mirrorWarnings(streams.Err, eff.Warnings, nil, errStyle)
		return nil
	}
	env := output.NewEnvelope("Effect", eff)
	env.Warnings = append(env.Warnings, eff.Warnings...)
	if err := output.RenderWith(plainRemedyWriter{streams.Out}, streams.Format, env, style.Style{}, streams.expandData); err != nil {
		return clierr.From(err)
	}
	// renderTable (render.go) encodes env.Data alone, so envelope-level
	// warnings vanish for every format that does not serialize the full
	// envelope - table, name, template. Mirror the "# warning: <w>" stderr
	// convention renderResult uses for those formats; json/yaml carry warnings
	// inside the envelope and must not double-print.
	if !envelopeCarriesWarnings(streams.Format) {
		mirrorWarnings(streams.Err, env.Warnings, nil, errStyle)
	}
	return nil
}

// dryRunText renders --dry-run's default-format preview body: the action and
// DisplayKind/DisplayName identity (a generateName preview may carry a
// trailing "*" - never the committed success identity), the target
// server/cluster when set, a change summary from the Effect's diff entries,
// and the confirmation gate line naming the flags a real run needs.
// Effect.Warnings are deliberately NOT part of this body (see renderEffect).
func dryRunText(eff safety.Effect) string {
	var lines []string
	if eff.Server != "" {
		lines = append(lines, "server: "+PrintableRemedies(eff.Server))
	}
	if eff.Cluster != "" {
		lines = append(lines, "cluster: "+PrintableRemedies(eff.Cluster))
	}
	for _, d := range eff.Diff {
		// d.Path is an RFC 6901 JSON Pointer ("~1" for "/", "~0" for "~"),
		// printed verbatim: it is valid as-is for a JSON Patch op's "path".
		// The values are the fields the request sets or changes, computed
		// by the command: one line each, with control characters stripped
		// so a value cannot forge a line.
		lines = append(lines, output.Printable(fmt.Sprintf("%s: %s -> %s", d.Path, diffValueText(d.From), diffValueText(d.To))))
	}
	lines = append(lines, ObjectLines(eff.Objects, eff.DisplayKind)...)
	gate := "dry-run preview - re-run with --apply"
	if eff.Destructive {
		gate += " --yes"
	}
	gate += " to apply the change"
	lines = append(lines, gate)
	return TextLines(fmt.Sprintf("%s %s/%s", eff.Action, PrintableRemedies(eff.DisplayKind), PrintableRemedies(eff.DisplayName)), lines...).Text
}

// diffValueText renders one side of a diff entry for the text preview: a
// map or a list as compact JSON ({"k":"v"}, ["a","b"]) with <, > and & as
// typed, a missing side as <nil>, anything else as its plain text.
func diffValueText(v any) string {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Slice:
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err == nil {
			return strings.TrimSuffix(b.String(), "\n")
		}
	}
	return fmt.Sprintf("%v", v)
}

// renderErr writes ce to stderr in the invocation's requested format, after
// draining streams' NoticeSource: a notice recorded earlier in this
// invocation (e.g. a lazy store's loose-permission finding surfacing on the
// failure path) leads ce's own error-carried warnings - RenderError's notices
// parameter is documented as recorded-earliest-first for exactly this call.
func renderErr(streams IO, ce *clierr.Error) *clierr.Error {
	_, errStyle := stylesFor(streams)
	return renderErrDrained(streams, ce, errStyle, drainNotices(streams))
}

// renderErrDrained is renderErr for a caller that has already drained the
// invocation's NoticeSource and computed its stderr style: notices are the
// caller's drained entries (recorded-earliest-first), passed through
// unchanged; the source is never touched again, and the styles are not
// recomputed. finishResult and finishOnceOnlyResult use it after
// renderResult has already drained; every other error path goes through
// renderErr. A failed write of the error is not reported to the caller: the
// returned error, and so the exit code, is ce either way.
func renderErrDrained(streams IO, ce *clierr.Error, errStyle style.Style, notices []string) *clierr.Error {
	WriteError(streams.Err, streams.Format, streams.ExpandError(ce), errStyle, streams.expandAll(notices)...)
	return ce
}

// RenderError writes ce to w in format: the structured envelope, byte-for-byte
// what json/yaml have always emitted, for those two formats - yaml serializes
// as YAML, not JSON dressed up as one - and a short operator-facing text block
// for every other format, including an unrecognized one (an untrusted format
// value renders as text rather than being guessed at as an envelope). This
// is the always-plain convenience form of RenderErrorStyled: every
// production render site - the pipeline and internal/cli's own pre-dispatch
// failures alike - resolves a style and calls RenderErrorStyled directly.
//
// notices and ce's own warning ride (WithWarnings) are merged in one fixed
// order - notices first (recorded earliest, e.g. a NoticeSource drained
// ahead of the failing step), then ce.Warnings() (attached by the failing
// step itself) - and placed at the envelope's top-level "warnings" field for
// json/yaml, or as "# warning:" stderr lines ahead of the error text
// otherwise. Never inside the error payload itself, so a warning cannot
// print twice.
func RenderError(w io.Writer, format string, ce *clierr.Error, notices ...string) error {
	return RenderErrorStyled(w, format, ce, style.Style{}, notices...)
}

// RenderErrorStyled is RenderError with presentation control: s styles the
// "error (CODE):" prefix red and the "hint:"/"# warning:" prefixes cyan and
// yellow respectively, applied only when format is among the styleable set
// (table/default/wide) - json, yaml, name, and template render byte-identical
// to RenderError regardless of s, the same eligibility rule RenderStyled
// enforces for the table renderer.
func RenderErrorStyled(w io.Writer, format string, ce *clierr.Error, s style.Style, notices ...string) error {
	// A caller with an invocation expands its remedy commands first
	// (IO.ExpandError); whatever is left is written as the plain command.
	ce = ce.MapText(plainRemedies)
	warnings := append(append([]string{}, notices...), ce.Warnings()...)
	for i, warn := range warnings {
		warnings[i] = plainRemedies(warn)
	}
	if envelopeCarriesWarnings(format) {
		env := output.ErrorEnvelope(ce)
		env.Warnings = warnings
		return output.Render(w, format, env)
	}
	if !textRenderFormat(format) {
		s = style.Style{}
	}
	var b strings.Builder
	for _, warn := range warnings {
		fmt.Fprintf(&b, "%s %s\n", s.Yellow("# warning:"), output.Printable(warn))
	}
	b.WriteString(errorText(ce, s))
	_, err := w.Write([]byte(b.String()))
	return err
}

// WriteError renders ce to w as RenderErrorStyled does and reports no result:
// the caller's exit code is ce's own whether or not the write worked; no
// caller has an error to drop. When the render fails (the format cannot
// encode ce, or the write fails) and no byte of this error reached w, it
// writes the one line "error (CODE): message" instead, the same for every
// format, with the message free of control characters and remedy markers; a
// partial write is never followed by that line, which would tear the
// document. When that write fails too, it stops.
func WriteError(w io.Writer, format string, ce *clierr.Error, s style.Style, notices ...string) {
	cw := &countingWriter{w: w}
	if RenderErrorStyled(cw, format, ce, s, notices...) == nil || cw.n > 0 {
		return
	}
	line := "error (" + ce.Code + "): " + output.Printable(ce.Message) + "\n"
	_, _ = io.WriteString(w, line)
}

// countingWriter counts the bytes its writer accepted.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// errorText renders ce as the default-format stderr block: a header line
// naming the code (and "retryable" when the error is retryable), an indented
// hint line when present, an indented candidates line - each candidate
// rendered "name (id)" - when present, and finally the details payload's own
// projected lines (detailsTextLines) when one is attached, each value of which
// is shown with its hidden characters as escapes. s styles the
// "error (CODE):" and "hint:" prefixes only, never the message/hint body.
func errorText(ce *clierr.Error, s style.Style) string {
	retry := ""
	if ce.Retryable {
		retry = ", retryable"
	}
	prefix := fmt.Sprintf("error (%s%s):", ce.Code, retry)
	// Message, hint, candidates, and details may all carry server text
	// (an admission message, a display name), so each line passes the
	// terminal-safety boundary before it reaches stderr. A details line shows
	// each value with its hidden characters as escapes (detailLineText);
	// the other lines have them removed.
	header := s.Red(prefix) + " " + output.Printable(ce.Message)

	var lines []string
	if ce.Hint != "" {
		lines = append(lines, s.Cyan("hint:")+" "+output.Printable(ce.Hint))
	}
	if len(ce.Candidates) > 0 {
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = fmt.Sprintf("%s (%s)", output.Printable(c.Name), output.Printable(c.ID))
		}
		label := "candidates"
		if ce.CandidatesTotal > 0 {
			label = fmt.Sprintf("candidates (%d of %d)", len(ce.Candidates), ce.CandidatesTotal)
		}
		lines = append(lines, label+": "+strings.Join(names, ", "))
	}
	for _, l := range detailsTextLines(ce.Details) {
		lines = append(lines, detailLineText(l))
	}
	return TextLines(header, lines...).Text
}

// detailsFallbackLine is the neutral line rendered when a details payload
// reached the renderer without going through clierr.Error.WithDetails (an
// Error literal setting Details directly, for example) - an internal error, not
// a supported way to ship an unreviewed details type.
const detailsFallbackLine = "the error has more details - re-run with -o json to see them"

// detailsTextLines projects an error's Details field onto its default-format
// lines: the payload's own TextDetails() when it implements the interface,
// detailsFallbackLine otherwise, nothing when Details is unset. The lines hold
// the payload's values as they are; detailLineText writes them.
func detailsTextLines(details any) []clierr.DetailLine {
	if details == nil {
		return nil
	}
	if td, ok := details.(clierr.TextDetails); ok {
		return td.TextDetails()
	}
	return []clierr.DetailLine{clierr.Line(clierr.LabelPart(detailsFallbackLine))}
}

// detailLineText is l as the default format prints it: the indent, then each
// part's label as written and its value through clierr.IDText, so a value that
// holds a control character, an invisible format character or a line or
// paragraph separator shows as a double-quoted string with escapes, and two
// values that differ by a hidden character print differently. The indent stays
// outside the quotes.
// An invalid UTF-8 byte is already an escape when the text reaches
// output.Printable, which removes and replaces nothing in it: the labels hold
// no rune it removes.
func detailLineText(l clierr.DetailLine) string {
	var b strings.Builder
	b.WriteString(l.Indent)
	for _, p := range l.Parts {
		b.WriteString(p.Label)
		b.WriteString(clierr.IDText(p.Value))
	}
	return output.Printable(b.String())
}

// finishResult enforces the SuccessExitCodes declaration, renders the
// result, and surfaces the declared status. An undeclared status is an
// internal error rendered through renderErr like every other pipeline
// error. For an ORDINARY command a render failure - a shape-contract
// violation (checkResultShape) or a stdout write failure - is rendered to
// stderr exactly once here, so a non-zero exit is never silent regardless
// of what already committed server-side. A Delivery: DeliveryOnceOnly
// command instead renders through finishOnceOnlyResult, which buffers the
// render and emits a committed-context error to stderr on failure - see
// that function's doc.
func finishResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	if !allowedExitStatus(d.SuccessExitCodes, res.ExitStatus) {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s returned undeclared exit status %d", d.Name, res.ExitStatus)))
	}
	if d.Delivery == DeliveryOnceOnly {
		return finishOnceOnlyResult(streams, d, res)
	}
	// streams.Out here is the invocation's real stdout (never a discardable
	// buffer - only finishOnceOnlyResult writes into one), so a nil error
	// means whatever renderResult returned as "not yet delivered" already
	// landed for real.
	notices, ce := renderResult(streams, d, res)
	if ce != nil {
		_, errStyle := stylesFor(streams)
		return 0, renderErrDrained(streams, ce.WithWarnings(notices...), errStyle, nil)
	}
	return res.ExitStatus, nil
}

// finishOnceOnlyResult renders a DeliveryOnceOnly command's result BUFFERED:
// the payload is encoded fully into memory (Warnings still render to the
// real stderr immediately - only the once-only stdout payload is
// buffered), then written to the real stdout in a single Write. A
// DeliveryOnceOnly executor MUST return a non-nil Result.CommittedContext
// on every successful return; its absence is an internal error, never a
// silent skip, since a later render failure would otherwise have nothing
// to report. When the buffered render fails, or the final single write
// fails, the failure is emitted as a structured, non-retryable error
// envelope to stderr (best-effort - its own write failure is not reported
// and the caller still gets the correct exit code): it names the resource the
// mutation already committed, whether the final write may have exposed a
// partial value (n > 0 on the failing write), and the non-mutating
// recovery - never inviting a blind retry of a mutation that already
// happened.
func finishOnceOnlyResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	// Computed once for every error path below: the paths after the
	// renderResult call must not go through renderErr, whose own drain would
	// hit the already-emptied NoticeSource and whose styles are recomputed
	// per call - the drained notices ride each error's warnings instead.
	_, errStyle := stylesFor(streams)
	if res.CommittedContext == nil {
		return 0, renderErrDrained(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: %s is DeliveryOnceOnly but returned no CommittedContext", d.Name)).
			WithWarnings(drainNotices(streams)...), errStyle, nil)
	}
	buffered := streams
	var buf bytes.Buffer
	buffered.Out = &buf
	// pending is nil when renderResult already delivered every notice for
	// real (mirrored straight to the real streams.Err, unaffected by the Out
	// swap above) - non-nil only when the notices' one and only delivery so
	// far is inside buf, which the caller below might still discard.
	pending, ce := renderResult(buffered, d, res)
	if ce != nil {
		// Nothing was written anywhere (checkResultShape and the
		// exclusivity/Summary checks all return before any write): every
		// drained notice is still unaccounted for.
		return 0, renderErrDrained(streams, committedContextError(res.CommittedContext, false,
			"the change was applied, but its output could not be formatted: "+ce.Message).
			WithWarnings(pending...), errStyle, nil)
	}
	n, err := streams.Out.Write(buf.Bytes())
	if err != nil {
		// buf held the only copy of any json/yaml-embedded notice; it is
		// about to be discarded, so pending rides the replacement error
		// instead. A pending nil here (the text/RawBytes/Raw/non-json-yaml
		// branches) means nothing to add - those notices already reached
		// the real stderr inside the renderResult call above, independent
		// of this failing stdout write.
		return 0, renderErrDrained(streams, committedContextError(res.CommittedContext, n > 0,
			"the change was applied, but writing its output failed: "+err.Error()).
			WithWarnings(pending...), errStyle, nil)
	}
	return res.ExitStatus, nil
}

// committedContextError builds the shared post-commit-failure shape: a
// GENERIC, non-retryable error carrying clierr.CommittedContextDetails, with
// message as its text.
func committedContextError(cc *CommittedContext, partialOutput bool, message string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, message).
		WithDetails(clierr.CommittedContextDetails{
			Committed:     true,
			ResourceKind:  cc.ResourceKind,
			ResourceName:  cc.ResourceName,
			SecretName:    cc.SecretName,
			PartialOutput: partialOutput,
			Recovery:      cc.Recovery,
		})
}

// allowedExitStatus reports whether a successful result may carry status.
// An empty declaration means only 0.
func allowedExitStatus(declared []int, status int) bool {
	if status == 0 {
		return true
	}
	return slices.Contains(declared, status)
}

// toClierr gives a command's error its envelope shape through clierr.From: a
// structured *clierr.Error passes through (a %w-wrapped one is unwrapped, so
// hint and candidates survive), a server status in the error's chain gets
// its code from the status table, and anything else is GENERIC. When ctx,
// the command's own context, has been cancelled, the operator's interrupt is
// what ended the command whatever error the cancelled path surfaced, so the
// error is INTERRUPTED with that error as its cause. A deadline on ctx is
// not an interrupt.
func toClierr(ctx context.Context, err error) *clierr.Error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return interrupted(err)
	}
	return clierr.From(err)
}

// interrupted builds the INTERRUPTED error for a command that ended with
// err after its context was cancelled. A bare cancellation (err is the
// context's own error, or wraps it) says "interrupted" alone; any other
// surfaced error keeps its message, details and warnings, so a partial
// outcome the command reported is not lost to the classification.
func interrupted(err error) *clierr.Error {
	out := clierr.Interrupted().WithCause(err)
	if errors.Is(err, context.Canceled) {
		return out
	}
	out.Message += ": " + err.Error()
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		out.Details = ce.Details
		out = out.WithWarnings(ce.Warnings()...)
	}
	return out
}

// emptyValueError refuses a value-taking flag typed with an empty value,
// which would otherwise act as if the flag were omitted: a list across
// every namespace, a default, a filter dropped. A flag whose empty value
// means something omission does not sets AllowEmpty. A bool flag typed
// empty is EmptyBoolError's: the parsed value cannot show it.
func emptyValueError(f ArgSpec, in Input) *clierr.Error {
	if f.Type == "bool" || f.AllowEmpty || !in.IsSet(f.Name) {
		return nil
	}
	empty := blank(in.String(f.Name))
	if f.Type == "stringSlice" {
		entries := in.Strings(f.Name)
		empty = len(entries) == 0 || slices.ContainsFunc(entries, blank)
	}
	switch {
	case !empty:
		return nil
	case f.Required:
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("--%s is empty; pass a value", f.Name))
	default:
		return EmptyValueError(f.Name)
	}
}

// EmptyBoolError is the refusal of the bool flags typed with "=" and an
// empty value or one of only whitespace, each as typed without the "=":
// the value meant is unknown, so neither on nor off is assumed. Each flag is
// named once; a line names a bounded number of distinct bool flags, so every
// one is named.
func EmptyBoolError(flags []string) *clierr.Error {
	var named, fixes []string
	for _, f := range flags {
		if slices.Contains(named, f+"=") {
			continue
		}
		named = append(named, f+"=")
		fixes = append(fixes, f+" or "+f+"=true|false")
	}
	verb := "has"
	if len(named) > 1 {
		verb = "have"
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s %s an empty value; a bool flag typed with = takes true or false", strings.Join(named, ", "), verb)).
		WithHint("write " + strings.Join(fixes, "; "))
}

// DroppedWordsError is the refusal of a command line with words, as typed,
// after a "-" on its own. It names the first maxNamedWords of them and
// counts the rest, except that a word starting with "-" named last is
// named with the word after it unless that word starts with "-" too, so a
// flag is never named without the value that may follow it (`'-o' 'json'`,
// never `'-o' and 1 more`). Which flags take a value is not known for
// words after a "-" that ends the line.
func DroppedWordsError(words []string) *clierr.Error {
	named := min(len(words), maxNamedWords)
	if named < len(words) && startsWithDash(words[named-1]) && !startsWithDash(words[named]) {
		named++
	}
	quoted := make([]string, named)
	for i, w := range words[:named] {
		quoted[i] = clierr.ShellQuote(w)
	}
	text := strings.Join(quoted, " ")
	if rest := len(words) - named; rest > 0 {
		text += fmt.Sprintf(" and %d more", rest)
	}
	return clierr.New(clierr.CodeValidation,
		"the words after a - on its own are not used: "+text).
		WithHint("put - last, or after -- when it is meant as an argument")
}

// DroppedWordsCountError is DroppedWordsError for a line the walk has not
// shown to be an ordinary command's: a command that takes a secret input, a
// group, or a leaf with no descriptor. The words after a "-" on its own may
// hold a secret, so it counts them and names none.
func DroppedWordsCountError(count int) *clierr.Error {
	text := fmt.Sprintf("%d words after a - on its own are not used", count)
	if count == 1 {
		text = "1 word after a - on its own is not used"
	}
	return clierr.New(clierr.CodeValidation, text).
		WithHint("put - last, or after -- when it is meant as an argument")
}

// startsWithDash reports whether w, with the whitespace around it cut off,
// starts with "-".
func startsWithDash(w string) bool { return strings.HasPrefix(strings.TrimSpace(w), "-") }

// EmptyValueError is the refusal of a typed empty value of the flag name
// that may be omitted.
func EmptyValueError(name string) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--%s is empty; pass a value or omit --%s", name, name))
}

// blank reports a value that names nothing: empty, or spaces only.
func blank(v string) bool { return strings.TrimSpace(v) == "" }
