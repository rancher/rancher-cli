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
