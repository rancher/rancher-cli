package conformance

import (
	"cmp"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRejectedWordingStaysOut asserts that no message, help text or user
// document uses a word or phrase the project replaced with a plainer one.
// A replaced word cannot come back through a later change.
//
// What is read:
//   - Every string literal of the production Go source, with the same walk
//     as TestNoCommaSoInMessages: a + chain is one string, comments and
//     identifiers are never read. A string with no whitespace is not prose
//     (a flag, an environment variable, a field name, an error code, a URL
//     path, a machine value) and is skipped. The packages that only
//     support tests are left out: their strings are fixtures and fake
//     server replies.
//   - README.md, COMMANDS.md, REFERENCE.md, MIGRATING.md,
//     skills/rancher/SKILL.md and CREDENTIAL_HELPERS.md, one paragraph at
//     a time: a run of non-blank lines outside code fences, joined with
//     single spaces, so a phrase wrapped across two lines is found. A
//     heading is a paragraph of its own and names the section of the lines
//     below it. Inline code spans and fenced code blocks (at any indent)
//     are not read. A fence that never closes fails the test, because it
//     hides everything below it. Indented (four-space) code blocks are read
//     as prose.
//   - install.sh, install.ps1, .github/ISSUE_TEMPLATE/bug.yml,
//     .github/ISSUE_TEMPLATE/feature.yml and scripts/check-release-tag, one
//     line at a time. A line whose first non-blank characters are # or // is
//     a comment and is not read, as Go comments are not; every other line is
//     one unit: a phrase wrapped across two lines is not found. Code
//     spans are not blanked. A hit reports file:line. The completion
//     scripts and .github/ISSUE_TEMPLATE/config.yml (no prose) are not read.
//
// Matching is case-insensitive and whole-word: the characters on each side
// of a match are not a letter, a digit, a hyphen or an underscore, and the
// words of a phrase are joined by any run of whitespace. Each row of
// rejectedWording lists every form it refuses and the wording to use
// instead; the first form is the row's name.
//
// A use that is allowed is listed in one of two lists. The phrase list
// names a longer phrase, compared case-sensitively, that contains the
// term and is fine. The path list names a term, a file, for a document the
// section, the exact number of uses, and a reason: either "text not yet
// reworded" for a use that a later change replaces, or a permanent reason
// for a use that is not prose, such as a column header. A use inside a
// listed phrase counts for no path entry. Every entry must match at least
// one use, and a path entry's count must equal the uses found, so a new
// use inside a covered file or section fails. The change that rewords the
// text of a row removes its "text not yet reworded" entries, and the change
// that stops a further word adds a row.
//
// Words left out of the table, each for its own reason:
//   - holds, hold, held: mostly another sense (a store or a lock holds
//     something).
//   - quota: Rancher's phrase "resource quota" is the term to use, and a
//     bare "quota" would need a phrase entry for each of about twelve uses.
//   - rotate, rotation: a token rotated on the server is another object;
//     the command name rotate-secret never matches (hyphen rule).
//   - word: the replaced sense (a command-line item) cannot be told from
//     the ordinary word by pattern.
//   - lifetime, identity record: more false positives than uses.
//   - auth provider, auth providers: Rancher's API names them
//     (authproviders) and the flag is --auth-provider.
//   - Downstream: the replacement differs only by case.
//   - kill (verb), create request: ordinary senses ("kill the process"; an
//     HTTP create request).
//   - the helper, caller, key, request, secret, note, token, files:
//     ordinary nouns in this repository.
//   - DISPLAYNAME: matching is case-insensitive, so a row would also
//     refuse the API field name displayName, which prose literals name as a
//     field. The table headers are checked by
//     TestDisplayNameColumnsAreHeadedDisplayName in internal/engine/kinds.
//   - ops: the replaced sense (an RFC 6902 entry, now "operation") cannot
//     be told from "server ops" (the CLI's commands) by pattern; the phrase
//     named ops is a row of the table.
//   - cascades, consumed: other text still uses them in their own sense (the
//     description of a user delete in skills/rancher/SKILL.md).
//   - decoding: an ordinary word ("decoding the object of row 3"); the decode
//     errors of the registration token commands now say "cannot read".
//   - converges: an ordinary Kubernetes word.
//   - Sensitive kind, dry-run/apply --yes: the command table in COMMANDS.md
//     uses dry-run/apply --yes, and skills/rancher/SKILL.md uses Sensitive
//     kind, as the names of the gate.
//   - curated (alone): ordinary word here (curated commands and kinds); the
//     phrases for logs and targets of Rancher components are refused.
//   - pool, probe (alone): "machine pool" and "SelfUser request" are the terms;
//     the bare words name flags, names and the fidelity vocabulary.
//   - stamp (the noun): the cache binding stamp and the logged-out stamp are
//     named values; only "stamped", "stamps" and "unstamped" are refused.
//   - discovered: the Kubernetes API discovery sense ("discovered: <list>").
//   - stale: an ordinary adjective for a token or an owner reference.
//   - collides with: an exit code collision is the plain sense.
//   - envelope (alone): the CLI's name for its JSON output; only "envelope key"
//     is refused.
//   - effect, Effect: the ordinary word ("no effect", "takes effect", a
//     table header); the rejected compound is the row dry-run effect.
//   - members (for the namespaces of a project): the word also means
//     people, so only the phrase member namespaces is refused.
//   - best-effort, superseded, authenticated, resolution, receipt, harness,
//     interrupted, redact: ordinary words in other text ("mandatory instead
//     of best-effort", "a superseded token", "system:authenticated",
//     "Credential-store resolution", "install receipt", "coding-agent
//     harnesses"); only the compounds in the table are refused.
//   - authenticated gh: the document phrases hold a code span, which the
//     document scan blanks: no form could match them.
//   - capable (alone), violates, this means: ordinary words.
//   - collector: "node collector" is the plain name of the support-tools
//     component; comments and one internal string name the code's own
//     collectors.
//   - management plane, identity, scope: ordinary words with other uses.
//   - enrichment, annotated, verdict, advisory, disconnected, not available,
//     unknown, marker, scan: ordinary words with other uses.
//   - rancher cli: the term is rancher-cli, and other text still says
//     rancher cli.
//   - whole value: an ordinary phrase.
//   - setting, settings: ordinary words with other uses (a Rancher Setting,
//     the settings of a command); the replaced sense, the telemetry key,
//     cannot be told from them by pattern.
//   - control, controls: ordinary words with other uses; the replaced
//     sense (what decided recording) cannot be told from them by pattern.
//   - lost (alone): an ordinary adjective; only the phrases lose events and
//     lost events are refused.
//   - receiver: an ordinary word (a method receiver); the replaced sense, a
//     reader of usage events, cannot be told from it by pattern.
//   - star: an ordinary word; the replaced sense (the "*" of a hidden path)
//     cannot be told from it by pattern.
//   - snapshot object: a part of the chosen name "etcd snapshot object".
//   - fleet workspace, the server's table: matching ignores case, so a row
//     would also refuse "Fleet workspace" and "the server's Table".
func TestRejectedWordingStaysOut(t *testing.T) {
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	require.NotEmpty(t, tree.files, "the walk saw no source files")

	units := productionWordingUnits(tree.fset, tree.files, tree.paths)
	unreadable := false
	for _, path := range wordingDocs {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		docUnits, err := readWordingDoc(path, string(src))
		if err != nil {
			assert.Fail(t, err.Error())
			unreadable = true
			continue
		}
		units = append(units, docUnits...)
	}
	for _, path := range wordingScripts {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		units = append(units, readWordingScript(path, string(src))...)
	}
	if unreadable {
		return
	}
	require.NotEmpty(t, units)

	for _, failure := range checkWording(rejectedWording, wordingPhraseAllowlist, wordingPathAllowlist, units) {
		assert.Fail(t, failure)
	}
}

// helpFlag is the part of a flag's entry in the schema golden that the help
// text test reads.
type helpFlag struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// helpLifetimeAllowed lists the flags whose help may still say "lifetime",
// each with the reason. A listed flag whose help no longer says it fails the
// test, so the entry goes when the text changes.
var helpLifetimeAllowed = map[string]string{
	"auth login --ttl": "text not yet reworded",
}

// TestHelpTextUsesDecidedWords asserts that no flag help in the schema golden
// says "lifetime" except the flags in helpLifetimeAllowed (a token's time
// setting is its "expiration"), and that no flag named description begins
// its help with "note" (the value is a "description").
// The schema golden is tied to the code by TestSchemaManifestGolden, so the
// help in the code is covered through it. The two words are not rows of
// rejectedWording: "lifetime" and "note" are ordinary nouns elsewhere.
func TestHelpTextUsesDecidedWords(t *testing.T) {
	raw, err := os.ReadFile("internal/cli/testdata/schema.golden.json")
	require.NoError(t, err)
	var manifest struct {
		Commands []struct {
			Name  string     `json:"name"`
			Flags []helpFlag `json:"flags"`
		} `json:"commands"`
		GlobalFlags []helpFlag `json:"globalFlags"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.NotEmpty(t, manifest.Commands)

	lifetime := regexp.MustCompile(`(?i)\blifetime\b`)
	seen := map[string]bool{}
	check := func(command string, flag helpFlag) {
		label := strings.TrimSpace(command + " --" + strings.TrimPrefix(flag.Name, "--"))
		if lifetime.MatchString(flag.Desc) {
			if _, ok := helpLifetimeAllowed[label]; ok {
				seen[label] = true
			} else {
				assert.Fail(t, label+": the help says \"lifetime\"; say \"expiration\": "+flag.Desc)
			}
		}
		if strings.TrimPrefix(flag.Name, "--") == "description" && strings.HasPrefix(strings.ToLower(flag.Desc), "note") {
			assert.Fail(t, label+": the help begins with \"note\"; say \"description\": "+flag.Desc)
		}
	}
	for _, command := range manifest.Commands {
		for _, flag := range command.Flags {
			check(command.Name, flag)
		}
	}
	for _, flag := range manifest.GlobalFlags {
		check("", flag)
	}
	for label := range helpLifetimeAllowed {
		assert.True(t, seen[label], "%s is listed in helpLifetimeAllowed but its help no longer says \"lifetime\"", label)
	}
}

// TestRejectedWordingTables asserts the term table and both allowlists are
// laid out for conflict-free edits: one entry per line, sorted, no
// duplicates, every entry named after a table row, every entry with a reason.
func TestRejectedWordingTables(t *testing.T) {
	for _, problem := range validateWordingTables(rejectedWording, wordingPhraseAllowlist, wordingPathAllowlist) {
		assert.Fail(t, problem)
	}
	assert.NotEmpty(t, rejectedWording)
}

// wordingDocs are the user documents the scan reads.
var wordingDocs = []string{
	"README.md",
	"COMMANDS.md",
	"REFERENCE.md",
	"MIGRATING.md",
	"skills/rancher/SKILL.md",
	"CREDENTIAL_HELPERS.md",
}

// wordingScripts are the install scripts, the issue forms and the release tag
// check the scan reads, one line at a time.
var wordingScripts = []string{
	"install.sh",
	"install.ps1",
	".github/ISSUE_TEMPLATE/bug.yml",
	".github/ISSUE_TEMPLATE/feature.yml",
	"scripts/check-release-tag",
}

// wordingSkippedPrefixes are packages that only support tests; the walk
// reads them because they are not _test.go files, and their strings are
// fixtures and fake server replies.
var wordingSkippedPrefixes = []string{
	"internal/testrancher/",
	"internal/testtransport/",
	"internal/remedytest/",
	"internal/testfs/",
	"internal/envtestlog/",
	"internal/commands/cmdtest/",
	"internal/runtime/completion/completiontest/",
}

func wordingSkipsPath(path string) bool {
	return slices.ContainsFunc(wordingSkippedPrefixes, func(p string) bool {
		return strings.HasPrefix(path, p)
	})
}

// wordingRow is one term: every form it refuses (the first is the row's
// name) and the wording to use instead.
type wordingRow struct {
	forms   []string
	instead string
}

// rejectedWording is the term table: one row per line, sorted by the first
// form.
var rejectedWording = []wordingRow{
	{[]string{"ace clusters"}, "clusters with an Authorized Cluster Endpoint"},
	{[]string{"ai agent", "ai agent harness", "ai agents"}, "coding agent (a coding-agent harness)"},
	{[]string{"allowlisted"}, "a fixed list of"},
	{[]string{"api-reachable"}, "what Rancher's Kubernetes API can reach"},
	{[]string{"as you"}, "with your permissions"},
	{[]string{"audit extras"}, "extra information for audit logs"},
	{[]string{"auth bootstrap"}, "authentication with your stored login"},
	{[]string{"auth lifecycle"}, "name the steps (key generation, TTL clamp, identity derivation)"},
	{[]string{"backing secret", "backing secrets"}, "Secret"},
	{[]string{"bare command", "bare commands"}, "the exec plugin's command"},
	{[]string{"bespoke"}, "dedicated"},
	{[]string{"bin-dir", "bin-dirs", "bin dir", "bin dirs"}, "bin directory"},
	{[]string{"budget", "budgets"}, "time limit"},
	{[]string{"caller identity"}, "your user ID"},
	{[]string{"caller-scoped"}, "with your permissions"},
	{[]string{"canonical owner identity"}, "the owner's apiVersion|kind|namespace|name|uid"},
	{[]string{"capability manifest"}, "schema manifest"},
	{[]string{"capable authenticated"}, "a gh CLI 2.68.0 or newer that is logged in"},
	{[]string{"cdn fetch", "storage fetch"}, "could not download from the redirect URL"},
	{[]string{"channel server route"}, "name the path: it was not found on the server"},
	{[]string{"clears cleanly"}, "succeeds and changes nothing"},
	{[]string{"closed list", "closed lists"}, "fixed list"},
	{[]string{"command bug", "command bugs"}, "internal error"},
	{[]string{"computed statement"}, "say that the bindings do not show what a user can do"},
	{[]string{"configuration directory"}, "config directory"},
	{[]string{"controller-owned"}, "a controller creates and manages it"},
	{[]string{"correlation"}, "the lookup of the provisioning Cluster"},
	{[]string{"could not be checked"}, "could not be read"},
	{[]string{"credential store helper", "credential store helpers"}, "credential helper"},
	{[]string{"curated component logs", "curated logs", "curated rancher-component logs", "curated target", "curated targets"}, "logs of Rancher components"},
	{[]string{"curated flag", "curated flags"}, "the command's own flags"},
	{[]string{"current switch"}, "the 'current' symlink does not point to the installed version"},
	{[]string{"degradable"}, "that can degrade"},
	{[]string{"degrades rather than fails"}, "the command still exits 0 and names what it could not read"},
	{[]string{"degrades to", "degrade to", "degraded to"}, "say what the command shows instead"},
	{[]string{"direct context", "direct contexts", "direct endpoint", "direct endpoints"}, "Authorized Cluster Endpoint"},
	{[]string{"do not resolve"}, "are broken or not executable"},
	{[]string{"downstream-client"}, "a failure to connect to the downstream cluster"},
	{[]string{"dry-run effect", "dry-run effects"}, "dry-run preview"},
	{[]string{"envelope key", "envelope keys"}, "top-level Kubernetes object field"},
	{[]string{"escalates to", "escalate to"}, "requires"},
	{[]string{"escape hatch"}, "say what it does and when to use it"},
	{[]string{"etcd snapshot file", "etcd snapshot files"}, "snapshot file"},
	{[]string{"exec credential plugin"}, "exec plugin"},
	{[]string{"exec entry", "exec entries", "exec user entry"}, "exec block"},
	{[]string{"executable", "executables"}, "binaries"},
	{[]string{"exit status"}, "exit code"},
	{[]string{"first-level reference"}, "the pod's direct owner"},
	{[]string{"flat contract form", "flat form"}, "flat event"},
	{[]string{"fleet workspace namespace"}, "Fleet workspace (namespace)"},
	{[]string{"floor", "floors", "version-floor", "version-floors"}, "minimum"},
	{[]string{"go duration"}, "a duration such as 90m or 12h"},
	{[]string{"group-sync cache"}, "the group memberships Rancher last refreshed"},
	{[]string{"half-set"}, "has only one of the two fields set"},
	{[]string{"hard-error", "hard-errors"}, "fails"},
	{[]string{"id extraction"}, "reads the asset ids"},
	{[]string{"identity probe", "identity probes", "selfuser probe", "selfuser probes"}, "SelfUser request"},
	{[]string{"internal duration"}, "state the limit"},
	{[]string{"join credentials", "join token"}, "cluster registration token"},
	{[]string{"junction", "junctions"}, "a Windows directory link (NTFS junction)"},
	{[]string{"kept version"}, "a version the index no longer lists"},
	{[]string{"killed"}, "stopped"},
	{[]string{"kind view", "kind views"}, "a sentence: to see one kind, run: <command>"},
	{[]string{"kinds array"}, "kind names"},
	{[]string{"layout is corrupt"}, "the data directory is damaged"},
	{[]string{"legacy spelling"}, "older name"},
	{[]string{"list item segment"}, "wildcard segment"},
	{[]string{"listing is exhausted"}, "no published release found in the release listing"},
	{[]string{"lose events", "lost events"}, "dropped events"},
	{[]string{"machine-config kind", "machine-config kinds"}, "machine config kind"},
	{[]string{"main container"}, "the containers and init containers of each pod"},
	{[]string{"management cluster id", "management cluster ids", "management cluster's name"}, "the cluster's id (c-...)"},
	{[]string{"manual quantity"}, "number of nodes"},
	{[]string{"member namespaces", "member namespace"}, "the project's namespaces"},
	{[]string{"membership set"}, "the project's namespaces"},
	{[]string{"named get", "named gets"}, "a get of one object"},
	{[]string{"named ops"}, "commands that take an object name"},
	{[]string{"namespace map", "project map"}, "the list of namespaces or projects"},
	{[]string{"needs auth"}, "needs a GitHub token"},
	{[]string{"no-follow replace"}, "replace a symlink without following it"},
	{[]string{"no-op"}, "changes nothing"},
	{[]string{"node count"}, "number of nodes"},
	{[]string{"node join", "node-join"}, "node command"},
	{[]string{"node-driver pool", "node-driver pools"}, "machine pool"},
	{[]string{"observe the following output"}, "see this output"},
	{[]string{"oci normalization"}, "a short reference is not expanded to a full registry name"},
	{[]string{"one fate"}, "are read together"},
	{[]string{"one read of its id"}, "gets it by its id"},
	{[]string{"ordinary secret", "ordinary secrets"}, "a secret in a namespace"},
	{[]string{"owner chain"}, "the pod's owners"},
	{[]string{"owning source object"}, "the object it was created from"},
	{[]string{"page cap"}, "stopped after 100 pages"},
	{[]string{"paired warning", "paired warnings"}, "one warning for each entry"},
	{[]string{"pass a project namespace"}, "pass one of the project's namespaces"},
	{[]string{"per-replica"}, "of a single replica"},
	{[]string{"positional value", "positional values"}, "argument value"},
	{[]string{"principal form", "principal forms"}, "principal prefix"},
	{[]string{"project bucket", "project-namespace rows"}, "one row for the project"},
	{[]string{"provider list", "provider lists"}, "list of auth configs"},
	{[]string{"quorum majority"}, "quorum"},
	{[]string{"rancher dashboard"}, "Rancher UI"},
	{[]string{"re-asserts", "reasserts"}, "sets it back"},
	{[]string{"receipt skipped"}, "install receipt not written"},
	{[]string{"release fetch", "release fetches"}, "fetching the releases"},
	{[]string{"release resolution", "prerelease resolution"}, "look up the latest stable release"},
	{[]string{"resolver binding"}, "contract and stability set"},
	{[]string{"role vocabulary"}, "role templates and global roles"},
	{[]string{"roles across scopes"}, "role bindings at global, cluster and project level"},
	{[]string{"runaway loop"}, "the listing never ends"},
	{[]string{"sanctioned override"}, "name the command to run first"},
	{[]string{"scanned"}, "read"},
	{[]string{"schema vocabulary", "schema kind vocabulary"}, "the kinds rancher schema lists"},
	{[]string{"schema-legal"}, "allowed"},
	{[]string{"scope id"}, "the cluster or project it names"},
	{[]string{"scope instance"}, "cluster or project"},
	{[]string{"scope/sso"}, "missing scope or SSO authorization"},
	{[]string{"scriptblock", "scriptblocks"}, "script block"},
	{[]string{"selector-scoped"}, "the pods its selector matches"},
	{[]string{"server dry run"}, "server-side dry-run"},
	{[]string{"shown exactly once"}, "shown once"},
	{[]string{"single-node docker install"}, "Rancher running as a single Docker container"},
	{[]string{"spliced", "splices"}, "replaced"},
	{[]string{"stability marker", "stability markers"}, "stability level"},
	{[]string{"stamped", "stamps", "unstamped"}, "set (an annotation)"},
	{[]string{"steve collection envelope"}, "Rancher API collection"},
	{[]string{"stored revisions"}, "revisions"},
	{[]string{"stored values"}, "the release's values"},
	{[]string{"tag grammar"}, "a valid release tag"},
	{[]string{"tag redirect"}, "redirect to a release tag URL"},
	{[]string{"text card"}, "text output"},
	{[]string{"the pair"}, "the two binaries"},
	{[]string{"the plan"}, "the dry-run preview"},
	{[]string{"unexpected answer", "the answer is cluster"}, "unexpected response"},
	{[]string{"upload stage"}, "say what happens: when uploading is added"},
	{[]string{"user record", "user records"}, "User object"},
	{[]string{"user-password workflow", "declared marker"}, "local user passwords"},
	{[]string{"verified empty", "verified-empty"}, "the result has no rows"},
	{[]string{"wins over", "win over", "won over"}, "takes precedence over"},
	{[]string{"withheld"}, "left out"},
}

// wordingPhrase allows a longer phrase that contains the term of a row.
type wordingPhrase struct {
	term, phrase, reason string
}

// wordingPhraseAllowlist: one entry per line, sorted by term, then phrase.
var wordingPhraseAllowlist = []wordingPhrase{
	{"escape hatch", "(the CI escape hatch)", "names the CI way to skip credential storage, not the variable that skips the config locks"},
	{"junction", "(NTFS junction)", "names the Windows link type where the exact kind matters"},
	{"legacy spelling", "(legacy spelling of --cacert)", "the legacy token command's flag --cacerts, a flag and not an environment variable"},
}

// wordingPath allows count uses of a term in a file and, for a document,
// the section (the heading text) the uses sit under; a Go file has no
// section.
type wordingPath struct {
	term, path, section string
	count               int
	reason              string
}

// wordingPathAllowlist: one entry per line, sorted by term, then path, then
// section.
var wordingPathAllowlist = []wordingPath{
	{"as you", "REFERENCE.md", "Access management", 2, "text not yet reworded"},
	{"as you", "internal/engine/access/explain.go", "", 1, "text not yet reworded"},
	{"as you", "internal/engine/access/list.go", "", 1, "text not yet reworded"},
	{"as you", "internal/engine/access/whocan.go", "", 1, "text not yet reworded"},
	{"auth bootstrap", "skills/rancher/SKILL.md", "Conventions", 1, "text not yet reworded"},
	{"bespoke", "internal/cli/capability_coverage.go", "", 4, "text not yet reworded"},
	{"bin-dir", "install.ps1", "", 1, "the dry-run output key, kept because a test asserts it"},
	{"bin-dir", "install.sh", "", 1, "the dry-run output key, kept because a test asserts it"},
	{"budget", "COMMANDS.md", "Servers & configuration", 1, "text not yet reworded"},
	{"budget", "MIGRATING.md", "Changed in this CLI", 1, "text not yet reworded"},
	{"budget", "REFERENCE.md", "Revoking the CLI's tokens", 2, "text not yet reworded"},
	{"budget", "internal/commands/configcmd/config_info.go", "", 1, "text not yet reworded"},
	{"budget", "internal/commands/configcmd/set_completion.go", "", 2, "text not yet reworded"},
	{"budget", "internal/engine/access/fanout.go", "", 2, "text not yet reworded"},
	{"budget", "internal/token/retire.go", "", 1, "text not yet reworded"},
	{"budget", "skills/rancher/SKILL.md", "10. Role grants and secrets", 1, "text not yet reworded"},
	{"budget", "skills/rancher/SKILL.md", "Conventions", 1, "text not yet reworded"},
	{"canonical owner identity", "internal/engine/inspect/shared.go", "", 1, "a panic text no user sees"},
	{"configuration directory", "internal/config/lock.go", "", 1, "text not yet reworded"},
	{"degrades to", "skills/rancher/SKILL.md", "9. Observability & support", 1, "text not yet reworded"},
	{"direct context", "COMMANDS.md", "kubectl integration", 1, "text not yet reworded"},
	{"direct context", "REFERENCE.md", "What each part means", 1, "text not yet reworded"},
	{"direct context", "REFERENCE.md", "What the CLI sends to the server", 1, "text not yet reworded"},
	{"direct context", "REFERENCE.md", "Where it shows up on the server", 1, "text not yet reworded"},
	{"direct context", "internal/engine/kubeconfig/command.go", "", 4, "text not yet reworded"},
	{"direct context", "internal/token/directwait.go", "", 1, "text not yet reworded"},
	{"direct context", "internal/token/token.go", "", 3, "text not yet reworded"},
	{"direct context", "skills/rancher/SKILL.md", "8. kubectl interop", 2, "text not yet reworded"},
	{"dry-run effect", "skills/rancher/SKILL.md", "4. Mutation safety loop - never skip", 2, "text not yet reworded"},
	{"escalates to", "skills/rancher/SKILL.md", "11. OIDC clients", 1, "text not yet reworded"},
	{"exec credential plugin", "REFERENCE.md", "What each part means", 1, "text not yet reworded"},
	{"exec entry", "COMMANDS.md", "kubectl integration", 1, "text not yet reworded"},
	{"exec entry", "REFERENCE.md", "Environment variables", 2, "text not yet reworded"},
	{"exec entry", "REFERENCE.md", "The user a kubeconfig names (`--user-id`)", 3, "text not yet reworded"},
	{"exec entry", "REFERENCE.md", "kubectl integration", 3, "text not yet reworded"},
	{"exec entry", "internal/config/config.go", "", 1, "text not yet reworded"},
	{"exec entry", "internal/engine/kubeconfig/command.go", "", 1, "text not yet reworded"},
	{"exec entry", "internal/token/entry.go", "", 6, "text not yet reworded"},
	{"exec entry", "internal/token/noterminal.go", "", 3, "text not yet reworded"},
	{"exec entry", "internal/token/owner.go", "", 8, "text not yet reworded"},
	{"exec entry", "internal/token/regenerate.go", "", 1, "text not yet reworded"},
	{"exec entry", "internal/token/signin.go", "", 1, "text not yet reworded"},
	{"exec entry", "skills/rancher/SKILL.md", "8. kubectl interop", 4, "text not yet reworded"},
	{"executable", "install.sh", "", 1, "a file permission in an error message, not a program"},
	{"exit status", "internal/runtime/pipeline.go", "", 1, "text not yet reworded"},
	{"exit status", "skills/rancher/SKILL.md", "2. Discover, don't guess", 1, "text not yet reworded"},
	{"floor", "REFERENCE.md", "Supported server version", 2, "text not yet reworded"},
	{"floor", "skills/rancher/SKILL.md", "1. Verify auth before anything else", 1, "text not yet reworded"},
	{"hard-error", "CREDENTIAL_HELPERS.md", "Responses", 1, "text not yet reworded"},
	{"identity probe", "REFERENCE.md", "Supported server version", 2, "text not yet reworded"},
	{"junction", "install.ps1", "", 2, "not prose: the LinkType value and the New-Item parameter"},
	{"killed", "REFERENCE.md", "Revoking the CLI's tokens", 1, "text not yet reworded"},
	{"no-op", "CREDENTIAL_HELPERS.md", "Responses", 1, "text not yet reworded"},
	{"node count", "REFERENCE.md", "Kinds & verbs", 1, "text not yet reworded"},
	{"provider list", "MIGRATING.md", "Changed in this CLI", 3, "text not yet reworded"},
	{"provider list", "REFERENCE.md", "Auth configs (`--auth-provider`)", 2, "text not yet reworded"},
	{"provider list", "internal/token/token.go", "", 2, "text not yet reworded"},
	{"stamped", "REFERENCE.md", "Revoking the CLI's tokens", 1, "text not yet reworded"},
	{"the plan", "skills/rancher/SKILL.md", "4. Mutation safety loop - never skip", 1, "text not yet reworded"},
	{"unexpected answer", "internal/engine/access/discovery.go", "", 1, "text not yet reworded"},
	{"wins over", "REFERENCE.md", "Access management", 1, "text not yet reworded"},
	{"wins over", "skills/rancher/SKILL.md", "Conventions", 1, "text not yet reworded"},
}

// wordingMatcher finds the forms of one row.
type wordingMatcher struct {
	term    string
	instead string
	re      *regexp.Regexp
}

// newWordingMatchers compiles a term table. The words of a form are joined
// by \s+ and the longest form at a position wins.
func newWordingMatchers(rows []wordingRow) []wordingMatcher {
	matchers := make([]wordingMatcher, 0, len(rows))
	for _, row := range rows {
		forms := make([]string, 0, len(row.forms))
		for _, form := range row.forms {
			words := strings.Fields(form)
			for i, w := range words {
				words[i] = regexp.QuoteMeta(w)
			}
			forms = append(forms, strings.Join(words, `\s+`))
		}
		re := regexp.MustCompile(`(?i)(?:` + strings.Join(forms, "|") + `)`)
		re.Longest()
		matchers = append(matchers, wordingMatcher{term: row.forms[0], instead: row.instead, re: re})
	}
	return matchers
}

// wordingEdge reports whether r can continue a word: a letter, a digit, a
// hyphen or an underscore.
func wordingEdge(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_'
}

// find returns the [start, end) offsets of every whole-word match in text.
func (m wordingMatcher) find(text string) [][2]int {
	var spans [][2]int
	for _, loc := range m.re.FindAllStringIndex(text, -1) {
		if before, size := utf8.DecodeLastRuneInString(text[:loc[0]]); size > 0 && wordingEdge(before) {
			continue
		}
		if after, size := utf8.DecodeRuneInString(text[loc[1]:]); size > 0 && wordingEdge(after) {
			continue
		}
		spans = append(spans, [2]int{loc[0], loc[1]})
	}
	return spans
}

// wordingUnit is one piece of text to match: a folded Go string or a
// paragraph of a document. text has code spans blanked; lineOf maps an
// offset in text to a line number of the file.
type wordingUnit struct {
	path, section string
	text          string
	lineOf        func(offset int) int
}

// productionWordingUnits lists the strings of the parsed files, leaving
// out the files under a skipped prefix.
func productionWordingUnits(fset *token.FileSet, files []*ast.File, paths []string) []wordingUnit {
	var units []wordingUnit
	for i, f := range files {
		if wordingSkipsPath(paths[i]) {
			continue
		}
		units = append(units, goWordingUnits(fset, paths[i], f)...)
	}
	return units
}

// goWordingUnits lists the strings of f that hold whitespace.
func goWordingUnits(fset *token.FileSet, path string, f *ast.File) []wordingUnit {
	var units []wordingUnit
	for _, c := range foldedChains(f) {
		if strings.IndexFunc(c.text, unicode.IsSpace) < 0 {
			continue
		}
		units = append(units, wordingUnit{
			path: path,
			text: c.text,
			lineOf: func(offset int) int {
				part := c.partAt(offset)
				line := fset.Position(part.lit.Pos()).Line
				if strings.HasPrefix(part.lit.Value, "`") {
					// A raw string's newlines are source lines; an
					// interpreted string's "\n" escapes are not.
					line += strings.Count(c.text[part.offset:offset], "\n")
				}
				return line
			},
		})
	}
	return units
}

// fenceRun reports the fence character and the length of the run of
// backticks or tildes a line starts with (after its indent), or 0.
func fenceRun(line string) (byte, int) {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if line[0] == '`' && strings.Contains(line[n:], "`") {
		// A backtick fence's info text holds no backtick: this line
		// starts with an inline code span.
		return 0, 0
	}
	return line[0], n
}

// blankCodeSpans replaces every inline code span (a run of backticks up to
// the next run of the same length) with NUL bytes of the same length, so no
// term matches across it and offsets stay put. A run with no closing run is
// left as written.
func blankCodeSpans(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); {
		if b[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(b) && b[j] == '`' {
			j++
		}
		run := j - i
		closed := false
		for k := j; k < len(b); {
			if b[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(b) && b[m] == '`' {
				m++
			}
			if m-k == run {
				for x := i; x < m; x++ {
					b[x] = 0
				}
				i = m
				closed = true
				break
			}
			k = m
		}
		if !closed {
			i = j
		}
	}
	return string(b)
}

// docParagraph joins the lines of a paragraph with single spaces and
// blanks its code spans. A hit reports the line its first word sits on.
func docParagraph(path, section string, first int, lines []string) wordingUnit {
	starts := make([]int, len(lines))
	n := 0
	for i, line := range lines {
		starts[i] = n
		n += len(line) + 1
	}
	return wordingUnit{
		path:    path,
		section: section,
		text:    blankCodeSpans(strings.Join(lines, " ")),
		lineOf: func(offset int) int {
			i, _ := slices.BinarySearchFunc(starts, offset, func(start, off int) int {
				return cmp.Compare(start, off+1)
			})
			return first + max(i-1, 0)
		},
	}
}

// readWordingDoc splits a Markdown document into paragraphs and headings
// outside code fences. It fails when a fence never closes.
func readWordingDoc(path, src string) ([]wordingUnit, error) {
	var units []wordingUnit
	var para []string
	paraLine := 0
	section := ""
	var fenceChar byte
	fenceLen, fenceLine := 0, 0
	flush := func() {
		if len(para) > 0 {
			units = append(units, docParagraph(path, section, paraLine, para))
			para = nil
		}
	}
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimSuffix(line, "\r")
		n := i + 1
		trimmed := strings.TrimLeft(line, " \t")
		char, run := fenceRun(trimmed)
		if fenceLen > 0 {
			if char == fenceChar && run >= fenceLen && strings.TrimSpace(trimmed[run:]) == "" {
				fenceLen = 0
			}
			continue
		}
		switch {
		case run >= 3:
			flush()
			fenceChar, fenceLen, fenceLine = char, run, n
		case strings.HasPrefix(line, "#"):
			flush()
			section = strings.TrimSpace(strings.TrimLeft(line, "#"))
			units = append(units, docParagraph(path, section, n, []string{section}))
		case strings.TrimSpace(line) == "":
			flush()
		default:
			if len(para) == 0 {
				paraLine = n
			}
			para = append(para, strings.TrimSpace(line))
		}
	}
	flush()
	if fenceLen > 0 {
		return nil, &wordingFenceError{path: path, line: fenceLine}
	}
	return units, nil
}

// readWordingScript splits an install script, an issue form or a shell script
// into one unit per line, skipping a comment line (its first non-blank
// characters are # or //). A hit reports the line it sits on.
func readWordingScript(path, src string) []wordingUnit {
	var units []wordingUnit
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		n := i + 1
		units = append(units, wordingUnit{
			path:   path,
			text:   line,
			lineOf: func(int) int { return n },
		})
	}
	return units
}

type wordingFenceError struct {
	path string
	line int
}

func (e *wordingFenceError) Error() string {
	return e.path + ": unclosed fence at line " + strconv.Itoa(e.line) + "; the text below it is not read"
}

// wordingKey names a group of uses: one term in one file and section.
type wordingKey struct {
	term, path, section string
}

type wordingHit struct {
	line    int
	text    string
	instead string
}

// wordingSection is the failure-message suffix for a section.
func wordingSection(section string) string {
	if section == "" {
		return ""
	}
	return ` (section "` + section + `")`
}

// checkWording matches every unit against the table and returns the
// failures: a use with no path entry, a path entry whose count differs from
// the uses found, and an entry that matches nothing. Every use is reported,
// not only the first of a string.
func checkWording(rows []wordingRow, phrases []wordingPhrase, paths []wordingPath, units []wordingUnit) []string {
	matchers := newWordingMatchers(rows)
	usedPhrase := make([]bool, len(phrases))
	usedPath := make([]bool, len(paths))
	pathAt := map[wordingKey]int{}
	for i, p := range paths {
		pathAt[wordingKey{p.term, p.path, p.section}] = i
	}

	hits := map[wordingKey][]wordingHit{}
	var order []wordingKey
	for _, u := range units {
		for _, m := range matchers {
			for _, span := range m.find(u.text) {
				if wordingInPhrase(m.term, u.text, span, phrases, usedPhrase) {
					continue
				}
				key := wordingKey{m.term, u.path, u.section}
				if _, seen := hits[key]; !seen {
					order = append(order, key)
				}
				hits[key] = append(hits[key], wordingHit{
					line:    u.lineOf(span[0]),
					text:    u.text[span[0]:span[1]],
					instead: m.instead,
				})
			}
		}
	}

	var failures []string
	for _, key := range order {
		found := hits[key]
		i, ok := pathAt[key]
		if !ok {
			for _, h := range found {
				failures = append(failures, key.path+":"+strconv.Itoa(h.line)+`: "`+h.text+`" - use "`+h.instead+`"`+wordingSection(key.section))
			}
			continue
		}
		usedPath[i] = true
		if paths[i].count != len(found) {
			failures = append(failures, "allowlist entry "+key.term+" "+key.path+" covers "+strconv.Itoa(len(found))+" hits, entry says "+strconv.Itoa(paths[i].count)+wordingSection(key.section))
		}
	}
	for i, p := range paths {
		if !usedPath[i] {
			failures = append(failures, "allowlist entry "+p.term+" "+p.path+" matches nothing; remove this entry"+wordingSection(p.section))
		}
	}
	for i, p := range phrases {
		if !usedPhrase[i] {
			failures = append(failures, "allowlist entry "+p.term+" "+p.phrase+" matches nothing; remove this entry")
		}
	}
	return failures
}

// wordingInPhrase reports whether the match at span lies inside an
// occurrence of an allowed phrase of the same term, and marks that phrase
// used.
func wordingInPhrase(term, text string, span [2]int, phrases []wordingPhrase, used []bool) bool {
	covered := false
	for i, p := range phrases {
		if p.term != term {
			continue
		}
		for from := 0; ; {
			at := strings.Index(text[from:], p.phrase)
			if at < 0 {
				break
			}
			start := from + at
			if start <= span[0] && span[1] <= start+len(p.phrase) {
				used[i] = true
				covered = true
			}
			from = start + 1
		}
	}
	return covered
}

// validateWordingTables checks the layout of the term table and both
// allowlists.
func validateWordingTables(rows []wordingRow, phrases []wordingPhrase, paths []wordingPath) []string {
	var problems []string
	names := map[string]bool{}
	forms := map[string]string{}
	for i, row := range rows {
		if len(row.forms) == 0 || row.instead == "" {
			problems = append(problems, "term table row "+strconv.Itoa(i)+" needs forms and a wording to use instead")
			continue
		}
		names[row.forms[0]] = true
		for _, form := range row.forms {
			if form != strings.ToLower(form) || form != strings.Join(strings.Fields(form), " ") {
				problems = append(problems, "term table form "+form+" must be lower case with single spaces")
			}
			if other, dup := forms[form]; dup {
				problems = append(problems, "term table form "+form+" is in rows "+other+" and "+row.forms[0])
			}
			forms[form] = row.forms[0]
		}
		if i > 0 && len(rows[i-1].forms) > 0 && rows[i-1].forms[0] >= row.forms[0] {
			problems = append(problems, "term table row "+row.forms[0]+" is out of order or repeated")
		}
	}
	for i, p := range phrases {
		if !names[p.term] || p.phrase == "" || p.reason == "" {
			problems = append(problems, "phrase entry "+p.term+" "+p.phrase+" needs a term from the table, a phrase and a reason")
		}
		if i > 0 && cmp.Or(cmp.Compare(phrases[i-1].term, p.term), cmp.Compare(phrases[i-1].phrase, p.phrase)) >= 0 {
			problems = append(problems, "phrase entry "+p.term+" "+p.phrase+" is out of order or repeated")
		}
	}
	for i, p := range paths {
		if !names[p.term] || p.path == "" || p.count < 1 || p.reason == "" {
			problems = append(problems, "path entry "+p.term+" "+p.path+" needs a term from the table, a path, a count of at least 1 and a reason")
		}
		if i > 0 {
			q := paths[i-1]
			if cmp.Or(cmp.Compare(q.term, p.term), cmp.Compare(q.path, p.path), cmp.Compare(q.section, p.section)) >= 0 {
				problems = append(problems, "path entry "+p.term+" "+p.path+" "+p.section+" is out of order or repeated")
			}
		}
	}
	return problems
}

// wordingProbe feeds one snippet to the reader and the matcher with its own
// tables and states the rule it asserts.
type wordingProbe struct {
	rule    string
	doc     string
	rows    []wordingRow
	phrases []wordingPhrase
	paths   []wordingPath
	want    []string
}

var probeRows = []wordingRow{
	{[]string{"bin-dir", "bin dir", "bin dirs"}, "bin directory"},
	{[]string{"wins over", "win over"}, "takes precedence over"},
}

var budgetRows = []wordingRow{
	{[]string{"budget", "budgets"}, "time limit"},
}

func TestRejectedWordingProbes(t *testing.T) {
	notYet := "text not yet reworded"
	probes := []wordingProbe{
		{
			rule: "a use in a paragraph fails with its line and the wording to use",
			doc:  "Title\n\nSome text.\nThis wins over that.\n",
			want: []string{`doc.md:4: "wins over" - use "takes precedence over"`},
		},
		{
			rule: "a phrase wrapped across two lines is found, at the line of its first word",
			doc:  "first line wins\nover the rest\n",
			want: []string{`doc.md:1: "wins over" - use "takes precedence over"`},
		},
		{
			rule: "matching ignores case and prints the text as written",
			doc:  "It WINS   Over it.\n",
			want: []string{`doc.md:1: "WINS   Over" - use "takes precedence over"`},
		},
		{
			rule: "every use is reported, not only the first in a paragraph",
			doc:  "a wins over b and c win over d\n",
			want: []string{
				`doc.md:1: "wins over" - use "takes precedence over"`,
				`doc.md:1: "win over" - use "takes precedence over"`,
			},
		},
		{
			rule: "a code span passes, and a code span that wraps a line passes",
			doc:  "see `wins over` and `a\nwins over b` here\n",
		},
		{
			rule: "a term is not matched across a code span",
			doc:  "it wins `x` over it\n",
		},
		{
			rule: "a fenced block passes at any indent, even with a blank line inside",
			doc:  "- item\n\n    ```sh\n    a\n\n    wins over\n    ```\n\nend\n",
		},
		{
			rule: "a fence may be indented with a tab, with a blank line inside",
			doc:  "\t```\n\ta\n\n\twins over\n\t```\nend\n",
		},
		{
			rule: "a closing fence with text after it does not close the block",
			doc:  "```\nwins over\n``` x\nwins over\n```\nclean\n",
		},
		{
			rule: "a line that starts with an inline code span is not a fence",
			doc:  "```a``` wins over\n",
			want: []string{`doc.md:1: "wins over" - use "takes precedence over"`},
		},
		{
			rule: "a tilde fence may hold backticks after its opening",
			doc:  "~~~ `x`\nwins over\n~~~\nclean\n",
		},
		{
			rule: "a heading starts at column 0; a # line indented four spaces is text of the section",
			doc:  "# Where\n\n    # wins over\n",
			want: []string{`doc.md:3: "wins over" - use "takes precedence over" (section "Where")`},
		},
		{
			rule: "Windows line ends read the same hits and lines as Unix line ends",
			doc:  "a wins\r\nover b\r\n\r\n```\r\nwins over\r\n\r\n```\r\n# H\r\nx wins over\r\n",
			want: []string{
				`doc.md:1: "wins over" - use "takes precedence over"`,
				`doc.md:9: "wins over" - use "takes precedence over" (section "H")`,
			},
		},
		{
			rule: "a closing fence shorter than its opening leaves the block open",
			doc:  "````\n```\nwins over\n````\nclean\n",
		},
		{
			rule: "a tilde fence closes with tildes, not backticks",
			doc:  "~~~\nwins over\n```\nwins over\n~~~\n",
		},
		{
			rule: "text after a closed fence is read again",
			doc:  "```\nwins over\n```\nwins over\n",
			want: []string{`doc.md:4: "wins over" - use "takes precedence over"`},
		},
		{
			rule: "a fence that never closes fails",
			doc:  "text\n```\nwins over\n",
			want: []string{"doc.md: unclosed fence at line 2; the text below it is not read"},
		},
		{
			rule: "a heading is a paragraph of its own and names the section",
			doc:  "## It wins over\nbody\n",
			want: []string{`doc.md:1: "wins over" - use "takes precedence over" (section "It wins over")`},
		},
		{
			rule: "a hit under a heading reports the section",
			doc:  "## Where\n\nit wins over\n",
			want: []string{`doc.md:3: "wins over" - use "takes precedence over" (section "Where")`},
		},
		{
			rule: "a hyphen or an underscore beside the term is not a boundary",
			doc:  "bin-directory bin_dir xbin dir bin dirx a-bin dir\n",
		},
		{
			rule: "an underscore beside the term is not a boundary",
			doc:  "x_budget budget_x\n",
			rows: budgetRows,
		},
		{
			rule: "a digit beside the term is not a boundary",
			doc:  "budget2 2budget\n",
			rows: budgetRows,
		},
		{
			rule: "a hyphen after the term is not a boundary",
			doc:  "budget-x\n",
			rows: budgetRows,
		},
		{
			rule: "a plain use of the term is a hit",
			doc:  "the budget, then.\n",
			rows: budgetRows,
			want: []string{`doc.md:1: "budget" - use "time limit"`},
		},
		{
			rule: "a hyphenated form matches, and the longest form wins",
			doc:  "the bin-dir and the bin dirs\n",
			want: []string{
				`doc.md:1: "bin-dir" - use "bin directory"`,
				`doc.md:1: "bin dirs" - use "bin directory"`,
			},
		},
		{
			rule:    "a use inside an allowed phrase passes",
			doc:     "an (NTFS wins over) rule\n",
			phrases: []wordingPhrase{{"wins over", "(NTFS wins over)", "a title"}},
		},
		{
			rule:    "the same phrase in another case is a use",
			doc:     "an (ntfs wins over) rule\n",
			phrases: []wordingPhrase{{"wins over", "(NTFS wins over)", "a title"}},
			want: []string{
				`doc.md:1: "wins over" - use "takes precedence over"`,
				"allowlist entry wins over (NTFS wins over) matches nothing; remove this entry",
			},
		},
		{
			rule:    "a phrase entry of another term does not cover the use",
			doc:     "an (NTFS wins over) rule\n",
			phrases: []wordingPhrase{{"bin-dir", "(NTFS wins over)", "a title"}},
			want: []string{
				`doc.md:1: "wins over" - use "takes precedence over"`,
				"allowlist entry bin-dir (NTFS wins over) matches nothing; remove this entry",
			},
		},
		{
			rule:  "a path entry covers a file and section with its count",
			doc:   "## Where\n\nit wins over it, and wins over.\n",
			paths: []wordingPath{{"wins over", "doc.md", "Where", 2, notYet}},
		},
		{
			rule:  "a path entry for one section does not cover another section of the file",
			doc:   "## Where\n\nit wins over it\n\n## Other\n\nit wins over it\n",
			paths: []wordingPath{{"wins over", "doc.md", "Where", 1, notYet}},
			want:  []string{`doc.md:7: "wins over" - use "takes precedence over" (section "Other")`},
		},
		{
			rule:  "a path entry for a form covers the row it names",
			doc:   "it win over it\n",
			paths: []wordingPath{{"wins over", "doc.md", "", 1, notYet}},
		},
		{
			rule:  "a count above the uses found fails",
			doc:   "it wins over it\n",
			paths: []wordingPath{{"wins over", "doc.md", "", 2, notYet}},
			want:  []string{"allowlist entry wins over doc.md covers 1 hits, entry says 2"},
		},
		{
			rule:  "a count below the uses found fails",
			doc:   "it wins over it and win over it\n",
			paths: []wordingPath{{"wins over", "doc.md", "", 1, notYet}},
			want:  []string{"allowlist entry wins over doc.md covers 2 hits, entry says 1"},
		},
		{
			rule:  "a count mismatch names the section",
			doc:   "# Where\nit wins over it\n",
			paths: []wordingPath{{"wins over", "doc.md", "Where", 2, notYet}},
			want:  []string{`allowlist entry wins over doc.md covers 1 hits, entry says 2 (section "Where")`},
		},
		{
			rule:  "an entry that matches nothing fails",
			doc:   "nothing here\n",
			paths: []wordingPath{{"wins over", "doc.md", "Where", 1, notYet}},
			want:  []string{`allowlist entry wins over doc.md matches nothing; remove this entry (section "Where")`},
		},
		{
			rule:    "a use inside an allowed phrase counts for no path entry",
			doc:     "an (NTFS wins over) rule and wins over\n",
			phrases: []wordingPhrase{{"wins over", "(NTFS wins over)", "a title"}},
			paths:   []wordingPath{{"wins over", "doc.md", "", 1, notYet}},
		},
	}
	for _, p := range probes {
		t.Run(p.rule, func(t *testing.T) {
			rows := p.rows
			if rows == nil {
				rows = probeRows
			}
			units, err := readWordingDoc("doc.md", p.doc)
			var got []string
			if err != nil {
				got = []string{err.Error()}
			} else {
				got = checkWording(rows, p.phrases, p.paths, units)
			}
			assert.Equal(t, p.want, got)
		})
	}
}

// TestRejectedWordingGoStrings asserts which Go text the scan reads: a
// string with whitespace, a + chain folded into one string, the line of
// the literal that holds a match, and never a comment.
func TestRejectedWordingGoStrings(t *testing.T) {
	src := `package p
const a = "budget"
const b = "the budget"
const c = "x " + v + "budget y"
// a comment about a budget
const d = "--budget"
var e = "x" +
	"a wins over b"
var f = f("one wins over two", "three win over four")
const g = "a wins over b and win over c"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	require.NoError(t, err)
	rows := []wordingRow{
		{[]string{"budget", "budgets"}, "time limit"},
		{[]string{"wins over", "win over"}, "takes precedence over"},
	}
	got := checkWording(rows, nil, nil, goWordingUnits(fset, "p.go", f))
	assert.Equal(t, []string{
		`p.go:3: "budget" - use "time limit"`,
		`p.go:4: "budget" - use "time limit"`,
		`p.go:8: "wins over" - use "takes precedence over"`,
		`p.go:9: "wins over" - use "takes precedence over"`,
		`p.go:9: "win over" - use "takes precedence over"`,
		`p.go:10: "wins over" - use "takes precedence over"`,
		`p.go:10: "win over" - use "takes precedence over"`,
	}, got, "a string with no whitespace, and a comment, are not text")

	got = checkWording(rows, nil, []wordingPath{{"budget", "p.go", "", 2, "text not yet reworded"}}, goWordingUnits(fset, "p.go", f))
	assert.Len(t, got, 5, "a Go entry has no section and covers the two uses in the file: %v", got)
	for _, failure := range got {
		assert.NotContains(t, failure, `"budget"`)
	}
}

// TestRejectedWordingTableChecks asserts that the layout check refuses an
// unsorted or repeated entry and an entry with no table row.
func TestRejectedWordingTableChecks(t *testing.T) {
	rows := []wordingRow{
		{[]string{"a b"}, "x"},
		{[]string{"c"}, "y"},
	}
	assert.Empty(t, validateWordingTables(rows, nil, nil))

	unsorted := []wordingRow{rows[1], rows[0]}
	require.Len(t, validateWordingTables(unsorted, nil, nil), 1)

	repeated := []wordingRow{rows[0], {[]string{"d", "a b"}, "z"}}
	assert.NotEmpty(t, validateWordingTables(repeated, nil, nil), "a form in two rows")

	paths := []wordingPath{
		{"c", "b.md", "", 1, "r"},
		{"c", "a.md", "", 1, "r"},
	}
	assert.NotEmpty(t, validateWordingTables(rows, nil, paths), "paths out of order")
	assert.NotEmpty(t, validateWordingTables(rows, nil, []wordingPath{{"zz", "a.md", "", 1, "r"}}), "a term with no row")
	assert.NotEmpty(t, validateWordingTables(rows, nil, []wordingPath{{"c", "a.md", "", 0, "r"}}), "a count of zero")
	assert.NotEmpty(t, validateWordingTables(rows, []wordingPhrase{{"c", "x c y", ""}}, nil), "a phrase with no reason")
}

// TestRejectedWordingTableForms asserts the forms and wording of rows of
// the real table: a hyphenated form followed by a plural letter is a hit,
// and the row for "degrades to" tells what to write.
func TestRejectedWordingTableForms(t *testing.T) {
	units, err := readWordingDoc("doc.md", "the version-floors and the bin-dirs\n\nit degrades to nothing\n")
	require.NoError(t, err)
	assert.Equal(t, []string{
		`doc.md:1: "bin-dirs" - use "bin directory"`,
		`doc.md:1: "version-floors" - use "minimum"`,
		`doc.md:3: "degrades to" - use "say what the command shows instead"`,
	}, checkWording(rejectedWording, nil, nil, units))
}

// TestRejectedWordingSkippedPackages asserts the same literal passes in a
// package that only supports tests and fails in a production package.
func TestRejectedWordingSkippedPackages(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", "package p\nconst a = \"the budget\"\n", 0)
	require.NoError(t, err)
	rows := []wordingRow{{[]string{"budget"}, "time limit"}}
	for _, prefix := range wordingSkippedPrefixes {
		path := prefix + "x.go"
		assert.Empty(t, checkWording(rows, nil, nil, productionWordingUnits(fset, []*ast.File{f}, []string{path})), path)
	}
	assert.Equal(t, []string{`internal/cli/x.go:2: "budget" - use "time limit"`},
		checkWording(rows, nil, nil, productionWordingUnits(fset, []*ast.File{f}, []string{"internal/cli/x.go"})))
}

// TestRejectedWordingGoLines asserts the line of a hit in a string that
// spans lines: a raw string reports the line of the hit, an interpreted
// string the line of its literal (its "\\n" escapes are not source lines),
// and a chain the line of the literal that holds the hit, also when the
// hit is on a later line of a later raw literal in the chain.
func TestRejectedWordingGoLines(t *testing.T) {
	src := "package p\n" +
		"var a = `first\n" + // line 2
		"second\n" + // line 3
		"third budget`\n" + // line 4
		"var b = \"x\\ny budget\"\n" + // line 5
		"var c = `one\n" + // line 6
		"two` +\n" + // line 7
		"\" budget\"\n" + // line 8
		"var d = `alpha\n" + // line 9
		"beta` + `gamma\n" + // line 10
		"delta budget`\n" // line 11
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	require.NoError(t, err)
	rows := []wordingRow{{[]string{"budget"}, "time limit"}}
	assert.Equal(t, []string{
		`p.go:4: "budget" - use "time limit"`,
		`p.go:5: "budget" - use "time limit"`,
		`p.go:8: "budget" - use "time limit"`,
		`p.go:11: "budget" - use "time limit"`,
	}, checkWording(rows, nil, nil, goWordingUnits(fset, "p.go", f)))
}

// TestRejectedWordingScriptLines asserts which lines of an install script or
// an issue form the scan reads: a comment line (# or //, after any indent) is
// skipped, a line that holds a # after its text is read, and a hit reports the
// file and the line.
func TestRejectedWordingScriptLines(t *testing.T) {
	src := "#!/bin/sh\n" + // line 1
		"# a bin dir in a comment\n" + // line 2
		"    // wins over in a comment\n" + // line 3
		"\t# wins over, indented\n" + // line 4
		"err \"the bin dir is full\"\n" + // line 5
		"note \"  rancher skill install   # a bin dir\"\n" + // line 6
		"\n" + // line 7
		"description: this wins over that\r\n" // line 8
	units := readWordingScript("script.sh", src)
	assert.Equal(t, []string{
		`script.sh:5: "bin dir" - use "bin directory"`,
		`script.sh:6: "bin dir" - use "bin directory"`,
		`script.sh:8: "wins over" - use "takes precedence over"`,
	}, checkWording(probeRows, nil, nil, units))

	assert.Empty(t, checkWording(probeRows, nil, nil, readWordingScript("script.sh", "# only a bin dir comment\n// wins over\n")),
		"a rejected phrase in a comment line is skipped")
}
