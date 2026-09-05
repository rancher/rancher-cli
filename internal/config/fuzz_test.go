package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzConfigLoad fuzzes Load against arbitrary file bytes. One worker-private
// temp file is created once, outside f.Fuzz, and overwritten on every
// iteration - no per-iteration TempDir or cleanup accumulation. Setup I/O
// failures fail distinctly (t.Fatal), never as parser findings. Never call
// t.Parallel() here - it panics inside f.Fuzz.
func FuzzConfigLoad(f *testing.F) {
	path := FilePath(f.TempDir())

	f.Add("")
	f.Add("currentServer: prod\nservers:\n  prod:\n    url: https://example.com\n")
	f.Add("not: valid: yaml: [")
	f.Add("servers: []\n")
	f.Add("readOnly: true\n")
	f.Add("\x00\x01\x02")
	f.Add("currentServer: \"" + strings.Repeat("a", 4096) + "\"\n")

	f.Fuzz(func(t *testing.T, data string) {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("setup: writing fuzzed config: %v", err)
		}

		cfg, err := Load(path) // (a): no panic
		require.NotNil(t, cfg, "Load must return a non-nil Config alongside an error")

		if err != nil {
			return
		}

		// (b) "safe to re-save": Write succeeds and a reload of the
		// rewritten file succeeds and is usable.
		require.NoError(t, cfg.Write())
		reloaded, err := Load(path)
		require.NoError(t, err)
		require.NotNil(t, reloaded)
		assert.NotNil(t, reloaded.Servers)
	})
}
