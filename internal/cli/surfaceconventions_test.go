package cli

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// surfaceConcept is one row of the flag name table: a flag name and the one
// name of its concept on every command.
type surfaceConcept struct {
	name string
	use  string
}

// surfaceConcepts has every flag name of the schema manifest, one row per
// line, sorted by name. A row whose use differs from its name marks a flag
// that does not follow rule 1 on every command that has it. A change to a
// flag name edits this table in the same commit:
//   - a new flag name gets a row whose use is its own name, unless an
//     existing name has the same meaning: then the flag takes that name;
//   - a flag name that is to be renamed gets the new name as its use, and
//     each command that has it gets a row in surfaceConceptAllowlist, with
//     the count raised by one for each;
//   - when no command has a name any more, its row goes, and so do its
//     allowlist rows, with the count lowered; the new name gets a row of its
//     own.
var surfaceConcepts = []surfaceConcept{
	{"address", "address"},
	{"agent", "agent"},
	{"all", "all"},
	{"all-clusters", "all-clusters"},
	{"all-namespaces", "all-namespaces"},
	{"all-versions", "all-versions"},
	{"allow-deprecated", "allow-deprecated"},
	{"apply", "apply"},
	{"as", "as"},
	{"auth-flow", "auth-flow"},
	{"auth-provider", "auth-provider"},
	{"by", "by"},
	{"cacert", "cacert"},
	{"cacerts", "cacert"},
	{"chart", "chart"},
	{"cleanup-on-fail", "cleanup-on-fail"},
	{"clear-cacert", "clear-cacert"},
	{"cloud-credential", "cloud-credential"},
	{"cluster", "cluster"},
	{"clusters", "cluster"},
	{"color", "color"},
	{"config", "config"},
	{"container", "container"},
	{"container-default-resource-limit", "container-default-resource-limit"},
	{"contract", "contract"},
	{"control-plane-concurrency", "control-plane-concurrency"},
	{"control-plane-delete-emptydir", "control-plane-delete-emptydir-data"},
	{"control-plane-drain", "control-plane-drain"},
	{"control-plane-drain-force", "control-plane-drain-force"},
	{"control-plane-drain-timeout", "control-plane-drain-timeout"},
	{"control-plane-grace-period", "control-plane-grace-period"},
	{"controlplane", "control-plane"},
	{"credential-store", "credential-store"},
	{"current-context", "current-context"},
	{"current-password-stdin", "current-password-stdin"},
	{"custom", "custom"},
	{"debug", "debug"},
	{"delete-emptydir-data", "delete-emptydir-data"},
	{"delete-namespaces", "delete-namespaces"},
	{"description", "description"},
	{"destructive", "destructive"},
	{"disable-openapi-validation", "disable-openapi-validation"},
	{"display-name", "display-name"},
	{"drain-timeout", "drain-timeout"},
	{"dry-run", "dry-run"},
	{"enabled", "enabled"},
	{"etcd", "etcd"},
	{"exec-command", "exec-command"},
	{"filename", "filename"},
	{"for", "for"},
	{"force", "force"},
	{"from-file", "from-file"},
	{"from-literal", "from-literal"},
	{"global", "global"},
	{"global-role", "global-role"},
	{"grace-period", "grace-period"},
	{"group", "group"},
	{"helm-timeout", "helm-timeout"},
	{"helm-wait", "helm-wait"},
	{"history-max", "history-max"},
	{"image", "image"},
	{"import", "import"},
	{"include-default-entry", "include-default-entry"},
	{"include-deprecated", "include-deprecated"},
	{"include-system", "include-system"},
	{"internal-address", "internal-address"},
	{"ipv6", "ipv6"},
	{"k8s-version", "k8s-version"},
	{"keep-history", "keep-history"},
	{"kind", "kind"},
	{"label", "label"},
	{"machine-config", "machine-config"},
	{"match", "match"},
	{"max", "max-quantity"},
	{"min", "min-quantity"},
	{"monitoring", "monitoring"},
	{"must-change-password", "must-change-password"},
	{"mutating", "mutating"},
	{"name", "name"},
	{"namespace", "namespace"},
	{"namespace-default-quota", "namespace-default-quota"},
	{"network", "network"},
	{"no-hooks", "no-hooks"},
	{"no-input", "no-input"},
	{"no-system-default-registry", "no-system-default-registry"},
	{"no-verify", "no-verify"},
	{"node", "node"},
	{"node-name", "node-name"},
	{"nodes", "quantity"},
	{"non-resource-url", "non-resource-url"},
	{"output", "output"},
	{"output-file", "output-file"},
	{"password-stdin", "password-stdin"},
	{"patch", "patch"},
	{"patch-file", "patch-file"},
	{"pod", "pod"},
	{"pool", "pool"},
	{"pool-name", "pool"},
	{"previous", "previous"},
	{"private-networking", "private-networking"},
	{"project", "project"},
	{"quantity", "quantity"},
	{"quota-limit", "quota-limit"},
	{"read-only", "read-only"},
	{"redirect-uri", "redirect-uri"},
	{"refresh-ttl", "refresh-ttl"},
	{"region", "region"},
	{"registration", "registration"},
	{"repo", "repo"},
	{"require-installed", "require-installed"},
	{"reset-values", "reset-values"},
	{"role", "role"},
	{"scope", "scope"},
	{"search", "search"},
	{"selector", "selector"},
	{"server", "url"},
	{"set", "set"},
	{"show", "show"},
	{"show-denied", "show-denied"},
	{"since", "since"},
	{"size", "size"},
	{"skip-verify", "skip-verify"},
	{"subresource", "subresource"},
	{"system-default-registry", "system-default-registry"},
	{"tags", "tags"},
	{"tail", "tail"},
	{"taint", "taint"},
	{"take-ownership", "take-ownership"},
	{"targets", "targets"},
	{"timeout", "timeout"},
	{"timestamps", "timestamps"},
	{"token-stdin", "token-stdin"},
	{"token-ttl", "token-ttl"},
	{"ttl", "ttl"},
	{"type", "type"},
	{"url", "url"},
	{"user", "user"},
	{"user-id", "user-id"},
	{"values", "values"},
	{"verb", "verb"},
	{"version", "version"},
	{"wait", "wait"},
	{"windows", "windows"},
	{"worker", "worker"},
	{"worker-concurrency", "worker-concurrency"},
	{"worker-delete-emptydir", "worker-delete-emptydir-data"},
	{"worker-drain", "worker-drain"},
	{"worker-drain-force", "worker-drain-force"},
	{"worker-drain-timeout", "worker-drain-timeout"},
	{"worker-grace-period", "worker-grace-period"},
	{"yes", "yes"},
}

// surfaceConceptsWhere names the concept table in failure messages.
const surfaceConceptsWhere = "the concept table (surfaceConcepts in internal/cli/surfaceconventions_test.go)"

// surfaceConceptHits returns the sites whose flag name differs from the name
// of its concept, and the problems of the table itself: a flag name with no
// row, a row no flag has, two rows for one name, and a row out of order.
// Every flag, own or global, is read.
func surfaceConceptHits(m runtime.Manifest, table []surfaceConcept) ([]surfaceHit, []string) {
	var hits []surfaceHit
	var problems []string
	use := map[string]string{}
	for i, row := range table {
		if _, dup := use[row.name]; dup {
			problems = append(problems, fmt.Sprintf("%s has two rows for --%s; delete one", surfaceConceptsWhere, row.name))
		}
		if i > 0 && table[i-1].name > row.name {
			problems = append(problems, fmt.Sprintf("the row for --%s in %s is out of order; keep the rows sorted by name", row.name, surfaceConceptsWhere))
		}
		use[row.name] = row.use
	}
	for _, row := range table {
		if next, ok := use[row.use]; row.use != row.name && ok && next != row.use {
			problems = append(problems, fmt.Sprintf("the row for --%s in %s gives the name --%s, whose own row gives --%s; give --%s the name --%s", row.name, surfaceConceptsWhere, row.use, next, row.name, next))
		}
	}
	seen := map[string]bool{}
	firstSite := map[string]string{}
	for _, s := range surfaceSites(m) {
		if !s.isFlag() {
			continue
		}
		seen[s.name] = true
		want, ok := use[s.name]
		if !ok {
			if _, noted := firstSite[s.name]; !noted {
				firstSite[s.name] = s.key()
			}
			continue
		}
		if want != s.name {
			hits = append(hits, surfaceHit{s.key(), fmt.Sprintf("the concept table gives --%s the name --%s; rename the flag to --%s", s.name, want, want)})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(firstSite)) {
		problems = append(problems, fmt.Sprintf("--%s is not in %s; it is first used by %s. If another flag name has the same meaning, rename this flag to it; otherwise add the row {%q, %q}", name, surfaceConceptsWhere, firstSite[name], name, name))
	}
	for _, row := range table {
		if !seen[row.name] {
			problems = append(problems, fmt.Sprintf("%s has a row for --%s, but no command has that flag; remove the row", surfaceConceptsWhere, row.name))
		}
	}
	return hits, problems
}

var surfaceListWords = regexp.MustCompile(`(?i)key=value|comma-separated|separated by`)

// surfaceListHits returns every string flag whose description says it takes a
// list of identifiers or of key=value pairs.
func surfaceListHits(m runtime.Manifest) []surfaceHit {
	var hits []surfaceHit
	for _, s := range surfaceSites(m) {
		if s.isFlag() && s.typ == "string" && surfaceListWords.MatchString(s.desc) {
			hits = append(hits, surfaceHit{s.key(), fmt.Sprintf("a string flag whose description contains %q; make it a repeatable flag", surfaceListWords.FindString(s.desc))})
		}
	}
	return hits
}

var surfaceSwitchWords = regexp.MustCompile(`true\|false|false\|true`)

// surfaceSwitchHits returns every string flag that takes an on/off value: its
// enum is exactly true and false, or its description contains true|false or
// false|true.
func surfaceSwitchHits(m runtime.Manifest) []surfaceHit {
	var hits []surfaceHit
	for _, s := range surfaceSites(m) {
		if !s.isFlag() || s.typ != "string" {
			continue
		}
		trueFalse := len(s.enum) == 2 && slices.Contains(s.enum, "true") && slices.Contains(s.enum, "false")
		if trueFalse || surfaceSwitchWords.MatchString(s.desc) {
			hits = append(hits, surfaceHit{s.key(), "a string flag whose enum or description gives the values true and false; make it a bool flag"})
		}
	}
	return hits
}

var (
	surfaceCodeSpan  = regexp.MustCompile("`[^`]*`")
	surfaceBarList   = regexp.MustCompile(`\w\|\w`)
	surfaceWordsList = regexp.MustCompile(`\(\s*[a-z][a-z0-9-]*(?:\s*,\s*[a-z][a-z0-9-]*){2,}\s*\)`)
)

// surfaceEnumHits returns every value flag with no enum whose description,
// with its backtick spans blanked, writes a list of values: a "|" between two
// word characters, or a parenthesized list of three or more comma-separated
// lowercase words. It does not read the help rows, which print the enum.
func surfaceEnumHits(m runtime.Manifest) []surfaceHit {
	var hits []surfaceHit
	for _, s := range surfaceSites(m) {
		if !s.isValueFlag() || len(s.enum) > 0 {
			continue
		}
		desc := surfaceCodeSpan.ReplaceAllString(s.desc, " ")
		if surfaceBarList.MatchString(desc) || surfaceWordsList.MatchString(desc) {
			hits = append(hits, surfaceHit{s.key(), fmt.Sprintf("the description %q lists values; make the flag an enum with those values", s.desc)})
		}
	}
	return hits
}

// surfaceShort is an allowed short flag: its letter and the long name it
// stands for.
type surfaceShort struct {
	letter string
	name   string
}

// surfaceShorts are the letters kubectl gives a meaning, with the long name
// that meaning has here, in the order rule 10 of SURFACE.md lists them;
// TestEveryShortFlagMeansWhatKubectlMeans compares the two.
var surfaceShorts = []surfaceShort{
	{"n", "namespace"},
	{"o", "output"},
	{"l", "selector"},
	{"p", "patch"},
	{"f", "filename"},
	{"A", "all-namespaces"},
	{"c", "container"},
	{"v", "v"},
	{"h", "help"},
}

// surfaceShortHits returns every short form in the manifest, own or global,
// that is not one of the allowed pairs. The CLI's own -h and -v are outside
// the manifest and are not read.
func surfaceShortHits(m runtime.Manifest, allowed []surfaceShort) []surfaceHit {
	var hits []surfaceHit
	for _, s := range surfaceSites(m) {
		for _, letter := range s.letters {
			if !slices.Contains(allowed, surfaceShort{letter, s.name}) {
				hits = append(hits, surfaceHit{s.letterKey(letter), fmt.Sprintf("-%s is not kubectl's short form of --%s; remove -%s", letter, s.name, letter)})
			}
		}
	}
	return hits
}

// surfaceObjectHits returns every positional named cluster, namespace,
// project or server, which name where an object belongs and not the object,
// and every flag named name, a value that belongs in an argument.
func surfaceObjectHits(m runtime.Manifest) []surfaceHit {
	var hits []surfaceHit
	for _, s := range surfaceSites(m) {
		switch {
		case s.kind == surfacePositional && slices.Contains([]string{"cluster", "namespace", "project", "server"}, s.name):
			why := fmt.Sprintf("the %s an object belongs to is a flag, not an argument; make it --%s and make the object the argument", s.name, s.name)
			if s.name == "server" {
				why = "a server entry is the argument <name>; rename <server> to <name>"
			}
			hits = append(hits, surfaceHit{s.key(), why})
		case s.isFlag() && s.name == "name":
			hits = append(hits, surfaceHit{s.key(), "the object is an argument, not --name; move the value of --name to the argument <name>"})
		}
	}
	return hits
}

// surfaceVerbRow is one row of the verb table: the exact verbs, the verb
// prefixes and the whole command names it accepts. verbFirstOnly limits it to
// the verb-first groups; localOnly limits it to a group whose commands all
// have the command type local.
type surfaceVerbRow struct {
	verbs         []string
	prefixes      []string
	commands      []string
	verbFirstOnly bool
	localOnly     bool
}

// surfaceVerbTable is the verb table of rule 12. nouns are the top-level
// commands that print a thing and never change server state; verbFirst are
// the groups whose first word is the verb.
type surfaceVerbTable struct {
	rows      []surfaceVerbRow
	nouns     []string
	verbFirst []string
}

// surfaceVerbs has the rows of the verb table in SURFACE.md under rule 12, in
// the same order; TestEveryCommandUsesTheVerbTable compares the two.
var surfaceVerbs = surfaceVerbTable{
	rows: []surfaceVerbRow{
		{verbs: []string{"create", "get", "patch", "delete"}, verbFirstOnly: true},
		{verbs: []string{"inspect"}, verbFirstOnly: true},
		{prefixes: []string{"add-", "remove-", "rotate-"}},
		{verbs: []string{"install", "upgrade", "uninstall"}},
		{verbs: []string{"grant", "revoke"}},
		{commands: []string{
			"cluster scale", "cluster upgrade", "cluster save-etcd-snapshot",
			"cluster pause-autoscaling", "cluster resume-autoscaling",
			"node drain", "node stop-drain",
		}},
		{prefixes: []string{"set-", "clear-"}},
		{verbs: []string{"status"}},
		{
			verbs:     []string{"edit", "path", "login", "logout", "prune"},
			prefixes:  []string{"add-", "update-", "delete-", "use-", "list-"},
			localOnly: true,
		},
		{verbs: []string{"on", "off"}},
		{verbs: []string{"show", "reset", "upload"}},
	},
	nouns: []string{
		"charts", "images", "usage", "logs", "kubeconfig", "schema", "completion", "support-bundle",
	},
	verbFirst: []string{"create", "get", "patch", "delete", "inspect"},
}

// surfaceVerbHits returns every command whose verb matches no row it may use,
// and every top-level noun whose command type is mutating. The verb is the
// first word in a verb-first group, the second word in another group and the
// whole name of a top-level command.
func surfaceVerbHits(m runtime.Manifest, table surfaceVerbTable) []surfaceHit {
	notLocal := map[string]bool{}
	for _, c := range m.Commands {
		if group, _, ok := strings.Cut(c.Name, " "); ok && c.CommandType != runtime.Local {
			notLocal[group] = true
		}
	}
	var hits []surfaceHit
	for _, c := range m.Commands {
		group, rest, grouped := strings.Cut(c.Name, " ")
		verbFirst := grouped && slices.Contains(table.verbFirst, group)
		localGroup := grouped && !notLocal[group]
		verb := c.Name
		switch {
		case verbFirst:
			verb = group
		case grouped:
			verb = rest
		}
		allowed := !grouped && slices.Contains(table.nouns, c.Name)
		localOnlyMatch := false
		for _, row := range table.rows {
			if row.verbFirstOnly && !verbFirst {
				continue
			}
			if !slices.Contains(row.commands, c.Name) &&
				!slices.Contains(row.verbs, verb) &&
				!slices.ContainsFunc(row.prefixes, func(p string) bool { return strings.HasPrefix(verb, p) }) {
				continue
			}
			if row.localOnly && !localGroup {
				localOnlyMatch = true
				continue
			}
			allowed = true
		}
		switch {
		case !grouped && slices.Contains(table.nouns, c.Name) && c.CommandType == runtime.Mutating:
			hits = append(hits, surfaceHit{c.Name, "a top-level noun never changes server state; name the command with a verb from the verb table"})
		case allowed:
		case localOnlyMatch:
			hits = append(hits, surfaceHit{c.Name, fmt.Sprintf("the verb %q is only for a group whose commands are all local; use another verb from the verb table", verb)})
		default:
			hits = append(hits, surfaceHit{c.Name, fmt.Sprintf("the verb %q is not in the verb table; use a verb from the table. To add a verb, add a row to the table in SURFACE.md and to surfaceVerbs in internal/cli/surfaceconventions_test.go (the test compares the two)", verb)})
		}
	}
	return hits
}

var surfaceConceptAllowlist = surfaceAllowlist{name: "surfaceConceptAllowlist", count: 13, rows: []surfaceRow{
	{"auth get-token --server", "a flag of the exec block of a kubeconfig, which Rancher writes and existing kubeconfig files have"},
	{"cluster scale --nodes", "to be renamed --quantity, the name of the machine pool field"},
	{"cluster set-autoscaling --max", "to be renamed --max-quantity"},
	{"cluster set-autoscaling --min", "to be renamed --min-quantity"},
	{"cluster set-upgrade-strategy --control-plane-delete-emptydir", "to be renamed --control-plane-delete-emptydir-data, the name of the drain option field"},
	{"cluster set-upgrade-strategy --worker-delete-emptydir", "to be renamed --worker-delete-emptydir-data, the name of the drain option field"},
	{"create cluster --controlplane", "to be renamed --control-plane, the name the upgrade strategy flags use"},
	{"create cluster --pool-name", "to be renamed --pool, the name the cluster commands use"},
	{"create kubeconfig --clusters", "to be replaced by a repeatable --cluster"},
	{"inspect cluster --controlplane", "to be renamed --control-plane, the name the upgrade strategy flags use"},
	{"support-bundle --clusters", "to be replaced by a repeatable --cluster"},
	{"token --cacerts", "a flag of the exec block of a kubeconfig, which Rancher writes and existing kubeconfig files have"},
	{"token --server", "a flag of the exec block of a kubeconfig, which Rancher writes and existing kubeconfig files have"},
}}

var surfaceListAllowlist = surfaceAllowlist{name: "surfaceListAllowlist", count: 10, rows: []surfaceRow{
	{"create kubeconfig --clusters", "a comma list; to be replaced by a repeatable --cluster"},
	{"create machine-config --tags", "a comma list; to become a repeatable --tag"},
	{"create project --container-default-resource-limit", "a comma list of key=value pairs; to become repeatable"},
	{"create project --namespace-default-quota", "a comma list of key=value pairs; to become repeatable"},
	{"create project --quota-limit", "a comma list of key=value pairs; to become repeatable"},
	{"create secret --from-literal", "a comma list of key=value pairs; to repeat as kubectl's --from-literal does"},
	{"inspect cluster --address", "a comma list of addresses; to become repeatable"},
	{"inspect cluster --internal-address", "a comma list of addresses; to become repeatable"},
	{"support-bundle --clusters", "a comma list; to be replaced by a repeatable --cluster"},
	{"support-bundle --targets", "a comma list; to become a repeatable --target"},
}}

var surfaceSwitchAllowlist = surfaceAllowlist{name: "surfaceSwitchAllowlist", count: 3, rows: []surfaceRow{
	{"config set-completion --network", "a string flag whose values are true and false; to become a bool flag"},
	{"create kubeconfig --include-default-entry", "a string flag whose values are true and false; to become a bool flag"},
	{"create user --enabled", "a string flag whose values are true and false; to become a bool flag"},
}}

var surfaceEnumAllowlist = surfaceAllowlist{name: "surfaceEnumAllowlist", count: 1, rows: []surfaceRow{
	{"support-bundle --targets", "the description lists the values; to become an enum"},
}}

var surfaceShortAllowlist = surfaceAllowlist{name: "surfaceShortAllowlist", count: 1, rows: []surfaceRow{
	{"-c (--config)", "the short form of --config; kubectl's -c is --container"},
}}

var surfaceObjectAllowlist = surfaceAllowlist{name: "surfaceObjectAllowlist", count: 11, rows: []surfaceRow{
	{"auth get-token --name", "a flag of the exec block of a kubeconfig, which Rancher writes and existing kubeconfig files have"},
	{"cluster clear-autoscaling <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster pause-autoscaling <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster resume-autoscaling <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster save-etcd-snapshot <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster scale <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster set-autoscaling <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster set-upgrade-strategy <cluster>", "the cluster is the object of the cluster commands"},
	{"cluster upgrade <cluster>", "the cluster is the object of the cluster commands"},
	{"project add-namespace <namespace>", "the namespace is the object of the command"},
	{"project remove-namespace <namespace>", "the namespace is the object of the command"},
}}

var surfaceVerbAllowlist = surfaceAllowlist{name: "surfaceVerbAllowlist", count: 9, rows: []surfaceRow{
	{"access can", "a question group: the command is named after the question it asks"},
	{"access clusters", "a question group: the command is named after the question it asks"},
	{"access explain", "a question group: the command is named after the question it asks"},
	{"access list", "to be renamed access rules"},
	{"access projects", "a question group: the command is named after the question it asks"},
	{"access who-can", "a question group: the command is named after the question it asks"},
	{"auth get-token", "the exec plugin name Rancher writes into kubeconfigs"},
	{"extension list", "lists the extensions the ClusterRepos offer and the installed ones, which no single kind has"},
	{"token", "the older exec plugin name"},
}}

// TestEveryFlagNameIsInTheConceptTable asserts rule 1: every flag name of the
// schema manifest has a row in the concept table, every row has a flag, and
// no flag has a name other than the one name of its concept on every command,
// except the commands on the allowlist.
func TestEveryFlagNameIsInTheConceptTable(t *testing.T) {
	t.Parallel()
	rule := surfaceRuleLabel(t, surfaceDocText(t))
	hits, problems := surfaceConceptHits(surfaceManifest(t), surfaceConcepts)
	for _, problem := range problems {
		assert.Fail(t, rule+": "+problem)
	}
	surfaceReport(t, surfaceCheck(rule, hits, surfaceConceptAllowlist))
}

// TestEveryListFlagIsRepeatable asserts rule 4: no string flag takes a list
// of identifiers or of key=value pairs in one value, as its description
// shows, except the commands on the allowlist. A comma split inside a command
// whose description says nothing of commas is not checked here.
func TestEveryListFlagIsRepeatable(t *testing.T) {
	t.Parallel()
	rule := surfaceRuleLabel(t, surfaceDocText(t))
	surfaceReport(t, surfaceCheck(rule, surfaceListHits(surfaceManifest(t)), surfaceListAllowlist))
}

// TestEverySwitchIsABoolFlag asserts rule 8: no string flag holds an on/off
// value, as its enum or its description shows, except the commands on the
// allowlist.
func TestEverySwitchIsABoolFlag(t *testing.T) {
	t.Parallel()
	rule := surfaceRuleLabel(t, surfaceDocText(t))
	surfaceReport(t, surfaceCheck(rule, surfaceSwitchHits(surfaceManifest(t)), surfaceSwitchAllowlist))
}

// TestEveryValueListIsAnEnum asserts rule 9 where its input exists: no value
// flag lists its values in its description instead of an enum, except the
// commands on the allowlist. The help rows that print the enum are not
// checked here.
func TestEveryValueListIsAnEnum(t *testing.T) {
	t.Parallel()
	rule := surfaceRuleLabel(t, surfaceDocText(t))
	surfaceReport(t, surfaceCheck(rule, surfaceEnumHits(surfaceManifest(t)), surfaceEnumAllowlist))
}

// TestEveryShortFlagMeansWhatKubectlMeans asserts rule 10: every short flag
// in the schema manifest is kubectl's letter for the same long name, except
// the flags on the allowlist. The CLI's own -h and -v are outside the
// manifest and are not read. It also asserts that rule 10 of SURFACE.md lists
// the letters of surfaceShorts, in the same order.
func TestEveryShortFlagMeansWhatKubectlMeans(t *testing.T) {
	t.Parallel()
	doc := surfaceDocText(t)
	rule := surfaceRuleLabel(t, doc)
	failures := surfaceCheck(rule, surfaceShortHits(surfaceManifest(t), surfaceShorts), surfaceShortAllowlist)
	for _, p := range surfaceShortListProblems(surfaceDocShortLetters(doc), surfaceShorts) {
		failures = append(failures, rule+": "+p)
	}
	surfaceReport(t, failures)
}

// TestEveryPositionalIsTheObject asserts rule 11: no positional is named
// after a scope (cluster, namespace, project, server) and no flag is named
// name, except the commands on the allowlist.
func TestEveryPositionalIsTheObject(t *testing.T) {
	t.Parallel()
	rule := surfaceRuleLabel(t, surfaceDocText(t))
	surfaceReport(t, surfaceCheck(rule, surfaceObjectHits(surfaceManifest(t)), surfaceObjectAllowlist))
}

// TestEveryCommandUsesTheVerbTable asserts rule 12: the verb of every command
// is in the verb table, a verb for local commands appears only in a group of
// local commands, and no top-level noun changes server state, except the
// commands on the allowlist. It also asserts that the verb table in SURFACE.md
// has the rows of surfaceVerbs, in the same order.
func TestEveryCommandUsesTheVerbTable(t *testing.T) {
	t.Parallel()
	doc := surfaceDocText(t)
	rule := surfaceRuleLabel(t, doc)
	failures := surfaceCheck(rule, surfaceVerbHits(surfaceManifest(t), surfaceVerbs), surfaceVerbAllowlist)
	for _, p := range surfaceVerbTableProblems(surfaceDocVerbRows(doc), surfaceVerbs) {
		failures = append(failures, rule+": "+p)
	}
	surfaceReport(t, failures)
}
