package provisioning

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optionCandidate is one typed value at one position of one option flag, with
// the outcome the rule gives it.
type optionCandidate struct {
	name   string
	flag   string
	value  string
	opts   NodeCommandOptions
	accept bool
}

// withValue is NodeCommandOptions with v as the only value of flag.
func withValue(flag, v string) NodeCommandOptions {
	switch flag {
	case NodeOptionNodeName:
		return NodeCommandOptions{NodeName: v}
	case NodeOptionAddress:
		return NodeCommandOptions{Address: v}
	case NodeOptionInternalAddress:
		return NodeCommandOptions{InternalAddress: v}
	case NodeOptionLabel:
		return NodeCommandOptions{Labels: []string{v}}
	case NodeOptionTaint:
		return NodeCommandOptions{Taints: []string{v}}
	}
	panic("no value flag " + flag)
}

// inChars reports whether c is a letter, a digit or one of extra.
func inChars(c byte, extra string) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(extra, c) >= 0
}

// optionPosition builds the value that has a character at one position and
// says which characters the position accepts.
type optionPosition struct {
	name   string
	flag   string
	build  func(c string) string
	accept func(c byte) bool
}

var optionPositions = []optionPosition{
	{"label key", NodeOptionLabel, func(c string) string { return "a" + c + "b=v" }, func(c byte) bool { return inChars(c, "./_-") }},
	{"label value", NodeOptionLabel, func(c string) string { return "k=a" + c + "b" }, func(c byte) bool { return inChars(c, "._-") }},
	{"taint key", NodeOptionTaint, func(c string) string { return "a" + c + "b=v:NoSchedule" }, func(c byte) bool { return inChars(c, "./_-") }},
	{"taint value", NodeOptionTaint, func(c string) string { return "k=a" + c + "b:NoSchedule" }, func(c byte) bool { return inChars(c, "._-") }},
	{"taint effect", NodeOptionTaint, func(c string) string { return "k=v:No" + c + "Schedule" }, func(byte) bool { return false }},
	// A node name is a lowercase DNS subdomain for Kubernetes: no
	// uppercase letter and no "_", although the Rancher UI keeps both.
	{"node name", NodeOptionNodeName, func(c string) string { return "a" + c + "b" }, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-'
	}},
	// "," separates two addresses.
	{"address piece", NodeOptionAddress, func(c string) string { return "a" + c + "b" }, func(c byte) bool { return inChars(c, ".:_-,") }},
	{"internal address piece", NodeOptionInternalAddress, func(c string) string { return "a" + c + "b" }, func(c byte) bool { return inChars(c, ".:_-,") }},
}

// nonASCII are characters outside ASCII, an invalid byte among them.
var nonASCII = []string{"\u2018", "\u2019", "\u00a0", "\u200b", "\xff"}

// optionCandidates is every candidate the value rule is checked on: every
// ASCII byte and each non-ASCII character at each position, and the rows of
// Kubernetes' own rules.
func optionCandidates() []optionCandidate {
	var out []optionCandidate
	for _, p := range optionPositions {
		for b := range 0x80 {
			c := string(rune(b))
			out = append(out, optionCandidate{
				name: fmt.Sprintf("%s byte %#02x", p.name, b), flag: p.flag, value: p.build(c),
				opts: withValue(p.flag, p.build(c)), accept: p.accept(byte(b)),
			})
		}
		for _, c := range nonASCII {
			out = append(out, optionCandidate{
				name: fmt.Sprintf("%s %+q", p.name, c), flag: p.flag, value: p.build(c), opts: withValue(p.flag, p.build(c)),
			})
		}
	}
	long := strings.Repeat("a", 64)
	for _, row := range []struct {
		name, flag, value string
		accept            bool
	}{
		{"uppercase node name", NodeOptionNodeName, "Node1", false},
		{"lowercase node name", NodeOptionNodeName, "node1.example", true},
		{"node name starting with a dot", NodeOptionNodeName, ".node", false},
		{"64-character label value", NodeOptionLabel, "k=" + long, false},
		{"63-character label value", NodeOptionLabel, "k=" + long[:63], true},
		{"label key with two slashes", NodeOptionLabel, "a/b/c=v", false},
		{"label key with a prefix", NodeOptionLabel, "example.com/tier=db", true},
		{"label key with an uppercase prefix", NodeOptionLabel, "Example.com/tier=db", false},
		{"label key starting with a dash", NodeOptionLabel, "-a=v", false},
		{"64-character label key name", NodeOptionLabel, long + "=v", false},
		{"taint key with two slashes", NodeOptionTaint, "a/b/c=v:NoSchedule", false},
		{"taint key starting with a dot", NodeOptionTaint, ".a=v:NoSchedule", false},
		{"64-character taint value", NodeOptionTaint, "k=" + long + ":NoExecute", false},
		{"taint value ending in a dash", NodeOptionTaint, "k=v-:NoSchedule", false},
		{"taint effect PreferNoSchedule", NodeOptionTaint, "k=v:PreferNoSchedule", true},
		{"taint effect NoExecute", NodeOptionTaint, "k=v:NoExecute", true},
		{"taint effect in lowercase", NodeOptionTaint, "k=v:noschedule", false},
		{"address list", NodeOptionAddress, "10.0.0.1,fd00::1", true},
		{"address with an empty piece", NodeOptionAddress, "a,,b", false},
		{"address ending in a comma", NodeOptionAddress, "a,", false},
		{"address backslash alone", NodeOptionAddress, `\`, false},
		{"address two backslashes", NodeOptionAddress, `\\`, false},
		{"internal address backslash alone", NodeOptionInternalAddress, `\`, false},
	} {
		out = append(out, optionCandidate{name: row.name, flag: row.flag, value: row.value, opts: withValue(row.flag, row.value), accept: row.accept})
	}
	return out
}

// composedValue is the text ComposeNodeCommand prints for the one value of
// the candidate.
func composedValue(c optionCandidate) string {
	spelling := map[string]string{
		NodeOptionNodeName: "--node-name", NodeOptionAddress: "--address", NodeOptionInternalAddress: "--internal-address",
		NodeOptionLabel: "--label", NodeOptionTaint: "--taints",
	}[c.flag]
	return spelling + " '" + c.value + "'"
}

// TestNodeCommandOptionValueIsAcceptedExactlyByItsRule asserts every typed
// value is accepted exactly when its characters are in the Rancher UI's set
// for its position, its shape is key=value or key=value:effect where the flag
// needs one, and Kubernetes' rule for its kind accepts it; an accepted value is
// printed unchanged, a refused one is a VALIDATION error that names the flag.
func TestNodeCommandOptionValueIsAcceptedExactlyByItsRule(t *testing.T) {
	t.Parallel()
	const base = "curl -fL https://rancher.example/system-agent-install.sh | sudo sh -s - --token tok"
	for _, c := range optionCandidates() {
		valid, err := ValidateNodeCommandOptions(c.opts)
		if !c.accept {
			require.Error(t, err, c.name)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, c.name)
			assert.Equal(t, clierr.CodeValidation, ce.Code, c.name)
			assert.True(t, strings.HasPrefix(ce.Message, "--"+c.flag+" "), "%s: %s", c.name, ce.Message)
			continue
		}
		require.NoError(t, err, c.name)
		text, result := ComposeNodeCommand(base, valid)
		require.Equal(t, Composed, result, c.name)
		assert.Contains(t, text, " "+composedValue(c), "%s: the value is printed as typed", c.name)
	}
}

// TestNodeCommandOptionRefusalShowsTheValueEscaped asserts a refusal shows
// the typed value and the refused character with every non-ASCII rune and
// invalid byte escaped, and the exact text of each kind of refusal.
func TestNodeCommandOptionRefusalShowsTheValueEscaped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 64)
	for _, row := range []struct {
		name string
		opts NodeCommandOptions
		want string
	}{
		{"space in a label key", NodeCommandOptions{Labels: []string{"a b=c"}},
			`--label "a b=c": the key has " ", which the Rancher UI would remove; use letters, digits, ".", "/", "_" or "-"`},
		{"curly quote in a label key", NodeCommandOptions{Labels: []string{"a\u2018b=c"}},
			`--label "a\u2018b=c": the key has "\u2018", which the Rancher UI would remove; use letters, digits, ".", "/", "_" or "-"`},
		{"invalid byte in a label value", NodeCommandOptions{Labels: []string{"k=a\xffb"}},
			`--label "k=a\xffb": the value has "\xff", which the Rancher UI would remove; use letters, digits, ".", "_" or "-"`},
		{"tab in a node name", NodeCommandOptions{NodeName: "a\tb"},
			`--node-name "a\tb": the node name has "\t", which the Rancher UI would remove; use letters, digits, ".", "_" or "-"`},
		{"space in an address", NodeCommandOptions{Address: "10.0.0.1, 10.0.0.2"},
			`--address "10.0.0.1, 10.0.0.2": an address has " ", which the Rancher UI would remove; use letters, digits, ".", ":", "_" or "-", with "," between addresses`},
		{"empty address piece", NodeCommandOptions{InternalAddress: "a,,b"},
			`--internal-address "a,,b": the list has an empty address; separate addresses with one ","`},
		{"label with no equals sign", NodeCommandOptions{Labels: []string{"tier"}},
			`--label "tier": the label has no "="; give it as key=value`},
		{"label with an empty key", NodeCommandOptions{Labels: []string{"=db"}},
			`--label "=db": the key is empty; give the label as key=value`},
		{"label with an empty value", NodeCommandOptions{Labels: []string{"tier="}},
			`--label "tier=": the value is empty; give the label as key=value`},
		{"taint with no equals sign", NodeCommandOptions{Taints: []string{"k:NoSchedule"}},
			`--taint "k:NoSchedule": the taint has no "="; give it as key=value:effect`},
		{"taint with no colon", NodeCommandOptions{Taints: []string{"k=v"}},
			`--taint "k=v": the taint has no ":"; give it as key=value:effect`},
		{"taint with an empty value", NodeCommandOptions{Taints: []string{"k=:NoSchedule"}},
			`--taint "k=:NoSchedule": the value is empty; give the taint as key=value:effect`},
		{"taint with an empty effect", NodeCommandOptions{Taints: []string{"k=v:"}},
			`--taint "k=v:": the effect is empty; use NoSchedule, PreferNoSchedule or NoExecute`},
		{"taint with another effect", NodeCommandOptions{Taints: []string{"k=v:Evict"}},
			`--taint "k=v:Evict": the effect is "Evict"; use NoSchedule, PreferNoSchedule or NoExecute`},
		{"taint value with a colon", NodeCommandOptions{Taints: []string{"k=a:b:NoSchedule"}},
			`--taint "k=a:b:NoSchedule": the value has ":", which the Rancher UI would remove; use letters, digits, ".", "_" or "-"`},
		{"64-character label value", NodeCommandOptions{Labels: []string{"k=" + long}},
			`--label "k=` + long + `": the value breaks the Kubernetes rule for a label value: must be no more than 63 bytes`},
		{"uppercase node name", NodeCommandOptions{NodeName: "Node1"},
			`--node-name "Node1": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
		{"node name starting with a dash", NodeCommandOptions{NodeName: "-node"},
			`--node-name "-node": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
		{"node name ending with a dash", NodeCommandOptions{NodeName: "node-"},
			`--node-name "node-": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
		{"node name ending with a dot", NodeCommandOptions{NodeName: "node."},
			`--node-name "node.": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
		{"254-character node name", NodeCommandOptions{NodeName: strings.Repeat("a", 254)},
			`--node-name "` + strings.Repeat("a", 254) + `": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
		{"label key given twice", NodeCommandOptions{Labels: []string{"tier=db", "zone=a", "tier=web"}},
			`--label tier is given twice`},
		{"label key Rancher sets", NodeCommandOptions{Labels: []string{"rke.cattle.io/node-name=n1"}},
			`--label rke.cattle.io/node-name: Rancher sets this label on the node itself`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := ValidateNodeCommandOptions(row.opts)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, row.want, ce.Message)
		})
	}
}

// TestNodeCommandOptionsWithWindowsRefuseWhatTheWindowsNodeCommandCannotTake
// asserts a role flag, any label and a second taint are refused with
// Windows, and that one taint is accepted.
func TestNodeCommandOptionsWithWindowsRefuseWhatTheWindowsNodeCommandCannotTake(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		opts NodeCommandOptions
		want string
	}{
		{"etcd", NodeCommandOptions{Etcd: true, Windows: true}, "--etcd conflicts with --windows; remove --etcd (Windows nodes can only be workers)"},
		{"controlplane", NodeCommandOptions{ControlPlane: true, Windows: true}, "--controlplane conflicts with --windows; remove --controlplane (Windows nodes can only be workers)"},
		{"worker", NodeCommandOptions{Worker: true, Windows: true}, "--worker conflicts with --windows; remove --worker (Windows nodes can only be workers)"},
		{"label", NodeCommandOptions{Labels: []string{"tier=db"}, Windows: true}, "--label conflicts with --windows; remove --label (the Windows node command already passes -Label)"},
		{"two taints", NodeCommandOptions{Taints: []string{"a=b:NoSchedule", "c=d:NoSchedule"}, Windows: true}, "--taint given more than once conflicts with --windows; remove all but one --taint"},
		{"a role before a bad value", NodeCommandOptions{Worker: true, NodeName: "A B", Windows: true}, "--worker conflicts with --windows; remove --worker (Windows nodes can only be workers)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := ValidateNodeCommandOptions(row.opts)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, row.want, ce.Message)
		})
	}
	valid, err := ValidateNodeCommandOptions(NodeCommandOptions{Taints: []string{"os=windows:NoSchedule"}, NodeName: "win1", Windows: true})
	require.NoError(t, err)
	assert.True(t, valid.Windows())
}

// TestNodeCommandOptionRefusalOrderFollowsTheFlags asserts the first flag in
// the option flag order whose value is refused is the one the error names.
func TestNodeCommandOptionRefusalOrderFollowsTheFlags(t *testing.T) {
	t.Parallel()
	_, err := ValidateNodeCommandOptions(NodeCommandOptions{NodeName: "a b", Labels: []string{"x"}, Taints: []string{"y"}})
	require.ErrorContains(t, err, "--node-name ")
	_, err = ValidateNodeCommandOptions(NodeCommandOptions{Address: "a b", Labels: []string{"x"}})
	require.ErrorContains(t, err, "--address ")
	_, err = ValidateNodeCommandOptions(NodeCommandOptions{Labels: []string{"x"}, Taints: []string{"y"}})
	require.ErrorContains(t, err, "--label ")
}

// TestEveryKeyRancherSetsIsRefusedAsATypedLabel asserts every label key
// Rancher sets on a node that joins with the node command is refused.
func TestEveryKeyRancherSetsIsRefusedAsATypedLabel(t *testing.T) {
	t.Parallel()
	keys := []string{
		"cattle.io/os",
		"rke.cattle.io/cluster-name",
		"rke.cattle.io/control-plane-role",
		"rke.cattle.io/etcd-role",
		"rke.cattle.io/machine-id",
		"rke.cattle.io/machine",
		"rke.cattle.io/node-name",
		"rke.cattle.io/worker-role",
		"cluster.x-k8s.io/control-plane",
		"cluster.x-k8s.io/cluster-name",
		"plan.cattle.io/cluster-group",
		"plan.cattle.io/cluster-kind",
		"plan.cattle.io/cluster-name",
		"plan.cattle.io/machine-group",
		"plan.cattle.io/machine-kind",
		"plan.cattle.io/machine-name",
	}
	for _, key := range keys {
		_, err := ValidateNodeCommandOptions(NodeCommandOptions{Labels: []string{key + "=x"}})
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, key)
		assert.Equal(t, clierr.CodeValidation, ce.Code, key)
		assert.Equal(t, "--label "+key+": Rancher sets this label on the node itself", ce.Message)
	}
	_, err := ValidateNodeCommandOptions(NodeCommandOptions{Labels: []string{"rke.cattle.io/other=x"}})
	require.NoError(t, err, "a key Rancher does not set is accepted")
}

// The node commands the tests use, in the shape of Rancher's four commands
// (Linux and Windows, each secure and insecure): the agent environment
// variables, the install script URL, the server, the token and the CA
// checksum ending.
func linuxBase(insecure bool, envVars, checksum string) string {
	curl := "curl -fL"
	if insecure {
		curl = "curl --insecure -fL"
	}
	return fmt.Sprintf("%s %s %s | sudo %s sh -s - --server %s --label 'cattle.io/os=linux' --token %s%s",
		envVars, curl, "https://rancher.example/system-agent-install.sh", envVars, "https://rancher.example", "tok123", checksum)
}

func windowsBase(insecure bool, envVars, checksum string) string {
	curl := "curl.exe -fL"
	if insecure {
		curl = "curl.exe --insecure -fL"
	}
	return fmt.Sprintf("%s %s %s -o install.ps1; Set-ExecutionPolicy Bypass -Scope Process -Force; ./install.ps1 -Server %s -Label 'cattle.io/os=windows' -Token %s -Worker%s",
		envVars, curl, "https://rancher.example/wins-agent-install.ps1", "https://rancher.example", "tok123", checksum)
}

// TestComposedNodeCommandIsTheNodeCommandFollowedByTheOptions asserts every
// composed command is the node command text as given, one space, then the
// options in the Rancher UI's order, each value single-quoted.
func TestComposedNodeCommandIsTheNodeCommandFollowedByTheOptions(t *testing.T) {
	t.Parallel()
	secure := linuxBase(false, "", " --ca-checksum abc123")
	insecure := linuxBase(true, "", "")
	withEnv := linuxBase(false, `HTTP_PROXY="http://proxy:3128" NO_PROXY="127.0.0.1"`, "")
	win := windowsBase(false, "", " -CaChecksum abc123")
	winInsecure := windowsBase(true, `$env:HTTP_PROXY="http://proxy:3128";`, "")
	for _, row := range []struct {
		name string
		base string
		opts NodeCommandOptions
		want string
	}{
		{"no role flag", secure, NodeCommandOptions{Labels: []string{"tier=db"}}, secure + " --etcd --controlplane --worker --label 'tier=db'"},
		{"one role", secure, NodeCommandOptions{Worker: true}, secure + " --worker"},
		{"two roles", insecure, NodeCommandOptions{Etcd: true, ControlPlane: true}, insecure + " --etcd --controlplane"},
		{"three roles", insecure, NodeCommandOptions{Worker: true, Etcd: true, ControlPlane: true}, insecure + " --etcd --controlplane --worker"},
		{"node name", insecure, NodeCommandOptions{NodeName: "node-1"}, insecure + " --etcd --controlplane --worker --node-name 'node-1'"},
		{"address", secure, NodeCommandOptions{Worker: true, Address: "10.0.0.5,fd00::5"}, secure + " --worker --address '10.0.0.5,fd00::5'"},
		{"internal address", secure, NodeCommandOptions{Worker: true, InternalAddress: "192.168.1.5"}, secure + " --worker --internal-address '192.168.1.5'"},
		{"labels in typed order", secure, NodeCommandOptions{Worker: true, Labels: []string{"zone=b", "app=a"}}, secure + " --worker --label 'zone=b' --label 'app=a'"},
		{"taints in typed order", secure, NodeCommandOptions{Worker: true, Taints: []string{"z=1:NoSchedule", "a=2:NoExecute"}}, secure + " --worker --taints 'z=1:NoSchedule' --taints 'a=2:NoExecute'"},
		{"every option, typed in another order", withEnv, NodeCommandOptions{
			Taints: []string{"t=v:PreferNoSchedule"}, Labels: []string{"l=v"}, NodeName: "n1", InternalAddress: "10.1.0.1", Address: "10.0.0.1", Worker: true, Etcd: true,
		}, withEnv + " --etcd --worker --address '10.0.0.1' --internal-address '10.1.0.1' --node-name 'n1' --label 'l=v' --taints 't=v:PreferNoSchedule'"},
		{"a node command ending in the token", insecure, NodeCommandOptions{ControlPlane: true}, insecure + " --controlplane"},
		{"windows alone", win, NodeCommandOptions{Windows: true}, win},
		{"windows node name", win, NodeCommandOptions{Windows: true, NodeName: "win1"}, win + " -NodeName 'win1'"},
		{"windows every option", winInsecure, NodeCommandOptions{
			Windows: true, Taints: []string{"os=windows:NoSchedule"}, NodeName: "win1", InternalAddress: "10.1.0.2", Address: "10.0.0.2",
		}, winInsecure + " -Address '10.0.0.2' -InternalAddress '10.1.0.2' -NodeName 'win1' -Taint 'os=windows:NoSchedule'"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			valid, err := ValidateNodeCommandOptions(row.opts)
			require.NoError(t, err)
			got, result := ComposeNodeCommand(row.base, valid)
			require.Equal(t, Composed, result)
			assert.Equal(t, row.want, got)
		})
	}
}

// TestComposedNodeCommandIsLeftOutForAnUnusableNodeCommand asserts no command
// is composed from an empty node command or one that still has the token
// placeholder.
func TestComposedNodeCommandIsLeftOutForAnUnusableNodeCommand(t *testing.T) {
	t.Parallel()
	valid, err := ValidateNodeCommandOptions(NodeCommandOptions{Worker: true})
	require.NoError(t, err)
	for _, base := range []string{"", linuxBase(false, "", "")[:40] + " --token {token}"} {
		got, result := ComposeNodeCommand(base, valid)
		assert.Equal(t, BaseNotFormatted, result, base)
		assert.Empty(t, got)
	}
	// No accepted value is one FlagFragment refuses; options built here
	// without the validator show the result such a value would give.
	for _, o := range []NodeCommandOptions{{Address: `a\`}, {Windows: true, NodeName: "a'b"}, {Labels: []string{`k=v\`}}} {
		got, result := ComposeNodeCommand(linuxBase(false, "", ""), ValidNodeCommandOptions{o: o})
		assert.Equal(t, ValueNotQuotable, result, "%+v", o)
		assert.Empty(t, got)
	}
}

// acceptedValues are the values of o that a composed command prints.
func acceptedValues(o NodeCommandOptions) []string {
	var out []string
	for _, v := range []string{o.NodeName, o.Address, o.InternalAddress} {
		if v != "" {
			out = append(out, v)
		}
	}
	return append(append(out, o.Labels...), o.Taints...)
}

// assertAcceptedIsQuotable asserts every value of o is one every supported
// shell reads as written when o is accepted.
func assertAcceptedIsQuotable(t *testing.T, name string, o NodeCommandOptions) {
	t.Helper()
	if _, err := ValidateNodeCommandOptions(o); err != nil {
		return
	}
	for _, v := range acceptedValues(o) {
		assert.False(t, clierr.NotQuotable(v), "%s: accepted value %+q", name, v)
		_, ok := clierr.FlagFragment("--label", v)
		assert.True(t, ok, "%s: accepted value %+q", name, v)
	}
}

// TestEveryAcceptedNodeCommandOptionValueCanBeSingleQuoted asserts that every
// value the rule accepts, over every candidate of the value rule's table
// whatever outcome it expects, can be single-quoted for every supported shell.
func TestEveryAcceptedNodeCommandOptionValueCanBeSingleQuoted(t *testing.T) {
	t.Parallel()
	for _, c := range optionCandidates() {
		assertAcceptedIsQuotable(t, c.name, c.opts)
	}
}

// FuzzNodeCommandOptionValue asserts that every value the rule accepts at
// any option flag can be single-quoted for every supported shell and is
// printed unchanged.
func FuzzNodeCommandOptionValue(f *testing.F) {
	flags := []string{NodeOptionNodeName, NodeOptionAddress, NodeOptionInternalAddress, NodeOptionLabel, NodeOptionTaint}
	for _, c := range optionCandidates() {
		for i, flag := range flags {
			if flag == c.flag {
				f.Add(uint8(i), c.value)
			}
		}
	}
	f.Fuzz(func(t *testing.T, i uint8, v string) {
		if v == "" {
			return
		}
		flag := flags[int(i)%len(flags)]
		o := withValue(flag, v)
		valid, err := ValidateNodeCommandOptions(o)
		if err != nil {
			ce, ok := errors.AsType[*clierr.Error](err)
			if !ok || ce.Code != clierr.CodeValidation || !strings.HasPrefix(ce.Message, "--"+flag+" ") {
				t.Fatalf("refusal of %+q: %v", v, err)
			}
			return
		}
		if clierr.NotQuotable(v) {
			t.Fatalf("accepted value %+q cannot be quoted", v)
		}
		text, result := ComposeNodeCommand("base", valid)
		if result != Composed || !strings.Contains(text, "'"+v+"'") {
			t.Fatalf("accepted value %+q composed as %+q", v, text)
		}
	})
}

// TestWindowsNodeCommandAppliesExactlyWhereTheRancherUIShowsIt asserts the
// Windows node command rule of each Rancher minor: K3s is refused first, the
// Kubernetes version is read from the field the server's minor reads (the
// provisioning spec below v2.15, the management status from v2.15 on and for
// a server that states no number or whose version is not read), a version
// that does not parse or is below v1.21.0 is refused, no version yet is a
// retryable GENERIC error, and the CNI rule of the minor applies last.
func TestWindowsNodeCommandAppliesExactlyWhereTheRancherUIShowsIt(t *testing.T) {
	t.Parallel()
	const (
		k3s      = "--windows: the cluster runs K3s; Windows nodes join only RKE2 clusters"
		older    = `is older than v1.21.0`
		noParse  = `which this CLI cannot compare with v1.21.0`
		cni213   = `on Rancher before v2.14 the Rancher UI shows the Windows node command only for calico`
		cni214   = `the Rancher UI shows the Windows node command only for calico, and for flannel from Kubernetes v1.29.2`
		retry    = "retry"
		accepted = ""
	)
	at := func(server string, read bool, provider, desired, observed, cni string) WindowsClusterFacts {
		return WindowsClusterFacts{ServerVersion: server, ServerVersionRead: read, Provider: provider, DesiredVersion: desired, ObservedVersion: observed, CNI: cni}
	}
	v215 := func(observed, cni string) WindowsClusterFacts {
		return at("v2.15.3", true, "rke2", "v1.33.4+rke2r1", observed, cni)
	}
	rows := []struct {
		name  string
		facts WindowsClusterFacts
		want  string
	}{
		{"rke2", v215("v1.33.4+rke2r1", "calico"), accepted},
		{"k3s", at("v2.15.3", true, "k3s", "v1.33.4+k3s1", "v1.33.4+k3s1", ""), k3s},
		{"k3s with no version", at("v2.15.3", true, "k3s", "", "", ""), k3s},
		{"below v1.21.0", v215("v1.20.15", ""), older},
		{"v1.21.0", v215("v1.21.0", ""), accepted},
		{"v1.29.1", v215("v1.29.1", ""), accepted},
		{"v1.29.2", v215("v1.29.2", ""), accepted},
		{"no version", v215("", ""), retry},
		{"v1.20.15+rke2r1", v215("v1.20.15+rke2r1", ""), older},
		{"v1.21.0+rke2r1", v215("v1.21.0+rke2r1", ""), accepted},
		{"v1.29.1+rke2r2", v215("v1.29.1+rke2r2", ""), accepted},
		{"v1.29.2+rke2r1", v215("v1.29.2+rke2r1", ""), accepted},
		{"v1.29.2+rke2r2", v215("v1.29.2+rke2r2", ""), accepted},
		{"v1.21.0-rc1+rke2r1", v215("v1.21.0-rc1+rke2r1", ""), noParse},
		{"v1.29.2-rc1+rke2r1", v215("v1.29.2-rc1+rke2r1", ""), noParse},
		{"v1.29.2-rc1+rke2r1 with flannel", v215("v1.29.2-rc1+rke2r1", "flannel"), noParse},
		{"CNI unset", v215("v1.33.4+rke2r1", ""), accepted},
		{"calico", v215("v1.33.4+rke2r1", "calico"), accepted},
		{"flannel", v215("v1.33.4+rke2r1", "flannel"), accepted},
		{"flannel at v1.29.1+rke2r2", v215("v1.29.1+rke2r2", "flannel"), cni214},
		{"flannel at v1.29.2", v215("v1.29.2", "flannel"), accepted},
		{"flannel at v1.29.2+rke2r1", v215("v1.29.2+rke2r1", "flannel"), accepted},
		{"cilium", v215("v1.33.4+rke2r1", "cilium"), cni214},
		{"canal", v215("v1.33.4+rke2r1", "canal"), cni214},
		{"v2.13 calico", at("v2.13.11", true, "rke2", "v1.31.2+rke2r1", "", "calico"), accepted},
		{"v2.13 flannel", at("v2.13.11", true, "rke2", "v1.31.2+rke2r1", "v1.31.2+rke2r1", "flannel"), cni213},
		{"v2.13 cilium", at("v2.13.11", true, "rke2", "v1.31.2+rke2r1", "v1.31.2+rke2r1", "cilium"), cni213},
		{"v2.14 flannel", at("v2.14.7", true, "rke2", "v1.32.1+rke2r1", "", "flannel"), accepted},
		{"v2.14 reads the provisioning spec", at("v2.14.7", true, "rke2", "", "v1.32.1+rke2r1", ""), retry},
		{"v2.15 reads the management status", at("v2.15.3", true, "rke2", "v1.32.1+rke2r1", "", ""), retry},
		{"no version number reads the management status", at("head", true, "rke2", "v1.32.1+rke2r1", "", ""), retry},
		{"no version number flannel", at("head", true, "rke2", "", "v1.33.4+rke2r1", "flannel"), accepted},
		{"a failed version read reads the management status", at("", false, "rke2", "v1.32.1+rke2r1", "", ""), retry},
		{"a failed version read gets the newest CNI rule", at("", false, "rke2", "", "v1.33.4+rke2r1", "flannel"), accepted},
		{"upgrade to v1.29.2 on v2.14", at("v2.14.7", true, "rke2", "v1.29.2+rke2r1", "v1.29.1+rke2r1", "flannel"), accepted},
		{"upgrade to v1.29.2 on v2.15", at("v2.15.3", true, "rke2", "v1.29.2+rke2r1", "v1.29.1+rke2r1", "flannel"), cni214},
		{"upgrade from v1.29.2 on v2.14", at("v2.14.7", true, "rke2", "v1.29.1+rke2r1", "v1.29.2+rke2r1", "flannel"), cni214},
		{"upgrade from v1.29.2 on v2.15", at("v2.15.3", true, "rke2", "v1.29.1+rke2r1", "v1.29.2+rke2r1", "flannel"), accepted},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := CheckWindowsCommand(row.facts)
			if row.want == accepted {
				require.NoError(t, err)
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.True(t, strings.HasPrefix(ce.Message, "--windows: "), ce.Message)
			if row.want == retry {
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Equal(t, "--windows: the Windows node command is available once the cluster reports its Kubernetes version; try again then", ce.Message)
				return
			}
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.False(t, ce.Retryable)
			require.ErrorContains(t, err, row.want)
		})
	}
	ce, ok := errors.AsType[*clierr.Error](WindowsNotReadyError())
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, "--windows: the Windows node command is available once the cluster is connected and has etcd, control plane and worker nodes; try again then", ce.Message)
}

// TestNodeCommandOptionValueRuleIsTheSameWithWindows asserts every candidate
// of a flag the Windows node command takes has the same outcome with Windows, and
// an accepted value is printed unchanged after its Windows option name.
func TestNodeCommandOptionValueRuleIsTheSameWithWindows(t *testing.T) {
	t.Parallel()
	spelling := map[string]string{
		NodeOptionNodeName: "-NodeName", NodeOptionAddress: "-Address", NodeOptionInternalAddress: "-InternalAddress", NodeOptionTaint: "-Taint",
	}
	const base = "./install.ps1 -Token tok -Worker"
	for _, c := range optionCandidates() {
		name, ok := spelling[c.flag]
		if !ok {
			continue
		}
		o := c.opts
		o.Windows = true
		valid, err := ValidateNodeCommandOptions(o)
		if !c.accept {
			require.Error(t, err, c.name)
			assert.True(t, strings.HasPrefix(err.Error(), "--"+c.flag+" "), "%s: %v", c.name, err)
			continue
		}
		require.NoError(t, err, c.name)
		text, result := ComposeNodeCommand(base, valid)
		require.Equal(t, Composed, result, c.name)
		assert.Equal(t, base+" "+name+" '"+c.value+"'", text, c.name)
	}
}
