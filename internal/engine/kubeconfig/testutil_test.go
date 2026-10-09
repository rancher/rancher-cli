package kubeconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// fakeClient answers GET and LIST from maps keyed "name" (cluster scope)
// or "ns/name", and "resource|ns" for lists; every other method is
// unreachable from this package.
type fakeClient struct {
	Objects   map[string]*unstructured.Unstructured
	Lists     map[string]*unstructured.UnstructuredList
	GetErrFor map[string]error
	ListErr   error
	Before    func(ctx context.Context) error
	// Gets records the resource of every Get, in order.
	Gets []string
}

func (f *fakeClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	f.Gets = append(f.Gets, gvr.Resource)
	if f.Before != nil {
		if err := f.Before(ctx); err != nil {
			return nil, err
		}
	}
	key := name
	if ns != "" {
		key = ns + "/" + name
	}
	if err, ok := f.GetErrFor[key]; ok {
		return nil, err
	}
	obj, ok := f.Objects[key]
	if !ok {
		return nil, clierr.New(clierr.CodeNotFound, key+" not found")
	}
	return obj, nil
}

func (f *fakeClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	if f.Before != nil {
		if err := f.Before(ctx); err != nil {
			return nil, err
		}
	}
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	if l, ok := f.Lists[gvr.Resource+"|"+ns]; ok {
		return l, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (f *fakeClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("unreachable")
}
func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("unreachable")
}
func (f *fakeClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return errors.New("unreachable")
}
func (f *fakeClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("unreachable")
}
func (f *fakeClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("unreachable")
}
func (f *fakeClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("unreachable")
}

var _ engine.ClusterClient = (*fakeClient)(nil)

// clusterGetter is a steve.Getter that serves management clusters by name
// as the JSON object steve returns. A name it does not hold is a not-found
// answer shaped like the root client's 404; a name in errs fails with its
// error; before, when set, runs first and may fail the read. Every request
// is recorded under mu; read requests only after the reads finished.
type clusterGetter struct {
	objs     map[string]*unstructured.Unstructured
	errs     map[string]error
	before   func(ctx context.Context) error
	mu       sync.Mutex
	requests []steve.Request
}

func newClusterGetter(clusters ...*unstructured.Unstructured) *clusterGetter {
	g := &clusterGetter{objs: map[string]*unstructured.Unstructured{}}
	for _, c := range clusters {
		g.objs[c.GetName()] = c
	}
	return g
}

func (g *clusterGetter) Get(ctx context.Context, r steve.Request) ([]byte, error) {
	g.mu.Lock()
	g.requests = append(g.requests, r)
	g.mu.Unlock()
	if g.before != nil {
		if err := g.before(ctx); err != nil {
			return nil, err
		}
	}
	if err, ok := g.errs[r.Name]; ok {
		return nil, err
	}
	obj, ok := g.objs[r.Name]
	if r.Type != steve.ClustersType || !ok {
		return nil, clierr.New(clierr.CodeNotFound, r.Path()+" not found").WithCause(clierr.ResponseError(http.MethodGet, 404, nil))
	}
	return json.Marshal(obj.Object)
}

// failingRoot is an engine.RootClient whose every read fails with err.
type failingRoot struct{ err error }

func (r failingRoot) GetJSON(context.Context, string) ([]byte, error) { return nil, r.err }

// testPEM is a self-signed certificate in PEM, the shape every CA field
// carries.
func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// mgmtCluster builds a management Cluster with the endpoint fields set.
func mgmtCluster(id string, enabled bool, fqdn, caCerts, statusCA string) *unstructured.Unstructured {
	spec := map[string]any{"displayName": id, "localClusterAuthEndpoint": map[string]any{"enabled": enabled}}
	if fqdn != "" {
		spec["localClusterAuthEndpoint"].(map[string]any)["fqdn"] = fqdn
	}
	if caCerts != "" {
		spec["localClusterAuthEndpoint"].(map[string]any)["caCerts"] = caCerts
	}
	obj := map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Cluster", "metadata": map[string]any{"name": id}, "spec": spec}
	if statusCA != "" {
		obj["status"] = map[string]any{"caCert": statusCA}
	}
	return &unstructured.Unstructured{Object: obj}
}

// feature builds the multi-cluster-management Feature with spec.value.
func feature(value any) *unstructured.Unstructured {
	obj := map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Feature", "metadata": map[string]any{"name": mcmFeature}, "spec": map[string]any{}}
	if value != nil {
		obj["spec"].(map[string]any)["value"] = value
	}
	return &unstructured.Unstructured{Object: obj}
}

// mgmtNode builds a management Node; addresses are (type, address) pairs.
func mgmtNode(name, hostname string, controlPlane, ready bool, withAnnotations bool, externalIPAnnotationValue string, addresses ...[2]string) unstructured.Unstructured {
	var addrs []any
	for _, a := range addresses {
		addrs = append(addrs, map[string]any{"type": a[0], "address": a[1]})
	}
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	meta := map[string]any{"name": name}
	if withAnnotations {
		meta["annotations"] = map[string]any{"a": "b"}
	}
	status := map[string]any{"internalNodeStatus": map[string]any{"addresses": addrs, "conditions": []any{map[string]any{"type": "Ready", "status": readyStatus}}}}
	if externalIPAnnotationValue != "" {
		status["nodeAnnotations"] = map[string]any{externalIPAnnotation: externalIPAnnotationValue}
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Node", "metadata": meta,
		"spec":   map[string]any{"controlPlane": controlPlane, "requestedHostname": hostname},
		"status": status,
	}}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
