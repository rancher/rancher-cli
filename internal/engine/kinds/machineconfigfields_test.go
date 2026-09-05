package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMachineConfigFieldsCoverEveryMachineConfigKind asserts the bijection:
// every MachineConfigGroup kind row has a field-metadata entry, and every
// entry names a real machine-config kind row.
func TestMachineConfigFieldsCoverEveryMachineConfigKind(t *testing.T) {
	t.Parallel()
	rows := map[string]bool{}
	for _, k := range All() {
		if k.GVR.Group == MachineConfigGroup {
			rows[k.Name] = true
		}
	}
	for name := range rows {
		_, ok := MachineConfigFieldsFor(name)
		assert.True(t, ok, "kind row %q has no machine-config field metadata", name)
	}
	for name := range machineConfigFields {
		assert.True(t, rows[name], "field metadata %q names no machine-config kind row", name)
	}
}

// TestMachineConfigCredentialFieldsLiteral asserts the credential-carried
// field set per kind as LITERALS - deliberately not derived, so a partial
// edit to the metadata cannot silently move expectation and result
// together (independent-source doctrine). Source-confirmed against
// rancher's DriverData (machinedriver_data.go); see the file doc comment.
func TestMachineConfigCredentialFieldsLiteral(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"amazonec2config":     {"accessKey", "secretKey"},
		"azureconfig":         {"clientId", "clientSecret", "environment", "subscriptionId", "tenantId"},
		"digitaloceanconfig":  {"accessToken"},
		"harvesterconfig":     {"clusterId", "clusterType", "kubeconfigContent"},
		"vmwarevsphereconfig": {"password", "username", "vcenter", "vcenterPort"},
	}
	for kind, fields := range want {
		f, ok := MachineConfigFieldsFor(kind)
		require.True(t, ok, kind)
		assert.Equal(t, fields, f.Credential, kind)
	}
}

// TestMachineConfigFieldsDigitaloceanVocabulary asserts the DO curated and
// known sets as literals (rancher/machine digitalocean.go GetCreateFlags,
// lower-camel per nodedriver's ToLowerCamelCase, sshKeyPath renamed to
// sshKeyContents per FileToFieldAliases).
func TestMachineConfigFieldsDigitaloceanVocabulary(t *testing.T) {
	t.Parallel()
	f, ok := MachineConfigFieldsFor("digitaloceanconfig")
	require.True(t, ok)
	assert.Equal(t, []string{
		"image", "ipv6", "monitoring", "privateNetworking", "region", "size", "tags",
	}, f.Curated)
	assert.Equal(t, []string{
		"backups", "image", "ipv6", "monitoring", "privateNetworking",
		"region", "size", "sshKeyContents", "sshKeyFingerprint",
		"sshPort", "sshUser", "tags", "userdata",
	}, f.Known)
}

// TestMachineConfigFieldsInvariants: Known contains every Curated key and
// no Credential key - a curated flag always writes a known field, and the
// completion set (Known - Curated - Credential, MC-16) can be computed by
// subtraction alone, so no candidate can be one validation rejects.
func TestMachineConfigFieldsInvariants(t *testing.T) {
	t.Parallel()
	for name, f := range machineConfigFields {
		known := map[string]bool{}
		for _, k := range f.Known {
			known[k] = true
		}
		for _, c := range f.Curated {
			assert.True(t, known[c], "%s: curated %q missing from Known", name, c)
		}
		for _, c := range f.Credential {
			assert.False(t, known[c], "%s: credential %q must not be Known", name, c)
		}
	}
}
