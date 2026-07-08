package provider

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type dynamicClient struct{ dyn dynamic.Interface }

// NewClusterClient builds the dynamic-client ClusterClient for a rest.Config
// (one cluster proxy).
func NewClusterClient(rc *rest.Config) (engine.ClusterClient, error) {
	dyn, err := kube.Dynamic(rc)
	if err != nil {
		return nil, err
	}
	return &dynamicClient{dyn: dyn}, nil
}

func (r *dynamicClient) ri(gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	res := r.dyn.Resource(gvr)
	if ns == "" {
		return res
	}
	return res.Namespace(ns)
}

func (r *dynamicClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	out, err := r.ri(gvr, ns).Get(ctx, name, metav1.GetOptions{})
	return out, mapErr(err)
}

func (r *dynamicClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns string) (*unstructured.UnstructuredList, error) {
	out, err := r.ri(gvr, ns).List(ctx, metav1.ListOptions{})
	return out, mapErr(err)
}

func (r *dynamicClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	opts := metav1.CreateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Create(ctx, obj, opts)
	return out, mapErr(err)
}

func (r *dynamicClient) Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	opts := metav1.UpdateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Update(ctx, obj, opts)
	return out, mapErr(err)
}

func (r *dynamicClient) Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error {
	opts := metav1.DeleteOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if resourceVersion != "" {
		opts.Preconditions = &metav1.Preconditions{ResourceVersion: &resourceVersion}
	}
	return mapErr(r.ri(gvr, ns).Delete(ctx, name, opts))
}
