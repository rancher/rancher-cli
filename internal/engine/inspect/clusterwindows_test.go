package inspect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// windowsCluster is a management Cluster c-abc as the Windows rule reads it:
// status.provider, status.info.kubernetesVersion and the conditions given as
// type=status pairs.
func windowsCluster(provider, version string, conditions ...string) *unstructured.Unstructured {
	obj := mgmtClusterFixture("c-abc", "prod", "active", version)
	_ = unstructured.SetNestedField(obj.Object, provider, "status", "provider")
	if version != "" {
		_ = unstructured.SetNestedField(obj.Object, version, "status", "info", "kubernetesVersion")
	}
	var conds []any
	for _, c := range conditions {
		typ, status, _ := strings.Cut(c, "=")
		conds = append(conds, map[string]any{"type": typ, "status": status})
	}
	if conds != nil {
		_ = unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions")
	}
	return obj
}

// managementNode is a management.cattle.io Node with the three role fields.
func managementNode(name string, etcd, controlPlane, worker bool) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "c-abc"},
		"spec":     map[string]any{"etcd": etcd, "controlPlane": controlPlane, "worker": worker},
	}}
}

// windowsCase is one --windows run: the cluster's reads and the outcome.
type windowsCase struct {
	cluster     *unstructured.Unstructured
	prov        *unstructured.Unstructured
	nodes       []unstructured.Unstructured
	nodesErr    error
	server      string
	serverErr   error
	tokenErr    error
	in          input
	provListErr error
}

// readyNodes are Linux nodes with every role, spread over three nodes.
var readyNodes = []unstructured.Unstructured{
	managementNode("n1", true, false, false), managementNode("n2", false, true, false), managementNode("n3", false, false, true),
}

// windowsProv is the provisioning Cluster of a custom cluster with cni as its
// spec.rkeConfig.machineGlobalConfig.cni.
func windowsProv(cni string) *unstructured.Unstructured {
	prov := customProvCluster()
	if cni != "" {
		_ = unstructured.SetNestedField(prov.Object, cni, "spec", "rkeConfig", "machineGlobalConfig", "cni")
	}
	return prov
}

// readyWindowsCase is a cluster every check accepts, on a v2.15 server.
func readyWindowsCase() windowsCase {
	return windowsCase{
		cluster: windowsCluster("rke2", "v1.33.4+rke2r1", "Connected=True", "Ready=True"),
		prov:    windowsProv(""),
		nodes:   readyNodes,
		server:  "v2.15.3",
		in:      optionsIn(true, map[string]bool{"windows": true}, map[string]string{"node-name": "win1"}, nil),
	}
}

// run executes the case and returns the result, the fakes and the error.
func (wc windowsCase) run(t *testing.T) (runtime.Result, *fakeFactory, *fakeRoot, error) {
	t.Helper()
	f, c, r := optionsFixture(t, wc.prov, nil)
	r.cluster = wc.cluster
	f.serverVersion, f.serverVersionErr = wc.server, wc.serverErr
	if wc.tokenErr != nil {
		r.err = wc.tokenErr
	}
	provKind := mustKind(t, "provisioning-cluster")
	if wc.provListErr != nil {
		c.withListErr(provKind.GVR, wc.provListErr)
	}
	if wc.nodesErr != nil {
		c.withListErr(managementNodeGVR, wc.nodesErr)
	} else {
		c.withListNS(managementNodeGVR, "c-abc", &unstructured.UnstructuredList{Items: wc.nodes})
	}
	res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), wc.in)
	return res, f, r, err
}

const (
	notReady  = "--windows: the Windows node command is available once the cluster is connected and has etcd, control plane and worker nodes; try again then"
	noVersion = "--windows: the Windows node command is available once the cluster reports its Kubernetes version; try again then"
	readFail  = "--windows: cannot tell whether the cluster accepts Windows nodes: "
	k3sReason = "--windows: the cluster runs K3s; Windows nodes join only RKE2 clusters"
)

// TestWindowsOptionComposesTheWindowsNodeCommands asserts --windows composes
// the two Windows node commands under withOptions, and no Linux one, adds no
// top-level key, and with no other option prints Rancher's Windows node
// command as it is.
func TestWindowsOptionComposesTheWindowsNodeCommands(t *testing.T) {
	t.Parallel()
	res, _, _, err := readyWindowsCase().run(t)
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	doc, _ := marshaledDoc(t, res.Data)
	reg, _ := doc["registration"].(map[string]any)
	assert.Equal(t, map[string]any{
		"windowsNodeCommand":         optionsWindowsNodeCommand + " -NodeName 'win1'",
		"insecureWindowsNodeCommand": optionsInsecureWindowsNodeCommand + " -NodeName 'win1'",
	}, reg["withOptions"])
	assert.Equal(t, optionsNodeCommand, reg["nodeCommand"], "the node command fields are unchanged")
	assert.NotContains(t, reg, "windowsNodeCommand", "no Windows field outside withOptions")
	for _, key := range []string{"windowsNodeCommand", "insecureWindowsNodeCommand", "withOptions"} {
		assert.NotContains(t, doc, key, "no top-level key")
	}
	assert.Contains(t, res.Text, "  Windows node command with options:\n    "+optionsWindowsNodeCommand+" -NodeName 'win1'\n")
	assert.Contains(t, res.Text, "  Windows node command with options (insecure):\n    "+optionsInsecureWindowsNodeCommand+" -NodeName 'win1'\n")
	assert.NotContains(t, res.Text, "  node command with options")

	alone := readyWindowsCase()
	alone.in = optionsIn(true, map[string]bool{"windows": true}, nil, nil)
	res, _, _, err = alone.run(t)
	require.NoError(t, err)
	reg = registrationOf(t, res)
	composed, _ := reg["withOptions"].(map[string]any)
	assert.Equal(t, optionsWindowsNodeCommand, composed["windowsNodeCommand"], "--windows alone is Rancher's command as it is")
}

// TestWindowsOptionEmptinessAndPlaceholders asserts that with every surfaced
// field empty the section has withOptions alone, and that a Windows node
// command left out gives only the composed command's warning.
func TestWindowsOptionEmptinessAndPlaceholders(t *testing.T) {
	t.Parallel()
	t.Run("every surfaced field empty", func(t *testing.T) {
		t.Parallel()
		wc := readyWindowsCase()
		f, c, r := optionsFixture(t, wc.prov, map[string]any{"nodeCommand": "", "insecureNodeCommand": "", "command": "", "insecureCommand": "", "manifestUrl": ""})
		r.cluster = wc.cluster
		c.withListNS(managementNodeGVR, "c-abc", &unstructured.UnstructuredList{Items: readyNodes})
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), wc.in)
		require.NoError(t, err)
		reg := registrationOf(t, res)
		require.NotNil(t, reg)
		assert.Len(t, reg, 1, "withOptions alone: %v", reg)
		assert.Contains(t, reg, "withOptions")
	})
	t.Run("a Windows node command left out", func(t *testing.T) {
		t.Parallel()
		wc := readyWindowsCase()
		f, c, r := optionsFixture(t, wc.prov, map[string]any{"windowsNodeCommand": "curl.exe x; ./install.ps1 -Token {token} -Worker"})
		r.cluster = wc.cluster
		c.withListNS(managementNodeGVR, "c-abc", &unstructured.UnstructuredList{Items: readyNodes})
		res, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), wc.in)
		require.NoError(t, err)
		assert.Equal(t, []string{"registration token c-abc/default-token: windowsNodeCommand not formatted by the server; the node command with options is left out"}, res.Warnings)
		composed, _ := registrationOf(t, res)["withOptions"].(map[string]any)
		assert.Equal(t, map[string]any{"insecureWindowsNodeCommand": optionsInsecureWindowsNodeCommand + " -NodeName 'win1'"}, composed)
	})
}

// TestWindowsOptionIsRefusedWhereTheRancherUIShowsNoWindowsNodeCommand
// asserts each refusal of --windows from the cluster's reads, in the order of
// the checks: the outcome of a row where several conditions hold is the
// earlier check's, and no refusal reads the token.
func TestWindowsOptionIsRefusedWhereTheRancherUIShowsNoWindowsNodeCommand(t *testing.T) {
	t.Parallel()
	const (
		validation = clierr.CodeValidation
		generic    = clierr.CodeGeneric
	)
	type row struct {
		name      string
		change    func(*windowsCase)
		code      string
		message   string
		retryable bool
	}
	cluster := func(provider, version string, conds ...string) func(*windowsCase) {
		return func(wc *windowsCase) { wc.cluster = windowsCluster(provider, version, conds...) }
	}
	rows := []row{
		{"k3s", cluster("k3s", "v1.33.4+k3s1", "Connected=True"), validation, k3sReason, false},
		{"k3s with no version", cluster("k3s", "", "Connected=True"), validation, k3sReason, false},
		{"no version", cluster("rke2", "", "Connected=True"), generic, noVersion, true},
		{"below v1.21.0", cluster("rke2", "v1.20.15+rke2r1", "Connected=True"), validation,
			`--windows: the cluster's Kubernetes version "v1.20.15+rke2r1" is older than v1.21.0, the oldest version the Rancher UI shows the Windows node command for`, false},
		{"a pre-release version", cluster("rke2", "v1.29.2-rc1+rke2r1", "Connected=True"), validation,
			`--windows: the cluster's Kubernetes version "v1.29.2-rc1+rke2r1" is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with v1.21.0, the oldest version the Rancher UI shows the Windows node command for`, false},
		{"cilium", func(wc *windowsCase) { wc.prov = windowsProv("cilium") }, validation,
			`--windows: the cluster's CNI is "cilium"; the Rancher UI shows the Windows node command only for calico, and for flannel from Kubernetes v1.29.2`, false},
		{"flannel on v2.13", func(wc *windowsCase) { wc.prov, wc.server = windowsProv("flannel"), "v2.13.11" }, validation,
			`--windows: the cluster's CNI is "flannel"; on Rancher before v2.14 the Rancher UI shows the Windows node command only for calico`, false},
		{"an unsupported CNI and Connected False", func(wc *windowsCase) {
			wc.prov = windowsProv("canal")
			wc.cluster = windowsCluster("rke2", "v1.33.4+rke2r1", "Connected=False", "Ready=True")
		}, validation, `--windows: the cluster's CNI is "canal"; the Rancher UI shows the Windows node command only for calico, and for flannel from Kubernetes v1.29.2`, false},
		{"no condition", cluster("rke2", "v1.33.4+rke2r1"), generic, notReady, true},
		{"no worker node", func(wc *windowsCase) { wc.nodes = readyNodes[:2] }, generic, notReady, true},
		{"no node", func(wc *windowsCase) { wc.nodes = nil }, generic, notReady, true},
		{"a failed Node List", func(wc *windowsCase) { wc.nodesErr = errors.New("connection reset") }, generic, readFail + "connection reset", false},
		{"a refused Node List keeps its code", func(wc *windowsCase) {
			wc.nodesErr = clierr.New(clierr.CodeRBAC, "nodes.management.cattle.io is forbidden")
		}, clierr.CodeRBAC, readFail + "nodes.management.cattle.io is forbidden", false},
		{"a failed Node List and K3s", func(wc *windowsCase) {
			wc.nodesErr = errors.New("connection reset")
			wc.cluster = windowsCluster("k3s", "v1.33.4+k3s1", "Connected=True")
		}, validation, k3sReason, false},
		{"a failed token read after every check", func(wc *windowsCase) {
			wc.tokenErr = errors.New("token store unavailable")
			wc.in = optionsIn(true, map[string]bool{"windows": true}, nil, nil)
		}, generic, "--windows: cannot read the cluster registration token: token store unavailable", false},
	}
	for _, rw := range rows {
		t.Run(rw.name, func(t *testing.T) {
			t.Parallel()
			wc := readyWindowsCase()
			rw.change(&wc)
			_, _, r, err := wc.run(t)
			ce := refusalOf(t, err, rw.code, rw.message)
			assert.Equal(t, rw.retryable, ce.Retryable)
			if !strings.Contains(rw.message, "registration token") {
				assert.NotContains(t, r.path, "default-token", "the token is not read")
			}
		})
	}
	for _, rw := range []row{
		{"every role on one node", func(wc *windowsCase) { wc.nodes = []unstructured.Unstructured{managementNode("n1", true, true, true)} }, "", "", false},
		{"flannel at v1.29.2", func(wc *windowsCase) {
			wc.prov = windowsProv("flannel")
			wc.cluster = windowsCluster("rke2", "v1.29.2+rke2r1", "Connected=True")
		}, "", "", false},
		{"a failed version read gets the newest rules", func(wc *windowsCase) {
			wc.prov, wc.serverErr = windowsProv("flannel"), errors.New("no response")
		}, "", "", false},
		{"v2.14 reads the provisioning spec", func(wc *windowsCase) {
			wc.server = "v2.14.7"
			wc.cluster = windowsCluster("rke2", "", "Connected=True")
		}, "", "", false},
	} {
		t.Run("accepted: "+rw.name, func(t *testing.T) {
			t.Parallel()
			wc := readyWindowsCase()
			rw.change(&wc)
			_, _, _, err := wc.run(t)
			require.NoError(t, err)
		})
	}
}

// TestWindowsOptionUnavailableLookupIsRefusedBeforeAnyVersionRead asserts an
// unavailable or ambiguous provisioning lookup refuses --windows with the
// lookup's own code and its candidates, before the server version or the
// token is read.
func TestWindowsOptionUnavailableLookupIsRefusedBeforeAnyVersionRead(t *testing.T) {
	t.Parallel()
	t.Run("a failed lookup", func(t *testing.T) {
		t.Parallel()
		wc := readyWindowsCase()
		wc.provListErr = errors.New("clusters.provisioning.cattle.io is forbidden")
		_, f, r, err := wc.run(t)
		assertRefusal(t, err, clierr.CodeGeneric, readFail+"clusters.provisioning.cattle.io is forbidden")
		assert.Zero(t, f.versionCalls, "no server version read")
		assert.NotContains(t, r.path, "default-token")
	})
	t.Run("an ambiguous match", func(t *testing.T) {
		t.Parallel()
		provKind := mustKind(t, "provisioning-cluster")
		wc := readyWindowsCase()
		f, c, r := optionsFixture(t, nil, nil)
		r.cluster = wc.cluster
		c.withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*provClusterFixture("prov-b", "c-abc"), *provClusterFixture("prov-a", "c-abc"),
		}})
		_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), wc.in)
		ce := refusalOf(t, err, clierr.CodeAmbiguousSelection,
			readFail+`2 provisioning.cattle.io Clusters report status.clusterName "c-abc"; cannot tell which one belongs to this cluster`)
		assert.Equal(t, []clierr.Candidate{
			{ID: "fleet-default/prov-a", Name: "fleet-default/prov-a"},
			{ID: "fleet-default/prov-b", Name: "fleet-default/prov-b"},
		}, ce.Candidates)
		assert.Zero(t, f.versionCalls)
		assert.NotContains(t, r.path, "default-token")
	})
}

// TestWindowsOptionCancellationReturnsAsItself asserts a canceled or expired
// context during the Node List or the token read is the command's error
// itself, never the refusal of --windows.
func TestWindowsOptionCancellationReturnsAsItself(t *testing.T) {
	t.Parallel()
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, where := range []string{"node list", "token read"} {
			t.Run(fmt.Sprintf("%s %v", where, ctxErr), func(t *testing.T) {
				t.Parallel()
				wc := readyWindowsCase()
				if where == "node list" {
					wc.nodesErr = fmt.Errorf("list nodes: %w", ctxErr)
				} else {
					wc.tokenErr = fmt.Errorf("fetch default-token: %w", ctxErr)
				}
				_, _, _, err := wc.run(t)
				require.ErrorIs(t, err, ctxErr)
				_, structured := errors.AsType[*clierr.Error](err)
				assert.False(t, structured, "the cancellation is not wrapped")
			})
		}
	}
}

// TestWindowsOptionConflictsAreRefusedBeforeAnyRequest asserts a role flag,
// a label or a second taint with --windows is refused before any request.
func TestWindowsOptionConflictsAreRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	windows := map[string]bool{"windows": true}
	for _, rw := range []struct {
		name string
		in   input
		want string
	}{
		{"etcd", optionsIn(true, map[string]bool{"windows": true, "etcd": true}, nil, nil), "--etcd conflicts with --windows; remove --etcd (Windows nodes can only be workers)"},
		{"controlplane", optionsIn(true, map[string]bool{"windows": true, "controlplane": true}, nil, nil), "--controlplane conflicts with --windows; remove --controlplane (Windows nodes can only be workers)"},
		{"worker", optionsIn(true, map[string]bool{"windows": true, "worker": true}, nil, nil), "--worker conflicts with --windows; remove --worker (Windows nodes can only be workers)"},
		{"label", optionsIn(true, windows, nil, map[string][]string{"label": {"tier=db"}}), "--label conflicts with --windows; remove --label (the Windows node command already passes -Label)"},
		{"two taints", optionsIn(true, windows, nil, map[string][]string{"taint": {"a=b:NoSchedule", "c=d:NoSchedule"}}), "--taint given more than once conflicts with --windows; remove all but one --taint"},
		{"without --registration", optionsIn(false, map[string]bool{"windows": true, "worker": true}, nil, nil), "--worker conflicts with --windows; remove --worker (Windows nodes can only be workers)"},
	} {
		t.Run(rw.name, func(t *testing.T) {
			t.Parallel()
			f, c, r := optionsFixture(t, customProvCluster(), nil)
			_, err := execRead(t, NewInspectCluster(f, fakeRoots{rc: r}), rw.in)
			assertRefusal(t, err, clierr.CodeValidation, rw.want)
			assertNoRequest(t, f, c, r)
			assert.Zero(t, f.versionCalls)
		})
	}
}

// TestWindowsOptionUsesReadyOnlyWithoutConnected asserts every pair of the
// management Cluster's Connected condition (absent, True, False, Unknown)
// and Ready condition (True, False, Unknown): when a Connected condition
// exists, its status must be True; only without one is the Ready condition
// used, and it must be True.
func TestWindowsOptionUsesReadyOnlyWithoutConnected(t *testing.T) {
	t.Parallel()
	for _, connected := range []string{"", "True", "False", "Unknown"} {
		for _, ready := range []string{"True", "False", "Unknown"} {
			accepted := connected == "True" || connected == "" && ready == "True"
			t.Run(fmt.Sprintf("Connected %q Ready %q", connected, ready), func(t *testing.T) {
				t.Parallel()
				conds := []string{"Ready=" + ready}
				if connected != "" {
					conds = append(conds, "Connected="+connected)
				}
				wc := readyWindowsCase()
				wc.cluster = windowsCluster("rke2", "v1.33.4+rke2r1", conds...)
				_, _, _, err := wc.run(t)
				if accepted {
					require.NoError(t, err)
					return
				}
				ce := refusalOf(t, err, clierr.CodeGeneric, notReady)
				assert.True(t, ce.Retryable)
			})
		}
	}
}
