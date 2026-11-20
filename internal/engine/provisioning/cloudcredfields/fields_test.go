package cloudcredfields

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCloudCredentialFieldsPerDriver asserts the fields, the secret fields and
// the required fields the annotations of each built-in node driver give, plus a
// driver whose passwordFields names a public field and one with an SSH key
// field, and the static s3 and alibaba rows.
func TestCloudCredentialFieldsPerDriver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prefix   string
		a        Annotations
		public   []string
		secret   []string
		required []string
	}{
		{"amazonec2", Annotations{Public: "accessKey", Private: "secretKey"}, []string{"accessKey", "defaultRegion"}, []string{"secretKey"}, []string{"accessKey", "secretKey"}},
		{"azure", Annotations{Public: "clientId,subscriptionId,environment,tenantId", Private: "clientSecret", Optional: "tenantId"},
			[]string{"clientId", "environment", "subscriptionId", "tenantId"}, []string{"clientSecret"}, []string{"clientId", "clientSecret", "environment", "subscriptionId"}},
		{"digitalocean", Annotations{Private: "accessToken"}, nil, []string{"accessToken"}, []string{"accessToken"}},
		{"exoscale", Annotations{Public: "apiKey", Private: "apiSecretKey"}, []string{"apiKey"}, []string{"apiSecretKey"}, []string{"apiKey", "apiSecretKey"}},
		{"google", Annotations{Private: "authEncodedJson"}, nil, []string{"authEncodedJson"}, []string{"authEncodedJson"}},
		{"harvester", Annotations{Public: "clusterType,clusterId", Private: "kubeconfigContent", Optional: "clusterId", Defaults: "clusterType:imported"},
			[]string{"clusterId", "clusterType"}, []string{"kubeconfigContent"}, []string{"kubeconfigContent"}},
		{"linode", Annotations{Private: "token"}, nil, []string{"token"}, []string{"token"}},
		{"nutanix", Annotations{Public: "endpoint,username,port", Private: "password"}, []string{"endpoint", "port", "username"}, []string{"password"}, []string{"endpoint", "password", "port", "username"}},
		{"oci", Annotations{Public: "tenancyId,userId,fingerprint", Private: "privateKeyContents"}, []string{"fingerprint", "tenancyId", "userId"}, []string{"privateKeyContents"}, []string{"fingerprint", "privateKeyContents", "tenancyId", "userId"}},
		{"otc", Annotations{Public: "accessKey,username", Private: "secretKey,password,token"}, []string{"accessKey", "username"}, []string{"password", "secretKey", "token"}, []string{"accessKey", "password", "secretKey", "token", "username"}},
		{"openstack", Annotations{Private: "password"}, nil, []string{"password"}, []string{"password"}},
		{"packet", Annotations{Private: "apiKey"}, nil, []string{"apiKey"}, []string{"apiKey"}},
		{"pnap", Annotations{Public: "clientIdentifier,clientSecret", Password: "clientSecret"}, []string{"clientIdentifier"}, []string{"clientSecret"}, []string{"clientIdentifier", "clientSecret"}},
		{"rackspace", Annotations{Private: "apiKey"}, nil, []string{"apiKey"}, []string{"apiKey"}},
		{"softlayer", Annotations{Private: "apiKey"}, nil, []string{"apiKey"}, []string{"apiKey"}},
		{"vmwarevsphere", Annotations{Public: "username,vcenter,vcenterPort", Private: "password", Defaults: "vcenterPort:443"},
			[]string{"username", "vcenter", "vcenterPort"}, []string{"password"}, []string{"password", "username", "vcenter"}},
		{"outscale", Annotations{Public: "accessKey,region", Private: "secretKey"}, []string{"accessKey", "region"}, []string{"secretKey"}, []string{"accessKey", "region", "secretKey"}},
		{"added", Annotations{Public: "host,token", Password: "token"}, []string{"host"}, []string{"token"}, []string{"host", "token"}},
		{"sshdriver", Annotations{Public: "user,sshKey,sshKeyContents"}, []string{"user"}, []string{"sshKey", "sshKeyContents"}, []string{"sshKey", "sshKeyContents", "user"}},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			t.Parallel()
			d, ok := FromAnnotations(tc.prefix, tc.a)
			require.True(t, ok)
			assert.Equal(t, tc.public, d.Names(func(f Field) bool { return !f.Secret }), "public")
			assert.Equal(t, tc.secret, d.Names(func(f Field) bool { return f.Secret }), "secret")
			assert.Equal(t, tc.required, d.Names(func(f Field) bool { return f.Required }), "required")
		})
	}

	t.Run("static", func(t *testing.T) {
		t.Parallel()
		s3, ok := Static("s3")
		require.True(t, ok)
		assert.Equal(t, []string{"secretKey"}, s3.Names(func(f Field) bool { return f.Secret }))
		assert.Equal(t, []string{"accessKey", "secretKey"}, s3.Names(func(f Field) bool { return f.Required }))
		assert.Equal(t, []string{"defaultEndpointCA"}, s3.Names(func(f Field) bool { return f.Multiline }))
		alibaba, ok := Static("alibaba")
		require.True(t, ok)
		assert.Equal(t, []string{"accessKeySecret"}, alibaba.Names(func(f Field) bool { return f.Secret }))
		assert.Empty(t, alibaba.Names(func(f Field) bool { return f.Required }))
		_, ok = Static("amazonec2")
		assert.False(t, ok)
	})

	t.Run("no credential field", func(t *testing.T) {
		t.Parallel()
		_, ok := FromAnnotations("empty", Annotations{Password: "x"})
		assert.False(t, ok)
	})
}

// TestCloudCredentialDriverLabel asserts the annotation value is the UI's
// label: aws, gcp and oracle for the three drivers the UI renames, the prefix
// in lower case for every other, a mixed-case display name included.
func TestCloudCredentialDriverLabel(t *testing.T) {
	t.Parallel()
	for prefix, want := range map[string]string{
		"amazonec2": "aws", "google": "gcp", "oci": "oracle", "azure": "azure", "digitalocean": "digitalocean",
		"harvester": "harvester", "s3": "s3", "alibaba": "alibaba", "MyDriver": "mydriver",
	} {
		assert.Equal(t, want, Label(prefix), prefix)
	}
}

// TestSecretFoldedFindsEverySpellingOfASecretField asserts the secret lookup
// ignores case, so no spelling of a secret field passes as a public one.
func TestSecretFoldedFindsEverySpellingOfASecretField(t *testing.T) {
	t.Parallel()
	d, _ := Static("s3")
	f, ok := d.SecretFolded("SECRETKEY")
	require.True(t, ok)
	assert.Equal(t, "secretKey", f.Name)
	_, ok = d.SecretFolded("accesskey")
	assert.False(t, ok)
}

// TestOperatorActiveAndOfferedPrefixes asserts the kev2-operators setting is
// read as a JSON list, with an absent or unreadable value meaning inactive, and
// that the offered drivers are the usable node drivers with credential fields,
// s3, and alibaba only while its operator is active.
func TestOperatorActiveAndOfferedPrefixes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setting map[string]any
		want    bool
	}{
		"active":          {map[string]any{"value": `[{"name":"alibaba","active":true}]`}, true},
		"inactive":        {map[string]any{"value": `[{"name":"alibaba","active":false}]`}, false},
		"other operator":  {map[string]any{"value": `[{"name":"eks","active":true}]`}, false},
		"empty value":     {map[string]any{"value": "", "default": `[{"name":"alibaba","active":true}]`}, true},
		"unreadable":      {map[string]any{"value": "{}"}, false},
		"absent":          {nil, false},
		"no operator set": {map[string]any{"value": "[]"}, false},
	} {
		assert.Equal(t, tc.want, OperatorActive(tc.setting, "alibaba"), name)
	}

	obj := func(name, display string, active, add bool, public string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": name, "annotations": map[string]any{"publicCredentialFields": public}},
			"spec":     map[string]any{"displayName": display, "active": active, "addCloudCredential": add},
		}
	}
	drivers := []NodeDriver{
		NodeDriverOf(obj("amazonec2", "amazonec2", true, false, "accessKey")),
		NodeDriverOf(obj("linode", "linode", false, true, "token")),
		NodeDriverOf(obj("pnap", "pnap", false, false, "clientIdentifier")),
		NodeDriverOf(obj("nofields", "nofields", true, false, "")),
	}
	assert.Equal(t, []string{"amazonec2", "linode", "s3"}, OfferedPrefixes(drivers, false))
	assert.Equal(t, []string{"alibaba", "amazonec2", "linode", "s3"}, OfferedPrefixes(drivers, true))
}
