package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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

	// ServerIdentity, when set, resolves the server name for Effect.Server
	// stamping after a successful Plan. AdaptEngine wires the factory's
	// Identity; other adapters leave it nil.
	ServerIdentity func() (string, error)
}

// Run executes one command through the single middleware path. b is the
// resolver binding for the command's op ({} for config/local commands); the
// pipeline stamps Tier/Stability from it — Effect/Result values set by the
// command are overwritten, never trusted.
func Run(ctx context.Context, cmd Command, in Input, io IO, b safety.Binding) *clierr.Error {
	d := cmd.Describe()

	if io.DryRun && d.CommandType != Mutating {
		return renderErr(io, clierr.New(clierr.CodeDryRunNotApplicable,
			fmt.Sprintf("%s is a %s command", d.Name, d.CommandType)).
			WithHint("re-run without --dry-run"))
	}

	if ce := validateInput(d, in); ce != nil {
		return renderErr(io, ce)
	}

	switch c := cmd.(type) {
	case MutatingCommand:
		return runMutating(ctx, c, in, io, b)
	case ReadCommand:
		return runRead(ctx, c, in, io, b)
	default:
		return renderErr(io, clierr.New(clierr.CodeGeneric,
			"command implements neither ReadCommand nor MutatingCommand: "+d.Name))
	}
}

// runMutating drives the Plan → invariant checks → tier/server stamping →
// confirmation gate → Execute path for a mutating command.
func runMutating(ctx context.Context, c MutatingCommand, in Input, io IO, b safety.Binding) *clierr.Error {
	eff, exec, err := c.Plan(ctx, in)
	if err != nil {
		return renderErr(io, toClierr(err).WithPhase("plan"))
	}
	if ce := checkPlan(eff, exec); ce != nil {
		return renderErr(io, ce)
	}
	eff.Tier, eff.Stability = b.Tier, b.Stability
	if io.ServerIdentity != nil {
		server, err := io.ServerIdentity()
		if err != nil {
			return renderErr(io, toClierr(err).WithPhase("plan"))
		}
		eff.Server = server
	}
	if io.DryRun {
		return renderEffect(io, eff)
	}
	if missing := missingFlags(io, eff); len(missing) > 0 {
		return renderErr(io, clierr.New(clierr.CodeConfirmationRequired,
			"confirmation required for this mutation").
			WithHint("re-run with: "+strings.Join(missing, " ")).
			WithTier(string(eff.Tier)))
	}
	res, err := exec(ctx)
	if err != nil {
		return renderErr(io, toClierr(err).WithPhase("execute"))
	}
	res.Tier, res.Stability = string(b.Tier), string(b.Stability)
	if b.Tier == safety.TierNorman && !eff.ConflictDetection {
		res.Warnings = append(res.Warnings, "served via norman tier: no conflict detection")
	}
	return renderResult(io, res)
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

// runRead executes a read command and stamps the binding tier/stability onto
// the result.
func runRead(ctx context.Context, c ReadCommand, in Input, io IO, b safety.Binding) *clierr.Error {
	p := ui.New(io.Err, io.In, io.NoInput, io.IsTTY)
	res, err := c.Execute(ctx, in, p)
	if err != nil {
		return renderErr(io, toClierr(err))
	}
	res.Tier, res.Stability = string(b.Tier), string(b.Stability)
	return renderResult(io, res)
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
	if eff.Stability == safety.Deprecated && !io.AllowDeprecated {
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
	env.Tier, env.Stability = string(eff.Tier), string(eff.Stability)
	if !eff.ConflictDetection && eff.Tier == safety.TierNorman {
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
