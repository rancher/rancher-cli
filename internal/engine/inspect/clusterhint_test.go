package inspect

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// TestRegistrationHintNamesTheConfigDirectoryInJSON asserts the hint of the
// withheld registration section of inspect cluster, written as json for a run
// against a config directory, is the get command with --config and the
// directory after the word rancher and no character of the remedy marker, and
// that the text output prints the withheld line with no directory.
func TestRegistrationHintNamesTheConfigDirectoryInJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)

	client, root := inspectClusterFixture(t)
	run := func(format string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
		_, ce := runtime.Run(t.Context(), cmd, input{args: []string{"c-abc"}},
			runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: dir}, safety.Binding{})
		require.Nil(t, ce)
		return out.String()
	}

	var doc struct {
		Data struct {
			Registration struct {
				Hint string `json:"hint"`
			} `json:"registration"`
		} `json:"data"`
	}
	jsonOut := run("json")
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &doc))
	assert.Equal(t, "rancher"+prefix+" get clusterregistrationtoken default-token --namespace 'c-abc'", doc.Data.Registration.Hint)

	text := run("")
	assert.Contains(t, text, "registration: left out (contains the cluster registration token)")
	assert.NotContains(t, text, "--config")
}
