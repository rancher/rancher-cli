package commands

import (
	"context"

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
	return runtime.Descriptor{Name: "config get-servers", Usage: "List configured servers", CommandType: runtime.Local}
}
func (s serverLsCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(s.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	names := sortedKeys(cfg.Servers)
	list := make(serverList, 0, len(names))
	for _, n := range names {
		list = append(list, serverEntry{Current: n == cfg.CurrentServer, Name: n, URL: cfg.Servers[n].URL})
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
		Args:     []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Examples: []string{"rancher config use-server prod"},
	}
}
func (s serverSwitchCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	var name string
	if err := config.WithLock(cmdCtx, s.dir, func() error {
		cfg, err := config.Load(config.FilePath(s.dir))
		if err != nil {
			return err
		}
		if len(in.Args()) == 0 {
			cands := make([]clierr.Candidate, 0, len(cfg.Servers))
			for n := range cfg.Servers {
				cands = append(cands, clierr.Candidate{ID: n, Name: n})
			}
			return clierr.New(clierr.CodeAmbiguousSelection, "server name required").
				WithCandidates(cands).WithHint("rancher config use-server <name>")
		}
		name = in.Args()[0]
		if _, ok := cfg.Servers[name]; !ok {
			return clierr.New(clierr.CodeNotFound, "server not found: "+name)
		}
		cfg.CurrentServer = name
		return cfg.Write()
	}); err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "ServerSwitch",
		Data: map[string]string{"current": name}}, nil
}

// CompleteArgs implements runtime.ShellCompleter: `config use-server <TAB>`
// completes server names from the already-loaded config snapshot (AC-19).
func (serverSwitchCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
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
