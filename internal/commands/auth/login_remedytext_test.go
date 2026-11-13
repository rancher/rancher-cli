package auth

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// DashedNameRemedy is a text a login or config command prints for an entry
// named "-x", and the one command in it that the text must end its clause
// with. The external tests of this package parse every Command with the
// command tree.
type DashedNameRemedy struct {
	Site    string
	Text    string
	Command string
}

// DashedNameRemedies runs each login path that writes a command naming an
// entry, with the entry named "-x", and returns what it printed.
func DashedNameRemedies(t *testing.T) []DashedNameRemedy {
	t.Helper()
	const name = "-x"
	var rows []DashedNameRemedy
	add := func(site, text, command string) {
		rows = append(rows, DashedNameRemedy{Site: site, Text: text, Command: clierr.Rancher + command})
	}

	saveCA := " config update-server --cacert <file> -- '-x'"
	add("the CA from the variable, plain login", loginVariableRemedy(false, name), saveCA)
	add("save the CA for the entry", loginSaveCARemedy(name), saveCA)
	add("no saved CA notice", variableNotice(name, ""), saveCA)
	add("different saved CA notice", variableNotice(name, "/etc/saved.pem"), saveCA)

	collision := nameCollisionError(name, "https://old.example.com", "https://new.example.com", false)
	add("name collision hint", collision.Hint, " config update-server --url 'https://new.example.com' -- '-x'")

	cfg := &config.Config{Servers: map[string]config.Server{name: {URL: "https://r.example.com"}}}
	_, err := resolveLoginTarget(cfg, name, "", "/etc/ca.pem")
	ce := asClierr(t, err)
	add("--cacert without --url", ce.Hint, " config update-server --cacert '/etc/ca.pem' -- '-x'")

	add("switch hint", configUseServerHint(name).String(), " config use-server -- '-x'")

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, "https://r.ex", "", false)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}}, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	add("the current server is not set", lineHolding(t, res.Text, "the current server is not set"), " config use-server -- '-x'")

	dir, store = cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://prod.ex", "", true)
	seedServer(t, dir, name, "https://x.ex", "", false)
	res, err = newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}}, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	add("the current server remains another", lineHolding(t, res.Text, "the current server remains"), " config use-server -- '-x'")

	dir, store = cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, "https://r.ex", filepath.Join(t.TempDir(), "missing.pem"), false)
	_, err = tlsLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}}, cmdtest.StdinPrompter("id:sec\n"))
	ce = asClierr(t, err)
	add("the stored CA file is missing", ce.Message, " config delete-server -- '-x'")

	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	tlsSrv := newTLSFake(t, fix)
	dir, store = cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, tlsSrv.URL, "", false)
	_, err = tlsLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": tlsSrv.URL}, Pos: []string{name}},
		ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true))
	ce = asClierr(t, err)
	add("a known entry with a rotated CA", ce.Message, " auth login -- '-x'")

	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store = cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, srv.URL, "", false)
	cmd := newPlainLoginCmd(dir, store)
	cmd.beforeCommitHook = func() {
		cfg, _ := config.Load(config.FilePath(dir))
		cfg.Servers[name] = config.Server{URL: srv.URL + "/changed"}
		_ = cfg.Write()
	}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{name}}
	_, err = cmd.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	ce = asClierr(t, err)
	add("the entry changed during the login", ce.Hint, " auth login -- '-x'")

	dir, _ = cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, "https://r.ex", "", false)
	snap := register.AuthSnapshot{Name: name, CanonicalURL: "https://r.ex"}
	verifyErr := clierr.New(clierr.CodeGeneric, "verify failed")
	undone := asClierr(t, decorateDelegatedVerifyFailure(t.Context(), dir, verifyErr, snap))
	add("undo the registration", undone.Message, " config delete-server -- '-x'")
	add("log in again after the registration", undone.Hint, " auth login -- '-x'")
	return rows
}

// ExpectedCommand is a command a text must hold, and the arguments the
// command tree reads from it once its placeholders hold a value.
type ExpectedCommand struct {
	Command string
	Args    []string
}

// FixedCommandRemedy is a text of a login, logout or status path that names
// commands with no entry name of the caller's: each Command must be in Text
// and end its clause.
type FixedCommandRemedy struct {
	Site     string
	Text     string
	Commands []ExpectedCommand
}

// FixedCommandRemedies runs the paths that name a command with a placeholder
// or with no argument, and returns what they printed.
func FixedCommandRemedies(t *testing.T) []FixedCommandRemedy {
	t.Helper()
	var rows []FixedCommandRemedy
	add := func(site, text string, commands ...ExpectedCommand) {
		marked := make([]ExpectedCommand, 0, len(commands))
		for _, c := range commands {
			marked = append(marked, ExpectedCommand{Command: clierr.Rancher + c.Command, Args: c.Args})
		}
		rows = append(rows, FixedCommandRemedy{Site: site, Text: text, Commands: marked})
	}
	name := []string{"x"}

	dir, _ := cmdtest.FilePolicyDir(t)
	_, err := resolveLoginTarget(&config.Config{Servers: map[string]config.Server{}}, "", "", "/etc/ca.pem")
	add("--cacert with no name and no --url", asClierr(t, err).Message,
		ExpectedCommand{" config update-server <name> --cacert <file>", name})
	_, err = resolveLoginTarget(&config.Config{Servers: map[string]config.Server{}}, "", "", "")
	add("no servers configured", asClierr(t, err).Message, ExpectedCommand{" auth login --url <url>", nil})
	_, err = urlLoginTarget(&config.Config{Servers: map[string]config.Server{}}, "", "https://[::1]:1", "")
	add("a name cannot be derived", asClierr(t, err).Message, ExpectedCommand{" auth login <name> --url <url>", name})

	logoutBoth := []ExpectedCommand{{" auth logout <name>", name}, {" auth logout --all", nil}}
	_, err = (&logoutCmd{dir: dir}).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}, Pos: []string{"prod"}}, ui.Discard())
	add("a name and --all", asClierr(t, err).Message, logoutBoth...)
	_, err = (&logoutCmd{dir: dir}).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	add("no current server", asClierr(t, err).Message, logoutBoth...)

	add("the rotated token advisory", rotatedTokenAdvisory(), ExpectedCommand{" auth login", nil})

	seedServer(t, dir, "prod", "https://r.ex", "", false)
	changed := register.AuthSnapshot{Name: "prod", CanonicalURL: "https://other.ex"}
	got := decorateDelegatedVerifyFailure(t.Context(), dir, clierr.New(clierr.CodeGeneric, "verify failed"), changed)
	add("the entry changed since the registration", asClierr(t, got).Message, ExpectedCommand{" config list-servers", nil})
	return rows
}

// asClierr returns the structured error err holds.
func asClierr(t *testing.T, err error) *clierr.Error {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	return ce
}

// lineHolding returns the line of text that holds want.
func lineHolding(t *testing.T, text, want string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	require.Failf(t, "no such line", "%q is not in %q", want, text)
	return ""
}

// TestDashedNameCommandsEndTheirClauseAfterTheFlags asserts each command a
// login path writes for an entry whose name starts with a dash puts the
// flags first and "--" before the name, and is followed only by the end of
// the text or a semicolon.
func TestDashedNameCommandsEndTheirClauseAfterTheFlags(t *testing.T) {
	t.Parallel()
	for _, row := range DashedNameRemedies(t) {
		t.Run(row.Site, func(t *testing.T) {
			_, after, found := strings.Cut(row.Text, row.Command)
			require.True(t, found, "%q is not in %q", row.Command, row.Text)
			assert.True(t, after == "" || strings.HasPrefix(after, ";"), "the command does not end its clause: %q", after)
		})
	}
}

// TestADelegatedVerifyFailureEndsWithItsCommand asserts the verify failure
// of a login that registered its entry: a non-TLS failure names the option to
// bypass verification as the first alternative to the undo, a TLS_TRUST
// failure has no option, and the command is last. An entry that changed since
// the registration gets the command that lists the entries, with no hint.
func TestADelegatedVerifyFailureEndsWithItsCommand(t *testing.T) {
	t.Parallel()
	del := clierr.Rancher + " config delete-server 'prod'"
	list := clierr.Rancher + " config list-servers"
	for _, tc := range []struct {
		name        string
		err         *clierr.Error
		wantUndone  string
		wantChanged string
		wantCut     string
	}{
		{"a TLS_TRUST failure", clierr.New(clierr.CodeTLSTrust, "certificate is not trusted"),
			"certificate is not trusted; undo the registration with: " + del,
			`certificate is not trusted; server entry "prod" may have changed since registration; check it with: ` + list,
			"certificate is not trusted; " + runtime.UnprintableConfigDirNote},
		{"a failure that is not TLS", clierr.New(clierr.CodeGeneric, "verify failed"),
			"verify failed; bypass verification with --no-verify; or undo the registration with: " + del,
			`verify failed; bypass verification with --no-verify; server entry "prod" may have changed since registration; check it with: ` + list,
			"verify failed; bypass verification with --no-verify; " + runtime.UnprintableConfigDirNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", "https://r.ex", "", false)

			current := register.AuthSnapshot{Name: "prod", CanonicalURL: "https://r.ex"}
			got := asClierr(t, decorateDelegatedVerifyFailure(t.Context(), dir, tc.err, current))
			assert.Equal(t, tc.wantUndone, got.Message)
			cut := runtime.IO{RemedyDir: "/d\x01"}.ExpandText(got.Message)
			assert.Equal(t, tc.wantCut, cut, "a cut for an unprintable directory keeps the facts and the bypass")
			assert.Equal(t, clierr.Rancher+" auth login 'prod'", got.Hint)

			changed := register.AuthSnapshot{Name: "prod", CanonicalURL: "https://other.ex"}
			got = asClierr(t, decorateDelegatedVerifyFailure(t.Context(), dir, tc.err, changed))
			assert.Equal(t, tc.wantChanged, got.Message)
			assert.Empty(t, got.Hint)
		})
	}
}

// TestALoginHintIsWrittenOnlyForAnEntryAuthLoginAccepts asserts that auth
// login refuses the exec plugin's own login entry names before the commit
// stage, with no hint, and that the builder of the login hint gives none for
// such a name and the login command for an ordinary one. The hint of an entry
// that changed during a login is asserted for a dashed name in the table of
// DashedNameRemedies and for "prod" in TestLoginConflictAfterDerivedTokenRevokesAndReports.
func TestALoginHintIsWrittenOnlyForAnEntryAuthLoginAccepts(t *testing.T) {
	t.Parallel()
	const name = "exec-login-r.example.com"
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, name, srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{name}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	ce := asClierr(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
	_, ok := config.LoginHintFor(name)
	assert.False(t, ok)
	prod, ok := config.LoginHintFor("prod")
	assert.True(t, ok)
	assert.Equal(t, clierr.Rancher+" auth login 'prod'", prod.String())
}

// TestTheFactBeforeACommandSurvivesTheCutOfAnUnprintableDirectory asserts that
// when the config directory cannot be printed and the commands are left out,
// each text that states a fact before its command keeps the fact, and a text
// that is only a command keeps nothing of it.
func TestTheFactBeforeACommandSurvivesTheCutOfAnUnprintableDirectory(t *testing.T) {
	t.Parallel()
	kept := map[string]string{
		"the CA from the variable, plain login":    "the CA came from RANCHER_CLI_CACERT",
		"no saved CA notice":                       "has no saved CA",
		"different saved CA notice":                "keeps its saved CA",
		"the current server is not set":            "the current server is not set",
		"the current server remains another":       "the current server remains",
		"the stored CA file is missing":            "reading stored CA for server entry",
		"a known entry with a rotated CA":          "is already registered",
		"undo the registration":                    "verify failed",
		"--cacert with no name and no --url":       "--cacert without --url",
		"no servers configured":                    "no servers configured",
		"a name cannot be derived":                 "a server name cannot be derived from an IPv6 address",
		"a name and --all":                         "specify a server name or --all, not both",
		"no current server":                        "no current server",
		"the rotated token advisory":               "stored token not found on the server (revoked or rotated)",
		"the entry changed since the registration": "verify failed",
	}
	commandOnly := []string{"save the CA for the entry", "name collision hint", "--cacert without --url", "switch hint",
		"the entry changed during the login", "log in again after the registration"}
	cut := func(text string) string { return runtime.IO{RemedyDir: "/d\x01"}.ExpandText(text) }
	seen := map[string]bool{}
	check := func(site, text string) {
		seen[site] = true
		got := cut(text)
		assert.NotContains(t, got, "rancher", site)
		if fact, ok := kept[site]; ok {
			assert.Contains(t, got, fact, site)
			assert.NotEqual(t, runtime.UnprintableConfigDirNote, got, "%s: the note alone", site)
			return
		}
		assert.True(t, slices.Contains(commandOnly, site), "%s has no expected fact", site)
	}
	for _, row := range DashedNameRemedies(t) {
		check(row.Site, row.Text)
	}
	for _, row := range FixedCommandRemedies(t) {
		check(row.Site, row.Text)
	}
	for site := range kept {
		assert.True(t, seen[site], "%s is in no table", site)
	}
}
