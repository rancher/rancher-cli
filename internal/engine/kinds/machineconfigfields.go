package kinds

// MachineConfigFields is one machine-config kind's driver-field vocabulary -
// the single compiled source shared by `create machine-config`
// (validation) and the completion surface (--set key candidates), the
// OutputFormats one-source pattern.
//
// Source of truth - verify against https://github.com/rancher/rancher
// when editing:
//   - credential fields: pkg/data/management/machinedriver_data.go,
//     DriverData (Public+Private+Optional credential fields per driver).
//   - field names/casings: lower-camel of the docker-machine flag minus
//     the driver prefix - pkg/controllers/management/drivers/nodedriver/
//     utils.go (FlagToField/ToLowerCamelCase) - with FileToFieldAliases
//     renames applied (machine_driver.go: DO publishes sshKeyContents,
//     never sshKeyPath; rancher's clusters/node_test.go asserts that).
//   - DO driver flags: the rancher/machine module (go.mod pin in
//     rancher/rancher), drivers/digitalocean/digitalocean.go GetCreateFlags.
type MachineConfigFields struct {
	// Curated driver fields carried by dedicated create machine-config
	// flags. Empty for the four kinds not yet curated.
	Curated []string
	// Known non-credential driver fields - the full field vocabulary a
	// --set key may name. Superset of Curated; empty for uncurated kinds.
	// The --set COMPLETION candidates are Known minus Curated minus
	// Credential: a curated field is set via its flag and
	// --set rejects it, so suggesting it would violate
	// suggestions-valid-verbatim.
	Known []string
	// Credential-carried fields - rejected by create machine-config with a
	// cloud-credential hint, never suggested. Stored in the
	// canonical confirmed casings (they feed completion and docs); the
	// rejection folds case, so any casing of these names is refused.
	Credential []string
	// Secret-carried fields: the fields whose values a preview or a diff
	// never shows. They are the fields Rancher types "password" for the
	// driver (its SSH key fields and the passwordFields and
	// privateCredentialFields of the driver's definition) at Rancher
	// v2.15.2, plus sessionToken and sshPassword, which the drivers
	// publish and Rancher does not type "password". A name matches the
	// top-level field with case folded and hides everything under it.
	Secret []string
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
		Secret:     []string{"accessToken", "sshKeyContents"},
	},
	"amazonec2config": {
		Credential: []string{"accessKey", "secretKey"},
		Secret:     []string{"secretKey", "sessionToken", "sshKeyContents"},
	},
	"azureconfig": {
		Credential: []string{"clientId", "clientSecret", "environment", "subscriptionId", "tenantId"},
		Secret:     []string{"clientSecret"},
	},
	"harvesterconfig": {
		Credential: []string{"clusterId", "clusterType", "kubeconfigContent"},
		Secret:     []string{"kubeconfigContent", "sshPassword"},
	},
	"vmwarevsphereconfig": {
		Credential: []string{"password", "username", "vcenter", "vcenterPort"},
		Secret:     []string{"password", "sshPassword"},
	},
}

// MachineConfigFieldsFor returns the driver-field vocabulary for a
// machine-config kind row's Name (a kinds.MachineConfigGroup row).
func MachineConfigFieldsFor(kindName string) (MachineConfigFields, bool) {
	f, ok := machineConfigFields[kindName]
	return f, ok
}
