// Package kubeconfig derives the direct endpoint entries of a cluster's
// Authorized Cluster Endpoint from the management objects Rancher's own
// kubeconfig generator reads: the management Cluster (the endpoint's
// enabled flag, fqdn and CA), the multi-cluster-management Feature (the
// switch Rancher's generator is built with), and the management Nodes when
// no fqdn names a single address.
package kubeconfig

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// Endpoint is one direct entry to emit.
type Endpoint struct {
	// Name is the context and cluster entry suffix: "fqdn", or the node's
	// requested hostname with the cluster id prefix removed.
	Name string
	// Server is https://<fqdn> or https://<address>:6443 (JoinHostPort).
	Server string
	// CAData is the PEM the entry trusts; nil when the cluster carries none.
	CAData []byte
	// Ready is true for the fqdn entry and, for a node, its Ready condition.
	Ready bool
}

// Direct is what DirectEndpoints derived: the entries to emit and, when any
// were omitted or skipped, the reasons, raw (the command sanitizes them).
type Direct struct {
	Endpoints []Endpoint
	// Omitted holds one reason per omission or skipped node; empty means
	// the file needs no notice.
	Omitted []string
}

// mcmFeature is the Feature Rancher's kubeconfig generator is built with:
// without it the generator emits no direct entries.
const mcmFeature = "multi-cluster-management"

// directPort is the kube-apiserver port every control-plane node serves.
const directPort = "6443"

// externalIPAnnotation is the node status annotation Rancher consults for
// an external address when the node reports no ExternalIP.
const externalIPAnnotation = "rke.cattle.io/external-ip"

var nodeGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "nodes"}

// DirectEndpoints reads the management cluster and, when the endpoint is
// enabled, the multi-cluster-management Feature and the cluster's nodes.
// A nil error with no Endpoints and no Omitted means the endpoint is off.
// Errors are returned only for a cancelled or expired context and for a
// management cluster that is not found; every completed refusal or failure
// lands in Omitted.
func DirectEndpoints(ctx context.Context, mgmt engine.ClusterClient, clusterID string) (Direct, error) {
	var d Direct
	clusterKind, _ := kinds.Lookup("cluster")
	cluster, err := mgmt.Get(ctx, clusterKind.GVR, "", clusterID)
	if err != nil {
		if ctx.Err() != nil {
			return Direct{}, ctx.Err()
		}
		if clierr.IsNotFound(err) {
			return Direct{}, err
		}
		d.Omitted = append(d.Omitted, "management cluster "+clusterID+" not readable: "+err.Error())
		return d, nil
	}
	enabled, found, err := unstructured.NestedBool(cluster.Object, "spec", "localClusterAuthEndpoint", "enabled")
	if err != nil {
		d.Omitted = append(d.Omitted, "spec.localClusterAuthEndpoint.enabled is not a bool")
		return d, nil
	}
	if !found || !enabled {
		return Direct{}, nil
	}
	fqdn, _, err := unstructured.NestedString(cluster.Object, "spec", "localClusterAuthEndpoint", "fqdn")
	if err != nil {
		d.Omitted = append(d.Omitted, "spec.localClusterAuthEndpoint.fqdn is not a string")
		return d, nil
	}
	caCerts, _, err := unstructured.NestedString(cluster.Object, "spec", "localClusterAuthEndpoint", "caCerts")
	if err != nil {
		d.Omitted = append(d.Omitted, "spec.localClusterAuthEndpoint.caCerts is not a string")
		return d, nil
	}
	clusterCA, _, err := unstructured.NestedString(cluster.Object, "status", "caCert")
	if err != nil {
		d.Omitted = append(d.Omitted, "status.caCert is not a string")
		return d, nil
	}

	featureKind, _ := kinds.Lookup("feature")
	feature, err := mgmt.Get(ctx, featureKind.GVR, "", mcmFeature)
	if err != nil {
		if ctx.Err() != nil {
			return Direct{}, ctx.Err()
		}
		d.Omitted = append(d.Omitted, "the "+mcmFeature+" feature is not readable: "+err.Error())
		return d, nil
	}
	if on := engine.FeatureEffectiveValue(feature); on == nil || !*on {
		d.Omitted = append(d.Omitted, "the "+mcmFeature+" feature is off")
		return d, nil
	}

	if fqdn != "" {
		server := "https://" + fqdn
		if err := validEndpointURL(server); err != nil {
			d.Omitted = append(d.Omitted, "fqdn "+fqdn+": "+err.Error())
			return d, nil
		}
		var ca []byte
		if caCerts != "" {
			if !validPEM([]byte(caCerts)) {
				d.Omitted = append(d.Omitted, "spec.localClusterAuthEndpoint.caCerts is not a PEM certificate")
				return d, nil
			}
			ca = []byte(caCerts)
		}
		d.Endpoints = append(d.Endpoints, Endpoint{Name: "fqdn", Server: server, CAData: ca, Ready: true})
		return d, nil
	}

	var nodeCA []byte
	if clusterCA != "" {
		pem, err := base64.StdEncoding.DecodeString(clusterCA)
		if err != nil || !validPEM(pem) {
			d.Omitted = append(d.Omitted, "status.caCert is not a base64 PEM certificate")
			return d, nil
		}
		nodeCA = pem
	}
	nodes, err := mgmt.List(ctx, nodeGVR, clusterID, "")
	if err != nil {
		if ctx.Err() != nil {
			return Direct{}, ctx.Err()
		}
		d.Omitted = append(d.Omitted, "management nodes of "+clusterID+" not readable: "+err.Error())
		return d, nil
	}
	// Rancher's generator walks its informer cache, whose order is not
	// defined; the entries and the current-context choice here follow the
	// node name, so two runs against one cluster agree.
	items := slices.Clone(nodes.Items)
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	seen := map[string]bool{}
	controlPlane := 0
	for i := range items {
		node := &items[i]
		cp, _, err := unstructured.NestedBool(node.Object, "spec", "controlPlane")
		if err != nil {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": spec.controlPlane is not a bool")
			continue
		}
		if !cp {
			continue
		}
		controlPlane++
		hostname, _, err := unstructured.NestedString(node.Object, "spec", "requestedHostname")
		if err != nil {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": spec.requestedHostname is not a string")
			continue
		}
		suffix := strings.TrimPrefix(hostname, clusterID+"-")
		if suffix == "" {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": empty requestedHostname")
			continue
		}
		if seen[suffix] {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": duplicate context name "+suffix)
			continue
		}
		address, err := endpointAddress(node)
		if err != nil {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": "+err.Error())
			continue
		}
		ready, err := nodeReady(node)
		if err != nil {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": "+err.Error())
			continue
		}
		server := "https://" + net.JoinHostPort(address, directPort)
		if err := validEndpointURL(server); err != nil {
			d.Omitted = append(d.Omitted, "node "+node.GetName()+": "+err.Error())
			continue
		}
		seen[suffix] = true
		d.Endpoints = append(d.Endpoints, Endpoint{Name: suffix, Server: server, CAData: nodeCA, Ready: ready})
	}
	if controlPlane == 0 {
		d.Omitted = append(d.Omitted, "no control-plane node in the management nodes of "+clusterID)
	}
	return d, nil
}

// endpointAddress picks the address Rancher's generator picks: the first
// ExternalIP, else the external-ip status annotation but only when the node
// carries a metadata annotations map at all (Rancher's helper gates the
// status lookup on that map), else the last InternalIP. Every entry is
// validated before any is chosen: a present field of the wrong shape
// anywhere in the node is an error, never read as absent, so the node is
// skipped and named rather than emitted with an address picked past the
// defect.
func endpointAddress(node *unstructured.Unstructured) (string, error) {
	addresses, _, err := unstructured.NestedSlice(node.Object, "status", "internalNodeStatus", "addresses")
	if err != nil {
		return "", fmt.Errorf("status.internalNodeStatus.addresses is not a list")
	}
	external, internal := "", ""
	for i, a := range addresses {
		m, ok := a.(map[string]any)
		if !ok {
			return "", fmt.Errorf("status.internalNodeStatus.addresses[%d] is not an object", i)
		}
		typ, addr, err := stringFields(m, "type", "address")
		if err != nil {
			return "", fmt.Errorf("status.internalNodeStatus.addresses[%d]: %v", i, err)
		}
		switch {
		case typ == "ExternalIP" && addr != "" && external == "":
			external = addr
		case typ == "InternalIP" && addr != "":
			internal = addr
		}
	}
	metaAnnotations, _, err := unstructured.NestedStringMap(node.Object, "metadata", "annotations")
	if err != nil {
		return "", fmt.Errorf("metadata.annotations is not a string map")
	}
	statusAnnotations, _, err := unstructured.NestedStringMap(node.Object, "status", "nodeAnnotations")
	if err != nil {
		return "", fmt.Errorf("status.nodeAnnotations is not a string map")
	}
	switch {
	case external != "":
		return external, nil
	case metaAnnotations != nil && statusAnnotations[externalIPAnnotation] != "":
		return statusAnnotations[externalIPAnnotation], nil
	case internal != "":
		return internal, nil
	}
	return "", fmt.Errorf("no address")
}

// nodeReady reads the node's Ready condition after validating every
// condition entry; a malformed list or entry anywhere is an error, so the
// node is skipped and named.
func nodeReady(node *unstructured.Unstructured) (bool, error) {
	conditions, _, err := unstructured.NestedSlice(node.Object, "status", "internalNodeStatus", "conditions")
	if err != nil {
		return false, fmt.Errorf("status.internalNodeStatus.conditions is not a list")
	}
	ready := false
	for i, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok {
			return false, fmt.Errorf("status.internalNodeStatus.conditions[%d] is not an object", i)
		}
		typ, status, err := stringFields(m, "type", "status")
		if err != nil {
			return false, fmt.Errorf("status.internalNodeStatus.conditions[%d]: %v", i, err)
		}
		if typ == "Ready" {
			ready = status == "True"
		}
	}
	return ready, nil
}

// stringFields reads two fields that must be strings when present.
func stringFields(m map[string]any, a, b string) (string, string, error) {
	var out [2]string
	for i, key := range []string{a, b} {
		v, present := m[key]
		if !present {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", "", fmt.Errorf("%s is not a string", key)
		}
		out[i] = s
	}
	return out[0], out[1], nil
}

// validEndpointURL accepts an absolute https URL with a host, an empty path
// (or exactly "/"), and no userinfo, query or fragment: the only shape a
// direct cluster entry may carry, so a shared exec credential is never
// built from a URL kubectl or the plugin would read differently.
func validEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("scheme %q is not https", u.Scheme)
	case u.Hostname() == "":
		return fmt.Errorf("no host in %q", raw)
	case u.User != nil:
		return fmt.Errorf("userinfo in %q", raw)
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return fmt.Errorf("query or fragment in %q", raw)
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("path in %q", raw)
	}
	return nil
}

// validPEM reports whether pem parses as at least one certificate with the
// parser the plugin's probe transport uses.
func validPEM(pem []byte) bool {
	return x509.NewCertPool().AppendCertsFromPEM(pem)
}
