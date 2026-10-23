package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/user"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createPlannerCallersFile lists the functions that use a create planner;
// test/conformance proves the list is complete, and the table below has one
// row per entry.
const createPlannerCallersFile = "testdata/create-planner-callers.txt"

// previewResult is what one create command shows for one input: the Effect
// and the text of the --dry-run preview.
type previewResult struct {
	eff  safety.Effect
	text string
}

// previewCase is one input of a create command and what its preview must
// show: each path in present with its value, each path in absent left out,
// and a diff that is not empty (unless empty is set: the command sets
// nothing past its name). A value that must be hidden reads "<redacted>".
type previewCase struct {
	name    string
	plan    func(t *testing.T) previewResult
	present map[string]any
	absent  []string
	empty   bool
}

// createPreviewRow is one create command (grant included): the function
// that plans it, as test/conformance names it, and its cases.
type createPreviewRow struct {
	command string
	caller  string
	cases   []previewCase
}

// previewCommand serves an Effect a Plan already built to the pipeline, which
// renders it with no input of its own: the text of the preview is what
// --dry-run prints.
type previewCommand struct{ eff safety.Effect }

func (previewCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "create preview", CommandType: runtime.Mutating}
}

func (c previewCommand) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return c.eff, func(context.Context) (runtime.Result, error) {
		return runtime.Result{}, fmt.Errorf("a preview never executes")
	}, nil
}

// planPreview runs the command's Plan and the text rendering of its Effect.
func planPreview(t *testing.T, ctx context.Context, cmd runtime.Command, in runtime.Input) previewResult {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	eff, _, err := mc.Plan(ctx, in)
	require.NoError(t, err)
	var out bytes.Buffer
	_, ce := runtime.Run(ctx, previewCommand{eff}, fieldCoverageInput{}, runtime.IO{Out: &out, Err: io.Discard, DryRun: true}, safety.Binding{})
	require.Nil(t, ce)
	return previewResult{eff: eff, text: out.String()}
}

// leafValues flattens a value to the leaf pointers a preview lists: maps by
// key and lists by index; an empty map or list is a leaf of its own.
func leafValues(path string, v any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = v
		}
		for k, child := range t {
			leafValues(path+"/"+engine.PointerFor([]string{k})[1:], child, out)
		}
	case []any:
		if len(t) == 0 {
			out[path] = v
		}
		for i, child := range t {
			leafValues(path+"/"+strconv.Itoa(i), child, out)
		}
	default:
		out[path] = v
	}
}

// coverageCases are the cases of a command in createFieldCoverageTable: each
// run's covered fields, with the value the run expects, are the entries the
// preview must list. hidden names fields whose value the preview hides.
func coverageCases(t *testing.T, command string, hidden ...string) []previewCase {
	t.Helper()
	cov, ok := createFieldCoverageTable[command]
	require.True(t, ok, command)
	prefix := ""
	for _, r := range loadRancherAPIFields(t).Resources {
		if r.Resource == cov.resource && r.Path != "" {
			prefix = "/" + strings.ReplaceAll(r.Path, ".", "/")
		}
	}
	var cases []previewCase
	for i, run := range cov.runs {
		present := map[string]any{}
		var absentPaths []string
		for f, want := range run.want {
			if want == absent {
				absentPaths = append(absentPaths, prefix+"/"+f)
				continue
			}
			if slices.Contains(hidden, f) {
				want = "<redacted>"
			}
			leafValues(prefix+"/"+engine.PointerFor([]string{f})[1:], want, present)
		}
		cases = append(cases, previewCase{
			name: fmt.Sprintf("run %d", i),
			plan: func(t *testing.T) previewResult {
				t.Helper()
				rec := &fieldRecorder{}
				return planPreview(t, t.Context(), cov.command(rec, run.input), run.input)
			},
			present: present,
			absent:  absentPaths,
		})
	}
	return cases
}

func manifestCase(name, clusterFlag, manifest string, present map[string]any, absentPaths ...string) previewCase {
	return previewCase{
		name: name,
		plan: func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml", "cluster": clusterFlag}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{manifest}), in)
		},
		present: present,
		absent:  absentPaths,
	}
}

func machineConfigCase(name string, args []string, values map[string]string, sets []string, present map[string]any, absentPaths ...string) previewCase {
	return previewCase{
		name: name,
		plan: func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{args: args, values: values, lists: map[string][]string{"set": sets}}
			return planPreview(t, t.Context(), provisioning.NewCreateMachineConfig(&fieldRecorder{}), in)
		},
		present: present,
		absent:  absentPaths,
	}
}

// grantPreview runs grant through the whole CLI against a test server, once
// as json for the diff and once as text.
func grantPreview(t *testing.T, args ...string) previewResult {
	t.Helper()
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	srv := httptest.NewTLSServer(grHandler(&serverOpts{}))
	t.Cleanup(srv.Close)
	dir := grConfigDir(t, srv)
	argv := append([]string{"grant"}, args...)
	argv = append(argv, "--dry-run")
	stdout, _, err := grRun(t, grRoot(t), dir, append(slices.Clone(argv), "-o", "json")...)
	require.NoError(t, err)
	var env effectEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env))
	var diff []safety.DiffEntry
	for _, d := range env.Data.Diff {
		diff = append(diff, safety.DiffEntry{Path: d.Path, From: d.From, To: d.To})
	}
	text, _, err := grRun(t, grRoot(t), dir, argv...)
	require.NoError(t, err)
	return previewResult{eff: safety.Effect{Diff: diff}, text: string(text)}
}

func projectContext(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-x", ID: "p-1", DisplayName: "web"})
}

// createPreviewRows is the table over every create command. Each row names
// the function that plans the command (a key of create-planner-callers.txt).
func createPreviewRows(t *testing.T) []createPreviewRow {
	t.Helper()
	const (
		projectKey  = "field.cattle.io~1projectId"
		lastApplied = "kubectl.kubernetes.io~1last-applied-configuration"
	)
	return []createPreviewRow{
		{"create project", "internal/engine/projects.createCmd.Plan", coverageCases(t, "create project")},
		{"create user", "internal/engine/user.createCmd.Plan", coverageCases(t, "create user")},
		{"create oidc-client", "internal/engine/oidcclient.createCmd.Plan", coverageCases(t, "create oidc-client")},
		{"create token", "internal/engine/extres.createTokenCmd.Plan", coverageCases(t, "create token")},
		{"create kubeconfig", "internal/engine/extres.createKubeconfigCmd.Plan", coverageCases(t, "create kubeconfig")},
		{"create password-change-request", "internal/engine/extres.createPasswordChangeRequestCmd.Plan",
			coverageCases(t, "create password-change-request", "newPassword")},
		{"create cluster", "internal/engine/provisioning.createClusterCmd.Plan", coverageCases(t, "create cluster")},
		{"create namespace", "internal/engine/projects.createNamespaceCmd.Plan", []previewCase{{
			name: "in a project",
			plan: func(t *testing.T) previewResult {
				t.Helper()
				in := fieldCoverageInput{args: []string{"team-b"}, values: map[string]string{"cluster": "c-x", "project": "web"}}
				return planPreview(t, projectContext(t), projects.NewCreateNamespace(&fieldRecorder{}), in)
			},
			present: map[string]any{
				"/metadata/labels/" + projectKey:      "p-1",
				"/metadata/annotations/" + projectKey: "c-x:p-1",
			},
		}, {
			name: "no project",
			plan: func(t *testing.T) previewResult {
				t.Helper()
				in := fieldCoverageInput{args: []string{"team-c"}, values: map[string]string{"cluster": "c-x"}}
				return planPreview(t, t.Context(), projects.NewCreateNamespace(&fieldRecorder{}), in)
			},
			absent: []string{"/metadata/labels", "/metadata/annotations", "/metadata/name", "/kind", "/apiVersion"},
			empty:  true,
		}}},
		{"create secret", "internal/engine/projects.createSecretCmd.Plan", []previewCase{{
			name: "in a namespace",
			plan: func(t *testing.T) previewResult {
				t.Helper()
				in := fieldCoverageInput{
					args:   []string{"app-secret"},
					values: map[string]string{"cluster": "c-x", "namespace": "team-a", "type": "kubernetes.io/basic-auth", "from-literal": "username=admin,password=s3cret"},
				}
				return planPreview(t, t.Context(), projects.NewCreateSecret(&fieldRecorder{}), in)
			},
			present: map[string]any{
				"/metadata/namespace":  "team-a",
				"/type":                "kubernetes.io/basic-auth",
				"/stringData/username": "<redacted>",
				"/stringData/password": "<redacted>",
			},
			absent: []string{"/metadata/labels"},
		}, {
			name: "in a project",
			plan: func(t *testing.T) previewResult {
				t.Helper()
				in := fieldCoverageInput{
					args:   []string{"app-secret"},
					values: map[string]string{"cluster": "c-x", "project": "web", "from-literal": "token=abc"},
				}
				return planPreview(t, projectContext(t), projects.NewCreateSecret(&fieldRecorder{}), in)
			},
			present: map[string]any{
				"/metadata/namespace": "c-x-p-1",
				"/metadata/labels/management.cattle.io~1project-scoped-secret": "p-1",
				"/type":             "Opaque",
				"/stringData/token": "<redacted>",
			},
		}}},
		{"create resource", "internal/engine/resources.createCmd.Plan", []previewCase{
			manifestCase("a namespace", "c-x", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-d\n  labels:\n    env: dev\n  annotations:\n    owner: ops\n",
				map[string]any{"/metadata/labels/env": "dev", "/metadata/annotations/owner": "ops"},
				"/metadata/name", "/kind", "/apiVersion"),
			manifestCase("a secret", "c-x", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: team-a\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: '{\"data\":{\"k\":\"c3A=\"}}'\ndata:\n  k: c2VjcmV0\nstringData:\n  s: plain\ntype: Opaque\n",
				map[string]any{
					"/metadata/namespace":                  "team-a",
					"/metadata/annotations/" + lastApplied: "<redacted>",
					"/data/k":                              "<redacted>",
					"/stringData/s":                        "<redacted>",
					"/type":                                "Opaque",
				}),
			manifestCase("a user", "", "apiVersion: management.cattle.io/v3\nkind: User\nmetadata:\n  name: u-abc\nusername: alice\npassword: Sup3rS3cretPass\nenabled: true\n",
				map[string]any{"/username": "alice", "/password": "<redacted>", "/enabled": true}),
		}},
		{"create machine-config", "internal/engine/provisioning.createMachineConfigCmd.Plan", []previewCase{
			machineConfigCase("curated flags", []string{"digitaloceanconfig", "web-pool"},
				map[string]string{"region": "nyc3", "size": "s-2vcpu-4gb", "image": "ubuntu-24-10-x64", "tags": "web"}, nil,
				map[string]any{"/region": "nyc3", "/size": "s-2vcpu-4gb", "/image": "ubuntu-24-10-x64", "/tags": "web", "/metadata/namespace": "fleet-default"},
				"/metadata/name", "/kind", "/apiVersion", "/monitoring"),
			machineConfigCase("an ssh key", []string{"digitaloceanconfig", "web-pool"}, nil,
				[]string{"sshKeyContents=-----BEGIN KEY-----", "sshPort=22"},
				map[string]any{"/sshKeyContents": "<redacted>", "/sshPort": "22"}),
			machineConfigCase("a session token", []string{"amazonec2config", "aws-pool"}, nil,
				[]string{"region=us-east-1", `SessionToken={"k":"v"}`},
				map[string]any{"/region": "us-east-1", "/SessionToken/k": "<redacted>"}),
			machineConfigCase("an ssh password", []string{"vmwarevsphereconfig", "vm-pool"}, nil,
				[]string{"sshPassword=hunter2", "cpuCount=2"},
				map[string]any{"/sshPassword": "<redacted>", "/cpuCount": "2"}),
		}},
		{"grant", "internal/engine/rbac.grantCmd.Plan", []previewCase{{
			name: "project scope",
			plan: func(t *testing.T) previewResult {
				t.Helper()
				return grantPreview(t, "u-abc", "--role", "project-member", "--project", "p-x", "--cluster", "c-x")
			},
			present: map[string]any{
				"/metadata/namespace": "p-x",
				"/projectName":        "c-x:p-x",
				"/roleTemplateName":   "project-member",
				"/userName":           "u-abc",
			},
			absent: []string{"/metadata/generateName", "/kind", "/apiVersion"},
		}}},
	}
}

// TestCreatePreviewListsTheFieldsEveryCreateSets runs every create command's
// Plan for each case of its row and asserts the preview lists each field the
// input sets at its leaf path with the value the row states, leaves out
// every path the row marks absent and the fields that name the object, and
// is not empty.
func TestCreatePreviewListsTheFieldsEveryCreateSets(t *testing.T) {
	for _, row := range createPreviewRows(t) {
		for _, tc := range row.cases {
			t.Run(row.command+" "+tc.name, func(t *testing.T) {
				res := tc.plan(t)
				got := map[string]any{}
				for _, e := range res.eff.Diff {
					assert.Nil(t, e.From, "%s: a create entry has no from", e.Path)
					got[e.Path] = e.To
				}
				if tc.empty {
					assert.Empty(t, res.eff.Diff)
				} else {
					assert.NotEmpty(t, res.eff.Diff, "the preview lists no field")
				}
				for _, p := range []string{"/apiVersion", "/kind", "/metadata/name", "/metadata/generateName"} {
					assert.NotContains(t, got, p, "the headline shows the identity fields")
				}
				for _, path := range slices.Sorted(maps.Keys(tc.present)) {
					if assert.Contains(t, got, path) {
						assert.Equal(t, tc.present[path], got[path], path)
					}
					assert.Contains(t, res.text, path+":", "the text lists "+path)
				}
				for _, path := range tc.absent {
					for p := range got {
						assert.False(t, p == path || strings.HasPrefix(p, path+"/"), "the preview lists %s", p)
					}
				}
			})
		}
	}
}

// TestCreatePreviewTableCoversEveryPlanner asserts the table has one row for
// each function in the key file (the file lists every use of a create
// planner, proved by test/conformance), one row for each create command and
// for grant, and a case for each row.
func TestCreatePreviewTableCoversEveryPlanner(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(createPlannerCallersFile)
	require.NoError(t, err)
	keys := strings.Fields(string(raw))
	require.NotEmpty(t, keys)

	rows := createPreviewRows(t)
	var callers, commands []string
	for _, row := range rows {
		callers = append(callers, row.caller)
		commands = append(commands, row.command)
		assert.NotEmpty(t, row.cases, row.command)
	}
	slices.Sort(callers)
	assert.Equal(t, keys, callers, "one row per key of %s", createPlannerCallersFile)
	assert.Len(t, slices.Compact(slices.Clone(callers)), len(callers), "a key names one row")

	m, err := buildManifest("v-test")
	require.NoError(t, err)
	want := []string{"grant"}
	for _, c := range m.Commands {
		if strings.HasPrefix(c.Name, "create ") {
			want = append(want, c.Name)
		}
	}
	slices.Sort(want)
	slices.Sort(commands)
	assert.Equal(t, want, commands, "one row per create command and grant")
}

// TestCreatePreviewHidesSecretValues runs the commands that can send a secret
// and asserts the value never appears in the json-marshaled Effect or in the
// text preview, and the path does.
func TestCreatePreviewHidesSecretValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    func(t *testing.T) previewResult
		secrets []string
		paths   []string
	}{
		{"secret literal", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{args: []string{"s"}, values: map[string]string{"cluster": "c-x", "namespace": "team-a", "from-literal": "password=lit-secret-1"}}
			return planPreview(t, t.Context(), projects.NewCreateSecret(&fieldRecorder{}), in)
		}, []string{"lit-secret-1"}, []string{"/stringData/password"}},
		{"secret manifest", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml", "cluster": "c-x"}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{
				"apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: team-a\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: 'annot-secret-2'\ndata:\n  k: ZGF0YS1zZWNyZXQtMw==\nstringData:\n  s: string-secret-4\n"}), in)
		}, []string{"annot-secret-2", "ZGF0YS1zZWNyZXQtMw==", "string-secret-4"},
			[]string{"/data/k", "/stringData/s", "/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration"}},
		{"user manifest", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml"}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{
				"apiVersion: management.cattle.io/v3\nkind: User\nmetadata:\n  name: u-abc\nusername: alice\npassword: user-secret-5\n"}), in)
		}, []string{"user-secret-5"}, []string{"/password"}},
		{"secret manifest in another case", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml", "cluster": "c-x"}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{
				"apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: team-a\nData:\n  k: Y2FzZS1zZWNyZXQtMTY=\nStringData:\n  s: case-secret-17\n"}), in)
		}, []string{"Y2FzZS1zZWNyZXQtMTY=", "case-secret-17"}, []string{"/Data/k", "/StringData/s"}},
		{"user manifest in another case", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml"}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{
				"apiVersion: management.cattle.io/v3\nkind: User\nmetadata:\n  name: u-abc\nusername: alice\nPassword: user-secret-18\n"}), in)
		}, []string{"user-secret-18"}, []string{"/Password"}},
		{"machine config manifest", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{values: map[string]string{"filename": "m.yaml"}}
			return planPreview(t, t.Context(), resources.NewCreate(&fieldRecorder{}, fieldCoverageSource{
				"apiVersion: rke-machine-config.cattle.io/v1\nkind: Amazonec2Config\nmetadata:\n  name: aws\n  namespace: fleet-default\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: 'annot-secret-6'\nsecretKey: key-secret-7\nsshKeyContents: ssh-secret-8\nregion: us-east-1\n"}), in)
		}, []string{"annot-secret-6", "key-secret-7", "ssh-secret-8"},
			[]string{"/secretKey", "/sshKeyContents", "/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration"}},
		{"ssh key", func(t *testing.T) previewResult {
			t.Helper()
			return machineConfigCase("", []string{"digitaloceanconfig", "w"}, nil, []string{"sshKeyContents=ssh-secret-9"}, nil).plan(t)
		}, []string{"ssh-secret-9"}, []string{"/sshKeyContents"}},
		{"session token", func(t *testing.T) previewResult {
			t.Helper()
			return machineConfigCase("", []string{"amazonec2config", "w"}, nil, []string{"sessionToken=token-secret-10"}, nil).plan(t)
		}, []string{"token-secret-10"}, []string{"/sessionToken"}},
		{"session token in other case, as a map", func(t *testing.T) previewResult {
			t.Helper()
			return machineConfigCase("", []string{"amazonec2config", "w"}, nil, []string{`SessionToken={"k":"map-secret-11"}`}, nil).plan(t)
		}, []string{"map-secret-11"}, []string{"/SessionToken/k"}},
		{"vsphere ssh password", func(t *testing.T) previewResult {
			t.Helper()
			return machineConfigCase("", []string{"vmwarevsphereconfig", "w"}, nil, []string{"sshPassword=pass-secret-12"}, nil).plan(t)
		}, []string{"pass-secret-12"}, []string{"/sshPassword"}},
		{"harvester ssh password", func(t *testing.T) previewResult {
			t.Helper()
			return machineConfigCase("", []string{"harvesterconfig", "w"}, nil, []string{"sshPassword=pass-secret-13"}, nil).plan(t)
		}, []string{"pass-secret-13"}, []string{"/sshPassword"}},
		{"password change request", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{args: []string{"u-abc"}, bools: map[string]bool{"password-stdin": true}, stdin: "pcr-secret-14"}
			return planPreview(t, t.Context(), extres.NewCreatePasswordChangeRequest(&fieldRecorder{}, fieldCoverageSource{in.stdin}), in)
		}, []string{"pcr-secret-14"}, []string{"/spec/newPassword"}},
		{"user password", func(t *testing.T) previewResult {
			t.Helper()
			in := fieldCoverageInput{args: []string{"alice"}, bools: map[string]bool{"password-stdin": true}, stdin: "user-secret-15"}
			return planPreview(t, t.Context(), user.NewCreate(&fieldRecorder{}, fieldCoverageSource{in.stdin}), in)
		}, []string{"user-secret-15"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.plan(t)
			raw, err := json.Marshal(res.eff)
			require.NoError(t, err)
			for _, secret := range tc.secrets {
				assert.NotContains(t, string(raw), secret, "the diff shows the value")
				assert.NotContains(t, res.text, secret, "the text shows the value")
			}
			got := map[string]any{}
			for _, e := range res.eff.Diff {
				got[e.Path] = e.To
			}
			for _, path := range tc.paths {
				if assert.Contains(t, got, path) {
					assert.Equal(t, "<redacted>", got[path], path)
				}
				assert.Contains(t, res.text, path+": <nil> -> <redacted>", "the text lists the path")
			}
		})
	}
}
