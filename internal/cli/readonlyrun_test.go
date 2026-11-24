package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaReadOnlyOf decodes the read-only state `schema -o json` reports:
// whether read-only mode is on, its sources, and whether the
// readOnlySources key is present at all.
func schemaReadOnlyOf(t *testing.T, stdout []byte) (readOnly bool, sources []string, hasSources bool) {
	t.Helper()
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env), string(stdout))
	raw, ok := env.Data["readOnly"]
	require.True(t, ok, "the schema output has no readOnly field")
	require.NoError(t, json.Unmarshal(raw, &readOnly))
	raw, hasSources = env.Data["readOnlySources"]
	if hasSources {
		require.NoError(t, json.Unmarshal(raw, &sources))
	}
	return readOnly, sources, hasSources
}

// readOnlyRunEnv clears the environment source of read-only mode and keeps
// credentials out of the OS keychain. Tests that call it cannot run in
// parallel with others.
func readOnlyRunEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvReadOnly, "")
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
}

// TestParallelRunsReportTheirOwnReadOnlyState asserts that runs started
// together from one command registry each report their own read-only
// state: in each pair one run is read-only, through --read-only or through
// readOnly in its config file, and the other is not, and neither reports
// the state of the other.
func TestParallelRunsReportTheirOwnReadOnlyState(t *testing.T) {
	readOnlyRunEnv(t)
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)

	type run struct {
		root        *Command
		dir         string
		args        []string
		wantSources []string
		stdout      []byte
		stderr      []byte
		err         error
	}
	const pairs = 50
	runs := make([]*run, 0, 2*pairs)
	for i := range pairs {
		readOnly := &run{dir: t.TempDir(), args: []string{"--read-only", "schema", "-o", "json"}, wantSources: []string{"--read-only"}}
		if i%2 == 1 {
			require.NoError(t, os.WriteFile(config.FilePath(readOnly.dir), []byte("readOnly: true\n"), 0o600))
			readOnly.args = []string{"schema", "-o", "json"}
			readOnly.wantSources = []string{"config setting readOnly"}
		}
		plain := &run{dir: t.TempDir(), args: []string{"schema", "-o", "json"}}
		runs = append(runs, readOnly, plain)
	}
	for _, r := range runs {
		r.root = wr.root("v-test")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Go(func() {
			<-start
			r.stdout, r.stderr, r.err = grRun(t, r.root, r.dir, r.args...)
		})
	}
	close(start)
	wg.Wait()

	for i, r := range runs {
		require.NoError(t, r.err, "run %d: %s", i, r.stderr)
		readOnly, sources, hasSources := schemaReadOnlyOf(t, r.stdout)
		if r.wantSources == nil {
			assert.False(t, readOnly, "run %d", i)
			assert.False(t, hasSources, "run %d reports readOnlySources %v", i, sources)
			continue
		}
		assert.True(t, readOnly, "run %d", i)
		assert.Equal(t, r.wantSources, sources, "run %d", i)
	}
}

// TestTwoRunsOfOneRegistryKeepTheirOwnReadOnlyState asserts that the
// read-only state the root's Before resolves stays with its own run. Two
// lines on roots of one command registry run their Before hooks, the
// read-only line first, and only then their commands. For schema, the
// read-only line still reports read-only mode and the other line does not.
// For a mutating command, the read-only line is refused for read-only mode
// and the other line is not.
func TestTwoRunsOfOneRegistryKeepTheirOwnReadOnlyState(t *testing.T) {
	readOnlyRunEnv(t)
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	readOnlyRoot, plainRoot := wr.root("v-test"), wr.root("v-test")

	type run struct {
		p        parsedLine
		out, err bytes.Buffer
	}
	newRun := func(root *Command, words ...string) *run {
		r := &run{}
		root.Writer, root.ErrWriter = &r.out, &r.err
		r.p = parseLine(root, append([]string{"--config", t.TempDir()}, words...))
		return r
	}
	// runPair runs the Before hooks of both lines, then the Action of each,
	// and returns the error of each Action.
	runPair := func(t *testing.T, leafName string, readOnly, plain *run) (readOnlyErr, plainErr error) {
		t.Helper()
		for _, r := range []*run{readOnly, plain} {
			require.NoError(t, runBefore(t.Context(), &r.p, r.p.end.level), r.err.String())
		}
		errs := make([]error, 2)
		for i, r := range []*run{readOnly, plain} {
			leaf := r.p.levels[r.p.end.level].cmd
			require.Equal(t, leafName, leaf.Name)
			errs[i] = leaf.Action(t.Context(), leaf, &Input{p: &r.p, level: r.p.end.level})
		}
		return errs[0], errs[1]
	}

	t.Run("schema", func(t *testing.T) {
		readOnly := newRun(readOnlyRoot, "--read-only", "schema", "-o", "json")
		plain := newRun(plainRoot, "schema", "-o", "json")
		readOnlyErr, plainErr := runPair(t, "schema", readOnly, plain)
		require.NoError(t, readOnlyErr, readOnly.err.String())
		require.NoError(t, plainErr, plain.err.String())

		got, sources, _ := schemaReadOnlyOf(t, readOnly.out.Bytes())
		assert.True(t, got)
		assert.Equal(t, []string{"--read-only"}, sources)
		got, sources, hasSources := schemaReadOnlyOf(t, plain.out.Bytes())
		assert.False(t, got)
		assert.False(t, hasSources, "readOnlySources %v", sources)
	})

	// The read-only check of a mutating command comes before its input is
	// checked: the line that is not read-only fails on its missing input, and
	// no server is needed.
	t.Run("mutating command", func(t *testing.T) {
		readOnly := newRun(readOnlyRoot, "--read-only", "-o", "json", "create", "resource")
		plain := newRun(plainRoot, "-o", "json", "create", "resource")
		readOnlyErr, plainErr := runPair(t, "resource", readOnly, plain)
		require.Error(t, readOnlyErr)
		require.Error(t, plainErr)

		var env errEnvelope
		require.NoError(t, json.Unmarshal(readOnly.err.Bytes(), &env), readOnly.err.String())
		assert.Contains(t, env.Data.Error.Message, "read-only mode: create resource")
		env = errEnvelope{}
		require.NoError(t, json.Unmarshal(plain.err.Bytes(), &env), plain.err.String())
		assert.NotEmpty(t, env.Data.Error.Message)
		assert.NotContains(t, env.Data.Error.Message, "read-only mode")
	})
}
