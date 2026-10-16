package auth

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// variableServer is a TLS server whose self-signed certificate only the CA
// in fix trusts, so a login reaches it only through the CA it connects with.
func variableServer(t *testing.T) (url string, fix tlsFixture) {
	t.Helper()
	fix = selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)
	return srv.URL, fix
}

func otherCA(t *testing.T) string {
	t.Helper()
	return selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)).pem
}

func urlLoginInput(name, url string) cmdtest.Input {
	return cmdtest.Input{Str: map[string]string{"url": url}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}}
}

func plainLoginInput(name string) cmdtest.Input {
	return cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}}
}

func noCANotice(name string) string {
	return "server entry '" + name + "' has no saved CA and this login used RANCHER_CLI_CACERT; " +
		"commands run without that variable will not trust the server (logout uses the variable for this entry); " +
		"to save the CA, run: rancher config update-server '" + name + "' --cacert <file>"
}

func differentCANotice(name string) string {
	return "server entry '" + name + "' keeps its saved CA, which differs from RANCHER_CLI_CACERT that this login used; " +
		"commands run without that variable, and logout, use the saved CA, and fail if it does not fit the server; " +
		"to replace it, run: rancher config update-server '" + name + "' --cacert <file>"
}

func hasNotice(res runtime.Result, text string) bool {
	return slices.Contains(res.Warnings, text)
}

func noticesOfTheVariable(res runtime.Result) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "RANCHER_CLI_CACERT") {
			out = append(out, w)
		}
	}
	return out
}

// TestLoginURLNewEntryConnectsWithTheVariableAndSavesNoCA asserts a login of
// a new entry with no --cacert connects with RANCHER_CLI_CACERT, saves no CA
// and no fingerprint, records that the variable supplied the connection, and
// warns that the entry has no saved CA.
func TestLoginURLNewEntryConnectsWithTheVariableAndSavesNoCA(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	dir, store := cmdtest.FilePolicyDir(t)

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), urlLoginInput("prod", url), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, hasNotice(res, noCANotice("prod")), "warnings: %v", res.Warnings)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	got := cfg.Servers["prod"]
	assert.Empty(t, got.CA, "the variable is never saved")
	assert.Empty(t, got.CAFingerprint)
	assert.True(t, got.CAFromEnvironment)
	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
}

// TestAnUnusableTokenUnderTheVariableCarriesTheNoticeOnce asserts the
// failure for a token that is not id:secret, which is built from the login
// state that already holds the early warnings, reports the variable notice
// once.
func TestAnUnusableTokenUnderTheVariableCarriesTheNoticeOnce(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	dir, store := cmdtest.FilePolicyDir(t)

	_, err := tlsLoginCmd(dir, store).Execute(t.Context(), urlLoginInput("prod", url), cmdtest.StdinPrompter("not-an-id-secret\n"))
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code, "%v", err)
	n := 0
	for _, w := range ce.Warnings() {
		if w == noCANotice("prod") {
			n++
		}
	}
	assert.Equal(t, 1, n, "warnings: %v", ce.Warnings())
}

// TestLoginURLWithoutTheVariableRecordsNothing asserts a login that connects
// with the entry's own --cacert saves it, sets no record, and gives no notice
// even with the variable set to a CA that would not verify the server.
func TestLoginURLWithoutTheVariableRecordsNothing(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, otherCA(t))
	dir, store := cmdtest.FilePolicyDir(t)
	in := urlLoginInput("prod", url)
	in.Str["cacert"] = fix.pem

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Empty(t, noticesOfTheVariable(res))
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, fix.pem, cfg.Servers["prod"].CA, "--cacert is saved")
	assert.False(t, cfg.Servers["prod"].CAFromEnvironment)
}

// TestLoginURLHTTPGivesNoVariableNotice asserts no CA applies to an http://
// URL, so nothing connected with the variable: no notice and no record.
func TestLoginURLHTTPGivesNoVariableNotice(t *testing.T) {
	t.Setenv(config.EnvCACert, otherCA(t))
	dir, store := cmdtest.FilePolicyDir(t)

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), urlLoginInput("dev", "http://127.0.0.1:1"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Empty(t, noticesOfTheVariable(res))
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.False(t, cfg.Servers["dev"].CAFromEnvironment)
}

// TestLoginURLWarnsEvenWhenThePreflightCannotReachTheServer asserts the
// notice does not wait for the server: the entry is saved either way.
func TestLoginURLWarnsEvenWhenThePreflightCannotReachTheServer(t *testing.T) {
	t.Setenv(config.EnvCACert, otherCA(t))
	dir, store := cmdtest.FilePolicyDir(t)

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), urlLoginInput("prod", "https://127.0.0.1:1"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, hasNotice(res, noCANotice("prod")), "warnings: %v", res.Warnings)
}

// TestLoginKnownEntryKeepsItsSavedCAWhenTheVariableDiffers asserts, on both
// paths, a known entry with a saved CA that differs from the variable
// connects with the variable, keeps its saved CA and fingerprint without a
// CONFLICT, and warns that the entry keeps its own CA.
func TestLoginKnownEntryKeepsItsSavedCAWhenTheVariableDiffers(t *testing.T) {
	for _, path := range []string{"--url", "plain"} {
		t.Run(path, func(t *testing.T) {
			url, fix := variableServer(t)
			t.Setenv(config.EnvCACert, fix.pem)
			saved := otherCA(t)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", url, saved, false)
			seedServerCAFingerprint(t, dir, "prod", "f1")
			in := plainLoginInput("prod")
			if path == "--url" {
				in = urlLoginInput("prod", url)
			}

			res, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
			require.NoError(t, err)
			assert.True(t, hasNotice(res, differentCANotice("prod")), "warnings: %v", res.Warnings)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			got := cfg.Servers["prod"]
			assert.Equal(t, saved, got.CA)
			assert.Equal(t, "f1", got.CAFingerprint)
			assert.False(t, got.CAFromEnvironment, "the entry has a CA")
		})
	}
}

// TestLoginKnownEntryWithTheVariableAsItsSavedCAGivesNoNotice asserts no
// notice when the entry's saved CA resolves to the variable's bytes.
func TestLoginKnownEntryWithTheVariableAsItsSavedCAGivesNoNotice(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	dir, store := cmdtest.FilePolicyDir(t)
	caFile := cmdtest.WriteCAFile(t, fix.pem)
	seedServer(t, dir, "prod", url, caFile, false)

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Empty(t, noticesOfTheVariable(res))
}

// TestLoginKnownEntryWithNoCARecordsAndClearsTheVariable asserts a plain
// login of an entry with no saved CA records that the variable supplied the
// connection (with the first notice), and a later login that connected
// without the variable clears the record.
func TestLoginKnownEntryWithNoCARecordsAndClearsTheVariable(t *testing.T) {
	url, fix := variableServer(t)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", url, "", false)

	t.Setenv(config.EnvCACert, fix.pem)
	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, hasNotice(res, noCANotice("prod")), "warnings: %v", res.Warnings)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.True(t, cfg.Servers["prod"].CAFromEnvironment)
	assert.Empty(t, cfg.Servers["prod"].CA)

	t.Setenv(config.EnvCACert, "")
	seedServer(t, dir, "plain", "http://127.0.0.1:1", "", false)
	marked, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	e := marked.Servers["plain"]
	e.CAFromEnvironment = true
	marked.Servers["plain"] = e
	require.NoError(t, marked.Write())
	_, err = tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("plain"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.False(t, cfg.Servers["plain"].CAFromEnvironment, "a login that connected without the variable clears the record")
}

// TestLoginWithTheVariableStillConflictsWhenTheSavedCAChanges asserts a
// concurrent change of the entry's saved CA during a variable-connected login
// is a CONFLICT: the variable is never rechecked, the saved CA still is.
func TestLoginWithTheVariableStillConflictsWhenTheSavedCAChanges(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", url, otherCA(t), false)
	cmd := tlsLoginCmd(dir, store)
	changed := otherCA(t)
	cmd.beforeCommitHook = func() {
		cfg, _ := config.Load(config.FilePath(dir))
		e := cfg.Servers["prod"]
		e.CA = changed
		cfg.Servers["prod"] = e
		_ = cfg.Write()
	}

	_, err := cmd.Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:sec\n"))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, terr, "no credential is stored, got %q", tok)
}

// TestLoginExplicitCacertThatDiffersFromTheSavedCAStaysAConflict asserts the
// variable changes nothing about an explicit --cacert: one that differs from
// a known entry's saved CA is still a CONFLICT.
func TestLoginExplicitCacertThatDiffersFromTheSavedCAStaysAConflict(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	dir, store := cmdtest.FilePolicyDir(t)
	saved := otherCA(t)
	seedServer(t, dir, "prod", url, saved, false)
	in := urlLoginInput("prod", url)
	in.Str["cacert"] = fix.pem

	_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, saved, cfg.Servers["prod"].CA, "unchanged")
}

// TestLoginUnreadableSavedCABlocksTheLoginUnderTheVariable asserts an
// entry's unreadable saved CA stops the login even when the variable would
// have connected, with the entry's own text.
func TestLoginUnreadableSavedCABlocksTheLoginUnderTheVariable(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	for _, path := range []string{"--url", "plain"} {
		t.Run(path, func(t *testing.T) {
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", url, missing, false)
			in := plainLoginInput("prod")
			if path == "--url" {
				in = urlLoginInput("prod", url)
			}
			_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, "missing.pem")
			assert.NotContains(t, ce.Message, "RANCHER_CLI_CACERT")
			_, terr := store.GetToken(config.ServerRef{Name: "prod"})
			assert.Error(t, terr)
		})
	}
}

// TestLoginUnreadableVariableSaysSo asserts an unreadable variable path is
// "reading RANCHER_CLI_CACERT: ..." on both paths.
func TestLoginUnreadableVariableSaysSo(t *testing.T) {
	url, _ := variableServer(t)
	missing := filepath.Join(t.TempDir(), "variable-missing.pem")
	t.Setenv(config.EnvCACert, missing)
	for _, path := range []string{"--url", "plain"} {
		t.Run(path, func(t *testing.T) {
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", url, "", false)
			in := plainLoginInput("prod")
			if path == "--url" {
				in = urlLoginInput("prod", url)
			}
			_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
			require.ErrorContains(t, err, "reading RANCHER_CLI_CACERT: ")
			require.ErrorContains(t, err, "variable-missing.pem")
		})
	}
}

// TestLoginUnknownAuthorityUnderTheVariableNamesTheVariable asserts, on each
// path, the unknown-authority error of a login whose CA came from
// RANCHER_CLI_CACERT: no trust prompt is offered, the text names the
// variable and the remedy of that path, and says nothing of --cacert as the
// way to pass the CA, of the entry's saved CA or of an existing registration.
func TestLoginUnknownAuthorityUnderTheVariableNamesTheVariable(t *testing.T) {
	url, _ := variableServer(t)
	t.Setenv(config.EnvCACert, otherCA(t))
	for _, tc := range []struct {
		name   string
		known  bool
		in     func() cmdtest.Input
		remedy string
	}{
		{"--url, new entry", false, func() cmdtest.Input { return urlLoginInput("prod", url) },
			"the CA came from RANCHER_CLI_CACERT - pass --cacert <file> to this login, or unset RANCHER_CLI_CACERT, or set it to the server's CA"},
		{"--url, known entry", true, func() cmdtest.Input { return urlLoginInput("prod", url) },
			"the CA came from RANCHER_CLI_CACERT - pass --cacert <file> to this login, or unset RANCHER_CLI_CACERT, or set it to the server's CA"},
		{"plain", true, func() cmdtest.Input { return plainLoginInput("prod") },
			"the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; to save a CA for this entry, run: rancher config update-server 'prod' --cacert <file>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := cmdtest.FilePolicyDir(t)
			if tc.known {
				seedServer(t, dir, "prod", url, "", false)
			}
			var out bytes.Buffer
			base := ui.New(&out, newLineReader("y", "id:sec"), false, true)
			p := &noticeCapturingPrompter{Prompter: base}
			in := tc.in()
			in.Flag["token-stdin"] = false

			_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, p)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			details, ok := ce.Details.(tlstrust.Details)
			require.True(t, ok)
			assert.Equal(t, tlstrust.KindUnknownAuthority, details.Kind)
			assert.Contains(t, ce.Message, tc.remedy)
			assert.Contains(t, ce.Message, "obtain the certificate from a source you trust")
			assert.NotContains(t, ce.Message, "and pass it with --cacert")
			assert.NotContains(t, ce.Message, "save it for this entry with")
			assert.NotContains(t, ce.Message, "is already registered")
			assert.Empty(t, ce.Hint)
			assert.Empty(t, p.notices, "no trust card is shown")
			assert.NotContains(t, out.String(), "trust this certificate")
			cfg, lerr := config.Load(config.FilePath(dir))
			require.NoError(t, lerr)
			if !tc.known {
				assert.NotContains(t, cfg.Servers, "prod", "a failed preflight registers nothing")
			}
		})
	}
}

// TestLoginKindsACAcannotFixKeepTheirTextUnderTheVariable asserts an expired
// certificate and a hostname mismatch keep their text under the variable and
// name no CA remedy.
func TestLoginKindsACAcannotFixKeepTheirTextUnderTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name string
		fix  tlsFixture
		kind tlstrust.Kind
		text string
	}{
		{"expired", selfSignedFixture(t, "127.0.0.1", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)), tlstrust.KindExpired, "has expired"},
		{"hostname", selfSignedFixture(t, "wronghost.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)), tlstrust.KindHostnameMismatch, "is not valid for"},
	} {
		for _, path := range []string{"--url", "plain"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				srv := newTLSFake(t, tc.fix)
				t.Setenv(config.EnvCACert, tc.fix.pem)
				dir, store := cmdtest.FilePolicyDir(t)
				seedServer(t, dir, "prod", srv.URL, "", false)
				in := plainLoginInput("prod")
				if path == "--url" {
					in = urlLoginInput("prod", srv.URL)
				}
				_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
				details, ok := ce.Details.(tlstrust.Details)
				require.True(t, ok)
				assert.Equal(t, tc.kind, details.Kind)
				assert.Contains(t, ce.Message, tc.text)
				assert.NotContains(t, ce.Message, "RANCHER_CLI_CACERT")
				assert.NotContains(t, ce.Message, "is already registered", "no pointer to the plain login, which fails the same way")
				assert.Empty(t, ce.Hint)
			})
		}
	}
}

// TestLoginDoesNotApplyTheVariableToAnotherServerThanRancherCLIURLNames
// asserts the RANCHER_CLI_URL rule on both paths: with RANCHER_CLI_URL naming
// another server the variable is skipped, so the login connects with system
// roots and fails as it does with no variable; naming the server itself, the
// variable applies.
func TestLoginDoesNotApplyTheVariableToAnotherServerThanRancherCLIURLNames(t *testing.T) {
	url, fix := variableServer(t)
	t.Setenv(config.EnvCACert, fix.pem)
	for _, path := range []string{"--url", "plain"} {
		t.Run(path, func(t *testing.T) {
			in := func() cmdtest.Input {
				if path == "--url" {
					return urlLoginInput("prod", url)
				}
				return plainLoginInput("prod")
			}
			t.Run("another server", func(t *testing.T) {
				t.Setenv(config.EnvURL, "https://other.example")
				dir, store := cmdtest.FilePolicyDir(t)
				seedServer(t, dir, "prod", url, "", false)
				_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in(), cmdtest.StdinPrompter("id:sec\n"))
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
				assert.NotContains(t, ce.Message, "RANCHER_CLI_CACERT", "the variable was not used")
			})
			t.Run("this server", func(t *testing.T) {
				t.Setenv(config.EnvURL, url)
				dir, store := cmdtest.FilePolicyDir(t)
				seedServer(t, dir, "prod", url, "", false)
				_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in(), cmdtest.StdinPrompter("id:sec\n"))
				require.NoError(t, err)
			})
		})
	}
}
