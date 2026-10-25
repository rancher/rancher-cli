package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// recordingEnv makes the environment one recorded run reads: new temporary
// HOME, USERPROFILE and EDITOR, the file credential store, a config
// directory that exists, recording turned on, and the server and the
// command line the process keeps reset when the test ends. It returns the
// config directory. The test must not call t.Parallel(): the settings it
// makes are process-wide.
func recordingEnv(tb testing.TB) string {
	tb.Helper()
	restoreArgv(tb)
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("USERPROFILE", home)
	tb.Setenv("XDG_CONFIG_HOME", home)
	tb.Setenv("EDITOR", "rancher-test-no-such-editor")
	tb.Setenv("VISUAL", "rancher-test-no-such-editor")
	tb.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	tb.Setenv(telemetry.EnvTelemetry, "")
	dir := tb.TempDir()
	tb.Setenv(config.EnvConfigDir, dir)
	tb.Chdir(tb.TempDir())
	tb.Cleanup(telemetry.SetEnabledForTest())
	tb.Cleanup(telemetry.ResetServerForTest)
	tb.Cleanup(resetRecordingState)
	return dir
}

// restoreArgv puts the captured command line back when the test ends.
func restoreArgv(tb testing.TB) {
	tb.Helper()
	orig, origW, origP := rawArgv, rawCurrentWord, rawCompleting
	tb.Cleanup(func() { rawArgv, rawCurrentWord, rawCompleting = orig, origW, origP })
}

// writeConfig writes text as the config.yaml of dir.
func writeConfig(tb testing.TB, dir, text string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(dir, 0o700))
	require.NoError(tb, os.WriteFile(config.FilePath(dir), []byte(text), 0o600))
}

// serverConfig is a config.yaml with the named servers and the current one.
func serverConfig(current string, servers map[string]string) string {
	var b strings.Builder
	if current != "" {
		b.WriteString("currentServer: " + current + "\n")
	}
	b.WriteString("servers:\n")
	for name, url := range servers {
		b.WriteString("  " + name + ":\n    url: " + url + "\n")
	}
	return b.String()
}

// recordedRun is what one run of a command line wrote and returned.
type recordedRun struct {
	stdout, stderr string
	res            runResult
}

// runRecorded runs argv, the words after the program name, through run on root
// with captured output and returns what it wrote and how it ended.
func runRecorded(tb testing.TB, root *ucli.Command, argv ...string) recordedRun {
	tb.Helper()
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
	ctx, cancel := context.WithTimeout(tb.Context(), 60*time.Second)
	defer cancel()
	res := run(ctx, root, append([]string{"rancher"}, argv...))
	return recordedRun{stdout: out.String(), stderr: errOut.String(), res: res}
}

// newRoot builds the command tree.
func newRoot(tb testing.TB) *ucli.Command {
	tb.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(tb, err)
	return root
}

// storedRuns returns the command run events stored in dir, oldest first.
func storedRuns(tb testing.TB, dir string) []telemetry.StoredEvent {
	tb.Helper()
	if _, err := os.Stat(filepath.Join(dir, "telemetry")); err != nil {
		return nil
	}
	s, err := telemetry.OpenStore(dir)
	require.NoError(tb, err)
	defer func() { _ = s.Close() }()
	all, err := s.List()
	require.NoError(tb, err)
	var runs []telemetry.StoredEvent
	for _, ev := range all {
		if ev.Name == telemetry.EventCommandRun {
			runs = append(runs, ev)
		}
	}
	return runs
}

// storedFlags returns the flags of a stored command run event.
func storedFlags(ev telemetry.StoredEvent) []string {
	var out []string
	if list, ok := ev.Fields["io.rancher.cli.command.flags"].([]any); ok {
		for _, v := range list {
			out = append(out, v.(string))
		}
	}
	return out
}

// countBuilds replaces the event builder with one that counts its calls,
// and returns the counter; the builder is put back when the test ends.
func countBuilds(tb testing.TB) *int {
	tb.Helper()
	n := new(int)
	orig := newRunEvent
	newRunEvent = func(in telemetry.RunInput) telemetry.CommandRun {
		*n++
		return orig(in)
	}
	tb.Cleanup(func() { newRunEvent = orig })
	return n
}

// observeFiles collects the telemetry file operations until the test ends.
func observeFiles(tb testing.TB) *[]string {
	tb.Helper()
	var ops []string
	tb.Cleanup(telemetry.SetObserverForTest(func(op string) { ops = append(ops, op) }))
	return &ops
}

// contextWithCancel returns a context that is cancelled when the test ends
// or the returned function is called.
func contextWithCancel(tb testing.TB) (context.Context, context.CancelFunc) {
	tb.Helper()
	ctx, cancel := context.WithCancel(tb.Context())
	tb.Cleanup(cancel)
	return ctx, cancel
}

// runWithContext is runRecorded with the context of the caller.
func runWithContext(tb testing.TB, ctx context.Context, root *ucli.Command, argv ...string) recordedRun {
	tb.Helper()
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
	res := run(ctx, root, append([]string{"rancher"}, argv...))
	return recordedRun{stdout: out.String(), stderr: errOut.String(), res: res}
}

// seedCachedHit writes a config with prod as its current server, at the
// server's URL, and the cached credentials the exec plugin serves for the
// cluster c-1 without a request: the plugin's, under the name prod, and the
// legacy token command's, for the user kube.
func seedCachedHit(tb testing.TB, dir, server string) {
	tb.Helper()
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: server, CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(tb, cfg.Write())
	store := config.NewFileStore(dir)
	canon := config.CanonicalDir(dir)
	identity := config.CanonicalServerIdentity(server)
	expiry := time.Now().Add(time.Hour)
	plugin := config.CacheKey("prod", identity, "c-1")
	require.NoError(tb, store.SetCachedCred(plugin, "cached-tok", expiry, "prod", server, "c-1",
		config.CacheBindingForSeedOf(canon, cfg, "prod", server, "c-1", "", "")))
	shim := config.LegacyCachePrefix + "kube"
	require.NoError(tb, store.SetCachedCred(config.CacheKeyFor(shim, identity, "c-1", "", ""), "cached-tok", expiry, shim, server, "c-1",
		config.CacheBindingForServer(canon, cfg, server, true)))
}
