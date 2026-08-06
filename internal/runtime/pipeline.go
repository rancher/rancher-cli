package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// Run executes one command through the single middleware path. b is the
// resolver binding for the command's op ({} for config/local commands); the
// internal Binding drives the --allow-deprecated gate.
func Run(ctx context.Context, cmd Command, in Input, streams IO, b safety.Binding) (int, *clierr.Error) {
	d := cmd.Describe()

	if ce := validateFormat(streams.Format); ce != nil {
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
	for _, f := range OutputFormats {
		if format == f {
			return nil
		}
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
			ok := false
			for _, e := range a.Enum {
				if args[i] == e {
					ok = true
					break
				}
			}
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
					ok := false
					for _, e := range f.Enum {
						if v == e {
							ok = true
							break
						}
					}
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
			ok := false
			for _, e := range f.Enum {
				if v == e {
					ok = true
					break
				}
			}
			if !ok {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--%s=%q is not one of %s", f.Name, v, strings.Join(f.Enum, "|"))).
					WithHint(fmt.Sprintf("allowed: %s", strings.Join(f.Enum, ", ")))
			}
		}
	}
	return nil
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

func renderResult(streams IO, res Result) *clierr.Error {
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
	if res.Text != "" && tableFormat(streams.Format) {
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
	if streams.Format != "json" && streams.Format != "yaml" {
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
	// Table/default rendering encodes env.Data alone (render.go's renderTable),
	// so envelope-level warnings would otherwise vanish exactly where a user
	// looks. Mirror the same "# warning: <w>" stderr convention Result
	// already documents for table mode (command.go's Text field).
	if tableFormat(streams.Format) {
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
// error; a render failure is returned unrendered (write-failure parity
// with the previous renderResult contract).
func finishResult(streams IO, d Descriptor, res Result) (int, *clierr.Error) {
	if !allowedExitStatus(d.SuccessExitCodes, res.ExitStatus) {
		return 0, renderErr(streams, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: %s returned undeclared exit status %d", d.Name, res.ExitStatus)))
	}
	if ce := renderResult(streams, res); ce != nil {
		return 0, ce
	}
	return res.ExitStatus, nil
}

// allowedExitStatus reports whether a successful result may carry status.
// An empty declaration means only 0.
func allowedExitStatus(declared []int, status int) bool {
	if status == 0 {
		return true
	}
	for _, c := range declared {
		if c == status {
			return true
		}
	}
	return false
}

// toClierr normalizes any error into a structured *clierr.Error, unwrapping
// %w-wrapped clierr errors so hint/candidates survive.
func toClierr(err error) *clierr.Error {
	var ce *clierr.Error
	if errors.As(err, &ce) {
		return ce
	}
	return clierr.New(clierr.CodeGeneric, err.Error())
}
