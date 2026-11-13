package verbforms

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func indexed(name string) string { return fmt.Sprintf("%[2]q %[1]s", "x", clierr.ShellQuote(name)) }

func plus(name string) string { return fmt.Sprintf("%+q", clierr.ShellQuote(name)) }

func sharp() string { return fmt.Sprintf("%#q", clierr.Rancher) }

func star(name string) string { return fmt.Sprintf("%*s %q", 3, "x", clierr.ShellQuote(name)) }

func percent(name string) string { return fmt.Sprintf("%% %q", clierr.ShellQuote(name)) }

func commandToSpace(name string) string { return fmt.Sprintf("%s %q", clierr.ShellQuote(name), name) }

func commandToSpaceAfterStar(name string) string {
	return fmt.Sprintf("%*s %q", 3, clierr.ShellQuote(name), name)
}
