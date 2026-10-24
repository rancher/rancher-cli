package cli

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lineOutcome is what one command line did: what it wrote, the exit code
// the process would have, and the code and message of the error envelope
// on stderr (empty when stderr holds none).
type lineOutcome struct {
	stdout, stderr string
	exit           int
	code, message  string
}

// runLine runs argv, which carries -o json when an envelope is wanted,
// through a fresh root. The exit code is the one Run returns.
func runLine(t *testing.T, argv []string) lineOutcome {
	t.Helper()
	stdout, stderr, code := runHelpArgvAsMain(t, argv...)
	out := lineOutcome{stdout: stdout, stderr: stderr, exit: code}
	if stderr != "" {
		var env errEnvelope
		if json.Unmarshal([]byte(stderr), &env) == nil {
			out.code, out.message = env.Data.Error.Code, env.Data.Error.Message
		}
	}
	return out
}

// TestUnknownCommandIsReportedBeforeAnythingAfterIt asserts, for the root
// and every command with subcommands, that a first word the command does
// not know is reported as an unknown command whatever is typed after it (a
// flag the command does not define, -A, --version, -v), that an empty
// first word is reported the same way instead of showing help, that a
// blank first word keeps being reported, and that --help before an empty
// word still shows the command's own help. Every command in the tree must
// stop flag parsing after its first word, which is what makes the first
// word win. Not parallel: the captured argv is process-wide.
func TestUnknownCommandIsReportedBeforeAnythingAfterIt(t *testing.T) {
	restoreRawArgv(t)

	nodes := levelCommands(t)
	paths := []string{}
	for _, path := range slices.Sorted(maps.Keys(nodes)) {
		if path == "" || len(nodes[path].VisibleCommands()) > 0 {
			paths = append(paths, path)
		}
	}
	require.Contains(t, paths, "")
	require.Greater(t, len(paths), 1)

	rows := []struct {
		words   []string
		message string
	}{
		{[]string{"bogusword", "--madeupflag", "v"}, `unknown command "bogusword"`},
		{[]string{"bogusword", "-A"}, `unknown command "bogusword"`},
		{[]string{"bogusword", "--version"}, `unknown command "bogusword"`},
		{[]string{"bogusword", "-v"}, `unknown command "bogusword"`},
		{[]string{""}, `unknown command ""`},
		{[]string{"", "--madeupflag", "v"}, `unknown command ""`},
		{[]string{"", "pod"}, `unknown command ""`},
		{[]string{"", "--help"}, `unknown command ""`},
		{[]string{" "}, `unknown command " "`},
		{[]string{"--help", ""}, ""},
	}

	for _, path := range paths {
		node := nodes[path]
		name := path
		if path == "" {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("stops flag parsing after the first word", func(t *testing.T) {
				if assert.NotNil(t, node.StopOnNthArg) {
					assert.Equal(t, 1, *node.StopOnNthArg)
				}
			})

			prefix := append([]string{"-o", "json"}, strings.Fields(path)...)
			usageName := strings.TrimSpace("rancher " + path)
			usage := regexp.MustCompile(`(?m)^USAGE:\n\s+` + regexp.QuoteMeta(usageName) + `( |$)`)
			for _, row := range rows {
				quoted := make([]string, len(row.words))
				for i, word := range row.words {
					quoted[i] = strconv.Quote(word)
				}
				t.Run(strings.Join(quoted, " "), func(t *testing.T) {
					got := runLine(t, append(slices.Clone(prefix), row.words...))
					if row.message == "" {
						assert.Equal(t, 0, got.exit)
						assert.Empty(t, got.stderr)
						assert.Regexp(t, usage, got.stdout)
						return
					}
					assert.Equal(t, 2, got.exit)
					assert.Empty(t, got.stdout)
					assert.Equal(t, "VALIDATION", got.code, "stderr: %s", got.stderr)
					assert.Equal(t, row.message, got.message)
				})
			}
		})
	}
}

// TestKnownCommandLinesKeepTheirResult asserts the result of command lines
// that name a known command first, or end flag parsing before any word, so
// that stopping flag parsing after an unknown first word leaves them as
// they were: flag errors from a leaf and from before the first word, the
// -A detail, a leaf run after --, an unknown word after --, the version, the
// help cards and the refusal of a bare - before more words. Not parallel:
// the captured argv is process-wide.
func TestKnownCommandLinesKeepTheirResult(t *testing.T) {
	restoreRawArgv(t)

	const flagError = "flag provided but not defined: -madeupflag; pass --help for usage"
	const allNamespaces = "flag provided but not defined: -A; pass --help for usage; there is no all-namespaces flag"
	const getDetail = " - list operations already span all namespaces; use --namespace where the kind accepts one"

	rows := []struct {
		argv    []string
		exit    int
		message string
		stdout  string
	}{
		{[]string{"-o", "json", "get", "pod", "--madeupflag", "v"}, 2, flagError, ""},
		{[]string{"get", "-o", "json", "pod", "--madeupflag", "v"}, 2, flagError, ""},
		{[]string{"-o", "json", "get", "--madeupflag", "v", "pod"}, 2, flagError, ""},
		{[]string{"-o", "json", "get", "-A"}, 2, allNamespaces + getDetail, ""},
		{[]string{"-o", "json", "get", "pod", "-A"}, 2, allNamespaces + getDetail, ""},
		{[]string{"-o", "json", "access", "-A", "list"}, 2, allNamespaces, ""},
		{[]string{"-o", "json", "get", "--", "pod"}, 2, "--cluster is required; Rancher's own namespaces (cattle-system, cattle-tokens, cattle-fleet-system) live on the local cluster, pass --cluster local", ""},
		{[]string{"-o", "json", "get", "bogus", "--", "--madeupflag"}, 2, `unknown command "bogus"`, ""},
		{[]string{"-o", "json", "get", "-", "--madeupflag", "v"}, 2, "'--madeupflag' 'v' after a - on its own; a - on its own ends the command line", ""},
		{[]string{"--version"}, 0, "", `^rancher version v-test\n$`},
		{[]string{"-v"}, 0, "", `^rancher version v-test\n$`},
		{[]string{"get"}, 0, "", `(?m)^USAGE:\n\s+rancher get( |$)`},
		{nil, 0, "", `(?m)^USAGE:\n\s+rancher( |$)`},
	}

	for _, row := range rows {
		name := strings.Join(row.argv, " ")
		if name == "" {
			name = "no words"
		}
		t.Run(name, func(t *testing.T) {
			got := runLine(t, row.argv)
			assert.Equal(t, row.exit, got.exit)
			if row.message != "" {
				assert.Empty(t, got.stdout)
				assert.Equal(t, "VALIDATION", got.code, "stderr: %s", got.stderr)
				assert.Equal(t, row.message, got.message)
				return
			}
			assert.Empty(t, got.stderr)
			assert.Regexp(t, row.stdout, got.stdout)
		})
	}
}
