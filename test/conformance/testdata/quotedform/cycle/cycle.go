package cycle

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func selfReference() string {
	s := clierr.Rancher
	s = s + s
	return strconv.Quote(s)
}

func again(n int) string {
	if n == 0 {
		return clierr.Rancher
	}
	return again(n - 1)
}

func recursive() string { return strconv.Quote(again(3)) }
