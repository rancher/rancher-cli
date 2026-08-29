package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// The timeout bounds live in the config package (shared with the TAB-time
// dispatch); the alias keeps this file's validation messages in one place.
const completionTimeoutMax = config.CompletionTimeoutMax

type setCompletionCmd struct{ dir string }

// NewSetCompletion builds `config set-completion` (local): persist completion
// preferences (network on/off, request timeout) in config.yaml.
func NewSetCompletion(dir string) runtime.Command {
	return &setCompletionCmd{dir: dir}
}

func (setCompletionCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config set-completion",
		Usage: "Configure live shell completion (network on/off, timeout)",
		Desc: "Stores completion preferences in config.yaml. " +
			"--network true enables live resource-name completion via the Rancher API. " +
			"--timeout sets the per-TAB budget for all HTTP requests (max 2s; values above clamp silently). " +
			"Prints a credential-store capability note when enabling --network.",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			// Type "string" + Enum because runtime.Input has no presence method:
			// a bool flag cannot distinguish "omitted" from "false" (finding 5).
			{Name: "network", Type: "string", Enum: []string{"true", "false"}, Desc: "enable live resource-name completion (true|false)"},
			{Name: "timeout", Type: "string", Desc: "per-TAB HTTP `budget` (e.g. 500ms, 1s; max 2s)"},
		},
		Examples: []string{
			"rancher config set-completion --network=true",
			"rancher config set-completion --network=true --timeout=1s",
		},
		Shape: runtime.ShapeText,
	}
}

func (c setCompletionCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	networkStr := in.String("network")
	timeoutStr := in.String("timeout")
	if networkStr == "" && timeoutStr == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide --network and/or --timeout").
			WithHint("run: rancher config set-completion --network=true [--timeout=500ms]")
	}
	if networkStr != "" && networkStr != "true" && networkStr != "false" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--network=%q is not valid; use true or false", networkStr))
	}

	// Parse and clamp the timeout before taking the lock — validation errors
	// should not require the lock.
	var parsedTimeout time.Duration
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--timeout=%q is not a valid duration: %v", timeoutStr, err)).
				WithHint("examples: 500ms, 1s, 2s")
		}
		if d > completionTimeoutMax {
			d = completionTimeoutMax
		}
		parsedTimeout = d
	}

	var (
		result map[string]any
		note   string
	)
	if err := config.WithLock(cmdCtx, c.dir, func() error {
		cfg, err := config.Load(config.FilePath(c.dir))
		if err != nil {
			return err
		}
		switch networkStr {
		case "true":
			cfg.Completion.Network = true
		case "false":
			cfg.Completion.Network = false
		}
		// Store as a plain-text duration string; EffectiveCompletionTimeout
		// parses and clamps at use (hand-edited values outside [1ms,2s] are clamped
		// when consumed, not rejected at write).
		if timeoutStr != "" {
			cfg.Completion.Timeout = parsedTimeout.String()
		}
		if err := cfg.Write(); err != nil {
			return err
		}
		result = map[string]any{
			"network": cfg.Completion.Network,
			"timeout": cfg.Completion.Timeout,
		}
		// The capability note accompanies ENABLING network completion in this
		// invocation, not every later timeout tweak while it happens to be on.
		if networkStr == "true" {
			note = completionCapabilityNote(c.dir, cfg)
		}
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}

	if note != "" {
		result["note"] = note
		return runtime.Result{Kind: "CompletionConfig", Data: result, Text: "# " + note + "\n"}, nil
	}
	res := runtime.TextLines("completion settings updated")
	res.Kind = "CompletionConfig"
	res.Data = result
	return res, nil
}

// completionCapabilityNote returns the credential-store-aware note for
// --network=true (AC-3). Classification uses ONLY env token presence,
// the declared credential-store policy, and a read-only ReadPin; it never
// probes the keyring and never writes a pin.
func completionCapabilityNote(dir string, cfg *config.Config) string {
	if os.Getenv(config.EnvToken) != "" {
		return "live completion enabled; RANCHER_CLI_TOKEN detected — full capability."
	}
	policy := config.EffectivePolicy(cfg.CredentialStore)
	pin := config.ReadPin(dir)

	isKeychain := policy == "keychain" ||
		(policy == "auto" && pin == "keychain")
	isFile := policy == "file" ||
		(policy == "auto" && pin == "file")
	isUnpinned := policy == "auto" && pin == ""

	switch {
	case isKeychain:
		return "cluster/project name completion needs RANCHER_CLI_TOKEN or the file credential store; " +
			"namespace/resource completion works after kubeconfig (exec-credential) use, " +
			"and only when the cluster is referenced by id"
	case isFile:
		return "live completion enabled with full capability (file credential store)."
	case isUnpinned:
		return "credential store not yet resolved; live completion will use RANCHER_CLI_TOKEN " +
			"or cached exec credentials until the store is pinned " +
			"(run any authenticated command to pin it)"
	default:
		return "live completion enabled."
	}
}
