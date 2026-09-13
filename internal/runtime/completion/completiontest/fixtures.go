package completiontest

// CloudCredentialsBody is a cattle-global-data Secret list: a credential
// with a unique display name and a driver data key, two sharing a display
// name, one whose display name is another's id, one with a display name
// outside the shell-safe charset, one without the display annotation, a
// marker-only credential without the name prefix, and a Secret that is
// no credential.
const CloudCredentialsBody = `{"items":[
	{"metadata":{"name":"cc-abc12","annotations":{"field.cattle.io/name":"aws-prod"}},"data":{"amazonec2credentialConfig-accessKey":"UkVEQUNURUQ="}},
	{"metadata":{"name":"cc-dup1","annotations":{"field.cattle.io/name":"dup"}}},
	{"metadata":{"name":"cc-dup2","annotations":{"field.cattle.io/name":"dup"}}},
	{"metadata":{"name":"cc-collide","annotations":{"field.cattle.io/name":"cc-abc12"}}},
	{"metadata":{"name":"cc-hostile","annotations":{"field.cattle.io/name":"bad name!"}}},
	{"metadata":{"name":"cc-noannotation"}},
	{"metadata":{"name":"marker-only"},"data":{"azurecredentialConfig-clientId":"UkVEQUNURUQ="}},
	{"metadata":{"name":"other-secret"},"data":{"password":"UkVEQUNURUQ="}}
]}`

// DiscoveryCoreBody is the aggregated discovery document of a cluster's
// core group: pods (a short name, three subresources), nodes
// (cluster-scoped, two) and services (one).
const DiscoveryCoreBody = `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[
	{"metadata":{"name":""},"versions":[{"version":"v1","freshness":"Current","resources":[
		{"resource":"pods","singularResource":"pod","shortNames":["po"],"scope":"Namespaced","responseKind":{"kind":"Pod"},"verbs":["get","list"],
			"subresources":[{"subresource":"log","verbs":["get"]},{"subresource":"status","verbs":["get"]},{"subresource":"exec","verbs":["get"]}]},
		{"resource":"nodes","singularResource":"node","shortNames":["no"],"scope":"Cluster","responseKind":{"kind":"Node"},"verbs":["get","list"],
			"subresources":[{"subresource":"status","verbs":["get"]},{"subresource":"proxy","verbs":["get"]}]},
		{"resource":"services","singularResource":"service","shortNames":["svc"],"scope":"Namespaced","responseKind":{"kind":"Service"},"verbs":["get","list"],
			"subresources":[{"subresource":"status","verbs":["get"]}]}
	]}]}
]}`

// DiscoveryGroupsBody is the aggregated discovery document of a cluster's
// named groups: deployments; a resource name two groups serve
// (ingresses); a group whose first version the server marked stale and
// whose second is current (gauges, offered from the current one); and a
// group whose only version is stale (widgets, never offered).
const DiscoveryGroupsBody = `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[
	{"metadata":{"name":"apps"},"versions":[{"version":"v1","freshness":"Current","resources":[
		{"resource":"deployments","singularResource":"deployment","shortNames":["deploy"],"scope":"Namespaced","responseKind":{"kind":"Deployment"},"verbs":["get","list"],
			"subresources":[{"subresource":"scale","verbs":["get"]},{"subresource":"status","verbs":["get"]}]}
	]}]},
	{"metadata":{"name":"networking.k8s.io"},"versions":[{"version":"v1","freshness":"Current","resources":[
		{"resource":"ingresses","singularResource":"ingress","shortNames":["ing"],"scope":"Namespaced","responseKind":{"kind":"Ingress"},"verbs":["get","list"],
			"subresources":[{"subresource":"status","verbs":["get"]}]}
	]}]},
	{"metadata":{"name":"gateway.example.io"},"versions":[{"version":"v1","freshness":"Current","resources":[
		{"resource":"ingresses","singularResource":"ingress","scope":"Namespaced","responseKind":{"kind":"Ingress"},"verbs":["get","list"],
			"subresources":[{"subresource":"conditions","verbs":["get"]}]}
	]}]},
	{"metadata":{"name":"metrics.example.io"},"versions":[
		{"version":"v2","freshness":"Stale","resources":[{"resource":"gauges","singularResource":"gauge","scope":"Namespaced","responseKind":{"kind":"Gauge"},"verbs":["get"],
			"subresources":[{"subresource":"stale","verbs":["get"]}]}]},
		{"version":"v1","freshness":"Current","resources":[{"resource":"gauges","singularResource":"gauge","scope":"Namespaced","responseKind":{"kind":"Gauge"},"verbs":["get"],
			"subresources":[{"subresource":"status","verbs":["get"]}]}]}
	]},
	{"metadata":{"name":"dead.example"},"versions":[{"version":"v1beta1","freshness":"Stale","resources":[
		{"resource":"widgets","singularResource":"widget","scope":"Namespaced","responseKind":{"kind":"Widget"},"verbs":["get","list"]}
	]}]}
]}`

// SubjectsBody is a management User list with every shape the subject
// projection decides on: a plain user, a repeated row, a system account,
// a username outside the shell-safe charset, a user without principals,
// a user whose id is its username, two users sharing a username, a user
// whose display name is another's username, a user whose username is
// another user's id, and a user whose username is a system account's id.
const SubjectsBody = `{"items":[
	{"metadata":{"name":"u-alice"},"username":"alice","displayName":"Alice","principalIds":["local://u-alice"]},
	{"metadata":{"name":"u-bob"},"username":"bob","displayName":"Bob","principalIds":["local://u-bob"]},
	{"metadata":{"name":"u-bob"},"username":"bob","displayName":"Bob","principalIds":["local://u-bob"]},
	{"metadata":{"name":"u-sys"},"username":"system-svc","displayName":"System","principalIds":["system://u-sys"]},
	{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
	{"metadata":{"name":"u-nopids"},"username":"nopids"},
	{"metadata":{"name":"sameid"},"username":"sameid","displayName":"sameid","principalIds":["local://sameid"]},
	{"metadata":{"name":"u-dup1"},"username":"dup","displayName":"Dup One","principalIds":["local://u-dup1"]},
	{"metadata":{"name":"u-dup2"},"username":"dup","displayName":"Dup Two","principalIds":["local://u-dup2"]},
	{"metadata":{"name":"u-carol"},"username":"carol","displayName":"nopids","principalIds":["local://u-carol"]},
	{"metadata":{"name":"u-dave"},"username":"u-alice","displayName":"Dave","principalIds":["local://u-dave"]},
	{"metadata":{"name":"u-erin"},"username":"u-sys","displayName":"Erin","principalIds":["local://u-erin"]}
]}`

// DiscoveryLegacyCoreBody is the legacy /api document (APIVersions) of the
// same cluster DiscoveryCoreBody describes.
const DiscoveryLegacyCoreBody = `{"kind":"APIVersions","versions":["v1"]}`

// DiscoveryLegacyGroupsBody is the legacy /apis document (APIGroupList)
// of the same cluster DiscoveryGroupsBody describes: the four served
// groups with their preferred versions (metrics.example.io prefers v1,
// the version the aggregated document marks current) and the dead group,
// whose document is not served.
const DiscoveryLegacyGroupsBody = `{"kind":"APIGroupList","apiVersion":"v1","groups":[
	{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
	{"name":"networking.k8s.io","versions":[{"groupVersion":"networking.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"networking.k8s.io/v1","version":"v1"}},
	{"name":"gateway.example.io","versions":[{"groupVersion":"gateway.example.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"gateway.example.io/v1","version":"v1"}},
	{"name":"metrics.example.io","versions":[{"groupVersion":"metrics.example.io/v2","version":"v2"},{"groupVersion":"metrics.example.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"metrics.example.io/v1","version":"v1"}},
	{"name":"dead.example","versions":[{"groupVersion":"dead.example/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"dead.example/v1beta1","version":"v1beta1"}}
]}`

// DiscoveryLegacyVersionBodies are the legacy group version documents
// (APIResourceList) of the same cluster, keyed by the path under the
// cluster's proxy root: the same resources, singular and short names and
// subresources the aggregated documents carry, a subresource as its own
// "<resource>/<sub>" entry. The dead group's document is absent (the
// server answers it 503).
var DiscoveryLegacyVersionBodies = map[string]string{
	"/api/v1": `{"kind":"APIResourceList","groupVersion":"v1","resources":[
		{"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod","verbs":["get","list"],"shortNames":["po"]},
		{"name":"pods/log","singularName":"","namespaced":true,"kind":"Pod","verbs":["get"]},
		{"name":"pods/status","singularName":"","namespaced":true,"kind":"Pod","verbs":["get"]},
		{"name":"pods/exec","singularName":"","namespaced":true,"kind":"PodExecOptions","verbs":["get"]},
		{"name":"nodes","singularName":"node","namespaced":false,"kind":"Node","verbs":["get","list"],"shortNames":["no"]},
		{"name":"nodes/status","singularName":"","namespaced":false,"kind":"Node","verbs":["get"]},
		{"name":"nodes/proxy","singularName":"","namespaced":false,"kind":"NodeProxyOptions","verbs":["get"]},
		{"name":"services","singularName":"service","namespaced":true,"kind":"Service","verbs":["get","list"],"shortNames":["svc"]},
		{"name":"services/status","singularName":"","namespaced":true,"kind":"Service","verbs":["get"]}
	]}`,
	"/apis/apps/v1": `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[
		{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get","list"],"shortNames":["deploy"]},
		{"name":"deployments/scale","singularName":"","namespaced":true,"group":"autoscaling","version":"v1","kind":"Scale","verbs":["get"]},
		{"name":"deployments/status","singularName":"","namespaced":true,"kind":"Deployment","verbs":["get"]}
	]}`,
	"/apis/networking.k8s.io/v1": `{"kind":"APIResourceList","groupVersion":"networking.k8s.io/v1","resources":[
		{"name":"ingresses","singularName":"ingress","namespaced":true,"kind":"Ingress","verbs":["get","list"],"shortNames":["ing"]},
		{"name":"ingresses/status","singularName":"","namespaced":true,"kind":"Ingress","verbs":["get"]}
	]}`,
	"/apis/gateway.example.io/v1": `{"kind":"APIResourceList","groupVersion":"gateway.example.io/v1","resources":[
		{"name":"ingresses","singularName":"ingress","namespaced":true,"kind":"Ingress","verbs":["get","list"]},
		{"name":"ingresses/conditions","singularName":"","namespaced":true,"kind":"Ingress","verbs":["get"]}
	]}`,
	"/apis/metrics.example.io/v1": `{"kind":"APIResourceList","groupVersion":"metrics.example.io/v1","resources":[
		{"name":"gauges","singularName":"gauge","namespaced":true,"kind":"Gauge","verbs":["get"]},
		{"name":"gauges/status","singularName":"","namespaced":true,"kind":"Gauge","verbs":["get"]}
	]}`,
}
