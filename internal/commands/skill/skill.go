package skill

import (
	"context"
	"fmt"
	"os"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/skillinstall"
)

// The table-renderable payloads of the skill commands. Each implements
// output.Tabular so the default (-o table) format produces aligned columns
// rather than falling back to indented JSON, and each shows only the columns
// its command fills. The JSON and YAML output of all three is the same list of
// skillinstall.Outcome.
type (
	// skillOutcomes is the payload of skill install.
	skillOutcomes []skillinstall.Outcome
	// skillUninstallOutcomes is the payload of skill uninstall.
	skillUninstallOutcomes []skillinstall.Outcome
	// skillStatusOutcomes is the payload of skill status.
	skillStatusOutcomes []skillinstall.Outcome
)

var (
	installColumns   = []string{"HARNESS", "SCOPE", "ACTION", "VERDICT", "VERSION", "DETAIL"}
	uninstallColumns = []string{"HARNESS", "SCOPE", "ACTION", "DETAIL"}
	statusColumns    = []string{"HARNESS", "SCOPE", "VERDICT", "VERSION", "DETAIL"}
)

func (skillOutcomes) Columns() []string          { return installColumns }
func (skillUninstallOutcomes) Columns() []string { return uninstallColumns }
func (skillStatusOutcomes) Columns() []string    { return statusColumns }

func (s skillOutcomes) Rows() [][]string          { return outcomeRows(s, installColumns) }
func (s skillUninstallOutcomes) Rows() [][]string { return outcomeRows(s, uninstallColumns) }
func (s skillStatusOutcomes) Rows() [][]string    { return outcomeRows(s, statusColumns) }

// outcomeRows builds one row per outcome with the cell of each named column.
func outcomeRows(outcomes []skillinstall.Outcome, columns []string) [][]string {
	rows := make([][]string, 0, len(outcomes))
	for _, o := range outcomes {
		row := make([]string, 0, len(columns))
		for _, c := range columns {
			switch c {
			case "HARNESS":
				row = append(row, o.Target.Harness)
			case "SCOPE":
				row = append(row, string(o.Target.Scope))
			case "ACTION":
				row = append(row, o.Action)
			case "VERDICT":
				row = append(row, string(o.Verdict))
			case "VERSION":
				row = append(row, o.CLIVersion)
			case "DETAIL":
				detail := o.Reason
				if detail == "" {
					detail = o.Target.Dir
				}
				row = append(row, detail)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// skillFlags returns the flags shared by the skill commands; scopeDesc is the
// help text of --scope, which differs per command.
func skillFlags(scopeDesc string) []runtime.ArgSpec {
	return []runtime.ArgSpec{
		{Name: "agent", Type: "string", Enum: skillinstall.AgentNames(), Desc: "target harness (or `all`; default all)"},
		{Name: "scope", Type: "string", Enum: []string{"user", "project"}, Desc: scopeDesc},
	}
}

func skillSelection(in runtime.Input) (agent string, scope skillinstall.Scope, cwd string, err error) {
	agent = in.String("agent")
	if agent == "" {
		agent = "all"
	}
	scope = skillinstall.Scope(in.String("scope"))
	if scope == "" {
		scope = skillinstall.ScopeUser
	}
	if scope == skillinstall.ScopeProject {
		cwd, err = os.Getwd()
	}
	return agent, scope, cwd, err
}

func skillTargets(in runtime.Input) ([]skillinstall.Target, error) {
	agent, scope, cwd, err := skillSelection(in)
	if err != nil {
		return nil, clierr.From(err)
	}
	targets, err := skillinstall.Targets(agent, scope, cwd)
	if err != nil {
		return nil, clierr.New(clierr.CodeValidation, err.Error())
	}
	return targets, nil
}

func nothingDetectedErr() *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		"no coding-agent harness detected in the home directory; "+
			"supported: claude, codex, copilot, agents; use --scope project to use the current repository instead")
}

// statusTarget is the check that gives an install row its verdict; tests
// replace it to reach the error path.
var statusTarget = skillinstall.StatusTarget

type skillInstallCmd struct{ version string }

// NewSkillInstall builds `skill install`.
func NewInstall(version string) runtime.Command { return skillInstallCmd{version} }

func (skillInstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "skill install",
		Usage: "Install the rancher-cli agent skill into coding-agent harnesses",
		Flags: append(skillFlags("install location `scope`: user or project (default user)"),
			runtime.ArgSpec{Name: "force", Type: "bool", Desc: "replace content at the target that rancher-cli did not install"}),
		Examples: []string{
			"rancher skill install",
			"rancher skill install --agent claude --scope project",
		},
		CommandType: runtime.Local,
		Shape:       runtime.ShapeTabular,
	}
}

func (c skillInstallCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	targets, err := skillTargets(in)
	if err != nil {
		return runtime.Result{}, err
	}
	if len(targets) == 0 {
		return runtime.Result{}, nothingDetectedErr()
	}
	force := in.Bool("force")
	lockDir, err := skillinstall.DefaultLockDir()
	if err != nil {
		return runtime.Result{}, clierr.From(err)
	}
	leases, err := skillinstall.Acquire(lockDir, targets, "install", force)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, err.Error()+"; no target was changed")
	}
	defer skillinstall.ReleaseAll(leases)

	var warnings []string
	outcomes := make([]skillinstall.Outcome, 0, len(leases))
	failed := false
	for _, l := range leases {
		// A changed or unchanged target reports the version it carries; a
		// failed target reports none - nothing was installed there. The
		// verdict is the one skill status reports for the same target.
		o := skillinstall.Outcome{Target: l.Target(), Action: "unchanged", CLIVersion: c.version}
		changed, w, err := l.InstallIfChanged(c.version, force)
		warnings = append(warnings, w...)
		switch {
		case err != nil:
			o.Action, o.Reason, o.CLIVersion, failed = "failed", err.Error(), "", true
		default:
			if changed {
				o.Action = "changed"
			}
			v, _, statusErr := statusTarget(o.Target, c.version)
			if statusErr != nil {
				warnings = append(warnings, fmt.Sprintf("could not check %s: %v", o.Target.Dir, statusErr))
			} else {
				o.Verdict = v
			}
		}
		outcomes = append(outcomes, o)
	}
	if failed {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric,
			"skill install failed for part of the selected targets").
			WithDetails(skillinstall.Report{Outcomes: outcomes}).
			WithWarnings(warnings...)
	}
	return runtime.Result{Kind: "skillOutcomes", Data: skillOutcomes(outcomes), Warnings: warnings}, nil
}

type skillUninstallCmd struct{ version string }

// NewSkillUninstall builds `skill uninstall`.
func NewUninstall(version string) runtime.Command { return skillUninstallCmd{version} }

func (skillUninstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "skill uninstall",
		Usage:       "Remove the agent skills that rancher-cli installed",
		Flags:       skillFlags("install location `scope` to remove from: user or project (default user)"),
		Examples:    []string{"rancher skill uninstall --agent claude"},
		CommandType: runtime.Local,
		Shape:       runtime.ShapeTabular,
	}
}

func (c skillUninstallCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	targets, err := skillTargets(in)
	if err != nil {
		return runtime.Result{}, err
	}
	if len(targets) == 0 {
		return runtime.Result{}, nothingDetectedErr()
	}
	lockDir, err := skillinstall.DefaultLockDir()
	if err != nil {
		return runtime.Result{}, clierr.From(err)
	}
	leases, err := skillinstall.Acquire(lockDir, targets, "uninstall", false)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, err.Error()+"; no target was changed")
	}
	defer skillinstall.ReleaseAll(leases)

	var warnings []string
	outcomes := make([]skillinstall.Outcome, 0, len(leases))
	failed := false
	for _, l := range leases {
		o := skillinstall.Outcome{Target: l.Target()}
		removed, w, err := l.Uninstall()
		warnings = append(warnings, w...)
		switch {
		case err != nil:
			o.Action, o.Reason, failed = "failed", err.Error(), true
		case removed:
			o.Action = "changed"
		default:
			o.Action, o.Reason = "skipped", "not installed"
		}
		outcomes = append(outcomes, o)
	}
	if failed {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric,
			"skill uninstall changed only part of the selected targets").
			WithDetails(skillinstall.Report{Outcomes: outcomes}).
			WithWarnings(warnings...)
	}
	return runtime.Result{Kind: "skillOutcomes", Data: skillUninstallOutcomes(outcomes), Warnings: warnings}, nil
}

type skillStatusCmd struct{ version string }

// NewSkillStatus builds `skill status`. Success may exit 7: selected
// targets not in the requested state (skew/modified/foreign, or absent
// under --require-installed).
func NewStatus(version string) runtime.Command { return skillStatusCmd{version} }

func (skillStatusCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "skill status",
		Usage: "Report, for each harness, the installed skill version and whether its files are unchanged",
		Flags: append(skillFlags("install location `scope` to check: user or project (default user)"),
			runtime.ArgSpec{Name: "require-installed", Type: "bool", Desc: "fail when a selected target is absent"}),
		Examples: []string{
			"rancher skill status",
			"rancher skill status --agent claude --require-installed",
		},
		CommandType:      runtime.Local,
		SuccessExitCodes: []int{0, 7},
		Shape:            runtime.ShapeTabular,
	}
}

func (c skillStatusCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	targets, err := skillTargets(in)
	if err != nil {
		return runtime.Result{}, err
	}
	res := runtime.Result{Kind: "skillStatus"}
	if len(targets) == 0 {
		res.Data = skillStatusOutcomes{}
		res.Warnings = []string{"no coding-agent harness detected; nothing to report"}
		return res, nil
	}
	rows := make(skillStatusOutcomes, 0, len(targets))
	exit := 0
	for _, t := range targets {
		v, installed, err := skillinstall.StatusTarget(t, c.version)
		if err != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("could not check %s: %v", t.Dir, err))
		}
		rows = append(rows, skillinstall.Outcome{Target: t, Verdict: v, CLIVersion: installed})
		switch v {
		case skillinstall.VerdictSkew, skillinstall.VerdictModified, skillinstall.VerdictForeign:
			exit = 7
		case skillinstall.VerdictAbsent:
			if in.Bool("require-installed") {
				exit = 7
			}
		}
	}
	res.Data = rows
	res.ExitStatus = exit
	return res, nil
}
