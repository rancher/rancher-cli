package cli_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	surfacecli "github.com/rancher/rancher-cli/surface/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runNameLikeCompletion drives a v2-protocol completion request through
// BuildRoot + root.Run. The caller must have configured env vars
// (RANCHER_CLI_CREDENTIAL_STORE, RANCHER_CLI_CONFIG_DIR) before calling.
// NOT parallel: mutates package-level rawArgv / rawCurrentWord globals.
func runNameLikeCompletion(t *testing.T, words []string, currentWord string) string {
	t.Helper()
	origV1 := surfacecli.GetRawV1CompletionForTest()
	t.Cleanup(func() { surfacecli.SetRawV1CompletionForTest(origV1) })
	scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
	scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
	surfacecli.CaptureArgvForTest(scriptArgs)
	root, err := surfacecli.BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = io.Discard
	runArgv := surfacecli.SanitizedArgsForTest(append([]string{"rancher"}, scriptArgs...))
	_ = root.Run(t.Context(), runArgv)
	return out.String()
}

// extractTokens splits completion output into the token portion of each
// candidate line (the part before the first ":", stripping trailing newlines).
func extractTokens(output string) []string {
	var tokens []string
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		tokens = append(tokens, strings.SplitN(line, ":", 2)[0])
	}
	return tokens
}

// TestFlagNameCompletionDoubleDash is the primary regression test for the
// SiteNameLike v2 bug: `usage --<TAB>` must emit long-form flag candidates
// for both the leaf's own flags (--cluster, --by, --namespace, --node) AND
// the global flags inherited from root (--output, --help). Before the fix,
// delegation to urfave's DefaultCompleteWithFlags produced no output because
// the "--" current word was stripped from argv (v2 protocol) and urfave
// always took the subcommand branch.
func TestFlagNameCompletionDoubleDash(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"usage"}, "--")
	tokens := extractTokens(got)

	// Leaf flags.
	assert.Contains(t, tokens, "--cluster")
	assert.Contains(t, tokens, "--by")
	assert.Contains(t, tokens, "--namespace")
	assert.Contains(t, tokens, "--node")
	// Global flags inherited from root.
	assert.Contains(t, tokens, "--output")
	assert.Contains(t, tokens, "--help")
	// longOnly=true: every emitted token must be a long-form flag (starts with "--").
	for _, tok := range tokens {
		assert.Truef(t, strings.HasPrefix(tok, "--"),
			"longOnly mode: expected long-form token, got %q", tok)
	}
}

// TestFlagNameCompletionSingleDash: `usage -<TAB>` must emit both short aliases
// (-o, -h) and long forms (--output, --help) — the "-" prefix enables all
// forms, not long-only.
func TestFlagNameCompletionSingleDash(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"usage"}, "-")
	tokens := extractTokens(got)

	// Short aliases must be present.
	assert.Contains(t, tokens, "-o", "-<TAB> must include short alias -o for --output")
	assert.Contains(t, tokens, "-h", "-<TAB> must include short alias -h for --help")
	// Corresponding long forms must also be present.
	assert.Contains(t, tokens, "--output")
	assert.Contains(t, tokens, "--help")
}

// TestFlagNameCompletionAliasPairing asserts that -h and --help are correctly
// paired: -h present under `-<TAB>`, --help present under `--<TAB>`.
func TestFlagNameCompletionAliasPairing(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	longTokens := extractTokens(runNameLikeCompletion(t, []string{"usage"}, "--"))
	shortTokens := extractTokens(runNameLikeCompletion(t, []string{"usage"}, "-"))

	assert.Contains(t, longTokens, "--help", "--<TAB> must include --help")
	assert.NotContains(t, shortTokens, "-help", "-<TAB> must not include -help (wrong prefix)")
	assert.Contains(t, shortTokens, "-h", "-<TAB> must include -h short alias")
	assert.NotContains(t, longTokens, "-h", "--<TAB> must not include short -h (longOnly)")
}

// TestFlagNameCompletionPartialFilter: `usage --clu<TAB>` must include
// --cluster (prefix matches) and exclude --by (prefix does not match).
func TestFlagNameCompletionPartialFilter(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"usage"}, "--clu")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--cluster", "--clu<TAB> must match --cluster")
	assert.NotContains(t, tokens, "--by", "--clu<TAB> must not include --by")
	assert.NotContains(t, tokens, "--namespace", "--clu<TAB> must not include --namespace")
}

// TestFlagNameCompletionConfigSetCompletion is the user-reported reproduction:
// `config set-completion --<TAB>` must return the set-completion leaf's flags
// (--network, --timeout) plus --help. Before the fix, output was empty.
func TestFlagNameCompletionConfigSetCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"config", "set-completion"}, "--")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--network")
	assert.Contains(t, tokens, "--timeout")
	assert.Contains(t, tokens, "--help")
}

// TestSubcommandCompletionTopLevel: `rancher <TAB>` (cur "") must list
// top-level subcommands including get, usage, config, inspect, auth.
// This goes through urfave's default completion for the root command (not
// universalShellCompleteFunc) and should pass before and after the fix.
func TestSubcommandCompletionTopLevel(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{}, "")
	assert.Contains(t, got, "get")
	assert.Contains(t, got, "usage")
	assert.Contains(t, got, "config")
	assert.Contains(t, got, "inspect")
	assert.Contains(t, got, "auth")
}

// TestSubcommandCompletionConfigGroup: `config <TAB>` (cur "") must list
// config subcommands including set-completion.
// Goes through urfave's default completion for the config group command.
func TestSubcommandCompletionConfigGroup(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"config"}, "")
	assert.Contains(t, got, "set-completion")
}

// TestFlagNameCompletionFormatColonSeparated asserts the "name:usage" line format:
// a flag with non-empty usage must be emitted as "token:usage" (colon-separated),
// and a subcommand with non-empty usage must be emitted as "name:usage".
// This guards against format drift that would silently break shell-script parsing.
func TestFlagNameCompletionFormatColonSeparated(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	// Flag colon format: --output has usage "table|json|yaml|name|template=<tpl>".
	flagOut := runNameLikeCompletion(t, []string{"usage"}, "--")
	foundFlagColon := false
	for line := range strings.SplitSeq(strings.TrimSpace(flagOut), "\n") {
		if strings.HasPrefix(line, "--output:") {
			foundFlagColon = true
			break
		}
	}
	assert.True(t, foundFlagColon, "flag output must contain a 'name:usage' colon-form line for --output")

	// Subcommand colon format: top-level subcommands like "get" have a usage string.
	subOut := runNameLikeCompletion(t, []string{}, "")
	foundSubColon := false
	for line := range strings.SplitSeq(strings.TrimSpace(subOut), "\n") {
		if strings.HasPrefix(line, "get:") || strings.HasPrefix(line, "config:") || strings.HasPrefix(line, "auth:") {
			foundSubColon = true
			break
		}
	}
	assert.True(t, foundSubColon, "subcommand output must contain a 'name:usage' colon-form line")
}

// TestFlagNameCompletionV1ProtocolLeaf verifies that the v1 protocol path is
// preserved: when no proto sentinel is present (rawProtoV2=false), the
// SiteNameLike branch delegates to urfave's DefaultCompleteWithFlags, which
// reads the partial word ("--") from cmd.Args().Slice() — urfave preserves
// "--" in parsedArgs in shellCompletion mode (command_parse.go) — and emits
// leaf flag names via printFlagSuggestions.
func TestFlagNameCompletionV1ProtocolLeaf(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	// v1 shape: no proto sentinel; "--" is a trailing argv token urfave sees.
	// CaptureArgvForTest without proto sentinel → rawProtoV2=false, rawCurrentWord="".
	origV1 := surfacecli.GetRawV1CompletionForTest()
	t.Cleanup(func() { surfacecli.SetRawV1CompletionForTest(origV1) })
	surfacecli.CaptureArgvForTest([]string{"usage", "--", "--generate-shell-completion"})
	root, err := surfacecli.BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = io.Discard
	_ = root.Run(t.Context(), []string{"rancher", "usage", "--", "--generate-shell-completion"})
	got := out.String()

	// urfave's DefaultCompleteWithFlags (v1 path) reads "--" from parsedArgs and
	// calls printFlagSuggestions, which emits the leaf's own primary flag names.
	assert.Contains(t, got, "--cluster", "v1 leaf: --cluster must appear via urfave flag path")
}

// TestGroupFlagNameCompletionConfigDoubleDash: `config --<TAB>` under v2
// protocol must emit flag candidates. Before the fix, aliasAwareGroupComplete
// read an empty lastArg from cmd.Args() (current word stripped), fell into
// the subcommand branch, and listed config subcommands instead of flags.
func TestGroupFlagNameCompletionConfigDoubleDash(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"config"}, "--")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--help", "config --<TAB> must include --help")
}

// TestGroupFlagNameCompletionGetDoubleDash: `get --<TAB>` under v2 protocol
// must emit flag candidates (global flags visible at the get group level).
func TestGroupFlagNameCompletionGetDoubleDash(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "--")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "--help", "get --<TAB> must include --help")
	assert.Contains(t, tokens, "--output", "get --<TAB> must include global --output")
}

// TestGroupSubcommandCompletionGetKindNamesRegression guards that `get <TAB>`
// still lists kind names (and aliases) after the group-completion twin fix.
// This confirms the v2-gated flag branch does NOT overwrite the existing
// alias-aware subcommand listing for non-flag currentWords.
func TestGroupSubcommandCompletionGetKindNamesRegression(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "")
	assert.Contains(t, got, "pod:", "get <TAB> must still list kind names (regression guard)")
	assert.Contains(t, got, "po:", "get <TAB> must still list kind aliases (regression guard)")
}

// TestGroupSubcommandCompletionGetPrefixFiltered: `get po<TAB>` under v2
// must return only the kind name "pod" and its alias "po", NOT the full 60+
// unfiltered kind list. This is the High finding: aliasAwareGroupComplete
// fell through to the unfiltered v1 branch for non-flag currentWords.
func TestGroupSubcommandCompletionGetPrefixFiltered(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "po")
	tokens := extractTokens(got)

	// Must include pod and its alias po (both start with "po").
	assert.Contains(t, tokens, "pod", "get po<TAB> must include pod")
	assert.Contains(t, tokens, "po", "get po<TAB> must include alias po")
	// Must NOT include kinds that don't start with "po".
	assert.NotContains(t, tokens, "gitrepos", "get po<TAB> must not include gitrepos")
	assert.NotContains(t, tokens, "bundle", "get po<TAB> must not include bundle")
	assert.NotContains(t, tokens, "settings", "get po<TAB> must not include settings")
	// Must not include the help subcommand.
	assert.NotContains(t, tokens, "help", "get po<TAB> must not include help")
}

// TestGroupEqualsFormFlagValueCompletion: `get --output=<TAB>` under v2
// must return the --output= enum candidates, NOT empty output. Before the fix,
// aliasAwareGroupComplete routed --output= into flagNameCandidates, which
// trimmed to "output=" and never matched any flag name.
func TestGroupEqualsFormFlagValueCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "--output=")
	// Must include equals-form output candidates.
	assert.Contains(t, got, "--output=json", "get --output=<TAB> must include --output=json")
	assert.Contains(t, got, "--output=yaml", "get --output=<TAB> must include --output=yaml")
	assert.Contains(t, got, "--output=table", "get --output=<TAB> must include --output=table")
	// Must not be empty.
	assert.NotEmpty(t, strings.TrimSpace(got), "get --output=<TAB> must produce output")
}

// TestLeafEqualsFormFlagValueCompletion: `usage --output=<TAB>` under v2
// already worked before this fix; guard it as a regression test alongside
// the group path.
func TestLeafEqualsFormFlagValueCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"usage"}, "--output=")
	assert.Contains(t, got, "--output=json", "usage --output=<TAB> must include --output=json")
	assert.Contains(t, got, "--output=yaml", "usage --output=<TAB> must include --output=yaml")
}

// TestGroupAliasOnlyPrefixCompletion: `get ns<TAB>` must return the alias "ns"
// (and NOT "namespace", which doesn't start with "ns"). Before the fix, the alias
// loop was gated on the canonical name prefix-matching first — since "namespace"
// does not start with "ns", the alias loop never ran and zero candidates were
// returned.
func TestGroupAliasOnlyPrefixCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "ns")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "ns", "get ns<TAB> must include alias ns")
	assert.NotContains(t, tokens, "namespace", "get ns<TAB> must not include namespace (doesn't share prefix)")
}

// TestGroupAliasOnlyPrefixSvc: `get svc<TAB>` must return "svc" (alias for
// service). "service" does not start with "svc", so this only passes after the
// independent alias-filter fix.
func TestGroupAliasOnlyPrefixSvc(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get"}, "svc")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "svc", "get svc<TAB> must include alias svc")
	assert.NotContains(t, tokens, "service", "get svc<TAB> must not include service (doesn't share prefix)")
}

// TestGroupSpaceFormFlagValueCompletion: `get --output <TAB>` (space form,
// current word empty after the value-taking flag) must return bare enum values
// (json/yaml/table/name), not the kind list. Before the fix, the empty
// rawCurrentWord fell through to the bare-word/kind-list branch.
func TestGroupSpaceFormFlagValueCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	// Simulate: argv has "get --output" already; current word is "".
	got := runNameLikeCompletion(t, []string{"get", "--output"}, "")
	tokens := extractTokens(got)

	assert.Contains(t, tokens, "json", "get --output <TAB> must include json")
	assert.Contains(t, tokens, "yaml", "get --output <TAB> must include yaml")
	assert.Contains(t, tokens, "table", "get --output <TAB> must include table")
	assert.Contains(t, tokens, "name", "get --output <TAB> must include name")
	assert.NotContains(t, tokens, "template", "get --output <TAB> must not suggest bare template (not a valid value)")
	assert.Contains(t, tokens, "template=", "get --output <TAB> must include the template= form for discoverability")
	assert.Contains(t, tokens, "template={{.}}", "get --output <TAB> must include the runnable seed template")
	// Must not spill into kind names.
	assert.NotContains(t, tokens, "pod", "get --output <TAB> must not include kind names")
}

// TestImagesByFlagValueCompletion: `images --by <TAB>` must offer exactly
// the enum values image and workload (IM-22); --match must offer nothing.
func TestImagesByFlagValueCompletion(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"images", "--by"}, "")
	tokens := extractTokens(got)
	assert.Contains(t, tokens, "image", "images --by <TAB> must include image")
	assert.Contains(t, tokens, "workload", "images --by <TAB> must include workload")
	assert.NotContains(t, tokens, "namespace", "images --by <TAB> must not leak usage's enum")

	got = runNameLikeCompletion(t, []string{"images", "--match"}, "")
	assert.Empty(t, extractTokens(got), "images --match <TAB> is free text, no suggestions (IM-23)")
}

// TestFlagNameCompletionIncludeSystem asserts the no-silent-no-op generation
// site: --include-system is offered on `get users` (SystemFilter kind), but
// absent on `get pods` (no SystemFilter) and on `delete user` (the shared
// getFlags core, reused by delete/patch, must never carry it).
func TestFlagNameCompletionIncludeSystem(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get", "users"}, "--")
	assert.Contains(t, extractTokens(got), "--include-system", "get users --<TAB> must offer --include-system")

	got = runNameLikeCompletion(t, []string{"get", "pods"}, "--")
	assert.NotContains(t, extractTokens(got), "--include-system", "get pods --<TAB> must not offer --include-system")

	got = runNameLikeCompletion(t, []string{"delete", "user"}, "--")
	assert.NotContains(t, extractTokens(got), "--include-system", "delete user --<TAB> must not offer --include-system")
}

// TestFlagNameCompletionSelector asserts --selector is offered on every
// get, never on delete or patch (the shared getFlags core), and that its
// value has no candidates: a label selector is free text.
func TestFlagNameCompletionSelector(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord globals and env.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	got := runNameLikeCompletion(t, []string{"get", "pods"}, "--")
	assert.Contains(t, extractTokens(got), "--selector", "get pods --<TAB> must offer --selector")

	got = runNameLikeCompletion(t, []string{"delete", "pod"}, "--")
	assert.NotContains(t, extractTokens(got), "--selector", "delete pod --<TAB> must not offer --selector")

	got = runNameLikeCompletion(t, []string{"patch", "pod"}, "--")
	assert.NotContains(t, extractTokens(got), "--selector", "patch pod --<TAB> must not offer --selector")

	got = runNameLikeCompletion(t, []string{"get", "pods", "--selector"}, "")
	assert.Empty(t, extractTokens(got), "a label selector is free text: no candidates")
}
