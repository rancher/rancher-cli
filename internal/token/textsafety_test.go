package token

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

// hostileValue is the value planted at the path p: the path in angle brackets,
// then the hostile tail.
func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// assertNoForgedLine asserts that no line of out starts with the forged line
// after its leading spaces.
func assertNoForgedLine(t *testing.T, out string) {
	t.Helper()
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

// assertLineRule asserts that out holds nothing the writer filter removes and
// has no line that starts with the forged line.
func assertLineRule(t *testing.T, out string) {
	t.Helper()
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	assertNoForgedLine(t, out)
}

// assertPaths asserts that out holds the value planted at each path.
func assertPaths(t *testing.T, out string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		assert.Contains(t, out, "<"+p+">", "the text holds %s", p)
	}
}

// noticeTexts is every notice the prompter was given, as the code built it
// and before the prompter's own filter.
func (e *eventPrompter) noticeTexts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		if msg, ok := strings.CutPrefix(ev, "notice: "); ok {
			out = append(out, msg)
		}
	}
	return out
}

// noticeWith is the one notice that holds text, as the code built it.
func noticeWith(t *testing.T, p *eventPrompter, text string) string {
	t.Helper()
	var found []string
	for _, msg := range p.noticeTexts() {
		if strings.Contains(msg, text) {
			found = append(found, msg)
		}
	}
	require.Len(t, found, 1, "the notices holding %q in %q", text, p.noticeTexts())
	return found[0]
}

// textSafetyDeletion is a delete with one token of each kind of line the text
// gives: a revoked cache token, a superseded token that was not revoked, two
// tokens an entry's next login revokes (a user's entry and a plugin's), and
// a pasted login token. Every value that reaches a line comes from value.
func textSafetyDeletion(value func(path string) string) *Deletion {
	const serverURL = "https://rancher.invalid"
	server := config.CanonicalServerIdentity(serverURL)
	owner := value("TokenUserID")
	view := &config.Config{Servers: map[string]config.Server{
		"prod":   {URL: serverURL, TokenUserID: owner},
		"plugin": {URL: serverURL, TokenUserID: owner, Provenance: clierr.ProvenanceExecLogin},
	}}
	ref := func(p string) *TokenRef { return &TokenRef{Kind: TokenKindExt, Name: value(p)} }
	return &Deletion{
		cfg: view, view: view, removed: map[string]bool{},
		items: []*revocationItem{
			{source: sourceCache, ref: ref("cacheRef"), server: server, revoked: true},
			{source: sourceSuperseded, ref: ref("supersededRef"), server: server, reason: reasonFailed, errText: value("revokeError")},
			{source: sourceCache, ref: ref("nextLoginRef"), server: server, owner: owner, entry: "prod", reason: reasonFailed, errText: "the server refused"},
			{source: sourceCache, ref: ref("pluginRef"), server: server, owner: owner, entry: "plugin", reason: reasonFailed, errText: "the server refused"},
			{source: sourceLogin, ref: ref("pastedRef"), server: server, reason: reasonUserSupplied, loginTarget: "prod"},
		},
	}
}

// TestRevocationLinesKeepStoredValuesOnTheirLines asserts that no stored token
// name, owner or revoke error can start a line of the revoke report text.
func TestRevocationLinesKeepStoredValuesOnTheirLines(t *testing.T) {
	d := textSafetyDeletion(hostileValue)
	text := strings.Join(d.TextLines(), "\n") + "\n"
	assertPaths(t, text, "cacheRef", "supersededRef", "revokeError", "nextLoginRef", "pluginRef", "TokenUserID", "pastedRef")
	assertLineRule(t, text)
}

// TestRevocationLinesKeepTheRemedyCommandOfAnErrorText asserts that the revoke
// error of a line keeps the remedy command it holds, which the invocation
// writes for its config directory, and that the value around it still cannot
// start a line.
func TestRevocationLinesKeepTheRemedyCommandOfAnErrorText(t *testing.T) {
	command := clierr.Rancher + " config update-server 'prod' --url https://rancher.invalid"
	d := textSafetyDeletion(hostileValue)
	for _, it := range d.items {
		if it.reason == reasonFailed && it.entry == "" {
			it.errText = hostileValue("revokeError") + "; change the entry's URL with " + command + "; the token was not sent"
		}
	}
	text := strings.Join(d.TextLines(), "\n") + "\n"
	assertPaths(t, text, "revokeError")
	assertLineRule(t, text)
	assert.Contains(t, text, command, "the line keeps the remedy command with its marker")

	written := runtime.IO{RemedyDir: t.TempDir()}.ExpandLines(text)
	assert.Contains(t, written, "rancher --config ", "the command names the config directory")
	assert.NotContains(t, written, clierr.RemedyMarker)
}

// renderedData is the envelope a structured format renders, read back.
type renderedData struct {
	Data     map[string]any `json:"data"`
	Warnings []string       `json:"warnings"`
}

// renderAndRead renders env in format and reads the rendered bytes back.
func renderAndRead(t *testing.T, format string, env *output.Envelope) renderedData {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, output.Render(&buf, format, env))
	var got renderedData
	if format == "json" {
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	} else {
		require.NoError(t, yaml.Unmarshal(buf.Bytes(), &got))
	}
	return got
}

// entryNamed is the object of list whose ref name is name.
func entryNamed(t *testing.T, list any, name string) map[string]any {
	t.Helper()
	items, ok := list.([]any)
	require.True(t, ok, "a list in %v", list)
	for _, item := range items {
		m, ok := item.(map[string]any)
		require.True(t, ok)
		if ref, ok := m["ref"].(map[string]any); ok && ref["name"] == name {
			return m
		}
	}
	require.Failf(t, "no entry", "no entry names %q in %v", name, items)
	return nil
}

// TestRevocationReportJSONAndYAMLKeepStoredValuesAsTheyAre asserts that the
// revoke report carries the token names and the revoke error as they are in
// json and yaml.
func TestRevocationReportJSONAndYAMLKeepStoredValuesAsTheyAre(t *testing.T) {
	rep := textSafetyDeletion(hostileValue).Report()
	env := output.NewEnvelope("LogoutResult", map[string]any{"revoked": rep.Revoked, "notRevoked": rep.NotRevoked})
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			got := renderAndRead(t, format, env)
			revoked := entryNamed(t, got.Data["revoked"], hostileValue("cacheRef"))
			assert.Equal(t, hostileValue("cacheRef"), revoked["ref"].(map[string]any)["name"])
			failed := entryNamed(t, got.Data["notRevoked"], hostileValue("supersededRef"))
			assert.Equal(t, hostileValue("revokeError"), failed["error"], "the report keeps the revoke error as it is")
		})
	}
}

// TestRevocationWarningsKeepStoredValuesAsTheyAre asserts that the warnings
// of a delete carry the token name, the revoke error and the owner of an
// entry as they are in json and yaml, for a user's entry and a plugin's.
func TestRevocationWarningsKeepStoredValuesAsTheyAre(t *testing.T) {
	d := textSafetyDeletion(hostileValue)
	env := output.NewEnvelope("LogoutResult", map[string]any{})
	env.Warnings = d.Warnings()
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			got := renderAndRead(t, format, env)
			holding := func(parts ...string) bool {
				for _, w := range got.Warnings {
					if every(w, parts...) {
						return true
					}
				}
				return false
			}
			assert.True(t, holding(hostileValue("supersededRef"), hostileValue("revokeError")), "a warning holds the ref and the revoke error as they are: %q", got.Warnings)
			assert.True(t, holding("recorded on prod", "the next login of prod as "+hostileValue("TokenUserID")), "a warning holds the owner of the user's entry as it is: %q", got.Warnings)
			assert.True(t, holding("recorded on plugin", "the next kubectl sign-in through plugin as "+hostileValue("TokenUserID")), "a warning holds the owner of the plugin's entry as it is: %q", got.Warnings)
		})
	}
}

// every reports whether s holds each of parts.
func every(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// TestAuthConfigCandidatesKeepTheLabelAsItIs asserts that the candidates of a
// refused --auth-provider value carry the auth config label as it is in json
// and yaml.
func TestAuthConfigCandidatesKeepTheLabelAsItIs(t *testing.T) {
	entry := &AuthProvider{Type: "openLdapProvider", ID: hostileValue("authConfig")}
	_, err := selectAuthProvider(t.Context(), []typedProvider{entry}, nil, "no-such-config", true, ui.Discard())
	ce := clierr.From(err)
	require.NotNil(t, ce)
	env := output.ErrorEnvelope(ce)
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			got := renderAndRead(t, format, env)
			candidates, ok := got.Data["error"].(map[string]any)["candidates"].([]any)
			require.True(t, ok)
			require.Len(t, candidates, 1)
			assert.Equal(t, authConfigLabel(entry), candidates[0].(map[string]any)["name"], "the candidate keeps the label as it is")
			assert.Equal(t, entry.ID, candidates[0].(map[string]any)["id"])
		})
	}
}

// azureProvider is an Azure AD auth config with the given endpoints.
func azureProvider(authURL, deviceAuthURL, tokenURL string) *AzureADProvider {
	return &AzureADProvider{
		Type: "azureADProvider", ClientID: "client", Scopes: []string{"openid"},
		AuthURL: authURL, DeviceAuthURL: deviceAuthURL, TokenURL: tokenURL,
	}
}

// assertNotice asserts the line rule on a notice as the code built it and on
// the bytes the prompter wrote.
func assertNotice(t *testing.T, msg, written string, paths ...string) {
	t.Helper()
	assertPaths(t, msg, paths...)
	assertLineRule(t, msg)
	assertNoForgedLine(t, written)
}

// TestSignInMessagesKeepServerValuesOnTheirLines asserts that no server value
// can start a line of a message a sign-in gives, as the code builds the
// message and as the prompter writes it. A sign-in address cannot hold a
// newline, so its row asserts only the characters the writer filter removes.
func TestSignInMessagesKeepServerValuesOnTheirLines(t *testing.T) {
	t.Run("a refused delete of the login token", func(t *testing.T) {
		restore := SetSAMLTimingForTest(time.Minute, time.Millisecond)
		t.Cleanup(restore)
		const bearer = "token-abcde:secret"
		var written lockedBuffer
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				body, err := json.Marshal(map[string]string{"type": "error", "message": hostileValue("deleteError")})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(body)
				return
			}
			m := samlLoginURLRE.FindStringSubmatch(written.String())
			if m == nil {
				http.Error(w, "login url not published yet", http.StatusInternalServerError)
				return
			}
			loginURL, err := url.Parse(m[1])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			raw, err := base64.StdEncoding.DecodeString(loginURL.Query().Get("publicKey"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var key rsa.PublicKey
			if err := json.Unmarshal(raw, &key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			sealed, err := rsa.EncryptOAEP(crypto.SHA256.New(), rand.Reader, &key, []byte(bearer), nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": base64.StdEncoding.EncodeToString(sealed)})
		}))
		t.Cleanup(srv.Close)

		p := newEventPrompter(ui.New(&written, nil, true, false))
		_, err := samlAuth(t.Context(), srv.Client(), &loginInput{server: srv.URL}, p)
		require.NoError(t, err)
		assertNotice(t, noticeWith(t, p, "error deleting auth token"), written.String(), "deleteError")
	})

	t.Run("a rejected stored credential", func(t *testing.T) {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, true, false))
		body, err := json.Marshal(map[string]any{"kind": "Status", "status": "Failure", "reason": "Unauthorized", "code": http.StatusUnauthorized, "message": hostileValue("rejection")})
		require.NoError(t, err)
		rejected := fmt.Errorf("%w: %w", errCredentialUnauthorized, clierr.ResponseError(http.MethodPost, http.StatusUnauthorized, body))
		got := tokenCmd{}.classifyRejection(rejected, storedCredential{name: "prod"}, "", true, p)
		require.ErrorIs(t, got, errStoredCredentialRejected)
		msg := noticeWith(t, p, "was rejected")
		assertNotice(t, msg, written.String(), "rejection")
		assert.Contains(t, msg, clierr.Rancher+" auth login", "the refresh command keeps this process's remedy marker")
	})

	t.Run("one auth config and an unsupported provider", func(t *testing.T) {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, false, true))
		entries := []typedProvider{&AuthProvider{Type: "openLdapProvider", ID: hostileValue("authConfig")}}
		_, err := selectAuthProvider(t.Context(), entries, []unsupportedAuthConfig{{Type: hostileValue("unsupportedType")}}, "", false, p)
		require.NoError(t, err)
		assertNotice(t, noticeWith(t, p, "Auth config: "), written.String(), "authConfig", "unsupportedType")
	})

	t.Run("a menu of auth configs", func(t *testing.T) {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, newLineReader("0"), false, true))
		entries := []typedProvider{
			&AuthProvider{Type: "openLdapProvider", ID: hostileValue("firstAuthConfig")},
			&AuthProvider{Type: "githubProvider", ID: hostileValue("secondAuthConfig")},
		}
		_, err := selectAuthProvider(t.Context(), entries, []unsupportedAuthConfig{{Type: hostileValue("unsupportedType")}}, "", false, p)
		require.NoError(t, err)
		assertNotice(t, noticeWith(t, p, "Auth configs:"), written.String(), "firstAuthConfig", "secondAuthConfig", "unsupportedType")
	})

	t.Run("the auth config a sign-in goes through", func(t *testing.T) {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, false, true))
		entries := []typedProvider{
			&AuthProvider{Type: "openLdapProvider", ID: hostileValue("chosenAuthConfig")},
			&AuthProvider{Type: "openLdapProvider", ID: "other"},
		}
		msg := signInNotice(entries, entries[0])
		p.Notice(msg)
		assertNotice(t, msg, written.String(), "chosenAuthConfig")
	})

	t.Run("a device code", func(t *testing.T) {
		idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/idp/devicecode":
				writeTestJSON(w, http.StatusOK, map[string]any{
					"device_code":      "device",
					"user_code":        hostileValue("userCode"),
					"verification_uri": hostileValue("verificationURI"),
					"expires_in":       60,
					"interval":         1,
				})
			default:
				writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "access_denied"})
			}
		}))
		t.Cleanup(idp.Close)
		provider := azureProvider(idp.URL+"/idp/authorize", idp.URL+"/idp/devicecode", idp.URL+"/idp/token")
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, false, false))
		_, err := oauthDeviceCodeAuth(t.Context(), idp.Client(), &loginInput{server: "https://rancher.invalid", authProvider: "azureADProvider", idpClient: idp.Client()}, provider, p)
		require.Error(t, err, "the identity provider denies the access token request")
		assertNotice(t, noticeWith(t, p, "To sign in"), written.String(), "userCode", "verificationURI")
	})

	t.Run("a sign-in address the browser did not open", func(t *testing.T) {
		provider := azureProvider("https://idp.invalid/<authUrl>\u202e\u2028\u0085\u009b", "https://idp.invalid/devicecode", "https://idp.invalid/token")
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, false, false))
		browser := func(string) error { return errors.New("no display") }
		_, err := oauthAuthCodeAuth(t.Context(), failingTransportClient(t), &loginInput{server: "https://rancher.invalid", authProvider: "azureADProvider"}, provider, 50*time.Millisecond, browser, p)
		require.Error(t, err)
		msg := noticeWith(t, p, "Failed to open browser automatically")
		assertPaths(t, msg, "authUrl")
		assert.Equal(t, output.PrintableLines(msg), msg, "the sign-in address holds no character the writer filter removes")
	})

	t.Run("the expiry of a stored session token", func(t *testing.T) {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, false, false))
		msg := legacyFallbackNotice((*clierr.TokenFate)(nil).LoginTokenNoun(), hostileValue("expiresAt"))
		p.Notice(msg)
		assertNotice(t, msg, written.String(), "expiresAt")
	})
}
