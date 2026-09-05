package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configAddServerCmd struct {
	dir          string
	tlsPreflight tlsPreflightFunc
}

// NewConfigAddServer builds `config add-server <name> --url <url>
// [--cacert <path|PEM>]` (local): the registration path's add caller -
// establishes trust and records a server entry, bootstrapping/repairing the
// current-server pointer when it is empty or dangling. Never an upsert: an
// existing name with a different URL or CA is CONFLICT.
func NewConfigAddServer(dir string) runtime.Command {
	return &configAddServerCmd{dir: dir, tlsPreflight: tlsPreflightDefault}
}

func (configAddServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config add-server",
		Usage:       "Add a Rancher server entry",
		CommandType: runtime.Local,
		Network:     true,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Required: true, Desc: "Rancher server `url` (scheme required)"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM"},
		},
		Examples: []string{"rancher config add-server qa1 --url https://qa1.example.com"},
		Shape:    runtime.ShapeText,
	}
}

func (c configAddServerCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]
	if ce := validateEntryName(name); ce != nil {
		return runtime.Result{}, ce
	}
	url := in.String("url")
	if url == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--url is required")
	}

	canonicalURL, err := canonicalizeServerURL(url)
	if err != nil {
		return runtime.Result{}, err
	}
	probe, err := prepareRegistration(ctx, c.tlsPreflight, canonicalURL, in.String("cacert"))
	if err != nil {
		return runtime.Result{}, err
	}

	out, err := commitCreateUnderLock(ctx, c.dir, name, probe)
	if err != nil {
		return runtime.Result{}, err
	}

	res := runtime.Result{Kind: "ServerAdded", Data: map[string]any{
		"name": out.name, "url": out.canonicalURL, "ca": caPresence(out.caSelector),
		"currentServer": out.currentServer, "unchanged": !out.created,
	}}
	if probe.versionWarning != "" {
		res.Warnings = append(res.Warnings, probe.versionWarning)
	}
	if !out.created {
		tr := runtime.TextLines("server " + out.name + " unchanged")
		res.Text, res.StyledText = tr.Text, tr.StyledText
		return res, nil
	}
	var lines []string
	lines = append(lines, "url: "+out.canonicalURL)
	if caPresence(out.caSelector) != "none" {
		lines = append(lines, "ca: pinned")
	}
	if out.pointerRepaired {
		lines = append(lines, "current server set to "+out.name)
	}
	tr := runtime.TextLines("server "+out.name+" added", lines...)
	res.Text, res.StyledText = tr.Text, tr.StyledText
	return res, nil
}
