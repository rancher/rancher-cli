package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/sirupsen/logrus"
	ucli "github.com/urfave/cli/v3"
)

// entry is one command's declarative registration: its verb group and exactly
// one constructor — engine (needs a cluster client factory), engineSrc (also
// needs a FileSource for -f input), or local (needs the config dir/store). The
// registration loop probes, validates, registers, and wires each entry through
// the adapter matching the constructor kind.
type entry struct {
	group     string
	engine    func(engine.ClusterClientFactory) runtime.Command
	engineSrc func(engine.ClusterClientFactory, engine.FileSource) runtime.Command
	local     func(dir string, s config.CredStore) runtime.Command
}

// buildEntries is the single source of command wiring: the bespoke config/token
// and create commands, plus generated get/delete commands for every kind in the
// table. Adding a command is one row here (plus a kind-table row for a new
// kind).
func buildEntries() []entry {
	entries := []entry{
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return commands.NewLogin(d, s) }},
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return commands.NewLogout(d, s) }},
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return commands.NewAuthCheck(d, s) }},
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return token.NewTokenCommand(d, s) }},
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return token.NewListTokensCommand(d, s) }},
		{group: "auth", local: func(d string, s config.CredStore) runtime.Command { return token.NewDeleteTokenCommand(d, s) }},
		{group: "config", local: func(d string, _ config.CredStore) runtime.Command { return commands.NewContextCurrent(d) }},
		{group: "config", local: func(d string, _ config.CredStore) runtime.Command { return commands.NewContextSet(d) }},
		{group: "config", local: func(d string, _ config.CredStore) runtime.Command { return commands.NewContextUse(d) }},
		{group: "config", local: func(d string, _ config.CredStore) runtime.Command { return commands.NewServerLs(d) }},
		{group: "config", local: func(d string, _ config.CredStore) runtime.Command { return commands.NewServerSwitch(d) }},
		{group: "config", local: func(_ string, _ config.CredStore) runtime.Command { return commands.NewConfigPurgeLegacy() }},
		{group: "create", engine: func(f engine.ClusterClientFactory) runtime.Command { return projects.NewCreate(f) }},
		{group: "create", engineSrc: func(f engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
			return resources.NewCreate(f, src)
		}},
	}
	for _, k := range kinds.All() {
		k := k
		entries = append(entries, entry{group: "get", engine: func(f engine.ClusterClientFactory) runtime.Command {
			return resources.NewGet(k, f)
		}})
		entries = append(entries, entry{group: "patch", engineSrc: func(f engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
			return resources.NewPatch(k, f, src)
		}})
		if k.Deletable {
			entries = append(entries, entry{group: "delete", engine: func(f engine.ClusterClientFactory) runtime.Command {
				return resources.NewDelete(k, f)
			}})
		}
	}
	return entries
}

// wireEntry validates one entry and returns its urfave leaf. It probes
// Describe() with nil deps (Describe must be dir/store/client-independent),
// checks the name's group matches the declared group, requires every engine
// command — read or mutating — to resolve to a binding, registers the probe,
// and wires it through the adapter for its constructor kind.
func wireEntry(reg *runtime.Registry, resolver runtime.Resolver, e entry) (*ucli.Command, error) {
	var probe runtime.Command
	switch {
	case e.engine != nil:
		probe = e.engine(nil)
	case e.engineSrc != nil:
		probe = e.engineSrc(nil, nil)
	case e.local != nil:
		probe = e.local("", nil)
	default:
		return nil, fmt.Errorf("entry in group %q sets none of engine, engineSrc, or local", e.group)
	}
	d := probe.Describe()

	// The tree is two levels deep: a command name is exactly one token (a
	// top-level command) or two (group + leaf). Reject 3+ tokens with a clear
	// message rather than a misleading group mismatch.
	if n := strings.Count(d.Name, " "); n > 1 {
		return nil, fmt.Errorf("command name %q must be one or two space-separated tokens, has %d", d.Name, n+1)
	}
	if g := groupName(d.Name); g != e.group {
		return nil, fmt.Errorf("command %q is wired under group %q but its name implies %q", d.Name, e.group, g)
	}
	if e.engine != nil || e.engineSrc != nil {
		if _, ok := resolver.Resolve(d.Name); !ok {
			return nil, fmt.Errorf("engine command %q has no resolver binding", d.Name)
		}
	}

	reg.Register(probe)

	switch {
	case e.engine != nil:
		return AdaptEngine(reg, e.engine), nil
	case e.engineSrc != nil:
		return AdaptEngineSrc(reg, e.engineSrc), nil
	default:
		return AdaptFactory(reg, e.local), nil
	}
}

// groupUsages documents the known verb groups. A group discovered from the
// entries but absent here still gets a (usage-less) group command — command
// reachability never depends on an allowlist.
var groupUsages = map[string]string{
	"auth":   "Auth operations",
	"config": "Config operations",
	"create": "Create resources",
	"get":    "Read resources",
	"patch":  "Patch resources",
	"delete": "Delete resources",
}

// assembleCommands builds the root command list: the top-level commands
// followed by one group command per group in first-appearance order. Leaves in
// the "" group are attached at the top level.
func assembleCommands(topLevel []*ucli.Command, order []string, groups map[string][]*ucli.Command) []*ucli.Command {
	out := append([]*ucli.Command{}, topLevel...)
	for _, g := range order {
		leaves := groups[g]
		if len(leaves) == 0 {
			continue
		}
		if g == "" {
			out = append(out, leaves...)
			continue
		}
		out = append(out, &ucli.Command{Name: g, Usage: groupUsages[g], Commands: leaves})
	}
	return out
}

// BuildRoot constructs the root command from the declarative entry table. Kind
// validation and resolver composition fail fast; then one loop probes,
// validates, registers, and wires every command. `schema` is a fixed command
// backed by the same registry. Global flags are defined at root so subcommand
// actions can read them (c.Bool("dry-run"), c.String("config"), ...).
func BuildRoot(version string) (*ucli.Command, error) {
	if err := kinds.Validate(); err != nil {
		return nil, fmt.Errorf("kind table: %w", err)
	}
	resolver, err := runtime.ComposeResolvers(provider.NewResolver(), runtime.MapResolver(kinds.Bindings()))
	if err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}
	reg := runtime.NewRegistry(resolver)

	groups := map[string][]*ucli.Command{}
	var order []string
	for _, e := range buildEntries() {
		leaf, err := wireEntry(reg, resolver, e)
		if err != nil {
			return nil, err
		}
		if _, seen := groups[e.group]; !seen {
			order = append(order, e.group)
		}
		groups[e.group] = append(groups[e.group], leaf)
	}

	// schema is a fixed command backed by the registry (no dir/store, no binding).
	schemaCmd := runtime.NewSchemaCommand(reg, version)
	reg.Register(schemaCmd)

	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("command registry: %w", err)
	}

	commandsList := assembleCommands([]*ucli.Command{Adapt(reg, schemaCmd)}, order, groups)

	root := &ucli.Command{
		Name:    "rancher",
		Usage:   "Rancher CLI",
		Version: version,
		Flags: append([]ucli.Flag{
			&ucli.BoolFlag{Name: "debug", Usage: "Debug logging"},
			&ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "Path to rancher config dir",
				Sources: ucli.EnvVars("RANCHER_CONFIG_DIR")},
		}, GlobalFlags()...),
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			if c.Bool("debug") {
				logrus.SetLevel(logrus.DebugLevel)
			}
			return ctx, nil
		},
		Commands: commandsList,
	}
	return root, nil
}
