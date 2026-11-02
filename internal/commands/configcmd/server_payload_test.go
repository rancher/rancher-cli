package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// notRevokedItem is a token of data.notRevoked as the json and yaml documents
// carry it.
type notRevokedItem struct {
	Error    string   `json:"error"`
	Recovery []string `json:"recovery"`
	LoginAs  string   `json:"loginAs"`
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

// anyHolds reports whether a string of the decoded document holds text.
func anyHolds(document any, text string) bool {
	return slices.ContainsFunc(decodedStrings(document), func(s string) bool { return strings.Contains(s, text) })
}

// serverCommandOutput runs a command of the server entry prod over the
// directory with a certificate failure, for an invocation whose remedies name
// remedyDir, and returns the notRevoked items its json or yaml data holds, the
// decoded document, everything it wrote to standard output and the config
// directory it ran against.
func serverCommandOutput(t *testing.T, name, format, remedyDir string) (items []notRevokedItem, doc any, raw, configDir string) {
	t.Helper()
	r := newTLSServerRevokeDir(t, false)
	var cmd runtime.Command
	in := cmdtest.Input{Pos: []string{"prod"}}
	switch name {
	case "config delete-server":
		cmd = NewConfigDeleteServer(r.dir)
	case "config update-server":
		cmd = &configUpdateServerCmd{dir: r.dir, tlsPreflight: func(context.Context, string, string) (string, error) { return "", nil }}
		in.Str = map[string]string{"url": "https://new.invalid"}
	default:
		require.FailNow(t, "unknown command", name)
	}
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: remedyDir}, safety.Binding{})
	require.Nil(t, ce)
	if format == "" {
		return nil, nil, out.String(), r.dir
	}
	var typed struct {
		Data struct {
			NotRevoked []notRevokedItem `json:"notRevoked"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &typed), "%q", out.String())
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc), "%q", out.String())
	return typed.Data.NotRevoked, doc, out.String(), r.dir
}

// TestServerCommandPayloadsNameTheDirectoryOfTheInvocation asserts, for config
// delete-server and config update-server over a server whose certificate
// failed verification, that the recovery commands of each notRevoked item in
// the json and yaml data name --config and the directory when the config
// directory is not the default, and name none for the default directory; that
// each item names the user to log in as; that the text output of config
// update-server names the directory too; and that a directory a terminal
// cannot show keeps the shape of each list, with the sentence that says why in
// place of each command and no path.
func TestServerCommandPayloadsNameTheDirectoryOfTheInvocation(t *testing.T) {
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)
	hasMarker := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
	}

	for _, name := range []string{"config delete-server", "config update-server"} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(name+" "+format+" in a directory that is not the default", func(t *testing.T) {
				items, _, raw, _ := serverCommandOutput(t, name, format, remedyDir)
				require.NotEmpty(t, items)
				assert.False(t, hasMarker(raw), "%q", raw)
				commands := 0
				for _, n := range items {
					require.NotEmpty(t, n.Recovery)
					for _, c := range n.Recovery {
						commands++
						assert.True(t, strings.HasPrefix(c, "rancher"+prefix+" "), "%q", c)
					}
					assert.NotEmpty(t, n.LoginAs)
				}
				assert.Positive(t, commands)
			})
			t.Run(name+" "+format+" in the default directory", func(t *testing.T) {
				items, doc, _, _ := serverCommandOutput(t, name, format, "")
				require.NotEmpty(t, items)
				assert.False(t, anyHolds(doc, "--config"), "no string of the document names a config directory")
				for _, n := range items {
					require.NotEmpty(t, n.Recovery)
					for _, c := range n.Recovery {
						assert.True(t, strings.HasPrefix(c, "rancher "), "%q", c)
					}
				}
			})
			t.Run(name+" "+format+" in a directory a terminal cannot show", func(t *testing.T) {
				plain, _, _, _ := serverCommandOutput(t, name, format, "")
				items, doc, _, configDir := serverCommandOutput(t, name, format, "/d\x01")
				require.Len(t, items, len(plain))
				assert.False(t, anyHolds(doc, configDir), "no string of the document names the directory the command ran against")
				assert.False(t, anyHolds(doc, "rancher "), "no string of the document holds a command")
				for i, n := range items {
					require.Len(t, n.Recovery, len(plain[i].Recovery))
					for _, c := range n.Recovery {
						assert.Equal(t, runtime.UnprintableConfigDirNote, c)
					}
				}
			})
		}
		t.Run(name+" text output", func(t *testing.T) {
			_, _, raw, _ := serverCommandOutput(t, name, "", remedyDir)
			if name == "config update-server" {
				assert.Contains(t, raw, "rancher"+prefix+" ")
			}
			assert.False(t, hasMarker(raw), "%q", raw)
		})
	}
}

// failedDeleteServerOutput runs config delete-server whose local delete fails
// after a revoke failed on the server's certificate, which keeps the entry and
// words the token's message for its next login (it holds a command), for an
// invocation whose remedies name remedyDir, and returns what the command wrote
// to standard error in format and the error message of each notRevoked item.
func failedDeleteServerOutput(t *testing.T, format, remedyDir string) (doc any, messages []string) {
	t.Helper()
	r := newTLSServerRevokeDir(t, false)
	cmd := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd)
	cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, error) {
		return nil, nil, errors.New("the keychain refused")
	}
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, cmdtest.Input{Pos: []string{"prod"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: remedyDir}, safety.Binding{})
	require.NotNil(t, ce)
	var typed struct {
		Data struct {
			Error struct {
				Details struct {
					NotRevoked []notRevokedItem `json:"notRevoked"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(errBuf.Bytes(), &typed), "%q", errBuf.String())
	require.NoError(t, yaml.Unmarshal(errBuf.Bytes(), &doc), "%q", errBuf.String())
	for _, n := range typed.Data.Error.Details.NotRevoked {
		if n.Error != "" {
			messages = append(messages, n.Error)
		}
	}
	return doc, messages
}

// TestAFailedDeleteServerPayloadNamesTheDirectoryInTheMessageOfAToken asserts
// the message of a notRevoked item that holds a command (the one that saves
// the server's CA for the entry's next login) names --config and the directory
// when the config directory is not the default, names none for the default
// directory, and for a directory a terminal cannot show keeps the words before
// the command, says why the command is left out and holds no command.
func TestAFailedDeleteServerPayloadNamesTheDirectoryInTheMessageOfAToken(t *testing.T) {
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)
	const save = "config update-server 'prod' --cacert <file>"
	for _, format := range []string{"json", "yaml"} {
		t.Run(format+" in a directory that is not the default", func(t *testing.T) {
			_, messages := failedDeleteServerOutput(t, format, remedyDir)
			require.NotEmpty(t, messages)
			for _, m := range messages {
				assert.Contains(t, m, "save it for entry 'prod' with: rancher"+prefix+" "+save)
			}
		})
		t.Run(format+" in the default directory", func(t *testing.T) {
			doc, messages := failedDeleteServerOutput(t, format, "")
			require.NotEmpty(t, messages)
			for _, m := range messages {
				assert.Contains(t, m, "save it for entry 'prod' with: rancher "+save)
			}
			assert.False(t, anyHolds(doc, "--config"), "no string of the document names a config directory")
		})
		t.Run(format+" in a directory a terminal cannot show", func(t *testing.T) {
			doc, messages := failedDeleteServerOutput(t, format, "/d\x01")
			require.NotEmpty(t, messages)
			for _, m := range messages {
				assert.Contains(t, m, "the server presented a certificate", "the words before the command are kept")
				assert.Contains(t, m, runtime.UnprintableConfigDirNote)
				assert.NotContains(t, m, "rancher ")
			}
			assert.False(t, anyHolds(doc, "rancher "), "no string of the document holds a command")
		})
	}
}

// TestANotRevokedTokenNamesNoUserWhenItsRecoveryNeedsNoLogin asserts, decoded
// from the json document of config update-server, that an item whose recovery
// is the administrator's delete of a v3 token and an item with no recovery (a
// pasted login) have no loginAs key, and that an item whose recovery logs in as
// its owner has one.
func TestANotRevokedTokenNamesNoUserWhenItsRecoveryNeedsNoLogin(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	for _, name := range []string{"v3", "pasted", "owned"} {
		addServerForUpdateTests(t, dir, name, "https://"+name+".example.test")
		require.NoError(t, store.SetToken(config.ServerRef{Name: name, URL: "https://" + name + ".example.test"}, "tok"))
	}
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	for name, s := range map[string]config.Server{
		"v3":     {Provenance: "derived", TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-a"}, TokenUserID: "user-a"},
		"owned":  {Provenance: "derived", TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-a"}, TokenUserID: "user-a"},
		"pasted": {},
	} {
		e := cfg.Servers[name]
		e.Provenance, e.TokenRef, e.TokenUserID = s.Provenance, s.TokenRef, s.TokenUserID
		cfg.Servers[name] = e
	}
	require.NoError(t, cfg.Write())

	for name, wantLoginAs := range map[string]bool{"v3": false, "pasted": false, "owned": true} {
		var out, errBuf bytes.Buffer
		cmd := &configUpdateServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) { return "", nil }}
		_, ce := runtime.Run(t.Context(), cmd, cmdtest.Input{Pos: []string{name}, Str: map[string]string{"url": "https://new-" + name + ".example.test"}},
			runtime.IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.Nil(t, ce, name)
		var doc struct {
			Data struct {
				NotRevoked []map[string]any `json:"notRevoked"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &doc), "%q", out.String())
		require.NotEmpty(t, doc.Data.NotRevoked, name)
		for _, item := range doc.Data.NotRevoked {
			if wantLoginAs {
				assert.Equal(t, "user-a", item["loginAs"], name)
			} else {
				assert.NotContains(t, item, "loginAs", name)
			}
		}
	}
}
