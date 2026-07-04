package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
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
	authCheckF := func(dir string, s config.CredStore) runtime.Command { return commands.NewAuthCheck(dir, s) }
	purgeF := func(_ string, _ config.CredStore) runtime.Command { return commands.NewConfigPurgeLegacy() }
	tokenF := func(dir string, s config.CredStore) runtime.Command { return token.NewTokenCommand(dir, s) }
	delTokenF := func(dir string, s config.CredStore) runtime.Command { return token.NewDeleteTokenCommand(dir, s) }
	ctxCurF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextCurrent(dir) }
	ctxSwF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextSwitch(dir) }
	ctxUseF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewContextUse(dir) }
	srvLsF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewServerLs(dir) }
	srvSwF := func(dir string, _ config.CredStore) runtime.Command { return commands.NewServerSwitch(dir) }

	schemaCmd := runtime.NewSchemaCommand(reg, version)

	// Register ONE instance of every command into the single registry for the
	// schema manifest (Describe() only; dir/store irrelevant).
	for _, f := range []func(string, config.CredStore) runtime.Command{
		loginF, tokenF, delTokenF, authCheckF, purgeF, ctxCurF, ctxSwF, ctxUseF, srvLsF, srvSwF,
	} {
		reg.Register(f("", nil))
	}
	reg.Register(schemaCmd)

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
			{Name: "context", Usage: "Context operations",
				Commands: []*ucli.Command{AdaptFactory(ctxSwF), AdaptFactory(ctxCurF), AdaptFactory(ctxUseF)}},
			{Name: "server", Usage: "Server operations",
				Commands: []*ucli.Command{AdaptFactory(srvLsF), AdaptFactory(srvSwF)}},
			{Name: "auth", Usage: "Auth operations",
				Commands: []*ucli.Command{
					AdaptFactory(loginF),     // auth login
					AdaptFactory(authCheckF), // auth check
					AdaptFactory(tokenF),     // auth get-token
					AdaptFactory(delTokenF),  // auth delete-token
				}},
			{Name: "config", Usage: "Config operations",
				Commands: []*ucli.Command{AdaptFactory(purgeF)}},
		},
	}
	return root, nil
}
