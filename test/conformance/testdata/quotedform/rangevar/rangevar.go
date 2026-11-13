package rangevar

import (
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func literal() []string {
	var out []string
	for _, c := range []string{clierr.Rancher + " get users"} {
		out = append(out, fmt.Sprintf("%q", c))
	}
	return out
}

func local() []string {
	cmds := []string{clierr.Rancher + " get users"}
	var out []string
	for i, c := range cmds {
		out = append(out, strconv.Quote(c), strconv.Itoa(i))
	}
	return out
}

func plain() []string {
	var out []string
	for _, n := range []string{"a", "b"} {
		out = append(out, strconv.Quote(n))
	}
	return out
}
