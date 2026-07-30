// Package skillinstall installs the embedded rancher agents skill into
// coding-agent harness skill directories, with provenance and integrity
// checks. See docs/design/2026-07-25-skill-install-design.md.
package skillinstall

import (
	"fmt"
	"os"
	"path/filepath"
)

type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// Harness is one reviewed row of the target map. Adding a harness is adding
// a row (the kinds-table philosophy); verbs iterate rows generically.
type Harness struct {
	Name       string
	UserDir    func() (string, error)
	ProjectDir func(cwd string) string
	Detect     func() bool
	// AliasOf collapses this row's PROJECT scope into another row's dir —
	// copilot reads the shared .agents/skills in-repo.
	AliasOf string
}

// Target is one resolved installation directory. The json tags are load-
// bearing: Outcome rows render into the envelope and the e2e scenario
// asserts data.0.target.harness.
type Target struct {
	Harness string `json:"harness"`
	Scope   Scope  `json:"scope"`
	Dir     string `json:"dir"`
}

func homeSub(parts ...string) func() (string, error) {
	return func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(append([]string{home}, parts...)...), nil
	}
}

// envOrHomeSub returns the resolved path: if env is set, joins it with parts[1:]
// (the shared tail); else delegates to homeSub(parts). parts[0] is the home-
// relative harness dir that the env value replaces; the rest of the tail is shared.
func envOrHomeSub(env string, parts ...string) func() (string, error) {
	return func() (string, error) {
		if v := os.Getenv(env); v != "" {
			return filepath.Join(append([]string{v}, parts[1:]...)...), nil
		}
		return homeSub(parts...)()
	}
}

// harnessRootDepth is how many path segments every harness row's UserDir
// sits below its detection root (e.g. ~/.claude/skills/rancher → ~/.claude).
// A future harness whose layout differs needs its own probe, not this one.
const harnessRootDepth = 2

func dirExists(f func() (string, error)) func() bool {
	return func() bool {
		p, err := f()
		if err != nil {
			return false
		}
		for i := 0; i < harnessRootDepth; i++ {
			p = filepath.Dir(p)
		}
		info, err := os.Stat(p)
		return err == nil && info.IsDir()
	}
}

// Harnesses returns the reviewed harness rows in reporting order.
func Harnesses() []Harness {
	claudeUser := homeSub(".claude", "skills", "rancher")
	codexUser := envOrHomeSub("CODEX_HOME", ".codex", "skills", "rancher")
	copilotUser := envOrHomeSub("COPILOT_HOME", ".copilot", "skills", "rancher")
	agentsUser := homeSub(".agents", "skills", "rancher")
	return []Harness{
		{
			Name: "claude", UserDir: claudeUser,
			ProjectDir: func(cwd string) string { return filepath.Join(cwd, ".claude", "skills", "rancher") },
			Detect:     dirExists(claudeUser),
		},
		{
			Name: "codex", UserDir: codexUser,
			ProjectDir: func(cwd string) string { return filepath.Join(cwd, ".codex", "skills", "rancher") },
			Detect:     dirExists(codexUser),
		},
		{
			Name: "copilot", UserDir: copilotUser,
			ProjectDir: func(cwd string) string { return filepath.Join(cwd, ".agents", "skills", "rancher") },
			Detect:     dirExists(copilotUser),
			AliasOf:    "agents",
		},
		{
			Name: "agents", UserDir: agentsUser,
			ProjectDir: func(cwd string) string { return filepath.Join(cwd, ".agents", "skills", "rancher") },
			Detect:     dirExists(agentsUser),
		},
	}
}

// AgentNames is the --agent completion enum: table names plus "all".
func AgentNames() []string {
	names := make([]string, 0, 5)
	for _, h := range Harnesses() {
		names = append(names, h.Name)
	}
	return append(names, "all")
}

// Targets resolves the selected agent+scope to deduplicated final dirs.
// User scope + "all" selects detected harnesses only. Project scope
// collapses aliased rows (copilot → agents) so a shared dir appears once.
// Deduplication uses canonicalTarget so two textual paths to the same
// physical directory (e.g. CODEX_HOME set to a symlink of ~/.claude) produce
// only one target. Table order determines the winner: the first row
// (claude, codex, copilot, agents) that claims a canonical identity keeps
// its harness name; later aliases are skipped.
func Targets(agent string, scope Scope, cwd string) ([]Target, error) {
	rows := Harnesses()
	if agent != "all" {
		found := false
		for _, h := range rows {
			if h.Name == agent {
				rows, found = []Harness{h}, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown agent %q (valid: %v)", agent, AgentNames())
		}
	}
	var out []Target
	seen := map[string]bool{}
	for _, h := range rows {
		name := h.Name
		var dir string
		switch scope {
		case ScopeProject:
			if h.AliasOf != "" {
				name = h.AliasOf
			}
			dir = h.ProjectDir(cwd)
		case ScopeUser:
			if agent == "all" && !h.Detect() {
				continue
			}
			d, err := h.UserDir()
			if err != nil {
				return nil, err
			}
			dir = d
		default:
			return nil, fmt.Errorf("unknown scope %q", scope)
		}
		if seen[canonicalTarget(dir)] {
			continue
		}
		seen[canonicalTarget(dir)] = true
		out = append(out, Target{Harness: name, Scope: scope, Dir: dir})
	}
	return out, nil
}
