package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// tableAccept is the Accept value kubectl sends for its text views: the
// server's Table in either version, else the ordinary JSON answer.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io," +
	"application/json;as=Table;v=v1beta1;g=meta.k8s.io," +
	"application/json"

var _ engine.TableReader = (*dynamicClient)(nil)

// tableRequest starts a GET for gvr's path, built from path segments alone
// as the dynamic client builds it: a request that names its resource or
// object through the builder's own fields words a server refusal with a
// suffix List and Get do not have. The namespace and name are validated
// with the dynamic client's own check and text.
func (r *dynamicClient) tableRequest(gvr schema.GroupVersionResource, ns, name string) (*rest.Request, error) {
	if msgs := rest.IsValidPathSegmentName(ns); len(msgs) != 0 {
		return nil, fmt.Errorf("invalid namespace %q: %v", ns, msgs)
	}
	if name != "" {
		if msgs := rest.IsValidPathSegmentName(name); len(msgs) != 0 {
			return nil, fmt.Errorf("invalid resource name %q: %v", name, msgs)
		}
	}
	var segments []string
	if gvr.Group == "" {
		segments = []string{"api", gvr.Version}
	} else {
		segments = []string{"apis", gvr.Group, gvr.Version}
	}
	if ns != "" {
		segments = append(segments, "namespaces", ns)
	}
	segments = append(segments, gvr.Resource)
	if name != "" {
		segments = append(segments, name)
	}
	return r.tbl.Get().AbsPath(segments...).SetHeader("Accept", tableAccept), nil
}

// ListTable lists gvr's objects in ns as the server's Table.
func (r *dynamicClient) ListTable(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (engine.TableAnswer, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return engine.TableAnswer{}, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	req, err := r.tableRequest(gvr, ns, "")
	if err != nil {
		return engine.TableAnswer{}, mapRequestErr(ctx, http.MethodGet, err, r.request("list", gvr, ns, ""))
	}
	if selector != "" {
		req = req.Param("labelSelector", selector)
	}
	out, err := readTable(ctx, req, true)
	return out, r.forbiddenHint(gvr, "list", ns, "", mapRequestErr(ctx, http.MethodGet, err, r.request("list", gvr, ns, "")))
}

// GetTable reads the named object as the server's Table.
func (r *dynamicClient) GetTable(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (engine.TableAnswer, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return engine.TableAnswer{}, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	if name == "" {
		return engine.TableAnswer{}, mapRequestErr(ctx, http.MethodGet, errors.New("name is required"), r.request("get", gvr, ns, name))
	}
	req, err := r.tableRequest(gvr, ns, name)
	if err != nil {
		return engine.TableAnswer{}, mapRequestErr(ctx, http.MethodGet, err, r.request("get", gvr, ns, name))
	}
	out, err := readTable(ctx, req, false)
	return out, r.forbiddenHint(gvr, "get", ns, name, r.curateNotFound(gvr, name, mapRequestErr(ctx, http.MethodGet, err, r.request("get", gvr, ns, name))))
}

// readTable sends req and decodes the answer: a Table of either version, or,
// for any other body, the objects it holds (a list when list is true, else
// one object). A server error comes back as the request's own error.
func readTable(ctx context.Context, req *rest.Request, list bool) (engine.TableAnswer, error) {
	res := req.Do(ctx)
	if err := res.Error(); err != nil {
		return engine.TableAnswer{}, err
	}
	if obj, err := res.Get(); err == nil {
		if table, ok := obj.(*metav1.Table); ok {
			if err := engine.DecodeRowObjects(table); err != nil {
				return engine.TableAnswer{}, err
			}
			return engine.TableAnswer{Table: table}, nil
		}
	}
	// Any other body is decoded as List and Get decode it: a body that is
	// no object at all fails with their error.
	if list {
		var l unstructured.UnstructuredList
		if err := res.Into(&l); err != nil {
			return engine.TableAnswer{}, err
		}
		return engine.TableAnswer{Items: l.Items}, nil
	}
	var one unstructured.Unstructured
	if err := res.Into(&one); err != nil {
		return engine.TableAnswer{}, err
	}
	return engine.TableAnswer{Items: []unstructured.Unstructured{one}}, nil
}
