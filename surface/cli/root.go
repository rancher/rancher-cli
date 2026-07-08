package cli

import (
	"context"
	"fmt"

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

// BuildRoot constructs the Phase-0 root command. ONE registry backs `schema`;
// every config-touching command resolves its dir/store per-run via
// AdaptFactory -> resolveDeps, so --config takes effect and migration fires
// only on first run. Global flags are defined at root so subcommand actions
// can read them (c.Bool("dry-run"), c.String("config"), ...).
func BuildRoot(version string) (*ucli.Command, error) {
	reg := runtime.NewRegistry(provider.NewResolver())

	// Config-touching command factories (dir/store bound at run time by AdaptFactory).
	loginF := func(dir string, s config.CredStore) runtime.Command { return commands.NewLogin(dir, s) }
	logoutF := func(dir string, s config.CredStore) runtime.Command { return commands.NewLogout(dir, s) }
	authCheckF := func(dir string, s config.CredStore) runtime.Command { return commands.NewAuthCheck(dir, s) }
	purgeF := func(_ string, _ config.CredStore) runtime.Command { return commands.NewConfigPurgeLegacy() }
	tokenF := func(dir string, s config.CredStore) runtime.Command { return token.NewTokenCommand(dir, s) }
	delTokenF := func(dir string, s config.CredStore) runtime.Command { return token.NewDeleteTokenCommand(dir, s) }
	listTokenF := func(dir string, s config.CredStore) runtime.Command { return token.NewListTokensCommand(dir, s) }
	ctxCurF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextCurrent(dir) }
	ctxSwF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextSet(dir) }
	ctxUseF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextUse(dir) }
	srvLsF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewServerLs(dir) }
	srvSwF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewServerSwitch(dir) }

	// Engine command factories (cluster client bound at run time by AdaptEngine).
	clusterKind, ok := kinds.Lookup("cluster")
	if !ok {
		return nil, fmt.Errorf("kind table is missing the cluster kind")
	}
	getClusterF := func(clients engine.ClusterClientFactory) runtime.Command {
		return resources.NewGet(clusterKind, clients)
	}
	createProjectF := func(clients engine.ClusterClientFactory) runtime.Command {
		return projects.NewCreate(clients)
	}

	schemaCmd := runtime.NewSchemaCommand(reg, version)

	// Register ONE instance of every command into the single registry for the
	// schema manifest (Describe() only; dir/store irrelevant).
	for _, f := range []func(string, config.CredStore) runtime.Command{
		loginF, logoutF, tokenF, delTokenF, listTokenF, authCheckF, purgeF, ctxCurF, ctxSwF, ctxUseF, srvLsF, srvSwF,
	} {
		reg.Register(f("", nil))
	}
	reg.Register(getClusterF(nil))
	reg.Register(createProjectF(nil))
	reg.Register(schemaCmd)

	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("command registry: %w", err)
	}

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
		Commands: []*ucli.Command{
			Adapt(schemaCmd),
			{Name: "auth", Usage: "Auth operations",
				Commands: []*ucli.Command{
					AdaptFactory(loginF),     // auth login
					AdaptFactory(logoutF),    // auth logout
					AdaptFactory(authCheckF), // auth check
					AdaptFactory(tokenF),     // auth get-token
					AdaptFactory(listTokenF), // auth list-tokens
					AdaptFactory(delTokenF),  // auth delete-token
				}},
			{Name: "config", Usage: "Config operations",
				Commands: []*ucli.Command{
					AdaptFactory(ctxCurF), // config current-context
					AdaptFactory(ctxSwF),  // config set-context
					AdaptFactory(ctxUseF), // config use-context
					AdaptFactory(srvLsF),  // config get-servers
					AdaptFactory(srvSwF),  // config use-server
					AdaptFactory(purgeF),  // config purge-legacy
				}},
			{Name: "create", Usage: "Create resources", Commands: []*ucli.Command{
				AdaptEngine(reg, createProjectF),
			}},
			{Name: "get", Usage: "Read resources", Commands: []*ucli.Command{
				AdaptEngine(reg, getClusterF),
			}},
		},
	}
	return root, nil
}
