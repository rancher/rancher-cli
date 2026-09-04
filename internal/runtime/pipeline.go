package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
	// terminal — decided independently, since one stream can be redirected
	// while the other is not (`rancher get pod | less` still colors
	// stderr). Distinct from IsTTY above, which is stdin-only and feeds the
	// prompter, not styling.
	OutTTY bool
	ErrTTY bool

	// ReadOnly is the OR of three sources — config `readOnly: true`, a
	// non-empty RANCHER_CLI_READONLY, and --read-only — resolved once per
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
	// command's Descriptor carries a CapabilityRequirement — ordinary
	// commands pay nothing, and adapters that never serve requirement-
	// bearing commands may leave it nil. A requirement-bearing command
	// reaching Run with a nil checker is mis-wiring and fails loudly.
	Capability CapabilityChecker

	// Notices drains operator-facing advisories recorded outside any
	// Result by this invocation's credential-store dependency (a first-pin
	// notice, a keychain-degrade warning) — wired from the resolved store
	// wherever one exists; nil for a command with no store dependency
	// (schema, a bare local command). renderResult, renderEffect, renderErr,
	// and finishOnceOnlyResult each drain it exactly once, feeding the one
	// render they own.
	Notices NoticeSource
}

// NoticeSource drains buffered operator-facing notices in the order they
// were recorded, destructively: a call returns only what was recorded since
// the previous drain. Implemented by a credential store so that resolving it
// lazily — possibly racing a command-body goroutine — never loses a notice
// recorded outside any Result. Draining must not itself force a lazy store to
// resolve: a command that never touches credentials must still see zero
// store-init side effects.
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

	if ce := validateFormat(streams.Format); ce != nil {
		return 0, renderErr(streams, ce)
	}

	if ce := validateColor(streams.Color); ce != nil {
		return 0, renderErr(streams, ce)
	}

	// The output-policy gate runs immediately after format-syntax
	// validation and before EVERYTHING else — read-only/dry-run gates,
	// input validation, capability resolution, Plan — so a rejected format
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

	if streams.DryRun && d.CommandType != Mutating {
		return 0, renderErr(streams, clierr.New(clierr.CodeDryRunNotApplicable,
			fmt.Sprintf("%s is a %s command; re-run without --dry-run", d.Name, d.CommandType)))
	}

	if ce := validateInput(d, in); ce != nil {
		return 0, renderErr(streams, ce)
	}

	// Capability detection is a mandatory stage for any command carrying a
	// requirement — commands never opt in or out — and runs after
	// server/credential resolution but before any resource access (reads)
	// or the server-side dry-run Plan (mutations), so a Prime-backed kind
	// that may not exist on a community server never gets to 404 first.
	//
	// The static descriptor value is validated BEFORE its IsSet use: IsSet
	// (Tier != "") cannot see a half-set value, so validating after it
	// fails open — only a fully valid zero value selects the resolver.
	if ce := ValidCapabilityShape(d.Capability, d.Name); ce != nil {
		return 0, renderErr(streams, ce)
	}
	req := d.Capability
	if !req.IsSet() {
		if r, ok := cmd.(CapabilityResolver); ok {
			var err error
			req, err = r.ResolveCapability(ctx, in)
			if err != nil {
				return 0, renderErr(streams, toClierr(err))
			}
			// Then the resolver's return — an arbitrary runtime value
			// startup validation never saw. A malformed value is a command
			// bug, stopped before the checker and Plan/Execute.
			if ce := ValidCapabilityShape(req, d.Name); ce != nil {
				return 0, renderErr(streams, ce)
			}
		}
	}
	if req.IsSet() {
		if ce := checkCapability(ctx, d, req, streams); ce != nil {
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
			"command implements neither ReadCommand nor MutatingCommand: "+d.Name))
	}
}

// runMutating drives the Plan → invariant checks → server stamping →
// confirmation gate → Execute path for a mutating command.
func runMutating(ctx context.Context, c MutatingCommand, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	eff, exec, err := c.Plan(ctx, in)
	if err != nil {
		return 0, renderErr(streams, toClierr(err))
	}
	if ce := checkPlan(eff, exec); ce != nil {
		return 0, renderErr(streams, ce)
	}
	if streams.ServerIdentity != nil {
		server, err := streams.ServerIdentity()
		if err != nil {
			return 0, renderErr(streams, toClierr(err))
		}
		eff.Server = server
	}
	if streams.DryRun {
		return 0, renderEffect(streams, eff)
	}
	if missing := missingFlags(streams, eff, b); len(missing) > 0 {
		return 0, renderErr(streams, clierr.New(clierr.CodeConfirmationRequired,
			"confirmation required for this mutation; re-run with "+strings.Join(missing, " ")))
	}
	res, err := exec(ctx)
	if err != nil {
		return 0, renderErr(streams, toClierr(err))
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

// checkPlan enforces the command-bug invariants on a Plan result: a non-nil
// executor, a valid action, and destructive-delete. It never renders; the
// caller wraps the returned error.
func checkPlan(eff safety.Effect, exec ExecuteFunc) *clierr.Error {
	if exec == nil {
		return clierr.New(clierr.CodeGeneric, "command bug: Plan returned no executor")
	}
	if !eff.Action.Valid() {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: invalid effect action %q", eff.Action))
	}
	if eff.DisplayKind == "" || eff.DisplayName == "" {
		return clierr.New(clierr.CodeGeneric,
			"command bug: effect has an empty DisplayKind or DisplayName")
	}
	if eff.Action == safety.ActionDelete && !eff.Destructive {
		return clierr.New(clierr.CodeGeneric, "command bug: delete effect must be destructive")
	}
	return nil
}

// runRead executes a read command.
func runRead(ctx context.Context, c ReadCommand, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	p := ui.New(streams.Err, streams.In, streams.NoInput, streams.IsTTY)
	res, err := c.Execute(ctx, in, p)
	if err != nil {
		return 0, renderErr(streams, toClierr(err))
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
// depth for non-pipeline callers).
func validateFormat(format string) *clierr.Error {
	if format == "" {
		return nil
	}
	if slices.Contains(OutputFormats, format) {
		return nil
	}
	if src, ok := strings.CutPrefix(format, output.TemplatePrefix); ok {
		if src == "" {
			return clierr.New(clierr.CodeValidation,
				"--output template source is empty; pass -o template='{{ .field }}'")
		}
		if _, err := template.New("out").Parse(src); err != nil {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("invalid template: %v; must be valid Go text/template syntax", err))
		}
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--output=%q is not one of %s|template=<go-template>", format, strings.Join(OutputFormats, "|")))
}

// validateColor closes the --color value set: "" (the flag omitted, same as
// "auto") and the ColorModes literals. Run calls it before dispatch so an
// unrecognized value is a structured VALIDATION error rather than a silent
// fall-through to auto.
func validateColor(color string) *clierr.Error {
	if color == "" || slices.Contains(ColorModes, color) {
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--color=%q is not one of %s", color, strings.Join(ColorModes, "|")))
}

// validateOutputPolicy enforces a Descriptor's OutputPolicy: a command that
// restricts formats (typically to exclude "name", which would silently
// discard a once-only value) rejects an unsupported format here, before any
// capability resolution or network work — the zero OutputPolicy always
// passes.
func validateOutputPolicy(d Descriptor, format string) *clierr.Error {
	if d.OutputPolicy.Allows(format) {
		return nil
	}
	shown := format
	if shown == "" {
		shown = "table (the default)"
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s does not support --output=%s; allowed: %s",
			d.Name, shown, strings.Join(d.OutputPolicy.AllowedFormats, ", ")))
}

// validateInput checks the Descriptor's Args and Flags against the parsed Input:
// required positional args must be present and non-empty; enum args/flags must
// hold a listed value; required flags must be non-empty. Returns a structured
// VALIDATION error (nil if valid). Commands wanting custom missing-input UX
// (e.g. a candidate list) mark the field Required:false and validate it
// themselves in Execute.
func validateInput(d Descriptor, in Input) *clierr.Error {
	args := in.Args()
	for i, a := range d.Args {
		if a.Required && (i >= len(args) || args[i] == "") {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("argument <%s> is required", a.Name))
		}
		if len(a.Enum) > 0 && i < len(args) && args[i] != "" {
			ok := slices.Contains(a.Enum, args[i])
			if !ok {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("argument <%s>=%q is not one of %s", a.Name, args[i], strings.Join(a.Enum, "|")))
			}
		}
	}
	for _, f := range d.Flags {
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
						return clierr.New(clierr.CodeValidation,
							fmt.Sprintf("--%s=%q is not one of %s", f.Name, v, strings.Join(f.Enum, "|")))
					}
				}
			}
			continue
		}
		v := in.String(f.Name)
		if f.Required && v == "" {
			return missingFlagError(f)
		}
		if len(f.Enum) > 0 && v != "" {
			ok := slices.Contains(f.Enum, v)
			if !ok {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--%s=%q is not one of %s", f.Name, v, strings.Join(f.Enum, "|")))
			}
		}
	}
	return nil
}

// missingFlagError is the refusal for a required flag that was not passed:
// the flag's own MissingMessage and MissingHint when its descriptor set
// them, the generic form otherwise.
func missingFlagError(f ArgSpec) *clierr.Error {
	if f.MissingMessage == "" {
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
// the PRIME_REQUIRED message names the command (d.Name — for generated
// verbs that carries the kind; for create resource it names the command, a
// reviewed acceptance until the capability Name identifier lands) and its
// details.feature carries req.Feature, the machine-readable identifier.
// The Disabled message names req.Feature directly and carries no details —
// the remedy is the feature flag, not an entitlement. Its wording forks on
// dec.EditionPrime: a known-Prime server gets the actionable "enable
// feature <name>"; every other edition gets a statement that holds
// regardless of tier, since a community or unknown edition might still turn
// out to require Prime once the feature is tiered server-side.
func checkCapability(ctx context.Context, d Descriptor, req CapabilityRequirement, streams IO) *clierr.Error {
	if streams.Capability == nil {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s declares a capability requirement but no capability checker is wired; "+
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
			fmt.Sprintf("%s is disabled on this server; availability is decided by the connected server — %s",
				req.Feature, clierr.LearnMorePrimeURL))
	case CapabilityPrimeRequired:
		return clierr.New(clierr.CodePrimeRequired,
			fmt.Sprintf("%s requires Rancher Prime; the connected server does not have Rancher Prime; %s",
				d.Name, clierr.LearnMorePrimeURL)).
			WithDetails(clierr.PrimeRequiredDetails{Feature: req.Feature, Server: dec.Server})
	case CapabilityUnsupported:
		return clierr.New(clierr.CodeCapabilityUnsupported,
			fmt.Sprintf("this server does not support %s; upgrade the connected server to a version that carries this workflow", req.Unsupported)).
			WithDetails(clierr.CapabilityUnsupportedDetails{Feature: req.Feature, Server: dec.Server})
	default: // CapabilityProceed, CapabilityUnknown
		return nil
	}
}

// ValidCapabilityShape reports the malformation in req as a CodeGeneric
// error, or nil when req is valid: the complete zero value, or a known Tier
// with a non-empty Feature (and, for CapabilityExtDiscovery, a non-empty
// Unsupported). It does not hide behind IsSet (Tier != "") — a half-set
// value ({Feature set, Tier ""}) would otherwise read as absent and skip
// every gate that keys on IsSet, including the coverage rule's
// Capability-bearing filter. Shared by Registry.Validate, kinds'
// validateKinds, and both pipeline boundaries in Run, so a malformed value
// is rejected identically wherever it can appear. name identifies the owning
// command or kind in the message.
func ValidCapabilityShape(req CapabilityRequirement, name string) *clierr.Error {
	switch {
	case req.Tier == "" && req.Feature == "" && req.Unsupported == "":
		return nil
	case req.Tier == "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability requirement has Feature/Unsupported set with no Tier", name))
	case req.Tier != CapabilityPrime && req.Tier != CapabilityExtDiscovery:
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: unknown capability tier %q", name, req.Tier))
	case req.Feature == "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability tier %q requires a detector Feature key", name, req.Tier))
	case req.Tier == CapabilityPrime && req.Unsupported != "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability tier %q does not use Unsupported", name, req.Tier))
	case req.Tier == CapabilityExtDiscovery && req.Unsupported == "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability tier %q requires Unsupported", name, req.Tier))
	default:
		return nil
	}
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
// invariant: Text, Raw, and RawBytes each claim the whole stdout; Summary
// rides only the envelope/Data path.
const payloadExclusivityRule = "Text, Raw, and RawBytes each claim the whole stdout; Summary rides only the envelope/Data path"

// tableFormat reports whether format is one of the two formats rendered as text
// tables: the default ("") and the explicit "table" value.
func tableFormat(format string) bool {
	return format == "" || format == "table"
}

// textRenderFormat reports whether format renders a ShapeText Result's Text
// field verbatim: table/default, plus "wide" — for a ShapeText command,
// -o wide is table-family (no separate wide body exists for text).
func textRenderFormat(format string) bool {
	return tableFormat(format) || format == "wide"
}

// checkResultShape verifies res's actual payload against d's declared
// OutputShape — the closed-world conformance contract every registered
// command commits to. A mismatch is a command bug, the same posture as
// payloadExclusivityRule: the command populated the wrong Result field (or
// none) for the shape its own Descriptor declares.
//
// The check is scoped to the render path the requested format actually
// takes. ShapeTabular and the RawBytes/Raw payloads (ShapeBytes,
// ShapeExecJSON) back every format identically — Tabular Data renders the
// same envelope-or-table content regardless of -o, and the RawBytes/Raw
// branches in renderResult never look at format at all — so those three
// checks apply unconditionally. ShapeText is different: Text is read only
// by the table-family render path (textRenderFormat); -o json/-o yaml
// render the envelope over Data exactly as before, and -o name/-o template
// do the same (render.go's existing "-o name over non-Tabular Data emits
// nothing" contract), so an unpopulated Text is not a shape violation
// there — checking it under those formats would reject an unimplemented
// command's Data-only rendering even though nothing about that rendering
// is actually broken.
func checkResultShape(d Descriptor, res Result, format string) *clierr.Error {
	if res.OnceOnlyNotice != "" && d.Delivery != DeliveryOnceOnly {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s is not DeliveryOnceOnly but returned OnceOnlyNotice", d.Name))
	}
	if res.StyledText != nil && res.Text == "" {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s returned StyledText but no Text", d.Name))
	}
	switch d.Shape {
	case ShapeTabular:
		if _, ok := res.Data.(output.Tabular); !ok {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("command bug: %s declares ShapeTabular but its Data does not implement output.Tabular", d.Name))
		}
	case ShapeText:
		if textRenderFormat(format) && res.Text == "" {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("command bug: %s declares ShapeText but returned no Text", d.Name))
		}
	case ShapeBytes:
		if res.RawBytes == nil {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("command bug: %s declares ShapeBytes but returned no RawBytes", d.Name))
		}
	case ShapeExecJSON:
		if !res.Raw {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("command bug: %s declares ShapeExecJSON but Result.Raw is not set", d.Name))
		}
	}
	return nil
}

// envelopeCarriesWarnings reports whether the output format serializes the
// full structured envelope to the user — code, hint, details, and the
// top-level "warnings" key alike: only json and yaml do. Every other format
// (table, name, template, "") writes a plain-text projection instead, so a
// field that lives only in the envelope would otherwise be silently dropped.
func envelopeCarriesWarnings(format string) bool {
	return format == "json" || format == "yaml"
}

// mirrorWarnings writes each entry of warnings to w as a "# warning: <w>"
// line, skipping an entry already counted in silent — a value-matched
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
		_, _ = fmt.Fprintf(w, "%s %s\n", s.Yellow("# warning:"), warn)
	}
}

// renderResult renders res per d's declared shape, having first drained
// streams' NoticeSource and merged it ahead of res.Warnings (recorded
// earliest, delivered first — the same merge order RenderError applies on
// the error path). The returned notices are the drained entries NOT YET
// irrevocably delivered by a write independent of streams.Out succeeding:
// nil when the branch taken mirrors warnings straight to the real stderr
// (Text, RawBytes, Raw, and every non-json/yaml envelope format) or when
// nothing was recorded; the drained slice itself when nothing was written
// at all (an early command-bug return) or when the only place the notices
// landed is inside the json/yaml bytes just handed to streams.Out — bytes a
// once-only caller may still discard if the real stdout write that follows
// fails. An ordinary (non-buffered) caller's streams.Out IS real stdout, so
// it has nothing left to do with a non-nil return on success; only a
// caller writing into a discardable buffer (finishOnceOnlyResult) needs to
// act on it.
func renderResult(streams IO, d Descriptor, res Result) ([]string, *clierr.Error) {
	outStyle, errStyle := stylesFor(streams)
	notices := drainNotices(streams)
	res.Warnings = append(append([]string{}, notices...), res.Warnings...)

	if ce := checkResultShape(d, res, streams.Format); ce != nil {
		return notices, ce
	}
	// Result's payload channels are mutually exclusive (payloadExclusivityRule);
	// any combination is a command bug.
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
		return notices, clierr.New(clierr.CodeGeneric, "command bug: "+payloadExclusivityRule)
	}
	// Summary rides only over a real table: the JSON-fallback-era use (a
	// Summary line papering over non-Tabular Data) is gone along with the
	// fallback itself.
	if res.Summary != "" {
		if _, ok := res.Data.(output.Tabular); !ok {
			return notices, clierr.New(clierr.CodeGeneric, "command bug: Summary requires Tabular Data")
		}
	}
	if res.Text != "" && textRenderFormat(streams.Format) {
		text := res.Text
		if res.StyledText != nil {
			text = res.StyledText(outStyle)
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
		if _, err := streams.Out.Write([]byte(text)); err != nil {
			return notices, clierr.New(clierr.CodeGeneric, err.Error())
		}
		// Mirror every Warnings entry to stderr except OnceOnlyNotice
		// (matched by value — see that field's doc for why position
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
			return notices, clierr.New(clierr.CodeGeneric, err.Error())
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
			return notices, clierr.New(clierr.CodeGeneric, err.Error())
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
	// "table" and "wide" — `get --wait`'s wide output is table-family too,
	// not a separate body, so the wait line belongs on it as well.
	if textRenderFormat(streams.Format) && res.Summary != "" {
		if _, err := fmt.Fprintln(streams.Out, res.Summary); err != nil {
			return notices, clierr.New(clierr.CodeGeneric, err.Error())
		}
	}
	if err := output.RenderStyled(streams.Out, streams.Format, env, outStyle); err != nil {
		return notices, clierr.New(clierr.CodeGeneric, err.Error())
	}
	// renderTable (output/render.go) only ever reads env.Data, so an
	// envelope-level warning would otherwise reach the user only when the
	// format embeds the full envelope (json/yaml). Mirror the
	// "# warning: <w>" stderr convention for every other format: table/default
	// (where a user looks at the rendered rows), name (whose stdout is the raw
	// name field — no envelope), and template (whose stdout is the template
	// expansion — no envelope). json/yaml carry warnings inside the envelope;
	// mirroring there would double-print.
	if !envelopeCarriesWarnings(streams.Format) {
		mirrorWarnings(streams.Err, res.Warnings, nil, errStyle)
		return nil, nil
	}
	// json/yaml: the notices are embedded only in the envelope bytes just
	// written to streams.Out, not mirrored anywhere else — report them back
	// so a caller writing into a discardable buffer can recover them if the
	// real write that follows fails.
	return notices, nil
}

// renderEffect renders a --dry-run preview. table/default/wide (wide is
// table-family here too — there is no separate wide body for a preview)
// render the pipeline-owned dryRunText body; json/yaml keep today's Effect
// envelope unchanged, as do the raw formats (name/template), which read the
// envelope's target field.
func renderEffect(streams IO, eff safety.Effect) *clierr.Error {
	_, errStyle := stylesFor(streams)
	// A dry-run Plan can still touch a lazily-resolving credential store
	// (reading the current server's CA, say); drain ahead of eff.Warnings
	// so that notice is not silently stranded in the source until whatever
	// later invocation happens to drain it.
	eff.Warnings = append(drainNotices(streams), eff.Warnings...)
	if textRenderFormat(streams.Format) {
		if _, err := streams.Out.Write([]byte(dryRunText(eff))); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
		// Effect.Warnings are deliberately excluded from dryRunText's body
		// — mirror them to stderr here so they are not silently dropped,
		// the same contract renderResult's Text branch applies to
		// Result.Warnings.
		mirrorWarnings(streams.Err, eff.Warnings, nil, errStyle)
		return nil
	}
	env := output.NewEnvelope("Effect", eff)
	env.Warnings = append(env.Warnings, eff.Warnings...)
	if err := output.Render(streams.Out, streams.Format, env); err != nil {
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
	// renderTable (render.go) encodes env.Data alone, so envelope-level
	// warnings vanish for every format that does not serialize the full
	// envelope — table, name, template. Mirror the "# warning: <w>" stderr
	// convention renderResult uses for those formats; json/yaml carry warnings
	// inside the envelope and must not double-print.
	if !envelopeCarriesWarnings(streams.Format) {
		mirrorWarnings(streams.Err, env.Warnings, nil, errStyle)
	}
	return nil
}

// dryRunText renders --dry-run's default-format preview body: the action and
// DisplayKind/DisplayName identity (a generateName preview may carry a
// trailing "*" — never the committed success identity), the target
// server/cluster when set, a change summary from the Effect's diff entries,
// and the confirmation gate line naming the flags a real run needs.
// Effect.Warnings are deliberately NOT part of this body (see renderEffect).
func dryRunText(eff safety.Effect) string {
	var lines []string
	if eff.Server != "" {
		lines = append(lines, "server: "+eff.Server)
	}
	if eff.Cluster != "" {
		lines = append(lines, "cluster: "+eff.Cluster)
	}
	for _, d := range eff.Diff {
		lines = append(lines, fmt.Sprintf("%s: %v -> %v", d.Path, d.From, d.To))
	}
	gate := "dry run — re-run with --apply"
	if eff.Destructive {
		gate += " --yes"
	}
	gate += " to execute"
	lines = append(lines, gate)
	return TextLines(fmt.Sprintf("%s %s/%s", eff.Action, eff.DisplayKind, eff.DisplayName), lines...).Text
}

// renderErr writes ce to stderr in the invocation's requested format, after
// draining streams' NoticeSource: a notice recorded earlier in this
// invocation (e.g. a lazy store's first-pin notice surfacing on the failure
// path) leads ce's own error-carried warnings — RenderError's notices
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
// renderErr.
func renderErrDrained(streams IO, ce *clierr.Error, errStyle style.Style, notices []string) *clierr.Error {
	_ = RenderErrorStyled(streams.Err, streams.Format, ce, errStyle, notices...)
	return ce
}

// RenderError writes ce to w in format: the structured envelope, byte-for-byte
// what json/yaml have always emitted, for those two formats — yaml serializes
// as YAML, not JSON dressed up as one — and a short operator-facing text block
// for every other format, including an unrecognized one (an untrusted format
// value renders as text rather than being guessed at as an envelope). This
// is the always-plain convenience form of RenderErrorStyled: every
// production render site — the pipeline and surface/cli's own pre-dispatch
// failures alike — resolves a style and calls RenderErrorStyled directly.
//
// notices and ce's own warning ride (WithWarnings) are merged in one fixed
// order — notices first (recorded earliest, e.g. a NoticeSource drained
// ahead of the failing step), then ce.Warnings() (attached by the failing
// step itself) — and placed at the envelope's top-level "warnings" field for
// json/yaml, or as "# warning:" stderr lines ahead of the error text
// otherwise. Never inside the error payload itself, so a warning cannot
// print twice.
func RenderError(w io.Writer, format string, ce *clierr.Error, notices ...string) error {
	return RenderErrorStyled(w, format, ce, style.Style{}, notices...)
}

// RenderErrorStyled is RenderError with presentation control: s styles the
// "error (CODE):" prefix red and the "hint:"/"# warning:" prefixes cyan and
// yellow respectively, applied only when format is among the styleable set
// (table/default/wide) — json, yaml, name, and template render byte-identical
// to RenderError regardless of s, the same eligibility rule RenderStyled
// enforces for the table renderer.
func RenderErrorStyled(w io.Writer, format string, ce *clierr.Error, s style.Style, notices ...string) error {
	warnings := append(append([]string{}, notices...), ce.Warnings()...)
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
		fmt.Fprintf(&b, "%s %s\n", s.Yellow("# warning:"), warn)
	}
	b.WriteString(errorText(ce, s))
	_, err := w.Write([]byte(b.String()))
	return err
}

// errorText renders ce as the default-format stderr block: a header line
// naming the code (and "retryable" when the error is retryable), an indented
// hint line when present, an indented candidates line — each candidate
// rendered "name (id)" — when present, and finally the details payload's own
// projected lines (detailsTextLines) when one is attached. s styles the
// "error (CODE):" and "hint:" prefixes only, never the message/hint body.
func errorText(ce *clierr.Error, s style.Style) string {
	retry := ""
	if ce.Retryable {
		retry = ", retryable"
	}
	prefix := fmt.Sprintf("error (%s%s):", ce.Code, retry)
	header := s.Red(prefix) + " " + ce.Message

	var lines []string
	if ce.Hint != "" {
		lines = append(lines, s.Cyan("hint:")+" "+ce.Hint)
	}
	if len(ce.Candidates) > 0 {
		names := make([]string, len(ce.Candidates))
		for i, c := range ce.Candidates {
			names[i] = fmt.Sprintf("%s (%s)", c.Name, c.ID)
		}
		lines = append(lines, "candidates: "+strings.Join(names, ", "))
	}
	lines = append(lines, detailsTextLines(ce.Details)...)
	return TextLines(header, lines...).Text
}

// detailsFallbackLine is the neutral line rendered when a details payload
// reached the renderer without going through clierr.Error.WithDetails (an
// Error literal setting Details directly, for example) — a command bug, not
// a supported way to ship an unreviewed details type.
const detailsFallbackLine = "additional detail recorded — re-run with -o json to inspect"

// detailsTextLines projects an error's Details field onto its default-format
// text lines: the payload's own TextDetails() when it implements the
// interface, detailsFallbackLine otherwise, nothing when Details is unset.
func detailsTextLines(details any) []string {
	if details == nil {
		return nil
	}
	if td, ok := details.(clierr.TextDetails); ok {
		return td.TextDetails()
	}
	return []string{detailsFallbackLine}
}

// finishResult enforces the SuccessExitCodes declaration, renders the
// result, and surfaces the declared status. An undeclared status is an
// internal error rendered through renderErr like every other pipeline
// error. For an ORDINARY command a render failure — a shape-contract
// violation (checkResultShape) or a stdout write failure — is rendered to
// stderr exactly once here, so a non-zero exit is never silent regardless
// of what already committed server-side. A Delivery: DeliveryOnceOnly
// command instead renders through finishOnceOnlyResult, which buffers the
// render and emits a committed-context error to stderr on failure — see
// that function's doc.
func finishResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	if !allowedExitStatus(d.SuccessExitCodes, res.ExitStatus) {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s returned undeclared exit status %d", d.Name, res.ExitStatus)))
	}
	if d.Delivery == DeliveryOnceOnly {
		return finishOnceOnlyResult(streams, d, res)
	}
	// streams.Out here is the invocation's real stdout (never a discardable
	// buffer — only finishOnceOnlyResult writes into one), so a nil error
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
// real stderr immediately — only the once-only stdout payload is
// buffered), then written to the real stdout in a single Write. A
// DeliveryOnceOnly executor MUST return a non-nil Result.CommittedContext
// on every successful return; its absence is a command-bug error, never a
// silent skip, since a later render failure would otherwise have nothing
// to report. When the buffered render fails, or the final single write
// fails, the failure is emitted as a structured, non-retryable error
// envelope to stderr (best-effort — its own write failure is ignored, and
// the caller still gets the correct exit code): it names the resource the
// mutation already committed, whether the final write may have exposed a
// partial value (n > 0 on the failing write), and the non-mutating
// recovery — never inviting a blind retry of a mutation that already
// happened.
func finishOnceOnlyResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	// Computed once for every error path below: the paths after the
	// renderResult call must not go through renderErr, whose own drain would
	// hit the already-emptied NoticeSource and whose styles are recomputed
	// per call — the drained notices ride each error's warnings instead.
	_, errStyle := stylesFor(streams)
	if res.CommittedContext == nil {
		return 0, renderErrDrained(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s is DeliveryOnceOnly but returned no CommittedContext", d.Name)).
			WithWarnings(drainNotices(streams)...), errStyle, nil)
	}
	buffered := streams
	var buf bytes.Buffer
	buffered.Out = &buf
	// pending is nil when renderResult already delivered every notice for
	// real (mirrored straight to the real streams.Err, unaffected by the Out
	// swap above) — non-nil only when the notices' one and only delivery so
	// far is inside buf, which the caller below might still discard.
	pending, ce := renderResult(buffered, d, res)
	if ce != nil {
		// Nothing was written anywhere (checkResultShape and the
		// exclusivity/Summary checks all return before any write): every
		// drained notice is still unaccounted for.
		return 0, renderErrDrained(streams, committedContextError(res.CommittedContext, false, ce.Message).
			WithWarnings(pending...), errStyle, nil)
	}
	n, err := streams.Out.Write(buf.Bytes())
	if err != nil {
		// buf held the only copy of any json/yaml-embedded notice; it is
		// about to be discarded, so pending rides the replacement error
		// instead. A pending nil here (the text/RawBytes/Raw/non-json-yaml
		// branches) means nothing to add — those notices already reached
		// the real stderr inside the renderResult call above, independent
		// of this failing stdout write.
		return 0, renderErrDrained(streams, committedContextError(res.CommittedContext, n > 0, err.Error()).
			WithWarnings(pending...), errStyle, nil)
	}
	return res.ExitStatus, nil
}

// committedContextError builds the shared post-commit-failure shape: a
// GENERIC, non-retryable error carrying clierr.CommittedContextDetails.
func committedContextError(cc *CommittedContext, partialOutput bool, cause string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("rendering failed after the mutation committed: %s", cause)).
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

// toClierr normalizes any error into a structured *clierr.Error, unwrapping
// %w-wrapped clierr errors so hint/candidates survive.
func toClierr(err error) *clierr.Error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return ce
	}
	return clierr.New(clierr.CodeGeneric, err.Error())
}
