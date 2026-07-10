package commands

import (
	"context"
	"os"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configPathCmd struct{ dir string }

// NewConfigPath builds `config path` (local, no network/auth): prints the
// resolved config file path — the agent-usable "where do I point tooling"
// primitive. dir is already the flag > RANCHER_CONFIG_DIR > XDG-default
// resolution (config.ResolveDir, run once by the CLI adapter's resolveDeps,
// the single place every config command's dir comes from); a ResolveDir
// failure there aborts before this command's Execute ever runs, so its
// error is never swallowed. Prints the path whether or not the file exists
// yet — Load treats a missing file as an empty, usable config, so the path
// is still the right answer either way.
func NewConfigPath(dir string) runtime.Command { return &configPathCmd{dir: dir} }

func (configPathCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config path",
		Usage:       "Print the resolved config file path",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config path"},
		Toolset:     "core",
	}
}

func (c configPathCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	path := config.FilePath(c.dir)
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return pathResult(path, true), nil
	case os.IsNotExist(statErr):
		return pathResult(path, false), nil
	default:
		return runtime.Result{}, statErr
	}
}

func pathResult(path string, exists bool) runtime.Result {
	return runtime.Result{
		Kind: "ConfigPath",
		Data: map[string]any{"path": path, "exists": exists},
		Text: path + "\n",
	}
}
