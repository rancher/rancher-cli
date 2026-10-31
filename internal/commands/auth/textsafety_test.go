package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

// dataTail is hostileTail without the characters -o yaml refuses or reads
// back changed (DEL and the controls U+0080 to U+009F, which include U+0085),
// for the rows that render the value through a structured format.
const dataTail = "\x1b[2J\t\u202e\u2028\r\nforged line"

// hostileValue is the value planted at the path p: the path in angle brackets,
// then the hostile tail.
func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// dataValue is hostileValue with the tail a structured format carries.
func dataValue(p string) string { return "<" + p + ">" + dataTail }

// plainValue is the value planted at a path whose rule keeps a newline out of
// it: the path in angle brackets alone.
func plainValue(p string) string { return "<" + p + ">" }

// textClasses classifies every text path of a type: the paths whose values the
// builder filters (printed), the paths whose values hold no newline by rule,
// the paths the builder does not print and the paths a test sets by hand.
// Each entry of a map says why in a sentence.
type textClasses struct {
	printed    []string
	rawByRule  map[string]string
	notPrinted map[string]string
	setByHand  map[string]string
	// nonText classifies every path of a field whose type holds no text (a
	// bool or a number), which no text path covers.
	nonText map[string]string
}

// assertComplete asserts that every reason is non-empty and that the four
// classes together are exactly the paths.
func (c textClasses) assertComplete(t *testing.T, what string, paths []string) {
	t.Helper()
	union := slices.Clone(c.printed)
	for _, m := range []map[string]string{c.rawByRule, c.notPrinted, c.setByHand} {
		for path, reason := range m {
			assert.NotEmpty(t, reason, "the reason for %s", path)
			union = append(union, path)
		}
	}
	assert.ElementsMatch(t, paths, union, "every text field of %s is classified exactly once", what)
}

// value is the planted text for the path p.
func (c textClasses) value(p string) string {
	if _, ok := c.rawByRule[p]; ok {
		return plainValue(p)
	}
	return hostileValue(p)
}

// shown is every path whose value a text prints.
func (c textClasses) shown() []string {
	out := slices.Clone(c.printed)
	for p := range c.rawByRule {
		out = append(out, p)
	}
	return out
}

// assertShown asserts that the text of some run prints every shown path and
// that no run prints a path classified as not printed.
func (c textClasses) assertShown(t *testing.T, all string) {
	t.Helper()
	for _, p := range c.shown() {
		assert.Contains(t, all, "<"+p+">", "a text prints %s", p)
	}
	for p := range c.notPrinted {
		assert.NotContains(t, all, "<"+p+">", "no text prints %s", p)
	}
}

// assertLineRule asserts that out holds nothing the writer filter removes and
// has no line that starts with the forged line after its leading spaces.
func assertLineRule(t *testing.T, out string) {
	t.Helper()
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	assertNoForgedLine(t, out)
}

// assertNoForgedLine asserts that no line of out starts with the forged line
// after its leading spaces.
func assertNoForgedLine(t *testing.T, out string) {
	t.Helper()
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

// exportedFields is the name of every exported field of typ, of any type.
func exportedFields(typ reflect.Type) []string {
	var out []string
	for f := range typ.Fields() {
		if f.IsExported() {
			out = append(out, f.Name)
		}
	}
	return out
}

// nonTextPaths is the path of every exported field of typ whose type holds no
// text: a bool, a number, or a struct with no exported field. A path joins the
// field names as remedytest.TextPaths does. It reaches through structs,
// pointers, slices, arrays and maps; a type that contains itself is walked
// once.
func nonTextPaths(typ reflect.Type) []string {
	errorPointer := reflect.TypeFor[*clierr.Error]()
	var out []string
	var walk func(t reflect.Type, path string, stack []reflect.Type)
	walk = func(t reflect.Type, path string, stack []reflect.Type) {
		switch t.Kind() {
		case reflect.String, reflect.Interface:
		case reflect.Pointer:
			if t != errorPointer {
				walk(t.Elem(), path, stack)
			}
		case reflect.Slice, reflect.Array:
			walk(t.Elem(), path+"[]", stack)
		case reflect.Map:
			walk(t.Key(), path+"{key}", stack)
			walk(t.Elem(), path+"{}", stack)
		case reflect.Struct:
			if slices.Contains(stack, t) {
				return
			}
			if len(exportedFields(t)) == 0 {
				out = append(out, path)
				return
			}
			for f := range t.Fields() {
				if !f.IsExported() {
					continue
				}
				name := f.Name
				if path != "" {
					name = path + "." + name
				}
				walk(f.Type, name, append(stack, t))
			}
		default:
			out = append(out, path)
		}
	}
	walk(typ, "", nil)
	return out
}

// assertNonTextComplete asserts that every reason is non-empty and that the
// non-text class is exactly the non-text paths of typ, so a new field of a
// type that holds text in another form (bytes, a time) is classified too.
func (c textClasses) assertNonTextComplete(t *testing.T, typ reflect.Type) {
	t.Helper()
	var paths []string
	for path, reason := range c.nonText {
		assert.NotEmpty(t, reason, "the reason for %s", path)
		paths = append(paths, path)
	}
	assert.ElementsMatch(t, nonTextPaths(typ), paths, "every field of %s that holds no text is classified exactly once", typ)
}

// checkCmd is an auth status command over dir whose probes answer as given.
func checkCmd(dir string, store config.CredStore) *authCheckCmd {
	return &authCheckCmd{
		dir: dir, store: store,
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
	}
}

// TestAuthStatusKeepsEveryTokenValueOnItsLine asserts that a value the token
// probe returns, in every text field of the metadata, cannot start a line of
// the auth status text, and that the data keeps the value as it is.
func TestAuthStatusKeepsEveryTokenValueOnItsLine(t *testing.T) {
	classes := textClasses{
		printed: []string{"Name", "Provider", "ExpiresAt"},
		setByHand: map[string]string{
			"Kind": "a fixed label of the CLI: the token probe sets it to session or derived",
		},
		nonText: map[string]string{
			"Current": "a bool, which holds no text",
		},
	}
	typ := reflect.TypeFor[provider.TokenMetadata]()
	classes.assertComplete(t, typ.String(), remedytest.TextPaths(typ))
	classes.assertNonTextComplete(t, typ)

	var all strings.Builder
	for _, run := range []struct{ name, token string }{
		{"ext token", "ext/x:secret"},
		{"v3 token", "token-x:secret"},
	} {
		t.Run(run.name, func(t *testing.T) {
			t.Setenv("RANCHER_CLI_TOKEN", run.token)
			planted, ok := reflect.TypeAssert[provider.TokenMetadata](remedytest.PlantText(t, typ, classes.value))
			require.True(t, ok)
			planted.Kind = "session"
			probe := func(context.Context, config.Effective, string) (provider.TokenMetadata, error) { return planted, nil }
			dir := seedAuthedDir(t)
			cmd := checkCmd(dir, config.NewFileStore(dir))
			cmd.token, cmd.v3token = probe, probe

			res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
			require.NoError(t, err)
			assertLineRule(t, res.Text)
			all.WriteString(res.Text)

			token, ok := res.Data.(map[string]any)["token"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, planted.Name, token["name"], "the data keeps the name as it is")
			assert.Equal(t, planted.Provider, token["provider"], "the data keeps the provider as it is")
			assert.Equal(t, planted.ExpiresAt, token["expiresAt"], "the data keeps the expiry as it is")
		})
	}
	classes.assertShown(t, all.String())
}

// TestAuthStatusKeepsStoredCredentialValuesOnTheirLines asserts that a value
// stored with the credential cannot start a line of the credential line, in
// every text field of the server entry, and that the data keeps it as it is.
func TestAuthStatusKeepsStoredCredentialValuesOnTheirLines(t *testing.T) {
	classes := textClasses{
		printed: []string{"TokenUserID", "TokenAuthConfig", "TokenRef.Name", "CredentialExpiresAt"},
		rawByRule: map[string]string{
			"Provenance": "a stored enum the CLI writes; credentialReport prints it as it is",
		},
		setByHand: map[string]string{
			"TokenRef.Kind": "set to the ext kind, which the CLI writes",
		},
		notPrinted: map[string]string{
			"CA":                                      "the certificate is never printed",
			"CAFingerprint":                           "the fingerprint is never printed",
			"CacheBindingID":                          "internal state, never printed",
			"CacheWritesPaused":                       "printed by the config status report, not here",
			"LoggedOutAt":                             "a time the CLI wrote itself, never printed here",
			"URL":                                     "the credential line does not print the server URL",
			"SupersededTokens[].Ref.Kind":             "superseded tokens are not part of the credential line",
			"SupersededTokens[].Ref.Name":             "superseded tokens are not part of the credential line",
			"SupersededTokens[].Server":               "superseded tokens are not part of the credential line",
			"SupersededTokens[].UserID":               "superseded tokens are not part of the credential line",
			"SupersededTokens[].ExpiresAt":            "superseded tokens are not part of the credential line",
			"SupersededTokens[].SupersededAt":         "superseded tokens are not part of the credential line",
			"SupersededTokens[].LastAttempt.Result":   "superseded tokens are not part of the credential line",
			"SupersededTokens[].LastAttempt.Ref.Kind": "superseded tokens are not part of the credential line",
			"SupersededTokens[].LastAttempt.Ref.Name": "superseded tokens are not part of the credential line",
			"SupersededTokens[].LastAttempt.Error":    "superseded tokens are not part of the credential line",
			"SupersededTokens[].LastAttempt.TLSError": "superseded tokens are not part of the credential line",
		},
		nonText: map[string]string{
			"CAFromEnvironment":                           "a bool, which holds no text",
			"SupersededTokens[].LastAttempt.Status":       "an HTTP status number, which holds no newline",
			"SupersededTokens[].LastAttempt.LogoutStatus": "an HTTP status number, which holds no newline",
		},
	}
	typ := reflect.TypeFor[config.Server]()
	classes.assertComplete(t, typ.String(), remedytest.TextPaths(typ))
	classes.assertNonTextComplete(t, typ)

	planted, ok := reflect.TypeAssert[config.Server](remedytest.PlantText(t, typ, classes.value))
	require.True(t, ok)
	planted.TokenRef.Kind = clierr.TokenKindExt

	data, line := credentialReport(planted)
	text := renderAuthCheckText(true, "default", "v1", nil, nil, nil, nil, storeReport{policy: "auto"}, line)
	assertLineRule(t, line)
	assertLineRule(t, text)
	classes.assertShown(t, text)

	assert.Equal(t, planted.TokenUserID, data["userId"], "the data keeps the owner as it is")
	assert.Equal(t, planted.TokenAuthConfig, data["authConfig"], "the data keeps the auth config as it is")
	assert.Equal(t, planted.CredentialExpiresAt, data["expiresAt"], "the data keeps the expiry as it is")
	ref, ok := data["tokenRef"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, planted.TokenRef.Name, ref["name"], "the data keeps the token name as it is")
}

// TestTrustCardKeepsEveryCertificateValueOnItsLine asserts that no value of
// the certificate facts can start a line of the trust card, with and without
// a prior fingerprint.
func TestTrustCardKeepsEveryCertificateValueOnItsLine(t *testing.T) {
	classes := textClasses{
		printed: []string{"Fingerprint", "Subject", "Issuer", "NotBefore", "NotAfter"},
		setByHand: map[string]string{
			"Kind": "set to the unknown authority kind, a fixed value of the CLI",
		},
	}
	classes.assertComplete(t, "tlstrust.Details", exportedFields(reflect.TypeFor[tlstrust.Details]()))

	d := tlstrust.Details{
		Kind:        tlstrust.KindUnknownAuthority,
		Fingerprint: hostileValue("Fingerprint"),
		Subject:     hostileValue("Subject"),
		Issuer:      hostileValue("Issuer"),
		NotBefore:   hostileValue("NotBefore"),
		NotAfter:    hostileValue("NotAfter"),
	}
	var all strings.Builder
	for _, prior := range []string{"", plainValue("priorFingerprint")} {
		card := trustCard(d, prior)
		assertLineRule(t, card)
		all.WriteString(card)
	}
	classes.assertShown(t, all.String())
	assert.Contains(t, all.String(), "<priorFingerprint>", "the changed identity warning names the prior fingerprint")
}

// TestAuthStatusKeepsServerValuesOnTheirLines asserts that the server
// version, the Rancher version and the user id cannot start a line of the
// auth status text, and that the data keeps each as it is.
func TestAuthStatusKeepsServerValuesOnTheirLines(t *testing.T) {
	for _, row := range []struct {
		name  string
		set   func(c *authCheckCmd, value string)
		field string
	}{
		{"server version", func(c *authCheckCmd, v string) {
			c.preflight = func(context.Context, config.Effective) (string, error) { return v, nil }
		}, "serverVersion"},
		{"rancher version", func(c *authCheckCmd, v string) {
			c.rancherVersion = func(context.Context, config.Effective) (string, error) { return v, nil }
		}, "rancherVersion"},
		{"user id", func(c *authCheckCmd, v string) {
			c.identity = func(context.Context, config.Effective) (string, error) { return v, nil }
		}, "identity"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("RANCHER_CLI_TOKEN", "token-x:secret")
			dir := seedAuthedDir(t)
			cmd := checkCmd(dir, config.NewFileStore(dir))
			planted := hostileValue(row.field)
			row.set(cmd, planted)

			res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
			require.NoError(t, err)
			assert.Contains(t, res.Text, "<"+row.field+">")
			assertLineRule(t, res.Text)

			data, ok := res.Data.(map[string]any)
			require.True(t, ok)
			switch row.field {
			case "rancherVersion":
				got, ok := data[row.field].(*string)
				require.True(t, ok)
				assert.Equal(t, planted, *got, "the data keeps the version as it is")
			case "identity":
				got, ok := data[row.field].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, planted, got["userID"], "the data keeps the user id as it is")
			default:
				assert.Equal(t, planted, data[row.field], "the data keeps the version as it is")
			}
		})
	}
}

// statusEnvelope is the part of the auth status envelope the structured rows
// read.
type statusEnvelope struct {
	Data map[string]any `json:"data"`
}

// sub is the object at the key of m.
func sub(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	got, ok := m[key].(map[string]any)
	require.True(t, ok, "%s is an object in %v", key, m)
	return got
}

// TestAuthStatusJSONAndYAMLKeepServerValuesAsTheyAre asserts that the text of
// auth status holds no forged line while json and yaml carry every value as
// it is.
func TestAuthStatusJSONAndYAMLKeepServerValuesAsTheyAre(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "")
	dir := seedAuthedDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	s := cfg.Servers["default"]
	s.Provenance = clierr.ProvenanceDerived
	s.TokenUserID, s.TokenAuthConfig = dataValue("credential.userId"), dataValue("credential.authConfig")
	s.TokenRef = &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: dataValue("credential.tokenRef.name")}
	s.CredentialExpiresAt = dataValue("credential.expiresAt")
	cfg.Servers["default"] = s
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "default", URL: s.URL}, "ext/token-live:secretvalue"))

	build := func() *authCheckCmd {
		cmd := checkCmd(dir, store)
		cmd.preflight = func(context.Context, config.Effective) (string, error) { return dataValue("serverVersion"), nil }
		cmd.rancherVersion = func(context.Context, config.Effective) (string, error) { return dataValue("rancherVersion"), nil }
		cmd.identity = func(context.Context, config.Effective) (string, error) { return dataValue("identity.userID"), nil }
		cmd.token = func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{
				Name: dataValue("token.name"), Kind: "session",
				Provider: dataValue("token.provider"), ExpiresAt: dataValue("token.expiresAt"),
			}, nil
		}
		return cmd
	}
	run := func(format string) string {
		var out, errBuf bytes.Buffer
		_, ce := runtime.Run(t.Context(), build(), cmdtest.Input{}, runtime.IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
		require.Nil(t, ce)
		return out.String()
	}

	t.Run("text", func(t *testing.T) {
		stdout := run("")
		assertNoForgedLine(t, stdout)
		for _, p := range []string{"serverVersion", "rancherVersion", "identity.userID", "token.name", "token.provider", "token.expiresAt",
			"credential.userId", "credential.authConfig", "credential.tokenRef.name", "credential.expiresAt"} {
			assert.Contains(t, stdout, "<"+p+">")
		}
	})
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var env statusEnvelope
			raw := []byte(run(format))
			if format == "json" {
				require.NoError(t, json.Unmarshal(raw, &env))
			} else {
				require.NoError(t, yaml.Unmarshal(raw, &env))
			}
			assert.Equal(t, dataValue("serverVersion"), env.Data["serverVersion"])
			assert.Equal(t, dataValue("rancherVersion"), env.Data["rancherVersion"])
			assert.Equal(t, dataValue("identity.userID"), sub(t, env.Data, "identity")["userID"])
			token := sub(t, env.Data, "token")
			assert.Equal(t, dataValue("token.name"), token["name"])
			assert.Equal(t, dataValue("token.provider"), token["provider"])
			assert.Equal(t, dataValue("token.expiresAt"), token["expiresAt"])
			credential := sub(t, env.Data, "credential")
			assert.Equal(t, dataValue("credential.userId"), credential["userId"])
			assert.Equal(t, dataValue("credential.authConfig"), credential["authConfig"])
			assert.Equal(t, dataValue("credential.tokenRef.name"), sub(t, credential, "tokenRef")["name"])
			assert.Equal(t, dataValue("credential.expiresAt"), credential["expiresAt"])
		})
	}
}

// subjectCertificate is a self-signed certificate whose common name is name.
func subjectCertificate(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

// TestCertificateDetailsJSONKeepTheSubjectAsItIs asserts that the details of
// a certificate failure carry the subject and issuer as they are in json and
// yaml.
func TestCertificateDetailsJSONKeepTheSubjectAsItIs(t *testing.T) {
	cert := subjectCertificate(t, dataValue("subject"))
	failure := &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{cert},
		Err:                    x509.UnknownAuthorityError{Cert: cert},
	}
	ce := tlstrust.Classify(failure, "rancher.invalid", time.Now(), tlstrust.Remedy{})
	require.NotNil(t, ce)

	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, output.Render(&buf, format, output.ErrorEnvelope(ce)))
			var env statusEnvelope
			if format == "json" {
				require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
			} else {
				require.NoError(t, yaml.Unmarshal(buf.Bytes(), &env))
			}
			details := sub(t, sub(t, env.Data, "error"), "details")
			assert.Equal(t, cert.Subject.String(), details["subject"], "the details keep the subject as it is")
			assert.Equal(t, cert.Issuer.String(), details["issuer"], "the details keep the issuer as it is")
		})
	}
}
