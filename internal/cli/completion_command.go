package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/cli/completions"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type completionCommand struct{}

// NewCompletion builds the `completion <shell>` command: it emits an
// in-repo, registry-wired shell completion script. This is deliberately our
// own command rather than unhiding urfave's auto-appended built-in - that
// built-in bypasses reg.Register entirely, so it would never appear in
// `rancher schema`, and an advertised-surface CLI cannot have a load-bearing
// command its own manifest denies exists.
func newCompletion() runtime.Command { return completionCommand{} }

func (completionCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "completion",
		Usage: "Print a shell completion script (ignores -o; always the raw script)",
		Args: []runtime.ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: runtime.ShellCompletionShells()},
		},
		Examples: []string{
			"source <(rancher completion bash)",
			"rancher completion fish > ~/.config/fish/completions/rancher.fish",
		},
		CommandType: runtime.Local,
		// The shell script is a bare byte stream that never varies with -o;
		// every format but the default table would silently accept and then
		// ignore it.
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table"}},
		Shape:        runtime.ShapeBytes,
	}
}

// Execute renders the requested shell's script verbatim (RawBytes: no
// envelope, identical output under every -o). shell is guaranteed present,
// non-empty, and one of runtime.ShellCompletionShells() here - both the
// missing-arg and unknown-shell cases are the free Required/Enum VALIDATION
// in runtime.validateInput, before Execute ever runs.
func (completionCommand) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	script, _ := completions.Script(in.Args()[0])
	return runtime.Result{RawBytes: script}, nil
}

// CompleteArgs implements runtime.ShellCompleter: the completion command
// completes its own shell argument. view is unused - the shell list is fixed.
func (completionCommand) CompleteArgs(_ context.Context, _ runtime.ConfigView, _ []string) []string {
	return runtime.ShellCompletionShells()
}
