package tuple

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func pair(n string) (string, error) { return n, errors.New(clierr.Rancher + " auth login") }

func commandFirst() (string, error) { return clierr.Rancher + " auth login", nil }

func named() (text string, err error) {
	text = "plain"
	err = errors.New(clierr.Rancher + " auth login")
	return
}

func namedCommand() (cmd string, err error) {
	cmd = clierr.Rancher + " auth login"
	return
}

func plainFirst() string {
	s, _ := pair("x")
	return fmt.Sprintf("%q", s)
}

func plainNamed() string {
	text, _ := named()
	return strconv.Quote(text)
}

func quotedCommandFirst() string {
	c, _ := commandFirst()
	return strconv.Quote(c)
}

func quotedNamedCommand() string {
	c, _ := namedCommand()
	return fmt.Sprintf("%q", c)
}
