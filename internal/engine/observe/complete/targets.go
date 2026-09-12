package complete

// Target describes one curated log source: a fixed (or, for fleet-agent,
// dynamically resolved) namespace/selector/container on either the local or
// a downstream cluster.
type Target struct {
	Name           string
	LocalOnly      bool
	DownstreamOnly bool
	Namespace      string // static default; "" when Resolve computes it dynamically (fleet-agent)
	Selector       string
	Container      string // documented default container; "" lists every container
}

// Targets is the curated table, verbatim: namespaces, selectors, and
// containers are source-verified against rancher/rancher - do not
// re-derive them.
var Targets = []Target{
	{Name: "server", LocalOnly: true, Namespace: "cattle-system", Selector: "app=rancher"},
	{Name: "agent", DownstreamOnly: true, Namespace: "cattle-system", Selector: "app=cattle-cluster-agent", Container: "cluster-register"},
	{Name: "fleet-agent", Selector: "app=fleet-agent"},
	{Name: "kube-api-auth", DownstreamOnly: true, Namespace: "cattle-system", Selector: "app=kube-api-auth", Container: "kube-api-auth"},
	{Name: "webhook", LocalOnly: true, Namespace: "cattle-system", Selector: "app=rancher-webhook"},
	// The imperative API extension server (ext.cattle.io: tokens, kubeconfigs)
	// runs as its own deployment on the local cluster; its store-side view of
	// a token or kubeconfig operation is only visible here.
	{Name: "api-extension", LocalOnly: true, Namespace: "cattle-system", Selector: "app=api-extension"},
}

// Lookup resolves a target by name.
func Lookup(name string) (Target, bool) {
	for _, t := range Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// Names returns every target name, in table order (for the command's usage
// text and unknown-target hints).
func Names() []string {
	out := make([]string, len(Targets))
	for i, t := range Targets {
		out[i] = t.Name
	}
	return out
}
