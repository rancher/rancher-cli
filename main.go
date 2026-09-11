package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/skillinstall"
	ucli "github.com/urfave/cli/v3"
)

// VERSION is injected at build time via -ldflags "-X main.VERSION=...".
var VERSION = "dev"

//go:embed skills/rancher
var skillFS embed.FS

func main() {
	skillSub, err := fs.Sub(skillFS, "skills/rancher")
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher: embedded skill tree unavailable:", err)
		os.Exit(1)
	}
	skillinstall.SetSource(skillSub)

	cli.CaptureArgv(os.Args[1:])
	root, err := cli.BuildRoot(VERSION)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.Run(ctx, cli.SanitizedArgs(os.Args)); err != nil {
		if ec, ok := errors.AsType[ucli.ExitCoder](err); ok {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
