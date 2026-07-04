package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type serverListData struct {
	rows [][]string
}

func (s serverListData) Columns() []string { return []string{"CURRENT", "NAME", "URL"} }
func (s serverListData) Rows() [][]string  { return s.rows }

type serverLsCmd struct{ dir string }

// NewServerLs builds `config get-servers` (local).
func NewServerLs(dir string) runtime.Command { return &serverLsCmd{dir: dir} }

func (serverLsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "config get-servers", Usage: "List configured servers", CommandType: runtime.Local}
}
func (serverLsCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (s serverLsCmd) Execute(_ context.Context, _ runtime.Input, _ safety.Effect) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(s.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows [][]string
	current := ""
	if c, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		current = c.Server
	}
	for _, n := range names {
		mark := ""
		if n == current {
			mark = "*"
		}
		rows = append(rows, []string{mark, n, cfg.Servers[n].URL})
	}
	return runtime.Result{Kind: "ServerList", Data: serverListData{rows: rows}}, nil
}

type serverSwitchCmd struct{ dir string }

// NewServerSwitch builds `config use-server <name>` (local).
func NewServerSwitch(dir string) runtime.Command { return &serverSwitchCmd{dir: dir} }

func (serverSwitchCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config use-server",
		Usage:       "Switch the current server",
		CommandType: runtime.Local,
		// Required:false — a missing name is handled here with an AMBIGUOUS_SELECTION
		// candidate list (server names), richer than the generic validateInput error.
		Args: []runtime.ArgSpec{{Name: "name", Type: "string"}},
	}
}
func (serverSwitchCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (s serverSwitchCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(s.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if len(in.Args()) == 0 {
		cands := make([]clierr.Candidate, 0, len(cfg.Servers))
		for n := range cfg.Servers {
			cands = append(cands, clierr.Candidate{ID: n, Name: n})
		}
		return runtime.Result{}, clierr.New(clierr.CodeAmbiguousSelection, "server name required").
			WithCandidates(cands).WithHint("rancher config use-server <name>")
	}
	name := in.Args()[0]
	if _, ok := cfg.Servers[name]; !ok {
		return runtime.Result{}, clierr.New(clierr.CodeNotFound, "server not found: "+name)
	}
	// Pick the context bound to this server (by Context.Server, NOT by assuming the
	// context key equals the server name — a config may be hand-edited). Map order
	// is random, so choose deterministically: prefer the current context if it
	// already targets this server, else the lexicographically first match; create
	// one if none exist.
	var matches []string
	for cn, cctx := range cfg.Contexts {
		if cctx.Server == name {
			matches = append(matches, cn)
		}
	}
	sort.Strings(matches)
	ctxName := ""
	if cur := cfg.CurrentContext; cur != "" {
		if cc, ok := cfg.Contexts[cur]; ok && cc.Server == name {
			ctxName = cur
		}
	}
	if ctxName == "" && len(matches) > 0 {
		ctxName = matches[0]
	}
	if ctxName == "" {
		ctxName = name
		cfg.Contexts[ctxName] = config.Context{Server: name}
	}
	// When several contexts target this server, the choice is not obvious — surface
	// it instead of silently emitting a switch-looking success. (Selecting a
	// specific context among them is a config edit / `config set-context`.)
	var warnings []string
	if len(matches) > 1 {
		warnings = append(warnings, fmt.Sprintf(
			"multiple contexts target server %q (%s); activated %q — run `rancher config use-context <name>` to pick a different one",
			name, strings.Join(matches, ", "), ctxName))
	}
	cfg.CurrentContext = ctxName
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "ServerSwitch", Warnings: warnings,
		Data: map[string]string{"current": name, "context": ctxName}}, nil
}
