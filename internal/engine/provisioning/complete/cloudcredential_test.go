package complete

import (
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
	drivers := `{"items":[
		{"metadata":{"name":"amazonec2","annotations":{"publicCredentialFields":"accessKey,defaultRegion","privateCredentialFields":"secretKey","optionalCredentialFields":"defaultRegion"}},"spec":{"displayName":"amazonec2","active":true}},
		{"metadata":{"name":"digitalocean","annotations":{"privateCredentialFields":"accessToken"}},"spec":{"displayName":"digitalocean","active":false,"addCloudCredential":true}},
		{"metadata":{"name":"linode","annotations":{"privateCredentialFields":"token"}},"spec":{"displayName":"linode","active":false}},
		{"metadata":{"name":"custom","annotations":{"publicCredentialFields":"project,zone","passwordFields":"zone"}},"spec":{"displayName":"custom","active":true}},
		{"metadata":{"name":"harvester","annotations":{"privateCredentialFields":"kubeconfigContent"}},"spec":{"displayName":"harvester","active":true}},
		{"metadata":{"name":"nofields"},"spec":{"displayName":"nofields","active":true}},
		{"metadata":{"name":"amazonec2-dup","annotations":{"publicCredentialFields":"other"}},"spec":{"displayName":"amazonec2","active":true}}
	]}`
	return completiontest.New(t, map[string]string{nodeDriversPath: drivers, operatorsPath: setting})
}

// TestCloudCredentialSourceClaims asserts the sites the create source owns:
// <driver> with two reads; --set and --from-file once a driver is typed and
// before the value holds "=", with one read; every other site is declined.
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
		{name: "an inactive driver offers no field", req: completion.Request{Flag: "set", Positionals: []string{"linode"}}, requests: 1},
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
