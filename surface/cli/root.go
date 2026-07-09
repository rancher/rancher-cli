package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/sirupsen/logrus"
	ucli "github.com/urfave/cli/v3"
)

// entry is one command's declarative registration: its verb group and a single
// constructor taking runDeps. The constructor pulls whatever it needs (client
// factory, file source, config dir/store) off runDeps and ignores the rest, so
// adding a command is one row here — the compiler enforces the dependency set,
// with no per-shape adapter or wiring switch.
type entry struct {
	group string
	make  func(runDeps) runtime.Command
}

// buildEntries is the single source of command wiring: the bespoke config/token
// and create commands, plus generated get/delete commands for every kind in the
// table. Adding a command is one row here (plus a kind-table row for a new
// kind).
func buildEntries() []entry {
	entries := []entry{
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogin(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogout(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewAuthCheck(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewTokenCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewListTokensCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewDeleteTokenCommand(d.dir, d.store) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextCurrent(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextSet(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextUse(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerLs(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerSwitch(d.dir) }},
		{group: "config", make: func(runDeps) runtime.Command { return commands.NewConfigPurgeLegacy() }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreate(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return resources.NewCreate(d.clients, d.src) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewPod(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewDeployment(d.clients) }},
		{group: "top", make: func(d runDeps) runtime.Command { return inspect.NewTopNode(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewImages(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewUsage(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return commands.NewKubeconfig(d.dir, d.clients) }},
	}
	for _, k := range kinds.All() {
		k := k
		entries = append(entries, entry{group: "get", make: func(d runDeps) runtime.Command {
			return resources.NewGet(k, d.clients)
		}})
		entries = append(entries, entry{group: "patch", make: func(d runDeps) runtime.Command {
			return resources.NewPatch(k, d.clients, d.src)
		}})
		if k.Deletable {
			entries = append(entries, entry{group: "delete", make: func(d runDeps) runtime.Command {
				return resources.NewDelete(k, d.clients)
			}})
		}
	}
	return entries
}

// wireEntry validates one entry and returns its urfave leaf. It probes
// Describe() with zero deps (Describe must be dir/store/client-independent),
// checks the name's group matches the declared group, requires every engine
// command — read or mutating (CommandType != Local) — to resolve to a binding,
// registers the probe, and wires it through the single run adapter.
func wireEntry(reg *runtime.Registry, resolver runtime.Resolver, e entry) (*ucli.Command, error) {
	if e.make == nil {
		return nil, fmt.Errorf("entry in group %q has no constructor", e.group)
	}
	d := e.make(runDeps{}).Describe()

	// The tree is two levels deep: a command name is exactly one token (a
	// top-level command) or two (group + leaf). Reject 3+ tokens with a clear
	// message rather than a misleading group mismatch.
	if n := strings.Count(d.Name, " "); n > 1 {
		return nil, fmt.Errorf("command name %q must be one or two space-separated tokens, has %d", d.Name, n+1)
	}
	if g := groupName(d.Name); g != e.group {
		return nil, fmt.Errorf("command %q is wired under group %q but its name implies %q", d.Name, e.group, g)
	}
	// Engine commands (read or mutating) must resolve to a binding; Local
	// config/auth commands carry no tier.
	if d.CommandType != runtime.Local {
		if _, ok := resolver.Resolve(d.Name); !ok {
			return nil, fmt.Errorf("engine command %q has no resolver binding", d.Name)
		}
	}

	reg.Register(e.make(runDeps{}))
	return adaptEntry(reg, e.make), nil
}

// groupUsages documents the known verb groups. A group discovered from the
// entries but absent here still gets a (usage-less) group command — command
// reachability never depends on an allowlist.
var groupUsages = map[string]string{
	"auth":    "Auth operations",
	"config":  "Config operations",
	"create":  "Create resources",
	"get":     "Read resources",
	"patch":   "Patch resources",
	"delete":  "Delete resources",
	"inspect": "Inspect composite reads",
	"top":     "Resource usage composites",
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
			config.SetContextOverride(c.String("context"), c.String("project"))
			return ctx, nil
		},
		Commands: commandsList,
	}
	return root, nil
}
