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
}

// Run executes one command through the single middleware path. b is the
// resolver binding for the command's op ({} for config/local commands); the
// internal Binding drives the --allow-deprecated gate.
func Run(ctx context.Context, cmd Command, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	d := cmd.Describe()

	if ce := validateFormat(streams.Format); ce != nil {
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
			fmt.Sprintf("read-only mode: %s is a mutating command", d.Name)).
			WithHint("enabled by "+strings.Join(streams.ReadOnlySources, ", ")))
	}

	if streams.DryRun && d.CommandType != Mutating {
		return 0, renderErr(streams, clierr.New(clierr.CodeDryRunNotApplicable,
			fmt.Sprintf("%s is a %s command", d.Name, d.CommandType)).
			WithHint("re-run without --dry-run"))
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
			"confirmation required for this mutation").
			WithHint("re-run with: "+strings.Join(missing, " ")))
	}
	res, err := exec(ctx)
	if err != nil {
		return 0, renderErr(streams, toClierr(err))
	}
	res.Warnings = append(append([]string{}, eff.Warnings...), res.Warnings...)
	return finishResult(streams, c.Describe(), res)
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
			return clierr.New(clierr.CodeValidation, "--output template source is empty").
				WithHint("pass -o template='{{ .field }}'")
		}
		if _, err := template.New("out").Parse(src); err != nil {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf("invalid template: %v", err)).
				WithHint("template must be valid Go text/template syntax")
		}
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--output=%q is not one of %s|template=<go-template>", format, strings.Join(OutputFormats, "|"))).
		WithHint(fmt.Sprintf("allowed: %s, template=<go-template>", strings.Join(OutputFormats, ", ")))
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
		fmt.Sprintf("%s does not support --output=%s", d.Name, shown)).
		WithHint(fmt.Sprintf("allowed: %s", strings.Join(d.OutputPolicy.AllowedFormats, ", ")))
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
				fmt.Sprintf("argument <%s> is required", a.Name)).
				WithHint(fmt.Sprintf("provide <%s>", a.Name))
		}
		if len(a.Enum) > 0 && i < len(args) && args[i] != "" {
			ok := slices.Contains(a.Enum, args[i])
			if !ok {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("argument <%s>=%q is not one of %s", a.Name, args[i], strings.Join(a.Enum, "|"))).
					WithHint(fmt.Sprintf("allowed: %s", strings.Join(a.Enum, ", ")))
			}
		}
	}
	for _, f := range d.Flags {
		if f.Type == "stringSlice" {
			entries := in.Strings(f.Name)
			if f.Required && len(entries) == 0 {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--%s is required", f.Name)).
					WithHint(fmt.Sprintf("pass --%s <value>", f.Name))
			}
			if len(f.Enum) > 0 {
				for _, v := range entries {
					if v == "" {
						continue
					}
					ok := slices.Contains(f.Enum, v)
					if !ok {
						return clierr.New(clierr.CodeValidation,
							fmt.Sprintf("--%s=%q is not one of %s", f.Name, v, strings.Join(f.Enum, "|"))).
							WithHint(fmt.Sprintf("allowed: %s", strings.Join(f.Enum, ", ")))
					}
				}
			}
			continue
		}
		v := in.String(f.Name)
		if f.Required && v == "" {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s is required", f.Name)).
				WithHint(fmt.Sprintf("pass --%s <value>", f.Name))
		}
		if len(f.Enum) > 0 && v != "" {
			ok := slices.Contains(f.Enum, v)
			if !ok {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--%s=%q is not one of %s", f.Name, v, strings.Join(f.Enum, "|"))).
					WithHint(fmt.Sprintf("allowed: %s", strings.Join(f.Enum, ", ")))
			}
		}
	}
	return nil
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
			fmt.Sprintf("%s declares a capability requirement but no capability checker is wired", d.Name)).
			WithHint("register the command through the standard server-command path")
	}
	dec := streams.Capability.Check(ctx, req)
	switch dec.Outcome {
	case CapabilityDisabled:
		if dec.EditionPrime {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s is disabled", req.Feature)).
				WithHint(fmt.Sprintf("enable feature %s", req.Feature))
		}
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s is disabled on this server", req.Feature)).
			WithHint("availability is decided by the connected server — " + clierr.LearnMorePrimeURL)
	case CapabilityPrimeRequired:
		return clierr.New(clierr.CodePrimeRequired,
			fmt.Sprintf("%s requires Rancher Prime; the connected server does not have Rancher Prime", d.Name)).
			WithHint("this capability requires Rancher Prime — " + clierr.LearnMorePrimeURL).
			WithDetails(clierr.PrimeRequiredDetails{Feature: req.Feature, Server: dec.Server})
	default: // CapabilityProceed, CapabilityUnknown
		return nil
	}
}

// ValidCapabilityShape reports the malformation in req as a CodeGeneric
// error, or nil when req is valid: the complete zero value, or a known Tier
// with a non-empty Feature. It does not hide behind IsSet (Tier != "") — a
// half-set value ({Feature set, Tier ""}) would otherwise read as absent and
// skip every gate that keys on IsSet, including the coverage rule's
// Capability-bearing filter. Shared by Registry.Validate, kinds'
// validateKinds, and both pipeline boundaries in Run, so a malformed value
// is rejected identically wherever it can appear. name identifies the owning
// command or kind in the message.
func ValidCapabilityShape(req CapabilityRequirement, name string) *clierr.Error {
	switch {
	case req.Tier == "" && req.Feature == "":
		return nil
	case req.Tier == "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability requirement has Feature %q with no Tier", name, req.Feature))
	case req.Tier != CapabilityPrime:
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: unknown capability tier %q", name, req.Tier))
	case req.Feature == "":
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: capability tier %q requires a detector Feature key", name, req.Tier))
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

// envelopeCarriesWarnings reports whether the output format serializes the full
// stdout envelope — and therefore the top-level "warnings" key — to the user:
// only json and yaml do. Every other format (table, name, template, "") writes
// the data portion only, so warnings that land only in the envelope would
// otherwise be silently dropped.
func envelopeCarriesWarnings(format string) bool {
	return format == "json" || format == "yaml"
}

func renderResult(streams IO, d Descriptor, res Result) *clierr.Error {
	if ce := checkResultShape(d, res, streams.Format); ce != nil {
		return ce
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
		return clierr.New(clierr.CodeGeneric, "command bug: "+payloadExclusivityRule)
	}
	if res.Text != "" && textRenderFormat(streams.Format) {
		if _, err := streams.Out.Write([]byte(res.Text)); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
		for _, w := range res.Warnings {
			_, _ = fmt.Fprintf(streams.Err, "# warning: %s\n", w)
		}
		return nil
	}
	if res.RawBytes != nil {
		if _, err := streams.Out.Write(res.RawBytes); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
		return nil
	}
	// Raw results (exec-credential) bypass the envelope: kubectl parses a single
	// bare JSON object from the plugin's stdout.
	if res.Raw {
		enc := json.NewEncoder(streams.Out)
		if err := enc.Encode(res.Data); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
		return nil
	}
	env := output.NewEnvelope(res.Kind, res.Data)
	env.Warnings = res.Warnings
	env.Summary = res.Summary
	if tableFormat(streams.Format) && res.Summary != "" {
		if _, err := fmt.Fprintln(streams.Out, res.Summary); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
	}
	if err := output.Render(streams.Out, streams.Format, env); err != nil {
		return clierr.New(clierr.CodeGeneric, err.Error())
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
		for _, w := range res.Warnings {
			_, _ = fmt.Fprintf(streams.Err, "# warning: %s\n", w)
		}
	}
	return nil
}

func renderEffect(streams IO, eff safety.Effect) *clierr.Error {
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
		for _, w := range env.Warnings {
			_, _ = fmt.Fprintf(streams.Err, "# warning: %s\n", w)
		}
	}
	return nil
}

// renderErr writes the structured error envelope to stderr. Errors are ALWAYS
// rendered as JSON regardless of -o: the output format governs stdout data
// only, so scripts and agents never lose the machine-readable code/hint (CW-1).
func renderErr(streams IO, ce *clierr.Error) *clierr.Error {
	_ = output.Render(streams.Err, "json", output.ErrorEnvelope(ce))
	return ce
}

// finishResult enforces the SuccessExitCodes declaration, renders the
// result, and surfaces the declared status. An undeclared status is an
// internal error rendered through renderErr like every other pipeline
// error. For an ORDINARY command a render failure is returned unrendered
// (write-failure parity with the previous renderResult contract); a
// Delivery: DeliveryOnceOnly command instead renders through
// finishOnceOnlyResult, which buffers the render and emits a committed-
// context error to stderr on failure — see that function's doc.
func finishResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	if !allowedExitStatus(d.SuccessExitCodes, res.ExitStatus) {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s returned undeclared exit status %d", d.Name, res.ExitStatus)))
	}
	if d.Delivery == DeliveryOnceOnly {
		return finishOnceOnlyResult(streams, d, res)
	}
	if ce := renderResult(streams, d, res); ce != nil {
		return 0, ce
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
	if res.CommittedContext == nil {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s is DeliveryOnceOnly but returned no CommittedContext", d.Name)))
	}
	buffered := streams
	var buf bytes.Buffer
	buffered.Out = &buf
	if ce := renderResult(buffered, d, res); ce != nil {
		return 0, renderErr(streams, committedContextError(res.CommittedContext, false, ce.Message))
	}
	n, err := streams.Out.Write(buf.Bytes())
	if err != nil {
		return 0, renderErr(streams, committedContextError(res.CommittedContext, n > 0, err.Error()))
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
