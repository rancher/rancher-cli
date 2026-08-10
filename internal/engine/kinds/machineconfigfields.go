package kinds

// MachineConfigFields is one machine-config kind's driver-field vocabulary
// — the single compiled source shared by `create machine-config`
// (validation) and the completion surface (--set key candidates), the
// OutputFormats one-source pattern (MC-16).
//
// Source of truth — verify against https://github.com/rancher/rancher
// when editing:
//   - credential fields: pkg/data/management/machinedriver_data.go,
//     DriverData (Public+Private+Optional credential fields per driver).
//   - field names/casings: lower-camel of the docker-machine flag minus
//     the driver prefix — pkg/controllers/management/drivers/nodedriver/
//     utils.go (FlagToField/ToLowerCamelCase) — with FileToFieldAliases
//     renames applied (machine_driver.go: DO publishes sshKeyContents,
//     never sshKeyPath; rancher's clusters/node_test.go pins that).
//   - DO driver flags: the rancher/machine module (go.mod pin in
//     rancher/rancher), drivers/digitalocean/digitalocean.go GetCreateFlags.
type MachineConfigFields struct {
	// Curated driver fields carried by dedicated create machine-config
	// flags (MC-5). Empty for the four kinds not yet curated.
	Curated []string
	// Known non-credential driver fields — the full field vocabulary a
	// --set key may name. Superset of Curated; empty for uncurated kinds.
	// The --set COMPLETION candidates are Known minus Curated minus
	// Credential (MC-16): a curated field is set via its flag and MC-7
	// rejects it through --set, so suggesting it would violate
	// suggestions-valid-verbatim.
	Known []string
	// Credential-carried fields — rejected by create machine-config with a
	// cloud-credential hint (MC-8), never suggested. Stored in the
	// canonical confirmed casings (they feed completion and docs); the
	// rejection folds case, so any casing of these names is refused.
	Credential []string
}

var machineConfigFields = map[string]MachineConfigFields{
	"digitaloceanconfig": {
		Curated: []string{"image", "ipv6", "monitoring", "privateNetworking", "region", "size", "tags"},
		Known: []string{
			"backups", "image", "ipv6", "monitoring", "privateNetworking",
			"region", "size", "sshKeyContents", "sshKeyFingerprint",
			"sshPort", "sshUser", "tags", "userdata",
		},
		Credential: []string{"accessToken"},
	},
	"amazonec2config": {
		Credential: []string{"accessKey", "secretKey"},
	},
	"azureconfig": {
		Credential: []string{"clientId", "clientSecret", "environment", "subscriptionId", "tenantId"},
	},
	"harvesterconfig": {
		Credential: []string{"clusterId", "clusterType", "kubeconfigContent"},
	},
	"vmwarevsphereconfig": {
		Credential: []string{"password", "username", "vcenter", "vcenterPort"},
	},
}

// MachineConfigFieldsFor returns the driver-field vocabulary for a
// machine-config kind row's Name (a kinds.MachineConfigGroup row).
func MachineConfigFieldsFor(kindName string) (MachineConfigFields, bool) {
	f, ok := machineConfigFields[kindName]
	return f, ok
}
