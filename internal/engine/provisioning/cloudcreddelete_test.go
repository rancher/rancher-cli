package provisioning

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ccdFull          = "cattle-global-data:" + ccpID
	ccdManagement    = "management.cattle.io"
	ccdProvisioning  = "provisioning.cattle.io"
	ccdProvisionPath = "/v1/provisioning.cattle.io.clusters"
	ccdManagePath    = "/v1/management.cattle.io.clusters"
)

// ccdRoot is the root client of a test: it responds to each path from a map and
// records the requests.
type ccdRoot struct {
	mu      sync.Mutex
	answers map[string]any
	status  int
	paths   []string
}

func (r *ccdRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	if r.status != 0 {
		return nil, clierr.ResponseError(http.MethodGet, r.status, nil)
	}
	answer, ok := r.answers[path]
	if !ok {
		return nil, clierr.ResponseError(http.MethodGet, http.StatusNotFound, nil)
	}
	return json.Marshal(answer)
}

func (r *ccdRoot) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

type ccdRoots struct{ root *ccdRoot }

func (f ccdRoots) Root() (engine.RootClient, error) { return f.root, nil }

// ccdForbidden is the error of a Kubernetes List the caller may not make.
func ccdForbidden() error {
	status := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden, Message: "the server said no"}}
	return clierr.New(clierr.CodeRBAC, "kubernetes responded with Forbidden").WithCause(status)
}

func ccdSteveItems(items ...map[string]any) map[string]any {
	data := make([]any, len(items))
	for i, item := range items {
		data[i] = item
	}
	return map[string]any{"data": data, "count": len(items), "revision": "1", "type": "collection"}
}

func ccdProvisioningCluster(ns, name string, spec map[string]any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec,
	}}
}

func ccdManagementCluster(id, display string, spec map[string]any) unstructured.Unstructured {
	spec["displayName"] = display
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": id},
		"spec":     spec,
	}}
}

// ccdRig is a delete command over a rig that has one stored credential.
type ccdRig struct {
	*ccpRig
	del  *deleteCloudCredentialCmd
	root *ccdRoot
}

func newCCDRig(base *ccwRig, driver string) *ccdRig {
	r := &ccdRig{ccpRig: newCCPRig(base, driver), root: &ccdRoot{answers: map[string]any{}}}
	r.del = NewDeleteCloudCredential(fakeFactory{c: r.client}, ccdRoots{root: r.root}, r.norman).(*deleteCloudCredentialCmd)
	// The server deletes the Secret when it accepts the delete.
	r.norman.onDelete = func() { r.change(nil) }
	return r
}

func (r *ccdRig) listClusters(group string, items ...unstructured.Unstructured) {
	r.client.clusterLists = cloneLists(r.client.clusterLists)
	r.client.clusterLists[group] = func() (*unstructured.UnstructuredList, error) {
		return &unstructured.UnstructuredList{Items: items}, nil
	}
}

func (r *ccdRig) refuseClusters(group string, err error) {
	r.client.clusterLists = cloneLists(r.client.clusterLists)
	r.client.clusterLists[group] = func() (*unstructured.UnstructuredList, error) { return nil, err }
}

func cloneLists(m map[string]func() (*unstructured.UnstructuredList, error)) map[string]func() (*unstructured.UnstructuredList, error) {
	out := map[string]func() (*unstructured.UnstructuredList, error){}
	maps.Copy(out, m)
	return out
}

func (r *ccdRig) plan(t *testing.T, in ccwInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return r.del.Plan(t.Context(), in)
}

// run sends the delete through the pipeline and returns what it wrote to the
// output, the output, and its error.
func (r *ccdRig) run(t *testing.T, in ccwInput, format string, dryRun, yes bool) (written, stdout string, ce *clierr.Error) {
	t.Helper()
	var out, errOut bytes.Buffer
	_, ce = runtime.Run(t.Context(), r.del, in, runtime.IO{Out: &out, Err: &errOut, Format: format, DryRun: dryRun, Apply: !dryRun, Yes: yes}, safety.Binding{})
	written = out.String() + errOut.String()
	if ce != nil {
		raw, _ := json.Marshal(ce)
		written += ce.Error() + ce.Message + ce.Hint + string(raw)
	}
	return written, out.String(), ce
}

// TestCloudCredentialDeleteReferences asserts the preview of a delete refuses a
// provisioning cluster that names the credential for the whole cluster or a
// machine pool, by the exact id form Rancher compares, with the namespace after
// Rancher's text; and that the references Rancher does not check are warnings.
func TestCloudCredentialDeleteReferences(t *testing.T) {
	t.Parallel()
	pool := func(value string) map[string]any {
		return map[string]any{"rkeConfig": map[string]any{"machinePools": []any{
			map[string]any{"name": "other", "cloudCredentialSecretName": "cattle-global-data:cc-other"},
			map[string]any{"name": "pool", "cloudCredentialSecretName": value},
		}}}
	}
	for _, tc := range []struct {
		name     string
		clusters []unstructured.Unstructured
		want     string
	}{
		{"the whole cluster", []unstructured.Unstructured{ccdProvisioningCluster("fleet-default", "prod", map[string]any{"cloudCredentialSecretName": ccdFull})}, "prod|fleet-default"},
		{"a machine pool", []unstructured.Unstructured{ccdProvisioningCluster("fleet-default", "prod", pool(ccdFull))}, "prod|fleet-default"},
		{"the first by namespace and name", []unstructured.Unstructured{
			ccdProvisioningCluster("fleet-default", "zeta", map[string]any{"cloudCredentialSecretName": ccdFull}),
			ccdProvisioningCluster("fleet-default", "alpha", map[string]any{"cloudCredentialSecretName": ccdFull}),
			ccdProvisioningCluster("fleet-aaa", "omega", map[string]any{"cloudCredentialSecretName": ccdFull}),
		}, "omega|fleet-aaa"},
		{"a near match of the whole cluster", []unstructured.Unstructured{ccdProvisioningCluster("fleet-default", "prod", map[string]any{"cloudCredentialSecretName": ccdFull + "x"})}, ""},
		{"a near match of a machine pool", []unstructured.Unstructured{ccdProvisioningCluster("fleet-default", "prod", pool(ccdFull+"x"))}, ""},
		{"the secret name alone", []unstructured.Unstructured{ccdProvisioningCluster("fleet-default", "prod", map[string]any{"cloudCredentialSecretName": ccpID})}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.listClusters(ccdProvisioning, tc.clusters...)
			_, _, err := r.plan(t, ccpInputOf())
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			ce := clierr.From(err)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			name, ns, _ := strings.Cut(tc.want, "|")
			assert.Equal(t, "Cloud credential is currently referenced by provisioning cluster "+name+" (namespace "+ns+")", ce.Message)
		})
	}

	t.Run("a refusal reads no management cluster", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		r.listClusters(ccdProvisioning, ccdProvisioningCluster("fleet-default", "prod", map[string]any{"cloudCredentialSecretName": ccdFull}))
		_, _, err := r.plan(t, ccpInputOf())
		require.Error(t, err)
		assert.Equal(t, []string{ccdProvisioning}, r.client.clusterListGroups)
	})

	t.Run("the references Rancher does not check are warnings", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		s3 := func(value string) map[string]any {
			return map[string]any{"rkeConfig": map[string]any{"etcd": map[string]any{"s3": map[string]any{"cloudCredentialName": value}}}}
		}
		r.listClusters(ccdProvisioning,
			ccdProvisioningCluster("fleet-default", "full", s3(ccdFull)),
			ccdProvisioningCluster("fleet-default", "bare", s3(ccpID)),
			ccdProvisioningCluster("fleet-default", "near", s3(ccpID+"x")),
			ccdProvisioningCluster("fleet-default", "none", map[string]any{}))
		r.listClusters(ccdManagement,
			ccdManagementCluster("c-eks", "eks one", map[string]any{"eksConfig": map[string]any{"amazonCredentialSecret": ccdFull}}),
			ccdManagementCluster("c-aks", "aks one", map[string]any{"aksConfig": map[string]any{"azureCredentialSecret": ccpID}}),
			ccdManagementCluster("c-gke", "gke one", map[string]any{"gkeConfig": map[string]any{"googleCredentialSecret": ccdFull}}),
			ccdManagementCluster("c-ali", "ali one", map[string]any{"aliConfig": map[string]any{"alibabaCredentialSecret": ccdFull}}),
			ccdManagementCluster("c-near", "near one", map[string]any{"eksConfig": map[string]any{"amazonCredentialSecret": ccdFull + "x"}}))
		eff, _, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		const tail = "; Rancher does not check that reference and deletes the credential"
		assert.Equal(t, []string{
			"previewed from server reads: Rancher checks the request only when it is sent",
			"--apply reads the cloud credential again and stops if it changed; a change after that read is not detected",
			"cluster aks one (c-aks) uses this cloud credential in spec.aksConfig.azureCredentialSecret" + tail,
			"cluster ali one (c-ali) uses this cloud credential in spec.aliConfig.alibabaCredentialSecret" + tail,
			"cluster eks one (c-eks) uses this cloud credential in spec.eksConfig.amazonCredentialSecret" + tail,
			"cluster gke one (c-gke) uses this cloud credential in spec.gkeConfig.googleCredentialSecret" + tail,
			"provisioning cluster bare in namespace fleet-default uses this cloud credential for etcd snapshots in S3 (spec.rkeConfig.etcd.s3.cloudCredentialName)" + tail,
			"provisioning cluster full in namespace fleet-default uses this cloud credential for etcd snapshots in S3 (spec.rkeConfig.etcd.s3.cloudCredentialName)" + tail,
		}, eff.Warnings)
	})

	t.Run("a refused List is a warning", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			prepare func(*ccdRig)
			want    []string
		}{
			"both, with no root client": {func(r *ccdRig) {
				r.del.roots = nil
				r.refuseClusters(ccdProvisioning, ccdForbidden())
				r.refuseClusters(ccdManagement, ccdForbidden())
			}, []string{ccdProvisionedWarning, ccdManagedWarning}},
			"both, and Steve refuses too": {func(r *ccdRig) {
				r.root.status = http.StatusForbidden
				r.refuseClusters(ccdProvisioning, ccdForbidden())
				r.refuseClusters(ccdManagement, ccdForbidden())
			}, []string{ccdProvisionedWarning, ccdManagedWarning}},
			"both, and Steve does not have the type": {func(r *ccdRig) {
				r.root.status = http.StatusNotFound
				r.refuseClusters(ccdProvisioning, ccdForbidden())
				r.refuseClusters(ccdManagement, ccdForbidden())
			}, []string{ccdProvisionedWarning, ccdManagedWarning}},
			"the management clusters only": {func(r *ccdRig) {
				r.refuseClusters(ccdManagement, ccdForbidden())
			}, []string{ccdManagedWarning}},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newCCDRig(newCCWRig(nil), "amazonec2")
				tc.prepare(r)
				eff, _, err := r.plan(t, ccpInputOf())
				require.NoError(t, err)
				assert.Equal(t, tc.want, eff.Warnings[2:])
			})
		}
	})

	t.Run("Steve responds after a Kubernetes 403", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		r.refuseClusters(ccdProvisioning, ccdForbidden())
		r.refuseClusters(ccdManagement, ccdForbidden())
		r.root.answers[ccdManagePath] = ccdSteveItems(map[string]any{
			"id": "c-eks", "metadata": map[string]any{"name": "c-eks"},
			"spec": map[string]any{"displayName": "eks one", "eksConfig": map[string]any{"amazonCredentialSecret": ccdFull}},
		})
		r.root.answers[ccdProvisionPath] = ccdSteveItems(map[string]any{
			"id": "fleet-default/prod", "metadata": map[string]any{"name": "prod", "namespace": "fleet-default"},
			"spec": map[string]any{"rkeConfig": map[string]any{"etcd": map[string]any{"s3": map[string]any{"cloudCredentialName": ccdFull}}}},
		})
		eff, _, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		assert.Len(t, eff.Warnings, 6)
		assert.Contains(t, eff.Warnings[2], "cluster eks one (c-eks) uses this cloud credential in spec.eksConfig.amazonCredentialSecret")
		assert.Contains(t, eff.Warnings[3], "provisioning cluster prod in namespace fleet-default uses this cloud credential for etcd snapshots in S3")
		assert.Equal(t, []string{ccdProvisionedReadable, ccdManagedReadable}, eff.Warnings[4:])
		assert.ElementsMatch(t, []string{ccdProvisionPath, ccdManagePath}, r.root.requested())
	})

	t.Run("a Steve response warns that only the clusters the caller has permission to get were checked", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			forbidden []string
			want      []string
		}{
			"the provisioning clusters": {[]string{ccdProvisioning}, []string{ccdProvisionedReadable}},
			"the management clusters":   {[]string{ccdManagement}, []string{ccdManagedReadable}},
			"both":                      {[]string{ccdProvisioning, ccdManagement}, []string{ccdProvisionedReadable, ccdManagedReadable}},
			"neither":                   {nil, []string{}},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newCCDRig(newCCWRig(nil), "amazonec2")
				for _, group := range tc.forbidden {
					r.refuseClusters(group, ccdForbidden())
				}
				r.root.answers[ccdProvisionPath] = ccdSteveItems()
				r.root.answers[ccdManagePath] = ccdSteveItems()
				eff, _, err := r.plan(t, ccpInputOf())
				require.NoError(t, err)
				assert.Equal(t, tc.want, eff.Warnings[2:])
			})
		}
	})

	t.Run("a Steve error other than 403 or 404 ends the preview", func(t *testing.T) {
		t.Parallel()
		for _, group := range []string{ccdProvisioning, ccdManagement} {
			for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
				t.Run(group+" "+strconv.Itoa(status), func(t *testing.T) {
					t.Parallel()
					r := newCCDRig(newCCWRig(nil), "amazonec2")
					r.root.status = status
					r.refuseClusters(group, ccdForbidden())
					eff, _, err := r.plan(t, ccpInputOf())
					require.Error(t, err)
					assert.Empty(t, eff.Warnings, "no preview says the references were checked")
					assert.NotEmpty(t, r.root.requested(), "Steve is read after the Kubernetes 403")
					assert.Empty(t, r.norman.deletes)
				})
			}
		}
	})

	t.Run("Steve refuses a cluster that names the credential", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		r.refuseClusters(ccdProvisioning, ccdForbidden())
		r.root.answers[ccdProvisionPath] = ccdSteveItems(map[string]any{
			"id": "fleet-default/prod", "metadata": map[string]any{"name": "prod", "namespace": "fleet-default"},
			"spec": map[string]any{"cloudCredentialSecretName": ccdFull},
		})
		_, _, err := r.plan(t, ccpInputOf())
		require.ErrorContains(t, err, "currently referenced by provisioning cluster prod (namespace fleet-default)")
	})

	for _, group := range []string{ccdProvisioning, ccdManagement} {
		t.Run("another error of the List of "+group+" ends the preview", func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.refuseClusters(group, clierr.New(clierr.CodeGeneric, "the server responded with 500: the server is down"))
			_, _, err := r.plan(t, ccpInputOf())
			require.ErrorContains(t, err, "the server is down")
			assert.Empty(t, r.root.requested(), "Steve is read only after a 403")
			assert.Empty(t, r.norman.deletes)
		})
	}

	t.Run("Steve is read only after a Kubernetes 403", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		_, _, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		assert.Empty(t, r.root.requested())
	})
}

const (
	ccdServerReads = "previewed from server reads: Rancher checks the request only when it is sent"
	ccdReadsAgain  = "--apply reads the cloud credential again and stops if it changed; a change after that read is not detected"

	ccdProvisionedWarning = "the provisioning clusters were not checked for references to this cloud credential because you do not have permission to list them; Rancher refuses the delete when a provisioning cluster uses it"
	ccdManagedWarning     = "the hosted clusters (EKS, AKS, GKE and Alibaba ACK) were not checked for references to this cloud credential because you do not have permission to list the management clusters"

	ccdProvisionedReadable = "only the provisioning clusters you have permission to get were checked for references to this cloud credential"
	ccdManagedReadable     = "only the management clusters you have permission to get were checked for references to this cloud credential"
)

// TestCloudCredentialDeleteEffect asserts the Effect of a delete: its identity,
// the resourceVersion it read with no conflict detection, no diff, and the
// fixed warnings.
func TestCloudCredentialDeleteEffect(t *testing.T) {
	t.Parallel()
	r := newCCDRig(newCCWRig(nil), "amazonec2")
	eff, _, err := r.plan(t, ccpInputOf())
	require.NoError(t, err)
	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.Equal(t, "cloudcredentials", eff.Target)
	assert.Equal(t, "cloud-credential", eff.DisplayKind)
	assert.Equal(t, ccpID, eff.DisplayName)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.False(t, eff.ConflictDetection)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	assert.Empty(t, eff.Diff)
	assert.Equal(t, []string{
		"previewed from server reads: Rancher checks the request only when it is sent",
		"--apply reads the cloud credential again and stops if it changed; a change after that read is not detected",
	}, eff.Warnings)
	assert.Equal(t, "true", r.del.Describe().Destructive)
	assert.False(t, r.del.Describe().SecretInput)

	t.Run("the prefixed id of a credential that is another's display name", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		other := secretObj("cc-other", map[string]string{"field.cattle.io/name": ccpID}, map[string]any{"amazonec2credentialConfig-x": "x"})
		r.client.secrets = listOf(ccpStored("amazonec2", "7"), other)
		eff, _, err := r.plan(t, ccwInput{args: []string{"cattle-global-data:" + ccpID}})
		require.NoError(t, err)
		assert.Len(t, eff.Warnings, 2, "the selection adds no warning")
		assert.Equal(t, ccpID, eff.DisplayName)
	})
}

// TestCloudCredentialDeleteWaitFlagsBeforeAnyRequest asserts a bad --timeout and
// --wait=false with --timeout are refused before the credential is read.
func TestCloudCredentialDeleteWaitFlagsBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   func() ccwInput
		want string
	}{
		"a bad timeout": {func() ccwInput {
			in := ccpInputOf()
			in.timeout = "soon"
			return in
		}, "--timeout"},
		"wait=false with a timeout": {func() ccwInput {
			in := ccpInputOf()
			in.wait = new(false)
			in.timeout = "1m"
			return in
		}, "--wait=false conflicts with --timeout; remove one"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			_, _, err := r.plan(t, tc.in())
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, r.client.gets)
			assert.Empty(t, r.client.clusterListGroups)
		})
	}
}

// TestCloudCredentialDeleteDryRunSendsNoWrite asserts a preview sends no
// DELETE on any path, a refused List included.
func TestCloudCredentialDeleteDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(*ccdRig){
		"plain": func(*ccdRig) {},
		"a refused List of provisioning clusters": func(r *ccdRig) {
			r.refuseClusters(ccdProvisioning, ccdForbidden())
		},
		"a refused List of management clusters": func(r *ccdRig) {
			r.refuseClusters(ccdManagement, ccdForbidden())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			prepare(r)
			_, _, ce := r.run(t, ccpInputOf(), "json", true, false)
			require.Nil(t, ce)
			assert.Empty(t, r.norman.deletes)
			assert.Empty(t, r.norman.updates)
		})
	}
}

// TestCloudCredentialDeleteGates asserts a delete needs --apply and --yes.
func TestCloudCredentialDeleteGates(t *testing.T) {
	t.Parallel()
	r := newCCDRig(newCCWRig(nil), "amazonec2")
	_, _, ce := r.run(t, ccpInputOf(), "json", false, false)
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
	assert.Empty(t, r.norman.deletes)

	_, _, ce = r.run(t, ccpInputOf(), "json", false, true)
	require.Nil(t, ce)
	assert.Equal(t, []string{ccdFull}, r.norman.deletes)
}

// TestCloudCredentialDeleteReReadRefusesAChange asserts a delete reads the
// Secret again before its one request: a different resourceVersion ends in
// CONFLICT, retryable, with nothing sent; a Secret that is gone is NOT_FOUND
// with the hint to list the credentials.
func TestCloudCredentialDeleteReReadRefusesAChange(t *testing.T) {
	t.Parallel()
	t.Run("a different resourceVersion", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		r.change(ccpStored("amazonec2", "8"))
		_, err = exec(t.Context())
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "the cloud credential changed since the preview; try again", ce.Message)
		assert.Empty(t, r.norman.deletes)
	})
	t.Run("a Secret that is gone", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		r.change(nil)
		_, err = exec(t.Context())
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "cloud credential not found: "+ccpID, ce.Message)
		assert.Contains(t, ce.Hint, "rancher get cloud-credential")
		assert.Empty(t, r.norman.deletes)
	})
	t.Run("an unchanged Secret", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, ccpInputOf())
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{ccdFull}, r.norman.deletes)
		assert.Equal(t, "CloudCredential", res.Kind)
		assert.Equal(t, createdCredentialRows{{ID: ccdFull, SecretName: ccpID, DisplayName: "prod", Driver: "amazonec2"}}, res.Data)
	})
}

// TestCloudCredentialDeleteResponseClasses asserts the code, the retry advice,
// the text and the hint of each response a DELETE can get.
func TestCloudCredentialDeleteResponseClasses(t *testing.T) {
	t.Parallel()
	request := &clierr.Request{Server: "https://rancher.test"}
	refused := func(status int, body string) error {
		return &engine.ActionRefused{Err: clierr.FromResponse("DELETE x on the Rancher server", http.MethodDelete, status, nil, request), Body: []byte(body), Request: request}
	}
	unknown := func(status int, body string) error {
		return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodDelete, status, nil), Body: []byte(body)}
	}
	inUse := `{"type":"error","code":"InvalidAction","message":"Cloud credential is currently referenced by provisioning cluster prod"}`
	for _, tc := range []struct {
		name      string
		err       error
		code      string
		retryable bool
		contains  string
		hint      string
	}{
		{"401", refused(http.StatusUnauthorized, ""), clierr.CodeAuth, false, "", ""},
		{"403", refused(http.StatusForbidden, ""), clierr.CodeRBAC, false, "", ""},
		{"404", refused(http.StatusNotFound, ""), clierr.CodeNotFound, false, "", "rancher get cloud-credential"},
		{"409", refused(http.StatusConflict, ""), clierr.CodeConflict, false, "", ""},
		{"422 in use", refused(http.StatusUnprocessableEntity, inUse), clierr.CodeValidation, false, "Cloud credential is currently referenced by provisioning cluster prod", ""},
		{"500", unknown(http.StatusInternalServerError, ""), clierr.CodeGeneric, false, "did not say whether Rancher deleted the cloud credential", "rancher get cloud-credential 'cc-abcde'"},
		{"502", unknown(http.StatusBadGateway, ""), clierr.CodeGeneric, false, "did not say whether Rancher deleted the cloud credential", ""},
		{"504", unknown(http.StatusGatewayTimeout, ""), clierr.CodeGeneric, false, "did not say whether Rancher deleted the cloud credential", ""},
		{"a lost response", &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric, "the connection failed")}, clierr.CodeGeneric, false, "no response arrived", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.norman.deleteErr = tc.err
			_, _, ce := r.run(t, ccpInputOf(), "json", false, true)
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Contains(t, ce.Message, tc.contains)
			if tc.hint != "" {
				assert.Contains(t, ce.Hint+ce.Message, tc.hint)
			}
			assert.Len(t, r.norman.deletes, 1)
		})
	}
}

// TestCloudCredentialDeleteWait asserts the wait after a delete: NOT_FOUND or a
// Secret of the same name with a new uid ends it, --wait=false reads nothing
// after the DELETE, a timeout and an interrupt end in the committed error with
// the get hint, and a refused read ends it with a warning.
func TestCloudCredentialDeleteWait(t *testing.T) {
	t.Parallel()
	t.Run("NOT_FOUND ends the wait", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			_, stdout, ce := r.run(t, ccpInputOf(), "json", false, true)
			require.Nil(t, ce)
			var env struct {
				Warnings []string `json:"warnings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &env))
			assert.Equal(t, []string{ccdServerReads, ccdReadsAgain}, env.Warnings, "the preview's warnings only")
		})
	})
	t.Run("a new uid ends the wait", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.norman.onDelete = func() {
				replaced := ccpStored("amazonec2", "9")
				replaced.SetUID("uid-2")
				r.change(replaced)
			}
			_, _, ce := r.run(t, ccpInputOf(), "json", false, true)
			require.Nil(t, ce)
		})
	})
	t.Run("--wait=false reads nothing after the DELETE", func(t *testing.T) {
		t.Parallel()
		r := newCCDRig(newCCWRig(nil), "amazonec2")
		in := ccpInputOf()
		in.wait = new(false)
		var before int
		r.norman.onDelete = func() {
			r.client.mu.Lock()
			before = r.client.gets
			r.client.mu.Unlock()
		}
		_, _, ce := r.run(t, in, "json", false, true)
		require.Nil(t, ce)
		r.client.mu.Lock()
		defer r.client.mu.Unlock()
		assert.Equal(t, before, r.client.gets)
	})
	t.Run("a timeout", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.norman.onDelete = nil
			in := ccpInputOf()
			in.timeout = "30s"
			_, _, ce := r.run(t, in, "json", false, true)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.False(t, ce.Retryable)
			assert.True(t, strings.HasPrefix(ce.Message, "timed out after 30s waiting for cloud credential cc-abcde to be deleted; the deletion was requested; last state: "), ce.Message)
			assert.Contains(t, ce.Hint, "rancher get cloud-credential 'cc-abcde'")
			details, _ := json.Marshal(ce.Details)
			assert.Contains(t, string(details), `"resourceKind":"cloud-credential"`)
			assert.Contains(t, string(details), `"committed":true`)
		})
	})
	t.Run("an interrupt", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			ctx, cancel := context.WithCancel(t.Context())
			r.norman.onDelete = func() { go func() { cancel() }() }
			var out, errOut bytes.Buffer
			_, ce := runtime.Run(ctx, r.del, ccpInputOf(), runtime.IO{Out: &out, Err: &errOut, Format: "json", Apply: true, Yes: true}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Contains(t, ce.Message, "stopped waiting for cloud credential cc-abcde to be deleted; the deletion was requested")
			details, _ := json.Marshal(ce.Details)
			assert.Contains(t, string(details), "get cloud-credential 'cc-abcde'")
		})
	})
	t.Run("a refused read ends with a warning", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			r.norman.onDelete = func() {
				r.client.getSecret = func(string) (*unstructured.Unstructured, error) {
					return nil, clierr.New(clierr.CodeRBAC, "forbidden")
				}
			}
			_, stdout, ce := r.run(t, ccpInputOf(), "json", false, true)
			require.Nil(t, ce)
			var env struct {
				Warnings []string `json:"warnings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &env))
			assert.Equal(t, []string{ccdServerReads, ccdReadsAgain,
				"your access to cloud credential cc-abcde ended after the delete request; the deletion was requested and not confirmed"}, env.Warnings)
		})
	})
}

// TestCloudCredentialDeleteNeverShowsAStoredValue asserts that no value of the
// stored Secret's data (decoded or in base64) and no stored annotation value is
// in anything a delete writes or returns: on a preview, a delete, a conflict
// found by the read before the request, a refused List of each kind and each
// response a DELETE can get, in JSON and in text.
func TestCloudCredentialDeleteNeverShowsAStoredValue(t *testing.T) {
	t.Parallel()
	needles := []string{ccpAnnotation, ccpStoredData}
	echo := func() string {
		raw, _ := json.Marshal(map[string]string{"type": "error", "code": "X", "message": "response for " + ccpID})
		return string(raw)
	}
	type row struct {
		name     string
		err      error
		prepare  func(*ccdRig)
		code     string
		requests int
	}
	rows := []row{
		{"a delete", nil, nil, "", 1},
		{"a 422 in use", ccsRefused(http.StatusUnprocessableEntity, echo()), nil, clierr.CodeValidation, 1},
		{"a 500", ccsUnknown(http.StatusInternalServerError, echo()), nil, clierr.CodeGeneric, 1},
		{"a lost response", &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric, "the connection failed")}, nil, clierr.CodeGeneric, 1},
		{"a change found by the read before the request", nil, func(r *ccdRig) {
			r.client.getSecret = func(string) (*unstructured.Unstructured, error) { return ccpStored("amazonec2", "8"), nil }
		}, clierr.CodeConflict, 0},
		{"a refused List of provisioning clusters", nil, func(r *ccdRig) { r.refuseClusters(ccdProvisioning, ccdForbidden()) }, "", 1},
		{"a refused List of management clusters", nil, func(r *ccdRig) { r.refuseClusters(ccdManagement, ccdForbidden()) }, "", 1},
	}
	for _, format := range []string{"json", ""} {
		t.Run("a preview "+format, func(t *testing.T) {
			t.Parallel()
			r := newCCDRig(newCCWRig(nil), "amazonec2")
			written, stdout, ce := r.run(t, ccpInputOf(), format, true, false)
			require.Nil(t, ce)
			ccsAssertKind(t, format, stdout, "Effect", "cloud-credential")
			assert.Empty(t, r.norman.deletes)
			assertNoForms(t, needles, written, "the preview")
		})
		for _, tc := range rows {
			t.Run(tc.name+" "+format, func(t *testing.T) {
				t.Parallel()
				r := newCCDRig(newCCWRig(nil), "amazonec2")
				r.norman.deleteErr = tc.err
				if tc.prepare != nil {
					tc.prepare(r)
				}
				written, stdout, ce := r.run(t, ccpInputOf(), format, false, true)
				if tc.code == "" {
					require.Nil(t, ce)
					ccsAssertKind(t, format, stdout, "CloudCredential", ccpID)
				} else {
					require.NotNil(t, ce)
					assert.Equal(t, tc.code, ce.Code)
				}
				require.Len(t, r.norman.deletes, tc.requests)
				assertNoForms(t, needles, written, "what the run wrote")
				assert.NotContains(t, written, base64.StdEncoding.EncodeToString([]byte(ccpStoredData)))
			})
		}
	}
}
