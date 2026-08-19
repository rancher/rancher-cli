package cli_test

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
)

// outputFormatCandidates is the closed -o <TAB> candidate set the root-level
// tests pin: the five literal formats plus the two template forms.
var outputFormatCandidates = []string{"table", "wide", "json", "yaml", "name", "template=", "template={{.}}"}

// TestRootSpaceFormOutputCompletion: `rancher -o <TAB>` (space form, empty
// current word) must emit exactly the output-format candidate set — not
// command names. Before the fix, rootShellComplete always routed through
// nameLikeCandidates, so a flag-value site at root listed top-level commands.
func TestRootSpaceFormOutputCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"-o"}, "")
	tokens := extractTokens(got)

	assert.ElementsMatch(t, outputFormatCandidates, tokens,
		"rancher -o <TAB> must emit exactly the output-format candidates")
}

// TestRootSpaceFormOutputCompletionPartial: `rancher -o temp<TAB>` must still
// include both template forms (the binary emits the full enum; the shell
// prefix-filters).
func TestRootSpaceFormOutputCompletionPartial(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"-o"}, "temp")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "template=", "rancher -o temp<TAB> must include template=")
	assert.Contains(t, tokens, "template={{.}}", "rancher -o temp<TAB> must include template={{.}}")
}

// TestRootEqualsFormOutputCompletion: `rancher --output=<TAB>` must emit
// full --output=<value> tokens including the template forms, same as the
// group-level equals-form path already does.
func TestRootEqualsFormOutputCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "--output=")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--output=json", "rancher --output=<TAB> must include --output=json")
	assert.Contains(t, tokens, "--output=yaml", "rancher --output=<TAB> must include --output=yaml")
	assert.Contains(t, tokens, "--output=template={{.}}", "rancher --output=<TAB> must include --output=template={{.}}")
}

// TestRootEqualsFormShortOutputCompletion: the short spelling `rancher -o=<TAB>`
// must emit -o=<value> tokens (the candidates reuse the typed spelling).
func TestRootEqualsFormShortOutputCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "-o=")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "-o=json", "rancher -o=<TAB> must include -o=json")
	assert.Contains(t, tokens, "-o=template={{.}}", "rancher -o=<TAB> must include -o=template={{.}}")
}

// TestRootBareWordStillListsCommands pins the regression boundary: a bare
// word at root (`rancher <TAB>`) must keep listing top-level commands after
// the flag-value routing fix.
func TestRootBareWordStillListsCommands(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "get", "rancher <TAB> must still list get")
	assert.Contains(t, tokens, "config", "rancher <TAB> must still list config")
	assert.Contains(t, tokens, "auth", "rancher <TAB> must still list auth")
}

// TestRootPartialFlagNameStillCompletes pins the other regression boundary:
// a partial flag name at root (`rancher --out<TAB>`) must keep suggesting
// --output.
func TestRootPartialFlagNameStillCompletes(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "--out")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--output", "rancher --out<TAB> must suggest --output")
}

// TestRootUnknownFlagEqualsFormNoCandidates: an equals-form value for a flag
// that is NOT a recognized global value-taking flag (e.g. the per-leaf
// --cluster) must produce zero candidates at root — the same guard the group
// path carries, which also keeps live HTTP from firing before a kind is chosen.
func TestRootUnknownFlagEqualsFormNoCandidates(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "--cluster=")
	assert.Empty(t, strings.TrimSpace(got),
		"rancher --cluster=<TAB> must produce no candidates at root")
}
