package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// fuzzHermetic gives a fuzz target the environment its inputs run in: a temporary home, working
// directory and config directory, the file credential store, a mock keychain, no telemetry, and
// none of the other RANCHER_CLI_* and color variables. It returns the config directory.
// The command line the process captured is put back when the target ends.
func fuzzHermetic(tb testing.TB) (configDir string) {
	tb.Helper()
	argv, word, completing := rawArgv, rawCurrentWord, rawCompleting
	tb.Cleanup(func() { rawArgv, rawCurrentWord, rawCompleting = argv, word, completing })
	home, configDir := tb.TempDir(), tb.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		tb.Setenv(name, home)
	}
	tb.Chdir(tb.TempDir())
	for _, entry := range testfs.EnvWithoutGit() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RANCHER_CLI_") {
			tb.Setenv(name, "")
			require.NoError(tb, os.Unsetenv(name))
		}
	}
	for _, name := range []string{"NO_COLOR", "CLICOLOR_FORCE", "FORCE_COLOR", "KUBERNETES_EXEC_INFO"} {
		tb.Setenv(name, "")
		require.NoError(tb, os.Unsetenv(name))
	}
	tb.Setenv(config.EnvConfigDir, configDir)
	tb.Setenv(config.EnvCredentialStore, "file")
	tb.Setenv(telemetry.EnvTelemetry, "false")
	keyring.MockInit()
	return configDir
}

// fuzzNoStoredConfig clears the config that completion reads, so no input sends a live request,
// and puts it back when the target ends.
func fuzzNoStoredConfig(tb testing.TB) {
	tb.Helper()
	kept := storedConfigSnapshot()
	view, _ := loadedConfigView()
	dir := view.ConfigDir()
	tb.Cleanup(func() { setStoredConfig(kept, dir) })
	setStoredConfig(nil, "")
}

// fuzzGoldenLines returns the line of every row of the golden directories named in dirs (parse or
// candidates, under testdata), once each. When perFile is above zero, only the first perFile rows
// of each file count.
// A line of a candidates file has its protocol words (the marker, the current word and the flag
// that asks for candidates) removed when stripProtocol is true.
func fuzzGoldenLines(tb testing.TB, stripProtocol bool, perFile int, dirs ...string) [][]string {
	tb.Helper()
	var lines [][]string
	seen := map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join("testdata", dir, "*.jsonl"))
		require.NoError(tb, err)
		for _, file := range files {
			data, err := os.ReadFile(file)
			require.NoError(tb, err)
			rows := 0
			for raw := range strings.SplitSeq(string(data), "\n") {
				if raw == "" {
					continue
				}
				if rows++; perFile > 0 && rows > perFile {
					break
				}
				var row struct {
					Line []string `json:"line"`
				}
				require.NoError(tb, json.Unmarshal([]byte(raw), &row))
				line := row.Line
				if stripProtocol && completionRequest(line) {
					words, _ := splitCompletionLine(line)
					line = words
				}
				key := strings.Join(line, "\x00") + "\x01" + strconv.FormatBool(line == nil)
				if !seen[key] {
					seen[key] = true
					lines = append(lines, line)
				}
			}
		}
	}
	return lines
}

// splitCompletionLine returns the words of a completion request line without its protocol
// words, and the word being completed.
func splitCompletionLine(line []string) (words []string, current string) {
	for _, w := range line {
		switch {
		case w == protoSentinel, w == completionFlag:
		case strings.HasPrefix(w, curPrefix):
			current = strings.TrimPrefix(w, curPrefix)
		default:
			words = append(words, w)
		}
	}
	return words, current
}

// fuzzSplit returns the words an argsBlob and its presence flag stand for.
func fuzzSplit(argsBlob string, hasArgs bool) []string {
	if !hasArgs {
		return nil
	}
	return strings.Split(argsBlob, "\x00")
}

// fuzzJoin is the inverse of fuzzSplit for a seed.
func fuzzJoin(words []string) (argsBlob string, hasArgs bool) {
	return strings.Join(words, "\x00"), words != nil
}

// fuzzHasConfigWord reports whether a word is a spelling of --config, which names a directory
// the command would read or write.
func fuzzHasConfigWord(words []string) bool {
	for _, w := range words {
		name, _, _ := strings.Cut(w, "=")
		if name == "--config" || name == "-config" || name == "-c" {
			return true
		}
	}
	return false
}
