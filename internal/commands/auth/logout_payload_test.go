package auth

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// failedLogoutOutput runs a logout of prod whose local delete fails after the
// revokes (the credentials file is a directory) for an invocation whose
// remedies name remedyDir, and returns what it wrote to standard error in
// format and the config directory the logout ran against.
func failedLogoutOutput(t *testing.T, format, remedyDir string) (output, configDir string) {
	t.Helper()
	r := newRevokeDir(t, testrancher.Options{})
	creds := config.CredentialsFilePath(r.dir)
	require.NoError(t, os.Remove(creds))
	require.NoError(t, os.Mkdir(creds, 0o700))
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), NewLogout(r.dir), cmdtest.Input{Pos: []string{"prod"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: remedyDir}, safety.Binding{})
	require.NotNil(t, ce)
	return errBuf.String(), r.dir
}

// errorDocument decodes the json or yaml error document.
func errorDocument(t *testing.T, document string) (message string, details map[string]any) {
	t.Helper()
	var doc struct {
		Data struct {
			Error struct {
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc), "%q", document)
	require.NotEmpty(t, doc.Data.Error.Details, "%q", document)
	return doc.Data.Error.Message, doc.Data.Error.Details
}

// notRevokedItems returns the notRevoked items of decoded details.
func notRevokedItems(t *testing.T, details map[string]any) []map[string]any {
	t.Helper()
	list, ok := details["notRevoked"].([]any)
	require.True(t, ok, "%v", details)
	var out []map[string]any
	for _, item := range list {
		m, ok := item.(map[string]any)
		require.True(t, ok, "%v", item)
		out = append(out, m)
	}
	return out
}

// recoveryOf returns the recovery list of one notRevoked item.
func recoveryOf(t *testing.T, item map[string]any) []string {
	t.Helper()
	var list []string
	recovery, _ := item["recovery"].([]any)
	for _, c := range recovery {
		s, ok := c.(string)
		require.True(t, ok, "%v", c)
		list = append(list, s)
	}
	return list
}

// decodedStrings returns every string of a decoded document, keys and values.
func decodedStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, decodedStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range x {
			out = append(out, k)
			out = append(out, decodedStrings(e)...)
		}
		return out
	}
	return nil
}

// decodedHolds reports whether a decoded document has a string with text in it.
func decodedHolds(t *testing.T, document, text string) bool {
	t.Helper()
	var doc any
	require.NoError(t, yaml.Unmarshal([]byte(document), &doc), "%q", document)
	return slices.ContainsFunc(decodedStrings(doc), func(s string) bool { return strings.Contains(s, text) })
}

// TestAFailedLogoutPayloadNamesTheDirectoryOfTheInvocation asserts the
// notRevoked recovery of a logout whose local delete failed, decoded from the
// json and yaml error documents, is the logout to run again: with a config
// directory that is not the default it names --config and the directory, in
// the data and in the text output; with the default directory it names none;
// with a directory a terminal cannot show it keeps the list and its length and
// holds an empty command, the message keeps the fact before the command, and
// no detail names the directory the logout ran against. The
// item names no user to log in as: the recovery needs no login.
func TestAFailedLogoutPayloadNamesTheDirectoryOfTheInvocation(t *testing.T) {
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)
	const rerun = "auth logout 'prod'"

	for _, format := range []string{"json", "yaml"} {
		t.Run(format+" in a directory that is not the default", func(t *testing.T) {
			got, _ := failedLogoutOutput(t, format, remedyDir)
			_, details := errorDocument(t, got)
			items := notRevokedItems(t, details)
			require.Len(t, items, 1)
			assert.Equal(t, []string{"rancher" + prefix + " " + rerun}, recoveryOf(t, items[0]))
			assert.NotContains(t, items[0], "loginAs", "the recovery needs no login")
			assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", got)
		})
		t.Run(format+" in the default directory", func(t *testing.T) {
			got, _ := failedLogoutOutput(t, format, "")
			_, details := errorDocument(t, got)
			items := notRevokedItems(t, details)
			require.Len(t, items, 1)
			assert.Equal(t, []string{"rancher " + rerun}, recoveryOf(t, items[0]))
			assert.False(t, decodedHolds(t, got, "--config"), "no string of the document names a config directory")
		})
		t.Run(format+" in a directory a terminal cannot show", func(t *testing.T) {
			got, configDir := failedLogoutOutput(t, format, "/d\x01")
			message, details := errorDocument(t, got)
			items := notRevokedItems(t, details)
			require.Len(t, items, 1)
			assert.Equal(t, []string{""}, recoveryOf(t, items[0]))
			assert.Contains(t, message, "failed to log out", "the fact before the command is kept")
			detailStrings := decodedStrings(details)
			assert.False(t, slices.ContainsFunc(detailStrings, func(s string) bool { return strings.Contains(s, "rancher ") }), "no detail holds a command")
			assert.False(t, slices.ContainsFunc(detailStrings, func(s string) bool { return strings.Contains(s, configDir) }),
				"no detail names the directory the logout ran against")
		})
	}

	t.Run("text output", func(t *testing.T) {
		got, _ := failedLogoutOutput(t, "", remedyDir)
		lines := strings.Split(strings.TrimSpace(got), "\n")
		assert.True(t, slices.ContainsFunc(lines, func(l string) bool {
			return strings.HasSuffix(l, "to finish, run: rancher"+prefix+" "+rerun)
		}), "the closing line of the details names the directory: %q", got)
		assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", got)
	})

	t.Run("text output in a directory a terminal cannot show", func(t *testing.T) {
		got, _ := failedLogoutOutput(t, "", "/d\x01")
		lines := strings.Split(strings.TrimSpace(got), "\n")
		assert.True(t, slices.ContainsFunc(lines, func(l string) bool {
			return strings.HasSuffix(l, "those credentials are gone regardless of this failure")
		}), "the closing line keeps its fact without the command: %q", got)
		assert.NotContains(t, got, "to finish", "no command is offered")
		assert.NotContains(t, got, "rancher auth logout", "no command is offered")
	})
}
