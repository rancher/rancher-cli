package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/rancher/rancher-cli/internal/skillinstall"
	"github.com/rancher/rancher-cli/skills"
)

// VERSION is injected at build time via -ldflags "-X main.VERSION=...".
var VERSION = "dev"

func main() {
	skillinstall.SetSource(skills.Rancher())

	if cli.OutdatedCompletionScript(os.Args[1:], os.Stderr) {
		return
	}
	httpx.SetUserAgent(invocation.Detect(VERSION, os.Getenv).UserAgent())
	root, err := cli.BuildRoot(VERSION)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher:", err)
		os.Exit(1)
	}
	ctx, stop := cli.InterruptContext(context.Background(), os.Exit)
	defer stop()
	if err := cli.Run(ctx, root, os.Args); err != nil {
		os.Exit(cli.ReportRunError(root, err))
	}
}
