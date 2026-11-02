package token

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// failedDeleteTokenOutput runs auth delete-token --all whose local delete of
// the cache fails after the config write, for an invocation whose remedies
// name remedyDir, and returns what it wrote to standard error in format and
// the config directory the command ran against.
func failedDeleteTokenOutput(t *testing.T, format, remedyDir string) (output, configDir string) {
	t.Helper()
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.cache("ct", x.token("alice"), x.own("prod"), true)
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), NewDeleteTokenCommand(x.dir, failingSecondHold{CredStore: x.store}), tin{b: map[string]bool{"all": true}},
		runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: remedyDir}, safety.Binding{})
	require.NotNil(t, ce)
	return errBuf.String(), x.dir
}

// decodedErrorDocument decodes the json or yaml error document: its message
// and the recovery list of each notRevoked item of its details.
func decodedErrorDocument(t *testing.T, document string) (message string, recoveries [][]string) {
	t.Helper()
	var doc struct {
		Data struct {
			Error struct {
				Message string `json:"message"`
				Details struct {
					NotRevoked []struct {
						Recovery []string `json:"recovery"`
					} `json:"notRevoked"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc), "%q", document)
	for _, n := range doc.Data.Error.Details.NotRevoked {
		recoveries = append(recoveries, n.Recovery)
	}
	return doc.Data.Error.Message, recoveries
}

// notRevokedKeys is, for each notRevoked item of the error document, the keys
// the item holds.
func notRevokedKeys(t *testing.T, document string) []map[string]any {
	t.Helper()
	var doc struct {
		Data struct {
			Error struct {
				Details struct {
					NotRevoked []map[string]any `json:"notRevoked"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc), "%q", document)
	return doc.Data.Error.Details.NotRevoked
}

// TestAFailedDeleteTokenPayloadNamesTheDirectoryOfTheInvocation asserts the
// message and the notRevoked recovery of an auth delete-token whose local
// delete failed, decoded from the json and yaml error documents, are the
// delete to run again: with a config directory that is not the default they
// name --config and the directory, in the data and in the text output; with
// the default directory they name none; with a directory a terminal cannot
// show the recovery list keeps its length and holds an empty command, and the
// message keeps the fact before the command.
func TestAFailedDeleteTokenPayloadNamesTheDirectoryOfTheInvocation(t *testing.T) {
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)
	const rerun = "auth delete-token --all"

	for _, format := range []string{"json", "yaml"} {
		t.Run(format+" in a directory that is not the default", func(t *testing.T) {
			got, _ := failedDeleteTokenOutput(t, format, remedyDir)
			message, recoveries := decodedErrorDocument(t, got)
			assert.Equal(t, [][]string{{"rancher" + prefix + " " + rerun}}, recoveries)
			items := notRevokedKeys(t, got)
			require.Len(t, items, 1)
			assert.NotContains(t, items[0], "loginAs", "the recovery needs no login")
			assert.Contains(t, message, "to finish, run: rancher"+prefix+" "+rerun)
			assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", got)
		})
		t.Run(format+" in the default directory", func(t *testing.T) {
			got, _ := failedDeleteTokenOutput(t, format, "")
			message, recoveries := decodedErrorDocument(t, got)
			assert.Equal(t, [][]string{{"rancher " + rerun}}, recoveries)
			assert.Contains(t, message, "to finish, run: rancher "+rerun)
			assert.False(t, slices.ContainsFunc(decodedStringsOf(t, got), func(s string) bool { return strings.Contains(s, "--config") }),
				"no string of the document names a config directory")
		})
		t.Run(format+" in a directory a terminal cannot show", func(t *testing.T) {
			got, configDir := failedDeleteTokenOutput(t, format, "/d\x01")
			message, recoveries := decodedErrorDocument(t, got)
			assert.Equal(t, [][]string{{""}}, recoveries)
			assert.NotContains(t, message, "rancher")
			assert.Contains(t, message, "failed to delete", "the fact before the command is kept")
			assert.Contains(t, message, runtime.UnprintableConfigDirNote)
			assert.False(t, slices.ContainsFunc(decodedStringsOf(t, got), func(s string) bool { return strings.Contains(s, configDir) }),
				"no string of the document names the directory the command ran against")
		})
	}

	t.Run("text output", func(t *testing.T) {
		got, _ := failedDeleteTokenOutput(t, "", remedyDir)
		assert.Contains(t, got, "to finish, run: rancher"+prefix+" "+rerun)
		assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", got)
	})
}
