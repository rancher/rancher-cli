package main

import (
	"context"
	"errors"
	"os"

	surfacecli "github.com/rancher/rancher-cli/surface/cli"
	"github.com/sirupsen/logrus"
	ucli "github.com/urfave/cli/v3"
)

// VERSION is injected at build time via -ldflags "-X main.VERSION=...".
var VERSION = "dev"

func main() {
	root, err := surfacecli.BuildRoot(VERSION)
	if err != nil {
		logrus.Fatal(err)
	}
	if err := root.Run(context.Background(), os.Args); err != nil {
		// The adapter returns cli.Exit("", code); honor that exact exit code.
		var ec ucli.ExitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
