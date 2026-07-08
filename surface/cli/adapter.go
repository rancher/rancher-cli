package cli

import (
	"context"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/sirupsen/logrus"
	ucli "github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// GlobalFlags returns the confirmation/output flags shared by all commands.
// These are placed on the root command in v3; they are inherited by subcommands
// via urfave/cli/v3's flag resolution (e.g. `rancher ping -o json`,
// `rancher clusters scale ... --apply`). (--config and --debug are on root in root.go.)
func GlobalFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.BoolFlag{Name: "dry-run", Usage: "preview the Effect without changing anything"},
		&ucli.BoolFlag{Name: "apply", Usage: "execute the mutation"},
		&ucli.BoolFlag{Name: "yes", Usage: "confirm a destructive Effect"},
		&ucli.BoolFlag{Name: "allow-deprecated", Usage: "permit a deprecated-tier write"},
		&ucli.BoolFlag{Name: "no-input", Usage: "never prompt; ambiguity becomes an error"},
		&ucli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "table|json|yaml|name|template"},
	}
}

// cmdInput adapts a *cli.Command to runtime.Input.
type cmdInput struct{ c *ucli.Command }

func (i cmdInput) String(name string) string { return i.c.String(name) }
func (i cmdInput) Bool(name string) bool     { return i.c.Bool(name) }
func (i cmdInput) Args() []string            { return i.c.Args().Slice() }

// leafName returns the last space-separated token of a command name
// ("auth check" -> "check") for use as the urfave leaf/subcommand name.
func leafName(fullName string) string {
	if idx := strings.LastIndex(fullName, " "); idx >= 0 {
		return fullName[idx+1:]
	}
	return fullName
}

// flagsFor converts a Descriptor's named Flags into urfave flags (string/bool).
// Required-ness and Enum membership are intentionally NOT set on the urfave flag:
// they are enforced by runtime.validateInput so violations surface as structured
// clierr envelopes (urfave's own required/validator errors are unstructured).
func flagsFor(d runtime.Descriptor) []ucli.Flag {
	var out []ucli.Flag
	for _, f := range d.Flags {
		if f.Type == "bool" {
			out = append(out, &ucli.BoolFlag{Name: f.Name})
		} else {
			out = append(out, &ucli.StringFlag{Name: f.Name})
		}
	}
	return out
}

// buildIO reads the resolved global flags/streams for a run.
func buildIO(c *ucli.Command) runtime.IO {
	r := c.Root()
	in := r.Reader
	if in == nil {
		in = os.Stdin
	}
	isTTY := false
	if fp, ok := in.(*os.File); ok {
		isTTY = term.IsTerminal(int(fp.Fd()))
	}
	return runtime.IO{
		Out:             r.Writer,
		Err:             r.ErrWriter,
		In:              in,
		Format:          c.String("output"),
		DryRun:          c.Bool("dry-run"),
		Apply:           c.Bool("apply"),
		Yes:             c.Bool("yes"),
		AllowDeprecated: c.Bool("allow-deprecated"),
		NoInput:         c.Bool("no-input"),
		IsTTY:           isTTY,
	}
}

// Adapt wraps a fixed runtime.Command that needs no per-run config dir/store
// (e.g. `schema`). Prefer AdaptFactory for commands that touch config.
func Adapt(rc runtime.Command) *ucli.Command {
	d := rc.Describe()
	return &ucli.Command{
		Name:  leafName(d.Name),
		Usage: d.Usage,
		Flags: flagsFor(d),
		Action: func(ctx context.Context, c *ucli.Command) error {
			if ce := runtime.Run(ctx, rc, cmdInput{c}, buildIO(c), safety.Binding{}); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
}

// AdaptFactory wraps a command that needs the config dir/store resolved at
// RUN time from the (persistent) --config flag. `make` must build the command
// from (dir, store); its Describe() must not depend on dir/store.
func AdaptFactory(make func(dir string, store config.CredStore) runtime.Command) *ucli.Command {
	d := make("", nil).Describe() // Describe is dir/store-independent
	return &ucli.Command{
		Name:  leafName(d.Name),
		Usage: d.Usage,
		Flags: flagsFor(d),
		Action: func(ctx context.Context, c *ucli.Command) error {
			dir, store, err := resolveDeps(c)
			if err != nil {
				return exit(clierr.New(clierr.CodeGeneric, err.Error()))
			}
			if ce := runtime.Run(ctx, make(dir, store), cmdInput{c}, buildIO(c), safety.Binding{}); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
}

// resolveDeps resolves the config dir from --config (flag > env > XDG), runs a
// one-time legacy migration when no new config exists yet, and opens the
// credential store per the config's policy. This is THE single place dir/store
// are resolved, so --config always takes effect and migration never re-fires.
func resolveDeps(c *ucli.Command) (string, config.CredStore, error) {
	dir, err := config.ResolveDir(c.String("config"))
	if err != nil {
		return "", nil, err
	}
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return "", nil, err
	}
	// Load before OpenStore so the declared policy drives store selection, and
	// migration lands tokens in the backend the config declares.
	store, err := config.OpenStore(cfg.CredentialStore, dir)
	if err != nil {
		return "", nil, err
	}
	if _, statErr := os.Stat(config.FilePath(dir)); os.IsNotExist(statErr) {
		if legacy, ok := config.FindLegacy(); ok {
			// FindLegacy scans the real $HOME; only auto-migrate into the DEFAULT
			// config dir. An explicitly overridden dir gets a hint, never a
			// silent import of the user's real cli2.json.
			if c.String("config") != "" || os.Getenv("RANCHER_CONFIG_DIR") != "" {
				logrus.Warnf("legacy config found at %s; not importing into an explicitly overridden config dir", legacy)
			} else if _, mErr := config.Migrate(legacy, dir, store); mErr == nil {
				logrus.Warnf("migrated legacy config from %s; remove it with `rancher config purge-legacy`", legacy)
			}
		}
	}
	return dir, store, nil
}

// exit converts a structured error into a urfave exit with the right code.
func exit(ce *clierr.Error) error {
	return ucli.Exit("", ce.ExitCode())
}
