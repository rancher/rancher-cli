package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/oidcclient"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/user"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// rancherAPIFieldsGolden is written by `make rancher-api-fields-update`
// (scripts/rancher-api-fields): the top-level fields of the Rancher API
// types the bespoke create commands build, read from the pinned oracle
// versions' pkg/apis.
const rancherAPIFieldsGolden = "testdata/rancher-api-fields.golden.json"

type rancherAPIFields struct {
	Module  string `json:"module"`
	Oracles map[string]struct {
		Tag           string `json:"tag"`
		PseudoVersion string `json:"pseudoVersion"`
	} `json:"oracles"`
	Resources []struct {
		Resource string              `json:"resource"`
		Path     string              `json:"path"`
		Fields   map[string][]string `json:"fields"`
	} `json:"resources"`
}

func loadRancherAPIFields(t *testing.T) rancherAPIFields {
	t.Helper()
	raw, err := os.ReadFile(rancherAPIFieldsGolden)
	require.NoError(t, err)
	var g rancherAPIFields
	require.NoError(t, json.Unmarshal(raw, &g))
	require.NotEmpty(t, g.Resources)
	return g
}

// notYetOffered is the omission reason for a field a client could set on
// create but no flag offers yet.
const notYetOffered = "not yet offered"

// fieldSource is where a create command takes one golden field from:
// exactly one of a flag, a positional argument, or an omission reason
// saying why no flag sets it.
type fieldSource struct {
	flag, arg, omitted string
}

func byFlag(name string) fieldSource  { return fieldSource{flag: name} }
func byArg(name string) fieldSource   { return fieldSource{arg: name} }
func omitted(why string) fieldSource  { return fieldSource{omitted: why} }
func (s fieldSource) setsField() bool { return s.flag != "" || s.arg != "" }

// fieldCoverageInput is the runtime.Input of a field-coverage dry-run;
// stdin serves --password-stdin.
type fieldCoverageInput struct {
	args   []string
	values map[string]string
	bools  map[string]bool
	lists  map[string][]string
	stdin  string
}

func (i fieldCoverageInput) String(k string) string    { return i.values[k] }
func (i fieldCoverageInput) Bool(k string) bool        { return i.bools[k] }
func (i fieldCoverageInput) Strings(k string) []string { return i.lists[k] }
func (i fieldCoverageInput) Args() []string            { return i.args }
func (i fieldCoverageInput) IsSet(k string) bool {
	_, v := i.values[k]
	_, b := i.bools[k]
	_, l := i.lists[k]
	return v || b || l
}

// absent is the expected value of a field the dry-run object must not carry.
var absent = new(struct{})

// fieldCoverageRun is one field-coverage dry-run: its input, and the exact
// value every covered field must hold in the object sent (absent for a
// field the input leaves out).
type fieldCoverageRun struct {
	input fieldCoverageInput
	want  map[string]any
}

// createFieldCoverage is one bespoke create command's coverage table: the
// golden resource it builds, every golden field of it with its source,
// and the dry-runs that prove each flag and argument carries its own value
// into its field (see TestCreateFieldCoverageFlagsSetFields for the rules
// the runs must follow).
type createFieldCoverage struct {
	resource string
	// gvrResource is the API resource the command creates; the dry-run
	// object sent to it is the one whose fields are checked.
	gvrResource string
	fields      map[string]fieldSource
	command     func(*fieldRecorder, fieldCoverageInput) runtime.Command
	runs        []fieldCoverageRun
}

// createFieldCoverageTable is the reviewed table of every bespoke create
// command that builds a Rancher API type. A field of the golden that a
// table does not name, or a table entry the golden does not have, fails
// TestCreateFieldCoverageMatchesGolden; a flag or argument whose value
// stops reaching its field fails TestCreateFieldCoverageFlagsSetFields.
var createFieldCoverageTable = map[string]createFieldCoverage{
	"create project": {
		resource:    "management.cattle.io/v3 Project",
		gvrResource: "projects",
		fields: map[string]fieldSource{
			"clusterName":                   byFlag("cluster"),
			"containerDefaultResourceLimit": byFlag("container-default-resource-limit"),
			"description":                   byFlag("description"),
			"displayName":                   byArg("name"),
			"namespaceDefaultResourceQuota": byFlag("namespace-default-quota"),
			"resourceQuota":                 byFlag("quota-limit"),
		},
		command: func(r *fieldRecorder, _ fieldCoverageInput) runtime.Command { return projects.NewCreate(r) },
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{
				args: []string{"web"},
				values: map[string]string{
					"cluster":                          "c-x",
					"description":                      "web tier",
					"quota-limit":                      "limitsCpu=2",
					"namespace-default-quota":          "limitsCpu=1",
					"container-default-resource-limit": "limitsCpu=100m",
				},
			},
			want: map[string]any{
				"clusterName":                   "c-x",
				"containerDefaultResourceLimit": map[string]any{"limitsCpu": "100m"},
				"description":                   "web tier",
				"displayName":                   "web",
				"namespaceDefaultResourceQuota": map[string]any{"limit": map[string]any{"limitsCpu": "1"}},
				"resourceQuota":                 map[string]any{"limit": map[string]any{"limitsCpu": "2"}},
			},
		}, {
			input: fieldCoverageInput{
				args: []string{"api"},
				values: map[string]string{
					"cluster":                          "c-y",
					"description":                      "api tier",
					"quota-limit":                      "limitsMemory=4Gi",
					"namespace-default-quota":          "limitsMemory=2Gi",
					"container-default-resource-limit": "requestsMemory=32Mi",
				},
			},
			want: map[string]any{
				"clusterName":                   "c-y",
				"containerDefaultResourceLimit": map[string]any{"requestsMemory": "32Mi"},
				"description":                   "api tier",
				"displayName":                   "api",
				"namespaceDefaultResourceQuota": map[string]any{"limit": map[string]any{"limitsMemory": "2Gi"}},
				"resourceQuota":                 map[string]any{"limit": map[string]any{"limitsMemory": "4Gi"}},
			},
		}},
	},
	"create user": {
		resource:    "management.cattle.io/v3 User",
		gvrResource: "users",
		fields: map[string]fieldSource{
			"description":        byFlag("description"),
			"displayName":        byFlag("display-name"),
			"enabled":            byFlag("enabled"),
			"me":                 omitted("set by the server in its API responses to mark the calling user; a stored value is never read"),
			"mustChangePassword": byFlag("must-change-password"),
			"password":           omitted("deprecated: the password goes to the user's Secret in cattle-local-user-passwords, read from --password-stdin"),
			"principalIds":       omitted(notYetOffered),
			"username":           byArg("username"),
		},
		command: func(r *fieldRecorder, in fieldCoverageInput) runtime.Command {
			return user.NewCreate(r, fieldCoverageSource{in.stdin})
		},
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{
				args:   []string{"alice"},
				values: map[string]string{"display-name": "Alice", "enabled": "false", "description": "build robot"},
				bools:  map[string]bool{"password-stdin": true, "must-change-password": true},
				stdin:  "correctHorseBattery1",
			},
			want: map[string]any{
				"description": "build robot", "displayName": "Alice", "enabled": false,
				"mustChangePassword": true, "username": "alice",
			},
		}, {
			input: fieldCoverageInput{
				args:   []string{"bob"},
				values: map[string]string{"display-name": "Bob", "enabled": "true", "description": "ops"},
				bools:  map[string]bool{"password-stdin": true},
				stdin:  "correctHorseBattery2",
			},
			want: map[string]any{
				"description": "ops", "displayName": "Bob", "enabled": true,
				"mustChangePassword": absent, "username": "bob",
			},
		}, {
			input: fieldCoverageInput{
				args:   []string{"carol"},
				values: map[string]string{"display-name": "Carol", "enabled": "true", "description": "qa"},
				bools:  map[string]bool{"password-stdin": true, "must-change-password": true},
				stdin:  "correctHorseBattery3",
			},
			want: map[string]any{
				"description": "qa", "displayName": "Carol", "enabled": true,
				"mustChangePassword": true, "username": "carol",
			},
		}},
	},
	"create oidc-client": {
		resource:    "management.cattle.io/v3 OIDCClient",
		gvrResource: "oidcclients",
		fields: map[string]fieldSource{
			"description":                   byFlag("description"),
			"redirectURIs":                  byFlag("redirect-uri"),
			"refreshTokenExpirationSeconds": byFlag("refresh-ttl"),
			"scopes":                        byFlag("scope"),
			"tokenExpirationSeconds":        byFlag("token-ttl"),
		},
		command: func(r *fieldRecorder, _ fieldCoverageInput) runtime.Command {
			return oidcclient.NewCreate(r, fieldCoverageRoots{}, nil)
		},
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{
				args:   []string{"app-a"},
				values: map[string]string{"description": "app a", "token-ttl": "1h", "refresh-ttl": "24h"},
				lists:  map[string][]string{"redirect-uri": {"https://a.invalid/cb"}, "scope": {"openid"}},
			},
			want: map[string]any{
				"description":                   "app a",
				"redirectURIs":                  []any{"https://a.invalid/cb"},
				"refreshTokenExpirationSeconds": int64(86400),
				"scopes":                        []any{"openid"},
				"tokenExpirationSeconds":        int64(3600),
			},
		}, {
			input: fieldCoverageInput{
				args:   []string{"app-b"},
				values: map[string]string{"description": "app b", "token-ttl": "2h", "refresh-ttl": "48h"},
				lists:  map[string][]string{"redirect-uri": {"https://b.invalid/cb", "https://c.invalid/cb"}, "scope": {"openid", "profile"}},
			},
			want: map[string]any{
				"description":                   "app b",
				"redirectURIs":                  []any{"https://b.invalid/cb", "https://c.invalid/cb"},
				"refreshTokenExpirationSeconds": int64(172800),
				"scopes":                        []any{"openid", "profile"},
				"tokenExpirationSeconds":        int64(7200),
			},
		}},
	},
	"create token": {
		resource:    "ext.cattle.io/v1 Token",
		gvrResource: "tokens",
		fields: map[string]fieldSource{
			"clusterName":   byFlag("cluster"),
			"description":   byFlag("description"),
			"enabled":       omitted(notYetOffered),
			"kind":          omitted("marks a token created by a login (\"session\"); a token this command creates is a derived token, so the field stays empty"),
			"ttl":           byFlag("ttl"),
			"userID":        omitted("the server sets the calling user and refuses any other"),
			"userPrincipal": omitted("the server overwrites it with the calling user's principal"),
		},
		command: func(r *fieldRecorder, _ fieldCoverageInput) runtime.Command { return extres.NewCreateToken(r) },
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{values: map[string]string{"cluster": "c-x", "description": "ci", "ttl": "1h"}},
			want:  map[string]any{"clusterName": "c-x", "description": "ci", "ttl": int64(3600000)},
		}, {
			input: fieldCoverageInput{values: map[string]string{"cluster": "c-y", "description": "deploy", "ttl": "2h"}},
			want:  map[string]any{"clusterName": "c-y", "description": "deploy", "ttl": int64(7200000)},
		}},
	},
	"create kubeconfig": {
		resource:    "ext.cattle.io/v1 Kubeconfig",
		gvrResource: "kubeconfigs",
		fields: map[string]fieldSource{
			"clusters":            byFlag("clusters"),
			"currentContext":      byFlag("current-context"),
			"description":         byFlag("description"),
			"includeDefaultEntry": byFlag("include-default-entry"),
			"ttl":                 byFlag("ttl"),
		},
		command: func(r *fieldRecorder, _ fieldCoverageInput) runtime.Command { return extres.NewCreateKubeconfig(r) },
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{values: map[string]string{
				"clusters": "c-x,c-y", "current-context": "c-x", "description": "ci",
				"ttl": "1h", "include-default-entry": "false",
			}},
			want: map[string]any{
				"clusters": []any{"c-x", "c-y"}, "currentContext": "c-x", "description": "ci",
				"includeDefaultEntry": false, "ttl": int64(3600),
			},
		}, {
			input: fieldCoverageInput{values: map[string]string{
				"clusters": "c-x,c-y,c-z", "current-context": "c-y", "description": "ops",
				"ttl": "2h", "include-default-entry": "true",
			}},
			want: map[string]any{
				"clusters": []any{"c-x", "c-y", "c-z"}, "currentContext": "c-y", "description": "ops",
				"includeDefaultEntry": true, "ttl": int64(7200),
			},
		}},
	},
	"create password-change-request": {
		resource:    "ext.cattle.io/v1 PasswordChangeRequest",
		gvrResource: "passwordchangerequests",
		fields: map[string]fieldSource{
			"currentPassword": omitted(notYetOffered),
			"newPassword":     byFlag("password-stdin"),
			"userID":          byArg("user"),
		},
		command: func(r *fieldRecorder, in fieldCoverageInput) runtime.Command {
			return extres.NewCreatePasswordChangeRequest(r, fieldCoverageSource{in.stdin})
		},
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}, stdin: "correctHorseBattery1"},
			want:  map[string]any{"newPassword": "correctHorseBattery1", "userID": "u-abc123"},
		}, {
			input: fieldCoverageInput{args: []string{"u-def456"}, bools: map[string]bool{"password-stdin": true}, stdin: "correctHorseBattery2"},
			want:  map[string]any{"newPassword": "correctHorseBattery2", "userID": "u-def456"},
		}},
	},
	"create cluster": {
		resource:    "provisioning.cattle.io/v1 Cluster",
		gvrResource: "clusters",
		fields: map[string]fieldSource{
			"agentEnvVars":                                         omitted(notYetOffered),
			"cloudCredentialSecretName":                            byFlag("cloud-credential"),
			"clusterAPIConfig":                                     omitted(notYetOffered),
			"clusterAgentDeploymentCustomization":                  omitted(notYetOffered),
			"defaultClusterRoleForProjectMembers":                  omitted("deprecated: Rancher documents the field as unused"),
			"defaultPodSecurityAdmissionConfigurationTemplateName": omitted(notYetOffered),
			"enableNetworkPolicy":                                  omitted(notYetOffered),
			"fleetAgentDeploymentCustomization":                    omitted(notYetOffered),
			"kubernetesVersion":                                    byFlag("k8s-version"),
			"localClusterAuthEndpoint":                             omitted(notYetOffered),
			"redeploySystemAgentGeneration":                        omitted("raised on an existing cluster to redeploy its system agent; a new cluster has no agent to redeploy"),
			"rkeConfig":                                            byFlag("machine-config"),
			"webhookDeploymentCustomization":                       omitted(notYetOffered),
		},
		command: func(r *fieldRecorder, _ fieldCoverageInput) runtime.Command {
			return provisioning.NewCreateCluster(r, fieldCoverageRoots{})
		},
		runs: []fieldCoverageRun{{
			input: fieldCoverageInput{
				args: []string{"node-1"},
				values: map[string]string{
					"machine-config": "digitaloceanconfig/cfg", "cloud-credential": "cc-abc",
					"k8s-version": "v1.30.5+rke2r1", "pool-name": "pool1", "quantity": "1",
				},
				bools: map[string]bool{"etcd": true, "controlplane": true, "worker": true},
			},
			want: map[string]any{
				"cloudCredentialSecretName": "cattle-global-data:cc-abc",
				"kubernetesVersion":         "v1.30.5+rke2r1",
				"rkeConfig": map[string]any{"machinePools": []any{map[string]any{
					"name": "pool1", "etcdRole": true, "controlPlaneRole": true, "workerRole": true,
					"quantity":         int64(1),
					"machineConfigRef": map[string]any{"kind": "DigitaloceanConfig", "name": "cfg"},
				}}},
			},
		}, {
			input: fieldCoverageInput{
				args: []string{"node-2"},
				values: map[string]string{
					"machine-config": "amazonec2config/aws-cfg", "cloud-credential": "cc-def",
					"k8s-version": "v1.31.1+k3s1", "pool-name": "workers", "quantity": "3",
				},
				bools: map[string]bool{"etcd": true, "worker": true},
			},
			want: map[string]any{
				"cloudCredentialSecretName": "cattle-global-data:cc-def",
				"kubernetesVersion":         "v1.31.1+k3s1",
				"rkeConfig": map[string]any{"machinePools": []any{map[string]any{
					"name": "workers", "etcdRole": true, "controlPlaneRole": false, "workerRole": true,
					"quantity":         int64(3),
					"machineConfigRef": map[string]any{"kind": "Amazonec2Config", "name": "aws-cfg"},
				}}},
			},
		}},
	},
}

// createFieldCoverageOutOfScope names the create commands the table does
// not cover, each with the reason.
var createFieldCoverageOutOfScope = map[string]string{
	"create machine-config": "its fields come from each node driver's schema, which the server generates, not from the Rancher API types",
	"create namespace":      "builds a core Kubernetes Namespace, not a Rancher API type",
	"create secret":         "builds a core Kubernetes Secret, not a Rancher API type",
	"create resource":       "takes a whole manifest, so it has no field flags",
}

// TestCreateFieldCoverageClosedWorld asserts every create command is
// either in the coverage table or named out of scope with a reason, so a
// new bespoke create command fails here until it is classified.
func TestCreateFieldCoverageClosedWorld(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var creates []string
	for _, c := range m.Commands {
		if strings.HasPrefix(c.Name, "create ") {
			creates = append(creates, c.Name)
		}
	}
	require.NotEmpty(t, creates)
	for _, name := range creates {
		_, inTable := createFieldCoverageTable[name]
		_, out := createFieldCoverageOutOfScope[name]
		assert.True(t, inTable != out, "%s must be in exactly one of createFieldCoverageTable and createFieldCoverageOutOfScope", name)
	}
	for _, name := range slices.Concat(slices.Collect(maps.Keys(createFieldCoverageTable)), slices.Collect(maps.Keys(createFieldCoverageOutOfScope))) {
		assert.Contains(t, creates, name, "%s is classified but no such command exists", name)
	}
	for name, why := range createFieldCoverageOutOfScope {
		assert.NotEmpty(t, why, "%s needs a reason", name)
	}
}

// TestCreateFieldCoverageMatchesGolden asserts each command's table names
// exactly the golden fields of its resource, each with one source, and
// that every flag and argument it names exists on the command.
func TestCreateFieldCoverageMatchesGolden(t *testing.T) {
	t.Parallel()
	g := loadRancherAPIFields(t)
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	commands := map[string]runtime.SchemaCommand{}
	for _, c := range m.Commands {
		commands[c.Name] = c
	}
	golden := map[string]map[string][]string{}
	for _, r := range g.Resources {
		golden[r.Resource] = r.Fields
	}

	used := map[string]bool{}
	for name, cov := range createFieldCoverageTable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fields, ok := golden[cov.resource]
			require.True(t, ok, "%s: resource %q is not in %s; add it to scripts/rancher-api-fields and run make rancher-api-fields-update", name, cov.resource, rancherAPIFieldsGolden)
			for _, f := range slices.Sorted(maps.Keys(fields)) {
				_, ok := cov.fields[f]
				assert.True(t, ok, "%s: field %q of %s (in %v) is not in its table; name the flag that sets it, or omit it with a reason", name, f, cov.resource, fields[f])
			}
			for _, f := range slices.Sorted(maps.Keys(cov.fields)) {
				_, ok := fields[f]
				assert.True(t, ok, "%s: table entry %q is not a field of %s in %s; remove it", name, f, cov.resource, rancherAPIFieldsGolden)
			}

			cmd, ok := commands[name]
			require.True(t, ok)
			for f, src := range cov.fields {
				n := 0
				for _, s := range []string{src.flag, src.arg, src.omitted} {
					if s != "" {
						n++
					}
				}
				assert.Equal(t, 1, n, "%s: field %q needs exactly one of a flag, an argument or an omission reason", name, f)
				if src.flag != "" {
					assert.True(t, slices.ContainsFunc(cmd.Flags, func(a runtime.ArgSpec) bool { return a.Name == src.flag }),
						"%s: field %q names flag --%s, which the command does not have", name, f, src.flag)
				}
				if src.arg != "" {
					assert.True(t, slices.ContainsFunc(cmd.Args, func(a runtime.ArgSpec) bool { return a.Name == src.arg }),
						"%s: field %q names argument <%s>, which the command does not have", name, f, src.arg)
				}
			}
		})
		used[cov.resource] = true
	}
	for r := range golden {
		assert.True(t, used[r], "%s lists %s, which no create command in the table builds", rancherAPIFieldsGolden, r)
	}
}

// TestCreateFieldCoverageFlagsSetFields runs each command's dry-runs
// against a fake that records the objects sent, and asserts every covered
// field holds exactly the value its run expects. The table itself must
// follow three rules, checked first, so that a field written from anything
// but its own flag or argument fails some run:
//   - the first two runs expect a different value in every covered field
//     (a constant fails one of them);
//   - within a run, no two covered fields expect the same value (a field
//     written from another flag's value fails);
//   - for every two covered boolean fields (absent counts as false), some
//     run expects them equal and some run expects them different (a field
//     written from the other boolean, or from its negation, fails).
//
// A value nested inside one covered field (the machine pool's roles) is
// one field to these rules; its inputs are chosen by hand so that no two
// of them always match (etcd and control plane differ in the second run).
func TestCreateFieldCoverageFlagsSetFields(t *testing.T) {
	t.Parallel()
	g := loadRancherAPIFields(t)
	paths := map[string]string{}
	for _, r := range g.Resources {
		paths[r.Resource] = r.Path
	}
	for name, cov := range createFieldCoverageTable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var covered []string
			for f, src := range cov.fields {
				if src.setsField() {
					covered = append(covered, f)
				}
				if src.flag != "" {
					assert.True(t, cov.runs[0].input.IsSet(src.flag), "%s: the first run must set --%s (field %q)", name, src.flag, f)
				}
			}
			slices.Sort(covered)
			require.GreaterOrEqual(t, len(cov.runs), 2, "%s needs at least two runs", name)
			for i, run := range cov.runs {
				assert.Equal(t, covered, slices.Sorted(maps.Keys(run.want)), "%s: run %d must expect a value for exactly the covered fields", name, i)
			}
			for _, f := range covered {
				assert.NotEqual(t, cov.runs[0].want[f], cov.runs[1].want[f], "%s: the runs must expect different values for %q", name, f)
			}
			isBool := func(f string) bool {
				return slices.ContainsFunc(cov.runs, func(r fieldCoverageRun) bool { _, ok := r.want[f].(bool); return ok })
			}
			boolValue := func(v any) bool { b, _ := v.(bool); return b }
			for a, fa := range covered {
				for _, fb := range covered[a+1:] {
					if isBool(fa) && isBool(fb) {
						equal, differ := false, false
						for _, run := range cov.runs {
							if boolValue(run.want[fa]) == boolValue(run.want[fb]) {
								equal = true
							} else {
								differ = true
							}
						}
						assert.True(t, equal && differ, "%s: some run must expect %q and %q equal, and some run different", name, fa, fb)
						continue
					}
					for i, run := range cov.runs {
						va, vb := run.want[fa], run.want[fb]
						if va == absent || vb == absent {
							continue
						}
						assert.NotEqual(t, va, vb, "%s: run %d expects the same value for %q and %q", name, i, fa, fb)
					}
				}
			}

			var prefix []string
			if p := paths[cov.resource]; p != "" {
				prefix = strings.Split(p, ".")
			}
			for i, run := range cov.runs {
				rec := &fieldRecorder{}
				mc, ok := cov.command(rec, run.input).(runtime.MutatingCommand)
				require.True(t, ok)
				_, _, err := mc.Plan(t.Context(), run.input)
				require.NoError(t, err, "%s: run %d", name, i)
				obj := rec.dryRuns[cov.gvrResource]
				require.NotNil(t, obj, "%s sent no dry-run %s create", name, cov.gvrResource)
				for _, f := range covered {
					path := append(slices.Clone(prefix), f)
					got, found, err := unstructured.NestedFieldNoCopy(obj.Object, path...)
					require.NoError(t, err)
					where := fmt.Sprintf("%s run %d: %s -> %s", name, i, describeSource(cov.fields[f]), strings.Join(path, "."))
					if run.want[f] == absent {
						assert.False(t, found, "%s: want the field left out, got %v", where, got)
						continue
					}
					if assert.True(t, found, "%s: field not set", where) {
						assert.Equal(t, run.want[f], got, where)
					}
				}
			}
		})
	}
}

func describeSource(s fieldSource) string {
	if s.flag != "" {
		return "--" + s.flag
	}
	return "<" + s.arg + ">"
}

// scriptAssignment reads the first NAME="value" line of a script.
func scriptAssignment(t *testing.T, path, name string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^` + name + `="([^"]+)"$`).FindSubmatch(raw)
	require.NotEmptyf(t, m, "%s: no %s=\"...\" assignment", path, name)
	return string(m[1])
}

// imageTag returns the tag of a repo:tag@digest image reference.
func imageTag(t *testing.T, ref string) string {
	t.Helper()
	m := regexp.MustCompile(`^[^@]+:([^:@]+)@sha256:[0-9a-f]+$`).FindStringSubmatch(ref)
	require.NotEmptyf(t, m, "image reference %q is not repo:tag@sha256:digest", ref)
	return m[1]
}

// pseudoVersionRE is the shape of a pkg/apis pseudo-version: no release
// tag, a UTC commit time and the commit's first 12 hex digits.
var pseudoVersionRE = regexp.MustCompile(`^v0\.0\.0-[0-9]{14}-[0-9a-f]{12}$`)

// TestRancherAPIFieldsPinsMatchOracles asserts the tags
// scripts/rancher-api-fields reads are the oracle images' tags in
// scripts/e2e-fidelity, and that the golden was written from them, so
// bumping an oracle image fails until the golden is regenerated. The
// script derives each pseudo-version from its tag; the golden records it.
func TestRancherAPIFieldsPinsMatchOracles(t *testing.T) {
	t.Parallel()
	const fidelity, fields = "../../scripts/e2e-fidelity", "../../scripts/rancher-api-fields"
	tags := map[string]string{
		"community": scriptAssignment(t, fields, "COMMUNITY_TAG"),
		"prime":     scriptAssignment(t, fields, "PRIME_TAG"),
	}
	assert.Equal(t, imageTag(t, scriptAssignment(t, fidelity, "ORACLE_IMAGE")), tags["community"],
		"%s COMMUNITY_TAG must be the tag of ORACLE_IMAGE in %s", fields, fidelity)
	assert.Equal(t, imageTag(t, scriptAssignment(t, fidelity, "PRIME_ORACLE_IMAGE")), tags["prime"],
		"%s PRIME_TAG must be the tag of PRIME_ORACLE_IMAGE in %s", fields, fidelity)

	g := loadRancherAPIFields(t)
	require.Len(t, g.Oracles, len(tags))
	for edition, tag := range tags {
		got, ok := g.Oracles[edition]
		require.True(t, ok, "the golden has no %s oracle", edition)
		assert.Equal(t, tag, got.Tag, "%s: run make rancher-api-fields-update", edition)
		assert.Regexp(t, pseudoVersionRE, got.PseudoVersion, "%s: run make rancher-api-fields-update", edition)
	}
	oracleTags := slices.Collect(maps.Values(tags))
	for _, r := range g.Resources {
		for f, versions := range r.Fields {
			assert.NotEmpty(t, versions, "%s %s", r.Resource, f)
			for _, v := range versions {
				assert.Contains(t, oracleTags, v, "%s %s names a version that is not an oracle", r.Resource, f)
			}
		}
	}
}

// fieldRecorder is the ClusterClient and ClusterClientFactory of the
// field-coverage dry-runs: it records the first dry-run object sent to
// each API resource and answers every create with a copy of what it got.
type fieldRecorder struct {
	dryRuns map[string]*unstructured.Unstructured
}

func (r *fieldRecorder) Create(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if dryRun {
		if r.dryRuns == nil {
			r.dryRuns = map[string]*unstructured.Unstructured{}
		}
		if _, ok := r.dryRuns[gvr.Resource]; !ok {
			r.dryRuns[gvr.Resource] = obj.DeepCopy()
		}
	}
	out := obj.DeepCopy()
	if out.GetName() == "" {
		out.SetName("preview")
	}
	return out, nil
}

// Get answers the cloud-credential lookup of create cluster and the project
// lookup of create secret --project.
func (r *fieldRecorder) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "projects" {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "management.cattle.io/v3", "kind": "Project",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"status":   map[string]any{"backingNamespace": ns + "-" + name},
		}}, nil
	}
	if gvr.Resource == "secrets" {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": name, "namespace": ns},
		}}, nil
	}
	return nil, fmt.Errorf("fieldRecorder: unexpected get %s %s/%s", gvr.Resource, ns, name)
}

func (r *fieldRecorder) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}

func (r *fieldRecorder) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("fieldRecorder: unexpected update")
}

func (r *fieldRecorder) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return errors.New("fieldRecorder: unexpected delete")
}

func (r *fieldRecorder) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("fieldRecorder: unexpected logs")
}

func (r *fieldRecorder) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("fieldRecorder: unexpected review")
}

func (r *fieldRecorder) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("fieldRecorder: unexpected discovery")
}

func (r *fieldRecorder) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return r, nil
}

func (r *fieldRecorder) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return r, nil
}

func (r *fieldRecorder) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

// fieldCoverageRoots has no root client: create cluster's version check
// degrades to no warning without one, and oidc-client's Plan needs none.
type fieldCoverageRoots struct{}

func (fieldCoverageRoots) Root() (engine.RootClient, error) {
	return nil, errors.New("fieldCoverageRoots: no root client")
}

// fieldCoverageSource serves --password-stdin.
type fieldCoverageSource struct{ body string }

func (s fieldCoverageSource) Read(string) ([]byte, error) { return []byte(s.body + "\n"), nil }
