package cli

import (
	"maps"
	"reflect"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outputShapeInventory is the reviewed, closed-world set of every registered
// command's declared OutputShape: a REVIEWED mapping, never automatic
// inference of a future command's shape. Checked bidirectionally against the
// real registry by TestOutputShapeClosedWorldConformance: every registered
// command must appear here with the shape its Descriptor actually declares,
// and every entry must name a command the registry actually has - so an
// unreviewed shape (or a shape that silently drifted) fails the build, the
// same posture as oidcOnceOnlyInventory.
var outputShapeInventory = map[string]runtime.OutputShape{
	"access can":                        runtime.ShapeText,
	"access list":                       runtime.ShapeText,
	"access who-can":                    runtime.ShapeText,
	"access explain":                    runtime.ShapeText,
	"access clusters":                   runtime.ShapeText,
	"access projects":                   runtime.ShapeText,
	"auth delete-token":                 runtime.ShapeText,
	"auth get-token":                    runtime.ShapeExecJSON,
	"auth list-tokens":                  runtime.ShapeTabular,
	"auth login":                        runtime.ShapeText,
	"auth logout":                       runtime.ShapeText,
	"auth status":                       runtime.ShapeText,
	"cluster clear-autoscaling":         runtime.ShapeText,
	"cluster pause-autoscaling":         runtime.ShapeText,
	"cluster resume-autoscaling":        runtime.ShapeText,
	"cluster scale":                     runtime.ShapeText,
	"cluster set-autoscaling":           runtime.ShapeText,
	"completion":                        runtime.ShapeBytes,
	"config add-server":                 runtime.ShapeText,
	"config delete-server":              runtime.ShapeText,
	"config edit":                       runtime.ShapeText,
	"config list-servers":               runtime.ShapeTabular,
	"config path":                       runtime.ShapeText,
	"config set-completion":             runtime.ShapeText,
	"config set-token-reuse":            runtime.ShapeText,
	"telemetry status":                  runtime.ShapeText,
	"telemetry on":                      runtime.ShapeText,
	"telemetry off":                     runtime.ShapeText,
	"telemetry reset":                   runtime.ShapeText,
	"telemetry show":                    runtime.ShapeTabular,
	"telemetry upload":                  runtime.ShapeText,
	"config status":                     runtime.ShapeText,
	"config update-server":              runtime.ShapeText,
	"config use-server":                 runtime.ShapeText,
	"create cluster":                    runtime.ShapeText,
	"create kubeconfig":                 runtime.ShapeBytes,
	"create machine-config":             runtime.ShapeText,
	"create namespace":                  runtime.ShapeText,
	"create oidc-client":                runtime.ShapeText,
	"create password-change-request":    runtime.ShapeText,
	"create project":                    runtime.ShapeText,
	"create resource":                   runtime.ShapeText,
	"create secret":                     runtime.ShapeText,
	"create token":                      runtime.ShapeText,
	"create user":                       runtime.ShapeText,
	"delete amazonec2config":            runtime.ShapeText,
	"delete azureconfig":                runtime.ShapeText,
	"delete clusterroletemplatebinding": runtime.ShapeText,
	"delete configmap":                  runtime.ShapeText,
	"delete cronjob":                    runtime.ShapeText,
	"delete daemonset":                  runtime.ShapeText,
	"delete deployment":                 runtime.ShapeText,
	"delete digitaloceanconfig":         runtime.ShapeText,
	"delete gitrepo":                    runtime.ShapeText,
	"delete globalrolebinding":          runtime.ShapeText,
	"delete harvesterconfig":            runtime.ShapeText,
	"delete helmop":                     runtime.ShapeText,
	"delete horizontalpodautoscaler":    runtime.ShapeText,
	"delete ingress":                    runtime.ShapeText,
	"delete job":                        runtime.ShapeText,
	"delete k3k-cluster":                runtime.ShapeText,
	"delete kubeconfig":                 runtime.ShapeText,
	"delete limitrange":                 runtime.ShapeText,
	"delete management-token":           runtime.ShapeText,
	"delete namespace":                  runtime.ShapeText,
	"delete networkpolicy":              runtime.ShapeText,
	"delete oidcclient":                 runtime.ShapeText,
	"delete persistentvolumeclaim":      runtime.ShapeText,
	"delete pod":                        runtime.ShapeText,
	"delete poddisruptionbudget":        runtime.ShapeText,
	"delete projectroletemplatebinding": runtime.ShapeText,
	"delete project":                    runtime.ShapeText,
	"delete provisioning-cluster":       runtime.ShapeText,
	"delete replicaset":                 runtime.ShapeText,
	"delete resourcequota":              runtime.ShapeText,
	"delete secret":                     runtime.ShapeText,
	"delete serviceaccount":             runtime.ShapeText,
	"delete service":                    runtime.ShapeText,
	"delete statefulset":                runtime.ShapeText,
	"delete storageclass":               runtime.ShapeText,
	"delete token":                      runtime.ShapeText,
	"delete user":                       runtime.ShapeText,
	"delete vmwarevsphereconfig":        runtime.ShapeText,
	"get amazonec2config":               runtime.ShapeTabular,
	"get azureconfig":                   runtime.ShapeTabular,
	"get bundledeployment":              runtime.ShapeTabular,
	"get bundle":                        runtime.ShapeTabular,
	"get capi-cluster":                  runtime.ShapeTabular,
	"get cloud-credential":              runtime.ShapeTabular,
	"get clustergroup":                  runtime.ShapeTabular,
	"get clusterregistrationtoken":      runtime.ShapeTabular,
	"get clusterroletemplatebinding":    runtime.ShapeTabular,
	"get cluster":                       runtime.ShapeTabular,
	"get configmap":                     runtime.ShapeTabular,
	"get cronjob":                       runtime.ShapeTabular,
	"get daemonset":                     runtime.ShapeTabular,
	"get deployment":                    runtime.ShapeTabular,
	"get digitaloceanconfig":            runtime.ShapeTabular,
	"get etcd-snapshot":                 runtime.ShapeTabular,
	"get event":                         runtime.ShapeTabular,
	"get feature":                       runtime.ShapeTabular,
	"get fleet-cluster":                 runtime.ShapeTabular,
	"get fleet-workspace":               runtime.ShapeTabular,
	"get gitrepo":                       runtime.ShapeTabular,
	"get globalrolebinding":             runtime.ShapeTabular,
	"get globalrole":                    runtime.ShapeTabular,
	"get harvesterconfig":               runtime.ShapeTabular,
	"get helmop":                        runtime.ShapeTabular,
	"get horizontalpodautoscaler":       runtime.ShapeTabular,
	"get ingress":                       runtime.ShapeTabular,
	"get job":                           runtime.ShapeTabular,
	"get k3k-cluster":                   runtime.ShapeTabular,
	"get app":                           runtime.ShapeTabular,
	"get clusterrepo":                   runtime.ShapeTabular,
	"get limitrange":                    runtime.ShapeTabular,
	"get networkpolicy":                 runtime.ShapeTabular,
	"get operation":                     runtime.ShapeTabular,
	"get kubeconfig":                    runtime.ShapeTabular,
	"get kubernetes-versions":           runtime.ShapeTabular,
	"get machinedeployment":             runtime.ShapeTabular,
	"get machineset":                    runtime.ShapeTabular,
	"get machine":                       runtime.ShapeTabular,
	"get management-token":              runtime.ShapeTabular,
	"get namespace":                     runtime.ShapeTabular,
	"get nodedriver":                    runtime.ShapeTabular,
	"get node":                          runtime.ShapeTabular,
	"get oidcclient":                    runtime.ShapeTabular,
	"get persistentvolumeclaim":         runtime.ShapeTabular,
	"get persistentvolume":              runtime.ShapeTabular,
	"get pod":                           runtime.ShapeTabular,
	"get poddisruptionbudget":           runtime.ShapeTabular,
	"get projectroletemplatebinding":    runtime.ShapeTabular,
	"get project":                       runtime.ShapeTabular,
	"get provisioning-cluster":          runtime.ShapeTabular,
	"get replicaset":                    runtime.ShapeTabular,
	"get resourcequota":                 runtime.ShapeTabular,
	"get roletemplate":                  runtime.ShapeTabular,
	"get secret":                        runtime.ShapeTabular,
	"get serviceaccount":                runtime.ShapeTabular,
	"get service":                       runtime.ShapeTabular,
	"get setting":                       runtime.ShapeTabular,
	"get statefulset":                   runtime.ShapeTabular,
	"get storageclass":                  runtime.ShapeTabular,
	"get token":                         runtime.ShapeTabular,
	"get user":                          runtime.ShapeTabular,
	"get vmwarevsphereconfig":           runtime.ShapeTabular,
	"charts":                            runtime.ShapeTabular,
	"grant":                             runtime.ShapeText,
	"images":                            runtime.ShapeTabular,
	"inspect cluster":                   runtime.ShapeText,
	"inspect deployment":                runtime.ShapeText,
	"inspect pod":                       runtime.ShapeTabular,
	"inspect project":                   runtime.ShapeText,
	"inspect user":                      runtime.ShapeText,
	"kubeconfig":                        runtime.ShapeBytes,
	"logs":                              runtime.ShapeText,
	"oidc-client add-secret":            runtime.ShapeText,
	"oidc-client remove-secret":         runtime.ShapeText,
	"oidc-client rotate-secret":         runtime.ShapeText,
	"patch amazonec2config":             runtime.ShapeText,
	"patch azureconfig":                 runtime.ShapeText,
	"patch bundledeployment":            runtime.ShapeText,
	"patch bundle":                      runtime.ShapeText,
	"patch capi-cluster":                runtime.ShapeText,
	"patch clustergroup":                runtime.ShapeText,
	"patch clusterregistrationtoken":    runtime.ShapeText,
	"patch clusterroletemplatebinding":  runtime.ShapeText,
	"patch cluster":                     runtime.ShapeText,
	"patch configmap":                   runtime.ShapeText,
	"patch cronjob":                     runtime.ShapeText,
	"patch daemonset":                   runtime.ShapeText,
	"patch deployment":                  runtime.ShapeText,
	"patch digitaloceanconfig":          runtime.ShapeText,
	"patch etcd-snapshot":               runtime.ShapeText,
	"patch event":                       runtime.ShapeText,
	"patch feature":                     runtime.ShapeText,
	"patch fleet-cluster":               runtime.ShapeText,
	"patch fleet-workspace":             runtime.ShapeText,
	"patch gitrepo":                     runtime.ShapeText,
	"patch globalrolebinding":           runtime.ShapeText,
	"patch globalrole":                  runtime.ShapeText,
	"patch harvesterconfig":             runtime.ShapeText,
	"patch helmop":                      runtime.ShapeText,
	"patch horizontalpodautoscaler":     runtime.ShapeText,
	"patch ingress":                     runtime.ShapeText,
	"patch job":                         runtime.ShapeText,
	"patch k3k-cluster":                 runtime.ShapeText,
	"patch app":                         runtime.ShapeText,
	"patch clusterrepo":                 runtime.ShapeText,
	"patch limitrange":                  runtime.ShapeText,
	"patch networkpolicy":               runtime.ShapeText,
	"patch operation":                   runtime.ShapeText,
	"patch kubeconfig":                  runtime.ShapeText,
	"patch machinedeployment":           runtime.ShapeText,
	"patch machineset":                  runtime.ShapeText,
	"patch machine":                     runtime.ShapeText,
	"patch management-token":            runtime.ShapeText,
	"patch namespace":                   runtime.ShapeText,
	"patch nodedriver":                  runtime.ShapeText,
	"patch node":                        runtime.ShapeText,
	"patch oidcclient":                  runtime.ShapeText,
	"patch persistentvolumeclaim":       runtime.ShapeText,
	"patch persistentvolume":            runtime.ShapeText,
	"patch pod":                         runtime.ShapeText,
	"patch poddisruptionbudget":         runtime.ShapeText,
	"patch projectroletemplatebinding":  runtime.ShapeText,
	"patch project":                     runtime.ShapeText,
	"patch provisioning-cluster":        runtime.ShapeText,
	"patch replicaset":                  runtime.ShapeText,
	"patch resourcequota":               runtime.ShapeText,
	"patch roletemplate":                runtime.ShapeText,
	"patch secret":                      runtime.ShapeText,
	"patch serviceaccount":              runtime.ShapeText,
	"patch service":                     runtime.ShapeText,
	"patch setting":                     runtime.ShapeText,
	"patch statefulset":                 runtime.ShapeText,
	"patch storageclass":                runtime.ShapeText,
	"patch token":                       runtime.ShapeText,
	"patch user":                        runtime.ShapeText,
	"patch vmwarevsphereconfig":         runtime.ShapeText,
	"project add-namespace":             runtime.ShapeText,
	"project remove-namespace":          runtime.ShapeText,
	"revoke":                            runtime.ShapeText,
	"schema":                            runtime.ShapeText,
	"skill install":                     runtime.ShapeTabular,
	"skill status":                      runtime.ShapeTabular,
	"skill uninstall":                   runtime.ShapeTabular,
	"support-bundle":                    runtime.ShapeText,
	"token":                             runtime.ShapeExecJSON,
	"usage":                             runtime.ShapeTabular,
}

// bytesAndExecJSONOutputPolicy is the reviewed, closed-world OutputPolicy for
// every ShapeBytes/ShapeExecJSON command: a RawBytes or bare-JSON payload
// never varies with -o, so an unhonored format is a conformance failure
// rather than a silently ignored value. kubeconfig and completion follow the
// create-kubeconfig precedent (table only); auth get-token and the token
// shim assert the two exec-credential formats they have always honored.
var bytesAndExecJSONOutputPolicy = map[string]runtime.OutputPolicy{
	"kubeconfig":        {AllowedFormats: []string{"table"}},
	"create kubeconfig": {AllowedFormats: []string{"table"}},
	"completion":        {AllowedFormats: []string{"table"}},
	"auth get-token":    {AllowedFormats: []string{"table", "json"}},
	"token":             {AllowedFormats: []string{"table", "json"}},
}

// TestOutputShapeClosedWorldConformance asserts the declared OutputShape
// bidirectionally over the real, registered command set: every registered
// command must have a reviewed inventory entry with the exact shape its
// Descriptor declares, and every inventory entry must name a command the
// registry actually has - so a new command without a reviewed shape (or a
// shape that silently drifted from the reviewed value) fails the build.
func TestOutputShapeClosedWorldConformance(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	seen := map[string]bool{}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		seen[d.Name] = true

		want, ok := outputShapeInventory[d.Name]
		if !assert.True(t, ok, "command %q has no reviewed entry in outputShapeInventory - add one deliberately", d.Name) {
			continue
		}
		assert.Equal(t, want, d.Shape, "command %q declares shape %s but the reviewed inventory says %s", d.Name, d.Shape, want)
		assert.NotEqual(t, runtime.ShapeUnspecified, d.Shape, "command %q must never declare ShapeUnspecified", d.Name)

		if want, ok := bytesAndExecJSONOutputPolicy[d.Name]; ok {
			assert.True(t, reflect.DeepEqual(want, d.OutputPolicy),
				"command %q declares OutputPolicy %+v but the reviewed inventory says %+v", d.Name, d.OutputPolicy, want)
		}
	}

	for name := range outputShapeInventory {
		assert.True(t, seen[name], "outputShapeInventory entry %q names no registered command - remove it or check the spelling", name)
	}
}

// TestOutputShapeClosedWorldConformanceCatchesADeletedEntry proves the
// bidirectional check actually fails when a reviewed entry goes missing
// from the inventory - the mechanism once_only_conformance_test.go's
// pattern relies on, exercised directly here rather than only by manual
// verification.
func TestOutputShapeClosedWorldConformanceCatchesADeletedEntry(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	trimmed := map[string]runtime.OutputShape{}
	maps.Copy(trimmed, outputShapeInventory)
	delete(trimmed, "get pod")

	var missing bool
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if _, ok := trimmed[d.Name]; !ok {
			missing = true
			break
		}
	}
	assert.True(t, missing, "removing a registered command's inventory entry must be detectable")
}
