package cli_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// completionServer points the config at a counting server and sets network
// completion as asked. It returns the request count. Every response is an
// empty list.
func completionServer(t *testing.T, network bool) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// The project completion reads the steve collection of the
		// cluster's projects; every other request gets a plain list.
		if r.URL.Path == "/v1/management.cattle.io.projects/c-abc" {
			_, _ = w.Write([]byte(`{"count":1,"revision":"1","data":[{"id":"c-abc/p-xyz","metadata":{"name":"p-xyz"},"spec":{"clusterName":"c-abc","displayName":"prod"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"p-xyz"},"spec":{"displayName":"prod"}}]}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: network},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvToken, "tok")
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	cli.SetStoredConfigForTest(t, cfg, dir)
	return &hits
}

// completionLevels returns the argv path of the root (empty), of every group
// and of every leaf in the built tree, so a command added later is covered.
func completionLevels(t *testing.T) (root *ucli.Command, groups, leaves [][]string) {
	t.Helper()
	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	var walk func(c *ucli.Command, path []string)
	walk = func(c *ucli.Command, path []string) {
		for _, sub := range c.Commands {
			if sub.Hidden {
				continue
			}
			p := append(slices.Clone(path), sub.Name)
			if len(sub.VisibleCommands()) == 0 {
				leaves = append(leaves, p)
				continue
			}
			groups = append(groups, p)
			walk(sub, p)
		}
	}
	walk(root, nil)
	return root, groups, leaves
}

// TestCompletionRejectedAndAcceptedLinesAtEveryLevel asserts, end to end
// through urfave, for the root, every group and every leaf of the built tree,
// that a line with a flag the level does not accept (`--bogus`) offers
// nothing, and that lines made only of flags the level accepts (a global flag
// with a value, --help) keep the candidates of the line without them. The
// candidates compared are the flag names of the level (`<TAB>` after `--`),
// which every level lists with network completion off, so a comparison is
// never empty against empty. The decision for every spelling of a rejected
// or accepted line is asserted on the tree in TestLineRejectedAtEveryLevel;
// here the rows that prove the decision reaches urfave's completion at each
// level are run, and the version flag (accepted at the root only) and the
// words a bare `-` and `-1` (a leaf keeps its candidates, the root and
// groups offer nothing) are run for the root, every group and the first leaf
// of each group. NOT parallel: mutates rawArgv and env.
func TestCompletionRejectedAndAcceptedLinesAtEveryLevel(t *testing.T) {
	completionServer(t, false)
	// A word that names no command reaches urfave's exit function.
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	t.Cleanup(func() { ucli.OsExiter = origExiter })
	_, groups, leaves := completionLevels(t)
	require.NotEmpty(t, groups)
	require.NotEmpty(t, leaves)
	levels := append(append([][]string{nil}, groups...), leaves...)
	flagNames := func(words []string) []string {
		return extractTokens(runNameLikeCompletion(t, words, "--"))
	}

	// The first leaf of each group, for the rows that do not depend on the
	// leaf: they run through the same completion function for every leaf.
	var sampleLeaves [][]string
	seen := map[string]bool{}
	for _, leaf := range leaves {
		if !seen[leaf[0]] {
			seen[leaf[0]] = true
			sampleLeaves = append(sampleLeaves, leaf)
		}
	}
	sampled := append(append([][]string{nil}, groups...), sampleLeaves...)

	for _, path := range levels {
		line := append(slices.Clone(path), "--bogus")
		assert.Empty(t, extractTokens(runNameLikeCompletion(t, line, "")), "rancher %v <TAB>: an undefined flag rejects the line", line)
		want := flagNames(path)
		require.NotEmpty(t, want, "rancher %v --<TAB> lists the flags of the level", path)
		for _, typed := range [][]string{{"-o", "json"}, {"--help"}} {
			line := append(slices.Clone(path), typed...)
			assert.Equal(t, want, flagNames(line), "rancher %v --<TAB>", line)
		}
	}

	for _, path := range sampled {
		atRoot := len(path) == 0
		isLeaf := slices.ContainsFunc(sampleLeaves, func(l []string) bool { return slices.Equal(l, path) })
		want := flagNames(path)
		for _, typed := range [][]string{{"-"}, {"-1"}} {
			line := append(slices.Clone(path), typed...)
			if isLeaf {
				assert.Equal(t, want, flagNames(line), "rancher %v --<TAB>: a word at a leaf keeps its candidates", line)
			} else {
				assert.Empty(t, flagNames(line), "rancher %v --<TAB>: a word names no command", line)
			}
		}
		for _, local := range [][]string{{"--version"}, {"-v"}} {
			line := append(slices.Clone(path), local...)
			got := flagNames(line)
			if atRoot {
				assert.Equal(t, want, got, "rancher %v --<TAB>: the root accepts the version flag", line)
			} else {
				assert.Empty(t, got, "rancher %v --<TAB>: only the root accepts the version flag", line)
			}
		}
	}
}

// TestCompletionV1ProtocolLeafKeepsPartialFlagNames asserts that under the v1
// protocol (no sentinel, so the argv still holds the partial word being
// completed) a leaf still completes a partial flag name: the partial word
// must not read as an undefined flag. NOT parallel: mutates rawArgv and env.
func TestCompletionV1ProtocolLeafKeepsPartialFlagNames(t *testing.T) {
	completionServer(t, false)
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{"get", "pods", "--clu"}, "--cluster"},
		{[]string{"get", "pods", "--cluster", "c1", "--na"}, "--namespace"},
	} {
		argv := append(slices.Clone(tc.words), "--generate-shell-completion")
		origV1 := cli.GetRawV1CompletionForTest()
		t.Cleanup(func() { cli.SetRawV1CompletionForTest(origV1) })
		cli.CaptureArgvForTest(argv)
		root, err := cli.BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = io.Discard
		_ = root.Run(t.Context(), append([]string{"rancher"}, argv...))
		assert.Contains(t, out.String(), tc.want, "rancher %v <TAB> under the v1 protocol", tc.words)
	}
}

// TestCompletionRejectedLineMakesNoRequest asserts that a line with a flag
// the level does not accept makes no HTTP request even when it ends at a
// flag with a live source: at a group (`get --cluster=c-abc --project`, the
// flag the group rejects names the cluster the project list needs) and at a
// leaf (`get pods --cluster c-abc --bogus --project`). The same leaf line
// without the undefined flag does fetch, so the zero is the rejection's.
// NOT parallel: mutates rawArgv and env.
func TestCompletionRejectedLineMakesNoRequest(t *testing.T) {
	hits := completionServer(t, true)

	hits.Store(0)
	got := runNameLikeCompletion(t, []string{"get", "pods", "--cluster", "c-abc", "--project"}, "")
	assert.Contains(t, got, "p-xyz", "the accepted line completes projects live")
	assert.Positive(t, hits.Load(), "the accepted line makes a request")

	for _, words := range [][]string{
		{"get", "--cluster=c-abc", "--project"},
		{"get", "--cluster", "c-abc", "--project"},
		{"get", "pods", "--cluster", "c-abc", "--bogus", "--project"},
	} {
		hits.Store(0)
		got := runNameLikeCompletion(t, words, "")
		assert.Empty(t, extractTokens(got), "rancher %v <TAB>", words)
		assert.Zero(t, hits.Load(), "rancher %v <TAB> makes no request", words)
	}
}
