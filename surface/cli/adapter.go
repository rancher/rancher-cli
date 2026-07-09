package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
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
		&ucli.StringFlag{Name: "context", Usage: "scope this invocation to cluster[:project] for server operations (overrides RANCHER_CONTEXT and the current context); a trailing colon (cluster:) clears the project; the project axis is informational only"},
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

// groupName returns the verb-group prefix of a command name ("auth check" ->
// "auth", "get pod" -> "get", "schema" -> ""). The registration loop checks it
// against the entry's declared group.
func groupName(fullName string) string {
	if idx := strings.LastIndex(fullName, " "); idx >= 0 {
		return fullName[:idx]
	}
	return ""
}

// flagsFor converts a Descriptor's named Flags into urfave flags (string/bool).
// Required-ness and Enum membership are intentionally NOT set on the urfave flag:
// they are enforced by runtime.validateInput so violations surface as structured
// clierr envelopes (urfave's own required/validator errors are unstructured).
func flagsFor(d runtime.Descriptor) []ucli.Flag {
	var out []ucli.Flag
	for _, f := range d.Flags {
		if f.Type == "bool" {
			out = append(out, &ucli.BoolFlag{Name: f.Name, Aliases: f.Aliases})
		} else {
			out = append(out, &ucli.StringFlag{Name: f.Name, Aliases: f.Aliases})
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
// (e.g. `schema`). The binding is derived from the registry uniformly, so a
// mutating command wired this way still runs under its gate.
func Adapt(reg *runtime.Registry, rc runtime.Command) *ucli.Command {
	d := rc.Describe()
	b := reg.Binding(d.Name)
	return &ucli.Command{
		Name:    leafName(d.Name),
		Aliases: d.Aliases,
		Usage:   d.Usage,
		Flags:   flagsFor(d),
		Action: func(ctx context.Context, c *ucli.Command) error {
			if ce := runtime.Run(ctx, rc, cmdInput{c}, buildIO(c), b); ce != nil {
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
	// FindLegacy scans the real $HOME; only auto-migrate into the DEFAULT config
	// dir. An explicitly overridden dir (--config or RANCHER_CONFIG_DIR) is
	// deliberately isolated — legacy import advice is irrelevant there, and
	// even a plain-text notice would corrupt a scripted session's stderr JSON
	// (the error envelope, when one follows, must stay the only/last JSON
	// object on stderr), so this case stays silent.
	explicitDir := c.String("config") != "" || os.Getenv("RANCHER_CONFIG_DIR") != ""
	if !explicitDir {
		if _, statErr := os.Stat(config.FilePath(dir)); os.IsNotExist(statErr) {
			if legacy, ok := config.FindLegacy(); ok {
				if _, mErr := config.Migrate(legacy, dir, store); mErr == nil {
					logrus.Warnf("migrated legacy config from %s; remove it with `rancher config purge-legacy`", legacy)
				}
			}
		}
	}
	return dir, store, nil
}

// exit converts a structured error into a urfave exit with the right code.
func exit(ce *clierr.Error) error {
	return ucli.Exit("", ce.ExitCode())
}

// commandNotFound is urfave/cli's seam for an unmatched command/subcommand
// name (registered as root's and every group's CommandNotFound). Without it,
// urfave's own DefaultShowCommandHelp hard-codes a plain-text "No help topic
// for '<name>'" on stderr and exits 3 — indistinguishable from the AUTH exit
// code documented for exit 3, and with no JSON envelope for a script to
// parse. CommandNotFoundFunc has no error return, so the VALIDATION envelope
// and the process exit both happen here directly, mirroring how urfave's own
// ShowRootCommandHelpAndExit calls OsExiter.
func commandNotFound(_ context.Context, c *ucli.Command, name string) {
	ce := clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown command %q", name)).
		WithHint("run: rancher schema")
	_ = output.Render(c.Root().ErrWriter, "json", output.ErrorEnvelope(ce))
	ucli.OsExiter(ce.ExitCode())
}
