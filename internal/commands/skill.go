package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/skillinstall"
)

// skillOutcomes is the table-renderable payload for skill install/uninstall/status.
// It implements output.Tabular so the default (-o table) format produces aligned
// columns rather than falling back to indented JSON.
type skillOutcomes []skillinstall.Outcome

func (skillOutcomes) Columns() []string {
	return []string{"HARNESS", "SCOPE", "ACTION", "VERDICT", "VERSION", "DETAIL"}
}

func (s skillOutcomes) Rows() [][]string {
	rows := make([][]string, 0, len(s))
	for _, o := range s {
		detail := o.Reason
		if detail == "" {
			detail = o.Target.Dir
		}
		rows = append(rows, []string{
			o.Target.Harness,
			string(o.Target.Scope),
			o.Action,
			string(o.Verdict),
			o.CLIVersion,
			detail,
		})
	}
	return rows
}

func skillFlags() []runtime.ArgSpec {
	return []runtime.ArgSpec{
		{Name: "agent", Type: "string", Enum: skillinstall.AgentNames(), Desc: "target harness (or `all`; default all)"},
		{Name: "scope", Type: "string", Enum: []string{"user", "project"}, Desc: "install `scope` (default user)"},
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
		return nil, clierr.New(clierr.CodeGeneric, err.Error())
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
			"supported: claude, codex, copilot, agents; use --scope project to install into the current repo instead")
}

type skillInstallCmd struct{ version string }

// NewSkillInstall builds `skill install`.
func NewSkillInstall(version string) runtime.Command { return skillInstallCmd{version} }

func (skillInstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "skill install",
		Usage: "Install the rancher agents skill into coding-agent harnesses",
		Flags: append(skillFlags(),
			runtime.ArgSpec{Name: "force", Type: "bool", Desc: "replace foreign content at the target"}),
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
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, err.Error())
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
		// A changed target reports the version it now carries; a failed
		// target reports none — nothing was installed there.
		o := skillinstall.Outcome{Target: l.Target(), Action: "changed", CLIVersion: c.version}
		w, err := l.Install(c.version, force)
		warnings = append(warnings, w...)
		if err != nil {
			o.Action, o.Reason, o.CLIVersion, failed = "failed", err.Error(), "", true
		}
		outcomes = append(outcomes, o)
	}
	if failed {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric,
			"skill install changed only part of the selected targets").
			WithDetails(skillinstall.Report{Outcomes: outcomes}).
			WithWarnings(warnings...)
	}
	return runtime.Result{Kind: "skillOutcomes", Data: skillOutcomes(outcomes), Warnings: warnings}, nil
}

type skillUninstallCmd struct{ version string }

// NewSkillUninstall builds `skill uninstall`.
func NewSkillUninstall(version string) runtime.Command { return skillUninstallCmd{version} }

func (skillUninstallCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "skill uninstall",
		Usage:       "Remove rancher-installed agents skills",
		Flags:       skillFlags(),
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
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, err.Error())
	}
	leases, err := skillinstall.Acquire(lockDir, targets, "uninstall", false)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, err.Error()+"; no target was changed")
	}
	defer skillinstall.ReleaseAll(leases)

	outcomes := make([]skillinstall.Outcome, 0, len(leases))
	failed := false
	for _, l := range leases {
		o := skillinstall.Outcome{Target: l.Target()}
		removed, err := l.Uninstall()
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
			WithDetails(skillinstall.Report{Outcomes: outcomes})
	}
	return runtime.Result{Kind: "skillOutcomes", Data: skillOutcomes(outcomes)}, nil
}

type skillStatusCmd struct{ version string }

// NewSkillStatus builds `skill status`. Success may exit 7: selected
// targets not in the requested state (skew/modified/foreign, or absent
// under --require-installed).
func NewSkillStatus(version string) runtime.Command { return skillStatusCmd{version} }

func (skillStatusCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "skill status",
		Usage: "Report installed skill versions and integrity per harness",
		Flags: append(skillFlags(),
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
		res.Data = skillOutcomes{}
		res.Warnings = []string{"no coding-agent harness detected; nothing to report"}
		return res, nil
	}
	rows := make(skillOutcomes, 0, len(targets))
	exit := 0
	for _, t := range targets {
		v, installed, err := skillinstall.StatusTarget(t, c.version)
		if err != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("checking %s: %v", t.Dir, err))
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
