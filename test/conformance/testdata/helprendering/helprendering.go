package helprendering

import (
	"os"

	ucli "github.com/urfave/cli/v3"
)

func printCustom(c *ucli.Command) { ucli.HelpPrinterCustom(os.Stdout, "", c, nil) }

func printDefault(c *ucli.Command) { ucli.DefaultPrintHelp(os.Stdout, "", c) }

func printDefaultCustom(c *ucli.Command) { ucli.DefaultPrintHelpCustom(os.Stdout, "", c, nil) }

func formatFlag(f ucli.Flag) string { return ucli.FlagStringer(f) }

func allowed(c *ucli.Command) {
	ucli.HelpPrinter(os.Stdout, "", c)
	ucli.ShowRootCommandHelp(c)
}

func flagString(f ucli.Flag) string { return f.String() }
