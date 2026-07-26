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

	"github.com/rancher/rancher-cli/internal/skillinstall"
	surfacecli "github.com/rancher/rancher-cli/surface/cli"
	"github.com/sirupsen/logrus"
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

	surfacecli.CaptureArgv(os.Args[1:])
	root, err := surfacecli.BuildRoot(VERSION)
	if err != nil {
		logrus.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.Run(ctx, surfacecli.SanitizedArgs(os.Args)); err != nil {
		var ec ucli.ExitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
