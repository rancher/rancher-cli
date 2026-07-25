package commands_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setCompletionInput is the test input type for set_completion_test.go.
// Named distinctly to avoid collision with the fakeInput in completion_test.go
// (package commands, white-box test file). Finding 5.
type setCompletionInput struct {
	args  []string
	flags map[string]string
}

func (f setCompletionInput) String(name string) string { return f.flags[name] }
func (f setCompletionInput) Bool(string) bool          { return false }
func (f setCompletionInput) Strings(string) []string   { return nil }
func (f setCompletionInput) IsSet(string) bool         { return false }
func (f setCompletionInput) Args() []string            { return f.args }

func TestSetCompletionNetwork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := commands.NewSetCompletion(dir).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), setCompletionInput{flags: map[string]string{"network": "true"}}, nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.True(t, cfg.Completion.Network)
}

func TestSetCompletionNetworkFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := commands.NewSetCompletion(dir).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), setCompletionInput{flags: map[string]string{"network": "false"}}, nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.False(t, cfg.Completion.Network)
}

func TestSetCompletionTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := commands.NewSetCompletion(dir).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), setCompletionInput{flags: map[string]string{"timeout": "750ms"}}, nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	// Timeout stored as string duration (finding 6); EffectiveCompletionTimeout parses it.
	assert.Equal(t, "750ms", cfg.Completion.Timeout)
	assert.Equal(t, 750*time.Millisecond, config.EffectiveCompletionTimeout(cfg))
}

func TestSetCompletionTimeoutClampsAt2s(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := commands.NewSetCompletion(dir).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), setCompletionInput{flags: map[string]string{"timeout": "10s"}}, nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "2s", cfg.Completion.Timeout)
	assert.Equal(t, 2*time.Second, config.EffectiveCompletionTimeout(cfg))
}

func TestSetCompletionTimeoutInvalidFallsToDefault(t *testing.T) {
	t.Parallel()
	// EffectiveCompletionTimeout must return 500ms for invalid stored values.
	cfg := &config.Config{Completion: config.CompletionConfig{Timeout: "not-a-duration"}}
	assert.Equal(t, 500*time.Millisecond, config.EffectiveCompletionTimeout(cfg))
}

func TestSetCompletionTimeoutNonPositiveFallsToDefault(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Completion: config.CompletionConfig{Timeout: "-1s"}}
	assert.Equal(t, 500*time.Millisecond, config.EffectiveCompletionTimeout(cfg))
}

func TestSetCompletionDescribeIsLocal(t *testing.T) {
	t.Parallel()
	d := commands.NewSetCompletion("").Describe()
	assert.Equal(t, "config set-completion", d.Name)
	assert.Equal(t, runtime.Local, d.CommandType)
}

// Ensure the Result payload can be decoded (decode-don't-string-match).
func TestSetCompletionResultDecodable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := commands.NewSetCompletion(dir).(runtime.ReadCommand)
	res, err := cmd.Execute(context.Background(), setCompletionInput{flags: map[string]string{"network": "true"}}, nil)
	require.NoError(t, err)
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, true, out["network"])
}
