package inspect

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

// hostileValue is the value planted at the path p: the path in angle brackets,
// then the hostile tail.
func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// plainValue is the value planted at a path whose rule keeps a newline out of
// it: the path in angle brackets alone.
func plainValue(p string) string { return "<" + p + ">" }

// textClasses classifies every text path of a result type: the paths whose
// values the builder filters ("printed"), the paths whose values hold no
// newline by rule, the paths the builder does not print, and the paths a test
// sets by hand. Each entry of a map says why in a sentence.
type textClasses struct {
	printed    []string
	rawByRule  map[string]string
	notPrinted map[string]string
	setByHand  map[string]string
}

// assertComplete asserts that every reason is non-empty and that the four
// classes together are exactly the text paths of typ.
func (c textClasses) assertComplete(t *testing.T, typ reflect.Type) {
	t.Helper()
	union := slices.Clone(c.printed)
	for _, m := range []map[string]string{c.rawByRule, c.notPrinted, c.setByHand} {
		for path, reason := range m {
			assert.NotEmpty(t, reason, "the reason for %s", path)
			union = append(union, path)
		}
	}
	assert.ElementsMatch(t, remedytest.TextPaths(typ), union, "every text field of %s is classified exactly once", typ)
}

// value is the planted text for the path p.
func (c textClasses) value(p string) string {
	if _, ok := c.rawByRule[p]; ok {
		return plainValue(p)
	}
	return hostileValue(p)
}

// assertLineRule asserts that out holds the angle-bracketed path of every
// entry of present, holds nothing the writer filter removes, and has no line
// that starts with the forged line after its leading spaces.
func assertLineRule(t *testing.T, out string, present []string) {
	t.Helper()
	for _, p := range present {
		assert.Contains(t, out, "<"+p+">", "the text prints %s", p)
	}
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

func assertAbsent(t *testing.T, out string, absent map[string]string) {
	t.Helper()
	for p := range absent {
		assert.NotContains(t, out, "<"+p+">", "the text does not print %s", p)
	}
}

func without(paths []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return slices.Contains(drop, p) })
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestClusterTextKeepsEveryServerValueOnItsLine(t *testing.T) {
	t.Parallel()
	classes := textClasses{
		printed: []string{
			"Cluster.Name", "Cluster.State", "Cluster.KubernetesVersion",
			"Agent.Message",
			"Status.Conditions[].Type", "Status.Conditions[].Status", "Status.Conditions[].Reason", "Status.Conditions[].Message",
			"MachinePools[].Name", "MachinePools[].MachineConfigKind", "MachinePools[].MachineConfigName",
			"Members[].Principal", "Members[].DisplayName", "Members[].Role",
			"Machines[].Name", "Machines[].Phase", "Machines[].Error",
			"Registration.NodeCommand", "Registration.InsecureNodeCommand", "Registration.Command",
			"Registration.InsecureCommand", "Registration.ManifestURL",
		},
		rawByRule: map[string]string{
			"Cluster.ID":             "a Kubernetes object name, which holds no newline",
			"ProvisioningNote":       "text the CLI writes itself",
			"MachinePools[].Roles[]": "role names the CLI writes itself",
		},
		notPrinted: map[string]string{
			"Registration.NotApplicable[]": "compared with two field names and never printed",
			"Registration.Message":         "carried in json and yaml only",
			"Registration.Hint":            "carried in json and yaml only",
			"Members[].Binding":            "the member line prints the display name, principal and role",
			"Degraded[].Section":           "carried in json and yaml only; the causes print as warnings",
			"Degraded[].State":             "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Code":              "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Target":            "carried in json and yaml only; the causes print as warnings",
		},
	}
	typ := reflect.TypeFor[clusterResult]()
	classes.assertComplete(t, typ)

	planted := remedytest.PlantText(t, typ, classes.value)
	full, ok := reflect.TypeAssert[clusterResult](planted)
	require.True(t, ok)

	textA := renderClusterText(full)
	assertLineRule(t, textA, without(append(slices.Clone(classes.printed), keys(classes.rawByRule)...), "ProvisioningNote"))
	assertAbsent(t, textA, classes.notPrinted)

	noSections := full
	noSections.Status, noSections.MachinePools, noSections.Machines = nil, nil, nil
	textB := renderClusterText(noSections)
	assert.Equal(t, 3, strings.Count(textB, "<ProvisioningNote>"), "the three sections without data print the note")
	assertLineRule(t, textB, []string{"ProvisioningNote"})
	assertAbsent(t, textB, classes.notPrinted)
}

func TestProjectTextKeepsEveryServerValueOnItsLine(t *testing.T) {
	t.Parallel()
	classes := textClasses{
		printed: []string{
			"DisplayName", "ResourceQuota{key}", "ResourceQuota{}",
			"Members[].Principal", "Members[].DisplayName", "Members[].Role",
			"Usage.CPURequest", "Usage.CPULimit", "Usage.CPUUsage",
			"Usage.MemoryRequest", "Usage.MemoryLimit", "Usage.MemoryUsage",
		},
		rawByRule: map[string]string{
			"ID":                "a Kubernetes object name, which holds no newline",
			"Cluster":           "a Kubernetes object name, which holds no newline",
			"Namespaces[].Name": "a Kubernetes object name, which holds no newline",
		},
		notPrinted: map[string]string{
			"Members[].Binding":  "the member line prints the display name, principal and role",
			"Degraded[].Section": "carried in json and yaml only; the causes print as warnings",
			"Degraded[].State":   "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Code":    "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Target":  "carried in json and yaml only; the causes print as warnings",
		},
	}
	typ := reflect.TypeFor[projectResult]()
	classes.assertComplete(t, typ)

	planted := remedytest.PlantText(t, typ, classes.value)
	r, ok := reflect.TypeAssert[projectResult](planted)
	require.True(t, ok)

	text := renderProjectText(r)
	assertLineRule(t, text, append(slices.Clone(classes.printed), keys(classes.rawByRule)...))
	assertAbsent(t, text, classes.notPrinted)
}

func TestUserTextKeepsEveryServerValueOnItsLine(t *testing.T) {
	t.Parallel()
	classes := textClasses{
		printed: []string{
			"User.Username", "User.DisplayName", "User.Principals[]", "User.Created",
			"User.Groups[].Principal", "User.Groups[].DisplayName", "User.Groups[].Provider",
			"User.LastLogin", "User.LastRefresh", "User.DisableAfter", "User.DeleteAfter",
			"User.Extra{key}", "User.Extra{}{key}", "User.Extra{}{}[]",
			"Roles.Global[].Principal", "Roles.Global[].Role", "Roles.Global[].Binding",
			"Roles.Clusters[].Cluster", "Roles.Clusters[].DisplayName",
			"Roles.Clusters[].Roles[].Principal", "Roles.Clusters[].Roles[].Role", "Roles.Clusters[].Roles[].Binding",
			"Roles.Projects[].Project", "Roles.Projects[].DisplayName",
			"Roles.Projects[].Roles[].Principal", "Roles.Projects[].Roles[].Role", "Roles.Projects[].Roles[].Binding",
		},
		rawByRule: map[string]string{
			"User.ID": "a Kubernetes object name, which holds no newline",
		},
		notPrinted: map[string]string{
			"Degraded[].Section":                   "carried in json and yaml only; the causes print as warnings",
			"Degraded[].State":                     "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Code":                      "carried in json and yaml only; the causes print as warnings",
			"Degraded[].Target":                    "carried in json and yaml only; the causes print as warnings",
			"Roles.Global[].DisplayName":           "a role line prints the role, the principal and the binding",
			"Roles.Clusters[].Roles[].DisplayName": "a role line prints the role, the principal and the binding",
			"Roles.Projects[].Roles[].DisplayName": "a role line prints the role, the principal and the binding",
		},
	}
	typ := reflect.TypeFor[userResult]()
	classes.assertComplete(t, typ)

	planted := remedytest.PlantText(t, typ, classes.value)
	r, ok := reflect.TypeAssert[userResult](planted)
	require.True(t, ok)

	text := renderUserText(r, map[string]bool{})
	assertLineRule(t, text, append(slices.Clone(classes.printed), keys(classes.rawByRule)...))
	assertAbsent(t, text, classes.notPrinted)
}

func TestConditionMessageIsFilteredBeforeItIsCut(t *testing.T) {
	t.Parallel()
	r := clusterResult{
		Cluster: clusterIdentity{ID: "c-x"},
		Status: &clusterStatus{Conditions: []clusterCondition{
			{Type: "Ready", Status: "False", Message: strings.Repeat("\x1b", 200) + "visible tail"},
		}},
	}
	text := renderClusterText(r)
	assert.Contains(t, text, "  Ready=False visible tail\n", "the escape characters are removed before the message is cut at 120 characters")
	assertLineRule(t, text, nil)
}

func TestProjectTextKeepsANestedQuotaKeyOnItsLine(t *testing.T) {
	t.Parallel()
	r := projectResult{
		ID:         "p-x",
		Namespaces: []projectNamespace{},
		ResourceQuota: map[string]any{
			hostileValue("outer"): map[string]any{hostileValue("inner"): "5"},
		},
	}
	text := renderProjectText(r)
	assertLineRule(t, text, []string{"outer", "inner"})
}

func TestDeploymentTextKeepsThePodPhaseOnItsLine(t *testing.T) {
	t.Parallel()
	pod := map[string]any{
		"metadata": map[string]any{"name": "web-abc"},
		"status":   map[string]any{"phase": hostileValue("phase")},
	}
	res := renderDeploymentText("web", "default", "c-x", map[string]any{}, []map[string]any{pod}, nil)
	assertLineRule(t, res.Text, []string{"phase"})
	assert.Contains(t, res.Text, "pod web-abc: <phase>")
}

func TestAServerValueOfOnlyRemovedCharactersPrintsAsEmpty(t *testing.T) {
	t.Parallel()
	r := clusterResult{
		Cluster: clusterIdentity{ID: "c-x"},
		Status: &clusterStatus{Conditions: []clusterCondition{
			{Type: "Ready", Status: "False", Reason: "\u202e", Message: "\x1b\r\n"},
		}},
		Registration: &registration{NodeCommand: "\x1b\n"},
	}
	text := renderClusterText(r)
	assert.Contains(t, text, "  Ready=False reason= \n", "the empty reason and the empty message leave a trailing space")
	assert.Contains(t, text, "  node command:\n    \n", "the label is followed by an empty indented line")
	assertLineRule(t, text, nil)
}

func TestInspectClusterJSONKeepsServerValuesAsTheyAre(t *testing.T) {
	t.Parallel()
	name, version, message := hostileValue("name"), hostileValue("version"), hostileValue("message")
	provKind := mustKind(t, "provisioning-cluster")
	prov := provClusterFixture("my-cluster", "c-abc")
	require.NoError(t, unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{"type": "Provisioned", "status": "False", "reason": "Waiting", "message": message},
	}, "status", "conditions"))
	client := newFakeClient().
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
	root := &fakeRoot{cluster: mgmtClusterFixture("c-abc", name, "active", version), body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}

	res, err := execRead(t, NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root}), registrationInput("c-abc"))
	require.NoError(t, err)

	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var doc struct {
		Cluster struct {
			Name              string `json:"name"`
			KubernetesVersion string `json:"kubernetesVersion"`
		} `json:"cluster"`
		Status struct {
			Conditions []struct {
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, name, doc.Cluster.Name, "json carries the display name as it is")
	assert.Equal(t, version, doc.Cluster.KubernetesVersion, "json carries the version as it is")
	require.NotEmpty(t, doc.Status.Conditions)
	assert.Equal(t, message, doc.Status.Conditions[0].Message, "json carries the condition message as it is")
}
