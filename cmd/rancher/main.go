package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/skillinstall"
	"github.com/rancher/rancher-cli/skills"
	ucli "github.com/urfave/cli/v3"
)

// VERSION is injected at build time via -ldflags "-X main.VERSION=...".
var VERSION = "dev"

func main() {
	skillinstall.SetSource(skills.Rancher())

	cli.CaptureArgv(os.Args[1:])
	root, err := cli.BuildRoot(VERSION)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher:", err)
		os.Exit(1)
	}
	ctx, stop := cli.InterruptContext(context.Background(), os.Exit)
	defer stop()
	if err := root.Run(ctx, cli.SanitizedArgs(os.Args)); err != nil {
		if ec, ok := errors.AsType[ucli.ExitCoder](err); ok {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
