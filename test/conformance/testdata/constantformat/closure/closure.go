package closure

import (
	"github.com/rancher/rancher-cli/internal/clierr"
)

func run(sink func(string)) {
	say := func(format string, args ...any) { sink(format) }
	say(clierr.Rancher+" auth login %s", "x")
}
