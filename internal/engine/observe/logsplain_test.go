package observe

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// TestLogsTextPrintsEveryLineAsTheContainerWroteIt asserts the text of
// `logs`, which is the output of a program in a container, prints a line
// that holds this process's marker before the word rancher as the plain
// command, never with --config and the directory, for a config directory
// that can be printed and for one that cannot; a text with U+2063 and the
// word rancher stays as it is, and so does every other line.
func TestLogsTextPrintsEveryLineAsTheContainerWroteIt(t *testing.T) {
	t.Parallel()
	log := clierr.Rancher + " get pods\n\u2063rancher delete x\nordinary line\n"
	for name, dir := range map[string]string{"a printable directory": t.TempDir(), "an unprintable directory": "/home/u/a\nb"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			downstream := newFakeClient().
				withListNS(podKind.GVR, "cattle-system", podList("agent-1")).
				withLog("main", log)
			factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
			in := input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}}

			var out, errBuf bytes.Buffer
			_, ce := runtime.Run(t.Context(), NewLogs(factory, factory), in, runtime.IO{Out: &out, Err: &errBuf, RemedyDir: dir}, safety.Binding{})
			require.Nil(t, ce)
			got := out.String()
			assert.Contains(t, got, "[agent-1/main] rancher get pods\n")
			assert.Contains(t, got, "[agent-1/main] \u2063rancher delete x\n")
			assert.Contains(t, got, "[agent-1/main] ordinary line\n")
			assert.NotContains(t, got, "--config")
			assert.NotContains(t, got, clierr.RemedyMarker)
			assert.NotContains(t, got, runtime.UnprintableConfigDirNote)
		})
	}
}
