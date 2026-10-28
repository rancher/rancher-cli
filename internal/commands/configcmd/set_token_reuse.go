package configcmd

import (
	"context"
	"fmt"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type setTokenReuseCmd struct{ dir string }

// NewSetTokenReuse builds `config set-token-reuse on|off` (local): the
// opt-in that lets auth get-token create one reusable login credential per
// server instead of a per-cluster interactive login.
func NewSetTokenReuse(dir string) runtime.Command { return &setTokenReuseCmd{dir: dir} }

func (setTokenReuseCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config set-token-reuse",
		Usage: "Let the kubectl exec plugin reuse one login credential per server (on|off)",
		Desc: "Stores the token-reuse setting in config.yaml (default off). " +
			"With it on, the first auth get-token cache miss for a server runs one interactive login, " +
			"stores the resulting unscoped login credential under a server entry named exec-login-<host> " +
			"(exec-login-<host>-<user id> for a call with --user-id, exec-login-<host>-<user id>-<auth config> with --auth-provider too) " +
			"with provenance execLogin, and creates " +
			"cluster-scoped tokens from it silently for every cluster of that server; with --user-id and no " +
			"--cluster the token is one for Rancher itself. Off restores the per-cluster interactive login without deleting " +
			"the stored credential; the output then lists every dormant plugin-created login and the " +
			"commands that remove it (rancher auth logout <entry>, then rancher config delete-server <entry>) " +
			"together with any superseded tokens it still names. auth login credentials are never affected.",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "state", Type: "string", Required: true, Enum: []string{"on", "off"}, Desc: "on or off"}},
		Examples:    []string{"rancher config set-token-reuse on", "rancher config set-token-reuse off"},
		Shape:       runtime.ShapeText,
	}
}

func (c setTokenReuseCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	on := in.Args()[0] == "on"
	var dormant []map[string]any
	var lines []string
	if err := config.WithLock(ctx, c.dir, func() error {
		cfg, err := config.Load(config.FilePath(c.dir))
		if err != nil {
			return err
		}
		cfg.TokenReuse = on
		if err := cfg.Write(); err != nil {
			return err
		}
		if !on {
			dormant, lines = dormantPluginLogins(cfg)
		}
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}
	data := map[string]any{"tokenReuse": on}
	headline := "token reuse on: the next auth get-token cache miss for a server runs one login and reuses it across that server's clusters"
	if !on {
		headline = "token reuse off: auth get-token logs in per cluster again"
		data["dormantLogins"] = dormant
	}
	res := runtime.TextLines(headline, lines...)
	res.Kind = "TokenReuseSetting"
	res.Data = data
	return res, nil
}

// dormantPluginLogins lists every plugin login entry that still holds a
// credential or superseded records: the setting stops their use but does
// not delete anything, so the operator is told where each still lives and
// how to remove it. Sorted by name.
func dormantPluginLogins(cfg *config.Config) ([]map[string]any, []string) {
	names := make([]string, 0, len(cfg.Servers))
	for n, s := range cfg.Servers {
		if s.Provenance == clierr.ProvenanceExecLogin {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	var out []map[string]any
	var lines []string
	for _, n := range names {
		s := cfg.Servers[n]
		entry := entryReport(n, s)
		out = append(out, entry)
		lines = append(lines, fmt.Sprintf("plugin-created login %s (%s) is dormant, not deleted", n, config.DisplayServerURL(s.URL)))
		if s.CLIOwned() && s.LoggedOutAt == "" {
			lines = append(lines, "  remove its credential with: rancher auth logout "+clierr.ShellQuote(n))
		}
		lines = append(lines, "  remove the entry and its records with: rancher config delete-server "+clierr.ShellQuote(n))
		lines = append(lines, recordLines(n, s, "  ")...)
	}
	return out, lines
}
