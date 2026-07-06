package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
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
}

// Run executes one command through the single middleware path.
// Returns a structured error (nil on success); the caller maps it to exit code.
func Run(ctx context.Context, cmd Command, in Input, io IO) *clierr.Error {
	d := cmd.Describe()

	// Single pre-switch guard: --dry-run is meaningful only for mutating.
	if io.DryRun && d.CommandType != Mutating {
		return renderErr(io, clierr.New(clierr.CodeDryRunNotApplicable,
			fmt.Sprintf("%s is a %s command", d.Name, d.CommandType)).
			WithHint("re-run without --dry-run"))
	}

	// Schema-driven input validation (required flags present, enum membership).
	// Done here, not via urfave flag flags, so failures are structured errors.
	if ce := validateInput(d, in); ce != nil {
		return renderErr(io, ce)
	}

	p := ui.New(io.Err, io.In, io.NoInput, io.IsTTY)

	switch d.CommandType {
	case ReadOnly, Local:
		res, err := cmd.Execute(ctx, in, safety.Effect{}, p)
		if err != nil {
			return renderErr(io, toClierr(err))
		}
		return renderResult(io, res)

	case Mutating:
		eff, err := cmd.Plan(ctx, in)
		if err != nil {
			ce := toClierr(err).WithPhase("plan")
			return renderErr(io, ce)
		}
		if io.DryRun {
			return renderEffect(io, eff)
		}
		if missing := missingFlags(io, eff); len(missing) > 0 {
			return renderErr(io, clierr.New(clierr.CodeConfirmationRequired,
				"confirmation required for this mutation").
				WithHint("re-run with: "+strings.Join(missing, " ")).
				WithTier(eff.Tier))
		}
		res, err := cmd.Execute(ctx, in, eff, p)
		if err != nil {
			return renderErr(io, toClierr(err).WithPhase("execute"))
		}
		return renderResult(io, res)

	default:
		return renderErr(io, clierr.New(clierr.CodeGeneric,
			"unknown command type: "+string(d.CommandType)))
	}
}

// validateInput checks the Descriptor's Args and Flags against the parsed Input:
// required positional args must be present and non-empty; required flags must be
// non-empty; enum flags must hold a listed value. Returns a structured VALIDATION
// error (nil if valid). Commands wanting custom missing-input UX (e.g. a candidate
// list) mark the field Required:false and validate it themselves in Execute.
func validateInput(d Descriptor, in Input) *clierr.Error {
	args := in.Args()
	for i, a := range d.Args {
		if a.Required && (i >= len(args) || args[i] == "") {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf("argument <%s> is required", a.Name)).
				WithHint(fmt.Sprintf("provide <%s>", a.Name))
		}
	}
	for _, f := range d.Flags {
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

// missingFlags computes the confirmation flags absent for this Effect.
func missingFlags(io IO, eff safety.Effect) []string {
	var missing []string
	if !io.Apply {
		missing = append(missing, "--apply")
	}
	if eff.Destructive && !io.Yes {
		missing = append(missing, "--yes")
	}
	if eff.Stability == string(provider.Deprecated) && !io.AllowDeprecated {
		missing = append(missing, "--allow-deprecated")
	}
	return missing
}

func renderResult(io IO, res Result) *clierr.Error {
	// Raw results (exec-credential) bypass the envelope: kubectl parses a single
	// bare JSON object from the plugin's stdout.
	if res.Raw {
		enc := json.NewEncoder(io.Out)
		if err := enc.Encode(res.Data); err != nil {
			return clierr.New(clierr.CodeGeneric, err.Error())
		}
		return nil
	}
	env := output.NewEnvelope(res.Kind, res.Data)
	env.Warnings = res.Warnings
	env.Tier, env.Stability = res.Tier, res.Stability
	if err := output.Render(io.Out, io.Format, env); err != nil {
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
	return nil
}

func renderEffect(io IO, eff safety.Effect) *clierr.Error {
	env := output.NewEnvelope("Effect", eff)
	env.Tier, env.Stability = eff.Tier, eff.Stability
	if !eff.ConflictDetection && eff.Tier == string(provider.TierNorman) {
		env.Warnings = append(env.Warnings, "served via norman tier: no conflict detection")
	}
	if err := output.Render(io.Out, io.Format, env); err != nil {
		return clierr.New(clierr.CodeGeneric, err.Error())
	}
	return nil
}

func renderErr(io IO, ce *clierr.Error) *clierr.Error {
	errFmt := io.Format
	if errFmt == "name" || errFmt == "" {
		errFmt = "json"
	}
	_ = output.Render(io.Err, errFmt, output.ErrorEnvelope(ce))
	return ce
}

// toClierr normalizes any error into a structured *clierr.Error, unwrapping
// %w-wrapped clierr errors so hint/phase/candidates survive.
func toClierr(err error) *clierr.Error {
	var ce *clierr.Error
	if errors.As(err, &ce) {
		return ce
	}
	return clierr.New(clierr.CodeGeneric, err.Error())
}
