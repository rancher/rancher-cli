package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configInfoCmd struct{ dir string }

// NewConfigInfo builds `config info` (local, no network/auth): the resolved
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
		Name:  "config info",
		Usage: "Print config diagnostics: file path and effective credential store",
		Desc: "Reports the resolved config file path (and whether it exists) " +
			"and the effective credential-store policy with its source: the " +
			"RANCHER_CLI_CREDENTIAL_STORE environment variable, the config " +
			"file's credentialStore key, or the built-in default (auto). " +
			"Under auto, also reports the probed backend pin when present. " +
			"Also reports completion settings: completion.network (live API " +
			"completion on/off) and completion.timeout (effective per-TAB " +
			"budget after clamping/default), with completion.timeoutRaw when " +
			"the stored value was clamped or unparseable. The first place to " +
			"check when tab-completion shows no candidates.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config info"},
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

	// One ordered field list drives BOTH output shapes, so -o json and the
	// plain-text rendering cannot drift apart. Conditional fields follow the
	// append-if idiom: pin and completion.timeoutRaw are added only when they
	// carry information.
	type field struct {
		key string
		val any
	}
	fields := []field{
		{"path", path},
		{"exists", exists},
		{"credentialStore", policy},
		{"credentialStoreSource", source},
	}
	if pin != "" {
		fields = append(fields, field{"pin", pin})
	}

	// Completion settings — always present so `config info` can answer
	// "why does TAB show no candidates?" without knowing whether completion
	// was ever configured. CompletionTimeoutWithRaw resolves effective vs.
	// raw in one pass so the two cannot disagree.
	effectiveTimeout, rawTimeout, diverges := config.CompletionTimeoutWithRaw(cfg)
	fields = append(fields,
		field{"completion.network", cfg.Completion.Network},
		field{"completion.timeout", effectiveTimeout.String()},
	)
	if diverges {
		fields = append(fields, field{"completion.timeoutRaw", rawTimeout})
	}

	data := make(map[string]any, len(fields))
	var text strings.Builder
	for _, f := range fields {
		data[f.key] = f.val
		fmt.Fprintf(&text, "%s: %v\n", f.key, f.val)
	}
	return runtime.Result{Kind: "ConfigInfo", Data: data, Text: text.String()}, nil
}
