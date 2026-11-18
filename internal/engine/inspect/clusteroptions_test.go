package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// optionsToken is the registration token of the option fixtures: a value
// that appears nowhere else; a search for it finds only the token.
const optionsToken = "tok-REGOPT-7f3a9c"

// Rancher's node commands of the option fixtures, in the server's shape.
var (
	optionsNodeCommand                = " curl -fL https://rancher.example/system-agent-install.sh | sudo  sh -s - --server https://rancher.example --label 'cattle.io/os=linux' --token " + optionsToken + " --ca-checksum abc123"
	optionsInsecureNodeCommand        = " curl --insecure -fL https://rancher.example/system-agent-install.sh | sudo  sh -s - --server https://rancher.example --label 'cattle.io/os=linux' --token " + optionsToken
	optionsWindowsNodeCommand         = " curl.exe -fL https://rancher.example/wins-agent-install.ps1 -o install.ps1; Set-ExecutionPolicy Bypass -Scope Process -Force; ./install.ps1 -Server https://rancher.example -Label 'cattle.io/os=windows' -Token " + optionsToken + " -Worker -CaChecksum abc123"
	optionsInsecureWindowsNodeCommand = " curl.exe --insecure -fL https://rancher.example/wins-agent-install.ps1 -o install.ps1; Set-ExecutionPolicy Bypass -Scope Process -Force; ./install.ps1 -Server https://rancher.example -Label 'cattle.io/os=windows' -Token " + optionsToken + " -Worker"
)

// optionsCRTBody is the default token of c-abc with Rancher-shaped node
// commands; overrides replace status fields.
func optionsCRTBody(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	status := map[string]any{
		"nodeCommand":                optionsNodeCommand,
		"insecureNodeCommand":        optionsInsecureNodeCommand,
		"windowsNodeCommand":         optionsWindowsNodeCommand,
		"insecureWindowsNodeCommand": optionsInsecureWindowsNodeCommand,
	}
	maps.Copy(status, overrides)
	return crtBodyFixture(t, "c-abc", optionsToken, status)
}

// customProvCluster is the provisioning Cluster of a custom cluster:
// spec.rkeConfig set, no machine pools.
func customProvCluster() *unstructured.Unstructured {
	prov := provClusterFixture("my-custom", "c-abc")
	_ = unstructured.SetNestedField(prov.Object, "v1.33.4+rke2r1", "spec", "kubernetesVersion")
	return prov
}

// optionsFixture is a custom cluster c-abc whose default token has
// Rancher-shaped node commands.
func optionsFixture(t *testing.T, prov *unstructured.Unstructured, crtOverrides map[string]any) (*fakeFactory, *fakeClient, *fakeRoot) {
	t.Helper()
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient()
	if prov != nil {
		client.withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
	}
	root := &fakeRoot{cluster: mgmtClusterFixture("c-abc", "prod", "active", "v1.33.4+rke2r1"), body: optionsCRTBody(t, crtOverrides)}
	return &fakeFactory{client: client}, client, root
}

// optionsIn is an input for cluster c-abc with the given flags; registration
// adds --registration.
func optionsIn(registration bool, b map[string]bool, s map[string]string, ss map[string][]string) input {
	in := input{args: []string{"c-abc"}, b: map[string]bool{"registration": registration}, s: s, ss: ss}
	maps.Copy(in.b, b)
	return in
}

// assertNoRequest asserts the request log of the fakes is empty: no cluster
// resolution, no client, no read of any kind.
func assertNoRequest(t *testing.T, f *fakeFactory, c *fakeClient, r *fakeRoot) {
	t.Helper()
	assert.Empty(t, f.resolveCalls, "no cluster resolution")
	assert.Empty(t, f.clusterCalls, "no cluster client")
	assert.Empty(t, c.getCalls, "no Get")
	assert.Empty(t, c.listGVRs, "no List")
	assert.Empty(t, r.path, "no root read")
	assert.Empty(t, r.clusterPaths, "no management Cluster read")
	assert.Empty(t, r.attrPaths, "no UserAttribute read")
}

// refusalOf asserts err is a *clierr.Error with code whose message is
// want, and returns it.
func refusalOf(t *testing.T, err error, code, want string) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "a structured error: %v", err)
	assert.Equal(t, code, ce.Code)
	assert.Equal(t, want, ce.Message)
	return ce
}

// assertRefusal asserts err is a *clierr.Error with code whose message is
// want.
func assertRefusal(t *testing.T, err error, code, want string) {
	t.Helper()
	_ = refusalOf(t, err, code, want)
}

// TestNodeCommandOptionValueRefusalSendsNoRequest asserts every refused
// typed value is a VALIDATION error naming its flag, before any request, with
// or without --registration.
func TestNodeCommandOptionValueRefusalSendsNoRequest(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		s    map[string]string
		ss   map[string][]string
		want string
	}{
		{"space in a label key", nil, map[string][]string{"label": {"a b=c"}},
			`--label "a b=c": the key has " ", which the Rancher UI would remove; use letters, digits, ".", "/", "_" or "-"`},
		{"curly quote in a node name", map[string]string{"node-name": "a\u2018b"}, nil,
			`--node-name "a\u2018b": the node name has "\u2018", which the Rancher UI would remove; use letters, digits, ".", "_" or "-"`},
		{"wildcard in an address", map[string]string{"address": "10.0.0.*"}, nil,
			`--address "10.0.0.*": an address has "*", which the Rancher UI would remove; use letters, digits, ".", ":", "_" or "-", with "," between addresses`},
		{"empty internal address piece", map[string]string{"internal-address": "a,"}, nil,
			`--internal-address "a,": the list has an empty address; separate addresses with one ","`},
		{"taint effect", nil, map[string][]string{"taint": {"k=v:Evict"}},
			`--taint "k=v:Evict": the effect is "Evict"; use NoSchedule, PreferNoSchedule or NoExecute`},
		{"label key twice", nil, map[string][]string{"label": {"tier=a", "tier=b"}}, `--label tier is given twice`},
		{"label key Rancher sets", nil, map[string][]string{"label": {"cattle.io/os=linux"}}, `--label cattle.io/os: Rancher sets this label on the node itself`},
		{"uppercase node name", map[string]string{"node-name": "Node1"}, nil,
			`--node-name "Node1": this name breaks the Kubernetes rule for a node name; use lowercase letters, digits, "-" and ".", starting and ending with a letter or digit, at most 253 characters`},
	} {
		for _, registration := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s registration %t", row.name, registration), func(t *testing.T) {
				t.Parallel()
				f, c, r := optionsFixture(t, customProvCluster(), nil)
				cmd := NewInspectCluster(f, fakeRoots{rc: r})
				_, err := execRead(t, cmd, optionsIn(registration, nil, row.s, row.ss))
				assertRefusal(t, err, clierr.CodeValidation, row.want)
				assertNoRequest(t, f, c, r)
			})
		}
	}
}

// optionFlagValues is one accepted value for each value-taking option flag;
// a bool flag needs none.
var optionFlagValues = map[string]string{
	provisioning.NodeOptionNodeName:        "node-1",
	provisioning.NodeOptionAddress:         "10.0.0.5",
	provisioning.NodeOptionInternalAddress: "10.1.0.5",
	provisioning.NodeOptionLabel:           "tier=db",
	provisioning.NodeOptionTaint:           "dedicated=db:NoSchedule",
}

// optionFlagsOf returns the descriptor's option flags: every flag but
// --registration.
func optionFlagsOf(t *testing.T) []runtime.ArgSpec {
	t.Helper()
	var out []runtime.ArgSpec
	for _, f := range NewInspectCluster(nil, nil).Describe().Flags {
		if f.Name != "registration" {
			out = append(out, f)
		}
	}
	require.NotEmpty(t, out)
	return out
}

// setFlag adds flag f with its accepted value to in.
func setFlag(t *testing.T, in input, f runtime.ArgSpec) input {
	t.Helper()
	switch f.Type {
	case "bool":
		in.b[f.Name] = true
	case "string":
		v, ok := optionFlagValues[f.Name]
		require.True(t, ok, "option flag --%s has no value row", f.Name)
		if in.s == nil {
			in.s = map[string]string{}
		}
		in.s[f.Name] = v
	case "stringSlice":
		v, ok := optionFlagValues[f.Name]
		require.True(t, ok, "option flag --%s has no value row", f.Name)
		if in.ss == nil {
			in.ss = map[string][]string{}
		}
		in.ss[f.Name] = []string{v}
	default:
		require.Fail(t, "option flag --"+f.Name+" has a type with no row: "+f.Type)
	}
	return in
}

// hintWords is the hint text each option flag adds, with its accepted value.
func hintWords(f runtime.ArgSpec) string {
	if f.Type == "bool" {
		return " --" + f.Name
	}
	return " --" + f.Name + " '" + optionFlagValues[f.Name] + "'"
}

// TestEveryOptionFlagNeedsRegistration asserts no option flag, alone or with
// the others, reads the token or shows a command without --registration: the
// refusal names the first option flag set and carries the command with
// --registration added as its hint, and no request is sent.
func TestEveryOptionFlagNeedsRegistration(t *testing.T) {
	t.Parallel()
	const reason = " needs --registration: it adds options to the node command, which contains the cluster registration token"
	flags := optionFlagsOf(t)
	run := func(t *testing.T, in input) (*clierr.Error, runtime.Result) {
		t.Helper()
		f, c, r := optionsFixture(t, customProvCluster(), nil)
		cmd := NewInspectCluster(f, fakeRoots{rc: r})
		res, err := execRead(t, cmd, in)
		require.Error(t, err)
		assertNoRequest(t, f, c, r)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		b, jerr := json.Marshal(ce)
		require.NoError(t, jerr)
		assert.NotContains(t, string(b), optionsToken)
		assert.Nil(t, res.Data, "no data")
		assert.Empty(t, res.Text, "no text")
		return ce, res
	}
	for _, flag := range flags {
		t.Run(flag.Name, func(t *testing.T) {
			t.Parallel()
			ce, _ := run(t, setFlag(t, optionsIn(false, nil, nil, nil), flag))
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "--"+flag.Name+reason, ce.Message)
			assert.Equal(t, clierr.Rancher+" inspect cluster 'c-abc' --registration"+hintWords(flag), ce.Hint)
		})
	}
	t.Run("every Linux option flag", func(t *testing.T) {
		t.Parallel()
		in := optionsIn(false, nil, nil, nil)
		want := clierr.Rancher + " inspect cluster 'c-abc' --registration"
		for _, flag := range flags {
			if flag.Name == provisioning.NodeOptionWindows {
				continue
			}
			in = setFlag(t, in, flag)
			want += hintWords(flag)
		}
		ce, _ := run(t, in)
		assert.Equal(t, "--etcd"+reason, ce.Message)
		assert.Equal(t, want, ce.Hint)
	})
	t.Run("every option flag the Windows node command takes", func(t *testing.T) {
		t.Parallel()
		in := optionsIn(false, map[string]bool{"windows": true}, map[string]string{"node-name": "win1", "address": "10.0.0.5", "internal-address": "10.1.0.5"},
			map[string][]string{"taint": {"os=windows:NoSchedule"}})
		ce, _ := run(t, in)
		assert.Equal(t, "--node-name"+reason, ce.Message)
		assert.Equal(t, clierr.Rancher+" inspect cluster 'c-abc' --registration --node-name 'win1' --address '10.0.0.5' --internal-address '10.1.0.5' --taint 'os=windows:NoSchedule' --windows", ce.Hint)
	})
	t.Run("the first option flag in the flag order is named", func(t *testing.T) {
		t.Parallel()
		ce, _ := run(t, optionsIn(false, map[string]bool{"worker": true}, nil, map[string][]string{"label": {"a=b"}}))
		assert.Equal(t, "--worker"+reason, ce.Message)
		assert.Equal(t, clierr.Rancher+" inspect cluster 'c-abc' --registration --worker --label 'a=b'", ce.Hint)
	})
	t.Run("labels and taints in typed order", func(t *testing.T) {
		t.Parallel()
		ce, _ := run(t, optionsIn(false, nil, nil, map[string][]string{"label": {"z=1", "a=2"}, "taint": {"k=v:NoExecute"}}))
		assert.Equal(t, clierr.Rancher+" inspect cluster 'c-abc' --registration --label 'z=1' --label 'a=2' --taint 'k=v:NoExecute'", ce.Hint)
	})
	t.Run("a cluster argument with a hidden character leaves the hint out", func(t *testing.T) {
		t.Parallel()
		in := optionsIn(false, map[string]bool{"worker": true}, nil, nil)
		in.args = []string{"c'abc"}
		f, c, r := optionsFixture(t, customProvCluster(), nil)
		_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), in)
		ce := refusalOf(t, err, clierr.CodeValidation, "--worker"+reason+"; "+clierr.LeftOutNote)
		assert.Empty(t, ce.Hint)
		assertNoRequest(t, f, c, r)
	})
}

// registrationOf decodes the registration section of a result, nil when it
// is absent.
func registrationOf(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	doc, _ := marshaledDoc(t, res.Data)
	reg, _ := doc["registration"].(map[string]any)
	return reg
}

// TestNodeCommandOptionsComposeUnderWithOptions asserts the composed
// commands are set only on the --registration path with an option flag, under
// registration.withOptions, from the token's node commands; a node command of
// the token that is left out leaves its field out with a warning, withOptions
// is absent when it has no field, never {}, and the token's node command
// fields are unchanged.
func TestNodeCommandOptionsComposeUnderWithOptions(t *testing.T) {
	t.Parallel()
	worker := map[string]bool{"worker": true}
	t.Run("both Linux commands", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), nil)
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, worker, nil, map[string][]string{"label": {"tier=db"}}))
		require.NoError(t, err)
		assert.Empty(t, res.Warnings)
		reg := registrationOf(t, res)
		assert.Equal(t, optionsNodeCommand, reg["nodeCommand"], "the node command field is unchanged")
		assert.Equal(t, optionsInsecureNodeCommand, reg["insecureNodeCommand"])
		assert.Equal(t, map[string]any{
			"nodeCommand":         optionsNodeCommand + " --worker --label 'tier=db'",
			"insecureNodeCommand": optionsInsecureNodeCommand + " --worker --label 'tier=db'",
		}, reg["withOptions"])
		assert.Contains(t, res.Text, "  node command with options:\n    "+optionsNodeCommand+" --worker --label 'tier=db'\n")
		assert.Contains(t, res.Text, "  node command with options (insecure):\n    ")
		assert.NotContains(t, res.Text, "Windows node command")
		doc, _ := marshaledDoc(t, res.Data)
		assert.NotContains(t, doc, "withOptions", "no top-level key is added")
	})
	t.Run("one node command left out", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), map[string]any{"insecureNodeCommand": "curl --insecure -fL x | sudo sh -s - --token {token}"})
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, worker, nil, nil))
		require.NoError(t, err)
		assert.Equal(t, []string{
			"registration token c-abc/default-token: insecureNodeCommand not formatted by the server; omitted",
			"registration token c-abc/default-token: insecureNodeCommand not formatted by the server; the node command with options is left out",
		}, res.Warnings)
		reg := registrationOf(t, res)
		assert.Equal(t, map[string]any{"nodeCommand": optionsNodeCommand + " --worker"}, reg["withOptions"])
	})
	t.Run("every node command left out", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), map[string]any{"nodeCommand": "{token}", "insecureNodeCommand": ""})
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, worker, nil, nil))
		require.NoError(t, err)
		assert.Equal(t, []string{
			"registration token c-abc/default-token: nodeCommand not formatted by the server; omitted",
			"registration token c-abc/default-token: nodeCommand not formatted by the server; the node command with options is left out",
			"registration token c-abc/default-token: insecureNodeCommand not formatted by the server; the node command with options is left out",
		}, res.Warnings)
		reg := registrationOf(t, res)
		require.NotNil(t, reg, "the import commands keep the section")
		assert.NotContains(t, reg, "withOptions", "withOptions is absent, never {}")
		assert.NotContains(t, res.Text, "with options")
	})
	t.Run("every field empty", func(t *testing.T) {
		t.Parallel()
		empty := map[string]any{"nodeCommand": "", "insecureNodeCommand": "", "command": "", "insecureCommand": "", "manifestUrl": ""}
		f, _, r := optionsFixture(t, customProvCluster(), empty)
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, worker, nil, nil))
		require.NoError(t, err)
		assert.Nil(t, registrationOf(t, res), "the section is absent when no field is left")
		assert.Contains(t, res.Text, "registration: unavailable")
	})
	t.Run("an unavailable provisioning lookup still composes the commands and warns that it cannot determine whether they apply", func(t *testing.T) {
		t.Parallel()
		provKind := mustKind(t, "provisioning-cluster")
		f, c, r := optionsFixture(t, nil, nil)
		c.withListErr(provKind.GVR, errors.New("clusters.provisioning.cattle.io is forbidden"))
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, worker, nil, nil))
		require.NoError(t, err)
		reg := registrationOf(t, res)
		composed, _ := reg["withOptions"].(map[string]any)
		assert.Equal(t, optionsNodeCommand+" --worker", composed["nodeCommand"])
		assert.Contains(t, res.Warnings, "cannot determine whether the node commands apply: the provisioning.cattle.io Cluster for this cluster is unavailable")
	})
}

// TestNoOptionFlagKeepsTheOutput asserts that with no option flag the
// registration section and the REGISTRATION text show the five fields (a
// placeholder field is left out and warned about), notApplicable on an
// imported cluster, and the message and hint without --registration, and have
// no withOptions key and no warning about the Windows fields. The fixtures are
// those of the registration tests.
func TestNoOptionFlagKeepsTheOutput(t *testing.T) {
	t.Parallel()
	provKind := mustKind(t, "provisioning-cluster")
	imported := provClusterFixture("imp", "c-abc")
	unstructured.RemoveNestedField(imported.Object, "spec", "rkeConfig")
	importURL := "https://rancher.example/v3/import/" + optionsToken + ".yaml"
	full := func(drop ...string) map[string]any {
		m := map[string]any{
			"nodeCommand":         optionsNodeCommand,
			"insecureNodeCommand": optionsInsecureNodeCommand,
			"command":             "curl -sfL " + importURL + " | kubectl apply -f -",
			"insecureCommand":     "curl --insecure -sfL " + importURL + " | kubectl apply -f -",
			"manifestUrl":         importURL,
		}
		for _, d := range drop {
			delete(m, d)
		}
		return m
	}
	withNotApplicable := full()
	withNotApplicable["notApplicable"] = []any{"nodeCommand", "insecureNodeCommand"}
	withheld := map[string]any{
		"withheld": true,
		"message":  "registration was left out because it contains the cluster registration token; rerun with --registration to include it",
		"hint":     clierr.Rancher + " get clusterregistrationtoken default-token --namespace 'c-abc'",
	}
	omitted := func(field string) []string {
		return []string{"registration token c-abc/default-token: " + field + " not formatted by the server; omitted"}
	}
	for _, row := range []struct {
		name      string
		prov      *unstructured.Unstructured
		listErr   bool
		overrides map[string]any
		in        input
		want      map[string]any
		warnings  []string
	}{
		{"custom", customProvCluster(), false, nil, registrationInput("c-abc"), full(), nil},
		{"imported", imported, false, nil, registrationInput("c-abc"), withNotApplicable, nil},
		{"provisioning unavailable", nil, true, nil, registrationInput("c-abc"), full(), []string{
			"cannot determine whether the node commands apply: the provisioning.cattle.io Cluster for this cluster is unavailable",
			"provisioning unavailable: forbidden",
		}},
		{"placeholder in nodeCommand", customProvCluster(), false, map[string]any{"nodeCommand": "{token}"}, registrationInput("c-abc"), full("nodeCommand"), omitted("nodeCommand")},
		{"placeholder in insecureNodeCommand", customProvCluster(), false, map[string]any{"insecureNodeCommand": "{token}"}, registrationInput("c-abc"), full("insecureNodeCommand"), omitted("insecureNodeCommand")},
		{"placeholder in command", customProvCluster(), false, map[string]any{"command": "{token}"}, registrationInput("c-abc"), full("command"), omitted("command")},
		{"placeholder in insecureCommand", customProvCluster(), false, map[string]any{"insecureCommand": "{token}"}, registrationInput("c-abc"), full("insecureCommand"), omitted("insecureCommand")},
		{"placeholder in manifestUrl", customProvCluster(), false, map[string]any{"manifestUrl": "{token}"}, registrationInput("c-abc"), full("manifestUrl"), omitted("manifestUrl")},
		{"placeholder in windowsNodeCommand", customProvCluster(), false, map[string]any{"windowsNodeCommand": "{token}"}, registrationInput("c-abc"), full(), nil},
		{"placeholder in insecureWindowsNodeCommand", customProvCluster(), false, map[string]any{"insecureWindowsNodeCommand": "{token}"}, registrationInput("c-abc"), full(), nil},
		{"withheld", customProvCluster(), false, nil, input{args: []string{"c-abc"}}, withheld, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f, c, r := optionsFixture(t, row.prov, row.overrides)
			if row.listErr {
				c.withListErr(provKind.GVR, errors.New("forbidden"))
			}
			res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), row.in)
			require.NoError(t, err)
			assert.Equal(t, row.want, registrationOf(t, res))
			assert.Equal(t, row.warnings, res.Warnings)
			_, section, found := strings.Cut(res.Text, "\nREGISTRATION\n")
			require.True(t, found)
			assert.Equal(t, registrationText(row.want), section)
		})
	}
	t.Run("every surfaced field empty", func(t *testing.T) {
		t.Parallel()
		empty := map[string]any{"nodeCommand": "", "insecureNodeCommand": "", "command": "", "insecureCommand": "", "manifestUrl": ""}
		f, _, r := optionsFixture(t, customProvCluster(), empty)
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), registrationInput("c-abc"))
		require.NoError(t, err)
		assert.Nil(t, registrationOf(t, res), "the Windows fields do not count without an option flag")
		assert.Empty(t, res.Warnings)
	})
}

// registrationText is the REGISTRATION section of the text output for a
// registration section with no withOptions: a field per line pair in the
// fixed order, an imported cluster's node commands marked.
func registrationText(reg map[string]any) string {
	if reg["withheld"] == true {
		return "  registration: left out (contains the cluster registration token) - rerun with --registration\n"
	}
	marked := reg["notApplicable"] != nil
	var b strings.Builder
	for _, f := range []struct{ key, label string }{
		{"nodeCommand", "node command"}, {"insecureNodeCommand", "node command (insecure)"},
		{"command", "import command"}, {"insecureCommand", "import command (insecure)"}, {"manifestUrl", "manifestUrl"},
	} {
		v, ok := reg[f.key].(string)
		if !ok {
			continue
		}
		label := f.label
		if marked && (f.key == "nodeCommand" || f.key == "insecureNodeCommand") {
			label += " [not applicable: imported cluster]"
		}
		b.WriteString("  " + label + ":\n    " + v + "\n")
	}
	return b.String()
}

// TestNodeCommandOptionsApplyOnlyToACustomCluster asserts an option flag on a
// cluster that is not custom is a VALIDATION error naming the first option
// flag set and the cluster id, before the token read.
func TestNodeCommandOptionsApplyOnlyToACustomCluster(t *testing.T) {
	t.Parallel()
	imported := provClusterFixture("imp", "c-abc")
	unstructured.RemoveNestedField(imported.Object, "spec", "rkeConfig")
	pooled := provClusterFixture("pooled", "c-abc")
	_ = unstructured.SetNestedSlice(pooled.Object, []any{map[string]any{"name": "pool1", "quantity": int64(1), "workerRole": true}}, "spec", "rkeConfig", "machinePools")
	const prefix = ": node command options apply only to a custom cluster; c-abc "
	for _, row := range []struct {
		name string
		prov *unstructured.Unstructured
		in   input
		want string
	}{
		{"imported", imported, optionsIn(true, map[string]bool{"etcd": true}, nil, nil), "--etcd" + prefix + "is imported"},
		{"machine pools", pooled, optionsIn(true, nil, map[string]string{"node-name": "n1"}, nil), "--node-name" + prefix + "has machine pools"},
		{"no provisioning Cluster", nil, optionsIn(true, nil, nil, map[string][]string{"taint": {"k=v:NoSchedule"}}), "--taint" + prefix + "has no provisioning.cattle.io Cluster"},
		{"the first option flag set", imported, optionsIn(true, map[string]bool{"worker": true}, nil, map[string][]string{"label": {"a=b"}}), "--worker" + prefix + "is imported"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f, _, r := optionsFixture(t, row.prov, nil)
			res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), row.in)
			assertRefusal(t, err, clierr.CodeValidation, row.want)
			assert.NotContains(t, r.path, "default-token", "the token is not read")
			assert.Empty(t, res.Warnings)
		})
	}
}

// TestOptionFlagTokenReadFailureFailsTheCommand asserts a failed token read
// with an option flag fails the command with the read's own code, its message
// naming the first option flag set; a canceled context returns as itself;
// without an option flag the read still degrades.
func TestOptionFlagTokenReadFailureFailsTheCommand(t *testing.T) {
	t.Parallel()
	const prefix = "--label: cannot read the cluster registration token: "
	labels := map[string][]string{"label": {"tier=db"}}
	t.Run("a structured error keeps its code", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), nil)
		r.err = clierr.New(clierr.CodeNotFound, "default-token not found")
		_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, nil, nil, labels))
		assertRefusal(t, err, clierr.CodeNotFound, prefix+"default-token not found")
	})
	t.Run("another error is GENERIC with it as the cause", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("connection reset")
		f, _, r := optionsFixture(t, customProvCluster(), nil)
		r.err = cause
		_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, nil, nil, labels))
		assertRefusal(t, err, clierr.CodeGeneric, prefix+"connection reset")
		require.ErrorIs(t, err, cause)
	})
	t.Run("a root factory failure", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), nil)
		roots := &rootsFailingAfter{rc: r, ok: 1, err: errors.New("no server configured")}
		_, err := execRead(t, NewInspectCluster(f, roots), optionsIn(true, map[string]bool{"worker": true}, nil, nil))
		assertRefusal(t, err, clierr.CodeGeneric, "--worker: cannot read the cluster registration token: no server configured")
	})
	t.Run("a canceled context returns as itself", func(t *testing.T) {
		t.Parallel()
		for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
			f, _, r := optionsFixture(t, customProvCluster(), nil)
			r.err = fmt.Errorf("fetch default-token: %w", ctxErr)
			res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, nil, nil, labels))
			require.ErrorIs(t, err, ctxErr)
			_, structured := errors.AsType[*clierr.Error](err)
			assert.False(t, structured, "the cancellation is not wrapped")
			assert.Empty(t, res.Warnings)
		}
	})
	t.Run("without an option flag the read degrades", func(t *testing.T) {
		t.Parallel()
		f, _, r := optionsFixture(t, customProvCluster(), nil)
		r.err = errors.New("connection reset")
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), registrationInput("c-abc"))
		require.NoError(t, err)
		assert.Nil(t, registrationOf(t, res))
		doc, _ := marshaledDoc(t, res.Data)
		assert.Equal(t, []any{map[string]any{"section": "registration", "state": "unavailable", "code": "GENERIC"}}, doc["degraded"])
	})
}

// TestAmbiguousProvisioningMatchCarriesCandidates asserts several
// provisioning Clusters that report the cluster's id give an
// AMBIGUOUS_SELECTION error listing each as <namespace>/<name>, sorted, while
// the degraded entry and its warning stay as they were.
func TestAmbiguousProvisioningMatchCarriesCandidates(t *testing.T) {
	t.Parallel()
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*provClusterFixture("prov-c", "c-abc"), *provClusterFixture("prov-a", "c-abc"), *provClusterFixture("other", "c-xyz"), *provClusterFixture("prov-b", "c-abc"),
	}})
	sec := newSections(t.Context(), sectionProvisioning, sectionMachines, sectionMembers, sectionRegistration)
	prov, lookup, note, cancelErr, lookupErr := correlateProvisioningCluster(t.Context(), client, sec, "c-abc")
	require.NoError(t, cancelErr)
	assert.Nil(t, prov)
	assert.Equal(t, provUnavailable, lookup)
	assert.Empty(t, note)
	ce := refusalOf(t, lookupErr, clierr.CodeAmbiguousSelection,
		`3 provisioning.cattle.io Clusters report status.clusterName "c-abc"; cannot tell which one belongs to this cluster`)
	assert.Equal(t, []clierr.Candidate{
		{ID: "fleet-default/prov-a", Name: "fleet-default/prov-a"},
		{ID: "fleet-default/prov-b", Name: "fleet-default/prov-b"},
		{ID: "fleet-default/prov-c", Name: "fleet-default/prov-c"},
	}, ce.Candidates)
	entries, warnings := sec.Finalize()
	assert.Equal(t, []sectionOutcome{{Section: sectionProvisioning, State: stateUnavailable, Code: clierr.CodeAmbiguousSelection}}, entries)
	assert.Equal(t, []string{`provisioning unavailable: 3 provisioning.cattle.io Clusters report status.clusterName "c-abc"; cannot tell which one belongs to this cluster`}, warnings)

	f := &fakeFactory{client: client}
	r := &fakeRoot{cluster: mgmtClusterFixture("c-abc", "prod", "active", ""), body: optionsCRTBody(t, nil)}
	res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), optionsIn(true, map[string]bool{"worker": true}, nil, nil))
	require.NoError(t, err, "Linux options compose when the lookup is unavailable")
	assert.Contains(t, registrationOf(t, res), "withOptions")
}

var _ engine.ClusterClientFactory = (*fakeFactory)(nil)

// TestBooleanOptionTypedFalseIsRefused asserts every boolean node command
// option typed as false is refused before any request, with or without
// --registration: the node command gets only the options typed.
func TestBooleanOptionTypedFalseIsRefused(t *testing.T) {
	t.Parallel()
	bools := map[string]bool{}
	for _, f := range NewInspectCluster(nil, nil).Describe().Flags {
		if f.Type == "bool" {
			bools[f.Name] = true
		}
	}
	var checked []string
	for _, flag := range provisioning.NodeOptionFlags {
		if !bools[flag] {
			continue
		}
		checked = append(checked, flag)
		for _, registration := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s registration %t", flag, registration), func(t *testing.T) {
				t.Parallel()
				f, c, r := optionsFixture(t, customProvCluster(), nil)
				in := optionsIn(registration, map[string]bool{flag: false}, nil, map[string][]string{"label": {"tier=db"}})
				in.set = map[string]bool{flag: true}
				_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), in)
				want := "--" + flag + "=false: give only the flags for the roles the node takes (--etcd, --controlplane, --worker); with no role flag it takes all three"
				if flag == provisioning.NodeOptionWindows {
					want = "--windows=false: remove --windows"
				}
				ce := refusalOf(t, err, clierr.CodeValidation, want)
				assert.Equal(t, 2, ce.ExitCode())
				assertNoRequest(t, f, c, r)
			})
		}
	}
	assert.ElementsMatch(t, []string{"etcd", "controlplane", "worker", "windows"}, checked)
}

// TestLeftOutWarningNamesWhyTheCommandIsLeftOut asserts the warning for a
// composed command left out: the node command not formatted by the server, or a value
// no supported shell reads as written, which gets the left-out note.
func TestLeftOutWarningNamesWhyTheCommandIsLeftOut(t *testing.T) {
	t.Parallel()
	assert.Empty(t, appendLeftOut(nil, "c-abc", "nodeCommand", provisioning.Composed))
	assert.Equal(t, []string{"registration token c-abc/default-token: nodeCommand not formatted by the server; the node command with options is left out"},
		appendLeftOut(nil, "c-abc", "nodeCommand", provisioning.BaseNotFormatted))
	assert.Equal(t, []string{"registration token c-abc/default-token: windowsNodeCommand with options; " + clierr.LeftOutNote},
		appendLeftOut(nil, "c-abc", "windowsNodeCommand", provisioning.ValueNotQuotable))
}
