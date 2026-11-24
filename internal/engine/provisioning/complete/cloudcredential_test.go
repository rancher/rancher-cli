package complete

import (
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nodeDriversPath = "/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers"
	operatorsPath   = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/kev2-operators"
)

const driversBody = `{"items":[
		{"metadata":{"name":"amazonec2","annotations":{"publicCredentialFields":"accessKey,defaultRegion","privateCredentialFields":"secretKey","optionalCredentialFields":"defaultRegion"}},"spec":{"displayName":"amazonec2","active":true}},
		{"metadata":{"name":"digitalocean","annotations":{"privateCredentialFields":"accessToken"}},"spec":{"displayName":"digitalocean","active":false,"addCloudCredential":true}},
		{"metadata":{"name":"linode","annotations":{"privateCredentialFields":"token"}},"spec":{"displayName":"linode","active":false}},
		{"metadata":{"name":"custom","annotations":{"publicCredentialFields":"project,zone","passwordFields":"zone"}},"spec":{"displayName":"custom","active":true}},
		{"metadata":{"name":"harvester","annotations":{"privateCredentialFields":"kubeconfigContent"}},"spec":{"displayName":"harvester","active":true}},
		{"metadata":{"name":"nofields"},"spec":{"displayName":"nofields","active":true}},
		{"metadata":{"name":"amazonec2-dup","annotations":{"publicCredentialFields":"other"}},"spec":{"displayName":"amazonec2","active":true}}
	]}`

// credentialServer serves two node drivers with a credential (one active, one
// with spec.addCloudCredential only), one inactive driver, one driver with no
// credential field, a second node driver that repeats a display name, and the
// kev2-operators setting.
func credentialServer(t *testing.T, alibabaActive bool) *completiontest.Server {
	t.Helper()
	setting := `{"value":"[{\"name\":\"alibaba\",\"active\":false}]"}`
	if alibabaActive {
		setting = `{"value":"[{\"name\":\"alibaba\",\"active\":true}]"}`
	}
	drivers := driversBody
	return completiontest.New(t, map[string]string{nodeDriversPath: drivers, operatorsPath: setting})
}

// TestCloudCredentialSourceClaims asserts the sites the create source owns:
// <driver> with two reads; --set and --from-file once a driver is typed and
// before the value has "=", with one read; every other site is declined.
func TestCloudCredentialSourceClaims(t *testing.T) {
	t.Parallel()
	src := CreateCloudCredentialSource()
	tests := []struct {
		name string
		req  completion.Request
		want completion.Requirements
		ok   bool
	}{
		{name: "driver", req: completion.Request{Index: 0}, want: completion.ManagementTwoListings(), ok: true},
		{name: "set after a driver", req: completion.Request{Flag: "set", Positionals: []string{"amazonec2"}}, want: completion.ManagementListing(), ok: true},
		{name: "from-file after a driver", req: completion.Request{Flag: "from-file", Positionals: []string{"amazonec2"}}, want: completion.ManagementListing(), ok: true},
		{name: "set with no driver", req: completion.Request{Flag: "set"}},
		{name: "set after the field", req: completion.Request{Flag: "set", Partial: "secretKey=", Positionals: []string{"amazonec2"}}},
		{name: "from-file after the field", req: completion.Request{Flag: "from-file", Partial: "secretKey=ab", Positionals: []string{"amazonec2"}}},
		{name: "display name", req: completion.Request{Index: 1, Positionals: []string{"amazonec2"}}},
		{name: "description", req: completion.Request{Flag: "description", Positionals: []string{"amazonec2"}}},
		{name: "wait", req: completion.Request{Flag: "wait", Positionals: []string{"amazonec2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqs, ok := src.Claim(tt.req)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.want, reqs)
			}
		})
	}
}

// TestCloudCredentialCompletion asserts every value the create source offers is
// one the command accepts: <driver> offers only a driver with a credential
// schema and credential fields, s3, and alibaba while its operator is active;
// --set offers no secret field and --from-file no public single-line field; a
// field already typed is not offered again; a driver the command refuses offers
// no field; each source sends no more requests than the allowance it claims.
func TestCloudCredentialCompletion(t *testing.T) {
	t.Parallel()
	src := CreateCloudCredentialSource()
	tests := []struct {
		name     string
		alibaba  bool
		req      completion.Request
		want     []string
		requests int
	}{
		{name: "drivers with alibaba off", req: completion.Request{Index: 0},
			want: []string{"amazonec2", "custom", "digitalocean", "harvester", "s3"}, requests: 2},
		{name: "drivers with alibaba on", alibaba: true, req: completion.Request{Index: 0},
			want: []string{"alibaba", "amazonec2", "custom", "digitalocean", "harvester", "s3"}, requests: 2},
		{name: "set offers the public fields", req: completion.Request{Flag: "set", Positionals: []string{"amazonec2"}},
			want: []string{"accessKey", "defaultRegion"}, requests: 1},
		{name: "set leaves out a field typed with from-file", req: completion.Request{Flag: "set", Positionals: []string{"amazonec2"},
			Tail: []string{"amazonec2", "prod", "--set", "accessKey=AKIA", "--from-file=secretKey=./k"}},
			want: []string{"defaultRegion"}, requests: 1},
		{name: "from-file offers the secret fields", req: completion.Request{Flag: "from-file", Positionals: []string{"amazonec2"}},
			want: []string{"secretKey"}, requests: 1},
		{name: "from-file leaves out a field already typed", req: completion.Request{Flag: "from-file", Positionals: []string{"amazonec2"},
			Tail: []string{"amazonec2", "prod", "--from-file", "secretKey=./k"}}, requests: 1},
		{name: "a password field named in passwordFields is not offered to set", req: completion.Request{Flag: "set", Positionals: []string{"custom"}},
			want: []string{"project"}, requests: 1},
		{name: "a driver with addCloudCredential and no active flag", req: completion.Request{Flag: "from-file", Positionals: []string{"digitalocean"}},
			want: []string{"accessToken"}, requests: 1},
		{name: "a multi-line field of a secret kind", req: completion.Request{Flag: "from-file", Positionals: []string{"harvester"}},
			want: []string{"kubeconfigContent"}, requests: 1},
		{name: "an inactive driver offers no field", req: completion.Request{Flag: "from-file", Positionals: []string{"linode"}}, requests: 1},
		{name: "a driver with no credential field offers none", req: completion.Request{Flag: "set", Positionals: []string{"nofields"}}, requests: 1},
		{name: "an unknown driver offers none", req: completion.Request{Flag: "set", Positionals: []string{"nonesuch"}}, requests: 1},
		{name: "s3 set offers its public fields with no request", req: completion.Request{Flag: "set", Positionals: []string{"s3"}},
			want: []string{"accessKey", "defaultBucket", "defaultEndpoint", "defaultEndpointCA", "defaultFolder", "defaultRegion", "defaultSkipSSLVerify"}},
		{name: "s3 from-file offers the secret and the multi-line field", req: completion.Request{Flag: "from-file", Positionals: []string{"s3"}},
			want: []string{"defaultEndpointCA", "secretKey"}},
		{name: "alibaba while inactive offers none", req: completion.Request{Flag: "from-file", Positionals: []string{"alibaba"}}, requests: 1},
		{name: "alibaba while active", alibaba: true, req: completion.Request{Flag: "from-file", Positionals: []string{"alibaba"}},
			want: []string{"accessKeySecret"}, requests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := credentialServer(t, tt.alibaba)
			reqs, ok := src.Claim(tt.req)
			require.True(t, ok)
			limit := 1
			if reqs.Allowance == completion.AllowanceTwo {
				limit = 2
			}
			conn := s.Conn(limit)
			got := src.Complete(t.Context(), conn, tt.req)
			assert.ElementsMatch(t, tt.want, got.Names)
			assert.Empty(t, got.Prefix)
			assert.False(t, conn.Exceeded(), "the source sent more requests than it claimed")
			assert.Len(t, s.Requested(), tt.requests)
		})
	}
}

// TestGetCloudCredentialSource asserts get cloud-credential's <credential>
// argument offers what create cluster's --cloud-credential offers, from one
// listing, and nothing at any other site.
func TestGetCloudCredentialSource(t *testing.T) {
	t.Parallel()
	src := GetCloudCredentialSource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for name, req := range map[string]completion.Request{
		"second positional": {Index: 1},
		"output flag":       {Flag: "output"},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, name)
	}

	s := completiontest.New(t, map[string]string{cloudCredsPath: `{"items":[
		{"metadata":{"name":"cc-abc12","annotations":{"field.cattle.io/name":"aws-prod"}}},
		{"metadata":{"name":"unrelated"}}]}`})
	got := src.Complete(t.Context(), s.Conn(1), completion.Request{Index: 0})
	assert.Equal(t, []string{"cc-abc12", "aws-prod"}, got.Names)
	assert.Equal(t, []string{cloudCredsPath}, s.Requested())
}

// storedCredentialsBody lists the Secrets of cattle-global-data the patch source
// resolves its credential from: one per driver, two that share a display name,
// one whose display name is another's id, and some that name no driver.
const storedCredentialsBody = `{"items":[
	{"metadata":{"name":"cc-aws1","annotations":{"field.cattle.io/name":"aws-prod"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-s3","annotations":{"field.cattle.io/name":"backup"}},"data":{"s3credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-hv","annotations":{"field.cattle.io/name":"hv"}},"data":{"harvestercredentialConfig-kubeconfigContent":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-twin1","annotations":{"field.cattle.io/name":"twin"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-twin2","annotations":{"field.cattle.io/name":"twin"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-shadow","annotations":{"field.cattle.io/name":"cc-aws1"}},"data":{"harvestercredentialConfig-kubeconfigContent":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-nokeys","annotations":{"field.cattle.io/name":"empty"}}},
	{"metadata":{"name":"unrelated"},"data":{"password":"ZmFrZQ=="}}
]}`

// TestPatchCloudCredentialSourceClaims asserts the sites the patch source owns:
// <credential> with one read; --set and --from-file once a credential is typed
// and before the value has "=", with two reads; every other site is declined,
// the display name and the description included.
func TestPatchCloudCredentialSourceClaims(t *testing.T) {
	t.Parallel()
	src := PatchCloudCredentialSource()
	tests := []struct {
		name string
		req  completion.Request
		want completion.Requirements
		ok   bool
	}{
		{name: "credential", req: completion.Request{Index: 0}, want: completion.ManagementListing(), ok: true},
		{name: "set after a credential", req: completion.Request{Flag: "set", Positionals: []string{"prod"}}, want: completion.ManagementTwoListings(), ok: true},
		{name: "from-file after a credential", req: completion.Request{Flag: "from-file", Positionals: []string{"prod"}}, want: completion.ManagementTwoListings(), ok: true},
		{name: "set with no credential", req: completion.Request{Flag: "set"}},
		{name: "set after the field", req: completion.Request{Flag: "set", Partial: "accessKey=", Positionals: []string{"prod"}}},
		{name: "from-file after the field", req: completion.Request{Flag: "from-file", Partial: "secretKey=ab", Positionals: []string{"prod"}}},
		{name: "second argument", req: completion.Request{Index: 1, Positionals: []string{"prod"}}},
		{name: "display name", req: completion.Request{Flag: "display-name", Positionals: []string{"prod"}}},
		{name: "description", req: completion.Request{Flag: "description", Positionals: []string{"prod"}}},
		{name: "wait", req: completion.Request{Flag: "wait", Positionals: []string{"prod"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqs, ok := src.Claim(tt.req)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.want, reqs)
			}
		})
	}
}

// TestPatchCloudCredentialCompletion asserts the patch source offers the fields
// of the driver the typed credential stores, by id only: --set the public fields
// and --from-file the secret and multi-line ones, a field already typed left
// out; nothing for a display name, for a bare id that is another credential's
// display name (the id given as "cattle-global-data:<id>" offers the fields),
// for a value that resolves to no credential, for a credential whose keys name
// no driver, or when the listing is refused; each source stays within the
// requests it claims, and s3 needs no second one. The <credential> argument
// offers every id but a bare id that is another credential's display name, and
// no display name.
func TestPatchCloudCredentialCompletion(t *testing.T) {
	t.Parallel()
	src := PatchCloudCredentialSource()
	tests := []struct {
		name     string
		bodies   map[string]string
		req      completion.Request
		want     []string
		requests int
	}{
		{name: "set by id", req: completion.Request{Flag: "set", Positionals: []string{"cc-twin1"}},
			want: []string{"accessKey", "defaultRegion"}, requests: 2},
		{name: "set by the namespaced id", req: completion.Request{Flag: "set", Positionals: []string{"cattle-global-data:cc-twin1"}},
			want: []string{"accessKey", "defaultRegion"}, requests: 2},
		{name: "set by the namespaced id of a credential that is another's display name", req: completion.Request{Flag: "set", Positionals: []string{"cattle-global-data:cc-aws1"}},
			want: []string{"accessKey", "defaultRegion"}, requests: 2},
		{name: "a bare id that is another credential's display name offers none", req: completion.Request{Flag: "from-file", Positionals: []string{"cc-aws1"}}, requests: 1},
		{name: "a display name offers none", req: completion.Request{Flag: "set", Positionals: []string{"aws-prod"}}, requests: 1},
		{name: "a namespaced display name offers none", req: completion.Request{Flag: "set", Positionals: []string{"cattle-global-data:aws-prod"}}, requests: 1},
		{name: "from-file offers the secret fields", req: completion.Request{Flag: "from-file", Positionals: []string{"cc-twin1"}},
			want: []string{"secretKey"}, requests: 2},
		{name: "set leaves out a field typed with from-file", req: completion.Request{Flag: "set", Positionals: []string{"cc-twin1"},
			Tail: []string{"cc-twin1", "--from-file=accessKey=./k"}}, want: []string{"defaultRegion"}, requests: 2},
		{name: "a multi-line field of a secret kind", req: completion.Request{Flag: "from-file", Positionals: []string{"cc-hv"}},
			want: []string{"kubeconfigContent"}, requests: 2},
		{name: "s3 set needs no second request", req: completion.Request{Flag: "set", Positionals: []string{"cc-s3"}},
			want: []string{"accessKey", "defaultBucket", "defaultEndpoint", "defaultEndpointCA", "defaultFolder", "defaultRegion", "defaultSkipSSLVerify"}, requests: 1},
		{name: "s3 from-file offers the secret and the multi-line field", req: completion.Request{Flag: "from-file", Positionals: []string{"cc-s3"}},
			want: []string{"defaultEndpointCA", "secretKey"}, requests: 1},
		{name: "a display name on two credentials offers none", req: completion.Request{Flag: "set", Positionals: []string{"twin"}}, requests: 1},
		{name: "a value that is no credential offers none", req: completion.Request{Flag: "set", Positionals: []string{"nonesuch"}}, requests: 1},
		{name: "a Secret that is no credential offers none", req: completion.Request{Flag: "set", Positionals: []string{"unrelated"}}, requests: 1},
		{name: "a credential with no driver key offers none", req: completion.Request{Flag: "set", Positionals: []string{"cc-nokeys"}}, requests: 1},
		{name: "a refused listing offers none", bodies: map[string]string{}, req: completion.Request{Flag: "set", Positionals: []string{"cc-twin1"}}, requests: 1},
		{name: "the credential argument", req: completion.Request{Index: 0},
			want: []string{"cc-s3", "cc-hv", "cc-twin1", "cc-twin2", "cc-shadow", "cc-nokeys"}, requests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bodies := map[string]string{
				cloudCredsPath:  storedCredentialsBody,
				nodeDriversPath: driversBody,
				operatorsPath:   `{"value":"[{\"name\":\"alibaba\",\"active\":false}]"}`,
			}
			if tt.bodies != nil {
				bodies = tt.bodies
			}
			s := completiontest.New(t, bodies)
			reqs, ok := src.Claim(tt.req)
			require.True(t, ok)
			limit := 1
			if reqs.Allowance == completion.AllowanceTwo {
				limit = 2
			}
			conn := s.Conn(limit)
			got := src.Complete(t.Context(), conn, tt.req)
			assert.ElementsMatch(t, tt.want, got.Names)
			assert.Empty(t, got.Prefix)
			assert.False(t, conn.Exceeded(), "the source sent more requests than it claimed")
			assert.Len(t, s.Requested(), tt.requests)
		})
	}
}

// TestPatchCloudCredentialCompletionOffersNoSecretToSet asserts the patch
// source never offers a secret field to --set, and for a driver it cannot look
// up never returns the fields of another.
func TestPatchCloudCredentialCompletionOffersNoSecretToSet(t *testing.T) {
	t.Parallel()
	src := PatchCloudCredentialSource()
	s := completiontest.New(t, map[string]string{
		cloudCredsPath:  storedCredentialsBody,
		nodeDriversPath: driversBody,
		operatorsPath:   `{"value":"[]"}`,
	})
	got := src.Complete(t.Context(), s.Conn(2), completion.Request{Flag: "set", Positionals: []string{"cc-twin1"}})
	assert.NotContains(t, got.Names, "secretKey")
	none := src.Complete(t.Context(), s.Conn(2), completion.Request{Flag: "set", Positionals: []string{"no-such-credential"}})
	assert.Empty(t, none.Names)
}

// TestDeleteCloudCredentialSource asserts the delete's <credential> argument
// offers the credential ids with the display names as descriptions, from one
// listing, and nothing at any other site, the wait flags included.
func TestDeleteCloudCredentialSource(t *testing.T) {
	t.Parallel()
	src := DeleteCloudCredentialSource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for name, req := range map[string]completion.Request{
		"second argument": {Index: 1},
		"wait":            {Flag: "wait", Positionals: []string{"cc-abc12"}},
		"timeout":         {Flag: "timeout", Positionals: []string{"cc-abc12"}},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, name)
	}

	s := completiontest.New(t, map[string]string{cloudCredsPath: `{"items":[
		{"metadata":{"name":"cc-abc12","annotations":{"field.cattle.io/name":"aws-prod"}}},
		{"metadata":{"name":"unrelated"}}]}`})
	got := src.Complete(t.Context(), s.Conn(1), completion.Request{Index: 0})
	assert.Equal(t, []string{"cc-abc12"}, got.Names)
	assert.Equal(t, map[string]string{"cc-abc12": "aws-prod"}, got.Descriptions)
	assert.Equal(t, []string{cloudCredsPath}, s.Requested())
}

// changingCredentialsBody lists the credentials of the table below: cc-x, whose
// id is also the display name of cc-y, and cc-z, whose display name is no id.
const changingCredentialsBody = `{"items":[
	{"metadata":{"name":"cc-x","annotations":{"field.cattle.io/name":"x-display"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-y","annotations":{"field.cattle.io/name":"cc-x"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}},
	{"metadata":{"name":"cc-z","annotations":{"field.cattle.io/name":"z-display"}},"data":{"amazonec2credentialConfig-secretKey":"ZmFrZQ=="}}
]}`

// TestMutationsOfferOnlyCredentialIDs asserts, for create cluster
// --cloud-credential and the <credential> argument of patch and delete, that
// the candidates are the Secret names with the display name as the
// description, that no display name is offered, and that cc-x, which is the
// display name of cc-y, is left out because the command refuses it as
// ambiguous. For the --set of patch, a display name offers no field, a bare id
// that is another credential's display name offers none, and the id given as
// "cattle-global-data:<id>" offers the fields. The get source still
// offers the display names.
func TestMutationsOfferOnlyCredentialIDs(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{cloudCredsPath: changingCredentialsBody, nodeDriversPath: driversBody,
		operatorsPath: `{"value":"[]"}`}
	positional := []struct {
		name string
		src  completion.Source
		req  completion.Request
	}{
		{"create cluster", CreateClusterSource(), completion.Request{Flag: "cloud-credential"}},
		{"patch", PatchCloudCredentialSource(), completion.Request{Index: 0}},
		{"delete", DeleteCloudCredentialSource(), completion.Request{Index: 0}},
	}
	for _, tt := range positional {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.src.Complete(t.Context(), completiontest.New(t, bodies).Conn(1), tt.req)
			assert.Equal(t, []string{"cc-y", "cc-z"}, got.Names)
			assert.Equal(t, map[string]string{"cc-y": "cc-x", "cc-z": "z-display"}, got.Descriptions)
		})
	}

	patch := PatchCloudCredentialSource()
	for value, want := range map[string][]string{
		"z-display":                    nil,
		"cattle-global-data:z-display": nil,
		"cc-x":                         nil,
		"cattle-global-data:cc-x":      {"accessKey", "defaultRegion"},
		"cc-z":                         {"accessKey", "defaultRegion"},
	} {
		t.Run("set "+value, func(t *testing.T) {
			t.Parallel()
			got := patch.Complete(t.Context(), completiontest.New(t, bodies).Conn(2), completion.Request{Flag: "set", Positionals: []string{value}})
			assert.ElementsMatch(t, want, got.Names)
		})
	}

	t.Run("get", func(t *testing.T) {
		t.Parallel()
		got := GetCloudCredentialSource().Complete(t.Context(), completiontest.New(t, bodies).Conn(1), completion.Request{Index: 0})
		assert.Equal(t, []string{"cc-x", "cc-y", "cc-z", "x-display", "z-display"}, slices.Sorted(slices.Values(got.Names)))
	})
}
