package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// serverEntry is one server in the ServerList payload. Fields are exported so the
// -o json / -o yaml envelope carries real data; the slice also implements
// output.Tabular for -o table / -o name.
type serverEntry struct {
	Current bool   `json:"current"`
	Name    string `json:"name"`
	URL     string `json:"url"`
}

type serverList []serverEntry

func (serverList) Columns() []string { return []string{"CURRENT", "NAME", "URL"} }
func (s serverList) Rows() [][]string {
	rows := make([][]string, 0, len(s))
	for _, e := range s {
		mark := ""
		if e.Current {
			mark = "*"
		}
		rows = append(rows, []string{mark, e.Name, e.URL})
	}
	return rows
}

type serverLsCmd struct{ dir string }

// NewServerLs builds `config get-servers` (local).
func NewServerLs(dir string) runtime.Command { return &serverLsCmd{dir: dir} }

func (serverLsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "config get-servers", Usage: "List configured servers", CommandType: runtime.Local, Toolset: "core"}
}
func (s serverLsCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(s.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	current := ""
	if c, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		current = c.Server
	}
	list := make(serverList, 0, len(names))
	for _, n := range names {
		list = append(list, serverEntry{Current: n == current, Name: n, URL: cfg.Servers[n].URL})
	}
	return runtime.Result{Kind: "ServerList", Data: list}, nil
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
		Args:    []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Toolset: "core",
	}
}
func (s serverSwitchCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
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
	ctxName, matches := resolveContextForServer(cfg, name)
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

// CompleteArgs implements runtime.ShellCompleter: `config use-server <TAB>`
// completes server names. dir is the resolved config dir the TAB-time
// callback passes in — never s.dir, which is empty on the zero-deps probe
// instance adaptEntry type-asserts at wiring time.
func (serverSwitchCmd) CompleteArgs(_ context.Context, dir string) []string {
	return serverNames(dir)
}

// serverNames returns the configured server names, sorted
// lexicographically — Go map iteration order is not stable, and completion
// candidate order is pinned (external review finding 2). Shared by
// `config use-server` and `config delete-server`.
func serverNames(dir string) []string {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// serverNotFoundWithCandidates builds the NOT_FOUND error `config
// delete-server` and `auth logout` both return for an unknown server name:
// every currently configured server offered back as a selectable candidate
// (finding 6 — this was byte-for-byte duplicated between the two commands).
// `config use-server`'s own not-found path stays a plain NOT_FOUND with no
// candidates (deliberately out of scope for this consolidation — ledgered
// as a follow-up, not implemented here).
func serverNotFoundWithCandidates(cfg *config.Config, name string) *clierr.Error {
	cands := make([]clierr.Candidate, 0, len(cfg.Servers))
	for s := range cfg.Servers {
		cands = append(cands, clierr.Candidate{ID: s, Name: s})
	}
	return clierr.New(clierr.CodeNotFound, "server not found: "+name).
		WithCandidates(cands)
}

// resolveContextForServer picks the context to activate for server name: the
// current context if it already targets the server, else the
// lexicographically first match, else "" (caller creates one). matches is the
// full sorted match list (for the multiple-contexts warning). Contexts are
// matched by Context.Server, NOT by assuming the context key equals the
// server name — a config may be hand-edited.
func resolveContextForServer(cfg *config.Config, name string) (ctxName string, matches []string) {
	for cn, cctx := range cfg.Contexts {
		if cctx.Server == name {
			matches = append(matches, cn)
		}
	}
	sort.Strings(matches)
	if cur := cfg.CurrentContext; cur != "" {
		if cc, ok := cfg.Contexts[cur]; ok && cc.Server == name {
			return cur, matches
		}
	}
	if len(matches) > 0 {
		return matches[0], matches
	}
	return "", matches
}
