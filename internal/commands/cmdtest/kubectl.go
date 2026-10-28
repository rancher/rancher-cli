package cmdtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/require"
)

// Kubectl is one exec-plugin process for the entry name against url,
// built the way the CLI builds it: the binding stamp read and the config
// load of the root hook, then resolveDeps' load and lazy store. It
// returns the credential kubectl is handed, or the command's error; a
// failure to set the process up fails the test. Call it from the test's
// own goroutine; RunKubectl is the form for any other.
func Kubectl(t *testing.T, dir, url, name string) (string, error) {
	t.Helper()
	tok, err := RunKubectl(t.Context(), dir, url, name)
	if setup, ok := errors.AsType[*setupError](err); ok {
		require.NoError(t, setup.err)
	}
	return tok, err
}

// RunKubectl is Kubectl for a goroutine other than the test's: every
// error is returned, the process's setup included.
func RunKubectl(ctx context.Context, dir, url, name string) (string, error) {
	anchor := config.NewCacheAnchor(dir)
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return "", &setupError{err}
	}
	_, store, err := config.LoadAndStore(dir, false)
	if err != nil {
		return "", &setupError{err}
	}
	cmd := token.NewTokenCommand(dir, store, anchor.WithConfig(cfg)).(runtime.ReadCommand)
	res, err := cmd.Execute(ctx, Input{Str: map[string]string{"server": url, "name": name, "cluster": "local"}}, ui.Discard())
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(res.Data)
	if err != nil {
		return "", &setupError{err}
	}
	var cred struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return "", &setupError{err}
	}
	return cred.Status.Token, nil
}

// setupError is a failure of the process around the command, as opposed
// to the command's own error.
type setupError struct{ err error }

func (e *setupError) Error() string { return e.err.Error() }
func (e *setupError) Unwrap() error { return e.err }
