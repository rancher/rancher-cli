package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configInfoCmd struct{ dir string }

// NewConfigInfo builds `config status` (local, no network/auth): the resolved
// config file path plus the EFFECTIVE credential-store policy and its source
// (env > config > default auto), with the auto-probe pin when one exists.
// This is the one place that answers "which credential store am I actually
// using": an env-supplied store never persists into config.yaml, so neither
// the file nor `config get-servers` can reveal it (vault-helper live
// exercise, S-05). dir carries the same flag > env > XDG resolution every
// config command receives from resolveDeps.
func NewConfigInfo(dir string) runtime.Command { return &configInfoCmd{dir: dir} }

func (configInfoCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config status",
		Usage: "Print config diagnostics: file path and effective credential store",
		Desc: "Reports the resolved config file path (and whether it exists), " +
			"the current server (empty when not logged in), " +
			"and the effective credential-store policy with its source: the " +
			"RANCHER_CLI_CREDENTIAL_STORE environment variable, the config " +
			"file's credentialStore key, or the built-in default (auto). " +
			"Under auto, also reports the probed backend pin when present. " +
			"Also reports a nested completion section (network: live API " +
			"completion on/off; timeout: effective per-TAB budget after " +
			"clamping/default; timeoutRaw: only when the stored value was " +
			"clamped or unparseable). The first place to check when " +
			"tab-completion shows no candidates.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config status"},
	}
}

func (c configInfoCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	path := config.FilePath(c.dir)
	exists, err := configFileExists(path)
	if err != nil {
		return runtime.Result{}, err
	}

	cfg, err := config.Load(path)
	if err != nil {
		return runtime.Result{}, err
	}
	policy, source := config.EffectivePolicyWithSource(cfg.CredentialStore)
	// The pin is auto's probed backend choice — meaningless (and, when
	// stale, actively misleading) under any explicit policy, so it is
	// reported only when the effective policy is auto.
	pin := ""
	if policy == "auto" {
		pin = config.ReadPin(c.dir)
	}

	// field is either a scalar (key: val) or a nested section
	// (key: []field) rendered as an indented block. One declaration drives
	// both the JSON data map and the plain-text output, so -o json and text
	// cannot drift apart for any field — flat or nested.
	type field struct {
		key string
		val any // scalar any, or []field for a nested section
	}

	fields := []field{
		{"path", path},
		{"exists", exists},
		{"currentServer", cfg.CurrentServer},
		{"credentialStore", policy},
		{"credentialStoreSource", source},
	}
	if pin != "" {
		fields = append(fields, field{"pin", pin})
	}

	// Completion settings — always present so `config status` can answer
	// "why does TAB show no candidates?" without knowing whether completion
	// was ever configured. CompletionTimeoutWithRaw resolves effective vs.
	// raw in one pass so the two cannot disagree.
	effectiveTimeout, rawTimeout, diverges := config.CompletionTimeoutWithRaw(cfg)
	completionFields := []field{
		{"network", cfg.Completion.Network},
		{"timeout", effectiveTimeout.String()},
	}
	if diverges {
		completionFields = append(completionFields, field{"timeoutRaw", rawTimeout})
	}
	fields = append(fields, field{"completion", completionFields})

	data := make(map[string]any, len(fields))
	var text strings.Builder
	for _, f := range fields {
		if sub, ok := f.val.([]field); ok {
			subData := make(map[string]any, len(sub))
			fmt.Fprintf(&text, "%s:\n", f.key)
			for _, s := range sub {
				subData[s.key] = s.val
				fmt.Fprintf(&text, "  %s: %v\n", s.key, s.val)
			}
			data[f.key] = subData
		} else {
			data[f.key] = f.val
			fmt.Fprintf(&text, "%s: %v\n", f.key, f.val)
		}
	}
	return runtime.Result{Kind: "ConfigInfo", Data: data, Text: text.String()}, nil
}
